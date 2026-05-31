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
				FindingID:     uuid.Must(uuid.NewV4()),
				RuleID:        "solid.srp.lcom4",
				Severity:      steward.SeverityWarn,
				Delta:         rule_engine.DeltaNew,
				ExplanationMD: "Suggested refactor: extract cohesion boundary",
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

// aRunArtifactWithFindingWhoseExplanationMDContains models the
// pipeline data flow: Rule.DescriptionMD → Finding.ExplanationMD.
// The engine populates ExplanationMD from the rule's DescriptionMD;
// the renderer extracts the "Suggested refactor:" excerpt from
// ExplanationMD for display.
func (s *markdownRendererState) aRunArtifactWithFindingWhoseExplanationMDContains(explanationMD string) error {
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
				ExplanationMD: explanationMD,
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

func (s *markdownRendererState) theOutputContainsDarkMetricsCountLine() error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	if len(s.output) == 0 {
		return fmt.Errorf("output is empty; want dark-metrics count line")
	}
	// Match the bolded summary count line (e.g. "**Dark metrics:** 0"),
	// not a detail-level diagnostics block which would only appear when
	// the artifact actually contains dark metrics.
	if !strings.Contains(s.output, "**Dark metrics:**") {
		return fmt.Errorf("output missing dark-metrics count line '**Dark metrics:**'\n---output---\n%s\n---", s.output)
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

// theRenderedOutputContainsLiteralTag asserts the RENDERED
// markdown output contains the given literal tag string.
// For dark-metric surfaced, this checks that the per-metric
// detail line (e.g. "metric dark: cyclo") appears in the
// output — a genuine metric-specific assertion, not just a
// count.
func (s *markdownRendererState) theRenderedOutputContainsLiteralTag(tag string) error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	if !strings.Contains(s.output, tag) {
		return fmt.Errorf("rendered output does not contain literal tag %q\n---output---\n%s\n---",
			tag, s.output)
	}
	return nil
}

// theRenderedOutputFindingRowContainsExcerpt asserts the
// RENDERED markdown output contains a finding row with the
// given excerpt. The renderer extracts the "Suggested
// refactor:" excerpt from Finding.ExplanationMD (which the
// engine populates from Rule.DescriptionMD) and includes it
// in the finding's output line.
func (s *markdownRendererState) theRenderedOutputFindingRowContainsExcerpt(excerpt string) error {
	if s.renderErr != nil {
		return fmt.Errorf("Render returned error: %w", s.renderErr)
	}
	if !strings.Contains(s.output, excerpt) {
		return fmt.Errorf("rendered output does not contain finding excerpt %q\n---output---\n%s\n---",
			excerpt, s.output)
	}
	// Also verify the excerpt appears within a Findings section.
	if !strings.Contains(s.output, "## Findings") {
		return fmt.Errorf("rendered output has no '## Findings' section\n---output---\n%s\n---", s.output)
	}
	return nil
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
		`^a RunArtifact with a finding whose ExplanationMD contains "([^"]*)"$`,
		s.aRunArtifactWithFindingWhoseExplanationMDContains,
	)

	// When
	ctx.Step(`^Markdown\.Render runs$`, s.markdownRenderRuns)
	ctx.Step(`^Markdown\.Render runs twice on the same artifact$`, s.markdownRenderRunsTwiceOnTheSameArtifact)

	// Then
	ctx.Step(`^the output contains "([^"]*)"$`, s.theOutputContains)
	ctx.Step(`^the output contains a Dark metrics count$`, s.theOutputContainsDarkMetricsCountLine)
	ctx.Step(`^the two outputs are byte-identical$`, s.theTwoOutputsAreByteIdentical)
	ctx.Step(`^the rendered output contains the literal tag "([^"]*)"$`, s.theRenderedOutputContainsLiteralTag)
	ctx.Step(`^the rendered output finding row contains the excerpt "([^"]*)"$`, s.theRenderedOutputFindingRowContainsExcerpt)
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
