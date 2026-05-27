package insights

// Stage 7.3 freshness_test.go covers the percentile-staleness
// projection the Management read verbs `mgmt.read.cross_repo`
// and `mgmt.read.portfolio` attach to their response envelopes
// (Stage 6.3). Three normative scenarios land here:
//
//   * `fresh-percentile-no-banner` (impl-plan Stage 7.3): a
//     `built_at` within the window returns `Degraded=false`
//     and an empty reason.
//   * `stale-percentile-banner-on-insights`: a `built_at`
//     past the window returns
//     `Degraded=true, Reason=DegradedReasonPercentileStale`.
//   * `gate-never-emits-percentile-stale`: the
//     `percentile_stale` reason string is INSIGHTS-ONLY --
//     the constant is pinned here so the Stage 6.1
//     eval.gate-side validator can compare against it
//     verbatim (verified at the type level rather than
//     reaching into the gate package).
//
// All tests inject a fake clock so a fixture's `built_at`
// can be classified deterministically without sleeping for an
// hour.

import (
	"testing"
	"time"
)

// fakeClock returns a constant time. Pinned-instant testing
// is the only way to make freshness comparisons deterministic
// without `time.Sleep` -- a real-clock test would drift across
// CI runs.
type fakeClock struct{ t time.Time }

func (f fakeClock) Now() time.Time { return f.t }

// newFreshnessAt builds a [Freshness] with the canonical
// production window and a fixed clock set to `now`. Centralised
// here so each test does not repeat the same constructor.
func newFreshnessAt(now time.Time) *Freshness {
	return &Freshness{
		Window: time.Duration(FreshnessWindowSeconds) * time.Second,
		Clock:  fakeClock{now},
	}
}

// TestFreshness_FreshSnapshotReturnsNoBanner pins impl-plan
// Stage 7.3 scenario `fresh-percentile-no-banner`. A
// `built_at` ten minutes before `now` is well within the
// 3600s window -- Status MUST be `Degraded=false` with an
// empty `Reason` so the `mgmt.read.*` envelope encodes no
// `degraded_reason` field on the wire.
func TestFreshness_FreshSnapshotReturnsNoBanner(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	f := newFreshnessAt(now)
	builtAt := now.Add(-10 * time.Minute)

	got := f.Evaluate(builtAt)
	if got.Degraded {
		t.Errorf("Degraded=true for built_at=%v (10min ago); want false", builtAt)
	}
	if got.Reason != "" {
		t.Errorf("Reason=%q for fresh row; want empty", got.Reason)
	}
	if !got.BuiltAt.Equal(builtAt) {
		t.Errorf("BuiltAt=%v, want %v echoed", got.BuiltAt, builtAt)
	}
	if !got.EvaluatedAt.Equal(now) {
		t.Errorf("EvaluatedAt=%v, want %v from fake clock", got.EvaluatedAt, now)
	}
}

// TestFreshness_StaleSnapshotReturnsPercentileStaleBanner pins
// impl-plan Stage 7.3 scenario
// `stale-percentile-banner-on-insights`. A `built_at` two
// hours before `now` is past the 3600s window -- Status MUST
// be `Degraded=true, Reason=DegradedReasonPercentileStale`.
func TestFreshness_StaleSnapshotReturnsPercentileStaleBanner(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	f := newFreshnessAt(now)
	builtAt := now.Add(-2 * time.Hour)

	got := f.Evaluate(builtAt)
	if !got.Degraded {
		t.Errorf("Degraded=false for built_at=%v (2h ago); want true", builtAt)
	}
	if got.Reason != DegradedReasonPercentileStale {
		t.Errorf("Reason=%q, want %q", got.Reason, DegradedReasonPercentileStale)
	}
}

// TestFreshness_BoundaryAtExactWindowIsFresh asserts the
// inclusive-window contract documented on [Freshness.Window]:
// a row whose age EQUALS the window is treated as FRESH so a
// snapshot that just landed on the threshold isn't
// prematurely flagged. The strict inequality `age > Window`
// is what the implementation uses.
func TestFreshness_BoundaryAtExactWindowIsFresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	f := newFreshnessAt(now)
	// Exactly at the threshold -- 3600 seconds.
	builtAt := now.Add(-time.Duration(FreshnessWindowSeconds) * time.Second)

	got := f.Evaluate(builtAt)
	if got.Degraded {
		t.Errorf("Degraded=true at boundary age==Window; want false (inclusive window)")
	}
}

// TestFreshness_OneSecondPastBoundaryIsStale complements the
// boundary test: a row one second past the threshold MUST be
// classified stale.
func TestFreshness_OneSecondPastBoundaryIsStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	f := newFreshnessAt(now)
	builtAt := now.Add(-time.Duration(FreshnessWindowSeconds+1) * time.Second)

	got := f.Evaluate(builtAt)
	if !got.Degraded {
		t.Errorf("Degraded=false 1s past window; want true")
	}
	if got.Reason != DegradedReasonPercentileStale {
		t.Errorf("Reason=%q, want %q", got.Reason, DegradedReasonPercentileStale)
	}
}

