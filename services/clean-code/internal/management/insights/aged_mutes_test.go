package insights

// Stage 10.2 aged_mutes_test.go pins the two impl-plan
// scenarios verbatim:
//
//   * `aged-mute-listed-not-enforced` -- a mute(created_at=now-100d)
//     appears in the report under the default 90-day threshold,
//     AND the helper exposes no enforcement side effect (the
//     in-memory backend's row is untouched after the read).
//
//   * `unmute-removes-from-report` -- after appending an
//     override(mute=false) row for the same (rule, scope) tuple,
//     the next [AgedMutes.Report] call omits the pair.
//
// Coverage also locks the canonical defaults, the
// threshold-override path, deterministic sort order, tie-break
// on `(created_at, override_id)`, the nil-clock fallback, the
// nil-reader sentinel, the cancellation-propagation contract,
// and the `threshold<=0 -> default` guard against missing
// `threshold_days` query arguments accidentally surfacing every
// mute.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

// listAllOverridesFunc adapts a closure into an
// [OverrideReader] so tests can build a one-shot fake without
// declaring a new struct per scenario.
type listAllOverridesFunc func(ctx context.Context) ([]OverrideRecord, error)

func (f listAllOverridesFunc) ListAllOverrides(ctx context.Context) ([]OverrideRecord, error) {
	return f(ctx)
}

// sliceReader wires a fixed slice of [OverrideRecord]s as an
// [OverrideReader]. Mutating the slice between calls models
// the "operator appends a mute=false row" path so the
// `unmute-removes-from-report` scenario can read the report
// twice with different backend state.
type sliceReader struct {
	rows []OverrideRecord
	// calls counts how many times ListAllOverrides was
	// invoked -- the aged-mute tests assert the Insights
	// projection does not double-read the backend per call.
	calls int
}

func (s *sliceReader) ListAllOverrides(ctx context.Context) ([]OverrideRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.calls++
	// Return a copy so the test mutating `s.rows` between
	// calls cannot also corrupt the slice the report's
	// reducer is iterating.
	out := make([]OverrideRecord, len(s.rows))
	copy(out, s.rows)
	return out, nil
}

// fixedClock is a deterministic [Clock] -- identical to
// [fakeClock] in freshness_test.go; duplicated here to keep
// each test file self-contained.
type fixedClock struct{ t time.Time }

func (f fixedClock) Now() time.Time { return f.t }

// newAt builds a sample override record with the given
// (rule_id, repo_id, scope_kind, scope_signature_glob,
// mute, created_at, override_id). Centralised so each
// scenario reads as data, not boilerplate.
func newAt(ruleID, repoID, scopeKind, glob string, mute bool, createdAt time.Time, overrideID string) OverrideRecord {
	return OverrideRecord{
		OverrideID: overrideID,
		RuleID:     ruleID,
		Scope: OverrideScope{
			RepoID:             repoID,
			ScopeKind:          scopeKind,
			ScopeSignatureGlob: glob,
		},
		Mute:      mute,
		Reason:    "noisy in v1; revisit Q3",
		ActorID:   "operator-test@example.com",
		CreatedAt: createdAt,
	}
}

