package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// registerMandatoryPassing wires all three Stage 1.1 mandatory
// readiness gates (postgres, otel_exporter, signing_key_cache)
// with no-op success checks. Tests that want to exercise the
// "all gates green" path call this and then layer on a single
// failing / panicking / slow check on top.
func registerMandatoryPassing(h *Handler) {
	h.AddReadyCheck("postgres", func(_ context.Context) error { return nil })
	h.AddReadyCheck("otel_exporter", func(_ context.Context) error { return nil })
	h.AddReadyCheck("signing_key_cache", func(_ context.Context) error { return nil })
}

func TestHealthz_OK(t *testing.T) {
	t.Parallel()
	h := New("1.2.3", "deadbeef", "2026-01-01T00:00:00Z")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	h.Healthz(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", rr.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rr.Body.String(), err)
	}
	cases := map[string]string{
		"status":     "ok",
		"version":    "1.2.3",
		"commit":     "deadbeef",
		"build_time": "2026-01-01T00:00:00Z",
	}
	for k, want := range cases {
		if body[k] != want {
			t.Errorf("body[%q] = %q; want %q", k, body[k], want)
		}
	}
}

func TestHealthz_RejectsPOST(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	h.Healthz(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d; want 405", rr.Code)
	}
}

// TestReadyz_EmptyReturns503 is the load-bearing assertion for
// Stage 1.1 acceptance scenario "/readyz returns 503 unless all
// mandatory checks have registered": a freshly-booted process
// with no checks wired MUST NOT advertise readiness.
func TestReadyz_EmptyReturns503(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (no checks registered yet)", rr.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rr.Body.String(), err)
	}
	if body["status"] != "not_ready" {
		t.Errorf("status = %v; want not_ready", body["status"])
	}
}

// TestReadyz_DefaultMandatorySet asserts that the default
// mandatory-check set matches the implementation-plan.md
// Stage 1.1 contract verbatim: PostgreSQL pool, OTel exporter,
// signing-key cache.
func TestReadyz_DefaultMandatorySet(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	got := h.MandatoryChecks()
	want := []string{"otel_exporter", "postgres", "signing_key_cache"} // sorted
	if len(got) != len(want) {
		t.Fatalf("MandatoryChecks = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("MandatoryChecks[%d] = %q; want %q", i, got[i], want[i])
		}
	}
}

// TestReadyz_MissingMandatoryCheckReturns503 is the
// implementation-plan.md line-53 invariant: /readyz MUST NOT
// return 200 until every mandatory gate (postgres / OTel
// exporter / signing-key cache) has been wired AND is passing.
// Registering a single non-mandatory probe must NOT flip
// /readyz to 200 -- the prior iteration's "any one passing
// check" semantics regressed this contract.
func TestReadyz_MissingMandatoryCheckReturns503(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	// Register a passing non-mandatory probe so /readyz has
	// "something" registered, then assert it still 503s
	// because none of the three mandatory gates is wired.
	h.AddReadyCheck("some_subsystem", func(_ context.Context) error { return nil })

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (mandatory gates not registered)", rr.Code)
	}
	var body struct {
		Status string                 `json:"status"`
		Checks map[string]readyResult `json:"checks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rr.Body.String(), err)
	}
	for _, mand := range []string{"postgres", "otel_exporter", "signing_key_cache"} {
		got, ok := body.Checks[mand]
		if !ok {
			t.Errorf("mandatory check %s missing from response body", mand)
			continue
		}
		if got.Status != "not_ready" {
			t.Errorf("mandatory check %s status = %q; want not_ready", mand, got.Status)
		}
		if got.Reason != "not registered" {
			t.Errorf("mandatory check %s reason = %q; want %q", mand, got.Reason, "not registered")
		}
	}
}

// TestReadyz_PartialMandatoryReturns503 is the structural
// complement of TestReadyz_MissingMandatoryCheckReturns503:
// even with two of the three mandatory gates wired and
// passing, /readyz MUST stay 503 until the third arrives.
func TestReadyz_PartialMandatoryReturns503(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	h.AddReadyCheck("postgres", func(_ context.Context) error { return nil })
	h.AddReadyCheck("otel_exporter", func(_ context.Context) error { return nil })
	// signing_key_cache deliberately omitted

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (signing_key_cache missing)", rr.Code)
	}
}

func TestReadyz_AllMandatoryPassReturns200(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	registerMandatoryPassing(h)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200, body=%s", rr.Code, rr.Body.String())
	}
	var body struct {
		Status string                 `json:"status"`
		Checks map[string]readyResult `json:"checks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %q: %v", rr.Body.String(), err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %s; want ok", body.Status)
	}
	for name, res := range body.Checks {
		if res.Status != "ok" {
			t.Errorf("check %s = %+v; want ok", name, res)
		}
	}
}

