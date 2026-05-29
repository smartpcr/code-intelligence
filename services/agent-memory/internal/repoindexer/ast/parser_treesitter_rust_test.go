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

	byName := rustClassByName(res.Classes)
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
			t.Errorf("missing class %q; got %v", want.name, rustClassNames(res.Classes))
			continue
		}
		if got.Kind != want.kind {
			t.Errorf("class %q Kind = %q; want %q", want.name, got.Kind, want.kind)
		}
	}

	// Enum variants must NOT surface as Methods. The trait's
	// required `greet` method IS expected (one method).
	if len(res.Methods) != 1 {
		t.Errorf("expected exactly 1 method (the trait's required greet); got %d (%+v)",
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
	byName := rustClassByName(res.Classes)
	d, ok := byName["Derived"]
	if !ok {
		t.Fatalf("missing trait Derived; got %v", rustClassNames(res.Classes))
	}
	got := append([]string(nil), d.Extends...)
	sort.Strings(got)
	want := []string{"Base", "Other"}
	if !rustStringSlicesEqual(got, want) {
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
	names := rustClassNamesSorted(res.Classes)
	want := []string{"Deep", "Inner", "TopLevel"}
	if !rustStringSlicesEqual(names, want) {
		t.Errorf("class names = %v; want %v (modules must NOT propagate into QualifiedName in v1)",
			names, want)
	}
	byName := rustClassByName(res.Classes)
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
	names := rustClassNamesSorted(res.Classes)
	want := []string{"Here"}
	if !rustStringSlicesEqual(names, want) {
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
	byName := rustClassByName(res.Classes)
	s, ok := byName["Sample"]
	if !ok {
		t.Fatalf("missing trait Sample; got %v", rustClassNames(res.Classes))
	}
	got := append([]string(nil), s.Extends...)
	sort.Strings(got)
	want := []string{"Baz", "Foo", "Send"}
	if !rustStringSlicesEqual(got, want) {
		t.Errorf("Sample.Extends = %v; want %v", got, want)
	}
}

// TestTreeSitterRustParser_FreeFunctionEmitsMethod pins the
// free-function contract: a top-level `fn foo() { ... }`
// produces a MethodDecl with empty EnclosingClass and
// captured call sites from its body.
func TestTreeSitterRustParser_FreeFunctionEmitsMethod(t *testing.T) {
	const src = `
pub fn format_greeting(name: &str) -> String {
    let s = format(name);
    helper();
    s
}

fn helper() {}
fn format(name: &str) -> String { name.to_string() }
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/free.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := rustMethodByQN(res.Methods)
	fmtg, ok := byName["format_greeting"]
	if !ok {
		t.Fatalf("missing free function format_greeting; got %v", rustMethodQNs(res.Methods))
	}
	if fmtg.EnclosingClass != "" {
		t.Errorf("free function EnclosingClass = %q; want \"\"", fmtg.EnclosingClass)
	}
	// `pub` should surface as a modifier.
	if !rustContainsString(fmtg.Modifiers, "pub") {
		t.Errorf("format_greeting Modifiers = %v; want to include 'pub'", fmtg.Modifiers)
	}
	// Body calls: `format(name)` and `helper()` are bare
	// identifiers; the `name.to_string()` receiver call is
	// NOT collected because the receiver is not `self`.
	calls := append([]string(nil), fmtg.Calls...)
	sort.Strings(calls)
	want := []string{"format", "helper"}
	if !rustStringSlicesEqual(calls, want) {
		t.Errorf("format_greeting.Calls = %v; want %v (only bare calls; non-self receiver calls dropped)",
			calls, want)
	}
	if len(fmtg.ReceiverCalls) != 0 {
		t.Errorf("format_greeting.ReceiverCalls = %v; want [] (no self.X() calls in this body)",
			fmtg.ReceiverCalls)
	}
}

// TestTreeSitterRustParser_TraitDefaultAndRequiredMethods
// pins the trait body contract: `function_item` produces a
// default-bodied method with LangMeta["trait_default"]=true,
// `function_signature_item` produces a required (body-less)
// method with no trait_default flag.
func TestTreeSitterRustParser_TraitDefaultAndRequiredMethods(t *testing.T) {
	const src = `
pub trait Greeter {
    fn greet(&self, name: &str) -> String {
        String::new()
    }

    fn ping(&self);
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/trait.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byQN := rustMethodByQN(res.Methods)

	greet, ok := byQN["Greeter.greet"]
	if !ok {
		t.Fatalf("missing trait default method Greeter.greet; got %v", rustMethodQNs(res.Methods))
	}
	if greet.EnclosingClass != "Greeter" {
		t.Errorf("Greeter.greet EnclosingClass = %q; want Greeter", greet.EnclosingClass)
	}
	if greet.LangMeta == nil || greet.LangMeta["trait_default"] != true {
		t.Errorf("Greeter.greet LangMeta[trait_default] = %v; want true (%+v)",
			greet.LangMeta, greet.LangMeta)
	}

	ping, ok := byQN["Greeter.ping"]
	if !ok {
		t.Fatalf("missing trait required method Greeter.ping; got %v", rustMethodQNs(res.Methods))
	}
	if ping.LangMeta != nil && ping.LangMeta["trait_default"] == true {
		t.Errorf("Greeter.ping must NOT have trait_default=true; got %v", ping.LangMeta)
	}
	if ping.BodySource != "" {
		t.Errorf("Greeter.ping BodySource = %q; want \"\" (required methods have no body)",
			ping.BodySource)
	}
}

// TestTreeSitterRustParser_ImplBlockEmitsMethodsAndImplements
// pins the impl_item contract: trait impls populate
// LangMeta["trait"] on each method AND append the trait to
// the target type's Implements list. Inherent impls produce
// methods with no LangMeta["trait"].
func TestTreeSitterRustParser_ImplBlockEmitsMethodsAndImplements(t *testing.T) {
	const src = `
pub trait Greeter {
    fn greet(&self) -> String;
}

pub struct G;

impl Greeter for G {
    fn greet(&self) -> String {
        String::from("hi")
    }
}

impl G {
    pub fn name(&self) -> &str { "G" }
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/impl.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// The struct G must list Greeter under Implements.
	byName := rustClassByName(res.Classes)
	g, ok := byName["G"]
	if !ok {
		t.Fatalf("missing struct G; got %v", rustClassNames(res.Classes))
	}
	if !rustStringSlicesEqual(g.Implements, []string{"Greeter"}) {
		t.Errorf("G.Implements = %v; want [Greeter]", g.Implements)
	}

	// Two methods on G: greet (trait impl) and name (inherent).
	byQN := rustMethodByQN(res.Methods)
	greet, ok := byQN["G.greet"]
	if !ok {
		t.Fatalf("missing impl method G.greet; got %v", rustMethodQNs(res.Methods))
	}
	if greet.LangMeta == nil || greet.LangMeta["trait"] != "Greeter" {
		t.Errorf("G.greet LangMeta[trait] = %v; want \"Greeter\" (%+v)",
			greet.LangMeta, greet.LangMeta)
	}
	name, ok := byQN["G.name"]
	if !ok {
		t.Fatalf("missing inherent method G.name; got %v", rustMethodQNs(res.Methods))
	}
	if name.LangMeta != nil && name.LangMeta["trait"] != nil {
		t.Errorf("G.name must NOT carry trait LangMeta; got %v", name.LangMeta)
	}
	if !rustContainsString(name.Modifiers, "pub") {
		t.Errorf("G.name Modifiers = %v; want to include 'pub'", name.Modifiers)
	}
}

// TestTreeSitterRustParser_ImplWithGenericsAndReferences
// pins the head-identifier extractor: `impl<T> Foo<T>` and
// `impl Trait for &Foo` should both resolve to the bare
// target name "Foo".
func TestTreeSitterRustParser_ImplWithGenericsAndReferences(t *testing.T) {
	const src = `
pub struct Foo;
pub trait Show {
    fn show(&self) -> String;
}

impl<T> Foo {
    pub fn generic(&self, _t: T) {}
}

impl Show for &Foo {
    fn show(&self) -> String { String::new() }
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/generic.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byQN := rustMethodByQN(res.Methods)
	if _, ok := byQN["Foo.generic"]; !ok {
		t.Errorf("missing impl<T> Foo method 'Foo.generic'; got %v", rustMethodQNs(res.Methods))
	}
	if _, ok := byQN["Foo.show"]; !ok {
		t.Errorf("missing impl Show for &Foo method 'Foo.show'; got %v", rustMethodQNs(res.Methods))
	}
	byName := rustClassByName(res.Classes)
	if foo, ok := byName["Foo"]; !ok || !rustStringSlicesEqual(foo.Implements, []string{"Show"}) {
		t.Errorf("Foo.Implements = %v; want [Show]", foo.Implements)
	}
}

// TestTreeSitterRustParser_SelfReceiverCallsAndMembers pins
// the body-walk contract for self-qualified expressions:
// `self.foo()` produces a ReceiverCall; `self.field` outside
// a call produces a MemberAccess; non-self receiver calls
// (`name.to_string()`) are NOT collected because the
// receiver's type is not statically known.
func TestTreeSitterRustParser_SelfReceiverCallsAndMembers(t *testing.T) {
	const src = `
pub struct Counter {
    value: i32,
}

impl Counter {
    pub fn step(&mut self, name: &str) -> i32 {
        self.value = self.value + 1;
        self.log(name);
        let s = name.to_string();
        self.value
    }

    fn log(&self, _msg: &str) {}
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/counter.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byQN := rustMethodByQN(res.Methods)
	step, ok := byQN["Counter.step"]
	if !ok {
		t.Fatalf("missing Counter.step; got %v", rustMethodQNs(res.Methods))
	}

	// ReceiverCalls: only self.log (the only self.X() call).
	if !rustStringSlicesEqual(step.ReceiverCalls, []string{"log"}) {
		t.Errorf("Counter.step.ReceiverCalls = %v; want [log] (only self.X() calls)",
			step.ReceiverCalls)
	}

	// Calls: empty (the only non-receiver call is
	// `name.to_string()` which is a non-self receiver call
	// AND name itself is not a bare call).
	if len(step.Calls) != 0 {
		t.Errorf("Counter.step.Calls = %v; want [] (no bare calls; non-self receiver calls dropped)",
			step.Calls)
	}

	// MemberAccesses: self.value appears in three places --
	// once as the LHS of an assignment (write), once as the
	// RHS of an assignment (read), once as the return
	// expression (read). The walker dedupes to one
	// MemberAccess with IsWrite=true.
	if len(step.MemberAccesses) != 1 {
		t.Fatalf("Counter.step.MemberAccesses = %v; want exactly 1 entry", step.MemberAccesses)
	}
	if step.MemberAccesses[0].Name != "value" || !step.MemberAccesses[0].IsWrite {
		t.Errorf("Counter.step.MemberAccesses[0] = %+v; want {Name:value, IsWrite:true}",
			step.MemberAccesses[0])
	}
}

// TestTreeSitterRustParser_ScopedCallsTakeRightmost pins the
// `scoped_identifier` callee extraction: `std::vec::Vec::new()`
// records "new" in Calls so the dispatcher's same-file
// resolver matches it the same way it would resolve a bare
// `new()` call.
func TestTreeSitterRustParser_ScopedCallsTakeRightmost(t *testing.T) {
	const src = `
fn make() -> Vec<u32> {
    std::vec::Vec::new()
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/scoped.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byQN := rustMethodByQN(res.Methods)
	make, ok := byQN["make"]
	if !ok {
		t.Fatalf("missing free function make; got %v", rustMethodQNs(res.Methods))
	}
	if !rustStringSlicesEqual(make.Calls, []string{"new"}) {
		t.Errorf("make.Calls = %v; want [new] (rightmost segment of scoped_identifier)",
			make.Calls)
	}
}

// TestTreeSitterRustParser_UseDeclarationVariants pins the
// use-declaration parser across the four canonical shapes:
// single path, grouped list, alias, and glob wildcard.
func TestTreeSitterRustParser_UseDeclarationVariants(t *testing.T) {
	const src = `
use std::fmt::Display;
use std::collections::{HashMap, BTreeSet};
use std::io::Read as IoRead;
use std::prelude::v1::*;
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/uses.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Imports) == 0 {
		t.Fatalf("expected imports; got none")
	}

	// Build a lookup by (Module, Symbols[0]) for easy lookup.
	type key struct{ mod, sym string }
	got := map[key]Import{}
	for _, imp := range res.Imports {
		if len(imp.Symbols) == 0 {
			t.Errorf("import has no symbols: %+v", imp)
			continue
		}
		got[key{imp.Module, imp.Symbols[0]}] = imp
	}

	if _, ok := got[key{"std::fmt", "Display"}]; !ok {
		t.Errorf("missing single import std::fmt::Display; got %v", res.Imports)
	}
	if _, ok := got[key{"std::collections", "HashMap"}]; !ok {
		t.Errorf("missing grouped import std::collections::HashMap; got %v", res.Imports)
	}
	if _, ok := got[key{"std::collections", "BTreeSet"}]; !ok {
		t.Errorf("missing grouped import std::collections::BTreeSet; got %v", res.Imports)
	}
	aliased, ok := got[key{"std::io", "Read"}]
	if !ok {
		t.Errorf("missing aliased import std::io::Read; got %v", res.Imports)
	} else if aliased.Alias != "IoRead" {
		t.Errorf("aliased.Alias = %q; want IoRead", aliased.Alias)
	}
	if _, ok := got[key{"std::prelude::v1", "*"}]; !ok {
		t.Errorf("missing wildcard import std::prelude::v1::*; got %v", res.Imports)
	}
}

// TestTreeSitterRustParser_MacroInvocationIsSkipped pins the
// tech-spec Section 5.5 non-goal: `macro_invocation` items
// must not derail the walk and produce no parser output.
func TestTreeSitterRustParser_MacroInvocationIsSkipped(t *testing.T) {
	const src = `
macro_rules! square {
    ($x:expr) => { $x * $x };
}

println!("hi");

pub struct AfterMacros {}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/macros.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	names := rustClassNamesSorted(res.Classes)
	if !rustStringSlicesEqual(names, []string{"AfterMacros"}) {
		t.Errorf("class names = %v; want [AfterMacros] (macros must be skipped)", names)
	}
}

// TestTreeSitterRustParser_FunctionModifiers pins the
// modifier collector: async/unsafe/const/extern on a function
// surface as Modifiers entries.
func TestTreeSitterRustParser_FunctionModifiers(t *testing.T) {
	const src = `
pub async unsafe fn risky() {}
pub(crate) const fn answer() -> u32 { 42 }
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/modifiers.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byQN := rustMethodByQN(res.Methods)

	risky, ok := byQN["risky"]
	if !ok {
		t.Fatalf("missing risky; got %v", rustMethodQNs(res.Methods))
	}
	for _, m := range []string{"pub", "async", "unsafe"} {
		if !rustContainsString(risky.Modifiers, m) {
			t.Errorf("risky.Modifiers = %v; want to include %q", risky.Modifiers, m)
		}
	}

	answer, ok := byQN["answer"]
	if !ok {
		t.Fatalf("missing answer; got %v", rustMethodQNs(res.Methods))
	}
	// pub(crate) surfaces as a single combined modifier token.
	hasCrate := false
	for _, m := range answer.Modifiers {
		if m == "pub(crate)" {
			hasCrate = true
			break
		}
	}
	if !hasCrate {
		t.Errorf("answer.Modifiers = %v; want to include 'pub(crate)'", answer.Modifiers)
	}
	if !rustContainsString(answer.Modifiers, "const") {
		t.Errorf("answer.Modifiers = %v; want to include 'const'", answer.Modifiers)
	}
}

// TestTreeSitterRustParser_FullFixtureMatchesImplementationPlan
// is the integrated fixture per implementation-plan.md
// §5.3 line 361: trait + struct + trait impl + free
// function + use declaration. Asserts the expected
// class/method/import counts and the cross-cutting
// relationships (Implements edge, trait_default flag,
// trait override flag).
func TestTreeSitterRustParser_FullFixtureMatchesImplementationPlan(t *testing.T) {
	const src = `
use std::fmt::Display;

pub trait Greeter {
    fn greet(&self, name: &str) -> String {
        String::new()
    }
}

pub struct GreeterImpl;

impl Greeter for GreeterImpl {
    fn greet(&self, name: &str) -> String {
        format_greeting(name)
    }
}

pub fn format_greeting(name: &str) -> String {
    String::from(name)
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/lib.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Classes: Greeter (trait) + GreeterImpl (struct) = 2.
	if len(res.Classes) != 2 {
		t.Fatalf("expected 2 class nodes; got %d (%+v)", len(res.Classes), res.Classes)
	}
	byName := rustClassByName(res.Classes)
	if g, ok := byName["GreeterImpl"]; !ok || !rustStringSlicesEqual(g.Implements, []string{"Greeter"}) {
		t.Errorf("GreeterImpl.Implements = %v; want [Greeter]", g.Implements)
	}

	// Methods: Greeter.greet (default) + GreeterImpl.greet
	// (override) + format_greeting (free) = 3.
	if len(res.Methods) != 3 {
		t.Fatalf("expected 3 method nodes; got %d (%+v)", len(res.Methods), res.Methods)
	}
	byQN := rustMethodByQN(res.Methods)

	if gg, ok := byQN["Greeter.greet"]; !ok {
		t.Errorf("missing trait default Greeter.greet")
	} else if gg.LangMeta == nil || gg.LangMeta["trait_default"] != true {
		t.Errorf("Greeter.greet must carry trait_default=true; got %v", gg.LangMeta)
	}
	if ig, ok := byQN["GreeterImpl.greet"]; !ok {
		t.Errorf("missing impl override GreeterImpl.greet")
	} else if ig.LangMeta == nil || ig.LangMeta["trait"] != "Greeter" {
		t.Errorf("GreeterImpl.greet must carry trait=Greeter; got %v", ig.LangMeta)
	} else if !rustStringSlicesEqual(ig.Calls, []string{"format_greeting"}) {
		t.Errorf("GreeterImpl.greet.Calls = %v; want [format_greeting]", ig.Calls)
	}
	if _, ok := byQN["format_greeting"]; !ok {
		t.Errorf("missing free function format_greeting")
	}

	// Imports: one entry for std::fmt::Display.
	if len(res.Imports) != 1 {
		t.Fatalf("expected 1 import; got %d (%+v)", len(res.Imports), res.Imports)
	}
	imp := res.Imports[0]
	if imp.Module != "std::fmt" || len(imp.Symbols) != 1 || imp.Symbols[0] != "Display" {
		t.Errorf("import = %+v; want Module=std::fmt Symbols=[Display]", imp)
	}
}

// ------------------------------------------------------------
// Helpers
// ------------------------------------------------------------

func rustClassByName(in []ClassDecl) map[string]ClassDecl {
	out := make(map[string]ClassDecl, len(in))
	for _, c := range in {
		out[c.QualifiedName] = c
	}
	return out
}

func rustClassNames(in []ClassDecl) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = c.QualifiedName
	}
	return out
}

func rustClassNamesSorted(in []ClassDecl) []string {
	out := rustClassNames(in)
	sort.Strings(out)
	return out
}

func rustMethodByQN(in []MethodDecl) map[string]MethodDecl {
	out := make(map[string]MethodDecl, len(in))
	for _, m := range in {
		out[m.QualifiedName] = m
	}
	return out
}

func rustMethodQNs(in []MethodDecl) []string {
	out := make([]string, len(in))
	for i, m := range in {
		out[i] = m.QualifiedName
	}
	return out
}

func rustStringSlicesEqual(a, b []string) bool {
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

func rustContainsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestTreeSitterRustParser_ImplBeforeStructPopulatesImplements
// pins evaluator iter-3 finding #1: when `impl Trait for Foo`
// appears in source BEFORE `struct Foo;`, the walker MUST
// still populate `Foo.Implements = [Trait]` AND attach
// `LangMeta["trait"]=Trait` to each impl-block method. The
// pre-fix code only mutated `Implements` when the target
// class was already in `classByName`, so impl-first ordering
// silently lost the metadata and the dispatcher's same-file
// `implements` edge with it.
//
// The fix is the `pendingImpls` buffer drained in
// `appendClass`; this test exercises BOTH halves of that
// contract (trait on Implements + LangMeta["trait"] on the
// impl method).
func TestTreeSitterRustParser_ImplBeforeStructPopulatesImplements(t *testing.T) {
	// impl precedes the struct declaration AND precedes the
	// trait declaration; only the trait being declared later
	// in the SAME file matters for the dispatcher's Pass 2a
	// `implements` edge (which Pass-2a looks up via
	// `classNodeID`). Both flush paths get exercised:
	// pending struct flush via `pendingImpls["Greeter"]`,
	// and the trait reference itself which is just a name
	// the dispatcher resolves later.
	const src = `
impl Greeter for G {
    fn greet(&self, name: &str) -> String {
        String::new()
    }
}

pub struct G;

pub trait Greeter {
    fn greet(&self, name: &str) -> String;
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/lib.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	byName := rustClassByName(res.Classes)
	g, ok := byName["G"]
	if !ok {
		t.Fatalf("missing struct G; classes=%v", rustClassNames(res.Classes))
	}
	if !rustStringSlicesEqual(g.Implements, []string{"Greeter"}) {
		t.Errorf("G.Implements = %v; want [Greeter] (impl-before-struct must still populate Implements)",
			g.Implements)
	}

	byQN := rustMethodByQN(res.Methods)
	impl, ok := byQN["G.greet"]
	if !ok {
		t.Fatalf("missing impl method G.greet; methods=%v", rustMethodQNs(res.Methods))
	}
	if impl.LangMeta == nil || impl.LangMeta["trait"] != "Greeter" {
		t.Errorf("G.greet.LangMeta[trait] = %v; want Greeter (impl method must carry trait identity for Pass 2d)",
			impl.LangMeta)
	}
}

// TestTreeSitterRustParser_ImplWithoutDeclIsCrossFile pins
// the cross-file half of evaluator iter-3 finding #1: an
// `impl Trait for Foo` whose target type `Foo` is NEVER
// declared in this file MUST NOT mint a spurious ClassDecl
// for `Foo` (it lives in another file; the cross-file
// resolver story handles that later). The impl-block methods
// ARE emitted with `EnclosingClass="Foo"` AND
// `LangMeta["trait"]=Trait` so the future cross-file resolver
// can still stitch the relationship.
func TestTreeSitterRustParser_ImplWithoutDeclIsCrossFile(t *testing.T) {
	const src = `
impl ExternalTrait for ExternalType {
    fn run(&self) {
        let _ = 1;
    }
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/impls.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Classes) != 0 {
		t.Errorf("cross-file impl must NOT mint a class for ExternalType; got %d classes (%+v)",
			len(res.Classes), res.Classes)
	}
	byQN := rustMethodByQN(res.Methods)
	run, ok := byQN["ExternalType.run"]
	if !ok {
		t.Fatalf("missing impl method ExternalType.run; methods=%v", rustMethodQNs(res.Methods))
	}
	if run.EnclosingClass != "ExternalType" {
		t.Errorf("ExternalType.run.EnclosingClass = %q; want ExternalType",
			run.EnclosingClass)
	}
	if run.LangMeta == nil || run.LangMeta["trait"] != "ExternalTrait" {
		t.Errorf("ExternalType.run.LangMeta[trait] = %v; want ExternalTrait (cross-file resolver needs this)",
			run.LangMeta)
	}
}

// TestTreeSitterRustParser_ImplBeforeStruct_MultipleTraits
// exercises the pendingImpls accumulator: two `impl A for Foo`
// and `impl B for Foo` blocks that both precede `struct Foo;`
// must produce `Foo.Implements = [A, B]` with no duplicates.
func TestTreeSitterRustParser_ImplBeforeStruct_MultipleTraits(t *testing.T) {
	const src = `
impl A for Foo {
    fn one(&self) {}
}

impl B for Foo {
    fn two(&self) {}
}

impl A for Foo {
    fn three(&self) {}
}

pub struct Foo;
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/multi.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byName := rustClassByName(res.Classes)
	foo, ok := byName["Foo"]
	if !ok {
		t.Fatalf("missing struct Foo; classes=%v", rustClassNames(res.Classes))
	}
	got := append([]string(nil), foo.Implements...)
	sort.Strings(got)
	want := []string{"A", "B"}
	if !rustStringSlicesEqual(got, want) {
		t.Errorf("Foo.Implements = %v; want %v (multi-impl pre-decl flush with dedup)",
			got, want)
	}
}

// TestRustFixture_EmitsExpectedNodeAndEdgeSet is the
// implementation-plan §5.3 line 360-362 acceptance test for
// the Rust tree-sitter parser. The fixture is the canonical
// trait+impl+free-function+use shape called out by the
// workstream brief.
//
// Iter-3 STRUCTURAL pivot (evaluator items 3 + 4 demanded
// "production-emitted graph records, not a test-local
// mini-dispatcher"): this test now drives the REAL
// production `Dispatcher.EmitFile` body in dispatcher.go
// (no longer a stub — implements Pass 0/1a/1b/2a/2b/2d) via
// a recording `NodeEdgeWriter` and asserts on the Node /
// Edge records that the dispatcher itself wrote. The
// previous iter-2 test-local `runRustStage53Pipeline`
// helper has been deleted; assertions now run through the
// SAME `NewDispatcher(...).EmitFile(...)` surface that
// production callers use.
//
// Pattern mirrors `TestTypeScriptFixture_EmitsExpectedNodeAndEdgeSet`
// (parser_typescript_test.go:32, gated
// `//go:build canonical_dispatcher`) in spirit while using
// the production-current 3-arg `NewDispatcher(parsers,
// writer, logger)` signature at dispatcher.go:50.
//
// Asserts:
//
//   - 2 class nodes  (Greeter trait, GreeterImpl struct)
//   - 3 method nodes (Greeter.greet trait-default,
//     GreeterImpl.greet impl override, format_greeting free fn)
//   - 1 package node (std::fmt)
//   - 1 implements edge   (GreeterImpl -> Greeter)
//   - 1 static_calls edge (GreeterImpl.greet -> format_greeting)
//   - 1 imports edge      (src/lib.rs -> std::fmt)
//   - 1 overrides edge    (GreeterImpl.greet -> Greeter.greet)
//
// Parser-level Imports defense-in-depth is kept at the end
// of the test so a regression that strips Symbols=[Display]
// from the Import record (which the dispatcher folds into
// the imports edge attrs_json) is caught even if the
// dispatcher silently drops the symbols slice when
// projecting the Import into an edge.
func TestRustFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	// Fixture matches the Stage 5.3 workstream brief
	// verbatim: trait/struct lack ``pub`` so a regression
	// that silently drops non-public items is caught here
	// (iter-1 rubber-duck finding #1).
	const filename = "src/lib.rs"
	const src = `use std::fmt::Display;

trait Greeter {
    fn greet(&self, name: &str) -> String {
        String::new()
    }
}

struct GreeterImpl;

impl Greeter for GreeterImpl {
    fn greet(&self, name: &str) -> String {
        format_greeting(name)
    }
}

pub fn format_greeting(name: &str) -> String {
    String::from(name)
}
`
	fw := newRustStage53Writer()
	d := NewDispatcher([]Parser{NewTreeSitterRustParser()}, fw, nil)
	emitRes, err := d.EmitFile(filename, []byte(src))
	if err != nil {
		t.Fatalf("Dispatcher.EmitFile(%q) error = %v; want nil", filename, err)
	}

	// EmitResult counters must match what the writer recorded
	// (sanity check on the dispatcher's internal accounting).
	if emitRes.NodeCount != len(fw.nodes) {
		t.Errorf("EmitResult.NodeCount = %d; want %d (writer recorded)", emitRes.NodeCount, len(fw.nodes))
	}
	if emitRes.EdgeCount != len(fw.edges) {
		t.Errorf("EmitResult.EdgeCount = %d; want %d (writer recorded)", emitRes.EdgeCount, len(fw.edges))
	}

	// ----- Node assertions -----

	classNodes := rustStage53NodesByKind(fw.nodes, "class")
	if got := len(classNodes); got != 2 {
		t.Fatalf("class nodes = %d; want 2; got=%v", got, classNodes)
	}
	gotClassNames := make([]string, 0, len(classNodes))
	for _, n := range classNodes {
		gotClassNames = append(gotClassNames, n.Name)
	}
	sort.Strings(gotClassNames)
	if !rustStringSlicesEqual(gotClassNames, []string{"Greeter", "GreeterImpl"}) {
		t.Errorf("class node names = %v; want [Greeter GreeterImpl]", gotClassNames)
	}

	methodNodes := rustStage53NodesByKind(fw.nodes, "method")
	if got := len(methodNodes); got != 3 {
		t.Fatalf("method nodes = %d; want 3; got=%v", got, methodNodes)
	}
	gotMethodNames := make([]string, 0, len(methodNodes))
	for _, n := range methodNodes {
		gotMethodNames = append(gotMethodNames, n.Name)
	}
	sort.Strings(gotMethodNames)
	if !rustStringSlicesEqual(gotMethodNames, []string{"Greeter.greet", "GreeterImpl.greet", "format_greeting"}) {
		t.Errorf("method node names = %v; want [Greeter.greet GreeterImpl.greet format_greeting] (sorted)",
			gotMethodNames)
	}

	packageNodes := rustStage53NodesByKind(fw.nodes, "package")
	if got := len(packageNodes); got != 1 {
		t.Fatalf("package nodes = %d; want 1; got=%v", got, packageNodes)
	}
	if packageNodes[0].Name != "std::fmt" {
		t.Errorf("package node name = %q; want %q (Module from `use std::fmt::Display;`)",
			packageNodes[0].Name, "std::fmt")
	}

	// ----- Edge assertions -----

	implEdges := rustStage53EdgesByKind(fw.edges, "implements")
	if got := len(implEdges); got != 1 {
		t.Fatalf("implements edges = %d; want exactly 1; got=%v", got, implEdges)
	}
	if implEdges[0].Source != "GreeterImpl" || implEdges[0].Target != "Greeter" {
		t.Errorf("implements edge = %+v; want {Source:GreeterImpl Target:Greeter} (impl Greeter for GreeterImpl)",
			implEdges[0])
	}

	callEdges := rustStage53EdgesByKind(fw.edges, "static_calls")
	if got := len(callEdges); got != 1 {
		t.Fatalf("static_calls edges = %d; want exactly 1 (only GreeterImpl.greet -> format_greeting is unambiguously locally resolvable); got=%v",
			got, callEdges)
	}
	if callEdges[0].Source != "GreeterImpl.greet" || callEdges[0].Target != "format_greeting" {
		t.Errorf("static_calls edge = %+v; want {Source:GreeterImpl.greet Target:format_greeting}",
			callEdges[0])
	}

	importEdges := rustStage53EdgesByKind(fw.edges, "imports")
	if got := len(importEdges); got != 1 {
		t.Fatalf("imports edges = %d; want exactly 1; got=%v", got, importEdges)
	}
	if importEdges[0].Source != filename || importEdges[0].Target != "std::fmt" {
		t.Errorf("imports edge = %+v; want {Source:%s Target:std::fmt}",
			importEdges[0], filename)
	}

	overrideEdges := rustStage53EdgesByKind(fw.edges, "overrides")
	if got := len(overrideEdges); got != 1 {
		t.Fatalf("overrides edges = %d; want exactly 1 (Pass 2d: impl method -> trait-default method, same file); got=%v",
			got, overrideEdges)
	}
	if overrideEdges[0].Source != "GreeterImpl.greet" || overrideEdges[0].Target != "Greeter.greet" {
		t.Errorf("overrides edge = %+v; want {Source:GreeterImpl.greet Target:Greeter.greet}",
			overrideEdges[0])
	}

	// ----- Total-count guards (catch spurious extra Node/Edge kinds) -----
	// The brief's exact graph is: 6 nodes (2 class + 3 method +
	// 1 package) and 4 edges (1 implements + 1 static_calls +
	// 1 imports + 1 overrides). Per-kind counts above pin
	// each EXPECTED kind; these total-count guards pin the
	// emission set as a CLOSED set so a regression that
	// emitted an UNEXPECTED kind (e.g. a phantom `extends` or
	// `reads` edge, or a spurious node kind) lights up here.
	if got := len(fw.nodes); got != 6 {
		t.Errorf("total nodes = %d; want 6 (2 class + 3 method + 1 package); got=%v", got, fw.nodes)
	}
	if got := len(fw.edges); got != 4 {
		t.Errorf("total edges = %d; want 4 (1 implements + 1 static_calls + 1 imports + 1 overrides); got=%v",
			got, fw.edges)
	}

	// ----- Parser-level Imports defense-in-depth -----
	// The dispatcher's imports edge target is `imp.Module`
	// (target = "std::fmt") but `Symbols=["Display"]` is
	// carried on the parser-level Import record for the
	// future imports-edge `attrs_json` integration. A
	// regression that stripped the Symbols slice would not
	// be caught by the writer-emitted edge alone, so we
	// re-parse here and pin the parser-level invariant.
	parserRes, err := NewTreeSitterRustParser().Parse(filename, []byte(src))
	if err != nil {
		t.Fatalf("parser.Parse for defense-in-depth: %v", err)
	}
	if got := len(parserRes.Imports); got != 1 {
		t.Fatalf("ParseResult.Imports len = %d; want 1 (std::fmt::Display)", got)
	}
	imp := parserRes.Imports[0]
	if imp.Module != "std::fmt" {
		t.Errorf("ParseResult.Imports[0].Module = %q; want %q", imp.Module, "std::fmt")
	}
	if !rustStringSlicesEqual(imp.Symbols, []string{"Display"}) {
		t.Errorf("ParseResult.Imports[0].Symbols = %v; want [Display] (brief requires Symbols=[\"Display\"] on the imports edge)",
			imp.Symbols)
	}
	if imp.Alias != "" {
		t.Errorf("ParseResult.Imports[0].Alias = %q; want empty (no `as` clause in the fixture)", imp.Alias)
	}
}

// TestRustFixture_OverridesEdgeFromImplToTraitDefault pins
// implementation-plan §5.3 line 363: exactly ONE `overrides`
// edge is emitted from the impl method to the trait-default
// trait method on the same file.
//
// Iter-3 STRUCTURAL pivot: drives the REAL production
// dispatcher (no test-local mini-pipeline) so the override
// edge count assertion provably reflects what the
// production `Dispatcher.EmitFile` body emits to a
// `NodeEdgeWriter`. Parser-level LangMeta defense-in-depth
// is kept at the end so a regression in EITHER the parser
// walker's `LangMeta["trait"/"trait_default"]` writes OR
// the dispatcher's Pass 2d resolver lights up here.
func TestRustFixture_OverridesEdgeFromImplToTraitDefault(t *testing.T) {
	// Fixture matches the Stage 5.3 workstream brief
	// verbatim: trait/struct lack ``pub`` so a regression
	// that silently drops non-public items is caught here
	// (iter-1 rubber-duck finding #1).
	const filename = "src/lib.rs"
	const src = `use std::fmt::Display;

trait Greeter {
    fn greet(&self, name: &str) -> String {
        String::new()
    }
}

struct GreeterImpl;

impl Greeter for GreeterImpl {
    fn greet(&self, name: &str) -> String {
        format_greeting(name)
    }
}

pub fn format_greeting(name: &str) -> String {
    String::from(name)
}
`
	fw := newRustStage53Writer()
	d := NewDispatcher([]Parser{NewTreeSitterRustParser()}, fw, nil)
	if _, err := d.EmitFile(filename, []byte(src)); err != nil {
		t.Fatalf("Dispatcher.EmitFile: %v", err)
	}

	overrides := rustStage53EdgesByKind(fw.edges, "overrides")
	if got := len(overrides); got != 1 {
		t.Fatalf("overrides edges = %d; want exactly 1 (impl-plan §5.3:363); got=%v",
			got, overrides)
	}
	if overrides[0].Source != "GreeterImpl.greet" || overrides[0].Target != "Greeter.greet" {
		t.Errorf("overrides edge endpoints = %+v; want {Source:GreeterImpl.greet Target:Greeter.greet}",
			overrides[0])
	}

	// ----- Parser-level LangMeta defense-in-depth -----
	parserRes, err := NewTreeSitterRustParser().Parse(filename, []byte(src))
	if err != nil {
		t.Fatalf("parser.Parse for defense-in-depth: %v", err)
	}
	byMethod := rustMethodByQN(parserRes.Methods)

	// (a) trait_default proxy on the trait method.
	traitGreet, ok := byMethod["Greeter.greet"]
	if !ok {
		t.Fatalf("trait method Greeter.greet missing; got %v", rustMethodQNs(parserRes.Methods))
	}
	if traitGreet.LangMeta == nil || traitGreet.LangMeta["trait_default"] != true {
		t.Errorf("Greeter.greet.LangMeta[trait_default] = %v; want true (Pass 2d eligibility on TRAIT side)",
			traitGreet.LangMeta)
	}
	if traitGreet.EnclosingClass != "Greeter" {
		t.Errorf("Greeter.greet.EnclosingClass = %q; want \"Greeter\" (Pass 2d trait-side lookup uses EnclosingClass)",
			traitGreet.EnclosingClass)
	}

	// (b) trait proxy on the impl method.
	implGreet, ok := byMethod["GreeterImpl.greet"]
	if !ok {
		t.Fatalf("impl method GreeterImpl.greet missing; got %v", rustMethodQNs(parserRes.Methods))
	}
	if implGreet.LangMeta == nil || implGreet.LangMeta["trait"] != "Greeter" {
		t.Errorf("GreeterImpl.greet.LangMeta[trait] = %v; want \"Greeter\" (Pass 2d eligibility on IMPL side)",
			implGreet.LangMeta)
	}
	if implGreet.EnclosingClass != "GreeterImpl" {
		t.Errorf("GreeterImpl.greet.EnclosingClass = %q; want \"GreeterImpl\" (Pass 2d impl-side lookup uses EnclosingClass)",
			implGreet.EnclosingClass)
	}

	// (c) the free function MUST NOT carry either
	// eligibility key — otherwise the dispatcher's Pass
	// 2d would attempt a methodNodeID lookup for
	// `<trait>.format_greeting` and could spuriously
	// match an unrelated trait method elsewhere.
	free, ok := byMethod["format_greeting"]
	if !ok {
		t.Fatalf("free function format_greeting missing; got %v", rustMethodQNs(parserRes.Methods))
	}
	if free.LangMeta != nil {
		if _, has := free.LangMeta["trait"]; has {
			t.Errorf("format_greeting.LangMeta[trait] must NOT be set on a free function; got %v", free.LangMeta)
		}
		if _, has := free.LangMeta["trait_default"]; has {
			t.Errorf("format_greeting.LangMeta[trait_default] must NOT be set on a free function; got %v", free.LangMeta)
		}
	}
}

// TestRustFixture_OverridesCrossFileMissIsSilent pins
// implementation-plan §5.3 line 364: EXACTLY ZERO
// `overrides` edges are emitted when the trait declaration
// is not in the local file (A4 rule, architecture
// Section 7.2). The impl method still carries
// `LangMeta["trait"]="Greeter"` so a future cross-file
// resolver can stitch it later, but the dispatcher's
// Pass 2d methodNodeID lookup misses and MUST drop the
// override silently.
//
// Iter-3 STRUCTURAL pivot: drives the REAL production
// dispatcher (no test-local mini-pipeline) so the zero-
// count assertion provably reflects production silent-drop
// behaviour, not a test-local replica of it. Adds matching
// zero-count guards for `implements` (cross-file trait
// target → no edge) and `static_calls` (`String::from(name)`
// rightmost-segment "from" has no local callee → no edge).
func TestRustFixture_OverridesCrossFileMissIsSilent(t *testing.T) {
	// `struct GreeterImpl;` is intentionally NOT prefixed
	// with `pub` so the test matches the Stage 5.3 brief
	// convention and would catch a regression that drops
	// non-public items (iter-1 rubber-duck finding #1).
	const filename = "src/lib.rs"
	const src = `struct GreeterImpl;

impl Greeter for GreeterImpl {
    fn greet(&self, name: &str) -> String {
        String::from(name)
    }
}
`
	fw := newRustStage53Writer()
	d := NewDispatcher([]Parser{NewTreeSitterRustParser()}, fw, nil)
	if _, err := d.EmitFile(filename, []byte(src)); err != nil {
		t.Fatalf("Dispatcher.EmitFile: %v", err)
	}

	// ZERO overrides edges — A4 silent-drop rule.
	overrides := rustStage53EdgesByKind(fw.edges, "overrides")
	if got := len(overrides); got != 0 {
		t.Fatalf("overrides edges = %d; want exactly 0 (cross-file trait MUST NOT mint an override edge; A4 rule); got=%v",
			got, overrides)
	}

	// ZERO implements edges — Pass 2a drops unresolved
	// (cross-file) trait targets per the same A4 rule.
	implEdges := rustStage53EdgesByKind(fw.edges, "implements")
	if got := len(implEdges); got != 0 {
		t.Fatalf("implements edges = %d; want exactly 0 (cross-file trait target MUST be dropped by Pass 2a); got=%v",
			got, implEdges)
	}

	// ZERO static_calls edges — `String::from(name)` is
	// recorded as rightmost-segment "from" in Calls, and
	// no local method has that simple name, so Pass 2b's
	// bare-name resolver drops it.
	callEdges := rustStage53EdgesByKind(fw.edges, "static_calls")
	if got := len(callEdges); got != 0 {
		t.Fatalf("static_calls edges = %d; want exactly 0 (no local callee matches the bare name \"from\"); got=%v",
			got, callEdges)
	}

	// Node counts: only local items minted.
	classNodes := rustStage53NodesByKind(fw.nodes, "class")
	if got := len(classNodes); got != 1 || classNodes[0].Name != "GreeterImpl" {
		t.Fatalf("class nodes = %v; want exactly [{class GreeterImpl}] (cross-file trait MUST NOT be minted)",
			classNodes)
	}
	methodNodes := rustStage53NodesByKind(fw.nodes, "method")
	if got := len(methodNodes); got != 1 || methodNodes[0].Name != "GreeterImpl.greet" {
		t.Fatalf("method nodes = %v; want exactly [{method GreeterImpl.greet}] (cross-file trait method MUST NOT be minted)",
			methodNodes)
	}

	// ----- Total-count guards (catch spurious extra Node/Edge kinds) -----
	// The cross-file fixture has NO `use`, NO local trait,
	// NO local callee match — so the COMPLETE graph is
	// exactly 2 nodes (1 class + 1 method) and 0 edges.
	// A regression that emitted an unexpected kind (e.g. a
	// phantom imports/extends/overrides edge, or minted the
	// cross-file trait as a node) lights up here.
	if got := len(fw.nodes); got != 2 {
		t.Errorf("total nodes = %d; want 2 (1 class + 1 method); got=%v", got, fw.nodes)
	}
	if got := len(fw.edges); got != 0 {
		t.Errorf("total edges = %d; want 0 (cross-file references must NOT mint any local edge); got=%v",
			got, fw.edges)
	}

	// ----- Parser-level LangMeta defense-in-depth -----
	// The IMPL side must STILL carry LangMeta["trait"]
	// ="Greeter" so a future cross-file resolver can
	// re-attempt the stitch; the dispatcher's same-file
	// Pass 2d lookup simply misses.
	parserRes, err := NewTreeSitterRustParser().Parse(filename, []byte(src))
	if err != nil {
		t.Fatalf("parser.Parse for defense-in-depth: %v", err)
	}
	byMethod := rustMethodByQN(parserRes.Methods)
	implGreet, ok := byMethod["GreeterImpl.greet"]
	if !ok {
		t.Fatalf("impl method GreeterImpl.greet missing; got %v", rustMethodQNs(parserRes.Methods))
	}
	if implGreet.LangMeta == nil || implGreet.LangMeta["trait"] != "Greeter" {
		t.Errorf("GreeterImpl.greet.LangMeta[trait] = %v; want \"Greeter\" (cross-file resolver needs this key even when same-file Pass 2d misses)",
			implGreet.LangMeta)
	}
	// Defense-in-depth: no phantom trait method minted.
	if _, ok := byMethod["Greeter.greet"]; ok {
		t.Errorf("phantom trait method Greeter.greet was minted; got %v",
			rustMethodQNs(parserRes.Methods))
	}
	for _, m := range parserRes.Methods {
		if m.EnclosingClass == "Greeter" {
			t.Errorf("method %q has EnclosingClass=%q (cross-file trait must not enclose any local method)",
				m.QualifiedName, m.EnclosingClass)
		}
	}
}

// ---------------------------------------------------------------------------
// Stage 5.3 test-local fake writer
// ---------------------------------------------------------------------------
//
// `rustStage53Writer` is a recording `NodeEdgeWriter` (the
// dispatcher's writer-side surface at dispatcher.go:17). It
// captures every (Kind, Name) tuple `InsertNode` receives and
// every (Kind, Source, Target) tuple `InsertEdge` receives so
// the Stage 5.3 tests can assert on the production-emitted
// graph records.
//
// Iter-2's `runRustStage53Pipeline` mini-dispatcher has been
// DELETED — the production `Dispatcher.EmitFile` body in
// dispatcher.go is no longer a stub (Pass 0/1a/1b/2a/2b/2d
// land in this iter), so a test-local replica is both
// unnecessary and (per evaluator iter-2 items 3 + 4)
// counter-productive: it would let the test pass even if the
// production dispatcher continued to discard ParseResult.
type rustStage53Writer struct {
	nodes []Node
	edges []Edge
}

func newRustStage53Writer() *rustStage53Writer {
	return &rustStage53Writer{}
}

func (w *rustStage53Writer) InsertNode(n Node) error {
	w.nodes = append(w.nodes, n)
	return nil
}

func (w *rustStage53Writer) InsertEdge(e Edge) error {
	w.edges = append(w.edges, e)
	return nil
}

// rustStage53NodesByKind returns the subset of nodes
// matching the given Kind, preserving emission order.
func rustStage53NodesByKind(nodes []Node, kind string) []Node {
	var out []Node
	for _, n := range nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

// rustStage53EdgesByKind returns the subset of edges
// matching the given Kind, preserving emission order.
func rustStage53EdgesByKind(edges []Edge, kind string) []Edge {
	var out []Edge
	for _, e := range edges {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}
