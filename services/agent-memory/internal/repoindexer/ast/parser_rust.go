package ast

// TreeSitterRustParser parses Rust source files using tree-sitter.
type TreeSitterRustParser struct{}

// NewTreeSitterRustParser creates a new Rust parser.
func NewTreeSitterRustParser() *TreeSitterRustParser {
	return &TreeSitterRustParser{}
}

func (p *TreeSitterRustParser) Language() string     { return "rust" }
func (p *TreeSitterRustParser) Extensions() []string { return []string{".rs"} }

func (p *TreeSitterRustParser) Parse(filename string, src []byte) (ParseResult, error) {
	return ParseResult{}, nil
}
