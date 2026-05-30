package recipes_test

import (
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	astv1 "github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/v1"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestInterfaceWidthRecipe_MetricKindIsCanonical pins the
// literal `interface_width` spelling (NOT `class_width`,
// `wmc`, or `method_count` -- the closed-set guard forbids
// long-form aliases).
func TestInterfaceWidthRecipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	if got := recipes.NewInterfaceWidthRecipe().MetricKind(); got != "interface_width" {
		t.Fatalf("MetricKind() = %q, want %q", got, "interface_width")
	}
}

// TestInterfaceWidthRecipe_VersionStartsAtOne pins v1.
func TestInterfaceWidthRecipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	if got := recipes.NewInterfaceWidthRecipe().Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1", got)
	}
}

// TestInterfaceWidthRecipe_PackIsSolid -- the architecture
// Sec 1.4.1 row 8 pack column pins `solid`.
func TestInterfaceWidthRecipe_PackIsSolid(t *testing.T) {
	t.Parallel()
	if got := recipes.NewInterfaceWidthRecipe().Pack(); got != recipes.PackSolid {
		t.Fatalf("Pack() = %q, want %q", got, recipes.PackSolid)
	}
}

// TestInterfaceWidthRecipe_AppliesTo_NilAstReturnsFalse --
// nil is refused.
func TestInterfaceWidthRecipe_AppliesTo_NilAstReturnsFalse(t *testing.T) {
	t.Parallel()
	if recipes.NewInterfaceWidthRecipe().AppliesTo(nil) {
		t.Fatalf("AppliesTo(nil) = true, want false")
	}
}

// TestInterfaceWidthRecipe_AppliesTo_DegradedAstReturnsFalse
// -- a parser that bailed mid-file would silently undercount
// methods; the recipe MUST skip rather than emit a
// misleading value.
func TestInterfaceWidthRecipe_AppliesTo_DegradedAstReturnsFalse(t *testing.T) {
	t.Parallel()
	ast := newAstBuilder("foo.go", false).build()
	ast.DegradedReason = "parser-panic"
	if recipes.NewInterfaceWidthRecipe().AppliesTo(ast) {
		t.Fatalf("AppliesTo on degraded AST = true, want false")
	}
}

// TestInterfaceWidthRecipe_AppliesTo_NoCapabilityRequired --
// unlike fan_in / fan_out / lcom4, this recipe walks the
// scope tree only; no capability flag is required.
func TestInterfaceWidthRecipe_AppliesTo_NoCapabilityRequired(t *testing.T) {
	t.Parallel()
	// withCapability=false -> no decision_blocks flag, no
	// call_edges flag, no field_accesses flag.
	ast := newAstBuilder("foo.go", false).build()
	if !recipes.NewInterfaceWidthRecipe().AppliesTo(ast) {
		t.Fatalf("AppliesTo = false; want true (no capability flag is required for interface_width)")
	}
}

// TestInterfaceWidthRecipe_Compute_DirectMethodChildren --
// the value is the count of `SCOPE_KIND_METHOD` scopes whose
// `parent_scope_id` is the class's id. A class with 3 direct
// methods emits a draft of value 3.
func TestInterfaceWidthRecipe_Compute_DirectMethodChildren(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Widget", "")
	b.addMethod("Method1", c.GetScopeId())
	b.addMethod("Method2", c.GetScopeId())
	b.addMethod("Method3", c.GetScopeId())

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("Compute drafts = %d, want 1 (one class)", len(drafts))
	}
	d := drafts[0]
	if d.MetricKind != "interface_width" {
		t.Errorf("draft.MetricKind = %q, want %q", d.MetricKind, "interface_width")
	}
	if d.Value != 3 {
		t.Errorf("draft.Value = %v, want 3 (three direct method children)", d.Value)
	}
	if d.Scope.Kind != scope.KindClass {
		t.Errorf("draft.Scope.Kind = %q, want %q", d.Scope.Kind, scope.KindClass)
	}
	if d.Scope.LocalID != c.GetScopeId() {
		t.Errorf("draft.Scope.LocalID = %q, want %q", d.Scope.LocalID, c.GetScopeId())
	}
	if d.Pack != recipes.PackSolid {
		t.Errorf("draft.Pack = %q, want %q", d.Pack, recipes.PackSolid)
	}
}

// TestInterfaceWidthRecipe_Compute_NestedMethodsNotCounted --
// a method declared inside another method (a closure or
// nested function) has its parent_scope_id pointing at the
// outer method, NOT the class. interface_width counts only
// DIRECT method children of the class.
func TestInterfaceWidthRecipe_Compute_NestedMethodsNotCounted(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Widget", "")
	m := b.addMethod("Outer", c.GetScopeId())
	b.addMethod("Inner", m.GetScopeId())     // nested in outer -> not counted
	b.addMethod("DirectOnly", c.GetScopeId()) // direct -> counted

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 2 {
		t.Errorf("draft.Value = %v, want 2 (Outer + DirectOnly; Inner nests under Outer)", drafts[0].Value)
	}
}

