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
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// ---------------------------------------------------------------------------
// Shared helpers (one copy per package)
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		t.Skipf("required env var %s is not set — skipping", name)
	}
	return v
}

func moduleRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
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

type cppParserState struct {
	// Scenario 1: Build under CGO=on
	cgoEnabled    string
	buildExitCode int
	buildOutput   string

	// Scenario 2: Class + base + in-class method
	source      string
	parseResult ast.ParseResult
	foundClass  *ast.ClassDecl

	// Scenario 3: In-class declaration + out-of-line definition dedupe
	dedupeSource      string
	dedupeParseResult ast.ParseResult
	dedupeMethod      *ast.MethodDecl
}

// ---------------------------------------------------------------------------
// Scenario 1 — Build under CGO=on
// ---------------------------------------------------------------------------

func (s *cppParserState) cgoEnabledIsSetTo(val string) error {
	s.cgoEnabled = val
	return nil
}

func (s *cppParserState) goBuildRunsOnTheAstPackageFromServicesAgentMemory() error {
	modRoot, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("cannot locate module root: %w", err)
	}
	cmd := exec.Command("go", "build", "./internal/repoindexer/ast/...")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+s.cgoEnabled)
	out, err := cmd.CombinedOutput()
	s.buildOutput = string(out)
	if err != nil {
		s.buildExitCode = 1
		return nil
	}
	s.buildExitCode = 0
	return nil
}

