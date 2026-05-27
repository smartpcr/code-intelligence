package recipes_test

import (
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestFanInRecipe_MetricKindIsCanonical pins the literal
// `fan_in` spelling (NOT `fanin`, `fan-in`, `incoming_calls`,
// or any drift). The architecture Sec 1.4.1 row 5 metric_kind
// column entry is exactly `fan_in`.
func TestFanInRecipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	if got := recipes.NewFanInRecipe().MetricKind(); got != "fan_in" {
		t.Fatalf("MetricKind() = %q, want %q (NOT %q -- closed-set guard)",
			got, "fan_in", "fanin")
	}
}

// TestFanInRecipe_VersionStartsAtOne pins v1; bumps must
// pair with a `metric_version` bump (architecture C4).
func TestFanInRecipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	if got := recipes.NewFanInRecipe().Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1", got)
	}
}

// TestFanInRecipe_AppliesTo_NoCapabilitySkips -- Stage 2.1
// parser fleet does not emit `calls` edges; AppliesTo must
// refuse without the `call_edges` flag.
func TestFanInRecipe_AppliesTo_NoCapabilitySkips(t *testing.T) {
	t.Parallel()
	ast := newAstBuilder("foo.go", false).build()
	if recipes.NewFanInRecipe().AppliesTo(ast) {
		t.Fatalf("AppliesTo without call_edges = true; want false")
	}
}

// TestFanInRecipe_AppliesTo_DegradedSkips -- architecture
// Sec 3.4 lines 490-494: a degraded AST is not eligible for
// a computed row.
func TestFanInRecipe_AppliesTo_DegradedSkips(t *testing.T) {
	t.Parallel()
	ast := newAstBuilder("foo.go", false).withCallEdges().build()
	ast.DegradedReason = "parse_truncated"
	r := recipes.NewFanInRecipe()
	if r.AppliesTo(ast) {
		t.Errorf("AppliesTo(degraded) = true, want false")
	}
	if len(r.Compute(ast)) != 0 {
		t.Errorf("Compute(degraded) emitted drafts; want 0 (computed rows are never degraded)")
	}
}

// TestFanInRecipe_TagsPackAndSource -- SOLID-pack defaults
// (pack=solid per Sec 1.4.1 row 5 pack column, source=
// computed). One method on its own emits with these tags.
func TestFanInRecipe_TagsPackAndSource(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	b.addMethod("m1", cls.GetScopeId())
	drafts := recipes.NewFanInRecipe().Compute(b.build())
	if len(drafts) == 0 {
		t.Fatalf("want >=1 draft (method + class), got 0")
	}
	for _, d := range drafts {
		if d.Pack != recipes.PackSolid {
			t.Errorf("Pack=%q, want %q (fan_in is solid-pack)", d.Pack, recipes.PackSolid)
		}
		if d.Source != recipes.SourceComputed {
			t.Errorf("Source=%q, want %q", d.Source, recipes.SourceComputed)
		}
		if d.MetricKind != "fan_in" {
			t.Errorf("MetricKind=%q, want fan_in", d.MetricKind)
		}
		if d.MetricVersion != 1 {
			t.Errorf("MetricVersion=%d, want 1", d.MetricVersion)
		}
	}
}

// TestFanInRecipe_DirectlyEmitsAtMethodClassFile -- per
// architecture Sec 1.4.1 row 5, fan_in's pinned scope set
// is `{method, class, file}`. The per-file recipe MUST emit
// at ALL THREE kinds; each emitted row is the authoritative
// computed-tier value for that scope at this SHA (see
// [FanInRecipe] doc "Authoritative per-parser semantics").
func TestFanInRecipe_DirectlyEmitsAtMethodClassFile(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	b.addMethod("m1", cls.GetScopeId())
	b.addMethod("m2", cls.GetScopeId())
	drafts := recipes.NewFanInRecipe().Compute(b.build())
	wantKinds := map[scope.Kind]int{
		scope.KindMethod: 2,
		scope.KindClass:  1,
		scope.KindFile:   1,
	}
	gotKinds := map[scope.Kind]int{}
	for _, d := range drafts {
		gotKinds[d.Scope.Kind]++
	}
	for k, n := range wantKinds {
		if gotKinds[k] != n {
			t.Errorf("scope_kind=%q count = %d, want %d", k, gotKinds[k], n)
		}
	}
	if gotKinds[scope.KindFile] == 0 {
		t.Errorf("fan_in MUST emit at scope_kind=file (Sec 1.4.1 row 5 pins {method, class, file})")
	}
}

