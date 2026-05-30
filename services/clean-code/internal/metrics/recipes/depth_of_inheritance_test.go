package recipes_test

import (
	"testing"

	astv1 "github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/v1"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestDepthOfInheritanceRecipe_MetricKindIsCanonical pins
// the literal `depth_of_inheritance` spelling (NOT `dit`,
// `inheritance_depth`, or `class_depth` -- closed-set guard
// forbids long-form aliases).
func TestDepthOfInheritanceRecipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	if got := recipes.NewDepthOfInheritanceRecipe().MetricKind(); got != "depth_of_inheritance" {
		t.Fatalf("MetricKind() = %q, want %q", got, "depth_of_inheritance")
	}
}

// TestDepthOfInheritanceRecipe_VersionStartsAtOne pins v1.
func TestDepthOfInheritanceRecipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	if got := recipes.NewDepthOfInheritanceRecipe().Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1", got)
	}
}

// TestDepthOfInheritanceRecipe_PackIsSolid -- architecture
// Sec 1.4.1 row 9 pack column pins `solid`.
func TestDepthOfInheritanceRecipe_PackIsSolid(t *testing.T) {
	t.Parallel()
	if got := recipes.NewDepthOfInheritanceRecipe().Pack(); got != recipes.PackSolid {
		t.Fatalf("Pack() = %q, want %q", got, recipes.PackSolid)
	}
}

// TestDepthOfInheritanceRecipe_AppliesTo_NilAstReturnsFalse
// -- nil is refused.
func TestDepthOfInheritanceRecipe_AppliesTo_NilAstReturnsFalse(t *testing.T) {
	t.Parallel()
	if recipes.NewDepthOfInheritanceRecipe().AppliesTo(nil) {
		t.Fatalf("AppliesTo(nil) = true, want false")
	}
}

// TestDepthOfInheritanceRecipe_AppliesTo_DegradedAstReturnsFalse
// -- a parser that bailed mid-file would have a truncated
// extends/embeds edge set; the recipe MUST skip rather than
// emit a misleading depth.
func TestDepthOfInheritanceRecipe_AppliesTo_DegradedAstReturnsFalse(t *testing.T) {
	t.Parallel()
	ast := newAstBuilder("foo.go", false).build()
	ast.DegradedReason = "parser-panic"
	if recipes.NewDepthOfInheritanceRecipe().AppliesTo(ast) {
		t.Fatalf("AppliesTo on degraded AST = true, want false")
	}
}

// TestDepthOfInheritanceRecipe_Compute_NoExtendsEmitsZero --
// a class with no extends/embeds edges has DIT = 0 (root of
// the hierarchy). The draft is still emitted (the row is
// the authoritative "this class is a root" signal).
func TestDepthOfInheritanceRecipe_Compute_NoExtendsEmitsZero(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	b.addClass("Root", "")

	drafts := recipes.NewDepthOfInheritanceRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 0 {
		t.Errorf("draft.Value = %v, want 0", drafts[0].Value)
	}
	if drafts[0].Scope.Kind != scope.KindClass {
		t.Errorf("draft.Scope.Kind = %q, want %q", drafts[0].Scope.Kind, scope.KindClass)
	}
}

// TestDepthOfInheritanceRecipe_Compute_ExternalExtendsCountsOne
// -- the per-file recipe sees a class extending an external
// (`qualified:Foo`) ancestor as DIT=1: one extends edge
// step, then the ancestor chain is invisible from this
// AstFile. The architecture's Sec 3.3 lit-metric table
// describes this as "LIT (scope tree only)".
func TestDepthOfInheritanceRecipe_Compute_ExternalExtendsCountsOne(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Sub", "")
	addExtendsEdgeToQualified(b, c.GetScopeId(), "external.Base")

	drafts := recipes.NewDepthOfInheritanceRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 1 {
		t.Errorf("draft.Value = %v, want 1 (external ancestor)", drafts[0].Value)
	}
}

