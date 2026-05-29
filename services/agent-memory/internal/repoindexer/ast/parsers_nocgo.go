//go:build !cgo

package ast

// defaultParsers returns the parser set for non-CGO builds.
// Without CGO, tree-sitter-backed parsers (including Rust) are
// unavailable; the function returns nil.
func defaultParsers() []Parser {
	return nil
}
