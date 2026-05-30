package effort

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
)

// mkScope returns a fresh, non-nil UUID. The fallback formula
// is purely a function of (kind, loc, cyclo, fan_in) so the
// specific id value never affects the output; we mint a fresh
// one per test to keep each case independent of every other.
func mkScope(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid.NewV4: %v", err)
	}
	return id
}

// mkTask materialises a [refactor.RefactorTask] with only the
// fields the [FallbackModel] consumes set. The planner-supplied
// `EffortHours` is the OUTPUT of `Estimate`, not an input.
func mkTask(t *testing.T, kind refactor.TaskKind, scopeID uuid.UUID) refactor.RefactorTask {
	t.Helper()
	return refactor.RefactorTask{
		ScopeID: scopeID,
		Kind:    kind,
		RuleID:  "solid.srp.lcom4_high",
	}
}

// fixedProvider returns an [EffortInputProvider] that yields
// (loc, cyclo, fanIn, true) for the supplied scopeID and
// (0,0,0,false) for every other id. Used in the table-driven
// formula tests where each row pins a single scope.
func fixedProvider(want uuid.UUID, loc, cyclo, fanIn float64) EffortInputProvider {
	return func(id uuid.UUID) (float64, float64, float64, bool) {
		if id == want {
			return loc, cyclo, fanIn, true
		}
		return 0, 0, 0, false
	}
}

// silentLogger discards all log output. Used in non-log
// assertion tests so the test binary's stderr stays clean.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// -----------------------------------------------------------------------------
// Pinned outputs from REFACTOR-GUIDE e2e-scenarios.md "Stage 1.3"
// -----------------------------------------------------------------------------
//
// For loc=500, cyclo=20, fan_in=8 the base is:
//   base = 0.02*500 + 0.10*20 + 0.05*8 + 1.0 = 10 + 2 + 0.4 + 1 = 13.4
//
// Multiplied by the per-kind factor and rounded half-up to
// one decimal, the five canonical kinds produce:
//
//   split_class             13.4 * 1.5 = 20.1
//   invert_dependency       13.4 * 1.3 = 17.42 -> 17.4
//   break_cycle             13.4 * 1.4 = 18.76 -> 18.8
//   extract_method          13.4 * 0.7 =  9.38 ->  9.4
//   consolidate_duplication 13.4 * 1.0 = 13.4

func TestFallbackModel_PinnedE2EFixtureOutputs(t *testing.T) {
	scope := mkScope(t)
	cases := []struct {
		name string
		kind refactor.TaskKind
		want float64
	}{
		{"split_class", refactor.TaskKindSplitClass, 20.1},
		{"invert_dependency", refactor.TaskKindInvertDependency, 17.4},
		{"break_cycle", refactor.TaskKindBreakCycle, 18.8},
		{"extract_method", refactor.TaskKindExtractMethod, 9.4},
		{"consolidate_duplication", refactor.TaskKindConsolidateDuplication, 13.4},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := New(silentLogger(),
				WithInputSource(fixedProvider(scope, 500, 20, 8)))
			got, err := m.Estimate(mkTask(t, c.kind, scope), refactor.HotSpot{}, refactor.PolicySnapshot{})
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			if got != c.want {
				t.Fatalf("Estimate(%s) = %v, want %v", c.kind, got, c.want)
			}
		})
	}
}

// TestFallbackModel_UpperClampHits80 verifies that the
// formula saturates at exactly 80.0 (REFACTOR-GUIDE tech-spec
// Sec 8.5 line 959 max bound).
func TestFallbackModel_UpperClampHits80(t *testing.T) {
	scope := mkScope(t)
	// Pick inputs that drive base WAY above the clamp:
	//   loc=100_000  -> 2000 hours from loc term alone.
	//   Times split_class factor 1.5 -> 3000. Clamps to 80.0.
	m := New(silentLogger(), WithInputSource(fixedProvider(scope, 100000, 0, 0)))
	got, err := m.Estimate(mkTask(t, refactor.TaskKindSplitClass, scope), refactor.HotSpot{}, refactor.PolicySnapshot{})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got != 80.0 {
		t.Fatalf("upper clamp: got %v, want 80.0", got)
	}
}

