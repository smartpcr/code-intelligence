//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		t.Skipf("required env var %s is not set — skipping", name)
	}
	return v
}

// moduleRoot returns the absolute path to the services/agent-memory
// module directory by walking up from the test file's location.
func moduleRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	// thisFile is .../services/agent-memory/test/e2e/code-intelligence-.../xxx_test.go
	// filepath.Dir(thisFile) → .../test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT
	// Up 1 → .../test/e2e
	// Up 2 → .../test
	// Up 3 → .../services/agent-memory (module root)
	dir := filepath.Dir(thisFile)
	for i := 0; i < 3; i++ {
		dir = filepath.Dir(dir)
	}
	// Verify go.mod exists at the resolved path.
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("go.mod not found at %s: %w", dir, err)
	}
	return dir, nil
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type additiveParserState struct {
	// Scenario 1: LangMeta nil compiles unchanged
	parseResults  []ast.ParseResult
	buildExitCode int
	buildOutput   string

	// Scenario 3: ErrParserUnavailable identity
	wrappedErr     error
	errorsIsResult bool
}

// ---------------------------------------------------------------------------
// Scenario 1 — LangMeta nil compiles unchanged
//
// Fixtures include import statements so the Import LangMeta
// assertion is non-vacuous (evaluator finding #3).
// ---------------------------------------------------------------------------

// tsFixture includes a class, a method, and an import statement.
const tsFixture = `import { EventEmitter } from "events";

export class Greeter extends EventEmitter {
  greet(name: string): string {
    return "hello " + name;
  }
}
`

// pyFixture includes a class, a method, and an import statement.
const pyFixture = `import os

class Greeter:
    def greet(self, name):
        path = os.getcwd()
        return 'hello ' + name
`

func (s *additiveParserState) existingTSAndPythonParsers() error {
	return nil
}

func (s *additiveParserState) goBuildRunsFromTheModuleRoot() error {
	modRoot, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("cannot locate module root: %w", err)
	}
	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = modRoot
	out, err := cmd.CombinedOutput()
	s.buildOutput = string(out)
	if err != nil {
		s.buildExitCode = 1
		return nil // don't fail the step — the Then step checks the exit code
	}
	s.buildExitCode = 0
	return nil
}

func (s *additiveParserState) itSucceedsWithExitCode0() error {
	if s.buildExitCode != 0 {
		return fmt.Errorf("go build ./... failed (exit %d):\n%s", s.buildExitCode, s.buildOutput)
	}
	return nil
}

func (s *additiveParserState) existingParserTypescriptTestGoTestsPass() error {
	modRoot, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("cannot locate module root: %w", err)
	}
	cmd := exec.Command("go", "test", "-count=1", "-run", "TestTypeScript", "./internal/repoindexer/ast/...")
	cmd.Dir = modRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("parser_typescript_test.go tests failed:\n%s", string(out))
	}
	return nil
}

func (s *additiveParserState) existingParserPythonTestGoTestsPass() error {
	modRoot, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("cannot locate module root: %w", err)
	}
	cmd := exec.Command("go", "test", "-count=1", "-run", "TestPython", "./internal/repoindexer/ast/...")
	cmd.Dir = modRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("parser_python_test.go tests failed:\n%s", string(out))
	}
	return nil
}

func (s *additiveParserState) eachParserParsesASourceFileThatEmitsClassesMethodsAndImports() error {
	tsParser := ast.NewTypeScriptParser()
	pyParser := ast.NewPythonParser()

	tsResult, err := tsParser.Parse("test.ts", []byte(tsFixture))
	if err != nil {
		return fmt.Errorf("TS parse: %w", err)
	}
	if len(tsResult.Imports) == 0 {
		return fmt.Errorf("TS fixture produced zero imports — fixture is broken")
	}

	pyResult, err := pyParser.Parse("test.py", []byte(pyFixture))
	if err != nil {
		return fmt.Errorf("Python parse: %w", err)
	}
	if len(pyResult.Imports) == 0 {
		return fmt.Errorf("Python fixture produced zero imports — fixture is broken")
	}

	s.parseResults = []ast.ParseResult{tsResult, pyResult}
	return nil
}

