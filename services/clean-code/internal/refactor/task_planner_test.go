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

// failingPlanTaskWriter returns a pinned error from
// WriteRefactorPlanAndTasks. Used to assert [TaskPlanner.Plan]
// surfaces a wrapped error when the atomic write fails.
type failingPlanTaskWriter struct{ err error }

func (w failingPlanTaskWriter) WriteRefactorPlanAndTasks(
	_ context.Context, _ RefactorPlan, _ []RefactorTask,
) error {
	return w.err
}

// -----------------------------------------------------------------------------
// Canonical enum / validator tests
// -----------------------------------------------------------------------------

// TestCanonicalTaskKinds_AreExactlyTheFiveCanonicalValues
// pins the closed enum per architecture Sec 5.5.3 line 1274
// + migration 0003 line 140-146. Adding a sixth value to
// [CanonicalTaskKinds] without coordinating with the
// migration is a schema drift; this test fails loudly.
func TestCanonicalTaskKinds_AreExactlyTheFiveCanonicalValues(t *testing.T) {
	want := []TaskKind{
		"split_class",
		"extract_method",
		"invert_dependency",
		"break_cycle",
		"consolidate_duplication",
	}
	if len(CanonicalTaskKinds) != len(want) {
		t.Fatalf("len(CanonicalTaskKinds) = %d, want %d",
			len(CanonicalTaskKinds), len(want))
	}
	for i, k := range want {
		if CanonicalTaskKinds[i] != k {
			t.Errorf("CanonicalTaskKinds[%d] = %q, want %q",
				i, CanonicalTaskKinds[i], k)
		}
	}
}

// TestValidateTaskKind_AcceptsCanonical confirms every
// canonical value passes the validator. Belt-and-braces for
// the closed enum.
func TestValidateTaskKind_AcceptsCanonical(t *testing.T) {
	for _, k := range CanonicalTaskKinds {
		if err := ValidateTaskKind(k); err != nil {
			t.Errorf("ValidateTaskKind(%q) = %v, want nil", k, err)
		}
	}
}

// TestValidateTaskKind_RejectsIter3Aliases confirms the six
// iter-3 alias values are REJECTED with
// [ErrRejectedTaskKindAlias]. The workstream brief calls out
// this exact set; regression of this test signals drift.
func TestValidateTaskKind_RejectsIter3Aliases(t *testing.T) {
	aliases := []TaskKind{
		"extract_function",
		"introduce_interface",
		"reduce_inheritance",
		"reduce_coupling",
		"reduce_lcom",
		"reduce_duplication",
	}
	for _, k := range aliases {
		err := ValidateTaskKind(k)
		if err == nil {
			t.Errorf("ValidateTaskKind(%q) = nil, want ErrRejectedTaskKindAlias", k)
			continue
		}
		if !errors.Is(err, ErrRejectedTaskKindAlias) {
			t.Errorf("ValidateTaskKind(%q) = %v, want ErrRejectedTaskKindAlias", k, err)
		}
		// The error message MUST name the offending kind so
		// the operator sees actionable feedback.
		if !strings.Contains(err.Error(), string(k)) {
			t.Errorf("ValidateTaskKind(%q) error = %q, missing kind name", k, err.Error())
		}
	}
}

// TestValidateTaskKind_RejectsUnknown confirms an
// unrecognised kind returns [ErrUnknownTaskKind] -- distinct
// from the rejected-alias sentinel so a typo is
// distinguishable from a deliberate iter-3 drift.
func TestValidateTaskKind_RejectsUnknown(t *testing.T) {
	err := ValidateTaskKind("typo_not_in_any_set")
	if err == nil {
		t.Fatalf("ValidateTaskKind = nil, want ErrUnknownTaskKind")
	}
	if !errors.Is(err, ErrUnknownTaskKind) {
		t.Errorf("ValidateTaskKind = %v, want ErrUnknownTaskKind", err)
	}
	if errors.Is(err, ErrRejectedTaskKindAlias) {
		t.Errorf("ValidateTaskKind = %v, must NOT be ErrRejectedTaskKindAlias", err)
	}
}

// -----------------------------------------------------------------------------
// DefaultTaskKindForRule mapping tests
// -----------------------------------------------------------------------------

