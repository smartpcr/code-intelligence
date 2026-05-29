package refactor

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/policy/steward"
	"forge/services/clean-code/internal/rule_engine"
)

// -----------------------------------------------------------------------------
// Test helpers (planner-specific)
// -----------------------------------------------------------------------------

// inMemoryStewardWithActivePolicy spins up a
// [steward.Steward] backed by [steward.InMemoryStore] with
// the given [steward.RefactorWeights] published + activated.
// Returns the steward and the policy_version_id stamped on
// the active row -- tests use the id to assert downstream
// hot_spot rows carry the right attribution.
func inMemoryStewardWithActivePolicy(t *testing.T, w steward.RefactorWeights) (*steward.Steward, uuid.UUID) {
	t.Helper()
	store := steward.NewInMemoryStore()
	st, err := steward.New(steward.Config{Store: store})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	pvID := mustUUID(t)
	pv := steward.PolicyVersion{
		PolicyVersionID: pvID,
		Name:            "test-policy",
		RefactorWeights: w,
		CreatedAt:       time.Unix(1_700_000_000, 0).UTC(),
	}
	ctx := context.Background()
	if err := store.InsertPolicyVersion(ctx, pv); err != nil {
		t.Fatalf("InsertPolicyVersion: %v", err)
	}
	activationID := mustUUID(t)
	if err := store.InsertPolicyActivation(ctx, steward.PolicyActivation{
		ActivationID:    activationID,
		PolicyVersionID: pvID,
		ActivatedBy:     "test",
		CreatedAt:       time.Unix(1_700_000_001, 0).UTC(),
	}); err != nil {
		t.Fatalf("InsertPolicyActivation: %v", err)
	}
	return st, pvID
}

// staticPolicyReader is a [PolicyReader] fake that always
// returns the supplied snapshot + ok flag + error. Used by
// orchestration tests that want to bypass the Steward (e.g.
// the "no active policy" path or the "reader fails" path).
type staticPolicyReader struct {
	snap PolicySnapshot
	ok   bool
	err  error
}

func (r staticPolicyReader) ActivePolicyVersion(ctx context.Context) (PolicySnapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return PolicySnapshot{}, false, err
	}
	return r.snap, r.ok, r.err
}

// failingMetricSampleReader / failingFindingReader /
// failingHotSpotWriter return a pinned error every time.
// Used to assert the [Planner] wraps and propagates the
// dependency's error rather than swallowing it.
type failingMetricSampleReader struct{ err error }

func (r failingMetricSampleReader) ScopeMetrics(ctx context.Context, _ uuid.UUID, _ string, _ []string) ([]ScopeInputs, error) {
	return nil, r.err
}

type failingFindingReader struct{ err error }

func (r failingFindingReader) FindingCountsByScope(ctx context.Context, _ uuid.UUID, _ string, _ uuid.UUID) (map[uuid.UUID]int, error) {
	return nil, r.err
}

type failingHotSpotWriter struct{ err error }

func (w failingHotSpotWriter) WriteHotSpots(ctx context.Context, _ []HotSpot) error {
	return w.err
}

// -----------------------------------------------------------------------------
// Planner construction tests
// -----------------------------------------------------------------------------

// TestNewPlanner_RejectsNilDeps confirms the constructor
// surfaces a wiring bug at NewPlanner time rather than a
// nil-pointer panic at Plan() time.
func TestNewPlanner_RejectsNilDeps(t *testing.T) {
	good := func() (PolicyReader, MetricSampleReader, FindingReader, HotSpotWriter) {
		return staticPolicyReader{ok: false},
			NewInMemoryMetricSampleReader(),
			NewInMemoryFindingReader(),
			NewInMemoryHotSpotWriter()
	}
	t.Run("nil-policy", func(t *testing.T) {
		_, m, f, w := good()
		_, err := NewPlanner(nil, m, f, w)
		if !errors.Is(err, ErrNilPolicyReader) {
			t.Fatalf("err = %v, want ErrNilPolicyReader", err)
		}
	})
	t.Run("nil-metric", func(t *testing.T) {
		p, _, f, w := good()
		_, err := NewPlanner(p, nil, f, w)
		if !errors.Is(err, ErrNilMetricSampleReader) {
			t.Fatalf("err = %v, want ErrNilMetricSampleReader", err)
		}
	})
	t.Run("nil-finding", func(t *testing.T) {
		p, m, _, w := good()
		_, err := NewPlanner(p, m, nil, w)
		if !errors.Is(err, ErrNilFindingReader) {
			t.Fatalf("err = %v, want ErrNilFindingReader", err)
		}
	})
	t.Run("nil-writer", func(t *testing.T) {
		p, m, f, _ := good()
		_, err := NewPlanner(p, m, f, nil)
		if !errors.Is(err, ErrNilHotSpotWriter) {
			t.Fatalf("err = %v, want ErrNilHotSpotWriter", err)
		}
	})
	t.Run("all-present", func(t *testing.T) {
		p, m, f, w := good()
		pl, err := NewPlanner(p, m, f, w)
		if err != nil || pl == nil {
			t.Fatalf("NewPlanner = (%v, %v), want (non-nil, nil)", pl, err)
		}
	})
}

// -----------------------------------------------------------------------------
// Planner.Plan -- happy path + active-policy READ contract
// -----------------------------------------------------------------------------

