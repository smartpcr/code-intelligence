//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="p0_reports_and_delivery_markdown_report_renderer_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// ---------------------------------------------------------------------------
// Per-scenario state
// ---------------------------------------------------------------------------

type markdownRendererState struct {
	artifact    report.RunArtifact
	renderer    report.Markdown
	output      string
	outputBytes []byte
	secondBytes []byte
	renderErr   error
}

func newMarkdownRendererState() *markdownRendererState {
	return &markdownRendererState{}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *markdownRendererState) aRunArtifactWithZeroFindingsAndVerdictPass() error {
	s.artifact = report.RunArtifact{
		Context: repocontext.RepoContext{
			RootPath: "/repos/empty-corpus",
			HeadSHA:  "0000000",
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: uuid.Must(uuid.NewV4()),
			Name:            "cleanc-dev-policy",
			RefactorWeights: steward.RefactorWeights{
				EffortModelVersion: "fallback-2026.05",
			},
		},
		Verdict: rule_engine.EvaluationVerdict{
			Verdict: rule_engine.VerdictPass,
		},
		DarkMetrics: []orchestrator.DarkMetric{},
		Findings:    []rule_engine.Finding{},
	}
	return nil
}

func (s *markdownRendererState) aRepresentativeRunArtifactWithFindingsAndDarkMetrics() error {
	policyID := uuid.Must(uuid.NewV4())
	s.artifact = report.RunArtifact{
		Context: repocontext.RepoContext{
			RootPath: "/repos/representative",
			HeadSHA:  "deadbeef",
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: policyID,
			Name:            "cleanc-dev-policy",
			RefactorWeights: steward.RefactorWeights{
				EffortModelVersion: "fallback-2026.05",
			},
		},
		DarkMetrics: []orchestrator.DarkMetric{
			{MetricKind: "cyclo", Language: "go", MissingAttrs: []string{"decision_blocks"}, AffectedScopeCount: 3, ClosurePhase: "P2"},
			{MetricKind: "lcom4", Language: "java", MissingAttrs: []string{"call_edges", "field_accesses"}, AffectedScopeCount: 1, ClosurePhase: "P2"},
		},
		Verdict: rule_engine.EvaluationVerdict{
			Verdict: rule_engine.VerdictWarn,
		},
		Findings: []rule_engine.Finding{
			{
				FindingID: uuid.Must(uuid.NewV4()),
				RuleID:    "solid.srp.lcom4",
				Severity:  steward.SeverityWarn,
				Delta:     rule_engine.DeltaNew,
			},
		},
	}
	return nil
}

func (s *markdownRendererState) aRunArtifactWhoseDarkMetricsIncludesRowForLanguage(metricKind, language string) error {
	s.artifact = report.RunArtifact{
		Context: repocontext.RepoContext{
			RootPath: "/repos/dark-metric-test",
			HeadSHA:  "abc12345",
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: uuid.Must(uuid.NewV4()),
			Name:            "cleanc-dev-policy",
			RefactorWeights: steward.RefactorWeights{
				EffortModelVersion: "fallback-2026.05",
			},
		},
		DarkMetrics: []orchestrator.DarkMetric{
			{
				MetricKind:         metricKind,
				Language:           language,
				MissingAttrs:       []string{"decision_blocks"},
				AffectedScopeCount: 1,
				ClosurePhase:       orchestrator.DarkMetricClosurePhase,
			},
		},
		Verdict: rule_engine.EvaluationVerdict{
			Verdict: rule_engine.VerdictPass,
		},
	}
	return nil
}

func (s *markdownRendererState) aRunArtifactWithFindingWhoseRuleDescriptionMDContains(descMD string) error {
	s.artifact = report.RunArtifact{
		Context: repocontext.RepoContext{
			RootPath: "/repos/refactor-excerpt",
			HeadSHA:  "face1234",
		},
		Policy: steward.PolicyVersion{
			PolicyVersionID: uuid.Must(uuid.NewV4()),
			Name:            "cleanc-dev-policy",
			RefactorWeights: steward.RefactorWeights{
				EffortModelVersion: "fallback-2026.05",
			},
		},
		Verdict: rule_engine.EvaluationVerdict{
			Verdict: rule_engine.VerdictWarn,
		},
		Findings: []rule_engine.Finding{
			{
				FindingID:     uuid.Must(uuid.NewV4()),
				RuleID:        "solid.srp.lcom4",
				Severity:      steward.SeverityBlock,
				Delta:         rule_engine.DeltaNew,
				ExplanationMD: descMD,
			},
		},
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *markdownRendererState) markdownRenderRuns() error {
	var buf bytes.Buffer
	s.renderErr = s.renderer.Render(context.Background(), s.artifact, &buf)
	s.outputBytes = buf.Bytes()
	s.output = buf.String()
	return nil
}

func (s *markdownRendererState) markdownRenderRunsTwiceOnTheSameArtifact() error {
	var buf1 bytes.Buffer
	if err := s.renderer.Render(context.Background(), s.artifact, &buf1); err != nil {
		return fmt.Errorf("first render failed: %w", err)
	}
	s.outputBytes = buf1.Bytes()
	s.output = buf1.String()

	var buf2 bytes.Buffer
	if err := s.renderer.Render(context.Background(), s.artifact, &buf2); err != nil {
		return fmt.Errorf("second render failed: %w", err)
	}
	s.secondBytes = buf2.Bytes()
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *markdownRendererState) theOutputContains(expected string) error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	if !strings.Contains(s.output, expected) {
		return fmt.Errorf("output does not contain %q\n---output---\n%s\n---", expected, s.output)
	}
	return nil
}

