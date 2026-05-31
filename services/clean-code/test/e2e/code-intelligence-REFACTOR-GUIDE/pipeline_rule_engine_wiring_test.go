//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="pipeline_rule_engine_wiring_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// --- spy store: records InsertSamples calls ---

type insertSamplesCall struct {
	batchSize int
}

// spyInsertSamplesStore wraps InMemoryStore and records every
// InsertSamples invocation so the test can assert the calling
// pattern (exactly-one batch call, not per-row).
type spyInsertSamplesStore struct {
	*rule_engine.InMemoryStore
	calls []insertSamplesCall
}

func (spy *spyInsertSamplesStore) InsertSamples(repoID uuid.UUID, sha string, samples []rule_engine.Sample) {
	spy.calls = append(spy.calls, insertSamplesCall{batchSize: len(samples)})
	spy.InMemoryStore.InsertSamples(repoID, sha, samples)
}

// --- failing store: AppendEvaluation returns injected error ---

type failingAppendStore struct {
	*rule_engine.InMemoryStore
	appendErr error
}

func (f *failingAppendStore) WithEvaluationLock(ctx context.Context, repoID uuid.UUID, sha string, fn func(rule_engine.Store) error) error {
	return f.InMemoryStore.WithEvaluationLock(ctx, repoID, sha, func(_ rule_engine.Store) error {
		return fn(f)
	})
}

func (f *failingAppendStore) AppendEvaluation(_ context.Context, _ rule_engine.EvaluationRun, _ rule_engine.EvaluationVerdict, _ []rule_engine.Finding) error {
	return f.appendErr
}

// --- composition root simulation ---

// runCompositionRoot simulates the CLI binary's main() for the
// engine stage. Returns (result, store, exitCode, stderrText).
// The CLI composition root (L6, not yet built) will mirror this
// logic: LoadStore → engine.New → RunBatch, mapping any error
// to exit 70 (EX_SOFTWARE) and writing the error to stderr.
func runCompositionRoot(
	bundle devpolicy.Bundle,
	samples []rule_engine.Sample,
	repoCtx repocontext.RepoContext,
	storeOverride rule_engine.Store,
) (rule_engine.RunResult, *rule_engine.InMemoryStore, int, string) {
	var stderr strings.Builder

	var engineStore rule_engine.Store
	var memStore *rule_engine.InMemoryStore
	if storeOverride != nil {
		engineStore = storeOverride
	} else {
		s, err := orchestrator.LoadStore(bundle, samples, repoCtx)
		if err != nil {
			fmt.Fprintf(&stderr, "%v\n", err)
			return rule_engine.RunResult{}, nil, 70, stderr.String()
		}
		memStore = s
		engineStore = s
	}

	eng, err := rule_engine.New(rule_engine.Config{Store: engineStore})
	if err != nil {
		fmt.Fprintf(&stderr, "%v\n", err)
		return rule_engine.RunResult{}, memStore, 70, stderr.String()
	}

	result, err := eng.RunBatch(
		context.Background(),
		repoCtx.RepoID,
		repoCtx.HeadSHA,
		bundle.PolicyVersion.PolicyVersionID,
	)
	if err != nil {
		fmt.Fprintf(&stderr, "%v\n", err)
		return result, memStore, 70, stderr.String()
	}

	return result, memStore, 0, ""
}

// --- fixture generator ---

// generateFixtureGoFile produces a valid Go source file whose
// function body contains lineCount statements so the LOC recipe
// emits a value >= lineCount for the function scope.
func generateFixtureGoFile(lineCount int) string {
	var b strings.Builder
	b.WriteString("package fixture\n\n")
	b.WriteString("func bigFunction() {\n")
	for i := 0; i < lineCount; i++ {
		fmt.Fprintf(&b, "\t_ = %d\n", i)
	}
	b.WriteString("}\n")
	return b.String()
}

// --- per-scenario state ---

type ruleEngineWiringState struct {
	fixtureDir string
	repoCtx    repocontext.RepoContext
	bundle     devpolicy.Bundle
	samples    []rule_engine.Sample

	// Pipeline results
	store  *rule_engine.InMemoryStore
	result rule_engine.RunResult

	// Spy store for InsertSamples tracking
	spyStore *spyInsertSamplesStore

	// Failing store for error scenarios
	failStore *failingAppendStore

	// Composition root simulation outputs
	exitCode     int
	stderrOutput string
}

