//go:build !cgo

// Package ast — non-CGO fallback stub for the Rust parser symbol.
//
// Why this file is `//go:build !cgo`:
// The real Rust parser lives in parser_treesitter_rust.go behind
// `//go:build cgo`. Without this tag both files would define
// `NewTreeSitterRustParser` simultaneously in a CGO-enabled build,
// producing a duplicate-declaration compile error that prevents
// `TestRustFixture_EmitsExpectedNodeAndEdgeSet` from ever running
// on a host with a C toolchain (iter-6 evaluator finding #2).
//
// Under `CGO_ENABLED=0` this stub provides a no-op implementation
// so the type and factory remain referenceable for tooling that
// imports the package symbolically; the production parsers_nocgo.go
// dispatcher registration intentionally OMITS Rust (rust on the
// non-CGO path is documented as unsupported), so this stub is
// never actually invoked at runtime.
package ast

// TreeSitterRustParser parses Rust source files using tree-sitter.
// Non-CGO stub: returns no nodes / edges. The real implementation
// lives in parser_treesitter_rust.go behind `//go:build cgo`.
type TreeSitterRustParser struct{}

// NewTreeSitterRustParser creates a new Rust parser stub
// (non-CGO build). The real factory in parser_treesitter_rust.go
// returns a `LanguageParser` interface value; this stub returns the
// concrete type because it is only ever used to satisfy compile-time
// symbol references on the non-CGO path.
func NewTreeSitterRustParser() *TreeSitterRustParser {
	return &TreeSitterRustParser{}
}

func (p *TreeSitterRustParser) Language() string     { return "rust" }
func (p *TreeSitterRustParser) Extensions() []string { return []string{".rs"} }

func (p *TreeSitterRustParser) Parse(filename string, src []byte) (ParseResult, error) {
	return ParseResult{}, nil
}
