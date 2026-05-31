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

	promptsPath  string
	reportPath   string
	findingsPath string
	taskCount    int
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
	if s.fixtureRoot != "" && strings.Contains(s.fixtureRoot, "cleanc-e2e-") {
		_ = os.RemoveAll(s.fixtureRoot)
	}
}

// createWideStructFixture generates a temporary Go project
// containing exactly `n` struct types, each with 16 public
// methods. This reliably triggers exactly `n`
// solid.srp.interface_width_high findings (threshold >= 15
// public methods on a class scope), each producing one
// split_class refactor task. All findings are warn-severity
// so exit code is 0 with the default --exit-on=block.
func createWideStructFixture(n int) (string, error) {
	dir, err := os.MkdirTemp("", "cleanc-e2e-wide-*")
	if err != nil {
		return "", fmt.Errorf("create wide-struct fixture dir: %w", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/wide-fixture\n\ngo 1.21\n"), 0644); err != nil {
		return "", err
	}

	var buf strings.Builder
	buf.WriteString("package wide\n\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&buf, "type Wide%d struct{}\n", i)
		for j := 0; j < 16; j++ {
			fmt.Fprintf(&buf, "func (Wide%d) M%02d() {}\n", i, j)
		}
		buf.WriteString("\n")
	}

	if err := os.WriteFile(filepath.Join(dir, "wide.go"),
		[]byte(buf.String()), 0644); err != nil {
		return "", err
	}

	return dir, nil
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

// aFixtureWithNRefactorTasks creates a dynamic Go fixture
// that produces exactly n refactor tasks. Each task is a
// split_class task triggered by a wide struct with 16 public
// methods (exceeding the interface_width >= 15 threshold).
func (s *emitPromptsWiringState) aFixtureWithNRefactorTasks(n int) error {
	dir, err := createWideStructFixture(n)
	if err != nil {
		return fmt.Errorf("create %d-task fixture: %w", n, err)
	}
	s.fixtureRoot = dir
	s.taskCount = n
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

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithEmitPromptsStdoutAndOutAndFindings() error {
	s.reportPath = filepath.Join(s.tmpDir, "report.md")
	s.findingsPath = filepath.Join(s.tmpDir, "findings.json")
	return s.runCleancEmitPrompts("analyze", s.fixtureRoot,
		"--emit-prompts", "-",
		"--out", s.reportPath,
		"--findings", s.findingsPath)
}

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithEmitPromptsAgainstZeroTaskFixture() error {
	s.promptsPath = filepath.Join(s.tmpDir, "prompts.jsonl")
	return s.runCleancEmitPrompts("analyze", s.fixtureRoot,
		"--emit-prompts", s.promptsPath)
}

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithEmitPromptsStdoutNoOut() error {
	return s.runCleancEmitPrompts("analyze", ".",
		"--emit-prompts", "-")
}

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithBareEmitPrompts() error {
	return s.runCleancEmitPrompts("analyze", ".", "--emit-prompts")
}

func (s *emitPromptsWiringState) cleancAnalyzeRunsWithEmitPromptsFileAndOut() error {
	s.promptsPath = filepath.Join(s.tmpDir, "prompts.jsonl")
	s.reportPath = filepath.Join(s.tmpDir, "report.md")
	// Use the relative filename "prompts.jsonl" for --emit-prompts
	// so the markdown diagnostics block renders
	// "Prompts emitted: N to prompts.jsonl" (matching the
	// acceptance scenario's exact text). Setting cmd.Dir to
	// tmpDir ensures the file is created there.
	cmd := exec.Command(s.binaryPath, "analyze", s.fixtureRoot,
		"--emit-prompts", "prompts.jsonl",
		"--out", s.reportPath)
	cmd.Dir = s.tmpDir
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

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *emitPromptsWiringState) promptsJSONLExistsWithExactlyNLines(expected int) error {
	data, err := os.ReadFile(s.promptsPath)
	if err != nil {
		return fmt.Errorf("prompts.jsonl not found at %s: %w\nstdout: %s\nstderr: %s",
			s.promptsPath, err, s.stdout, s.stderr)
	}
	lines := nonEmptyLines(data)
	if len(lines) != expected {
		return fmt.Errorf("expected exactly %d JSONL lines, got %d\nstdout: %s\nstderr: %s",
			expected, len(lines), s.stdout, s.stderr)
	}
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

// stdoutJSONLMatchesFindingsTaskCount independently verifies
// that the number of JSONL lines on stdout equals the number
// of tasks in findings.json. The task count comes from the
// findings artifact (an independent source), NOT from stdout
// itself, so the assertion proves one-line-per-task rather
// than just counting lines.
func (s *emitPromptsWiringState) stdoutJSONLMatchesFindingsTaskCount() error {
	// Read the independent task count from findings.json.
	findingsData, err := os.ReadFile(s.findingsPath)
	if err != nil {
		return fmt.Errorf("findings.json not found at %s: %w\nstderr: %s",
			s.findingsPath, err, s.stderr)
	}
	var artifact struct {
		Tasks []json.RawMessage `json:"Tasks"`
	}
	if err := json.Unmarshal(findingsData, &artifact); err != nil {
		return fmt.Errorf("findings.json unmarshal: %w", err)
	}
	expectedTasks := len(artifact.Tasks)

	// Count and validate JSONL lines on stdout.
	lines := nonEmptyLines([]byte(s.stdout))
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			return fmt.Errorf("stdout line %d is not valid JSON: %s", i+1, line)
		}
	}

	if len(lines) != expectedTasks {
		return fmt.Errorf("stdout has %d JSONL lines but findings.json has %d tasks",
			len(lines), expectedTasks)
	}
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
	if strings.Contains(s.stderr, devpolicy.BannerText) {
		return fmt.Errorf("dev banner found in stderr — pipeline started before flag rejection\nstderr was:\n%s", s.stderr)
	}
	if strings.TrimSpace(s.stdout) != "" {
		return fmt.Errorf("stdout is non-empty — pipeline may have started\nstdout was:\n%s", s.stdout)
	}
	if s.exitCode != 64 {
		return fmt.Errorf("exit code %d suggests a pipeline stage ran (expected 64/ExitUsage)", s.exitCode)
	}
	return nil
}

// diagnosticsBlockContainsPromptsEmitted verifies the
// acceptance scenario's diagnostics-count contract:
//
//	"the diagnostics block contains 'Prompts emitted: 7 to prompts.jsonl'"
//
// The markdown renderer outputs (with the PromptDest field):
//
//	- **Prompts emitted:** 7 to prompts.jsonl
//
// The step asserts the EXACT text from the acceptance scenario
// appears as a substring of the rendered markdown report.
func (s *emitPromptsWiringState) diagnosticsBlockContainsPromptsEmitted(text string) error {
	reportData, err := os.ReadFile(s.reportPath)
	if err != nil {
		return fmt.Errorf("cannot read report.md at %s: %w", s.reportPath, err)
	}
	report := string(reportData)

	// Strip markdown bold markers so we can match plain text
	// from the acceptance scenario against the rendered report.
	plain := strings.ReplaceAll(report, "**", "")

	// Assert the exact spec text is a substring of the report.
	if !strings.Contains(plain, text) {
		idx := strings.Index(report, "## Diagnostics")
		excerpt := "(no Diagnostics section found)"
		if idx >= 0 {
			end := idx + 500
			if end > len(report) {
				end = len(report)
			}
			excerpt = report[idx:end]
		}
		return fmt.Errorf("markdown report does not contain the exact text %q\ndiagnostics excerpt:\n%s",
			text, excerpt)
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
	ctx.Step(`^a fixture with (\d+) refactor tasks$`, s.aFixtureWithNRefactorTasks)
	ctx.Step(`^a minimal fixture producing zero tasks$`, s.aMinimalFixtureProducingZeroTasks)

	// When
	ctx.Step(`^cleanc analyze runs with --emit-prompts prompts\.jsonl$`, s.cleancAnalyzeRunsWithEmitPromptsFile)
	ctx.Step(`^cleanc analyze runs with --emit-prompts - and --out report\.md and --findings findings\.json$`, s.cleancAnalyzeRunsWithEmitPromptsStdoutAndOutAndFindings)
	ctx.Step(`^cleanc analyze runs with --emit-prompts prompts\.jsonl against the zero-task fixture$`, s.cleancAnalyzeRunsWithEmitPromptsAgainstZeroTaskFixture)
	ctx.Step(`^cleanc analyze runs with --emit-prompts - and no --out flag$`, s.cleancAnalyzeRunsWithEmitPromptsStdoutNoOut)
	ctx.Step(`^cleanc analyze runs with bare --emit-prompts$`, s.cleancAnalyzeRunsWithBareEmitPrompts)
	ctx.Step(`^cleanc analyze runs with --emit-prompts prompts\.jsonl and --out report\.md$`, s.cleancAnalyzeRunsWithEmitPromptsFileAndOut)

	// Then
	ctx.Step(`^prompts\.jsonl exists with exactly (\d+) lines$`, s.promptsJSONLExistsWithExactlyNLines)
	ctx.Step(`^prompts\.jsonl has exactly (\d+) lines$`, s.promptsJSONLExistsWithExactlyNLines)
	ctx.Step(`^each prompts\.jsonl line is a valid JSON object$`, s.eachPromptsJSONLLineIsValid)
	ctx.Step(`^the emit-prompts exit code is (\d+)$`, s.emitPromptsExitCodeIs)
	ctx.Step(`^report\.md exists with the markdown report$`, s.reportMDExistsWithMarkdownReport)
	ctx.Step(`^stdout receives the JSONL stream with one line per task verified against findings\.json$`, s.stdoutJSONLMatchesFindingsTaskCount)
	ctx.Step(`^prompts\.jsonl exists and is zero bytes$`, s.promptsJSONLExistsAndIsZeroBytes)
	ctx.Step(`^emit-prompts stderr contains "([^"]*)"$`, s.emitPromptsStderrContains)
	ctx.Step(`^no emit-prompts pipeline stage starts$`, s.noEmitPromptsPipelineStageStarts)
	ctx.Step(`^the diagnostics block contains "([^"]*)"$`, s.diagnosticsBlockContainsPromptsEmitted)
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