// TestAgedMutes_AgedMuteListedNotEnforced pins impl-plan
// Stage 10.2 scenario `aged-mute-listed-not-enforced`: a
// mute(created_at = now - 100d) under the default 90-day
// threshold MUST appear in the report (age > threshold) AND
// the backend row MUST remain unmodified after the read (no
// enforcement -- iter 1 evaluator item 5).
func TestAgedMutes_AgedMuteListedNotEnforced(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	mute := newAt(
		"solid.srp.lcom4_high",
		"00000000-0000-0000-0000-000000000001",
		"class",
		"com.example.legacy.OrderProcessor",
		true,
		now.Add(-100*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
	)
	backend := &sliceReader{rows: []OverrideRecord{mute}}
	am := &AgedMutes{
		Reader:    backend,
		Clock:     fixedClock{now},
		Threshold: time.Duration(AgedMuteDefaultThresholdDays) * 24 * time.Hour,
	}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(report)=%d, want 1 (the 100-day mute should be listed)", len(got))
	}
	if got[0].OverrideID != mute.OverrideID {
		t.Errorf("OverrideID=%q, want %q", got[0].OverrideID, mute.OverrideID)
	}
	if got[0].AgeDays != 100 {
		t.Errorf("AgeDays=%d, want 100", got[0].AgeDays)
	}
	if got[0].RuleID != mute.RuleID {
		t.Errorf("RuleID=%q, want %q", got[0].RuleID, mute.RuleID)
	}
	if got[0].Scope != mute.Scope {
		t.Errorf("Scope=%+v, want %+v", got[0].Scope, mute.Scope)
	}
	if got[0].Reason != mute.Reason {
		t.Errorf("Reason=%q, want %q", got[0].Reason, mute.Reason)
	}
	if got[0].ActorID != mute.ActorID {
		t.Errorf("ActorID=%q, want %q", got[0].ActorID, mute.ActorID)
	}
	// No enforcement -- the row in the backend MUST remain
	// untouched. The aged-mute report is a READ surface; if
	// a future regression bolted on an "auto-expire" path it
	// would either mutate the backend or invoke a writer
	// callback. Neither happens.
	if len(backend.rows) != 1 {
		t.Errorf("len(backend.rows)=%d, want 1 (Report MUST NOT mutate the backend)", len(backend.rows))
	}
	if !reflect.DeepEqual(backend.rows[0], mute) {
		t.Errorf("backend.rows[0]=%+v, want %+v (Report MUST NOT mutate the row)", backend.rows[0], mute)
	}
}

// TestAgedMutes_UnmuteRemovesFromReport pins impl-plan
// Stage 10.2 scenario `unmute-removes-from-report`: after
// appending an override(mute=false) row for the same
// (rule_id, scope) tuple, the next [AgedMutes.Report] call
// omits the pair.
//
// The fixture sequence:
//
//	t0 (100d ago): mute=true   -- aged
//	t1 (1 hour ago): mute=false -- the operator's unmute
//
// First call: backend has only the mute -> 1 entry.
// Operator appends the unmute via mgmt.override (simulated
// here by appending to the backend slice).
// Second call: backend has both rows -> 0 entries (the
// latest-row-wins reduction picks the mute=false row).
func TestAgedMutes_UnmuteRemovesFromReport(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	mute := newAt(
		"solid.srp.lcom4_high",
		"00000000-0000-0000-0000-000000000001",
		"class",
		"com.example.legacy.OrderProcessor",
		true,
		now.Add(-100*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
	)
	backend := &sliceReader{rows: []OverrideRecord{mute}}
	am := NewAgedMutes(backend)
	am.Clock = fixedClock{now}

	first, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("first Report: %v", err)
	}
	if len(first) != 1 {
		t.Fatalf("first len(report)=%d, want 1 (aged mute should appear before the unmute)", len(first))
	}

	// Operator appends an override(mute=false) row for the
	// SAME (rule_id, scope) tuple via mgmt.override. The
	// timestamp is fresher than the mute so latest-row-wins
	// picks the unmute.
	unmute := newAt(
		mute.RuleID,
		mute.Scope.RepoID,
		mute.Scope.ScopeKind,
		mute.Scope.ScopeSignatureGlob,
		false,
		now.Add(-1*time.Hour),
		"22222222-2222-2222-2222-222222222222",
	)
	unmute.Reason = "" // unmute carries empty reason per the steward contract
	backend.rows = append(backend.rows, unmute)

	second, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("second Report: %v", err)
	}
	if len(second) != 0 {
		t.Fatalf("second len(report)=%d, want 0 (unmute should drop the pair off the report)", len(second))
	}
	// Sanity: backend was read twice (once per Report call).
	if backend.calls != 2 {
		t.Errorf("backend.calls=%d, want 2 (one read per Report)", backend.calls)
	}
}

