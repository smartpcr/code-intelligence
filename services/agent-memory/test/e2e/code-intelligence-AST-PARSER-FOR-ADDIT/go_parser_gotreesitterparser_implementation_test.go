//go:build e2e && cgo

// Real Go-parser e2e (not a stub). The goTreeSitterParser stage owns
// the actual Go parser in
// `internal/repoindexer/ast/parser_treesitter_go.go`, so every
// scenario below exercises real walker output rather than empty
// placeholder ParseResult shapes (contrast with the C# stub
// counterpart in this directory which only pins the LanguageParser
// surface).
//
// Scenarios mirror the workstream's stated extraction contract
// (struct/interface/type-alias ClassDecls with embeds LangMeta,
// pointer-receiver MethodDecls with receiver_ptr/receiver_type
// LangMeta and ReceiverAliases, grouped imports with dot/blank
// LangMeta flags) so the e2e gate verifies the same surface the
// unit tests in `parser_treesitter_go_test.go` cover, but through
// the public LanguageParser interface and Gherkin scenarios that
// downstream consumers can read.
//
// Build tag is `e2e && cgo` to match the C++/C# e2e files in this
// directory; the godog suite only runs when both tags are set.
// The package's shared moduleRoot/helper functions (defined in
// shared_additive_surfaces_and_dispatcher_edits_*_test.go) are
// intentionally NOT re-declared here.

package e2e

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type goParserState struct {
	parser      ast.LanguageParser
	source      string
	parseResult ast.ParseResult
}

// ---------------------------------------------------------------------------
// Scenario 1 — Language and Extensions contract
// ---------------------------------------------------------------------------

func (s *goParserState) theGoTreeSitterParserIsConstructed() error {
	s.parser = ast.NewTreeSitterGoParser()
	if s.parser == nil {
		return fmt.Errorf("NewTreeSitterGoParser returned nil")
	}
	return nil
}

func (s *goParserState) theParserLanguageIsGo(lang string) error {
	if got := s.parser.Language(); got != lang {
		return fmt.Errorf("Language: want %q, got %q", lang, got)
	}
	return nil
}

func (s *goParserState) theParserExtensionsIncludeGo(ext string) error {
	for _, e := range s.parser.Extensions() {
		if e == ext {
			return nil
		}
	}
	return fmt.Errorf("Extensions %v does not include %q", s.parser.Extensions(), ext)
}

// ---------------------------------------------------------------------------
// Shared parse helpers
// ---------------------------------------------------------------------------

func (s *goParserState) goSource(label string, src *godog.DocString) error {
	_ = label
	s.source = strings.TrimSpace(src.Content)
	return nil
}

func (s *goParserState) goStructSource(src *godog.DocString) error { return s.goSource("struct", src) }
func (s *goParserState) goInterfaceSource(src *godog.DocString) error {
	return s.goSource("interface", src)
}
func (s *goParserState) goPointerReceiverSource(src *godog.DocString) error {
	return s.goSource("pointer-receiver", src)
}
func (s *goParserState) goTypeAliasSource(src *godog.DocString) error {
	return s.goSource("type-alias", src)
}
func (s *goParserState) goImportSource(src *godog.DocString) error { return s.goSource("import", src) }

func (s *goParserState) theSourceIsParsedWithTheGoTreeSitterParser() error {
	if s.parser == nil {
		s.parser = ast.NewTreeSitterGoParser()
	}
	result, err := s.parser.Parse("fixture.go", []byte(s.source))
	if err != nil {
		return fmt.Errorf("Go parse failed: %w", err)
	}
	s.parseResult = result
	return nil
}

// ---------------------------------------------------------------------------
// ClassDecl assertions
// ---------------------------------------------------------------------------

func (s *goParserState) findClass(qn string) *ast.ClassDecl {
	for i := range s.parseResult.Classes {
		if s.parseResult.Classes[i].QualifiedName == qn {
			return &s.parseResult.Classes[i]
		}
	}
	return nil
}

func (s *goParserState) theResultContainsAClassDeclWithQualifiedNameAndKind(qn, kind string) error {
	cls := s.findClass(qn)
	if cls == nil {
		names := make([]string, 0, len(s.parseResult.Classes))
		for _, c := range s.parseResult.Classes {
			names = append(names, c.QualifiedName)
		}
		return fmt.Errorf("ClassDecl %q not found; got %v", qn, names)
	}
	if cls.Kind != kind {
		return fmt.Errorf("ClassDecl %q Kind: want %q, got %q", qn, kind, cls.Kind)
	}
	return nil
}

