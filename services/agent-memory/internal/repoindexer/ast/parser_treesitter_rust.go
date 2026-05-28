//go:build cgo

// Rust tree-sitter LanguageParser implementation.
//
// This file is the v1 Rust parser core (per the AST-PARSER-FOR-
// ADDIT story, phase "rust parser", stage
// "rustTreeSitterParser implementation"). It rides the upstream
// `github.com/smacker/go-tree-sitter/rust` binding and emits the
// same `ParseResult` shape as the TypeScript / Python tree-sitter
// parsers in parser_treesitter.go -- the dispatcher and downstream
// consumers see no behavioural difference at runtime.
//
// Scope of v1 (matches the workstream brief):
//
//   - Walk the `source_file` root.
//   - Recurse into `mod_item` (in-file modules) WITHOUT
//     propagating the module name into `QualifiedName`. The
//     dispatcher's canonical-signature scheme keys class /
//     method nodes by `<repoURL>::class::<relPath>#<qualifiedName>`;
//     embedding the module name here would break a follow-up
//     cross-file resolver story that lives on top of `crate::*`
//     paths (a later workstream owns module-qualified
//     resolution).
//   - Emit `ClassDecl` for `struct_item` (Kind="struct"),
//     `enum_item` (Kind="enum"), and `trait_item` (Kind="trait").
//     A trait's supertrait clause `trait Foo: Bar + Baz`
//     populates `Extends` with the bare type identifiers
//     (`Bar`, `Baz`) so the dispatcher can emit the matching
//     `extends` edges per the Section 4.4.3 catalogue.
//   - Skip enum variants -- they are NOT emitted as Methods.
//     A variant is a constructor-style payload, not a callable
//     symbol; conflating the two would pollute the local-symbol
//     table used by `static_calls` resolution.
//
// Out of scope for v1 (deferred to follow-up workstreams):
//
//   - `function_item` (free functions) and `impl_item`
//     (impl-block methods) Method emission.
//   - `use_declaration` Import emission.
//   - Method-body call extraction (`Calls`, `ReceiverCalls`,
//     `MemberAccesses`).
//
// Those surfaces stay nil / empty on the v1 ParseResult so the
// dispatcher's two-pass insert protocol still completes without
// emitting partial / inconsistent edges. The follow-up stages
// add them additively (per the architecture's invariant C12:
// new parser surfaces must be DESCRIPTIVE, not IDENTIFYING).

package ast

import (
	"context"
	"fmt"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/rust"
)

// NewTreeSitterRustParser returns a LanguageParser that uses
// the upstream tree-sitter Rust grammar (covers `.rs`).
func NewTreeSitterRustParser() LanguageParser {
	return rustTreeSitterParser{}
}

// =====================================================================
// Rust tree-sitter implementation
// =====================================================================

type rustTreeSitterParser struct{}

func (rustTreeSitterParser) Language() string     { return "rust" }
func (rustTreeSitterParser) Extensions() []string { return []string{".rs"} }

func (rustTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, rust.GetLanguage())
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter rust parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	w := rustWalker{src: src}
	w.walkTop(root)
	return ParseResult{
		Classes: w.classes,
		Methods: w.methods,
		Imports: w.imports,
	}, nil
}

// Node-type constants. Kept here so a future grammar bump has
// one place to look (mirrors the TS / Python sections of
// parser_treesitter.go).
const (
	rustNodeModItem         = "mod_item"
	rustNodeDeclarationList = "declaration_list"
	rustNodeStructItem      = "struct_item"
	rustNodeEnumItem        = "enum_item"
	rustNodeTraitItem       = "trait_item"
	rustNodeTraitBounds     = "trait_bounds"
	rustNodeTypeIdentifier  = "type_identifier"
	rustNodeScopedTypeID    = "scoped_type_identifier"
	rustNodeGenericType     = "generic_type"
)

type rustWalker struct {
	src     []byte
	classes []ClassDecl
	methods []MethodDecl
	imports []Import
}

// walkTop visits every named child of the `source_file` root.
// Top-level `mod_item` children are recursed into so nested
// declarations surface at file scope (per the v1 contract --
// the module name is NOT prepended to `QualifiedName`).
func (w *rustWalker) walkTop(root *sitter.Node) {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.visitItem(root.NamedChild(int(i)))
	}
}

// visitItem dispatches a single named node by type. Unknown
// node types (function_item, impl_item, use_declaration, ...)
// are NOT recursed into -- they are handled by follow-up
// stages whose own walkers run alongside this one.
func (w *rustWalker) visitItem(n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case rustNodeStructItem:
		w.handleStruct(n)
	case rustNodeEnumItem:
		w.handleEnum(n)
	case rustNodeTraitItem:
		w.handleTrait(n)
	case rustNodeModItem:
		w.descendIntoModule(n)
	}
}

