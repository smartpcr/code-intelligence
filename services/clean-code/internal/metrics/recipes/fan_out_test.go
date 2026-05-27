package recipes_test

import (
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestFanOutRecipe_MetricKindIsCanonical pins the literal
// `fan_out` spelling (NOT `fanout`, `fan-out`, or
// `outgoing_calls`).
func TestFanOutRecipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	if got := recipes.NewFanOutRecipe().MetricKind(); got != "fan_out" {
		t.Fatalf("MetricKind() = %q, want %q (NOT %q -- closed-set guard)",
			got, "fan_out", "fanout")
	}
}

// TestFanOutRecipe_VersionStartsAtOne pins v1; bumps must
// pair with a `metric_version` bump (architecture C4).
func TestFanOutRecipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	if got := recipes.NewFanOutRecipe().Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1", got)
	}
}

// TestFanOutRecipe_AppliesTo_NoCapabilitySkips -- Stage 2.1
// parser fleet does not yet emit `calls` edges.
func TestFanOutRecipe_AppliesTo_NoCapabilitySkips(t *testing.T) {
	t.Parallel()
	ast := newAstBuilder("foo.go", false).build()
	if recipes.NewFanOutRecipe().AppliesTo(ast) {
		t.Fatalf("AppliesTo without call_edges = true; want false")
	}
}

// TestFanOutRecipe_AppliesTo_DegradedSkips -- architecture
// Sec 3.4 lines 490-494.
func TestFanOutRecipe_AppliesTo_DegradedSkips(t *testing.T) {
	t.Parallel()
	ast := newAstBuilder("foo.go", false).withCallEdges().build()
	ast.DegradedReason = "parse_truncated"
	r := recipes.NewFanOutRecipe()
	if r.AppliesTo(ast) {
		t.Errorf("AppliesTo(degraded) = true, want false")
	}
	if len(r.Compute(ast)) != 0 {
		t.Errorf("Compute(degraded) emitted drafts; want 0")
	}
}

// TestFanOutRecipe_TagsPackAndSource -- SOLID-pack defaults.
func TestFanOutRecipe_TagsPackAndSource(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	b.addMethod("m1", cls.GetScopeId())
	drafts := recipes.NewFanOutRecipe().Compute(b.build())
	if len(drafts) == 0 {
		t.Fatalf("want >=1 draft, got 0")
	}
	for _, d := range drafts {
		if d.Pack != recipes.PackSolid {
			t.Errorf("Pack=%q, want %q (fan_out is solid-pack)", d.Pack, recipes.PackSolid)
		}
		if d.Source != recipes.SourceComputed {
			t.Errorf("Source=%q, want %q", d.Source, recipes.SourceComputed)
		}
		if d.MetricKind != "fan_out" {
			t.Errorf("MetricKind=%q, want fan_out", d.MetricKind)
		}
		if d.MetricVersion != 1 {
			t.Errorf("MetricVersion=%d, want 1", d.MetricVersion)
		}
	}
}

// TestFanOutRecipe_DirectlyEmitsAtMethodClassFile -- all
// three kinds in the applicability set are directly emitted
// because `edge.from` is always inside the file (per-file
// authoritative at every level). Sec 1.4.1 row 6 pins
// `method, class, file`.
func TestFanOutRecipe_DirectlyEmitsAtMethodClassFile(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	b.addMethod("m1", cls.GetScopeId())
	b.addMethod("m2", cls.GetScopeId())
	drafts := recipes.NewFanOutRecipe().Compute(b.build())
	got := map[scope.Kind]int{}
	for _, d := range drafts {
		got[d.Scope.Kind]++
	}
	if got[scope.KindMethod] != 2 {
		t.Errorf("method count = %d, want 2", got[scope.KindMethod])
	}
	if got[scope.KindClass] != 1 {
		t.Errorf("class count = %d, want 1", got[scope.KindClass])
	}
	if got[scope.KindFile] != 1 {
		t.Errorf("file count = %d, want 1 (fan_out is per-file authoritative at file scope)", got[scope.KindFile])
	}
}

