package dsl

import (
	"errors"
	"strings"
	"testing"
)

// TestParser_WellFormedPredicates covers the happy path for
// the grammar's atom/operator/precedence shapes. Each case
// is a predicate the Stage 5.5 / 5.6 rule packs are expected
// to author, plus a few hand-written shapes that exercise
// nested precedence.
func TestParser_WellFormedPredicates(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		src  string
	}{
		{"single_compare_string", "metric_kind == 'lcom4'"},
		{"single_compare_number", "value > 10"},
		{"and_chain",
			"metric_kind == 'lcom4' AND scope_kind == 'class' AND value > 10"},
		{"or_chain",
			"metric_kind == 'fan_in' OR metric_kind == 'fan_out'"},
		{"and_or_precedence",
			"metric_kind == 'lcom4' AND value > 10 OR metric_kind == 'fan_in'"},
		{"parenthesised_grouping",
			"(metric_kind == 'lcom4' OR metric_kind == 'fan_in') AND value > 10"},
		{"not_atom",
			"NOT (metric_kind == 'lcom4')"},
		{"not_binds_tighter_than_and",
			"NOT metric_kind == 'lcom4' AND value > 10"},
		{"degraded_bool_compare",
			"degraded == false"},
		{"pack_filter",
			"pack == 'solid'"},
		{"source_filter",
			"source == 'computed'"},
		{"scope_kind_filter",
			"scope_kind == 'class'"},
		{"case_insensitive_keywords",
			"metric_kind == 'lcom4' and value > 10 or scope_kind == 'class'"},
		{"value_eq",
			"value == 0"},
		{"cycle_member_int",
			"metric_kind == 'cycle_member' AND value >= 1"},
		{"line_comment_ok",
			"# rule: SRP\nmetric_kind == 'lcom4' AND value > 10"},
		{"deep_nested",
			"NOT (NOT (metric_kind == 'lcom4' OR (scope_kind == 'class' AND value > 0)))"},
		{"true_literal_atom",
			"true"},
		{"false_literal_atom",
			"false"},
		{"degraded_true_compare",
			"degraded == true"},
		{"coverage_line_ratio_compare",
			"metric_kind == 'coverage_line_ratio' AND value < 0.8"},
		{"coverage_branch_ratio_compare",
			"metric_kind == 'coverage_branch_ratio' AND value < 0.6"},
		{"coverage_ratio_or_chain",
			"metric_kind == 'coverage_line_ratio' OR metric_kind == 'coverage_branch_ratio'"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			node, err := Parse(c.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", c.src, err)
			}
			if node == nil {
				t.Fatalf("Parse(%q) returned nil node", c.src)
			}
		})
	}
}

// TestParser_RejectsMalformed pins the line/column error
// shape the Stage 5.4 acceptance criterion mandates.
func TestParser_RejectsMalformed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		src        string
		wantKind   error
		wantSubstr string
	}{
		{
			name:       "empty_input",
			src:        "",
			wantKind:   ErrParse,
			wantSubstr: "expected operand",
		},
		{
			name:       "trailing_junk_after_predicate",
			src:        "metric_kind == 'lcom4' value > 10",
			wantKind:   ErrParse,
			wantSubstr: "trailing",
		},
		{
			name:       "missing_rhs",
			src:        "metric_kind ==",
			wantKind:   ErrParse,
			wantSubstr: "expected operand",
		},
		{
			name:       "missing_close_paren",
			src:        "(metric_kind == 'lcom4'",
			wantKind:   ErrParse,
			wantSubstr: "expected '",
		},
		{
			name:       "no_comparison_op",
			src:        "metric_kind 'lcom4'",
			wantKind:   ErrParse,
			wantSubstr: "comparison operator",
		},
		{
			name:       "unknown_field",
			src:        "metric_KIND == 'lcom4'",
			wantKind:   ErrSemantic,
			wantSubstr: "unknown field",
		},
		{
			name:       "ordering_on_string_field",
			src:        "metric_kind > 'cyclo'",
			wantKind:   ErrType,
			wantSubstr: "ordering operator",
		},
		{
			name:       "type_mismatch_value_eq_string",
			src:        "value == 'foo'",
			wantKind:   ErrType,
			wantSubstr: "type mismatch",
		},
		{
			name:       "type_mismatch_degraded_eq_number",
			src:        "degraded == 1",
			wantKind:   ErrType,
			wantSubstr: "type mismatch",
		},
		{
			name:       "unknown_metric_kind",
			src:        "metric_kind == 'lines_of_code'",
			wantKind:   ErrSemantic,
			wantSubstr: "unknown metric_kind",
		},
		{
			name:       "unknown_metric_kind_coverage_line_alias",
			src:        "metric_kind == 'coverage_line'",
			wantKind:   ErrSemantic,
			wantSubstr: "unknown metric_kind",
		},
		{
			name:       "unknown_metric_kind_coverage_branch_alias",
			src:        "metric_kind == 'coverage_branch'",
			wantKind:   ErrSemantic,
			wantSubstr: "unknown metric_kind",
		},
		{
			name:       "unknown_scope_kind",
			src:        "scope_kind == 'function'",
			wantKind:   ErrSemantic,
			wantSubstr: "unknown scope_kind",
		},
		{
			name:       "unknown_pack",
			src:        "pack == 'core'",
			wantKind:   ErrSemantic,
			wantSubstr: "unknown pack",
		},
		{
			name:       "unknown_source",
			src:        "source == 'manual'",
			wantKind:   ErrSemantic,
			wantSubstr: "unknown source",
		},
		{
			name:       "lex_bare_equals",
			src:        "value = 10",
			wantKind:   ErrLex,
			wantSubstr: "expected '=='",
		},
		{
			name:       "lex_unterminated_string",
			src:        "metric_kind == 'lcom4",
			wantKind:   ErrLex,
			wantSubstr: "unterminated string",
		},
		{
			name:       "threshold_missing_arg",
			src:        "threshold()",
			wantKind:   ErrParse,
			wantSubstr: "threshold() takes a single string-literal",
		},
		{
			name:       "threshold_non_string_arg",
			src:        "threshold(10)",
			wantKind:   ErrParse,
			wantSubstr: "threshold() takes a single string-literal",
		},
		{
			name:       "and_without_rhs",
			src:        "metric_kind == 'lcom4' AND",
			wantKind:   ErrParse,
			wantSubstr: "expected operand",
		},
		{
			name:       "or_without_rhs",
			src:        "metric_kind == 'lcom4' OR",
			wantKind:   ErrParse,
			wantSubstr: "expected operand",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(c.src)
			if err == nil {
				t.Fatalf("Parse(%q): want error, got nil", c.src)
			}
			if !errors.Is(err, c.wantKind) {
				t.Errorf("Parse(%q): kind=%v, want %v (err=%v)", c.src, errors.Unwrap(err), c.wantKind, err)
			}
			if !strings.Contains(err.Error(), c.wantSubstr) {
				t.Errorf("Parse(%q): err=%q, want substring %q", c.src, err.Error(), c.wantSubstr)
			}
			// Every error MUST carry a non-zero
			// [Position] so callers can render a
			// caret. The Stage 5.4 acceptance criterion
			// line 500 pins this.
			var dsErr *Error
			if errors.As(err, &dsErr) {
				if dsErr.Pos.IsZero() {
					t.Errorf("Parse(%q): err has zero Position; want line/column populated", c.src)
				}
			} else {
				t.Errorf("Parse(%q): err is not a *dsl.Error: %T", c.src, err)
			}
		})
	}
}