// descendIntoModule walks the body of an in-file `mod foo { ... }`
// block without propagating `foo` into any `QualifiedName`. The
// body is a `declaration_list` child; we iterate its named
// children and re-dispatch via visitItem.
//
// `mod foo;` (out-of-line module reference) has no body field
// and is skipped -- the actual declarations live in another
// file (`foo.rs` or `foo/mod.rs`) that the Repo Indexer will
// surface on its own.
func (w *rustWalker) descendIntoModule(n *sitter.Node) {
	body := n.ChildByFieldName("body")
	if body == nil {
		// Fallback: scan named children for a declaration_list.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c != nil && c.Type() == rustNodeDeclarationList {
				body = c
				break
			}
		}
	}
	if body == nil {
		return
	}
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		w.visitItem(body.NamedChild(int(i)))
	}
}

func (w *rustWalker) handleStruct(n *sitter.Node) {
	name := w.declName(n)
	if name == "" {
		return
	}
	w.classes = append(w.classes, ClassDecl{
		QualifiedName: name,
		Kind:          "struct",
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
	})
}

func (w *rustWalker) handleEnum(n *sitter.Node) {
	name := w.declName(n)
	if name == "" {
		return
	}
	// v1 contract: enum variants are NOT emitted as Methods.
	// A variant is a constructor-style payload, not a callable
	// symbol, so conflating it with the file's method set would
	// pollute the local-symbol table used by static_calls
	// resolution.
	w.classes = append(w.classes, ClassDecl{
		QualifiedName: name,
		Kind:          "enum",
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
	})
}

func (w *rustWalker) handleTrait(n *sitter.Node) {
	name := w.declName(n)
	if name == "" {
		return
	}
	cls := ClassDecl{
		QualifiedName: name,
		Kind:          "trait",
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
	}
	// Supertrait clause: `trait Foo: A + B { ... }`. tree-sitter
	// exposes the bound list as a `trait_bounds` node, accessible
	// via the `bounds` field; iterate its type-identifier leaves
	// to populate Extends. Names are raw identifiers; the
	// dispatcher resolves each against the same file's declared
	// classes and emits an `extends` edge per resolved entry,
	// dropping unresolved bare names (matching the documented
	// contract on ClassDecl.Extends).
	if bounds := n.ChildByFieldName("bounds"); bounds != nil {
		cls.Extends = collectRustTraitBoundIdents(bounds, w.src)
	} else {
		// Fallback: scan for a trait_bounds named child. The
		// grammar consistently exposes the `bounds` field, but
		// the defensive walk keeps the helper total if a future
		// grammar bump renames the field.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c != nil && c.Type() == rustNodeTraitBounds {
				cls.Extends = collectRustTraitBoundIdents(c, w.src)
				break
			}
		}
	}
	w.classes = append(w.classes, cls)
}

// declName returns the textual content of an item's `name`
// field, or "" when missing. The grammar gives every named
// item (struct_item, enum_item, trait_item, mod_item, ...) a
// `name` field whose child is a `type_identifier` (for type-
// like items) or `identifier` (for value-like items).
func (w *rustWalker) declName(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return ""
	}
	return nameNode.Content(w.src)
}

// collectRustTraitBoundIdents walks a `trait_bounds` subtree
// and returns the bare type-identifier names that appear as
// supertraits. A bound like `Foo + bar::Baz + Qux<T>` produces
// `["Foo", "Baz", "Qux"]` -- type arguments and lifetime
// bounds are intentionally dropped so the dispatcher's local-
// symbol resolver does not emit bogus `extends` edges to type
// parameters.
//
// The walk explicitly STOPS at type_arguments / type_parameters
// subtrees (mirroring the equivalent guard in
// collectTSIdentifiers); lifetime parameters and bare lifetime
// bounds like `'a` are filtered out by virtue of being
// `lifetime` nodes rather than identifier nodes.
//
// For a `scoped_type_identifier` (e.g. `std::fmt::Debug`) we
// take only the final segment -- the dispatcher's same-file
// resolver only matches bare names, so retaining the leading
// `std::fmt::` prefix would always fail to resolve. Cross-file
// resolution lands in a later workstream.
func collectRustTraitBoundIdents(n *sitter.Node, src []byte) []string {
	var out []string
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		if node == nil {
			return
		}
		switch node.Type() {
		case rustNodeTypeIdentifier:
			out = append(out, node.Content(src))
			return
		case rustNodeScopedTypeID:
			// scoped_type_identifier exposes the final
			// segment as a `name` field of type
			// type_identifier; fall back to the last
			// named child when the field is absent.
			if nm := node.ChildByFieldName("name"); nm != nil {
				out = append(out, nm.Content(src))
				return
			}
			if cnt := node.NamedChildCount(); cnt > 0 {
				last := node.NamedChild(int(cnt - 1))
				if last != nil && last.Type() == rustNodeTypeIdentifier {
					out = append(out, last.Content(src))
				}
			}
			return
		case rustNodeGenericType:
			// `Qux<T>` -- recurse into the head (a
			// type_identifier or scoped_type_identifier)
			// but stop before the type_arguments subtree.
			if head := node.ChildByFieldName("type"); head != nil {
				walk(head)
			}
			return
		case "type_arguments", "type_parameters":
			// Identifiers inside are type parameters /
			// instantiations, not supertraits.
			return
		}
		for i := uint32(0); i < node.NamedChildCount(); i++ {
			walk(node.NamedChild(int(i)))
		}
	}
	walk(n)
	return out
}