func TestReadyz_OneMandatoryFailsReturns503(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	h.AddReadyCheck("postgres", func(_ context.Context) error { return nil })
	h.AddReadyCheck("otel_exporter", func(_ context.Context) error {
		return errors.New("dial tcp: connection refused")
	})
	h.AddReadyCheck("signing_key_cache", func(_ context.Context) error { return nil })

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503", rr.Code)
	}
	var body struct {
		Status string                 `json:"status"`
		Checks map[string]readyResult `json:"checks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Checks["otel_exporter"].Status != "not_ready" {
		t.Errorf("otel_exporter status = %s; want not_ready", body.Checks["otel_exporter"].Status)
	}
	if body.Checks["otel_exporter"].Reason == "" {
		t.Errorf("otel_exporter reason should carry the error string; got empty")
	}
	if body.Checks["postgres"].Status != "ok" {
		t.Errorf("postgres should still report ok; got %s", body.Checks["postgres"].Status)
	}
}

func TestReadyz_CheckPanicsReportsNotReady(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	h.AddReadyCheck("postgres", func(_ context.Context) error { return nil })
	h.AddReadyCheck("otel_exporter", func(_ context.Context) error { return nil })
	h.AddReadyCheck("signing_key_cache", func(_ context.Context) error {
		panic("kaboom")
	})

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (panicking check should not crash readyz)", rr.Code)
	}
}

// TestReadyz_HonoursTimeout asserts the HARD timeout invariant:
// a probe that ignores the context-cancellation signal MUST NOT
// wedge /readyz. The handler short-circuits on ctx.Done()
// without waiting for the stuck goroutine to return.
func TestReadyz_HonoursTimeout(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	h.SetTimeout(20 * time.Millisecond)
	// `slow` is intentionally non-context-aware: it sleeps a
	// full 5 seconds regardless of ctx, simulating a probe
	// that has a bug or hits a kernel-level block. The hard
	// timeout MUST abandon it.
	h.AddReadyCheck("slow", func(_ context.Context) error {
		time.Sleep(5 * time.Second)
		return nil
	})

	start := time.Now()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rr, req)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("readyz blocked %s; expected the hard timeout (20ms) to short-circuit", elapsed)
	}
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d; want 503 (timed-out check should be reported as not-ready)", rr.Code)
	}
	var body struct {
		Status string                 `json:"status"`
		Checks map[string]readyResult `json:"checks"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if body.Checks["slow"].Status != "not_ready" {
		t.Errorf("slow check status = %s; want not_ready", body.Checks["slow"].Status)
	}
	if body.Checks["slow"].Reason == "" {
		t.Errorf("slow check reason should describe the timeout; got empty")
	}
}

// TestReadyz_HungProbeDoesNotLeak asserts that abandoning a
// hung probe does NOT deadlock subsequent Readyz invocations.
// The goroutine launched against the prior request keeps
// running until its own sleep returns (or forever in a
// pathological case) but its eventual send into the buffered
// channel does not block the next request's collection loop.
func TestReadyz_HungProbeDoesNotBlockSubsequentRequests(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	h.SetTimeout(20 * time.Millisecond)
	h.AddReadyCheck("slow", func(_ context.Context) error {
		time.Sleep(2 * time.Second)
		return nil
	})

	for i := 0; i < 3; i++ {
		start := time.Now()
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		h.Readyz(rr, req)
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("request %d blocked %s; subsequent requests must not be wedged by prior hangs", i, elapsed)
		}
	}
}

func TestRoutes_MountsBoth(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	registerMandatoryPassing(h)
	mux := h.Routes()
	for _, path := range []string{"/healthz", "/readyz"} {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		mux.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s mounted but returned %d (want 200): %s", path, rr.Code, rr.Body.String())
		}
	}
}

func TestAddReadyCheck_EmptyNameNoop(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	h.AddReadyCheck("", func(_ context.Context) error { return nil })
	h.AddReadyCheck("real", nil)
	if got := h.ChecksRegistered(); len(got) != 0 {
		t.Errorf("ChecksRegistered = %v; want empty (both registrations should be rejected)", got)
	}
}

func TestRemoveReadyCheck(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	h.AddReadyCheck("postgres", func(_ context.Context) error { return nil })
	h.RemoveReadyCheck("postgres")
	if got := h.ChecksRegistered(); len(got) != 0 {
		t.Errorf("ChecksRegistered = %v; want empty after removal", got)
	}
}

// TestSetMandatoryChecks_Overrides verifies the test/extension
// hook: callers can replace the default mandatory set when
// later stages add new gates or when a focused test wants to
// exercise non-default behaviour.
func TestSetMandatoryChecks_Overrides(t *testing.T) {
	t.Parallel()
	h := New("v", "c", "b")
	h.SetMandatoryChecks([]string{"alpha", "beta"})
	got := h.MandatoryChecks()
	want := []string{"alpha", "beta"}
	if len(got) != len(want) {
		t.Fatalf("MandatoryChecks = %v; want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("MandatoryChecks[%d] = %q; want %q", i, got[i], want[i])
		}
	}
	// Registering the new mandatory pair (no defaults) and
	// having them pass must flip /readyz to 200.
	h.AddReadyCheck("alpha", func(_ context.Context) error { return nil })
	h.AddReadyCheck("beta", func(_ context.Context) error { return nil })
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	h.Readyz(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 after custom mandatory set passes", rr.Code)
	}
}
