//go:build e2e

package e2e

import (
	"fmt"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// ---------------------------------------------------------------------------
// Scenario state for stage 4.2: register-csharp-parser-in-parsers-cgo-go
// ---------------------------------------------------------------------------

type csharpRegState struct {
	parser ast.Parser
}

// ---------------------------------------------------------------------------
// Given: the dispatcher under CGO=on
//
// parsers_cgo.go registers the C# parser via init(). Because the ast
// package is imported above, the init() has already run by test time.
// ---------------------------------------------------------------------------

func (s *csharpRegState) theDispatcherUnderCGOOn() error {
	// parsers_cgo.go init() fires on import — nothing else needed.
	return nil
}

// ---------------------------------------------------------------------------
// When: selectParser("foo.cs", nil) / selectParser("foo.csx", nil)
//
// Calls ast.SelectParser to exercise the default extension→parser map
// populated by parsers_cgo.go.
// ---------------------------------------------------------------------------

func (s *csharpRegState) selectParserRunsFor(filename string) error {
	p := ast.SelectParser(filename, nil)
	if p == nil {
		return fmt.Errorf("SelectParser(%q, nil) returned nil — extension not registered", filename)
	}
	s.parser = p
	return nil
}

// ---------------------------------------------------------------------------
// Then: Language() == "csharp"
// ---------------------------------------------------------------------------

func (s *csharpRegState) theSelectedParserLanguageIs(expected string) error {
	if s.parser == nil {
		return fmt.Errorf("no parser was selected in the When step")
	}
	got := s.parser.Language()
	if got != expected {
		return fmt.Errorf("Language() = %q, want %q", got, expected)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_csharp_parser_register_csharp_parser_in_parsers_cgo_go(ctx *godog.ScenarioContext) {
	s := &csharpRegState{}

	ctx.Given(`^the dispatcher under CGO=on$`, s.theDispatcherUnderCGOOn)
	ctx.When(`^selectParser runs for "([^"]*)"$`, s.selectParserRunsFor)
	ctx.Then(`^the selected parser Language is "([^"]*)"$`, s.theSelectedParserLanguageIs)
}

func TestE2E_csharp_parser_register_csharp_parser_in_parsers_cgo_go(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_csharp_parser_register_csharp_parser_in_parsers_cgo_go,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"csharp_parser_register_csharp_parser_in_parsers_cgo_go.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
