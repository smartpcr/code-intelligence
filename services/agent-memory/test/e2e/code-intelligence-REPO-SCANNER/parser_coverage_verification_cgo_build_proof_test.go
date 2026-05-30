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
	// Validate that the Gherkin-specified relative path matches the resolved module root.
	normalised := filepath.ToSlash(s.modRoot)
	if !strings.HasSuffix(normalised, modRelPath) {
		return fmt.Errorf("modRelPath %q does not match resolved module root %q", modRelPath, s.modRoot)
	}

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

func (s *cgoBuildState) underCGO1FileIsCompiledInAstPackage(fileName string) error {
	// Use `go list` with CGO_ENABLED=1 to verify the named test file
	// is in the compiled file set for the ast package.
	goFiles, testFiles, err := s.listASTFiles("1")
	if err != nil {
		return err
	}
	all := append(goFiles, testFiles...)
	for _, f := range all {
		if f == fileName {
			return nil
		}
	}
	return fmt.Errorf("%s NOT compiled under CGO_ENABLED=1 (should be included); files: %v", fileName, all)
}

func (s *cgoBuildState) underCGO0FileIsExcluded(fileName string) error {
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

func (s *cgoBuildState) parsersNocgoRegistersOnly(fileName, expectedConstructor string) error {
	// Read parsers_nocgo.go and verify only the expected constructor
	// appears in the defaultParsers() return list.
	filePath := filepath.Join(s.modRoot, "internal", "repoindexer", "ast", fileName)
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", fileName, err)
	}
	src := string(content)

	// Extract constructor calls: lines matching New*Parser() inside defaultParsers.
	var constructors []string
	for _, line := range strings.Split(src, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "New") && strings.Contains(trimmed, "Parser()") {
			// Strip trailing comma and parens for the constructor name.
			name := strings.TrimRight(trimmed, " ,")
			name = strings.TrimSuffix(name, "()")
			constructors = append(constructors, name)
		}
	}

	if len(constructors) == 0 {
		return fmt.Errorf("%s: no New*Parser() constructors found in file;\ncontent:\n%s", fileName, src)
	}
	if len(constructors) != 1 || constructors[0] != expectedConstructor {
		return fmt.Errorf(
			"%s: expected only %s, found %v — additional parsers registered under nocgo",
			fileName, expectedConstructor, constructors,
		)
	}
	return nil
}

func (s *cgoBuildState) goEnvCGOEnabledAfterEchoMarkerEquals(expected string) error {
	// The Makefile's test-cgo target runs:
	//   @echo "==> test-cgo: active Go toolchain (CGO_ENABLED=1)"
	//   @CGO_ENABLED=1 go env CGO_ENABLED CC CXX
	// `go env CGO_ENABLED CC CXX` prints each value on its own line.
	// The CGO_ENABLED value is the FIRST line after the echo marker.
	// We locate the echo marker and assert the immediately following
	// line equals the expected value, tying the assertion precisely
	// to the `go env CGO_ENABLED` probe output.
	lines := strings.Split(s.makeOutput, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "==> test-cgo:") && strings.Contains(trimmed, "CGO_ENABLED") {
			// Found the echo marker — the next line is the CGO_ENABLED value.
			if i+1 >= len(lines) {
				return fmt.Errorf("echo marker found at line %d but no subsequent line;\noutput:\n%s", i, s.makeOutput)
			}
			actual := strings.TrimSpace(lines[i+1])
			if actual != expected {
				return fmt.Errorf(
					"go env CGO_ENABLED probe (line after echo marker) = %q, want %q;\nmarker line: %q\nprobe line: %q\nfull output:\n%s",
					actual, expected, trimmed, lines[i+1], s.makeOutput,
				)
			}
			return nil
		}
	}
	return fmt.Errorf(
		"echo marker line containing '==> test-cgo:' and 'CGO_ENABLED' not found in make output;\noutput:\n%s",
		s.makeOutput,
	)
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
	ctx.Then(`^under CGO_ENABLED=1 "([^"]*)" is compiled in the ast package$`, s.underCGO1FileIsCompiledInAstPackage)
	ctx.Then(`^under CGO_ENABLED=0 "([^"]*)" is excluded by build tags$`, s.underCGO0FileIsExcluded)
	ctx.Then(`^under CGO_ENABLED=0 "([^"]*)" is included by build tags$`, s.underCGO0FileIsIncluded)
	ctx.Then(`^"([^"]*)" registers only "([^"]*)" as additional parsers$`, s.parsersNocgoRegistersOnly)
	ctx.Then(`^the "go env CGO_ENABLED" value printed after the toolchain echo marker equals "([^"]*)"$`, s.goEnvCGOEnabledAfterEchoMarkerEquals)
}

func TestE2E_parser_coverage_verification_cgo_build_proof(t *testing.T) {
	// Resolve the feature file relative to this source file so the test
	// works regardless of the working directory `go test` is invoked from.
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile), "parser_coverage_verification_cgo_build_proof.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_parser_coverage_verification_cgo_build_proof,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{featurePath},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}