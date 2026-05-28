package refactor

// Stage 8.2 tests for the [TaskPlanner], the
// rule_id->TaskKind mapper, the canonical/rejected enum
// validators, and the SQL writer's atomic transaction
// contract. The Stage 8.1 [Planner] tests still live in
// `planner_test.go`; this file adds Stage 8.2-only
// coverage so the two stages can evolve independently.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// -----------------------------------------------------------------------------
// Stage 8.2 test helpers
// -----------------------------------------------------------------------------

// weightsTopN returns a unit-weights bundle with the supplied
// TopN. Used by tests that exercise the top-N truncation
// path. `EffortModelVersion` and `WindowDays` are pinned to
// the same values [weightsUnit] uses so the test corpus
// composites match the Stage 8.1 fixtures.
func weightsTopN(topN int) steward.RefactorWeights {
	w := weightsUnit()
	w.TopN = topN
	return w
}

// failingFindingDetailReader returns a pinned error every
// call. Used to assert [TaskPlanner.Plan] wraps and
// propagates the dependency error.
type failingFindingDetailReader struct{ err error }

func (r failingFindingDetailReader) FindingDetails(
	_ context.Context, _ uuid.UUID, _ string, _ uuid.UUID, _ []uuid.UUID,
) ([]FindingDetail, error) {
	return nil, r.err
}

// failingPlanTaskWriter returns a pinned error from every
// WriteRefactorPlanAndTasks call.
type failingPlanTaskWriter struct{ err error }

func (w failingPlanTaskWriter) WriteRefactorPlanAndTasks(
	_ context.Context, _ RefactorPlan, _ []RefactorTask,
) error {
	return w.err
}

// failingHotSpotReader returns a pinned error from every
// LatestHotSpotsByScore call. Used to assert the
// [TaskPlanner] wraps and surfaces a hot_spot read failure
// (rubber-duck iter-2 finding #3 surface).
type failingHotSpotReader struct{ err error }

func (r failingHotSpotReader) LatestHotSpotsByScore(
	_ context.Context, _ uuid.UUID, _ string, _ uuid.UUID, _ int,
) ([]HotSpot, error) {
	return nil, r.err
}

// -----------------------------------------------------------------------------
// Canonical enum + rejected alias coverage
// -----------------------------------------------------------------------------

// TestCanonicalTaskKinds_AreExactlyTheFiveCanonicalValues pins
// the closed five-value set. A regression that adds a sixth
// value silently here would let the planner emit a kind the
// migration's ENUM type rejects -- the catch is at v1
// authoring time, not at run time. The slice order is the
// architecture Sec 5.5.3 line 1274 declaration order.
func TestCanonicalTaskKinds_AreExactlyTheFiveCanonicalValues(t *testing.T) {
	want := []TaskKind{
		TaskKindSplitClass,
		TaskKindExtractMethod,
		TaskKindInvertDependency,
		TaskKindBreakCycle,
		TaskKindConsolidateDuplication,
	}
	if len(CanonicalTaskKinds) != len(want) {
		t.Fatalf("len(CanonicalTaskKinds) = %d, want %d",
			len(CanonicalTaskKinds), len(want))
	}
	for i, w := range want {
		if CanonicalTaskKinds[i] != w {
			t.Errorf("CanonicalTaskKinds[%d] = %q, want %q",
				i, CanonicalTaskKinds[i], w)
		}
	}
}

// TestValidateTaskKind_AcceptsCanonical is a sanity check.
func TestValidateTaskKind_AcceptsCanonical(t *testing.T) {
	for _, k := range CanonicalTaskKinds {
		if err := ValidateTaskKind(k); err != nil {
			t.Errorf("ValidateTaskKind(%q) = %v, want nil", k, err)
		}
	}
}

// TestValidateTaskKind_RejectsIter3Aliases pins the
// workstream brief's REJECTED set. Each of the six aliases
// MUST yield [ErrRejectedTaskKindAlias]. A regression that
// silently accepts one of these would re-introduce the
// iter-3 drift the planner is meant to refuse.
func TestValidateTaskKind_RejectsIter3Aliases(t *testing.T) {
	aliases := []TaskKind{
		"extract_function",
		"introduce_interface",
		"reduce_inheritance",
		"reduce_coupling",
		"reduce_lcom",
		"reduce_duplication",
	}
	for _, a := range aliases {
		t.Run(string(a), func(t *testing.T) {
			err := ValidateTaskKind(a)
			if !errors.Is(err, ErrRejectedTaskKindAlias) {
				t.Errorf("ValidateTaskKind(%q) = %v, want ErrRejectedTaskKindAlias", a, err)
			}
			// Sanity: also flagged by IsRejectedTaskKindAlias.
			if !IsRejectedTaskKindAlias(a) {
				t.Errorf("IsRejectedTaskKindAlias(%q) = false, want true", a)
			}
		})
	}
}

