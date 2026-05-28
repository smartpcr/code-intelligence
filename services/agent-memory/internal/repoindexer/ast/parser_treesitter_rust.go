//go:build cgo

// Rust tree-sitter LanguageParser implementation.
//
// This file is the v1 Rust parser core (per the AST-PARSER-FOR-
// ADDIT story, phase "rust parser", stage
// "rustTreeSitterParser implementation", implementation-plan.md
// Stage 5.1 lines 327-334). It rides the upstream
// `github.com/smacker/go-tree-sitter/rust` binding and emits
// the same `ParseResult` shape as the TypeScript / Python
// tree-sitter parsers in parser_treesitter.go -- the dispatcher
// and downstream consumers see no behavioural difference at
// runtime.
//
// v1 scope (matches implementation-plan.md:327-334):
//
//   - Walk the `source_file` root and recurse into `mod_item`
//     bodies WITHOUT propagating the module name into
//     `QualifiedName`. The dispatcher's canonical-signature
//     scheme keys class / method nodes by
//     `<repoURL>::class::<relPath>#<qualifiedName>`; embedding
//     the module name here would break a follow-up cross-file
//     resolver story that lives on top of `crate::*` paths.
//   - Emit `ClassDecl` for `struct_item` (Kind="struct"),
//     `enum_item` (Kind="enum"), and `trait_item` (Kind="trait").
//     A trait's supertrait clause `trait Foo: Bar + Baz`
//     populates `Extends` with the bare type identifiers.
//   - Skip enum variants -- they are NOT emitted as Methods.
//     A variant is a constructor-style payload, not a callable
//     symbol; conflating the two would pollute the local-symbol
//     table used by `static_calls` resolution.
//   - Emit `MethodDecl` for `function_item` and
//     `function_signature_item` inside a `trait_item` body
//     (the latter has no `body` field and represents a
//     trait-required method); the default-bodied form sets
//     `LangMeta["trait_default"]=true` so a later cross-file
//     resolver can identify trait defaults distinctly from
//     impl overrides.
//   - Emit `MethodDecl` for `function_item` inside an
//     `impl_item`. Inherent impls (`impl Foo { ... }`) set
//     `EnclosingClass=<Type>`; trait impls
//     (`impl Trait for Foo { ... }`) additionally set
//     `LangMeta["trait"]=<TraitName>` so the dispatcher's
//     Pass 2d can emit `overrides` edges, and append
//     `TraitName` to the target type's `Implements` so the
//     dispatcher's Pass 1 emits the matching `implements` edge.
//   - Emit `MethodDecl{EnclosingClass:""}` for free
//     `function_item` at file scope (and at the file-scope
//     equivalent inside in-file `mod_item` bodies).
//   - Walk each method/function body for:
//       * `call_expression` with `identifier` or
//         `scoped_identifier` callee -> `Calls` (the rightmost
//         segment of a `scoped_identifier` is used so
//         `std::vec::Vec::new()` resolves as `new`);
//       * `call_expression` with `field_expression` callee
//         where the receiver is `self` -> `ReceiverCalls`;
//         non-`self` receiver method calls are NOT emitted
//         in v1 (the receiver type is not statically known
//         and the dispatcher's same-file resolver cannot
//         resolve them without producing wrong edges);
//       * `field_expression` with `self` receiver (outside a
//         `call_expression`) -> `MemberAccesses`. The IsWrite
//         flag is set when the access appears on the LHS of
//         an `assignment_expression` or as the target of a
//         `compound_assignment_expr`.
//   - Emit `Import` per `use_declaration`:
//       * `use foo::bar::Baz;` -> `{Module:"foo::bar",
//         Symbols:["Baz"]}`;
//       * `use foo::bar::{A, B, C};` -> three Imports sharing
//         `Module:"foo::bar"`, each with one `Symbols` entry;
//       * `use foo::Bar as Baz;` -> `Symbols:["Bar"], Alias:"Baz"`;
//       * `use foo::*;` -> `Symbols:["*"]`;
//       * nested `use foo::{bar::{X, Y}, z::Z};` -> multiple
//         Imports with their fully-qualified Module prefixes.
//   - Collect `pub` / `pub(crate)` / `pub(super)` visibility
//     plus `async` / `unsafe` / `const` / `extern` function
//     modifiers (lower-case tokens); `macro_invocation` items
//     are explicitly skipped per the tech-spec non-goal.
//
// Out of scope for v1 (deferred to follow-up workstreams,
// catalogued on parser.go::MethodDecl):
//
//   - Generic type parameters and `where` clauses (the
//     identifier walk stops at `type_arguments` /
//     `type_parameters` for the same reason the TS walker
//     does -- type parameters are not heritage targets).
//   - Inherent type method resolution against `Box<Foo>` /
//     `Arc<Foo>` smart-pointer receivers (the rich receiver
//     story belongs to a later workstream).
//   - Cross-file `use` resolution (the dispatcher's later
//     cross-file resolver story stitches in-repo paths).

