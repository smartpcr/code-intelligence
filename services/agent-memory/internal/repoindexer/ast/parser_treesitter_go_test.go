//go:build cgo

package ast

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
)

// TestGoTreeSitterFixture_EmitsExpectedNodeAndEdgeSet pins
// the Stage 3.2 acceptance requirement for the Go parser:
// "Add a fixture-driven parser test that asserts a known Go
// file produces the expected Class + Method + Import set."
// The fixture is a small, deliberately-canonical Go file
// that exercises every grammar surface called out in the
// workstream brief:
//
//   - one struct with an embedded field (so `embeds`
//     LangMeta is exercised)
//   - one interface with a method spec
//   - one type alias (`type Celsius float64`)
//   - one free function
//   - one pointer-receiver method calling a value-receiver
//     method on the same receiver via `r.X()` (so
//     `ReceiverCalls` and the `ReceiverAliases` pointer-
//     marker contract are exercised)
//   - one value-receiver method calling a free function
//     (so `Calls` is exercised)
//   - one assignment to a receiver field (so `MemberAccesses`
//     classifies it as a write)
//   - one grouped import block with one aliased entry and
//     one blank import (so per-spec line + LangMeta
//     dot/blank flags are exercised)
//   - one single-line import "fmt" (so non-grouped path
//     works too).
func TestGoTreeSitterFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	const src = `package hello

import "fmt"

import (
	"strings"
	io "io"
	_ "embed"
)

type Stringer interface {
	String() string
}

type Celsius float64

type Greeter struct {
	*Base
	prefix string
}

type Base struct {
	id int
}

func formatGreeting(prefix, name string) string {
	return fmt.Sprintf("%s %s", strings.TrimSpace(prefix), name)
}

func (g Greeter) greet(name string) string {
	return formatGreeting(g.prefix, name)
}

func (g *Greeter) rename(p string) {
	g.prefix = p
	_ = io.EOF
	g.greet(p)
}
`
	parser := NewTreeSitterGoParser()
	if parser.Language() != "go" {
		t.Fatalf("Language() = %q; want %q", parser.Language(), "go")
	}
	exts := parser.Extensions()
	if len(exts) != 1 || exts[0] != ".go" {
		t.Fatalf("Extensions() = %v; want [\".go\"]", exts)
	}

	res, err := parser.Parse("pkg/hello.go", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// ----- Classes -----
	classByName := map[string]ClassDecl{}
	for _, c := range res.Classes {
		classByName[c.QualifiedName] = c
	}
	for _, want := range []string{"Stringer", "Celsius", "Greeter", "Base"} {
		if _, ok := classByName[want]; !ok {
			t.Errorf("class %q missing from emitted set; got %v", want, classNames(res.Classes))
		}
	}
	if got := classByName["Stringer"].Kind; got != "interface" {
		t.Errorf("Stringer.Kind = %q; want interface", got)
	}
	if got := classByName["Celsius"].Kind; got != "type_alias" {
		t.Errorf("Celsius.Kind = %q; want type_alias", got)
	}
	if got := classByName["Greeter"].Kind; got != "struct" {
		t.Errorf("Greeter.Kind = %q; want struct", got)
	}
	if got := classByName["Base"].Kind; got != "struct" {
		t.Errorf("Base.Kind = %q; want struct", got)
	}

	// Greeter embeds *Base -- check LangMeta["embeds"].
	greeter := classByName["Greeter"]
	embeds, ok := greeter.LangMeta["embeds"].([]string)
	if !ok {
		t.Fatalf("Greeter.LangMeta[embeds] missing or wrong type; got %#v", greeter.LangMeta)
	}
	if len(embeds) != 1 || embeds[0] != "*Base" {
		t.Errorf("Greeter embeds = %v; want [*Base]", embeds)
	}
	// Base has only a named field, no embeds -- LangMeta
	// should be nil (per "no per-language attrs" contract).
	if got := classByName["Base"].LangMeta; got != nil {
		t.Errorf("Base.LangMeta = %v; want nil (only embedded fields populate it)", got)
	}

	// ----- Methods -----
	methodByName := map[string]MethodDecl{}
	for _, m := range res.Methods {
		methodByName[m.QualifiedName] = m
	}
	for _, want := range []string{"formatGreeting", "Greeter.greet", "*Greeter.rename", "Stringer.String"} {
		if _, ok := methodByName[want]; !ok {
			t.Errorf("method %q missing; got %v", want, methodNames(res.Methods))
		}
	}

	// formatGreeting -- free function.
	fmtGreet := methodByName["formatGreeting"]
	if fmtGreet.EnclosingClass != "" {
		t.Errorf("formatGreeting.EnclosingClass = %q; want empty", fmtGreet.EnclosingClass)
	}
	if !containsString(fmtGreet.Calls, "Sprintf") && !containsString(fmtGreet.Calls, "TrimSpace") {
		// The free function calls `fmt.Sprintf(...)` and
		// `strings.TrimSpace(...)` -- both are selector
		// calls (pkg.Func), which the walker correctly
		// drops. Bare-call set should be empty.
		t.Errorf("formatGreeting.Calls leaked selector calls: got %v", fmtGreet.Calls)
	}
	if len(fmtGreet.Calls) != 0 {
		t.Errorf("formatGreeting.Calls = %v; want [] (all callees are package-qualified)", fmtGreet.Calls)
	}

	// Greeter.greet -- value-receiver method calling free function.
	greet := methodByName["Greeter.greet"]
	if greet.EnclosingClass != "Greeter" {
		t.Errorf("Greeter.greet.EnclosingClass = %q; want Greeter", greet.EnclosingClass)
	}
	if greet.LangMeta["receiver"] != "g" {
		t.Errorf("Greeter.greet.LangMeta[receiver] = %v; want g", greet.LangMeta["receiver"])
	}
	if got, ok := greet.LangMeta["receiver_ptr"]; ok && got != false {
		t.Errorf("Greeter.greet.LangMeta[receiver_ptr] = %v; want absent/false", got)
	}
	if !containsString(greet.Calls, "formatGreeting") {
		t.Errorf("Greeter.greet.Calls missing formatGreeting; got %v", greet.Calls)
	}
	// `g.prefix` access (read) should appear in MemberAccesses.
	if !hasMemberAccess(greet.MemberAccesses, "prefix", false) {
		t.Errorf("Greeter.greet should record read access to prefix; got %v", greet.MemberAccesses)
	}

	// *Greeter.rename -- pointer-receiver method.
	rename := methodByName["*Greeter.rename"]
	if rename.EnclosingClass != "Greeter" {
		t.Errorf("*Greeter.rename.EnclosingClass = %q; want Greeter (bare)", rename.EnclosingClass)
	}
	if rename.LangMeta["receiver_ptr"] != true {
		t.Errorf("*Greeter.rename.LangMeta[receiver_ptr] = %v; want true", rename.LangMeta["receiver_ptr"])
	}
	if rename.LangMeta["receiver_type"] != "Greeter" {
		t.Errorf("*Greeter.rename.LangMeta[receiver_type] = %v; want Greeter", rename.LangMeta["receiver_type"])
	}
	// ReceiverAliases must contain "Greeter.rename" so the
	// dispatcher Pass 2b can register the Node id under
	// the bare-class alias key.
	if len(rename.ReceiverAliases) != 1 || rename.ReceiverAliases[0] != "Greeter.rename" {
		t.Errorf("*Greeter.rename.ReceiverAliases = %v; want [Greeter.rename]", rename.ReceiverAliases)
	}
	if !containsString(rename.ReceiverCalls, "greet") {
		t.Errorf("*Greeter.rename.ReceiverCalls missing greet; got %v", rename.ReceiverCalls)
	}
	// g.prefix on LHS of assignment -- write.
	if !hasMemberAccess(rename.MemberAccesses, "prefix", true) {
		t.Errorf("*Greeter.rename should record WRITE access to prefix; got %v", rename.MemberAccesses)
	}

	// Stringer.String -- interface method spec, no body.
	stringerMethod := methodByName["Stringer.String"]
	if stringerMethod.EnclosingClass != "Stringer" {
		t.Errorf("Stringer.String.EnclosingClass = %q; want Stringer", stringerMethod.EnclosingClass)
	}
	if stringerMethod.BodySource != "" {
		t.Errorf("Stringer.String should have empty body; got %q", stringerMethod.BodySource)
	}

	// ----- Imports -----
	importByMod := map[string]Import{}
	for _, i := range res.Imports {
		importByMod[i.Module] = i
	}
	for _, want := range []string{"fmt", "strings", "io", "embed"} {
		if _, ok := importByMod[want]; !ok {
			t.Errorf("import %q missing; got %v", want, importModules(res.Imports))
		}
	}
	if importByMod["io"].Alias != "io" {
		// Tree-sitter records `io "io"` as an alias even
		// though the alias matches the path. The parser
		// records whatever the grammar surfaces; consumers
		// can detect identity-aliases downstream if
		// meaningful.
		t.Errorf("io import alias = %q; want io", importByMod["io"].Alias)
	}
	if got, ok := importByMod["embed"].LangMeta["blank_import"]; !ok || got != true {
		t.Errorf("embed import LangMeta[blank_import] = %v; want true", got)
	}
	// Lines: `import "fmt"` on its own line (line 3) and
	// the grouped block from line 5; verify per-spec lines
	// are correctly distinguished (grouped imports must
	// NOT collapse onto the `import (` line).
	if importByMod["fmt"].Line != 3 {
		t.Errorf("fmt import line = %d; want 3", importByMod["fmt"].Line)
	}
	if got := importByMod["strings"].Line; got <= 5 || got >= 9 {
		t.Errorf("strings import line = %d; want between 6 and 8 (per-spec)", got)
	}
}

