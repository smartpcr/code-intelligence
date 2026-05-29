package ast

// TreeSitterGoParser parses Go source files using tree-sitter.
type TreeSitterGoParser struct{}

// NewTreeSitterGoParser creates a new Go parser.
func NewTreeSitterGoParser() *TreeSitterGoParser {
	return &TreeSitterGoParser{}
}

func (p *TreeSitterGoParser) Language() string     { return "go" }
func (p *TreeSitterGoParser) Extensions() []string { return []string{".go"} }

func (p *TreeSitterGoParser) Parse(filename string, src []byte) (*ParseResult, error) {
	return &ParseResult{Language: "go"}, nil
}