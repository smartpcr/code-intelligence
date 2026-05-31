//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="p1_structured_prompt_emitter_reserved_verbs_and_flags_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// reservedVerbsState holds per-scenario state for the reserved
// verbs and flags e2e scenarios.
type reservedVerbsState struct {
	binaryPath  string
	moduleRoot  string
	fixtureRoot string
	tmpDir      string

	stdout   string
	stderr   string
	exitCode int

	// outPath and findingsPath track artifact paths so the
	// "no --out or --findings file is created" assertion can
	// verify they were never written.
	outPath      string
	findingsPath string
}

func newReservedVerbsState() *reservedVerbsState {
	return &reservedVerbsState{}
}

func (s *reservedVerbsState) cleanup() {
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir)
	}
	if s.fixtureRoot != "" && strings.Contains(s.fixtureRoot, "cleanc-e2e-reserved-") {
		_ = os.RemoveAll(s.fixtureRoot)
	}
}

// runCleancReserved executes the cleanc binary with the given
// arguments and captures stdout, stderr, and exit code.
func (s *reservedVerbsState) runCleancReserved(args ...string) error {
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

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *reservedVerbsState) aBuiltCleancBinaryForReservedVerbs() error {
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

	tmpDir, err := os.MkdirTemp("", "cleanc-e2e-reserved-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	s.tmpDir = tmpDir
	return nil
}

func (s *reservedVerbsState) aFixtureRepoForReservedFlags() error {
	dir, err := os.MkdirTemp("", "cleanc-e2e-reserved-fixture-*")
	if err != nil {
		return fmt.Errorf("create fixture dir: %w", err)
	}
	s.fixtureRoot = dir

	// Minimal Go module so cleanc analyze recognises the directory.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/reserved-fixture\n\ngo 1.21\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		return err
	}

	// Pre-set artifact paths inside tmpDir so we can check they
	// were never created.
	s.outPath = filepath.Join(s.tmpDir, "report.md")
	s.findingsPath = filepath.Join(s.tmpDir, "findings.json")
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *reservedVerbsState) cleancApplyRuns() error {
	return s.runCleancReserved("apply", "00000000-0000-0000-0000-000000000000")
}

func (s *reservedVerbsState) cleancAnalyzeTelemetryOtlpRuns() error {
	return s.runCleancReserved("analyze", s.fixtureRoot,
		"--out", s.outPath,
		"--findings", s.findingsPath,
		"--telemetry-otlp", "http://localhost:4317")
}

func (s *reservedVerbsState) cleancAnalyzeWithChurnRuns() error {
	return s.runCleancReserved("analyze", s.fixtureRoot,
		"--out", s.outPath,
		"--findings", s.findingsPath,
		"--with-churn")
}

func (s *reservedVerbsState) cleancAnalyzeSnippetCapLinesRuns() error {
	return s.runCleancReserved("analyze", s.fixtureRoot,
		"--out", s.outPath,
		"--findings", s.findingsPath,
		"--snippet-cap-lines", "100")
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *reservedVerbsState) theReservedExitCodeIs(expected int) error {
	if s.exitCode != expected {
		return fmt.Errorf("exit code = %d, want %d\nstdout=%s\nstderr=%s",
			s.exitCode, expected, s.stdout, s.stderr)
	}
	return nil
}

func (s *reservedVerbsState) reservedStderrContains(phrase string) error {
	if !strings.Contains(s.stderr, phrase) {
		return fmt.Errorf("stderr does not contain %q\nstderr=%s", phrase, s.stderr)
	}
	return nil
}

func (s *reservedVerbsState) noOutOrFindingsFileIsCreated() error {
	for _, p := range []string{s.outPath, s.findingsPath} {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return fmt.Errorf("artifact %q should not exist but does", p)
		}
	}
	return nil
}

func (s *reservedVerbsState) theReservedExitCodeIs64BeforeAnyPipelineStageStarts() error {
	if s.exitCode != 64 {
		return fmt.Errorf("exit code = %d, want 64\nstdout=%s\nstderr=%s",
			s.exitCode, s.stdout, s.stderr)
	}
	// Pipeline stages emit diagnostics to stderr; a pre-pipeline
	// rejection must NOT contain any walker / parse / recipe /
	// planner output.
	for _, marker := range []string{
		"walker:", "parse:", "recipe:", "planner:",
		"Findings:", "Tasks:", "Report written",
	} {
		if strings.Contains(s.stderr, marker) {
			return fmt.Errorf("stderr contains pipeline marker %q — rejection was not before pipeline\nstderr=%s",
				marker, s.stderr)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Initializer
// ---------------------------------------------------------------------------

func InitializeScenario_p1_structured_prompt_emitter_reserved_verbs_and_flags(ctx *godog.ScenarioContext) {
	s := newReservedVerbsState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a built cleanc binary for reserved verbs$`,
		s.aBuiltCleancBinaryForReservedVerbs)
	ctx.Step(`^a fixture repo for reserved flags$`,
		s.aFixtureRepoForReservedFlags)

	// When
	ctx.Step(`^cleanc apply 00000000-0000-0000-0000-000000000000 runs$`,
		s.cleancApplyRuns)
	ctx.Step(`^cleanc analyze \. --telemetry-otlp http://localhost:4317 runs$`,
		s.cleancAnalyzeTelemetryOtlpRuns)
	ctx.Step(`^cleanc analyze \. --with-churn runs$`,
		s.cleancAnalyzeWithChurnRuns)
	ctx.Step(`^cleanc analyze \. --snippet-cap-lines 100 runs$`,
		s.cleancAnalyzeSnippetCapLinesRuns)

	// Then
	ctx.Step(`^the reserved exit code is (\d+)$`,
		s.theReservedExitCodeIs)
	ctx.Step(`^reserved stderr contains "([^"]*)"$`,
		s.reservedStderrContains)
	ctx.Step(`^no --out or --findings file is created$`,
		s.noOutOrFindingsFileIsCreated)
	ctx.Step(`^the reserved exit code is 64 BEFORE any pipeline stage starts$`,
		s.theReservedExitCodeIs64BeforeAnyPipelineStageStarts)
	ctx.Step(`^the reserved exit code is 64 before any pipeline stage starts$`,
		s.theReservedExitCodeIs64BeforeAnyPipelineStageStarts)
}

func TestE2E_p1_structured_prompt_emitter_reserved_verbs_and_flags(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_p1_structured_prompt_emitter_reserved_verbs_and_flags,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"p1_structured_prompt_emitter_reserved_verbs_and_flags.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero exit from godog suite")
	}
}
