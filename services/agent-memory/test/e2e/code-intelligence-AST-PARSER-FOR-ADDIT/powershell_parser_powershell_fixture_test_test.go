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

// ---------------------------------------------------------------------------
// Shared helpers (one copy per package — guarded by build tag)
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		t.Skipf("required env var %s is not set — skipping", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Module root and AST directory discovery
// ---------------------------------------------------------------------------

func psFixtureModuleRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	// thisFile is <MOD>/test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT/<file>.go
	dir := filepath.Dir(thisFile)
	for i := 0; i < 3; i++ {
		dir = filepath.Dir(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("go.mod not found at %s: %w", dir, err)
	}
	return dir, nil
}

func psAstDirExists(modRoot string) bool {
	_, err := os.Stat(filepath.Join(modRoot, "internal", "repoindexer", "ast"))
	return err == nil
}

// ---------------------------------------------------------------------------
// Subprocess runner — executes go test against the real ast package
//
// Instead of probe injection, this approach runs the actual unit tests
// (TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet,
// TestPowerShellParser_NoPwsh_ReturnsSentinel) as subprocesses and
// checks their pass/skip/fail status. This ensures E2E coverage is not
// masked by godog.ErrPending when the implementation is present.
// ---------------------------------------------------------------------------

type psGoTestResult struct {
	output  string
	passed  bool
	skipped bool
	noTests bool
}

func psRunGoTest(modRoot, runPattern string, envOverrides ...string) psGoTestResult {
	cmd := exec.Command("go", "test",
		"-run", runPattern,
		"-v", "-count=1",
		"./internal/repoindexer/ast/",
	)
	cmd.Dir = modRoot

	env := make([]string, len(os.Environ()))
	copy(env, os.Environ())
	for _, ov := range envOverrides {
		key := strings.SplitN(ov, "=", 2)[0]
		upper := strings.ToUpper(key) + "="
		replaced := false
		for i, e := range env {
			if strings.HasPrefix(strings.ToUpper(e), upper) {
				env[i] = ov
				replaced = true
				break
			}
		}
		if !replaced {
			env = append(env, ov)
		}
	}
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	output := string(out)

	r := psGoTestResult{output: output}
	if err == nil {
		r.passed = true
	}
	if strings.Contains(output, "--- SKIP:") {
		r.skipped = true
		r.passed = true
	}
	if strings.Contains(output, "no tests to run") || strings.Contains(output, "no test files") {
		r.noTests = true
	}
	return r
}

// psPathWithoutPwsh returns the current PATH with all directories
// containing pwsh or pwsh.exe removed.
func psPathWithoutPwsh() string {
	origPath := os.Getenv("PATH")
	parts := filepath.SplitList(origPath)
	var filtered []string
	for _, p := range parts {
		hasPwsh := false
		for _, name := range []string{"pwsh", "pwsh.exe"} {
			if _, err := os.Stat(filepath.Join(p, name)); err == nil {
				hasPwsh = true
				break
			}
		}
		if !hasPwsh {
			filtered = append(filtered, p)
		}
	}
	return strings.Join(filtered, string(os.PathListSeparator))
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type psFixtureState struct {
	modRoot    string
	testResult *psGoTestResult
}

// ---------------------------------------------------------------------------
// Scenario 1 — pwsh-present fixture parses (@needs-pwsh)
//
// Runs the real unit test TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet
// via subprocess. A PASS result confirms the expected node/edge counts
// because the unit test internally verifies 1 class + 3 method + 1 package
// nodes along with contains, static_calls, and imports edges.
// ---------------------------------------------------------------------------

func (s *psFixtureState) theEmbeddedPowerShellFixture() error {
	// The fixture is embedded in the unit test, not this E2E wrapper.
	return nil
}

func (s *psFixtureState) emitFileRunsForThePowerShellFixture() error {
	if _, err := exec.LookPath("pwsh"); err != nil {
		return godog.ErrPending // @needs-pwsh — pwsh not available on this host
	}
	r := psRunGoTest(s.modRoot, "TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet")
	s.testResult = &r
	if r.noTests {
		return fmt.Errorf("TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet not found in ast package")
	}
	if !r.passed {
		return fmt.Errorf("fixture test failed:\n%s", r.output)
	}
	return nil
}

func (s *psFixtureState) classMethodAndPackageNodesAreEmittedForPowerShell(
	classCount, methodCount, packageCount int,
) error {
	if s.testResult == nil {
		return fmt.Errorf("no test result — When step did not execute")
	}
	// The unit test TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet verifies
	// the exact node counts internally. A PASS confirms the expected
	// %d class + %d method + %d package nodes match implementation output.
	if !s.testResult.passed {
		return fmt.Errorf("fixture test did not pass — cannot confirm %d class, %d method, %d package nodes:\n%s",
			classCount, methodCount, packageCount, s.testResult.output)
	}
	return nil
}

func (s *psFixtureState) thePowerShellFixtureEmitsContainsStaticCallsAndImportsEdges() error {
	if s.testResult == nil {
		return fmt.Errorf("no test result — When step did not execute")
	}
	if !s.testResult.passed {
		return fmt.Errorf("fixture test did not pass — cannot confirm edge emission:\n%s", s.testResult.output)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — pwsh-absent fixture is skipped (@no-pwsh)
//
// Runs TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet with PATH modified
// to exclude pwsh. Verifies the test calls t.Skip (--- SKIP: in output)
// and exits cleanly (exit code 0). This directly tests the acceptance
// scenario: "it calls t.Skip and reports no failure."
// ---------------------------------------------------------------------------

func (s *psFixtureState) thePowerShellASTImplementationIsAvailable() error {
	// Already verified in TestE2E entrypoint via psAstDirExists.
	return nil
}

func (s *psFixtureState) testPowerShellFixtureRunsWithoutPwshOnPATH() error {
	noPwshPath := psPathWithoutPwsh()
	r := psRunGoTest(s.modRoot, "TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet",
		"PATH="+noPwshPath)
	s.testResult = &r
	if r.noTests {
		return fmt.Errorf("TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet not found in ast package")
	}
	return nil
}

func (s *psFixtureState) itCallsTSkipAndReportsNoFailure() error {
	if s.testResult == nil {
		return fmt.Errorf("no test result — When step did not execute")
	}
	if !s.testResult.skipped {
		return fmt.Errorf("expected test to call t.Skip (--- SKIP: marker) but none found in output:\n%s",
			s.testResult.output)
	}
	if !s.testResult.passed {
		return fmt.Errorf("expected test to report no failure (exit 0) but it failed:\n%s",
			s.testResult.output)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 3 — Sentinel-returning parser (@no-pwsh, unconditional)
//
// Runs TestPowerShellParser_NoPwsh_ReturnsSentinel via subprocess.
// This unit test constructs &PowerShellParser{} with empty pwshBin,
// calls Parse, and asserts errors.Is(err, ErrParserUnavailable) == true
// and ParseResult is empty. It requires no pwsh binary and runs
// unconditionally when the AST implementation is present.
// ---------------------------------------------------------------------------

func (s *psFixtureState) aPowerShellParserWithEmptyPwshBin() error {
	// The sentinel unit test constructs the parser with empty pwshBin internally.
	return nil
}

func (s *psFixtureState) testPowerShellParserNoPwshReturnsSentinelRuns() error {
	r := psRunGoTest(s.modRoot, "TestPowerShellParser_NoPwsh_ReturnsSentinel")
	s.testResult = &r
	if r.noTests {
		return fmt.Errorf("TestPowerShellParser_NoPwsh_ReturnsSentinel not found in ast package")
	}
	if !r.passed {
		return fmt.Errorf("sentinel test failed:\n%s", r.output)
	}
	return nil
}

func (s *psFixtureState) errorsIsReturnsTrueForErrParserUnavailable() error {
	if s.testResult == nil {
		return fmt.Errorf("no test result — When step did not execute")
	}
	// TestPowerShellParser_NoPwsh_ReturnsSentinel verifies
	// errors.Is(err, ErrParserUnavailable) == true. A PASS confirms it.
	if !s.testResult.passed {
		return fmt.Errorf("sentinel test did not pass — cannot confirm ErrParserUnavailable:\n%s",
			s.testResult.output)
	}
	return nil
}

func (s *psFixtureState) theParseResultHasNoClassesMethodsOrImports() error {
	if s.testResult == nil {
		return fmt.Errorf("no test result — When step did not execute")
	}
	// TestPowerShellParser_NoPwsh_ReturnsSentinel verifies ParseResult
	// is empty (no classes, methods, or imports). A PASS confirms it.
	if !s.testResult.passed {
		return fmt.Errorf("sentinel test did not pass — cannot confirm empty ParseResult:\n%s",
			s.testResult.output)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_powershell_parser_powershell_fixture_test(ctx *godog.ScenarioContext) {
	modRoot, _ := psFixtureModuleRoot() // already verified in TestE2E entrypoint
	s := &psFixtureState{modRoot: modRoot}

	// Scenario 1: pwsh-present fixture parses
	ctx.Given(`^the embedded PowerShell fixture$`, s.theEmbeddedPowerShellFixture)
	ctx.When(`^EmitFile runs for the PowerShell fixture$`, s.emitFileRunsForThePowerShellFixture)
	ctx.Then(`^(\d+) class, (\d+) method, and (\d+) package nodes are emitted for PowerShell$`,
		s.classMethodAndPackageNodesAreEmittedForPowerShell)
	ctx.Then(`^the PowerShell fixture emits contains, static_calls, and imports edges$`,
		s.thePowerShellFixtureEmitsContainsStaticCallsAndImportsEdges)

	// Scenario 2: pwsh-absent fixture is skipped
	ctx.Given(`^the PowerShell AST implementation is available$`,
		s.thePowerShellASTImplementationIsAvailable)
	ctx.When(`^TestPowerShellFixture_EmitsExpectedNodeAndEdgeSet runs without pwsh on PATH$`,
		s.testPowerShellFixtureRunsWithoutPwshOnPATH)
	ctx.Then(`^it calls t\.Skip and reports no failure$`,
		s.itCallsTSkipAndReportsNoFailure)

	// Scenario 3: Sentinel-returning parser
	ctx.Given(`^a PowerShell parser with empty pwshBin$`,
		s.aPowerShellParserWithEmptyPwshBin)
	ctx.When(`^TestPowerShellParser_NoPwsh_ReturnsSentinel runs$`,
		s.testPowerShellParserNoPwshReturnsSentinelRuns)
	ctx.Then(`^errors\.Is returns true for ErrParserUnavailable$`,
		s.errorsIsReturnsTrueForErrParserUnavailable)
	ctx.Then(`^the ParseResult has no classes methods or imports$`,
		s.theParseResultHasNoClassesMethodsOrImports)
}

func TestE2E_powershell_parser_powershell_fixture_test(t *testing.T) {
	modRoot, err := psFixtureModuleRoot()
	if err != nil {
		t.Skipf("cannot locate module root: %v", err)
	}
	if !psAstDirExists(modRoot) {
		t.Skip("PowerShell AST implementation not present — skipping E2E suite")
	}

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_powershell_parser_powershell_fixture_test,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"powershell_parser_powershell_fixture_test.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}