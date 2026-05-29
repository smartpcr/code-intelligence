package refactor

import (
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/policy/dsl"
	"forge/services/clean-code/internal/policy/steward"
	"forge/services/clean-code/internal/rule_engine"
)

// -----------------------------------------------------------------------------
// Test helpers
// -----------------------------------------------------------------------------

// mustUUID returns a fresh uuid or fails the test. Avoids
// the boilerplate of `if err != nil { t.Fatal... }` at every
// callsite.
func mustUUID(t *testing.T) uuid.UUID {
	t.Helper()
	u, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid.NewV4: %v", err)
	}
	return u
}

// mustParseUUID returns a uuid parsed from s or fails the
// test.
func mustParseUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.FromString(s)
	if err != nil {
		t.Fatalf("uuid.FromString(%q): %v", s, err)
	}
	return u
}

// countingIDFactory returns a uuid factory that emits
// deterministic uuids: uuid_xxxxxxxx-0000-0000-0000-N where
// N counts upward. Used by tests that need byte-identical
// output across runs.
func countingIDFactory() func() (uuid.UUID, error) {
	var counter uint8 = 0
	return func() (uuid.UUID, error) {
		counter++
		var u uuid.UUID
		u[15] = counter
		return u, nil
	}
}

// fixedClock returns a clock that always returns t.
func fixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// pv42 returns a deterministic uuid used as the test's pinned
// policy_version_id (the `hotspot-pins-policy-version` test
// scenario's `pv-42`).
func pv42(t *testing.T) uuid.UUID {
	t.Helper()
	return mustParseUUID(t, "00000000-0000-0000-0000-000000000042")
}

// weightsUnit returns the canonical (1,1,1,1) weight bundle
// used by the formula scenario.
func weightsUnit() steward.RefactorWeights {
	return steward.RefactorWeights{
		Alpha:              1,
		Beta:               1,
		Gamma:              1,
		Delta:              1,
		EffortModelVersion: "v0-test",
		WindowDays:         90,
	}
}

// approxEqual reports whether a and b agree to within eps.
// Used by tests that compare floating-point scores -- never
// `==` because intermediate rounding may differ across
// targets.
func approxEqual(a, b, eps float64) bool {
	if math.IsNaN(a) || math.IsNaN(b) {
		return false
	}
	if math.IsInf(a, 0) || math.IsInf(b, 0) {
		return a == b
	}
	return math.Abs(a-b) <= eps
}

// -----------------------------------------------------------------------------
// hotspot-score-formula -- impl-plan Stage 8.1 Test Scenario #1
// -----------------------------------------------------------------------------

// TestScore_HotspotScoreFormula pins the canonical formula
// from impl-plan Stage 8.1:
//
//	Given z-scores (1.0, 2.0, 0.5) and finding_count=3
//	  with weights (1, 1, 1, 1),
//	When score is computed,
//	Then it equals 1.0 + 2.0 + 0.5 + 3 = 6.5 (round to 2dp).
//
// The scenario test pins the formula directly through the
// public [Score] helper (no z-score computation involved --
// the breakdown values are supplied).
func TestScore_HotspotScoreFormula(t *testing.T) {
	weights := weightsUnit()
	breakdown := Breakdown{
		ComplexityZ:  1.0,
		ChurnZ:       2.0,
		CouplingZ:    0.5,
		FindingCount: 3,
	}
	got := Score(weights, breakdown)
	const want = 6.5
	if !approxEqual(got, want, 1e-9) {
		t.Fatalf("Score = %v, want %v", got, want)
	}
}

// TestScore_RespectsEachWeight verifies the four terms enter
// the sum linearly with their pinned weight. A bug that
// silently swapped beta and gamma would land here.
func TestScore_RespectsEachWeight(t *testing.T) {
	cases := []struct {
		name    string
		weights steward.RefactorWeights
		b       Breakdown
		want    float64
	}{
		{
			name:    "complexity-only",
			weights: steward.RefactorWeights{Alpha: 1, Beta: 0, Gamma: 0, Delta: 0},
			b:       Breakdown{ComplexityZ: 3, ChurnZ: 99, CouplingZ: 99, FindingCount: 99},
			want:    3,
		},
		{
			name:    "churn-only",
			weights: steward.RefactorWeights{Alpha: 0, Beta: 2, Gamma: 0, Delta: 0},
			b:       Breakdown{ComplexityZ: 99, ChurnZ: 4, CouplingZ: 99, FindingCount: 99},
			want:    8,
		},
		{
			name:    "coupling-only",
			weights: steward.RefactorWeights{Alpha: 0, Beta: 0, Gamma: 5, Delta: 0},
			b:       Breakdown{ComplexityZ: 99, ChurnZ: 99, CouplingZ: 2, FindingCount: 99},
			want:    10,
		},
		{
			name:    "finding-only-raw-count",
			weights: steward.RefactorWeights{Alpha: 0, Beta: 0, Gamma: 0, Delta: 1.5},
			b:       Breakdown{ComplexityZ: 99, ChurnZ: 99, CouplingZ: 99, FindingCount: 4},
			want:    6,
		},
		{
			name:    "negative-weights",
			weights: steward.RefactorWeights{Alpha: -1, Beta: 0, Gamma: 0, Delta: 0},
			b:       Breakdown{ComplexityZ: 3, FindingCount: 0},
			want:    -3,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Score(c.weights, c.b)
			if !approxEqual(got, c.want, 1e-9) {
				t.Fatalf("Score = %v, want %v", got, c.want)
			}
		})
	}
}