// TestValidateTaskKind_RejectsUnknown confirms a typo / future
// kind that's NOT in the canonical or rejected set surfaces
// [ErrUnknownTaskKind].
func TestValidateTaskKind_RejectsUnknown(t *testing.T) {
	unknowns := []TaskKind{
		"",
		"split_classs",         // typo of split_class
		"INVERT_DEPENDENCY",    // wrong case
		"future_kind_v2",       // future spec value
		"refactor_to_strategy", // not in any set
	}
	for _, k := range unknowns {
		t.Run(string(k), func(t *testing.T) {
			err := ValidateTaskKind(k)
			if !errors.Is(err, ErrUnknownTaskKind) {
				t.Errorf("ValidateTaskKind(%q) = %v, want ErrUnknownTaskKind", k, err)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// DefaultTaskKindForRule mapping
// -----------------------------------------------------------------------------

// TestDefaultTaskKindForRule_MapsCanonicalRuleFamilies pins
// the per-family mapping table. Each line is one canonical
// rule family from the Stage 5.5 / Stage 6.x rule-pack
// briefs. A regression that drops a mapping or remaps a
// family to the wrong kind fails here.
func TestDefaultTaskKindForRule_MapsCanonicalRuleFamilies(t *testing.T) {
	cases := []struct {
		ruleID string
		want   TaskKind
	}{
		// SRP family -> split_class
		{"solid.srp.lcom4_high", TaskKindSplitClass},
		{"solid.srp.interface_width_high", TaskKindSplitClass},
		{"solid.srp", TaskKindSplitClass},
		// ISP family -> split_class (interface segregation = split fat interface)
		{"solid.isp.client_method_overshare", TaskKindSplitClass},
		// OCP / LSP families -> extract_method
		{"solid.ocp.modified_existing", TaskKindExtractMethod},
		{"solid.lsp.subtype_breaks_postcondition", TaskKindExtractMethod},
		// DIP family -> invert_dependency
		{"solid.dip.depends_on_concrete", TaskKindInvertDependency},
		// decoupling cycles -> break_cycle
		{"decoupling.cycle_member", TaskKindBreakCycle},
		{"decoupling.cycles.size3_or_larger", TaskKindBreakCycle},
		// decoupling duplication -> consolidate_duplication
		{"decoupling.duplication_ratio_high", TaskKindConsolidateDuplication},
		// decoupling coupling family -> invert_dependency
		{"decoupling.coupling.fan_out_high", TaskKindInvertDependency},
		{"decoupling.cbo_high", TaskKindInvertDependency},
		{"decoupling.fan_in_high", TaskKindInvertDependency},
		{"decoupling.fan_out_high", TaskKindInvertDependency},
	}
	for _, tc := range cases {
		t.Run(tc.ruleID, func(t *testing.T) {
			got, ok := DefaultTaskKindForRule(tc.ruleID)
			if !ok {
				t.Fatalf("DefaultTaskKindForRule(%q) returned (_, false); want true",
					tc.ruleID)
			}
			if got != tc.want {
				t.Errorf("DefaultTaskKindForRule(%q) = %q, want %q",
					tc.ruleID, got, tc.want)
			}
		})
	}
}

// TestDefaultTaskKindForRule_UnknownReturnsFalse confirms a
// rule_id outside the canonical families returns (_, false)
// rather than guessing.
func TestDefaultTaskKindForRule_UnknownReturnsFalse(t *testing.T) {
	unknowns := []string{
		"",
		"unknown.family.rule",
		"solid",       // no dot suffix
		"decoupling",  // no dot suffix
		"foo.bar.baz", // not a clean-code family
	}
	for _, u := range unknowns {
		t.Run(u, func(t *testing.T) {
			if _, ok := DefaultTaskKindForRule(u); ok {
				t.Errorf("DefaultTaskKindForRule(%q) returned ok=true; want false", u)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// NewTaskPlanner -- construction-time validation
// -----------------------------------------------------------------------------

// TestNewTaskPlanner_RejectsNilDeps confirms every dependency
// is nil-checked at construction so a composition-root
// wiring bug surfaces immediately. Each branch returns the
// dependency-specific sentinel so callers can disambiguate.
func TestNewTaskPlanner_RejectsNilDeps(t *testing.T) {
	policy := staticPolicyReader{ok: false}
	hsReader := NewInMemoryHotSpotReader()
	details := NewInMemoryFindingDetailReader()
	planWriter := NewInMemoryRefactorPlanTaskWriter()

	cases := []struct {
		name string
		make func() (*TaskPlanner, error)
		want error
	}{
		{
			name: "nil PolicyReader",
			make: func() (*TaskPlanner, error) {
				return NewTaskPlanner(nil, hsReader, details, planWriter)
			},
			want: ErrNilPolicyReader,
		},
		{
			name: "nil HotSpotReader",
			make: func() (*TaskPlanner, error) {
				return NewTaskPlanner(policy, nil, details, planWriter)
			},
			want: ErrNilHotSpotReader,
		},
		{
			name: "nil FindingDetailReader",
			make: func() (*TaskPlanner, error) {
				return NewTaskPlanner(policy, hsReader, nil, planWriter)
			},
			want: ErrNilFindingDetailReader,
		},
		{
			name: "nil RefactorPlanTaskWriter",
			make: func() (*TaskPlanner, error) {
				return NewTaskPlanner(policy, hsReader, details, nil)
			},
			want: ErrNilPlanTaskWriter,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tc.make()
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestNewTaskPlanner_RejectsNilOptionCallbacks confirms a
// caller passing `nil` through any callback option is
// rejected at construction (rubber-duck iter-2 finding #5).
// Each option surfaces its OWN sentinel so the operator
// sees which option was misconfigured.
func TestNewTaskPlanner_RejectsNilOptionCallbacks(t *testing.T) {
	mk := func(opt TaskOption) (*TaskPlanner, error) {
		return NewTaskPlanner(
			staticPolicyReader{ok: false},
			NewInMemoryHotSpotReader(),
			NewInMemoryFindingDetailReader(),
			NewInMemoryRefactorPlanTaskWriter(),
			opt,
		)
	}
	cases := []struct {
		name string
		opt  TaskOption
		want error
	}{
		{"WithTaskIDFactory(nil)", WithTaskIDFactory(nil), ErrNilIDFactoryOption},
		{"WithTaskClock(nil)", WithTaskClock(nil), ErrNilClockOption},
		{"WithRuleKindMapper(nil)", WithRuleKindMapper(nil), ErrNilRuleKindMapper},
		{"WithSummaryFunc(nil)", WithSummaryFunc(nil), ErrNilSummaryFunc},
		{"WithTaskDescriptionFunc(nil)", WithTaskDescriptionFunc(nil), ErrNilTaskDescriptionFunc},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mk(tc.opt)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestNewTaskPlanner_RejectsNonCanonicalDefaultKind confirms
// [WithDefaultKind] with a non-canonical or rejected-alias
// kind is rejected at construction (rubber-duck finding #11).
func TestNewTaskPlanner_RejectsNonCanonicalDefaultKind(t *testing.T) {
	mk := func(k TaskKind) (*TaskPlanner, error) {
		return NewTaskPlanner(
			staticPolicyReader{ok: false},
			NewInMemoryHotSpotReader(),
			NewInMemoryFindingDetailReader(),
			NewInMemoryRefactorPlanTaskWriter(),
			WithDefaultKind(k),
		)
	}
	// Rejected alias.
	if _, err := mk("extract_function"); !errors.Is(err, ErrRejectedTaskKindAlias) {
		t.Errorf("WithDefaultKind(alias) err = %v, want ErrRejectedTaskKindAlias", err)
	}
	// Unknown typo.
	if _, err := mk("typo_kind"); !errors.Is(err, ErrUnknownTaskKind) {
		t.Errorf("WithDefaultKind(typo) err = %v, want ErrUnknownTaskKind", err)
	}
	// Canonical -- must succeed.
	if _, err := mk(TaskKindSplitClass); err != nil {
		t.Errorf("WithDefaultKind(canonical) err = %v, want nil", err)
	}
}

// -----------------------------------------------------------------------------
// TaskPlanner.Plan -- end-to-end orchestration tests
// -----------------------------------------------------------------------------

// taskPlannerFixture bundles the in-memory dependencies a
// TaskPlanner test needs. Reduces boilerplate per test.
//
// Stage 8.2 wiring: the [TaskPlanner] READS existing hot_spot
// rows (it does NOT recompute them). The fixture exposes
// `addHotSpot` to pre-seed rows and `addFindingDetail` to
// pre-seed qualifying finding details. A test wanting the
// full surface calls `seedDefaultBatch(...)` which inserts
// N hot_spots at scores ranging high-to-low.
type taskPlannerFixture struct {
	repoID     uuid.UUID
	sha        string
	wantPVID   uuid.UUID
	batchAt    time.Time
	st         *steward.Steward
	hsReader   *InMemoryHotSpotReader
	details    *InMemoryFindingDetailReader
	planWriter *InMemoryRefactorPlanTaskWriter
}

// newTaskPlannerFixture wires a fresh fixture. The active
// policy carries the supplied weights. The fixture does NOT
// auto-seed hot_spot rows -- the caller invokes
// `seedDefaultBatch` or `addHotSpot` for full control over
// the score ordering and the (scope, rule_id) matrix.
func newTaskPlannerFixture(t *testing.T, w steward.RefactorWeights) *taskPlannerFixture {
	t.Helper()
	st, pvID := inMemoryStewardWithActivePolicy(t, w)
	return &taskPlannerFixture{
		repoID:     mustUUID(t),
		sha:        "feedface",
		wantPVID:   pvID,
		batchAt:    time.Unix(1_700_000_100, 0).UTC(),
		st:         st,
		hsReader:   NewInMemoryHotSpotReader(),
		details:    NewInMemoryFindingDetailReader(),
		planWriter: NewInMemoryRefactorPlanTaskWriter(),
	}
}

// scopeAt returns a deterministic scope_id for the i-th
// scope position (0-indexed). Matches the pattern Stage 8.1
// fixtures use so debug output is consistent.
func (fx *taskPlannerFixture) scopeAt(t *testing.T, i int) uuid.UUID {
	t.Helper()
	return mustParseUUID(t,
		fmt.Sprintf("00000000-0000-0000-0000-0000000000%02x", i+1))
}

// addHotSpot pre-seeds ONE hot_spot row for the given scope
// + score, stamped with the fixture's `batchAt`. Returns
// the inserted row so the test can assert on its HotspotID.
func (fx *taskPlannerFixture) addHotSpot(t *testing.T, scopeID uuid.UUID, score float64) HotSpot {
	t.Helper()
	hs := HotSpot{
		HotspotID:       mustUUID(t),
		RepoID:          fx.repoID,
		SHA:             fx.sha,
		ScopeID:         scopeID,
		Score:           score,
		PolicyVersionID: fx.wantPVID,
		CreatedAt:       fx.batchAt,
	}
	fx.hsReader.Insert(hs)
	return hs
}

// seedDefaultBatch inserts `n` hot_spot rows at descending
// scores. Scope_i gets score = (1000 - i*10); scope 0 wins
// the score-DESC sort. Returns the slice of inserted rows in
// insertion order so tests can map scope -> hotspot_id.
func (fx *taskPlannerFixture) seedDefaultBatch(t *testing.T, n int) []HotSpot {
	t.Helper()
	out := make([]HotSpot, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fx.addHotSpot(t, fx.scopeAt(t, i), float64(1000-i*10)))
	}
	return out
}

// addFindingDetail registers ONE (scope, rule_id) finding
// detail row for the active policy_version. Multiple calls
// for the same (scope, rule_id) are deduped by the detail
// reader.
func (fx *taskPlannerFixture) addFindingDetail(scopeID uuid.UUID, ruleID string) {
	fx.details.Insert(InMemoryFindingWithRule{
		InMemoryFinding: InMemoryFinding{
			RepoID:          fx.repoID,
			SHA:             fx.sha,
			ScopeID:         scopeID,
			PolicyVersionID: fx.wantPVID,
			Delta:           rule_engine.DeltaNew,
		},
		RuleID: ruleID,
	})
}

// newPlanner wires a TaskPlanner against the fixture using
// deterministic ID + clock. `opts` lets a test customise.
func (fx *taskPlannerFixture) newPlanner(t *testing.T, opts ...TaskOption) *TaskPlanner {
	t.Helper()
	base := []TaskOption{
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Unix(1_700_000_200, 0).UTC())),
	}
	planner, err := NewTaskPlanner(
		&StewardPolicyReader{Steward: fx.st},
		fx.hsReader, fx.details, fx.planWriter,
		append(base, opts...)...,
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	return planner
}

// TestTaskPlanner_Plan_HappyPath_TopNTruncatesPlanCoverage
// covers the core Stage 8.2 contract:
//
//   - The TaskPlanner reads the pre-seeded hot_spot batch
//     (it does NOT recompute hot_spots).
//   - Only the top-N hot_spots appear in the plan's
//     `hotspot_ids` JSON array.
//   - Each top-N hot_spot with a qualifying finding emits
//     one task per unique rule_id.
//   - The plan + tasks are written via the atomic writer.
//   - `PolicyVersionID` on every returned hot_spot matches
//     the active policy.
func TestTaskPlanner_Plan_HappyPath_TopNTruncatesPlanCoverage(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(3))
	// 5 hot_spots seeded; score-DESC ranking is
	// {scope0=1000, scope1=990, scope2=980, scope3=970, scope4=960}.
	seeded := fx.seedDefaultBatch(t, 5)
	if len(seeded) != 5 {
		t.Fatalf("seeded = %d, want 5", len(seeded))
	}
	// Assign one canonical rule_id to each scope so the
	// task kind mapping is deterministic. Top-3 scopes
	// (scope0, scope1, scope2) get findings -> tasks.
	// Scopes 3 + 4 are OUTSIDE the top-3 plan window so
	// their findings are not read.
	fx.addFindingDetail(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	fx.addFindingDetail(fx.scopeAt(t, 1), "solid.dip.depends_on_concrete")
	fx.addFindingDetail(fx.scopeAt(t, 2), "decoupling.cycle_member")

	planner := fx.newPlanner(t)

	got, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// (1) PolicyVersionID propagated.
	if got.PolicyVersionID != fx.wantPVID {
		t.Errorf("PolicyVersionID = %s, want %s", got.PolicyVersionID, fx.wantPVID)
	}

	// (2) HotSpots is the TOP-3 (truncated to TopN).
	if len(got.HotSpots) != 3 {
		t.Errorf("len(got.HotSpots) = %d, want 3", len(got.HotSpots))
	}

	// (3) Plan written.
	plans := fx.planWriter.Plans()
	if len(plans) != 1 {
		t.Fatalf("len(plans) = %d, want 1", len(plans))
	}
	plan := plans[0]
	if plan.PlanID == uuid.Nil {
		t.Errorf("plan.PlanID is zero")
	}
	if plan.RepoID != fx.repoID {
		t.Errorf("plan.RepoID = %s, want %s", plan.RepoID, fx.repoID)
	}
	if plan.SHA != fx.sha {
		t.Errorf("plan.SHA = %q, want %q", plan.SHA, fx.sha)
	}

	// (4) `hotspot_ids` has EXACTLY the top-3 hot_spot ids in
	// score-DESC order (the InMemoryHotSpotReader's sort).
	if len(plan.HotspotIDs) != 3 {
		t.Fatalf("len(plan.HotspotIDs) = %d, want 3 (TopN truncation)",
			len(plan.HotspotIDs))
	}
	for i := 0; i < 3; i++ {
		if plan.HotspotIDs[i] != got.HotSpots[i].HotspotID {
			t.Errorf("plan.HotspotIDs[%d] = %s, want %s",
				i, plan.HotspotIDs[i], got.HotSpots[i].HotspotID)
		}
	}

	// (5) summary_md is non-empty and names the sha.
	if plan.SummaryMD == "" {
		t.Errorf("plan.SummaryMD is empty")
	}
	if !strings.Contains(plan.SummaryMD, fx.sha) {
		t.Errorf("plan.SummaryMD = %q, missing sha %q", plan.SummaryMD, fx.sha)
	}

	// (6) Tasks: one per top-3 hot_spot (each scope was
	// assigned exactly one canonical rule).
	tasks := fx.planWriter.Tasks()
	if len(tasks) != 3 {
		t.Fatalf("len(tasks) = %d, want 3", len(tasks))
	}
	wantKindForScope := map[uuid.UUID]TaskKind{
		fx.scopeAt(t, 0): TaskKindSplitClass,
		fx.scopeAt(t, 1): TaskKindInvertDependency,
		fx.scopeAt(t, 2): TaskKindBreakCycle,
	}
	for _, task := range tasks {
		wantKind, ok := wantKindForScope[task.ScopeID]
		if !ok {
			t.Errorf("task for unexpected scope %s", task.ScopeID)
			continue
		}
		if task.Kind != wantKind {
			t.Errorf("task[scope=%s].Kind = %q, want %q",
				task.ScopeID, task.Kind, wantKind)
		}
		if task.PlanID != plan.PlanID {
			t.Errorf("task[scope=%s].PlanID = %s, want %s",
				task.ScopeID, task.PlanID, plan.PlanID)
		}
		if task.TaskID == uuid.Nil {
			t.Errorf("task[scope=%s].TaskID is zero", task.ScopeID)
		}
		if task.EffortHours != 0 {
			t.Errorf("task[scope=%s].EffortHours = %v, want 0 (Stage 8.2 placeholder)",
				task.ScopeID, task.EffortHours)
		}
		if task.RuleID == "" {
			t.Errorf("task[scope=%s].RuleID is empty", task.ScopeID)
		}
		if task.DescriptionMD == "" {
			t.Errorf("task[scope=%s].DescriptionMD is empty", task.ScopeID)
		}
		if !task.CreatedAt.Equal(plan.CreatedAt) {
			t.Errorf("task[scope=%s].CreatedAt = %s, want plan.CreatedAt = %s",
				task.ScopeID, task.CreatedAt, plan.CreatedAt)
		}
	}
}

// TestTaskPlanner_Plan_TopNZeroEmitsAllHotSpots confirms
// TopN=0 means "no truncation" -- all scored hot_spots
// appear in plan.HotspotIDs.
func TestTaskPlanner_Plan_TopNZeroEmitsAllHotSpots(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(0))
	fx.seedDefaultBatch(t, 4)
	for i := 0; i < 4; i++ {
		fx.addFindingDetail(fx.scopeAt(t, i), "solid.srp.lcom4_high")
	}
	planner := fx.newPlanner(t)
	got, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(got.HotSpots) != 4 {
		t.Errorf("len(got.HotSpots) = %d, want 4 (TopN=0 returns all)",
			len(got.HotSpots))
	}
	plans := fx.planWriter.Plans()
	if len(plans) != 1 {
		t.Fatalf("len(plans) = %d, want 1", len(plans))
	}
	if len(plans[0].HotspotIDs) != 4 {
		t.Errorf("len(plan.HotspotIDs) = %d, want 4 (TopN=0 means no truncation)",
			len(plans[0].HotspotIDs))
	}
	if len(fx.planWriter.Tasks()) != 4 {
		t.Errorf("len(tasks) = %d, want 4", len(fx.planWriter.Tasks()))
	}
}

// TestTaskPlanner_Plan_TopNExceedsHotSpotCount confirms a
// TopN larger than the available hot_spot count clamps
// gracefully via the reader (which returns AT MOST the
// requested count).
func TestTaskPlanner_Plan_TopNExceedsHotSpotCount(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(20))
	fx.seedDefaultBatch(t, 3)
	for i := 0; i < 3; i++ {
		fx.addFindingDetail(fx.scopeAt(t, i), "solid.srp.lcom4_high")
	}
	planner := fx.newPlanner(t)
	_, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	plans := fx.planWriter.Plans()
	if len(plans) != 1 {
		t.Fatalf("len(plans) = %d, want 1", len(plans))
	}
	if len(plans[0].HotspotIDs) != 3 {
		t.Errorf("len(plan.HotspotIDs) = %d, want 3 (clamped to hot_spot count)",
			len(plans[0].HotspotIDs))
	}
}

// TestTaskPlanner_Plan_HotspotWithoutFindings_EmitsZeroTasks
// pins rubber-duck Stage 8.2 design review finding #2: a
// hot_spot whose `finding_count` is zero (metric-only signal)
// is STILL listed in `plan.HotspotIDs` but emits ZERO tasks
// -- the planner does NOT fabricate a synthetic rule_id.
func TestTaskPlanner_Plan_HotspotWithoutFindings_EmitsZeroTasks(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(2))
	fx.seedDefaultBatch(t, 2)
	// Scope 0 gets findings; scope 1 has metric-only signal.
	fx.addFindingDetail(fx.scopeAt(t, 0), "solid.srp.lcom4_high")

	planner := fx.newPlanner(t)
	got, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(got.HotSpots) != 2 {
		t.Errorf("len(got.HotSpots) = %d, want 2", len(got.HotSpots))
	}
	plans := fx.planWriter.Plans()
	if len(plans) != 1 {
		t.Fatalf("len(plans) = %d, want 1", len(plans))
	}
	if len(plans[0].HotspotIDs) != 2 {
		t.Errorf("len(plan.HotspotIDs) = %d, want 2", len(plans[0].HotspotIDs))
	}
	tasks := fx.planWriter.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1 (no synthetic rule_id fabrication)",
			len(tasks))
	}
	if tasks[0].ScopeID != fx.scopeAt(t, 0) {
		t.Errorf("task.ScopeID = %s, want scope-0 %s",
			tasks[0].ScopeID, fx.scopeAt(t, 0))
	}
	if tasks[0].RuleID != "solid.srp.lcom4_high" {
		t.Errorf("task.RuleID = %q, want %q",
			tasks[0].RuleID, "solid.srp.lcom4_high")
	}
}

// TestTaskPlanner_Plan_DedupesByScopeAndRule confirms two
// findings of the SAME (scope_id, rule_id) emit ONE task --
// rubber-duck Stage 8.2 design review finding #7.
func TestTaskPlanner_Plan_DedupesByScopeAndRule(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(1))
	fx.seedDefaultBatch(t, 1)
	// Same scope, same rule, multiple firings.
	fx.addFindingDetail(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	fx.addFindingDetail(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	fx.addFindingDetail(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	// Same scope, DIFFERENT rule -- emits a second task.
	fx.addFindingDetail(fx.scopeAt(t, 0), "decoupling.cycle_member")

	planner := fx.newPlanner(t)
	_, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	tasks := fx.planWriter.Tasks()
	if len(tasks) != 2 {
		t.Fatalf("len(tasks) = %d, want 2 (one per unique rule_id)",
			len(tasks))
	}
	gotRules := []string{tasks[0].RuleID, tasks[1].RuleID}
	sort.Strings(gotRules)
	wantRules := []string{"decoupling.cycle_member", "solid.srp.lcom4_high"}
	for i, r := range wantRules {
		if gotRules[i] != r {
			t.Errorf("gotRules[%d] = %q, want %q", i, gotRules[i], r)
		}
	}
}

// TestTaskPlanner_Plan_UnmappedRuleFallsBackToDefaultKind
// confirms an unmapped rule_id falls back to the
// [WithDefaultKind] kind. The default is
// [TaskKindExtractMethod].
func TestTaskPlanner_Plan_UnmappedRuleFallsBackToDefaultKind(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(1))
	fx.seedDefaultBatch(t, 1)
	fx.addFindingDetail(fx.scopeAt(t, 0), "unknown.family.rule")
	planner := fx.newPlanner(t)
	_, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	tasks := fx.planWriter.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if tasks[0].Kind != TaskKindExtractMethod {
		t.Errorf("task.Kind = %q, want %q (default fallback)",
			tasks[0].Kind, TaskKindExtractMethod)
	}
}

// TestTaskPlanner_Plan_RuleMapperOverride confirms
// [WithRuleKindMapper] overrides the default mapping table.
func TestTaskPlanner_Plan_RuleMapperOverride(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(1))
	fx.seedDefaultBatch(t, 1)
	fx.addFindingDetail(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	custom := func(ruleID string) (TaskKind, bool) {
		if ruleID == "solid.srp.lcom4_high" {
			return TaskKindBreakCycle, true
		}
		return "", false
	}
	planner := fx.newPlanner(t, WithRuleKindMapper(custom))
	_, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	tasks := fx.planWriter.Tasks()
	if len(tasks) != 1 {
		t.Fatalf("len(tasks) = %d, want 1", len(tasks))
	}
	if tasks[0].Kind != TaskKindBreakCycle {
		t.Errorf("task.Kind = %q, want %q (custom mapper)",
			tasks[0].Kind, TaskKindBreakCycle)
	}
}

// TestTaskPlanner_Plan_RuleMapperReturnsRejectedAlias_Aborts
// confirms a rule_id mapper that returns a REJECTED alias
// kind aborts the whole batch -- no plan, no tasks written.
// Belt-and-braces for a buggy custom mapper.
func TestTaskPlanner_Plan_RuleMapperReturnsRejectedAlias_Aborts(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(1))
	fx.seedDefaultBatch(t, 1)
	fx.addFindingDetail(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	custom := func(ruleID string) (TaskKind, bool) {
		return "extract_function", true
	}
	planner := fx.newPlanner(t, WithRuleKindMapper(custom))
	_, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if !errors.Is(err, ErrRejectedTaskKindAlias) {
		t.Fatalf("err = %v, want ErrRejectedTaskKindAlias", err)
	}
	if len(fx.planWriter.Plans()) != 0 {
		t.Errorf("len(plans) = %d, want 0 (batch aborted)",
			len(fx.planWriter.Plans()))
	}
	if len(fx.planWriter.Tasks()) != 0 {
		t.Errorf("len(tasks) = %d, want 0 (batch aborted)",
			len(fx.planWriter.Tasks()))
	}
}

// TestTaskPlanner_Plan_NoActivePolicy_ReturnsSentinel confirms
// the Stage 8.1 [ErrNoActivePolicy] sentinel propagates
// unchanged through the Stage 8.2 wrapper -- callers can
// branch on the same sentinel.
func TestTaskPlanner_Plan_NoActivePolicy_ReturnsSentinel(t *testing.T) {
	planner, err := NewTaskPlanner(
		staticPolicyReader{ok: false},
		NewInMemoryHotSpotReader(),
		NewInMemoryFindingDetailReader(),
		NewInMemoryRefactorPlanTaskWriter(),
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Now())),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), mustUUID(t), "sha")
	if !errors.Is(err, ErrNoActivePolicy) {
		t.Fatalf("err = %v, want ErrNoActivePolicy", err)
	}
}

// TestTaskPlanner_Plan_EmptyInput_NoPlanWritten confirms a
// (repo_id, sha) with NO hot_spot rows produces NO plan and
// NO task. The writer is NOT called -- emitting an empty
// plan would be semantically meaningless.
func TestTaskPlanner_Plan_EmptyInput_NoPlanWritten(t *testing.T) {
	st, _ := inMemoryStewardWithActivePolicy(t, weightsTopN(5))
	planWriter := NewInMemoryRefactorPlanTaskWriter()
	planner, err := NewTaskPlanner(
		&StewardPolicyReader{Steward: st},
		NewInMemoryHotSpotReader(),
		NewInMemoryFindingDetailReader(),
		planWriter,
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Unix(1_700_000_300, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	got, err := planner.Plan(context.Background(), mustUUID(t), "deadbeef")
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(got.HotSpots) != 0 {
		t.Errorf("len(got.HotSpots) = %d, want 0", len(got.HotSpots))
	}
	if got.Plan.PlanID != uuid.Nil {
		t.Errorf("got.Plan.PlanID = %s, want uuid.Nil", got.Plan.PlanID)
	}
	if len(planWriter.Plans()) != 0 {
		t.Errorf("len(planWriter.Plans()) = %d, want 0 (writer not called)",
			len(planWriter.Plans()))
	}
}

// TestTaskPlanner_Plan_NegativeTopN_ReturnsErrInvalidTopN
// covers the defensive branch -- composition root wiring
// that bypasses [steward.validatePublishRequest].
func TestTaskPlanner_Plan_NegativeTopN_ReturnsErrInvalidTopN(t *testing.T) {
	w := weightsUnit()
	snap := PolicySnapshot{
		PolicyVersionID: pv42(t),
		Weights:         w,
	}
	snap.Weights.TopN = -1 // bypass steward validation
	planner, err := NewTaskPlanner(
		staticPolicyReader{snap: snap, ok: true},
		NewInMemoryHotSpotReader(),
		NewInMemoryFindingDetailReader(),
		NewInMemoryRefactorPlanTaskWriter(),
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Now())),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), mustUUID(t), "sha")
	if !errors.Is(err, ErrInvalidTopN) {
		t.Fatalf("err = %v, want ErrInvalidTopN", err)
	}
}

// TestTaskPlanner_Plan_HotSpotReaderError_PropagatesAndWraps
// asserts the dependency's error is surfaced with a wrap.
func TestTaskPlanner_Plan_HotSpotReaderError_PropagatesAndWraps(t *testing.T) {
	st, _ := inMemoryStewardWithActivePolicy(t, weightsTopN(2))
	boom := errors.New("hot_spot boom")
	planner, err := NewTaskPlanner(
		&StewardPolicyReader{Steward: st},
		failingHotSpotReader{err: boom},
		NewInMemoryFindingDetailReader(),
		NewInMemoryRefactorPlanTaskWriter(),
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Unix(1_700_000_400, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), mustUUID(t), "deadbeef")
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wraps boom", err)
	}
	if !strings.Contains(err.Error(), "read hot_spot batch") {
		t.Errorf("err = %v, missing 'read hot_spot batch' wrap", err)
	}
}

// TestTaskPlanner_Plan_FindingDetailReaderError_PropagatesAndWraps
// asserts the dependency's error is surfaced with a wrap
// (errors.Is round-trip).
func TestTaskPlanner_Plan_FindingDetailReaderError_PropagatesAndWraps(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(2))
	fx.seedDefaultBatch(t, 1)
	boom := errors.New("detail boom")
	planner, err := NewTaskPlanner(
		&StewardPolicyReader{Steward: fx.st},
		fx.hsReader,
		failingFindingDetailReader{err: boom},
		fx.planWriter,
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Unix(1_700_000_400, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), fx.repoID, fx.sha)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wraps boom", err)
	}
	if !strings.Contains(err.Error(), "read finding_details") {
		t.Errorf("err = %v, missing 'read finding_details' wrap", err)
	}
}

// TestTaskPlanner_Plan_PlanTaskWriterError_PropagatesAndWraps
// asserts a failed atomic write surfaces with a wrap.
func TestTaskPlanner_Plan_PlanTaskWriterError_PropagatesAndWraps(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(1))
	fx.seedDefaultBatch(t, 1)
	fx.addFindingDetail(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	boom := errors.New("plan write boom")

	planner, err := NewTaskPlanner(
		&StewardPolicyReader{Steward: fx.st},
		fx.hsReader,
		fx.details,
		failingPlanTaskWriter{err: boom},
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Unix(1_700_000_500, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), fx.repoID, fx.sha)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want wraps boom", err)
	}
	if !strings.Contains(err.Error(), "write plan+tasks") {
		t.Errorf("err = %v, missing 'write plan+tasks' wrap", err)
	}
}

// TestTaskPlanner_PlanFromSnapshot_BypassesPolicyRead
// pins the race-safe entrypoint (rubber-duck iter-2
// finding #1): PlanFromSnapshot uses the supplied
// snapshot's PolicyVersionID directly instead of re-reading
// the active policy, so a composition root that already
// has a snapshot from Stage 8.1 [Planner.Plan] can pin the
// SAME policy_version for both passes.
func TestTaskPlanner_PlanFromSnapshot_BypassesPolicyRead(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(2))
	fx.seedDefaultBatch(t, 2)
	fx.addFindingDetail(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	fx.addFindingDetail(fx.scopeAt(t, 1), "solid.dip.depends_on_concrete")

	// Build a planner backed by a PolicyReader that would
	// FAIL if invoked -- PlanFromSnapshot must not consult
	// it.
	failingPolicy := staticPolicyReader{err: errors.New("policy reader should not be called")}
	planner, err := NewTaskPlanner(
		failingPolicy,
		fx.hsReader, fx.details, fx.planWriter,
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Unix(1_700_000_600, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}

	// Hand-build the snapshot Stage 8.1 would have returned.
	snap := PolicySnapshot{
		PolicyVersionID: fx.wantPVID,
		Weights:         weightsTopN(2),
	}
	got, err := planner.PlanFromSnapshot(context.Background(), fx.repoID, fx.sha, snap)
	if err != nil {
		t.Fatalf("PlanFromSnapshot: %v", err)
	}
	if got.PolicyVersionID != fx.wantPVID {
		t.Errorf("got.PolicyVersionID = %s, want %s",
			got.PolicyVersionID, fx.wantPVID)
	}
	if len(got.Tasks) != 2 {
		t.Errorf("len(got.Tasks) = %d, want 2", len(got.Tasks))
	}
}

// TestTaskPlanner_PlanFromSnapshot_ZeroPolicyVersionID_Rejected
// confirms the snapshot-input validation surface catches a
// caller that passed a zero-PV snapshot.
func TestTaskPlanner_PlanFromSnapshot_ZeroPolicyVersionID_Rejected(t *testing.T) {
	planner, err := NewTaskPlanner(
		staticPolicyReader{ok: false},
		NewInMemoryHotSpotReader(),
		NewInMemoryFindingDetailReader(),
		NewInMemoryRefactorPlanTaskWriter(),
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Now())),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	_, err = planner.PlanFromSnapshot(context.Background(),
		mustUUID(t), "sha", PolicySnapshot{})
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "PolicyVersionID is zero") {
		t.Errorf("err = %v, missing 'PolicyVersionID is zero' guard", err)
	}
}

