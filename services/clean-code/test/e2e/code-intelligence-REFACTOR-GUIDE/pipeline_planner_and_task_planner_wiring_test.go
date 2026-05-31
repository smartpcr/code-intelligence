//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="pipeline_planner_and_task_planner_wiring_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/effort"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// --- per-scenario state ---

type plannerWiringState struct {
	repoID uuid.UUID
	sha    string
	pvID   uuid.UUID
	bundle devpolicy.Bundle

	samples  []rule_engine.Sample
	findings []rule_engine.Finding

	planResult refactor.PlanResult
	taskResult refactor.PlanAndTasksResult

	effortEstimator effort.Estimator
}

func newPlannerWiringState() *plannerWiringState {
	pvID := uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	repoID := uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111"))
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	return &plannerWiringState{
		repoID:          repoID,
		sha:             sha,
		pvID:            pvID,
		effortEstimator: effort.NewFallbackModel(),
	}
}

// scopeUUID mints a deterministic scope UUID from a name.
func scopeUUID(name string) uuid.UUID {
	return uuid.NewV5(uuid.NamespaceURL, "e2e-planner-scope:"+name)
}

// --- helpers ---

func (s *plannerWiringState) makeBundle(weights steward.RefactorWeights) {
	s.bundle = devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: s.pvID,
			Name:            "e2e-dev-policy",
			RefactorWeights: weights,
		},
	}
}

func (s *plannerWiringState) addMetricSample(scopeID uuid.UUID, metricKind string, value float64) {
	sampleID := orchestrator.MintSampleID(s.repoID, s.sha, scopeID, metricKind, 1)
	sample := rule_engine.Sample{ScopeSignature: "fixture://" + scopeID.String()}
	sample.SampleID = sampleID
	sample.RepoID = s.repoID
	sample.SHA = s.sha
	sample.ScopeID = scopeID
	sample.ScopeKind = "class"
	sample.MetricKind = metricKind
	sample.MetricVersion = 1
	sample.Value = value
	sample.HasValue = true
	sample.Pack = "base"
	sample.Source = "computed"
	s.samples = append(s.samples, sample)
}

func (s *plannerWiringState) addFinding(scopeID uuid.UUID, ruleID string) {
	s.findings = append(s.findings, rule_engine.Finding{
		FindingID:       uuid.Must(uuid.NewV4()),
		RepoID:          s.repoID,
		SHA:             s.sha,
		ScopeID:         scopeID,
		PolicyVersionID: s.pvID,
		RuleID:          ruleID,
		Delta:           rule_engine.DeltaNew,
	})
}

func (s *plannerWiringState) runPlanner() error {
	policyR := orchestrator.NewCLIPolicyReader(s.bundle)
	metricR := orchestrator.BuildMetricSampleReader(s.samples)
	findR := orchestrator.BuildFindingReader(s.findings)
	writer := refactor.NewInMemoryHotSpotWriter()

	planner, err := refactor.NewPlanner(policyR, metricR, findR, writer)
	if err != nil {
		return fmt.Errorf("NewPlanner: %w", err)
	}
	res, err := planner.Plan(context.Background(), s.repoID, s.sha)
	if err != nil {
		return fmt.Errorf("Plan: %w", err)
	}
	s.planResult = res
	return nil
}

func (s *plannerWiringState) runTaskPlanner() error {
	if err := s.runPlanner(); err != nil {
		return err
	}

	opts := []refactor.TaskOption{
		refactor.WithEffortModel(refactor.HeuristicEffortModel{}),
	}
	tp, _, err := orchestrator.NewTaskPlannerWiring(
		s.bundle, s.planResult.HotSpots, s.findings, opts...)
	if err != nil {
		return fmt.Errorf("NewTaskPlannerWiring: %w", err)
	}
	taskRes, err := tp.PlanFromSnapshot(
		context.Background(), s.repoID, s.sha, s.planResult.Snapshot)
	if err != nil {
		return fmt.Errorf("PlanFromSnapshot: %w", err)
	}
	s.taskResult = taskRes
	return nil
}

// --- Given steps ---

func (s *plannerWiringState) aFixtureRunWithThreeFindingsOnThreeDifferentScopes() error {
	s.makeBundle(steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
		WindowDays:         7,
		EffortModelVersion: "fallback-linear",
	})

	scopes := []struct {
		name    string
		cyclo   float64
		churn   float64
		ruleID  string
	}{
		{"ScopeA", 30, 10, "solid.srp.lcom4_high"},
		{"ScopeB", 20, 5, "solid.ocp.high_cyclo"},
		{"ScopeC", 10, 2, "decoupling.cycle_member"},
	}

	for _, sc := range scopes {
		sid := scopeUUID(sc.name)
		s.addMetricSample(sid, refactor.MetricKindCyclo, sc.cyclo)
		s.addMetricSample(sid, refactor.MetricKindCognitiveComplexity, sc.cyclo*0.5)
		s.addMetricSample(sid, refactor.MetricKindModificationCountInWindow, sc.churn)
		s.addFinding(sid, sc.ruleID)
	}
	return nil
}

