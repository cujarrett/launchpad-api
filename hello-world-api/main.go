package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type integrationStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "ok", "unavailable", or "not_configured"
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

// probeNATS checks whether the NATS server referenced by NATS_URL is reachable.
func probeNATS() integrationStatus {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		return integrationStatus{Name: "NATS", Status: "not_configured"}
	}
	u, err := url.Parse(natsURL)
	if err != nil {
		return integrationStatus{Name: "NATS", Status: "unavailable", Detail: "invalid URL"}
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "4222"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 3*time.Second)
	if err != nil {
		return integrationStatus{Name: "NATS", Status: "starting", Detail: "service binding ready, waiting for service"}
	}
	_ = conn.Close()
	detail := host
	if stream := os.Getenv("NATS_STREAM"); stream != "" {
		detail += " (stream: " + stream + ")"
	}
	return integrationStatus{Name: "NATS", Status: "ok", Detail: detail}
}

func checkIntegrations() []integrationStatus {
	root := envOrDefault("SERVICE_BINDING_ROOT", "/bindings")
	return []integrationStatus{
		probeBinding(root, "sql", "PostgreSQL"),
		probeBinding(root, "cache", "Redis"),
		probeNATS(),
	}
}

func main() {
	port := envOrDefault("PORT", "8080")

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{})))

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
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
		w.Header().Set("Content-Type", "application/json")
		integrations := checkIntegrations()
		ready := true
		for _, i := range integrations {
			if i.Status != "ok" && i.Status != "not_configured" {
				ready = false
				break
			}
		}
		json.NewEncoder(w).Encode(struct { //nolint:errcheck
			Message      string              `json:"message"`
			Workspace    string              `json:"workspace"`
			Timestamp    string              `json:"timestamp"`
			Ready        bool                `json:"ready"`
			Integrations []integrationStatus `json:"integrations"`
		}{
			Message:      "Hello from the Launchpad sandbox!",
			Workspace:    envOrDefault("NAMESPACE", "guest"),
			Timestamp:    time.Now().UTC().Format(time.RFC3339),
			Ready:        ready,
			Integrations: integrations,
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