func (s *goParserState) theClassDeclLangMetaEmbedsListContains(qn, want string) error {
	cls := s.findClass(qn)
	if cls == nil {
		return fmt.Errorf("ClassDecl %q not found", qn)
	}
	if cls.LangMeta == nil {
		return fmt.Errorf("ClassDecl %q LangMeta is nil; expected embeds containing %q", qn, want)
	}
	raw, ok := cls.LangMeta["embeds"]
	if !ok {
		return fmt.Errorf("ClassDecl %q LangMeta has no embeds key; got %+v", qn, cls.LangMeta)
	}
	embeds, ok := raw.([]string)
	if !ok {
		return fmt.Errorf("ClassDecl %q LangMeta embeds is not []string; got %T %+v", qn, raw, raw)
	}
	for _, e := range embeds {
		if e == want {
			return nil
		}
	}
	return fmt.Errorf("ClassDecl %q embeds %v does not contain %q", qn, embeds, want)
}

// ---------------------------------------------------------------------------
// MethodDecl assertions
// ---------------------------------------------------------------------------

func (s *goParserState) findMethod(qn string) *ast.MethodDecl {
	for i := range s.parseResult.Methods {
		if s.parseResult.Methods[i].QualifiedName == qn {
			return &s.parseResult.Methods[i]
		}
	}
	return nil
}

func (s *goParserState) theResultContainsAMethodDeclWithQualifiedNameAndEnclosingClassGo(qn, enclosing string) error {
	m := s.findMethod(qn)
	if m == nil {
		names := make([]string, 0, len(s.parseResult.Methods))
		for _, mm := range s.parseResult.Methods {
			names = append(names, mm.QualifiedName)
		}
		return fmt.Errorf("MethodDecl %q not found; got %v", qn, names)
	}
	if m.EnclosingClass != enclosing {
		return fmt.Errorf("MethodDecl %q EnclosingClass: want %q, got %q", qn, enclosing, m.EnclosingClass)
	}
	return nil
}

func (s *goParserState) theMethodDeclReceiverAliasesListContains(qn, want string) error {
	m := s.findMethod(qn)
	if m == nil {
		return fmt.Errorf("MethodDecl %q not found", qn)
	}
	for _, a := range m.ReceiverAliases {
		if a == want {
			return nil
		}
	}
	return fmt.Errorf("MethodDecl %q ReceiverAliases %v does not contain %q", qn, m.ReceiverAliases, want)
}

func (s *goParserState) theMethodDeclLangMetaReceiverTypeIs(qn, want string) error {
	m := s.findMethod(qn)
	if m == nil {
		return fmt.Errorf("MethodDecl %q not found", qn)
	}
	if m.LangMeta == nil {
		return fmt.Errorf("MethodDecl %q LangMeta is nil", qn)
	}
	got, ok := m.LangMeta["receiver_type"].(string)
	if !ok {
		return fmt.Errorf("MethodDecl %q LangMeta receiver_type missing or not string; got %+v", qn, m.LangMeta["receiver_type"])
	}
	if got != want {
		return fmt.Errorf("MethodDecl %q LangMeta receiver_type: want %q, got %q", qn, want, got)
	}
	return nil
}

func (s *goParserState) theMethodDeclLangMetaReceiverPtrIsTrue(qn string) error {
	m := s.findMethod(qn)
	if m == nil {
		return fmt.Errorf("MethodDecl %q not found", qn)
	}
	if m.LangMeta == nil {
		return fmt.Errorf("MethodDecl %q LangMeta is nil", qn)
	}
	got, ok := m.LangMeta["receiver_ptr"].(bool)
	if !ok || !got {
		return fmt.Errorf("MethodDecl %q LangMeta receiver_ptr: want true, got %+v", qn, m.LangMeta["receiver_ptr"])
	}
	return nil
}

// ---------------------------------------------------------------------------
// Import assertions
// ---------------------------------------------------------------------------

func (s *goParserState) findImports(module string) []ast.Import {
	var out []ast.Import
	for _, imp := range s.parseResult.Imports {
		if imp.Module == module {
			out = append(out, imp)
		}
	}
	return out
}

func (s *goParserState) theResultContainsAnImportWithModule(module string) error {
	if len(s.findImports(module)) == 0 {
		names := make([]string, 0, len(s.parseResult.Imports))
		for _, imp := range s.parseResult.Imports {
			names = append(names, imp.Module)
		}
		return fmt.Errorf("no Import with Module %q; got %v", module, names)
	}
	return nil
}