package ast

import (
	"context"
	"fmt"
	"strings"

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
	w := newRustWalker(src)
	w.walkTop(root)
	return ParseResult{
		Classes: w.classes,
		Methods: w.methods,
		Imports: w.imports,
	}, nil
}

// Grammar node-type constants. Centralising the literal strings
// keeps a single edit site if a future grammar bump renames a
// node (mirrors the discipline in parser_treesitter.go and
// parser_treesitter_cpp.go).
const (
	rustNodeSourceFile           = "source_file"
	rustNodeModItem              = "mod_item"
	rustNodeDeclarationList      = "declaration_list"
	rustNodeStructItem           = "struct_item"
	rustNodeEnumItem             = "enum_item"
	rustNodeTraitItem            = "trait_item"
	rustNodeImplItem             = "impl_item"
	rustNodeFunctionItem         = "function_item"
	rustNodeFunctionSignature    = "function_signature_item"
	rustNodeUseDeclaration       = "use_declaration"
	rustNodeMacroInvocation      = "macro_invocation"
	rustNodeMacroDefinition      = "macro_definition"
	rustNodeTraitBounds          = "trait_bounds"
	rustNodeTypeIdentifier       = "type_identifier"
	rustNodeScopedTypeID         = "scoped_type_identifier"
	rustNodeGenericType          = "generic_type"
	rustNodeReferenceType        = "reference_type"
	rustNodePointerType          = "pointer_type"
	rustNodeIdentifier           = "identifier"
	rustNodeScopedIdentifier     = "scoped_identifier"
	rustNodeFieldIdentifier      = "field_identifier"
	rustNodeSelfExpr             = "self"
	rustNodeCallExpression       = "call_expression"
	rustNodeFieldExpression      = "field_expression"
	rustNodeAssignmentExpr       = "assignment_expression"
	rustNodeCompoundAssignmentEx = "compound_assignment_expr"
	rustNodeBlock                = "block"
	rustNodeParameters           = "parameters"
	rustNodeVisibilityModifier   = "visibility_modifier"
	rustNodeFunctionModifiers    = "function_modifiers"
	rustNodeScopedUseList        = "scoped_use_list"
	rustNodeUseList              = "use_list"
	rustNodeUseAsClause          = "use_as_clause"
	rustNodeUseWildcard          = "use_wildcard"
	rustNodeCrate                = "crate"
	rustNodeSelfKw               = "self"
	rustNodeSuperKw              = "super"
)

// rustWalker accumulates ParseResult slices in source order
// across a single Parse call. The `classByName` map is used by
// impl_item handling to mutate a previously-emitted ClassDecl's
// Implements list when the impl carries a trait clause.
//
// `pendingImpls` is the impl-before-decl buffer per evaluator
// iter-3 finding #1. Valid Rust permits `impl Trait for Foo
// { ... }` to appear BEFORE `struct Foo;` in source order. The
// walker streams items in document order, so a forward-only
// `classByName` lookup in `handleImpl` would silently lose the
// Implements metadata (and the same-file `implements` edge the
// dispatcher's Pass 2a emits from it). The buffer records the
// trait name keyed by the target type; `appendClass` drains
// matching entries the first time the target is declared, then
// deletes the key so a later same-name declaration in a sibling
// in-file `mod_item` does NOT inherit the bond.
type rustWalker struct {
	src          []byte
	classes      []ClassDecl
	methods      []MethodDecl
	imports      []Import
	classByName  map[string]int      // QualifiedName -> classes[] index
	pendingImpls map[string][]string // target QN -> trait names awaiting decl
}

