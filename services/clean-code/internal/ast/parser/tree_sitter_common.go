//go:build cgo

package parser

import (
	sitter "github.com/smacker/go-tree-sitter"
)

// nodeText returns the source byte slice covered by `n` as a
// string.
func nodeText(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	start := n.StartByte()
	end := n.EndByte()
	if int(end) > len(src) {
		end = uint32(len(src))
	}
	if start > end {
		start = end
	}
	return string(src[start:end])
}

// nodeRange returns the canonical AstRange for `n`. Tree-sitter
// uses 0-based rows / columns; our proto pins lines+columns at
// 1-based (matches LSP, IDE editors, and architecture Sec 5.2.3
// example records).
func nodeRange(n *sitter.Node) *AstRange {
	if n == nil {
		return &AstRange{}
	}
	sp := n.StartPoint()
	ep := n.EndPoint()
	return &AstRange{
		StartByte: n.StartByte(),
		EndByte:   n.EndByte(),
		StartLine: sp.Row + 1,
		EndLine:   ep.Row + 1,
		StartCol:  sp.Column + 1,
		EndCol:    ep.Column + 1,
	}
}

// findChildByFieldName returns the first child of `n` registered
// under `fieldName` in the tree-sitter grammar, or nil if none
// is present. The wrapper exists so per-language adapters can
// stay independent of the bindings package surface.
func findChildByFieldName(n *sitter.Node, fieldName string) *sitter.Node {
	if n == nil {
		return nil
	}
	return n.ChildByFieldName(fieldName)
}

// stripQuotes peels a single layer of double-quote, single-quote
// or backtick delimiters from the input, used to extract the
// module path from a TypeScript / Java string literal.
func stripQuotes(s string) string {
	if len(s) < 2 {
		return s
	}
	first := s[0]
	last := s[len(s)-1]
	if first == last && (first == '"' || first == '\'' || first == '`') {
		return s[1 : len(s)-1]
	}
	return s
}