// TestScore_FindingCountUsesRawNotZ verifies the impl-plan
// pin that the `delta` weight multiplies the RAW finding
// count -- not a z-scored value. A regression that
// accidentally z-scored finding_count would change the score
// even though FindingCount is held constant.
func TestScore_FindingCountUsesRawNotZ(t *testing.T) {
	w := steward.RefactorWeights{Alpha: 0, Beta: 0, Gamma: 0, Delta: 1}
	// All zero z-scores; only finding_count drives the score.
	b := Breakdown{FindingCount: 7}
	got := Score(w, b)
	if got != 7 {
		t.Fatalf("Score = %v, want 7 (raw finding count)", got)
	}
}

// -----------------------------------------------------------------------------
// hotspot-pins-policy-version -- impl-plan Stage 8.1 Test Scenario #2
// -----------------------------------------------------------------------------

// TestComputer_HotspotPinsPolicyVersion pins the canonical
// policy-attribution contract from impl-plan Stage 8.1:
//
//	Given the active policy_version_id='pv-42',
//	When a hot_spot row is written,
//	Then hot_spot.policy_version_id='pv-42' is recorded.
//
// The Computer takes a [PolicySnapshot] (bundling the id
// with the weights so a caller cannot mismatch them) and
// stamps every emitted row's PolicyVersionID from that
// snapshot.
func TestComputer_HotspotPinsPolicyVersion(t *testing.T) {
	pv := pv42(t)
	repo := mustUUID(t)
	scope1 := mustUUID(t)
	scope2 := mustUUID(t)

	c := NewComputer(
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(1_700_000_000, 0).UTC())),
	)

	out, err := c.Compute(
		PolicySnapshot{PolicyVersionID: pv, Weights: weightsUnit()},
		repo,
		"deadbeef",
		[]ScopeInputs{
			{ScopeID: scope1, Cyclo: 10, HasCyclo: true, FindingCount: 1},
			{ScopeID: scope2, Cyclo: 5, HasCyclo: true, FindingCount: 0},
		},
	)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	for i, comp := range out {
		if comp.HotSpot.PolicyVersionID != pv {
			t.Errorf("out[%d].HotSpot.PolicyVersionID = %s, want %s",
				i, comp.HotSpot.PolicyVersionID, pv)
		}
		if comp.HotSpot.RepoID != repo {
			t.Errorf("out[%d].HotSpot.RepoID = %s, want %s",
				i, comp.HotSpot.RepoID, repo)
		}
		if comp.HotSpot.SHA != "deadbeef" {
			t.Errorf("out[%d].HotSpot.SHA = %q, want %q",
				i, comp.HotSpot.SHA, "deadbeef")
		}
		if comp.HotSpot.HotspotID == uuid.Nil {
			t.Errorf("out[%d].HotSpot.HotspotID is nil", i)
		}
	}
}

// -----------------------------------------------------------------------------
// RobustZ tests
// -----------------------------------------------------------------------------

// TestRobustZ_Empty checks the boundary case: no
// distribution, no signal -- return 0.
func TestRobustZ_Empty(t *testing.T) {
	got := RobustZ(nil, 5)
	if got != 0 {
		t.Fatalf("RobustZ(nil, 5) = %v, want 0", got)
	}
	got = RobustZ([]float64{}, 5)
	if got != 0 {
		t.Fatalf("RobustZ([]float64{}, 5) = %v, want 0", got)
	}
}

// TestRobustZ_ConstantDistribution covers the
// constant-distribution edge case: every value is identical
// so MAD AND stddev are both 0 -- no signal, return 0 for
// every query.
func TestRobustZ_ConstantDistribution(t *testing.T) {
	dist := []float64{3, 3, 3, 3, 3}
	got := RobustZ(dist, 3)
	if got != 0 {
		t.Fatalf("RobustZ(const, 3) = %v, want 0", got)
	}
	// Querying with an out-of-distribution value still returns 0
	// because the distribution carries no scale information.
	got = RobustZ(dist, 100)
	if got != 0 {
		t.Fatalf("RobustZ(const, 100) = %v, want 0", got)
	}
}

// TestRobustZ_SparseOutlier covers the rubber-duck design
// review's iter 1 finding #2: a distribution like
// `[0, 0, 0, 100]` has MAD=0 but the outlier `100` is a real
// hotspot signal. The fallback to standard z-score rescues
// it. Verify the outlier produces a positive z-score and the
// medians produce a negative one.
func TestRobustZ_SparseOutlier(t *testing.T) {
	dist := []float64{0, 0, 0, 100}
	// MAD = 0 (median of |0-0|, |0-0|, |0-0|, |100-0| =
	// median(0,0,0,100) = 0). Fallback to standard z-score:
	//   mean = (0+0+0+100)/4 = 25
	//   variance = ((0-25)^2 * 3 + (100-25)^2) / 4 = (1875 + 5625)/4 = 1875
	//   stddev = sqrt(1875) ≈ 43.301
	//   z(100) = (100 - 25) / 43.301 ≈ 1.732
	//   z(0)   = (0 - 25)   / 43.301 ≈ -0.577
	got := RobustZ(dist, 100)
	if !approxEqual(got, 75/math.Sqrt(1875), 1e-9) {
		t.Errorf("RobustZ(sparse, 100) = %v, want ≈ %v",
			got, 75/math.Sqrt(1875))
	}
	got = RobustZ(dist, 0)
	if !approxEqual(got, -25/math.Sqrt(1875), 1e-9) {
		t.Errorf("RobustZ(sparse, 0) = %v, want ≈ %v",
			got, -25/math.Sqrt(1875))
	}
}