func newRustWalker(src []byte) *rustWalker {
	return &rustWalker{
		src:          src,
		classByName:  map[string]int{},
		pendingImpls: map[string][]string{},
	}
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

// visitItem dispatches a single named node by type. Items we
// do not recognise are ignored at this level; macro invocations
// are skipped explicitly so a stray `println!()` at file scope
// does not derail the walk.
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
	case rustNodeImplItem:
		w.handleImpl(n)
	case rustNodeFunctionItem:
		w.handleFreeFunction(n)
	case rustNodeUseDeclaration:
		w.handleUseDeclaration(n)
	case rustNodeModItem:
		w.descendIntoModule(n)
	case rustNodeMacroInvocation, rustNodeMacroDefinition:
		// explicit non-goal per tech spec Section 5.5
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

// =====================================================================
// Class-like items (struct, enum, trait)
// =====================================================================

func (w *rustWalker) handleStruct(n *sitter.Node) {
	name := w.declName(n)
	if name == "" {
		return
	}
	w.appendClass(ClassDecl{
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
	w.appendClass(ClassDecl{
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
	// Supertrait clause: `trait Foo: A + B { ... }`.
	if bounds := n.ChildByFieldName("bounds"); bounds != nil {
		cls.Extends = collectRustTraitBoundIdents(bounds, w.src)
	} else {
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c != nil && c.Type() == rustNodeTraitBounds {
				cls.Extends = collectRustTraitBoundIdents(c, w.src)
				break
			}
		}
	}
	w.appendClass(cls)

	// Walk the trait body for required/provided methods.
	// `function_item` with a body is a default-bodied method
	// (`LangMeta["trait_default"]=true`); `function_signature_item`
	// is a declaration-only required method (no body).
	body := traitOrImplBody(n)
	if body == nil {
		return
	}
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		member := body.NamedChild(int(i))
		if member == nil {
			continue
		}
		switch member.Type() {
		case rustNodeFunctionItem:
			w.appendTraitDefaultMethod(member, name)
		case rustNodeFunctionSignature:
			w.appendTraitRequiredMethod(member, name)
		}
	}
}

// =====================================================================
// impl blocks
// =====================================================================

// handleImpl emits methods for every `function_item` inside the
// impl body. The `type` field carries the target (e.g. the
// struct being impl'd); the optional `trait` field carries the
// trait being implemented. For trait impls we additionally
// append the trait name to the target type's Implements list.
//
// Two source-order cases:
//
//   - target class ALREADY declared earlier in this file:
//     append the trait to its `Implements` immediately.
//   - target class NOT yet declared (impl precedes struct OR
//     target lives in a different file): buffer the trait in
//     `pendingImpls`. `appendClass` drains the buffer when the
//     in-file `struct Foo` is later seen; cross-file targets
//     remain in the buffer at end-of-Parse and are dropped on
//     the next `Parse` call (the dispatcher persists trait
//     identity on each method's LangMeta["trait"] regardless,
//     so a future cross-file resolver can still emit the edge).
func (w *rustWalker) handleImpl(n *sitter.Node) {
	targetType := w.extractImplTarget(n)
	if targetType == "" {
		return
	}
	traitName := ""
	if tr := n.ChildByFieldName("trait"); tr != nil {
		traitName = collectRustHeadIdent(tr, w.src)
	}
	if traitName != "" {
		if idx, ok := w.classByName[targetType]; ok {
			// Already-seen path: mutate the existing
			// ClassDecl's Implements in place.
			w.classes[idx].Implements = appendUnique(w.classes[idx].Implements, traitName)
		} else {
			// impl-before-decl / cross-file path: buffer
			// the trait for `appendClass` to flush when
			// (or if) the target declaration arrives.
			w.pendingImpls[targetType] = appendUnique(w.pendingImpls[targetType], traitName)
		}
	}

	body := traitOrImplBody(n)
	if body == nil {
		return
	}
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		member := body.NamedChild(int(i))
		if member == nil || member.Type() != rustNodeFunctionItem {
			continue
		}
		w.appendImplMethod(member, targetType, traitName)
	}
}

// extractImplTarget returns the bare type identifier of an
// impl's target. Handles:
//   - `impl Foo { ... }`                  -> "Foo"
//   - `impl<T> Foo<T> { ... }`            -> "Foo"
//   - `impl Trait for Foo { ... }`        -> "Foo"
//   - `impl Trait for &Foo { ... }`       -> "Foo"
//   - `impl Trait for &mut Foo { ... }`   -> "Foo"
//   - `impl Trait for Box<Foo> { ... }`   -> "Box"  (limitation:
//     smart-pointer canonicalisation belongs to a later
//     workstream; v1 records the syntactic head.)
//   - `impl Trait for foo::Bar { ... }`   -> "Bar"
func (w *rustWalker) extractImplTarget(n *sitter.Node) string {
	t := n.ChildByFieldName("type")
	if t == nil {
		return ""
	}
	return collectRustHeadIdent(t, w.src)
}

// =====================================================================
// Free functions
// =====================================================================

// handleFreeFunction emits a MethodDecl with empty
// EnclosingClass for a `function_item` at file scope (or at the
// file-scope equivalent inside an in-file `mod_item` body).
func (w *rustWalker) handleFreeFunction(n *sitter.Node) {
	name := w.declName(n)
	if name == "" {
		return
	}
	m := w.buildMethod(n, name, "")
	w.methods = append(w.methods, m)
}

// appendTraitDefaultMethod emits a default-bodied trait method.
// LangMeta["trait_default"]=true so the dispatcher can
// distinguish trait-side defaults from impl-side overrides.
func (w *rustWalker) appendTraitDefaultMethod(n *sitter.Node, traitName string) {
	name := w.declName(n)
	if name == "" {
		return
	}
	qn := traitName + "." + name
	m := w.buildMethod(n, qn, traitName)
	if m.LangMeta == nil {
		m.LangMeta = map[string]any{}
	}
	m.LangMeta["trait_default"] = true
	w.methods = append(w.methods, m)
}

// appendTraitRequiredMethod emits a declaration-only trait
// method (`function_signature_item`, no body). Required-method
// nodes have no body so Calls/ReceiverCalls/MemberAccesses
// stay empty.
func (w *rustWalker) appendTraitRequiredMethod(n *sitter.Node, traitName string) {
	name := w.declName(n)
	if name == "" {
		return
	}
	qn := traitName + "." + name
	m := MethodDecl{
		QualifiedName:  qn,
		EnclosingClass: traitName,
		ParamSignature: extractParams(n, w.src),
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
		Modifiers:      collectRustModifiers(n, w.src),
	}
	w.methods = append(w.methods, m)
}

// appendImplMethod emits an impl-block method. When `traitName`
// is non-empty the method carries `LangMeta["trait"]=traitName`
// so the dispatcher's Pass 2d can emit the matching
// `overrides` edge against the trait's default method (or
// persist the trait identity for cross-file lookup).
func (w *rustWalker) appendImplMethod(n *sitter.Node, targetType, traitName string) {
	name := w.declName(n)
	if name == "" {
		return
	}
	qn := targetType + "." + name
	m := w.buildMethod(n, qn, targetType)
	if traitName != "" {
		if m.LangMeta == nil {
			m.LangMeta = map[string]any{}
		}
		m.LangMeta["trait"] = traitName
	}
	w.methods = append(w.methods, m)
}

// buildMethod constructs the common MethodDecl shape for any
// Rust function (free, trait-default, impl). Walks the body
// for calls/member-accesses; populates body span fields.
func (w *rustWalker) buildMethod(n *sitter.Node, qn, enclosing string) MethodDecl {
	m := MethodDecl{
		QualifiedName:  qn,
		EnclosingClass: enclosing,
		ParamSignature: extractParams(n, w.src),
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
		Modifiers:      collectRustModifiers(n, w.src),
	}
	if body := n.ChildByFieldName("body"); body != nil && body.Type() == rustNodeBlock {
		m.BodySource, m.BodyStartByte, m.BodyEndByte = rustStripBraceSpan(w.src, body)
		m.BodyStartLine = int(body.StartPoint().Row) + 1
		m.BodyEndLine = int(body.EndPoint().Row) + 1
		m.Calls = uniqueStringsInsert(walkRustCalls(body, w.src))
		m.ReceiverCalls = uniqueStringsInsert(walkRustReceiverCalls(body, w.src))
		m.MemberAccesses = walkRustMemberAccesses(body, w.src)
	}
	return m
}

// =====================================================================
// use_declaration parsing
// =====================================================================

// handleUseDeclaration emits one Import per imported symbol.
// A use_declaration has an `argument` field whose subtree
// encodes the path / list / alias / wildcard variants.
func (w *rustWalker) handleUseDeclaration(n *sitter.Node) {
	arg := n.ChildByFieldName("argument")
	if arg == nil {
		// Fallback: scan named children.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c != nil {
				arg = c
				break
			}
		}
	}
	if arg == nil {
		return
	}
	line := int(n.StartPoint().Row) + 1
	expandRustUseTree(arg, w.src, "", line, &w.imports)
}

