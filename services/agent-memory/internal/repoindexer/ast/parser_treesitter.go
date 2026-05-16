//go:build cgo

// Package ast tree-sitter parser core.
//
// This file implements the LanguageParser interface on top of
// the smacker/go-tree-sitter bindings, satisfying the
// implementation-plan §3.2 mandate that the parser core ride
// real tree-sitter grammars (NOT regex scanners). The CGO=0
// scanner parsers in parser_typescript.go / parser_python.go
// remain available as a fallback for environments where CGO
// is unavailable -- see parsers_nocgo.go for that selection.
//
// Design choices:
//
//   - Each language has a dedicated implementation that walks
//     the syntax tree with tree-sitter's TreeCursor API.
//     Cursor walks are zero-allocation per step (the underlying
//     traversal mutates one C-allocated cursor) so they're the
//     right primitive for whole-file extraction.
//   - The ParseResult contract is identical between the scanner
//     and tree-sitter back-ends; the dispatcher and downstream
//     consumers see no behavioural difference at runtime.
//   - All grammar-specific node-type strings (`class_definition`
//     for Python, `class_declaration` for TS, ...) live in
//     this file as named constants so future grammar bumps
//     have one place to look.

package ast

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// NewTreeSitterTypeScriptParser returns a LanguageParser that
// uses the upstream tree-sitter TypeScript family of grammars.
// The returned instance claims `.ts`, `.tsx`, `.js`, `.jsx`,
// `.mjs`, `.cjs`; at parse time it routes `.tsx` / `.jsx`
// files through the upstream `tsx` grammar (which accepts JSX
// element syntax) and every other extension through the
// non-JSX `typescript` grammar. Both grammars share the same
// node-type vocabulary so the walker code does not branch on
// the grammar choice.
func NewTreeSitterTypeScriptParser() LanguageParser {
	return tsTreeSitterParser{}
}

// NewTreeSitterPythonParser returns a LanguageParser that uses
// the upstream tree-sitter Python grammar (covers `.py` and
// `.pyi`).
func NewTreeSitterPythonParser() LanguageParser {
	return pyTreeSitterParser{}
}

// =====================================================================
// TypeScript / JavaScript tree-sitter implementation
// =====================================================================

type tsTreeSitterParser struct{}

func (tsTreeSitterParser) Language() string { return "typescript" }
func (tsTreeSitterParser) Extensions() []string {
	return []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"}
}

func (tsTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, tsGrammarFor(relPath))
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter ts parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	w := tsWalker{src: src}
	w.walkTop(root)
	return ParseResult{
		Classes: w.classes,
		Methods: w.methods,
		Imports: w.imports,
	}, nil
}

// tsGrammarFor selects the upstream tree-sitter grammar for a
// TypeScript-family source file. The `typescript` grammar
// rejects JSX expression syntax, so files written in TSX or
// JSX must be routed to the `tsx` grammar even though both
// claim the same `.ts` family extension set (per evaluator
// finding #5 -- `.tsx`/`.jsx` were previously parsed with the
// non-JSX grammar and produced ERROR nodes on any JSX
// element). The grammar is a property of the file's
// surface-level syntax, not its semantic content, so the
// extension is the right signal.
func tsGrammarFor(relPath string) *sitter.Language {
	switch strings.ToLower(path.Ext(relPath)) {
	case ".tsx", ".jsx":
		return tsx.GetLanguage()
	default:
		return typescript.GetLanguage()
	}
}

const (
	tsNodeProgram           = "program"
	tsNodeClassDecl         = "class_declaration"
	tsNodeAbstractClass     = "abstract_class_declaration"
	tsNodeInterfaceDecl     = "interface_declaration"
	tsNodeFunctionDecl      = "function_declaration"
	tsNodeMethodDef         = "method_definition"
	tsNodeMethodSignature   = "method_signature"
	tsNodeClassBody         = "class_body"
	tsNodeInterfaceBody     = "interface_body"
	tsNodeObjectType        = "object_type"
	tsNodeImportStmt        = "import_statement"
	tsNodeExtendsClause     = "class_heritage"
	tsNodeImplementsClause  = "implements_clause"
	tsNodeExtendsTypeClause = "extends_type_clause"
	tsNodeStatementBlock    = "statement_block"
	tsNodeIdentifier        = "identifier"
	tsNodeTypeIdentifier    = "type_identifier"
	tsNodeFormalParameters  = "formal_parameters"
	tsNodeCallExpression    = "call_expression"
	tsNodeMemberExpression  = "member_expression"
	tsNodeAssignmentExpr    = "assignment_expression"
	tsNodeThis              = "this"
)

