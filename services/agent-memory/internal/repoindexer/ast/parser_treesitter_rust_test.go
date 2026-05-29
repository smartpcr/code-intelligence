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
// workstream brief; the parser must surface every node and
// every relationship the dispatcher's Pass 1-2d passes need
// to mint the corresponding graph edges:
//
//   - "2 class nodes" -> 2 ClassDecl entries
//     (Greeter trait, GreeterImpl struct)
//   - "3 method nodes" -> 3 MethodDecl entries
//     (Greeter.greet trait-default, GreeterImpl.greet impl
//     override, format_greeting free function)
//   - "1 implements edge" -> the GreeterImpl ClassDecl
//     carries `Implements=["Greeter"]` (dispatcher Pass 2a
//     mints the edge from this slice)
//   - "1 static_calls edge" -> GreeterImpl.greet MethodDecl
//     carries `Calls=["format_greeting"]` (dispatcher Pass 2b
//     resolves bare-name calls against the same-file callee
//     index to mint the edge)
//   - "1 imports edge" -> a single Import entry with
//     Module="std::fmt" and Symbols=["Display"] (dispatcher
//     Pass 0/2a mints the synthetic package node + edge)
//
// Mirrors TestCSharpFixture_EmitsExpectedNodeAndEdgeSet (Stage
// 4.3) and TestCFixture_EmitsExpectedNodeAndEdgeSet (Stage
// 3.4): the "edge" terminology in the workstream brief maps
// to the parser-level ParseResult fields the dispatcher
// consumes. The end-to-end dispatcher-emitted Edge set lives
// in parser_treesitter_rust_dispatcher_test.go
// (TestDispatcherFixture_RustGraph_StageFiveThree, gated on
// `//go:build cgo && canonical_dispatcher`).
func TestRustFixture_EmitsExpectedNodeAndEdgeSet(t *testing.T) {
	// Fixture matches the Stage 5.3 workstream brief
	// verbatim: trait/struct lack ``pub`` so a regression
	// that silently drops non-public items is caught here
	// (per rubber-duck iter-1 finding #1).
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
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/lib.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// ----- Class nodes (2): Greeter trait + GreeterImpl struct.
	if got := len(res.Classes); got != 2 {
		t.Fatalf("expected 2 class nodes; got %d (%v)",
			got, rustClassNames(res.Classes))
	}
	byClass := rustClassByName(res.Classes)
	greeter, ok := byClass["Greeter"]
	if !ok {
		t.Fatalf("class %q missing; got %v", "Greeter", rustClassNames(res.Classes))
	}
	if greeter.Kind != "trait" {
		t.Errorf("Greeter.Kind = %q; want %q", greeter.Kind, "trait")
	}
	greeterImpl, ok := byClass["GreeterImpl"]
	if !ok {
		t.Fatalf("class %q missing; got %v", "GreeterImpl", rustClassNames(res.Classes))
	}
	if greeterImpl.Kind != "struct" {
		t.Errorf("GreeterImpl.Kind = %q; want %q", greeterImpl.Kind, "struct")
	}

	// ----- Method nodes (3): Greeter.greet + GreeterImpl.greet + format_greeting.
	if got := len(res.Methods); got != 3 {
		t.Fatalf("expected 3 method nodes; got %d (%v)",
			got, rustMethodQNs(res.Methods))
	}
	byMethod := rustMethodByQN(res.Methods)
	for _, want := range []string{"Greeter.greet", "GreeterImpl.greet", "format_greeting"} {
		if _, ok := byMethod[want]; !ok {
			t.Errorf("method %q missing; got %v", want, rustMethodQNs(res.Methods))
		}
	}

	// ----- Implements edge (GreeterImpl -> Greeter): one entry on GreeterImpl.Implements.
	if !rustStringSlicesEqual(greeterImpl.Implements, []string{"Greeter"}) {
		t.Errorf("GreeterImpl.Implements = %v; want [Greeter] (parser proxy for the implements edge)",
			greeterImpl.Implements)
	}
	// Greeter trait MUST NOT itself carry an Implements
	// (it IS the target of an implements edge, not the
	// source). A walker that conflates impl-block traits
	// into the trait's own Implements would falsely
	// produce an extra implements edge.
	if len(greeter.Implements) != 0 {
		t.Errorf("Greeter.Implements = %v; want [] (the trait is the implements TARGET, not the source)",
			greeter.Implements)
	}

	// ----- static_calls edge (GreeterImpl.greet -> format_greeting):
	// one bare-name entry on GreeterImpl.greet.Calls.
	implGreet := byMethod["GreeterImpl.greet"]
	if !rustStringSlicesEqual(implGreet.Calls, []string{"format_greeting"}) {
		t.Errorf("GreeterImpl.greet.Calls = %v; want [format_greeting] (parser proxy for static_calls edge)",
			implGreet.Calls)
	}
	// The body has NO self-qualified calls and NO
	// receiver method calls, so ReceiverCalls must stay
	// empty — otherwise the dispatcher's Pass 2b would
	// mint a SECOND static_calls edge and break the "1
	// static_calls edge" assertion.
	if len(implGreet.ReceiverCalls) != 0 {
		t.Errorf("GreeterImpl.greet.ReceiverCalls = %v; want [] (no self.X() in the fixture body)",
			implGreet.ReceiverCalls)
	}
	// The trait-default Greeter.greet body calls
	// `String::new()` which the parser records as the
	// rightmost-segment "new" in Calls. That bare name
	// is NOT a same-file callee, so the dispatcher's
	// Pass 2b drops it -> still only 1 static_calls edge
	// total. We deliberately do NOT assert
	// Greeter.greet.Calls here so a future scoped-call
	// refactor (e.g. dropping rightmost-segment recording)
	// does not spuriously break this fixture; the
	// dispatcher-level test
	// TestDispatcherFixture_RustGraph_StageFiveThree
	// pins the final edge count of 1.

	// ----- imports edge (file -> std::fmt package, Symbols=[Display]):
	// one Import entry with the precise Module + Symbols shape.
	if got := len(res.Imports); got != 1 {
		t.Fatalf("expected 1 import (std::fmt::Display); got %d (%v)",
			got, res.Imports)
	}
	imp := res.Imports[0]
	if imp.Module != "std::fmt" {
		t.Errorf("import.Module = %q; want %q", imp.Module, "std::fmt")
	}
	if !rustStringSlicesEqual(imp.Symbols, []string{"Display"}) {
		t.Errorf("import.Symbols = %v; want [Display]", imp.Symbols)
	}
	if imp.Alias != "" {
		t.Errorf("import.Alias = %q; want empty (no `as` clause in the fixture)", imp.Alias)
	}
}

