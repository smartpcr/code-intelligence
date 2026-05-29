//go:build !cgo

package ast

// defaultParsers returns the scanner-backed parser set the
// dispatcher uses when CGO is disabled at build time. CGO is
// the default OFF state for the `make test` portable path on
// stock Windows toolchains and for `go test` invocations that
// do not set `CGO_ENABLED=1`. The lightweight stdlib-only
// scanners (parser_typescript.go, parser_python.go) provide
// the same `LanguageParser` contract as the tree-sitter
// implementations -- the dispatcher and downstream consumers
// see no difference at runtime.
//
// The CGO-on counterpart lives in parsers_cgo.go and is
// selected when the binary is built with `CGO_ENABLED=1`; see
// doc.go "Parser implementations" for the full story on which
// path is canonical when.
//
// Additional languages registered ONLY on the CGO-on path:
//
//   - C# (`.cs`, `.csx`) -- the C# parser
//     (parser_treesitter_csharp.go) rides the tree-sitter-c-sharp
//     grammar via CGO and has no scanner-backed fallback in v1.
//     A `.cs` file ingested on the CGO=off toolchain falls
//     through the dispatcher's "no parser registered for
//     extension" branch, which surfaces a structured log and
//     a non-fatal skip; the file simply does not contribute
//     nodes / edges to the graph for that build. Adding a
//     scanner-backed C# parser is tracked as a follow-up.
func defaultParsers() []LanguageParser {
	return []LanguageParser{
		NewTypeScriptParser(),
		NewPythonParser(),
	}
}
