//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="p1_structured_prompt_emitter_emit_prompts_flag_wiring_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
)

// emitPromptsWiringState holds per-scenario state for the
// --emit-prompts flag wiring e2e scenarios.
type emitPromptsWiringState struct {
	binaryPath  string
	moduleRoot  string
	tmpDir      string
	fixtureRoot string

	stdout   string
	stderr   string
	exitCode int

	promptsPath string
	reportPath  string
	taskCount   int
}

func newEmitPromptsWiringState() *emitPromptsWiringState {
	return &emitPromptsWiringState{}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *emitPromptsWiringState) runCleancEmitPrompts(args ...string) error {
	cmd := exec.Command(s.binaryPath, args...)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	err := cmd.Run()
	s.stdout = stdoutBuf.String()
	s.stderr = stderrBuf.String()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			s.exitCode = exitErr.ExitCode()
		} else {
			return fmt.Errorf("command execution failed: %w", err)
		}
	} else {
		s.exitCode = 0
	}
	return nil
}

func (s *emitPromptsWiringState) cleanup() {
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir)
	}
	// Clean up dynamically-created fixture dirs (zero-task
	// fixtures live outside tmpDir).
	if s.fixtureRoot != "" && strings.Contains(s.fixtureRoot, "cleanc-e2e-zero-tasks") {
		_ = os.RemoveAll(s.fixtureRoot)
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *emitPromptsWiringState) aBuiltCleancBinaryForEmitPromptsWiring() error {
	s.moduleRoot = resolveModuleRootForWiring()
	s.binaryPath = resolveBinaryPath(s.moduleRoot)

	if _, err := os.Stat(s.binaryPath); err != nil {
		if buildErr := buildCleancBinary(s.moduleRoot, s.binaryPath); buildErr != nil {
			return fmt.Errorf("bin/cleanc not found and build failed: %w", buildErr)
		}
		if _, err2 := os.Stat(s.binaryPath); err2 != nil {
			return fmt.Errorf("build succeeded but bin/cleanc still not found at %s: %w", s.binaryPath, err2)
		}
	}

	tmpDir, err := os.MkdirTemp("", "cleanc-e2e-emit-prompts-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	s.tmpDir = tmpDir
	return nil
}

// aFixtureThatProducesRefactorTasks uses the checked-in loc-srp
// fixture which produces warn-severity SRP findings and 2
// refactor tasks — enough to exercise the JSONL emitter without
// tripping the --exit-on block threshold (exit 0).
func (s *emitPromptsWiringState) aFixtureThatProducesRefactorTasks() error {
	s.fixtureRoot = filepath.Join(s.moduleRoot, "internal", "cli", "testdata", "fixtures", "loc-srp")
	if _, err := os.Stat(filepath.Join(s.fixtureRoot, "go.mod")); err != nil {
		return fmt.Errorf("loc-srp fixture not found at %s: %w", s.fixtureRoot, err)
	}
	return nil
}

// aMinimalFixtureProducingZeroTasks creates a temporary
// directory with a trivial Go file that triggers zero findings
// and therefore zero refactor tasks.
func (s *emitPromptsWiringState) aMinimalFixtureProducingZeroTasks() error {
	dir, err := os.MkdirTemp("", "cleanc-e2e-zero-tasks-*")
	if err != nil {
		return fmt.Errorf("create zero-task fixture dir: %w", err)
	}
	s.fixtureRoot = dir

	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/zero-tasks\n\ngo 1.21\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithEmitPromptsFile() error {
	s.promptsPath = filepath.Join(s.tmpDir, "prompts.jsonl")
	return s.runCleancEmitPrompts("analyze", s.fixtureRoot,
		"--emit-prompts", s.promptsPath)
}

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithEmitPromptsStdoutAndOutFile() error {
	s.reportPath = filepath.Join(s.tmpDir, "report.md")
	return s.runCleancEmitPrompts("analyze", s.fixtureRoot,
		"--emit-prompts", "-",
		"--out", s.reportPath)
}

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithEmitPromptsAgainstZeroTaskFixture() error {
	s.promptsPath = filepath.Join(s.tmpDir, "prompts.jsonl")
	return s.runCleancEmitPrompts("analyze", s.fixtureRoot,
		"--emit-prompts", s.promptsPath)
}

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithEmitPromptsStdoutNoOut() error {
	// The collision between --emit-prompts - and the default
	// --out (stdout) is detected after flag parsing but before
	// the pipeline starts. Using "." as the repo-path satisfies
	// the positional-argument check.
	return s.runCleancEmitPrompts("analyze", ".",
		"--emit-prompts", "-")
}

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithBareEmitPrompts() error {
	// Pass a valid repo-path so the positional-argument check
	// does not fire before the bare --emit-prompts detection.
	return s.runCleancEmitPrompts("analyze", ".", "--emit-prompts")
}

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithEmitPromptsFileAndOut() error {
	s.promptsPath = filepath.Join(s.tmpDir, "prompts.jsonl")
	s.reportPath = filepath.Join(s.tmpDir, "report.md")
	return s.runCleancEmitPrompts("analyze", s.fixtureRoot,
		"--emit-prompts", s.promptsPath,
		"--out", s.reportPath)
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *emitPromptsWiringState) promptsJSONLExistsWithOneLinePerTask() error {
	data, err := os.ReadFile(s.promptsPath)
	if err != nil {
		return fmt.Errorf("prompts.jsonl not found at %s: %w\nstdout: %s\nstderr: %s",
			s.promptsPath, err, s.stdout, s.stderr)
	}
	lines := nonEmptyLines(data)
	if len(lines) == 0 {
		return fmt.Errorf("prompts.jsonl has zero lines (expected ≥1)\nstdout: %s\nstderr: %s",
			s.stdout, s.stderr)
	}
	s.taskCount = len(lines)
	return nil
}

func (s *emitPromptsWiringState) eachPromptsJSONLLineIsValid() error {
	data, err := os.ReadFile(s.promptsPath)
	if err != nil {
		return fmt.Errorf("cannot read prompts.jsonl: %w", err)
	}
	lines := nonEmptyLines(data)
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			return fmt.Errorf("prompts.jsonl line %d is not valid JSON: %s", i+1, line)
		}
	}
	return nil
}

