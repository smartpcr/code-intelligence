//go:build cgo

package ast

// defaultParsers returns the parser set for CGO-enabled builds.
// Includes tree-sitter-backed parsers plus the PowerShell subprocess parser.
//
// Order is significant only when two parsers share an extension: the
// LAST entry wins via the dispatcher's `extMap` overwrite during
// `NewDispatcher`. Today the C and C++ parsers have NO overlapping
// extensions -- `.h` is claimed solely by cTreeSitterParser
// (see parser_treesitter_c.go), while cppTreeSitterParser claims only
// the unambiguous C++ headers (`.hpp`, `.hh`, `.hxx`, `.h++`) and
// sources (`.cc`, `.cpp`, `.cxx`, `.c++`). The C-before-C++ ordering
// is therefore defensive: TestDefaultParsers_CBeforeCpp pins it so
// that if a future edit accidentally adds `.h` to the C++ parser's
// Extensions(), `.h` files continue to route to C rather than being
// silently overwritten by the later C++ entry. For all currently
// non-overlapping extensions the slice order is irrelevant.
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
