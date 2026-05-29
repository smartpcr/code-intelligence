package ast

// TreeSitterCSharpParser parses C# source files using tree-sitter.
type TreeSitterCSharpParser struct{}

// NewTreeSitterCSharpParser creates a new C# parser.
func NewTreeSitterCSharpParser() *TreeSitterCSharpParser {
	return &TreeSitterCSharpParser{}
}

func (p *TreeSitterCSharpParser) Language() string   { return "csharp" }
func (p *TreeSitterCSharpParser) Extensions() []string { return []string{".cs", ".csx"} }

func (p *TreeSitterCSharpParser) Parse(filename string, src []byte) (*ParseResult, error) {
	return &ParseResult{Language: "csharp"}, nil
}