type tsWalker struct {
	src     []byte
	classes []ClassDecl
	methods []MethodDecl
	imports []Import
}

func (w *tsWalker) walkTop(root *sitter.Node) {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		child := root.NamedChild(int(i))
		w.visitTopLevel(child, "")
	}
}

func (w *tsWalker) visitTopLevel(n *sitter.Node, container string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case tsNodeClassDecl, tsNodeAbstractClass, tsNodeInterfaceDecl:
		w.handleClass(n, container)
	case tsNodeFunctionDecl:
		w.handleFreeFunction(n)
	case tsNodeImportStmt:
		w.handleImport(n)
	default:
		// Recurse so nested constructs (`export class`, etc.)
		// still produce nodes. tree-sitter wraps named decls
		// in `export_statement` / `lexical_declaration` nodes
		// which we descend into here.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			w.visitTopLevel(n.NamedChild(int(i)), container)
		}
	}
}

func (w *tsWalker) handleClass(n *sitter.Node, outer string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	qualified := name
	if outer != "" {
		qualified = outer + "." + name
	}
	kind := "class"
	if n.Type() == tsNodeInterfaceDecl {
		kind = "interface"
	}
	cls := ClassDecl{
		QualifiedName: qualified,
		Kind:          kind,
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
	}

	// `class_heritage` carries both `extends` and `implements`
	// children. Walk it and partition by inner node type.
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(int(i))
		switch child.Type() {
		case tsNodeExtendsClause:
			for j := uint32(0); j < child.NamedChildCount(); j++ {
				sub := child.NamedChild(int(j))
				switch sub.Type() {
				case tsNodeExtendsTypeClause:
					cls.Extends = append(cls.Extends, collectTSIdentifiers(sub, w.src)...)
				case tsNodeImplementsClause:
					cls.Implements = append(cls.Implements, collectTSIdentifiers(sub, w.src)...)
				}
			}
		case tsNodeExtendsTypeClause:
			cls.Extends = append(cls.Extends, collectTSIdentifiers(child, w.src)...)
		case tsNodeImplementsClause:
			cls.Implements = append(cls.Implements, collectTSIdentifiers(child, w.src)...)
		}
	}

	body := n.ChildByFieldName("body")
	if body == nil {
		// interface_declaration body field is named the same
		// but the inner block is `object_type`; fall through
		// is fine because the search below scans by type.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c.Type() == tsNodeClassBody || c.Type() == tsNodeInterfaceBody || c.Type() == tsNodeObjectType {
				body = c
				break
			}
		}
	}
	w.classes = append(w.classes, cls)

	if body != nil {
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			member := body.NamedChild(int(i))
			switch member.Type() {
			case tsNodeMethodDef, tsNodeMethodSignature:
				w.handleMethod(member, qualified)
			case tsNodeClassDecl, tsNodeAbstractClass, tsNodeInterfaceDecl:
				w.handleClass(member, qualified)
			}
		}
	}
}

