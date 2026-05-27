package decoupling

import (
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
)

// collectMetricKindLiterals walks a parsed DSL AST and
// returns every string literal that appears as the OTHER
// operand of a `metric_kind` field comparison -- i.e. the
// `'cycle_member'` in `metric_kind == 'cycle_member'`. The
// helper recurses through every compound node
// ([dsl.OrNode] / [dsl.AndNode] / [dsl.NotNode]) so a deeply
// nested predicate is fully scanned.
//
// The returned slice may contain duplicates (e.g. a predicate
// like `metric_kind == 'fan_in' OR metric_kind == 'fan_in'`
// emits two entries) -- callers that need a set MUST
// deduplicate; here we keep the slice flat so a test can also
// assert on multiplicity if it wants to.
//
// Lives in a non-test file so a future bootstrap step (Stage
// 5.5 / 5.6 follow-up) that loads these rulepacks at startup
// can reuse the same walker to surface the metric_kind set a
// rule depends on (for example, to gate-protect a rule from
// running when the upstream materialiser is absent).
func collectMetricKindLiterals(node dsl.Node) []string {
	var out []string
	walkAST(node, func(n dsl.Node) {
		if c, ok := n.(dsl.CompareNode); ok {
			if lit, hit := metricKindLiteral(c); hit {
				out = append(out, lit)
			}
		}
	})
	return out
}

// collectThresholdIDs walks node and returns the raw uuid
// strings of every `threshold('<uuid>')` atom. Strings -- not
// uuid.UUIDs -- because the caller (the YAML pinning test)
// compares the captured text against the literal UUID inside
// the YAML file to catch a typo before the [dsl.Bind] step
// has a chance to mask it as "unknown threshold_id". Order is
// pre-order traversal so multiple thresholds in the same
// rulepack stay in their declared order.
func collectThresholdIDs(node dsl.Node) []string {
	var out []string
	walkAST(node, func(n dsl.Node) {
		if t, ok := n.(dsl.ThresholdNode); ok {
			out = append(out, t.IDText)
		}
	})
	return out
}

// collectAllMetricKinds walks node and returns the union of
// (a) every metric_kind LITERAL referenced via a
// `metric_kind == '<lit>'` comparison, and (b) every
// metric_kind that a `threshold('<uuid>')` atom RESOLVES TO
// when bound against [Resolver].
//
// This is the canonical "what metric_kinds does this
// predicate touch?" answer the Stage 5.6 brief asks for ("a
// test asserting each predicate references only canonical
// metric_kinds"). With the v1 coupling / duplication
// rulepacks using `threshold('<uuid>')` atoms, the literal
// walker alone would return empty for those rules; this
// helper resolves the thresholds and surfaces their
// `MetricKind` field so the canon-guard check still applies.
//
// Returns ([]string, error) -- a threshold whose UUID is not
// in [Resolver]'s map is an immediate error (the YAML
// references an unknown UUID = the rulepack and the
// thresholds.go canonical table have drifted). Order is
// "literals first, then threshold-bound, both in walk order".
func collectAllMetricKinds(node dsl.Node) ([]string, error) {
	resolver := Resolver()
	out := collectMetricKindLiterals(node)
	for _, idText := range collectThresholdIDs(node) {
		id, err := uuid.FromString(idText)
		if err != nil {
			return nil, err
		}
		t, err := resolver.Lookup(id)
		if err != nil {
			return nil, err
		}
		out = append(out, t.MetricKind)
	}
	return out, nil
}

// walkAST traverses node depth-first and invokes visit on
// every visited node. Replaces the older type-switched
// "collect metric_kind literals" inline helper so multiple
// extractors (literals, threshold IDs, ...) can share the
// same traversal without duplicating the type switch.
//
// An unhandled node kind (a future addition like a not-yet-
// imagined [dsl.RangeNode]) is a no-op rather than a panic
// so a future AST addition does not break existing callers.
func walkAST(node dsl.Node, visit func(dsl.Node)) {
	if node == nil {
		return
	}
	visit(node)
	switch n := node.(type) {
	case dsl.OrNode:
		for _, c := range n.Children {
			walkAST(c, visit)
		}
	case dsl.AndNode:
		for _, c := range n.Children {
			walkAST(c, visit)
		}
	case dsl.NotNode:
		walkAST(n.Child, visit)
	case dsl.CompareNode:
		walkAST(n.LHS, visit)
		walkAST(n.RHS, visit)
	}
}

// metricKindLiteral returns (literal, true) when one side of
// the comparison is the `metric_kind` field and the other is
// a string literal. Order-independent: handles both
// `metric_kind == 'X'` and `'X' == metric_kind`.
func metricKindLiteral(n dsl.CompareNode) (string, bool) {
	if isMetricKindField(n.LHS) {
		if s, ok := stringLiteral(n.RHS); ok {
			return s, true
		}
	}
	if isMetricKindField(n.RHS) {
		if s, ok := stringLiteral(n.LHS); ok {
			return s, true
		}
	}
	return "", false
}

// isMetricKindField reports whether n is the `metric_kind`
// field reference. Used by [metricKindLiteral] to decide
// which side of a [dsl.CompareNode] to read as the literal.
func isMetricKindField(n dsl.Node) bool {
	f, ok := n.(dsl.FieldNode)
	return ok && f.Field == "metric_kind"
}

// stringLiteral returns (value, true) when n is a string
// literal node. Returns ("", false) otherwise.
func stringLiteral(n dsl.Node) (string, bool) {
	sl, ok := n.(dsl.StringLitNode)
	if !ok {
		return "", false
	}
	return sl.Value, true
}
