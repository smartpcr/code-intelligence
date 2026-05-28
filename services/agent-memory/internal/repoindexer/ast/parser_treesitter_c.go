//go:build cgo

// Package ast tree-sitter C parser.
//
// This file implements the LanguageParser interface for the C
// language on top of the smacker/go-tree-sitter C grammar,
// per the cTreeSitterParser brief in the AST-PARSER-FOR-ADDIT
// story (tech-spec.md Section 5.1).
//
// The parser walks the `translation_unit` root, visits each
// named child, and emits:
//
//   - `ClassDecl{Kind:"struct"|"union"|"enum",
//     QualifiedName:<identifier>}` for `struct_specifier` /
//     `union_specifier` / `enum_specifier` nodes with a
//     non-empty body. Anonymous specifiers (no identifier --
//     typedef carriers such as `typedef struct { ... } Foo;`)
//     are SKIPPED, as are forward declarations (`struct
//     Foo;`, no body).
//   - `MethodDecl{EnclosingClass:"",
//     QualifiedName:<declarator-id>,
//     ParamSignature:<between outer parens>,
//     BodySource:<brace-stripped>}` for every
//     `function_definition`. Prototype-only declarations
//     (`int foo(int);`) are SKIPPED in v1.
//   - `Import{Module:<path>}` for every `preproc_include`
//     directive. System includes (`#include <stdio.h>`) keep
//     the bracketed path verbatim (`stdio.h`); local includes
//     (`#include "local.h"`) gain a `./` prefix so the
//     dispatcher's `isRelativeImport` filter drops the edge
//     (architecture Section 4.3). A path that already begins
//     with `.` or `/` is left as-is to avoid `././local.h`
//     doubling.
//
// Per tech-spec.md Section 5.1:
//   - Modifier collection is restricted to `static`, `inline`,
//     `extern`, `const`; other storage-class / type-qualifier
//     tokens are not surfaced in v1.
//   - `ReceiverCalls` / `MemberAccesses` are always empty (C
//     has no `this`).
//   - `LangMeta` is always nil -- the decl kind already rides
//     `ClassDecl.Kind`.
//
// Call-site extraction is limited to plain identifier callees
// inside the function body. Function-pointer calls
// (`(*cb)(x)`) and member calls (`obj->method(x)`) are NOT
// recorded because the target name is not resolvable at parse
// time (tech-spec Section 5.1, last two rows of the construct
// table).

package ast

import (
	"context"
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	tsc "github.com/smacker/go-tree-sitter/c"
)

// NewTreeSitterCParser returns a LanguageParser that uses the
// upstream tree-sitter C grammar. The returned instance claims
// `.c` and `.h`. The `.h` -> C routing is the pinned default
// (`dot-h-extension-routing`); C++-only headers must use
// `.hpp` / `.hh` / `.hxx` / `.h++` to opt into the C++
// grammar (tech-spec Section 8 R6).
func NewTreeSitterCParser() LanguageParser { return cTreeSitterParser{} }

// cTreeSitterParser is the C LanguageParser implementation
// described in tech-spec.md Section 5.1.
type cTreeSitterParser struct{}

func (cTreeSitterParser) Language() string { return "c" }

func (cTreeSitterParser) Extensions() []string { return []string{".c", ".h"} }

func (cTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, tsc.GetLanguage())
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter c parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	w := cWalker{src: src}
	w.walkTop(root)
	return ParseResult{
		Classes: w.classes,
		Methods: w.methods,
		Imports: w.imports,
	}, nil
}

// Grammar node-type constants -- per the existing TS / Python
// pattern (parser_treesitter.go lines 110-136), all grammar-
// specific strings live here so a smacker grammar bump pins
// the diff to one place (tech-spec Section 8 R7).
const (
	cNodeTranslationUnit    = "translation_unit"
	cNodeDeclaration        = "declaration"
	cNodeFunctionDefinition = "function_definition"
	cNodeFunctionDeclarator = "function_declarator"
	cNodePointerDeclarator  = "pointer_declarator"
	cNodeParenDeclarator    = "parenthesized_declarator"
	cNodeStructSpecifier    = "struct_specifier"
	cNodeUnionSpecifier     = "union_specifier"
	cNodeEnumSpecifier      = "enum_specifier"
	cNodePreprocInclude     = "preproc_include"
	cNodeSystemLibString    = "system_lib_string"
	cNodeStringLiteral      = "string_literal"
	cNodeCompoundStatement  = "compound_statement"
	cNodeCallExpression     = "call_expression"
	cNodeIdentifier         = "identifier"
	cNodeFieldIdentifier    = "field_identifier"
	cNodeTypeIdentifier     = "type_identifier"
	cNodeFieldDeclList      = "field_declaration_list"
	cNodeEnumeratorList     = "enumerator_list"
	cNodeStorageClass       = "storage_class_specifier"
	cNodeTypeQualifier      = "type_qualifier"
	cNodeLinkageSpec        = "linkage_specification"
	cNodePreprocIfdef       = "preproc_ifdef"
	cNodePreprocIf          = "preproc_if"
	cNodePreprocElse        = "preproc_else"
	cNodePreprocElif        = "preproc_elif"
)