// TestFanInRecipe_KnownValue_OneCallIntoMethod -- a `calls`
// edge from m_caller to m_target inside the same file MUST
// increment m_target's fan_in by 1, and (because the caller
// is in a different class) the target's class fan_in by 1.
// The FILE row counts only edges whose `from` is OUTSIDE
// the file's subtree -- both endpoints here are internal to
// this file, so the file-scope row's authoritative value is
// 0 (no external `from` in the producer's edge list at this
// SHA; see [FanInRecipe] doc "Authoritative per-parser
// semantics").
func TestFanInRecipe_KnownValue_OneCallIntoMethod(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	callerCls := b.addClass("Caller", "")
	targetCls := b.addClass("Target", "")
	mCaller := b.addMethod("call", callerCls.GetScopeId())
	mTarget := b.addMethod("target", targetCls.GetScopeId())
	b.addCallEdge(mCaller.GetScopeId(), mTarget.GetScopeId())

	drafts := recipes.NewFanInRecipe().Compute(b.build())
	got := map[string]float64{}
	for _, d := range drafts {
		got[string(d.Scope.Kind)+":"+d.Scope.LocalID] = d.Value
	}
	if got["method:"+mTarget.GetScopeId()] != 1 {
		t.Errorf("method(target) fan_in = %v, want 1", got["method:"+mTarget.GetScopeId()])
	}
	if got["method:"+mCaller.GetScopeId()] != 0 {
		t.Errorf("method(call) fan_in = %v, want 0 (no incoming)", got["method:"+mCaller.GetScopeId()])
	}
	if got["class:"+targetCls.GetScopeId()] != 1 {
		t.Errorf("class(Target) fan_in = %v, want 1", got["class:"+targetCls.GetScopeId()])
	}
	if got["class:"+callerCls.GetScopeId()] != 0 {
		t.Errorf("class(Caller) fan_in = %v, want 0", got["class:"+callerCls.GetScopeId()])
	}
	// File-scope: with both endpoints resolved INSIDE this
	// file (no external `from`), the producer surfaced zero
	// inbound calls at the file boundary; the file row's
	// authoritative value is 0. See [FanInRecipe] doc
	// "Authoritative per-parser semantics".
	var fileVal float64 = -1
	for _, d := range drafts {
		if d.Scope.Kind == scope.KindFile {
			fileVal = d.Value
		}
	}
	if fileVal < 0 {
		t.Errorf("no file-scope fan_in row emitted; Sec 1.4.1 row 5 pins file as a directly-emitted scope_kind")
	} else if fileVal != 0 {
		t.Errorf("file fan_in = %v with both endpoints internal; want 0 (no external `from` edges surfaced at this SHA)", fileVal)
	}
}

// TestFanInRecipe_IntraClassCallNotCountedAtClass -- a call
// from m1 to m2 within the SAME class is intra-subtree at
// the class level; fan_in is OUTBOUND-of-subtree only, so
// the class fan_in MUST NOT count it. (Intra-class coupling
// is captured by lcom4, not by fan_in.) At the METHOD level
// the call IS inbound to m2 (m1 is outside m2's subtree),
// so method-level fan_in DOES count it. At the FILE level
// the call is intra-subtree (both endpoints in the file), so
// file fan_in MUST NOT count it (= 0).
func TestFanInRecipe_IntraClassCallNotCountedAtClass(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	m1 := b.addMethod("m1", cls.GetScopeId())
	m2 := b.addMethod("m2", cls.GetScopeId())
	b.addCallEdge(m1.GetScopeId(), m2.GetScopeId())

	drafts := recipes.NewFanInRecipe().Compute(b.build())
	got := map[string]float64{}
	for _, d := range drafts {
		got[string(d.Scope.Kind)+":"+d.Scope.LocalID] = d.Value
	}
	if got["class:"+cls.GetScopeId()] != 0 {
		t.Errorf("class fan_in = %v with intra-class call; want 0 (intra-subtree excluded)", got["class:"+cls.GetScopeId()])
	}
	if got["method:"+m2.GetScopeId()] != 1 {
		t.Errorf("method(m2) fan_in = %v, want 1 (m1 is outside m2's subtree)", got["method:"+m2.GetScopeId()])
	}
	if got["method:"+m1.GetScopeId()] != 0 {
		t.Errorf("method(m1) fan_in = %v, want 0", got["method:"+m1.GetScopeId()])
	}
	// File-scope: intra-file call is intra-subtree at the
	// file level; the file's `to` is in the file's subtree
	// AND the file's `from` (a local scope) is also in the
	// file's subtree, so the edge is excluded. The producer
	// surfaced no inbound-from-external edges, so the file
	// row's authoritative value is 0.
	var fileVal float64 = -1
	for _, d := range drafts {
		if d.Scope.Kind == scope.KindFile {
			fileVal = d.Value
		}
	}
	if fileVal < 0 {
		t.Errorf("no file-scope fan_in row emitted; Sec 1.4.1 row 5 pins file")
	} else if fileVal != 0 {
		t.Errorf("file fan_in = %v with intra-file call; want 0 (intra-subtree excluded; no external `from` in the producer's edge list)", fileVal)
	}
}

