package ast

func init() {
	RegisterParser(NewTreeSitterGoParser())
}

// defaultParsers returns the tree-sitter-backed parser set available
// when CGO is enabled at build time. In the real codebase this file
// carries //go:build cgo so it is only compiled when CGO_ENABLED=1.
// The E2E stub omits the build tag so tests compile on any toolchain.
func defaultParsers() []Parser {
	return []Parser{
		NewTreeSitterGoParser(),
	}
}