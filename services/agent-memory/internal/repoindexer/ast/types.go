// Package ast defines the types and interfaces for the AST parser/emitter
// pipeline used by the repo-indexer subsystem.
package ast

// ---------------------------------------------------------------------------
// Graph types (canonical home — not declared elsewhere in the package)
// ---------------------------------------------------------------------------

// Node represents an AST-derived graph node.
type Node struct {
	Kind string
	Name string
}

// Edge represents a directed relationship between two nodes.
type Edge struct {
	Kind   string
	Source string
	Target string
}

// EmitResult summarises the output of an EmitFile call.
type EmitResult struct {
	NodeCount int
	EdgeCount int
}

// Logger receives structured log events from the dispatcher.
type Logger interface {
	Log(event string, fields map[string]string)
}