// TestRobustZ_TextbookCase pins a textbook MAD scenario.
// Distribution [8, 9, 10, 11, 12]; median = 10; absolute
// deviations [2, 1, 0, 1, 2]; MAD = 1; sigma = 1.4826.
// Query x=12 -> z = (12-10)/1.4826 ≈ 1.349.
func TestRobustZ_TextbookCase(t *testing.T) {
	dist := []float64{8, 9, 10, 11, 12}
	got := RobustZ(dist, 12)
	want := 2.0 / madToSigma
	if !approxEqual(got, want, 1e-9) {
		t.Fatalf("RobustZ(textbook, 12) = %v, want %v", got, want)
	}
}

// TestRobustZ_DoesNotMutateDistribution ensures the helper
// is non-destructive -- the caller's slice is preserved
// exactly. A bug that sorted the input in place would land
// here.
func TestRobustZ_DoesNotMutateDistribution(t *testing.T) {
	dist := []float64{5, 3, 1, 4, 2}
	saved := append([]float64{}, dist...)
	_ = RobustZ(dist, 10)
	if !reflect.DeepEqual(dist, saved) {
		t.Fatalf("input mutated: was %v, now %v", saved, dist)
	}
}

// TestRobustZ_EvenLengthMedian pins the even-length median
// branch (arithmetic mean of the two middle elements).
func TestRobustZ_EvenLengthMedian(t *testing.T) {
	// Distribution [1, 2, 3, 4]; median = 2.5; deviations
	// |1-2.5|=1.5, |2-2.5|=0.5, |3-2.5|=0.5, |4-2.5|=1.5;
	// sorted [0.5,0.5,1.5,1.5]; MAD = (0.5+1.5)/2 = 1.
	dist := []float64{1, 2, 3, 4}
	got := RobustZ(dist, 4)
	want := (4 - 2.5) / madToSigma
	if !approxEqual(got, want, 1e-9) {
		t.Fatalf("RobustZ(even, 4) = %v, want %v", got, want)
	}
}

// -----------------------------------------------------------------------------
// ScopeInputs aggregation tests
// -----------------------------------------------------------------------------

// TestScopeInputs_RawComplexitySum confirms the sum-
// aggregation rule for the complexity dimension (cyclo +
// cognitive_complexity). The architecture spec is silent on
// the operator; the package documents `sum` as the canonical
// choice. A bug that switched to max / mean would land here.
func TestScopeInputs_RawComplexitySum(t *testing.T) {
	in := ScopeInputs{
		Cyclo:                  5,
		HasCyclo:               true,
		CognitiveComplexity:    3,
		HasCognitiveComplexity: true,
	}
	got, ok := in.RawComplexity()
	if !ok || got != 8 {
		t.Fatalf("RawComplexity = (%v, %v), want (8, true)", got, ok)
	}
}

// TestScopeInputs_RawComplexityPartial confirms that when
// only one of the two complexity inputs is present, the
// present value is the raw complexity (no zero-fill that
// would distort the sum).
func TestScopeInputs_RawComplexityPartial(t *testing.T) {
	t.Run("only-cyclo", func(t *testing.T) {
		in := ScopeInputs{Cyclo: 7, HasCyclo: true}
		got, ok := in.RawComplexity()
		if !ok || got != 7 {
			t.Fatalf("got (%v, %v), want (7, true)", got, ok)
		}
	})
	t.Run("only-cognitive", func(t *testing.T) {
		in := ScopeInputs{CognitiveComplexity: 9, HasCognitiveComplexity: true}
		got, ok := in.RawComplexity()
		if !ok || got != 9 {
			t.Fatalf("got (%v, %v), want (9, true)", got, ok)
		}
	})
	t.Run("neither", func(t *testing.T) {
		in := ScopeInputs{}
		got, ok := in.RawComplexity()
		if ok || got != 0 {
			t.Fatalf("got (%v, %v), want (0, false)", got, ok)
		}
	})
}

// TestScopeInputs_RawCouplingSum mirrors the
// complexity-sum test for the coupling dimension
// (coupling_between_objects + fan_out).
func TestScopeInputs_RawCouplingSum(t *testing.T) {
	in := ScopeInputs{
		CouplingBetweenObjects:    4,
		HasCouplingBetweenObjects: true,
		FanOut:                    6,
		HasFanOut:                 true,
	}
	got, ok := in.RawCoupling()
	if !ok || got != 10 {
		t.Fatalf("RawCoupling = (%v, %v), want (10, true)", got, ok)
	}
}

// TestScopeInputs_RawChurnSingle confirms the churn
// dimension has a single input (modification_count_in_window).
func TestScopeInputs_RawChurnSingle(t *testing.T) {
	in := ScopeInputs{ModificationCount: 42, HasModificationCount: true}
	got, ok := in.RawChurn()
	if !ok || got != 42 {
		t.Fatalf("RawChurn = (%v, %v), want (42, true)", got, ok)
	}
	missing := ScopeInputs{}
	got, ok = missing.RawChurn()
	if ok || got != 0 {
		t.Fatalf("RawChurn(missing) = (%v, %v), want (0, false)", got, ok)
	}
}