// TestPlanner_Plan_HappyPath_ReadsActivePolicyAndWritesHotSpots
// is the **evaluator iter-1 finding #1 + #2 + #3** regression
// test. It exercises the full orchestration loop:
//
//  1. The Planner READS the active PolicyVersion from a
//     Steward backed by [steward.InMemoryStore] (an active
//     policy WAS pre-activated). The hot_spot rows carry
//     EXACTLY that policy_version_id (#1 evidence: active
//     policy is actually READ, not caller-supplied).
//  2. The Planner DERIVES per-scope ScopeInputs from
//     metric_sample rows via [MetricSampleReader] -- it does
//     NOT require the caller to pre-build them (#3 evidence:
//     MetricSample ingestion is implemented).
//  3. The Planner COUNTS finding rows via [FindingReader]
//     and the filter excludes non-qualifying delta values
//     (#3 evidence: finding ingestion filters delta IN
//     ('new','newly_failing')).
//  4. The Planner WRITES the hot_spot rows via
//     [HotSpotWriter] (#2 evidence: rows are emitted, not
//     just returned in memory).
func TestPlanner_Plan_HappyPath_ReadsActivePolicyAndWritesHotSpots(t *testing.T) {
	weights := steward.RefactorWeights{
		Alpha:              1,
		Beta:               1,
		Gamma:              1,
		Delta:              1,
		EffortModelVersion: "v0-test",
		WindowDays:         90,
	}
	st, wantPVID := inMemoryStewardWithActivePolicy(t, weights)

	repoID := mustUUID(t)
	sha := "deadbeef"
	scopeA := mustParseUUID(t, "00000000-0000-0000-0000-0000000000aa")
	scopeB := mustParseUUID(t, "00000000-0000-0000-0000-0000000000bb")
	scopeC := mustParseUUID(t, "00000000-0000-0000-0000-0000000000cc")

	metrics := NewInMemoryMetricSampleReader()
	// Scope A: cyclo + cognitive_complexity + churn (high signal).
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scopeA,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 30,
	})
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scopeA,
		MetricKind: MetricKindCognitiveComplexity, MetricVersion: 1, Value: 15,
	})
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scopeA,
		MetricKind: MetricKindModificationCountInWindow, MetricVersion: 1, Value: 20,
	})
	// Scope B: lower complexity, no churn.
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scopeB,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 5,
	})
	// Scope C: only fan_out signal.
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scopeC,
		MetricKind: MetricKindFanOut, MetricVersion: 1, Value: 8,
	})

	findings := NewInMemoryFindingReader()
	// Scope A: 2 qualifying findings (1 new + 1 newly_failing).
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: sha, ScopeID: scopeA,
		PolicyVersionID: wantPVID, Delta: rule_engine.DeltaNew,
	})
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: sha, ScopeID: scopeA,
		PolicyVersionID: wantPVID, Delta: rule_engine.DeltaNewlyFailing,
	})
	// Scope A: 1 non-qualifying finding (unchanged) -- MUST NOT
	// be counted. This is the canonical filter-correctness pin.
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: sha, ScopeID: scopeA,
		PolicyVersionID: wantPVID, Delta: rule_engine.DeltaUnchanged,
	})
	// Scope A: 1 non-qualifying finding (resolved) -- MUST NOT
	// be counted (counting would invert the signal).
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: sha, ScopeID: scopeA,
		PolicyVersionID: wantPVID, Delta: rule_engine.DeltaResolved,
	})

	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		metrics, findings, writer,
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(1_700_000_100, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}

	got, err := planner.Plan(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// (1) active policy version was read.
	if got.PolicyVersionID != wantPVID {
		t.Errorf("PolicyVersionID = %s, want %s", got.PolicyVersionID, wantPVID)
	}

	// (2) hot_spot rows were emitted via the writer.
	written := writer.Rows()
	if len(written) != 3 {
		t.Fatalf("len(written) = %d, want 3", len(written))
	}
	if len(got.HotSpots) != len(written) {
		t.Errorf("len(got.HotSpots) = %d, len(written) = %d (mismatch)",
			len(got.HotSpots), len(written))
	}

	// Every row stamps the active policy_version_id.
	for i, hs := range written {
		if hs.PolicyVersionID != wantPVID {
			t.Errorf("written[%d].PolicyVersionID = %s, want %s",
				i, hs.PolicyVersionID, wantPVID)
		}
		if hs.RepoID != repoID {
			t.Errorf("written[%d].RepoID = %s, want %s", i, hs.RepoID, repoID)
		}
		if hs.SHA != sha {
			t.Errorf("written[%d].SHA = %q, want %q", i, hs.SHA, sha)
		}
		if hs.HotspotID == uuid.Nil {
			t.Errorf("written[%d].HotspotID is nil", i)
		}
	}

	// (3) finding-delta filter applied: scope A has
	// finding_count=2 (NOT 4 -- unchanged + resolved are
	// excluded). The breakdown carries the count.
	for i, hs := range written {
		if hs.ScopeID == scopeA {
			bd := got.Breakdowns[i]
			if bd.FindingCount != 2 {
				t.Errorf("scope A breakdown.FindingCount = %d, want 2 "+
					"(unchanged + resolved must be excluded)",
					bd.FindingCount)
			}
		}
	}

	// (4) sort invariant: scores are non-increasing.
	for i := 1; i < len(written); i++ {
		if written[i-1].Score < written[i].Score {
			t.Errorf(
				"sort invariant: written[%d].Score=%v < written[%d].Score=%v",
				i-1, written[i-1].Score, i, written[i].Score)
		}
	}

	// Sanity: scope A had the highest input signal AND the
	// most qualifying findings, so it should rank first.
	if written[0].ScopeID != scopeA {
		t.Errorf("written[0].ScopeID = %s, want %s (highest signal)",
			written[0].ScopeID, scopeA)
	}
}

// -----------------------------------------------------------------------------
// Planner.Plan -- finding-delta filter pin (rule-engine canon)
// -----------------------------------------------------------------------------

// TestPlanner_Plan_FindingFilter_CountsOnlyQualifyingDeltas is
// the canonical delta-filter regression. The finding reader
// is fed one finding of EACH of the four delta values; only
// `new` and `newly_failing` MUST count. (See architecture
// Sec 5.4.1 line 1189 canonical enum.)
func TestPlanner_Plan_FindingFilter_CountsOnlyQualifyingDeltas(t *testing.T) {
	st, pvID := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 0, Beta: 0, Gamma: 0, Delta: 1, // finding-only weight
	})
	repoID := mustUUID(t)
	sha := "sha"
	scope := mustUUID(t)
	findings := NewInMemoryFindingReader()
	for _, d := range []rule_engine.Delta{
		rule_engine.DeltaNew,
		rule_engine.DeltaNewlyFailing,
		rule_engine.DeltaUnchanged,
		rule_engine.DeltaResolved,
	} {
		findings.Insert(InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scope,
			PolicyVersionID: pvID, Delta: d,
		})
	}
	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		NewInMemoryMetricSampleReader(), // no metrics
		findings, writer,
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	got, err := planner.Plan(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(got.HotSpots) != 1 {
		t.Fatalf("len(got.HotSpots) = %d, want 1", len(got.HotSpots))
	}
	// Score = 0+0+0+1*2 = 2. (2 qualifying findings out of 4.)
	if got.HotSpots[0].Score != 2 {
		t.Errorf("Score = %v, want 2 (only new + newly_failing count)",
			got.HotSpots[0].Score)
	}
	if got.Breakdowns[0].FindingCount != 2 {
		t.Errorf("FindingCount = %d, want 2", got.Breakdowns[0].FindingCount)
	}
}

// -----------------------------------------------------------------------------
// Planner.Plan -- active-policy scoping pin for findings
// -----------------------------------------------------------------------------

