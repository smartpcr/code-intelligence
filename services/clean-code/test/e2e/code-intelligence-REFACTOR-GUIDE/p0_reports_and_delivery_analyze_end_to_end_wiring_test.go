//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="p0_reports_and_delivery_analyze_end_to_end_wiring_test.go" company="Microsoft Corp.">
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
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
)

// analyzeWiringState holds per-scenario state for the
// analyze end-to-end wiring scenarios.
type analyzeWiringState struct {
	binaryPath  string
	moduleRoot  string
	tmpDir      string
	fixtureRoot string

	stdout   string
	stderr   string
	exitCode int

	reportPath   string
	findingsPath string
}

func newAnalyzeWiringState() *analyzeWiringState {
	return &analyzeWiringState{}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// resolveModuleRootForWiring returns the services/clean-code module root.
func resolveModuleRootForWiring() string {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("could not locate go.mod above test file")
		}
		dir = parent
	}
}

func binaryNameWiring() string {
	if runtime.GOOS == "windows" {
		return "cleanc.exe"
	}
	return "cleanc"
}

func (s *analyzeWiringState) runCleancArgs(args ...string) error {
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

func (s *analyzeWiringState) cleanup() {
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir)
	}
}

// createCycleFixture builds a temporary directory containing two Go
// packages that import each other, forming a dependency cycle. The
// cycle_member recipe detects such cycles and the decoupling rule
// pack maps them to a block-severity finding.
func (s *analyzeWiringState) createCycleFixture() error {
	dir, err := os.MkdirTemp("", "cleanc-e2e-cycle-*")
	if err != nil {
		return fmt.Errorf("create cycle fixture dir: %w", err)
	}
	s.fixtureRoot = dir

	// go.mod so the module path is known.
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/cycle-fixture\n\ngo 1.21\n"), 0644); err != nil {
		return err
	}

	// Package a imports package b.
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "a", "a.go"),
		[]byte("package a\n\nimport _ \"example.com/cycle-fixture/b\"\n\nfunc A() {}\n"), 0644); err != nil {
		return err
	}

	// Package b imports package a.
	if err := os.MkdirAll(filepath.Join(dir, "b"), 0755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "b", "b.go"),
		[]byte("package b\n\nimport _ \"example.com/cycle-fixture/a\"\n\nfunc B() {}\n"), 0644); err != nil {
		return err
	}

	return nil
}