// -----------------------------------------------------------------------------
// InMemoryHotSpotReader unit tests
// -----------------------------------------------------------------------------

// TestInMemoryHotSpotReader_PicksLatestBatch confirms the
// reader returns rows whose CreatedAt equals the max across
// the (repo, sha, pv) tuple -- older batches are excluded.
func TestInMemoryHotSpotReader_PicksLatestBatch(t *testing.T) {
	r := NewInMemoryHotSpotReader()
	repoID := mustUUID(t)
	sha := "deadbeef"
	pvID := pv42(t)
	scopeA := mustParseUUID(t, "00000000-0000-0000-0000-0000000000a1")
	scopeB := mustParseUUID(t, "00000000-0000-0000-0000-0000000000b2")
	old := time.Unix(1_700_000_000, 0).UTC()
	latest := time.Unix(1_700_000_100, 0).UTC()

	r.Insert(HotSpot{
		HotspotID: mustUUID(t), RepoID: repoID, SHA: sha,
		ScopeID: scopeA, Score: 999, PolicyVersionID: pvID,
		CreatedAt: old,
	})
	r.Insert(HotSpot{
		HotspotID: mustUUID(t), RepoID: repoID, SHA: sha,
		ScopeID: scopeA, Score: 10, PolicyVersionID: pvID,
		CreatedAt: latest,
	})
	r.Insert(HotSpot{
		HotspotID: mustUUID(t), RepoID: repoID, SHA: sha,
		ScopeID: scopeB, Score: 20, PolicyVersionID: pvID,
		CreatedAt: latest,
	})

	got, err := r.LatestHotSpotsByScore(context.Background(), repoID, sha, pvID, 0)
	if err != nil {
		t.Fatalf("LatestHotSpotsByScore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (latest batch only)", len(got))
	}
	// Score-DESC: scopeB (20) before scopeA (10).
	if got[0].ScopeID != scopeB {
		t.Errorf("got[0].ScopeID = %s, want scopeB %s", got[0].ScopeID, scopeB)
	}
	if got[1].ScopeID != scopeA {
		t.Errorf("got[1].ScopeID = %s, want scopeA %s", got[1].ScopeID, scopeA)
	}
}