// -----------------------------------------------------------------------------
// Delta filter
// -----------------------------------------------------------------------------

// TestIsHotSpotQualifyingDelta pins the canonical filter:
// `new` and `newly_failing` count; `unchanged` and
// `resolved` do not. A regression that accidentally added
// `unchanged` (re-counting chronic findings) or `resolved`
// (inverting the signal) would land here.
func TestIsHotSpotQualifyingDelta(t *testing.T) {
	cases := []struct {
		d    rule_engine.Delta
		want bool
	}{
		{rule_engine.DeltaNew, true},
		{rule_engine.DeltaNewlyFailing, true},
		{rule_engine.DeltaUnchanged, false},
		{rule_engine.DeltaResolved, false},
		{rule_engine.Delta("regression"), false}, // legacy alias must NOT qualify
		{rule_engine.Delta(""), false},
	}
	for _, c := range cases {
		t.Run(string(c.d), func(t *testing.T) {
			got := IsHotSpotQualifyingDelta(c.d)
			if got != c.want {
				t.Fatalf("IsHotSpotQualifyingDelta(%q) = %v, want %v",
					c.d, got, c.want)
			}
		})
	}
}

// TestHotSpotQualifyingDeltas_ClosedSet asserts the exported
// canonical slice carries exactly the two qualifying
// values; a future maintainer who appends `unchanged`
// without thinking through the semantics will trip this
// test.
func TestHotSpotQualifyingDeltas_ClosedSet(t *testing.T) {
	want := []rule_engine.Delta{
		rule_engine.DeltaNew,
		rule_engine.DeltaNewlyFailing,
	}
	if !reflect.DeepEqual(HotSpotQualifyingDeltas, want) {
		t.Fatalf("HotSpotQualifyingDeltas = %v, want %v",
			HotSpotQualifyingDeltas, want)
	}
}

// -----------------------------------------------------------------------------
// Input metric kinds
// -----------------------------------------------------------------------------

// TestHotSpotInputMetricKinds_AreCanonical asserts that every
// metric_kind the hotspot scoring depends on is a member of
// the DSL canon set. A typo here (e.g. `cyclomatic_complexity`
// instead of `cyclo`) would slip past the SQL writer until
// runtime; the canon-guard test catches it at compile time.
func TestHotSpotInputMetricKinds_AreCanonical(t *testing.T) {
	for _, k := range HotSpotInputMetricKinds {
		if !dsl.IsCanonicalMetricKind(k) {
			t.Errorf("metric_kind %q is not in dsl.CanonicalMetricKinds",
				k)
		}
	}
}

// TestHotSpotInputMetricKinds_ExpectedShape pins the closed
// set: cyclo, cognitive_complexity, modification_count_in_window,
// coupling_between_objects, fan_out (five strings, in that
// order matching architecture Sec 1.4.1 rows 1, 6, 12, 9, 8).
func TestHotSpotInputMetricKinds_ExpectedShape(t *testing.T) {
	want := []string{
		"cyclo",
		"cognitive_complexity",
		"modification_count_in_window",
		"coupling_between_objects",
		"fan_out",
	}
	if !reflect.DeepEqual(HotSpotInputMetricKinds, want) {
		t.Fatalf("HotSpotInputMetricKinds = %v, want %v",
			HotSpotInputMetricKinds, want)
	}
}

// -----------------------------------------------------------------------------
// Computer.Compute -- happy path, multi-scope
// -----------------------------------------------------------------------------

// TestComputer_Compute_MultiScopeHappyPath drives a small
// synthetic distribution and verifies:
//   - all scopes appear in the output (no silent drop),
//   - rows are sorted by score DESC, scope_id ASC,
//   - every row carries the snapshot's policy_version_id,
//   - rows in a batch share one CreatedAt,
//   - the highest-score row really is the highest-input row.
func TestComputer_Compute_MultiScopeHappyPath(t *testing.T) {
	pv := mustUUID(t)
	repo := mustUUID(t)

	// Three scopes with deliberately different complexity
	// inputs so the z-scores are non-trivial.
	scopeA := mustParseUUID(t, "00000000-0000-0000-0000-0000000000aa")
	scopeB := mustParseUUID(t, "00000000-0000-0000-0000-0000000000bb")
	scopeC := mustParseUUID(t, "00000000-0000-0000-0000-0000000000cc")
	inputs := []ScopeInputs{
		{ScopeID: scopeA, Cyclo: 30, HasCyclo: true, FindingCount: 2},
		{ScopeID: scopeB, Cyclo: 10, HasCyclo: true, FindingCount: 0},
		{ScopeID: scopeC, Cyclo: 20, HasCyclo: true, FindingCount: 1},
	}
	createdAt := time.Unix(1_700_000_000, 0).UTC()
	c := NewComputer(
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(createdAt)),
	)
	out, err := c.Compute(
		PolicySnapshot{PolicyVersionID: pv, Weights: weightsUnit()},
		repo,
		"abc123",
		inputs,
	)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len(out) = %d, want 3", len(out))
	}
	// Determinism: scope A had the highest cyclo + the most
	// findings, so it should rank first.
	if out[0].HotSpot.ScopeID != scopeA {
		t.Errorf("out[0].ScopeID = %s, want %s", out[0].HotSpot.ScopeID, scopeA)
	}
	// Pinned attributes appear on every row.
	for i, comp := range out {
		if comp.HotSpot.PolicyVersionID != pv {
			t.Errorf("out[%d].PolicyVersionID = %s, want %s",
				i, comp.HotSpot.PolicyVersionID, pv)
		}
		if !comp.HotSpot.CreatedAt.Equal(createdAt) {
			t.Errorf("out[%d].CreatedAt = %v, want %v",
				i, comp.HotSpot.CreatedAt, createdAt)
		}
	}
	// Sort invariant: scores are non-increasing.
	for i := 1; i < len(out); i++ {
		if out[i-1].HotSpot.Score < out[i].HotSpot.Score {
			t.Errorf(
				"sort invariant broken: out[%d].Score=%v < out[%d].Score=%v",
				i-1, out[i-1].HotSpot.Score, i, out[i].HotSpot.Score)
		}
	}
}