// TestGoTreeSitterParser_HandlesEmptyFile guards against the
// parser crashing on degenerate input.
func TestGoTreeSitterParser_HandlesEmptyFile(t *testing.T) {
	parser := NewTreeSitterGoParser()
	res, err := parser.Parse("empty.go", []byte(""))
	if err != nil {
		t.Fatalf("Parse empty: %v", err)
	}
	if len(res.Classes) != 0 || len(res.Methods) != 0 || len(res.Imports) != 0 {
		t.Fatalf("expected empty parse result; got %+v", res)
	}
}

// TestGoTreeSitterParser_PointerReceiverCollision exercises
// the same-name value vs pointer receiver case the Pass 2b
// multimap is designed for: both methods share the simple
// name `Bar` but their QualifiedNames differ (`Foo.Bar` vs
// `*Foo.Bar`). The pointer method's ReceiverAliases must
// include `Foo.Bar` so calls `r.Bar()` from a third sibling
// can resolve.
func TestGoTreeSitterParser_PointerReceiverCollision(t *testing.T) {
	const src = `package x

type Foo struct{}

func (f Foo) Bar() {}

func (f *Foo) Bar() {}
`
	parser := NewTreeSitterGoParser()
	res, err := parser.Parse("x.go", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var ptrMethod, valueMethod *MethodDecl
	for i := range res.Methods {
		m := &res.Methods[i]
		switch m.QualifiedName {
		case "*Foo.Bar":
			ptrMethod = m
		case "Foo.Bar":
			valueMethod = m
		}
	}
	if valueMethod == nil {
		t.Fatalf("value-receiver Foo.Bar not emitted; got %v", methodNames(res.Methods))
	}
	if ptrMethod == nil {
		t.Fatalf("pointer-receiver *Foo.Bar not emitted; got %v", methodNames(res.Methods))
	}
	if valueMethod.EnclosingClass != "Foo" {
		t.Errorf("value Foo.Bar.EnclosingClass = %q; want Foo", valueMethod.EnclosingClass)
	}
	if ptrMethod.EnclosingClass != "Foo" {
		t.Errorf("pointer *Foo.Bar.EnclosingClass = %q; want Foo (bare; the * lives on QualifiedName)",
			ptrMethod.EnclosingClass)
	}
	if len(ptrMethod.ReceiverAliases) != 1 || ptrMethod.ReceiverAliases[0] != "Foo.Bar" {
		t.Errorf("pointer *Foo.Bar.ReceiverAliases = %v; want [Foo.Bar]",
			ptrMethod.ReceiverAliases)
	}
	if len(valueMethod.ReceiverAliases) != 0 {
		t.Errorf("value Foo.Bar.ReceiverAliases = %v; want [] (no alias needed; QualifiedName matches multimap key)",
			valueMethod.ReceiverAliases)
	}
}

// TestGoTreeSitterParser_GroupedTypeDeclaration exercises
// `type ( ... )` blocks containing a mix of struct, interface,
// and alias specs.
func TestGoTreeSitterParser_GroupedTypeDeclaration(t *testing.T) {
	const src = `package x

type (
	A struct{ x int }
	B interface{ Foo() }
	C = int
	D float64
)
`
	parser := NewTreeSitterGoParser()
	res, err := parser.Parse("x.go", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	kinds := map[string]string{}
	for _, c := range res.Classes {
		kinds[c.QualifiedName] = c.Kind
	}
	wantKinds := map[string]string{
		"A": "struct",
		"B": "interface",
		"C": "type_alias",
		"D": "type_alias",
	}
	for name, want := range wantKinds {
		if got, ok := kinds[name]; !ok {
			t.Errorf("type %q missing from grouped declaration; got %v", name, kinds)
		} else if got != want {
			t.Errorf("type %q kind = %q; want %q", name, got, want)
		}
	}
	// Interface method should be emitted too.
	found := false
	for _, m := range res.Methods {
		if m.QualifiedName == "B.Foo" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("interface method B.Foo missing; got %v", methodNames(res.Methods))
	}
}

// --- helpers ---

func classNames(cs []ClassDecl) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.QualifiedName)
	}
	return out
}

