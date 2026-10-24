package main

import (
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pins for the public sandbox map, kept in launchpad-api's own Sql (binding at /bindings/sql/).
// Location comes from Cloudflare's "Add visitor location headers" managed transform.
// Public pins are deliberately coarse:
//   - US only
//   - rounded to one decimal, about 11 km, before they are stored
//   - no counts
//   - refreshed once a day, so a pin can't be matched to the moment someone launched
const (
	sandboxPinsRefresh = 24 * time.Hour
	sandboxPinsRetry   = 5 * time.Minute

	sandboxSchema = `CREATE TABLE IF NOT EXISTS sandbox_launches (
		launched_at timestamptz NOT NULL DEFAULT now(),
		lat double precision NOT NULL,
		lon double precision NOT NULL
	)`
	sandboxPrune = `DELETE FROM sandbox_launches WHERE launched_at <= now() - interval '365 days'`
	sandboxPinsQ = `SELECT DISTINCT lat, lon FROM sandbox_launches WHERE launched_at > now() - interval '365 days'`
)

type sandboxPin struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type sandboxPins struct {
	bindingDir string

	poolMu sync.Mutex
	pool   *pgxpool.Pool

	mu   sync.RWMutex
	pins []sandboxPin
}

func newSandboxPins(bindingDir string) *sandboxPins {
	return &sandboxPins{bindingDir: bindingDir}
}

func (a *app) handleGuestLocations(w http.ResponseWriter, r *http.Request) {
	pins := a.sandboxPins.get()
	if pins == nil {
		http.Error(w, "locations unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=3600")
	writeJSON(w, pins)
}

// get never waits on the database, so the map costs a visitor nothing beyond a memory read.
func (s *sandboxPins) get() []sandboxPin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pins
}

// pinFromRequest is false for anything that isn't a US visitor with valid coordinates.
func pinFromRequest(r *http.Request) (sandboxPin, bool) {
	if strings.TrimSpace(r.Header.Get("Cf-Ipcountry")) != "US" {
		return sandboxPin{}, false
	}
	lat, latErr := strconv.ParseFloat(strings.TrimSpace(r.Header.Get("Cf-Iplatitude")), 64)
	lon, lonErr := strconv.ParseFloat(strings.TrimSpace(r.Header.Get("Cf-Iplongitude")), 64)
	if latErr != nil || lonErr != nil || math.Abs(lat) > 90 || math.Abs(lon) > 180 {
		return sandboxPin{}, false
	}
	return sandboxPin{Lat: math.Round(lat*10) / 10, Lon: math.Round(lon*10) / 10}, true
}

// record runs off the request path, so a slow or missing database never delays a sandbox launch.
func (s *sandboxPins) record(r *http.Request) {
	pin, ok := pinFromRequest(r)
	if !ok {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pool, err := s.connect(ctx)
		if err != nil {
			slog.Warn("sandbox pins: record", "err", err)
			return
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sandbox_launches (lat, lon) VALUES ($1, $2)`, pin.Lat, pin.Lon); err != nil {
			slog.Warn("sandbox pins: record", "err", err)
		}
	}()
}

// run refreshes the pins at startup and then daily, pruning launches older than a year.
// A failed refresh keeps the last good pins and tries again sooner.
func (s *sandboxPins) run(ctx context.Context) {
	for {
		wait := sandboxPinsRefresh
		if err := s.refresh(ctx); err != nil {
			slog.Warn("sandbox pins: refresh", "err", err)
			wait = sandboxPinsRetry
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (s *sandboxPins) refresh(ctx context.Context) error {
	pool, err := s.connect(ctx)
	if err != nil {
		return err
	}
	if _, err := pool.Exec(ctx, sandboxPrune); err != nil {
		return err
	}
	rows, err := pool.Query(ctx, sandboxPinsQ)
	if err != nil {
		return err
	}
	defer rows.Close()
	pins := []sandboxPin{}
	for rows.Next() {
		var p sandboxPin
		if err := rows.Scan(&p.Lat, &p.Lon); err != nil {
			return err
		}
		pins = append(pins, p)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	s.pins = pins
	s.mu.Unlock()
	return nil
}

// connect caches the pool after the first success and creates the table on that first connect.
// A failed connect is not cached, so a database that is still starting is retried next time.
func (s *sandboxPins) connect(ctx context.Context) (*pgxpool.Pool, error) {
	s.poolMu.Lock()
	defer s.poolMu.Unlock()
	if s.pool != nil {
		return s.pool, nil
	}
	read := func(key string) string {
		b, _ := os.ReadFile(filepath.Join(s.bindingDir, key))
		return strings.TrimSpace(string(b))
	}
	port := read("port")
	if port == "" {
		port = "5432"
	}
	dsn := (&url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(read("username"), read("password")),
		Host:     net.JoinHostPort(read("host"), port),
		Path:     "/" + read("database"),
		RawQuery: "sslmode=disable&connect_timeout=3",
	}).String()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if _, err := pool.Exec(ctx, sandboxSchema); err != nil {
		pool.Close()
		return nil, err
	}
	s.pool = pool
	return pool, nil
}
