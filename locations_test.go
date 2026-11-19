package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func lokiResult(series string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` + series + `]}}`
}

func lokiSeries(lat, lon string) string {
	return `{"metric":{"request_Cf_Iplatitude":"` + lat + `","request_Cf_Iplongitude":"` + lon + `"},"value":[1,"1"]}`
}

func TestRoundedPin(t *testing.T) {
	cases := []struct {
		name     string
		lat, lon string
		want     sandboxPin
		ok       bool
	}{
		{"rounds to one decimal", "30.26715", "-97.74306", sandboxPin{30.3, -97.7}, true},
		{"bad latitude", "95", "-97.7", sandboxPin{}, false},
		{"bad longitude", "30.2", "181", sandboxPin{}, false},
		{"not a number", "NaN", "-97.7", sandboxPin{}, false},
		{"empty", "", "", sandboxPin{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := roundedPin(c.lat, c.lon)
			if got != c.want || ok != c.ok {
				t.Errorf("got %v, %v; want %v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestSandboxPins_RefreshRoundsAndDropsRepeats(t *testing.T) {
	var gotPath, gotQuery string
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("query")
		_, _ = w.Write([]byte(lokiResult(
			lokiSeries("41.88", "-87.63") + "," +
				lokiSeries("41.91", "-87.61") + "," +
				lokiSeries("30.26715", "-97.74306") + "," +
				lokiSeries("abc", "-97.7"),
		)))
	}))
	defer loki.Close()

	s := newSandboxPins(loki.URL)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	want := []sandboxPin{{30.3, -97.7}, {41.9, -87.6}}
	if got := s.get(); !reflect.DeepEqual(got, want) {
		t.Errorf("pins = %v, want %v", got, want)
	}
	if gotPath != "/loki/api/v1/query" || gotQuery != sandboxPinsQuery {
		t.Errorf("asked Loki %s with %q", gotPath, gotQuery)
	}
}

func TestSandboxPins_FailedRefreshKeepsLastPins(t *testing.T) {
	healthy := true
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(lokiResult(lokiSeries("41.88", "-87.63"))))
	}))
	defer loki.Close()

	s := newSandboxPins(loki.URL)
	if err := s.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	healthy = false
	if err := s.refresh(context.Background()); err == nil {
		t.Fatal("a 500 from Loki should be an error")
	}
	if got := s.get(); len(got) != 1 {
		t.Errorf("pins = %v, want the last good pin kept", got)
	}
}

func TestGuestLocations_UnavailableUntilFirstRefresh(t *testing.T) {
	a := &app{sandboxPins: newSandboxPins("http://loki.invalid")}
	rec := httptest.NewRecorder()
	a.handleGuestLocations(rec, httptest.NewRequest(http.MethodGet, "/api/guest/locations", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 so the map stays hidden", rec.Code)
	}
}

func TestGuestLocations_NoLaunchesIsAnEmptyList(t *testing.T) {
	loki := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(lokiResult("")))
	}))
	defer loki.Close()

	a := &app{sandboxPins: newSandboxPins(loki.URL)}
	if err := a.sandboxPins.refresh(context.Background()); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	rec := httptest.NewRecorder()
	a.handleGuestLocations(rec, httptest.NewRequest(http.MethodGet, "/api/guest/locations", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "[]\n" {
		t.Errorf("got %d %q, want 200 []", rec.Code, rec.Body.String())
	}
}
