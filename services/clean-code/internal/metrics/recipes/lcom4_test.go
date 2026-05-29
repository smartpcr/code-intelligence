package recipes_test

import (
	"testing"

	"forge/services/clean-code/internal/ast/scope"
	"forge/services/clean-code/internal/metrics/recipes"
)

// TestLCOM4Recipe_MetricKindIsCanonical pins the literal
// `lcom4` spelling (NOT `lack_of_cohesion`, `lcom`, or
// `lcom_4` -- the closed-set guard forbids long-form aliases
// and underscore drift).
func TestLCOM4Recipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	r := recipes.NewLCOM4Recipe()
	if got := r.MetricKind(); got != "lcom4" {
		t.Fatalf("MetricKind() = %q, want %q (NOT %q -- closed-set guard)",
			got, "lcom4", "lack_of_cohesion")
	}
}

// TestLCOM4Recipe_VersionStartsAtOne pins v1; bumps must
// pair with a `metric_version` bump (architecture C4).
func TestLCOM4Recipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	if got := recipes.NewLCOM4Recipe().Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1", got)
	}
}

// TestLCOM4Recipe_AppliesTo_NilAst -- nil is refused.
func TestLCOM4Recipe_AppliesTo_NilAst(t *testing.T) {
	t.Parallel()
	if recipes.NewLCOM4Recipe().AppliesTo(nil) {
		t.Fatalf("AppliesTo(nil) = true, want false")
	}
}

// TestLCOM4Recipe_AppliesTo_NoFieldAccessesSkips -- the Stage
// 2.1 parser fleet does NOT yet emit field-access edges; the
// recipe MUST skip without that capability flag (production
// scans produce zero LCOM4 drafts today; architecture Sec 3.4
// "computed rows are never degraded=true" forbids stamping
// the row degraded). The recipe lights up automatically when
// a future parser stage stamps `field_accesses="true"`.
func TestLCOM4Recipe_AppliesTo_NoFieldAccessesSkips(t *testing.T) {
	t.Parallel()
	// call_edges present, field_accesses MISSING.
	ast := newAstBuilder("foo.go", false).withCallEdges().build()
	if recipes.NewLCOM4Recipe().AppliesTo(ast) {
		t.Fatalf("AppliesTo returned true without field_accesses capability; want false (Stage 2.1 parser does not emit field-access edges)")
	}
}

// TestLCOM4Recipe_AppliesTo_NoCallEdgesSkips -- LCOM4's
// algorithm (Hitz & Montazeri) treats intra-class method
// calls as connectivity. A producer that emits field
// accesses but NOT call edges can only see half of the
// connectivity graph: delegation-heavy classes would appear
// maximally non-cohesive. The recipe MUST skip without BOTH
// capabilities; emitting a `source='computed'` row off half
// the inputs would silently under-report cohesion.
func TestLCOM4Recipe_AppliesTo_NoCallEdgesSkips(t *testing.T) {
	t.Parallel()
	// field_accesses present, call_edges MISSING.
	ast := newAstBuilder("foo.go", false).withFieldAccesses().build()
	if recipes.NewLCOM4Recipe().AppliesTo(ast) {
		t.Fatalf("AppliesTo returned true without call_edges capability; want false (intra-class call connectivity is half of LCOM4)")
	}
}

// TestLCOM4Recipe_AppliesTo_DegradedSkips -- architecture
// Sec 3.4 lines 490-494: a degraded AST means "row not
// written, not stamped degraded". AppliesTo refuses; Compute
// returns nil.
func TestLCOM4Recipe_AppliesTo_DegradedSkips(t *testing.T) {
	t.Parallel()
	ast := newAstBuilder("foo.go", false).withFieldAccesses().withCallEdges().build()
	ast.DegradedReason = "parse_truncated"
	r := recipes.NewLCOM4Recipe()
	if r.AppliesTo(ast) {
		t.Errorf("AppliesTo(degraded) = true, want false")
	}
	if len(r.Compute(ast)) != 0 {
		t.Errorf("Compute(degraded) emitted drafts; want 0 (computed rows are never degraded)")
	}
}