// TestFanOutRecipe_KnownValue_CallToExternal -- a method
// calls an external symbol (a function declared in another
// file). The target resolves to "" (unknown to the per-file
// index) -- treated as out-of-subtree at every level, so
// fan_out is +1 at the method, +1 at the enclosing class,
// AND +1 at the file scope.
func TestFanOutRecipe_KnownValue_CallToExternal(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	m := b.addMethod("caller", cls.GetScopeId())
	b.addCallEdgeToExternal(m.GetScopeId(), "sym:external:99")

	drafts := recipes.NewFanOutRecipe().Compute(b.build())
	got := map[string]float64{}
	for _, d := range drafts {
		got[string(d.Scope.Kind)+":"+d.Scope.LocalID] = d.Value
	}
	if got["method:"+m.GetScopeId()] != 1 {
		t.Errorf("method fan_out = %v, want 1", got["method:"+m.GetScopeId()])
	}
	if got["class:"+cls.GetScopeId()] != 1 {
		t.Errorf("class fan_out = %v, want 1", got["class:"+cls.GetScopeId()])
	}
	// file count is 1 entry with value 1
	var fileCount, fileValue = 0, 0.0
	for k, v := range got {
		if len(k) >= 5 && k[:5] == "file:" {
			fileCount++
			fileValue = v
		}
	}
	if fileCount != 1 {
		t.Errorf("file-scope draft count = %d, want 1", fileCount)
	}
	if fileValue != 1 {
		t.Errorf("file fan_out = %v, want 1", fileValue)
	}
}

// TestFanOutRecipe_KnownValue_IntraClassCallNotCountedAtClass --
// a call from m1 to m2 inside the same class is intra-
// subtree at the class scope, so class fan_out MUST NOT
// count it. At the method scope m1's fan_out DOES count it
// (m2 is outside m1's subtree even though both are in the
// same class). At the file scope it is intra-subtree --
// fan_out MUST NOT count it.
func TestFanOutRecipe_KnownValue_IntraClassCallNotCountedAtClass(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	m1 := b.addMethod("m1", cls.GetScopeId())
	m2 := b.addMethod("m2", cls.GetScopeId())
	b.addCallEdge(m1.GetScopeId(), m2.GetScopeId())

	drafts := recipes.NewFanOutRecipe().Compute(b.build())
	got := map[string]float64{}
	for _, d := range drafts {
		got[string(d.Scope.Kind)+":"+d.Scope.LocalID] = d.Value
	}
	if got["method:"+m1.GetScopeId()] != 1 {
		t.Errorf("method(m1) fan_out = %v, want 1 (m2 is outside m1's subtree)", got["method:"+m1.GetScopeId()])
	}
	if got["method:"+m2.GetScopeId()] != 0 {
		t.Errorf("method(m2) fan_out = %v, want 0", got["method:"+m2.GetScopeId()])
	}
	if got["class:"+cls.GetScopeId()] != 0 {
		t.Errorf("class fan_out = %v with intra-class call; want 0 (intra-subtree excluded)", got["class:"+cls.GetScopeId()])
	}
	// File-level: also intra-file, so 0.
	for k, v := range got {
		if len(k) >= 5 && k[:5] == "file:" && v != 0 {
			t.Errorf("file fan_out = %v with only intra-file call; want 0", v)
		}
	}
}

// TestFanOutRecipe_SelfCallNotCounted -- a method that
// calls itself: every endpoint is in its own subtree at
// every level, so fan_out is 0 everywhere.
func TestFanOutRecipe_SelfCallNotCounted(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	m := b.addMethod("recur", cls.GetScopeId())
	b.addCallEdge(m.GetScopeId(), m.GetScopeId())

	drafts := recipes.NewFanOutRecipe().Compute(b.build())
	for _, d := range drafts {
		if d.Value != 0 {
			t.Errorf("scope_kind=%q value=%v with only self-call; want 0", d.Scope.Kind, d.Value)
		}
	}
}

