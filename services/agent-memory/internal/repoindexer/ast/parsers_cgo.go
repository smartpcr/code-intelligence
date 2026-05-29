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
// Registration order is the documented order from the
// AST-PARSER-FOR-ADDIT architecture (architecture.md Section
// 3 / tech-spec.md Section 4):
//
//	tsTreeSitterParser, pyTreeSitterParser,         (existing)
//	cTreeSitterParser, cppTreeSitterParser,         (this stage)
//	csharpTreeSitterParser,                         (Stage 3.5)
//	goTreeSitterParser, rustTreeSitterParser,       (later)
//	powershellParser                                (later)
//
// Order matters for two reasons:
//
//  1. `buildExtMap` (dispatcher.go) iterates this slice and
//     LATER entries overwrite EARLIER ones when two parsers
//     claim the same extension. The C parser claims `.c` and
//     `.h`; the C++ parser claims `.cc`, `.cpp`, `.cxx`,
//     `.c++`, `.hpp`, `.hh`, `.hxx`, `.h++` -- so today they
//     do not overlap, but pinning C before C++ keeps `.h ->
//     c` deterministic if a future C++ parser revision adds
//     `.h` to its set (the operator-pinned dot-h-extension-
//     routing rule says C wins regardless).
//
//  2. The `TestDispatcher_DotHRoutesToC_EvenWithCppHint`
//     test (added in the cross-language dispatcher-tests
//     stage, implementation-plan.md line 444) needs the C
//     parser registered first; documenting the order here
//     keeps that test deterministic across re-ordering
//     edits.
func defaultParsers() []LanguageParser {
	return []LanguageParser{
		NewTreeSitterTypeScriptParser(),
		NewTreeSitterPythonParser(),
		NewTreeSitterCParser(),
		NewTreeSitterCppParser(),
		NewTreeSitterCSharpParser(),
	}
}
