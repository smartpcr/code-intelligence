// Package ast provides language-aware AST parsers for the repo-indexer
// subsystem. Each parser implements the LanguageParser interface and
// emits ParseResult values that the dispatcher routes to the graph
// writer.
package ast

import "errors"

// ErrParserUnavailable is the sentinel error a parser returns when its
// backing grammar or runtime is not available in the current build.
var ErrParserUnavailable = errors.New("parser unavailable")

// LanguageParser is implemented by each language-specific parser.
type LanguageParser interface {
	Language() string
	Extensions() []string
	Parse(relPath string, src []byte) (ParseResult, error)
}

// ParseResult holds the output of parsing a single source file.
type ParseResult struct {
	Classes []ClassDecl
	Methods []MethodDecl
	Imports []Import
}

// ClassDecl represents a class, struct, interface, trait, or enum.
type ClassDecl struct {
	QualifiedName string
	Kind          string
	Implements    []string
	StartLine     int
	EndLine       int
	LangMeta      map[string]any
}

// MethodDecl describes one method or free-function declaration.
type MethodDecl struct {
	QualifiedName   string
	EnclosingClass  string
	ParamSignature  string
	BodySource      string
	StartLine       int
	EndLine         int
	BodyStartLine   int
	BodyEndLine     int
	BodyStartByte   int
	BodyEndByte     int
	Calls           []string
	ReceiverCalls   []string
	MemberAccesses  []MemberAccess
	Modifiers       []string
	ReceiverAliases []string
	LangMeta        map[string]any
}

// MemberAccess records a receiver-qualified field access.
type MemberAccess struct {
	Name    string
	IsWrite bool
}

// Import represents an import, include, use, or using directive.
type Import struct {
	Path     string
	LangMeta map[string]any
}
