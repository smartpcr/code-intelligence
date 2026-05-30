package recipes_test

import (
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
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
