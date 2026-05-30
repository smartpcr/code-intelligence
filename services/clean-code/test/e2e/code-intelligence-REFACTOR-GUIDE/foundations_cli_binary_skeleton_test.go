//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="foundations_cli_binary_skeleton_test.go" company="Microsoft Corp.">
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
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// requireEnv skips the test when the named environment variable is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

// cliSkeletonState holds per-scenario state for CLI binary
// skeleton scenarios.
type cliSkeletonState struct {
	// binaryPath is the absolute path to the pre-built cleanc binary
	// produced by `make build` (bin/cleanc relative to module root).
	binaryPath string
	// moduleRoot is the root of the services/clean-code module.
	moduleRoot string
	// tmpDir holds temp artifacts (e.g. --out targets) to clean up.
	tmpDir string

	// Command execution results.
	stdout   string
	stderr   string
	exitCode int

	// outFilePath is the --out target used in the unknown-sub-command
	// scenario to verify no output is emitted.
	outFilePath string

	// builtBinaryPath is the path checked in the makefile discovery
	// scenario after running `make build`.
	builtBinaryPath string
}

func newCliSkeletonState() *cliSkeletonState {
	return &cliSkeletonState{}
}

// resolveModuleRoot walks up from this test file to find the
// services/clean-code directory (the Go module root).
func resolveModuleRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile is .../services/clean-code/test/e2e/code-intelligence-REFACTOR-GUIDE/...
	dir := filepath.Dir(thisFile)
	// Walk up: code-intelligence-REFACTOR-GUIDE -> e2e -> test -> clean-code
	return filepath.Join(dir, "..", "..", "..")
}

// binaryName returns "cleanc" on Unix, "cleanc.exe" on Windows.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "cleanc.exe"
	}
	return "cleanc"
}

// --- Given steps ---