// TestLCOM4Recipe_TagsPackAndSource -- foundation-tier
// SOLID-pack defaults: pack=solid (NOT base; LCOM4 is the
// SRP-driver metric per architecture Sec 1.4.1 row 4 pack
// column), source=computed.
func TestLCOM4Recipe_TagsPackAndSource(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("MyClass", "")
	b.addMethod("m1", cls.GetScopeId())
	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("want 1 draft, got %d", len(drafts))
	}
	d := drafts[0]
	if d.Pack != recipes.PackSolid {
		t.Errorf("Pack=%q, want %q (LCOM4 is solid-pack per Sec 1.4.1 row 4)", d.Pack, recipes.PackSolid)
	}
	if d.Source != recipes.SourceComputed {
		t.Errorf("Source=%q, want %q", d.Source, recipes.SourceComputed)
	}
	if d.MetricKind != "lcom4" {
		t.Errorf("MetricKind=%q, want lcom4", d.MetricKind)
	}
	if d.MetricVersion != 1 {
		t.Errorf("MetricVersion=%d, want 1", d.MetricVersion)
	}
}

// TestLCOM4Recipe_EmitsAtClassScope -- architecture Sec
// 1.4.1 row 4 column 2 entry `class`. LCOM4 MUST emit at
// scope_kind=class and only class -- NOT method, NOT file,
// NOT interface, NOT package/repo.
func TestLCOM4Recipe_EmitsAtClassScope(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("MyClass", "")
	b.addMethod("m1", cls.GetScopeId())
	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("want 1 draft, got %d", len(drafts))
	}
	if drafts[0].Scope.Kind != scope.KindClass {
		t.Errorf("LCOM4 scope_kind = %q, want %q (Sec 1.4.1 row 4 pins class)",
			drafts[0].Scope.Kind, scope.KindClass)
	}
}

// TestLCOM4Recipe_NeverEmitsNonCanonicalScopeKinds -- defence
// against scope_kind drift (function, module, namespace
// are NOT in the canonical 7-enum; method, file, interface
// are NOT in lcom4's applicability set).
func TestLCOM4Recipe_NeverEmitsNonCanonicalScopeKinds(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("Inner", "")
	b.addMethod("m1", cls.GetScopeId())
	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	for _, d := range drafts {
		if d.Scope.Kind != scope.KindClass {
			t.Errorf("LCOM4 emitted scope_kind=%q; want only %q", d.Scope.Kind, scope.KindClass)
		}
	}
}

// TestLCOM4Recipe_KnownValue_TwoDisjointClusters --
// implementation-plan Stage 2.4 scenario
// `lcom4-class-known-value`: a class with FOUR methods split
// into two clusters (two methods share field A; two share
// field B; no cross-cluster edges) yields LCOM4 = 2.
//
// Layout:
//
//	class C {
//	    A, B fields
//	    m1, m2 both read A -> connected via A
//	    m3, m4 both write B -> connected via B
//	    no method calls bridge the clusters
//	}
//
// Expected: 2 connected components.
func TestLCOM4Recipe_KnownValue_TwoDisjointClusters(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("C", "")
	clsID := cls.GetScopeId()
	m1 := b.addMethod("m1", clsID)
	m2 := b.addMethod("m2", clsID)
	m3 := b.addMethod("m3", clsID)
	m4 := b.addMethod("m4", clsID)
	fldA := b.addField("A", clsID)
	fldB := b.addField("B", clsID)
	// Cluster {m1, m2} via field A.
	b.addFieldAccessEdge("reads_field", m1.GetScopeId(), fldA.GetSymbolId())
	b.addFieldAccessEdge("reads_field", m2.GetScopeId(), fldA.GetSymbolId())
	// Cluster {m3, m4} via field B.
	b.addFieldAccessEdge("writes_field", m3.GetScopeId(), fldB.GetSymbolId())
	b.addFieldAccessEdge("writes_field", m4.GetScopeId(), fldB.GetSymbolId())

	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("want 1 draft, got %d", len(drafts))
	}
	if drafts[0].Value != 2 {
		t.Errorf("LCOM4 value = %v, want 2 (two disjoint clusters)", drafts[0].Value)
	}
}

// TestLCOM4Recipe_KnownValue_SingleCluster -- a class whose
// methods all share at least one field is maximally cohesive
// (LCOM4 = 1).
func TestLCOM4Recipe_KnownValue_SingleCluster(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("C", "")
	clsID := cls.GetScopeId()
	m1 := b.addMethod("m1", clsID)
	m2 := b.addMethod("m2", clsID)
	m3 := b.addMethod("m3", clsID)
	fld := b.addField("shared", clsID)
	b.addFieldAccessEdge("reads_field", m1.GetScopeId(), fld.GetSymbolId())
	b.addFieldAccessEdge("reads_field", m2.GetScopeId(), fld.GetSymbolId())
	b.addFieldAccessEdge("writes_field", m3.GetScopeId(), fld.GetSymbolId())

	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 || drafts[0].Value != 1 {
		t.Fatalf("want LCOM4=1, got drafts=%+v", drafts)
	}
}

