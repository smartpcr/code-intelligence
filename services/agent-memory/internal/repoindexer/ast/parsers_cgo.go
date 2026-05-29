//go:build cgo

package ast

// defaultParsers returns the parser set for CGO-enabled builds.
// Includes tree-sitter-backed parsers plus the PowerShell subprocess parser.
func defaultParsers() []Parser {
	return []Parser{
		NewTreeSitterRustParser(),
		NewPowerShellParser(),
	}
}

func init() {
	for _, p := range defaultParsers() {
		RegisterParser(p)
	}
}
