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
// Language coverage note: Rust (.rs) is intentionally
// CGO-only. The v1 Rust parser
// (parser_treesitter_rust.go) rides the tree-sitter Rust
// grammar; no stdlib-only scanner fallback exists because
// Rust's surface (lifetimes, generics, macro invocations,
// pattern-matching) is too far from what a regex-line
// scanner can reliably handle. Files with `.rs` extensions
// in a CGO=0 binary therefore fall through the dispatcher
// without producing any class / method nodes -- callers that
// need Rust ingestion MUST build with `CGO_ENABLED=1` (the
// production default).
func defaultParsers() []LanguageParser {
	return []LanguageParser{
		NewTypeScriptParser(),
		NewPythonParser(),
	}
}