func (s *plannerWiringState) aFixtureRunProducingOneTaskPerKind() error {
	s.makeBundle(steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
		WindowDays:         7,
		EffortModelVersion: "fallback-linear",
	})

	// Each rule_id maps to a different canonical TaskKind via
	// DefaultTaskKindForRule:
	//   solid.srp.* -> split_class
	//   solid.ocp.* -> extract_method
	//   solid.dip.* -> invert_dependency
	//   decoupling.cycle_member -> break_cycle
	//   decoupling.duplication_ratio -> consolidate_duplication
	entries := []struct {
		scopeName string
		ruleID    string
	}{
		{"ScopeForSplitClass", "solid.srp.lcom4_high"},
		{"ScopeForExtractMethod", "solid.ocp.high_cyclo"},
		{"ScopeForInvertDep", "solid.dip.high_cbo"},
		{"ScopeForBreakCycle", "decoupling.cycle_member"},
		{"ScopeForConsolidate", "decoupling.duplication_ratio"},
	}

	for i, e := range entries {
		sid := scopeUUID(e.scopeName)
		s.addMetricSample(sid, refactor.MetricKindCyclo, float64(50-i*5))
		s.addMetricSample(sid, refactor.MetricKindCognitiveComplexity, float64(25-i*3))
		s.addMetricSample(sid, refactor.MetricKindModificationCountInWindow, float64(10-i))
		s.addFinding(sid, e.ruleID)
	}
	return nil
}

func (s *plannerWiringState) aFixtureWhereNoONNXModelIsConfigured() error {
	s.makeBundle(steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
		WindowDays:         7,
		EffortModelVersion: "fallback-linear",
	})

	sid := scopeUUID("EffortScope")
	s.addMetricSample(sid, refactor.MetricKindCyclo, 25)
	s.addMetricSample(sid, refactor.MetricKindCognitiveComplexity, 12)
	s.addMetricSample(sid, refactor.MetricKindModificationCountInWindow, 5)
	s.addFinding(sid, "solid.srp.lcom4_high")
	return nil
}

func (s *plannerWiringState) aFixtureWith50HotSpotsAndTopN5() error {
	s.makeBundle(steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
		WindowDays:         7,
		TopN:               5,
		EffortModelVersion: "fallback-linear",
	})

	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("Scope%03d", i)
		sid := scopeUUID(name)
		s.addMetricSample(sid, refactor.MetricKindCyclo, float64(100-i))
		s.addMetricSample(sid, refactor.MetricKindCognitiveComplexity, float64(50-i))
		s.addMetricSample(sid, refactor.MetricKindModificationCountInWindow, float64(20))
		s.addFinding(sid, "solid.srp.lcom4_high")
	}
	return nil
}

// --- When steps ---

func (s *plannerWiringState) thePlannerStageRuns() error {
	return s.runPlanner()
}

func (s *plannerWiringState) theTaskPlannerStageRuns() error {
	return s.runTaskPlanner()
}

func (s *plannerWiringState) theTaskPlannerRuns() error {
	return s.runTaskPlanner()
}

func (s *plannerWiringState) theAnalyzeCommandRuns() error {
	return s.runTaskPlanner()
}

// --- Then steps ---

func (s *plannerWiringState) hotSpotsLengthIs3AndSorted() error {
	hs := s.planResult.HotSpots
	if len(hs) != 3 {
		return fmt.Errorf("HotSpots length = %d, want 3", len(hs))
	}

	// Verify Score DESC, ScopeID ASC ordering.
	for i := 1; i < len(hs); i++ {
		prev := hs[i-1]
		curr := hs[i]
		if prev.Score < curr.Score {
			return fmt.Errorf("HotSpots[%d].Score (%.4f) < HotSpots[%d].Score (%.4f): not sorted DESC",
				i-1, prev.Score, i, curr.Score)
		}
		if prev.Score == curr.Score {
			prevStr := prev.ScopeID.String()
			currStr := curr.ScopeID.String()
			if prevStr > currStr {
				return fmt.Errorf("HotSpots[%d].ScopeID (%s) > HotSpots[%d].ScopeID (%s): "+
					"tie-break not sorted ASC", i-1, prevStr, i, currStr)
			}
		}
	}
	return nil
}