// expandRustUseTree recursively walks the argument subtree of
// a `use_declaration`, accumulating Imports keyed by their
// module prefix. The four leaf variants:
//
//   - identifier / scoped_identifier (e.g. `foo::bar::Baz`):
//     emit `{Module:<everything-before-last-segment>,
//     Symbols:[<last>]}`. A bare `use Baz;` (no path) emits
//     `{Module:"", Symbols:["Baz"]}`.
//   - use_as_clause (`foo::Bar as Baz`): emit `Symbols:["Bar"],
//     Alias:"Baz"`.
//   - use_wildcard (`foo::*`): emit `Symbols:["*"]`.
//   - scoped_use_list (`foo::{A, B}`): recurse into each
//     child with the scope-prefixed Module.
func expandRustUseTree(n *sitter.Node, src []byte, prefix string, line int, out *[]Import) {
	if n == nil {
		return
	}
	switch n.Type() {
	case rustNodeIdentifier, rustNodeTypeIdentifier:
		name := n.Content(src)
		mod, sym := splitRustUsePath(prefix, name)
		*out = append(*out, Import{Module: mod, Symbols: []string{sym}, Line: line})
	case rustNodeScopedIdentifier:
		full := rustScopedIdentifierString(n, src)
		// full is "a::b::c"; split at last "::"
		mod, sym := splitRustScopedPath(prefix, full)
		*out = append(*out, Import{Module: mod, Symbols: []string{sym}, Line: line})
	case rustNodeUseAsClause:
		path := n.ChildByFieldName("path")
		alias := n.ChildByFieldName("alias")
		if path == nil {
			return
		}
		var module, symbol string
		switch path.Type() {
		case rustNodeScopedIdentifier:
			full := rustScopedIdentifierString(path, src)
			module, symbol = splitRustScopedPath(prefix, full)
		case rustNodeIdentifier, rustNodeTypeIdentifier:
			module, symbol = splitRustUsePath(prefix, path.Content(src))
		default:
			module, symbol = prefix, path.Content(src)
		}
		imp := Import{Module: module, Symbols: []string{symbol}, Line: line}
		if alias != nil {
			imp.Alias = alias.Content(src)
		}
		*out = append(*out, imp)
	case rustNodeUseWildcard:
		// `foo::*` -- the scope prefix (if any) is on the
		// path child; if absent we're at top-level wildcard.
		mod := prefix
		if path := n.ChildByFieldName("path"); path != nil {
			switch path.Type() {
			case rustNodeScopedIdentifier:
				full := rustScopedIdentifierString(path, src)
				mod = joinRustScope(prefix, full)
			case rustNodeIdentifier, rustNodeTypeIdentifier:
				mod = joinRustScope(prefix, path.Content(src))
			}
		} else {
			// No path field -- the wildcard's siblings carry
			// the prefix (handled at the scoped_use_list
			// level below).
		}
		*out = append(*out, Import{Module: mod, Symbols: []string{"*"}, Line: line})
	case rustNodeScopedUseList:
		// `foo::bar::{A, B, C}` -- path field is the prefix,
		// list field is a use_list whose named children are
		// the individual leaves.
		path := n.ChildByFieldName("path")
		list := n.ChildByFieldName("list")
		newPrefix := prefix
		if path != nil {
			switch path.Type() {
			case rustNodeScopedIdentifier:
				newPrefix = joinRustScope(prefix, rustScopedIdentifierString(path, src))
			case rustNodeIdentifier, rustNodeTypeIdentifier:
				newPrefix = joinRustScope(prefix, path.Content(src))
			}
		}
		if list == nil {
			for i := uint32(0); i < n.NamedChildCount(); i++ {
				c := n.NamedChild(int(i))
				if c != nil && c.Type() == rustNodeUseList {
					list = c
					break
				}
			}
		}
		if list == nil {
			return
		}
		for i := uint32(0); i < list.NamedChildCount(); i++ {
			expandRustUseTree(list.NamedChild(int(i)), src, newPrefix, line, out)
		}
	case rustNodeUseList:
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			expandRustUseTree(n.NamedChild(int(i)), src, prefix, line, out)
		}
	case rustNodeSelfKw, rustNodeCrate, rustNodeSuperKw:
		// Bare `self` / `crate` / `super` -- treat as a
		// single Symbol with the existing prefix as Module.
		mod, sym := prefix, n.Content(src)
		*out = append(*out, Import{Module: mod, Symbols: []string{sym}, Line: line})
	}
}