type cWalker struct {
	src     []byte
	classes []ClassDecl
	methods []MethodDecl
	imports []Import
}

func (w *cWalker) walkTop(root *sitter.Node) {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.visitTopLevel(root.NamedChild(int(i)))
	}
}

// visitTopLevel dispatches one named child of the
// translation_unit (or a recursive container like
// `linkage_specification` for `extern "C" { ... }`). Each
// branch is TERMINAL -- declaration handling owns the inner
// struct/union/enum specifier extraction so a single source
// declaration does not double-emit through fall-through
// recursion (per rubber-duck #6).
func (w *cWalker) visitTopLevel(n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case cNodeFunctionDefinition:
		w.handleFunctionDefinition(n)
	case cNodeDeclaration:
		w.handleDeclaration(n)
	case cNodeStructSpecifier:
		w.handleStructLike(n, "struct")
	case cNodeUnionSpecifier:
		w.handleStructLike(n, "union")
	case cNodeEnumSpecifier:
		w.handleStructLike(n, "enum")
	case cNodePreprocInclude:
		w.handleInclude(n)
	case cNodeLinkageSpec:
		// `extern "C" { ... }` -- recurse so the contained
		// function_definition / declaration nodes are still
		// extracted. The brief does not call this out
		// explicitly but headers commonly wrap C-callable
		// surface in it and dropping the whole block would
		// silently under-cover the API.
		body := n.ChildByFieldName("body")
		if body == nil {
			for i := uint32(0); i < n.NamedChildCount(); i++ {
				c := n.NamedChild(int(i))
				if c.Type() == "declaration_list" {
					body = c
					break
				}
			}
		}
		if body != nil {
			for i := uint32(0); i < body.NamedChildCount(); i++ {
				w.visitTopLevel(body.NamedChild(int(i)))
			}
		}
	case cNodePreprocIfdef, cNodePreprocIf, cNodePreprocElse, cNodePreprocElif:
		// Preprocessor conditionals (`#ifndef HEADER_GUARD
		// ... #endif`, `#if CONDITION ... #else ... #endif`)
		// hold their conditional body as DIRECT named
		// children rather than under a `body` field. Header
		// files almost universally wrap their contents in an
		// include guard, so failing to recurse here would
		// silently drop every struct / include / prototype
		// in every header (see HeaderFileOnly fixture in the
		// test suite). Iterate every named child and re-
		// dispatch through visitTopLevel; non-matching child
		// types (the condition identifier, nested
		// preproc_else branches) fall through harmlessly.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			w.visitTopLevel(n.NamedChild(int(i)))
		}
	}
}

// handleDeclaration owns every `declaration` child of the
// translation unit. A declaration node may carry:
//   - a `struct_specifier` / `union_specifier` /
//     `enum_specifier` definition (e.g. `struct Foo { int x;
//     };`) -- emit a ClassDecl when the specifier names a
//     non-empty body.
//   - a `function_declarator` prototype (e.g. `int foo(int);`)
//     -- SKIP in v1 per tech-spec Section 5.1.
//   - a plain variable declaration -- SKIP (out of v1 scope).
//
// Per rubber-duck #6, this branch is terminal: visitTopLevel
// never recurses into declaration children, so all struct /
// union / enum specifier extraction lives here.
func (w *cWalker) handleDeclaration(n *sitter.Node) {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(int(i))
		switch child.Type() {
		case cNodeStructSpecifier:
			w.handleStructLike(child, "struct")
		case cNodeUnionSpecifier:
			w.handleStructLike(child, "union")
		case cNodeEnumSpecifier:
			w.handleStructLike(child, "enum")
		}
	}
}

