package dsl

import (
	"fmt"

	"github.com/gofrs/uuid"
)

// Predicate is a parsed + bound predicate, ready for
// high-frequency [Predicate.Eval] calls. The root holds the
// AST returned by [Parse] with every [ThresholdNode]
// resolved against the [ThresholdResolver] supplied at
// [Bind] time.
//
// Predicates are immutable -- the [Cache] hands the same
// `*Predicate` to every concurrent caller. [Eval] takes its
// own copy of the [Sample] value-type input so concurrent
// callers do not contend.
type Predicate struct {
	root Node
	// source preserves the original DSL string so a future
	// diagnostic surface (e.g. an admin UI listing the
	// active rules) can render the predicate the operator
	// authored.
	source string
}

// Source returns the original DSL text the predicate was
// compiled from. Used by the Insights surface to round-trip
// the active rules into operator-facing output.
func (p *Predicate) Source() string { return p.source }

// Compile is the canonical end-to-end builder: parse + bind.
// Equivalent to:
//
//	node, err := Parse(src)
//	if err != nil { return nil, err }
//	return Bind(node, src, resolver)
//
// Returns an [*Error] on any phase failure.
func Compile(src string, resolver ThresholdResolver) (*Predicate, error) {
	node, err := Parse(src)
	if err != nil {
		return nil, err
	}
	return Bind(node, src, resolver)
}

// Bind walks node and resolves every [ThresholdNode] against
// the supplied [ThresholdResolver]. Returns an [*Error] with
// Kind=[ErrBind] if any threshold_id is unknown to the
// resolver, or its argument is not a well-formed UUID.
//
// Bind also re-validates every resolved [Threshold] against
// the canonical metric_kind / scope_kind / op closed sets,
// guarding against an upstream Threshold row that somehow
// landed with a non-canonical value.
//
// The `src` argument is retained on the returned [Predicate]
// for diagnostic round-tripping; pass the original DSL text.
func Bind(node Node, src string, resolver ThresholdResolver) (*Predicate, error) {
	// A nil resolver is legal IFF the AST has no
	// ThresholdNode. We discover that lazily during the
	// walk via bindNode, so callers who know their predicate
	// has no thresholds need not synthesise an empty
	// resolver.
	bound, err := bindNode(node, resolver)
	if err != nil {
		return nil, err
	}
	return &Predicate{root: bound, source: src}, nil
}

func bindNode(n Node, resolver ThresholdResolver) (Node, error) {
	switch v := n.(type) {
	case OrNode:
		out := OrNode{nodeBase: v.nodeBase, Children: make([]Node, len(v.Children))}
		for i, c := range v.Children {
			b, err := bindNode(c, resolver)
			if err != nil {
				return nil, err
			}
			out.Children[i] = b
		}
		return out, nil
	case AndNode:
		out := AndNode{nodeBase: v.nodeBase, Children: make([]Node, len(v.Children))}
		for i, c := range v.Children {
			b, err := bindNode(c, resolver)
			if err != nil {
				return nil, err
			}
			out.Children[i] = b
		}
		return out, nil
	case NotNode:
		b, err := bindNode(v.Child, resolver)
		if err != nil {
			return nil, err
		}
		return NotNode{nodeBase: v.nodeBase, Child: b}, nil
	case CompareNode:
		// Compare's operands are leaves; nothing to bind.
		return v, nil
	case FieldNode, StringLitNode, NumberLitNode, BoolLitNode:
		return v, nil
	case ThresholdNode:
		if resolver == nil {
			return nil, newError(ErrBind, v.IDPos,
				"threshold('%s') referenced but no ThresholdResolver supplied to Bind", v.IDText)
		}
		id, err := uuid.FromString(v.IDText)
		if err != nil {
			return nil, newError(ErrBind, v.IDPos,
				"threshold() argument %q is not a valid UUID: %v", v.IDText, err)
		}
		t, err := resolver.Lookup(id)
		if err != nil {
			return nil, newWrappedError(ErrBind, v.IDPos, err,
				"threshold('%s') %v", v.IDText, err)
		}
		// Guard against a stale or mis-keyed resolver
		// returning a Threshold whose ThresholdID does
		// not match the requested key. Threshold rows
		// are immutable (architecture G3) and uniquely
		// keyed by their own threshold_id, so any
		// mismatch is an upstream bug we must refuse
		// rather than silently bind the wrong row.
		if t.ThresholdID != id {
			return nil, newError(ErrBind, v.IDPos,
				"threshold('%s') resolver returned mismatched threshold_id %s",
				v.IDText, t.ThresholdID)
		}
		if err := t.Validate(); err != nil {
			return nil, newError(ErrBind, v.IDPos,
				"threshold('%s') resolved to invalid row: %v", v.IDText, err)
		}
		// Defensive copy so a subsequent mutation of the
		// resolver's map cannot affect our captured row.
		tcopy := t
		return ThresholdNode{
			nodeBase: v.nodeBase,
			IDText:   v.IDText,
			IDPos:    v.IDPos,
			Bound:    &tcopy,
		}, nil
	}
	return nil, fmt.Errorf("dsl: bindNode: unhandled node type %T", n)
}