// splitRustUsePath produces the (Module, Symbol) pair for a
// bare identifier under an existing prefix. The prefix becomes
// the module; the identifier becomes the symbol.
func splitRustUsePath(prefix, ident string) (string, string) {
	return prefix, ident
}

// splitRustScopedPath splits a `::`-joined path like
// "foo::bar::Baz" into ("foo::bar", "Baz"), then prepends the
// prefix to the module. A path with no `::` becomes
// (prefix, path).
func splitRustScopedPath(prefix, full string) (string, string) {
	idx := strings.LastIndex(full, "::")
	if idx < 0 {
		return prefix, full
	}
	module := full[:idx]
	symbol := full[idx+2:]
	return joinRustScope(prefix, module), symbol
}

// joinRustScope concatenates a scope prefix and a tail with
// `::`, handling the empty cases.
func joinRustScope(prefix, tail string) string {
	if prefix == "" {
		return tail
	}
	if tail == "" {
		return prefix
	}
	return prefix + "::" + tail
}

// rustScopedIdentifierString returns the string form of a
// `scoped_identifier` like `std::fmt::Display`.
func rustScopedIdentifierString(n *sitter.Node, src []byte) string {
	return n.Content(src)
}

// =====================================================================
// Body walk: calls, receiver calls, member accesses
// =====================================================================

