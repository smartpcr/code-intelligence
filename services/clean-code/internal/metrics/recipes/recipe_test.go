package recipes_test

import (
	"testing"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// TestPackAndSourceConstants_AreCanonical pins the literal
// string values of the package-level constants so a typo
// surfaces as a test failure, not a silent FK violation on
// `MetricSample.pack` / `MetricSample.source` at insert.
// (Architecture Sec 5.2.1 lines 901-902 pin the closed enums.)
func TestPackAndSourceConstants_AreCanonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"PackBase", string(recipes.PackBase), "base"},
		{"PackSolid", string(recipes.PackSolid), "solid"},
		{"PackIngested", string(recipes.PackIngested), "ingested"},
		{"PackSystem", string(recipes.PackSystem), "system"},
		{"SourceComputed", string(recipes.SourceComputed), "computed"},
		{"SourceIngested", string(recipes.SourceIngested), "ingested"},
		{"SourceDerived", string(recipes.SourceDerived), "derived"},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// TestAttrDecisionBlocks_IsCanonical pins the capability
// attr key. A drift here would silently break the gate (the
// recipe would never apply because no producer would advertise
// the new spelling).
func TestAttrDecisionBlocks_IsCanonical(t *testing.T) {
	t.Parallel()
	if got := recipes.AttrDecisionBlocks; got != "decision_blocks" {
		t.Fatalf("AttrDecisionBlocks = %q, want %q", got, "decision_blocks")
	}
}

// TestAttrDecisionKind_IsCanonical pins the per-block-scope
// taxonomy attr key. Producers (parser stages 2.1+) cite
// this literal in their attr-stamp code; drift would break
// every recipe's decision lookup.
func TestAttrDecisionKind_IsCanonical(t *testing.T) {
	t.Parallel()
	if got := recipes.AttrDecisionKind; got != "decision_kind" {
		t.Fatalf("AttrDecisionKind = %q, want %q", got, "decision_kind")
	}
}

// TestDecisionKinds_AreCanonical pins every public
// [recipes.DecisionKind] constant to its literal string. The
// values are the closed contract between parser and recipe;
// any drift here would silently change the metric semantics
// for every emitted row.
func TestDecisionKinds_AreCanonical(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		got  recipes.DecisionKind
		want string
	}{
		{"DecisionIf", recipes.DecisionIf, "if"},
		{"DecisionElseIf", recipes.DecisionElseIf, "else_if"},
		{"DecisionElse", recipes.DecisionElse, "else"},
		{"DecisionFor", recipes.DecisionFor, "for"},
		{"DecisionWhile", recipes.DecisionWhile, "while"},
		{"DecisionDoWhile", recipes.DecisionDoWhile, "do_while"},
		{"DecisionCase", recipes.DecisionCase, "case"},
		{"DecisionCatch", recipes.DecisionCatch, "catch"},
		{"DecisionTernary", recipes.DecisionTernary, "ternary"},
		{"DecisionLogicalAnd", recipes.DecisionLogicalAnd, "logical_and"},
		{"DecisionLogicalOr", recipes.DecisionLogicalOr, "logical_or"},
		{"DecisionBreakLabel", recipes.DecisionBreakLabel, "break_label"},
		{"DecisionContLabel", recipes.DecisionContLabel, "continue_label"},
		{"DecisionGoto", recipes.DecisionGoto, "goto"},
		{"DecisionRecursion", recipes.DecisionRecursion, "recursion"},
	}
	for _, tc := range tests {
		if string(tc.got) != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, string(tc.got), tc.want)
		}
	}
}

// TestRecipeInterfaceConformance is a compile-time + runtime
// assertion that the cyclo / cognitive_complexity recipes
// implement [recipes.Recipe]. Compile-time via the var-typed
// assignment; runtime via a no-op call to each method to
// catch nil dispatch surprises.
func TestRecipeInterfaceConformance(t *testing.T) {
	t.Parallel()
	var cy recipes.Recipe = recipes.NewCycloRecipe()
	var co recipes.Recipe = recipes.NewCognitiveComplexityRecipe()
	if cy.MetricKind() == "" || co.MetricKind() == "" {
		t.Errorf("MetricKind empty: cy=%q co=%q", cy.MetricKind(), co.MetricKind())
	}
	if cy.Version() == 0 || co.Version() == 0 {
		t.Errorf("Version returns zero: cy=%d co=%d", cy.Version(), co.Version())
	}
	if cy.Pack() != recipes.PackBase || co.Pack() != recipes.PackBase {
		t.Errorf("base-pack recipes Pack(): cy=%q co=%q, want base", cy.Pack(), co.Pack())
	}
	if cy.AppliesTo(nil) || co.AppliesTo(nil) {
		t.Errorf("AppliesTo(nil) must be false")
	}
}

// TestSolidRecipesInterfaceConformance asserts the three
// Stage 2.4 SOLID-pack recipes implement [recipes.Recipe]
// with Pack()==PackSolid.
func TestSolidRecipesInterfaceConformance(t *testing.T) {
	t.Parallel()
	var l recipes.Recipe = recipes.NewLCOM4Recipe()
	var fi recipes.Recipe = recipes.NewFanInRecipe()
	var fo recipes.Recipe = recipes.NewFanOutRecipe()
	for _, r := range []recipes.Recipe{l, fi, fo} {
		if r.MetricKind() == "" {
			t.Errorf("MetricKind empty for solid-pack recipe")
		}
		if r.Version() == 0 {
			t.Errorf("Version returns zero for %q", r.MetricKind())
		}
		if r.Pack() != recipes.PackSolid {
			t.Errorf("Pack() = %q for %q, want %q", r.Pack(), r.MetricKind(), recipes.PackSolid)
		}
		if r.AppliesTo(nil) {
			t.Errorf("AppliesTo(nil) must be false for %q", r.MetricKind())
		}
	}
}

// TestScopeKindMapping_IsCanonicalSevenEnum pins the
// canonical seven-value enum that every recipe in this
// package targets (architecture Sec 5.2.3 lines 1039-1050:
// `repo | package | file | class | interface | method |
// block`). `function` and `module` are NOT in the enum and
// are caught at the [newDraft] panic boundary; that panic is
// asserted directly in `recipe_internal_test.go` (package
// recipes), where the helper is reachable.
func TestScopeKindMapping_IsCanonicalSevenEnum(t *testing.T) {
	t.Parallel()
	want := []scope.Kind{
		scope.KindRepo, scope.KindPackage, scope.KindFile,
		scope.KindClass, scope.KindInterface, scope.KindMethod,
		scope.KindBlock,
	}
	for _, k := range want {
		if !k.IsValid() {
			t.Errorf("scope.Kind %q is not in IsValid set", k)
		}
	}
	// Negative side -- the iter-1 evaluator item-3 forbidden
	// values MUST fail IsValid. These are the canonical drift
	// targets (drift to `function` is the most common
	// language-parser mistake when porting Halleck45's
	// ast-metrics terminology).
	for _, bad := range []scope.Kind{"function", "module", "namespace", "method_decl"} {
		if bad.IsValid() {
			t.Errorf("scope.Kind %q is unexpectedly valid", bad)
		}
	}
}
