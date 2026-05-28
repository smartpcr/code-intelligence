package ast

// LanguageParser is the interface every language parser must implement.
// The dispatcher registers parsers by the extensions they declare and
// routes incoming files to the matching parser.
type LanguageParser interface {
	Language() string
	Extensions() []string
	ParseFile(path string, src []byte) (*ParseResult, error)
}

// ParseResult holds the output of parsing a single source file.
type ParseResult struct {
	Classes []ClassDecl
	Methods []MethodDecl
	Imports []Import
}

// MethodDecl represents a parsed method or function declaration.
type MethodDecl struct {
	QualifiedName  string
	EnclosingClass string
	ParamSignature string
	StartLine      int
	EndLine        int
	Calls          []string
	ReceiverCalls  []string
	Modifiers      []string
	LangMeta       map[string]any
}

// ClassDecl represents a parsed class, struct, interface, or similar
// type-level declaration.
type ClassDecl struct {
	QualifiedName string
	Kind          string // "class", "struct", "interface", "enum", "trait"
	StartLine     int
	EndLine       int
	LangMeta      map[string]any
}

// Import represents an import, include, use, or using statement.
type Import struct {
	Path  string
	Alias string
}
