package recipes_test

import (
	"context"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestCycloRecipe_MetricKindIsCanonical is the iter-1
// evaluator item-3 closed-set guard: the recipe MUST advertise
// the canonical literal `cyclo`, NOT the long-form alias
// `cyclomatic_complexity`. A typo here surfaces as a
// foreign-key violation on `MetricSample.metric_kind` at
// insert time -- the explicit assertion catches it at the
// first test run instead.
func TestCycloRecipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycloRecipe()
	if got := r.MetricKind(); got != "cyclo" {
		t.Fatalf("MetricKind() = %q, want %q (NOT %q -- iter-1 evaluator item-3 forbids the long-form alias)",
			got, "cyclo", "cyclomatic_complexity")
	}
}

// TestCycloRecipe_VersionStartsAtOne pins the recipe's
// initial version. A bump must be accompanied by a
// `metric_version` bump on every emitted sample (architecture
// C4) so any drift here is loud.
func TestCycloRecipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycloRecipe()
	if got := r.Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1 (v1 recipes ship at version 1; a bump MUST land alongside a recipe_manifest row update)", got)
	}
}

// TestCycloRecipe_AppliesTo_NilAst asserts the recipe refuses
// a nil AST. A registry that passes nil is a programmer bug;
// returning true would let Compute crash on the nil deref.
func TestCycloRecipe_AppliesTo_NilAst(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycloRecipe()
	if r.AppliesTo(nil) {
		t.Fatalf("AppliesTo(nil) = true, want false")
	}
}

// TestCycloRecipe_AppliesTo_NoCapability is the gate the
// rubber-duck iter-1 critique #1 demanded: without the
// `decision_blocks` capability flag the recipe MUST silently
// skip emission so today's shallow parser output does not
// land a misleading `source='computed'` cyclo=1 row per
// method.
func TestCycloRecipe_AppliesTo_NoCapability(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycloRecipe()
	ast := newAstBuilder("foo.go", false).build()
	if r.AppliesTo(ast) {
		t.Fatalf("AppliesTo(ast without decision_blocks) = true, want false (capability gate must be honoured)")
	}
}

// TestCycloRecipe_AppliesTo_WithCapability is the positive
// path of the capability gate -- once the producer advertises
// `decision_blocks="true"` the recipe runs.
func TestCycloRecipe_AppliesTo_WithCapability(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycloRecipe()
	ast := newAstBuilder("foo.go", true).build()
	if !r.AppliesTo(ast) {
		t.Fatalf("AppliesTo(ast with decision_blocks=true) = false, want true")
	}
}

// TestCycloRecipe_KnownValue is the architecture-pinned
// canary: implementation-plan.md Stage 2.3 Test Scenarios
// `cyclo-known-value` -- a method with two `if` branches and
// one `for` loop emits `MetricSampleDraft(metric_kind='cyclo',
// value=4)` at canonical `scope_kind='method'`.
//
// The scenario explicitly forbids `scope_kind='function'`.
func TestCycloRecipe_KnownValue(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("sample.go", true)
	m := b.addMethod("BarMethod", "")
	b.addBlock(m, "if")
	b.addBlock(m, "if")
	b.addBlock(m, "for")

	drafts := recipes.NewCycloRecipe().Compute(b.build())

	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 4 {
		t.Errorf("method cyclo = %v, want 4 (1 baseline + 2 ifs + 1 for)", method.Value)
	}
	if method.Scope.Kind != scope.KindMethod {
		t.Errorf("method draft scope_kind = %q, want %q (canonical seven-enum)", method.Scope.Kind, scope.KindMethod)
	}
}

// TestCycloRecipe_NeverEmitsNonCanonicalScopeKinds is the
// iter-1 evaluator item-3 closed-set guard for scope kinds:
// across many realistic inputs, every draft's `scope_kind`
// MUST be one of `scope.KindMethod` or `scope.KindFile`. The
// canonical enum has NO `function` and NO `module`.
func TestCycloRecipe_NeverEmitsNonCanonicalScopeKinds(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("multi.go", true)
	m1 := b.addMethod("A", "")
	m2 := b.addMethod("B", "")
	b.addBlock(m1, "if")
	b.addBlock(m2, "for")
	b.addBlock(m2, "while")

	drafts := recipes.NewCycloRecipe().Compute(b.build())
	for _, d := range drafts {
		if d.Scope.Kind != scope.KindMethod && d.Scope.Kind != scope.KindFile {
			t.Errorf("emitted scope_kind=%q (must be method or file; canonical enum forbids %q and %q)",
				d.Scope.Kind, "function", "module")
		}
		if d.Scope.Kind == scope.Kind("function") || d.Scope.Kind == scope.Kind("module") {
			t.Errorf("emitted scope_kind=%q (forbidden: NOT in canonical seven-enum)", d.Scope.Kind)
		}
	}
}