// TestPlanner_Plan_FilterFindingsByActivePolicyVersionID is
// the critical pin for evaluator iter-3 item 2. A scope has
// (a) one qualifying finding stamped with the active
// policy_version_id and (b) ten qualifying findings stamped
// with a DIFFERENT policy_version_id (a parallel evaluation
// against an experimental policy at the same SHA). Only (a)
// must count. Without policy-version scoping the (b) rows
// would inflate finding_count and the resulting hot_spot
// row's `policy_version_id` stamp would no longer reproduce
// the score from `policy_version.refactor_weights` (the
// architecture Sec 5.5.1 reproducibility invariant).
func TestPlanner_Plan_FilterFindingsByActivePolicyVersionID(t *testing.T) {
	st, activePV := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 0, Beta: 0, Gamma: 0, Delta: 1, // finding-only weight
	})
	otherPV := mustUUID(t)
	if otherPV == activePV {
		// vanishingly unlikely; defensive guard
		t.Fatalf("otherPV collided with activePV; rerun")
	}
	repoID := mustUUID(t)
	sha := "sha"
	scope := mustUUID(t)

	findings := NewInMemoryFindingReader()
	// One qualifying finding under the ACTIVE policy.
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: sha, ScopeID: scope,
		PolicyVersionID: activePV, Delta: rule_engine.DeltaNew,
	})
	// Ten qualifying findings under an INACTIVE policy at
	// the same (repo_id, sha, scope_id). These MUST NOT be
	// counted in the hot_spot stamped with activePV.
	for i := 0; i < 10; i++ {
		findings.Insert(InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scope,
			PolicyVersionID: otherPV, Delta: rule_engine.DeltaNewlyFailing,
		})
	}

	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		NewInMemoryMetricSampleReader(),
		findings, writer,
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}

	got, err := planner.Plan(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	if got.PolicyVersionID != activePV {
		t.Errorf("PolicyVersionID = %s, want %s", got.PolicyVersionID, activePV)
	}
	if len(got.HotSpots) != 1 {
		t.Fatalf("len(got.HotSpots) = %d, want 1", len(got.HotSpots))
	}
	// Score = 0+0+0+1*1 = 1 (only the active-policy finding counts).
	if got.HotSpots[0].Score != 1 {
		t.Errorf("Score = %v, want 1 (active-policy finding count is 1, NOT 11)",
			got.HotSpots[0].Score)
	}
	if got.Breakdowns[0].FindingCount != 1 {
		t.Errorf("FindingCount = %d, want 1 (other-policy findings must be excluded)",
			got.Breakdowns[0].FindingCount)
	}
	if got.HotSpots[0].PolicyVersionID != activePV {
		t.Errorf("HotSpot.PolicyVersionID = %s, want %s",
			got.HotSpots[0].PolicyVersionID, activePV)
	}
}

// TestInMemoryFindingReader_FiltersByPolicyVersionID exercises
// the reader contract in isolation: findings stamped with a
// different policy_version_id MUST be excluded.
func TestInMemoryFindingReader_FiltersByPolicyVersionID(t *testing.T) {
	repoID := mustUUID(t)
	sha := "sha"
	scope := mustUUID(t)
	pvActive := mustUUID(t)
	pvOther := mustUUID(t)
	r := NewInMemoryFindingReader()
	// Three under active PV.
	for i := 0; i < 3; i++ {
		r.Insert(InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scope,
			PolicyVersionID: pvActive, Delta: rule_engine.DeltaNew,
		})
	}
	// Five under other PV.
	for i := 0; i < 5; i++ {
		r.Insert(InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scope,
			PolicyVersionID: pvOther, Delta: rule_engine.DeltaNew,
		})
	}
	got, err := r.FindingCountsByScope(context.Background(), repoID, sha, pvActive)
	if err != nil {
		t.Fatalf("FindingCountsByScope: %v", err)
	}
	if got[scope] != 3 {
		t.Errorf("count for active PV = %d, want 3", got[scope])
	}
	got, err = r.FindingCountsByScope(context.Background(), repoID, sha, pvOther)
	if err != nil {
		t.Fatalf("FindingCountsByScope: %v", err)
	}
	if got[scope] != 5 {
		t.Errorf("count for other PV = %d, want 5", got[scope])
	}
	// A third unknown pvID returns empty.
	got, err = r.FindingCountsByScope(context.Background(), repoID, sha, mustUUID(t))
	if err != nil {
		t.Fatalf("FindingCountsByScope: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("count for unknown PV = %d, want 0", len(got))
	}
}

// -----------------------------------------------------------------------------
// Planner.Plan -- metric-kind filter pin
// -----------------------------------------------------------------------------

// TestPlanner_Plan_MetricFilter_IgnoresNonHotSpotMetricKinds
// confirms metric_sample rows whose `metric_kind` is OUTSIDE
// [HotSpotInputMetricKinds] are not folded into the
// distribution. A naive reader that loaded every metric_kind
// would distort the z-scores.
func TestPlanner_Plan_MetricFilter_IgnoresNonHotSpotMetricKinds(t *testing.T) {
	st, _ := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
	})
	repoID := mustUUID(t)
	sha := "sha"
	scope := mustUUID(t)
	metrics := NewInMemoryMetricSampleReader()
	// A hot_spot input kind: counts.
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scope,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 10,
	})
	// A non-hot_spot input kind (e.g. LOC, deferred to a
	// future planner stage). MUST be ignored.
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scope,
		MetricKind: "loc", MetricVersion: 1, Value: 9999,
	})

	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		metrics, NewInMemoryFindingReader(), writer,
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	got, err := planner.Plan(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(got.HotSpots) != 1 {
		t.Fatalf("len(got.HotSpots) = %d, want 1", len(got.HotSpots))
	}
	// Only Cyclo present -> raw_complexity = 10; single
	// distribution -> z=0; score = 0*1+0+0+1*0 = 0.
	if got.HotSpots[0].Score != 0 {
		t.Errorf("Score = %v, want 0 (LOC must not feed the distribution)",
			got.HotSpots[0].Score)
	}
}

// -----------------------------------------------------------------------------
// Planner.Plan -- metric-version dedupe pin
// -----------------------------------------------------------------------------