// TestInMemoryHotSpotReader_FiltersByPolicyVersion confirms
// rows tagged with a different policy_version are not
// returned (architecture Sec 5.5.1 reproducibility
// invariant).
func TestInMemoryHotSpotReader_FiltersByPolicyVersion(t *testing.T) {
	r := NewInMemoryHotSpotReader()
	repoID := mustUUID(t)
	sha := "deadbeef"
	pvActive := pv42(t)
	pvOther := mustParseUUID(t, "00000000-0000-0000-0000-00000000ff42")
	scope := mustUUID(t)
	now := time.Unix(1_700_000_500, 0).UTC()

	r.Insert(HotSpot{
		HotspotID: mustUUID(t), RepoID: repoID, SHA: sha,
		ScopeID: scope, Score: 100, PolicyVersionID: pvActive,
		CreatedAt: now,
	})
	r.Insert(HotSpot{
		HotspotID: mustUUID(t), RepoID: repoID, SHA: sha,
		ScopeID: scope, Score: 999, PolicyVersionID: pvOther,
		CreatedAt: now,
	})

	got, err := r.LatestHotSpotsByScore(context.Background(), repoID, sha, pvActive, 0)
	if err != nil {
		t.Fatalf("LatestHotSpotsByScore: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (pvOther filtered out)", len(got))
	}
	if got[0].PolicyVersionID != pvActive {
		t.Errorf("got[0].PolicyVersionID = %s, want %s", got[0].PolicyVersionID, pvActive)
	}
}