func (s *additiveParserState) everyClassDeclLangMetaIsNil() error {
	for i, pr := range s.parseResults {
		if len(pr.Classes) == 0 {
			return fmt.Errorf("result %d has zero classes — fixture is broken", i)
		}
		for j, cls := range pr.Classes {
			if cls.LangMeta != nil {
				return fmt.Errorf("ClassDecl.LangMeta is not nil (result %d, class %d): %v", i, j, cls.LangMeta)
			}
		}
	}
	return nil
}

func (s *additiveParserState) everyMethodDeclLangMetaIsNil() error {
	for i, pr := range s.parseResults {
		if len(pr.Methods) == 0 {
			return fmt.Errorf("result %d has zero methods — fixture is broken", i)
		}
		for j, m := range pr.Methods {
			if m.LangMeta != nil {
				return fmt.Errorf("MethodDecl.LangMeta is not nil (result %d, method %d): %v", i, j, m.LangMeta)
			}
		}
	}
	return nil
}

func (s *additiveParserState) everyImportLangMetaIsNil() error {
	for i, pr := range s.parseResults {
		if len(pr.Imports) == 0 {
			return fmt.Errorf("result %d has zero imports — fixture is broken", i)
		}
		for j, imp := range pr.Imports {
			if imp.LangMeta != nil {
				return fmt.Errorf("Import.LangMeta is not nil (result %d, import %d): %v", i, j, imp.LangMeta)
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 3 — ErrParserUnavailable identity
//
// Direct reference to ast.ErrParserUnavailable — the test fails at
// compile time if the impl has not exported the sentinel.
// ---------------------------------------------------------------------------

func (s *additiveParserState) aWrappedErrorFmtErrorfWrappingAstErrParserUnavailable() error {
	s.wrappedErr = fmt.Errorf("powershell: %w (reason=pwsh_not_available)", ast.ErrParserUnavailable)
	return nil
}

func (s *additiveParserState) errorsIsIsEvaluatedAgainstAstErrParserUnavailable() error {
	s.errorsIsResult = errors.Is(s.wrappedErr, ast.ErrParserUnavailable)
	return nil
}

func (s *additiveParserState) itReturnsTrue() error {
	if !s.errorsIsResult {
		return fmt.Errorf("errors.Is(wrappedErr, ast.ErrParserUnavailable) returned false, want true")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_additive_parser_go_struct_surfaces(ctx *godog.ScenarioContext) {
	s := &additiveParserState{}

	// Scenario 1: LangMeta nil compiles unchanged
	ctx.Given(`^existing TS and Python parsers$`, s.existingTSAndPythonParsers)
	ctx.When(`^go build \./\.\.\. runs from the module root$`, s.goBuildRunsFromTheModuleRoot)
	ctx.Then(`^it succeeds with exit code 0$`, s.itSucceedsWithExitCode0)
	ctx.Then(`^existing parser_typescript_test\.go tests pass$`, s.existingParserTypescriptTestGoTestsPass)
	ctx.Then(`^existing parser_python_test\.go tests pass$`, s.existingParserPythonTestGoTestsPass)
	ctx.Then(`^each parser parses a source file that emits classes methods and imports$`, s.eachParserParsesASourceFileThatEmitsClassesMethodsAndImports)
	ctx.Then(`^every ClassDecl LangMeta is nil$`, s.everyClassDeclLangMetaIsNil)
	ctx.Then(`^every MethodDecl LangMeta is nil$`, s.everyMethodDeclLangMetaIsNil)
	ctx.Then(`^every Import LangMeta is nil$`, s.everyImportLangMetaIsNil)

	// Scenario 3: ErrParserUnavailable identity
	ctx.Given(`^a wrapped error fmt\.Errorf wrapping ast\.ErrParserUnavailable$`, s.aWrappedErrorFmtErrorfWrappingAstErrParserUnavailable)
	ctx.When(`^errors\.Is is evaluated against ast\.ErrParserUnavailable$`, s.errorsIsIsEvaluatedAgainstAstErrParserUnavailable)
	ctx.Then(`^it returns true$`, s.itReturnsTrue)
}

func TestE2E_shared_additive_surfaces_and_dispatcher_edits_additive_parser_go_struct_surfaces(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_additive_parser_go_struct_surfaces,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"shared_additive_surfaces_and_dispatcher_edits_additive_parser_go_struct_surfaces.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}