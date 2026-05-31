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

// failingAppendStore wraps InMemoryStore and overrides
// AppendEvaluation to return a configurable error, exercising
// the engine-error-surfaces acceptance scenario.
type failingAppendStore struct {
	*rule_engine.InMemoryStore
	appendErr error
}

// WithEvaluationLock delegates locking to the inner store but
// passes the wrapper as the Store so AppendEvaluation hits
// the failing override.
func (f *failingAppendStore) WithEvaluationLock(ctx context.Context, repoID uuid.UUID, sha string, fn func(rule_engine.Store) error) error {
	return f.InMemoryStore.WithEvaluationLock(ctx, repoID, sha, func(_ rule_engine.Store) error {
		return fn(f)
	})
}

// AppendEvaluation returns the injected error unconditionally.
func (f *failingAppendStore) AppendEvaluation(_ context.Context, _ rule_engine.EvaluationRun, _ rule_engine.EvaluationVerdict, _ []rule_engine.Finding) error {
	return f.appendErr
}

// --- per-scenario state ---

type ruleEngineWiringState struct {
	repoCtx repocontext.RepoContext
	bundle  devpolicy.Bundle
	samples []rule_engine.Sample

	store  *rule_engine.InMemoryStore
	result rule_engine.RunResult
	runErr error

	failStore *failingAppendStore
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

// --- Given steps ---

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

	sample := rule_engine.Sample{
		ScopeSignature: "fixture://" + scopeName,
	}
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

func (s *ruleEngineWiringState) zeroMetricSamples() error {
	s.samples = []rule_engine.Sample{}
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

func (s *ruleEngineWiringState) theOrchestratorRunsTheEngineStage() error {
	store, err := orchestrator.LoadStore(s.bundle, s.samples, s.repoCtx)
	if err != nil {
		return fmt.Errorf("LoadStore: %w", err)
	}
	s.store = store

	eng, err := rule_engine.New(rule_engine.Config{Store: store})
	if err != nil {
		return fmt.Errorf("rule_engine.New: %w", err)
	}

	s.result, s.runErr = eng.RunBatch(
		context.Background(),
		s.repoCtx.RepoID,
		s.repoCtx.HeadSHA,
		s.bundle.PolicyVersion.PolicyVersionID,
	)
	return nil
}

func (s *ruleEngineWiringState) loadStoreIsCalledWithTheSamples() error {
	store, err := orchestrator.LoadStore(s.bundle, s.samples, s.repoCtx)
	if err != nil {
		return fmt.Errorf("LoadStore: %w", err)
	}
	s.store = store
	return nil
}

func (s *ruleEngineWiringState) runBatchExecutesAgainstTheFailingStore() error {
	if s.failStore == nil {
		return fmt.Errorf("failStore not configured; add a Given step to create one")
	}

	eng, err := rule_engine.New(rule_engine.Config{Store: s.failStore})
	if err != nil {
		return fmt.Errorf("rule_engine.New: %w", err)
	}

	s.result, s.runErr = eng.RunBatch(
		context.Background(),
		s.repoCtx.RepoID,
		s.repoCtx.HeadSHA,
		s.bundle.PolicyVersion.PolicyVersionID,
	)
	return nil
}

// --- Then steps ---

func (s *ruleEngineWiringState) findingsContainAtLeastOneWithRuleIDAndDelta(ruleID, delta string) error {
	if s.runErr != nil {
		return fmt.Errorf("RunBatch failed unexpectedly: %v", s.runErr)
	}

	findings := s.store.Findings()
	for _, f := range findings {
		if f.RuleID == ruleID && string(f.Delta) == delta {
			return nil
		}
	}

	var details []string
	for _, f := range findings {
		details = append(details, fmt.Sprintf("{RuleID:%q Delta:%q Severity:%q}",
			f.RuleID, f.Delta, f.Severity))
	}
	return fmt.Errorf("no finding with RuleID=%q Delta=%q; got %d findings: %s",
		ruleID, delta, len(findings), strings.Join(details, ", "))
}

func (s *ruleEngineWiringState) findingsIsEmpty() error {
	if s.runErr != nil {
		return fmt.Errorf("RunBatch failed unexpectedly: %v", s.runErr)
	}
	if len(s.result.FindingIDs) != 0 {
		return fmt.Errorf("expected zero findings, got %d", len(s.result.FindingIDs))
	}
	return nil
}

func (s *ruleEngineWiringState) verdictIs(expected string) error {
	if s.runErr != nil {
		return fmt.Errorf("RunBatch failed unexpectedly: %v", s.runErr)
	}
	if string(s.result.Verdict) != expected {
		return fmt.Errorf("verdict = %q, want %q", s.result.Verdict, expected)
	}
	return nil
}

func (s *ruleEngineWiringState) theStoreContainsExactlyNMetricSamples(count int) error {
	if s.store == nil {
		return fmt.Errorf("store is nil; When step did not run or LoadStore failed")
	}
	rows, err := s.store.ListMetricSamples(
		context.Background(), s.repoCtx.RepoID, s.repoCtx.HeadSHA, nil,
	)
	if err != nil {
		return fmt.Errorf("ListMetricSamples: %w", err)
	}
	if len(rows) != count {
		return fmt.Errorf("ListMetricSamples returned %d samples, want %d", len(rows), count)
	}
	return nil
}

func (s *ruleEngineWiringState) runBatchReturnsAnErrorContaining(substring string) error {
	if s.runErr == nil {
		return fmt.Errorf("RunBatch did not return an error; expected error containing %q", substring)
	}
	if !strings.Contains(s.runErr.Error(), substring) {
		return fmt.Errorf("RunBatch error = %q, does not contain %q", s.runErr.Error(), substring)
	}
	return nil
}

func (s *ruleEngineWiringState) theErrorMapsToExitCode70() error {
	// The CLI composition root maps any engine.RunBatch error
	// to exit code 70 (EX_SOFTWARE per tech-spec Sec 8.6).
	// This step verifies the error is non-nil (which the CLI
	// would map to exit 70). The binary-level assertion is
	// deferred to Stage 6's CLI binary skeleton e2e.
	if s.runErr == nil {
		return fmt.Errorf("expected a non-nil error to map to exit code 70")
	}
	return nil
}

// --- Scenario initializer ---

func InitializeScenario_pipeline_rule_engine_wiring(ctx *godog.ScenarioContext) {
	s := newRuleEngineWiringState()

	// Given
	ctx.Step(`^a dev-mode bundle with rule "([^"]*)" predicate "([^"]*)" severity "([^"]*)"$`,
		s.aDevModeBundleWithRule)
	ctx.Step(`^a metric sample for scope "([^"]*)" kind "([^"]*)" metric "([^"]*)" value (\d+)$`,
		s.aMetricSample)
	ctx.Step(`^zero metric samples$`,
		s.zeroMetricSamples)
	ctx.Step(`^(\d+) metric samples for different scopes$`,
		s.metricSamplesForDifferentScopes)
	ctx.Step(`^a store whose AppendEvaluation returns error "([^"]*)"$`,
		s.aFailingStoreWhoseAppendEvaluationReturns)

	// When
	ctx.Step(`^the orchestrator runs the engine stage$`,
		s.theOrchestratorRunsTheEngineStage)
	ctx.Step(`^LoadStore is called with the samples$`,
		s.loadStoreIsCalledWithTheSamples)
	ctx.Step(`^RunBatch executes against the failing store$`,
		s.runBatchExecutesAgainstTheFailingStore)

	// Then
	ctx.Step(`^findings contain at least one entry with RuleID "([^"]*)" and Delta "([^"]*)"$`,
		s.findingsContainAtLeastOneWithRuleIDAndDelta)
	ctx.Step(`^findings is empty$`,
		s.findingsIsEmpty)
	ctx.Step(`^verdict is "([^"]*)"$`,
		s.verdictIs)
	ctx.Step(`^the store contains exactly (\d+) metric samples$`,
		s.theStoreContainsExactlyNMetricSamples)
	ctx.Step(`^RunBatch returns an error containing "([^"]*)"$`,
		s.runBatchReturnsAnErrorContaining)
	ctx.Step(`^the error maps to exit code 70$`,
		s.theErrorMapsToExitCode70)
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