// TestDispatcher_RoutesGoExtensionThroughDefaultParsers pins
// evaluator finding #4 from iter 1: a `.go` file must flow
// through the SAME defaultParsers() registration path that
// production wiring uses, not just exercise the parser in
// isolation. The test
//
//  1. builds a Dispatcher with the default parser set (which,
//     under //go:build cgo, comes from parsers_cgo.go and
//     includes NewTreeSitterGoParser),
//  2. asserts the dispatcher's internal extension table maps
//     ".go" to a parser whose Language() == "go", proving the
//     parsers_cgo.go registration entry actually reaches the
//     dispatcher's routing table, and
//  3. calls EmitFile with a synthetic Go source via an
//     EmitFileEvent whose Open() returns the source, exercising
//     the production EmitFile -> pickParser -> safeParse path
//     end-to-end so a future regression in extension lower-
//     casing, lookup order, or panic recovery is caught here.
//
// The test deliberately uses a no-op nodeEdgeWriter sentinel
// (`struct{}{}`) because the v1 Dispatcher.EmitFile does not
// write nodes/edges yet -- that pipeline ships with the Stage
// 3.2 dispatcher-landing workstream. The acceptance gate this
// test pins is ROUTING, not emission.
func TestDispatcher_RoutesGoExtensionThroughDefaultParsers(t *testing.T) {
	d := NewDispatcher(struct{}{})

	parsers := d.dispatcherParsersForTest()
	p, ok := parsers[".go"]
	if !ok {
		t.Fatalf("dispatcher has no parser registered for .go; registered extensions: %v", keysOf(parsers))
	}
	if got := p.Language(); got != "go" {
		t.Errorf("dispatcher .go parser Language() = %q, want %q", got, "go")
	}

	const src = `package routing

func Greet() string { return "hi" }
`
	ev := repoindexer.EmitFileEvent{
		RelPath: "routing/hello.go",
		Open: func() (repoindexer.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(src)), nil
		},
	}
	res, err := d.EmitFile(context.Background(), ev)
	if err != nil {
		t.Fatalf("dispatcher.EmitFile for .go returned error: %v", err)
	}
	if got := len(res.TouchedNodes); got != 0 {
		t.Errorf("v1 dispatcher should return empty TouchedNodes (Stage 3.2 pipeline lands the emission); got %d", got)
	}
}