// TestDepthOfInheritanceRecipe_Compute_InFileChainSumsSteps
// -- a 3-level chain Grandparent -> Parent -> Child within
// the same file emits depths 0 / 1 / 2 (root / mid / leaf).
func TestDepthOfInheritanceRecipe_Compute_InFileChainSumsSteps(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	gp := b.addClass("Grandparent", "")
	p := b.addClass("Parent", "")
	c := b.addClass("Child", "")
	addExtendsEdgeToScope(b, p.GetScopeId(), gp.GetScopeId())
	addExtendsEdgeToScope(b, c.GetScopeId(), p.GetScopeId())

	drafts := recipes.NewDepthOfInheritanceRecipe().Compute(b.build())
	if len(drafts) != 3 {
		t.Fatalf("drafts = %d, want 3", len(drafts))
	}
	byID := map[string]float64{}
	for _, d := range drafts {
		byID[d.Scope.LocalID] = d.Value
	}
	if byID[gp.GetScopeId()] != 0 {
		t.Errorf("Grandparent depth = %v, want 0", byID[gp.GetScopeId()])
	}
	if byID[p.GetScopeId()] != 1 {
		t.Errorf("Parent depth = %v, want 1", byID[p.GetScopeId()])
	}
	if byID[c.GetScopeId()] != 2 {
		t.Errorf("Child depth = %v, want 2", byID[c.GetScopeId()])
	}
}

// TestDepthOfInheritanceRecipe_Compute_EmbedsEdgesCount --
// Go-style struct embedding (`embeds`) counts as an
// inheritance step: the embedded type's method set
// promotes to the outer type's method set, which is the
// composition-as-inheritance shape DIT measures.
func TestDepthOfInheritanceRecipe_Compute_EmbedsEdgesCount(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	base := b.addClass("Base", "")
	outer := b.addClass("Outer", "")
	addEdge(b, "embeds", outer.GetScopeId(), &astv1.AstRef{
		Kind: parser.RefKindScope,
		Id:   base.GetScopeId(),
	})

	drafts := recipes.NewDepthOfInheritanceRecipe().Compute(b.build())
	if len(drafts) != 2 {
		t.Fatalf("drafts = %d, want 2", len(drafts))
	}
	byID := map[string]float64{}
	for _, d := range drafts {
		byID[d.Scope.LocalID] = d.Value
	}
	if byID[base.GetScopeId()] != 0 {
		t.Errorf("Base depth = %v, want 0", byID[base.GetScopeId()])
	}
	if byID[outer.GetScopeId()] != 1 {
		t.Errorf("Outer depth = %v, want 1 (embeds counts as inheritance)", byID[outer.GetScopeId()])
	}
}

// TestDepthOfInheritanceRecipe_Compute_ImplementsEdgesIgnored
// -- DIT (Chidamber & Kemerer) measures IMPLEMENTATION
// inheritance only. An implements edge contributes to
// coupling_between_objects but NOT to DIT.
func TestDepthOfInheritanceRecipe_Compute_ImplementsEdgesIgnored(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	c := b.addClass("Impl", "")
	addEdge(b, "implements", c.GetScopeId(), &astv1.AstRef{
		Kind: parser.RefKindScope,
		Id:   "qualified:io.Reader",
	})

	drafts := recipes.NewDepthOfInheritanceRecipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d, want 1", len(drafts))
	}
	if drafts[0].Value != 0 {
		t.Errorf("draft.Value = %v, want 0 (implements edges do NOT count toward DIT)", drafts[0].Value)
	}
}

