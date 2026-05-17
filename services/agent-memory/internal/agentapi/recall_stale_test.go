package agentapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"
)

// staleness_test.go validates the Stage 6.4 step-5 contract:
//
//   "Mark recall responses degraded_reason='reranker_model_stale'
//    when the latest reranker_model row is > 7 days old."
//
// The priority order documented on applyRerankerStaleness:
//   1. Pre-existing Degraded=true (from populateDegraded via the
//      HealthSource) WINS over a "merely stale" reranker.
//   2. Healthy + stale -> downgrade to reranker_model_stale.
//   3. Freshness source error -> silent (no flip to degraded).
//   4. Freshness source missing/unwired -> silent.
//   5. Healthy + fresh -> no change.

func staleSvc(freshness RerankerFreshnessSource) *Service {
	return &Service{
		logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		rerankerFreshness: freshness,
	}
}

// TestStaleness_FreshnessNil_NoOp: an unwired freshness source
// MUST NOT degrade an otherwise-healthy response.
func TestStaleness_FreshnessNil_NoOp(t *testing.T) {
	svc := staleSvc(nil)
	resp := RecallResponse{}
	svc.applyRerankerStaleness(context.Background(), &resp)
	if resp.Degraded || resp.DegradedReason != "" {
		t.Fatalf("nil freshness source should be a no-op; got Degraded=%v Reason=%q",
			resp.Degraded, resp.DegradedReason)
	}
}

// TestStaleness_FreshnessNoRow_NoOp: a freshness source that
// returns (_, false, nil) means "no published reranker_model
// yet". We CANNOT prove stale without a baseline, so the
// response is left as-is. This is the dev/cold-start contract.
func TestStaleness_FreshnessNoRow_NoOp(t *testing.T) {
	svc := staleSvc(RerankerFreshnessFunc(func(_ context.Context) (time.Time, bool, error) {
		return time.Time{}, false, nil
	}))
	resp := RecallResponse{}
	svc.applyRerankerStaleness(context.Background(), &resp)
	if resp.Degraded {
		t.Fatalf("no-row freshness should not flip Degraded; got %v %q",
			resp.Degraded, resp.DegradedReason)
	}
}

// TestStaleness_FreshnessError_NoOp: a freshness lookup that
// itself fails MUST NOT flip a healthy response to degraded.
// Per rubber-duck #8 on the summarize side: a degraded reason
// source going down must not change the verb's classifier
// behaviour.
func TestStaleness_FreshnessError_NoOp(t *testing.T) {
	svc := staleSvc(RerankerFreshnessFunc(func(_ context.Context) (time.Time, bool, error) {
		return time.Time{}, false, errors.New("connection refused")
	}))
	resp := RecallResponse{}
	svc.applyRerankerStaleness(context.Background(), &resp)
	if resp.Degraded {
		t.Fatalf("freshness lookup error should not flip Degraded; got %v %q",
			resp.Degraded, resp.DegradedReason)
	}
}

// TestStaleness_HealthyAndFresh_NoOp: a fresh model (trained
// within rerankerStaleAfter window) leaves the response
// untouched.
func TestStaleness_HealthyAndFresh_NoOp(t *testing.T) {
	fresh := time.Now().Add(-1 * 24 * time.Hour) // 1 day ago
	svc := staleSvc(RerankerFreshnessFunc(func(_ context.Context) (time.Time, bool, error) {
		return fresh, true, nil
	}))
	resp := RecallResponse{}
	svc.applyRerankerStaleness(context.Background(), &resp)
	if resp.Degraded {
		t.Fatalf("fresh model (1d old) should not flip Degraded; got %v %q",
			resp.Degraded, resp.DegradedReason)
	}
}

// TestStaleness_HealthyAndStale_FlipsToStale: a > 7-day-old
// trained_at flips a healthy response to
// `degraded_reason=reranker_model_stale`. This is the §6.4
// step-5 happy-path assertion.
func TestStaleness_HealthyAndStale_FlipsToStale(t *testing.T) {
	stale := time.Now().Add(-8 * 24 * time.Hour) // 8 days ago
	svc := staleSvc(RerankerFreshnessFunc(func(_ context.Context) (time.Time, bool, error) {
		return stale, true, nil
	}))
	resp := RecallResponse{}
	svc.applyRerankerStaleness(context.Background(), &resp)
	if !resp.Degraded {
		t.Fatalf("8-day-old model should flip Degraded=true; got %v %q",
			resp.Degraded, resp.DegradedReason)
	}
	if resp.DegradedReason != DegradedReasonRerankerModelStale {
		t.Fatalf("Reason: got %q want %q",
			resp.DegradedReason, DegradedReasonRerankerModelStale)
	}
}

// TestStaleness_HardOutageWinsOverStale: a pre-existing
// degraded reason set by populateDegraded (e.g.
// `episodic_log_unavailable` or another hard outage) MUST
// NOT be overwritten by a "merely stale" reranker. The
// upstream signal carries more actionable information.
func TestStaleness_HardOutageWinsOverStale(t *testing.T) {
	stale := time.Now().Add(-8 * 24 * time.Hour)
	svc := staleSvc(RerankerFreshnessFunc(func(_ context.Context) (time.Time, bool, error) {
		return stale, true, nil
	}))
	// Simulate populateDegraded having already set a hard
	// outage reason.
	resp := RecallResponse{
		Degraded:       true,
		DegradedReason: "episodic_log_unavailable",
	}
	svc.applyRerankerStaleness(context.Background(), &resp)

	if !resp.Degraded {
		t.Fatalf("Degraded must remain true; got %v", resp.Degraded)
	}
	if resp.DegradedReason != "episodic_log_unavailable" {
		t.Fatalf("hard outage reason was overwritten by staleness path: got %q want %q",
			resp.DegradedReason, "episodic_log_unavailable")
	}
}

// TestStaleness_BoundaryExactlySevenDays_NotStale: a
// trained_at exactly at the 7-day boundary is treated as
// fresh (the implementation uses `<=` so the equal-to point
// is "still fresh"). This guards against off-by-one
// regressions on the boundary.
func TestStaleness_BoundaryExactlySevenDays_NotStale(t *testing.T) {
	// Pin slightly inside the boundary so a sub-microsecond
	// timer drift between the freshness "now" and
	// time.Since() in the package does not cause the test to
	// flicker.
	atBoundary := time.Now().Add(-7*24*time.Hour + time.Second)
	svc := staleSvc(RerankerFreshnessFunc(func(_ context.Context) (time.Time, bool, error) {
		return atBoundary, true, nil
	}))
	resp := RecallResponse{}
	svc.applyRerankerStaleness(context.Background(), &resp)
	if resp.Degraded {
		t.Fatalf("trained_at at the 7d boundary should be FRESH; got %v %q",
			resp.Degraded, resp.DegradedReason)
	}
}
