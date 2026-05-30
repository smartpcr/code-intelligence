package recipes_test

import (
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	astv1 "github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/v1"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestCouplingBetweenObjectsRecipe_MetricKindIsCanonical
// pins the literal `coupling_between_objects` spelling (NOT
// `cbo`, `class_coupling`, or `external_deps` -- closed-set
// guard forbids long-form aliases).
func TestCouplingBetweenObjectsRecipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	if got := recipes.NewCouplingBetweenObjectsRecipe().MetricKind(); got != "coupling_between_objects" {
		t.Fatalf("MetricKind() = %q, want %q", got, "coupling_between_objects")
	}
}

// TestCouplingBetweenObjectsRecipe_VersionStartsAtOne -- v1
// pin; a parser-fleet capability bump (call_edges /
// field_accesses) MUST coincide with a metric_version bump.
func TestCouplingBetweenObjectsRecipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	if got := recipes.NewCouplingBetweenObjectsRecipe().Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1", got)
	}
}

// TestCouplingBetweenObjectsRecipe_PackIsSolid -- architecture
// Sec 1.4.1 row 10 pack column pins `solid`.
func TestCouplingBetweenObjectsRecipe_PackIsSolid(t *testing.T) {
	t.Parallel()
	if got := recipes.NewCouplingBetweenObjectsRecipe().Pack(); got != recipes.PackSolid {
		t.Fatalf("Pack() = %q, want %q", got, recipes.PackSolid)
	}
}

// TestCouplingBetweenObjectsRecipe_AppliesTo_NilAst -- nil
// is refused.
func TestCouplingBetweenObjectsRecipe_AppliesTo_NilAst(t *testing.T) {
	t.Parallel()
	if recipes.NewCouplingBetweenObjectsRecipe().AppliesTo(nil) {
		t.Fatalf("AppliesTo(nil) = true, want false")
	}
}

// TestCouplingBetweenObjectsRecipe_AppliesTo_DegradedAst --
// degraded AST is refused (truncated edge set would
// undercount coupling).
func TestCouplingBetweenObjectsRecipe_AppliesTo_DegradedAst(t *testing.T) {
	t.Parallel()
	ast := newAstBuilder("foo.go", false).build()
	ast.DegradedReason = "parser-panic"
	if recipes.NewCouplingBetweenObjectsRecipe().AppliesTo(ast) {
		t.Fatalf("AppliesTo on degraded = true, want false")
	}
}

// TestCouplingBetweenObjectsRecipe_AppliesTo_NoCapabilityRequired
// -- partial-coverage: this recipe runs UNCONDITIONALLY
// (no capability gate). It lights up extends/implements/
// embeds/imports today and grows monotonically as
// call_edges / field_accesses come online. Architecture
// Sec 3.3 lit-metric table classifies CBO as "partial
// (depends on edges)".
func TestCouplingBetweenObjectsRecipe_AppliesTo_NoCapabilityRequired(t *testing.T) {
	t.Parallel()
	ast := newAstBuilder("foo.go", false).build()
	if !recipes.NewCouplingBetweenObjectsRecipe().AppliesTo(ast) {
		t.Fatalf("AppliesTo = false, want true (CBO has no capability gate; runs partial today)")
	}
}

// TestCouplingBetweenObjectsRecipe_Compute_ExtendsAndImports
// -- a class that extends one external type and whose file
// imports another package -> CBO = 2 (two distinct external
// targets). The extends edge originates from the class
// scope; the imports edge originates from the file scope,
// which is the class's parent and therefore NOT in the
// class's subtree -- so the imports edge does NOT count
// toward THIS class. This test verifies the subtree-scoped
// semantics.
func TestCouplingBetweenObjectsRecipe_Compute_ExtendsCounts(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Sub", "")
	addExtendsEdgeToQualified(b, c.GetScopeId(), "external.Base")

	drafts := recipes.NewCouplingBetweenObjectsRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 1 {
		t.Errorf("draft.Value = %v, want 1 (extends external.Base)", drafts[0].Value)
	}
	if drafts[0].Scope.Kind != scope.KindClass {
		t.Errorf("draft.Scope.Kind = %q, want %q", drafts[0].Scope.Kind, scope.KindClass)
	}
}

// TestCouplingBetweenObjectsRecipe_Compute_MethodLevelImportsBubbleUpToClass
// -- an `imports` edge originating from a method scope (the
// method directly imports a symbol) bubbles up to the class
// because the method is inside the class's subtree.
func TestCouplingBetweenObjectsRecipe_Compute_MethodLevelImportsBubbleUpToClass(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Service", "")
	m := b.addMethod("DoWork", c.GetScopeId())
	addEdgeWithFrom(b, "imports", m.GetScopeId(), "qualified:external.Logger")

	drafts := recipes.NewCouplingBetweenObjectsRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 1 {
		t.Errorf("draft.Value = %v, want 1 (method-level import bubbles to class)", drafts[0].Value)
	}
}

