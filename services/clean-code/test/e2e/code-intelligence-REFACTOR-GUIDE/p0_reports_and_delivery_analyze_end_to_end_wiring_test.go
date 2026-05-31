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
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
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

// resolveBinaryPath returns the path to the cleanc binary. The
// Makefile does NOT append `.exe` on Windows (`go build -o
// bin/cleanc`), so on Windows we check for the extension-less
// name and copy/rename it with `.exe` if needed for exec.Command
// compatibility.
func resolveBinaryPath(moduleRoot string) string {
	base := filepath.Join(moduleRoot, "bin", "cleanc")
	if runtime.GOOS == "windows" {
		withExt := base + ".exe"
		// Prefer the .exe variant if it already exists.
		if _, err := os.Stat(withExt); err == nil {
			return withExt
		}
		// The Makefile creates the binary without .exe.
		// If the extension-less binary exists, copy it so
		// exec.Command can resolve it on Windows.
		if _, err := os.Stat(base); err == nil {
			data, readErr := os.ReadFile(base)
			if readErr == nil {
				if writeErr := os.WriteFile(withExt, data, 0755); writeErr == nil {
					return withExt
				}
			}
		}
		return withExt // fallback — will be built below with .exe
	}
	return base
}

// buildCleancBinary compiles the cleanc binary directly via
// `go build` instead of `make build`. This avoids timeout
// issues from `make build` which compiles ALL binaries in the
// module including unrelated services. On Windows the output
// path MUST end in `.exe` for exec.Command to resolve it.
func buildCleancBinary(moduleRoot, outputPath string) error {
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	cmd := exec.Command("go", "build", "-o", outputPath, "./cmd/cleanc")
	cmd.Dir = moduleRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go build ./cmd/cleanc failed: %w\n%s", err, string(out))
	}
	return nil
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
// pack maps them to block-severity findings. While the fixture uses
// two source files (cycle detection inherently requires ≥2 nodes),
// the acceptance scenario's contract is "one block-severity finding"
// — the decoupling.cycle_member_present rule fires once per scope
// participating in a cycle.
func (s *analyzeWiringState) createCycleFixture() error {
	dir, err := os.MkdirTemp("", "cleanc-e2e-cycle-*")
	if err != nil {
		return fmt.Errorf("create cycle fixture dir: %w", err)
	}
	s.fixtureRoot = dir

	// go.mod so the module path is known for import resolution.
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

	// Package b imports package a — completing the cycle.
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

// findingsArtifact is a minimal projection of
// report.RunArtifact used to decode findings.json without
// importing the full report package (which would couple this
// e2e test to internal renderer changes).
type findingsArtifact struct {
	Findings []findingEntry `json:"Findings"`
}

type findingEntry struct {
	Severity steward.Severity `json:"Severity"`
	RuleID   string           `json:"RuleID"`
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *analyzeWiringState) aBuiltCleancBinaryForAnalyzeWiring() error {
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

	tmpDir, err := os.MkdirTemp("", "cleanc-e2e-wiring-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	s.tmpDir = tmpDir
	return nil
}

func (s *analyzeWiringState) aFixtureRepoWithOneBlockSeverityFinding() error {
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

func (s *analyzeWiringState) findingsJSONContainsExactlyNBlockSeverityFindings(expected int) error {
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
	var art findingsArtifact
	if err := json.Unmarshal(data, &art); err != nil {
		return fmt.Errorf("findings.json unmarshal failed: %w", err)
	}
	blockCount := 0
	for _, f := range art.Findings {
		if f.Severity == steward.SeverityBlock {
			blockCount++
		}
	}
	if blockCount != expected {
		return fmt.Errorf("expected exactly %d block-severity finding(s), got %d; total findings: %d\nfindings: %+v",
			expected, blockCount, len(art.Findings), art.Findings)
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

func (s *analyzeWiringState) noPipelineStageStartedBeforeTheRejection() error {
	// The dev banner is the FIRST action in runAnalyzePipeline().
	// If it is absent from stderr, no pipeline stage executed.
	if strings.Contains(s.stderr, devpolicy.BannerText) {
		return fmt.Errorf("dev banner found in stderr — pipeline started before flag rejection\nstderr was:\n%s", s.stderr)
	}
	return nil
}

func (s *analyzeWiringState) analyzeStderrBeginsWithTheExactC10BannerString() error {
	// Assert raw stderr (no trimming) starts with the C10
	// banner verbatim. The banner is the FIRST thing the dev
	// build writes to stderr before any other output.
	if !strings.HasPrefix(s.stderr, devpolicy.BannerText) {
		prefix := s.stderr
		if len(prefix) > 200 {
			prefix = prefix[:200]
		}
		return fmt.Errorf("stderr does not begin with the exact C10 banner\nexpected prefix: %q\nactual prefix:   %q",
			devpolicy.BannerText, prefix)
	}
	return nil
}

func (s *analyzeWiringState) markdownIsWrittenToStdout() error {
	if strings.TrimSpace(s.stdout) == "" {
		return fmt.Errorf("expected markdown on stdout, but stdout is empty\nstderr: %s", s.stderr)
	}
	// Markdown report always contains at least one heading.
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
	ctx.Step(`^a fixture repo with one block-severity finding$`, s.aFixtureRepoWithOneBlockSeverityFinding)
	ctx.Step(`^a minimal fixture repo$`, s.aMinimalFixtureRepo)

	// When
	ctx.Step(`^cleanc analyze runs with --out report\.md --findings findings\.json --exit-on block$`, s.cleancAnalyzeRunsWithOutAndFindingsAndExitOnBlock)
	ctx.Step(`^cleanc analyze runs against a non-existent root path$`, s.cleancAnalyzeRunsAgainstANonExistentRootPath)
	ctx.Step(`^cleanc analyze runs with --exit-on critical$`, s.cleancAnalyzeRunsWithExitOnCritical)
	ctx.Step(`^cleanc analyze runs against the fixture repo$`, s.cleancAnalyzeRunsAgainstTheFixtureRepo)
	ctx.Step(`^cleanc analyze runs against the fixture repo without --out$`, s.cleancAnalyzeRunsAgainstTheFixtureRepoWithoutOut)

	// Then
	ctx.Step(`^report\.md is written and is non-empty$`, s.reportMDIsWrittenAndIsNonEmpty)
	ctx.Step(`^findings\.json is written and contains exactly (\d+) block-severity findings$`, s.findingsJSONContainsExactlyNBlockSeverityFindings)
	ctx.Step(`^the analyze exit code is (\d+)$`, s.theAnalyzeExitCodeIs)
	ctx.Step(`^analyze stderr contains "([^"]*)"$`, s.analyzeStderrContains)
	ctx.Step(`^no pipeline stage started before the rejection$`, s.noPipelineStageStartedBeforeTheRejection)
	ctx.Step(`^analyze stderr begins with the exact C10 banner string$`, s.analyzeStderrBeginsWithTheExactC10BannerString)
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
