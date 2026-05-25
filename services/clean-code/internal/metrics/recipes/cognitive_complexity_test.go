package recipes_test

import (
	"context"
	"testing"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestCognitiveComplexityRecipe_MetricKindIsCanonical pins
// the closed-set spelling `cognitive_complexity` -- the
// iter-1 evaluator item-3 forbids any short alias.
func TestCognitiveComplexityRecipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	r := recipes.NewCognitiveComplexityRecipe()
	if got := r.MetricKind(); got != "cognitive_complexity" {
		t.Fatalf("MetricKind() = %q, want %q (NOT %q)",
			got, "cognitive_complexity", "cognitive")
	}
}

// TestCognitiveComplexityRecipe_VersionStartsAtOne pins the
// recipe at version 1; a bump must be paired with a
// metric_version bump (architecture C4).
func TestCognitiveComplexityRecipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	r := recipes.NewCognitiveComplexityRecipe()
	if got := r.Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1", got)
	}
}

// TestCognitiveComplexityRecipe_AppliesTo_NilAst -- nil is
// always refused.
func TestCognitiveComplexityRecipe_AppliesTo_NilAst(t *testing.T) {
	t.Parallel()
	r := recipes.NewCognitiveComplexityRecipe()
	if r.AppliesTo(nil) {
		t.Fatalf("AppliesTo(nil) = true, want false")
	}
}

// TestCognitiveComplexityRecipe_AppliesTo_NoCapability is
// the same capability gate as cyclo: without the
// `decision_blocks` flag the recipe MUST skip emission.
func TestCognitiveComplexityRecipe_AppliesTo_NoCapability(t *testing.T) {
	t.Parallel()
	r := recipes.NewCognitiveComplexityRecipe()
	ast := newAstBuilder("foo.go", false).build()
	if r.AppliesTo(ast) {
		t.Fatalf("AppliesTo without capability returned true; gate must be honoured")
	}
}

// TestCognitiveComplexityRecipe_AppliesTo_WithCapability --
// positive path of the capability gate.
func TestCognitiveComplexityRecipe_AppliesTo_WithCapability(t *testing.T) {
	t.Parallel()
	r := recipes.NewCognitiveComplexityRecipe()
	ast := newAstBuilder("foo.go", true).build()
	if !r.AppliesTo(ast) {
		t.Fatalf("AppliesTo with capability returned false")
	}
}

// TestCognitiveComplexityRecipe_FlatIfsNoBonus -- a method
// with two FLAT `if`s scores 1+1=2 (no nesting bonus on
// either since both sit at depth 0).
func TestCognitiveComplexityRecipe_FlatIfsNoBonus(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	b.addBlock(m, "if")
	b.addBlock(m, "if")

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())
	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 2 {
		t.Errorf("two flat ifs cognitive = %v, want 2 (1 + 1, no nesting bonus)", method.Value)
	}
}

// TestCognitiveComplexityRecipe_NestedIfGetsBonus -- a
// nested `if` inside an `if` scores 1 + 2 = 3 (outer at depth
// 0 contributes 1; inner at depth 1 contributes 1+1=2).
// This is the SonarSource canonical example.
func TestCognitiveComplexityRecipe_NestedIfGetsBonus(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	outer := b.addBlock(m, "if")
	b.addBlock(outer, "if")

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())
	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 3 {
		t.Errorf("nested if cognitive = %v, want 3 (outer 1 + inner (1+1))", method.Value)
	}
}

// TestCognitiveComplexityRecipe_DeepNesting -- three `for`s
// nested score 1 + 2 + 3 = 6 (depths 0, 1, 2).
func TestCognitiveComplexityRecipe_DeepNesting(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	a := b.addBlock(m, "for")
	c := b.addBlock(a, "for")
	b.addBlock(c, "for")

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())
	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 6 {
		t.Errorf("three nested fors cognitive = %v, want 6 (1+2+3)", method.Value)
	}
}