// TestCouplingBetweenObjectsRecipe_Compute_DistinctTargetsDeduped
// -- two outbound edges to the SAME external target collapse
// to a single coupling.
func TestCouplingBetweenObjectsRecipe_Compute_DistinctTargetsDeduped(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Multi", "")
	addExtendsEdgeToQualified(b, c.GetScopeId(), "external.Foo")
	addEdgeWithFrom(b, "imports", c.GetScopeId(), "external.Foo")

	drafts := recipes.NewCouplingBetweenObjectsRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 1 {
		t.Errorf("draft.Value = %v, want 1 (two edges to same target collapse)", drafts[0].Value)
	}
}

// TestCouplingBetweenObjectsRecipe_Compute_SelfCouplingExcluded
// -- an extends edge from class A's method to another scope
// INSIDE class A's subtree does NOT count toward A's CBO
// (self-coupling is excluded by the classical CBO
// definition).
func TestCouplingBetweenObjectsRecipe_Compute_SelfCouplingExcluded(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Self", "")
	m1 := b.addMethod("A", c.GetScopeId())
	m2 := b.addMethod("B", c.GetScopeId())
	// m1 calls m2 (both inside Self) -- should NOT count.
	b.addCallEdge(m1.GetScopeId(), m2.GetScopeId())

	drafts := recipes.NewCouplingBetweenObjectsRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 0 {
		t.Errorf("draft.Value = %v, want 0 (self-coupling excluded)", drafts[0].Value)
	}
}

// TestCouplingBetweenObjectsRecipe_Compute_CallToOtherClassMethodCounts
// -- a method on class A calls a method on class B in the
// same file. The target method is inside B's subtree; the
// recipe walks up to find the nearest class/interface
// ancestor (B), and counts the coupling A -> B.
func TestCouplingBetweenObjectsRecipe_Compute_CallToOtherClassMethodCounts(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	ca := b.addClass("A", "")
	cb := b.addClass("B", "")
	ma := b.addMethod("Do", ca.GetScopeId())
	mb := b.addMethod("Help", cb.GetScopeId())
	b.addCallEdge(ma.GetScopeId(), mb.GetScopeId())

	drafts := recipes.NewCouplingBetweenObjectsRecipe().Compute(b.build())
	if len(drafts) != 2 {
		t.Fatalf("drafts = %d, want 2", len(drafts))
	}
	byID := map[string]float64{}
	for _, d := range drafts {
		byID[d.Scope.LocalID] = d.Value
	}
	if byID[ca.GetScopeId()] != 1 {
		t.Errorf("A CBO = %v, want 1 (A.Do calls B.Help -> coupling to B)", byID[ca.GetScopeId()])
	}
	if byID[cb.GetScopeId()] != 0 {
		t.Errorf("B CBO = %v, want 0 (B has no outbound edges)", byID[cb.GetScopeId()])
	}
}

// TestCouplingBetweenObjectsRecipe_Compute_ImplementsCounts
// -- implements edges DO contribute to CBO (the interface is
// an external dependency target) even though they do NOT
// contribute to DIT.
func TestCouplingBetweenObjectsRecipe_Compute_ImplementsCounts(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Impl", "")
	addEdgeWithFrom(b, "implements", c.GetScopeId(), "qualified:io.Reader")

	drafts := recipes.NewCouplingBetweenObjectsRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 1 {
		t.Errorf("draft.Value = %v, want 1 (implements -> external interface)", drafts[0].Value)
	}
}

// TestCouplingBetweenObjectsRecipe_Compute_ContainsExcluded
// -- the `contains` edge is structural (parent/child scope
// shape) and does NOT count toward coupling.
func TestCouplingBetweenObjectsRecipe_Compute_ContainsExcluded(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Holder", "")
	addEdgeWithFrom(b, "contains", c.GetScopeId(), "qualified:external.Inner")

	drafts := recipes.NewCouplingBetweenObjectsRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 0 {
		t.Errorf("draft.Value = %v, want 0 (contains edges are structural, not coupling)", drafts[0].Value)
	}
}

// TestCouplingBetweenObjectsRecipe_Compute_NoClassScopesReturnsNil
// -- a file with no class scopes emits zero drafts (CBO
// applies to classes only).
func TestCouplingBetweenObjectsRecipe_Compute_NoClassScopesReturnsNil(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	b.addInterface("I", "")
	b.addMethod("top", "")

	drafts := recipes.NewCouplingBetweenObjectsRecipe().Compute(b.build())
	if len(drafts) != 0 {
		t.Fatalf("drafts = %d, want 0", len(drafts))
	}
}

// addEdgeWithFrom appends an edge from a scope id to a
// `qualified:<name>` external target with a custom kind.
// Used by CBO tests to construct implements / imports /
// contains / calls edges with deterministic targets.
func addEdgeWithFrom(b *astBuilder, kind, fromScopeID, qualifiedTarget string) {
	addEdge(b, kind, fromScopeID, &astv1.AstRef{
		Kind: parser.RefKindScope,
		Id:   "qualified:" + qualifiedTarget,
	})
}