func (s *emitPromptsWiringState) emitPromptsExitCodeIs(expected int) error {
	if s.exitCode != expected {
		return fmt.Errorf("expected exit code %d, got %d\nstdout: %s\nstderr: %s",
			expected, s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

func (s *emitPromptsWiringState) reportMDExistsWithMarkdownReport() error {
	info, err := os.Stat(s.reportPath)
	if err != nil {
		return fmt.Errorf("report.md not found at %s: %w\nstdout: %s\nstderr: %s",
			s.reportPath, err, s.stdout, s.stderr)
	}
	if info.Size() == 0 {
		return fmt.Errorf("report.md is empty (0 bytes)")
	}
	data, err := os.ReadFile(s.reportPath)
	if err != nil {
		return fmt.Errorf("cannot read report.md: %w", err)
	}
	if !strings.Contains(string(data), "#") {
		return fmt.Errorf("report.md does not contain a markdown heading (#)")
	}
	return nil
}

func (s *emitPromptsWiringState) stdoutContainsJSONLLinesMatchingTaskCount() error {
	lines := nonEmptyLines([]byte(s.stdout))
	if len(lines) == 0 {
		return fmt.Errorf("stdout is empty (expected JSONL lines)\nstderr: %s", s.stderr)
	}
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			return fmt.Errorf("stdout line %d is not valid JSON: %s", i+1, line)
		}
	}
	s.taskCount = len(lines)
	return nil
}

func (s *emitPromptsWiringState) promptsJSONLExistsAndIsZeroBytes() error {
	info, err := os.Stat(s.promptsPath)
	if err != nil {
		return fmt.Errorf("prompts.jsonl not found at %s: %w\nstdout: %s\nstderr: %s",
			s.promptsPath, err, s.stdout, s.stderr)
	}
	if info.Size() != 0 {
		data, _ := os.ReadFile(s.promptsPath)
		return fmt.Errorf("expected zero bytes, got %d bytes:\n%s",
			info.Size(), string(data))
	}
	return nil
}

func (s *emitPromptsWiringState) emitPromptsStderrContains(msg string) error {
	if !strings.Contains(s.stderr, msg) {
		return fmt.Errorf("stderr does not contain %q\nstderr was:\n%s", msg, s.stderr)
	}
	return nil
}

func (s *emitPromptsWiringState) noEmitPromptsPipelineStageStarts() error {
	// Three independent proofs that no pipeline stage ran:
	//
	// 1. The dev banner (first thing runAnalyzePipeline emits)
	//    is absent from stderr.
	if strings.Contains(s.stderr, devpolicy.BannerText) {
		return fmt.Errorf("dev banner found in stderr — pipeline started before flag rejection\nstderr was:\n%s", s.stderr)
	}
	// 2. stdout is empty — if the pipeline had run, the
	//    markdown renderer would have written output.
	if strings.TrimSpace(s.stdout) != "" {
		return fmt.Errorf("stdout is non-empty — pipeline may have started\nstdout was:\n%s", s.stdout)
	}
	// 3. Exit code is 64 (ExitUsage), not 0/1/2/70.
	if s.exitCode != 64 {
		return fmt.Errorf("exit code %d suggests a pipeline stage ran (expected 64/ExitUsage)", s.exitCode)
	}
	return nil
}

func (s *emitPromptsWiringState) markdownDiagnosticsContainsPromptCount() error {
	data, err := os.ReadFile(s.reportPath)
	if err != nil {
		return fmt.Errorf("cannot read report.md at %s: %w", s.reportPath, err)
	}
	report := string(data)

	// The markdown renderer outputs:
	//   - **Refactor prompts emitted:** <N>
	// where N = len(art.Tasks) = JSONL line count.
	expected := fmt.Sprintf("Refactor prompts emitted:** %d", s.taskCount)
	if !strings.Contains(report, expected) {
		// Extract the diagnostics section for diagnostics.
		idx := strings.Index(report, "## Diagnostics")
		excerpt := "(no Diagnostics section found)"
		if idx >= 0 {
			end := idx + 500
			if end > len(report) {
				end = len(report)
			}
			excerpt = report[idx:end]
		}
		return fmt.Errorf("markdown report does not contain %q\ndiagnostics excerpt:\n%s",
			expected, excerpt)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario initializer and test entry point
// ---------------------------------------------------------------------------

func InitializeScenario_p1_structured_prompt_emitter_emit_prompts_flag_wiring(ctx *godog.ScenarioContext) {
	s := newEmitPromptsWiringState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a built cleanc binary for emit-prompts wiring$`, s.aBuiltCleancBinaryForEmitPromptsWiring)
	ctx.Step(`^a fixture that produces refactor tasks$`, s.aFixtureThatProducesRefactorTasks)
	ctx.Step(`^a minimal fixture producing zero tasks$`, s.aMinimalFixtureProducingZeroTasks)

	// When
	ctx.Step(`^cleanc analyze runs with --emit-prompts prompts\.jsonl$`, s.cleancAnalyzeRunsWithEmitPromptsFile)
	ctx.Step(`^cleanc analyze runs with --emit-prompts - and --out report\.md$`, s.cleancAnalyzeRunsWithEmitPromptsStdoutAndOutFile)
	ctx.Step(`^cleanc analyze runs with --emit-prompts prompts\.jsonl against the zero-task fixture$`, s.cleancAnalyzeRunsWithEmitPromptsAgainstZeroTaskFixture)
	ctx.Step(`^cleanc analyze runs with --emit-prompts - and no --out flag$`, s.cleancAnalyzeRunsWithEmitPromptsStdoutNoOut)
	ctx.Step(`^cleanc analyze runs with bare --emit-prompts$`, s.cleancAnalyzeRunsWithBareEmitPrompts)
	ctx.Step(`^cleanc analyze runs with --emit-prompts prompts\.jsonl and --out report\.md$`, s.cleancAnalyzeRunsWithEmitPromptsFileAndOut)

	// Then
	ctx.Step(`^prompts\.jsonl exists with one JSONL line per task$`, s.promptsJSONLExistsWithOneLinePerTask)
	ctx.Step(`^each prompts\.jsonl line is a valid JSON object$`, s.eachPromptsJSONLLineIsValid)
	ctx.Step(`^the emit-prompts exit code is (\d+)$`, s.emitPromptsExitCodeIs)
	ctx.Step(`^report\.md exists with the markdown report$`, s.reportMDExistsWithMarkdownReport)
	ctx.Step(`^stdout contains JSONL lines matching the task count$`, s.stdoutContainsJSONLLinesMatchingTaskCount)
	ctx.Step(`^prompts\.jsonl exists and is zero bytes$`, s.promptsJSONLExistsAndIsZeroBytes)
	ctx.Step(`^emit-prompts stderr contains "([^"]*)"$`, s.emitPromptsStderrContains)
	ctx.Step(`^no emit-prompts pipeline stage starts$`, s.noEmitPromptsPipelineStageStarts)
	ctx.Step(`^the markdown report diagnostics block contains the prompt count$`, s.markdownDiagnosticsContainsPromptCount)
}

func TestE2E_p1_structured_prompt_emitter_emit_prompts_flag_wiring(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_p1_structured_prompt_emitter_emit_prompts_flag_wiring,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"p1_structured_prompt_emitter_emit_prompts_flag_wiring.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