// TestCycloRecipe_TagsPackAndSource verifies every draft
// carries the foundation-tier defaults the architecture
// (Sec 1.4.1 row 1, Sec 5.2.1) requires: `pack='base'`,
// `source='computed'`.
func TestCycloRecipe_TagsPackAndSource(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	b.addBlock(m, "if")

	drafts := recipes.NewCycloRecipe().Compute(b.build())
	if len(drafts) == 0 {
		t.Fatalf("expected at least one draft")
	}
	for _, d := range drafts {
		if d.Pack != recipes.PackBase {
			t.Errorf("Pack=%q, want %q", d.Pack, recipes.PackBase)
		}
		if d.Source != recipes.SourceComputed {
			t.Errorf("Source=%q, want %q", d.Source, recipes.SourceComputed)
		}
		if d.MetricKind != "cyclo" {
			t.Errorf("MetricKind=%q, want cyclo", d.MetricKind)
		}
		if d.MetricVersion != 1 {
			t.Errorf("MetricVersion=%d, want 1", d.MetricVersion)
		}
	}
}

// TestCycloRecipe_EmptyMethodIsBaselineOne -- a method with
// no decision points has cyclo=1 (the single baseline path
// through the body).
func TestCycloRecipe_EmptyMethodIsBaselineOne(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("Empty", "")

	drafts := recipes.NewCycloRecipe().Compute(b.build())

	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 1 {
		t.Errorf("empty method cyclo = %v, want 1", method.Value)
	}
}

// TestCycloRecipe_FileSumsMethods -- the file-level draft is
// the sum of per-method cyclos.
func TestCycloRecipe_FileSumsMethods(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m1 := b.addMethod("A", "")    // baseline 1
	b.addBlock(m1, "if")          // +1 -> 2
	m2 := b.addMethod("B", "")    // baseline 1
	b.addBlock(m2, "for")         // +1 -> 2
	b.addBlock(m2, "logical_and") // +1 -> 3
	b.addMethod("Empty", "")      // baseline 1

	drafts := recipes.NewCycloRecipe().Compute(b.build())

	file := findFileDraft(t, drafts)
	if file.Value != 6 {
		t.Errorf("file cyclo = %v, want 6 (2 + 3 + 1)", file.Value)
	}
}

// TestCycloRecipe_FileWithNoMethodsIsZero -- a file with no
// methods emits value=0 (rubber-duck iter-1 critique #3:
// "empty file should usually be 0, not 1").
func TestCycloRecipe_FileWithNoMethodsIsZero(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("empty.go", true)

	drafts := recipes.NewCycloRecipe().Compute(b.build())

	file := findFileDraft(t, drafts)
	if file.Value != 0 {
		t.Errorf("file with no methods cyclo = %v, want 0", file.Value)
	}
}

// TestCycloRecipe_NestedDecisionsAreCounted -- a decision
// block nested inside another decision block counts once for
// cyclo (cyclo is depth-independent, unlike cognitive).
func TestCycloRecipe_NestedDecisionsAreCounted(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("nested.go", true)
	m := b.addMethod("M", "")
	outer := b.addBlock(m, "if")
	b.addBlock(outer, "if")
	b.addBlock(outer, "for")

	drafts := recipes.NewCycloRecipe().Compute(b.build())

	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 4 {
		t.Errorf("nested decisions cyclo = %v, want 4 (1 baseline + 3 decisions)", method.Value)
	}
}

// TestCycloRecipe_UnknownDecisionKindIgnored -- a block with
// an unknown `decision_kind` value contributes 0. This is the
// rubber-duck iter-1 critique #4 closed-set guard: a
// language-specific parser that invents a new decision kind
// MUST not silently bump the recipe's value; the contract
// requires a coordinated edit to `decisionTable`.
func TestCycloRecipe_UnknownDecisionKindIgnored(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	b.addBlock(m, "if")      // known -> +1
	b.addBlock(m, "garbage") // unknown -> +0
	b.addBlock(m, "")        // empty -> +0

	drafts := recipes.NewCycloRecipe().Compute(b.build())

	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Value != 2 {
		t.Errorf("method cyclo = %v, want 2 (1 baseline + 1 known decision; unknown/empty are ignored)", method.Value)
	}
}

// TestCycloRecipe_AllDecisionKindsCount asserts every entry
// in the closed `decisionTable` taxonomy that has
// `cycloDelta=1` actually contributes +1 to cyclo.
// Linear-only kinds (`break_label`, `continue_label`, `goto`,
// `recursion`, `else`) contribute 0.
func TestCycloRecipe_AllDecisionKindsCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		kind    string
		wantCyc int
	}{
		{"if", 2},
		{"else_if", 2},
		{"else", 1},
		{"for", 2},
		{"while", 2},
		{"do_while", 2},
		{"case", 2},
		{"catch", 2},
		{"ternary", 2},
		{"logical_and", 2},
		{"logical_or", 2},
		{"break_label", 1},
		{"continue_label", 1},
		{"goto", 1},
		{"recursion", 1},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()
			b := newAstBuilder("k.go", true)
			m := b.addMethod("M", "")
			b.addBlock(m, tc.kind)
			drafts := recipes.NewCycloRecipe().Compute(b.build())
			method := findMethodDraft(t, drafts, m.GetScopeId())
			if int(method.Value) != tc.wantCyc {
				t.Errorf("decision_kind=%q cyclo = %v, want %d",
					tc.kind, method.Value, tc.wantCyc)
			}
		})
	}
}

