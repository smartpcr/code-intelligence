//go:build cgo

// scanDefaultParsers (CGO build) returns the dispatcher parser
// list used by `codeintel scan`. It mirrors `ast.defaultParsers()`
// (which is unexported and CGO=on registers C/C++/C#/Go/Rust/PS)
// and augments the set with the pure-Go scanner parsers for
// Python and TypeScript/JavaScript PLUS their tree-sitter variants
// so the CLI covers the v1 language goal called out in story §1
// (per evaluator iter-1 feedback item 5).
//
// Order matters: when multiple parsers claim an extension the
// LAST one wins (per dispatcher.NewDispatcher extMap overwrite).
// The tree-sitter Python/TS parsers are listed AFTER the scanner
// variants so the higher-fidelity grammar wins when CGO is on.

package main

import "github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"

func scanDefaultParsers() []ast.Parser {
	return []ast.Parser{
		// Scanner fallbacks first.
		ast.NewPythonParser(),
		ast.NewTypeScriptParser(),
		// CGO defaults (C must come before C++ -- see
		// parsers_cgo.go for the rationale).
		ast.NewTreeSitterCParser(),
		ast.NewTreeSitterCppParser(),
		ast.NewTreeSitterCSharpParser(),
		ast.NewTreeSitterGoParser(),
		ast.NewTreeSitterRustParser(),
		// Tree-sitter Python / TS win over the scanner
		// variants when CGO is on.
		ast.NewTreeSitterPythonParser(),
		ast.NewTreeSitterTypeScriptParser(),
		ast.NewPowerShellParser(),
	}
}