func (w *tsWalker) handleMethod(n *sitter.Node, enclosing string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	qualified := name
	if enclosing != "" {
		qualified = enclosing + "." + name
	}
	params := ""
	if p := n.ChildByFieldName("parameters"); p != nil {
		params = trimParens(p.Content(w.src))
	}
	body := n.ChildByFieldName("body")
	method := MethodDecl{
		QualifiedName:  qualified,
		EnclosingClass: enclosing,
		ParamSignature: params,
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
		Modifiers:      collectTSModifiers(n, w.src),
	}
	if body != nil {
		// The tree-sitter `statement_block` node spans
		// `{ ... }` INCLUSIVELY -- to match the scanner
		// fallback (and to keep `CountLogicalLines`
		// consistent across CGO=on and CGO=off paths so a
		// 79-line method does not cross the 80-line block
		// threshold purely because of brace inflation), we
		// strip the outer braces. BodyStartByte / EndByte
		// likewise point at the FIRST / LAST interior byte
		// (skipping `{` and `}`); BodyStartLine /
		// BodyEndLine stay on the `{` / `}` lines because
		// span ingestor stack frames report those when an
		// exception is thrown on the entry / exit lines.
		method.BodySource, method.BodyStartByte, method.BodyEndByte =
			tsStripBraceSpan(w.src, body)
		method.BodyStartLine = int(body.StartPoint().Row) + 1
		method.BodyEndLine = int(body.EndPoint().Row) + 1
		method.Calls = uniqueStringsInsert(walkTSCalls(body, w.src))
		method.ReceiverCalls = uniqueStringsInsert(walkTSReceiverCalls(body, w.src))
		method.MemberAccesses = walkTSMemberAccesses(body, w.src)
	}
	w.methods = append(w.methods, method)
}

func (w *tsWalker) handleFreeFunction(n *sitter.Node) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	params := ""
	if p := n.ChildByFieldName("parameters"); p != nil {
		params = trimParens(p.Content(w.src))
	}
	body := n.ChildByFieldName("body")
	method := MethodDecl{
		QualifiedName:  name,
		ParamSignature: params,
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
		Modifiers:      collectTSModifiers(n, w.src),
	}
	if body != nil {
		// See `handleMethod` -- strip outer `{` / `}` to
		// match scanner semantics.
		method.BodySource, method.BodyStartByte, method.BodyEndByte =
			tsStripBraceSpan(w.src, body)
		method.BodyStartLine = int(body.StartPoint().Row) + 1
		method.BodyEndLine = int(body.EndPoint().Row) + 1
		method.Calls = uniqueStringsInsert(walkTSCalls(body, w.src))
		// Free functions have no receiver, so receiver-calls
		// and member-accesses are skipped (the dispatcher
		// would not resolve them anyway -- it gates on
		// EnclosingClass != "").
	}
	w.methods = append(w.methods, method)
}

func (w *tsWalker) handleImport(n *sitter.Node) {
	source := n.ChildByFieldName("source")
	if source == nil {
		// Fallback: scan children for the first string node.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c.Type() == "string" {
				source = c
				break
			}
		}
	}
	if source == nil {
		return
	}
	module := strings.Trim(source.Content(w.src), `"' `+"`")
	imp := Import{
		Module: module,
		Line:   int(n.StartPoint().Row) + 1,
	}
	// `import type ... from ...` puts the literal text `type`
	// as the second child of import_statement.
	if n.ChildCount() >= 2 {
		second := n.Child(1)
		if second != nil && second.Type() == "type" {
			imp.IsTypeOnly = true
		}
	}
	// Symbols / alias: walk the import_clause subtree.
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c.Type() != "import_clause" {
			continue
		}
		for j := uint32(0); j < c.NamedChildCount(); j++ {
			cc := c.NamedChild(int(j))
			switch cc.Type() {
			case "namespace_import":
				if id := cc.ChildByFieldName("name"); id != nil {
					imp.Alias = id.Content(w.src)
				}
			case "named_imports":
				for k := uint32(0); k < cc.NamedChildCount(); k++ {
					spec := cc.NamedChild(int(k))
					if spec.Type() == "import_specifier" {
						if nm := spec.ChildByFieldName("name"); nm != nil {
							imp.Symbols = append(imp.Symbols, nm.Content(w.src))
						}
					}
				}
			case tsNodeIdentifier:
				imp.Alias = cc.Content(w.src)
			}
		}
	}
	w.imports = append(w.imports, imp)
}

