//go:build cgo

package ast

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
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
			t.Errorf("class %q missing from emitted set; got %v", want, goClassNames(res.Classes))
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
	// The free function calls `fmt.Sprintf(...)` and
	// `strings.TrimSpace(...)` -- both are selector
	// calls (pkg.Func), which the walker correctly
	// drops. Bare-call set should be empty.
	if containsString(fmtGreet.Calls, "Sprintf") || containsString(fmtGreet.Calls, "TrimSpace") {
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

// goClassNames returns just the QualifiedName slice for a slice
// of ClassDecls; named with the `go` prefix so it does not
// collide with the sibling C++ test helper of the same shape
// in `parser_treesitter_cpp_test.go` (under //go:build cgo both
// files compile into the same package).
func goClassNames(cs []ClassDecl) []string {
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

// TestGoFixture_EmitsExpectedNodeAndEdgeSet is the dispatcher-
// shape companion to TestGoTreeSitterFixture_EmitsExpectedNodeAndEdgeSet
// (parser-direct) above. Where the parser-direct test pins the
// ParseResult shape the Go tree-sitter walker MUST produce, THIS
// test exercises the full EmitFile pipeline (parser routing →
// Pass 0 imports → Pass 1a classes → Pass 1b methods → Pass 2b
// bare-call resolver → Pass 2c reads/writes) against a minimal
// in-test fake writer and asserts the exact Node/Edge graph the
// workstream brief calls out:
//
//   - 1 class node (Greeter, kind=struct)
//   - 2 method nodes (*Greeter.Greet, formatGreeting)
//   - 3 contains edges (file→Greeter, Greeter→*Greeter.Greet,
//     file→formatGreeting)
//   - 1 static_calls edge (*Greeter.Greet → formatGreeting)
//   - 1 imports edge (file → fmt package node)
//
// Mirrors TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet
// (parser_typescript_test.go) in structure but is gated on
// `//go:build cgo` ONLY (not `cgo && canonical_dispatcher`) so
// the workstream brief's stated validation command
// (`go test ./internal/repoindexer/ast -count=1` with CGO on)
// exercises the dispatcher-emission Node/Edge assertions in a
// single run. The minimal helpers below (`goFakeWriter`,
// `goMakeEvent`, `goAttrString`, `goMustNodeIDForSig`,
// `goItoa`) are deliberately `go`-prefixed so they do NOT
// collide with the C-test-local equivalents (`cFakeWriter`
// etc. in parser_treesitter_c_dispatcher_test.go) nor the
// canonical_dispatcher-gated equivalents in dispatcher_test.go
// (`fakeNodeEdgeWriter`, `makeEvent`, `attrString`, etc.) when
// the `canonical_dispatcher` tag is ALSO set -- all three
// helper-sets coexist in the same package without symbol
// collision.
//
// Endpoints are resolved by canonical signature (NOT just by
// count) so a regression that flips edge direction cannot pass
// vacuously. Negative assertions pin 0 extends / 0 implements /
// 0 overrides edges so a walker that accidentally copied a
// C++ / Rust branch is caught. The Pass 2c `reads` edge that
// the dispatcher emits for the `g.prefix` access in Greet is
// pinned positively (1 reads edge from *Greeter.Greet → Greeter)
// so a regression that drops Pass 2c on Go is caught here too.
func TestGoFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	const src = `package hello

import "fmt"

type Greeter struct {
	prefix string
}

func (g *Greeter) Greet(name string) string {
	return formatGreeting(g.prefix, name)
}

func formatGreeting(prefix, name string) string {
	return fmt.Sprintf("%s %s", prefix, name)
}
`
	fw := newGoFakeWriter()
	d := NewDispatcher(fw)
	if _, err := d.EmitFile(context.Background(), goMakeEvent("src/hello.go", src)); err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	const (
		repoURL = "https://git.example/acme/svc"
		relPath = "src/hello.go"
	)

	// ----- class Nodes -----
	// Pinned: 1 class Node (Greeter, kind=struct).
	classes := fw.nodesOf("class")
	if len(classes) != 1 {
		t.Fatalf("class nodes = %d; want 1 (Greeter); nodes=%+v",
			len(classes), classes)
	}
	if got := goAttrString(t, classes[0].AttrsJSON, "language"); got != "go" {
		t.Errorf("class %s attrs.language = %q; want %q",
			classes[0].CanonicalSignature, got, "go")
	}
	if got := goAttrString(t, classes[0].AttrsJSON, "decl_kind"); got != "struct" {
		t.Errorf("class %s attrs.decl_kind = %q; want %q",
			classes[0].CanonicalSignature, got, "struct")
	}

	// ----- method Nodes -----
	// Pinned: 2 method Nodes (*Greeter.Greet pointer-receiver
	// method, formatGreeting free function).
	methods := fw.nodesOf("method")
	if len(methods) != 2 {
		t.Fatalf("method nodes = %d; want 2 (*Greeter.Greet, formatGreeting); nodes=%+v",
			len(methods), methods)
	}
	for _, m := range methods {
		if got := goAttrString(t, m.AttrsJSON, "language"); got != "go" {
			t.Errorf("method %s attrs.language = %q; want %q",
				m.CanonicalSignature, got, "go")
		}
	}

	// ----- package Nodes -----
	// Pinned: 1 package Node (fmt external). "fmt" is not
	// workspace-relative, so isRelativeImportSpecifier MUST
	// return false and the dispatcher MUST mint the package
	// node + imports edge.
	pkgs := fw.nodesOf("package")
	if len(pkgs) != 1 {
		t.Fatalf("package nodes = %d; want 1 (fmt); nodes=%+v",
			len(pkgs), pkgs)
	}
	// Pin the package-node attrs that `packageAttrsJSON` in
	// dispatcher.go actually emits: `module` (the import
	// specifier) and `source="external"` (every Pass 0 module
	// is external by construction). `language` is intentionally
	// NOT asserted -- it is NOT part of packageAttrsJSON's
	// output today.
	if got := goAttrString(t, pkgs[0].AttrsJSON, "module"); got != "fmt" {
		t.Errorf("fmt attrs.module = %q; want %q", got, "fmt")
	}
	if got := goAttrString(t, pkgs[0].AttrsJSON, "source"); got != "external" {
		t.Errorf("fmt attrs.source = %q; want %q", got, "external")
	}

	// ----- node ids by canonical signature -----
	// Signatures are built INLINE here (not via shared
	// classSignature / methodSignature / externalPackageSignature
	// helpers) so this test does not depend on any other
	// test-helper file that may or may not be present in the
	// package. The formats mirror dispatcher.go's `fmt.Sprintf`
	// calls in Pass 0 / Pass 1a / Pass 1b exactly so a
	// regression that changes the canonical signature shape is
	// caught here.
	greeterClassSig := fmt.Sprintf("%s::class::%s#%s", repoURL, relPath, "Greeter")
	// Go parser sets ParamSignature to the parens-stripped
	// parameter list verbatim (see parser_treesitter_go.go
	// trimParens(p.Content(...))): `(name string)` →
	// `"name string"`; `(prefix, name string)` →
	// `"prefix, name string"`.
	greetMethodSig := fmt.Sprintf("%s::method::%s#%s(%s)", repoURL, relPath, "*Greeter.Greet", "name string")
	formatMethodSig := fmt.Sprintf("%s::method::%s#%s(%s)", repoURL, relPath, "formatGreeting", "prefix, name string")
	fmtPkgSig := fmt.Sprintf("%s::package::%s", repoURL, "fmt")

	greeterClassID := goMustNodeIDForSig(t, fw, greeterClassSig)
	greetMethodID := goMustNodeIDForSig(t, fw, greetMethodSig)
	formatMethodID := goMustNodeIDForSig(t, fw, formatMethodSig)
	fmtPkgID := goMustNodeIDForSig(t, fw, fmtPkgSig)

	// ----- contains edges -----
	// Pinned: 3 contains edges. The pointer-receiver method
	// *Greeter.Greet's parent is the Greeter class (NOT the
	// file) because the dispatcher Pass 1b reparents methods
	// with EnclosingClass to their class node. The free
	// function formatGreeting has no EnclosingClass so its
	// parent is the file. Endpoints checked, not just count,
	// so a regression that mis-parents *Greeter.Greet under
	// the file (which would still produce 3 contains edges
	// but with the wrong source on one) is caught.
	contains := fw.edgesOf("contains")
	if len(contains) != 3 {
		t.Fatalf("contains edges = %d; want 3 (file→Greeter, Greeter→*Greeter.Greet, file→formatGreeting); edges=%+v",
			len(contains), contains)
	}
	type containsEdge struct{ src, dst string }
	want := map[containsEdge]bool{
		{src: "file-node-id", dst: greeterClassID}: false,
		{src: greeterClassID, dst: greetMethodID}:  false,
		{src: "file-node-id", dst: formatMethodID}: false,
	}
	for _, e := range contains {
		key := containsEdge{src: e.SrcNodeID, dst: e.DstNodeID}
		seen, expected := want[key]
		if !expected {
			t.Errorf("contains edge %s → %s: unexpected pair", e.SrcNodeID, e.DstNodeID)
			continue
		}
		if seen {
			t.Errorf("contains edge %s → %s: duplicate emission", e.SrcNodeID, e.DstNodeID)
			continue
		}
		want[key] = true
	}
	for k, hit := range want {
		if !hit {
			t.Errorf("contains edge %s → %s: expected but not emitted", k.src, k.dst)
		}
	}

	// ----- static_calls edges -----
	// Pinned: 1 static_calls edge (*Greeter.Greet →
	// formatGreeting). The Go walker captures `formatGreeting`
	// as a bare-identifier call in (*Greeter).Greet.Calls;
	// Pass 2b's simple-name multimap finds exactly one
	// formatGreeting method node and mints the edge.
	// Direction MUST be caller → callee.
	staticCalls := fw.edgesOf("static_calls")
	if len(staticCalls) != 1 {
		t.Fatalf("static_calls edges = %d; want 1 (*Greeter.Greet → formatGreeting); edges=%+v",
			len(staticCalls), staticCalls)
	}
	if staticCalls[0].SrcNodeID != greetMethodID || staticCalls[0].DstNodeID != formatMethodID {
		t.Errorf("static_calls edge = %s → %s; want %s → %s (*Greeter.Greet → formatGreeting)",
			staticCalls[0].SrcNodeID, staticCalls[0].DstNodeID,
			greetMethodID, formatMethodID)
	}

	// ----- imports edges -----
	// Pinned: 1 imports edge (file → fmt). The fixture has
	// exactly one `import "fmt"` statement, and "fmt" is not
	// workspace-relative, so isRelativeImportSpecifier returns
	// false and the dispatcher mints the edge.
	imports := fw.edgesOf("imports")
	if len(imports) != 1 {
		t.Fatalf("imports edges = %d; want 1 (file → fmt); edges=%+v",
			len(imports), imports)
	}
	if imports[0].SrcNodeID != "file-node-id" || imports[0].DstNodeID != fmtPkgID {
		t.Errorf("imports edge = %s → %s; want %s → %s (file → fmt)",
			imports[0].SrcNodeID, imports[0].DstNodeID,
			"file-node-id", fmtPkgID)
	}

	// ----- reads edges (Pass 2c) -----
	// The Go walker records `g.prefix` (a read of the
	// receiver field) in *Greeter.Greet.MemberAccesses with
	// IsWrite=false. Pass 2c aggregates per (method, isWrite)
	// pair into ONE edge: the dispatcher emits exactly one
	// `reads` edge from *Greeter.Greet to its enclosing
	// Greeter class. This positive assertion guards against a
	// regression that drops Pass 2c on Go.
	reads := fw.edgesOf("reads")
	if len(reads) != 1 {
		t.Errorf("reads edges = %d; want 1 (*Greeter.Greet → Greeter for g.prefix); edges=%+v",
			len(reads), reads)
	} else if reads[0].SrcNodeID != greetMethodID || reads[0].DstNodeID != greeterClassID {
		t.Errorf("reads edge = %s → %s; want %s → %s (*Greeter.Greet → Greeter)",
			reads[0].SrcNodeID, reads[0].DstNodeID,
			greetMethodID, greeterClassID)
	}

	// ----- negative edges -----
	// The Go fixture has no embedding / no interface
	// implementation / no trait override, so the dispatcher
	// MUST NOT mint extends / implements / overrides edges.
	// Greet writes nothing to g.* (it only reads g.prefix),
	// so the writes edge count MUST be 0 as well.
	if got := len(fw.edgesOf("extends")); got != 0 {
		t.Errorf("extends edges = %d; want 0 (no embedding in fixture)", got)
	}
	if got := len(fw.edgesOf("implements")); got != 0 {
		t.Errorf("implements edges = %d; want 0 (no interface impl in fixture)", got)
	}
	if got := len(fw.edgesOf("overrides")); got != 0 {
		t.Errorf("overrides edges = %d; want 0 (no trait override in fixture)", got)
	}
	if got := len(fw.edgesOf("writes")); got != 0 {
		t.Errorf("writes edges = %d; want 0 (Greet only reads g.prefix, no assignment)", got)
	}
}

// ----------------------------------------------------------
// Local test-only helpers for the `//go:build cgo` Go
// dispatcher test. Names are `go`-prefixed so they coexist
// with the C-test-local helpers (`cFakeWriter` etc. in
// parser_treesitter_c_dispatcher_test.go) AND the
// canonical_dispatcher-gated dispatcher_test.go helpers
// (`fakeNodeEdgeWriter`, `makeEvent`, `attrString`,
// `mustNodeIDForSig`, `itoa`) without symbol collision when
// either / both other tag combinations are set.
// ----------------------------------------------------------

// goFakeWriter is a minimal NodeEdgeWriter implementation
// that captures every InsertNode / InsertEdge call so the
// Go-fixture dispatcher test can assert on the exact emitted
// graph without a PostgreSQL connection. Mirrors the
// canonical_dispatcher-gated `fakeNodeEdgeWriter` semantics:
// idempotent inserts by (kind, canonical_signature) so two
// calls with the same signature return the same NodeID
// (matching the real writer's (repo_id, fingerprint) unique
// key behaviour).
type goFakeWriter struct {
	mu      sync.Mutex
	nodes   []graphwriter.NodeInput
	edges   []graphwriter.EdgeInput
	idBySig map[string]string
}

func newGoFakeWriter() *goFakeWriter {
	return &goFakeWriter{idBySig: map[string]string{}}
}

func (f *goFakeWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, dup := f.idBySig[in.CanonicalSignature]; dup {
		fp, _ := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
		return graphwriter.NodeRecord{NodeID: id, Fingerprint: fp, Inserted: false}, nil
	}
	id := "node-" + goItoa(len(f.nodes))
	f.idBySig[in.CanonicalSignature] = id
	f.nodes = append(f.nodes, in)
	fp, _ := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
	return graphwriter.NodeRecord{NodeID: id, Fingerprint: fp, Inserted: true}, nil
}

func (f *goFakeWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edges = append(f.edges, in)
	id := "edge-" + goItoa(len(f.edges)-1)
	return graphwriter.EdgeRecord{EdgeID: id, Inserted: true}, nil
}

func (f *goFakeWriter) nodesOf(kind string) []graphwriter.NodeInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []graphwriter.NodeInput
	for _, n := range f.nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func (f *goFakeWriter) edgesOf(kind string) []graphwriter.EdgeInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []graphwriter.EdgeInput
	for _, e := range f.edges {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// goStringReadCloser wraps a string as a repoindexer.ReadCloser
// for in-memory test EmitFileEvent.Open functions.
type goStringReadCloser struct {
	r *strings.Reader
}

func (s *goStringReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *goStringReadCloser) Close() error               { return nil }

// goMakeEvent constructs an EmitFileEvent backed by an
// in-memory source string. Mirrors `dispatcher_test.go`'s
// `makeEvent` so the canonical signature inputs (RepoURL,
// FileNodeID, SHA) line up with the dispatcher's
// signature-minting code and the test's inline-built
// expected-signature computations.
func goMakeEvent(relPath, src string) repoindexer.EmitFileEvent {
	return repoindexer.EmitFileEvent{
		RepoID:     fingerprint.MustParseRepoID("11111111-2222-3333-4444-555555555555"),
		RepoURL:    "https://git.example/acme/svc",
		SHA:        "shaABC",
		FileNodeID: "file-node-id",
		RelPath:    relPath,
		Open: func() (repoindexer.ReadCloser, error) {
			return &goStringReadCloser{r: strings.NewReader(src)}, nil
		},
	}
}

// goAttrString reads a string-valued key from a JSON-encoded
// attrs blob. Failing assertion via t.Fatalf rather than
// returning ("", err) so callers stay terse.
func goAttrString(t *testing.T, raw json.RawMessage, key string) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("attrs JSON unmarshal: %v (raw=%s)", err, string(raw))
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		t.Fatalf("attrs[%q] is %T; want string", key, v)
	}
	return s
}

// goMustNodeIDForSig returns the fake writer's NodeID for the
// given canonical signature, failing the test if no node
// matches. Used to translate from inline-built canonical
// signatures to the dispatcher-assigned NodeID so edge
// endpoint assertions don't depend on insertion order.
func goMustNodeIDForSig(t *testing.T, fw *goFakeWriter, sig string) string {
	t.Helper()
	fw.mu.Lock()
	defer fw.mu.Unlock()
	id, ok := fw.idBySig[sig]
	if !ok {
		known := make([]string, 0, len(fw.idBySig))
		for k := range fw.idBySig {
			known = append(known, k)
		}
		// Simple insertion sort for stable failure messages
		// (avoids pulling in `sort` just for a test diagnostic).
		for i := 1; i < len(known); i++ {
			for j := i; j > 0 && known[j-1] > known[j]; j-- {
				known[j-1], known[j] = known[j], known[j-1]
			}
		}
		t.Fatalf("no node found with canonical signature %q; known signatures=%v",
			sig, known)
	}
	return id
}

func goItoa(i int) string { return strconv.Itoa(i) }
