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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/effort"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// --- per-scenario state ---

type plannerWiringState struct {
	repoID  uuid.UUID
	sha     string
	pvID    uuid.UUID
	bundle  devpolicy.Bundle
	repoCtx repocontext.RepoContext

	samples  []rule_engine.Sample
	findings []rule_engine.Finding

	planResult refactor.PlanResult
	taskResult refactor.PlanAndTasksResult

	// orchDiagnostics stores the diagnostics from an actual
	// orchestrator.Run() invocation so the effort-fallback
	// assertion checks the real RunArtifact field.
	orchDiagnostics orchestrator.Diagnostics

	fixtureDir string
}

func newPlannerWiringState() *plannerWiringState {
	pvID := uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))
	repoID := uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111"))
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	return &plannerWiringState{
		repoID: repoID,
		sha:    sha,
		pvID:   pvID,
		repoCtx: repocontext.RepoContext{
			RootPath: "/tmp/e2e-planner-fixture",
			RepoID:   repoID,
			HeadSHA:  sha,
		},
	}
}

func (s *plannerWiringState) cleanup() {
	if s.fixtureDir != "" {
		_ = os.RemoveAll(s.fixtureDir)
	}
}

// scopeUUID mints a deterministic scope UUID from a name.
func scopeUUID(name string) uuid.UUID {
	return uuid.NewV5(uuid.NamespaceURL, "e2e-planner-scope:"+name)
}

// --- helpers ---

func (s *plannerWiringState) makeBundle(weights steward.RefactorWeights, rules []steward.Rule) {
	refs := make([]steward.RuleRef, 0, len(rules))
	for _, r := range rules {
		refs = append(refs, steward.RuleRef{RuleID: r.RuleID, Version: r.Version})
	}
	s.bundle = devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: s.pvID,
			Name:            "e2e-dev-policy",
			RuleRefs:        refs,
			RefactorWeights: weights,
			CreatedAt:       time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
		},
		Rules: rules,
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

func (s *plannerWiringState) runTaskPlanner(effortModel refactor.EffortModel) error {
	opts := []refactor.TaskOption{}
	if effortModel != nil {
		opts = append(opts, refactor.WithEffortModel(effortModel))
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

// runAnalyzeCompositionRoot exercises the full analyze-command-shaped
// pipeline: engine → planner → task planner, wired through the
// production orchestrator helpers that the CLI composition root uses.
func (s *plannerWiringState) runAnalyzeCompositionRoot(effortModel refactor.EffortModel) error {
	// Stage 2.3: engine wiring — load the in-memory store with the
	// dev-mode bundle + metric samples, then run the engine to
	// produce findings.
	store, err := orchestrator.LoadStore(s.bundle, s.samples, s.repoCtx)
	if err != nil {
		return fmt.Errorf("LoadStore: %w", err)
	}
	eng, err := rule_engine.New(rule_engine.Config{Store: store})
	if err != nil {
		return fmt.Errorf("rule_engine.New: %w", err)
	}
	_, err = eng.RunBatch(
		context.Background(), s.repoID, s.sha,
		s.bundle.PolicyVersion.PolicyVersionID)
	if err != nil {
		return fmt.Errorf("RunBatch: %w", err)
	}
	s.findings = store.Findings()

	// Stage 2.4a: planner wiring — the same orchestrator helpers the
	// CLI binary's composition root calls.
	if err := s.runPlanner(); err != nil {
		return err
	}

	// Stage 2.4b: task planner wiring.
	return s.runTaskPlanner(effortModel)
}

// createFixtureDir creates a temp directory with a Go file.
func (s *plannerWiringState) createFixtureDir(lineCount int, fileName string) error {
	dir, err := os.MkdirTemp("", "e2e-planner-*")
	if err != nil {
		return fmt.Errorf("create fixture dir: %w", err)
	}
	s.fixtureDir = dir

	var b strings.Builder
	b.WriteString("package fixture\n\n")
	b.WriteString("func bigFunction() {\n")
	for i := 0; i < lineCount; i++ {
		fmt.Fprintf(&b, "\t_ = %d\n", i)
	}
	b.WriteString("}\n")

	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("write fixture file: %w", err)
	}

	s.repoCtx = repocontext.RepoContext{
		RootPath: filepath.ToSlash(dir),
		RepoID:   uuid.NewV5(uuid.NamespaceURL, "cleanc.local-repo:"+filepath.ToSlash(dir)),
		HeadSHA:  s.sha,
	}
	s.repoID = s.repoCtx.RepoID
	return nil
}

// --- Given steps ---

func (s *plannerWiringState) aFixtureRunWithThreeFindingsOnThreeDifferentScopes() error {
	s.makeBundle(steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
		WindowDays:         7,
		EffortModelVersion: "fallback-linear",
	}, nil)

	scopes := []struct {
		name  string
		cyclo float64
		churn float64
		rule  string
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
		s.addFinding(sid, sc.rule)
	}
	return nil
}