// collectTSIdentifiers returns the type-identifier leaves
// reachable from n via a depth-first walk. Used to extract
// the named targets of `extends` / `implements` clauses
// without making assumptions about the exact wrapper-node
// shape (which varies between TS class_heritage and
// interface extends_type_clause).
func collectTSIdentifiers(n *sitter.Node, src []byte) []string {
	var out []string
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		if node == nil {
			return
		}
		switch node.Type() {
		case tsNodeTypeIdentifier, tsNodeIdentifier:
			out = append(out, node.Content(src))
			return
		}
		for i := uint32(0); i < node.NamedChildCount(); i++ {
			walk(node.NamedChild(int(i)))
		}
	}
	walk(n)
	return out
}

// collectTSModifiers returns the source text of every leaf
// modifier (`async`, `static`, `public`, `private`,
// `readonly`, `abstract`, `export`) appearing before the
// method's name. Order is source order.
func collectTSModifiers(n *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < n.ChildCount(); i++ {
		c := n.Child(int(i))
		switch c.Type() {
		case "async", "static", "public", "private", "protected",
			"readonly", "abstract", "override", "export":
			out = append(out, c.Type())
		case "accessibility_modifier":
			out = append(out, c.Content(src))
		}
	}
	return out
}

// walkTSCalls visits every call_expression under body and
// records the bare-name callee. Receiver-qualified calls
// (`this.foo()`, `obj.foo()`) are excluded -- they have a
// dedicated walker (walkTSReceiverCalls) for `this.X(`, and
// arbitrary-object calls remain out of scope until the
// cross-file resolver story.
func walkTSCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != tsNodeCallExpression {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil {
			return true
		}
		if fn.Type() == tsNodeIdentifier {
			out = append(out, fn.Content(src))
		}
		return true
	})
	return out
}

// walkTSReceiverCalls extracts `this.<name>(` call targets.
// The dispatcher resolves these against the enclosing class
// without ambiguity (per evaluator finding #5).
func walkTSReceiverCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != tsNodeCallExpression {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil || fn.Type() != tsNodeMemberExpression {
			return true
		}
		obj := fn.ChildByFieldName("object")
		prop := fn.ChildByFieldName("property")
		if obj == nil || prop == nil {
			return true
		}
		if obj.Type() == tsNodeThis && prop.Type() == "property_identifier" {
			out = append(out, prop.Content(src))
		}
		return true
	})
	return out
}

// walkTSMemberAccesses returns the per-method list of
// `this.<name>` references with IsWrite set when the access
// appears on the LHS of an assignment. Each name is recorded
// once with IsWrite=true winning over IsWrite=false (a field
// that is both read and written counts as a write because
// the write is the more semantically significant event for
// dependency tracking).
func walkTSMemberAccesses(body *sitter.Node, src []byte) []MemberAccess {
	writes := map[string]struct{}{}
	reads := []string{}
	seen := map[string]bool{}
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() == tsNodeAssignmentExpr {
			lhs := node.ChildByFieldName("left")
			if name, ok := isThisMember(lhs, src); ok {
				writes[name] = struct{}{}
				if !seen[name] {
					seen[name] = true
					reads = append(reads, name)
				}
				return true
			}
		}
		if node.Type() == tsNodeMemberExpression {
			if name, ok := isThisMember(node, src); ok {
				if !seen[name] {
					seen[name] = true
					reads = append(reads, name)
				}
			}
		}
		return true
	})
	out := make([]MemberAccess, 0, len(reads))
	sort.Strings(reads)
	for _, name := range reads {
		_, isW := writes[name]
		out = append(out, MemberAccess{Name: name, IsWrite: isW})
	}
	return out
}

func isThisMember(n *sitter.Node, src []byte) (string, bool) {
	if n == nil || n.Type() != tsNodeMemberExpression {
		return "", false
	}
	obj := n.ChildByFieldName("object")
	prop := n.ChildByFieldName("property")
	if obj == nil || prop == nil {
		return "", false
	}
	if obj.Type() != tsNodeThis {
		return "", false
	}
	if prop.Type() != "property_identifier" {
		return "", false
	}
	return prop.Content(src), true
}

