package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Pins for the public sandbox map, read from Loki.
// Alloy labels each sandbox launch in Traefik's access log as log_type="sandbox".
// Location comes from Cloudflare's "Add visitor location headers" managed transform.
// Public pins are deliberately coarse:
//   - US only, because Alloy keeps only US visitors
//   - rounded to one decimal, about 11 km
//   - no counts
const (
	sandboxPinsRefresh = 5 * time.Minute
	sandboxPinsRetry   = time.Minute

	// Fixed text with no request input, so a visitor never chooses what Loki is asked.
	sandboxPinsQuery = `sum by (request_Cf_Iplatitude, request_Cf_Iplongitude) (count_over_time({log_type="sandbox"} | json | request_Cf_Iplatitude!="" [365d]))`
)

type sandboxPin struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type sandboxPins struct {
	lokiURL string
	client  *http.Client

	mu   sync.RWMutex
	pins []sandboxPin
}

func newSandboxPins(lokiURL string) *sandboxPins {
	return &sandboxPins{lokiURL: lokiURL, client: &http.Client{Timeout: 20 * time.Second}}
}

func (a *app) handleGuestLocations(w http.ResponseWriter, r *http.Request) {
	pins := a.sandboxPins.get()
	if pins == nil {
		http.Error(w, "locations unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, pins)
}

// get never waits on Loki, so the map costs a visitor nothing beyond a memory read.
func (s *sandboxPins) get() []sandboxPin {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pins
}

// run refreshes the pins at startup and then every few minutes.
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
	target := s.lokiURL + "/loki/api/v1/query?" + url.Values{"query": {sandboxPinsQuery}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("loki returned %d", resp.StatusCode)
	}

	var body struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return err
	}

	// Rounding puts nearby launches on the same pin, so the set drops the repeats.
	seen := map[sandboxPin]bool{}
	for _, series := range body.Data.Result {
		if pin, ok := roundedPin(series.Metric["request_Cf_Iplatitude"], series.Metric["request_Cf_Iplongitude"]); ok {
			seen[pin] = true
		}
	}
	pins := make([]sandboxPin, 0, len(seen))
	for pin := range seen {
		pins = append(pins, pin)
	}
	sort.Slice(pins, func(i, j int) bool {
		if pins[i].Lat != pins[j].Lat {
			return pins[i].Lat < pins[j].Lat
		}
		return pins[i].Lon < pins[j].Lon
	})

	s.mu.Lock()
	s.pins = pins
	s.mu.Unlock()
	return nil
}

// roundedPin is false for anything that isn't a valid coordinate pair.
func roundedPin(latText, lonText string) (sandboxPin, bool) {
	lat, latErr := strconv.ParseFloat(latText, 64)
	lon, lonErr := strconv.ParseFloat(lonText, 64)
	// The negated form also rejects NaN, which fails every comparison.
	if latErr != nil || lonErr != nil || !(math.Abs(lat) <= 90) || !(math.Abs(lon) <= 180) {
		return sandboxPin{}, false
	}
	return sandboxPin{Lat: math.Round(lat*10) / 10, Lon: math.Round(lon*10) / 10}, true
}
