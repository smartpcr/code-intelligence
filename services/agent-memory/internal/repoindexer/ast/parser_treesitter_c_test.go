//go:build cgo

package ast

import (
	"sort"
	"strings"
	"testing"
)

// TestTreeSitterCParser_LanguageAndExtensions verifies the
// placeholder parser advertises the canonical C language id
// and the `.c` / `.h` extension set from the workstream brief
// §4 "Register extensions". The sibling stage workstream
// `stage-3.1-ctreesitterparser-implementation` will replace
// the stub Parse() with a real walker, but the
// (Language, Extensions) contract MUST stay stable through
// that swap because the dispatcher routes files by extension
// (lower-case) at registration time -- a drift here would
// silently mis-route real `.c` / `.h` source files.
func TestTreeSitterCParser_LanguageAndExtensions(t *testing.T) {
	p := NewTreeSitterCParser()
	if got := p.Language(); got != "c" {
		t.Fatalf("Language: want c, got %q", got)
	}
	want := []string{".c", ".h"}
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

// TestTreeSitterCParser_StubParseEmpty asserts that the
// placeholder Parse() returns an empty ParseResult (no error)
// for valid C input. The sibling stage workstream
// `stage-3.1-ctreesitterparser-implementation` REPLACES this
// test in-place with a fixture-driven walker test that
// asserts emitted Classes / Methods / Imports per the story
// brief §1 ("C: functions, structs, includes, function
// calls"). Until then, this test guards against accidental
// panics in the stub and pins the empty-return contract so a
// half-implemented sibling-stage walker doesn't silently
// regress the result shape.
func TestTreeSitterCParser_StubParseEmpty(t *testing.T) {
	const src = `
#include <stdio.h>

int add(int a, int b) {
  return a + b;
}

int main(void) {
  printf("%d\n", add(2, 3));
  return 0;
}
`
	p := NewTreeSitterCParser()
	res, err := p.Parse("src/main.c", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(res.Classes); got != 0 {
		t.Errorf("stub should return empty Classes; got %d (%+v)", got, res.Classes)
	}
	if got := len(res.Methods); got != 0 {
		t.Errorf("stub should return empty Methods; got %d (%+v)", got, res.Methods)
	}
	if got := len(res.Imports); got != 0 {
		t.Errorf("stub should return empty Imports; got %d (%+v)", got, res.Imports)
	}
}

// TestTreeSitterCParser_StubParseHeader confirms the stub
// also handles `.h` header input without error. Once the
// sibling stage lands the real walker, this test should be
// extended to assert that `#include` directives become
// Imports and that struct declarations become ClassDecls.
func TestTreeSitterCParser_StubParseHeader(t *testing.T) {
	const src = `
#ifndef WIDGET_H
#define WIDGET_H

struct Widget {
  int id;
  const char *name;
};

void widget_init(struct Widget *w, int id, const char *name);

#endif
`
	p := NewTreeSitterCParser()
	res, err := p.Parse("include/widget.h", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := len(res.Classes); got != 0 {
		t.Errorf("stub should return empty Classes; got %d", got)
	}
}

// TestCFixture_EmitsExpectedNodeAndEdgeSet pins the Stage 3.4
// (C-fixture-test) acceptance contract for the C tree-sitter
// parser. The fixture is a small, deliberately canonical C
// file that exercises every grammar surface the workstream
// brief calls out:
//
//   - one `struct Greeter { int n; };` declaration -- so the
//     parser proves it recognises `struct_specifier` and
//     emits a ClassDecl with `Kind="struct"`,
//   - two free functions (`greet`, `format_greeting`) -- so
//     the parser proves it recognises `function_definition`
//     at translation-unit scope and emits them as
//     MethodDecls with `EnclosingClass==""` (the parser-
//     level signal the dispatcher uses to mint
//     `file -> method` contains edges instead of
//     `class -> method`),
//   - one same-file static call `format_greeting(n)` from
//     inside `greet` -- so the bare-identifier call walker
//     records it in `greet.Calls` (the dispatcher's Pass 2b
//     resolver then matches against the same-file callee
//     index to mint a single `static_calls` edge), and
//   - two `#include` directives -- one system include
//     (`<stdio.h>`, non-relative; the dispatcher materialises
//     a synthetic external package node + a `file -> stdio.h`
//     imports edge), one local include (`"local.h"`, which
//     the parser prefixes with `./` so the dispatcher's
//     `isRelativeImport` filter drops it). The local include
//     MUST NOT produce an imports edge.
//
// The test exercises the parser directly (via `parser.Parse`,
// mirroring TestGoTreeSitterFixture_EmitsExpectedNodeAndEdgeSet
// in parser_treesitter_go_test.go and
// TestCSharpFixture_EmitsExpectedNodeAndEdgeSet in
// parser_treesitter_csharp_test.go) rather than at the
// dispatcher level (//go:build canonical_dispatcher, as used by
// TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet and the
// Python equivalent). The brief's "edge" terminology maps to
// ParseResult fields the dispatcher consumes:
//
//   - "contains edge"      -> file-level decl (ClassDecl or
//     MethodDecl with EnclosingClass=="");
//     the dispatcher mints one `contains`
//     edge per file-level decl in Pass 1
//   - "static_calls edge"  -> MethodDecl.Calls (bare-name
//     entries that the dispatcher's
//     Pass 2b resolver stitches against
//     the same-file callee index)
//   - "imports edge"       -> ParseResult.Imports entry with
//     a non-relative Module specifier
//     (the dispatcher's `isRelativeImport`
//     filter drops entries with a leading
//     `.` or `/`, so the parser's `./`-
//     prefix on local includes is the
//     contract that suppresses the edge)
//
// The C parser walker is owned by the sibling stage workstream
// `stage-3.1-ctreesitterparser-implementation`; this test pins
// the acceptance shape that walker must produce when its
// branch merges to `feature/memory`. Until that merge lands,
// running this test against the placeholder Parse() (which
// returns an empty ParseResult) will report the assertion
// gaps -- by design, so the operator can see the swap surface.
func TestCFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	const src = `#include <stdio.h>
#include "local.h"

struct Greeter {
    int n;
};

int format_greeting(int n) {
    return n + 1;
}

int greet(int n) {
    return format_greeting(n);
}
`
	parser := NewTreeSitterCParser()
	if got := parser.Language(); got != "c" {
		t.Fatalf("Language() = %q; want %q", got, "c")
	}

	res, err := parser.Parse("src/hello.c", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// ----- Classes -----
	// The brief pins exactly 1 class node: `Greeter` with
	// `Kind="struct"`. C has no `class` keyword, so the
	// only path that emits a ClassDecl here is the
	// `struct_specifier` handler.
	if got := len(res.Classes); got != 1 {
		t.Errorf("expected exactly 1 class/struct node; got %d (%v)",
			got, cFixtureClassNames(res.Classes))
	}
	greeter, ok := findCClass(res.Classes, "Greeter")
	if !ok {
		t.Fatalf("struct %q missing from emitted set; got %v",
			"Greeter", cFixtureClassNames(res.Classes))
	}
	// Kind assertion: the brief pins `Kind="struct"`. A
	// walker that mis-classifies the struct (e.g. emits it
	// with Kind="class" by copying the C# branch, or with
	// Kind="union"/"enum" by routing through the wrong
	// specifier handler) would still satisfy the count + name
	// check above. ClassDecl.Kind is persisted on the node's
	// attrs_json["decl_kind"] and downstream consumers (graph
	// queries, policy rules) route on it, so a kind
	// mis-emission is a real silent failure mode the test
	// must pin.
	if greeter.Kind != "struct" {
		t.Errorf("Greeter.Kind = %q; want %q (full=%+v)",
			greeter.Kind, "struct", greeter)
	}
	// C structs have no inheritance, no implements, and no
	// per-language LangMeta in v1 (tech-spec Section 5.1
	// pins LangMeta as nil for C). Pin all three so a future
	// walker revision that accidentally borrows from the C++
	// or Go branch is caught.
	if len(greeter.Extends) != 0 {
		t.Errorf("Greeter.Extends should be empty (C has no inheritance); got %v",
			greeter.Extends)
	}
	if len(greeter.Implements) != 0 {
		t.Errorf("Greeter.Implements should be empty (C has no interfaces); got %v",
			greeter.Implements)
	}
	if greeter.LangMeta != nil {
		t.Errorf("Greeter.LangMeta should be nil (C v1 has no per-language attrs); got %+v",
			greeter.LangMeta)
	}

	// ----- Methods -----
	// The brief pins exactly 2 method nodes: `greet` and
	// `format_greeting`. Both are free functions, so they
	// MUST be emitted with `EnclosingClass==""` -- the
	// dispatcher reads that field to decide whether to mint
	// the contains edge from the file (free function) or
	// from the enclosing class (member method).
	if got := len(res.Methods); got != 2 {
		t.Errorf("expected exactly 2 method nodes; got %d (%v)",
			got, methodNames(res.Methods))
	}
	greet, ok := findCMethod(res.Methods, "greet")
	if !ok {
		t.Fatalf("function %q missing from emitted set; got %v",
			"greet", methodNames(res.Methods))
	}
	format, ok := findCMethod(res.Methods, "format_greeting")
	if !ok {
		t.Fatalf("function %q missing from emitted set; got %v",
			"format_greeting", methodNames(res.Methods))
	}
	// EnclosingClass=="" is the parser-level contract that
	// drives the dispatcher's `file -> method` contains edge
	// (Pass 1b: `parentKind = "file"` when EnclosingClass is
	// empty). A walker that incorrectly nested a free
	// function under any class -- e.g. by treating `greet`
	// as a method of `Greeter` because their names share a
	// prefix -- would silently mis-route the contains edge.
	if greet.EnclosingClass != "" {
		t.Errorf("greet.EnclosingClass = %q; want %q (free function)",
			greet.EnclosingClass, "")
	}
	if format.EnclosingClass != "" {
		t.Errorf("format_greeting.EnclosingClass = %q; want %q (free function)",
			format.EnclosingClass, "")
	}

	// ----- contains edges (file -> Greeter, file -> greet, file -> format_greeting) -----
	// The brief pins exactly 3 contains edges. At the parser
	// level the dispatcher mints one `contains` edge per
	// file-level decl: ClassDecls always (Greeter -> 1 edge),
	// plus MethodDecls whose EnclosingClass is empty (free
	// functions -> 2 edges). The 1+2 = 3 total matches the
	// brief's pin. Counts above already prove this; the
	// assertion here documents the mapping explicitly so a
	// future refactor that, say, started emitting struct
	// fields as nested ClassDecls is caught here at the
	// fixture level rather than in a more distant
	// dispatcher-level regression.
	fileLevelDecls := len(res.Classes)
	for _, m := range res.Methods {
		if m.EnclosingClass == "" {
			fileLevelDecls++
		}
	}
	if fileLevelDecls != 3 {
		t.Errorf("expected exactly 3 file-level decls (-> 3 contains edges); got %d "+
			"(classes=%v methods=%v)", fileLevelDecls,
			cFixtureClassNames(res.Classes), methodNames(res.Methods))
	}

	// ----- static_calls edge (greet -> format_greeting) -----
	// The brief pins exactly 1 static_calls edge:
	// greet -> format_greeting. The C walker records bare-
	// identifier callees in `Calls` (`walkCCalls`); the
	// dispatcher's Pass 2b resolver then matches each
	// against the same-file callee index to mint the edge.
	if !containsString(greet.Calls, "format_greeting") {
		t.Errorf("greet.Calls should contain format_greeting; got %v",
			greet.Calls)
	}
	if got := len(greet.Calls); got != 1 {
		// Cardinality pin: a walker that double-emits the
		// call (e.g. visits the same call_expression node
		// twice through both the field accessor and the
		// generic named-children walk) would still satisfy
		// the contains-check above. Pin the count to catch
		// the duplicate-walk failure mode.
		t.Errorf("expected exactly 1 entry in greet.Calls (format_greeting); got %d (%v)",
			got, greet.Calls)
	}
	if got := len(greet.ReceiverCalls); got != 0 {
		// C has no `this` and no receiver-qualified calls;
		// ReceiverCalls must always be empty. A walker
		// that mis-routed a bare call into ReceiverCalls
		// would produce a second edge through Pass 2b's
		// receiver branch (double-emission across slices).
		t.Errorf("expected 0 entries in greet.ReceiverCalls (C has no receivers); got %d (%v)",
			got, greet.ReceiverCalls)
	}
	// format_greeting has no callees -- pin so a walker
	// that hallucinates self-references or leaks the parent
	// function's call set is caught.
	if got := len(format.Calls); got != 0 {
		t.Errorf("expected 0 entries in format_greeting.Calls (no inner calls); got %d (%v)",
			got, format.Calls)
	}

	// ----- imports edge (file -> stdio.h package) -----
	// The brief pins exactly 1 imports edge: file -> stdio.h.
	// System includes (`#include <stdio.h>`) keep the
	// bracketed path verbatim (`stdio.h` -- no leading `.`
	// or `/`), so `isRelativeImport` returns false and the
	// dispatcher mints the edge. Local includes
	// (`#include "local.h"`) get a `./` prefix from the
	// parser so the same filter drops them -- the brief
	// pins this as "0 imports edges for ./local.h".
	mods := importModules(res.Imports)
	if !containsString(mods, "stdio.h") {
		t.Errorf("import %q missing; got %v", "stdio.h", mods)
	}
	// The local include MUST be present in res.Imports with
	// the `./` prefix (NOT as a bare `local.h`). The bare
	// form would pass `isRelativeImport` (no leading `.`)
	// and produce an unwanted edge; the `./` prefix is the
	// contract that suppresses it. Pin both the presence of
	// the prefixed form and the absence of the bare form
	// so a walker that "cleans up" the prefix is caught.
	if containsString(mods, "local.h") {
		t.Errorf("import %q must NOT appear as bare path (would produce an imports edge); "+
			"the parser must emit it as %q so isRelativeImport drops it. got %v",
			"local.h", "./local.h", mods)
	}
	if !containsString(mods, "./local.h") {
		t.Errorf("import %q missing (the parser must prefix local includes with `./` "+
			"so the dispatcher drops them); got %v", "./local.h", mods)
	}
	// Cardinality pin: exactly one non-relative import
	// (stdio.h). Counting non-relative entries mirrors the
	// dispatcher's `isRelativeImport` filter exactly, so
	// this is the parser-level equivalent of "1 imports
	// edge total".
	nonRelative := 0
	for _, imp := range res.Imports {
		mod := imp.Module
		if mod == "" {
			continue
		}
		if strings.HasPrefix(mod, ".") || strings.HasPrefix(mod, "/") {
			continue
		}
		nonRelative++
	}
	if nonRelative != 1 {
		t.Errorf("expected exactly 1 non-relative import (-> 1 imports edge); got %d (%v)",
			nonRelative, mods)
	}
	// And the total set is exactly 2 (stdio.h + ./local.h);
	// pin so a walker that drops the local include outright
	// (instead of marking it relative) is caught -- the
	// dispatcher's relative-import path also writes a debug
	// log entry, so silently dropping the entry would lose
	// observability the operator relies on.
	if got := len(res.Imports); got != 2 {
		t.Errorf("expected exactly 2 imports (stdio.h + ./local.h); got %d (%v)",
			got, mods)
	}
}

// findCClass returns the ClassDecl whose QualifiedName equals
// want. C has no namespace prefixing, so exact-match is the
// only sensible lookup -- a tolerant suffix match (as used by
// findCSharpClass for `Demo.IGreeter`) would mask a walker
// regression that accidentally added a phantom prefix.
func findCClass(classes []ClassDecl, want string) (ClassDecl, bool) {
	for _, c := range classes {
		if c.QualifiedName == want {
			return c, true
		}
	}
	return ClassDecl{}, false
}

// findCMethod returns the MethodDecl whose QualifiedName
// equals want. As with findCClass, exact-match is the right
// contract for C: free functions live at file scope with no
// enclosing class, so their QualifiedName is the bare
// identifier verbatim.
func findCMethod(methods []MethodDecl, want string) (MethodDecl, bool) {
	for _, m := range methods {
		if m.QualifiedName == want {
			return m, true
		}
	}
	return MethodDecl{}, false
}

// cFixtureClassNames returns just the QualifiedName slice for
// a slice of ClassDecls. Named with the `cFixture` prefix so
// it does not collide with sibling helpers under //go:build cgo
// (parser_treesitter_cpp_test.go::classNames,
// parser_treesitter_go_test.go::goClassNames,
// parser_treesitter_csharp_test.go::csharpClassNames).
func cFixtureClassNames(cs []ClassDecl) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.QualifiedName)
	}
	sort.Strings(out)
	return out
}