func (s *cppParserState) theBuildSucceeds() error {
	if s.buildExitCode != 0 {
		return fmt.Errorf("go build failed (exit %d):\n%s", s.buildExitCode, s.buildOutput)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — Class + base + in-class method
// ---------------------------------------------------------------------------

func (s *cppParserState) cppSource(src *godog.DocString) error {
	s.source = strings.TrimSpace(src.Content)
	return nil
}

func (s *cppParserState) theSourceIsParsedWithTheCppTreeSitterParser() error {
	parser := ast.NewTreeSitterCppParser()
	result, err := parser.Parse("test.cpp", []byte(s.source))
	if err != nil {
		return fmt.Errorf("C++ parse failed: %w", err)
	}
	s.parseResult = result
	return nil
}

func (s *cppParserState) theResultContainsAClassDeclWithQualifiedName(name string) error {
	for i := range s.parseResult.Classes {
		if s.parseResult.Classes[i].QualifiedName == name {
			s.foundClass = &s.parseResult.Classes[i]
			return nil
		}
	}
	names := make([]string, len(s.parseResult.Classes))
	for i, c := range s.parseResult.Classes {
		names[i] = c.QualifiedName
	}
	return fmt.Errorf("no ClassDecl with QualifiedName %q found; have %v", name, names)
}

func (s *cppParserState) theClassDeclExtendsListContains(base string) error {
	if s.foundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	for _, ext := range s.foundClass.Extends {
		if ext == base {
			return nil
		}
	}
	return fmt.Errorf("ClassDecl %q Extends does not contain %q; have %v",
		s.foundClass.QualifiedName, base, s.foundClass.Extends)
}

func (s *cppParserState) theClassDeclLangMetaBaseAccessMapsTo(baseName, access string) error {
	if s.foundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	if s.foundClass.LangMeta == nil {
		return fmt.Errorf("ClassDecl %q LangMeta is nil", s.foundClass.QualifiedName)
	}
	baRaw, ok := s.foundClass.LangMeta["base_access"]
	if !ok {
		return fmt.Errorf("ClassDecl %q LangMeta has no base_access key; keys: %v",
			s.foundClass.QualifiedName, s.foundClass.LangMeta)
	}
	ba, ok := baRaw.(map[string]string)
	if !ok {
		return fmt.Errorf("base_access is not map[string]string: %T", baRaw)
	}
	actual, ok := ba[baseName]
	if !ok {
		return fmt.Errorf("base_access has no entry for %q; have %v", baseName, ba)
	}
	if actual != access {
		return fmt.Errorf("base_access[%q] = %q, want %q", baseName, actual, access)
	}
	return nil
}

func (s *cppParserState) theResultContainsAMethodDeclWithQualifiedNameAndEnclosingClass(qname, enclosing string) error {
	for _, m := range s.parseResult.Methods {
		if m.QualifiedName == qname && m.EnclosingClass == enclosing {
			return nil
		}
	}
	names := make([]string, len(s.parseResult.Methods))
	for i, m := range s.parseResult.Methods {
		names[i] = fmt.Sprintf("%s (enclosing=%s)", m.QualifiedName, m.EnclosingClass)
	}
	return fmt.Errorf("no MethodDecl with QualifiedName=%q EnclosingClass=%q; have %v", qname, enclosing, names)
}

// ---------------------------------------------------------------------------
// Scenario 3 — In-class declaration + out-of-line definition dedupe
// ---------------------------------------------------------------------------

func (s *cppParserState) cppSourceForDedupe(src *godog.DocString) error {
	s.dedupeSource = strings.TrimSpace(src.Content)
	return nil
}

func (s *cppParserState) theDedupeSourceIsParsedWithTheCppTreeSitterParser() error {
	parser := ast.NewTreeSitterCppParser()
	result, err := parser.Parse("test_dedupe.cpp", []byte(s.dedupeSource))
	if err != nil {
		return fmt.Errorf("C++ parse failed: %w", err)
	}
	s.dedupeParseResult = result
	return nil
}

func (s *cppParserState) parseResultMethodsContainsExactlyOneEntryWithQualifiedName(qname string) error {
	var matches []ast.MethodDecl
	for _, m := range s.dedupeParseResult.Methods {
		if m.QualifiedName == qname {
			matches = append(matches, m)
		}
	}
	if len(matches) == 0 {
		names := make([]string, len(s.dedupeParseResult.Methods))
		for i, m := range s.dedupeParseResult.Methods {
			names[i] = m.QualifiedName
		}
		return fmt.Errorf("no MethodDecl with QualifiedName %q; have %v", qname, names)
	}
	if len(matches) != 1 {
		return fmt.Errorf("expected exactly 1 MethodDecl with QualifiedName %q, got %d", qname, len(matches))
	}
	s.dedupeMethod = &matches[0]
	return nil
}

func (s *cppParserState) thatMethodEntryHasNonEmptyBodySource() error {
	if s.dedupeMethod == nil {
		return fmt.Errorf("no dedupe method was matched in the previous step")
	}
	if s.dedupeMethod.BodySource == "" {
		return fmt.Errorf("MethodDecl %q has empty BodySource; expected the out-of-line definition body to be captured",
			s.dedupeMethod.QualifiedName)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_c_and_cpp_parsers_cpptreesitterparser_implementation(ctx *godog.ScenarioContext) {
	s := &cppParserState{}

	// Scenario 1: Build under CGO=on
	ctx.Given(`^CGO_ENABLED is set to "([^"]*)"$`, s.cgoEnabledIsSetTo)
	ctx.When(`^go build runs on the ast package from services/agent-memory$`, s.goBuildRunsOnTheAstPackageFromServicesAgentMemory)
	ctx.Then(`^the build succeeds$`, s.theBuildSucceeds)

	// Scenario 2: Class + base + in-class method
	ctx.Given(`^C\+\+ source:$`, s.cppSource)
	ctx.When(`^the source is parsed with the C\+\+ tree-sitter parser$`, s.theSourceIsParsedWithTheCppTreeSitterParser)
	ctx.Then(`^the result contains a ClassDecl with QualifiedName "([^"]*)"$`, s.theResultContainsAClassDeclWithQualifiedName)
	ctx.Then(`^the ClassDecl Extends list contains "([^"]*)"$`, s.theClassDeclExtendsListContains)
	ctx.Then(`^the ClassDecl LangMeta base_access maps "([^"]*)" to "([^"]*)"$`, s.theClassDeclLangMetaBaseAccessMapsTo)
	ctx.Then(`^the result contains a MethodDecl with QualifiedName "([^"]*)" and EnclosingClass "([^"]*)"$`, s.theResultContainsAMethodDeclWithQualifiedNameAndEnclosingClass)

	// Scenario 3: In-class declaration + out-of-line definition dedupe
	ctx.Given(`^C\+\+ source for dedupe:$`, s.cppSourceForDedupe)
	ctx.When(`^the dedupe source is parsed with the C\+\+ tree-sitter parser$`, s.theDedupeSourceIsParsedWithTheCppTreeSitterParser)
	ctx.Then(`^ParseResult Methods contains exactly one entry with QualifiedName "([^"]*)"$`, s.parseResultMethodsContainsExactlyOneEntryWithQualifiedName)
	ctx.Then(`^that method entry has non-empty body source$`, s.thatMethodEntryHasNonEmptyBodySource)
}

func TestE2E_c_and_cpp_parsers_cpptreesitterparser_implementation(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_c_and_cpp_parsers_cpptreesitterparser_implementation,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"c_and_cpp_parsers_cpptreesitterparser_implementation.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}