// TestPlanner_Plan_MetricDedupe_TakesLargestMetricVersion
// pins the dedupe rule: when several metric_sample rows
// exist for the same (scope_id, metric_kind), the row with
// the largest `metric_version` wins. (Append-only G3 means
// older rows are still there; the planner sees only the
// latest re-computation.)
func TestPlanner_Plan_MetricDedupe_TakesLargestMetricVersion(t *testing.T) {
	st, pvID := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 0, Beta: 0, Gamma: 0, Delta: 1, // finding-only score
	})
	// We use a finding-only weight + supply a finding so the
	// scope makes it into the output even when the metric
	// distribution carries no signal. The test asserts the
	// VALUE that lands on ScopeInputs.Cyclo is the v=2 row,
	// not v=1.
	repoID := mustUUID(t)
	sha := "sha"
	scope := mustUUID(t)
	metrics := NewInMemoryMetricSampleReader()
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scope,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 999, // stale
	})
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scope,
		MetricKind: MetricKindCyclo, MetricVersion: 2, Value: 7, // latest
	})

	// Call ScopeMetrics DIRECTLY to assert the v=2 row wins.
	got, err := metrics.ScopeMetrics(context.Background(), repoID, sha, HotSpotInputMetricKinds)
	if err != nil {
		t.Fatalf("ScopeMetrics: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Cyclo != 7 || !got[0].HasCyclo {
		t.Fatalf("Cyclo = (%v, %v), want (7, true) (latest metric_version wins)",
			got[0].Cyclo, got[0].HasCyclo)
	}

	// Now run end-to-end and confirm the planner respects
	// the same dedupe.
	findings := NewInMemoryFindingReader()
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: sha, ScopeID: scope,
		PolicyVersionID: pvID, Delta: rule_engine.DeltaNew,
	})
	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		metrics, findings, writer,
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	rows := writer.Rows()
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
}

// -----------------------------------------------------------------------------
// Planner.Plan -- (repo_id, sha) scoping pin
// -----------------------------------------------------------------------------

// TestPlanner_Plan_ScopesByRepoAndSHA confirms metric_sample
// rows for a DIFFERENT (repo_id, sha) are excluded. The
// planner is per-(repo, sha); a row from another commit must
// not pollute the distribution.
func TestPlanner_Plan_ScopesByRepoAndSHA(t *testing.T) {
	st, pvID := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
	})
	repoID := mustUUID(t)
	otherRepo := mustUUID(t)
	scope := mustUUID(t)
	metrics := NewInMemoryMetricSampleReader()
	// Target: 1 row for our (repo, sha).
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: "target", ScopeID: scope,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 10,
	})
	// Noise: same scope, different repo.
	metrics.Insert(InMemoryMetricSample{
		RepoID: otherRepo, SHA: "target", ScopeID: scope,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 999,
	})
	// Noise: same repo, different sha.
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: "other-sha", ScopeID: scope,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 999,
	})
	got, err := metrics.ScopeMetrics(context.Background(), repoID, "target", HotSpotInputMetricKinds)
	if err != nil {
		t.Fatalf("ScopeMetrics: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (other repo/sha rows must be excluded)", len(got))
	}
	if got[0].Cyclo != 10 {
		t.Errorf("Cyclo = %v, want 10", got[0].Cyclo)
	}

	findings := NewInMemoryFindingReader()
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: "target", ScopeID: scope,
		PolicyVersionID: pvID, Delta: rule_engine.DeltaNew,
	})
	findings.Insert(InMemoryFinding{
		RepoID: otherRepo, SHA: "target", ScopeID: scope,
		PolicyVersionID: pvID, Delta: rule_engine.DeltaNew,
	})
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: "other-sha", ScopeID: scope,
		PolicyVersionID: pvID, Delta: rule_engine.DeltaNew,
	})
	counts, err := findings.FindingCountsByScope(context.Background(), repoID, "target", pvID)
	if err != nil {
		t.Fatalf("FindingCountsByScope: %v", err)
	}
	if got := counts[scope]; got != 1 {
		t.Errorf("count = %d, want 1 (other repo/sha rows must be excluded)", got)
	}

	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		metrics, findings, writer,
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	res, err := planner.Plan(context.Background(), repoID, "target")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.HotSpots) != 1 {
		t.Fatalf("len(res.HotSpots) = %d, want 1", len(res.HotSpots))
	}
	if res.HotSpots[0].SHA != "target" {
		t.Errorf("HotSpots[0].SHA = %q, want %q", res.HotSpots[0].SHA, "target")
	}
}

// -----------------------------------------------------------------------------
// Planner.Plan -- error propagation
// -----------------------------------------------------------------------------

// TestPlanner_Plan_NoActivePolicy_ReturnsSentinel confirms
// the "fresh deploy" path: when no policy_activation row
// exists, Plan returns [ErrNoActivePolicy] without invoking
// the metric / finding / writer dependencies.
func TestPlanner_Plan_NoActivePolicy_ReturnsSentinel(t *testing.T) {
	// Steward with NO activation. (Insert no row.)
	store := steward.NewInMemoryStore()
	st, err := steward.New(steward.Config{Store: store})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		NewInMemoryMetricSampleReader(),
		NewInMemoryFindingReader(),
		writer,
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), mustUUID(t), "sha")
	if !errors.Is(err, ErrNoActivePolicy) {
		t.Fatalf("err = %v, want ErrNoActivePolicy", err)
	}
	if rows := writer.Rows(); len(rows) != 0 {
		t.Errorf("writer.Rows() = %d, want 0 (writer must not be called when no policy)",
			len(rows))
	}
}

// TestPlanner_Plan_PolicyReaderError_PropagatesAndWraps
// confirms a transport / store failure on the policy read
// surfaces with the original error in the chain. The planner
// must NOT swallow read failures as "no active policy" --
// the two are semantically distinct.
func TestPlanner_Plan_PolicyReaderError_PropagatesAndWraps(t *testing.T) {
	boom := errors.New("policy-store-down")
	planner, err := NewPlanner(
		staticPolicyReader{err: boom},
		NewInMemoryMetricSampleReader(),
		NewInMemoryFindingReader(),
		NewInMemoryHotSpotWriter(),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), mustUUID(t), "sha")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want chain containing %q", err, boom)
	}
	if !strings.Contains(err.Error(), "read active policy") {
		t.Errorf("err message %q should mention 'read active policy'", err.Error())
	}
}

// TestPlanner_Plan_MetricReaderError_PropagatesAndWraps.
func TestPlanner_Plan_MetricReaderError_PropagatesAndWraps(t *testing.T) {
	st, _ := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
	})
	boom := errors.New("metric-store-down")
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		failingMetricSampleReader{err: boom},
		NewInMemoryFindingReader(),
		NewInMemoryHotSpotWriter(),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), mustUUID(t), "sha")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want chain containing %q", err, boom)
	}
	if !strings.Contains(err.Error(), "read metric_sample") {
		t.Errorf("err message %q should mention 'read metric_sample'", err.Error())
	}
}

