package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeGitHub int

func (code fakeGitHub) RoundTrip(*http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: int(code), Body: io.NopCloser(strings.NewReader("{}")), Header: http.Header{}}, nil
}

func TestMetrics_CountsGitHubAuthFailures(t *testing.T) {
	gh := newGithubClient("expired", "", "foo", "bar")
	gh.client.Transport = authFailureCounter{next: fakeGitHub(http.StatusUnauthorized), count: &gh.authFailures}
	a := &app{gh: gh}

	for range 2 {
		rec := httptest.NewRecorder()
		a.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 so the scrape still lands", rec.Code)
		}
	}

	rec := httptest.NewRecorder()
	a.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "launchpad_github_auth_failures_total 2\n") {
		t.Fatalf("body missing 2 auth failures:\n%s", rec.Body.String())
	}
}

func TestMetrics_IgnoresOtherGitHubErrors(t *testing.T) {
	gh := newGithubClient("ok", "", "foo", "bar")
	gh.client.Transport = authFailureCounter{next: fakeGitHub(http.StatusInternalServerError), count: &gh.authFailures}
	a := &app{gh: gh}

	rec := httptest.NewRecorder()
	a.handleMetrics(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "launchpad_github_auth_failures_total 0\n") {
		t.Fatalf("a 500 counted as an auth failure:\n%s", rec.Body.String())
	}
}