// TestRustFixture_OverridesEdgeFromImplToTraitDefault pins
// the implementation-plan §5.3 line 363 acceptance test for
// the Pass 2d overrides edge. The dispatcher's Pass 2d emits
// one `overrides` edge per impl method that satisfies BOTH:
//
//  1. MethodDecl.LangMeta["trait"] == <TraitName>
//     (i.e. the method was declared inside an
//     `impl Trait for Foo` block), AND
//  2. the same-file methodNodeID lookup for
//     `<TraitName>.<methodName>` succeeds AND the resolved
//     trait method carries LangMeta["trait_default"] == true
//     (i.e. the trait declared a DEFAULT body for the method,
//     making the impl method an "override" rather than a
//     "satisfies-required-spec").
//
// The fixture matches the workstream brief verbatim. Asserts
// the parser-level proxies for (1) and (2) so a regression in
// either LangMeta key surfaces here even when the test runs
// without the canonical_dispatcher build tag. The
// end-to-end overrides edge count = 1 lives in
// TestDispatcherFixture_RustGraph_StageFiveThree
// (parser_treesitter_rust_dispatcher_test.go,
// //go:build cgo && canonical_dispatcher).
func TestRustFixture_OverridesEdgeFromImplToTraitDefault(t *testing.T) {
	// Fixture matches the Stage 5.3 workstream brief
	// verbatim: trait/struct lack ``pub`` so a regression
	// that silently drops non-public items is caught here
	// (per rubber-duck iter-1 finding #1).
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
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/lib.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	byMethod := rustMethodByQN(res.Methods)

	// (1) trait_default proxy on the trait method.
	traitGreet, ok := byMethod["Greeter.greet"]
	if !ok {
		t.Fatalf("trait method Greeter.greet missing; got %v", rustMethodQNs(res.Methods))
	}
	if traitGreet.LangMeta == nil || traitGreet.LangMeta["trait_default"] != true {
		t.Errorf("Greeter.greet.LangMeta[trait_default] = %v; want true (Pass 2d eligibility key on the TRAIT side)",
			traitGreet.LangMeta)
	}
	if traitGreet.EnclosingClass != "Greeter" {
		t.Errorf("Greeter.greet.EnclosingClass = %q; want \"Greeter\" (Pass 2d resolves the trait-side endpoint via EnclosingClass for the same-file methodNodeID lookup)",
			traitGreet.EnclosingClass)
	}

	// (2) trait proxy on the impl method.
	implGreet, ok := byMethod["GreeterImpl.greet"]
	if !ok {
		t.Fatalf("impl method GreeterImpl.greet missing; got %v", rustMethodQNs(res.Methods))
	}
	if implGreet.LangMeta == nil || implGreet.LangMeta["trait"] != "Greeter" {
		t.Errorf("GreeterImpl.greet.LangMeta[trait] = %v; want \"Greeter\" (Pass 2d eligibility key on the IMPL side)",
			implGreet.LangMeta)
	}
	if implGreet.EnclosingClass != "GreeterImpl" {
		t.Errorf("GreeterImpl.greet.EnclosingClass = %q; want \"GreeterImpl\" (Pass 2d resolves the impl-side endpoint via EnclosingClass)",
			implGreet.EnclosingClass)
	}

	// The free function MUST NOT carry either Pass 2d
	// eligibility key — otherwise the dispatcher's Pass
	// 2d would attempt a methodNodeID lookup for
	// `<trait>.format_greeting` and could spuriously
	// match an unrelated trait method elsewhere in the
	// same file.
	free, ok := byMethod["format_greeting"]
	if !ok {
		t.Fatalf("free function format_greeting missing; got %v", rustMethodQNs(res.Methods))
	}
	if free.LangMeta != nil {
		if _, hasTrait := free.LangMeta["trait"]; hasTrait {
			t.Errorf("format_greeting.LangMeta[trait] must NOT be set on a free function; got %v",
				free.LangMeta)
		}
		if _, hasDefault := free.LangMeta["trait_default"]; hasDefault {
			t.Errorf("format_greeting.LangMeta[trait_default] must NOT be set on a free function; got %v",
				free.LangMeta)
		}
	}
}

