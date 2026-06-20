package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// fakeApp builds an app whose probes return fixed statuses, so the HTTP
// handlers can be tested without touching a real database or cache.
func fakeApp(statuses ...integrationStatus) *app {
	a := &app{bindingRoot: "/nonexistent"}
	for _, s := range statuses {
		s := s
		a.probes = append(a.probes, namedProbe{s.Name, func(context.Context) integrationStatus { return s }})
	}
	return a
}

func TestHandleRoot_FiltersNotConfiguredAndReportsReady(t *testing.T) {
	a := fakeApp(
		integrationStatus{Name: "PostgreSQL", Status: "ok", Detail: "wrote row, 3 total (5ms)"},
		integrationStatus{Name: "Redis", Status: "ok", Detail: "visit #7 (1ms)"},
		integrationStatus{Name: "NoSQL Database", Status: "not_configured"},
		integrationStatus{Name: "Object Storage", Status: "not_configured"},
	)

	rec := httptest.NewRecorder()
	a.handleRoot(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("CORS header = %q, want *", got)
	}

	var body struct {
		Ready        bool                `json:"ready"`
		Integrations []integrationStatus `json:"integrations"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Ready {
		t.Error("ready = false, want true (all wired backends ok)")
	}
	if len(body.Integrations) != 2 {
		t.Fatalf("integrations = %d, want 2 (not_configured filtered out)", len(body.Integrations))
	}
	if body.Integrations[0].Detail == "" {
		t.Error("expected round-trip detail to be surfaced")
	}
}

func TestHandleRoot_NotReadyWhenBackendStarting(t *testing.T) {
	a := fakeApp(
		integrationStatus{Name: "PostgreSQL", Status: "starting"},
		integrationStatus{Name: "Redis", Status: "ok"},
	)

	rec := httptest.NewRecorder()
	a.handleRoot(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	var body struct {
		Ready bool `json:"ready"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Ready {
		t.Error("ready = true, want false (a backend is still starting)")
	}
}

func TestHandleReadyz(t *testing.T) {
	tests := []struct {
		name     string
		statuses []integrationStatus
		want     int
	}{
		{"all ok", []integrationStatus{{Status: "ok"}, {Status: "ok"}}, http.StatusOK},
		{"ok and not_configured", []integrationStatus{{Status: "ok"}, {Status: "not_configured"}}, http.StatusOK},
		{"one starting", []integrationStatus{{Status: "ok"}, {Status: "starting"}}, http.StatusServiceUnavailable},
		{"one error", []integrationStatus{{Status: "error"}}, http.StatusServiceUnavailable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fakeApp(tt.statuses...)
			rec := httptest.NewRecorder()
			a.handleReadyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestReadBinding(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sql")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "host"), []byte("db.example.svc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "port"), []byte("5432"), 0o600); err != nil {
		t.Fatal(err)
	}

	b, ok := readBinding(root, "sql")
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if b["host"] != "db.example.svc" {
		t.Errorf("host = %q, want trimmed value", b["host"])
	}
	if b["port"] != "5432" {
		t.Errorf("port = %q, want 5432", b["port"])
	}

	if _, ok := readBinding(root, "missing"); ok {
		t.Error("ok = true for missing binding, want false")
	}
}

func TestPresenceProbe(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "object-storage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "type"), []byte("s3"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := presenceProbe(root, "object-storage", "Object Storage"); got.Status != "ok" {
		t.Errorf("status = %q, want ok", got.Status)
	}
	if got := presenceProbe(root, "nosql", "NoSQL Database"); got.Status != "not_configured" {
		t.Errorf("status = %q, want not_configured", got.Status)
	}
}