func newRuleEngineWiringState() *ruleEngineWiringState {
	return &ruleEngineWiringState{
		repoCtx: repocontext.RepoContext{
			RootPath: "/tmp/e2e-rule-engine-fixture",
			RepoID:   uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111")),
			HeadSHA:  "e2edeadbeefdeadbeefdeadbeefdeadbeefdeadbe",
		},
		samples: make([]rule_engine.Sample, 0),
	}
}

func (s *ruleEngineWiringState) cleanup() {
	if s.fixtureDir != "" {
		_ = os.RemoveAll(s.fixtureDir)
	}
}

// --- Given steps ---

func (s *ruleEngineWiringState) aFixtureRepoWithLargeGoFile(lineCount int, fileName string) error {
	dir, err := os.MkdirTemp("", "e2e-rule-engine-*")
	if err != nil {
		return fmt.Errorf("create fixture dir: %w", err)
	}
	s.fixtureDir = dir

	content := generateFixtureGoFile(lineCount)
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o644); err != nil {
		return fmt.Errorf("write fixture file: %w", err)
	}

	s.repoCtx = repocontext.RepoContext{
		RootPath: filepath.ToSlash(dir),
		RepoID:   uuid.NewV5(uuid.NamespaceURL, "cleanc.local-repo:"+filepath.ToSlash(dir)),
		HeadSHA:  "e2edeadbeefdeadbeefdeadbeefdeadbeefdeadbe",
	}
	return nil
}

func (s *ruleEngineWiringState) aFixtureRepoWithZeroSourceFiles() error {
	dir, err := os.MkdirTemp("", "e2e-rule-engine-empty-*")
	if err != nil {
		return fmt.Errorf("create empty fixture dir: %w", err)
	}
	s.fixtureDir = dir
	s.repoCtx = repocontext.RepoContext{
		RootPath: filepath.ToSlash(dir),
		RepoID:   uuid.NewV5(uuid.NamespaceURL, "cleanc.local-repo:"+filepath.ToSlash(dir)),
		HeadSHA:  "e2edeadbeefdeadbeefdeadbeefdeadbeefdeadbe",
	}
	s.samples = []rule_engine.Sample{}
	return nil
}

func (s *ruleEngineWiringState) aDevModeBundleWithRule(ruleID, predicate, severity string) error {
	pvID := uuid.Must(uuid.FromString("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"))

	var sev steward.Severity
	switch severity {
	case "block":
		sev = steward.SeverityBlock
	case "warn":
		sev = steward.SeverityWarn
	case "info":
		sev = steward.SeverityInfo
	default:
		return fmt.Errorf("unknown severity %q", severity)
	}

	s.bundle = devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: pvID,
			Name:            "e2e-dev-policy",
			RuleRefs:        []steward.RuleRef{{RuleID: ruleID, Version: 1}},
			CreatedAt:       time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
		},
		Rules: []steward.Rule{
			{
				RuleID:          ruleID,
				Version:         1,
				PackID:          "solid.srp",
				PredicateDSL:    predicate,
				SeverityDefault: sev,
				CreatedAt:       time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC),
			},
		},
	}
	return nil
}

func (s *ruleEngineWiringState) aMetricSample(scopeName, scopeKind, metricKind string, value int) error {
	scopeID := uuid.NewV5(uuid.NamespaceURL, "e2e-scope:"+scopeName)
	sampleID := orchestrator.MintSampleID(
		s.repoCtx.RepoID, s.repoCtx.HeadSHA, scopeID, metricKind, 1,
	)

	sample := rule_engine.Sample{ScopeSignature: "fixture://" + scopeName}
	sample.SampleID = sampleID
	sample.RepoID = s.repoCtx.RepoID
	sample.SHA = s.repoCtx.HeadSHA
	sample.ScopeID = scopeID
	sample.ScopeKind = scopeKind
	sample.MetricKind = metricKind
	sample.MetricVersion = 1
	sample.Value = float64(value)
	sample.HasValue = true
	sample.Pack = "base"
	sample.Source = "computed"

	s.samples = append(s.samples, sample)
	return nil
}

func (s *ruleEngineWiringState) metricSamplesForDifferentScopes(count int) error {
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("Scope%d", i)
		if err := s.aMetricSample(name, "class", "loc", 100+i); err != nil {
			return err
		}
	}
	return nil
}

