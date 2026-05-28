//go:build cgo

package ast

import (
	"sort"
	"testing"
)

// Tests for the Rust tree-sitter parser. Gated on `//go:build
// cgo` because the smacker rust binding links against a
// C-compiled grammar; environments without a C toolchain skip
// this file (and any other parser_treesitter_*.go tests).
//
// CI exercises this file on the Linux runner that builds with
// CGO_ENABLED=1 (the `make test-race` step in
// .github/workflows/agent-memory-ci.yml). Local Windows dev
// with the default CGO=0 toolchain silently skips it.

// TestTreeSitterRustParser_LanguageAndExtensions pins the
// metadata the dispatcher uses to wire this parser up.
func TestTreeSitterRustParser_LanguageAndExtensions(t *testing.T) {
	p := NewTreeSitterRustParser()
	if got := p.Language(); got != "rust" {
		t.Errorf("Language() = %q; want rust", got)
	}
	exts := p.Extensions()
	if len(exts) != 1 || exts[0] != ".rs" {
		t.Errorf("Extensions() = %v; want [\".rs\"]", exts)
	}
}

// TestTreeSitterRustParser_EmitsStructEnumTrait asserts the
// happy-path: each of the three class-like Rust item types
// surfaces as a ClassDecl with the documented Kind tag.
func TestTreeSitterRustParser_EmitsStructEnumTrait(t *testing.T) {
	const src = `
pub struct Greeter {
    prefix: String,
}

pub enum Status {
    Idle,
    Busy(u32),
    Done { code: i32 },
}

pub trait Greet {
    fn greet(&self, name: &str) -> String;
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/hello.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	byName := classByName(res.Classes)
	for _, want := range []struct {
		name string
		kind string
	}{
		{"Greeter", "struct"},
		{"Status", "enum"},
		{"Greet", "trait"},
	} {
		got, ok := byName[want.name]
		if !ok {
			t.Errorf("missing class %q; got %v", want.name, classNames(res.Classes))
			continue
		}
		if got.Kind != want.kind {
			t.Errorf("class %q Kind = %q; want %q", want.name, got.Kind, want.kind)
		}
	}

	// Enum variants must NOT surface as Methods (the v1
	// contract -- the workstream brief calls this out
	// explicitly). Confirm Methods is empty for this fixture;
	// follow-up stages add free-function and impl-method
	// emission additively.
	if len(res.Methods) != 0 {
		t.Errorf("expected zero methods in v1 (enum variants must not surface as methods); got %d (%+v)",
			len(res.Methods), res.Methods)
	}
}

// TestTreeSitterRustParser_TraitSupertraitsPopulateExtends
// pins the workstream's explicit contract: a `trait Foo: A + B`
// supertrait clause must populate Extends with [A, B].
func TestTreeSitterRustParser_TraitSupertraitsPopulateExtends(t *testing.T) {
	const src = `
pub trait Base {}
pub trait Other {}
pub trait Derived: Base + Other {
    fn ping(&self);
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/derived.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := classByName(res.Classes)
	d, ok := byName["Derived"]
	if !ok {
		t.Fatalf("missing trait Derived; got %v", classNames(res.Classes))
	}
	got := append([]string(nil), d.Extends...)
	sort.Strings(got)
	want := []string{"Base", "Other"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("Derived.Extends = %v; want %v", got, want)
	}
}

// TestTreeSitterRustParser_ModItemRecursesWithoutNamePropagation
// verifies the v1 contract for in-file modules: declarations
// inside `mod foo { ... }` surface at file scope with bare
// QualifiedName (`Inner`, not `foo::Inner` or `foo.Inner`).
// Cross-file resolution that respects module paths lives in
// a later workstream.
func TestTreeSitterRustParser_ModItemRecursesWithoutNamePropagation(t *testing.T) {
	const src = `
pub mod outer {
    pub struct Inner {
        x: i32,
    }

    pub mod nested {
        pub enum Deep {
            One,
            Two,
        }
    }
}

pub struct TopLevel {}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/mods.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	names := classNamesSorted(res.Classes)
	want := []string{"Deep", "Inner", "TopLevel"}
	if !stringSlicesEqual(names, want) {
		t.Errorf("class names = %v; want %v (modules must NOT propagate into QualifiedName in v1)",
			names, want)
	}
	// Confirm `Inner` carries Kind="struct" (not something
	// derived from the module wrapping).
	byName := classByName(res.Classes)
	if inner, ok := byName["Inner"]; !ok || inner.Kind != "struct" {
		t.Errorf("Inner missing or wrong Kind: %+v", inner)
	}
	if deep, ok := byName["Deep"]; !ok || deep.Kind != "enum" {
		t.Errorf("Deep missing or wrong Kind: %+v", deep)
	}
}

// TestTreeSitterRustParser_OutOfLineModIsSkipped pins the
// behaviour for `mod foo;` (no body, just a forward
// declaration that points at another file). The walker must
// not panic and must produce no spurious class entries.
func TestTreeSitterRustParser_OutOfLineModIsSkipped(t *testing.T) {
	const src = `
mod other;

pub struct Here {}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/lib.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	names := classNamesSorted(res.Classes)
	want := []string{"Here"}
	if !stringSlicesEqual(names, want) {
		t.Errorf("class names = %v; want %v", names, want)
	}
}

// TestTreeSitterRustParser_TraitBoundsDropTypeArgsAndLifetimes
// pins the bound-collector's guard rails: `trait T: Foo<X> +
// 'static + bar::Baz` must yield ["Foo", "Baz"] -- the type
// argument `X`, the lifetime `'static`, and the leading scope
// `bar::` are all dropped so the dispatcher's same-file
// resolver does not emit edges to identifiers that aren't
// actually supertraits.
func TestTreeSitterRustParser_TraitBoundsDropTypeArgsAndLifetimes(t *testing.T) {
	const src = `
pub trait Foo<T> {}
pub trait Sample: Foo<u32> + Send + bar::Baz + 'static {}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/bounds.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := classByName(res.Classes)
	s, ok := byName["Sample"]
	if !ok {
		t.Fatalf("missing trait Sample; got %v", classNames(res.Classes))
	}
	got := append([]string(nil), s.Extends...)
	sort.Strings(got)
	want := []string{"Baz", "Foo", "Send"}
	if !stringSlicesEqual(got, want) {
		t.Errorf("Sample.Extends = %v; want %v", got, want)
	}
}

// ------------------------------------------------------------
// Helpers
// ------------------------------------------------------------

func classByName(in []ClassDecl) map[string]ClassDecl {
	out := make(map[string]ClassDecl, len(in))
	for _, c := range in {
		out[c.QualifiedName] = c
	}
	return out
}

func classNames(in []ClassDecl) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = c.QualifiedName
	}
	return out
}

func classNamesSorted(in []ClassDecl) []string {
	out := classNames(in)
	sort.Strings(out)
	return out
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