// TestFanInRecipe_SelfCallNotCounted -- a method that calls
// itself (recursion) MUST NOT count toward its own fan_in.
func TestFanInRecipe_SelfCallNotCounted(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	m := b.addMethod("recur", cls.GetScopeId())
	b.addCallEdge(m.GetScopeId(), m.GetScopeId())

	drafts := recipes.NewFanInRecipe().Compute(b.build())
	for _, d := range drafts {
		if d.Scope.Kind == scope.KindMethod && d.Value != 0 {
			t.Errorf("method fan_in = %v with self-call; want 0", d.Value)
		}
	}
}

// TestFanInRecipe_NonCallEdgesIgnored -- only `calls` edges
// contribute. `imports`/`extends`/`implements`/`contains`
// (the Stage 2.1 parser's existing edge kinds) MUST NOT be
// counted toward fan_in.
func TestFanInRecipe_NonCallEdgesIgnored(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	m := b.addMethod("m", cls.GetScopeId())
	// Attach a non-call edge between two distinct scopes.
	b.addCallEdge("nope-from", m.GetScopeId()).Kind = "imports"

	drafts := recipes.NewFanInRecipe().Compute(b.build())
	for _, d := range drafts {
		if d.Value != 0 {
			t.Errorf("scope_kind=%q value=%v with only non-call edges; want 0", d.Scope.Kind, d.Value)
		}
	}
}

// TestFanInRecipe_NoCapabilityComputeIsNoop -- defence in
// depth: Compute without the call_edges capability returns
// nil even if a caller skipped AppliesTo.
func TestFanInRecipe_NoCapabilityComputeIsNoop(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false) // NO withCallEdges
	b.addClass("C", "")
	if got := recipes.NewFanInRecipe().Compute(b.build()); got != nil {
		t.Errorf("Compute without capability = %v, want nil", got)
	}
}

// TestFanInRecipe_NilAstIsNoop -- nil input is safe.
func TestFanInRecipe_NilAstIsNoop(t *testing.T) {
	t.Parallel()
	if got := recipes.NewFanInRecipe().Compute(nil); got != nil {
		t.Errorf("Compute(nil) = %v, want nil", got)
	}
}

// TestFanInRecipe_ScopeKindIsCanonicalNotModule -- defence
// against scope_kind drift (`module`, `function`,
// `namespace` are NOT in the canonical 7-enum). The
// directly-emitted set is `{method, class, file}` per
// Sec 1.4.1 row 5.
func TestFanInRecipe_ScopeKindIsCanonicalNotModule(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	b.addMethod("m", cls.GetScopeId())
	drafts := recipes.NewFanInRecipe().Compute(b.build())
	allowedDirect := map[scope.Kind]bool{
		scope.KindMethod: true,
		scope.KindClass:  true,
		scope.KindFile:   true,
	}
	for _, d := range drafts {
		if !allowedDirect[d.Scope.Kind] {
			t.Errorf("fan_in directly emitted scope_kind=%q; per-file recipe emits {method, class, file} only", d.Scope.Kind)
		}
		if d.Scope.Kind == scope.Kind("module") || d.Scope.Kind == scope.Kind("function") {
			t.Errorf("fan_in emitted drift scope_kind=%q", d.Scope.Kind)
		}
	}
}

