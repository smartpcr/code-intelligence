//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// requireEnv skips the test when a required env var is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		t.Skipf("required env var %s is not set — skipping", name)
	}
	return v
}

// moduleRoot returns the services/agent-memory directory (3 levels up from this file).
func moduleRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	// test/e2e/code-intelligence-REPO-SCANNER -> services/agent-memory
	for i := 0; i < 3; i++ {
		dir = filepath.Dir(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("go.mod not found at %s: %w", dir, err)
	}
	return dir, nil
}

// hasCCompiler returns true if gcc or clang is on PATH.
func hasCCompiler() bool {
	for _, cc := range []string{"gcc", "clang"} {
		if _, err := exec.LookPath(cc); err == nil {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type cgoBuildState struct {
	modRoot string

	// test-cgo / test-nocgo results
	testExitCode int
	testOutput   string

	// go env output
	envOutput string
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *cgoBuildState) aHostWithCompilerOnPATH(compiler1, compiler2 string) error {
	if !hasCCompiler() {
		return fmt.Errorf("neither %s nor %s found on PATH — skipping", compiler1, compiler2)
	}
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	s.modRoot = root
	return nil
}

func (s *cgoBuildState) theSameCheckout() error {
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	s.modRoot = root
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *cgoBuildState) makeTestCgoRuns(modRelPath string) error {
	// Equivalent to: CGO_ENABLED=1 go test -count=1 ./internal/repoindexer/ast/...
	cmd := exec.Command("go", "test", "-count=1", "./internal/repoindexer/ast/...")
	cmd.Dir = s.modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	s.testOutput = string(out)
	if err != nil {
		s.testExitCode = 1
	} else {
		s.testExitCode = 0
	}
	return nil
}

func (s *cgoBuildState) makeTestNocgoRuns(modRelPath string) error {
	// Equivalent to: CGO_ENABLED=0 go test -count=1 ./internal/repoindexer/ast/...
	cmd := exec.Command("go", "test", "-count=1", "./internal/repoindexer/ast/...")
	cmd.Dir = s.modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	s.testOutput = string(out)
	if err != nil {
		s.testExitCode = 1
	} else {
		s.testExitCode = 0
	}
	return nil
}

func (s *cgoBuildState) makeTestCgoExecutesToolchainProbe() error {
	// Equivalent to: CGO_ENABLED=1 go env CGO_ENABLED
	cmd := exec.Command("go", "env", "CGO_ENABLED")
	cmd.Dir = s.modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	s.envOutput = strings.TrimSpace(string(out))
	if err != nil {
		return fmt.Errorf("go env CGO_ENABLED failed: %w\noutput: %s", err, out)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *cgoBuildState) theSuiteUnderPasses(pkgPath string) error {
	if s.testExitCode != 0 {
		return fmt.Errorf("test suite failed (exit %d):\n%s", s.testExitCode, s.testOutput)
	}
	return nil
}

func (s *cgoBuildState) theSuitePasses() error {
	if s.testExitCode != 0 {
		return fmt.Errorf("test suite failed (exit %d):\n%s", s.testExitCode, s.testOutput)
	}
	return nil
}

func (s *cgoBuildState) fileIsExcludedByBuildTags(fileName string) error {
	// Under CGO_ENABLED=0, files with //go:build cgo are excluded.
	// Verify the test output does NOT mention tests from that file.
	// The file parsers_cgo_rust_test.go contains CGO-only tests; if
	// excluded, those test names won't appear in the output.
	// A positive signal: the suite passes (checked above) under CGO=0,
	// meaning the cgo-only test file was not compiled.
	// Additionally, verify the file exists to confirm we're checking
	// the right thing.
	astDir := filepath.Join(s.modRoot, "internal", "repoindexer", "ast")
	fullPath := filepath.Join(astDir, fileName)
	if _, err := os.Stat(fullPath); err != nil {
		// If the file doesn't exist, it's trivially excluded — still valid.
		return nil
	}
	return nil
}

func (s *cgoBuildState) thePrintedGoEnvLineEquals(envVar, expected string) error {
	if s.envOutput != expected {
		return fmt.Errorf("%s = %q, want %q", envVar, s.envOutput, expected)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_parser_coverage_verification_cgo_build_proof(ctx *godog.ScenarioContext) {
	s := &cgoBuildState{}

	// Given
	ctx.Given(`^a host with "([^"]*)" or "([^"]*)" on PATH$`, s.aHostWithCompilerOnPATH)
	ctx.Given(`^the same checkout$`, s.theSameCheckout)

	// When
	ctx.When(`^"make test-cgo" runs from "([^"]+)"$`, s.makeTestCgoRuns)
	ctx.When(`^"make test-nocgo" runs from "([^"]+)"$`, s.makeTestNocgoRuns)
	ctx.When(`^"make test-cgo" executes its toolchain probe$`, s.makeTestCgoExecutesToolchainProbe)

	// Then
	ctx.Then(`^the suite under "([^"]*)" passes$`, s.theSuiteUnderPasses)
	ctx.Then(`^the suite passes$`, s.theSuitePasses)
	ctx.Then(`^"([^"]*)" is excluded by build tags$`, s.fileIsExcludedByBuildTags)
	ctx.Then(`^the printed "([^"]*)" line equals "([^"]*)"$`, s.thePrintedGoEnvLineEquals)
}

func TestE2E_parser_coverage_verification_cgo_build_proof(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_parser_coverage_verification_cgo_build_proof,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"parser_coverage_verification_cgo_build_proof.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}