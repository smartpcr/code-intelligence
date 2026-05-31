//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="hardening_and_release_build_tag_matrix_test.go" company="Microsoft Corp.">
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

// buildTagMatrixState holds per-scenario state for the
// build-tag-matrix acceptance scenarios.
type buildTagMatrixState struct {
	moduleRoot string
	stdout     string
	stderr     string
	exitCode   int
}

func newBuildTagMatrixState() *buildTagMatrixState {
	return &buildTagMatrixState{}
}

// --- helpers ---

// resolveModuleRootBTM returns the services/clean-code module root
// by walking up from the current test file location.
func resolveModuleRootBTM() string {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	// thisFile: .../services/clean-code/test/e2e/code-intelligence-REFACTOR-GUIDE/...
	// Walk up: code-intelligence-REFACTOR-GUIDE -> e2e -> test -> clean-code
	return filepath.Join(dir, "..", "..", "..")
}

// prodBinaryName returns "cleanc-prod" on Unix, "cleanc-prod.exe" on Windows.
func prodBinaryName() string {
	if runtime.GOOS == "windows" {
		return "cleanc-prod.exe"
	}
	return "cleanc-prod"
}

// runCommandInDir executes a command in the given directory and captures
// stdout, stderr, and exit code into the state.
func (s *buildTagMatrixState) runCommandInDir(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
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

// --- Given steps ---

func (s *buildTagMatrixState) theSourceTree() error {
	s.moduleRoot = resolveModuleRootBTM()
	if _, err := os.Stat(filepath.Join(s.moduleRoot, "go.mod")); err != nil {
		return fmt.Errorf("go.mod not found at module root %s: %w", s.moduleRoot, err)
	}
	return nil
}

func (s *buildTagMatrixState) theServicesCleanCodeSourceTree() error {
	return s.theSourceTree()
}

// --- When steps ---

func (s *buildTagMatrixState) ciRunsMakeBuildProd() error {
	// On Windows where make may not be available, fall back to go build directly.
	if runtime.GOOS == "windows" {
		binDir := filepath.Join(s.moduleRoot, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			return fmt.Errorf("failed to create bin dir: %w", err)
		}
		outPath := filepath.Join(binDir, prodBinaryName())
		return s.runCommandInDir(s.moduleRoot, "go", "build", "-tags", "prod", "-o", outPath, "./cmd/cleanc")
	}
	return s.runCommandInDir(s.moduleRoot, "make", "build-prod")
}

func (s *buildTagMatrixState) ciRunsGoTestProdDevBypass() error {
	return s.runCommandInDir(s.moduleRoot, "go", "test", "-tags", "prod", "-v", "-count=1", "-run", "TestProdBuildExcludesDevBypass", "./internal/cli/devpolicy/...")
}

func (s *buildTagMatrixState) ciRunsMakeTestProd() error {
	// On Windows where make may not be available, fall back to the
	// equivalent go test invocation that the Makefile target wraps.
	if runtime.GOOS == "windows" {
		return s.runCommandInDir(s.moduleRoot, "go", "test", "-tags", "prod", "-count=1",
			"-run", "TestProdBuildExcludesDevBypass|TestLoadUnsignedBundle_Prod|TestBuildTagIsProd|TestFlagsDefaultDevMode|TestProdNewLoader",
			"./cmd/cleanc/...",
			"./internal/cli/devpolicy/...",
			"./internal/cli/flags/...",
		)
	}
	return s.runCommandInDir(s.moduleRoot, "make", "test-prod")
}

// --- Then steps ---

func (s *buildTagMatrixState) itExits0() error {
	if s.exitCode != 0 {
		return fmt.Errorf("expected exit code 0, got %d\nstdout:\n%s\nstderr:\n%s",
			s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

func (s *buildTagMatrixState) binCleancProdExists() error {
	path := filepath.Join(s.moduleRoot, "bin", prodBinaryName())
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("bin/cleanc-prod not found at %s: %w", path, err)
	}
	if info.IsDir() {
		return fmt.Errorf("expected a file at %s, got a directory", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("bin/cleanc-prod at %s is empty (0 bytes)", path)
	}
	return nil
}

func (s *buildTagMatrixState) theExitCodeIsBTM(expected int) error {
	if s.exitCode != expected {
		return fmt.Errorf("expected exit code %d, got %d\nstdout:\n%s\nstderr:\n%s",
			expected, s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

func (s *buildTagMatrixState) theTestOutputContains(substring string) error {
	combined := s.stdout + s.stderr
	if !strings.Contains(combined, substring) {
		return fmt.Errorf("test output does not contain %q\nstdout:\n%s\nstderr:\n%s",
			substring, s.stdout, s.stderr)
	}
	return nil
}

// --- Scenario initializer ---

func InitializeScenario_hardening_and_release_build_tag_matrix(ctx *godog.ScenarioContext) {
	s := newBuildTagMatrixState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		return ctx2, nil
	})

	// Given
	ctx.Step(`^the source tree$`, s.theSourceTree)
	ctx.Step(`^the services/clean-code source tree$`, s.theServicesCleanCodeSourceTree)

	// When
	ctx.Step(`^CI runs make build-prod$`, s.ciRunsMakeBuildProd)
	ctx.Step(`^CI runs go test -tags prod -run TestProdBuildExcludesDevBypass \./internal/cli/devpolicy/\.\.\.$`, s.ciRunsGoTestProdDevBypass)
	ctx.Step(`^CI runs make test-prod$`, s.ciRunsMakeTestProd)

	// Then
	ctx.Step(`^it exits 0$`, s.itExits0)
	ctx.Step(`^bin/cleanc-prod exists$`, s.binCleancProdExists)
	ctx.Step(`^the exit code is (\d+)$`, s.theExitCodeIsBTM)
	ctx.Step(`^the test output contains "([^"]*)"$`, s.theTestOutputContains)
}

func TestE2E_hardening_and_release_build_tag_matrix(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_hardening_and_release_build_tag_matrix,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"hardening_and_release_build_tag_matrix.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