// walkChildren applies fn to every descendant of root in
// document order. Returning false from fn stops the descent
// into that subtree; the walk continues with siblings.
func walkChildren(root *sitter.Node, fn func(*sitter.Node) bool) {
	if root == nil {
		return
	}
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		c := root.NamedChild(int(i))
		if c == nil {
			continue
		}
		if !fn(c) {
			continue
		}
		walkChildren(c, fn)
	}
}

func trimParens(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '(' && s[len(s)-1] == ')' {
		return s[1 : len(s)-1]
	}
	return s
}

// tsStripBraceSpan extracts the interior of a tree-sitter
// `statement_block` node (the `{ ... }` span of a TS/JS
// method or function body) and returns the inner source
// alongside the interior byte offsets. The tree-sitter node
// spans the braces inclusively; the scanner fallback in
// parser_typescript.go has always returned the brace-stripped
// form, so the CGO=on and CGO=off paths must agree to keep
// `CountLogicalLines` and the §8.2 block threshold stable
// across build modes (per evaluator finding #2 -- a 79-line
// body should never accidentally trip the 80-line threshold
// purely because the tree-sitter node included `{` and `}`).
//
// Byte-offset semantics: tree-sitter `EndByte()` is the byte
// AFTER the last byte of the node (exclusive), so for a body
// `{abc}` with StartByte=0 we get EndByte=5. The scanner
// records BodyEndByte = index-of-last-interior-byte = 2 (the
// `c`). To match that, we return `endByte - 2` for the end
// offset and `startByte + 1` for the start offset. The
// scanner returns those as the indices of the first and last
// interior bytes -- consistent with the brace-stripped
// BodySource.
//
// Returns ("", startByte, endByte) when the body is too short
// to have an interior (defensive; tree-sitter never produces
// such a node, but the helper stays total to avoid surprises).
func tsStripBraceSpan(src []byte, body *sitter.Node) (string, int, int) {
	startByte := int(body.StartByte())
	endByte := int(body.EndByte())
	content := body.Content(src)
	// Defensive guards: if the node does not actually start
	// with `{` and end with `}` (malformed syntax that
	// tree-sitter recovered from with ERROR nodes), fall
	// back to the raw content so callers still see SOMETHING.
	if len(content) < 2 || content[0] != '{' || content[len(content)-1] != '}' {
		return content, startByte, endByte
	}
	inner := content[1 : len(content)-1]
	// startByte + 1 skips the `{`; endByte - 2 skips the
	// `}` AND backs off the exclusive EndByte by one (so
	// the result is the inclusive index of the last
	// interior byte, matching the scanner's bodyClose - 1).
	return inner, startByte + 1, endByte - 2
}

// uniqueStringsInsert returns in with duplicates removed,
// preserving first-occurrence order.
func uniqueStringsInsert(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// =====================================================================
// Python tree-sitter implementation
// =====================================================================

type pyTreeSitterParser struct{}

func (pyTreeSitterParser) Language() string     { return "python" }
func (pyTreeSitterParser) Extensions() []string { return []string{".py", ".pyi"} }

func (pyTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, python.GetLanguage())
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter py parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	w := pyWalker{src: src}
	w.walkTop(root)
	return ParseResult{
		Classes: w.classes,
		Methods: w.methods,
		Imports: w.imports,
	}, nil
}

const (
	pyNodeModule        = "module"
	pyNodeClassDef      = "class_definition"
	pyNodeFunctionDef   = "function_definition"
	pyNodeDecoratedDef  = "decorated_definition"
	pyNodeImport        = "import_statement"
	pyNodeImportFrom    = "import_from_statement"
	pyNodeBlock         = "block"
	pyNodeIdentifier    = "identifier"
	pyNodeAttribute     = "attribute"
	pyNodeCall          = "call"
	pyNodeAssignment    = "assignment"
	pyNodeDottedName    = "dotted_name"
	pyNodeArgumentList  = "argument_list"
	pyNodeAliasedImport = "aliased_import"
)

type pyWalker struct {
	src     []byte
	classes []ClassDecl
	methods []MethodDecl
	imports []Import
}

func (w *pyWalker) walkTop(root *sitter.Node) {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.visitTopLevel(root.NamedChild(int(i)), "")
	}
}

