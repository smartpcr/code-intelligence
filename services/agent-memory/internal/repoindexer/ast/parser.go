package ast

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

// ErrParserUnavailable is the sentinel error a parser returns when its
// backing runtime (e.g. tree-sitter CGO binding, pwsh subprocess) is
// not available in the current build or environment.
var ErrParserUnavailable = errors.New("parser unavailable")

// UnavailableError wraps ErrParserUnavailable with a parser-specific reason.
type UnavailableError struct {
	Reason string
}

func (e *UnavailableError) Error() string         { return e.Reason + ": parser unavailable" }
func (e *UnavailableError) Is(target error) bool   { return target == ErrParserUnavailable }

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
	// Module is the imported module specifier (e.g.
	// `"./utils"`, `"os"`, `"@scope/pkg"`).
	Module string
	// Path is a legacy alias for Module retained for canonical
	// dispatcher tests and PowerShell parser test fixtures that
	// were authored when Import exposed both fields. Parsers
	// SHOULD populate Module; Path may be left empty.
	Path string
	// Symbols lists the named symbols imported from Module.
	// Empty for whole-module imports (`import os`,
	// `import "./utils"`).
	Symbols []string
	// Alias is the local alias for whole-module imports
	// (`import os as o` -> "o"; `import * as fs from "fs"`
	// -> "fs"). Empty when no alias was specified.
	Alias string
	// Line is the 1-based source line of the import
	// statement.
	Line int
	// IsTypeOnly is true for TS `import type` statements
	// (and the per-symbol `import { type Foo } from ...`
	// equivalent). Type-only imports do not produce a
	// runtime dependency, so consumers may want to weight
	// them differently in relevance scoring; the dispatcher
	// records the flag on the `imports` edge attrs but does
	// NOT skip the edge. Always false for Python.
	IsTypeOnly bool
	// LangMeta carries per-language attrs the dispatcher
	// folds into the `imports` edge `attrs_json` via
	// `mergeLangMeta` (architecture Section 4.4.2). A nil map
	// means "no per-language attrs" -- the merge is a no-op
	// and the existing TS/JS/Python parsers leave it nil, so
	// dispatcher output for those languages is byte-identical
	// across this surface.
	//
	// LangMeta is DESCRIPTIVE, NOT IDENTIFYING (architecture
	// invariant C12): two `Import`s that differ ONLY in
	// `LangMeta` values describe the same dependency edge and
	// are NOT routed into the import's identity (Module +
	// Symbols + Alias). Parsers MUST NOT push language-
	// specific data into those identifying fields.
	//
	// Parsers MUST NOT set keys whose names collide with the
	// dispatcher's first-class import-edge attrs keys (e.g.
	// `module`, `line`, `symbols`, `alias`, `is_type_only`,
	// `language`) -- the merge helper's first-class-key-wins
	// rule silently drops them. Well-known per-language keys
	// for imports (`dot_import`, `blank_import`, `is_static`,
	// `cmdlet_verb`, `module_kind`) are catalogued in
	// architecture Section 4.4.3.
	LangMeta map[string]any
}

// ---------------------------------------------------------------------------
// Global parser registry (additive E2E surface)
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

// Writer receives nodes and edges produced by the emitter. The
// canonical declarations of `Node`, `Edge`, `EmitResult`, and
// `Logger` live in `dispatcher.go` (those types form the
// dispatcher's core surface). `Writer` is kept here as an
// alias-shaped interface used by e2e tests that prefer the
// shorter name; any type satisfying `Writer` also satisfies the
// dispatcher's `NodeEdgeWriter` and vice-versa.
type Writer interface {
	InsertNode(n Node) error
	InsertEdge(e Edge) error
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
func DefaultParsers() []Parser {
	return defaultParsers()
}