// TestCognitiveComplexityRecipe_LogicalOperatorsNoBonus --
// `&&` and `||` inside a deeply nested `if` should still
// contribute 1 each (no nesting bonus per SonarSource).
func TestCognitiveComplexityRecipe_LogicalOperatorsNoBonus(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	outer := b.addBlock(m, "if")     // +1 cognitive
	b.addBlock(outer, "logical_and") // +1 (no nesting bonus)
	b.addBlock(outer, "logical_or")  // +1 (no nesting bonus)

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())
	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 3 {
		t.Errorf("if+&&+|| cognitive = %v, want 3 (1+1+1, logical never gets nesting bonus)", method.Value)
	}
}

// TestCognitiveComplexityRecipe_ElseIfNoBonus -- `else_if` is
// a continuation of the parent if; it contributes 1 but does
// NOT receive a nesting bonus and does NOT increment the
// nesting counter for its descendants. This matches
// SonarSource's `else if` rule.
func TestCognitiveComplexityRecipe_ElseIfNoBonus(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	outer := b.addBlock(m, "if")           // +1
	elseIf := b.addBlock(outer, "else_if") // +1 (no bonus)
	b.addBlock(elseIf, "if")               // depth 1 -> +1+1 = 2

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())
	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 4 {
		t.Errorf("if/else_if/nested-if cognitive = %v, want 4 (1+1+(1+1); else_if does NOT push nesting)", method.Value)
	}
}

// TestCognitiveComplexityRecipe_EmptyMethodIsZero -- a
// method with no decision blocks scores 0.
func TestCognitiveComplexityRecipe_EmptyMethodIsZero(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("Empty", "")

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())
	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 0 {
		t.Errorf("empty method cognitive = %v, want 0", method.Value)
	}
}

// TestCognitiveComplexityRecipe_FileSumsMethods -- file score
// is the sum of all per-method scores.
func TestCognitiveComplexityRecipe_FileSumsMethods(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m1 := b.addMethod("A", "")
	b.addBlock(m1, "if") // +1 -> 1
	m2 := b.addMethod("B", "")
	outer := b.addBlock(m2, "if") // +1
	b.addBlock(outer, "if")       // +(1+1) -> 3

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())

	file := findFileDraft(t, drafts)
	if file.Value != 4 {
		t.Errorf("file cognitive = %v, want 4 (1 + 3)", file.Value)
	}
	_ = m1
}

// TestCognitiveComplexityRecipe_NeverEmitsNonCanonicalScopeKinds
// -- iter-1 evaluator item-3 closed-set guard: every draft's
// scope_kind MUST be method or file, NEVER `function` or
// `module`.
func TestCognitiveComplexityRecipe_NeverEmitsNonCanonicalScopeKinds(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("multi.go", true)
	m1 := b.addMethod("A", "")
	m2 := b.addMethod("B", "")
	b.addBlock(m1, "if")
	b.addBlock(m2, "for")

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())
	for _, d := range drafts {
		if d.Scope.Kind != scope.KindMethod && d.Scope.Kind != scope.KindFile {
			t.Errorf("emitted scope_kind=%q (must be method or file)", d.Scope.Kind)
		}
		if d.Scope.Kind == scope.Kind("function") || d.Scope.Kind == scope.Kind("module") {
			t.Errorf("emitted forbidden scope_kind=%q (NOT in canonical seven-enum)", d.Scope.Kind)
		}
	}
}

// TestCognitiveComplexityRecipe_TagsPackAndSource -- pack
// and source are always the foundation-tier defaults.
func TestCognitiveComplexityRecipe_TagsPackAndSource(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	b.addBlock(m, "if")

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())
	for _, d := range drafts {
		if d.Pack != recipes.PackBase {
			t.Errorf("Pack=%q, want %q", d.Pack, recipes.PackBase)
		}
		if d.Source != recipes.SourceComputed {
			t.Errorf("Source=%q, want %q", d.Source, recipes.SourceComputed)
		}
		if d.MetricKind != "cognitive_complexity" {
			t.Errorf("MetricKind=%q, want cognitive_complexity", d.MetricKind)
		}
		if d.MetricVersion != 1 {
			t.Errorf("MetricVersion=%d, want 1", d.MetricVersion)
		}
	}
}

