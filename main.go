package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
)

type app struct {
	gh        *githubClient
	bcast     *broadcaster
	dynClient dynamic.Interface // nil if K8s unavailable
}

func main() {
	port := envOrDefault("PORT", "8080")

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	gh := newGithubClient(
		mustEnv("LAUNCHPAD_API"),
		envOrDefault("GITHUB_OWNER", "cujarrett"),
		envOrDefault("GITHUB_REPO", "homelab-workspaces"),
	)

	b := newBroadcaster()

	var dynClient dynamic.Interface
	if cfg, cfgErr := loadKubeconfig(); cfgErr != nil {
		slog.Warn("kubeconfig unavailable, K8s features disabled", "err", cfgErr)
	} else if c, cErr := dynamic.NewForConfig(cfg); cErr != nil {
		slog.Warn("dynamic client unavailable", "err", cErr)
	} else {
		dynClient = c
	}

	a := &app{gh: gh, bcast: b, dynClient: dynClient}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	auth, err := newAuthMiddleware(ctx)
	if err != nil {
		slog.Error("auth init failed", "err", err)
		os.Exit(1)
	}

	go startWatcher(ctx, b, dynClient)
	go a.startGuestCleanup(ctx)

	api := http.NewServeMux()
	api.HandleFunc("GET /api/workspaces", a.handleListWorkspaces)
	api.HandleFunc("GET /api/workspaces/{name}/resources", a.handleListResources)
	api.HandleFunc("POST /api/workspaces", a.handleCreateWorkspace)
	api.HandleFunc("POST /api/workspaces/{name}/resources", a.handleCreateResource)
	api.HandleFunc("DELETE /api/workspaces/{name}", a.handleDeleteWorkspace)
	api.HandleFunc("DELETE /api/workspaces/{name}/resources/{resource}", a.handleDeleteResource)
	api.HandleFunc("GET /api/workspaces/{name}/resources/{resource}/values", a.handleGetResourceValues)
	api.HandleFunc("GET /api/schema/{kind}", a.handleGetSchema)
	api.HandleFunc("GET /api/status/watch", a.handleStatusWatch)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /metrics", a.handleMetrics)
	// Guest endpoints — no auth required; registered before the catch-all.
	mux.HandleFunc("GET /api/guest/workspaces", a.handleListGuestWorkspaces)
	mux.HandleFunc("POST /api/guest/workspaces", a.handleCreateGuestWorkspace)
	mux.HandleFunc("POST /api/guest/workspaces/{name}/resources", a.handleCreateGuestResource)
	mux.HandleFunc("PATCH /api/guest/workspaces/{name}/resources/{resource}", a.handlePatchGuestResource)
	mux.Handle("/", auth.requireAuth(api))

	srv := &http.Server{
		Addr:         ":" + port,
		Handler:      requestLogger(mux),
		BaseContext:  func(_ net.Listener) context.Context { return ctx },
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 0, // SSE requires no write timeout
		IdleTimeout:  120 * time.Second,
	}

	slog.Info("launchpad-api starting", "port", port)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("launchpad-api shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown", "err", err)
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

func (sr *statusRecorder) Flush() {
	if f, ok := sr.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		// Skip healthz to avoid log noise from readiness probes.
		if r.URL.Path == "/healthz" {
			return
		}

		ip := r.Header.Get("X-Forwarded-For")
		if ip == "" {
			ip = r.RemoteAddr
		}

		slog.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).String(),
			"ip", ip,
		)
	})
}

// handleCreateResource validates the request, renders YAML, and writes to GitHub.
func (a *app) handleCreateResource(w http.ResponseWriter, r *http.Request) {
	if !requireContributor(w, r) {
		return
	}
	tenant := r.PathValue("name")
	if !validWorkspaceName.MatchString(tenant) {
		http.Error(w, "invalid workspace name", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var req writeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	req.Workspace = tenant

	// Default namespace param to tenant name if not set.
	if req.Params == nil {
		req.Params = map[string]any{}
	}
	if _, ok := req.Params["namespace"]; !ok {
		req.Params["namespace"] = tenant
	}

	if err := validate(req); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Ensure namespace.yaml exists for this tenant.
	nsPath := fmt.Sprintf("%s/namespace.yaml", tenant)
	if err := a.gh.upsertFile(ctx, nsPath, fmt.Sprintf("chore: ensure namespace for %s", tenant), RenderNamespace(tenant)); err != nil {
		slog.Error("upsert namespace", "err", err)
		http.Error(w, "failed to write namespace", http.StatusInternalServerError)
		return
	}

	yaml, err := RenderResource(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}

	resourcePath := fmt.Sprintf("%s/%s.yaml", tenant, req.Name)
	commitMsg := fmt.Sprintf("feat(%s): add %s %s", tenant, req.Kind, req.Name)
	if err := a.gh.upsertFile(ctx, resourcePath, commitMsg, yaml); err != nil {
		slog.Error("upsert resource", "err", err)
		http.Error(w, "failed to write resource", http.StatusInternalServerError)
		return
	}

	slog.Info("committed", "path", resourcePath)
	a.triggerArgoSync(tenant)
	w.WriteHeader(http.StatusAccepted)
}

// handleStatusWatch streams K8s XR status events to the client as SSE.
func (a *app) handleStatusWatch(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := a.bcast.subscribe()
	defer a.bcast.unsubscribe(ch)

	heartbeat := time.NewTicker(30 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
			flusher.Flush()
		case s := <-ch:
			data, err := json.Marshal(s)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// handleDeleteResource removes a resource YAML file from the GitHub repo.
func (a *app) handleDeleteResource(w http.ResponseWriter, r *http.Request) {
	if !requireContributor(w, r) {
		return
	}
	tenant := r.PathValue("name")
	resource := r.PathValue("resource")
	if !validWorkspaceName.MatchString(tenant) || !validWorkspaceName.MatchString(resource) {
		http.Error(w, "invalid workspace or resource name", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	path := fmt.Sprintf("%s/%s.yaml", tenant, resource)
	commitMsg := fmt.Sprintf("chore(%s): delete %s", tenant, resource)
	if err := a.gh.deleteFile(ctx, path, commitMsg); err != nil {
		slog.Error("delete resource", "err", err)
		http.Error(w, "failed to delete resource", http.StatusInternalServerError)
		return
	}

	slog.Info("deleted", "path", path)
	a.triggerArgoSync(tenant)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required env var not set", "key", key)
		os.Exit(1)
	}
	return v
}