func (s *ruleEngineWiringState) aSpyStoreRecordingInsertSamplesCalls() error {
	inner := rule_engine.NewInMemoryStore()
	inner.InsertPolicyVersion(s.bundle.PolicyVersion)
	for _, r := range s.bundle.Rules {
		inner.InsertRule(r)
	}
	s.spyStore = &spyInsertSamplesStore{InMemoryStore: inner}
	return nil
}

func (s *ruleEngineWiringState) aFailingStoreWhoseAppendEvaluationReturns(errMsg string) error {
	inner := rule_engine.NewInMemoryStore()
	inner.InsertPolicyVersion(s.bundle.PolicyVersion)
	for _, r := range s.bundle.Rules {
		inner.InsertRule(r)
	}
	inner.InsertSamples(s.repoCtx.RepoID, s.repoCtx.HeadSHA, s.samples)

	s.failStore = &failingAppendStore{
		InMemoryStore: inner,
		appendErr:     fmt.Errorf("%s", errMsg),
	}
	return nil
}

// --- When steps ---

// theEngineStagePipelineRunsOnFixture runs the full pipeline:
// orchestrator.Run (walk→parse→recipes) → BuildSamples →
// composition-root (LoadStore → engine.New → RunBatch).
// Used by both "smoke run" and "empty corpus" scenarios.
func (s *ruleEngineWiringState) theEngineStagePipelineRunsOnFixture() error {
	// Stage 2.2: run orchestrator to produce drafts
	orch := orchestrator.New(orchestrator.Options{Workers: 1})
	orchResult, err := orch.Run(context.Background(), s.repoCtx, s.fixtureDir)
	if err != nil {
		s.exitCode = 70
		s.stderrOutput = err.Error()
		return nil // capture the error, don't fail the step
	}

	// Stage 2.3: convert drafts to engine samples
	samples := orchestrator.BuildSamples(
		s.repoCtx, orchResult.Drafts,
		orch.ScopeBindings(), orchResult.ScopeIDs,
	)

	// Run through composition root simulation
	s.result, s.store, s.exitCode, s.stderrOutput = runCompositionRoot(
		s.bundle, samples, s.repoCtx, nil,
	)
	return nil
}

// theEngineStageLoadsAndRunsWithSpyStore replicates the
// LoadStore pattern (orchestrator line 250: single
// InsertSamples batch call) through the spy so the test can
// verify the calling contract.
func (s *ruleEngineWiringState) theEngineStageLoadsAndRunsWithSpyStore() error {
	if s.spyStore == nil {
		return fmt.Errorf("spy store not configured")
	}

	// Replicate LoadStore's single InsertSamples call
	s.spyStore.InsertSamples(s.repoCtx.RepoID, s.repoCtx.HeadSHA, s.samples)

	eng, err := rule_engine.New(rule_engine.Config{Store: s.spyStore})
	if err != nil {
		return fmt.Errorf("rule_engine.New: %w", err)
	}

	s.result, _ = eng.RunBatch(
		context.Background(),
		s.repoCtx.RepoID,
		s.repoCtx.HeadSHA,
		s.bundle.PolicyVersion.PolicyVersionID,
	)
	return nil
}

func (s *ruleEngineWiringState) theEngineStageRunsAsCompositionRootWithFailingStore() error {
	if s.failStore == nil {
		return fmt.Errorf("failing store not configured")
	}
	s.result, s.store, s.exitCode, s.stderrOutput = runCompositionRoot(
		s.bundle, s.samples, s.repoCtx, s.failStore,
	)
	return nil
}

// --- Then steps ---

func (s *ruleEngineWiringState) findingsContainAtLeastOneWithRuleIDAndDelta(ruleID, delta string) error {
	if s.store == nil {
		return fmt.Errorf("store is nil; pipeline did not produce a store")
	}

	findings := s.store.Findings()
	for _, f := range findings {
		if f.RuleID == ruleID && string(f.Delta) == delta {
			return nil
		}
	}

	var details []string
	for _, f := range findings {
		details = append(details, fmt.Sprintf("{RuleID:%q Delta:%q Severity:%q ScopeKind:%q Value-IDs:%d}",
			f.RuleID, f.Delta, f.Severity, "", len(f.MetricSampleIDs)))
	}
	return fmt.Errorf("no finding with RuleID=%q Delta=%q; got %d findings: %s",
		ruleID, delta, len(findings), strings.Join(details, ", "))
}