// handleStructLike emits a ClassDecl for a named, body-bearing
// struct / union / enum specifier. Anonymous specifiers
// (typedef carriers, anonymous-tag fields) and forward
// declarations (no body) are silently skipped per
// tech-spec.md Section 5.1.
func (w *cWalker) handleStructLike(n *sitter.Node, kind string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Content(w.src))
	if name == "" {
		return
	}
	body := n.ChildByFieldName("body")
	if body == nil {
		// Fall back to a positional scan -- some grammar
		// versions tag the body field name only for one of
		// struct/union/enum and leave the others to be
		// found via child type.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c.Type() == cNodeFieldDeclList || c.Type() == cNodeEnumeratorList {
				body = c
				break
			}
		}
	}
	if body == nil || body.NamedChildCount() == 0 {
		// Forward declaration (no body) or an empty body --
		// skip. v1 wants concrete shapes only.
		return
	}
	w.classes = append(w.classes, ClassDecl{
		QualifiedName: name,
		Kind:          kind,
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
	})
}

// handleFunctionDefinition emits one MethodDecl per top-level
// `function_definition`. The declarator chain is unwrapped
// strictly along the `declarator` field so nested function-
// pointer parameters (e.g. `int apply(int (*cb)(int))`) are
// NOT mistaken for the function's own declarator (per
// rubber-duck #3).
func (w *cWalker) handleFunctionDefinition(n *sitter.Node) {
	decl := n.ChildByFieldName("declarator")
	if decl == nil {
		return
	}
	fnDecl := findFunctionDeclarator(decl)
	if fnDecl == nil {
		return
	}
	name := extractDeclaratorName(fnDecl, w.src)
	if name == "" {
		return
	}
	params := ""
	if p := fnDecl.ChildByFieldName("parameters"); p != nil {
		params = trimParens(p.Content(w.src))
	}
	method := MethodDecl{
		QualifiedName:  name,
		EnclosingClass: "",
		ParamSignature: params,
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
		Modifiers:      collectCModifiers(n, w.src),
	}
	if body := n.ChildByFieldName("body"); body != nil &&
		body.Type() == cNodeCompoundStatement {
		method.BodySource, method.BodyStartByte, method.BodyEndByte =
			cStripBraceSpan(w.src, body)
		method.BodyStartLine = int(body.StartPoint().Row) + 1
		method.BodyEndLine = int(body.EndPoint().Row) + 1
		method.Calls = uniqueStringsInsert(walkCCalls(body, w.src))
	}
	w.methods = append(w.methods, method)
}

// findFunctionDeclarator walks the declarator chain from
// `decl` down through pointer_declarator /
// parenthesized_declarator wrappers and returns the innermost
// `function_declarator`. Returns nil if the chain does not
// terminate in a function declarator (e.g. a plain variable
// declaration).
//
// The unwrap is anchored to the `declarator` field at every
// step rather than walking arbitrary named children -- this
// prevents the search from descending into parameter lists
// (which themselves contain `function_declarator` nodes for
// function-pointer parameters, per rubber-duck #3).
func findFunctionDeclarator(decl *sitter.Node) *sitter.Node {
	cur := decl
	for cur != nil {
		switch cur.Type() {
		case cNodeFunctionDeclarator:
			return cur
		case cNodePointerDeclarator, cNodeParenDeclarator:
			cur = cur.ChildByFieldName("declarator")
		default:
			return nil
		}
	}
	return nil
}

// extractDeclaratorName returns the bare identifier the
// function_declarator binds. The inner `declarator` field is
// usually an `identifier` directly, but pointer chains and
// parenthesized wrappers may interpose. We unwrap defensively
// until we reach an identifier leaf.
func extractDeclaratorName(fnDecl *sitter.Node, src []byte) string {
	cur := fnDecl.ChildByFieldName("declarator")
	for cur != nil {
		switch cur.Type() {
		case cNodeIdentifier, cNodeFieldIdentifier, cNodeTypeIdentifier:
			return cur.Content(src)
		case cNodePointerDeclarator, cNodeParenDeclarator:
			cur = cur.ChildByFieldName("declarator")
		case cNodeFunctionDeclarator:
			// Nested function declarator -- function-pointer
			// returning function. Recurse one more level.
			cur = cur.ChildByFieldName("declarator")
		default:
			return ""
		}
	}
	return ""
}

