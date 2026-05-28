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
//
// Workstream scope (story code-intelligence:AST-PARSER-FOR-ADDIT).
// Tree-sitter parsers for the remaining target languages are
// landed by sibling stage workstreams and registered here in
// later commits, not by the Go-parser stage. The active
// sibling worktrees on this story (one stage per language /
// language-group) are:
//
//   - stage-3.1-ctreesitterparser-implementation
//     (branch: phase-c-and-cpp-parsers-stage-ctreesitterparser-implementation)
//     owns parser_treesitter_c.go + parser_treesitter_cpp.go;
//     will register NewTreeSitterCParser() / NewTreeSitterCppParser().
//   - stage-4.1-csharptreesitterparser-implementation owns
//     parser_treesitter_csharp.go; will register
//     NewTreeSitterCSharpParser().
//   - stage-5.1-rusttreesitterparser-implementation owns
//     parser_treesitter_rust.go; will register
//     NewTreeSitterRustParser().
//   - stage-6.1-powershellparser-subprocess-implementation
//     owns the PowerShell scanner/subprocess parser.
//
// This file therefore intentionally registers only the three
// languages whose tree-sitter parsers have already landed on
// `feature/memory` (TypeScript, Python, Go); the others will
// be added as their respective stage workstreams merge.
func defaultParsers() []LanguageParser {
	return []LanguageParser{
		NewTreeSitterTypeScriptParser(),
		NewTreeSitterPythonParser(),
		NewTreeSitterGoParser(),
	}
}