// walkRustCalls extracts bare-name call sites under body. A
// `call_expression` whose `function` field is an `identifier`
// records the identifier; a `scoped_identifier` records its
// rightmost segment so `std::vec::Vec::new()` and `new()`
// produce the same `Calls` entry (the same-file resolver
// matches on the bare name).
//
// Receiver-qualified calls (`self.foo()`, `x.foo()`) are NOT
// collected here -- they have a dedicated walker
// (walkRustReceiverCalls) that limits the receiver to `self`
// because the dispatcher's same-file resolver cannot
// disambiguate arbitrary-receiver method calls without static
// type information.
func walkRustCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != rustNodeCallExpression {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil {
			return true
		}
		switch fn.Type() {
		case rustNodeIdentifier:
			out = append(out, fn.Content(src))
		case rustNodeScopedIdentifier:
			// Take the rightmost segment via the `name` field.
			if nm := fn.ChildByFieldName("name"); nm != nil {
				out = append(out, nm.Content(src))
			}
		}
		return true
	})
	return out
}

// walkRustReceiverCalls extracts `self.<name>(...)` call
// targets. Non-`self` receiver calls are NOT collected --
// receiver type inference is out of scope for v1.
func walkRustReceiverCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != rustNodeCallExpression {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil || fn.Type() != rustNodeFieldExpression {
			return true
		}
		value := fn.ChildByFieldName("value")
		field := fn.ChildByFieldName("field")
		if value == nil || field == nil {
			return true
		}
		if value.Type() != rustNodeSelfExpr {
			return true
		}
		if field.Type() == rustNodeFieldIdentifier || field.Type() == rustNodeIdentifier {
			out = append(out, field.Content(src))
		}
		return true
	})
	return out
}

