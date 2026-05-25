package dsl

import (
	"fmt"
	"strconv"
)

// fieldType returns the static type of a field reference.
// Used by [parser.typeCheck] to verify a comparison's LHS
// and RHS unify before [Parse] returns.
func fieldType(name string) (string, bool) {
	switch name {
	case "metric_kind", "scope_kind", "pack", "source":
		return "string", true
	case "value":
		return "number", true
	case "degraded":
		return "bool", true
	default:
		return "", false
	}
}

// Parse compiles src into an unbound AST. The returned root
// is suitable for [Bind] -- it has been:
//
//   - lexed (whitespace stripped, tokens emitted with line/col);
//
//   - parsed against the precedence ladder
//     `OR < AND < NOT < atom`;
//
//   - parse-time type-checked (a comparison's operands have
//     unifying types -- `value == 'foo'` rejected before
//     [Bind]);
//
//   - canon-guarded (string literals in `metric_kind`,
//     `scope_kind`, `pack`, `source` comparisons are
//     members of the canonical closed set -- this is the
//     `dsl-rejects-unknown-metric-kind` canon-guard).
//
// On failure Parse returns an [*Error] whose Kind is one of
// [ErrLex], [ErrParse], [ErrSemantic], [ErrType] and whose
// Position points at the offending token. The
// implementation-plan Stage 5.4 acceptance criterion line
// 500 ("rejection of malformed ones with line/column error
// messages") is met by this shape.
func Parse(src string) (Node, error) {
	p, err := newParser(src)
	if err != nil {
		return nil, err
	}
	node, err := p.parsePredicate()
	if err != nil {
		return nil, err
	}
	if p.current.kind != tokEOF {
		return nil, newError(ErrParse, p.current.pos,
			"unexpected trailing %s after predicate", p.current.kind)
	}
	return node, nil
}

// parser implements a hand-written recursive descent parser
// with one-token lookahead.
type parser struct {
	lex     *lexer
	current token
}

func newParser(src string) (*parser, error) {
	p := &parser{lex: newLexer(src)}
	if err := p.advance(); err != nil {
		return nil, err
	}
	return p, nil
}

// advance moves to the next token; returns any lex error.
func (p *parser) advance() error {
	tok, err := p.lex.nextToken()
	if err != nil {
		return err
	}
	p.current = tok
	return nil
}

// expect asserts the current token kind and advances. On
// mismatch returns an [ErrParse] with a precise diagnostic.
func (p *parser) expect(kind tokenKind, ctx string) (token, error) {
	if p.current.kind != kind {
		return token{}, newError(ErrParse, p.current.pos,
			"expected %s in %s but got %s", kind, ctx, p.current.kind)
	}
	tok := p.current
	if err := p.advance(); err != nil {
		return token{}, err
	}
	return tok, nil
}

// parsePredicate is the grammar entry: an OR-expression.
func (p *parser) parsePredicate() (Node, error) {
	return p.parseOr()
}

func (p *parser) parseOr() (Node, error) {
	first, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	if p.current.kind != tokOr {
		return first, nil
	}
	pos := first.Pos()
	children := []Node{first}
	for p.current.kind == tokOr {
		if err := p.advance(); err != nil {
			return nil, err
		}
		rhs, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		children = append(children, rhs)
	}
	return OrNode{nodeBase: nodeBase{pos: pos}, Children: children}, nil
}

func (p *parser) parseAnd() (Node, error) {
	first, err := p.parseNot()
	if err != nil {
		return nil, err
	}
	if p.current.kind != tokAnd {
		return first, nil
	}
	pos := first.Pos()
	children := []Node{first}
	for p.current.kind == tokAnd {
		if err := p.advance(); err != nil {
			return nil, err
		}
		rhs, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		children = append(children, rhs)
	}
	return AndNode{nodeBase: nodeBase{pos: pos}, Children: children}, nil
}

func (p *parser) parseNot() (Node, error) {
	if p.current.kind == tokNot {
		pos := p.current.pos
		if err := p.advance(); err != nil {
			return nil, err
		}
		child, err := p.parseNot()
		if err != nil {
			return nil, err
		}
		// Per the documented grammar, NOT may be applied
		// to any atom -- parens, threshold calls, NOT
		// chains, comparisons, and bool_literal atoms.
		// Standalone field references (including
		// `degraded`) are NOT atoms; if a caller wrote
		// `NOT degraded`, parseAtom will already have
		// rejected `degraded` standalone before we got
		// here.
		return NotNode{nodeBase: nodeBase{pos: pos}, Child: child}, nil
	}
	return p.parseAtom()
}