// TestParser_PositionPointsAtOffendingToken pins the
// "line/column error messages" acceptance criterion -- the
// reported [Position] must point at the offending token, not
// at the start of the predicate.
func TestParser_PositionPointsAtOffendingToken(t *testing.T) {
	t.Parallel()
	// The bad token `lines_of_code` starts at column 16
	// (after "metric_kind == ").
	_, err := Parse("metric_kind == 'lines_of_code'")
	if err == nil {
		t.Fatalf("expected error")
	}
	var dsErr *Error
	if !errors.As(err, &dsErr) {
		t.Fatalf("err = %T %v", err, err)
	}
	if dsErr.Pos.Line != 1 || dsErr.Pos.Column != 16 {
		t.Errorf("Pos = %s, want 1:16", dsErr.Pos)
	}
}

// TestParser_ThresholdCallShape covers the syntactic shape
// of threshold() calls. Semantic resolution is exercised in
// evaluator_test (Bind is what fails when the uuid is
// unknown).
func TestParser_ThresholdCallShape(t *testing.T) {
	t.Parallel()
	// Parses; the uuid string is captured verbatim.
	src := "threshold('11111111-1111-1111-1111-111111111111')"
	node, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	tn, ok := node.(ThresholdNode)
	if !ok {
		t.Fatalf("Parse(%q) = %T, want ThresholdNode", src, node)
	}
	if tn.IDText != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("IDText = %q", tn.IDText)
	}
}

// TestParser_BoolLiteralSymmetric pins the bool_literal
// operand symmetry: `false == degraded`, `true == false`,
// and `degraded == false` all parse identically. The prior
// iter's parser asymmetrically consumed standalone `true` /
// `false` before attempting a comparison, so a LHS bool
// literal followed by a comparison op was misread as
// trailing junk. This test is the evaluator-feedback
// regression guard.
func TestParser_BoolLiteralSymmetric(t *testing.T) {

	t.Parallel()
	cases := []string{
		"false == degraded",
		"true == false",
		"false != true",
		"true == degraded AND value > 10",
		"false == degraded OR metric_kind == 'lcom4'",
		"NOT (false == degraded)",
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse(c); err != nil {
				t.Errorf("Parse(%q): %v", c, err)
			}
		})
	}
}

// TestParser_RejectStandaloneBoolField pins the
// counter-invariant: a standalone bool-typed field
// reference (e.g. `degraded`) is NOT a complete atom. The
// documented grammar's atom set includes `bool_literal` but
// NOT `bool_field`, and we deliberately don't accept the
// latter to keep the grammar narrow.
func TestParser_RejectStandaloneBoolField(t *testing.T) {
	t.Parallel()
	cases := []string{
		"degraded",
		"NOT degraded",
		"degraded AND metric_kind == 'lcom4'",
	}
	for _, c := range cases {
		c := c
		t.Run(c, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(c)
			if err == nil {
				t.Errorf("Parse(%q): want error, got nil", c)
				return
			}
			if !errors.Is(err, ErrParse) {
				t.Errorf("Parse(%q): err=%v, want ErrParse", c, err)
			}
		})
	}
}

// TestParser_CompositeRulePack covers a representative
// shape from the Stage 5.6 decoupling rulepack.
func TestParser_CompositeRulePack(t *testing.T) {
	t.Parallel()
	src := "# decoupling: cycles\nmetric_kind == 'cycle_member' AND value >= 1\n"
	if _, err := Parse(src); err != nil {
		t.Fatalf("Parse: %v", err)
	}
}
