//go:build cgo

package ast

// defaultParsers returns the tree-sitter-backed parser set
// the dispatcher uses when CGO is enabled at build time. The
// implementation plan calls out tree-sitter as the canonical
// parser core (implementation-plan.md §3.2 lines 425-427); we
// take that path whenever the toolchain supports it. The
// scanner-backed parsers remain available as the CGO=0 path
// (parsers_nocgo.go) so the agent-memory test suite can run
// on the portable Windows toolchain that ships with the
// repository's `make test` target.
//
// The tree-sitter parsers expose the same `LanguageParser`
// contract as the scanners; the dispatcher's two-pass insert
// protocol is independent of which side returned the
// `ParseResult`.
func defaultParsers() []LanguageParser {
	return []LanguageParser{
		NewTreeSitterTypeScriptParser(),
		NewTreeSitterPythonParser(),
		NewTreeSitterCSharpParser(),
	}
}