// TestCognitiveComplexityRecipe_LinearBumps -- linear flow-
// break decisions (labeled break/continue, goto, recursion)
// contribute 1 each, no nesting bonus.
func TestCognitiveComplexityRecipe_LinearBumps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind    string
		wantCog int
	}{
		{"break_label", 1},
		{"continue_label", 1},
		{"goto", 1},
		{"recursion", 1},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			b := newAstBuilder("p.go", true)
			m := b.addMethod("M", "")
			b.addBlock(m, tc.kind)
			drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())
			method := findMethodDraft(t, drafts, m.GetScopeId())
			if int(method.Value) != tc.wantCog {
				t.Errorf("decision_kind=%q cognitive = %v, want %d", tc.kind, method.Value, tc.wantCog)
			}
		})
	}
}

// TestCognitiveComplexityRecipe_NilAstIsNoop -- nil input is
// safe.
func TestCognitiveComplexityRecipe_NilAstIsNoop(t *testing.T) {
	t.Parallel()
	got := recipes.NewCognitiveComplexityRecipe().Compute(nil)
	if got != nil {
		t.Errorf("Compute(nil) = %v, want nil", got)
	}
}

// TestCognitiveComplexityRecipe_ClosureBoundaryStopsDescent
// -- a nested method scope (closure) does NOT contribute to
// the enclosing method's cognitive score.
func TestCognitiveComplexityRecipe_ClosureBoundaryStopsDescent(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	outer := b.addMethod("Outer", "")
	b.addBlock(outer, "if") // Outer score: 1
	inner := b.addMethod("Inner", outer.GetScopeId())
	innerOuter := b.addBlock(inner, "if") // Inner score: 1
	b.addBlock(innerOuter, "if")          // Inner score: + (1+1) = 3 total

	drafts := recipes.NewCognitiveComplexityRecipe().Compute(b.build())

	outerD := findMethodDraft(t, drafts, outer.GetScopeId())
	innerD := findMethodDraft(t, drafts, inner.GetScopeId())
	if outerD.Value != 1 {
		t.Errorf("Outer cognitive = %v, want 1 (Inner's score must NOT be counted in Outer)", outerD.Value)
	}
	if innerD.Value != 3 {
		t.Errorf("Inner cognitive = %v, want 3 (1 + (1+1) nested)", innerD.Value)
	}
}

// TestCognitiveComplexityRecipe_RealParserSkippedByGate --
// the same real-parser contract test as cyclo: today's Stage
// 2.1 parser does NOT advertise decision_blocks=true, so
// AppliesTo MUST return false.
func TestCognitiveComplexityRecipe_RealParserSkippedByGate(t *testing.T) {
	t.Parallel()
	src := []byte(`package sample
func A(x int) int {
    if x > 0 {
        return 1
    }
    return 0
}
`)
	out, err := parser.DefaultRegistry().Parse(context.Background(), "sample.go", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	r := recipes.NewCognitiveComplexityRecipe()
	if r.AppliesTo(out) {
		t.Fatalf("AppliesTo returned true on Stage 2.1 parser output; gate broken")
	}
}

// TestCognitiveComplexityRecipe_DegradedAstSkipsEmission --
// architecture Sec 3.4 lines 490-494: "Computed rows are
// never `degraded=true`: if an input is missing the row is
// not written, not stamped degraded -- only system-tier
// derivation stamps `degraded=true`". The recipe MUST
// silently skip a degraded AST rather than smuggle a
// `degraded_reason` onto a `source='computed'` row. Iter-1
// evaluator item-1 caught the inverse behaviour.
func TestCognitiveComplexityRecipe_DegradedAstSkipsEmission(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	b.addBlock(m, "if")
	ast := b.build()
	ast.DegradedReason = "tree_sitter_panic"

	r := recipes.NewCognitiveComplexityRecipe()
	if r.AppliesTo(ast) {
		t.Errorf("AppliesTo(degraded ast) = true, want false (Sec 3.4 computed rows never degraded)")
	}
	drafts := r.Compute(ast)
	if len(drafts) != 0 {
		t.Errorf("Compute(degraded ast) emitted %d drafts; want 0", len(drafts))
	}
}