// TestComputer_Compute_TieBreakerByScopeID confirms that
// ties in `Score` are broken by `ScopeID` ascending. A
// caller that asserts a specific ordering against equal
// scores depends on this determinism.
func TestComputer_Compute_TieBreakerByScopeID(t *testing.T) {
	pv := mustUUID(t)
	repo := mustUUID(t)
	// Two scopes with identical inputs -> identical scores.
	// ScopeID ordering should pick A before B.
	scopeA := mustParseUUID(t, "00000000-0000-0000-0000-000000000001")
	scopeB := mustParseUUID(t, "00000000-0000-0000-0000-000000000002")
	inputs := []ScopeInputs{
		// Intentionally feed B first so the natural input
		// order is the OPPOSITE of the expected output.
		{ScopeID: scopeB, Cyclo: 10, HasCyclo: true, FindingCount: 1},
		{ScopeID: scopeA, Cyclo: 10, HasCyclo: true, FindingCount: 1},
	}
	c := NewComputer(
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	out, err := c.Compute(
		PolicySnapshot{PolicyVersionID: pv, Weights: weightsUnit()},
		repo,
		"sha",
		inputs,
	)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	if out[0].HotSpot.Score != out[1].HotSpot.Score {
		t.Errorf("expected tied scores, got %v vs %v",
			out[0].HotSpot.Score, out[1].HotSpot.Score)
	}
	if out[0].HotSpot.ScopeID != scopeA {
		t.Errorf("tie-break: out[0].ScopeID = %s, want %s (ScopeID ASC)",
			out[0].HotSpot.ScopeID, scopeA)
	}
}

// TestComputer_Compute_MissingMetricsExcludedFromDistribution
// pins the canonical missing-vs-zero rule from the rubber-
// duck design review (iter 1 finding #1):
//
//	A scope missing a dimension is EXCLUDED from that
//	dimension's distribution AND contributes z=0 for the
//	dimension when its score is composed. Missing != 0.
//
// Setup: 5 scopes. Four have churn=10; the fifth has NO
// churn input. The four-element distribution has median 10
// and MAD 0 -> stddev fallback yields stddev 0 -> z=0 for
// the four present scopes too. The fifth scope contributes
// z=0 (because the dimension is absent on it). All five
// scopes get churn_z=0. The composite score then reflects
// only the cyclo input and finding count, NOT a phantom
// "churn=0 distorts the distribution" effect.
func TestComputer_Compute_MissingMetricsExcludedFromDistribution(t *testing.T) {
	pv := mustUUID(t)
	repo := mustUUID(t)
	scopes := []uuid.UUID{
		mustParseUUID(t, "00000000-0000-0000-0000-00000000a001"),
		mustParseUUID(t, "00000000-0000-0000-0000-00000000a002"),
		mustParseUUID(t, "00000000-0000-0000-0000-00000000a003"),
		mustParseUUID(t, "00000000-0000-0000-0000-00000000a004"),
		mustParseUUID(t, "00000000-0000-0000-0000-00000000a005"),
	}
	// Distribution for churn is built only from the present
	// values. Scope #5 has NO churn -> excluded from the
	// distribution.
	inputs := []ScopeInputs{
		{ScopeID: scopes[0], ModificationCount: 10, HasModificationCount: true,
			Cyclo: 5, HasCyclo: true},
		{ScopeID: scopes[1], ModificationCount: 10, HasModificationCount: true,
			Cyclo: 5, HasCyclo: true},
		{ScopeID: scopes[2], ModificationCount: 10, HasModificationCount: true,
			Cyclo: 5, HasCyclo: true},
		{ScopeID: scopes[3], ModificationCount: 10, HasModificationCount: true,
			Cyclo: 5, HasCyclo: true},
		{ScopeID: scopes[4], Cyclo: 5, HasCyclo: true /* no churn */},
	}
	c := NewComputer(
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	out, err := c.Compute(
		PolicySnapshot{PolicyVersionID: pv, Weights: weightsUnit()},
		repo,
		"sha",
		inputs,
	)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(out) != 5 {
		t.Fatalf("len(out) = %d, want 5 (no silent drops)", len(out))
	}
	// Every row's churn_z must be 0 (constant distribution
	// for #1-#4, missing for #5).
	for i, comp := range out {
		if comp.Breakdown.ChurnZ != 0 {
			t.Errorf("out[%d].ChurnZ = %v, want 0", i, comp.Breakdown.ChurnZ)
		}
	}
}

// TestComputer_Compute_EmptyInputReturnsEmpty confirms the
// idle-state behaviour: no scopes -> no rows, no error.
// Calling Compute on an empty input MUST NOT crash or
// fabricate rows.
func TestComputer_Compute_EmptyInputReturnsEmpty(t *testing.T) {
	c := NewComputer()
	out, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		mustUUID(t),
		"sha",
		nil,
	)
	if err != nil {
		t.Fatalf("Compute(empty): %v", err)
	}
	if out != nil {
		t.Fatalf("Compute(empty) = %v, want nil", out)
	}
}

// TestComputer_Compute_SingleScope verifies behaviour with
// exactly one input: the distribution is a singleton so MAD
// is 0 and stddev is 0 -> every z is 0 -> the score is
// dominated by delta * finding_count.
func TestComputer_Compute_SingleScope(t *testing.T) {
	pv := mustUUID(t)
	repo := mustUUID(t)
	scope := mustUUID(t)
	c := NewComputer(
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	out, err := c.Compute(
		PolicySnapshot{PolicyVersionID: pv, Weights: weightsUnit()},
		repo,
		"sha",
		[]ScopeInputs{
			{ScopeID: scope, Cyclo: 999, HasCyclo: true, FindingCount: 4},
		},
	)
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	// Single-element distribution -> z=0 for every dim ->
	// score = delta * finding_count = 1 * 4 = 4.
	if out[0].HotSpot.Score != 4 {
		t.Fatalf("Score = %v, want 4 (singleton -> z=0)", out[0].HotSpot.Score)
	}
}

// TestComputer_Compute_DeterministicAcrossCalls verifies G6:
// identical inputs produce byte-identical output across
// repeated calls when deterministic factories are injected.
// Specifically: ordering, ids, timestamps, breakdown,
// scores -- everything matches.
func TestComputer_Compute_DeterministicAcrossCalls(t *testing.T) {
	pv := mustUUID(t)
	repo := mustUUID(t)
	scopes := []uuid.UUID{
		mustParseUUID(t, "11111111-0000-0000-0000-000000000001"),
		mustParseUUID(t, "11111111-0000-0000-0000-000000000002"),
		mustParseUUID(t, "11111111-0000-0000-0000-000000000003"),
	}
	mk := func() *Computer {
		return NewComputer(
			WithIDFactory(countingIDFactory()),
			WithClock(fixedClock(time.Unix(42, 0).UTC())),
		)
	}
	build := func() []Computation {
		inputs := []ScopeInputs{
			{ScopeID: scopes[0], Cyclo: 5, HasCyclo: true, FindingCount: 1},
			{ScopeID: scopes[1], Cyclo: 10, HasCyclo: true, FindingCount: 0},
			{ScopeID: scopes[2], Cyclo: 7, HasCyclo: true, FindingCount: 2},
		}
		out, err := mk().Compute(
			PolicySnapshot{PolicyVersionID: pv, Weights: weightsUnit()},
			repo, "sha", inputs)
		if err != nil {
			t.Fatalf("Compute: %v", err)
		}
		return out
	}
	a, b := build(), build()
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("Compute is non-deterministic across calls:\n a=%+v\n b=%+v",
			a, b)
	}
}

// -----------------------------------------------------------------------------
// Computer.Compute -- validation failures
// -----------------------------------------------------------------------------

// TestComputer_Compute_RejectsZeroPolicyVersion confirms
// the snapshot validator rejects an unset PolicyVersionID
// rather than emitting hot_spot rows with a zero FK.
func TestComputer_Compute_RejectsZeroPolicyVersion(t *testing.T) {
	c := NewComputer()
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: uuid.Nil, Weights: weightsUnit()},
		mustUUID(t),
		"sha",
		[]ScopeInputs{{ScopeID: mustUUID(t)}},
	)
	if !errors.Is(err, ErrInvalidPolicySnapshot) {
		t.Fatalf("err = %v, want ErrInvalidPolicySnapshot", err)
	}
}