// TestLCOM4Recipe_KnownValue_IntraClassCallsBridgeClusters --
// classical Hitz & Montazeri LCOM4 treats intra-class method
// calls as connectivity edges. A class where m1 and m2 share
// no fields but m1 CALLS m2 is ONE cluster (LCOM4=1), NOT
// two. This is the lcom4-DELEGATION-counts-as-connectivity
// case the rubber-duck pass surfaced.
func TestLCOM4Recipe_KnownValue_IntraClassCallsBridgeClusters(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("C", "")
	clsID := cls.GetScopeId()
	m1 := b.addMethod("m1", clsID)
	m2 := b.addMethod("m2", clsID)
	// No shared-field edges; m1 calls m2.
	b.addCallEdge(m1.GetScopeId(), m2.GetScopeId())

	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("want 1 draft, got %d", len(drafts))
	}
	if drafts[0].Value != 1 {
		t.Errorf("LCOM4 = %v with m1->m2 intra-class call; want 1 (delegation bridges methods classically)", drafts[0].Value)
	}
}

// TestLCOM4Recipe_KnownValue_ZeroMethodsIsZero -- a class
// with no methods has no cohesion to measure; LCOM4 = 0 by
// convention (no disjoint method clusters when there are no
// methods).
func TestLCOM4Recipe_KnownValue_ZeroMethodsIsZero(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	b.addClass("Empty", "")
	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 || drafts[0].Value != 0 {
		t.Fatalf("want LCOM4=0 for zero-method class, got drafts=%+v", drafts)
	}
}

// TestLCOM4Recipe_KnownValue_OneMethodIsOne -- a class with
// exactly one method is a single component (LCOM4 = 1).
func TestLCOM4Recipe_KnownValue_OneMethodIsOne(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("Solo", "")
	b.addMethod("only", cls.GetScopeId())
	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 || drafts[0].Value != 1 {
		t.Fatalf("want LCOM4=1 for one-method class, got drafts=%+v", drafts)
	}
}

// TestLCOM4Recipe_KnownValue_AllDisjoint -- a class with N
// methods that share no fields and never call each other
// yields LCOM4 = N (maximum lack of cohesion).
func TestLCOM4Recipe_KnownValue_AllDisjoint(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("Scattered", "")
	clsID := cls.GetScopeId()
	for i := 0; i < 4; i++ {
		b.addMethod("m"+itoa(i), clsID)
	}
	// No edges at all.
	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("want 1 draft, got %d", len(drafts))
	}
	if drafts[0].Value != 4 {
		t.Errorf("LCOM4 = %v, want 4 (all 4 methods disjoint)", drafts[0].Value)
	}
}

// TestLCOM4Recipe_LocalFieldsIgnored -- a symbol with
// Kind="field" declared INSIDE a method body (ScopeId =
// method scope) is a local variable, NOT a class field, and
// MUST NOT contribute to LCOM4 connectivity. Otherwise a
// per-method local accidentally typed `field` could fuse
// methods that share no actual class state.
func TestLCOM4Recipe_LocalFieldsIgnored(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("C", "")
	clsID := cls.GetScopeId()
	m1 := b.addMethod("m1", clsID)
	m2 := b.addMethod("m2", clsID)
	// Add a "field" whose ScopeId is m1, NOT the class.
	local := b.addField("local", m1.GetScopeId())
	// And a field-access edge from m2 to that local --
	// inappropriate cross-method reference. The recipe MUST
	// NOT treat this as connecting m1 and m2.
	b.addFieldAccessEdge("reads_field", m2.GetScopeId(), local.GetSymbolId())

	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 {
		t.Fatalf("want 1 draft, got %d", len(drafts))
	}
	if drafts[0].Value != 2 {
		t.Errorf("LCOM4 = %v, want 2 (local-scoped \"field\" must NOT connect methods)", drafts[0].Value)
	}
}

// TestLCOM4Recipe_NoClassesIsNil -- a file with zero class
// scopes emits zero drafts (no class -> no row).
func TestLCOM4Recipe_NoClassesIsNil(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	if got := recipes.NewLCOM4Recipe().Compute(b.build()); got != nil {
		t.Errorf("Compute on class-free AST = %v, want nil", got)
	}
}

// TestLCOM4Recipe_NilAstIsNoop -- nil input is safe.
func TestLCOM4Recipe_NilAstIsNoop(t *testing.T) {
	t.Parallel()
	if got := recipes.NewLCOM4Recipe().Compute(nil); got != nil {
		t.Errorf("Compute(nil) = %v, want nil", got)
	}
}

