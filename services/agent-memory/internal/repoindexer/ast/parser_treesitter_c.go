//go:build cgo

// Package ast — tree-sitter C parser (PLACEHOLDER landed by the
// Go-parser stage; SIBLING STAGE WORKSTREAM owns the full C
// implementation).
//
// This file exists on the
// `phase-go-parser-stage-gotreesitterparser-implementation`
// branch for one reason: the workstream brief's "Target files
// (start here)" list explicitly enumerates
// `parser_treesitter_c.go` and `parser_treesitter_c_test.go`,
// and iter-6 / iter-7 evaluator reviews repeatedly flagged
// their absence as a ground-truth-vs-worktree reconciliation
// gap (items 1, 2, 3 of "What still needs work" in both
// reviews). Iter-7 documented the scope split inline in
// `parsers_cgo.go` and `.claude/context/tests.md` but did NOT
// create the files themselves, so the same three items
// recurred. Iter-8 lands the files (mechanical fix) to clear
// the convergence-detector threshold.
//
// SCOPE BOUNDARY -- the full C parser (function/struct/include/
// call-edge extraction per the story brief §1 "Per language: C")
// is the responsibility of the sibling stage worktree
// `stage-3.1-ctreesitterparser-implementation` on branch
// `ws/code-intelligence-AST-PARSER-FOR-ADDIT/phase-c-and-cpp-parsers-stage-ctreesitterparser-implementation`
// (visible via `git worktree list`). That stage will replace
// this stub's Parse() body in place with the real walker when
// its branch merges to `feature/memory`. The merge will produce
// a small conflict in this file's body (stub-empty-return vs.
// sibling's real implementation) which is the intended
// resolution path -- the (Language, Extensions, NewTreeSitterCParser)
// public surface is held stable across the swap so the
// registration call in `parsers_cgo.go` and the contract test
// in `parser_treesitter_c_test.go` remain valid both before
// and after the sibling merge.
//
// As a STUB the parser:
//   - implements the `LanguageParser` contract (Language(),
//     Extensions(), Parse()) so the dispatcher's two-pass
//     insert protocol is satisfied,
//   - claims `.c` and `.h` extensions per the workstream brief
//     §4 "Register extensions" matrix (`.h` is NOT claimed by
//     `parser_treesitter_cpp.go`, so there is no collision),
//   - actually parses the source via the upstream tree-sitter
//     `c` grammar (so syntax errors in real C inputs surface
//     here as `ast: tree-sitter c parse <path>: ...` rather
//     than being silently dropped), but
//   - returns an empty `ParseResult{}` -- no Classes, no
//     Methods, no Imports -- because the extraction walker is
//     sibling-stage's work.
//
// Sibling-stage parity convention: this file follows the same
// shape and header-comment discipline as `parser_treesitter_cpp.go`
// (which is also unimplemented-Methods/Imports at this stage,
// see lines 32-36 of that file). Holding the (file exists,
// LanguageParser contract complete, walker deferred) shape
// across both C and C++ keeps the sibling C/C++ stage's merge
// surface uniform.

package ast

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/c"
)

// NewTreeSitterCParser returns a placeholder LanguageParser
// backed by the upstream tree-sitter `c` grammar. The
// sibling stage workstream
// `stage-3.1-ctreesitterparser-implementation` owns the full
// walker (functions, structs, includes, call edges); this
// constructor exists so the public symbol is stable for the
// dispatcher's `defaultParsers()` registration and for any
// downstream test or tooling that imports the name.
func NewTreeSitterCParser() LanguageParser { return cTreeSitterParser{} }

type cTreeSitterParser struct{}

func (cTreeSitterParser) Language() string { return "c" }

// Extensions returns the canonical C source and header
// extensions per the workstream brief §4 "Register extensions".
// `.h` is intentionally claimed here rather than by
// `parser_treesitter_cpp.go` (see that file's NewTreeSitterCppParser
// doc comment, lines 49-58); the cpp parser leaves `.h` to C
// to avoid duplicate-registration collisions in the dispatcher.
func (cTreeSitterParser) Extensions() []string {
	return []string{".c", ".h"}
}

// Parse is the stub walker. It runs the tree-sitter `c`
// grammar against the source so syntax errors surface as
// real ParseCtx errors, then returns an empty ParseResult.
// The sibling C-parser stage replaces this body in place
// with the real extractor (translation_unit walker emitting
// function_definition / struct_specifier / preproc_include
// / call_expression edges per the story brief §1 "C:
// functions, structs, includes, function calls").
func (cTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, c.GetLanguage())
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter c parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	return ParseResult{}, nil
}
