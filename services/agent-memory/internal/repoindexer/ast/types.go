package ast

// The authoritative production parser types (LanguageParser,
// ParseResult, ClassDecl, MethodDecl, Import) live in parser.go.
//
// An earlier story stage briefly introduced a stripped-down
// duplicate of these types in this file. The duplicates were
// strict subsets of the parser.go definitions and conflicted
// with the production parsers (which rely on the richer fields
// such as Implements, BodySource, MemberAccesses, BodyStartLine,
// etc.), so they were removed here to restore a compilable
// package. New shared types belong in parser.go alongside the
// LanguageParser interface they support.
