//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="pipeline_dark_metric_diagnostics_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// ---------------------------------------------------------------------------
// Per-scenario state
// ---------------------------------------------------------------------------

type darkMetricDiagnosticsState struct {
	fixtureRoot string
	result      *orchestrator.Result
	resultErr   error
	table       *scopebinding.Table

	// moduleRoot is the absolute path to the services/clean-code
	// module root — needed by the subprocess test.
	moduleRoot string
}

func newDarkMetricDiagnosticsState() *darkMetricDiagnosticsState {
	return &darkMetricDiagnosticsState{
		table: scopebinding.NewTable(),
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *darkMetricDiagnosticsState) createFixtureRoot() error {
	if s.fixtureRoot != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "dark-metrics-e2e-*")
	if err != nil {
		return fmt.Errorf("create fixture root: %w", err)
	}
	s.fixtureRoot = dir
	return nil
}

func (s *darkMetricDiagnosticsState) writeFixture(relPath, content string) error {
	if err := s.createFixtureRoot(); err != nil {
		return err
	}
	abs := filepath.Join(s.fixtureRoot, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(abs), err)
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

func (s *darkMetricDiagnosticsState) cleanup() {
	if s.fixtureRoot != "" {
		_ = os.RemoveAll(s.fixtureRoot)
	}
}

func (s *darkMetricDiagnosticsState) repoCtx() repocontext.RepoContext {
	return repocontext.RepoContext{
		RootPath:   s.fixtureRoot,
		RepoID:     repocontext.MintRepoID(s.fixtureRoot),
		HeadSHA:    repocontext.HeadSHAWorkingCopySentinel,
		ModulePath: "",
		IsGitRepo:  false,
	}
}

func (s *darkMetricDiagnosticsState) runOrchestrator(opts orchestrator.Options) error {
	if opts.ScopeBindings == nil {
		opts.ScopeBindings = s.table
	}
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	o := orchestrator.New(opts)
	s.result, s.resultErr = o.Run(context.Background(), s.repoCtx(), s.fixtureRoot)
	return nil
}

// resolveModuleRoot walks up from the test's working directory
// to find the services/clean-code module root (contains go.mod).
func (s *darkMetricDiagnosticsState) resolveModuleRoot() error {
	if s.moduleRoot != "" {
		return nil
	}
	// godog CWD is the feature-file directory; walk up to find go.mod.
	dir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getwd: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			s.moduleRoot = dir
			return nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return fmt.Errorf("could not find go.mod walking up from CWD")
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// bogusRecipe — a fake recipe whose MetricKind is NOT in
// metricAttrRequirements so the dark-metric accumulator
// silently ignores it.
// ---------------------------------------------------------------------------

type bogusRecipe struct{}

func (r *bogusRecipe) MetricKind() string                                    { return "bogus_metric" }
func (r *bogusRecipe) Version() int                                          { return 1 }
func (r *bogusRecipe) Pack() recipes.Pack                                    { return recipes.PackBase }
func (r *bogusRecipe) AppliesTo(_ *parser.AstFile) bool                      { return false }
func (r *bogusRecipe) Compute(_ *parser.AstFile) []recipes.MetricSampleDraft { return nil }

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *darkMetricDiagnosticsState) aFixtureGoFileWithOneFunctionWhoseParserDoesNotStampDecisionBlocks() error {
	return s.writeFixture("pkg/widget.go", strings.Join([]string{
		"package pkg",
		"",
		"func Run() int { return 1 }",
		"",
	}, "\n"))
}

func (s *darkMetricDiagnosticsState) aRecipeRegisteredWithBogusMetricKindAndFakeAppliesToReturningFalse() error {
	return s.writeFixture("pkg/stub.go", "package pkg\n\nfunc Stub() {}\n")
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *darkMetricDiagnosticsState) theOrchestratorRuns() error {
	return s.runOrchestrator(orchestrator.Options{})
}

func (s *darkMetricDiagnosticsState) theOrchestratorRunsWithTheBogusRecipe() error {
	reg := recipes.NewRegistry()
	reg.Register(recipes.NewLocRecipe())
	reg.Register(&bogusRecipe{})
	return s.runOrchestrator(orchestrator.Options{Recipes: reg})
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *darkMetricDiagnosticsState) darkMetricsIncludesCycloRowExact(metricKind, language, missingAttr string, exactCount int, closurePhase string) error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	if s.result == nil {
		return fmt.Errorf("orchestrator returned nil result")
	}
	for _, dm := range s.result.Diagnostics.DarkMetrics {
		if dm.MetricKind == metricKind && dm.Language == language {
			if len(dm.MissingAttrs) != 1 || dm.MissingAttrs[0] != missingAttr {
				return fmt.Errorf("(%q,%q).MissingAttrs = %v, want [%q]",
					metricKind, language, dm.MissingAttrs, missingAttr)
			}
			if dm.AffectedScopeCount != exactCount {
				return fmt.Errorf("(%q,%q).AffectedScopeCount = %d, want exactly %d",
					metricKind, language, dm.AffectedScopeCount, exactCount)
			}
			if dm.ClosurePhase != closurePhase {
				return fmt.Errorf("(%q,%q).ClosurePhase = %q, want %q",
					metricKind, language, dm.ClosurePhase, closurePhase)
			}
			return nil
		}
	}
	var kinds []string
	for _, dm := range s.result.Diagnostics.DarkMetrics {
		kinds = append(kinds, fmt.Sprintf("(%s,%s)", dm.MetricKind, dm.Language))
	}
	return fmt.Errorf("no DarkMetric row for (%q,%q); got %v", metricKind, language, kinds)
}