// TestAgedMutes_RemuteAfterUnmuteReturnsAgedMute walks the
// full mute -> unmute -> remute lineage: a freshly-issued
// remute supersedes the unmute by latest-row-wins, but if
// THAT remute is itself older than the threshold, the (rule,
// scope) pair re-appears on the report. The reducer never
// looks past the per-group winner -- this asserts the
// architecture-mandated MAX(created_at) semantic.
func TestAgedMutes_RemuteAfterUnmuteReturnsAgedMute(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	mute := newAt(
		"solid.srp.lcom4_high",
		"repo-1",
		"class",
		"com.example.legacy.Foo",
		true,
		now.Add(-200*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
	)
	unmute := newAt(
		mute.RuleID, mute.Scope.RepoID, mute.Scope.ScopeKind, mute.Scope.ScopeSignatureGlob,
		false, now.Add(-150*24*time.Hour),
		"22222222-2222-2222-2222-222222222222",
	)
	remute := newAt(
		mute.RuleID, mute.Scope.RepoID, mute.Scope.ScopeKind, mute.Scope.ScopeSignatureGlob,
		true, now.Add(-100*24*time.Hour),
		"33333333-3333-3333-3333-333333333333",
	)
	backend := &sliceReader{rows: []OverrideRecord{mute, unmute, remute}}
	am := &AgedMutes{
		Reader:    backend,
		Clock:     fixedClock{now},
		Threshold: 90 * 24 * time.Hour,
	}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(report)=%d, want 1 (remute supersedes unmute, is 100d old)", len(got))
	}
	if got[0].OverrideID != remute.OverrideID {
		t.Errorf("OverrideID=%q, want %q (remute should win)", got[0].OverrideID, remute.OverrideID)
	}
	if got[0].AgeDays != 100 {
		t.Errorf("AgeDays=%d, want 100 (the remute, NOT the 200d original mute)", got[0].AgeDays)
	}
}

// TestAgedMutes_FreshRemuteIsNotAged asserts the inverse of
// the previous test: when the latest row is `mute=true` but
// younger than the threshold, the pair does NOT appear. This
// pins the rubber-duck blind spot that a stale (mute=true,
// 200d-old) row should NOT haunt a (rule, scope) that the
// operator has since freshly re-muted.
func TestAgedMutes_FreshRemuteIsNotAged(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	ancient := newAt(
		"solid.dip.cycles_in_module_graph",
		"repo-1", "package", "com.example.legacy",
		true, now.Add(-200*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
	)
	fresh := newAt(
		ancient.RuleID, ancient.Scope.RepoID, ancient.Scope.ScopeKind, ancient.Scope.ScopeSignatureGlob,
		true, now.Add(-1*24*time.Hour),
		"22222222-2222-2222-2222-222222222222",
	)
	backend := &sliceReader{rows: []OverrideRecord{ancient, fresh}}
	am := &AgedMutes{
		Reader:    backend,
		Clock:     fixedClock{now},
		Threshold: 90 * 24 * time.Hour,
	}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(report)=%d, want 0 (a fresh remute should supersede the ancient mute)", len(got))
	}
}

// TestAgedMutes_BoundaryAtExactThresholdIsNotAged pins the
// inclusive contract: a row whose `now - created_at` equals
// the threshold MUST NOT be reported (mirrors
// [Freshness.Window] inclusive semantics). Strict inequality
// `age > threshold` is what the implementation uses.
func TestAgedMutes_BoundaryAtExactThresholdIsNotAged(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	mute := newAt(
		"r", "repo", "class", "Foo", true,
		now.Add(-90*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
	)
	backend := &sliceReader{rows: []OverrideRecord{mute}}
	am := &AgedMutes{Reader: backend, Clock: fixedClock{now}, Threshold: 90 * 24 * time.Hour}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(report)=%d, want 0 at exact-boundary age==threshold (inclusive)", len(got))
	}
}

