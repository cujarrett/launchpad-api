package main

import (
	"net/http/httptest"
	"testing"
)

func TestPinFromRequest(t *testing.T) {
	cases := []struct {
		name               string
		country, lat, long string
		want               sandboxPin
		ok                 bool
	}{
		{"us rounds to one decimal", "US", "30.26715", "-97.74306", sandboxPin{30.3, -97.7}, true},
		{"outside the us", "DE", "52.52", "13.40", sandboxPin{}, false},
		{"no country", "", "30.2", "-97.7", sandboxPin{}, false},
		{"bad latitude", "US", "95", "-97.7", sandboxPin{}, false},
		{"not a number", "US", "NaN", "abc", sandboxPin{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/guest/workspaces", nil)
			r.Header.Set("CF-IPCountry", c.country)
			r.Header.Set("cf-iplatitude", c.lat)
			r.Header.Set("cf-iplongitude", c.long)
			got, ok := pinFromRequest(r)
			if got != c.want || ok != c.ok {
				t.Errorf("got %v, %v; want %v, %v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestSandboxPins_NilUntilFirstRefresh(t *testing.T) {
	if newSandboxPins(t.TempDir()).get() != nil {
		t.Error("pins before the first refresh should be nil, so the map stays hidden")
	}
}