func (s *darkMetricDiagnosticsState) darkMetricsDoesNotIncludeMetricKind(metricKind string) error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	if s.result == nil {
		return fmt.Errorf("orchestrator returned nil result")
	}
	for _, dm := range s.result.Diagnostics.DarkMetrics {
		if dm.MetricKind == metricKind {
			return fmt.Errorf("metric_kind=%q found in DarkMetrics but should NOT be flagged dark",
				metricKind)
		}
	}
	return nil
}

func (s *darkMetricDiagnosticsState) bogusAttrValidationBinaryExitsCode70WithStderrMatchingTechSpec() error {
	if err := s.resolveModuleRoot(); err != nil {
		return err
	}

	// Build the helper binary into a temp directory so we control
	// the exit code directly (go run can mask it).
	tmpDir, err := os.MkdirTemp("", "bogus-attr-bin-*")
	if err != nil {
		return fmt.Errorf("mktmpdir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	binName := "bogus_attr_exit70"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)

	buildCmd := exec.Command("go", "build", "-o", binPath,
		"./test/e2e/code-intelligence-REFACTOR-GUIDE/testdata/bogus_attr_exit70")
	buildCmd.Dir = s.moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("go build helper: %v\n%s", err, out)
	}

	// Run the helper and capture stderr + exit code.
	runCmd := exec.Command(binPath)
	var stderrBuf strings.Builder
	runCmd.Stderr = &stderrBuf
	runErr := runCmd.Run()

	// Assert exit code 70.
	if runErr == nil {
		return fmt.Errorf("helper exited with code 0; want 70")
	}
	exitErr, ok := runErr.(*exec.ExitError)
	if !ok {
		return fmt.Errorf("unexpected error type %T: %v", runErr, runErr)
	}
	if exitErr.ExitCode() != 70 {
		return fmt.Errorf("exit code = %d; want 70\nstderr: %s", exitErr.ExitCode(), stderrBuf.String())
	}

	// Assert stderr contains the tech-spec Sec 8.7 error string.
	stderr := stderrBuf.String()
	expectedFragment := "NOT in the closed dark-metric attr set (tech-spec Sec 8.7"
	if !strings.Contains(stderr, expectedFragment) {
		return fmt.Errorf("stderr %q does not contain %q", stderr, expectedFragment)
	}
	return nil
}

func (s *darkMetricDiagnosticsState) diagnosticsEffortSourceIs(expected string) error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	if s.result == nil {
		return fmt.Errorf("orchestrator returned nil result")
	}
	if s.result.Diagnostics.EffortSource != expected {
		return fmt.Errorf("Diagnostics.EffortSource = %q, want %q",
			s.result.Diagnostics.EffortSource, expected)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_pipeline_dark_metric_diagnostics(ctx *godog.ScenarioContext) {
	s := newDarkMetricDiagnosticsState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(
		`^a fixture Go file with one function whose parser does not stamp decision_blocks$`,
		s.aFixtureGoFileWithOneFunctionWhoseParserDoesNotStampDecisionBlocks,
	)
	ctx.Step(
		`^a recipe registered with MetricKind "bogus_metric" not in metricAttrRequirements and a fake AppliesTo returning false$`,
		s.aRecipeRegisteredWithBogusMetricKindAndFakeAppliesToReturningFalse,
	)

	// When
	ctx.Step(`^the orchestrator runs$`, s.theOrchestratorRuns)
	ctx.Step(`^the orchestrator runs with the bogus recipe$`, s.theOrchestratorRunsWithTheBogusRecipe)

	// Then
	ctx.Step(
		`^Diagnostics\.DarkMetrics includes a row with metric_kind "([^"]*)", language "([^"]*)", missing_attrs \["([^"]*)"\], affected_scope_count (\d+), and closure_phase "([^"]*)"$`,
		s.darkMetricsIncludesCycloRowExact,
	)
	ctx.Step(
		`^Diagnostics\.DarkMetrics does not include any row with metric_kind "([^"]*)"$`,
		s.darkMetricsDoesNotIncludeMetricKind,
	)
	ctx.Step(
		`^the dark-metric diagnostic does not include metric_kind "([^"]*)"$`,
		s.darkMetricsDoesNotIncludeMetricKind,
	)
	ctx.Step(
		`^the bogus_attr validation binary exits code 70 with stderr matching tech-spec Sec 8\.7$`,
		s.bogusAttrValidationBinaryExitsCode70WithStderrMatchingTechSpec,
	)
	ctx.Step(
		`^Diagnostics\.EffortSource is "([^"]*)"$`,
		s.diagnosticsEffortSourceIs,
	)
}

func TestE2E_pipeline_dark_metric_diagnostics(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_pipeline_dark_metric_diagnostics,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"pipeline_dark_metric_diagnostics.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
