package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type integrationStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ok", "starting", or "not_configured"
	Detail string `json:"detail,omitempty"`
}

// probeBinding checks whether a service binding directory exists and whether
// the service it describes is reachable via TCP.
func probeBinding(root, name, displayName string) integrationStatus {
	hostBytes, err := os.ReadFile(filepath.Join(root, name, "host"))
	if err != nil {
		return integrationStatus{Name: displayName, Status: "not_configured"}
	}
	host := strings.TrimSpace(string(hostBytes))

	portBytes, _ := os.ReadFile(filepath.Join(root, name, "port"))
	port := strings.TrimSpace(string(portBytes))
	if port == "" {
		switch name {
		case "sql":
			port = "5432"
		case "cache":
			port = "6379"
		}
	}

	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return integrationStatus{Name: displayName, Status: "starting", Detail: "service binding ready, waiting for service"}
	}
	_ = conn.Close()
	return integrationStatus{Name: displayName, Status: "ok", Detail: host}
}

// probeCloudBinding checks a cloud service binding (DynamoDB, S3, etc.) that
// has no TCP host. The XApi composition's init containers already block the app
// container until the "type" sentinel file is written, so this will always be
// "ok" (wired) or "not_configured" (not wired) — never "starting".
func probeCloudBinding(root, name, displayName string) integrationStatus {
	_, err := os.ReadFile(filepath.Join(root, name, "type"))
	if err != nil {
		return integrationStatus{Name: displayName, Status: "not_configured"}
	}
	return integrationStatus{Name: displayName, Status: "ok"}
}

func checkIntegrations() []integrationStatus {
	root := envOrDefault("SERVICE_BINDING_ROOT", "/bindings")
	return []integrationStatus{
		probeBinding(root, "sql", "PostgreSQL"),
		probeBinding(root, "cache", "Redis"),
		probeCloudBinding(root, "nosql", "NoSQL Database"),
		probeCloudBinding(root, "object-storage", "Object Storage"),
	}
}

func main() {
	port := envOrDefault("PORT", "8080")

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{})))

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		for _, i := range checkIntegrations() {
			if i.Status != "ok" && i.Status != "not_configured" {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
		}
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		integrations := checkIntegrations()
		ready := true
		var wired []integrationStatus
		for _, i := range integrations {
			if i.Status == "not_configured" {
				continue
			}
			wired = append(wired, i)
			if i.Status != "ok" {
				ready = false
			}
		}
		workspace := podNamespace()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct { //nolint:errcheck
			Message      string              `json:"message"`
			Workspace    string              `json:"workspace"`
			Timestamp    string              `json:"timestamp"`
			Ready        bool                `json:"ready"`
			Integrations []integrationStatus `json:"integrations"`
		}{
			Message:      "Hello from " + workspace + "!",
			Workspace:    workspace,
			Timestamp:    time.Now().UTC().Format(time.RFC3339),
			Ready:        ready,
			Integrations: wired,
		})
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      mux,
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	slog.Info("hello-world-api starting", "port", port)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("hello-world-api shutting down")
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func podNamespace() string {
	b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return "guest"
	}
	return strings.TrimSpace(string(b))
}