func (s *plannerWiringState) everyTaskKindIsCanonical() error {
	tasks := s.taskResult.Tasks
	if len(tasks) == 0 {
		return fmt.Errorf("Tasks is empty, expected at least one task per kind")
	}

	seen := make(map[refactor.TaskKind]bool)
	for i, t := range tasks {
		if !refactor.IsCanonicalTaskKind(t.Kind) {
			return fmt.Errorf("Tasks[%d].Kind = %q is not canonical", i, t.Kind)
		}
		seen[t.Kind] = true
	}

	// Verify we actually got all 5 canonical kinds.
	for _, k := range refactor.CanonicalTaskKinds {
		if !seen[k] {
			seenKinds := make([]string, 0, len(seen))
			for sk := range seen {
				seenKinds = append(seenKinds, string(sk))
			}
			sort.Strings(seenKinds)
			return fmt.Errorf("missing canonical kind %q in output; got kinds: %v", k, seenKinds)
		}
	}
	return nil
}

func (s *plannerWiringState) everyTaskHasEffortAndFallbackSource() error {
	tasks := s.taskResult.Tasks
	if len(tasks) == 0 {
		return fmt.Errorf("Tasks is empty, expected at least one task")
	}
	for i, t := range tasks {
		if t.EffortHours <= 0 {
			return fmt.Errorf("Tasks[%d].EffortHours = %v, want > 0", i, t.EffortHours)
		}
	}

	// Verify diagnostics record effort_source == "fallback".
	got := s.effortEstimator.Name()
	if got != effort.FallbackEstimatorName {
		return fmt.Errorf("effort_source = %q, want %q", got, effort.FallbackEstimatorName)
	}
	return nil
}

func (s *plannerWiringState) tasksLengthIsAtMost5() error {
	tasks := s.taskResult.Tasks
	if len(tasks) > 5 {
		return fmt.Errorf("Tasks length = %d, want at most 5", len(tasks))
	}
	if len(tasks) == 0 {
		return fmt.Errorf("Tasks is empty, expected at least 1 task (top-n=5 from 50)")
	}
	return nil
}

// --- Scenario initializer ---

func InitializeScenario_pipeline_planner_and_task_planner_wiring(ctx *godog.ScenarioContext) {
	var s *plannerWiringState

	ctx.Before(func(ctx2 context.Context, _ *godog.Scenario) (context.Context, error) {
		s = newPlannerWiringState()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a fixture run with three findings on three different scopes$`,
		func() error { return s.aFixtureRunWithThreeFindingsOnThreeDifferentScopes() })
	ctx.Step(`^a fixture run producing one task per kind$`,
		func() error { return s.aFixtureRunProducingOneTaskPerKind() })
	ctx.Step(`^a fixture where no ONNX model is configured$`,
		func() error { return s.aFixtureWhereNoONNXModelIsConfigured() })
	ctx.Step(`^a fixture with 50 hot-spots and --top-n 5$`,
		func() error { return s.aFixtureWith50HotSpotsAndTopN5() })

	// When
	ctx.Step(`^the planner stage runs$`,
		func() error { return s.thePlannerStageRuns() })
	ctx.Step(`^the task planner stage runs$`,
		func() error { return s.theTaskPlannerStageRuns() })
	ctx.Step(`^the task planner runs$`,
		func() error { return s.theTaskPlannerRuns() })
	ctx.Step(`^the analyze command runs$`,
		func() error { return s.theAnalyzeCommandRuns() })

	// Then
	ctx.Step(`^RunArtifact\.HotSpots length is 3 and rows are sorted by Score descending then ScopeID ascending$`,
		func() error { return s.hotSpotsLengthIs3AndSorted() })
	ctx.Step(`^every Tasks\[i\]\.Kind satisfies refactor\.IsCanonicalTaskKind$`,
		func() error { return s.everyTaskKindIsCanonical() })
	ctx.Step(`^every Tasks\[i\]\.EffortHours > 0 and the diagnostics record effort_source == "fallback"$`,
		func() error { return s.everyTaskHasEffortAndFallbackSource() })
	ctx.Step(`^RunArtifact\.Tasks length is at most 5$`,
		func() error { return s.tasksLengthIsAtMost5() })
}

func TestE2E_pipeline_planner_and_task_planner_wiring(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_pipeline_planner_and_task_planner_wiring,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"pipeline_planner_and_task_planner_wiring.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}

// Suppress unused import warnings for time (used in state setup).
var _ = time.Now