// TestFreshness_ZeroBuiltAtTreatedAsStale pins the empty-input
// contract documented on [Freshness.Evaluate]: a zero
// `time.Time` is the "no row" sentinel some backends return
// when the table is empty; we treat it as STALE so an
// unpopulated dashboard does not silently render misleading
// "fresh" metrics.
func TestFreshness_ZeroBuiltAtTreatedAsStale(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	f := newFreshnessAt(now)

	got := f.Evaluate(time.Time{})
	if !got.Degraded {
		t.Errorf("Degraded=false for zero built_at; want true")
	}
	if got.Reason != DegradedReasonPercentileStale {
		t.Errorf("Reason=%q, want %q", got.Reason, DegradedReasonPercentileStale)
	}
}

// TestFreshness_FutureBuiltAtTreatedAsFresh pins the
// clock-skew note on [Freshness.Evaluate]: a `built_at` in
// the future (writer clock ahead of reader clock) MUST NOT be
// flagged stale -- the resulting negative age is never `>
// Window`. The Insights surface does not police clock drift.
func TestFreshness_FutureBuiltAtTreatedAsFresh(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	f := newFreshnessAt(now)
	builtAt := now.Add(10 * time.Minute) // future

	got := f.Evaluate(builtAt)
	if got.Degraded {
		t.Error("Degraded=true for future built_at; want false (clock-skew tolerant)")
	}
}

// TestFreshness_NilClockFallsBackToSystem documents the
// nil-clock safety contract on [Freshness.now]: a [Freshness]
// constructed with a nil [Clock] does NOT panic on Evaluate;
// it falls back to [SystemClock] so the hot read path stays
// crash-free even under a wiring bug.
func TestFreshness_NilClockFallsBackToSystem(t *testing.T) {
	t.Parallel()
	f := &Freshness{
		Window: time.Duration(FreshnessWindowSeconds) * time.Second,
		Clock:  nil, // wiring bug, must not panic
	}
	// A built_at one second ago is fresh under any sane
	// system clock -- this asserts that the fall-back path
	// produces a Status without panicking.
	got := f.Evaluate(time.Now().Add(-time.Second))
	if got.Degraded {
		t.Error("Degraded=true with nil clock + fresh built_at; want false")
	}
}

// TestNewPercentileFreshness_WiresProductionDefaults pins the
// canonical composition-root constructor: window =
// FreshnessWindowSeconds, clock = SystemClock. A drift here
// would break the architecture Sec 8.2 freshness contract.
func TestNewPercentileFreshness_WiresProductionDefaults(t *testing.T) {
	t.Parallel()
	f := NewPercentileFreshness()
	if f == nil {
		t.Fatal("NewPercentileFreshness returned nil")
	}
	want := time.Duration(FreshnessWindowSeconds) * time.Second
	if f.Window != want {
		t.Errorf("Window=%v, want %v", f.Window, want)
	}
	if _, ok := f.Clock.(SystemClock); !ok {
		t.Errorf("Clock=%T, want SystemClock", f.Clock)
	}
}

// TestDegradedReasonPercentileStaleLiteral pins the
// architecture Sec 8.2 + tech-spec C17 / C21 canonical
// degraded-reason string. The eval.gate validator
// (Stage 6.1) reads this constant via fixed-string
// comparison; a typo here would silently break the
// `percentile-stale-not-on-gate` carve-out. Locking the
// literal at the type level catches the drift at compile +
// test time.
func TestDegradedReasonPercentileStaleLiteral(t *testing.T) {
	t.Parallel()
	if DegradedReasonPercentileStale != "percentile_stale" {
		t.Errorf("DegradedReasonPercentileStale=%q, want %q",
			DegradedReasonPercentileStale, "percentile_stale")
	}
}

// TestFreshnessWindowSecondsLiteral pins the tech-spec Sec 8.2
// `freshness_window_seconds=3600` parameter -- changing it
// requires a tech-spec amendment, so the literal is locked
// here.
func TestFreshnessWindowSecondsLiteral(t *testing.T) {
	t.Parallel()
	if FreshnessWindowSeconds != 3600 {
		t.Errorf("FreshnessWindowSeconds=%d, want 3600", FreshnessWindowSeconds)
	}
}

// TestSystemClock_NowIsRecent provides a smoke test that
// [SystemClock.Now] is not stuck at a hardcoded value. Loose
// bound (one minute) so the test is robust on slow CI
// runners.
func TestSystemClock_NowIsRecent(t *testing.T) {
	t.Parallel()
	before := time.Now()
	got := SystemClock{}.Now()
	after := time.Now()
	if got.Before(before) || got.After(after.Add(time.Minute)) {
		t.Errorf("SystemClock.Now=%v not within [%v, %v+1min]", got, before, after)
	}
}
