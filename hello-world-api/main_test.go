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

// writeBinding creates <root>/<name>/ with one file per key.
func writeBinding(t *testing.T, root, name string, keys map[string]string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for k, v := range keys {
		if err := os.WriteFile(filepath.Join(dir, k), []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHasProfile(t *testing.T) {
	creds := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(creds, []byte("[nosql]\naws_access_key_id = x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", creds)

	if !hasProfile("nosql") {
		t.Error("hasProfile(nosql) = false, want true")
	}
	if hasProfile("cache") {
		t.Error("hasProfile(cache) = true, want false (section not written yet)")
	}
}

func TestProbeNoSQL_StartingUntilCredentialsWritten(t *testing.T) {
	root := t.TempDir()
	writeBinding(t, root, "nosql", map[string]string{"table-name": "foo", "region": "us-east-1"})
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing"))

	a := &app{bindingRoot: root}
	if got := a.probeNoSQL(context.Background()); got.Status != "starting" {
		t.Errorf("status = %q, want starting (credentials not written yet)", got.Status)
	}

	a.bindingRoot = t.TempDir()
	if got := a.probeNoSQL(context.Background()); got.Status != "not_configured" {
		t.Errorf("status = %q, want not_configured (no binding)", got.Status)
	}
}

func TestProbeObjectStorage_DiscoversRefDirs(t *testing.T) {
	root := t.TempDir()
	writeBinding(t, root, "object-storage", map[string]string{"bucket": "b0", "region": "us-east-1"})
	writeBinding(t, root, "object-storage-1", map[string]string{"bucket": "b1", "region": "us-east-1"})
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing"))

	a := &app{bindingRoot: root}
	if got := a.probeObjectStorage(context.Background()); got.Status != "starting" {
		t.Errorf("status = %q, want starting (both refs found, credentials pending)", got.Status)
	}

	a.bindingRoot = t.TempDir()
	if got := a.probeObjectStorage(context.Background()); got.Status != "not_configured" {
		t.Errorf("status = %q, want not_configured (no object-storage dirs)", got.Status)
	}
}

func TestProbePostgres_DispatchesOnBindingShape(t *testing.T) {
	root := t.TempDir()
	// role-arn and no password → public-cloud IAM path, which reports starting
	// until the sidecar writes the sql profile.
	writeBinding(t, root, "sql", map[string]string{
		"host": "db.abc.us-east-1.rds.amazonaws.com", "port": "5432",
		"database": "foo", "username": "app", "role-arn": "arn:aws:iam::1:role/x",
	})
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "missing"))

	a := &app{bindingRoot: root}
	got := a.probePostgres(context.Background())
	if got.Status != "starting" {
		t.Errorf("status = %q, want starting (IAM path, credentials pending)", got.Status)
	}
}

func TestRegionFromHost(t *testing.T) {
	if got := regionFromHost("foo.abc123.us-west-2.rds.amazonaws.com"); got != "us-west-2" {
		t.Errorf("region = %q, want us-west-2", got)
	}
	if got := regionFromHost("postgres.e2e.svc.cluster.local"); got != "us-east-1" {
		t.Errorf("region = %q, want us-east-1 fallback", got)
	}
}

func TestPublishableSubject(t *testing.T) {
	tests := []struct {
		subjects []string
		want     string
	}{
		{[]string{"e2e.>"}, "e2e.probe"},
		{[]string{"foo.*.bar"}, "foo.probe.bar"},
		{[]string{"plain.subject"}, "plain.subject"},
		{nil, ""},
	}
	for _, tt := range tests {
		if got := publishableSubject(tt.subjects); got != tt.want {
			t.Errorf("publishableSubject(%v) = %q, want %q", tt.subjects, got, tt.want)
		}
	}
}

func TestNATSProbes_NotConfiguredWithoutEnv(t *testing.T) {
	t.Setenv("NATS_STREAM", "")
	t.Setenv("NATS_CONSUMER", "")
	a := &app{bindingRoot: t.TempDir()}

	if got := a.probeTopic(context.Background()); got.Status != "not_configured" {
		t.Errorf("topic status = %q, want not_configured", got.Status)
	}
	if got := a.probeSubscription(context.Background()); got.Status != "not_configured" {
		t.Errorf("subscription status = %q, want not_configured", got.Status)
	}
}

func TestProbeSubscription_ErrorsWithoutStream(t *testing.T) {
	t.Setenv("NATS_CONSUMER", "e2e-sub")
	t.Setenv("NATS_STREAM", "")
	a := &app{bindingRoot: t.TempDir()}

	got := a.probeSubscription(context.Background())
	if got.Status != "error" {
		t.Errorf("status = %q, want error (consumer set but stream missing)", got.Status)
	}
}