// TestDefaultTaskKindForRule_MapsCanonicalRuleFamilies pins
// the rule_id -> TaskKind prefix mapping table. The mapping
// is the v0 default; rule-pack authors can override via
// [WithRuleKindMapper] but the default MUST match this
// matrix.
func TestDefaultTaskKindForRule_MapsCanonicalRuleFamilies(t *testing.T) {
	tests := []struct {
		ruleID string
		want   TaskKind
	}{
		// SOLID
		{"solid.srp.lcom4_high", TaskKindSplitClass},
		{"solid.srp.interface_width_high", TaskKindSplitClass},
		{"solid.srp", TaskKindSplitClass},
		{"solid.isp.fat_interface", TaskKindSplitClass},
		{"solid.isp", TaskKindSplitClass},
		{"solid.ocp.modified_existing", TaskKindExtractMethod},
		{"solid.ocp", TaskKindExtractMethod},
		{"solid.lsp.precondition_strengthened", TaskKindExtractMethod},
		{"solid.lsp", TaskKindExtractMethod},
		{"solid.dip.depends_on_concrete", TaskKindInvertDependency},
		{"solid.dip", TaskKindInvertDependency},
		// Decoupling
		{"decoupling.cycle_member", TaskKindBreakCycle},
		{"decoupling.cycles.scc_size", TaskKindBreakCycle},
		{"decoupling.cycles", TaskKindBreakCycle},
		{"decoupling.duplication_ratio_high", TaskKindConsolidateDuplication},
		{"decoupling.duplication", TaskKindConsolidateDuplication},
		{"decoupling.coupling.cbo_high", TaskKindInvertDependency},
		{"decoupling.coupling", TaskKindInvertDependency},
		{"decoupling.cbo_high", TaskKindInvertDependency},
		{"decoupling.fan_in_high", TaskKindInvertDependency},
		{"decoupling.fan_out_high", TaskKindInvertDependency},
	}
	for _, tc := range tests {
		got, ok := DefaultTaskKindForRule(tc.ruleID)
		if !ok {
			t.Errorf("DefaultTaskKindForRule(%q) = (_, false), want true", tc.ruleID)
			continue
		}
		if got != tc.want {
			t.Errorf("DefaultTaskKindForRule(%q) = %q, want %q",
				tc.ruleID, got, tc.want)
		}
	}
}

// TestDefaultTaskKindForRule_UnknownReturnsFalse confirms an
// unmapped rule_id surfaces `(zero, false)` so the caller
// can fall back to the configured default.
func TestDefaultTaskKindForRule_UnknownReturnsFalse(t *testing.T) {
	cases := []string{
		"",
		"unknown.family.rule",
		"solid.unknown",
		"decoupling.unknown",
	}
	for _, ruleID := range cases {
		got, ok := DefaultTaskKindForRule(ruleID)
		if ok {
			t.Errorf("DefaultTaskKindForRule(%q) = (%q, true), want (_, false)",
				ruleID, got)
		}
	}
}

// -----------------------------------------------------------------------------
// NewTaskPlanner construction tests
// -----------------------------------------------------------------------------