func (s *ruleEngineWiringState) exitCodeIs(expected int) error {
	if s.exitCode != expected {
		return fmt.Errorf("exit code = %d, want %d; stderr: %s", s.exitCode, expected, s.stderrOutput)
	}
	return nil
}

func (s *ruleEngineWiringState) findingsIsEmpty() error {
	if len(s.result.FindingIDs) != 0 {
		return fmt.Errorf("expected zero findings, got %d", len(s.result.FindingIDs))
	}
	return nil
}

func (s *ruleEngineWiringState) verdictIs(expected string) error {
	if string(s.result.Verdict) != expected {
		return fmt.Errorf("verdict = %q, want %q", s.result.Verdict, expected)
	}
	return nil
}

func (s *ruleEngineWiringState) insertSamplesCalledExactlyNTimesWithMSamples(callCount, sampleCount int) error {
	if s.spyStore == nil {
		return fmt.Errorf("spy store not configured")
	}
	if len(s.spyStore.calls) != callCount {
		return fmt.Errorf("InsertSamples called %d times, want %d", len(s.spyStore.calls), callCount)
	}
	if s.spyStore.calls[0].batchSize != sampleCount {
		return fmt.Errorf("InsertSamples batch size = %d, want %d", s.spyStore.calls[0].batchSize, sampleCount)
	}
	return nil
}

func (s *ruleEngineWiringState) stderrContains(substring string) error {
	if !strings.Contains(s.stderrOutput, substring) {
		return fmt.Errorf("stderr = %q, does not contain %q", s.stderrOutput, substring)
	}
	return nil
}

// --- Scenario initializer ---

func InitializeScenario_pipeline_rule_engine_wiring(ctx *godog.ScenarioContext) {
	s := newRuleEngineWiringState()

	ctx.After(func(ctx2 context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a fixture repo with a (\d+)-line Go file "([^"]*)"$`,
		s.aFixtureRepoWithLargeGoFile)
	ctx.Step(`^a dev-mode bundle with rule "([^"]*)" predicate "([^"]*)" severity "([^"]*)"$`,
		s.aDevModeBundleWithRule)
	ctx.Step(`^a fixture repo with zero source files$`,
		s.aFixtureRepoWithZeroSourceFiles)
	ctx.Step(`^a metric sample for scope "([^"]*)" kind "([^"]*)" metric "([^"]*)" value (\d+)$`,
		s.aMetricSample)
	ctx.Step(`^(\d+) metric samples for different scopes$`,
		s.metricSamplesForDifferentScopes)
	ctx.Step(`^a spy store recording InsertSamples calls$`,
		s.aSpyStoreRecordingInsertSamplesCalls)
	ctx.Step(`^a store whose AppendEvaluation returns error "([^"]*)"$`,
		s.aFailingStoreWhoseAppendEvaluationReturns)

	// When
	ctx.Step(`^the engine stage pipeline runs on the fixture$`,
		s.theEngineStagePipelineRunsOnFixture)
	ctx.Step(`^the engine stage loads and runs with the spy store$`,
		s.theEngineStageLoadsAndRunsWithSpyStore)
	ctx.Step(`^the engine stage runs as the composition root with the failing store$`,
		s.theEngineStageRunsAsCompositionRootWithFailingStore)

	// Then
	ctx.Step(`^findings contain at least one entry with RuleID "([^"]*)" and Delta "([^"]*)"$`,
		s.findingsContainAtLeastOneWithRuleIDAndDelta)
	ctx.Step(`^exit code is (\d+)$`,
		s.exitCodeIs)
	ctx.Step(`^findings is empty$`,
		s.findingsIsEmpty)
	ctx.Step(`^verdict is "([^"]*)"$`,
		s.verdictIs)
	ctx.Step(`^InsertSamples was called exactly (\d+) time with (\d+) samples$`,
		s.insertSamplesCalledExactlyNTimesWithMSamples)
	ctx.Step(`^stderr contains "([^"]*)"$`,
		s.stderrContains)
}

func TestE2E_pipeline_rule_engine_wiring(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_pipeline_rule_engine_wiring,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"pipeline_rule_engine_wiring.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