// TestFallbackModel_LowerClampHits0Point1 verifies the floor.
// extract_method (factor 0.7) on an all-zero scope yields
// base 1.0, adjusted 0.7 -- already inside `[0.1, 80.0]` --
// so the lower clamp is only exercised by an explicit
// pathological override. We use the all-zero scope and
// extract_method to verify the formula's natural floor sits
// at 0.7 (not 0); the clamp itself we exercise below by
// patching the formula via WithInputSource returning
// sanitised-to-zero values.
//
// REFACTOR-GUIDE tech-spec Sec 8.5 line 959 pins the floor at
// `0.1`. A natural input that hits 0.1 would require a kind
// with factor <= 0.1/1.0 = 0.1 (none exists) so the clamp is
// genuinely only reachable by the all-zero-and-tiny-factor
// branch -- meaning the floor is unreachable in canonical
// kinds today. We assert the formula reaches its lowest
// canonical value (extract_method @ zero inputs = 0.7) AND
// document that the clamp would clip a hypothetical sub-0.1
// value, by feeding NaN/negative inputs through the
// sanitiser path.
func TestFallbackModel_NaturalFloorIsFactorTimesBaseOne(t *testing.T) {
	scope := mkScope(t)
	m := New(silentLogger(), WithInputSource(fixedProvider(scope, 0, 0, 0)))
	got, err := m.Estimate(mkTask(t, refactor.TaskKindExtractMethod, scope), refactor.HotSpot{}, refactor.PolicySnapshot{})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	// base = 1.0; * 0.7 = 0.7; clamp keeps; round = 0.7.
	if got != 0.7 {
		t.Fatalf("natural floor: got %v, want 0.7", got)
	}
}

// TestFallbackModel_LowerClampDirect exercises the 0.1 floor
// by calling the internal clamp helper on the boundary
// values. Black-box `Estimate` cannot reach below 0.7 with
// canonical kinds + finite inputs, so verifying the bound
// directly is the only way to pin it.
func TestFallbackModel_LowerClampDirect(t *testing.T) {
	if got := clamp(-5, fallbackMinHours, fallbackMaxHours); got != fallbackMinHours {
		t.Fatalf("clamp lower: got %v, want %v", got, fallbackMinHours)
	}
	if got := clamp(1000, fallbackMinHours, fallbackMaxHours); got != fallbackMaxHours {
		t.Fatalf("clamp upper: got %v, want %v", got, fallbackMaxHours)
	}
	if got := clamp(13.4, fallbackMinHours, fallbackMaxHours); got != 13.4 {
		t.Fatalf("clamp middle: got %v, want 13.4", got)
	}
}

// TestFallbackModel_OKFalseTreatsAllInputsAsZero pins the
// "scope unknown" branch: when the provider returns ok=false
// the formula uses (0, 0, 0) -> base 1.0 -> factor 1.5 ->
// 1.5 -> clamped 1.5 -> 1.5.
func TestFallbackModel_OKFalseTreatsAllInputsAsZero(t *testing.T) {
	scope := mkScope(t)
	other := mkScope(t)
	m := New(silentLogger(), WithInputSource(fixedProvider(other, 500, 20, 8)))
	got, err := m.Estimate(mkTask(t, refactor.TaskKindSplitClass, scope), refactor.HotSpot{}, refactor.PolicySnapshot{})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got != 1.5 {
		t.Fatalf("ok=false branch: got %v, want 1.5", got)
	}
}

// TestFallbackModel_PerMetricDarkness pins the "loc present,
// cyclo+fan_in dark" branch (the realistic scenario for a
// Python file in Stage 2.1 where decision_blocks/call_edges
// aren't emitted yet). REFACTOR-GUIDE tech-spec Sec 8.5
// final paragraph: missing inputs contribute zero.
//
// loc=500, cyclo=0, fan_in=0 -> base = 10 + 0 + 0 + 1 = 11.
// invert_dependency factor 1.3 -> 14.3 -> round -> 14.3.
func TestFallbackModel_PerMetricDarkness(t *testing.T) {
	scope := mkScope(t)
	m := New(silentLogger(), WithInputSource(fixedProvider(scope, 500, 0, 0)))
	got, err := m.Estimate(mkTask(t, refactor.TaskKindInvertDependency, scope), refactor.HotSpot{}, refactor.PolicySnapshot{})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got != 14.3 {
		t.Fatalf("per-metric darkness: got %v, want 14.3", got)
	}
}

