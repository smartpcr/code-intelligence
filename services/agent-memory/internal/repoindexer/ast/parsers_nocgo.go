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
func defaultParsers() []LanguageParser {
	return []LanguageParser{
		NewTypeScriptParser(),
		NewPythonParser(),
	}
}
