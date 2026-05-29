//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
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

// moduleRoot returns the services/agent-memory directory.
func moduleRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	// thisFile is <mod>/test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT/<file>.go
	// Walk up 3 levels to reach <mod>.
	dir := filepath.Dir(thisFile)
	for i := 0; i < 3; i++ {
		dir = filepath.Dir(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("go.mod not found at %s: %w", dir, err)
	}
	return dir, nil
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type rustParserState struct {
	// Scenario 1: Build
	buildExitCode int
	buildOutput   string

	// Scenario 2 & 3: Parse
	rustSource  string
	parseResult ast.ParseResult
	parseErr    error
}

// ---------------------------------------------------------------------------
// Scenario 1 — Build under CGO=on
// ---------------------------------------------------------------------------

func (s *rustParserState) cgoEnabled1() error {
	// CGO_ENABLED=1 is set on the build command in the When step.
	return nil
}

func (s *rustParserState) goBuildAstRunsFromModule() error {
	modRoot, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("cannot locate module root: %w", err)
	}
	cmd := exec.Command("go", "build", "./internal/repoindexer/ast/...")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	s.buildOutput = string(out)
	if err != nil {
		s.buildExitCode = 1
		return nil
	}
	s.buildExitCode = 0
	return nil
}

func (s *rustParserState) buildSucceeds() error {
	if s.buildExitCode != 0 {
		return fmt.Errorf("go build failed (exit %d):\n%s", s.buildExitCode, s.buildOutput)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — Trait + impl method emit
// ---------------------------------------------------------------------------

func (s *rustParserState) aRustSourceString(src *godog.DocString) error {
	s.rustSource = src.Content
	return nil
}

func (s *rustParserState) rustTreeSitterParserParseRuns() error {
	parser := ast.NewTreeSitterRustParser()
	result, err := parser.Parse("test.rs", []byte(s.rustSource))
	s.parseResult = result
	s.parseErr = err
	if err != nil {
		return fmt.Errorf("Parse returned error: %w", err)
	}
	return nil
}

func (s *rustParserState) methodsContainsWithLangMetaTraitDefaultTrue(qn string) error {
	for _, m := range s.parseResult.Methods {
		if m.QualifiedName == qn {
			if m.LangMeta == nil || m.LangMeta["trait_default"] != true {
				return fmt.Errorf("method %q LangMeta[trait_default] = %v; want true (LangMeta=%+v)",
					qn, m.LangMeta["trait_default"], m.LangMeta)
			}
			return nil
		}
	}
	return fmt.Errorf("method %q not found; have %v", qn, methodQNs(s.parseResult.Methods))
}

func (s *rustParserState) methodsContainsWithLangMetaTrait(qn, traitName string) error {
	for _, m := range s.parseResult.Methods {
		if m.QualifiedName == qn {
			if m.LangMeta == nil || m.LangMeta["trait"] != traitName {
				return fmt.Errorf("method %q LangMeta[trait] = %v; want %q (LangMeta=%+v)",
					qn, m.LangMeta["trait"], traitName, m.LangMeta)
			}
			return nil
		}
	}
	return fmt.Errorf("method %q not found; have %v", qn, methodQNs(s.parseResult.Methods))
}

func (s *rustParserState) classDeclHasImplementsContaining(className, traitName string) error {
	for _, c := range s.parseResult.Classes {
		if c.QualifiedName == className {
			for _, impl := range c.Implements {
				if impl == traitName {
					return nil
				}
			}
			return fmt.Errorf("ClassDecl %q Implements = %v; want entry %q",
				className, c.Implements, traitName)
		}
	}
	return fmt.Errorf("ClassDecl %q not found; have %v", className, classQNs(s.parseResult.Classes))
}

// ---------------------------------------------------------------------------
// Scenario 3 — Supertrait extends
// ---------------------------------------------------------------------------

func (s *rustParserState) classDeclHasExtendsContaining(className, parentName string) error {
	for _, c := range s.parseResult.Classes {
		if c.QualifiedName == className {
			for _, ext := range c.Extends {
				if ext == parentName {
					return nil
				}
			}
			return fmt.Errorf("ClassDecl %q Extends = %v; want entry %q",
				className, c.Extends, parentName)
		}
	}
	return fmt.Errorf("ClassDecl %q not found; have %v", className, classQNs(s.parseResult.Classes))
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func methodQNs(methods []ast.MethodDecl) []string {
	qns := make([]string, len(methods))
	for i, m := range methods {
		qns[i] = m.QualifiedName
	}
	return qns
}

func classQNs(classes []ast.ClassDecl) []string {
	qns := make([]string, len(classes))
	for i, c := range classes {
		qns[i] = c.QualifiedName
	}
	return qns
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_rust_parser_rusttreesitterparser_implementation(ctx *godog.ScenarioContext) {
	s := &rustParserState{}

	// Scenario 1: Build
	ctx.Given(`^CGO_ENABLED=1$`, s.cgoEnabled1)
	ctx.When(`^go build \./internal/repoindexer/ast/\.\.\. runs from services/agent-memory$`, s.goBuildAstRunsFromModule)
	ctx.Then(`^it succeeds$`, s.buildSucceeds)

	// Scenario 2: Trait + impl method emit
	ctx.Given(`^a Rust source string:$`, s.aRustSourceString)
	ctx.When(`^rustTreeSitterParser\.Parse runs$`, s.rustTreeSitterParserParseRuns)
	ctx.Then(`^Methods contains "([^"]*)" with LangMeta trait_default true$`, s.methodsContainsWithLangMetaTraitDefaultTrue)
	ctx.Then(`^Methods contains "([^"]*)" with LangMeta trait "([^"]*)"$`, s.methodsContainsWithLangMetaTrait)
	ctx.Then(`^ClassDecl "([^"]*)" has Implements containing "([^"]*)"$`, s.classDeclHasImplementsContaining)

	// Scenario 3: Supertrait extends
	ctx.Then(`^ClassDecl "([^"]*)" has Extends containing "([^"]*)"$`, s.classDeclHasExtendsContaining)
}

func TestE2E_rust_parser_rusttreesitterparser_implementation(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_rust_parser_rusttreesitterparser_implementation,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"rust_parser_rusttreesitterparser_implementation.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}