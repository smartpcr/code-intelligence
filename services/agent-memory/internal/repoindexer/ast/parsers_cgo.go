//go:build cgo

package ast

func init() {
	RegisterParser(NewTreeSitterGoParser())
}

// defaultParsers returns the tree-sitter-backed parser set available
// when CGO is enabled at build time. This file is only compiled when
// CGO_ENABLED=1; the CGO-off fallback lives in parsers_nocgo.go.
func defaultParsers() []Parser {
	return []Parser{
		NewTreeSitterGoParser(),
	}
}