// TestDepthOfInheritanceRecipe_Compute_MultipleAncestorsTakeMax
// -- a class with two parents (Python, C++, Scala traits)
// reports the depth of the DEEPEST chain.
func TestDepthOfInheritanceRecipe_Compute_MultipleAncestorsTakeMax(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	deep1 := b.addClass("Deep1", "")
	deep2 := b.addClass("Deep2", "")
	mid := b.addClass("Mid", "")
	child := b.addClass("Child", "")
	addExtendsEdgeToScope(b, mid.GetScopeId(), deep1.GetScopeId())
	addExtendsEdgeToScope(b, child.GetScopeId(), mid.GetScopeId())  // -> deep1 chain: 2
	addExtendsEdgeToScope(b, child.GetScopeId(), deep2.GetScopeId()) // -> deep2 chain: 1

	drafts := recipes.NewDepthOfInheritanceRecipe().Compute(b.build())
	byID := map[string]float64{}
	for _, d := range drafts {
		byID[d.Scope.LocalID] = d.Value
	}
	if byID[child.GetScopeId()] != 2 {
		t.Errorf("Child depth = %v, want 2 (max(2,1))", byID[child.GetScopeId()])
	}
}

// TestDepthOfInheritanceRecipe_Compute_CycleSafe -- an
// extends cycle (A extends B, B extends A) is a parser bug
// but the recipe MUST NOT loop forever. Returns a bounded
// value.
func TestDepthOfInheritanceRecipe_Compute_CycleSafe(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	a := b.addClass("A", "")
	c := b.addClass("B", "")
	addExtendsEdgeToScope(b, a.GetScopeId(), c.GetScopeId())
	addExtendsEdgeToScope(b, c.GetScopeId(), a.GetScopeId())

	// Should terminate; the value is bounded but we don't
	// pin a specific number (the cycle short-circuit returns
	// 1 for each step in the worst case).
	done := make(chan struct{})
	var drafts []recipes.MetricSampleDraft
	go func() {
		drafts = recipes.NewDepthOfInheritanceRecipe().Compute(b.build())
		close(done)
	}()
	select {
	case <-done:
	}
	if len(drafts) != 2 {
		t.Fatalf("drafts = %d, want 2", len(drafts))
	}
}

// TestDepthOfInheritanceRecipe_Compute_NoClassScopesReturnsNil
// -- a file with only interfaces or only methods emits no
// DIT drafts (DIT applies to classes only).
func TestDepthOfInheritanceRecipe_Compute_NoClassScopesReturnsNil(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("foo.go", false)
	b.addInterface("I", "")
	b.addMethod("topLevel", "")

	drafts := recipes.NewDepthOfInheritanceRecipe().Compute(b.build())
	if len(drafts) != 0 {
		t.Fatalf("drafts = %d, want 0", len(drafts))
	}
}

// --- per-test helpers ---

// addExtendsEdgeToScope is a tiny helper that appends an
// `extends`-kind edge from a class scope to another scope id
// within the same file.
func addExtendsEdgeToScope(b *astBuilder, fromScopeID, toScopeID string) {
	addEdge(b, "extends", fromScopeID, &astv1.AstRef{
		Kind: parser.RefKindScope,
		Id:   toScopeID,
	})
}

// addExtendsEdgeToQualified appends an `extends`-kind edge
// from a class scope to an external `qualified:<name>` ref
// (the canonical shape for "this class extends a type
// declared outside this file").
func addExtendsEdgeToQualified(b *astBuilder, fromScopeID, qualifiedName string) {
	addEdge(b, "extends", fromScopeID, &astv1.AstRef{
		Kind: parser.RefKindScope,
		Id:   "qualified:" + qualifiedName,
	})
}

// addEdge is the low-level edge-appender for tests that
// need an edge kind not covered by the existing
// addCallEdge / addFieldAccessEdge helpers.
func addEdge(b *astBuilder, kind, fromScopeID string, to *astv1.AstRef) {
	e := &astv1.AstEdge{
		Kind: kind,
		From: &astv1.AstRef{
			Kind: parser.RefKindScope,
			Id:   fromScopeID,
		},
		To: to,
	}
	ast := b.build()
	ast.Edges = append(ast.Edges, e)
}