// TestLCOM4Recipe_NoFieldAccessesComputeIsNoop -- defence in
// depth: Compute without the field_accesses capability must
// return nil even if a caller forgot the AppliesTo gate.
func TestLCOM4Recipe_NoFieldAccessesComputeIsNoop(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges() // NO withFieldAccesses
	b.addClass("C", "")
	if got := recipes.NewLCOM4Recipe().Compute(b.build()); got != nil {
		t.Errorf("Compute without field_accesses capability = %v, want nil (defence-in-depth)", got)
	}
}

// TestLCOM4Recipe_NoCallEdgesComputeIsNoop -- defence in
// depth: Compute without the call_edges capability must
// return nil even if a caller forgot the AppliesTo gate.
// Without call edges the recipe could only see field-share
// connectivity, missing the delegation half of the algorithm.
func TestLCOM4Recipe_NoCallEdgesComputeIsNoop(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses() // NO withCallEdges
	b.addClass("C", "")
	if got := recipes.NewLCOM4Recipe().Compute(b.build()); got != nil {
		t.Errorf("Compute without call_edges capability = %v, want nil (defence-in-depth)", got)
	}
}

// TestLCOM4Recipe_ScopeKindIsCanonicalNotModule -- explicit
// drift guard: the metric_kind=lcom4 row MUST NEVER carry a
// scope_kind outside the canonical 7-enum (`module`,
// `namespace`, `function` are drift signatures the iter-1
// evaluator flagged on the broader CLEAN-CODE workstream).
func TestLCOM4Recipe_ScopeKindIsCanonicalNotModule(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("C", "")
	b.addMethod("m", cls.GetScopeId())
	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	canonical := map[scope.Kind]bool{
		scope.KindRepo: true, scope.KindPackage: true, scope.KindFile: true,
		scope.KindClass: true, scope.KindInterface: true,
		scope.KindMethod: true, scope.KindBlock: true,
	}
	for _, d := range drafts {
		if !canonical[d.Scope.Kind] {
			t.Errorf("LCOM4 emitted non-canonical scope_kind=%q (canonical 7-enum only)", d.Scope.Kind)
		}
		if d.Scope.Kind == scope.Kind("module") || d.Scope.Kind == scope.Kind("function") || d.Scope.Kind == scope.Kind("namespace") {
			t.Errorf("LCOM4 emitted drift scope_kind=%q", d.Scope.Kind)
		}
	}
}

// TestLCOM4Recipe_NestedClosureDoesNotInflateMethodCount --
// a closure declared inside a method body has scope_kind=
// method but its NEAREST enclosing class ancestor reaches a
// method first (the outer method). The recipe MUST NOT count
// the closure as one of the class's methods; otherwise a
// 1-method class with one closure looks like a 2-method
// class with two disjoint clusters (LCOM4 would be 2 instead
// of 1).
func TestLCOM4Recipe_NestedClosureDoesNotInflateMethodCount(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	cls := b.addClass("C", "")
	m := b.addMethod("outer", cls.GetScopeId())
	// Closure nested under the outer method.
	b.addMethod("inner_closure", m.GetScopeId())
	drafts := recipes.NewLCOM4Recipe().Compute(b.build())
	if len(drafts) != 1 || drafts[0].Value != 1 {
		t.Fatalf("want LCOM4=1 (one method + nested closure must count as one component), got drafts=%+v", drafts)
	}
}