// createMinimalFixture builds a temporary directory containing a
// single, trivial Go file. No cycle exists so the only findings
// (if any) are warn-severity SOLID violations.
func (s *analyzeWiringState) createMinimalFixture() error {
	dir, err := os.MkdirTemp("", "cleanc-e2e-minimal-*")
	if err != nil {
		return fmt.Errorf("create minimal fixture dir: %w", err)
	}
	s.fixtureRoot = dir

	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/minimal-fixture\n\ngo 1.21\n"), 0644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0644); err != nil {
		return err
	}

	return nil
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *analyzeWiringState) aBuiltCleancBinaryForAnalyzeWiring() error {
	s.moduleRoot = resolveModuleRootForWiring()
	s.binaryPath = filepath.Join(s.moduleRoot, "bin", binaryNameWiring())

	if _, err := os.Stat(s.binaryPath); err != nil {
		cmd := exec.Command("make", "build")
		cmd.Dir = s.moduleRoot
		out, buildErr := cmd.CombinedOutput()
		if buildErr != nil {
			return fmt.Errorf("bin/cleanc not found and make build failed: %w\noutput: %s", buildErr, string(out))
		}
		if _, err2 := os.Stat(s.binaryPath); err2 != nil {
			return fmt.Errorf("make build succeeded but bin/cleanc still not found at %s: %w", s.binaryPath, err2)
		}
	}

	tmpDir, err := os.MkdirTemp("", "cleanc-e2e-wiring-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	s.tmpDir = tmpDir
	return nil
}

func (s *analyzeWiringState) aFixtureRepoWithOneGoFileThatTriggersABlockSeverityFinding() error {
	return s.createCycleFixture()
}

func (s *analyzeWiringState) aMinimalFixtureRepo() error {
	return s.createMinimalFixture()
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *analyzeWiringState) cleancAnalyzeRunsWithOutAndFindingsAndExitOnBlock() error {
	s.reportPath = filepath.Join(s.tmpDir, "report.md")
	s.findingsPath = filepath.Join(s.tmpDir, "findings.json")
	return s.runCleancArgs("analyze", s.fixtureRoot,
		"--out", s.reportPath,
		"--findings", s.findingsPath,
		"--exit-on", "block")
}

func (s *analyzeWiringState) cleancAnalyzeRunsAgainstANonExistentRootPath() error {
	return s.runCleancArgs("analyze", "/no/such/path/does/not/exist")
}

func (s *analyzeWiringState) cleancAnalyzeRunsWithExitOnCritical() error {
	return s.runCleancArgs("analyze", ".", "--exit-on", "critical")
}

func (s *analyzeWiringState) cleancAnalyzeRunsAgainstTheFixtureRepo() error {
	return s.runCleancArgs("analyze", s.fixtureRoot)
}

func (s *analyzeWiringState) cleancAnalyzeRunsAgainstTheFixtureRepoWithoutOut() error {
	return s.runCleancArgs("analyze", s.fixtureRoot)
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *analyzeWiringState) reportMDIsWrittenAndIsNonEmpty() error {
	info, err := os.Stat(s.reportPath)
	if err != nil {
		return fmt.Errorf("report.md not found at %s: %w\nstdout: %s\nstderr: %s",
			s.reportPath, err, s.stdout, s.stderr)
	}
	if info.Size() == 0 {
		return fmt.Errorf("report.md is empty (0 bytes)")
	}
	return nil
}

func (s *analyzeWiringState) findingsJSONIsWrittenAndIsValidJSON() error {
	data, err := os.ReadFile(s.findingsPath)
	if err != nil {
		return fmt.Errorf("findings.json not found at %s: %w\nstdout: %s\nstderr: %s",
			s.findingsPath, err, s.stdout, s.stderr)
	}
	if len(data) == 0 {
		return fmt.Errorf("findings.json is empty (0 bytes)")
	}
	if !json.Valid(data) {
		return fmt.Errorf("findings.json is not valid JSON:\n%s", string(data[:min(len(data), 500)]))
	}
	return nil
}

func (s *analyzeWiringState) theAnalyzeExitCodeIs(expected int) error {
	if s.exitCode != expected {
		return fmt.Errorf("expected exit code %d, got %d\nstdout: %s\nstderr: %s",
			expected, s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

func (s *analyzeWiringState) analyzeStderrContains(substring string) error {
	if !strings.Contains(s.stderr, substring) {
		return fmt.Errorf("stderr does not contain %q\nstderr was:\n%s", substring, s.stderr)
	}
	return nil
}

func (s *analyzeWiringState) noPipelineStageRunsBeforeTheExit() error {
	// When --exit-on carries an invalid value the dispatcher
	// rejects before any pipeline stage starts. The canonical
	// stderr message is the ExitOnUsageMessage constant:
	// "--exit-on must be one of info, warn, block".
	if !strings.Contains(s.stderr, "--exit-on must be one of") {
		return fmt.Errorf("stderr does not contain --exit-on validation message; pipeline may have started\nstderr was:\n%s", s.stderr)
	}
	// Confirm the dev-banner is NOT present (pipeline never started).
	if strings.Contains(s.stderr, devpolicy.BannerText) {
		return fmt.Errorf("stderr contains the dev banner, implying the pipeline started before --exit-on rejection\nstderr was:\n%s", s.stderr)
	}
	return nil
}

func (s *analyzeWiringState) analyzeStderrBeginsWithTheC10BannerString() error {
	trimmed := strings.TrimLeft(s.stderr, "\r\n")
	if !strings.HasPrefix(trimmed, devpolicy.BannerText) {
		return fmt.Errorf("stderr does not begin with the C10 banner\nexpected prefix: %q\nstderr was:\n%s",
			devpolicy.BannerText, s.stderr)
	}
	return nil
}

func (s *analyzeWiringState) markdownIsWrittenToStdout() error {
	if strings.TrimSpace(s.stdout) == "" {
		return fmt.Errorf("expected markdown on stdout, but stdout is empty\nstderr: %s", s.stderr)
	}
	// Minimal sanity check: markdown should contain a heading.
	if !strings.Contains(s.stdout, "#") {
		return fmt.Errorf("stdout does not contain a markdown heading (#)\nstdout was:\n%s", s.stdout[:min(len(s.stdout), 500)])
	}
	return nil
}

func (s *analyzeWiringState) theAnalyzeExitCodeIsZeroOrOne() error {
	if s.exitCode != 0 && s.exitCode != 1 {
		return fmt.Errorf("expected exit code 0 or 1, got %d\nstdout: %s\nstderr: %s",
			s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario initializer and test entry point
// ---------------------------------------------------------------------------

func InitializeScenario_p0_reports_and_delivery_analyze_end_to_end_wiring(ctx *godog.ScenarioContext) {
	s := newAnalyzeWiringState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a built cleanc binary for analyze wiring$`, s.aBuiltCleancBinaryForAnalyzeWiring)
	ctx.Step(`^a fixture repo with one Go file that triggers a block-severity finding$`, s.aFixtureRepoWithOneGoFileThatTriggersABlockSeverityFinding)
	ctx.Step(`^a minimal fixture repo$`, s.aMinimalFixtureRepo)

	// When
	ctx.Step(`^cleanc analyze runs with --out report\.md --findings findings\.json --exit-on block$`, s.cleancAnalyzeRunsWithOutAndFindingsAndExitOnBlock)
	ctx.Step(`^cleanc analyze runs against a non-existent root path$`, s.cleancAnalyzeRunsAgainstANonExistentRootPath)
	ctx.Step(`^cleanc analyze runs with --exit-on critical$`, s.cleancAnalyzeRunsWithExitOnCritical)
	ctx.Step(`^cleanc analyze runs against the fixture repo$`, s.cleancAnalyzeRunsAgainstTheFixtureRepo)
	ctx.Step(`^cleanc analyze runs against the fixture repo without --out$`, s.cleancAnalyzeRunsAgainstTheFixtureRepoWithoutOut)

	// Then
	ctx.Step(`^report\.md is written and is non-empty$`, s.reportMDIsWrittenAndIsNonEmpty)
	ctx.Step(`^findings\.json is written and is valid JSON$`, s.findingsJSONIsWrittenAndIsValidJSON)
	ctx.Step(`^the analyze exit code is (\d+)$`, s.theAnalyzeExitCodeIs)
	ctx.Step(`^analyze stderr contains "([^"]*)"$`, s.analyzeStderrContains)
	ctx.Step(`^no pipeline stage runs before the exit$`, s.noPipelineStageRunsBeforeTheExit)
	ctx.Step(`^analyze stderr begins with the C10 banner string$`, s.analyzeStderrBeginsWithTheC10BannerString)
	ctx.Step(`^markdown is written to stdout$`, s.markdownIsWrittenToStdout)
	ctx.Step(`^the analyze exit code is 0 or 1$`, s.theAnalyzeExitCodeIsZeroOrOne)
}

func TestE2E_p0_reports_and_delivery_analyze_end_to_end_wiring(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_p0_reports_and_delivery_analyze_end_to_end_wiring,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"p0_reports_and_delivery_analyze_end_to_end_wiring.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