// TestCycloRecipe_LocalIDIsPopulated -- the draft's
// `Scope.LocalID` is the parser's intra-file placeholder
// verbatim. The Metric Ingestor lifts this to a durable
// `scope_id` UUID; recipes never mint their own IDs
// (rubber-duck iter-1 critique #6).
func TestCycloRecipe_LocalIDIsPopulated(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	b.addBlock(m, "if")

	drafts := recipes.NewCycloRecipe().Compute(b.build())
	method := findMethodDraft(t, drafts, m.GetScopeId())
	if method.Scope.LocalID != m.GetScopeId() {
		t.Errorf("LocalID = %q, want %q (parser-emitted local:N must be preserved verbatim)",
			method.Scope.LocalID, m.GetScopeId())
	}
}

// TestCycloRecipe_DegradedAstSkipsEmission -- architecture
// Sec 3.4 lines 490-494 pins the rule for computed-tier
// rows: "Computed rows are never `degraded=true`: if an
// input is missing the row is not written, not stamped
// degraded -- only system-tier derivation stamps
// `degraded=true`". The iter-1 evaluator's item-1 caught the
// previous version smuggling `DegradedReason` onto the
// draft; the new contract is that a degraded AST produces
// ZERO drafts (both via `AppliesTo` and `Compute`).
func TestCycloRecipe_DegradedAstSkipsEmission(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	m := b.addMethod("M", "")
	b.addBlock(m, "if")
	ast := b.build()
	ast.DegradedReason = "parse_truncated"

	r := recipes.NewCycloRecipe()
	if r.AppliesTo(ast) {
		t.Errorf("AppliesTo(degraded ast) = true, want false (Sec 3.4: input missing => row not written)")
	}
	drafts := r.Compute(ast)
	if len(drafts) != 0 {
		t.Errorf("Compute(degraded ast) emitted %d drafts; want 0 (computed rows are never degraded=true, never written off a degraded input)", len(drafts))
	}
}

// TestCycloRecipe_NilAstIsNoop -- the recipe never panics on
// a nil AST and produces no drafts.
func TestCycloRecipe_NilAstIsNoop(t *testing.T) {
	t.Parallel()
	got := recipes.NewCycloRecipe().Compute(nil)
	if got != nil {
		t.Errorf("Compute(nil) = %v, want nil", got)
	}
}

// TestCycloRecipe_ClosureBoundaryStopsDescent -- a nested
// method scope (closure) inside another method is NOT counted
// in the parent's cyclo (the closure has its own row).
func TestCycloRecipe_ClosureBoundaryStopsDescent(t *testing.T) {
	t.Parallel()
	b := newAstBuilder("p.go", true)
	outer := b.addMethod("Outer", "")
	b.addBlock(outer, "if") // +1 on outer
	inner := b.addMethod("Inner", outer.GetScopeId())
	b.addBlock(inner, "for") // belongs to Inner

	drafts := recipes.NewCycloRecipe().Compute(b.build())
	outerD := findMethodDraft(t, drafts, outer.GetScopeId())
	innerD := findMethodDraft(t, drafts, inner.GetScopeId())
	if outerD.Value != 2 {
		t.Errorf("Outer cyclo = %v, want 2 (closure's for must NOT be counted in Outer)", outerD.Value)
	}
	if innerD.Value != 2 {
		t.Errorf("Inner cyclo = %v, want 2", innerD.Value)
	}
}

// TestCycloRecipe_RealParserSkippedByGate -- the rubber-duck
// iter-1 critique #10 "add a small contract test around real
// parser output to assert current behaviour is intentionally
// gated/skipped rather than silently baseline-valued". Today's
// Stage 2.1 parser fleet does NOT yet stamp
// `decision_blocks=true`, so AppliesTo MUST return false and
// Compute (if called regardless) emits the empty file draft
// because there are zero block-decisions to find.
func TestCycloRecipe_RealParserSkippedByGate(t *testing.T) {
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
	if got := out.GetAttrs()["decision_blocks"]; got == "true" {
		t.Fatalf("Stage 2.1 parser advertised decision_blocks=true; this test guarded against that. Update the recipes once the parser actually decomposes method bodies.")
	}
	r := recipes.NewCycloRecipe()
	if r.AppliesTo(out) {
		t.Fatalf("AppliesTo returned true on Stage 2.1 parser output; capability gate is broken")
	}
}