// parseAtom dispatches the atom production:
//
//	atom ::= "(" predicate ")"
//	      | threshold_call
//	      | comparison
//	      | bool_literal
//
// `(` and `threshold` get special treatment because their
// shape is recognisable from the leading token alone. For
// everything else we parse a LEFT operand and then look
// ahead: if a comparison operator follows we parse the
// rest of a comparison; otherwise we accept the operand
// only if it is a bare boolean literal (the grammar's
// `bool_literal` atom).
//
// This is the structural form of the parser that makes the
// bool_literal atom symmetric on both sides of a
// comparison: `true == degraded`, `false == true`, and
// `degraded == false` all parse identically. Standalone
// field references (e.g. `degraded` on its own) remain
// rejected -- the documented atom set only includes
// `bool_literal`, not `bool_field`.
func (p *parser) parseAtom() (Node, error) {
	switch p.current.kind {
	case tokLParen:
		if err := p.advance(); err != nil {
			return nil, err
		}
		inner, err := p.parsePredicate()
		if err != nil {
			return nil, err
		}
		if _, err := p.expect(tokRParen, "parenthesised predicate"); err != nil {
			return nil, err
		}
		return inner, nil
	case tokThreshold:
		return p.parseThresholdCall()
	}
	lhs, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	if isComparisonOp(p.current.kind) {
		return p.parseComparisonTail(lhs)
	}
	// Standalone operand: accepted ONLY as a bool literal
	// per the documented grammar. A standalone field
	// reference (`degraded`, `metric_kind`, etc.) is NOT
	// a complete atom and falls through to the error
	// below. We type-assert on the concrete BoolLitNode
	// rather than on `operandType(lhs) == "bool"` to
	// avoid accidentally accepting `degraded` standalone.
	if bl, ok := lhs.(BoolLitNode); ok {
		return bl, nil
	}
	return nil, newError(ErrParse, p.current.pos,
		"expected a comparison operator after %s operand but got %s",
		operandType(lhs), p.current.kind)
}

// parseThresholdCall consumes `threshold ( '<uuid>' )`. The
// UUID string is captured verbatim; [Bind] parses it as a
// uuid.UUID and looks it up in the [ThresholdResolver].
func (p *parser) parseThresholdCall() (Node, error) {
	callPos := p.current.pos
	if err := p.advance(); err != nil { // consume 'threshold'
		return nil, err
	}
	if _, err := p.expect(tokLParen, "threshold() call"); err != nil {
		return nil, err
	}
	if p.current.kind != tokString {
		return nil, newError(ErrParse, p.current.pos,
			"threshold() takes a single string-literal threshold_id argument; got %s", p.current.kind)
	}
	idTok := p.current
	if err := p.advance(); err != nil {
		return nil, err
	}
	if _, err := p.expect(tokRParen, "threshold() call"); err != nil {
		return nil, err
	}
	return ThresholdNode{
		nodeBase: nodeBase{pos: callPos},
		IDText:   idTok.text,
		IDPos:    idTok.pos,
	}, nil
}

// parseComparisonTail consumes `cmp_op operand` after an
// already-parsed left operand. The LHS is provided so the
// caller (parseAtom) can decide whether a leading operand
// is a standalone bool_literal atom or the start of a
// comparison.
//
// Closed-set canon-guards run here too: if the comparison
// involves `metric_kind` / `scope_kind` / `pack` / `source`
// and the other operand is a string literal, the literal
// MUST be in the canonical set. This is the
// `dsl-rejects-unknown-metric-kind` test scenario in the
// Stage 5.4 implementation plan.
func (p *parser) parseComparisonTail(lhs Node) (Node, error) {
	op := p.current.kind
	opPos := p.current.pos
	if err := p.advance(); err != nil {
		return nil, err
	}
	rhs, err := p.parseOperand()
	if err != nil {
		return nil, err
	}
	t, err := unifyOperandTypes(lhs, rhs, op, opPos)
	if err != nil {
		return nil, err
	}
	if err := canonGuard(lhs, rhs); err != nil {
		return nil, err
	}
	if err := canonGuard(rhs, lhs); err != nil {
		return nil, err
	}
	return CompareNode{
		nodeBase: nodeBase{pos: lhs.Pos()},
		Op:       op,
		LHS:      lhs,
		RHS:      rhs,
		Type:     t,
	}, nil
}

