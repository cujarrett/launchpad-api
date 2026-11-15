package main

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type fakeDir string

func (body fakeDir) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: http.Header{}}, nil
}

func TestGuestReady_RecordsClickToDone(t *testing.T) {
	cases := map[string]struct {
		dir  string
		want string
	}{
		"api":      {`[{"name":"foo-api.yaml"},{"name":"foo-spa.yaml"}]`, `launchpad_guest_ready_seconds{workspace="guest-foo",demo="api"} 42` + "\n"},
		"workflow": {`[{"name":"foo-api.yaml"},{"name":"foo-workflow-space.yaml"}]`, `launchpad_guest_ready_seconds{workspace="guest-foo",demo="workflow"} 42` + "\n"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			gh := newGithubClient("ok", "", "foo", "bar")
			gh.client.Transport = fakeDir(tc.dir)
			a := &app{gh: gh}

			a.recordGuestReady(context.Background(), "guest-foo", "2026-11-13T20:00:00Z", "2026-11-13T20:00:42Z")

			var out strings.Builder
			a.writeGuestReady(&out)
			if !strings.Contains(out.String(), tc.want) {
				t.Fatalf("missing %q in:\n%s", tc.want, out.String())
			}
		})
	}
}

func TestGuestReady_SkipsMissingOrBackwardTimes(t *testing.T) {
	gh := newGithubClient("ok", "", "foo", "bar")
	gh.client.Transport = fakeDir(`[]`)
	a := &app{gh: gh}

	a.recordGuestReady(context.Background(), "guest-foo", "", "2026-11-13T20:00:42Z")
	a.recordGuestReady(context.Background(), "guest-bar", "2026-11-13T20:00:42Z", "2026-11-13T20:00:00Z")

	var out strings.Builder
	a.writeGuestReady(&out)
	if strings.Contains(out.String(), "workspace=") {
		t.Fatalf("recorded a bad run:\n%s", out.String())
	}
}
