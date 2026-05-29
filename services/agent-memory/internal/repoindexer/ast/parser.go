package ast

import (
	"errors"
	"path/filepath"
	"sync"
)

// ErrParserUnavailable is the sentinel error a parser returns when its
// backing grammar or runtime (e.g. tree-sitter CGO binding) is not
// available in the current build.
var ErrParserUnavailable = errors.New("parser unavailable")

// Parser is implemented by each language-specific parser.
type Parser interface {
	Language() string
	Extensions() []string
	Parse(filename string, src []byte) (ParseResult, error)
}

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
	Name      string
	ClassName string
	LangMeta  map[string]string
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

// Writer receives nodes and edges produced by the emitter.
type Writer interface {
	InsertNode(n Node) error
	InsertEdge(e Edge) error
}

// Logger receives structured log events from the dispatcher.
type Logger interface {
	Log(event string, fields map[string]string)
}

// ---------------------------------------------------------------------------
// Global registry — used by parsers_cgo.go init()
// ---------------------------------------------------------------------------

var (
	mu     sync.RWMutex
	extMap = map[string]Parser{}
)

// RegisterParser adds a parser for each of its declared extensions.
func RegisterParser(p Parser) {
	mu.Lock()
	defer mu.Unlock()
	for _, ext := range p.Extensions() {
		extMap[ext] = p
	}
}

// SelectParser returns the registered parser for the given filename's
// extension, or nil if no parser is registered for that extension.
func SelectParser(filename string, src []byte) Parser {
	ext := filepath.Ext(filename)
	mu.RLock()
	defer mu.RUnlock()
	return extMap[ext]
}

// DefaultParsers returns the build-appropriate parser list.
// Under CGO=on this includes all tree-sitter parsers (including Rust).
// Under CGO=off (parsers_nocgo.go) this returns nil.
func DefaultParsers() []Parser {
	return defaultParsers()
}