// TestAgedMutes_OneSecondPastBoundaryIsAged complements the
// boundary test: a row one second past the threshold MUST be
// classified aged.
func TestAgedMutes_OneSecondPastBoundaryIsAged(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	threshold := 90 * 24 * time.Hour
	mute := newAt(
		"r", "repo", "class", "Foo", true,
		now.Add(-(threshold + time.Second)),
		"11111111-1111-1111-1111-111111111111",
	)
	backend := &sliceReader{rows: []OverrideRecord{mute}}
	am := &AgedMutes{Reader: backend, Clock: fixedClock{now}, Threshold: threshold}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("len(report)=%d, want 1 (1s past threshold should be aged)", len(got))
	}
}

// TestAgedMutes_TieBreakOnOverrideIDPicksLarger pins the
// `(created_at, override_id) DESC` tie-break that mirrors
// the SQL contract on [Store.LatestMatchingOverride]. Two
// rows with the SAME created_at and SAME (rule, scope)
// should resolve to the larger OverrideID.
func TestAgedMutes_TieBreakOnOverrideIDPicksLarger(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	stamp := now.Add(-100 * 24 * time.Hour)
	// Both rows have IDENTICAL CreatedAt; tie-break picks
	// the larger OverrideID lexicographically. Per the
	// reducer's `cand.OverrideID > cur.OverrideID` test, the
	// "ff..." id wins over "11...".
	loserMute := newAt(
		"r", "repo", "class", "Foo", true, stamp,
		"11111111-1111-1111-1111-111111111111",
	)
	loserMute.Reason = "older id"
	winnerUnmute := newAt(
		"r", "repo", "class", "Foo", false, stamp,
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
	)
	winnerUnmute.Reason = ""
	backend := &sliceReader{rows: []OverrideRecord{loserMute, winnerUnmute}}
	am := &AgedMutes{Reader: backend, Clock: fixedClock{now}, Threshold: 90 * 24 * time.Hour}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(report)=%d, want 0 (the larger-id unmute wins the tie-break and supersedes the mute)", len(got))
	}
}

// TestAgedMutes_DifferentScopesAreSeparateGroups asserts that
// two mutes with the same RuleID but different
// ScopeSignatureGlob values are NOT collapsed into one group.
// An unmute against scope-A must NOT silently mask an aged
// mute against scope-B.
func TestAgedMutes_DifferentScopesAreSeparateGroups(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	muteA := newAt(
		"r", "repo", "class", "com.example.Foo", true,
		now.Add(-100*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
	)
	muteB := newAt(
		"r", "repo", "class", "com.example.Bar", true,
		now.Add(-100*24*time.Hour),
		"22222222-2222-2222-2222-222222222222",
	)
	unmuteA := newAt(
		"r", "repo", "class", "com.example.Foo", false,
		now.Add(-1*time.Hour),
		"33333333-3333-3333-3333-333333333333",
	)
	unmuteA.Reason = ""
	backend := &sliceReader{rows: []OverrideRecord{muteA, muteB, unmuteA}}
	am := &AgedMutes{Reader: backend, Clock: fixedClock{now}, Threshold: 90 * 24 * time.Hour}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(report)=%d, want 1 (unmute on Foo must NOT mask aged mute on Bar)", len(got))
	}
	if got[0].Scope.ScopeSignatureGlob != "com.example.Bar" {
		t.Errorf("Scope.ScopeSignatureGlob=%q, want %q",
			got[0].Scope.ScopeSignatureGlob, "com.example.Bar")
	}
}

