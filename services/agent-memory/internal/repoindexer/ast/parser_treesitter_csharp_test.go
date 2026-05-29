//go:build cgo

package ast

import (
	"sort"
	"strings"
	"testing"
)

// TestTreeSitterCSharpParser_LanguageAndExtensions verifies the
// placeholder parser advertises the canonical C# language id
// and the `.cs` extension from the workstream brief §4
// "Register extensions". The sibling stage workstream
// `stage-4.1-csharptreesitterparser-implementation` will
// replace the stub Parse() with a real walker, but the
// (Language, Extensions) contract MUST stay stable through
// that swap because the dispatcher routes files by extension
// (lower-case) at registration time -- a drift here would
// silently mis-route real `.cs` source files.
func TestTreeSitterCSharpParser_LanguageAndExtensions(t *testing.T) {
	p := NewTreeSitterCSharpParser()
	if got := p.Language(); got != "csharp" {
		t.Fatalf("Language: want csharp, got %q", got)
	}
	want := []string{".cs"}
	got := append([]string(nil), p.Extensions()...)
	sort.Strings(got)
	sortedWant := append([]string(nil), want...)
	sort.Strings(sortedWant)
	if len(got) != len(sortedWant) {
		t.Fatalf("Extensions: want %v, got %v", sortedWant, got)
	}
	for i := range got {
		if got[i] != sortedWant[i] {
			t.Fatalf("Extensions[%d]: want %q, got %q (full want=%v got=%v)",
				i, sortedWant[i], got[i], sortedWant, got)
		}
	}
}

// TestCSharpFixture_EmitsExpectedNodeAndEdgeSet pins the
// Stage 4.3 (C#-fixture-test) acceptance contract for the C#
// tree-sitter parser. The fixture is a small, deliberately
// canonical C# file that exercises every grammar surface the
// workstream brief calls out:
//
//   - one `using System;` directive (so the imports field is
//     exercised and the dispatcher can later materialise the
//     `file -> System` package edge),
//   - one file-scoped `namespace Demo;` (C# 10 syntax),
//   - one interface (`IGreeter`) with a method spec
//     (`Greet`) — so the parser proves it recognises
//     `interface_declaration` and emits the method spec
//     even without a body,
//   - one base class (`Base`) with an expression-bodied
//     method (`Identify`) declared in the same file,
//   - one derived class (`HelloWorld`) that BOTH extends
//     `Base` and implements `IGreeter` — so the parser's
//     `base_list` walk separates the class base from the
//     interface bases and the dispatcher can resolve both
//     `extends` and `implements` edges against same-file
//     declarations,
//   - a regular method (`HelloWorld.Greet`) that issues a
//     bare-name call to a sibling static method
//     (`FormatGreeting`) in the same class — so the
//     parser's Calls list is exercised and the dispatcher
//     can resolve a same-file `static_calls` edge,
//   - a private static method (`FormatGreeting`) — so
//     `static` modifier extraction is exercised, and
//   - a private field (`prefix`) with an initializer — so
//     the parser does NOT misclassify field-with-initializer
//     as a method (the brief pins exactly 4 methods, not 5).
//
// This test runs at the PARSER level (//go:build cgo,
// mirroring TestGoTreeSitterFixture_EmitsExpectedNodeAndEdgeSet
// in parser_treesitter_go_test.go) rather than at the
// dispatcher level (//go:build canonical_dispatcher, as used by
// TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet and the
// Python equivalent). The brief's "edge" terminology maps to
// ParseResult fields the dispatcher consumes:
//
//   - "extends edge"       -> ClassDecl.Extends
//   - "implements edge"    -> ClassDecl.Implements
//   - "static_calls edge"  -> MethodDecl.Calls (bare-name
//     entries that the dispatcher's
//     Pass 2b resolver stitches against
//     the same-file callee index)
//   - "imports edge"       -> ParseResult.Imports (the
//     dispatcher mints the synthetic
//     package node + `imports` edge in
//     Pass 1 / 2a)
//
// Namespace prefixing (`Demo.IGreeter` vs bare `IGreeter`) is
// the parser's design choice and not pinned by the brief, so
// the helpers below match by simple-name / dotted suffix so
// the test stays meaningful regardless of which convention the
// walker adopts when the sibling implementation stage lands.
func TestCSharpFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	const src = `using System;

namespace Demo;

interface IGreeter
{
    string Greet(string name);
}

class Base
{
    public string Identify() => "base";
}

class HelloWorld : Base, IGreeter
{
    public string Greet(string name)
    {
        return FormatGreeting(this.prefix, name);
    }

    private static string FormatGreeting(string prefix, string name) => prefix + name;

    private string prefix = "hi";
}
`
	parser := NewTreeSitterCSharpParser()
	if got := parser.Language(); got != "csharp" {
		t.Fatalf("Language() = %q; want %q", got, "csharp")
	}

	res, err := parser.Parse("src/Demo/HelloWorld.cs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// ----- Classes / Interfaces -----
	// The brief pins exactly 3 class/interface nodes:
	// IGreeter (interface), Base (class), HelloWorld (class).
	for _, want := range []string{"IGreeter", "Base", "HelloWorld"} {
		if _, ok := findCSharpClass(res.Classes, want); !ok {
			t.Errorf("class/interface %q missing from emitted set; got %v",
				want, csharpClassNames(res.Classes))
		}
	}
	if got := len(res.Classes); got != 3 {
		t.Errorf("expected 3 class/interface nodes; got %d (%v)",
			got, csharpClassNames(res.Classes))
	}

	// ----- Methods -----
	// The brief pins exactly 4 method nodes:
	// IGreeter.Greet (interface method spec, no body),
	// Base.Identify, HelloWorld.Greet, HelloWorld.FormatGreeting.
	for _, want := range []string{
		"IGreeter.Greet",
		"Base.Identify",
		"HelloWorld.Greet",
		"HelloWorld.FormatGreeting",
	} {
		if _, ok := findCSharpMethod(res.Methods, want); !ok {
			t.Errorf("method %q missing; got %v",
				want, methodNames(res.Methods))
		}
	}
	if got := len(res.Methods); got != 4 {
		t.Errorf("expected 4 method nodes; got %d (%v)",
			got, methodNames(res.Methods))
	}

	// ----- Extends edge (HelloWorld -> Base) -----
	hello, ok := findCSharpClass(res.Classes, "HelloWorld")
	if !ok {
		t.Fatalf("HelloWorld class missing; got %v", csharpClassNames(res.Classes))
	}
	if !csharpHasSuffix(hello.Extends, "Base") {
		t.Errorf("HelloWorld.Extends should contain Base; got %v", hello.Extends)
	}
	if got := len(hello.Extends); got != 1 {
		t.Errorf("expected exactly 1 extends entry on HelloWorld (-> Base); got %d (%v)",
			got, hello.Extends)
	}

	// ----- Implements edge (HelloWorld -> IGreeter) -----
	if !csharpHasSuffix(hello.Implements, "IGreeter") {
		t.Errorf("HelloWorld.Implements should contain IGreeter; got %v", hello.Implements)
	}
	if got := len(hello.Implements); got != 1 {
		t.Errorf("expected exactly 1 implements entry on HelloWorld (-> IGreeter); got %d (%v)",
			got, hello.Implements)
	}

	// ----- static_calls edge (HelloWorld.Greet -> HelloWorld.FormatGreeting) -----
	greet, ok := findCSharpMethod(res.Methods, "HelloWorld.Greet")
	if !ok {
		t.Fatalf("HelloWorld.Greet method missing; got %v", methodNames(res.Methods))
	}
	// Bare-name call: the parser emits "FormatGreeting" in
	// Calls (the bare invocation source the dispatcher's
	// Pass 2b resolver matches against the same-file callee
	// index). A future parser revision that pre-qualifies
	// the call as `HelloWorld.FormatGreeting` is also
	// accepted, since the dispatcher would resolve either
	// form to the same Node.
	if !csharpHasSuffix(greet.Calls, "FormatGreeting") {
		t.Errorf("HelloWorld.Greet.Calls should contain FormatGreeting; got %v", greet.Calls)
	}

	// ----- Imports edge (file -> System) -----
	if !containsString(importModules(res.Imports), "System") {
		t.Errorf("import %q missing; got %v", "System", importModules(res.Imports))
	}
	if got := len(res.Imports); got != 1 {
		t.Errorf("expected exactly 1 import (System); got %d (%v)",
			got, importModules(res.Imports))
	}
}

