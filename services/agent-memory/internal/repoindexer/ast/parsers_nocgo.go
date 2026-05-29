//go:build !cgo

package ast

// defaultParsers returns the parser set for non-CGO builds.
// Tree-sitter parsers are unavailable without CGO, but the PowerShell
// subprocess parser works in either mode.
func defaultParsers() []Parser {
	return []Parser{
		NewPowerShellParser(),
	}
}

func init() {
	for _, p := range defaultParsers() {
		RegisterParser(p)
	}
}
