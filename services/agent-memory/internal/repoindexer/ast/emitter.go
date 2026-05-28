package ast

// The Stage 3.2 dispatcher (see dispatcher.go) owns the full
// emit pipeline directly against graphwriter.Writer. An
// earlier story stage briefly introduced a stripped-down
// Emitter helper plus stub NewGoParser / NewTypeScriptParser
// / NewPythonParser constructors in this file. The stubs were
// duplicates of the real parser constructors (NewPythonParser
// in parser_python.go, NewTypeScriptParser in
// parser_typescript.go, and the tree-sitter variants registered
// via parsers_cgo.go) and conflicted with the production
// dispatcher's writer / emit contract, so they were removed
// here to restore a compilable package. New shared emit helpers
// belong inside the dispatcher in dispatcher.go.