// findCSharpClass returns the ClassDecl whose QualifiedName
// equals want OR ends with "." + want (so `IGreeter` matches
// both `IGreeter` and `Demo.IGreeter`). Namespace prefixing is
// the walker's design choice; the brief's assertions are
// written in simple-name form.
func findCSharpClass(classes []ClassDecl, want string) (ClassDecl, bool) {
	for _, c := range classes {
		if c.QualifiedName == want || strings.HasSuffix(c.QualifiedName, "."+want) {
			return c, true
		}
	}
	return ClassDecl{}, false
}

// findCSharpMethod returns the MethodDecl whose QualifiedName
// equals want OR ends with "." + want. `want` is expected to
// be in `Class.Method` form (e.g. `HelloWorld.Greet`); the
// suffix match accepts a namespace-qualified emission such as
// `Demo.HelloWorld.Greet`.
func findCSharpMethod(methods []MethodDecl, want string) (MethodDecl, bool) {
	for _, m := range methods {
		if m.QualifiedName == want || strings.HasSuffix(m.QualifiedName, "."+want) {
			return m, true
		}
	}
	return MethodDecl{}, false
}

// csharpHasSuffix reports whether s contains an element that
// equals want or ends with "." + want. Used to assert
// extends / implements / Calls entries tolerantly of any
// namespace prefix the walker may attach.
func csharpHasSuffix(s []string, want string) bool {
	for _, v := range s {
		if v == want || strings.HasSuffix(v, "."+want) {
			return true
		}
	}
	return false
}

// csharpClassNames returns just the QualifiedName slice for a
// slice of ClassDecls. Named with the `csharp` prefix so it
// does not collide with sibling helpers under //go:build cgo
// (parser_treesitter_cpp_test.go::classNames,
// parser_treesitter_go_test.go::goClassNames).
func csharpClassNames(cs []ClassDecl) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.QualifiedName)
	}
	sort.Strings(out)
	return out
}