func (s *plannerWiringState) aFixtureRunProducingOneTaskPerKind() error {
	s.makeBundle(steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
		WindowDays:         7,
		EffortModelVersion: "fallback-linear",
	}, nil)

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
	// Create a real fixture dir so we can run the orchestrator and
	// capture its Diagnostics.EffortSource (the acceptance scenario
	// requires checking the actual RunArtifact field, not a
	// standalone constant).
	if err := s.createFixtureDir(200, "big.go"); err != nil {
		return err
	}

	s.makeBundle(steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
		WindowDays:         7,
		EffortModelVersion: "fallback-linear",
	}, nil)

	sid := scopeUUID("EffortScope")
	s.addMetricSample(sid, refactor.MetricKindCyclo, 25)
	s.addMetricSample(sid, refactor.MetricKindCognitiveComplexity, 12)
	s.addMetricSample(sid, refactor.MetricKindModificationCountInWindow, 5)
	s.addFinding(sid, "solid.srp.lcom4_high")
	return nil
}

func (s *plannerWiringState) aFixtureWith50HotSpotsAndTopN5() error {
	// The --top-n 5 flag in the analyze command maps to
	// RefactorWeights.TopN=5 at the composition root level. The
	// rule "solid.srp.loc_high" fires on each scope's loc sample
	// to produce findings through the engine stage.
	rule := steward.Rule{
		RuleID:          "solid.srp.loc_high",
		Version:         1,
		PackID:          "solid.srp",
		PredicateDSL:    "metric_kind == 'cyclo' AND value >= 1",
		SeverityDefault: steward.SeverityBlock,
		CreatedAt:       time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
	}
	s.makeBundle(steward.RefactorWeights{
		Alpha: 1, Beta: 1, Gamma: 1, Delta: 1,
		WindowDays:         7,
		TopN:               5,
		EffortModelVersion: "fallback-linear",
	}, []steward.Rule{rule})

	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("Scope%03d", i)
		sid := scopeUUID(name)
		s.addMetricSample(sid, refactor.MetricKindCyclo, float64(100-i))
		s.addMetricSample(sid, refactor.MetricKindCognitiveComplexity, float64(50-i))
		s.addMetricSample(sid, refactor.MetricKindModificationCountInWindow, float64(20))
	}
	return nil
}

// --- When steps ---

func (s *plannerWiringState) thePlannerStageRuns() error {
	return s.runPlanner()
}

func (s *plannerWiringState) theTaskPlannerStageRuns() error {
	if err := s.runPlanner(); err != nil {
		return err
	}
	return s.runTaskPlanner(refactor.HeuristicEffortModel{})
}

func (s *plannerWiringState) theTaskPlannerRuns() error {
	// Run the orchestrator on the fixture dir to capture
	// Diagnostics.EffortSource from the actual pipeline. The
	// default EffortEstimator is effort.NewFallbackModel() which
	// stamps EffortSource = "fallback" when no ONNX model is
	// configured.
	orch := orchestrator.New(orchestrator.Options{Workers: 1})
	orchResult, err := orch.Run(context.Background(), s.repoCtx, s.fixtureDir)
	if err != nil {
		return fmt.Errorf("orchestrator.Run: %w", err)
	}
	s.orchDiagnostics = orchResult.Diagnostics

	// Run planner + task planner with HeuristicEffortModel so
	// tasks get EffortHours > 0.
	if err := s.runPlanner(); err != nil {
		return err
	}
	return s.runTaskPlanner(refactor.HeuristicEffortModel{})
}

func (s *plannerWiringState) theAnalyzeCommandRuns() error {
	// Exercise the full analyze-command composition root:
	// engine → planner → task planner, using the orchestrator's
	// production wiring helpers (LoadStore, BuildMetricSampleReader,
	// BuildFindingReader, NewTaskPlannerWiring). The --top-n value
	// is already set in RefactorWeights.TopN, which is how the CLI
	// flag propagates through the composition root.
	return s.runAnalyzeCompositionRoot(refactor.HeuristicEffortModel{})
}

// --- Then steps ---

func (s *plannerWiringState) hotSpotsLengthIs3AndSorted() error {
	hs := s.planResult.HotSpots
	if len(hs) != 3 {
		return fmt.Errorf("HotSpots length = %d, want 3", len(hs))
	}

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

	// Check the actual RunArtifact diagnostics from the orchestrator
	// run — not a standalone constant. The orchestrator stamps
	// EffortSource from the wired effort.Estimator.Name() at the end
	// of Run(); when no ONNX model is configured, the default is
	// effort.NewFallbackModel() whose Name() returns "fallback".
	got := s.orchDiagnostics.EffortSource
	want := effort.FallbackEstimatorName
	if got != want {
		return fmt.Errorf("Diagnostics.EffortSource = %q, want %q", got, want)
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

	ctx.After(func(ctx2 context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		s.cleanup()
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