// walkRustMemberAccesses returns the per-method list of
// `self.<name>` field references with IsWrite set when the
// access appears on the LHS of an assignment or
// compound_assignment_expr. Each name is recorded once with
// IsWrite=true winning over IsWrite=false (matches the
// TypeScript walker's semantics).
//
// Field accesses that are the callee of a `call_expression`
// (i.e. `self.foo()`) are EXCLUDED -- they are already covered
// by walkRustReceiverCalls; counting them here too would
// produce spurious `reads` edges to method names.
func walkRustMemberAccesses(body *sitter.Node, src []byte) []MemberAccess {
	writes := map[string]struct{}{}
	var reads []string
	seen := map[string]bool{}
	// First pass: identify field expressions that are callees
	// of a call_expression so we can exclude them.
	calleeSet := map[*sitter.Node]struct{}{}
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() == rustNodeCallExpression {
			if fn := node.ChildByFieldName("function"); fn != nil && fn.Type() == rustNodeFieldExpression {
				calleeSet[fn] = struct{}{}
			}
		}
		return true
	})
	// Second pass: collect writes and reads.
	walkChildren(body, func(node *sitter.Node) bool {
		switch node.Type() {
		case rustNodeAssignmentExpr, rustNodeCompoundAssignmentEx:
			lhs := node.ChildByFieldName("left")
			if name, ok := isSelfField(lhs, src); ok {
				writes[name] = struct{}{}
				if !seen[name] {
					seen[name] = true
					reads = append(reads, name)
				}
			}
		case rustNodeFieldExpression:
			if _, isCallee := calleeSet[node]; isCallee {
				return true
			}
			if name, ok := isSelfField(node, src); ok {
				if !seen[name] {
					seen[name] = true
					reads = append(reads, name)
				}
			}
		}
		return true
	})
	if len(reads) == 0 {
		return nil
	}
	out := make([]MemberAccess, 0, len(reads))
	for _, name := range reads {
		_, isW := writes[name]
		out = append(out, MemberAccess{Name: name, IsWrite: isW})
	}
	return out
}

// isSelfField reports whether `n` is a `field_expression` of
// the form `self.<field>` and returns the field name.
func isSelfField(n *sitter.Node, src []byte) (string, bool) {
	if n == nil || n.Type() != rustNodeFieldExpression {
		return "", false
	}
	value := n.ChildByFieldName("value")
	field := n.ChildByFieldName("field")
	if value == nil || field == nil {
		return "", false
	}
	if value.Type() != rustNodeSelfExpr {
		return "", false
	}
	if field.Type() != rustNodeFieldIdentifier && field.Type() != rustNodeIdentifier {
		return "", false
	}
	return field.Content(src), true
}

// =====================================================================
// Shared helpers
// =====================================================================

// appendClass records a new ClassDecl AND remembers its index
// in classByName so impl_item handling can mutate its
// Implements list in place.
//
// Per evaluator iter-3 finding #1: any traits buffered in
// `pendingImpls` for this QualifiedName are flushed onto the
// ClassDecl's `Implements` AFTER any trait-bound entries the
// caller pre-populated (the caller's order wins on dedup), then
// the buffer key is DELETED. The deletion matters when an
// in-file `mod_item` later declares a struct with the same
// name -- without it, the second declaration would inherit
// trait bonds that semantically belong to a different type.
func (w *rustWalker) appendClass(c ClassDecl) {
	if pending, ok := w.pendingImpls[c.QualifiedName]; ok {
		for _, trait := range pending {
			c.Implements = appendUnique(c.Implements, trait)
		}
		delete(w.pendingImpls, c.QualifiedName)
	}
	idx := len(w.classes)
	w.classes = append(w.classes, c)
	w.classByName[c.QualifiedName] = idx
}

// declName returns the textual content of an item's `name`
// field, or "" when missing. The grammar gives every named
// item (struct_item, enum_item, trait_item, mod_item,
// function_item, ...) a `name` field whose child is a
// `type_identifier` (type-like items) or `identifier`
// (value-like items).
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

// traitOrImplBody returns the `declaration_list` body of a
// trait_item or impl_item, using the `body` field with a fallback
// scan for the first `declaration_list` named child.
func traitOrImplBody(n *sitter.Node) *sitter.Node {
	if body := n.ChildByFieldName("body"); body != nil {
		return body
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c != nil && c.Type() == rustNodeDeclarationList {
			return c
		}
	}
	return nil
}

// extractParams returns the source of a function's parameter
// list with the surrounding parentheses stripped. Mirrors the
// TS/Python parsers' approach so canonical signatures stay
// comparable across languages.
func extractParams(n *sitter.Node, src []byte) string {
	p := n.ChildByFieldName("parameters")
	if p == nil {
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c != nil && c.Type() == rustNodeParameters {
				p = c
				break
			}
		}
	}
	if p == nil {
		return ""
	}
	return trimParens(p.Content(src))
}