// TestInterfaceWidthRecipe_Compute_AppliesAtInterfaceScope
// -- interfaces emit drafts too, at `scope.KindInterface`.
func TestInterfaceWidthRecipe_Compute_AppliesAtInterfaceScope(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	i := b.addInterface("Reader", "")
	b.addMethod("Read", i.GetScopeId())
	b.addMethod("Close", i.GetScopeId())

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Scope.Kind != scope.KindInterface {
		t.Errorf("draft.Scope.Kind = %q, want %q", drafts[0].Scope.Kind, scope.KindInterface)
	}
	if drafts[0].Value != 2 {
		t.Errorf("draft.Value = %v, want 2", drafts[0].Value)
	}
}

// TestInterfaceWidthRecipe_Compute_ZeroMethodsEmitsZero -- a
// class with zero direct method children emits a draft of
// value 0 (NOT skipped; the row is the authoritative "this
// class has no public surface" signal for ISP).
func TestInterfaceWidthRecipe_Compute_ZeroMethodsEmitsZero(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	b.addClass("Empty", "")

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 0 {
		t.Errorf("draft.Value = %v, want 0 (empty class is still emitted)", drafts[0].Value)
	}
}

// TestInterfaceWidthRecipe_Compute_NoClassesOrInterfacesReturnsNil
// -- a file with no class / interface scopes (e.g. a Go file
// with only top-level functions) emits zero drafts.
func TestInterfaceWidthRecipe_Compute_NoClassesOrInterfacesReturnsNil(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	b.addMethod("topLevelFunc", "")

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 0 {
		t.Fatalf("drafts = %d, want 0", len(drafts))
	}
}

// TestInterfaceWidthRecipe_Compute_GoUnexportedMethodsExcluded
// -- under Go convention an unexported method (first rune
// lower-case) is NOT part of the public surface and MUST be
// excluded from interface_width. This pins the architecture
// Sec 1.4.1 row 8 "exposed surface" semantics for the Go
// adapter (which does not emit a `visibility` attr).
func TestInterfaceWidthRecipe_Compute_GoUnexportedMethodsExcluded(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Widget", "")
	b.addMethod("PublicA", c.GetScopeId())  // uppercase -> public
	b.addMethod("PublicB", c.GetScopeId())  // uppercase -> public
	b.addMethod("helper", c.GetScopeId())   // lowercase -> private
	b.addMethod("compute", c.GetScopeId())  // lowercase -> private
	b.addMethod("_internal", c.GetScopeId()) // leading underscore -> NOT a letter, not exported

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1 (one class)", len(drafts))
	}
	if drafts[0].Value != 2 {
		t.Errorf("draft.Value = %v, want 2 (only PublicA + PublicB are exported under Go convention)", drafts[0].Value)
	}
}

// TestInterfaceWidthRecipe_Compute_JavaPublicAttrCounted --
// Java methods carry `attrs["public"]="true"` when the
// source had an explicit `public` modifier (stamped by
// `javaMethodAttrs` at `internal/ast/parser/java.go:240`).
// Only methods with the attr set are counted toward the
// public surface; methods without it are package-private,
// private, or protected -- NOT public.
func TestInterfaceWidthRecipe_Compute_JavaPublicAttrCounted(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("Foo.java", false)
	b.file.Language = "java"
	c := b.addClass("Widget", "")
	mPub := b.addMethod("doThing", c.GetScopeId())
	setAttr(mPub, "public", "true")
	mPriv := b.addMethod("helper", c.GetScopeId())
	setAttr(mPriv, "private", "true")
	mProt := b.addMethod("template", c.GetScopeId())
	setAttr(mProt, "protected", "true")
	// no modifier == package-private under Java, NOT public
	b.addMethod("packagePrivate", c.GetScopeId())

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 1 {
		t.Errorf("draft.Value = %v, want 1 (only the method tagged public=true counts; private/protected/package-private excluded)", drafts[0].Value)
	}
}

// TestInterfaceWidthRecipe_Compute_JavaInterfaceMembersImplicitlyPublic
// -- Java interface methods do NOT need an explicit `public`
// modifier; the parser instead stamps
// `attrs["interface_member"]="true"`
// (`internal/ast/parser/java.go:248`). The recipe treats
// these as part of the public surface.
func TestInterfaceWidthRecipe_Compute_JavaInterfaceMembersImplicitlyPublic(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("Foo.java", false)
	b.file.Language = "java"
	i := b.addInterface("Reader", "")
	mA := b.addMethod("read", i.GetScopeId())
	setAttr(mA, "interface_member", "true")
	mB := b.addMethod("close", i.GetScopeId())
	setAttr(mB, "interface_member", "true")

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1 (one interface)", len(drafts))
	}
	if drafts[0].Value != 2 {
		t.Errorf("draft.Value = %v, want 2 (interface members are implicitly public)", drafts[0].Value)
	}
	if drafts[0].Scope.Kind != scope.KindInterface {
		t.Errorf("draft.Scope.Kind = %q, want %q", drafts[0].Scope.Kind, scope.KindInterface)
	}
}

