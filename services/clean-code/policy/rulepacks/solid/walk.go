package solid

import (
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
)

// collectMetricKindLiterals walks a parsed DSL AST and
// returns every string literal that appears as the OTHER
// operand of a `metric_kind` field comparison -- i.e. the
// `'lcom4'` in `metric_kind == 'lcom4'`. The helper recurses
// through every compound node ([dsl.OrNode] / [dsl.AndNode]
// / [dsl.NotNode]) so a deeply nested predicate is fully
// scanned.
//
// The returned slice may contain duplicates (e.g. a predicate
// like `metric_kind == 'fan_in' OR metric_kind == 'fan_in'`
// emits two entries) -- callers that need a set MUST
// deduplicate; here we keep the slice flat so a test can also
// assert on multiplicity if it wants to.
//
// The v1 SOLID rulepacks DO NOT use `threshold('<uuid>')`
// atoms (every cut-off is a literal in the predicate text),
// so this package does not need the threshold-resolving
// variant the decoupling family ships -- a pure literal
// walker is enough to answer "what metric_kinds does this
// rulepack reference?".
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

// collectScopeKindLiterals walks node and returns every
// string literal that appears as the OTHER operand of a
// `scope_kind` field comparison. Used by the conformance
// test to assert each SOLID rule pins an explicit scope_kind
// (defence-in-depth against a future drift that drops the
// scope binding from a predicate).
func collectScopeKindLiterals(node dsl.Node) []string {
	var out []string
	walkAST(node, func(n dsl.Node) {
		if c, ok := n.(dsl.CompareNode); ok {
			if lit, hit := scopeKindLiteral(c); hit {
				out = append(out, lit)
			}
		}
	})
	return out
}

// walkAST traverses node depth-first and invokes visit on
// every visited node. An unhandled node kind (a future
// addition like a not-yet-imagined [dsl.RangeNode]) is a
// no-op rather than a panic so a future AST addition does
// not break existing callers.
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
	return fieldStringLiteral(n, "metric_kind")
}

// scopeKindLiteral returns (literal, true) when one side of
// the comparison is the `scope_kind` field and the other is
// a string literal. Order-independent.
func scopeKindLiteral(n dsl.CompareNode) (string, bool) {
	return fieldStringLiteral(n, "scope_kind")
}

// fieldStringLiteral returns (value, true) when one side of
// the comparison is `field` and the other is a string
// literal. Order-independent. Used by [metricKindLiteral]
// and [scopeKindLiteral] to share the field-vs-literal type
// switch.
func fieldStringLiteral(n dsl.CompareNode, field string) (string, bool) {
	if isField(n.LHS, field) {
		if s, ok := stringLiteral(n.RHS); ok {
			return s, true
		}
	}
	if isField(n.RHS, field) {
		if s, ok := stringLiteral(n.LHS); ok {
			return s, true
		}
	}
	return "", false
}

// isField reports whether n is a reference to the named
// field. Used by [fieldStringLiteral] to decide which side
// of a [dsl.CompareNode] to read as the literal.
func isField(n dsl.Node, name string) bool {
	f, ok := n.(dsl.FieldNode)
	return ok && f.Field == name
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