// TestFanOutRecipe_NonCallEdgesIgnored -- imports / extends
// etc. MUST NOT contribute to fan_out.
func TestFanOutRecipe_NonCallEdgesIgnored(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	m := b.addMethod("m", cls.GetScopeId())
	// Insert an edge but flip its kind to a non-call value.
	e := b.addCallEdgeToExternal(m.GetScopeId(), "sym:x:1")
	e.Kind = "imports"

	drafts := recipes.NewFanOutRecipe().Compute(b.build())
	for _, d := range drafts {
		if d.Value != 0 {
			t.Errorf("scope_kind=%q value=%v with only non-call edges; want 0", d.Scope.Kind, d.Value)
		}
	}
}

// TestFanOutRecipe_NoCapabilityComputeIsNoop -- defence in
// depth.
func TestFanOutRecipe_NoCapabilityComputeIsNoop(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false)
	b.addClass("C", "")
	if got := recipes.NewFanOutRecipe().Compute(b.build()); got != nil {
		t.Errorf("Compute without capability = %v, want nil", got)
	}
}

// TestFanOutRecipe_NilAstIsNoop -- nil input is safe.
func TestFanOutRecipe_NilAstIsNoop(t *testing.T) {
	t.Parallel()
	if got := recipes.NewFanOutRecipe().Compute(nil); got != nil {
		t.Errorf("Compute(nil) = %v, want nil", got)
	}
}

// TestFanOutRecipe_ScopeKindIsCanonicalNotModule -- drift
// guard. Sec 1.4.1 row 6 pins the directly emitted set to
// {method, class, file}.
func TestFanOutRecipe_ScopeKindIsCanonicalNotModule(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	b.addMethod("m", cls.GetScopeId())
	drafts := recipes.NewFanOutRecipe().Compute(b.build())
	allowed := map[scope.Kind]bool{
		scope.KindMethod: true, scope.KindClass: true, scope.KindFile: true,
	}
	for _, d := range drafts {
		if !allowed[d.Scope.Kind] {
			t.Errorf("fan_out emitted scope_kind=%q; allowed set = {method, class, file}", d.Scope.Kind)
		}
		if d.Scope.Kind == scope.Kind("module") || d.Scope.Kind == scope.Kind("function") {
			t.Errorf("fan_out emitted drift scope_kind=%q", d.Scope.Kind)
		}
	}
}

// TestFanOutRecipe_CrossClassCallCountsAtBothClassesAndFile --
// a call from m1 in ClassA to m2 in ClassB inside the SAME
// file:
//   - method m1 fan_out = 1 (m2 outside m1)
//   - class ClassA fan_out = 1 (m2 is outside ClassA's subtree)
//   - class ClassB fan_out = 0 (the call originated outside
//     ClassB's subtree)
//   - file fan_out = 0 (the call is intra-file)
func TestFanOutRecipe_CrossClassCallCountsAtBothClassesAndFile(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	classA := b.addClass("ClassA", "")
	classB := b.addClass("ClassB", "")
	m1 := b.addMethod("m1", classA.GetScopeId())
	m2 := b.addMethod("m2", classB.GetScopeId())
	b.addCallEdge(m1.GetScopeId(), m2.GetScopeId())

	drafts := recipes.NewFanOutRecipe().Compute(b.build())
	got := map[string]float64{}
	for _, d := range drafts {
		got[string(d.Scope.Kind)+":"+d.Scope.LocalID] = d.Value
	}
	if got["method:"+m1.GetScopeId()] != 1 {
		t.Errorf("method(m1) fan_out = %v, want 1", got["method:"+m1.GetScopeId()])
	}
	if got["class:"+classA.GetScopeId()] != 1 {
		t.Errorf("class(ClassA) fan_out = %v, want 1 (m2 is in ClassB, outside ClassA)", got["class:"+classA.GetScopeId()])
	}
	if got["class:"+classB.GetScopeId()] != 0 {
		t.Errorf("class(ClassB) fan_out = %v, want 0 (call originated outside ClassB)", got["class:"+classB.GetScopeId()])
	}
	for k, v := range got {
		if len(k) >= 5 && k[:5] == "file:" && v != 0 {
			t.Errorf("file fan_out = %v with intra-file call; want 0 (call has both endpoints in this file)", v)
		}
	}
}
