//go:build e2e

package e2e

import (
	"context"
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
// Shared helpers (one copy per package — each stage file carries its own
// so go test compiles even when sibling stage files are absent).
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
	// filepath.Dir → .../test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT
	// i=0           → .../test/e2e
	// i=1           → .../test
	// i=2           → .../services/agent-memory  (module root)
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

type normalizeHintsState struct {
	// Scenarios 1 & 2: normalizeHints verification via unit-test runner
	hintInput     string
	expectedAlias string
	testOutput    string
	testExitCode  int

	// Scenario 3: extension precedence — uses ast.Dispatcher directly
	fileExt          string
	hintLang         string
	emittedNodes     []ast.Node
	routedToLanguage string
}

// ---------------------------------------------------------------------------
// runSubtest shells out to `go test` and runs a specific subtest of
// TestNormalizeHints_AliasExpansion. The subtest names follow the
// pattern "{alias} -> {canonical}" (e.g. "golang -> go").
// ---------------------------------------------------------------------------

func (s *normalizeHintsState) runNormalizeHintsSubtest(subtestPattern string) error {
	modRoot, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("cannot locate module root: %w", err)
	}

	runArg := fmt.Sprintf("TestNormalizeHints_AliasExpansion/%s", subtestPattern)
	cmd := exec.Command("go", "test", "-count=1", "-v", "-run", runArg, "./internal/repoindexer/ast/...")
	cmd.Dir = modRoot
	out, err := cmd.CombinedOutput()
	s.testOutput = string(out)
	if err != nil {
		s.testExitCode = 1
	} else {
		s.testExitCode = 0
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 1 — normalizeHints resolves new aliases
// ---------------------------------------------------------------------------

func (s *normalizeHintsState) languageHintsEqualTo(hint string) error {
	// Strip surrounding brackets and quotes: ["golang"] → golang
	hint = strings.Trim(hint, "[] \"")
	s.hintInput = hint
	return nil
}

func (s *normalizeHintsState) normalizeHintsRuns() error {
	// The subtest name in dispatcher_test.go is e.g. "golang_->_go"
	// but -run accepts a regex — matching on the alias prefix is enough.
	return s.runNormalizeHintsSubtest(s.hintInput)
}

func (s *normalizeHintsState) theResultContains(expected string) error {
	s.expectedAlias = strings.Trim(expected, "\"")
	if s.testExitCode != 0 {
		return fmt.Errorf(
			"normalizeHints unit test for %q failed (exit %d):\n%s",
			s.hintInput, s.testExitCode, s.testOutput,
		)
	}
	// Guard against false-pass: `go test -run NoMatch` exits 0 with
	// "no tests to run" — that is not a real PASS.
	if strings.Contains(s.testOutput, "no tests to run") {
		return fmt.Errorf(
			"normalizeHints subtest for %q did not match any test (no tests to run):\n%s",
			s.hintInput, s.testOutput,
		)
	}
	// Verify the specific subtest PASS line appeared.
	if !strings.Contains(s.testOutput, "--- PASS: TestNormalizeHints_AliasExpansion/") {
		return fmt.Errorf(
			"normalizeHints subtest for %q did not produce --- PASS line:\n%s",
			s.hintInput, s.testOutput,
		)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — Existing aliases preserved
// ---------------------------------------------------------------------------

func (s *normalizeHintsState) theResultStillContains(expected string) error {
	// Same verification as Scenario 1 — the unit test asserts the value.
	return s.theResultContains(expected)
}

// ---------------------------------------------------------------------------
// Scenario 3 — Extension precedence over hint
//
// Directly tests the exact acceptance scenario: creates stub C and C++
// parsers, registers both with ast.NewDispatcher, applies
// ast.WithLanguageHints(["cpp"]) to set a conflicting hint, sends a
// ".h" file, and verifies the C parser handled it — proving that
// extension routing wins even when LanguageHints explicitly names
// a different language.
//
// ast.WithLanguageHints is exported by the impl branch (the
// normalizeHints-alias-expansion impl dependency, status: complete).
// ---------------------------------------------------------------------------

// stubLangParser is a minimal ast.Parser that records which language
// handled a parse call via a distinctive class name.
type stubLangParser struct {
	lang string
	exts []string
}

func (p *stubLangParser) Parse(_ string, _ []byte) (ast.ParseResult, error) {
	return ast.ParseResult{
		Classes: []ast.ClassDecl{{Name: p.lang + "_sentinel_class"}},
	}, nil
}

func (p *stubLangParser) Language() string     { return p.lang }
func (p *stubLangParser) Extensions() []string { return p.exts }

// nodeCapturingWriter is a minimal ast.Writer that captures emitted nodes.
type nodeCapturingWriter struct {
	nodes []ast.Node
}

func (w *nodeCapturingWriter) InsertNode(n ast.Node) error {
	w.nodes = append(w.nodes, n)
	return nil
}

func (w *nodeCapturingWriter) InsertEdge(_ ast.Edge) error { return nil }

func (s *normalizeHintsState) aFileWithExtensionAndLanguageHints(ext, hint string) error {
	s.fileExt = strings.Trim(ext, "\"")
	s.hintLang = strings.Trim(hint, "[] \"")
	return nil
}

func (s *normalizeHintsState) selectParserRuns() error {
	// Register C parser for .c/.h and C++ parser for .cpp/.cc/.cxx/.hpp.
	cParser := &stubLangParser{lang: "c", exts: []string{".c", ".h"}}
	cppParser := &stubLangParser{lang: "cpp", exts: []string{".cpp", ".cc", ".cxx", ".hpp"}}
	w := &nodeCapturingWriter{}

	// Apply LanguageHints=["cpp"] — the conflicting hint that should NOT
	// override the extension-based routing for .h files.
	d := ast.NewDispatcher(
		ast.WithParser(cParser),
		ast.WithParser(cppParser),
		ast.WithWriter(w),
		ast.WithLanguageHints([]string{s.hintLang}),
	)

	// Dispatch a file using the captured extension (e.g. ".h" → "test.h").
	// Extension .h is registered to the C parser; LanguageHints says "cpp".
	// The dispatcher contract: extension match wins over hint.
	filename := "test" + s.fileExt
	_, err := d.EmitFile(filename, []byte("// stub source"))
	if err != nil {
		return fmt.Errorf("EmitFile(%q) with LanguageHints=[%q] failed: %w",
			filename, s.hintLang, err)
	}
	s.emittedNodes = w.nodes

	// Determine which parser actually handled the file by inspecting
	// the sentinel class name emitted by our stub parsers.
	for _, n := range w.nodes {
		if n.Kind == "class" && strings.HasSuffix(n.Name, "_sentinel_class") {
			s.routedToLanguage = strings.TrimSuffix(n.Name, "_sentinel_class")
			break
		}
	}
	return nil
}

func (s *normalizeHintsState) theReturnedParserLanguageIs(lang string) error {
	lang = strings.Trim(lang, "\"")
	if s.routedToLanguage == "" {
		return fmt.Errorf(
			"no parser handled file with extension %q and LanguageHints=[%q] — emitted nodes: %+v",
			s.fileExt, s.hintLang, s.emittedNodes)
	}
	if s.routedToLanguage != lang {
		return fmt.Errorf(
			"extension %q with LanguageHints=[%q] routed to parser %q; want %q "+
				"(extension must win over hint)",
			s.fileExt, s.hintLang, s.routedToLanguage, lang)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_normalizehints_alias_expansion(ctx *godog.ScenarioContext) {
	s := &normalizeHintsState{}

	// Reset state before each scenario to prevent leaks between scenarios.
	ctx.Before(func(goCtx context.Context, sc *godog.Scenario) (context.Context, error) {
		*s = normalizeHintsState{}
		return goCtx, nil
	})

	// Scenario 1: normalizeHints resolves new aliases
	ctx.Given(`^LanguageHints equal to (\[.*\])$`, s.languageHintsEqualTo)
	ctx.When(`^normalizeHints runs$`, s.normalizeHintsRuns)
	ctx.Then(`^the result contains "([^"]*)"$`, s.theResultContains)

	// Scenario 2: Existing aliases preserved
	ctx.Then(`^the result still contains "([^"]*)"$`, s.theResultStillContains)

	// Scenario 3: Extension precedence over hint
	ctx.Given(`^a "([^"]*)" file with LanguageHints equal to (\[.*\])$`, s.aFileWithExtensionAndLanguageHints)
	ctx.When(`^selectParser runs$`, s.selectParserRuns)
	ctx.Then(`^the returned parser Language is "([^"]*)"$`, s.theReturnedParserLanguageIs)
}

func TestE2E_shared_additive_surfaces_and_dispatcher_edits_normalizehints_alias_expansion(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_normalizehints_alias_expansion,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"shared_additive_surfaces_and_dispatcher_edits_normalizehints_alias_expansion.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}

// ---------------------------------------------------------------------------
// Migration probe — verifies the compose-backed postgres is reachable
// and the migration journal is intact. Skips when AGENT_MEMORY_PG_URL
// is unset (local dev without compose); CI pipelines always set it.
// ---------------------------------------------------------------------------

func TestE2E_shared_additive_surfaces_and_dispatcher_edits_normalizehints_alias_expansion_migration_probe(t *testing.T) {
	pgURL := requireEnv(t, "AGENT_MEMORY_PG_URL")
	modRoot, err := moduleRoot()
	if err != nil {
		t.Fatalf("cannot locate module root: %v", err)
	}

	cmd := exec.Command("go", "test", "-count=1", "-v",
		"-run", "TestMigrator_Up_AppliesAll|TestMigrations_0022_EdgeKindOverrides",
		"./migrations/...",
	)
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "AGENT_MEMORY_PG_URL="+pgURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("migration tests failed (AGENT_MEMORY_PG_URL=%s):\n%s", pgURL, string(out))
	}
	if !strings.Contains(string(out), "PASS") {
		t.Fatalf("migration tests did not produce PASS output:\n%s", string(out))
	}
}