func (w *pyWalker) visitTopLevel(n *sitter.Node, container string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case pyNodeClassDef:
		w.handleClass(n, container)
	case pyNodeFunctionDef:
		w.handleFreeFunction(n)
	case pyNodeDecoratedDef:
		// Skip past the decorators to the wrapped definition.
		def := n.ChildByFieldName("definition")
		if def != nil {
			w.visitTopLevel(def, container)
		}
	case pyNodeImport, pyNodeImportFrom:
		w.handleImport(n)
	}
}

func (w *pyWalker) handleClass(n *sitter.Node, outer string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	qualified := name
	if outer != "" {
		qualified = outer + "." + name
	}
	cls := ClassDecl{
		QualifiedName: qualified,
		Kind:          "class",
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
	}
	if sup := n.ChildByFieldName("superclasses"); sup != nil {
		for i := uint32(0); i < sup.NamedChildCount(); i++ {
			c := sup.NamedChild(int(i))
			if c.Type() == pyNodeIdentifier || c.Type() == pyNodeAttribute {
				cls.Extends = append(cls.Extends, c.Content(w.src))
			}
		}
	}
	w.classes = append(w.classes, cls)

	if body := n.ChildByFieldName("body"); body != nil {
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			member := body.NamedChild(int(i))
			switch member.Type() {
			case pyNodeFunctionDef:
				w.handleMethod(member, qualified)
			case pyNodeDecoratedDef:
				if def := member.ChildByFieldName("definition"); def != nil && def.Type() == pyNodeFunctionDef {
					w.handleMethod(def, qualified)
				}
			case pyNodeClassDef:
				w.handleClass(member, qualified)
			}
		}
	}
}

func (w *pyWalker) handleMethod(n *sitter.Node, enclosing string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	qualified := name
	if enclosing != "" {
		qualified = enclosing + "." + name
	}
	params := ""
	if p := n.ChildByFieldName("parameters"); p != nil {
		params = trimParens(p.Content(w.src))
	}
	body := n.ChildByFieldName("body")
	method := MethodDecl{
		QualifiedName:  qualified,
		EnclosingClass: enclosing,
		ParamSignature: params,
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
	}
	if body != nil {
		method.BodySource = body.Content(w.src)
		method.BodyStartLine = int(body.StartPoint().Row) + 1
		method.BodyEndLine = int(body.EndPoint().Row) + 1
		method.BodyStartByte = int(body.StartByte())
		method.BodyEndByte = int(body.EndByte())
		method.Calls = uniqueStringsInsert(walkPyCalls(body, w.src))
		method.ReceiverCalls = uniqueStringsInsert(walkPySelfCalls(body, w.src))
		method.MemberAccesses = walkPySelfAccesses(body, w.src)
	}
	w.methods = append(w.methods, method)
}

func (w *pyWalker) handleFreeFunction(n *sitter.Node) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	params := ""
	if p := n.ChildByFieldName("parameters"); p != nil {
		params = trimParens(p.Content(w.src))
	}
	body := n.ChildByFieldName("body")
	method := MethodDecl{
		QualifiedName:  name,
		ParamSignature: params,
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
	}
	if body != nil {
		method.BodySource = body.Content(w.src)
		method.BodyStartLine = int(body.StartPoint().Row) + 1
		method.BodyEndLine = int(body.EndPoint().Row) + 1
		method.BodyStartByte = int(body.StartByte())
		method.BodyEndByte = int(body.EndByte())
		method.Calls = uniqueStringsInsert(walkPyCalls(body, w.src))
	}
	w.methods = append(w.methods, method)
}