// TestAgedMutes_DeterministicSortOrder pins the
// (RuleID, RepoID, ScopeKind, Glob, OverrideID) sort key so
// two callers reading the same backend state see byte-
// identical JSON.
func TestAgedMutes_DeterministicSortOrder(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	muteRules := []OverrideRecord{
		newAt("solid.srp.lcom4_high", "repo-z", "class", "AAA", true,
			now.Add(-100*24*time.Hour), "11111111-1111-1111-1111-111111111111"),
		newAt("solid.dip.cycles", "repo-a", "package", "com.example", true,
			now.Add(-100*24*time.Hour), "22222222-2222-2222-2222-222222222222"),
		newAt("solid.srp.lcom4_high", "repo-a", "class", "BBB", true,
			now.Add(-100*24*time.Hour), "33333333-3333-3333-3333-333333333333"),
		newAt("solid.srp.lcom4_high", "repo-a", "class", "AAA", true,
			now.Add(-100*24*time.Hour), "44444444-4444-4444-4444-444444444444"),
	}
	backend := &sliceReader{rows: muteRules}
	am := &AgedMutes{Reader: backend, Clock: fixedClock{now}, Threshold: 90 * 24 * time.Hour}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("len(report)=%d, want 4", len(got))
	}
	wantOrder := []struct{ rule, repo, glob string }{
		{"solid.dip.cycles", "repo-a", "com.example"},
		{"solid.srp.lcom4_high", "repo-a", "AAA"},
		{"solid.srp.lcom4_high", "repo-a", "BBB"},
		{"solid.srp.lcom4_high", "repo-z", "AAA"},
	}
	for i, w := range wantOrder {
		if got[i].RuleID != w.rule || got[i].Scope.RepoID != w.repo || got[i].Scope.ScopeSignatureGlob != w.glob {
			t.Errorf("report[%d] = (rule=%q, repo=%q, glob=%q); want (%q, %q, %q)",
				i, got[i].RuleID, got[i].Scope.RepoID, got[i].Scope.ScopeSignatureGlob,
				w.rule, w.repo, w.glob)
		}
	}
}