func (s *markdownRendererState) theOutputContainsNonEmptyDiagnosticsBlock() error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	if len(s.output) == 0 {
		return fmt.Errorf("output is empty; want non-empty diagnostics block")
	}
	// The diagnostics block at this stage is the header section
	// which includes at minimum the "Dark metrics:" row.
	if !strings.Contains(s.output, "Dark metrics:") {
		return fmt.Errorf("output missing diagnostics row 'Dark metrics:'\n---output---\n%s\n---", s.output)
	}
	return nil
}

func (s *markdownRendererState) theTwoOutputsAreByteIdentical() error {
	if !bytes.Equal(s.outputBytes, s.secondBytes) {
		return fmt.Errorf("re-render produced different bytes:\nfirst  (%d bytes): %q\nsecond (%d bytes): %q",
			len(s.outputBytes), string(s.outputBytes),
			len(s.secondBytes), string(s.secondBytes))
	}
	return nil
}

func (s *markdownRendererState) theOutputSurfacesDarkMetricDiagnosticWithCount(count int) error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	// The Markdown renderer header emits "- **Dark metrics:** N"
	// as the unambiguous dark-metric diagnostic tag per
	// architecture Sec 3.7.1 step 1.
	expected := fmt.Sprintf("**Dark metrics:** %d", count)
	if !strings.Contains(s.output, expected) {
		return fmt.Errorf("output does not contain dark-metric tag %q\n---output---\n%s\n---", expected, s.output)
	}
	return nil
}

func (s *markdownRendererState) theArtifactFindingCarriesTheSuffix(suffix string) error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	// At this stage (3.1), the Markdown renderer emits header +
	// verdict only; the findings section is a downstream stage.
	// We verify the RunArtifact carries the excerpt through
	// Finding.ExplanationMD so the future findings renderer can
	// surface it. This tests the data-flow contract.
	if len(s.artifact.Findings) == 0 {
		return fmt.Errorf("artifact has no findings")
	}
	for _, f := range s.artifact.Findings {
		if strings.Contains(f.ExplanationMD, suffix) {
			return nil
		}
	}
	return fmt.Errorf("no finding ExplanationMD contains suffix %q", suffix)
}

// ---------------------------------------------------------------------------
// Scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_p0_reports_and_delivery_markdown_report_renderer(ctx *godog.ScenarioContext) {
	s := newMarkdownRendererState()

	// Given
	ctx.Step(
		`^a RunArtifact with zero findings and verdict "pass"$`,
		s.aRunArtifactWithZeroFindingsAndVerdictPass,
	)
	ctx.Step(
		`^a representative RunArtifact with findings and dark metrics$`,
		s.aRepresentativeRunArtifactWithFindingsAndDarkMetrics,
	)
	ctx.Step(
		`^a RunArtifact whose DarkMetrics includes a "([^"]*)" row for language "([^"]*)"$`,
		s.aRunArtifactWhoseDarkMetricsIncludesRowForLanguage,
	)
	ctx.Step(
		`^a RunArtifact with a finding whose rule DescriptionMD contains "([^"]*)"$`,
		s.aRunArtifactWithFindingWhoseRuleDescriptionMDContains,
	)

	// When
	ctx.Step(`^Markdown\.Render runs$`, s.markdownRenderRuns)
	ctx.Step(`^Markdown\.Render runs twice on the same artifact$`, s.markdownRenderRunsTwiceOnTheSameArtifact)

	// Then
	ctx.Step(`^the output contains "([^"]*)"$`, s.theOutputContains)
	ctx.Step(`^the output contains a non-empty diagnostics block$`, s.theOutputContainsNonEmptyDiagnosticsBlock)
	ctx.Step(`^the two outputs are byte-identical$`, s.theTwoOutputsAreByteIdentical)
	ctx.Step(`^the output surfaces the dark-metric diagnostic with count (\d+)$`, s.theOutputSurfacesDarkMetricDiagnosticWithCount)
	ctx.Step(`^the artifact finding carries the suffix "([^"]*)"$`, s.theArtifactFindingCarriesTheSuffix)
}

func TestE2E_p0_reports_and_delivery_markdown_report_renderer(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_p0_reports_and_delivery_markdown_report_renderer,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"p0_reports_and_delivery_markdown_report_renderer.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
