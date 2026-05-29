package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBuildMux_HealthzOK confirms the always-on /healthz route
// returns 200 with a stable body so Kubernetes liveness probes
// pass against the stub binary even before any indexer subsystem
// is wired up. Mirrors the contract pinned for the other
// composition roots (gateway, aggregator, eval-gate,
// metric-ingestor, refactor-planner).
func TestBuildMux_HealthzOK(t *testing.T) {
	mux := buildMux()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz: code=%d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("/healthz: body=%q, want substring \"ok\"", rec.Body.String())
	}
}

// TestBuildMux_MetricsPlaceholderResponds is the Stage 9.4
// iter-3 follow-up contract: the indexer mounts a /metrics
// route so its pod participates in the Prometheus scrape
// pipeline alongside its peers. The placeholder body keeps the
// route honest (no metric lines yet) without breaking the
// contract.
func TestBuildMux_MetricsPlaceholderResponds(t *testing.T) {
	mux := buildMux()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics: code=%d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "placeholder") {
		t.Fatalf("/metrics: body=%q, want substring \"placeholder\"", rec.Body.String())
	}
}

// TestLookupEnvOrDefault_PreservesUnsetVsExplicitlyEmpty pins
// the iter-2 evaluator feedback #2 contract on the indexer's
// helper: UNSET falls back to the default, EXPLICITLY EMPTY
// returns "" (the canonical "telemetry disabled" sentinel).
// Same contract as the helper in every other composition root.
func TestLookupEnvOrDefault_PreservesUnsetVsExplicitlyEmpty(t *testing.T) {
	const name = "CLEAN_CODE_INDEXER_TEST_VAR_THAT_DOES_NOT_EXIST"
	t.Setenv(name, "")
	if got := lookupEnvOrDefault(name, "fallback"); got != "" {
		t.Errorf("explicitly-empty: got %q; want \"\" (the disable sentinel)", got)
	}

	const missingName = "CLEAN_CODE_INDEXER_TEST_VAR_NEVER_SET"
	if got := lookupEnvOrDefault(missingName, "fallback"); got != "fallback" {
		t.Errorf("unset: got %q; want \"fallback\"", got)
	}
}