// TestInterfaceWidthRecipe_Compute_TypeScriptPrivateAndProtectedExcluded
// -- TS defaults to `public` so methods with NO visibility
// attr are public. Methods carrying `attrs["private"]` or
// `attrs["protected"]` are excluded (stamped by
// `internal/ast/parser/typescript.go::tsMethodAttrs`).
func TestInterfaceWidthRecipe_Compute_TypeScriptPrivateAndProtectedExcluded(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.ts", false)
	b.file.Language = "typescript"
	c := b.addClass("Widget", "")
	b.addMethod("publicDefault", c.GetScopeId()) // TS default = public
	mPriv := b.addMethod("privateOne", c.GetScopeId())
	setAttr(mPriv, "private", "true")
	mProt := b.addMethod("protectedOne", c.GetScopeId())
	setAttr(mProt, "protected", "true")
	b.addMethod("anotherPublic", c.GetScopeId())

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 2 {
		t.Errorf("draft.Value = %v, want 2 (TS default=public; private/protected excluded)", drafts[0].Value)
	}
}

// TestInterfaceWidthRecipe_Compute_PythonUnderscoreExcluded --
// PEP-8 says a name starting with `_` is private by
// convention. The recipe excludes them from the public
// surface.
func TestInterfaceWidthRecipe_Compute_PythonUnderscoreExcluded(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.py", false)
	b.file.Language = "python"
	c := b.addClass("Widget", "")
	b.addMethod("do_thing", c.GetScopeId())  // no underscore -> public
	b.addMethod("compute", c.GetScopeId())   // no underscore -> public
	b.addMethod("_helper", c.GetScopeId())   // leading _ -> private
	b.addMethod("_internal", c.GetScopeId()) // leading _ -> private

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 2 {
		t.Errorf("draft.Value = %v, want 2 (only no-underscore names are public under PEP-8)", drafts[0].Value)
	}
}

// TestInterfaceWidthRecipe_Compute_PythonDunderIsPublic --
// Python dunder methods (`__init__`, `__str__`, `__eq__`,
// ...) start with `__` and ARE part of the language's
// canonical public protocol; they must be counted toward
// the public surface even though they start with `_`.
func TestInterfaceWidthRecipe_Compute_PythonDunderIsPublic(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.py", false)
	b.file.Language = "python"
	c := b.addClass("Widget", "")
	b.addMethod("__init__", c.GetScopeId()) // dunder -> public
	b.addMethod("__str__", c.GetScopeId())  // dunder -> public
	b.addMethod("__eq__", c.GetScopeId())   // dunder -> public
	b.addMethod("_priv", c.GetScopeId())    // single underscore -> private

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 3 {
		t.Errorf("draft.Value = %v, want 3 (three dunder methods count; one _priv excluded)", drafts[0].Value)
	}
}

// TestInterfaceWidthRecipe_Compute_UnknownLanguageDefaultsPublic
// -- a hypothetical future language not yet in
// `parser.SupportedLanguages` falls through to the default
// branch and counts every direct method child. Pins the
// fail-loud-not-silent guarantee: over-counting surfaces as
// an INFLATED ISP signal a reviewer can spot, whereas
// under-counting would silently mask real ISP risk.
func TestInterfaceWidthRecipe_Compute_UnknownLanguageDefaultsPublic(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.unknown", false)
	b.file.Language = "rust" // not in SupportedLanguages
	c := b.addClass("Widget", "")
	b.addMethod("Pub", c.GetScopeId())
	b.addMethod("priv", c.GetScopeId())
	b.addMethod("_under", c.GetScopeId())

	drafts := recipes.NewInterfaceWidthRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 3 {
		t.Errorf("draft.Value = %v, want 3 (unknown language defaults to counting all methods)", drafts[0].Value)
	}
}

// setAttr is a tiny helper for the visibility scenarios: the
// test builder's addMethod does not initialise the attrs map
// (the recipe tests that need attrs are a small minority), so
// scenarios that want to stamp `public` / `private` /
// `protected` / `interface_member` use this to lazily create
// the map and set the key.
func setAttr(s *astv1.AstScope, key, value string) {
	if s.Attrs == nil {
		s.Attrs = map[string]string{}
	}
	s.Attrs[key] = value
}