// collectCModifiers returns the spec-allowed modifier tokens
// (`static`, `inline`, `extern`, `const`) appearing before the
// function definition's declarator. Only direct children of
// the function_definition node are inspected -- body-local
// declarations whose own modifiers would otherwise leak into
// the method's modifier list are excluded (per rubber-duck #2).
//
// Per tech-spec Section 5.1, only the four listed tokens are
// surfaced; other storage_class_specifier values (`auto`,
// `register`) and type_qualifier values (`volatile`,
// `restrict`) are dropped.
func collectCModifiers(n *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < n.ChildCount(); i++ {
		c := n.Child(int(i))
		if c == nil {
			continue
		}
		if c.Type() != cNodeStorageClass && c.Type() != cNodeTypeQualifier {
			continue
		}
		tok := strings.TrimSpace(c.Content(src))
		switch tok {
		case "static", "inline", "extern", "const":
			out = append(out, tok)
		}
	}
	return out
}

// cStripBraceSpan extracts the interior of a tree-sitter
// `compound_statement` (`{ ... }`) and returns the inner
// source alongside the interior byte offsets. The semantics
// mirror parser_treesitter.go::tsStripBraceSpan -- the
// scanner fallback (when one exists) and the tree-sitter
// back-end must agree on body byte spans so block subdivision
// (CountLogicalLines / DefaultBlockThreshold) is stable
// across CGO=on and CGO=off.
//
// Returns (content, startByte, endByte) verbatim on a body
// that does not look like `{...}` -- defensive against
// recovered ERROR trees the grammar may produce for malformed
// input.
func cStripBraceSpan(src []byte, body *sitter.Node) (string, int, int) {
	startByte := int(body.StartByte())
	endByte := int(body.EndByte())
	content := body.Content(src)
	if len(content) < 2 || content[0] != '{' || content[len(content)-1] != '}' {
		return content, startByte, endByte
	}
	inner := content[1 : len(content)-1]
	return inner, startByte + 1, endByte - 2
}

// walkCCalls visits every call_expression under body and
// records bare-name callees -- `foo(x)` where the function
// field is a plain identifier. Function-pointer calls
// (`(*cb)(x)`) and member-access calls (`obj->method(x)`,
// `obj.method(x)`) are skipped because the target name is
// not resolvable at parse time (tech-spec Section 5.1).
func walkCCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != cNodeCallExpression {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn != nil && fn.Type() == cNodeIdentifier {
			out = append(out, fn.Content(src))
		}
		return true
	})
	return out
}

// handleInclude emits one Import per `preproc_include`
// directive. The `path` field is either a `system_lib_string`
// (`<stdio.h>`) for system includes or a `string_literal`
// (`"local.h"`) for local includes.
//
// System path text is the bracketed form verbatim with the
// surrounding `<` and `>` stripped -- Module=`stdio.h` for
// `#include <stdio.h>`.
//
// Local path text is the quoted form stripped of `"` and
// prefixed with `./` so the dispatcher's `isRelativeImport`
// filter drops the edge (architecture Section 4.3). A path
// that already begins with `.` or `/` is left as-is to avoid
// `././local.h` doubling (per rubber-duck #1).
func (w *cWalker) handleInclude(n *sitter.Node) {
	pathNode := n.ChildByFieldName("path")
	if pathNode == nil {
		// Some grammar revisions don't tag the path field --
		// fall back to the first named child of the include
		// directive that is either a system_lib_string or a
		// string_literal.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c.Type() == cNodeSystemLibString || c.Type() == cNodeStringLiteral {
				pathNode = c
				break
			}
		}
	}
	if pathNode == nil {
		return
	}
	raw := pathNode.Content(w.src)
	module := ""
	switch pathNode.Type() {
	case cNodeSystemLibString:
		module = strings.Trim(raw, "<>")
	case cNodeStringLiteral:
		stripped := strings.Trim(raw, `"`)
		if stripped == "" {
			return
		}
		if strings.HasPrefix(stripped, ".") || strings.HasPrefix(stripped, "/") {
			module = stripped
		} else {
			module = "./" + stripped
		}
	default:
		return
	}
	if module == "" {
		return
	}
	w.imports = append(w.imports, Import{
		Module: module,
		Line:   int(n.StartPoint().Row) + 1,
	})
}
