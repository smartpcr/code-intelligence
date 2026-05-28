//go:build cgo

package ast

import (
	"strings"
	"testing"
)

// The tests in this file exercise the tree-sitter-backed C
// parser (parser_treesitter_c.go). They are gated on
// `//go:build cgo` because the smacker bindings link against
// C-compiled grammars; environments without a C toolchain
// silently skip this file -- that's intentional and matches
// the rest of the tree-sitter test suite
// (parser_treesitter_test.go).

// TestTreeSitterCParser_Metadata pins the parser's Language /
// Extensions surface so a future grammar bump that forgets to
// register `.c` / `.h` (or accidentally adds extra extensions
// the C++ parser also claims) fails loudly. The `.h` -> C
// routing is the pinned default per `dot-h-extension-routing`
// (tech-spec Section 8 R6).
func TestTreeSitterCParser_Metadata(t *testing.T) {
	p := NewTreeSitterCParser()
	if got := p.Language(); got != "c" {
		t.Fatalf("Language() = %q; want \"c\"", got)
	}
	exts := p.Extensions()
	want := map[string]bool{".c": false, ".h": false}
	for _, e := range exts {
		if _, ok := want[e]; !ok {
			t.Errorf("unexpected extension %q in Extensions() = %v", e, exts)
		}
		want[e] = true
	}
	for ext, seen := range want {
		if !seen {
			t.Errorf("missing extension %q in Extensions() = %v", ext, exts)
		}
	}
}

