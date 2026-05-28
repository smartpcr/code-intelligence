package main

// Stage 8.2 sanity tests for the [clean-code-refactor-planner]
// composition root. These tests intentionally do NOT spin up a
// real Postgres; they cover the env-validation surface and the
// /healthz handler. End-to-end coverage of the planner itself
// lives in `internal/refactor/`. The cmd-level tests focus on
// what `runPlanner` cannot: malformed env vars, missing keys,
// the opt-out branch.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
)

// -----------------------------------------------------------------------------
// parseTargetEnv
// -----------------------------------------------------------------------------

// TestParseTargetEnv_Happy confirms the canonical (repo_id, sha)
// pair round-trips.
func TestParseTargetEnv_Happy(t *testing.T) {
	want := uuid.Must(uuid.NewV4())
	t.Setenv(EnvRepoID, want.String())
	t.Setenv(EnvSHA, "deadbeef")
	gotRepo, gotSHA, err := parseTargetEnv()
	if err != nil {
		t.Fatalf("parseTargetEnv: %v", err)
	}
	if gotRepo != want {
		t.Errorf("repoID = %s, want %s", gotRepo, want)
	}
	if gotSHA != "deadbeef" {
		t.Errorf("sha = %q, want %q", gotSHA, "deadbeef")
	}
}

// TestParseTargetEnv_MissingRepoID confirms an empty repo_id is
// rejected with a clear error.
func TestParseTargetEnv_MissingRepoID(t *testing.T) {
	t.Setenv(EnvRepoID, "")
	t.Setenv(EnvSHA, "deadbeef")
	_, _, err := parseTargetEnv()
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), EnvRepoID) {
		t.Errorf("err = %v, missing %q in message", err, EnvRepoID)
	}
}

// TestParseTargetEnv_MalformedRepoID confirms a non-UUID
// repo_id is rejected.
func TestParseTargetEnv_MalformedRepoID(t *testing.T) {
	t.Setenv(EnvRepoID, "not-a-uuid")
	t.Setenv(EnvSHA, "deadbeef")
	_, _, err := parseTargetEnv()
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "not a UUID") {
		t.Errorf("err = %v, missing 'not a UUID' in message", err)
	}
}

// TestParseTargetEnv_ZeroRepoID confirms the all-zeros UUID is
// rejected -- a defensive guard against a misconfigured job
// that left the env unset and a parser that happens to accept
// "00000000-0000-0000-0000-000000000000".
func TestParseTargetEnv_ZeroRepoID(t *testing.T) {
	t.Setenv(EnvRepoID, uuid.Nil.String())
	t.Setenv(EnvSHA, "deadbeef")
	_, _, err := parseTargetEnv()
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "zero UUID") {
		t.Errorf("err = %v, missing 'zero UUID' in message", err)
	}
}

// TestParseTargetEnv_MissingSHA confirms an empty sha is
// rejected.
func TestParseTargetEnv_MissingSHA(t *testing.T) {
	t.Setenv(EnvRepoID, uuid.Must(uuid.NewV4()).String())
	t.Setenv(EnvSHA, "")
	_, _, err := parseTargetEnv()
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), EnvSHA) {
		t.Errorf("err = %v, missing %q in message", err, EnvSHA)
	}
}

// TestParseTargetEnv_TrimsWhitespace confirms accidental
// trailing/leading whitespace in the env value does not break
// the parser -- common YAML-pasting hazard.
func TestParseTargetEnv_TrimsWhitespace(t *testing.T) {
	want := uuid.Must(uuid.NewV4())
	t.Setenv(EnvRepoID, "  "+want.String()+"\n")
	t.Setenv(EnvSHA, "\tdeadbeef ")
	gotRepo, gotSHA, err := parseTargetEnv()
	if err != nil {
		t.Fatalf("parseTargetEnv: %v", err)
	}
	if gotRepo != want {
		t.Errorf("repoID = %s, want %s", gotRepo, want)
	}
	if gotSHA != "deadbeef" {
		t.Errorf("sha = %q, want %q", gotSHA, "deadbeef")
	}
}

// -----------------------------------------------------------------------------
// parseBoolEnv
// -----------------------------------------------------------------------------

// TestParseBoolEnv covers the truthy / falsy + whitespace
// matrix.
func TestParseBoolEnv(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"  ", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"on", true},
		{"false", false},
		{"0", false},
		{"no", false},
		{"garbage", false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q", c.in), func(t *testing.T) {
			if got := parseBoolEnv(c.in); got != c.want {
				t.Errorf("parseBoolEnv(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// buildMux
// -----------------------------------------------------------------------------

// TestBuildMux_HealthzOK confirms `/healthz` always returns 200
// even on opted-out deployments -- K8s liveness probes succeed.
func TestBuildMux_HealthzOK(t *testing.T) {
	mux := buildMux()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

// TestBuildMux_MetricsPlaceholderOK confirms the
// `/metrics` placeholder responds 200 so Prometheus scrapes do
// not flap on the unconfigured exporter.
func TestBuildMux_MetricsPlaceholderOK(t *testing.T) {
	mux := buildMux()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "clean-code-refactor-planner") {
		t.Errorf("body = %q, missing service name marker", w.Body.String())
	}
}

// TestBuildMux_UnknownPath404 confirms an unknown path returns
// 404 -- the mux does not over-respond.
func TestBuildMux_UnknownPath404(t *testing.T) {
	mux := buildMux()
	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}