func (p *parser) parseOperand() (Node, error) {
	switch p.current.kind {
	case tokIdent:
		name := p.current.text
		ft, ok := fieldType(name)
		if !ok {
			return nil, newError(ErrSemantic, p.current.pos,
				"unknown field %q (allowed: metric_kind, scope_kind, value, pack, source, degraded)", name)
		}
		pos := p.current.pos
		if err := p.advance(); err != nil {
			return nil, err
		}
		return FieldNode{nodeBase: nodeBase{pos: pos}, Field: name, Type: ft}, nil
	case tokString:
		s := p.current.text
		pos := p.current.pos
		if err := p.advance(); err != nil {
			return nil, err
		}
		return StringLitNode{nodeBase: nodeBase{pos: pos}, Value: s}, nil
	case tokNumber:
		raw := p.current.text
		pos := p.current.pos
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, newError(ErrParse, pos,
				"invalid number literal %q: %v", raw, err)
		}
		if err := p.advance(); err != nil {
			return nil, err
		}
		return NumberLitNode{nodeBase: nodeBase{pos: pos}, Value: v}, nil
	case tokTrue:
		pos := p.current.pos
		if err := p.advance(); err != nil {
			return nil, err
		}
		return BoolLitNode{nodeBase: nodeBase{pos: pos}, Value: true}, nil
	case tokFalse:
		pos := p.current.pos
		if err := p.advance(); err != nil {
			return nil, err
		}
		return BoolLitNode{nodeBase: nodeBase{pos: pos}, Value: false}, nil
	}
	return nil, newError(ErrParse, p.current.pos,
		"expected operand (field / string / number / bool) but got %s", p.current.kind)
}

// isComparisonOp reports whether k is one of the six
// comparison operators the grammar permits.
func isComparisonOp(k tokenKind) bool {
	switch k {
	case tokEQ, tokNE, tokGT, tokGE, tokLT, tokLE:
		return true
	default:
		return false
	}
}

// operandType returns the static type of an operand node.
func operandType(n Node) string {
	switch v := n.(type) {
	case FieldNode:
		return v.Type
	case StringLitNode:
		return "string"
	case NumberLitNode:
		return "number"
	case BoolLitNode:
		return "bool"
	}
	return ""
}

// unifyOperandTypes returns the unified type of the two
// operands or an [ErrType] error.  Ordering operators
// (`<`, `<=`, `>`, `>=`) require numeric operands;
// equality (`==`, `!=`) is legal on any matched type.
func unifyOperandTypes(lhs, rhs Node, op tokenKind, opPos Position) (string, error) {
	lt := operandType(lhs)
	rt := operandType(rhs)
	if lt == "" || rt == "" {
		return "", newError(ErrType, opPos,
			"could not determine operand type for comparison")
	}
	if lt != rt {
		return "", newError(ErrType, opPos,
			"type mismatch in comparison: %s %s %s", lt, op, rt)
	}
	if op != tokEQ && op != tokNE && lt != "number" {
		return "", newError(ErrType, opPos,
			"ordering operator %s requires numeric operands; got %s", op, lt)
	}
	return lt, nil
}

// canonGuard verifies that if `field` is one of the closed-
// set string fields and `lit` is a string literal, the
// literal value is in the canonical set. This is the
// canon-guard that the `dsl-rejects-unknown-metric-kind`
// test scenario relies on (Stage 5.4 implementation plan).
//
// canonGuard is called twice -- once with (lhs, rhs) and
// once with (rhs, lhs) -- so either side may carry the
// field reference.
func canonGuard(field Node, lit Node) error {
	fn, ok := field.(FieldNode)
	if !ok {
		return nil
	}
	sl, ok := lit.(StringLitNode)
	if !ok {
		return nil
	}
	switch fn.Field {
	case "metric_kind":
		if !IsCanonicalMetricKind(sl.Value) {
			return newError(ErrSemantic, sl.pos,
				"unknown metric_kind %q (architecture Sec 1.4 canonical set: %s)",
				sl.Value, listCanonical(canonicalMetricKinds))
		}
	case "scope_kind":
		if !IsCanonicalScopeKind(sl.Value) {
			return newError(ErrSemantic, sl.pos,
				"unknown scope_kind %q (canonical: %s)",
				sl.Value, listCanonical(canonicalScopeKinds))
		}
	case "pack":
		if !IsCanonicalPack(sl.Value) {
			return newError(ErrSemantic, sl.pos,
				"unknown pack %q (canonical: %s)",
				sl.Value, listCanonical(canonicalPacks))
		}
	case "source":
		if !IsCanonicalSource(sl.Value) {
			return newError(ErrSemantic, sl.pos,
				"unknown source %q (canonical: %s)",
				sl.Value, listCanonical(canonicalSources))
		}
	}
	return nil
}

// listCanonical renders a closed set as a sorted
// comma-separated list, suitable for inclusion in error
// messages. Sorting keeps test diffs deterministic.
func listCanonical(set map[string]struct{}) string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	// Tiny in-place selection sort; closed sets here are
	// small (<= 30 entries) so the algorithmic choice
	// doesn't matter -- determinism does.
	for i := 0; i < len(out); i++ {
		mn := i
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[mn] {
				mn = j
			}
		}
		out[i], out[mn] = out[mn], out[i]
	}
	return fmt.Sprintf("%v", out)
}
