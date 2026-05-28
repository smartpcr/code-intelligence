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

type csharpParserState struct {
	// Scenario 1: Build under CGO=on
	cgoEnabled    string
	buildExitCode int
	buildOutput   string

	// Scenario 2: Class with same-file interface implements
	ifaceSrc         string
	ifaceParseResult ast.ParseResult
	ifaceFoundClass  *ast.ClassDecl

	// Scenario 3: Class with same-file class extends
	extSrc         string
	extParseResult ast.ParseResult
	extFoundClass  *ast.ClassDecl

	// Scenario 4: Mixed same-file partition
	mixedSrc         string
	mixedParseResult ast.ParseResult
	mixedFoundClass  *ast.ClassDecl
}

// ---------------------------------------------------------------------------
// Scenario 1 — Build under CGO=on
// ---------------------------------------------------------------------------

func (s *csharpParserState) cgoEnabledIsSetTo(val string) error {
	s.cgoEnabled = val
	return nil
}

func (s *csharpParserState) goBuildRunsOnTheAstPackageFromServicesAgentMemory() error {
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

func (s *csharpParserState) theBuildSucceeds() error {
	if s.buildExitCode != 0 {
		return fmt.Errorf("go build failed (exit %d):\n%s", s.buildExitCode, s.buildOutput)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — Class with same-file interface implements
// ---------------------------------------------------------------------------

func (s *csharpParserState) csharpSourceWithInterface(src *godog.DocString) error {
	s.ifaceSrc = strings.TrimSpace(src.Content)
	return nil
}

func (s *csharpParserState) theSourceIsParsedWithTheCSharpTreeSitterParser() error {
	parser := ast.NewTreeSitterCSharpParser()
	result, err := parser.Parse("test.cs", []byte(s.ifaceSrc))
	if err != nil {
		return fmt.Errorf("C# parse failed: %w", err)
	}
	s.ifaceParseResult = result
	return nil
}

func (s *csharpParserState) theResultContainsAClassDeclWithQualifiedName(name string) error {
	for i := range s.ifaceParseResult.Classes {
		if s.ifaceParseResult.Classes[i].QualifiedName == name {
			s.ifaceFoundClass = &s.ifaceParseResult.Classes[i]
			return nil
		}
	}
	names := make([]string, len(s.ifaceParseResult.Classes))
	for i, c := range s.ifaceParseResult.Classes {
		names[i] = c.QualifiedName
	}
	return fmt.Errorf("no ClassDecl with QualifiedName %q found; have %v", name, names)
}

func (s *csharpParserState) theFooClassDeclHasEmptyExtends() error {
	if s.ifaceFoundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	if len(s.ifaceFoundClass.Extends) != 0 {
		return fmt.Errorf("ClassDecl %q Extends is not empty: %v",
			s.ifaceFoundClass.QualifiedName, s.ifaceFoundClass.Extends)
	}
	return nil
}

func (s *csharpParserState) theFooClassDeclImplementsContains(iface string) error {
	if s.ifaceFoundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	for _, impl := range s.ifaceFoundClass.Implements {
		if impl == iface {
			return nil
		}
	}
	return fmt.Errorf("ClassDecl %q Implements does not contain %q; have %v",
		s.ifaceFoundClass.QualifiedName, iface, s.ifaceFoundClass.Implements)
}

func (s *csharpParserState) theFooClassDeclLangMetaBaseRawContains(entry string) error {
	if s.ifaceFoundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	return checkLangMetaBaseRaw(s.ifaceFoundClass, entry)
}

// ---------------------------------------------------------------------------
// Scenario 3 — Class with same-file class extends
// ---------------------------------------------------------------------------

func (s *csharpParserState) csharpSourceWithBaseClass(src *godog.DocString) error {
	s.extSrc = strings.TrimSpace(src.Content)
	return nil
}

func (s *csharpParserState) theExtendsSourceIsParsedWithTheCSharpTreeSitterParser() error {
	parser := ast.NewTreeSitterCSharpParser()
	result, err := parser.Parse("test_ext.cs", []byte(s.extSrc))
	if err != nil {
		return fmt.Errorf("C# parse failed: %w", err)
	}
	s.extParseResult = result
	return nil
}

func (s *csharpParserState) theExtendsResultContainsAClassDeclWithQualifiedName(name string) error {
	for i := range s.extParseResult.Classes {
		if s.extParseResult.Classes[i].QualifiedName == name {
			s.extFoundClass = &s.extParseResult.Classes[i]
			return nil
		}
	}
	names := make([]string, len(s.extParseResult.Classes))
	for i, c := range s.extParseResult.Classes {
		names[i] = c.QualifiedName
	}
	return fmt.Errorf("no ClassDecl with QualifiedName %q found; have %v", name, names)
}

func (s *csharpParserState) theExtendsFooClassDeclExtendsContains(base string) error {
	if s.extFoundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	for _, ext := range s.extFoundClass.Extends {
		if ext == base {
			return nil
		}
	}
	return fmt.Errorf("ClassDecl %q Extends does not contain %q; have %v",
		s.extFoundClass.QualifiedName, base, s.extFoundClass.Extends)
}

func (s *csharpParserState) theExtendsFooClassDeclHasEmptyImplements() error {
	if s.extFoundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	if len(s.extFoundClass.Implements) != 0 {
		return fmt.Errorf("ClassDecl %q Implements is not empty: %v",
			s.extFoundClass.QualifiedName, s.extFoundClass.Implements)
	}
	return nil
}

func (s *csharpParserState) theExtendsFooClassDeclLangMetaBaseRawContains(entry string) error {
	if s.extFoundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	return checkLangMetaBaseRaw(s.extFoundClass, entry)
}

// ---------------------------------------------------------------------------
// Scenario 4 — Mixed same-file partition
// ---------------------------------------------------------------------------

func (s *csharpParserState) csharpSourceWithMixedInheritance(src *godog.DocString) error {
	s.mixedSrc = strings.TrimSpace(src.Content)
	return nil
}

func (s *csharpParserState) theMixedSourceIsParsedWithTheCSharpTreeSitterParser() error {
	parser := ast.NewTreeSitterCSharpParser()
	result, err := parser.Parse("test_mixed.cs", []byte(s.mixedSrc))
	if err != nil {
		return fmt.Errorf("C# parse failed: %w", err)
	}
	s.mixedParseResult = result
	return nil
}

func (s *csharpParserState) theMixedResultContainsAClassDeclWithQualifiedName(name string) error {
	for i := range s.mixedParseResult.Classes {
		if s.mixedParseResult.Classes[i].QualifiedName == name {
			s.mixedFoundClass = &s.mixedParseResult.Classes[i]
			return nil
		}
	}
	names := make([]string, len(s.mixedParseResult.Classes))
	for i, c := range s.mixedParseResult.Classes {
		names[i] = c.QualifiedName
	}
	return fmt.Errorf("no ClassDecl with QualifiedName %q found; have %v", name, names)
}

func (s *csharpParserState) theMixedFooClassDeclExtendsContains(base string) error {
	if s.mixedFoundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	for _, ext := range s.mixedFoundClass.Extends {
		if ext == base {
			return nil
		}
	}
	return fmt.Errorf("ClassDecl %q Extends does not contain %q; have %v",
		s.mixedFoundClass.QualifiedName, base, s.mixedFoundClass.Extends)
}

func (s *csharpParserState) theMixedFooClassDeclImplementsContains(iface string) error {
	if s.mixedFoundClass == nil {
		return fmt.Errorf("no ClassDecl was found in previous step")
	}
	for _, impl := range s.mixedFoundClass.Implements {
		if impl == iface {
			return nil
		}
	}
	return fmt.Errorf("ClassDecl %q Implements does not contain %q; have %v",
		s.mixedFoundClass.QualifiedName, iface, s.mixedFoundClass.Implements)
}

// ---------------------------------------------------------------------------
// Shared LangMeta helper
// ---------------------------------------------------------------------------

func checkLangMetaBaseRaw(cls *ast.ClassDecl, entry string) error {
	if cls.LangMeta == nil {
		return fmt.Errorf("ClassDecl %q LangMeta is nil", cls.QualifiedName)
	}
	brRaw, ok := cls.LangMeta["base_raw"]
	if !ok {
		return fmt.Errorf("ClassDecl %q LangMeta has no base_raw key; keys: %v",
			cls.QualifiedName, langMetaKeys(cls.LangMeta))
	}

	switch br := brRaw.(type) {
	case []string:
		for _, v := range br {
			if v == entry {
				return nil
			}
		}
		return fmt.Errorf("base_raw %v does not contain %q", br, entry)
	case []interface{}:
		for _, v := range br {
			if fmt.Sprintf("%v", v) == entry {
				return nil
			}
		}
		return fmt.Errorf("base_raw %v does not contain %q", br, entry)
	default:
		return fmt.Errorf("base_raw has unexpected type %T: %v", brRaw, brRaw)
	}
}

func langMetaKeys(m map[string]interface{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_csharp_parser_csharptreesitterparser_implementation(ctx *godog.ScenarioContext) {
	s := &csharpParserState{}

	// Scenario 1: Build under CGO=on
	ctx.Given(`^CGO_ENABLED is set to "([^"]*)"$`, s.cgoEnabledIsSetTo)
	ctx.When(`^go build runs on the ast package from services/agent-memory$`, s.goBuildRunsOnTheAstPackageFromServicesAgentMemory)
	ctx.Then(`^the build succeeds$`, s.theBuildSucceeds)

	// Scenario 2: Class with same-file interface implements
	ctx.Given(`^C# source with interface:$`, s.csharpSourceWithInterface)
	ctx.When(`^the source is parsed with the C# tree-sitter parser$`, s.theSourceIsParsedWithTheCSharpTreeSitterParser)
	ctx.Then(`^the result contains a ClassDecl with QualifiedName "([^"]*)"$`, s.theResultContainsAClassDeclWithQualifiedName)
	ctx.Then(`^the Foo ClassDecl has empty Extends$`, s.theFooClassDeclHasEmptyExtends)
	ctx.Then(`^the Foo ClassDecl Implements contains "([^"]*)"$`, s.theFooClassDeclImplementsContains)
	ctx.Then(`^the Foo ClassDecl LangMeta base_raw contains "([^"]*)"$`, s.theFooClassDeclLangMetaBaseRawContains)

	// Scenario 3: Class with same-file class extends
	ctx.Given(`^C# source with base class:$`, s.csharpSourceWithBaseClass)
	ctx.When(`^the extends source is parsed with the C# tree-sitter parser$`, s.theExtendsSourceIsParsedWithTheCSharpTreeSitterParser)
	ctx.Then(`^the extends result contains a ClassDecl with QualifiedName "([^"]*)"$`, s.theExtendsResultContainsAClassDeclWithQualifiedName)
	ctx.Then(`^the extends Foo ClassDecl Extends contains "([^"]*)"$`, s.theExtendsFooClassDeclExtendsContains)
	ctx.Then(`^the extends Foo ClassDecl has empty Implements$`, s.theExtendsFooClassDeclHasEmptyImplements)
	ctx.Then(`^the extends Foo ClassDecl LangMeta base_raw contains "([^"]*)"$`, s.theExtendsFooClassDeclLangMetaBaseRawContains)

	// Scenario 4: Mixed same-file partition
	ctx.Given(`^C# source with mixed inheritance:$`, s.csharpSourceWithMixedInheritance)
	ctx.When(`^the mixed source is parsed with the C# tree-sitter parser$`, s.theMixedSourceIsParsedWithTheCSharpTreeSitterParser)
	ctx.Then(`^the mixed result contains a ClassDecl with QualifiedName "([^"]*)"$`, s.theMixedResultContainsAClassDeclWithQualifiedName)
	ctx.Then(`^the mixed Foo ClassDecl Extends contains "([^"]*)"$`, s.theMixedFooClassDeclExtendsContains)
	ctx.Then(`^the mixed Foo ClassDecl Implements contains "([^"]*)"$`, s.theMixedFooClassDeclImplementsContains)
}

func TestE2E_csharp_parser_csharptreesitterparser_implementation(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_csharp_parser_csharptreesitterparser_implementation,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"csharp_parser_csharptreesitterparser_implementation.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}