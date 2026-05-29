//go:build cgo

package ast

// defaultParsers returns the parser set for CGO-enabled builds.
// Includes tree-sitter-backed parsers plus the PowerShell subprocess parser.
//
// Order is significant: when two parsers share an extension, the
// LAST one wins via the dispatcher's `extMap` overwrite during
// `NewDispatcher`. The C parser MUST be listed BEFORE the C++ parser
// so that `.h` continues to be routed to C++ when the C++ parser
// claims it (per TestDefaultParsers_CBeforeCpp). For non-overlapping
// extensions the order is irrelevant.
func defaultParsers() []Parser {
	return []Parser{
		NewTreeSitterCParser(),
		NewTreeSitterCppParser(),
		NewTreeSitterCSharpParser(),
		NewTreeSitterGoParser(),
		NewTreeSitterRustParser(),
		NewPowerShellParser(),
	}
}

func init() {
	for _, p := range defaultParsers() {
		RegisterParser(p)
	}
}