// TestRustFixture_OverridesCrossFileMissIsSilent pins the
// implementation-plan §5.3 line 364 acceptance test for the
// cross-file overrides miss path (A4 rule in architecture
// Section 7.2). The fixture contains an `impl Trait for Foo`
// where the trait `Greeter` is NOT declared in the same file
// — the parser must still tag the impl method with
// `LangMeta["trait"]="Greeter"` (so a future cross-file
// resolver can stitch the relationship) BUT the dispatcher's
// Pass 2d methodNodeID lookup for `Greeter.greet` will miss
// because the trait method is not in this file's symbol
// table, and Pass 2d MUST drop the override silently rather
// than mint a dangling edge.
//
// The parser-level assertion: the IMPL side still carries
// `LangMeta["trait"]="Greeter"` (so cross-file resolution can
// re-attempt later); the TRAIT side has no class/method node
// in the local ParseResult (so the dispatcher's same-file
// methodNodeID lookup will miss as required).
//
// The end-to-end "zero overrides edge" assertion lives in
// TestDispatcherFixture_RustGraph_CrossFileNoOverrides
// (parser_treesitter_rust_dispatcher_test.go,
// //go:build cgo && canonical_dispatcher).
func TestRustFixture_OverridesCrossFileMissIsSilent(t *testing.T) {
	// `struct GreeterImpl;` is intentionally NOT prefixed
	// with `pub` so the test matches the Stage 5.3 brief
	// convention and would catch a regression that drops
	// non-public items (per rubber-duck iter-1 finding #1).
	const src = `struct GreeterImpl;

impl Greeter for GreeterImpl {
    fn greet(&self, name: &str) -> String {
        String::from(name)
    }
}
`
	p := NewTreeSitterRustParser()
	res, err := p.Parse("src/lib.rs", []byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// Tight exact-count guard: only GreeterImpl is local;
	// the cross-file Greeter trait MUST NOT be minted as a
	// class node (per rubber-duck iter-1 finding #2).
	if got := len(res.Classes); got != 1 {
		t.Fatalf("expected 1 class node (local GreeterImpl only); got %d (%v)",
			got, rustClassNames(res.Classes))
	}

	// The trait MUST NOT be minted as a class node (it lives
	// in another file). Only GreeterImpl is declared here.
	byClass := rustClassByName(res.Classes)
	if _, ok := byClass["Greeter"]; ok {
		t.Errorf("cross-file trait Greeter was minted as a class node; classes=%v",
			rustClassNames(res.Classes))
	}
	if _, ok := byClass["GreeterImpl"]; !ok {
		t.Fatalf("local struct GreeterImpl missing; classes=%v", rustClassNames(res.Classes))
	}

	// Tight exact-count guard: only the impl-side
	// GreeterImpl.greet must exist; a regression that
	// minted a phantom trait method would push this to 2.
	if got := len(res.Methods); got != 1 {
		t.Fatalf("expected 1 method node (impl-side GreeterImpl.greet only); got %d (%v)",
			got, rustMethodQNs(res.Methods))
	}

	// The impl method MUST still carry the trait proxy so
	// the cross-file resolver can stitch it later; the
	// dispatcher's Pass 2d will simply miss in
	// methodNodeID and drop the edge silently.
	byMethod := rustMethodByQN(res.Methods)
	implGreet, ok := byMethod["GreeterImpl.greet"]
	if !ok {
		t.Fatalf("impl method GreeterImpl.greet missing; got %v", rustMethodQNs(res.Methods))
	}
	if implGreet.LangMeta == nil || implGreet.LangMeta["trait"] != "Greeter" {
		t.Errorf("GreeterImpl.greet.LangMeta[trait] = %v; want \"Greeter\" (cross-file resolver needs this even when same-file Pass 2d misses)",
			implGreet.LangMeta)
	}

	// No trait method in the local ParseResult — the
	// dispatcher's `Greeter.greet` methodNodeID lookup
	// will miss as the A4 rule requires. We assert the
	// absence of `Greeter.greet` (and the absence of any
	// method node whose EnclosingClass is the cross-file
	// trait) so a regression that minted a phantom trait
	// method is caught.
	if _, ok := byMethod["Greeter.greet"]; ok {
		t.Errorf("phantom trait method Greeter.greet was minted; got %v",
			rustMethodQNs(res.Methods))
	}
	for _, m := range res.Methods {
		if m.EnclosingClass == "Greeter" {
			t.Errorf("method %q has EnclosingClass=%q (cross-file trait must not enclose any local method)",
				m.QualifiedName, m.EnclosingClass)
		}
	}
}