// TestNewTaskPlanner_RejectsNilDeps confirms every required
// dependency surfaces a wiring error at NewTaskPlanner time
// rather than a nil-pointer panic at Plan() time.
func TestNewTaskPlanner_RejectsNilDeps(t *testing.T) {
	policy := staticPolicyReader{ok: false}
	metrics := NewInMemoryMetricSampleReader()
	findings := NewInMemoryFindingReader()
	hsWriter := NewInMemoryHotSpotWriter()
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
				return NewTaskPlanner(nil, metrics, findings, hsWriter, details, planWriter)
			},
			want: ErrNilPolicyReader,
		},
		{
			name: "nil MetricSampleReader",
			make: func() (*TaskPlanner, error) {
				return NewTaskPlanner(policy, nil, findings, hsWriter, details, planWriter)
			},
			want: ErrNilMetricSampleReader,
		},
		{
			name: "nil FindingReader",
			make: func() (*TaskPlanner, error) {
				return NewTaskPlanner(policy, metrics, nil, hsWriter, details, planWriter)
			},
			want: ErrNilFindingReader,
		},
		{
			name: "nil HotSpotWriter",
			make: func() (*TaskPlanner, error) {
				return NewTaskPlanner(policy, metrics, findings, nil, details, planWriter)
			},
			want: ErrNilHotSpotWriter,
		},
		{
			name: "nil FindingDetailReader",
			make: func() (*TaskPlanner, error) {
				return NewTaskPlanner(policy, metrics, findings, hsWriter, nil, planWriter)
			},
			want: ErrNilFindingDetailReader,
		},
		{
			name: "nil RefactorPlanTaskWriter",
			make: func() (*TaskPlanner, error) {
				return NewTaskPlanner(policy, metrics, findings, hsWriter, details, nil)
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

// TestNewTaskPlanner_RejectsNonCanonicalDefaultKind confirms
// [WithDefaultKind] with a non-canonical or rejected-alias
// kind is rejected at construction (rubber-duck finding #11).
func TestNewTaskPlanner_RejectsNonCanonicalDefaultKind(t *testing.T) {
	mk := func(k TaskKind) (*TaskPlanner, error) {
		return NewTaskPlanner(
			staticPolicyReader{ok: false},
			NewInMemoryMetricSampleReader(),
			NewInMemoryFindingReader(),
			NewInMemoryHotSpotWriter(),
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
type taskPlannerFixture struct {
	repoID     uuid.UUID
	sha        string
	wantPVID   uuid.UUID
	st         *steward.Steward
	metrics    *InMemoryMetricSampleReader
	findings   *InMemoryFindingReader
	hsWriter   *InMemoryHotSpotWriter
	details    *InMemoryFindingDetailReader
	planWriter *InMemoryRefactorPlanTaskWriter
}

// newTaskPlannerFixture wires a fresh fixture with TopN+
// scopes worth of metric_sample + finding rows, ready for the
// TaskPlanner to consume. `scopes` is the number of distinct
// scopes; finding details are NOT auto-populated -- the
// caller registers them with [taskPlannerFixture.addFinding]
// for full control over the (scope, rule_id) matrix.
func newTaskPlannerFixture(t *testing.T, w steward.RefactorWeights, scopes int) *taskPlannerFixture {
	t.Helper()
	st, pvID := inMemoryStewardWithActivePolicy(t, w)
	fx := &taskPlannerFixture{
		repoID:     mustUUID(t),
		sha:        "feedface",
		wantPVID:   pvID,
		st:         st,
		metrics:    NewInMemoryMetricSampleReader(),
		findings:   NewInMemoryFindingReader(),
		hsWriter:   NewInMemoryHotSpotWriter(),
		details:    NewInMemoryFindingDetailReader(),
		planWriter: NewInMemoryRefactorPlanTaskWriter(),
	}
	for i := 0; i < scopes; i++ {
		scopeID := mustParseUUID(t,
			fmt.Sprintf("00000000-0000-0000-0000-0000000000%02x", i+1))
		// Higher-index scope -> higher signal so the
		// score-DESC ranking is stable across runs.
		base := float64(10 + i*5)
		fx.metrics.Insert(InMemoryMetricSample{
			RepoID: fx.repoID, SHA: fx.sha, ScopeID: scopeID,
			MetricKind: MetricKindCyclo, MetricVersion: 1, Value: base,
		})
		fx.metrics.Insert(InMemoryMetricSample{
			RepoID: fx.repoID, SHA: fx.sha, ScopeID: scopeID,
			MetricKind: MetricKindCognitiveComplexity, MetricVersion: 1, Value: base / 2,
		})
		fx.metrics.Insert(InMemoryMetricSample{
			RepoID: fx.repoID, SHA: fx.sha, ScopeID: scopeID,
			MetricKind: MetricKindModificationCountInWindow, MetricVersion: 1, Value: base,
		})
	}
	return fx
}

// scopeAt returns the scope_id assigned by
// [newTaskPlannerFixture] for the i-th scope (0-indexed).
func (fx *taskPlannerFixture) scopeAt(t *testing.T, i int) uuid.UUID {
	t.Helper()
	return mustParseUUID(t,
		fmt.Sprintf("00000000-0000-0000-0000-0000000000%02x", i+1))
}

// addFinding records ONE finding row for the given scope +
// rule_id. The row is registered with BOTH the count reader
// (so the hot_spot picks up the finding_count signal) AND
// the detail reader (so the task planner picks up the
// rule_id).
func (fx *taskPlannerFixture) addFinding(scopeID uuid.UUID, ruleID string) {
	fx.findings.Insert(InMemoryFinding{
		RepoID:          fx.repoID,
		SHA:             fx.sha,
		ScopeID:         scopeID,
		PolicyVersionID: fx.wantPVID,
		Delta:           rule_engine.DeltaNew,
	})
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
		fx.metrics, fx.findings, fx.hsWriter,
		fx.details, fx.planWriter,
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
//   - All scored hot_spots are persisted (NOT truncated).
//   - Only the top-N hot_spots appear in the plan's
//     `hotspot_ids` JSON array.
//   - Each top-N hot_spot with a qualifying finding emits
//     one task per unique rule_id.
//   - The plan + tasks are written via the atomic writer.
//   - `PolicyVersionID` on every hot_spot matches the
//     active policy.
func TestTaskPlanner_Plan_HappyPath_TopNTruncatesPlanCoverage(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(3), 5)

	// Assign one canonical rule_id to each scope so the
	// task kind mapping is deterministic. Highest-signal
	// scopes (last few indices) get SRP / DIP / CYCLES so
	// the expected task list is predictable.
	fx.addFinding(fx.scopeAt(t, 4), "solid.srp.lcom4_high") // top-1 score
	fx.addFinding(fx.scopeAt(t, 3), "solid.dip.depends_on_concrete")
	fx.addFinding(fx.scopeAt(t, 2), "decoupling.cycle_member")
	// Scopes 0 + 1 are OUTSIDE the top-3 plan window.
	fx.addFinding(fx.scopeAt(t, 1), "solid.ocp.modified_existing")
	fx.addFinding(fx.scopeAt(t, 0), "decoupling.duplication_ratio_high")

	planner := fx.newPlanner(t)

	got, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}

	// (1) PolicyVersionID propagated.
	if got.PolicyVersionID != fx.wantPVID {
		t.Errorf("PolicyVersionID = %s, want %s", got.PolicyVersionID, fx.wantPVID)
	}

	// (2) All 5 hot_spots persisted via the hot_spot writer
	// (architecture Sec 5.5.1 append-only; TopN does NOT
	// truncate the hot_spot table).
	if len(fx.hsWriter.Rows()) != 5 {
		t.Errorf("len(hsWriter.Rows()) = %d, want 5",
			len(fx.hsWriter.Rows()))
	}
	if len(got.HotSpots) != 5 {
		t.Errorf("len(got.HotSpots) = %d, want 5", len(got.HotSpots))
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
	// score-DESC order.
	if len(plan.HotspotIDs) != 3 {
		t.Fatalf("len(plan.HotspotIDs) = %d, want 3 (TopN truncation)",
			len(plan.HotspotIDs))
	}
	// got.HotSpots is already score-DESC; plan.HotspotIDs[i]
	// MUST equal got.HotSpots[i].HotspotID for i in [0,3).
	for i := 0; i < 3; i++ {
		if plan.HotspotIDs[i] != got.HotSpots[i].HotspotID {
			t.Errorf("plan.HotspotIDs[%d] = %s, want %s",
				i, plan.HotspotIDs[i], got.HotSpots[i].HotspotID)
		}
	}

	// (5) summary_md is non-empty and names the
	// repo/sha/hot_spot count.
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
		fx.scopeAt(t, 4): TaskKindSplitClass,
		fx.scopeAt(t, 3): TaskKindInvertDependency,
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
			// Stage 8.2 emits 0.0 as the "unestimated"
			// placeholder; Stage 8.3 replaces it.
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
	fx := newTaskPlannerFixture(t, weightsTopN(0), 4)
	for i := 0; i < 4; i++ {
		fx.addFinding(fx.scopeAt(t, i), "solid.srp.lcom4_high")
	}
	planner := fx.newPlanner(t)
	got, err := planner.Plan(context.Background(), fx.repoID, fx.sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(got.HotSpots) != 4 {
		t.Errorf("len(got.HotSpots) = %d, want 4", len(got.HotSpots))
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
// gracefully -- no out-of-bounds slice.
func TestTaskPlanner_Plan_TopNExceedsHotSpotCount(t *testing.T) {
	fx := newTaskPlannerFixture(t, weightsTopN(20), 3)
	for i := 0; i < 3; i++ {
		fx.addFinding(fx.scopeAt(t, i), "solid.srp.lcom4_high")
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
	fx := newTaskPlannerFixture(t, weightsTopN(2), 2)
	// Scope 0 gets findings; scope 1 has metric-only signal.
	fx.addFinding(fx.scopeAt(t, 0), "solid.srp.lcom4_high")

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
	// Both hot_spots covered.
	if len(plans[0].HotspotIDs) != 2 {
		t.Errorf("len(plan.HotspotIDs) = %d, want 2", len(plans[0].HotspotIDs))
	}
	// But only ONE task (for the scope with findings).
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
	fx := newTaskPlannerFixture(t, weightsTopN(1), 1)
	// Same scope, same rule, multiple firings.
	fx.addFinding(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	fx.addFinding(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	fx.addFinding(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	// Same scope, DIFFERENT rule -- emits a second task.
	fx.addFinding(fx.scopeAt(t, 0), "decoupling.cycle_member")

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
	fx := newTaskPlannerFixture(t, weightsTopN(1), 1)
	fx.addFinding(fx.scopeAt(t, 0), "unknown.family.rule")
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
	fx := newTaskPlannerFixture(t, weightsTopN(1), 1)
	fx.addFinding(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
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
	fx := newTaskPlannerFixture(t, weightsTopN(1), 1)
	fx.addFinding(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
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
		NewInMemoryMetricSampleReader(),
		NewInMemoryFindingReader(),
		NewInMemoryHotSpotWriter(),
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
// (repo_id, sha) with no metric_sample + no finding produces
// NO plan and NO task. The writer is NOT called -- emitting
// an empty plan would be semantically meaningless.
func TestTaskPlanner_Plan_EmptyInput_NoPlanWritten(t *testing.T) {
	st, _ := inMemoryStewardWithActivePolicy(t, weightsTopN(5))
	hsWriter := NewInMemoryHotSpotWriter()
	planWriter := NewInMemoryRefactorPlanTaskWriter()
	planner, err := NewTaskPlanner(
		&StewardPolicyReader{Steward: st},
		NewInMemoryMetricSampleReader(),
		NewInMemoryFindingReader(),
		hsWriter,
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
	if len(hsWriter.Rows()) != 0 {
		t.Errorf("len(hsWriter.Rows()) = %d, want 0", len(hsWriter.Rows()))
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
		NewInMemoryMetricSampleReader(),
		NewInMemoryFindingReader(),
		NewInMemoryHotSpotWriter(),
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

// TestTaskPlanner_Plan_FindingDetailReaderError_PropagatesAndWraps
// asserts the dependency's error is surfaced with a wrap
// (errors.Is round-trip).
func TestTaskPlanner_Plan_FindingDetailReaderError_PropagatesAndWraps(t *testing.T) {
	st, _ := inMemoryStewardWithActivePolicy(t, weightsTopN(2))
	boom := errors.New("detail boom")
	repoID := mustUUID(t)
	sha := "deadbeef"
	scope0 := mustParseUUID(t, "00000000-0000-0000-0000-000000000001")
	metrics := NewInMemoryMetricSampleReader()
	metrics.Insert(InMemoryMetricSample{
		RepoID: repoID, SHA: sha, ScopeID: scope0,
		MetricKind: MetricKindCyclo, MetricVersion: 1, Value: 20,
	})
	findings := NewInMemoryFindingReader()
	findings.Insert(InMemoryFinding{
		RepoID: repoID, SHA: sha, ScopeID: scope0,
		PolicyVersionID: pv42(t), Delta: rule_engine.DeltaNew,
	})

	planner, err := NewTaskPlanner(
		&StewardPolicyReader{Steward: st},
		metrics, findings,
		NewInMemoryHotSpotWriter(),
		failingFindingDetailReader{err: boom},
		NewInMemoryRefactorPlanTaskWriter(),
		WithTaskIDFactory(countingIDFactory()),
		WithTaskClock(fixedClock(time.Unix(1_700_000_400, 0).UTC())),
	)
	if err != nil {
		t.Fatalf("NewTaskPlanner: %v", err)
	}
	_, err = planner.Plan(context.Background(), repoID, sha)
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
	fx := newTaskPlannerFixture(t, weightsTopN(1), 1)
	fx.addFinding(fx.scopeAt(t, 0), "solid.srp.lcom4_high")
	boom := errors.New("plan write boom")

	planner, err := NewTaskPlanner(
		&StewardPolicyReader{Steward: fx.st},
		fx.metrics, fx.findings, fx.hsWriter,
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

// -----------------------------------------------------------------------------
// InMemoryFindingDetailReader unit tests
// -----------------------------------------------------------------------------

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

