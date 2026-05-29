package parser

import (
	astv1 "forge/services/clean-code/internal/ast/v1"
)

// AstFile is the canonical AST file message, re-exported from the
// generated `astv1` package so callers under `internal/ast/parser`
// can spell `parser.AstFile` without a second proto import. The
// alias preserves wire-format identity with the proto package.
type AstFile = astv1.AstFile

// AstScope is the canonical scope message (file / package / class /
// interface / method / block) re-exported from `astv1`.
type AstScope = astv1.AstScope

// AstSymbol is the canonical named-symbol message re-exported from
// `astv1`. Symbols are tied to a parent scope by `parent_scope_id`.
type AstSymbol = astv1.AstSymbol

// AstEdge is the canonical edge message (call / extends / implements /
// imports / contains) re-exported from `astv1`.
type AstEdge = astv1.AstEdge

// AstRange is the 1-based inclusive source range re-exported from `astv1`.
type AstRange = astv1.AstRange

// AstRef is the typed scope-or-symbol reference re-exported from `astv1`.
type AstRef = astv1.AstRef

// ScopeKind is the canonical scope-kind enum re-exported from `astv1`;
// see architecture Sec 5.2.3 for the pinned ordinal layout.
type ScopeKind = astv1.ScopeKind

// AstRefKind is the canonical reference-kind enum re-exported from `astv1`.
type AstRefKind = astv1.AstRefKind

// Re-exported enum constants for `ScopeKind`. Listing them
// explicitly (rather than `type ScopeKind = astv1.ScopeKind`)
// keeps a `gopls`-driven import auto-completion landing on the
// short `parser.ScopeKind_*` name; the underlying values are
// the generated proto enum so wire-format identity is
// preserved.
const (
	ScopeKindUnspecified = astv1.ScopeKind_SCOPE_KIND_UNSPECIFIED
	ScopeKindRepo        = astv1.ScopeKind_SCOPE_KIND_REPO
	ScopeKindPackage     = astv1.ScopeKind_SCOPE_KIND_PACKAGE
	ScopeKindFile        = astv1.ScopeKind_SCOPE_KIND_FILE
	ScopeKindClass       = astv1.ScopeKind_SCOPE_KIND_CLASS
	ScopeKindInterface   = astv1.ScopeKind_SCOPE_KIND_INTERFACE
	ScopeKindMethod      = astv1.ScopeKind_SCOPE_KIND_METHOD
	ScopeKindBlock       = astv1.ScopeKind_SCOPE_KIND_BLOCK
)

// Re-exported enum constants for `AstRefKind`.
const (
	RefKindUnspecified = astv1.AstRefKind_AST_REF_KIND_UNSPECIFIED
	RefKindScope       = astv1.AstRefKind_AST_REF_KIND_SCOPE
	RefKindSymbol      = astv1.AstRefKind_AST_REF_KIND_SYMBOL
)