func (w *pyWalker) handleImport(n *sitter.Node) {
	switch n.Type() {
	case pyNodeImport:
		// `import a, b as c` -- iterate the name children.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			imp := Import{Line: int(n.StartPoint().Row) + 1}
			switch c.Type() {
			case pyNodeDottedName, pyNodeIdentifier:
				imp.Module = c.Content(w.src)
			case pyNodeAliasedImport:
				if nm := c.ChildByFieldName("name"); nm != nil {
					imp.Module = nm.Content(w.src)
				}
				if al := c.ChildByFieldName("alias"); al != nil {
					imp.Alias = al.Content(w.src)
				}
			default:
				continue
			}
			if imp.Module != "" {
				w.imports = append(w.imports, imp)
			}
		}
	case pyNodeImportFrom:
		modNode := n.ChildByFieldName("module_name")
		module := ""
		if modNode != nil {
			module = modNode.Content(w.src)
		} else {
			// Pure relative import (`from . import x`):
			// tree-sitter exposes the dots as anonymous
			// children. Reconstruct from the source range.
			module = strings.TrimSpace(string(w.src[n.StartByte():n.EndByte()]))
			if idx := strings.Index(module, " import"); idx > 0 {
				module = strings.TrimSpace(module[len("from "):idx])
			}
		}
		imp := Import{
			Module: module,
			Line:   int(n.StartPoint().Row) + 1,
		}
		// Each `name:` field gives one imported symbol; with
		// aliases we get a parent `aliased_import` instead.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c == modNode {
				continue
			}
			switch c.Type() {
			case pyNodeIdentifier, pyNodeDottedName:
				imp.Symbols = append(imp.Symbols, c.Content(w.src))
			case pyNodeAliasedImport:
				if nm := c.ChildByFieldName("name"); nm != nil {
					imp.Symbols = append(imp.Symbols, nm.Content(w.src))
				}
			}
		}
		w.imports = append(w.imports, imp)
	}
}

// walkPyCalls visits every `call` under body and records the
// bare-name callee. Calls that go through an attribute access
// (`obj.method()`) are excluded -- `self.method()` is captured
// separately by walkPySelfCalls (per evaluator finding #5).
func walkPyCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != pyNodeCall {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil {
			return true
		}
		if fn.Type() == pyNodeIdentifier {
			out = append(out, fn.Content(src))
		}
		return true
	})
	return out
}

// walkPySelfCalls extracts `self.<name>(` call targets so the
// dispatcher can emit unambiguous receiver-resolved
// static_calls edges.
func walkPySelfCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != pyNodeCall {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil || fn.Type() != pyNodeAttribute {
			return true
		}
		obj := fn.ChildByFieldName("object")
		attr := fn.ChildByFieldName("attribute")
		if obj == nil || attr == nil {
			return true
		}
		if obj.Type() == pyNodeIdentifier && obj.Content(src) == "self" {
			out = append(out, attr.Content(src))
		}
		return true
	})
	return out
}

// walkPySelfAccesses returns the per-method list of
// `self.<name>` references with IsWrite set when the access
// appears on the LHS of an assignment. The classification
// rule mirrors walkTSMemberAccesses.
func walkPySelfAccesses(body *sitter.Node, src []byte) []MemberAccess {
	writes := map[string]struct{}{}
	seen := map[string]bool{}
	var reads []string
	walkChildren(body, func(node *sitter.Node) bool {
		switch node.Type() {
		case pyNodeAssignment:
			lhs := node.ChildByFieldName("left")
			if name, ok := isPySelfMember(lhs, src); ok {
				writes[name] = struct{}{}
				if !seen[name] {
					seen[name] = true
					reads = append(reads, name)
				}
				return true
			}
		case pyNodeAttribute:
			if name, ok := isPySelfMember(node, src); ok {
				if !seen[name] {
					seen[name] = true
					reads = append(reads, name)
				}
			}
		}
		return true
	})
	sort.Strings(reads)
	out := make([]MemberAccess, 0, len(reads))
	for _, name := range reads {
		_, isW := writes[name]
		out = append(out, MemberAccess{Name: name, IsWrite: isW})
	}
	return out
}

func isPySelfMember(n *sitter.Node, src []byte) (string, bool) {
	if n == nil || n.Type() != pyNodeAttribute {
		return "", false
	}
	obj := n.ChildByFieldName("object")
	attr := n.ChildByFieldName("attribute")
	if obj == nil || attr == nil {
		return "", false
	}
	if obj.Type() != pyNodeIdentifier || obj.Content(src) != "self" {
		return "", false
	}
	return attr.Content(src), true
}
