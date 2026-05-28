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