// TestComputer_Compute_RejectsNonFiniteWeight confirms NaN
// / Inf weights are rejected up front -- a poisoned weight
// would propagate through every emitted row.
func TestComputer_Compute_RejectsNonFiniteWeight(t *testing.T) {
	c := NewComputer()
	cases := []struct {
		name string
		w    steward.RefactorWeights
	}{
		{"nan-alpha", steward.RefactorWeights{Alpha: math.NaN()}},
		{"inf-beta", steward.RefactorWeights{Beta: math.Inf(1)}},
		{"neg-inf-gamma", steward.RefactorWeights{Gamma: math.Inf(-1)}},
		{"nan-delta", steward.RefactorWeights{Delta: math.NaN()}},
	}
	for _, c2 := range cases {
		t.Run(c2.name, func(t *testing.T) {
			_, err := c.Compute(
				PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: c2.w},
				mustUUID(t),
				"sha",
				[]ScopeInputs{{ScopeID: mustUUID(t)}},
			)
			if !errors.Is(err, ErrInvalidPolicySnapshot) {
				t.Fatalf("err = %v, want ErrInvalidPolicySnapshot", err)
			}
		})
	}
}

// TestComputer_Compute_RejectsZeroScopeID confirms the
// per-input validator rejects a zero ScopeID up front.
func TestComputer_Compute_RejectsZeroScopeID(t *testing.T) {
	c := NewComputer()
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		mustUUID(t),
		"sha",
		[]ScopeInputs{{ScopeID: uuid.Nil}},
	)
	if !errors.Is(err, ErrInvalidScopeInputs) {
		t.Fatalf("err = %v, want ErrInvalidScopeInputs", err)
	}
}