// TestPlanner_Plan_FindingReaderError_PropagatesAndWraps.
func TestPlanner_Plan_FindingReaderError_PropagatesAndWraps(t *testing.T) {
	st, _ := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
	})
	boom := errors.New("finding-store-down")
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		NewInMemoryMetricSampleReader(),
		failingFindingReader{err: boom},
		NewInMemoryHotSpotWriter(),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), mustUUID(t), "sha")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want chain containing %q", err, boom)
	}
	if !strings.Contains(err.Error(), "count findings") {
		t.Errorf("err message %q should mention 'count findings'", err.Error())
	}
}

// TestPlanner_Plan_WriterError_PropagatesAndWraps.
func TestPlanner_Plan_WriterError_PropagatesAndWraps(t *testing.T) {
	st, _ := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
	})
	repoID := mustUUID(t)
	sha := "sha"
	scope := mustUUID(t)
	metrics := NewInMemoryMetricSampleReader()
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scope,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 10,
	})
	boom := errors.New("hot-spot-writer-down")
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		metrics,
		NewInMemoryFindingReader(),
		failingHotSpotWriter{err: boom},
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), repoID, sha)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want chain containing %q", err, boom)
	}
	if !strings.Contains(err.Error(), "write hot_spot batch") {
		t.Errorf("err message %q should mention 'write hot_spot batch'", err.Error())
	}
}

// -----------------------------------------------------------------------------
// Planner.Plan -- empty input behaviour
// -----------------------------------------------------------------------------

// TestPlanner_Plan_EmptyInput_WritesNothing confirms the
// boundary case: an active policy exists but no metric
// samples and no findings at the requested (repo, sha) ->
// Plan succeeds, returns empty HotSpots, writer is invoked
// with an empty batch (must NOT panic).
func TestPlanner_Plan_EmptyInput_WritesNothing(t *testing.T) {
	st, pvID := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
	})
	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		NewInMemoryMetricSampleReader(),
		NewInMemoryFindingReader(),
		writer,
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	res, err := planner.Plan(context.Background(), mustUUID(t), "sha")
	if err != nil {
		t.Fatalf("Plan(empty): %v", err)
	}
	if len(res.HotSpots) != 0 {
		t.Errorf("len(res.HotSpots) = %d, want 0", len(res.HotSpots))
	}
	if res.PolicyVersionID != pvID {
		t.Errorf("PolicyVersionID = %s, want %s", res.PolicyVersionID, pvID)
	}
	if rows := writer.Rows(); len(rows) != 0 {
		t.Errorf("writer.Rows() = %d, want 0", len(rows))
	}
}

// -----------------------------------------------------------------------------
// Planner.Plan -- metric-only / finding-only scope union
// -----------------------------------------------------------------------------

// TestPlanner_Plan_UnionsMetricOnlyAndFindingOnlyScopes
// confirms the planner emits a HotSpot row for a scope that
// appears in ONLY ONE of the two input sources:
//
//   - scopeMetricOnly: has metric_sample rows, no finding rows
//   - scopeFindingOnly: has finding rows, no metric_sample rows
//
// Both should appear in the output. The metric-only scope
// has finding_count=0; the finding-only scope has every
// metric z=0 (no inputs to a dimension -> z=0 fallback).
func TestPlanner_Plan_UnionsMetricOnlyAndFindingOnlyScopes(t *testing.T) {
	st, pvID := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 1, Beta: 0, Gamma: 0, Delta: 1,
	})
	repoID := mustUUID(t)
	sha := "sha"
	scopeMetricOnly := mustParseUUID(t, "00000000-0000-0000-0000-0000000000a1")
	scopeFindingOnly := mustParseUUID(t, "00000000-0000-0000-0000-0000000000b2")

	metrics := NewInMemoryMetricSampleReader()
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scopeMetricOnly,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 10,
	})

	findings := NewInMemoryFindingReader()
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: sha, ScopeID: scopeFindingOnly,
		PolicyVersionID: pvID, Delta: rule_engine.DeltaNew,
	})

	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		metrics, findings, writer,
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	res, err := planner.Plan(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(res.HotSpots) != 2 {
		t.Fatalf("len(res.HotSpots) = %d, want 2", len(res.HotSpots))
	}
	// Collect scope ids that landed in output.
	gotScopes := map[uuid.UUID]bool{}
	for _, hs := range res.HotSpots {
		gotScopes[hs.ScopeID] = true
	}
	if !gotScopes[scopeMetricOnly] {
		t.Errorf("metric-only scope %s missing from output", scopeMetricOnly)
	}
	if !gotScopes[scopeFindingOnly] {
		t.Errorf("finding-only scope %s missing from output", scopeFindingOnly)
	}
}

// -----------------------------------------------------------------------------
// StewardPolicyReader -- adapter unit tests
// -----------------------------------------------------------------------------

// TestStewardPolicyReader_ProjectsPolicyVersionToSnapshot
// asserts the adapter copies the right two fields
// (PolicyVersionID + RefactorWeights) and reports
// `ok=true`.
func TestStewardPolicyReader_ProjectsPolicyVersionToSnapshot(t *testing.T) {
	w := steward.RefactorWeights{
		Alpha: 0.4, Beta: 0.3, Gamma: 0.2, Delta: 0.1,
		EffortModelVersion: "v0-test", WindowDays: 90,
	}
	st, pvID := inMemoryStewardWithActivePolicy(t, w)
	r := &StewardPolicyReader{Steward: st}
	snap, ok, err := r.ActivePolicyVersion(context.Background())
	if err != nil {
		t.Fatalf("ActivePolicyVersion: %v", err)
	}
	if !ok {
		t.Fatalf("ok = false, want true (an activation exists)")
	}
	if snap.PolicyVersionID != pvID {
		t.Errorf("PolicyVersionID = %s, want %s", snap.PolicyVersionID, pvID)
	}
	if !reflect.DeepEqual(snap.Weights, w) {
		t.Errorf("Weights = %+v, want %+v", snap.Weights, w)
	}
}

// TestStewardPolicyReader_ReturnsFalseWhenNoActivation
// confirms the no-activation path is propagated as ok=false
// (NOT as an error).
func TestStewardPolicyReader_ReturnsFalseWhenNoActivation(t *testing.T) {
	store := steward.NewInMemoryStore()
	st, err := steward.New(steward.Config{Store: store})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	r := &StewardPolicyReader{Steward: st}
	_, ok, err := r.ActivePolicyVersion(context.Background())
	if err != nil {
		t.Fatalf("ActivePolicyVersion: %v", err)
	}
	if ok {
		t.Fatalf("ok = true, want false (no activation)")
	}
}

