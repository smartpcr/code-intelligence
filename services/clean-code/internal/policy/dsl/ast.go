package dsl

// Node is the marker interface for every AST node. The
// parser builds a tree of these; [Bind] walks the tree to
// resolve `threshold('<uuid>')` atoms into pointer
// references; [Predicate.Eval] interprets the bound tree.
//
// Nodes carry their source [Position] so structured errors
// (e.g. a type mismatch at evaluation time) can point at the
// original predicate text.
type Node interface {
	// Pos returns the source position the node started at.
	// Used for error messages.
	Pos() Position
	// isNode is an unexported marker so external packages
	// cannot add Node implementations without going through
	// [Parse].
	isNode()
}

// nodeBase carries the position field every Node embeds.
type nodeBase struct {
	pos Position
}

func (n nodeBase) Pos() Position { return n.pos }
func (n nodeBase) isNode()       {}

// OrNode is a left-leaning chain of `A OR B OR C` --
// shape `Children[0] OR Children[1] OR ... OR Children[N-1]`.
// The parser flattens chains so the evaluator avoids a deep
// stack on long disjunctions.
type OrNode struct {
	nodeBase
	Children []Node
}

// AndNode is the conjunctive analogue of [OrNode].
type AndNode struct {
	nodeBase
	Children []Node
}

// NotNode is unary negation. `Child` is the negated
// sub-predicate.
type NotNode struct {
	nodeBase
	Child Node
}

// CompareNode is a binary comparison `LHS <op> RHS`. After
// parse + type-check both sides have a compatible static
// type (string, number, or bool); the evaluator dispatches
// on that type.
type CompareNode struct {
	nodeBase
	Op  tokenKind // one of tokEQ, tokNE, tokGT, tokGE, tokLT, tokLE
	LHS Node
	RHS Node
	// Type is the unified operand type: "string", "number",
	// "bool". Set by the parser after type-checking.
	Type string
}

// ThresholdNode is the `threshold('<uuid>')` atom. After
// [Parse] only the raw uuid string is captured (IDText).
// [Bind] resolves it against a [ThresholdResolver] and
// stores the resolved row in Bound.
type ThresholdNode struct {
	nodeBase
	IDText string
	// IDPos is the position of the uuid string literal --
	// used to point bind errors at the offending argument.
	IDPos Position
	// Bound is set by [Bind]; nil on a freshly parsed AST.
	Bound *Threshold
}

// FieldNode names a [Sample] field as the operand of a
// comparison. Field is one of `metric_kind`, `scope_kind`,
// `value`, `pack`, `source`, `degraded` (the closed set
// pinned in [doc.go]).
//
// Type carries the static type of the field; the parser
// uses it for the type-unification check on the enclosing
// [CompareNode].
type FieldNode struct {
	nodeBase
	Field string
	Type  string
}

// StringLitNode is a string-typed operand.
type StringLitNode struct {
	nodeBase
	Value string
}

// NumberLitNode is a numeric operand.
type NumberLitNode struct {
	nodeBase
	Value float64
}

// BoolLitNode is a boolean operand or standalone atom.
type BoolLitNode struct {
	nodeBase
	Value bool
}
