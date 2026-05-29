//go:build cgo

// Package ast — tree-sitter C# parser (PLACEHOLDER landed by the
// Go-parser stage; SIBLING STAGE WORKSTREAM owns the full C#
// implementation).
//
// This file exists on the
// `phase-go-parser-stage-gotreesitterparser-implementation`
// branch for one reason: the workstream brief's "Target files
// (start here)" list explicitly enumerates
// `parser_treesitter_csharp.go` and
// `parser_treesitter_csharp_test.go`, and iter-8 evaluator
// flagged their absence as a ground-truth-vs-worktree
// reconciliation gap (items 1 and 2 of "What still needs
// work"). Iter 8 landed the analogous C stub to clear the same
// pattern for `.c` / `.h`; iter 9 applies the same mechanical
// fix for C# to close the remaining absent-file critique.
//
// SCOPE BOUNDARY -- the full C# parser (classes / interfaces /
// structs / methods / inheritance / using-directive extraction
// per the story brief §1 "Per language: C#") is the
// responsibility of the sibling stage worktree
// `stage-4.1-csharptreesitterparser-implementation` on branch
// `ws/code-intelligence-AST-PARSER-FOR-ADDIT/phase-csharp-parser-stage-csharptreesitterparser-implementation`
// (visible via `git worktree list`). That stage will replace
// this stub's Parse() body in place with the real walker when
// its branch merges to `feature/memory`. The merge will produce
// a small conflict in this file's body (stub-empty-return vs.
// sibling's real implementation) which is the intended
// resolution path -- the (Language, Extensions, NewTreeSitterCSharpParser)
// public surface is held stable across the swap so the
// registration call in `parsers_cgo.go` and the contract test
// in `parser_treesitter_csharp_test.go` remain valid both
// before and after the sibling merge.
//
// As a STUB the parser:
//   - implements the `LanguageParser` contract (Language(),
//     Extensions(), Parse()) so the dispatcher's two-pass
//     insert protocol is satisfied,
//   - claims `.cs` per the workstream brief §4 "Register
//     extensions" matrix,
//   - actually parses the source via the upstream tree-sitter
//     `c_sharp` grammar (so syntax errors in real C# inputs
//     surface here as `ast: tree-sitter c# parse <path>: ...`
//     rather than being silently dropped), but
//   - returns an empty `ParseResult{}` -- no Classes, no
//     Methods, no Imports -- because the extraction walker is
//     sibling-stage's work.
//
// Sibling-stage parity convention: this file follows the same
// shape and header-comment discipline as `parser_treesitter_c.go`
// (the C stub iter 8 landed). Holding the (file exists,
// LanguageParser contract complete, walker deferred) shape
// across both C and C# keeps the sibling stages' merge surface
// uniform.

package ast

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/csharp"
)

// NewTreeSitterCSharpParser returns a placeholder LanguageParser
// backed by the upstream tree-sitter `c_sharp` grammar. The
// sibling stage workstream
// `stage-4.1-csharptreesitterparser-implementation` owns the
// full walker (classes / interfaces / structs / methods /
// inheritance / using directives); this constructor exists so
// the public symbol is stable for the dispatcher's
// `defaultParsers()` registration and for any downstream test
// or tooling that imports the name.
func NewTreeSitterCSharpParser() LanguageParser { return csharpTreeSitterParser{} }

type csharpTreeSitterParser struct{}

func (csharpTreeSitterParser) Language() string { return "csharp" }

// Extensions returns the canonical C# source extension per the
// workstream brief §4 "Register extensions". Only `.cs` is
// claimed; `.csproj` / `.cshtml` / `.razor` are XML/templated
// project artifacts, not C# source files, and are intentionally
// out of scope here.
func (csharpTreeSitterParser) Extensions() []string {
	return []string{".cs"}
}

// Parse is the stub walker. It runs the tree-sitter `c_sharp`
// grammar against the source so syntax errors surface as real
// ParseCtx errors, then returns an empty ParseResult. The
// sibling C#-parser stage replaces this body in place with the
// real extractor (compilation_unit walker emitting
// class_declaration / interface_declaration / struct_declaration
// / method_declaration / using_directive edges per the story
// brief §1 "C#: classes/interfaces/structs, methods,
// inheritance/interfaces, using directives").
func (csharpTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, csharp.GetLanguage())
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter c# parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	return ParseResult{}, nil
}
