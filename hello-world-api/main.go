package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// integrationStatus is the per-backend result the SPA renders as a card.
//
//	ok             — round-trip (or presence check) succeeded
//	starting       — binding present, backend not reachable yet (retryable)
//	error          — binding present, round-trip failed
//	not_configured — no binding mounted; the card is hidden by the SPA
type integrationStatus struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// binding holds the servicebinding.io key/value files for one mounted binding.
type binding map[string]string

// readBinding loads every file under <root>/<name>/ into a map. A missing
// directory means the binding is not wired, signalled by ok=false.
func readBinding(root, name string) (binding, bool) {
	dir := filepath.Join(root, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	b := make(binding, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		v, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		b[e.Name()] = strings.TrimSpace(string(v))
	}
	return b, true
}

// namedProbe pairs a display name with the function that produces its status.
type namedProbe struct {
	name string
	run  func(ctx context.Context) integrationStatus
}

type app struct {
	bindingRoot string
	probes      []namedProbe

	mu    sync.Mutex
	pg    *pgxpool.Pool // cached after first successful connect
	redis *redis.Client // cached after first successful connect
}

func newApp(bindingRoot string) *app {
	a := &app{bindingRoot: bindingRoot}
	a.probes = []namedProbe{
		{"PostgreSQL", a.probePostgres},
		{"Redis", a.probeRedis},
		{"NoSQL Database", a.probeNoSQL},
		{"Object Storage", a.probeObjectStorage},
	}
	return a
}

func (a *app) checkIntegrations(ctx context.Context) []integrationStatus {
	out := make([]integrationStatus, len(a.probes))
	var wg sync.WaitGroup
	for i, p := range a.probes {
		wg.Add(1)
		go func(idx int, probe namedProbe) {
			defer wg.Done()
			out[idx] = probe.run(ctx)
		}(i, p)
	}
	wg.Wait()
	return out
}

// pgPool returns a cached pool, connecting on first use. A failed connect is
// not cached, so a backend that is still starting is retried on the next poll.
func (a *app) pgPool(ctx context.Context, b binding) (*pgxpool.Pool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.pg != nil {
		return a.pg, nil
	}
	port := b["port"]
	if port == "" {
		port = "5432"
	}
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(b["username"], b["password"]),
		Host:     net.JoinHostPort(b["host"], port),
		Path:     "/" + b["database"],
		RawQuery: "sslmode=disable&connect_timeout=3",
	}).String()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	a.pg = pool
	return pool, nil
}

// probePostgres performs a real write/read round-trip: it appends a row to a
// visit-counter table and reads the running total back. The detail string is
// the proof the integration works, not just that the port is open.
func (a *app) probePostgres(ctx context.Context) integrationStatus {
	const name = "PostgreSQL"
	b, ok := readBinding(a.bindingRoot, "sql")
	if !ok {
		return integrationStatus{Name: name, Status: "not_configured"}
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	pool, err := a.pgPool(ctx, b)
	if err != nil {
		return integrationStatus{Name: name, Status: "starting", Detail: "service binding ready, waiting for database"}
	}

	start := time.Now()
	if _, err := pool.Exec(ctx,
		`CREATE TABLE IF NOT EXISTS hello_visits (id bigserial PRIMARY KEY, at timestamptz NOT NULL DEFAULT now())`,
	); err != nil {
		slog.Warn("postgres probe failed", "op", "create", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO hello_visits DEFAULT VALUES`); err != nil {
		slog.Warn("postgres probe failed", "op", "insert", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
	}
	var total int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM hello_visits`).Scan(&total); err != nil {
		slog.Warn("postgres probe failed", "op", "read", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "read failed"}
	}
	return integrationStatus{
		Name:   name,
		Status: "ok",
		Detail: fmt.Sprintf("wrote row, %d total (%dms)", total, time.Since(start).Milliseconds()),
	}
}

// redisClient returns a cached client, connecting on first use. As with
// Postgres, a failed connect is not cached so it is retried next poll.
func (a *app) redisClient(ctx context.Context, b binding) (*redis.Client, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.redis != nil {
		return a.redis, nil
	}
	port := b["port"]
	if port == "" {
		port = "6379"
	}
	c := redis.NewClient(&redis.Options{
		Addr:         net.JoinHostPort(b["host"], port),
		Password:     b["password"], // empty for in-cluster Redis
		DialTimeout:  3 * time.Second,
		ReadTimeout:  3 * time.Second,
		WriteTimeout: 3 * time.Second,
	})
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, err
	}
	a.redis = c
	return c, nil
}

// probeRedis performs a real round-trip: it increments a visit counter and
// reads the new value back.
func (a *app) probeRedis(ctx context.Context) integrationStatus {
	const name = "Redis"
	b, ok := readBinding(a.bindingRoot, "cache")
	if !ok {
		return integrationStatus{Name: name, Status: "not_configured"}
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()

	c, err := a.redisClient(ctx, b)
	if err != nil {
		return integrationStatus{Name: name, Status: "starting", Detail: "service binding ready, waiting for cache"}
	}
	start := time.Now()
	n, err := c.Incr(ctx, "hello:visits").Result()
	if err != nil {
		slog.Warn("redis probe failed", "op", "incr", "err", err)
		return integrationStatus{Name: name, Status: "error", Detail: "write failed"}
	}
	return integrationStatus{
		Name:   name,
		Status: "ok",
		Detail: fmt.Sprintf("visit #%d (%dms)", n, time.Since(start).Milliseconds()),
	}
}

func (a *app) probeNoSQL(_ context.Context) integrationStatus {
	return presenceProbe(a.bindingRoot, "nosql", "NoSQL Database")
}

func (a *app) probeObjectStorage(_ context.Context) integrationStatus {
	return presenceProbe(a.bindingRoot, "object-storage", "Object Storage")
}

func presenceProbe(root, name, displayName string) integrationStatus {
	if _, ok := readBinding(root, name); !ok {
		return integrationStatus{Name: displayName, Status: "not_configured"}
	}
	credsFile := os.Getenv("AWS_SHARED_CREDENTIALS_FILE")
	if credsFile != "" {
		if data, err := os.ReadFile(credsFile); err == nil {
			// Check for INI section header (e.g., [nosql], [object-storage]) in credentials file
			if strings.Contains(string(data), "["+name+"]") {
				return integrationStatus{Name: displayName, Status: "ok", Detail: "credentials active"}
			}
		}
	}
	return integrationStatus{Name: displayName, Status: "ok", Detail: "workload identity pending"}
}

func main() {
	port := envOrDefault("PORT", "8080")
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{})))

	a := newApp(envOrDefault("SERVICE_BINDING_ROOT", "/bindings"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", a.handleReadyz)
	mux.HandleFunc("GET /", a.handleRoot)

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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
	if a.pg != nil {
		a.pg.Close()
	}
	if a.redis != nil {
		_ = a.redis.Close()
	}
}

func (a *app) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	for _, i := range a.checkIntegrations(r.Context()) {
		if i.Status != "ok" && i.Status != "not_configured" {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (a *app) handleRoot(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	integrations := a.checkIntegrations(r.Context())
	ready := true
	wired := make([]integrationStatus, 0, len(integrations))
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
	_ = json.NewEncoder(w).Encode(struct {
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