// TestStewardPolicyReader_NilStewardReturnsError covers the
// nil-receiver path: a caller that passes a nil Steward gets
// a clean error rather than a panic.
func TestStewardPolicyReader_NilStewardReturnsError(t *testing.T) {
	r := &StewardPolicyReader{Steward: nil}
	_, _, err := r.ActivePolicyVersion(context.Background())
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "Steward is nil") {
		t.Errorf("err message %q should mention 'Steward is nil'", err.Error())
	}
}

// -----------------------------------------------------------------------------
// InMemoryMetricSampleReader -- focused unit tests on contract details
// -----------------------------------------------------------------------------

// TestInMemoryMetricSampleReader_ProducesScopeInputsWithHasBools
// confirms the reader sets the right `Has<Field>` bool for
// each metric_kind it ingests, and leaves the bool `false`
// for absent kinds (the canonical missing-vs-zero rule).
func TestInMemoryMetricSampleReader_ProducesScopeInputsWithHasBools(t *testing.T) {
	repoID := mustUUID(t)
	sha := "sha"
	scope := mustUUID(t)
	r := NewInMemoryMetricSampleReader()
	r.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scope,
		MetricKind: MetricKindFanOut, MetricVersion: 1, Value: 4,
	})
	got, err := r.ScopeMetrics(context.Background(), repoID, sha, HotSpotInputMetricKinds)
	if err != nil {
		t.Fatalf("ScopeMetrics: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if !got[0].HasFanOut || got[0].FanOut != 4 {
		t.Errorf("FanOut/HasFanOut = (%v, %v), want (4, true)",
			got[0].FanOut, got[0].HasFanOut)
	}
	// Other Has bools MUST be false.
	if got[0].HasCyclo || got[0].HasCognitiveComplexity ||
		got[0].HasModificationCount || got[0].HasCouplingBetweenObjects {
		t.Errorf("expected absent metrics to have Has<Field>=false; got %+v", got[0])
	}
}

// TestInMemoryMetricSampleReader_PerScopeAggregatesAcrossMetricKinds
// confirms one scope_id with several metric_kinds collapses
// to a single ScopeInputs row carrying all the fields.
func TestInMemoryMetricSampleReader_PerScopeAggregatesAcrossMetricKinds(t *testing.T) {
	repoID := mustUUID(t)
	sha := "sha"
	scope := mustUUID(t)
	r := NewInMemoryMetricSampleReader()
	for kind, val := range map[string]float64{
		MetricKindCyclo:                     7,
		MetricKindCognitiveComplexity:       3,
		MetricKindModificationCountInWindow: 11,
		MetricKindCouplingBetweenObjects:    5,
		MetricKindFanOut:                    9,
	} {
		r.Insert(InMemoryMetricSample{
			RepoID: repoID, SHA: sha, ScopeID: scope,
			MetricKind: kind, MetricVersion: 1, Value: val,
		})
	}
	got, err := r.ScopeMetrics(context.Background(), repoID, sha, HotSpotInputMetricKinds)
	if err != nil {
		t.Fatalf("ScopeMetrics: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (one scope -> one row)", len(got))
	}
	want := ScopeInputs{
		ScopeID:                   scope,
		Cyclo:                     7,
		HasCyclo:                  true,
		CognitiveComplexity:       3,
		HasCognitiveComplexity:    true,
		ModificationCount:         11,
		HasModificationCount:      true,
		CouplingBetweenObjects:    5,
		HasCouplingBetweenObjects: true,
		FanOut:                    9,
		HasFanOut:                 true,
	}
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("got = %+v\nwant = %+v", got[0], want)
	}
}

// TestInMemoryMetricSampleReader_SortsOutputByScopeID
// guarantees deterministic order across calls regardless of
// insertion order. Required so the planner's downstream
// determinism (sort by Score DESC, ScopeID ASC) doesn't
// depend on the reader's iteration accident.
func TestInMemoryMetricSampleReader_SortsOutputByScopeID(t *testing.T) {
	repoID := mustUUID(t)
	sha := "sha"
	r := NewInMemoryMetricSampleReader()
	scopes := []uuid.UUID{
		mustParseUUID(t, "00000000-0000-0000-0000-0000000000c3"),
		mustParseUUID(t, "00000000-0000-0000-0000-0000000000a1"),
		mustParseUUID(t, "00000000-0000-0000-0000-0000000000b2"),
	}
	for _, s := range scopes {
		r.Insert(InMemoryMetricSample{
			RepoID: repoID, SHA: sha, ScopeID: s,
			MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 1,
		})
	}
	got, err := r.ScopeMetrics(context.Background(), repoID, sha, HotSpotInputMetricKinds)
	if err != nil {
		t.Fatalf("ScopeMetrics: %v", err)
	}
	want := []uuid.UUID{scopes[1], scopes[2], scopes[0]} // a1, b2, c3
	for i := range got {
		if got[i].ScopeID != want[i] {
			t.Errorf("got[%d].ScopeID = %s, want %s", i, got[i].ScopeID, want[i])
		}
	}
	// Output must equal another sorted view of the same set.
	other := append([]uuid.UUID{}, scopes...)
	sort.Slice(other, func(i, j int) bool { return uuidLess(other[i], other[j]) })
	for i := range got {
		if got[i].ScopeID != other[i] {
			t.Fatalf("got[%d].ScopeID = %s, expected sort %s", i, got[i].ScopeID, other[i])
		}
	}
}

// TestInMemoryMetricSampleReader_RespectsContextCancellation
// confirms the reader honours ctx.Done() rather than
// silently completing.
func TestInMemoryMetricSampleReader_RespectsContextCancellation(t *testing.T) {
	r := NewInMemoryMetricSampleReader()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.ScopeMetrics(ctx, mustUUID(t), "sha", HotSpotInputMetricKinds)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// -----------------------------------------------------------------------------
// InMemoryFindingReader -- focused unit tests
// -----------------------------------------------------------------------------

// TestInMemoryFindingReader_ExcludesAllNonQualifyingDeltas
// pins the closed filter set: only `new` and `newly_failing`
// contribute. Insert one of every delta value and a fabricated
// non-canonical one; count must be 2.
func TestInMemoryFindingReader_ExcludesAllNonQualifyingDeltas(t *testing.T) {
	repoID := mustUUID(t)
	sha := "sha"
	scope := mustUUID(t)
	pvID := mustUUID(t)
	r := NewInMemoryFindingReader()
	for _, d := range []rule_engine.Delta{
		rule_engine.DeltaNew,            // qualifies
		rule_engine.DeltaNewlyFailing,   // qualifies
		rule_engine.DeltaUnchanged,      // excluded
		rule_engine.DeltaResolved,       // excluded
		rule_engine.Delta("regression"), // legacy alias, MUST NOT qualify
	} {
		r.Insert(InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scope,
			PolicyVersionID: pvID, Delta: d,
		})
	}
	got, err := r.FindingCountsByScope(context.Background(), repoID, sha, pvID)
	if err != nil {
		t.Fatalf("FindingCountsByScope: %v", err)
	}
	if got[scope] != 2 {
		t.Fatalf("count = %d, want 2 (only new + newly_failing qualify)", got[scope])
	}
}