// Eval returns the boolean result of applying the predicate
// to sample. Pure -- no IO, no mutation, no goroutine state.
// The Stage 5.4 `dsl-deterministic` test scenario asserts
// that two calls with the same inputs return the same result.
//
// Eval returns an error only if the AST contains an
// unresolved [ThresholdNode] (which should be unreachable
// for a [Predicate] produced by [Compile] / [Bind]) or an
// unhandled node type (an internal invariant violation).
// A regular "predicate did not match this sample" outcome
// is `(false, nil)`.
func (p *Predicate) Eval(sample Sample) (bool, error) {
	return evalNode(p.root, sample)
}

func evalNode(n Node, sample Sample) (bool, error) {
	switch v := n.(type) {
	case OrNode:
		for _, c := range v.Children {
			b, err := evalNode(c, sample)
			if err != nil {
				return false, err
			}
			if b {
				return true, nil
			}
		}
		return false, nil
	case AndNode:
		for _, c := range v.Children {
			b, err := evalNode(c, sample)
			if err != nil {
				return false, err
			}
			if !b {
				return false, nil
			}
		}
		return true, nil
	case NotNode:
		b, err := evalNode(v.Child, sample)
		if err != nil {
			return false, err
		}
		return !b, nil
	case BoolLitNode:
		return v.Value, nil
	case CompareNode:
		return evalCompare(v, sample)
	case ThresholdNode:
		if v.Bound == nil {
			return false, newError(ErrBind, v.IDPos,
				"threshold('%s') is not bound; call dsl.Bind before Eval", v.IDText)
		}
		return evalThreshold(v.Bound, sample), nil
	}
	return false, fmt.Errorf("dsl: evalNode: unhandled node type %T", n)
}

// evalCompare applies a [CompareNode] to a [Sample]. The
// type-check at [Parse] time has already established that
// both operands have a unifying type, so we dispatch on the
// node's recorded Type.
func evalCompare(n CompareNode, sample Sample) (bool, error) {
	switch n.Type {
	case "string":
		l, lok := stringOperand(n.LHS, sample)
		r, rok := stringOperand(n.RHS, sample)
		if !lok || !rok {
			return false, nil
		}
		switch n.Op {
		case tokEQ:
			return l == r, nil
		case tokNE:
			return l != r, nil
		}
	case "number":
		l, lok := numberOperand(n.LHS, sample)
		r, rok := numberOperand(n.RHS, sample)
		if !lok || !rok {
			// Missing numeric value (e.g. degraded sample
			// with value=NULL) -> comparison is false
			// rather than an error. The rule engine
			// records the sample's degraded flag
			// separately; we don't want a degraded row
			// to noisily error every rule that touches
			// `value`.
			return false, nil
		}
		switch n.Op {
		case tokEQ:
			return l == r, nil
		case tokNE:
			return l != r, nil
		case tokGT:
			return l > r, nil
		case tokGE:
			return l >= r, nil
		case tokLT:
			return l < r, nil
		case tokLE:
			return l <= r, nil
		}
	case "bool":
		l, lok := boolOperand(n.LHS, sample)
		r, rok := boolOperand(n.RHS, sample)
		if !lok || !rok {
			return false, nil
		}
		switch n.Op {
		case tokEQ:
			return l == r, nil
		case tokNE:
			return l != r, nil
		}
	}
	return false, fmt.Errorf("dsl: evalCompare: unhandled (type=%s, op=%s)", n.Type, n.Op)
}

// stringOperand reads a string-typed operand from sample.
// Returns (value, true) on success, (_, false) when the
// field is not populated -- e.g. a Sample missing ScopeKind.
func stringOperand(n Node, sample Sample) (string, bool) {
	switch v := n.(type) {
	case StringLitNode:
		return v.Value, true
	case FieldNode:
		switch v.Field {
		case "metric_kind":
			return sample.MetricKind, sample.MetricKind != ""
		case "scope_kind":
			return sample.ScopeKind, sample.ScopeKind != ""
		case "pack":
			return sample.Pack, sample.Pack != ""
		case "source":
			return sample.Source, sample.Source != ""
		}
	}
	return "", false
}

// numberOperand reads a numeric operand from sample.
// Returns (value, true) on success. A nil [Sample.HasValue]
// for the `value` field returns false so the surrounding
// comparison short-circuits to false.
func numberOperand(n Node, sample Sample) (float64, bool) {
	switch v := n.(type) {
	case NumberLitNode:
		return v.Value, true
	case FieldNode:
		if v.Field == "value" {
			return sample.Value, sample.HasValue
		}
	}
	return 0, false
}

// boolOperand reads a boolean operand from sample.
func boolOperand(n Node, sample Sample) (bool, bool) {
	switch v := n.(type) {
	case BoolLitNode:
		return v.Value, true
	case FieldNode:
		if v.Field == "degraded" {
			return sample.Degraded, true
		}
	}
	return false, false
}

// evalThreshold applies a bound [Threshold] to a [Sample].
// The full triple-check (metric_kind matches AND scope_kind
// matches AND value comparison passes) MUST hold. A
// metric_kind / scope_kind mismatch returns false (the
// threshold simply does not apply to this sample); a value
// mismatch returns false; a missing value returns false.
func evalThreshold(t *Threshold, sample Sample) bool {
	if sample.MetricKind != t.MetricKind {
		return false
	}
	if sample.ScopeKind != t.ScopeKind {
		return false
	}
	if !sample.HasValue {
		return false
	}
	return t.Op.Compare(sample.Value, t.Value)
}
