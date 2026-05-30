// Package ast defines the types and interfaces for the AST parser/emitter
// pipeline used by the repo-indexer subsystem.
package ast

import (
	"errors"
)

// ErrParserUnavailable is the sentinel error a parser returns when its
// backing grammar or runtime (e.g. tree-sitter CGO binding) is not
// available in the current build. The dispatcher recognises this via
// errors.Is and logs a skip instead of propagating a hard failure.
var ErrParserUnavailable = errors.New("parser unavailable")

// ---------------------------------------------------------------------------
// Core data types
// ---------------------------------------------------------------------------

// ParseResult is the output of a single-file parse pass.
type ParseResult struct {
	Classes []ClassDecl
	Methods []MethodDecl
	Imports []Import
}

// ClassDecl represents a class, struct, interface, trait, or enum.
type ClassDecl struct {
	Name     string
	LangMeta map[string]string
}

// MethodDecl represents a method or free function.
type MethodDecl struct {
	Name            string
	ClassName       string
	LangMeta        map[string]string
	ReceiverAliases map[string]string // alias key → canonical target
}

// Import represents an import, include, use, or using directive.
type Import struct {
	Path     string
	LangMeta map[string]string
}

// Edge represents a directed relationship between two nodes.
type Edge struct {
	Kind   string
	Source string
	Target string
}

// Node represents an AST-derived graph node.
type Node struct {
	Kind string
	Name string
}

// EmitResult summarises the output of an EmitFile call.
type EmitResult struct {
	NodeCount int
	EdgeCount int
	CallsRaw  []string
}

// ---------------------------------------------------------------------------
// Interfaces
// ---------------------------------------------------------------------------

// Parser is implemented by each language-specific parser.
type Parser interface {
	Parse(filename string, src []byte) (ParseResult, error)
	Language() string
	Extensions() []string
}

// Writer receives nodes and edges produced by the emitter.
type Writer interface {
	InsertNode(n Node) error
	InsertEdge(e Edge) error
}

// Logger receives structured log events from the dispatcher.
type Logger interface {
	Log(event string, fields map[string]string)
}
