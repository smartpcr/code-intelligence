//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor_test.go" company="Microsoft Corp.">
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

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/suggest"
)

// snippetState holds per-scenario state for snippet extractor e2e scenarios.
type snippetState struct {
	fixtureDir  string
	fixturePath string
	maxLines    int
	startLine   int
	endLine     int
	snippet     string
	truncated   bool
	err         error

	// rawContent is the exact bytes written to disk for
	// the raw-bytes scenario so the assertion can compare.
	rawContent string

	// record holds the RefactorPromptRecord for the
	// metric-evidence-join scenario.
	record suggest.RefactorPromptRecord
}

func newSnippetState() *snippetState {
	return &snippetState{}
}

func (s *snippetState) cleanup() {
	if s.fixtureDir != "" {
		os.RemoveAll(s.fixtureDir)
	}
}

// writeFixture creates a temp file with the given content and
// records fixtureDir / fixturePath.
func (s *snippetState) writeFixture(name, content string) error {
	dir, err := os.MkdirTemp("", "snippet-e2e-*")
	if err != nil {
		return err
	}
	s.fixtureDir = dir
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		return err
	}
	s.fixturePath = p
	return nil
}

// --- Given steps ---------------------------------------------------------

func (s *snippetState) a500LineFixtureFile() error {
	var sb strings.Builder
	for i := 1; i <= 500; i++ {
		fmt.Fprintf(&sb, "line%03d\n", i)
	}
	return s.writeFixture("big.txt", sb.String())
}

func (s *snippetState) a50LineFixtureFile() error {
	var sb strings.Builder
	for i := 1; i <= 50; i++ {
		fmt.Fprintf(&sb, "line%02d\n", i)
	}
	return s.writeFixture("small.txt", sb.String())
}

func (s *snippetState) maxLinesIsSetTo(n int) error {
	s.maxLines = n
	return nil
}

func (s *snippetState) aFixtureFileContainingTabAndUTF8() error {
	// Literal tab followed by multi-byte UTF-8 (Japanese).
	s.rawContent = "\t日本語 // コメント\n  spaced\t/* c */  \n"
	return s.writeFixture("utf8.go", s.rawContent)
}

func (s *snippetState) aRefactorPromptRecordWithMetricEvidence(
	metricKind string, value float64, threshold float64, op string,
) error {
	s.record = suggest.RefactorPromptRecord{
		TaskID:              "task-001",
		PlanID:              "plan-001",
		PromptFormatVersion: suggest.PromptFormatVersion,
		MetricEvidence: []suggest.MetricEvidence{
			{
				MetricKind: metricKind,
				Value:      value,
				Threshold:  threshold,
				Op:         op,
			},
		},
	}
	return nil
}

// --- When steps ----------------------------------------------------------

func (s *snippetState) extractSnippetRunsOverLinesToEndLine(start, end int) error {
	s.startLine = start
	s.endLine = end
	s.snippet, s.truncated, s.err = suggest.ExtractSnippet(
		s.fixturePath, s.startLine, s.endLine, s.maxLines,
	)
	return s.err
}

func (s *snippetState) extractSnippetRunsOverFullFileRange() error {
	lineCount := strings.Count(s.rawContent, "\n")
	if !strings.HasSuffix(s.rawContent, "\n") {
		lineCount++
	}
	s.startLine = 1
	s.endLine = lineCount
	s.maxLines = lineCount + 100 // well above the line count
	s.snippet, s.truncated, s.err = suggest.ExtractSnippet(
		s.fixturePath, s.startLine, s.endLine, s.maxLines,
	)
	return s.err
}

// --- Then steps ----------------------------------------------------------

func (s *snippetState) theReturnedStringHasExactlyNLines(n int) error {
	got := countSnippetLines(s.snippet)
	if got != n {
		return fmt.Errorf("expected %d lines, got %d", n, got)
	}
	return nil
}

func (s *snippetState) truncatedIsTrue() error {
	if !s.truncated {
		return fmt.Errorf("expected truncated=true, got false")
	}
	return nil
}

func (s *snippetState) truncatedIsFalse() error {
	if s.truncated {
		return fmt.Errorf("expected truncated=false, got true")
	}
	return nil
}

func (s *snippetState) theLastLineIs(want string) error {
	lines := splitSnippetLines(s.snippet)
	if len(lines) == 0 {
		return fmt.Errorf("snippet is empty")
	}
	last := lines[len(lines)-1]
	if last != want {
		return fmt.Errorf("last line = %q, want %q", last, want)
	}
	return nil
}

