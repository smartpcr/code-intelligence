//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="hardening_and_release_custom_lint_rules_test.go" company="Microsoft Corp.">
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

// customLintRulesState holds per-scenario state for the
// custom-lint-rules acceptance scenarios.
type customLintRulesState struct {
	moduleRoot   string
	fixtureFiles []string // paths to temp fixture files for cleanup
	fixtureName  string   // base name of the current fixture (for assertions)
	stdout       string
	stderr       string
	exitCode     int
}

func newCustomLintRulesState() *customLintRulesState {
	return &customLintRulesState{}
}

// --- helpers ---

// resolveModuleRootCLR returns the services/clean-code module root
// by walking up from the current test file location.
func resolveModuleRootCLR() string {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	// thisFile: .../services/clean-code/test/e2e/code-intelligence-REFACTOR-GUIDE/...
	return filepath.Join(dir, "..", "..", "..")
}

// vettoolBinaryName returns the platform-appropriate binary name.
func vettoolBinaryName() string {
	if runtime.GOOS == "windows" {
		return "buildtaglint.exe"
	}
	return "buildtaglint"
}

// runVetToolInDir builds the buildtaglint vettool and runs
// `go vet -vettool=<binary>` over the CLI source trees.
func (s *customLintRulesState) runVetToolInDir() error {
	binDir := filepath.Join(s.moduleRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return fmt.Errorf("failed to create bin dir: %w", err)
	}

	vettoolPath := filepath.Join(binDir, vettoolBinaryName())

	// Step 1: build the vettool.
	buildCmd := exec.Command("go", "build", "-o", vettoolPath, "./tools/buildtaglint")
	buildCmd.Dir = s.moduleRoot
	if out, err := buildCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("failed to build buildtaglint: %s\n%s", err, string(out))
	}

	// Step 2: run go vet with the vettool.
	vetCmd := exec.Command("go", "vet",
		fmt.Sprintf("-vettool=%s", vettoolPath),
		"./cmd/cleanc/...", "./internal/cli/...")
	vetCmd.Dir = s.moduleRoot

	var stdoutBuf, stderrBuf bytes.Buffer
	vetCmd.Stdout = &stdoutBuf
	vetCmd.Stderr = &stderrBuf

	err := vetCmd.Run()
	s.stdout = stdoutBuf.String()
	s.stderr = stderrBuf.String()

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			s.exitCode = exitErr.ExitCode()
		} else {
			return fmt.Errorf("go vet execution failed: %w", err)
		}
	} else {
		s.exitCode = 0
	}
	return nil
}

// cleanup removes any fixture files created during the scenario.
func (s *customLintRulesState) cleanup() {
	for _, f := range s.fixtureFiles {
		os.Remove(f)
	}
}

// --- Given steps ---

func (s *customLintRulesState) aTestFixtureFileImportingDatabaseSQL() error {
	s.moduleRoot = resolveModuleRootCLR()
	if _, err := os.Stat(filepath.Join(s.moduleRoot, "go.mod")); err != nil {
		return fmt.Errorf("go.mod not found at module root %s: %w", s.moduleRoot, err)
	}

	s.fixtureName = "e2e_lint_fixture_sql.go"
	fixturePath := filepath.Join(s.moduleRoot, "internal", "cli", s.fixtureName)
	content := "package cli\n\nimport _ \"database/sql\"\n"

	if err := os.WriteFile(fixturePath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write fixture %s: %w", fixturePath, err)
	}
	s.fixtureFiles = append(s.fixtureFiles, fixturePath)
	return nil
}

func (s *customLintRulesState) aTestFixtureFileConstructingUnsignedPolicyVersion() error {
	s.moduleRoot = resolveModuleRootCLR()
	if _, err := os.Stat(filepath.Join(s.moduleRoot, "go.mod")); err != nil {
		return fmt.Errorf("go.mod not found at module root %s: %w", s.moduleRoot, err)
	}

	s.fixtureName = "e2e_lint_fixture_bypass.go"
	fixturePath := filepath.Join(s.moduleRoot, "internal", "cli", "devpolicy", s.fixtureName)
	content := `package devpolicy

import "github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"

// e2eLintFixtureBypass is an intentional lint-rule violation
// used by the custom-lint-rules e2e scenario.
var e2eLintFixtureBypass = steward.PolicyVersion{Signature: nil}
`

	if err := os.WriteFile(fixturePath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("failed to write fixture %s: %w", fixturePath, err)
	}
	s.fixtureFiles = append(s.fixtureFiles, fixturePath)
	return nil
}

func (s *customLintRulesState) theActualCLISourceTree() error {
	s.moduleRoot = resolveModuleRootCLR()
	if _, err := os.Stat(filepath.Join(s.moduleRoot, "go.mod")); err != nil {
		return fmt.Errorf("go.mod not found at module root %s: %w", s.moduleRoot, err)
	}
	return nil
}

// --- When steps ---

func (s *customLintRulesState) makeLintCLIRuns() error {
	return s.runVetToolInDir()
}

// --- Then steps ---

func (s *customLintRulesState) itExitsNonZeroCLR() error {
	if s.exitCode == 0 {
		return fmt.Errorf("expected non-zero exit code, got 0\nstdout:\n%s\nstderr:\n%s",
			s.stdout, s.stderr)
	}
	return nil
}

func (s *customLintRulesState) itExits0CLR() error {
	if s.exitCode != 0 {
		return fmt.Errorf("expected exit code 0, got %d\nstdout:\n%s\nstderr:\n%s",
			s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

func (s *customLintRulesState) stderrNamesTheFileAndTheRule(ruleName string) error {
	combined := s.stdout + s.stderr
	if !strings.Contains(combined, s.fixtureName) {
		return fmt.Errorf("output does not mention fixture file %q\nstdout:\n%s\nstderr:\n%s",
			s.fixtureName, s.stdout, s.stderr)
	}
	if !strings.Contains(combined, ruleName) {
		return fmt.Errorf("output does not mention rule %q\nstdout:\n%s\nstderr:\n%s",
			ruleName, s.stdout, s.stderr)
	}
	return nil
}

// --- Scenario initializer ---

func InitializeScenario_hardening_and_release_custom_lint_rules(ctx *godog.ScenarioContext) {
	s := newCustomLintRulesState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a test fixture file under internal/cli importing database/sql$`,
		s.aTestFixtureFileImportingDatabaseSQL)
	ctx.Step(`^a test fixture file under internal/cli/devpolicy constructing an unsigned PolicyVersion without a prod build tag$`,
		s.aTestFixtureFileConstructingUnsignedPolicyVersion)
	ctx.Step(`^the actual CLI source tree$`,
		s.theActualCLISourceTree)

	// When
	ctx.Step(`^make lint-cli runs$`, s.makeLintCLIRuns)

	// Then
	ctx.Step(`^it exits non-zero$`, s.itExitsNonZeroCLR)
	ctx.Step(`^it exits 0$`, s.itExits0CLR)
	ctx.Step(`^stderr names the file and the "([^"]*)" rule$`, s.stderrNamesTheFileAndTheRule)
}

func TestE2E_hardening_and_release_custom_lint_rules(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_hardening_and_release_custom_lint_rules,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"hardening_and_release_custom_lint_rules.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