// aBuiltCleancBinary locates the pre-built bin/cleanc artifact
// produced by the pre-test bootstrap (`make build`). This ensures the
// E2E scenarios exercise the SAME binary that the Makefile produces,
// not a separately-compiled temp copy.
func (s *cliSkeletonState) aBuiltCleancBinary() error {
	s.moduleRoot = resolveModuleRoot()
	s.binaryPath = filepath.Join(s.moduleRoot, "bin", binaryName())

	if _, err := os.Stat(s.binaryPath); err != nil {
		// Fallback: run `make build` if the binary is missing (e.g.
		// developer running tests without the bootstrap step).
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

	// Create a temp dir for --out assertions and other scratch files.
	tmpDir, err := os.MkdirTemp("", "cleanc-e2e-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	s.tmpDir = tmpDir
	return nil
}

// aCleanCheckout removes the bin/cleanc artifact so the makefile
// discovery scenario can verify `make build` creates it from scratch.
func (s *cliSkeletonState) aCleanCheckout() error {
	s.moduleRoot = resolveModuleRoot()
	s.builtBinaryPath = filepath.Join(s.moduleRoot, "bin", binaryName())
	// Remove prior build artifact to simulate a clean checkout.
	_ = os.Remove(s.builtBinaryPath)
	return nil
}

// --- When steps ---

func (s *cliSkeletonState) theUserRunsCleancVersion() error {
	return s.runCleanc("version")
}

func (s *cliSkeletonState) theUserRunsCleancFrobnicateWithOutPointingToATempFile() error {
	s.outFilePath = filepath.Join(s.tmpDir, "should-not-exist.out")
	return s.runCleanc("frobnicate", "--out", s.outFilePath)
}

func (s *cliSkeletonState) theUserRunsCleancAnalyzeWithNoPathArgument() error {
	return s.runCleanc("analyze")
}

func (s *cliSkeletonState) theDeveloperRunsMakeBuild() error {
	// `make -C services/clean-code build` from repo root ≡
	// `make build` from module root.
	cmd := exec.Command("make", "build")
	cmd.Dir = s.moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("make build failed: %w\noutput: %s", err, string(out))
	}
	return nil
}

// runCleanc executes the pre-built cleanc binary with the given
// arguments and captures stdout, stderr, and exit code.
func (s *cliSkeletonState) runCleanc(args ...string) error {
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

// --- Then steps ---

func (s *cliSkeletonState) stdoutIncludes(substring string) error {
	if !strings.Contains(s.stdout, substring) {
		return fmt.Errorf("stdout does not contain %q\nstdout was:\n%s", substring, s.stdout)
	}
	return nil
}

func (s *cliSkeletonState) stderrIncludes(substring string) error {
	if !strings.Contains(s.stderr, substring) {
		return fmt.Errorf("stderr does not contain %q\nstderr was:\n%s", substring, s.stderr)
	}
	return nil
}

func (s *cliSkeletonState) theExitCodeIs(expected int) error {
	if s.exitCode != expected {
		return fmt.Errorf("expected exit code %d, got %d\nstdout: %s\nstderr: %s",
			expected, s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

func (s *cliSkeletonState) stdoutIsEmpty() error {
	if strings.TrimSpace(s.stdout) != "" {
		return fmt.Errorf("expected empty stdout, got:\n%s", s.stdout)
	}
	return nil
}

func (s *cliSkeletonState) noOutputIsEmittedToTheOutPath() error {
	if s.outFilePath == "" {
		return fmt.Errorf("outFilePath not set; the --out When step must run first")
	}
	_, err := os.Stat(s.outFilePath)
	if err == nil {
		content, _ := os.ReadFile(s.outFilePath)
		return fmt.Errorf("--out file %q should not exist, but it does (size=%d, content=%q)",
			s.outFilePath, len(content), string(content))
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("unexpected error checking --out file: %w", err)
	}
	return nil
}

func (s *cliSkeletonState) stderrPrintsTheAnalyzeUsageBlock() error {
	if !strings.Contains(s.stderr, "usage: cleanc analyze") {
		return fmt.Errorf("stderr does not contain analyze usage block\nstderr was:\n%s", s.stderr)
	}
	return nil
}

func (s *cliSkeletonState) servicesCleanCodeBinCleancExistsAndIsExecutable() error {
	info, err := os.Stat(s.builtBinaryPath)
	if err != nil {
		return fmt.Errorf("binary not found at %s: %w", s.builtBinaryPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("expected a file, got a directory at %s", s.builtBinaryPath)
	}
	if runtime.GOOS != "windows" {
		if info.Mode()&0111 == 0 {
			return fmt.Errorf("binary at %s is not executable (mode: %s)", s.builtBinaryPath, info.Mode())
		}
	}
	return nil
}

// cleanup removes any temp artifacts created during the scenario.
func (s *cliSkeletonState) cleanup() {
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir)
	}
}

func InitializeScenario_foundations_cli_binary_skeleton(ctx *godog.ScenarioContext) {
	s := newCliSkeletonState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a built cleanc binary$`, s.aBuiltCleancBinary)
	ctx.Step(`^a clean checkout$`, s.aCleanCheckout)

	// When
	ctx.Step(`^the user runs cleanc version$`, s.theUserRunsCleancVersion)
	ctx.Step(`^the user runs cleanc frobnicate with --out pointing to a temp file$`, s.theUserRunsCleancFrobnicateWithOutPointingToATempFile)
	ctx.Step(`^the user runs cleanc analyze with no path argument$`, s.theUserRunsCleancAnalyzeWithNoPathArgument)
	ctx.Step(`^the developer runs make -C services/clean-code build$`, s.theDeveloperRunsMakeBuild)

	// Then
	ctx.Step(`^stdout includes "([^"]*)"$`, s.stdoutIncludes)
	ctx.Step(`^stderr includes "([^"]*)"$`, s.stderrIncludes)
	ctx.Step(`^the exit code is (\d+)$`, s.theExitCodeIs)
	ctx.Step(`^stdout is empty$`, s.stdoutIsEmpty)
	ctx.Step(`^no output is emitted to the --out path$`, s.noOutputIsEmittedToTheOutPath)
	ctx.Step(`^stderr prints the analyze usage block$`, s.stderrPrintsTheAnalyzeUsageBlock)
	ctx.Step(`^services/clean-code/bin/cleanc exists and is executable$`, s.servicesCleanCodeBinCleancExistsAndIsExecutable)
}

func TestE2E_foundations_cli_binary_skeleton(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_foundations_cli_binary_skeleton,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"foundations_cli_binary_skeleton.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}