// TestFallbackModel_NilProviderBehavesLikeOKFalse pins that
// a model constructed without [WithInputSource] does not
// panic and behaves as if every scope was unknown.
func TestFallbackModel_NilProviderBehavesLikeOKFalse(t *testing.T) {
	scope := mkScope(t)
	m := New(silentLogger())
	got, err := m.Estimate(mkTask(t, refactor.TaskKindSplitClass, scope), refactor.HotSpot{}, refactor.PolicySnapshot{})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got != 1.5 {
		t.Fatalf("nil provider: got %v, want 1.5", got)
	}
}

// TestFallbackModel_SanitisesNaNAndNegativeInputs pins the
// defensive-input sanitiser path: a caller-supplied provider
// that yields NaN/Inf/negative values must not poison the
// arithmetic. All three pathological inputs collapse to 0;
// formula collapses to `1.0 * factor`.
func TestFallbackModel_SanitisesNaNAndNegativeInputs(t *testing.T) {
	scope := mkScope(t)
	cases := []struct {
		name              string
		loc, cyclo, fanIn float64
	}{
		{"NaN loc", math.NaN(), 5, 5},
		{"+Inf cyclo", 10, math.Inf(+1), 5},
		{"-Inf fan_in", 10, 5, math.Inf(-1)},
		{"negative loc", -100, 5, 5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := New(silentLogger(), WithInputSource(fixedProvider(scope, c.loc, c.cyclo, c.fanIn)))
			got, err := m.Estimate(mkTask(t, refactor.TaskKindBreakCycle, scope), refactor.HotSpot{}, refactor.PolicySnapshot{})
			if err != nil {
				t.Fatalf("Estimate: %v", err)
			}
			// Only the sanitised input is zero; the other two
			// remain. We assert the result is FINITE and within
			// the clamp window, not the exact value (since two
			// of three inputs are still in play).
			if math.IsNaN(got) || math.IsInf(got, 0) {
				t.Fatalf("got non-finite %v from pathological inputs", got)
			}
			if got < fallbackMinHours || got > fallbackMaxHours {
				t.Fatalf("got %v outside clamp window [%v, %v]", got, fallbackMinHours, fallbackMaxHours)
			}
		})
	}
}

// TestFallbackModel_InvalidTaskKindReturnsError pins the
// ValidateTaskKind error path. A non-canonical kind must
// surface as a wrapped error so the planner's "abort the
// batch" contract triggers.
func TestFallbackModel_InvalidTaskKindReturnsError(t *testing.T) {
	scope := mkScope(t)
	m := New(silentLogger(), WithInputSource(fixedProvider(scope, 500, 20, 8)))
	task := refactor.RefactorTask{
		ScopeID: scope,
		Kind:    refactor.TaskKind("not-a-real-kind"),
		RuleID:  "x",
	}
	_, err := m.Estimate(task, refactor.HotSpot{}, refactor.PolicySnapshot{})
	if err == nil {
		t.Fatalf("Estimate(invalid kind): expected error, got nil")
	}
	if !strings.Contains(err.Error(), "effort.FallbackModel.Estimate") {
		t.Fatalf("error not wrapped with package context: %v", err)
	}
	// And confirm the underlying ValidateTaskKind sentinel
	// still unwraps so callers can errors.Is upstream.
	if inner := errors.Unwrap(err); inner == nil {
		t.Fatalf("expected wrapped error, unwrap returned nil: %v", err)
	}
}

// TestFallbackModel_ModeReturnsFallbackByDefault pins
// REFACTOR-GUIDE tech-spec C15 "Mode() returns the exact
// string fallback".
func TestFallbackModel_ModeReturnsFallbackByDefault(t *testing.T) {
	m := New(silentLogger())
	if got := m.Mode(); got != EffortSourceFallback {
		t.Fatalf("Mode default: got %q, want %q", got, EffortSourceFallback)
	}
	if got := m.Mode(); got != "fallback" {
		t.Fatalf("Mode default literal: got %q, want %q", got, "fallback")
	}
}

