// -----------------------------------------------------------------------
// <copyright file="planner_wiring_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package orchestrator_test

import (
	"context"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

func TestCLIPolicyReader_ProjectsBundleOntoSnapshot(t *testing.T) {
	t.Parallel()

	pvID := uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	weights := steward.RefactorWeights{
		Alpha:              1.5,
		Beta:               2.5,
		Gamma:              0.75,
		Delta:              3.0,
		EffortModelVersion: "fallback-linear",
		WindowDays:         30,
		TopN:               10,
	}
	bundle := devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: pvID,
			Name:            "cleanc-dev-policy",
			RefactorWeights: weights,
		},
	}

	reader := orchestrator.NewCLIPolicyReader(bundle)
	snap, ok, err := reader.ActivePolicyVersion(context.Background())
	if err != nil {
		t.Fatalf("ActivePolicyVersion err = %v, want nil", err)
	}
	if !ok {
		t.Fatalf("ActivePolicyVersion ok = false, want true (scaffold-mode bundle is always active)")
	}
	if snap.PolicyVersionID != pvID {
		t.Errorf("PolicyVersionID = %v, want %v", snap.PolicyVersionID, pvID)
	}
	if snap.Weights != weights {
		t.Errorf("Weights = %+v, want %+v", snap.Weights, weights)
	}
}