func (s *snippetState) theSnippetContainsExactlyNLines(n int) error {
	return s.theReturnedStringHasExactlyNLines(n)
}

func (s *snippetState) theReturnedSnippetPreservesExactByteSequence() error {
	if s.snippet != s.rawContent {
		return fmt.Errorf(
			"raw bytes not preserved.\n got=%q\nwant=%q",
			s.snippet, s.rawContent,
		)
	}
	return nil
}

func (s *snippetState) metricEvidenceContainsExactlyNEntries(n int) error {
	got := len(s.record.MetricEvidence)
	if got != n {
		return fmt.Errorf("expected %d metric_evidence entries, got %d", n, got)
	}
	return nil
}

func (s *snippetState) theEntryHas(
	metricKind string, value float64, threshold float64, op string,
) error {
	if len(s.record.MetricEvidence) == 0 {
		return fmt.Errorf("metric_evidence is empty")
	}
	e := s.record.MetricEvidence[0]
	if e.MetricKind != metricKind {
		return fmt.Errorf("metric_kind = %q, want %q", e.MetricKind, metricKind)
	}
	if e.Value != value {
		return fmt.Errorf("value = %v, want %v", e.Value, value)
	}
	if e.Threshold != threshold {
		return fmt.Errorf("threshold = %v, want %v", e.Threshold, threshold)
	}
	if e.Op != op {
		return fmt.Errorf("op = %q, want %q", e.Op, op)
	}
	return nil
}

// --- helpers -------------------------------------------------------------

// countSnippetLines counts lines in a snippet. A line is
// terminated by '\n' or is a non-empty tail without one.
func countSnippetLines(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// splitSnippetLines splits on '\n' and strips trailing empty
// element when the input ends with '\n'.
func splitSnippetLines(s string) []string {
	if s == "" {
		return nil
	}
	trimmed := strings.TrimSuffix(s, "\n")
	parts := strings.Split(trimmed, "\n")
	for i, p := range parts {
		parts[i] = strings.TrimSuffix(p, "\r")
	}
	return parts
}

// --- godog wiring --------------------------------------------------------

func InitializeScenario_p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor(ctx *godog.ScenarioContext) {
	s := newSnippetState()

	ctx.After(func(ctx2 context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a 500-line fixture file$`, s.a500LineFixtureFile)
	ctx.Step(`^a 50-line fixture file$`, s.a50LineFixtureFile)
	ctx.Step(`^maxLines is set to (\d+)$`, s.maxLinesIsSetTo)
	ctx.Step(`^a fixture file containing a literal tab followed by a multi-byte UTF-8 sequence$`, s.aFixtureFileContainingTabAndUTF8)
	ctx.Step(`^a RefactorPromptRecord with one metric evidence entry for metric_kind "([^"]*)" value (\d+) threshold (\d+) op "([^"]*)"$`, s.aRefactorPromptRecordWithMetricEvidence)

	// When
	ctx.Step(`^ExtractSnippet runs over lines (\d+) to (\d+)$`, s.extractSnippetRunsOverLinesToEndLine)
	ctx.Step(`^ExtractSnippet runs over the full file range$`, s.extractSnippetRunsOverFullFileRange)

	// Then
	ctx.Step(`^the returned string has exactly (\d+) lines$`, s.theReturnedStringHasExactlyNLines)
	ctx.Step(`^truncated is true$`, s.truncatedIsTrue)
	ctx.Step(`^truncated is false$`, s.truncatedIsFalse)
	ctx.Step(`^the last line is "([^"]*)"$`, s.theLastLineIs)
	ctx.Step(`^the snippet contains exactly (\d+) lines$`, s.theSnippetContainsExactlyNLines)
	ctx.Step(`^the returned snippet preserves the exact byte sequence$`, s.theReturnedSnippetPreservesExactByteSequence)
	ctx.Step(`^metric_evidence contains exactly (\d+) entry$`, s.metricEvidenceContainsExactlyNEntries)
	ctx.Step(`^the entry has metric_kind "([^"]*)" value (\d+) threshold (\d+) op "([^"]*)"$`, s.theEntryHas)
}

func TestE2E_p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"p1_structured_prompt_emitter_prompt_record_and_source_snippet_extractor.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
