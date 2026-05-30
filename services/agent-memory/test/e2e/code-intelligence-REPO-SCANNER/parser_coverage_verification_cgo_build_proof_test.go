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

	// make target results
	makeExitCode int
	makeOutput   string
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *cgoBuildState) aHostWithCompilerAndMake(compiler1, compiler2 string) error {
	if !hasCCompiler() {
		return fmt.Errorf("neither %s nor %s found on PATH", compiler1, compiler2)
	}
	if _, err := exec.LookPath("make"); err != nil {
		return fmt.Errorf("make not found on PATH: %w", err)
	}
	root, err := moduleRoot()
	if err != nil {
		return err
	}
	s.modRoot = root
	return nil
}

func (s *cgoBuildState) makeIsAvailable() error {
	if _, err := exec.LookPath("make"); err != nil {
		return fmt.Errorf("make not found on PATH: %w", err)
	}
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

func (s *cgoBuildState) makeTargetRunsFrom(target, modRelPath string) error {
	cmd := exec.Command("make", target)
	cmd.Dir = s.modRoot
	out, err := cmd.CombinedOutput()
	s.makeOutput = string(out)
	if err != nil {
		s.makeExitCode = 1
	} else {
		s.makeExitCode = 0
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *cgoBuildState) makeTargetExitsSuccessfully() error {
	if s.makeExitCode != 0 {
		return fmt.Errorf("make target failed (exit %d):\n%s", s.makeExitCode, s.makeOutput)
	}
	return nil
}

func (s *cgoBuildState) outputIncludesTestResultsFrom(pkgSubpath string) error {
	if !strings.Contains(s.makeOutput, pkgSubpath) {
		return fmt.Errorf("make output does not mention %q;\noutput:\n%s", pkgSubpath, s.makeOutput)
	}
	return nil
}

func (s *cgoBuildState) underCGO0FileIsExcluded(fileName string) error {
	// Use `go list` with CGO_ENABLED=0 to get the actual file lists
	// compiled for the ast package — this proves build-tag exclusion.
	goFiles, testFiles, err := s.listASTFiles("0")
	if err != nil {
		return err
	}
	all := append(goFiles, testFiles...)
	for _, f := range all {
		if f == fileName {
			return fmt.Errorf("%s IS compiled under CGO_ENABLED=0 (should be excluded); files: %v", fileName, all)
		}
	}
	return nil
}

func (s *cgoBuildState) underCGO0FileIsIncluded(fileName string) error {
	goFiles, testFiles, err := s.listASTFiles("0")
	if err != nil {
		return err
	}
	all := append(goFiles, testFiles...)
	for _, f := range all {
		if f == fileName {
			return nil
		}
	}
	return fmt.Errorf("%s NOT compiled under CGO_ENABLED=0 (should be included); files: %v", fileName, all)
}

func (s *cgoBuildState) makeOutputContainsProbeLineEqualTo(probeDesc, expected string) error {
	// The Makefile's test-cgo target runs:
	//   @echo "==> test-cgo: active Go toolchain (CGO_ENABLED=1)"
	//   @CGO_ENABLED=1 go env CGO_ENABLED CC CXX
	// `go env CGO_ENABLED CC CXX` prints each value on its own line.
	// The CGO_ENABLED value appears as the first env line after the echo.
	// We scan for a line that is exactly "1".
	lines := strings.Split(s.makeOutput, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == expected {
			return nil
		}
	}
	return fmt.Errorf("make output does not contain a line equal to %q;\noutput:\n%s", expected, s.makeOutput)
}

// listASTFiles returns the Go source files and test files compiled for
// the ast package under the given CGO_ENABLED value.
func (s *cgoBuildState) listASTFiles(cgoEnabled string) (goFiles []string, testFiles []string, err error) {
	cmd := exec.Command("go", "list", "-f",
		`{{range .GoFiles}}{{.}} {{end}}|||{{range .TestGoFiles}}{{.}} {{end}}`,
		"./internal/repoindexer/ast/")
	cmd.Dir = s.modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgoEnabled)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, nil, fmt.Errorf("go list failed (CGO_ENABLED=%s): %w\noutput: %s", cgoEnabled, err, out)
	}
	parts := strings.SplitN(strings.TrimSpace(string(out)), "|||", 2)
	goFiles = splitNonEmpty(parts[0])
	if len(parts) > 1 {
		testFiles = splitNonEmpty(parts[1])
	}
	return goFiles, testFiles, nil
}

func splitNonEmpty(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_parser_coverage_verification_cgo_build_proof(ctx *godog.ScenarioContext) {
	s := &cgoBuildState{}

	// Given
	ctx.Given(`^a host with "([^"]*)" or "([^"]*)" on PATH and "make" available$`, s.aHostWithCompilerAndMake)
	ctx.Given(`^"make" is available on PATH$`, s.makeIsAvailable)

	// When — matches both "make test-cgo" and "make test-nocgo"
	ctx.When(`^"make ([^"]*)" runs from "([^"]*)"$`, s.makeTargetRunsFrom)

	// Then
	ctx.Then(`^the make target exits successfully$`, s.makeTargetExitsSuccessfully)
	ctx.Then(`^the output includes test results from "([^"]*)"$`, s.outputIncludesTestResultsFrom)
	ctx.Then(`^under CGO_ENABLED=0 "([^"]*)" is excluded by build tags$`, s.underCGO0FileIsExcluded)
	ctx.Then(`^under CGO_ENABLED=0 "([^"]*)" is included by build tags$`, s.underCGO0FileIsIncluded)
	ctx.Then(`^the make output contains a "([^"]*)" probe line equal to "([^"]*)"$`, s.makeOutputContainsProbeLineEqualTo)
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