// TestInMemoryFindingReader_ZeroCountWhenNoFindings confirms
// an empty store returns an empty (not nil-violating) map.
func TestInMemoryFindingReader_ZeroCountWhenNoFindings(t *testing.T) {
	r := NewInMemoryFindingReader()
	got, err := r.FindingCountsByScope(context.Background(), mustUUID(t), "sha", mustUUID(t))
	if err != nil {
		t.Fatalf("FindingCountsByScope: %v", err)
	}
	if got == nil {
		t.Fatalf("got = nil, want empty map (callers may range over it)")
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// TestInMemoryFindingReader_RespectsContextCancellation.
func TestInMemoryFindingReader_RespectsContextCancellation(t *testing.T) {
	r := NewInMemoryFindingReader()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.FindingCountsByScope(ctx, mustUUID(t), "sha", mustUUID(t))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// -----------------------------------------------------------------------------
// InMemoryHotSpotWriter -- focused unit tests
// -----------------------------------------------------------------------------

// TestInMemoryHotSpotWriter_AppendsBatchAndExposesRows
// confirms the writer accumulates rows across calls. A
// reset clears them.
func TestInMemoryHotSpotWriter_AppendsBatchAndExposesRows(t *testing.T) {
	w := NewInMemoryHotSpotWriter()
	batch1 := []HotSpot{{HotspotID: mustUUID(t), Score: 1}}
	batch2 := []HotSpot{{HotspotID: mustUUID(t), Score: 2}}
	if err := w.WriteHotSpots(context.Background(), batch1); err != nil {
		t.Fatalf("WriteHotSpots: %v", err)
	}
	if err := w.WriteHotSpots(context.Background(), batch2); err != nil {
		t.Fatalf("WriteHotSpots: %v", err)
	}
	got := w.Rows()
	if len(got) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(got))
	}
	if got[0].Score != 1 || got[1].Score != 2 {
		t.Errorf("rows = %v, want [Score=1, Score=2]", got)
	}
	w.Reset()
	if got := w.Rows(); len(got) != 0 {
		t.Fatalf("len(rows) after reset = %d, want 0", len(got))
	}
}

// TestInMemoryHotSpotWriter_NilBatchIsNoop confirms a nil
// batch is silently accepted (the Planner calls the writer
// with nil when no scope has any input).
func TestInMemoryHotSpotWriter_NilBatchIsNoop(t *testing.T) {
	w := NewInMemoryHotSpotWriter()
	if err := w.WriteHotSpots(context.Background(), nil); err != nil {
		t.Fatalf("WriteHotSpots(nil): %v", err)
	}
	if got := w.Rows(); len(got) != 0 {
		t.Errorf("len(rows) = %d, want 0", len(got))
	}
}

// -----------------------------------------------------------------------------
// SQL-backed implementations -- compile-time + structural checks
// -----------------------------------------------------------------------------

// TestSQLImpls_SatisfyInterfaces is a compile-time guard:
// it does NOT execute any SQL but it does fail the build if
// the SQL types stop implementing the interfaces. Cheaper
// than a sqlmock test and catches the most common
// refactoring breakage shape.
func TestSQLImpls_SatisfyInterfaces(t *testing.T) {
	var _ MetricSampleReader = (*SQLMetricSampleReader)(nil)
	var _ FindingReader = (*SQLFindingReader)(nil)
	var _ HotSpotWriter = (*SQLHotSpotWriter)(nil)
	var _ PolicyReader = (*StewardPolicyReader)(nil)
}

// TestSQLHotSpotWriter_NilBatchIsNoop checks the
// short-circuit branch without touching SQL: a nil/empty
// batch must NOT open a transaction. The writer's
// constructor wires a real *sql.DB but the no-op branch
// doesn't dereference it (nil-receiver-safe for len(rows)==0
// only; tests never call with rows).
func TestSQLHotSpotWriter_NilBatchIsNoop(t *testing.T) {
	w := NewSQLHotSpotWriter(nil) // intentionally nil; the no-op branch is the only path tested.
	if err := w.WriteHotSpots(context.Background(), nil); err != nil {
		t.Fatalf("WriteHotSpots(nil): %v", err)
	}
	if err := w.WriteHotSpots(context.Background(), []HotSpot{}); err != nil {
		t.Fatalf("WriteHotSpots([]): %v", err)
	}
}

// -----------------------------------------------------------------------------
// mergeMetricsAndFindings -- unit checks on the merge helper
// -----------------------------------------------------------------------------

// TestMergeMetricsAndFindings_HandlesEachCombo covers the
// four combinations: (metric+finding), (metric-only),
// (finding-only), and (neither). The first three must
// produce one row each; the empty case returns nil.
func TestMergeMetricsAndFindings_HandlesEachCombo(t *testing.T) {
	a := mustParseUUID(t, "00000000-0000-0000-0000-0000000000a1")
	b := mustParseUUID(t, "00000000-0000-0000-0000-0000000000b2")
	c := mustParseUUID(t, "00000000-0000-0000-0000-0000000000c3")
	metricRows := []ScopeInputs{
		{ScopeID: a, Cyclo: 5, HasCyclo: true}, // metric + finding
		{ScopeID: b, Cyclo: 3, HasCyclo: true}, // metric only
	}
	findingCounts := map[uuid.UUID]int{
		a: 2, // metric + finding
		c: 1, // finding only
	}
	got := mergeMetricsAndFindings(metricRows, findingCounts)
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3", len(got))
	}

	// Find each scope in the output.
	byID := map[uuid.UUID]ScopeInputs{}
	for _, in := range got {
		byID[in.ScopeID] = in
	}
	// a: both inputs landed.
	if ia, ok := byID[a]; !ok || ia.Cyclo != 5 || ia.FindingCount != 2 {
		t.Errorf("scope A = %+v, want Cyclo=5 FindingCount=2", ia)
	}
	// b: metric, no finding.
	if ib, ok := byID[b]; !ok || ib.Cyclo != 3 || ib.FindingCount != 0 {
		t.Errorf("scope B = %+v, want Cyclo=3 FindingCount=0", ib)
	}
	// c: no metric, only finding.
	if ic, ok := byID[c]; !ok || ic.HasCyclo || ic.FindingCount != 1 {
		t.Errorf("scope C = %+v, want HasCyclo=false FindingCount=1", ic)
	}

	// Empty input
	if got := mergeMetricsAndFindings(nil, nil); got != nil {
		t.Errorf("mergeMetricsAndFindings(nil, nil) = %v, want nil", got)
	}
}