// TestComputer_Compute_RejectsNegativeFindingCount confirms
// the validator catches a logically-impossible negative
// COUNT. The `finding` table CHECK constraint would catch
// this at INSERT time, but the planner refuses to construct
// the row in the first place.
func TestComputer_Compute_RejectsNegativeFindingCount(t *testing.T) {
	c := NewComputer()
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		mustUUID(t),
		"sha",
		[]ScopeInputs{{ScopeID: mustUUID(t), FindingCount: -1}},
	)
	if !errors.Is(err, ErrInvalidScopeInputs) {
		t.Fatalf("err = %v, want ErrInvalidScopeInputs", err)
	}
}

// TestComputer_Compute_RejectsNonFiniteRawValue confirms the
// per-input validator catches NaN/Inf in present raw
// metrics. A poisoned metric_sample would otherwise propagate
// into the z-score computation.
func TestComputer_Compute_RejectsNonFiniteRawValue(t *testing.T) {
	c := NewComputer()
	cases := []struct {
		name string
		in   ScopeInputs
	}{
		{
			name: "cyclo-nan",
			in:   ScopeInputs{Cyclo: math.NaN(), HasCyclo: true},
		},
		{
			name: "cognitive-inf",
			in:   ScopeInputs{CognitiveComplexity: math.Inf(1), HasCognitiveComplexity: true},
		},
		{
			name: "churn-nan",
			in:   ScopeInputs{ModificationCount: math.NaN(), HasModificationCount: true},
		},
		{
			name: "cbo-inf",
			in:   ScopeInputs{CouplingBetweenObjects: math.Inf(-1), HasCouplingBetweenObjects: true},
		},
		{
			name: "fan-out-nan",
			in:   ScopeInputs{FanOut: math.NaN(), HasFanOut: true},
		},
	}
	for _, c2 := range cases {
		t.Run(c2.name, func(t *testing.T) {
			in := c2.in
			in.ScopeID = mustUUID(t)
			_, err := c.Compute(
				PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
				mustUUID(t),
				"sha",
				[]ScopeInputs{in},
			)
			if !errors.Is(err, ErrInvalidScopeInputs) {
				t.Fatalf("err = %v, want ErrInvalidScopeInputs", err)
			}
		})
	}
}

// TestComputer_Compute_RejectsDuplicateScopeID confirms the
// validator rejects a duplicate scope_id. The score formula
// is per-scope; silently merging two rows would distort the
// distribution and the score.
func TestComputer_Compute_RejectsDuplicateScopeID(t *testing.T) {
	c := NewComputer()
	dup := mustUUID(t)
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		mustUUID(t),
		"sha",
		[]ScopeInputs{
			{ScopeID: dup, Cyclo: 1, HasCyclo: true},
			{ScopeID: dup, Cyclo: 2, HasCyclo: true},
		},
	)
	if !errors.Is(err, ErrDuplicateScopeID) {
		t.Fatalf("err = %v, want ErrDuplicateScopeID", err)
	}
}

// TestComputer_Compute_RejectsEmptyRepoID confirms the
// caller's missing-repo bug surfaces as a clean error rather
// than producing a row that would fail the FK at INSERT.
func TestComputer_Compute_RejectsEmptyRepoID(t *testing.T) {
	c := NewComputer()
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		uuid.Nil,
		"sha",
		[]ScopeInputs{{ScopeID: mustUUID(t)}},
	)
	if !errors.Is(err, ErrEmptyRepoID) {
		t.Fatalf("err = %v, want ErrEmptyRepoID", err)
	}
}

// TestComputer_Compute_RejectsEmptySHA confirms an empty
// commit SHA is rejected -- hot_spot rows are bound to a
// specific commit.
func TestComputer_Compute_RejectsEmptySHA(t *testing.T) {
	c := NewComputer()
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		mustUUID(t),
		"",
		[]ScopeInputs{{ScopeID: mustUUID(t)}},
	)
	if !errors.Is(err, ErrEmptySHA) {
		t.Fatalf("err = %v, want ErrEmptySHA", err)
	}
}

// TestComputer_Compute_NilIDFactoryRejected confirms a
// caller that wired `WithIDFactory(nil)` gets a clean error
// at Compute time rather than a panic.
func TestComputer_Compute_NilIDFactoryRejected(t *testing.T) {
	c := NewComputer(WithIDFactory(nil))
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		mustUUID(t),
		"sha",
		[]ScopeInputs{{ScopeID: mustUUID(t)}},
	)
	if !errors.Is(err, ErrNilIDFactory) {
		t.Fatalf("err = %v, want ErrNilIDFactory", err)
	}
}

// TestComputer_Compute_NilClockRejected confirms the same
// for a nil clock.
func TestComputer_Compute_NilClockRejected(t *testing.T) {
	c := NewComputer(WithClock(nil))
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		mustUUID(t),
		"sha",
		[]ScopeInputs{{ScopeID: mustUUID(t)}},
	)
	if !errors.Is(err, ErrNilClock) {
		t.Fatalf("err = %v, want ErrNilClock", err)
	}
}

// TestComputer_Compute_PropagatesIDFactoryError confirms a
// faulty id factory surfaces the error rather than emitting
// a row with a zero uuid.
func TestComputer_Compute_PropagatesIDFactoryError(t *testing.T) {
	want := errors.New("id-factory-boom")
	c := NewComputer(WithIDFactory(func() (uuid.UUID, error) {
		return uuid.Nil, want
	}))
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		mustUUID(t),
		"sha",
		[]ScopeInputs{{ScopeID: mustUUID(t)}},
	)
	if err == nil || !strings.Contains(err.Error(), "id-factory-boom") {
		t.Fatalf("err = %v, want error containing %q", err, want)
	}
}