// TestFallbackModel_WithSourceTagOverridesMode confirms the
// [WithSourceTag] option overrides [Mode] and that an empty
// tag is ignored (defaults preserved).
func TestFallbackModel_WithSourceTagOverridesMode(t *testing.T) {
	m := New(silentLogger(), WithSourceTag("custom-fallback-v2"))
	if got := m.Mode(); got != "custom-fallback-v2" {
		t.Fatalf("Mode override: got %q, want %q", got, "custom-fallback-v2")
	}
	m2 := New(silentLogger(), WithSourceTag(""))
	if got := m2.Mode(); got != EffortSourceFallback {
		t.Fatalf("Empty tag should preserve default: got %q, want %q", got, EffortSourceFallback)
	}
}

// TestFallbackModel_FormulaVersionExposed pins the version
// accessor.
func TestFallbackModel_FormulaVersionExposed(t *testing.T) {
	m := New(silentLogger())
	if got := m.FormulaVersion(); got != FormulaVersion {
		t.Fatalf("FormulaVersion: got %q, want %q", got, FormulaVersion)
	}
	if !strings.HasPrefix(FormulaVersion, "fallback-") {
		t.Fatalf("FormulaVersion %q must start with %q", FormulaVersion, "fallback-")
	}
}

// TestFallbackModel_FirstInvocationEmitsWarning pins the
// REFACTOR-GUIDE e2e-scenarios.md "Effort fallback advertises
// its mode in diagnostics" feature -- the first Estimate
// emits exactly one WARNING line whose body contains the
// substring "deterministic fallback formula".
func TestFallbackModel_FirstInvocationEmitsWarning(t *testing.T) {
	var buf bytes.Buffer
	m := New(nil, withLogWriter(&buf))
	scope := mkScope(t)
	// First call emits the warning.
	if _, err := m.Estimate(mkTask(t, refactor.TaskKindSplitClass, scope), refactor.HotSpot{}, refactor.PolicySnapshot{}); err != nil {
		t.Fatalf("Estimate (1st): %v", err)
	}
	first := buf.String()
	if first == "" {
		t.Fatalf("expected warning on first invocation, got empty log")
	}
	if !strings.Contains(first, "deterministic fallback formula") {
		t.Fatalf("warning missing canonical substring: %q", first)
	}
	if !strings.Contains(first, FormulaVersion) {
		t.Fatalf("warning missing formula version %q: %q", FormulaVersion, first)
	}
	if !strings.Contains(strings.ToUpper(first), "WARN") {
		t.Fatalf("warning level not WARN: %q", first)
	}
	// Second call must NOT emit anything new (sync.Once).
	buf.Reset()
	if _, err := m.Estimate(mkTask(t, refactor.TaskKindExtractMethod, scope), refactor.HotSpot{}, refactor.PolicySnapshot{}); err != nil {
		t.Fatalf("Estimate (2nd): %v", err)
	}
	if got := buf.String(); got != "" {
		t.Fatalf("expected no new log on 2nd call, got %q", got)
	}
}

// TestFallbackModel_WarningOnceUnderConcurrency confirms the
// sync.Once gate holds when the planner calls Estimate from
// multiple goroutines (the production planner does not, but
// the EffortModel interface doc allows it and refactor's
// own tests assert it).
func TestFallbackModel_WarningOnceUnderConcurrency(t *testing.T) {
	var buf bytes.Buffer
	// slog.Handler is goroutine-safe, but a bytes.Buffer is
	// NOT. Wrap with a mutex so concurrent writes don't race
	// the test scaffolding (the production code logs via the
	// Handler abstraction which serialises internally; this
	// wrapper just protects the test scaffold buffer).
	w := &lockedWriter{w: &buf}
	m := New(nil, withLogWriter(w))
	scope := mkScope(t)
	const N = 64
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			_, _ = m.Estimate(mkTask(t, refactor.TaskKindSplitClass, scope), refactor.HotSpot{}, refactor.PolicySnapshot{})
		}()
	}
	wg.Wait()
	got := buf.String()
	count := strings.Count(got, "deterministic fallback formula")
	if count != 1 {
		t.Fatalf("expected exactly 1 warning line under %d concurrent calls, got %d. Buffer:\n%s", N, count, got)
	}
}

