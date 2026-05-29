//go:build e2e && cgo

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

type goParserState struct {
	// Scenario 1: Build
	buildExitCode int
	buildOutput   string

	// Scenario 2 & 3: Parse
	goSource    string
	parseResult ast.ParseResult
	parseErr    error
	method      *ast.MethodDecl
}

// ---------------------------------------------------------------------------
// Scenario 1 — Build under CGO=on
// ---------------------------------------------------------------------------

func (s *goParserState) cgoEnabled1() error {
	// CGO_ENABLED=1 is set on the build command in the When step.
	return nil
}

func (s *goParserState) goBuildAstRunsFromModule() error {
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

func (s *goParserState) buildSucceeds() error {
	if s.buildExitCode != 0 {
		return fmt.Errorf("go build failed (exit %d):\n%s", s.buildExitCode, s.buildOutput)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 & 3 — Pointer / value receiver canonical
// ---------------------------------------------------------------------------

func (s *goParserState) aGoSourceStringContaining(snippet string) error {
	s.goSource = "package main\n\ntype Foo struct{}\n\n" + snippet + "\n"
	return nil
}

func (s *goParserState) goTreeSitterParserParseRuns() error {
	parser := ast.NewTreeSitterGoParser()
	result, err := parser.Parse("test.go", []byte(s.goSource))
	s.parseResult = result
	s.parseErr = err
	if err != nil {
		return fmt.Errorf("Parse returned error: %w", err)
	}
	if len(result.Methods) == 0 {
		return fmt.Errorf("Parse returned zero methods")
	}
	s.method = &result.Methods[0]
	return nil
}

func (s *goParserState) qualifiedNameIs(expected string) error {
	if s.method.QualifiedName != expected {
		return fmt.Errorf("QualifiedName = %q, want %q", s.method.QualifiedName, expected)
	}
	return nil
}

func (s *goParserState) enclosingClassIs(expected string) error {
	if s.method.EnclosingClass != expected {
		return fmt.Errorf("EnclosingClass = %q, want %q", s.method.EnclosingClass, expected)
	}
	return nil
}

func (s *goParserState) receiverAliasesEquals(expected string) error {
	// expected comes in as `["Foo.Bar"]` — parse the bracketed list.
	want := parseBracketedList(expected)
	if len(want) != len(s.method.ReceiverAliases) {
		return fmt.Errorf("ReceiverAliases = %v, want %v", s.method.ReceiverAliases, want)
	}
	for i, w := range want {
		if s.method.ReceiverAliases[i] != w {
			return fmt.Errorf("ReceiverAliases[%d] = %q, want %q (full: %v)", i, s.method.ReceiverAliases[i], w, s.method.ReceiverAliases)
		}
	}
	return nil
}

func (s *goParserState) langMetaReceiverPtrIsTrue() error {
	v, ok := s.method.LangMeta["receiver_ptr"]
	if !ok {
		return fmt.Errorf("LangMeta has no receiver_ptr key; keys = %v", langMetaKeys(s.method.LangMeta))
	}
	if b, isBool := v.(bool); !isBool || !b {
		return fmt.Errorf("LangMeta[receiver_ptr] = %v (%T), want true", v, v)
	}
	return nil
}

func (s *goParserState) receiverAliasesIsNil() error {
	if s.method.ReceiverAliases != nil {
		return fmt.Errorf("ReceiverAliases = %v (len %d), want nil", s.method.ReceiverAliases, len(s.method.ReceiverAliases))
	}
	return nil
}

func (s *goParserState) langMetaReceiverPtrIsFalse() error {
	v, ok := s.method.LangMeta["receiver_ptr"]
	if !ok {
		return fmt.Errorf("LangMeta has no receiver_ptr key; keys = %v", langMetaKeys(s.method.LangMeta))
	}
	b, isBool := v.(bool)
	if !isBool {
		return fmt.Errorf("LangMeta[receiver_ptr] = %v (%T), want false (bool)", v, v)
	}
	if b {
		return fmt.Errorf("LangMeta[receiver_ptr] = true, want false")
	}
	return nil
}

func langMetaKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// parseBracketedList parses `["Foo.Bar"]` or `["a", "b"]` into a string slice.
func parseBracketedList(raw string) []string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "[")
	raw = strings.TrimSuffix(raw, "]")
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, `"`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_go_parser_gotreesitterparser_implementation(ctx *godog.ScenarioContext) {
	s := &goParserState{}

	// Scenario 1: Build
	ctx.Given(`^CGO_ENABLED=1$`, s.cgoEnabled1)
	ctx.When(`^go build \./internal/repoindexer/ast/\.\.\. runs from services/agent-memory$`, s.goBuildAstRunsFromModule)
	ctx.Then(`^it succeeds$`, s.buildSucceeds)

	// Scenario 2 & 3: Parse
	ctx.Given(`^a Go source string containing "([^"]*)"$`, s.aGoSourceStringContaining)
	ctx.When(`^goTreeSitterParser\.Parse runs$`, s.goTreeSitterParserParseRuns)
	ctx.Then(`^the returned MethodDecl has QualifiedName "([^"]*)"$`, s.qualifiedNameIs)
	ctx.Then(`^EnclosingClass is "([^"]*)"$`, s.enclosingClassIs)
	ctx.Then(`^ReceiverAliases equals (.+)$`, s.receiverAliasesEquals)
	ctx.Then(`^LangMeta receiver_ptr is true$`, s.langMetaReceiverPtrIsTrue)
	ctx.Then(`^ReceiverAliases is nil$`, s.receiverAliasesIsNil)
	ctx.Then(`^LangMeta receiver_ptr is false$`, s.langMetaReceiverPtrIsFalse)
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