//go:build !cgo

// scanDefaultParsers (non-CGO build): tree-sitter parsers are
// unavailable but the pure-Go scanners cover Python, TS/JS, and
// PowerShell still works via subprocess. Languages whose only
// implementation is tree-sitter (C/C++/C#/Go/Rust) skip with
// `no_parser`, surfaced in the scan summary per architecture S7.5.

package main

import "github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"

func scanDefaultParsers() []ast.Parser {
	return []ast.Parser{
		ast.NewPythonParser(),
		ast.NewTypeScriptParser(),
		ast.NewPowerShellParser(),
	}
}