// TestAgedMutes_ReportWithThresholdHonorsCustomThreshold pins
// the operator-overridable threshold path. A 30-day-old mute
// is NOT aged under 90 days (default) but IS aged under
// 14 days.
func TestAgedMutes_ReportWithThresholdHonorsCustomThreshold(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	mute := newAt(
		"r", "repo", "class", "Foo", true,
		now.Add(-30*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
	)
	backend := &sliceReader{rows: []OverrideRecord{mute}}
	am := NewAgedMutes(backend)
	am.Clock = fixedClock{now}

	defaultReport, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(defaultReport) != 0 {
		t.Errorf("default len(report)=%d, want 0 (30d < 90d threshold)", len(defaultReport))
	}

	twoWeekReport, err := am.ReportWithThreshold(context.Background(), 14*24*time.Hour)
	if err != nil {
		t.Fatalf("ReportWithThreshold(14d): %v", err)
	}
	if len(twoWeekReport) != 1 {
		t.Errorf("14d len(report)=%d, want 1 (30d > 14d threshold)", len(twoWeekReport))
	}
}

// TestAgedMutes_NonPositiveThresholdFallsBackToDefault guards
// against the failure mode where a missing `threshold_days`
// HTTP arg surfaces every mute. A zero or negative duration is
// treated as the default 90-day threshold.
func TestAgedMutes_NonPositiveThresholdFallsBackToDefault(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	mute := newAt(
		"r", "repo", "class", "Foo", true,
		now.Add(-30*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
	)
	backend := &sliceReader{rows: []OverrideRecord{mute}}
	am := NewAgedMutes(backend)
	am.Clock = fixedClock{now}

	for _, badThreshold := range []time.Duration{0, -time.Hour, -90 * 24 * time.Hour} {
		got, err := am.ReportWithThreshold(context.Background(), badThreshold)
		if err != nil {
			t.Fatalf("ReportWithThreshold(%v): %v", badThreshold, err)
		}
		if len(got) != 0 {
			t.Errorf("threshold=%v len(report)=%d, want 0 (should fall back to default 90d; 30d mute is fresh)",
				badThreshold, len(got))
		}
	}
}

// TestAgedMutes_NilClockFallsBackToSystem documents the
// nil-clock safety contract on [AgedMutes.now]: an AgedMutes
// constructed with a nil [Clock] does NOT panic on Report; it
// falls back to [SystemClock]. Mirrors
// [TestFreshness_NilClockFallsBackToSystem].
func TestAgedMutes_NilClockFallsBackToSystem(t *testing.T) {
	t.Parallel()
	// A mute one second ago is fresh under any sane system
	// clock; this asserts the nil-clock path produces a
	// report without panicking.
	mute := newAt(
		"r", "repo", "class", "Foo", true,
		time.Now().Add(-time.Second),
		"11111111-1111-1111-1111-111111111111",
	)
	backend := &sliceReader{rows: []OverrideRecord{mute}}
	am := &AgedMutes{Reader: backend, Clock: nil, Threshold: 90 * 24 * time.Hour}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report (nil clock): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(report)=%d, want 0 (fresh mute under fallback clock)", len(got))
	}
}

// TestAgedMutes_NilReaderReturnsSentinelError pins the
// composition-root safety contract: an AgedMutes built without
// an [OverrideReader] returns [ErrAgedMuteReaderUnavailable]
// rather than panicking, so the HTTP layer can map to 503.
func TestAgedMutes_NilReaderReturnsSentinelError(t *testing.T) {
	t.Parallel()
	am := &AgedMutes{Reader: nil, Clock: fixedClock{time.Now()}}
	_, err := am.Report(context.Background())
	if !errors.Is(err, ErrAgedMuteReaderUnavailable) {
		t.Errorf("err=%v, want ErrAgedMuteReaderUnavailable", err)
	}

	// Nil receiver MUST also degrade gracefully (e.g. a
	// composition root that forgot to even construct the
	// AgedMutes struct still gets a sentinel rather than a
	// nil-pointer panic).
	var nilAM *AgedMutes
	_, err = nilAM.Report(context.Background())
	if !errors.Is(err, ErrAgedMuteReaderUnavailable) {
		t.Errorf("nil-receiver err=%v, want ErrAgedMuteReaderUnavailable", err)
	}
}

// TestAgedMutes_ReaderErrorPropagates asserts the backend
// error is returned verbatim so an operator dashboard sees
// the underlying cause (driver error, table missing, etc.)
// in its log.
func TestAgedMutes_ReaderErrorPropagates(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("backend exploded")
	reader := listAllOverridesFunc(func(ctx context.Context) ([]OverrideRecord, error) {
		return nil, sentinel
	})
	am := &AgedMutes{Reader: reader, Clock: fixedClock{time.Now()}}
	_, err := am.Report(context.Background())
	if !errors.Is(err, sentinel) {
		t.Errorf("err=%v, want backend sentinel %v", err, sentinel)
	}
}

// TestAgedMutes_ContextCancellationPropagates pins the
// dashboard contract: an operator tab that closes mid-scan
// MUST propagate `ctx.Err()` so the backend cancels the table
// scan. The fake reader checks `ctx.Err()` and the test
// confirms the cancellation flows through Report verbatim.
func TestAgedMutes_ContextCancellationPropagates(t *testing.T) {
	t.Parallel()
	backend := &sliceReader{rows: nil}
	am := &AgedMutes{Reader: backend, Clock: fixedClock{time.Now()}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the call
	_, err := am.Report(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err=%v, want context.Canceled", err)
	}
}

// TestAgedMutes_EmptyBackendReturnsEmptyReport guards against
// a nil-vs-empty-slice JSON encoding regression. The wire
// shape MUST be `[]`, not `null`.
func TestAgedMutes_EmptyBackendReturnsEmptyReport(t *testing.T) {
	t.Parallel()
	backend := &sliceReader{rows: nil}
	am := NewAgedMutes(backend)
	am.Clock = fixedClock{time.Now()}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if got == nil {
		t.Error("Report returned nil; want a non-nil empty slice (JSON `[]`, not `null`)")
	}
	if len(got) != 0 {
		t.Errorf("len(report)=%d, want 0", len(got))
	}
}

// TestAgedMutes_OnlyUnmuteRowsReturnsEmptyReport guards
// against a regression that would surface `mute=false` rows.
// Even at >threshold age, an unmute is NOT a mute and MUST
// NOT appear.
func TestAgedMutes_OnlyUnmuteRowsReturnsEmptyReport(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	unmute := newAt(
		"r", "repo", "class", "Foo", false,
		now.Add(-200*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
	)
	unmute.Reason = ""
	backend := &sliceReader{rows: []OverrideRecord{unmute}}
	am := NewAgedMutes(backend)
	am.Clock = fixedClock{now}

	got, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(report)=%d, want 0 (only unmute rows; nothing aged)", len(got))
	}
}

// TestNewAgedMutes_WiresProductionDefaults pins the canonical
// composition-root constructor: 90-day threshold and a
// [SystemClock]. A drift here would break the Stage 10.2
// brief default.
func TestNewAgedMutes_WiresProductionDefaults(t *testing.T) {
	t.Parallel()
	backend := &sliceReader{}
	am := NewAgedMutes(backend)
	if am == nil {
		t.Fatal("NewAgedMutes returned nil")
	}
	if am.Reader == nil {
		t.Error("Reader=nil; want the supplied backend")
	}
	want := time.Duration(AgedMuteDefaultThresholdDays) * 24 * time.Hour
	if am.Threshold != want {
		t.Errorf("Threshold=%v, want %v", am.Threshold, want)
	}
	if _, ok := am.Clock.(SystemClock); !ok {
		t.Errorf("Clock=%T, want SystemClock", am.Clock)
	}
}

// TestAgedMuteDefaultThresholdDaysLiteral pins the Stage 10.2
// brief default (90) as a typed literal. A bump requires a
// brief amendment so this constant is locked at the test
// level.
func TestAgedMuteDefaultThresholdDaysLiteral(t *testing.T) {
	t.Parallel()
	if AgedMuteDefaultThresholdDays != 90 {
		t.Errorf("AgedMuteDefaultThresholdDays=%d, want 90", AgedMuteDefaultThresholdDays)
	}
}

// TestReduceAndFilter_DropsUnmuteAtBoundary directly exercises
// the pure reducer (no goroutines, no clock seam) to lock the
// "mute=false drops the pair off the report" branch even when
// the latest row is ALSO at the exact boundary. This guards
// against a regression where the boundary check ran before the
// mute filter, accidentally promoting an unmute back into the
// report.
func TestReduceAndFilter_DropsUnmuteAtBoundary(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	threshold := 90 * 24 * time.Hour
	// Mute that is way past the threshold.
	mute := newAt("r", "repo", "class", "Foo", true,
		now.Add(-200*24*time.Hour),
		"11111111-1111-1111-1111-111111111111")
	// Unmute exactly at the threshold age (fresh under the
	// inclusive boundary, BUT mute=false so the mute filter
	// should drop it anyway).
	unmute := newAt("r", "repo", "class", "Foo", false,
		now.Add(-threshold),
		"22222222-2222-2222-2222-222222222222")
	unmute.Reason = ""

	got := reduceAndFilter([]OverrideRecord{mute, unmute}, now, threshold)
	if len(got) != 0 {
		t.Errorf("len(report)=%d, want 0 (unmute supersedes; mute filter drops it regardless of age)", len(got))
	}
}

// TestAgedMute_Echo asserts the AgeDays field is computed from
// the (now - CreatedAt) duration with integer-day truncation
// (no rounding -- a 100d-1s-old mute reports 99 days).
func TestAgedMute_Echo(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		age  time.Duration
		want int
	}{
		{91 * 24 * time.Hour, 91},
		{100*24*time.Hour - time.Second, 99},
		{365 * 24 * time.Hour, 365},
	}
	for _, tc := range cases {
		mute := newAt("r", "repo", "class", "Foo", true,
			now.Add(-tc.age),
			"11111111-1111-1111-1111-111111111111")
		backend := &sliceReader{rows: []OverrideRecord{mute}}
		am := &AgedMutes{Reader: backend, Clock: fixedClock{now}, Threshold: 90 * 24 * time.Hour}
		got, err := am.Report(context.Background())
		if err != nil {
			t.Fatalf("age=%v Report: %v", tc.age, err)
		}
		if len(got) != 1 {
			t.Fatalf("age=%v len(report)=%d, want 1", tc.age, len(got))
		}
		if got[0].AgeDays != tc.want {
			t.Errorf("age=%v AgeDays=%d, want %d (floor-of-days)",
				tc.age, got[0].AgeDays, tc.want)
		}
	}
}