// TestDispatcher_SkipsUnknownExtension pins the no-CGO and
// unrecognized-extension contract documented on
// .claude/context/tests.md: when the dispatcher cannot find
// a parser for a file's extension and no language hint
// matches, it returns (EmitResult{}, nil) without error and
// without panicking. Combined with the dispatcher's debug-
// level `ast.dispatch.skip{reason="no_parser"}` log entry
// (asserted via the structured-log handler test in the Stage
// 3.2 landing workstream), this is the SAME skip path that
// fires for CGO-only languages when the no-CGO build runs --
// the dispatcher does NOT mint a separate skip reason for the
// CGO-unavailable case; both `unknown extension` and `no
// parser registered under this build tag` collapse onto the
// single `no_parser` slug so docs and routing speak one
// vocabulary.
func TestDispatcher_SkipsUnknownExtension(t *testing.T) {
	d := NewDispatcher(struct{}{})

	ev := repoindexer.EmitFileEvent{
		RelPath: "vendor/styles.css",
		Open: func() (repoindexer.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("body{}")), nil
		},
	}
	res, err := d.EmitFile(context.Background(), ev)
	if err != nil {
		t.Fatalf("dispatcher.EmitFile for unknown ext returned error: %v", err)
	}
	if got := len(res.TouchedNodes); got != 0 {
		t.Errorf("skip path must return empty result; got %d touched nodes", got)
	}
}

func keysOf(m map[string]LanguageParser) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func methodNames(ms []MethodDecl) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.QualifiedName)
	}
	return out
}

func importModules(is []Import) []string {
	out := make([]string, 0, len(is))
	for _, i := range is {
		out = append(out, i.Module)
	}
	return out
}

func containsString(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

func hasMemberAccess(accesses []MemberAccess, name string, wantWrite bool) bool {
	for _, a := range accesses {
		if a.Name == name && a.IsWrite == wantWrite {
			return true
		}
	}
	return false
}