// TestFallbackModel_InterfaceConformance is a compile-time
// shape check that survives the var-blank assertion in
// effort.go (if the interface changes, this and the var-blank
// both fail).
func TestFallbackModel_InterfaceConformance(t *testing.T) {
	var m refactor.EffortModel = New(silentLogger())
	if m == nil {
		t.Fatal("FallbackModel does not satisfy refactor.EffortModel")
	}
}

// TestFallbackModel_NilLoggerFallsBackToSlogDefault confirms
// that constructing with a nil logger does not panic and
// produces a working estimator.
func TestFallbackModel_NilLoggerFallsBackToSlogDefault(t *testing.T) {
	m := New(nil) // no logger, no provider
	if m == nil {
		t.Fatal("New(nil) returned nil")
	}
	scope := mkScope(t)
	got, err := m.Estimate(mkTask(t, refactor.TaskKindConsolidateDuplication, scope), refactor.HotSpot{}, refactor.PolicySnapshot{})
	if err != nil {
		t.Fatalf("Estimate: %v", err)
	}
	if got != 1.0 {
		t.Fatalf("nil logger path: got %v, want 1.0", got)
	}
}

// TestFallbackModel_RoundHalfUp pins the rounding rule
// (REFACTOR-GUIDE tech-spec Sec 8.5 line 960). 13.45 must
// round UP to 13.5; 13.44 down to 13.4.
func TestFallbackModel_RoundHalfUp(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{13.45, 13.5},
		{13.44, 13.4},
		{0.05, 0.1},
		{0.04, 0.0},
		{20.05, 20.1},
		{17.42, 17.4},
		{18.76, 18.8},
	}
	for _, c := range cases {
		if got := roundHalfUpOneDecimal(c.in); got != c.want {
			t.Fatalf("roundHalfUpOneDecimal(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestTaskKindFactor_Exhaustive pins every canonical kind's
// multiplier so a future refactor of the table is forced to
// update this test in lockstep.
func TestTaskKindFactor_Exhaustive(t *testing.T) {
	cases := []struct {
		kind refactor.TaskKind
		want float64
	}{
		{refactor.TaskKindSplitClass, 1.5},
		{refactor.TaskKindInvertDependency, 1.3},
		{refactor.TaskKindBreakCycle, 1.4},
		{refactor.TaskKindExtractMethod, 0.7},
		{refactor.TaskKindConsolidateDuplication, 1.0},
	}
	for _, c := range cases {
		if got := taskKindFactor(c.kind); got != c.want {
			t.Fatalf("taskKindFactor(%s) = %v, want %v", c.kind, got, c.want)
		}
	}
	// Unknown kind defaults to 1.0 (defensive; the
	// production path runs ValidateTaskKind upstream).
	if got := taskKindFactor(refactor.TaskKind("unknown-kind")); got != 1.0 {
		t.Fatalf("taskKindFactor(unknown) = %v, want 1.0", got)
	}
}

// TestSanitiseInput_AllBranches pins the helper's branches.
func TestSanitiseInput_AllBranches(t *testing.T) {
	cases := []struct{ in, want float64 }{
		{0, 0},
		{1.5, 1.5},
		{1e6, 1e6},
		{-0.0001, 0},
		{math.NaN(), 0},
		{math.Inf(+1), 0},
		{math.Inf(-1), 0},
	}
	for _, c := range cases {
		got := sanitiseInput(c.in)
		// NaN comparison via IsNaN, not ==.
		if math.IsNaN(c.want) {
			if !math.IsNaN(got) {
				t.Fatalf("sanitiseInput(%v) = %v, want NaN", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("sanitiseInput(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// -----------------------------------------------------------------------------
// Test scaffolding
// -----------------------------------------------------------------------------

// lockedWriter serialises Write calls so the concurrent
// warning-once test's scaffold buffer doesn't race.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
