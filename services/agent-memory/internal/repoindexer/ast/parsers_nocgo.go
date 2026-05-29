//go:build !cgo

package ast

// defaultParsers returns the parser set for non-CGO builds.
// Without CGO, tree-sitter-backed parsers (including Rust) are
// unavailable; scanner-based fallbacks for TypeScript and Python
// are returned instead.
func defaultParsers() []LanguageParser {
	return []LanguageParser{
		NewTypeScriptParser(),
		NewPythonParser(),
	}
}

func init() {
	for _, p := range defaultParsers() {
		RegisterParser(p)
	}
}