func (s *goParserState) theResultContainsAnImportWithModuleAndAlias(module, alias string) error {
	for _, imp := range s.findImports(module) {
		if imp.Alias == alias {
			return nil
		}
	}
	return fmt.Errorf("no Import with Module=%q Alias=%q; got %+v", module, alias, s.parseResult.Imports)
}

func (s *goParserState) theResultContainsAnImportWithModuleAndDotImportLangMeta(module string) error {
	for _, imp := range s.findImports(module) {
		if imp.LangMeta == nil {
			continue
		}
		if v, ok := imp.LangMeta["dot_import"].(bool); ok && v {
			return nil
		}
	}
	return fmt.Errorf("no Import with Module=%q and dot_import=true; got %+v", module, s.parseResult.Imports)
}

func (s *goParserState) theResultContainsAnImportWithModuleAndBlankImportLangMeta(module string) error {
	for _, imp := range s.findImports(module) {
		if imp.LangMeta == nil {
			continue
		}
		if v, ok := imp.LangMeta["blank_import"].(bool); ok && v {
			return nil
		}
	}
	return fmt.Errorf("no Import with Module=%q and blank_import=true; got %+v", module, s.parseResult.Imports)
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_go_parser_gotreesitterparser_implementation(ctx *godog.ScenarioContext) {
	s := &goParserState{}

	// Scenario 1: Language and Extensions contract
	ctx.Given(`^the Go tree-sitter parser is constructed$`, s.theGoTreeSitterParserIsConstructed)
	ctx.Then(`^the parser Language is "([^"]*)"$`, s.theParserLanguageIsGo)
	ctx.Then(`^the parser Extensions include "([^"]*)"$`, s.theParserExtensionsIncludeGo)

	// Source fixtures (label-disambiguated steps)
	ctx.Given(`^Go source for struct fixture:$`, s.goStructSource)
	ctx.Given(`^Go source for interface fixture:$`, s.goInterfaceSource)
	ctx.Given(`^Go source for pointer-receiver fixture:$`, s.goPointerReceiverSource)
	ctx.Given(`^Go source for type-alias fixture:$`, s.goTypeAliasSource)
	ctx.Given(`^Go source for import fixture:$`, s.goImportSource)

	ctx.When(`^the source is parsed with the Go tree-sitter parser$`, s.theSourceIsParsedWithTheGoTreeSitterParser)

	// ClassDecl assertions
	ctx.Then(`^the result contains a ClassDecl with QualifiedName "([^"]*)" and Kind "([^"]*)"$`, s.theResultContainsAClassDeclWithQualifiedNameAndKind)
	ctx.Then(`^the ClassDecl "([^"]*)" LangMeta embeds list contains "([^"]*)"$`, s.theClassDeclLangMetaEmbedsListContains)

	// MethodDecl assertions
	ctx.Then(`^the result contains a MethodDecl with QualifiedName "([^"]*)" and EnclosingClass "([^"]*)"$`, s.theResultContainsAMethodDeclWithQualifiedNameAndEnclosingClassGo)
	ctx.Then(`^the MethodDecl "([^"]*)" ReceiverAliases list contains "([^"]*)"$`, s.theMethodDeclReceiverAliasesListContains)
	ctx.Then(`^the MethodDecl "([^"]*)" LangMeta receiver_type is "([^"]*)"$`, s.theMethodDeclLangMetaReceiverTypeIs)
	ctx.Then(`^the MethodDecl "([^"]*)" LangMeta receiver_ptr is true$`, s.theMethodDeclLangMetaReceiverPtrIsTrue)

	// Import assertions
	ctx.Then(`^the result contains an Import with Module "([^"]*)"$`, s.theResultContainsAnImportWithModule)
	ctx.Then(`^the result contains an Import with Module "([^"]*)" and Alias "([^"]*)"$`, s.theResultContainsAnImportWithModuleAndAlias)
	ctx.Then(`^the result contains an Import with Module "([^"]*)" and dot_import LangMeta$`, s.theResultContainsAnImportWithModuleAndDotImportLangMeta)
	ctx.Then(`^the result contains an Import with Module "([^"]*)" and blank_import LangMeta$`, s.theResultContainsAnImportWithModuleAndBlankImportLangMeta)
}

func TestE2E_go_parser_gotreesitterparser_implementation(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_go_parser_gotreesitterparser_implementation,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"go_parser_gotreesitterparser_implementation.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