// collectRustModifiers returns the source text of every
// visibility / function modifier leaf appearing on an item.
// Order is source order. Recognised tokens:
//
//   - visibility_modifier: `pub`, `pub(crate)`, `pub(super)`,
//     `pub(in path)` -- the full verbatim text is recorded.
//   - function_modifiers child tokens: `async`, `unsafe`,
//     `const`, `extern` (the latter may be followed by an
//     ABI string like `"C"` which is NOT included in the
//     modifier slice; ABI persistence belongs to a later
//     workstream).
//
// `pub` followed by `pub(crate)`-style restrictions surfaces
// as a single combined token whose text matches the source.
func collectRustModifiers(n *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case rustNodeVisibilityModifier:
			out = append(out, c.Content(src))
		case rustNodeFunctionModifiers:
			// Iterate the modifier list's children -- each
			// is one of `async`, `unsafe`, `const`, `extern`.
			for j := uint32(0); j < c.ChildCount(); j++ {
				sub := c.Child(int(j))
				if sub == nil {
					continue
				}
				switch sub.Type() {
				case "async", "unsafe", "const", "extern":
					out = append(out, sub.Type())
				}
			}
		}
	}
	return out
}

// collectRustHeadIdent extracts the head type identifier of a
// type expression, peeling references / pointers / generics /
// scopes to recover the bare name suitable for matching
// against ClassDecls declared in the same file. Examples:
//
//   - `Foo`                  -> "Foo"
//   - `Foo<T>`               -> "Foo"
//   - `&Foo`                 -> "Foo"
//   - `&mut Foo`             -> "Foo"
//   - `*const Foo`           -> "Foo"
//   - `Box<Foo>`             -> "Box"  (limitation: smart-
//     pointer canonicalisation is a later workstream; v1
//     records the syntactic head)
//   - `foo::Bar`             -> "Bar"
//   - `crate::foo::Bar`      -> "Bar"
func collectRustHeadIdent(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case rustNodeTypeIdentifier, rustNodeIdentifier:
		return n.Content(src)
	case rustNodeScopedTypeID:
		if nm := n.ChildByFieldName("name"); nm != nil {
			return nm.Content(src)
		}
		// Fallback: last named child.
		if cnt := n.NamedChildCount(); cnt > 0 {
			last := n.NamedChild(int(cnt - 1))
			if last != nil {
				return collectRustHeadIdent(last, src)
			}
		}
	case rustNodeGenericType:
		if head := n.ChildByFieldName("type"); head != nil {
			return collectRustHeadIdent(head, src)
		}
		// Fallback: first named child.
		if n.NamedChildCount() > 0 {
			return collectRustHeadIdent(n.NamedChild(0), src)
		}
	case rustNodeReferenceType, rustNodePointerType:
		if inner := n.ChildByFieldName("type"); inner != nil {
			return collectRustHeadIdent(inner, src)
		}
		// Fallback: scan for the first identifier-bearing child.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c == nil {
				continue
			}
			if name := collectRustHeadIdent(c, src); name != "" {
				return name
			}
		}
	}
	// Last-ditch: scan named children for the first
	// type_identifier-bearing subtree.
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		if name := collectRustHeadIdent(c, src); name != "" {
			return name
		}
	}
	return ""
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
			if head := node.ChildByFieldName("type"); head != nil {
				walk(head)
			}
			return
		case "type_arguments", "type_parameters":
			return
		}
		for i := uint32(0); i < node.NamedChildCount(); i++ {
			walk(node.NamedChild(int(i)))
		}
	}
	walk(n)
	return out
}

// appendUnique adds s to dst only if dst does not already
// contain s. Used for trait-Implements accumulation when
// multiple `impl Trait for Foo` blocks in the same file
// reference the same trait.
func appendUnique(dst []string, s string) []string {
	for _, existing := range dst {
		if existing == s {
			return dst
		}
	}
	return append(dst, s)
}

// rustStripBraceSpan extracts the interior of a Rust `block`
// node and returns the inner source alongside the interior
// byte offsets. Same semantics as `tsStripBraceSpan` in
// parser_treesitter.go -- we strip the outer `{` / `}` so
// `CountLogicalLines` and the §8.2 block threshold stay
// consistent across language back-ends.
func rustStripBraceSpan(src []byte, body *sitter.Node) (string, int, int) {
	startByte := int(body.StartByte())
	endByte := int(body.EndByte())
	content := body.Content(src)
	if len(content) < 2 || content[0] != '{' || content[len(content)-1] != '}' {
		return content, startByte, endByte
	}
	inner := content[1 : len(content)-1]
	return inner, startByte + 1, endByte - 2
}