// TestInMemoryHotSpotReader_TruncatesToTopN confirms the
// reader trims the result to topN when topN > 0.
func TestInMemoryHotSpotReader_TruncatesToTopN(t *testing.T) {
	r := NewInMemoryHotSpotReader()
	repoID := mustUUID(t)
	sha := "deadbeef"
	pvID := pv42(t)
	now := time.Unix(1_700_000_700, 0).UTC()
	for i := 0; i < 5; i++ {
		r.Insert(HotSpot{
			HotspotID:       mustUUID(t),
			RepoID:          repoID,
			SHA:             sha,
			ScopeID:         mustParseUUID(t, fmt.Sprintf("00000000-0000-0000-0000-0000000000%02x", i+1)),
			Score:           float64(100 - i),
			PolicyVersionID: pvID,
			CreatedAt:       now,
		})
	}
	got, err := r.LatestHotSpotsByScore(context.Background(), repoID, sha, pvID, 3)
	if err != nil {
		t.Fatalf("LatestHotSpotsByScore: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len(got) = %d, want 3 (topN truncation)", len(got))
	}
	if got[0].Score != 100 || got[1].Score != 99 || got[2].Score != 98 {
		t.Errorf("got scores = [%v,%v,%v], want [100,99,98]",
			got[0].Score, got[1].Score, got[2].Score)
	}
}

