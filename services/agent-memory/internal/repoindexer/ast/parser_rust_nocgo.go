//go:build !cgo

package ast

// rustParserNoCGO is a stub returned when CGO is disabled.
// The real tree-sitter-backed Rust parser requires CGO_ENABLED=1.
type rustParserNoCGO struct{}

// NewRustParser returns a stub parser when CGO is disabled.
// Calling Parse on it returns ErrParserUnavailable.
func NewRustParser() Parser { return &rustParserNoCGO{} }

func (p *rustParserNoCGO) Language() string     { return "rust" }
func (p *rustParserNoCGO) Extensions() []string { return []string{".rs"} }

func (p *rustParserNoCGO) Parse(filename string, src []byte) (ParseResult, error) {
	return ParseResult{}, ErrParserUnavailable
}