func TestCLIPolicyReader_ContextCancelled(t *testing.T) {
	t.Parallel()

	reader := orchestrator.NewCLIPolicyReader(devpolicy.Bundle{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := reader.ActivePolicyVersion(ctx)
	if err == nil {
		t.Fatalf("ActivePolicyVersion(cancelled ctx) err = nil, want context.Canceled")
	}
}

// TestCLIPolicyReader_SatisfiesRefactorPolicyReader is a
// compile-time + behavioural check that the adapter slots
// directly into [refactor.NewPlanner] without an extra wrapper.
func TestCLIPolicyReader_SatisfiesRefactorPolicyReader(t *testing.T) {
	t.Parallel()
	var _ refactor.PolicyReader = orchestrator.NewCLIPolicyReader(devpolicy.Bundle{})
}

func TestBuildMetricSampleReader_FiltersToHotSpotInputKinds(t *testing.T) {
	t.Parallel()

	repoID := uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111"))
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	scopeA := uuid.Must(uuid.FromString("22222222-2222-4222-8222-222222222222"))
	scopeB := uuid.Must(uuid.FromString("33333333-3333-4333-8333-333333333333"))

	mk := func(scopeID uuid.UUID, kind string, value float64) rule_engine.Sample {
		return rule_engine.Sample{
			Sample: dsl.Sample{
				SampleID:      uuid.Must(uuid.NewV4()),
				RepoID:        repoID,
				SHA:           sha,
				ScopeID:       scopeID,
				MetricKind:    kind,
				MetricVersion: 1,
				Value:         value,
				HasValue:      true,
			},
		}
	}

	samples := []rule_engine.Sample{
		mk(scopeA, refactor.MetricKindCyclo, 12),
		mk(scopeA, refactor.MetricKindCognitiveComplexity, 8),
		mk(scopeA, refactor.MetricKindFanOut, 4),
		mk(scopeB, refactor.MetricKindCouplingBetweenObjects, 6),
		mk(scopeB, refactor.MetricKindModificationCountInWindow, 3),
		// Out-of-band metric kinds that MUST be filtered out:
		mk(scopeA, "loc", 99),
		mk(scopeA, "duplication_ratio", 0.4),
		mk(scopeB, "lcom4", 7),
	}

	reader := orchestrator.BuildMetricSampleReader(samples)
	if reader == nil {
		t.Fatalf("BuildMetricSampleReader returned nil")
	}

	got, err := reader.ScopeMetrics(context.Background(), repoID, sha, refactor.HotSpotInputMetricKinds)
	if err != nil {
		t.Fatalf("ScopeMetrics: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("ScopeMetrics returned %d scopes, want 2 (scopeA + scopeB)", len(got))
	}

	// Index by scope id for predicate-style assertions.
	byScope := map[uuid.UUID]refactor.ScopeInputs{}
	for _, in := range got {
		byScope[in.ScopeID] = in
	}
	a, ok := byScope[scopeA]
	if !ok {
		t.Fatalf("scopeA missing from ScopeMetrics output")
	}
	if !a.HasCyclo || a.Cyclo != 12 {
		t.Errorf("scopeA Cyclo = %v (has=%v), want 12 / true", a.Cyclo, a.HasCyclo)
	}
	if !a.HasCognitiveComplexity || a.CognitiveComplexity != 8 {
		t.Errorf("scopeA CognitiveComplexity = %v (has=%v), want 8 / true", a.CognitiveComplexity, a.HasCognitiveComplexity)
	}
	if !a.HasFanOut || a.FanOut != 4 {
		t.Errorf("scopeA FanOut = %v (has=%v), want 4 / true", a.FanOut, a.HasFanOut)
	}

	b, ok := byScope[scopeB]
	if !ok {
		t.Fatalf("scopeB missing from ScopeMetrics output")
	}
	if !b.HasCouplingBetweenObjects || b.CouplingBetweenObjects != 6 {
		t.Errorf("scopeB CBO = %v (has=%v), want 6 / true", b.CouplingBetweenObjects, b.HasCouplingBetweenObjects)
	}
	if !b.HasModificationCount || b.ModificationCount != 3 {
		t.Errorf("scopeB ModificationCount = %v (has=%v), want 3 / true", b.ModificationCount, b.HasModificationCount)
	}
}

func TestBuildMetricSampleReader_SkipsNoValueSamples(t *testing.T) {
	t.Parallel()

	repoID := uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111"))
	sha := "abc"
	scopeID := uuid.Must(uuid.FromString("22222222-2222-4222-8222-222222222222"))

	samples := []rule_engine.Sample{
		{Sample: dsl.Sample{
			RepoID: repoID, SHA: sha, ScopeID: scopeID,
			MetricKind: refactor.MetricKindCyclo, MetricVersion: 1,
			Value: 99, HasValue: false, // <- skipped
		}},
	}
	reader := orchestrator.BuildMetricSampleReader(samples)
	got, err := reader.ScopeMetrics(context.Background(), repoID, sha, refactor.HotSpotInputMetricKinds)
	if err != nil {
		t.Fatalf("ScopeMetrics: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("ScopeMetrics len = %d, want 0 (HasValue=false samples are skipped)", len(got))
	}
}

func TestBuildMetricSampleReader_EmptyAndNilReturnNonNilReader(t *testing.T) {
	t.Parallel()
	if r := orchestrator.BuildMetricSampleReader(nil); r == nil {
		t.Errorf("BuildMetricSampleReader(nil) = nil, want non-nil empty reader")
	}
	if r := orchestrator.BuildMetricSampleReader([]rule_engine.Sample{}); r == nil {
		t.Errorf("BuildMetricSampleReader([]) = nil, want non-nil empty reader")
	}
}

func TestBuildFindingReader_FiltersToQualifyingDeltas(t *testing.T) {
	t.Parallel()

	repoID := uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111"))
	sha := "deadbeef"
	pvID := uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	scopeA := uuid.Must(uuid.FromString("22222222-2222-4222-8222-222222222222"))
	scopeB := uuid.Must(uuid.FromString("33333333-3333-4333-8333-333333333333"))
	scopeC := uuid.Must(uuid.FromString("44444444-4444-4444-8444-444444444444"))

	mk := func(scopeID uuid.UUID, delta rule_engine.Delta) rule_engine.Finding {
		return rule_engine.Finding{
			FindingID:       uuid.Must(uuid.NewV4()),
			RepoID:          repoID,
			SHA:             sha,
			ScopeID:         scopeID,
			PolicyVersionID: pvID,
			Delta:           delta,
		}
	}

	findings := []rule_engine.Finding{
		mk(scopeA, rule_engine.DeltaNew),          // counted
		mk(scopeA, rule_engine.DeltaNew),          // counted -- 2 total
		mk(scopeB, rule_engine.DeltaNewlyFailing), // counted
		mk(scopeC, rule_engine.DeltaUnchanged),    // DROPPED
		mk(scopeC, rule_engine.DeltaResolved),     // DROPPED
	}

	reader := orchestrator.BuildFindingReader(findings)
	if reader == nil {
		t.Fatalf("BuildFindingReader returned nil")
	}
	counts, err := reader.FindingCountsByScope(context.Background(), repoID, sha, pvID)
	if err != nil {
		t.Fatalf("FindingCountsByScope: %v", err)
	}
	if got := counts[scopeA]; got != 2 {
		t.Errorf("counts[scopeA] = %d, want 2", got)
	}
	if got := counts[scopeB]; got != 1 {
		t.Errorf("counts[scopeB] = %d, want 1", got)
	}
	if got, ok := counts[scopeC]; ok {
		t.Errorf("counts[scopeC] = %d (present); want absent because unchanged+resolved are dropped", got)
	}
}

func TestBuildFindingReader_EmptyAndNilReturnNonNilReader(t *testing.T) {
	t.Parallel()
	if r := orchestrator.BuildFindingReader(nil); r == nil {
		t.Errorf("BuildFindingReader(nil) = nil, want non-nil empty reader")
	}
	if r := orchestrator.BuildFindingReader([]rule_engine.Finding{}); r == nil {
		t.Errorf("BuildFindingReader([]) = nil, want non-nil empty reader")
	}
}

// TestPlannerWiring_EndToEnd composes a real refactor.Planner
// from the three CLI wiring helpers and exercises Plan() so the
// integration that this workstream pins is exercised by a
// single test rather than only by piece-wise unit tests.
func TestPlannerWiring_EndToEnd(t *testing.T) {
	t.Parallel()

	repoID := uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111"))
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	pvID := uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	scope1 := uuid.Must(uuid.FromString("22222222-2222-4222-8222-222222222222"))

	bundle := devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: pvID,
			Name:            "cleanc-dev",
			RefactorWeights: steward.RefactorWeights{
				Alpha: 1, Beta: 1, Gamma: 1, Delta: 1, WindowDays: 7,
				EffortModelVersion: "fallback-linear",
			},
		},
	}

	samples := []rule_engine.Sample{
		{Sample: dsl.Sample{
			RepoID: repoID, SHA: sha, ScopeID: scope1,
			MetricKind: refactor.MetricKindCyclo, MetricVersion: 1,
			Value: 25, HasValue: true,
		}},
		{Sample: dsl.Sample{
			RepoID: repoID, SHA: sha, ScopeID: scope1,
			MetricKind: refactor.MetricKindCognitiveComplexity, MetricVersion: 1,
			Value: 12, HasValue: true,
		}},
		{Sample: dsl.Sample{
			RepoID: repoID, SHA: sha, ScopeID: scope1,
			MetricKind: refactor.MetricKindModificationCountInWindow, MetricVersion: 1,
			Value: 5, HasValue: true,
		}},
	}
	findings := []rule_engine.Finding{
		{
			FindingID: uuid.Must(uuid.NewV4()),
			RepoID:    repoID, SHA: sha, ScopeID: scope1,
			PolicyVersionID: pvID, Delta: rule_engine.DeltaNew,
		},
	}

	policyR := orchestrator.NewCLIPolicyReader(bundle)
	metricR := orchestrator.BuildMetricSampleReader(samples)
	findR := orchestrator.BuildFindingReader(findings)
	writer := refactor.NewInMemoryHotSpotWriter()

	planner, err := refactor.NewPlanner(policyR, metricR, findR, writer)
	if err != nil {
		t.Fatalf("NewPlanner: %v", err)
	}
	res, err := planner.Plan(context.Background(), repoID, sha)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if res.PolicyVersionID != pvID {
		t.Errorf("PolicyVersionID = %v, want %v", res.PolicyVersionID, pvID)
	}
	if len(res.HotSpots) != 1 {
		t.Fatalf("HotSpots len = %d, want 1", len(res.HotSpots))
	}
	hs := res.HotSpots[0]
	if hs.ScopeID != scope1 {
		t.Errorf("HotSpot.ScopeID = %v, want %v", hs.ScopeID, scope1)
	}
	if hs.PolicyVersionID != pvID {
		t.Errorf("HotSpot.PolicyVersionID = %v, want %v", hs.PolicyVersionID, pvID)
	}
	if len(res.Breakdowns) != 1 || res.Breakdowns[0].FindingCount != 1 {
		t.Errorf("Breakdowns FindingCount = %+v, want one row with count 1", res.Breakdowns)
	}

	// ---- Stage 8.2 TaskPlanner wiring on the SAME snapshot. ----
	// Architecture Sec 3.5 race-safety pin: reuse the
	// snapshot from Stage 8.1 rather than re-reading the
	// active policy.
	findings = append(findings, rule_engine.Finding{
		FindingID:       uuid.Must(uuid.NewV4()),
		RepoID:          repoID,
		SHA:             sha,
		ScopeID:         scope1,
		PolicyVersionID: pvID,
		RuleID:          "solid.srp.high_lcom4",
		Delta:           rule_engine.DeltaNew,
	})

	tp, planWriter, err := orchestrator.NewTaskPlannerWiring(bundle, res.HotSpots, findings)
	if err != nil {
		t.Fatalf("NewTaskPlannerWiring: %v", err)
	}
	taskRes, err := tp.PlanFromSnapshot(context.Background(), repoID, sha, res.Snapshot)
	if err != nil {
		t.Fatalf("TaskPlanner.PlanFromSnapshot: %v", err)
	}
	if taskRes.Plan.PlanID == uuid.Nil {
		t.Fatalf("PlanFromSnapshot: PlanID is uuid.Nil, want a freshly-minted plan id")
	}
	if taskRes.PolicyVersionID != pvID {
		t.Errorf("PlanAndTasksResult.PolicyVersionID = %v, want %v", taskRes.PolicyVersionID, pvID)
	}
	if len(taskRes.Tasks) == 0 {
		t.Fatalf("PlanFromSnapshot: Tasks empty, want >=1 task for scope %v", scope1)
	}
	if got := taskRes.Tasks[0].ScopeID; got != scope1 {
		t.Errorf("Tasks[0].ScopeID = %v, want %v", got, scope1)
	}
	if plans := planWriter.Plans(); len(plans) != 1 {
		t.Errorf("planWriter.Plans len = %d, want 1", len(plans))
	}
	if writtenTasks := planWriter.Tasks(); len(writtenTasks) != len(taskRes.Tasks) {
		t.Errorf("planWriter.Tasks len = %d, want %d (matching PlanAndTasksResult)", len(writtenTasks), len(taskRes.Tasks))
	}
}

func TestBuildFindingDetailReader_FiltersToQualifyingDeltasAndCarriesRuleID(t *testing.T) {
	t.Parallel()

	repoID := uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111"))
	sha := "deadbeef"
	pvID := uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	scopeA := uuid.Must(uuid.FromString("22222222-2222-4222-8222-222222222222"))
	scopeB := uuid.Must(uuid.FromString("33333333-3333-4333-8333-333333333333"))

	mk := func(scopeID uuid.UUID, ruleID string, delta rule_engine.Delta) rule_engine.Finding {
		return rule_engine.Finding{
			FindingID:       uuid.Must(uuid.NewV4()),
			RepoID:          repoID,
			SHA:             sha,
			ScopeID:         scopeID,
			PolicyVersionID: pvID,
			RuleID:          ruleID,
			Delta:           delta,
		}
	}
	findings := []rule_engine.Finding{
		mk(scopeA, "solid.srp.high_lcom4", rule_engine.DeltaNew),
		mk(scopeA, "solid.srp.high_lcom4", rule_engine.DeltaNew),     // dedup target
		mk(scopeB, "decoupling.cycle_member", rule_engine.DeltaNewlyFailing),
		mk(scopeA, "decoupling.fan_out_high", rule_engine.DeltaUnchanged), // DROPPED
		mk(scopeB, "solid.dip.high_cbo", rule_engine.DeltaResolved),       // DROPPED
	}

	reader := orchestrator.BuildFindingDetailReader(findings)
	if reader == nil {
		t.Fatalf("BuildFindingDetailReader returned nil")
	}
	got, err := reader.FindingDetails(context.Background(), repoID, sha, pvID, []uuid.UUID{scopeA, scopeB})
	if err != nil {
		t.Fatalf("FindingDetails: %v", err)
	}
	// Expect dedup'd set: (scopeA, srp.high_lcom4) + (scopeB, cycle_member).
	if len(got) != 2 {
		t.Fatalf("FindingDetails len = %d, want 2 (dedup + delta filter); got=%+v", len(got), got)
	}
	seen := map[string]bool{}
	for _, d := range got {
		seen[d.ScopeID.String()+"|"+d.RuleID] = true
	}
	if !seen[scopeA.String()+"|solid.srp.high_lcom4"] {
		t.Errorf("missing (scopeA, solid.srp.high_lcom4)")
	}
	if !seen[scopeB.String()+"|decoupling.cycle_member"] {
		t.Errorf("missing (scopeB, decoupling.cycle_member)")
	}
}

func TestBuildHotSpotReader_RoundTripsBatch(t *testing.T) {
	t.Parallel()

	repoID := uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111"))
	sha := "deadbeef"
	pvID := uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	scope1 := uuid.Must(uuid.FromString("22222222-2222-4222-8222-222222222222"))
	scope2 := uuid.Must(uuid.FromString("33333333-3333-4333-8333-333333333333"))

	now := time.Now().UTC().Truncate(time.Microsecond)
	rows := []refactor.HotSpot{
		{
			HotspotID:       uuid.Must(uuid.NewV4()),
			RepoID:          repoID,
			SHA:             sha,
			ScopeID:         scope1,
			Score:           10.0,
			PolicyVersionID: pvID,
			CreatedAt:       now,
		},
		{
			HotspotID:       uuid.Must(uuid.NewV4()),
			RepoID:          repoID,
			SHA:             sha,
			ScopeID:         scope2,
			Score:           3.0,
			PolicyVersionID: pvID,
			CreatedAt:       now,
		},
	}
	reader := orchestrator.BuildHotSpotReader(rows)
	if reader == nil {
		t.Fatalf("BuildHotSpotReader returned nil")
	}
	got, err := reader.LatestHotSpotsByScore(context.Background(), repoID, sha, pvID, 0)
	if err != nil {
		t.Fatalf("LatestHotSpotsByScore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("LatestHotSpotsByScore len = %d, want 2", len(got))
	}
	if got[0].ScopeID != scope1 {
		t.Errorf("score-DESC: got[0].ScopeID = %v, want %v (higher score first)", got[0].ScopeID, scope1)
	}
}

func TestBuildHotSpotReader_NilAndEmptyReturnNonNilReader(t *testing.T) {
	t.Parallel()
	if r := orchestrator.BuildHotSpotReader(nil); r == nil {
		t.Errorf("BuildHotSpotReader(nil) = nil, want non-nil")
	}
	if r := orchestrator.BuildHotSpotReader([]refactor.HotSpot{}); r == nil {
		t.Errorf("BuildHotSpotReader([]) = nil, want non-nil")
	}
}

func TestBuildFindingDetailReader_NilAndEmptyReturnNonNilReader(t *testing.T) {
	t.Parallel()
	if r := orchestrator.BuildFindingDetailReader(nil); r == nil {
		t.Errorf("BuildFindingDetailReader(nil) = nil, want non-nil")
	}
	if r := orchestrator.BuildFindingDetailReader([]rule_engine.Finding{}); r == nil {
		t.Errorf("BuildFindingDetailReader([]) = nil, want non-nil")
	}
}

func TestNewTaskPlannerWiring_RejectsNilOption(t *testing.T) {
	t.Parallel()
	bundle := devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")),
		},
	}
	// Pass an option that the underlying NewTaskPlanner
	// rejects (nil clock callback). The wiring helper must
	// surface the error rather than panic.
	_, _, err := orchestrator.NewTaskPlannerWiring(bundle, nil, nil, refactor.WithTaskClock(nil))
	if err == nil {
		t.Fatalf("NewTaskPlannerWiring(WithTaskClock(nil)) err = nil, want non-nil")
	}
}