// TestInMemoryHotSpotReader_EmptyResult covers the
// no-hot_spot path (Stage 8.1 has not run yet, or every
// scope was filtered out).
func TestInMemoryHotSpotReader_EmptyResult(t *testing.T) {
	r := NewInMemoryHotSpotReader()
	got, err := r.LatestHotSpotsByScore(context.Background(),
		mustUUID(t), "sha", pv42(t), 5)
	if err != nil {
		t.Fatalf("LatestHotSpotsByScore: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}



// TestInMemoryFindingDetailReader_FiltersByQualifyingDelta
// confirms the in-memory detail reader applies the
// `delta IN ('new','newly_failing')` filter.
func TestInMemoryFindingDetailReader_FiltersByQualifyingDelta(t *testing.T) {
	r := NewInMemoryFindingDetailReader()
	repoID := mustUUID(t)
	sha := "deadbeef"
	pvID := pv42(t)
	scope := mustUUID(t)

	add := func(delta rule_engine.Delta, ruleID string) {
		r.Insert(InMemoryFindingWithRule{
			InMemoryFinding: InMemoryFinding{
				RepoID:          repoID,
				SHA:             sha,
				ScopeID:         scope,
				PolicyVersionID: pvID,
				Delta:           delta,
			},
			RuleID: ruleID,
		})
	}
	add(rule_engine.DeltaNew, "rule.new")
	add(rule_engine.DeltaNewlyFailing, "rule.newly_failing")
	add(rule_engine.DeltaUnchanged, "rule.unchanged")     // filtered out
	add(rule_engine.DeltaResolved, "rule.resolved")       // filtered out

	got, err := r.FindingDetails(context.Background(), repoID, sha, pvID, []uuid.UUID{scope})
	if err != nil {
		t.Fatalf("FindingDetails: %v", err)
	}
	gotRules := make([]string, len(got))
	for i, d := range got {
		gotRules[i] = d.RuleID
	}
	sort.Strings(gotRules)
	want := []string{"rule.new", "rule.newly_failing"}
	if len(gotRules) != len(want) {
		t.Fatalf("len(gotRules) = %d, want %d (gotRules=%v)",
			len(gotRules), len(want), gotRules)
	}
	for i, r := range want {
		if gotRules[i] != r {
			t.Errorf("gotRules[%d] = %q, want %q", i, gotRules[i], r)
		}
	}
}

// TestInMemoryFindingDetailReader_FiltersByPolicyVersion
// confirms a row produced by a non-active policy is excluded.
func TestInMemoryFindingDetailReader_FiltersByPolicyVersion(t *testing.T) {
	r := NewInMemoryFindingDetailReader()
	repoID := mustUUID(t)
	sha := "deadbeef"
	activePV := pv42(t)
	otherPV := mustUUID(t)
	scope := mustUUID(t)

	r.Insert(InMemoryFindingWithRule{
		InMemoryFinding: InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scope,
			PolicyVersionID: activePV, Delta: rule_engine.DeltaNew,
		},
		RuleID: "rule.active",
	})
	r.Insert(InMemoryFindingWithRule{
		InMemoryFinding: InMemoryFinding{
			RepoID: repoID, SHA: sha, ScopeID: scope,
			PolicyVersionID: otherPV, Delta: rule_engine.DeltaNew,
		},
		RuleID: "rule.other_policy",
	})

	got, err := r.FindingDetails(context.Background(), repoID, sha, activePV, []uuid.UUID{scope})
	if err != nil {
		t.Fatalf("FindingDetails: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (only active-policy rows)", len(got))
	}
	if got[0].RuleID != "rule.active" {
		t.Errorf("got[0].RuleID = %q, want %q", got[0].RuleID, "rule.active")
	}
}

// TestInMemoryFindingDetailReader_DedupesByScopeAndRule
// confirms duplicate (scope_id, rule_id) pairs collapse to
// ONE detail row per the rubber-duck Stage 8.2 finding #7.
func TestInMemoryFindingDetailReader_DedupesByScopeAndRule(t *testing.T) {
	r := NewInMemoryFindingDetailReader()
	repoID := mustUUID(t)
	sha := "deadbeef"
	pvID := pv42(t)
	scope := mustUUID(t)
	for i := 0; i < 3; i++ {
		r.Insert(InMemoryFindingWithRule{
			InMemoryFinding: InMemoryFinding{
				RepoID: repoID, SHA: sha, ScopeID: scope,
				PolicyVersionID: pvID, Delta: rule_engine.DeltaNew,
			},
			RuleID: "rule.dup",
		})
	}
	got, err := r.FindingDetails(context.Background(), repoID, sha, pvID, []uuid.UUID{scope})
	if err != nil {
		t.Fatalf("FindingDetails: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1 (dedup)", len(got))
	}
}

// TestInMemoryFindingDetailReader_EmptyScopeIDs returns nil.
func TestInMemoryFindingDetailReader_EmptyScopeIDs(t *testing.T) {
	r := NewInMemoryFindingDetailReader()
	got, err := r.FindingDetails(context.Background(), mustUUID(t), "sha", pv42(t), nil)
	if err != nil {
		t.Fatalf("FindingDetails: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// -----------------------------------------------------------------------------
// Steward TopN publish validation (Stage 8.2 policy field)
// -----------------------------------------------------------------------------
// The publish-shape validation tests live in
// `services/clean-code/internal/policy/steward/topn_test.go`
// (sibling package): they exercise the unexported
// `validatePublishRequest` indirectly via [steward.Steward.Publish]
// and need the steward-package fixtures
// ([newKeysManagerWithMintedKey] etc.) that the
// refactor-package tests cannot import.