// TestFanInRecipe_KnownValue_FileScope_ExternalCallerCounted
// is the POSITIVE proof that a `fan_in` row at
// `scope_kind=file` actually counts inbound references --
// not just that a file-scope row is emitted.
//
// Setup: file `a.go` has a single class C with one method m.
// The parser surfaced (via cross-file resolution) one
// `calls` edge whose `from` is an EXTERNAL symbol (declared
// elsewhere) and whose `to` is method `C.m`.
//
// Per architecture Sec 1.4.1 row 5, fan_in at file = count
// of `calls` edges in this file's edge list whose `to`
// resolves into the file's subtree AND whose `from` does
// NOT (i.e. `from` is external -- resolves to "" via
// scopeIndex.refEnclosingScope on an unresolved-symbol ref).
// Expected counts at every emit scope:
//
// method(m) fan_in = 1 (one external caller)
// class(C)  fan_in = 1 (the caller is outside C's subtree)
// file(a.go) fan_in = 1 (the caller is outside the file)
//
// This is the AUTHORITATIVE value for this (repo, sha,
// scope, fan_in, v1) row at this SHA per architecture
// Sec 5.2.1 lines 909-921 G3 (computed rows are immutable
// once written; never updated, never overwritten).
func TestFanInRecipe_KnownValue_FileScope_ExternalCallerCounted(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	m := b.addMethod("m", cls.GetScopeId())
	// External caller (symbol declared in another file) calls
	// `C.m` -- the parser resolved the cross-file ref and
	// surfaced the inbound edge in THIS file's edge list.
	b.addCallEdgeFromExternal("sym:external:caller", m.GetScopeId())

	drafts := recipes.NewFanInRecipe().Compute(b.build())
	got := map[string]float64{}
	for _, d := range drafts {
		got[string(d.Scope.Kind)+":"+d.Scope.LocalID] = d.Value
	}
	if got["method:"+m.GetScopeId()] != 1 {
		t.Errorf("method(m) fan_in = %v, want 1 (one external caller)", got["method:"+m.GetScopeId()])
	}
	if got["class:"+cls.GetScopeId()] != 1 {
		t.Errorf("class(C) fan_in = %v, want 1 (caller is outside C's subtree)", got["class:"+cls.GetScopeId()])
	}
	// The positive file-scope assertion required by the
	// evaluator: file fan_in counts inbound references, not
	// just emits a row.
	var fileVal float64 = -1
	for _, d := range drafts {
		if d.Scope.Kind == scope.KindFile {
			fileVal = d.Value
		}
	}
	if fileVal < 0 {
		t.Fatalf("no file-scope fan_in row emitted")
	}
	if fileVal != 1 {
		t.Errorf("file fan_in = %v, want 1 (one inbound `calls` edge from external; authoritative value at this SHA per G3 immutability)", fileVal)
	}
}

// TestFanInRecipe_KnownValue_FileScope_MultipleExternalCallers
// reinforces the positive file-scope semantic with N>1
// external callers and a mix of internal + external callers
// going to different methods. File fan_in must equal the
// total count of inbound-from-external `calls` edges
// surfaced by the producer, irrespective of which method
// they target -- the file-scope row aggregates over the
// file's subtree.
func TestFanInRecipe_KnownValue_FileScope_MultipleExternalCallers(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("a.go", false).withCallEdges()
	cls := b.addClass("C", "")
	m1 := b.addMethod("m1", cls.GetScopeId())
	m2 := b.addMethod("m2", cls.GetScopeId())
	// Three external callers -- two into m1, one into m2.
	b.addCallEdgeFromExternal("sym:external:caller_a", m1.GetScopeId())
	b.addCallEdgeFromExternal("sym:external:caller_b", m1.GetScopeId())
	b.addCallEdgeFromExternal("sym:external:caller_c", m2.GetScopeId())
	// Plus one intra-class call (should NOT count toward
	// class or file -- intra-subtree at both levels).
	b.addCallEdge(m1.GetScopeId(), m2.GetScopeId())

	drafts := recipes.NewFanInRecipe().Compute(b.build())
	got := map[string]float64{}
	for _, d := range drafts {
		got[string(d.Scope.Kind)+":"+d.Scope.LocalID] = d.Value
	}
	// method(m1) = 2 external callers
	if got["method:"+m1.GetScopeId()] != 2 {
		t.Errorf("method(m1) fan_in = %v, want 2 (two external callers)", got["method:"+m1.GetScopeId()])
	}
	// method(m2) = 1 external + 1 intra-class (m1 -> m2)
	if got["method:"+m2.GetScopeId()] != 2 {
		t.Errorf("method(m2) fan_in = %v, want 2 (one external + one intra-class from m1)", got["method:"+m2.GetScopeId()])
	}
	// class(C) = 3 external callers; intra-class call
	// excluded as intra-subtree.
	if got["class:"+cls.GetScopeId()] != 3 {
		t.Errorf("class(C) fan_in = %v, want 3 (three external callers; intra-class excluded)", got["class:"+cls.GetScopeId()])
	}
	// file = 3 external callers; intra-file call excluded as
	// intra-subtree at file level.
	var fileVal float64 = -1
	for _, d := range drafts {
		if d.Scope.Kind == scope.KindFile {
			fileVal = d.Value
		}
	}
	if fileVal < 0 {
		t.Fatalf("no file-scope fan_in row emitted")
	}
	if fileVal != 3 {
		t.Errorf("file fan_in = %v, want 3 (three inbound from external; intra-file excluded; authoritative value at this SHA)", fileVal)
	}
}