// TestLCOM4Recipe_NestedClassMethodsNotAttributedToOuter is
// the regression test for the iter-4 evaluator's item 1
// finding: tree-sitter Java (and other class-supporting
// languages) emit nested `class_declaration` /
// `record_declaration` / `enum_declaration` /
// `interface_declaration` scopes from inside a class body,
// so an inner class `Inner` is a child of the outer class
// `Outer` in the scope tree (parent_scope_id walk goes
// Inner.m -> Inner -> Outer -> file).
//
// `methodsOfClass(Outer, idx)` MUST stop at the inner class
// boundary -- a method whose nearest enclosing class is
// `Inner` is NOT a method of `Outer`. Without the boundary
// check, the parent walk would step through `Inner` and
// successfully match `Outer` at the next hop, incorrectly
// inflating Outer's method count (and degrading Outer's
// LCOM4 with Inner's methods' connectivity).
//
// Scenario: Outer is EMPTY (no own methods, no fields).
// Inner has two methods that share NOTHING. LCOM4 for
// Outer must be 0 (zero methods of its own). LCOM4 for
// Inner must be 2 (two disjoint method clusters).
func TestLCOM4Recipe_NestedClassMethodsNotAttributedToOuter(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	outer := b.addClass("Outer", "")
	inner := b.addClass("Inner", outer.GetScopeId())
	// Two Inner methods that share no fields and no calls --
	// Inner's LCOM4 should be 2.
	b.addMethod("im1", inner.GetScopeId())
	b.addMethod("im2", inner.GetScopeId())

	drafts := recipes.NewLCOM4Recipe().Compute(b.build())

	got := map[string]float64{}
	for _, d := range drafts {
		got[d.Scope.LocalID] = d.Value
	}
	if got[outer.GetScopeId()] != 0 {
		t.Errorf("LCOM4(Outer) = %v, want 0 (Outer owns NO methods of its own; Inner's methods must NOT count toward Outer)", got[outer.GetScopeId()])
	}
	if got[inner.GetScopeId()] != 2 {
		t.Errorf("LCOM4(Inner) = %v, want 2 (two Inner methods sharing nothing == two disjoint components)", got[inner.GetScopeId()])
	}
}

// TestLCOM4Recipe_OuterMethodsExcludeInnerClassPollution
// stresses the same nearest-enclosing-class rule with a
// realistic Java-like shape: Outer has its OWN methods (a
// single cohesive cluster sharing one field), AND nests an
// Inner class with two disjoint methods. The bug would
// pull Inner's methods into Outer's connectivity graph,
// turning Outer's LCOM4 from 1 (one cohesive cluster) into
// 3 (one Outer cluster + 2 Inner singletons). The fix
// keeps Outer's LCOM4 = 1 and Inner's = 2.
func TestLCOM4Recipe_OuterMethodsExcludeInnerClassPollution(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	outer := b.addClass("Outer", "")
	f := b.addField("state", outer.GetScopeId())
	om1 := b.addMethod("om1", outer.GetScopeId())
	om2 := b.addMethod("om2", outer.GetScopeId())
	// Outer's two methods both touch the shared field --
	// they form one cohesive cluster: LCOM4(Outer) = 1.
	b.addFieldAccessEdge("reads_field", om1.GetScopeId(), f.GetSymbolId())
	b.addFieldAccessEdge("writes_field", om2.GetScopeId(), f.GetSymbolId())

	// Inner is a nested class with two disjoint methods.
	inner := b.addClass("Inner", outer.GetScopeId())
	b.addMethod("im1", inner.GetScopeId())
	b.addMethod("im2", inner.GetScopeId())

	drafts := recipes.NewLCOM4Recipe().Compute(b.build())

	got := map[string]float64{}
	for _, d := range drafts {
		got[d.Scope.LocalID] = d.Value
	}
	if got[outer.GetScopeId()] != 1 {
		t.Errorf("LCOM4(Outer) = %v, want 1 (Outer's two methods share `state`; Inner's methods must NOT bleed in)", got[outer.GetScopeId()])
	}
	if got[inner.GetScopeId()] != 2 {
		t.Errorf("LCOM4(Inner) = %v, want 2 (Inner's two methods share nothing == two components)", got[inner.GetScopeId()])
	}
}

// TestLCOM4Recipe_NestedInterfaceMethodsNotAttributedToOuter
// mirrors the nested-class test for nested INTERFACES (Java
// allows `interface I {}` declared inside a class body; the
// parser emits a `ScopeKindInterface` scope child of the
// enclosing class). methodsOfClass must stop at the
// interface boundary too -- a default method declared on
// the inner interface MUST NOT be attributed to the outer
// class.
func TestLCOM4Recipe_NestedInterfaceMethodsNotAttributedToOuter(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withFieldAccesses().withCallEdges()
	outer := b.addClass("Outer", "")
	innerIface := b.addInterface("InnerIface", outer.GetScopeId())
	b.addMethod("defaultMethod1", innerIface.GetScopeId())
	b.addMethod("defaultMethod2", innerIface.GetScopeId())

	drafts := recipes.NewLCOM4Recipe().Compute(b.build())

	var outerVal float64 = -1
	for _, d := range drafts {
		if d.Scope.LocalID == outer.GetScopeId() {
			outerVal = d.Value
		}
	}
	if outerVal != 0 {
		t.Errorf("LCOM4(Outer) = %v, want 0 (Outer has no methods of its own; the inner interface's default methods MUST NOT count toward Outer)", outerVal)
	}
}
