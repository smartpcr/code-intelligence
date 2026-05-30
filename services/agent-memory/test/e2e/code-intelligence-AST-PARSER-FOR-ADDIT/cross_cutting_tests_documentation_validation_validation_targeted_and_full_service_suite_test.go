//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// ---------------------------------------------------------------------------
// validationState holds per-scenario state for the validation suite.
// ---------------------------------------------------------------------------

type validationState struct {
	cgoEnabled string
	exitCode   int
	stdout     string
	stderr     string
}

// moduleRoot returns the Go module root (services/agent-memory).
// It walks up from this test file's directory until it finds go.mod.
func moduleRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", thisFile)
		}
		dir = parent
	}
}

// ---------------------------------------------------------------------------
// Step implementations
// ---------------------------------------------------------------------------

func (s *validationState) cgoEnabledIsSetTo(val string) error {
	s.cgoEnabled = val
	return nil
}

func (s *validationState) commandRunsFromTheModuleRoot(command string) error {
	modRoot, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("locating module root: %w", err)
	}

	parts := strings.Fields(command)
	if len(parts) == 0 {
		return fmt.Errorf("empty command")
	}

	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+s.cgoEnabled)

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf

	runErr := cmd.Run()
	s.stdout = stdoutBuf.String()
	s.stderr = stderrBuf.String()

	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			s.exitCode = exitErr.ExitCode()
		} else {
			return fmt.Errorf("exec failed: %w", runErr)
		}
	} else {
		s.exitCode = 0
	}
	return nil
}

func (s *validationState) theExitCodeIs(expected int) error {
	if s.exitCode != expected {
		// Include tail of combined output to aid debugging.
		combined := s.stdout + "\n" + s.stderr
		lines := strings.Split(combined, "\n")
		tail := lines
		if len(tail) > 40 {
			tail = tail[len(tail)-40:]
		}
		return fmt.Errorf(
			"expected exit code %d, got %d\n--- last 40 lines ---\n%s",
			expected, s.exitCode, strings.Join(tail, "\n"),
		)
	}
	return nil
}

// treeSitterTestNames lists test functions that live in cgo-only
// build-tagged files (parser_treesitter_*.go). When CGO_ENABLED=0
// these tests must NOT appear in the verbose test output.
var treeSitterTestNames = []string{
	"TestCTreeSitterParser",
	"TestCppTreeSitterParser",
	"TestCSharpTreeSitterParser",
	"TestGoTreeSitterParser",
	"TestRustTreeSitterParser",
}

func (s *validationState) theNewTreeSitterParserTestsAreExcludedByBuildTags() error {
	combined := s.stdout + "\n" + s.stderr
	for _, name := range treeSitterTestNames {
		// In verbose go test output, a running test shows as
		// "--- PASS: TestX" or "=== RUN   TestX". If any tree-sitter
		// test name appears, the build tags did not exclude it.
		if strings.Contains(combined, name) {
			return fmt.Errorf(
				"tree-sitter test %q was NOT excluded under CGO_ENABLED=0; "+
					"it should be gated by a //go:build cgo tag",
				name,
			)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_cross_cutting_tests_documentation_validation_validation_targeted_and_full_service_suite(ctx *godog.ScenarioContext) {
	s := &validationState{}

	ctx.Given(`^CGO_ENABLED is set to "([^"]*)"$`, s.cgoEnabledIsSetTo)
	ctx.When(`^"([^"]*)" runs from the module root$`, s.commandRunsFromTheModuleRoot)
	ctx.Then(`^the exit code is (\d+)$`, s.theExitCodeIs)
	ctx.Then(`^the new tree-sitter parser tests are excluded by build tags$`, s.theNewTreeSitterParserTestsAreExcludedByBuildTags)
}

func TestE2E_cross_cutting_tests_documentation_validation_validation_targeted_and_full_service_suite(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_cross_cutting_tests_documentation_validation_validation_targeted_and_full_service_suite,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"cross_cutting_tests_documentation_validation_validation_targeted_and_full_service_suite.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}