// TestTreeSitterCParser_ParsesFixture is the canonical C
// fixture: one struct, one enum, two function definitions
// (one of which calls the other), a system include, a local
// include, and a body-less prototype declaration. The
// expectations mirror tech-spec.md Section 5.1's construct
// table row by row.
func TestTreeSitterCParser_ParsesFixture(t *testing.T) {
	const src = `#include <stdio.h>
#include "local.h"

struct Point {
    int x;
    int y;
};

enum Color {
    RED,
    GREEN,
    BLUE
};

static int add(int a, int b) {
    return a + b;
}

int sum_three(int a, int b, int c) {
    int partial = add(a, b);
    return add(partial, c);
}

int prototype_only(int x);
`
	parser := NewTreeSitterCParser()
	res, err := parser.Parse("src/hello.c", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Classes -- struct Point + enum Color (2 total).
	classBy := map[string]ClassDecl{}
	for _, c := range res.Classes {
		classBy[c.QualifiedName] = c
	}
	if got, ok := classBy["Point"]; !ok {
		t.Errorf("missing struct Point; got %v", classDeclKeys(res.Classes))
	} else if got.Kind != "struct" {
		t.Errorf("Point.Kind = %q; want \"struct\"", got.Kind)
	}
	if got, ok := classBy["Color"]; !ok {
		t.Errorf("missing enum Color; got %v", classDeclKeys(res.Classes))
	} else if got.Kind != "enum" {
		t.Errorf("Color.Kind = %q; want \"enum\"", got.Kind)
	}

	// Methods -- add + sum_three (2 total). prototype_only
	// must NOT appear: v1 skips body-less function_declarator
	// prototypes per tech-spec Section 5.1.
	methods := map[string]MethodDecl{}
	for _, m := range res.Methods {
		methods[m.QualifiedName] = m
	}
	for _, want := range []string{"add", "sum_three"} {
		if _, ok := methods[want]; !ok {
			t.Errorf("missing method %q; got %v", want, methodKeys(methods))
		}
	}
	if _, ok := methods["prototype_only"]; ok {
		t.Errorf("prototype_only should NOT be emitted (no body)")
	}

	// Every method must have EnclosingClass="" -- C has no
	// classes (tech-spec Section 5.1).
	for name, m := range methods {
		if m.EnclosingClass != "" {
			t.Errorf("method %q: EnclosingClass = %q; want empty", name, m.EnclosingClass)
		}
	}

	// ParamSignature must hold the text between the outer
	// parens of the function declarator -- NOT including the
	// parens themselves (matches trimParens semantics shared
	// with the TS / Python tree-sitter walkers).
	add := methods["add"]
	if !strings.Contains(add.ParamSignature, "int a") || !strings.Contains(add.ParamSignature, "int b") {
		t.Errorf("add.ParamSignature = %q; want it to contain `int a` and `int b`", add.ParamSignature)
	}
	if strings.HasPrefix(strings.TrimSpace(add.ParamSignature), "(") {
		t.Errorf("add.ParamSignature still wraps parens: %q", add.ParamSignature)
	}

	// Modifiers: `add` is declared `static int add(...)`, so
	// the modifier list must include `static`. `sum_three`
	// has no modifiers.
	hasStatic := false
	for _, m := range add.Modifiers {
		if m == "static" {
			hasStatic = true
		}
	}
	if !hasStatic {
		t.Errorf("add.Modifiers = %v; want it to contain \"static\"", add.Modifiers)
	}
	sumThree := methods["sum_three"]
	if len(sumThree.Modifiers) != 0 {
		t.Errorf("sum_three.Modifiers = %v; want empty", sumThree.Modifiers)
	}

	// BodySource: brace-stripped per tech-spec Section 5.1
	// and parity with parser_treesitter.go::tsStripBraceSpan.
	// The first non-whitespace character must NOT be `{` and
	// the last non-whitespace character must NOT be `}`.
	if strings.HasPrefix(strings.TrimLeft(add.BodySource, " \t\n\r"), "{") {
		t.Errorf("add.BodySource still starts with `{`: %q", add.BodySource)
	}
	if strings.HasSuffix(strings.TrimRight(add.BodySource, " \t\n\r"), "}") {
		t.Errorf("add.BodySource still ends with `}`: %q", add.BodySource)
	}
	if !strings.Contains(add.BodySource, "return a + b") {
		t.Errorf("add.BodySource missing inner statement: %q", add.BodySource)
	}

	// BodyStartByte / BodyEndByte must point at the FIRST /
	// LAST interior bytes (matching the scanner contract and
	// tsStripBraceSpan -- per evaluator finding #2 on the TS
	// parser, the brace-strip parity is part of the v1
	// contract). For `add`, src[BodyStartByte-1] must be `{`
	// and src[BodyEndByte+1] must be `}`.
	srcBytes := []byte(src)
	if add.BodyStartByte <= 0 || add.BodyStartByte >= len(srcBytes) {
		t.Errorf("add.BodyStartByte out of range: %d (src len=%d)", add.BodyStartByte, len(srcBytes))
	} else if srcBytes[add.BodyStartByte-1] != '{' {
		t.Errorf("byte before BodyStartByte should be `{`; got %q at offset %d",
			srcBytes[add.BodyStartByte-1], add.BodyStartByte-1)
	}
	if add.BodyEndByte <= 0 || add.BodyEndByte >= len(srcBytes) {
		t.Errorf("add.BodyEndByte out of range: %d (src len=%d)", add.BodyEndByte, len(srcBytes))
	} else if srcBytes[add.BodyEndByte+1] != '}' {
		t.Errorf("byte after BodyEndByte should be `}`; got %q at offset %d",
			srcBytes[add.BodyEndByte+1], add.BodyEndByte+1)
	}

	// Calls: sum_three calls add (twice). Bare-name calls are
	// deduped via uniqueStringsInsert so `add` appears
	// exactly once in sum_three.Calls.
	gotCalls := map[string]int{}
	for _, c := range sumThree.Calls {
		gotCalls[c]++
	}
	if gotCalls["add"] != 1 {
		t.Errorf("sum_three.Calls = %v; want exactly one `add` entry", sumThree.Calls)
	}

	// ReceiverCalls / MemberAccesses must be empty -- C has
	// no `this` (tech-spec Section 5.1).
	if len(add.ReceiverCalls) != 0 {
		t.Errorf("add.ReceiverCalls = %v; want empty (C has no this)", add.ReceiverCalls)
	}
	if len(add.MemberAccesses) != 0 {
		t.Errorf("add.MemberAccesses = %v; want empty (C has no this)", add.MemberAccesses)
	}

	// LangMeta on classes / methods must be nil in v1.
	for name, c := range classBy {
		if c.LangMeta != nil {
			t.Errorf("class %q: LangMeta = %v; want nil in v1", name, c.LangMeta)
		}
	}
	for name, m := range methods {
		if m.LangMeta != nil {
			t.Errorf("method %q: LangMeta = %v; want nil in v1", name, m.LangMeta)
		}
	}

	// Imports: <stdio.h> -> stdio.h (no prefix); "local.h"
	// -> ./local.h (prefix). 2 imports total.
	if len(res.Imports) != 2 {
		t.Fatalf("Imports = %v; want exactly 2", res.Imports)
	}
	importBy := map[string]Import{}
	for _, imp := range res.Imports {
		importBy[imp.Module] = imp
	}
	if _, ok := importBy["stdio.h"]; !ok {
		t.Errorf("missing system import stdio.h; got %v", importModules(res.Imports))
	}
	if _, ok := importBy["./local.h"]; !ok {
		t.Errorf("missing local import ./local.h; got %v", importModules(res.Imports))
	}
}

// TestTreeSitterCParser_SkipsAnonymousAndForwardStruct covers
// two SKIP rules from tech-spec Section 5.1:
//   - anonymous struct (typedef carrier) -- no identifier ->
//     no ClassDecl
//   - forward declaration `struct Foo;` -- no body -> no
//     ClassDecl
//
// The single named, body-bearing struct in the fixture must
// still be extracted so the test cannot pass vacuously.
func TestTreeSitterCParser_SkipsAnonymousAndForwardStruct(t *testing.T) {
	const src = `struct Forward;

typedef struct {
    int x;
} TypedefCarrier;

struct Named {
    int field;
};
`
	res, err := NewTreeSitterCParser().Parse("src/x.c", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	names := map[string]bool{}
	for _, c := range res.Classes {
		names[c.QualifiedName] = true
	}
	if !names["Named"] {
		t.Errorf("missing Named; classes=%v", classDeclKeys(res.Classes))
	}
	if names["Forward"] {
		t.Errorf("forward declaration `struct Forward` should be skipped (no body)")
	}
	if names["TypedefCarrier"] {
		t.Errorf("typedef carrier should not be emitted in v1 (struct is anonymous)")
	}
	if names[""] {
		t.Errorf("empty-name class emitted; classes=%v", res.Classes)
	}
}

// TestTreeSitterCParser_UnionEmitsClassDecl covers the
// `union_specifier` -> ClassDecl{Kind="union"} branch in
// tech-spec Section 5.1.
func TestTreeSitterCParser_UnionEmitsClassDecl(t *testing.T) {
	const src = `union Number {
    int i;
    float f;
};
`
	res, err := NewTreeSitterCParser().Parse("src/u.c", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Classes) != 1 {
		t.Fatalf("Classes = %v; want exactly 1", res.Classes)
	}
	if got := res.Classes[0]; got.QualifiedName != "Number" || got.Kind != "union" {
		t.Errorf("got %+v; want QualifiedName=Number Kind=union", got)
	}
}

// TestTreeSitterCParser_HeaderFileOnlyEmitsClassesAndImports
// pins the `.h` routing: a header full of prototypes and
// includes (no function bodies) emits Classes + Imports, but
// NO Methods. This is the operational contract for the
// `dot-h-extension-routing` decision -- C++-only headers
// (with classes / templates) parse as C and emit nothing
// notable, the way the spec calls out.
func TestTreeSitterCParser_HeaderFileOnlyEmitsClassesAndImports(t *testing.T) {
	const src = `#ifndef LOCAL_H
#define LOCAL_H

#include <stddef.h>

struct Buffer {
    size_t len;
    char *data;
};

int buffer_init(struct Buffer *b, size_t cap);
void buffer_free(struct Buffer *b);

#endif
`
	res, err := NewTreeSitterCParser().Parse("include/local.h", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Methods) != 0 {
		t.Errorf("Methods = %v; want empty (header has only prototypes)", res.Methods)
	}
	hasBuffer := false
	for _, c := range res.Classes {
		if c.QualifiedName == "Buffer" && c.Kind == "struct" {
			hasBuffer = true
		}
	}
	if !hasBuffer {
		t.Errorf("missing struct Buffer; classes=%v", classDeclKeys(res.Classes))
	}
	hasStddef := false
	for _, imp := range res.Imports {
		if imp.Module == "stddef.h" {
			hasStddef = true
		}
	}
	if !hasStddef {
		t.Errorf("missing system import stddef.h; imports=%v", importModules(res.Imports))
	}
}

// TestTreeSitterCParser_PointerReturnAndFunctionPointerParam
// is the targeted grammar-nuance test from the rubber-duck
// review:
//   - pointer-returning function `static int *make_ptr(...)`
//     -- the outer declarator chain is
//     `pointer_declarator -> function_declarator`. The unwrap
//     must reach the inner `make_ptr` identifier.
//   - function-pointer parameter `int (*cb)(int)` -- there is
//     a nested function_declarator INSIDE the parameter list
//     that the declarator unwrap must NOT confuse with the
//     outer function's declarator (per rubber-duck #3).
func TestTreeSitterCParser_PointerReturnAndFunctionPointerParam(t *testing.T) {
	const src = `static int *make_ptr(int x) {
    return &x;
}

int apply(int (*cb)(int), int x) {
    return cb(x);
}
`
	res, err := NewTreeSitterCParser().Parse("src/p.c", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	names := map[string]MethodDecl{}
	for _, m := range res.Methods {
		names[m.QualifiedName] = m
	}
	if _, ok := names["make_ptr"]; !ok {
		t.Errorf("missing pointer-returning function make_ptr; got %v", methodKeys(names))
	}
	if _, ok := names["apply"]; !ok {
		t.Errorf("missing function apply; got %v", methodKeys(names))
	}
	// The function-pointer parameter must NOT mint a method
	// named `cb` -- that would mean the declarator unwrap
	// crossed into the parameter list.
	if _, ok := names["cb"]; ok {
		t.Errorf("function-pointer parameter `cb` should NOT be emitted as a method; got %v", methodKeys(names))
	}
}

// TestTreeSitterCParser_RegisteredInDefaultParsers verifies
// the parser is reachable through `defaultParsers()` (the
// factory parsers_cgo.go::defaultParsers wires into the
// dispatcher). Without this assertion, the parser could be
// implemented but silently unreachable -- the
// implemented-but-unreachable failure mode flagged by the
// rubber-duck review.
func TestTreeSitterCParser_RegisteredInDefaultParsers(t *testing.T) {
	parsers := defaultParsers()
	want := map[string]bool{".c": false, ".h": false}
	for _, p := range parsers {
		if p.Language() != "c" {
			continue
		}
		for _, ext := range p.Extensions() {
			if _, ok := want[ext]; ok {
				want[ext] = true
			}
		}
	}
	for ext, found := range want {
		if !found {
			t.Errorf("defaultParsers() does not register the C parser for %q; saw %d parsers", ext, len(parsers))
		}
	}
}

// classDeclKeys returns the sorted-style key list of a class
// slice for compact test error messages.
func classDeclKeys(classes []ClassDecl) []string {
	out := make([]string, 0, len(classes))
	for _, c := range classes {
		out = append(out, c.QualifiedName)
	}
	return out
}

// methodKeys is the method-side twin of classDeclKeys.
func methodKeys(m map[string]MethodDecl) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// importModules returns the module names of every Import for
// compact test error messages.
func importModules(imports []Import) []string {
	out := make([]string, 0, len(imports))
	for _, imp := range imports {
		out = append(out, imp.Module)
	}
	return out
}