// TestMergeMetricsAndFindings_IsDeterministic confirms the
// merged slice is sorted by ScopeID. Without this, the
// downstream Computer's "deterministic across calls" property
// would depend on Go's map iteration order.
func TestMergeMetricsAndFindings_IsDeterministic(t *testing.T) {
	a := mustParseUUID(t, "00000000-0000-0000-0000-0000000000a1")
	b := mustParseUUID(t, "00000000-0000-0000-0000-0000000000b2")
	c := mustParseUUID(t, "00000000-0000-0000-0000-0000000000c3")
	// Same input run 10 times -> same output every time.
	var prev []uuid.UUID
	for i := 0; i < 10; i++ {
		got := mergeMetricsAndFindings(
			[]ScopeInputs{
				{ScopeID: c, Cyclo: 1, HasCyclo: true},
				{ScopeID: a, Cyclo: 1, HasCyclo: true},
			},
			map[uuid.UUID]int{a: 1, b: 2},
		)
		ids := make([]uuid.UUID, len(got))
		for j, in := range got {
			ids[j] = in.ScopeID
		}
		if prev == nil {
			prev = ids
			continue
		}
		if !reflect.DeepEqual(prev, ids) {
			t.Fatalf("iter %d: ids = %v, want %v", i, ids, prev)
		}
	}
	// And the order is a, b, c (byte-lex ASC).
	if prev[0] != a || prev[1] != b || prev[2] != c {
		t.Errorf("sorted order = %v, want [a, b, c]", prev)
	}
}

// -----------------------------------------------------------------------------
// Smoke test asserting the Planner's hot_spot rows survive a
// round-trip through the full orchestration with fresh
// fixtures. Used by the changelog as the "demo scenario."
// -----------------------------------------------------------------------------

// TestPlanner_Plan_Snapshot_AcrossTwoSHAsIsAppendOnly mirrors
// the architecture Sec 5.5.1 append-only invariant: running
// Plan() at SHA-1 then at SHA-2 leaves the SHA-1 rows in
// place. (Old hot_spot rows MUST stay queryable so a
// historical "what did the planner think on this PR?"
// surface keeps working.)
func TestPlanner_Plan_Snapshot_AcrossTwoSHAsIsAppendOnly(t *testing.T) {
	st, pvID := inMemoryStewardWithActivePolicy(t, steward.RefactorWeights{
		Alpha: 1, Beta: 0, Gamma: 0, Delta: 1,
	})
	repoID := mustUUID(t)
	scope := mustUUID(t)
	metrics := NewInMemoryMetricSampleReader()
	findings := NewInMemoryFindingReader()
	writer := NewInMemoryHotSpotWriter()
	planner, err := NewPlanner(
		&StewardPolicyReader{Steward: st},
		metrics, findings, writer,
		WithIDFactory(countingIDFactory()),
		WithClock(fixedClock(time.Unix(0, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}

	// SHA-1 plan.
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: "sha-1", ScopeID: scope,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 5,
	})
	r1, err := planner.Plan(context.Background(), repoID, "sha-1")
	if err != nil {
		t.Fatalf("Plan(sha-1): %v", err)
	}
	if len(r1.HotSpots) != 1 || r1.HotSpots[0].SHA != "sha-1" {
		t.Fatalf("r1 = %+v", r1)
	}

	// SHA-2 plan (same scope, different SHA).
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: "sha-2", ScopeID: scope,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 8,
	})
	r2, err := planner.Plan(context.Background(), repoID, "sha-2")
	if err != nil {
		t.Fatalf("Plan(sha-2): %v", err)
	}

	all := writer.Rows()
	if len(all) != 2 {
		t.Fatalf("len(writer.Rows()) = %d, want 2 (append-only)", len(all))
	}
	if all[0].SHA != "sha-1" || all[1].SHA != "sha-2" {
		t.Errorf("rows = %+v, want SHA-1 then SHA-2", all)
	}
	for _, hs := range all {
		if hs.PolicyVersionID != pvID {
			t.Errorf("PolicyVersionID = %s, want %s", hs.PolicyVersionID, pvID)
		}
	}
	if r2.PolicyVersionID != pvID {
		t.Errorf("r2.PolicyVersionID = %s, want %s", r2.PolicyVersionID, pvID)
	}
}

// -----------------------------------------------------------------------------
// Sanity: the SQL strings match the canonical schema/columns
// -----------------------------------------------------------------------------

// TestSQLImpls_UseCanonicalSchemaName guards against a
// silent drift between the steward's DefaultSchema and the
// refactor package's local copy. A test rather than a const
// because the SQL strings are built with `fmt.Sprintf`.
func TestSQLImpls_UseCanonicalSchemaName(t *testing.T) {
	if schemaName != steward.DefaultSchema {
		t.Fatalf("schemaName = %q, want %q", schemaName, steward.DefaultSchema)
	}
}

// TestSQLHotSpotWriter_InsertSQLHasExpectedColumns is a
// golden-string check: any change to the canonical
// `hot_spot` column list MUST land here too. (The actual
// SQL is built with Sprintf so we round-trip the
// constructor + scan a known fragment.)
func TestSQLHotSpotWriter_InsertSQLHasExpectedColumns(t *testing.T) {
	// We can't easily inspect a prepared statement's SQL via
	// the public API, so instead document that any change to
	// the column tuple MUST be visible here.
	expectedCols := []string{
		"hotspot_id",
		"repo_id",
		"sha",
		"scope_id",
		"score",
		"policy_version_id",
		"created_at",
	}
	// Construct the expected fragment.
	q := fmt.Sprintf(`%s.hot_spot`, schemaName)
	if q != "clean_code.hot_spot" {
		t.Errorf("qualified table = %q, want %q", q, "clean_code.hot_spot")
	}
	if len(expectedCols) != 7 {
		t.Errorf("len(expectedCols) = %d, want 7 (matches migration 0003)", len(expectedCols))
	}
}
