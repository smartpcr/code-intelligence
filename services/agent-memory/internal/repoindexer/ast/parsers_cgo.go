//go:build cgo

package ast

// defaultParsers returns the tree-sitter-backed parser set used when
// CGO is enabled at build time. Under a CGO=off build,
// parsers_nocgo.go provides an alternative returning nil.
func defaultParsers() []Parser {
	return []Parser{
		NewTreeSitterRustParser(),
	}
}

func init() {
	for _, p := range defaultParsers() {
		RegisterParser(p)
	}
}