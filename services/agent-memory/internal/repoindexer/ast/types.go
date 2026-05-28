//go:build canonical_dispatcher

// Package ast — stub type declarations are gated behind the
// `canonical_dispatcher` build tag (never enabled in the
// current code base). The canonical declarations of
// ParseResult / ClassDecl / MethodDecl / Import /
// ErrParserUnavailable / LanguageParser live in parser.go.
// This file is kept on disk (rather than deleted) to preserve
// the half-rolled-back Stage 3.1 migration's history; once the
// Stage 3.2 dispatcher landing workstream lands, the canonical
// Edge / Node / EmitResult decls will move into a fresh file
// and this stub can be retired.
//
// The csharpTreeSitterParser workstream (iter 5) introduced
// this build-tag gate as a structural fix after four iterations
// of deferral failed to resolve the V1/V2 duplicate-symbol
// collision that blocked CGO=0 validation; the same pattern was
// independently adopted by the goTreeSitterParser sibling
// workstream (commit bd30500 on its branch).
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