// TestComputer_Compute_RejectsNilUUIDFromIDFactory confirms
// the regression case for evaluator iter-1 finding #4: a
// factory that returns `(uuid.Nil, nil)` MUST surface
// [ErrIDFactoryReturnedNil] rather than emit a hot_spot row
// with a zero PRIMARY KEY. (A zero uuid would either fail
// the SQL CHECK or succeed once and collide on the next
// batch -- a particularly nasty silent-data-corruption
// failure mode.)
func TestComputer_Compute_RejectsNilUUIDFromIDFactory(t *testing.T) {
	c := NewComputer(WithIDFactory(func() (uuid.UUID, error) {
		// Note: error is nil, only the uuid is zero.
		return uuid.Nil, nil
	}))
	_, err := c.Compute(
		PolicySnapshot{PolicyVersionID: mustUUID(t), Weights: weightsUnit()},
		mustUUID(t),
		"sha",
		[]ScopeInputs{{ScopeID: mustUUID(t)}},
	)
	if !errors.Is(err, ErrIDFactoryReturnedNil) {
		t.Fatalf("err = %v, want ErrIDFactoryReturnedNil", err)
	}
}

// -----------------------------------------------------------------------------
// Real-world-shape integration: PolicyVersion -> PolicySnapshot
// -----------------------------------------------------------------------------

// TestPolicySnapshot_DerivedFromSteward_PolicyVersion shows
// the canonical composition-root path: read the active
// PolicyVersion from the Steward, build a [PolicySnapshot],
// pass it to the Computer. The test doesn't go through the
// Steward (no SQL store wired in unit tests); it asserts
// that a freshly-constructed [steward.PolicyVersion] +
// [steward.RefactorWeights] cleanly populate the snapshot.
func TestPolicySnapshot_DerivedFromStewardPolicyVersion(t *testing.T) {
	pvID := mustUUID(t)
	freshness := 7200
	pv := steward.PolicyVersion{
		PolicyVersionID: pvID,
		Name:            "default-v1",
		RefactorWeights: steward.RefactorWeights{
			Alpha:                  0.4,
			Beta:                   0.3,
			Gamma:                  0.2,
			Delta:                  0.1,
			EffortModelVersion:     "v0-test",
			WindowDays:             90,
			FreshnessWindowSeconds: &freshness,
		},
	}
	snap := PolicySnapshot{
		PolicyVersionID: pv.PolicyVersionID,
		Weights:         pv.RefactorWeights,
	}

	// Smoke-test: snapshot is accepted by the validator.
	if err := validatePolicySnapshot(snap); err != nil {
		t.Fatalf("validatePolicySnapshot: %v", err)
	}

	c := NewComputer(
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(1, 0).UTC())),
	)
	scope := mustUUID(t)
	out, err := c.Compute(snap, mustUUID(t), "sha",
		[]ScopeInputs{{ScopeID: scope, Cyclo: 10, HasCyclo: true, FindingCount: 2}})
	if err != nil {
		t.Fatalf("Compute: %v", err)
	}
	if out[0].HotSpot.PolicyVersionID != pvID {
		t.Fatalf("HotSpot.PolicyVersionID = %s, want %s",
			out[0].HotSpot.PolicyVersionID, pvID)
	}
	// Singleton distribution + zero z + delta=0.1 + findings=2
	// = 0.2.
	if !approxEqual(out[0].HotSpot.Score, 0.2, 1e-9) {
		t.Fatalf("Score = %v, want 0.2", out[0].HotSpot.Score)
	}
}

// -----------------------------------------------------------------------------
// Sort helper sanity (uuidLess)
// -----------------------------------------------------------------------------

// TestUUIDLess_IsTotalOrdering smoke-tests the unexported
// uuidLess used as the tie-break compare. A bug there
// produces flaky sort behaviour at scale.
func TestUUIDLess_IsTotalOrdering(t *testing.T) {
	a := mustParseUUID(t, "00000000-0000-0000-0000-000000000001")
	b := mustParseUUID(t, "00000000-0000-0000-0000-000000000002")
	if !uuidLess(a, b) {
		t.Errorf("uuidLess(a, b) = false, want true")
	}
	if uuidLess(b, a) {
		t.Errorf("uuidLess(b, a) = true, want false")
	}
	if uuidLess(a, a) {
		t.Errorf("uuidLess(a, a) = true, want false (irreflexive)")
	}
}

// -----------------------------------------------------------------------------
// medianSorted unit checks
// -----------------------------------------------------------------------------

// TestMedianSorted covers edge cases on the small helper:
// empty, odd, even.
func TestMedianSorted(t *testing.T) {
	cases := []struct {
		name   string
		sorted []float64
		want   float64
	}{
		{"empty", nil, 0},
		{"singleton", []float64{42}, 42},
		{"odd", []float64{1, 2, 3, 4, 5}, 3},
		{"even", []float64{1, 2, 3, 4}, 2.5},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Caller pre-sorts; the helper does NOT sort.
			s := append([]float64{}, c.sorted...)
			sort.Float64s(s)
			got := medianSorted(s)
			if got != c.want {
				t.Fatalf("medianSorted(%v) = %v, want %v",
					c.sorted, got, c.want)
			}
		})
	}
}
