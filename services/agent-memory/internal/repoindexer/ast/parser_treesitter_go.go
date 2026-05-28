//go:build cgo

// Go tree-sitter parser.
//
// Walks the upstream `github.com/smacker/go-tree-sitter/golang`
// grammar to emit the same `ParseResult` shape the TS/JS and
// Python tree-sitter implementations produce in
// parser_treesitter.go. Implementation-plan §3.2 calls out
// tree-sitter as the canonical parser core for every supported
// language; this file extends that contract to `.go` source.
//
// Mapping (architecture Section 4.4.3 + tech-spec Section 5.4):
//
//   - `type_declaration` -> `type_spec` with `struct_type`
//     body -> ClassDecl{Kind: "struct", LangMeta: {"embeds":
//     [...]}} where `embeds` is the list of embedded type
//     names extracted from anonymous field_declarations (the
//     field_declaration has a `type` child but no
//     field_identifier "name" children).
//   - `type_declaration` -> `type_spec` with `interface_type`
//     body -> ClassDecl{Kind: "interface", LangMeta:
//     {"embeds": [...]}} where `embeds` is the list of
//     embedded interface names -- direct type_identifier /
//     qualified_type children of the interface body (anything
//     that is NOT a method_spec).
//   - `type_declaration` -> `type_alias` (the `type T = U`
//     syntax) OR a `type_spec` whose body is not a struct or
//     interface (e.g. `type Celsius float64`) ->
//     ClassDecl{Kind: "type_alias"}.
//   - `function_declaration` -> MethodDecl{EnclosingClass: ""}
//     (free function).
//   - `method_declaration` -> MethodDecl with the receiver
//     type as `EnclosingClass`. Pointer-receiver methods get
//     the operator-pinned `*` prefix on `QualifiedName` per
//     architecture Section 4.5 AND a ReceiverAliases entry
//     `EnclosingClass + "." + name` so sibling receiver-call
//     sites `r.Bar()` resolve through the alias when no
//     value-receiver collision exists. `EnclosingClass`
//     itself is always the BARE class name (no `*`); the
//     alias formula in parser.go::MethodDecl gates on that.
//     LangMeta records `receiver` (the receiver variable
//     name, e.g. "r"), `receiver_ptr` (bool, true for `*Foo`
//     receivers), and `receiver_type` (the receiver type
//     name).
//   - `import_declaration` -> one Import per `import_spec`
//     child, regardless of whether the parent is a single
//     `import "x"` or a grouped `import ( "x"; "y" )` form.
//     `import . "fmt"` (dot import) and `import _ "fmt"`
//     (blank import) carry `dot_import` / `blank_import`
//     LangMeta flags per parser.go::Import.
//
// Call walking inside method bodies:
//   - bare identifier calls `Foo()` -> Calls.
//   - selector calls whose operand is the receiver variable
//     `r.Bar()` -> ReceiverCalls; non-receiver selector calls
//     like `pkg.Func()` are dropped (left to the cross-file
//     resolver story).
//   - selector field accesses `r.field` -> MemberAccesses,
//     with `IsWrite` set when the access appears on the LHS
//     of an `assignment_statement`. Plural assignments
//     (`a.x, a.y = 1, 2`) are handled by walking the LHS
//     expression list.

package ast

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
)

// NewTreeSitterGoParser returns a LanguageParser that uses
// the upstream tree-sitter Go grammar (covers `.go`).
func NewTreeSitterGoParser() LanguageParser {
	return goTreeSitterParser{}
}

// Go grammar node-type constants. Mirror the
// parser_treesitter.go pattern of declaring every literal
// node name as a named const at the top of the file so a
// future grammar bump has one place to look.
const (
	goNodeSourceFile      = "source_file"
	goNodePackageClause   = "package_clause"
	goNodeImportDecl      = "import_declaration"
	goNodeImportSpecList  = "import_spec_list"
	goNodeImportSpec      = "import_spec"
	goNodeTypeDecl        = "type_declaration"
	goNodeTypeSpec        = "type_spec"
	goNodeTypeAlias       = "type_alias"
	goNodeStructType      = "struct_type"
	goNodeInterfaceType   = "interface_type"
	goNodeFieldDeclList   = "field_declaration_list"
	goNodeFieldDecl       = "field_declaration"
	goNodeFieldIdentifier = "field_identifier"
	// goNodeMethodElem is the modern smacker / upstream Go
	// grammar node name for an interface method declaration
	// (e.g. `Foo()` inside `interface { Foo() }`). The pinned
	// `github.com/smacker/go-tree-sitter` revision
	// `v0.0.0-20240827094217-dd81d9e9be82` emits `method_elem`,
	// NOT the legacy `method_spec` symbol (which is absent from
	// the grammar at that revision -- verified by `grep -c
	// method_spec parser.c` returning 0). `goNodeMethodSpec`
	// is retained as a fall-back so older grammars continue to
	// work if the dependency is bumped backwards.
	goNodeMethodElem = "method_elem"
	goNodeMethodSpec = "method_spec"
	// goNodeTypeElem is the modern interface-body wrapper for
	// an embedded type or a type-set union (Go 1.18+ generics).
	// In the pinned grammar, embedded interfaces appear as a
	// `type_elem` child of `interface_type` containing a single
	// `type_identifier` / `qualified_type` / `pointer_type`
	// (architecture §4.4.3 `embeds` key). Type-set unions
	// (`int | float64`) produce a single `type_elem` with
	// multiple type children separated by `|`; we treat each
	// distinct type child as a separate embed entry so the
	// downstream consumer can still see the named constraints.
	goNodeTypeElem           = "type_elem"
	goNodeFunctionDecl       = "function_declaration"
	goNodeMethodDecl         = "method_declaration"
	goNodeParamList          = "parameter_list"
	goNodeParamDecl          = "parameter_declaration"
	goNodePointerType        = "pointer_type"
	goNodeQualifiedType      = "qualified_type"
	goNodeTypeIdentifier     = "type_identifier"
	goNodeIdentifier         = "identifier"
	goNodePackageIdentifier  = "package_identifier"
	goNodeSelectorExpression = "selector_expression"
	goNodeCallExpression     = "call_expression"
	goNodeInterpretedString  = "interpreted_string_literal"
	goNodeRawString          = "raw_string_literal"
	goNodeBlock              = "block"
	goNodeAssignment         = "assignment_statement"
	goNodeShortVarDecl       = "short_var_declaration"
	goNodeBlankIdentifier    = "blank_identifier"
	goNodeDot                = "dot"
	goNodeExpressionList     = "expression_list"
)

type goTreeSitterParser struct{}

func (goTreeSitterParser) Language() string     { return "go" }
func (goTreeSitterParser) Extensions() []string { return []string{".go"} }

func (goTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, golang.GetLanguage())
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter go parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	w := goWalker{src: src}
	w.walkTop(root)
	return ParseResult{
		Classes: w.classes,
		Methods: w.methods,
		Imports: w.imports,
	}, nil
}

type goWalker struct {
	src     []byte
	classes []ClassDecl
	methods []MethodDecl
	imports []Import
}

func (w *goWalker) walkTop(root *sitter.Node) {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.visitTopLevel(root.NamedChild(int(i)))
	}
}

func (w *goWalker) visitTopLevel(n *sitter.Node) {
	if n == nil {
		return
	}
	switch n.Type() {
	case goNodeTypeDecl:
		// A `type_declaration` can wrap one `type_spec` /
		// `type_alias` (single-line form) or many specs
		// inside a parenthesised block. Iterate all named
		// children for each spec / alias.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			spec := n.NamedChild(int(i))
			switch spec.Type() {
			case goNodeTypeSpec, goNodeTypeAlias:
				w.handleTypeSpec(spec)
			}
		}
	case goNodeFunctionDecl:
		w.handleFunctionDecl(n)
	case goNodeMethodDecl:
		w.handleMethodDecl(n)
	case goNodeImportDecl:
		w.handleImportDecl(n)
	}
}

// handleTypeSpec dispatches on the `type` child to classify
// the declaration as struct / interface / type_alias and
// records embedded type names (architecture Section 4.4.3
// `embeds` LangMeta key).
func (w *goWalker) handleTypeSpec(spec *sitter.Node) {
	nameNode := spec.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	typeNode := spec.ChildByFieldName("type")

	kind := "type_alias"
	var meta map[string]any

	switch {
	case spec.Type() == goNodeTypeAlias:
		// `type T = U` -- always a pure alias regardless
		// of what U is structurally.
		kind = "type_alias"
	case typeNode == nil:
		kind = "type_alias"
	case typeNode.Type() == goNodeStructType:
		kind = "struct"
		if embeds := goCollectStructEmbeds(typeNode, w.src); len(embeds) > 0 {
			meta = map[string]any{"embeds": embeds}
		}
	case typeNode.Type() == goNodeInterfaceType:
		kind = "interface"
		if embeds := goCollectInterfaceEmbeds(typeNode, w.src); len(embeds) > 0 {
			meta = map[string]any{"embeds": embeds}
		}
	default:
		// `type Celsius float64`, `type Handler func(...)`,
		// `type Stringer = fmt.Stringer` (already handled
		// above), `type ID [16]byte`, etc. -- all of these
		// are alias-like in graph terms.
		kind = "type_alias"
	}

	cls := ClassDecl{
		QualifiedName: name,
		Kind:          kind,
		StartLine:     int(spec.StartPoint().Row) + 1,
		EndLine:       int(spec.EndPoint().Row) + 1,
		LangMeta:      meta,
	}
	w.classes = append(w.classes, cls)

	// For interfaces, emit method declarations for each
	// `method_elem` (modern grammar) or `method_spec` (legacy)
	// child so the dispatcher can mint method nodes against
	// the interface (matches the TS/JS `interface_declaration`
	// path in parser_treesitter.go). Both node shapes expose
	// `name` and `parameters` fields with identical semantics,
	// so a single handler covers them.
	if kind == "interface" && typeNode != nil {
		for i := uint32(0); i < typeNode.NamedChildCount(); i++ {
			child := typeNode.NamedChild(int(i))
			if child == nil {
				continue
			}
			if child.Type() == goNodeMethodElem || child.Type() == goNodeMethodSpec {
				w.handleInterfaceMethodElem(child, name)
			}
		}
	}
}

func (w *goWalker) handleInterfaceMethodElem(n *sitter.Node, enclosing string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	qualified := enclosing + "." + name
	params := ""
	if p := n.ChildByFieldName("parameters"); p != nil {
		params = trimParens(p.Content(w.src))
	}
	w.methods = append(w.methods, MethodDecl{
		QualifiedName:  qualified,
		EnclosingClass: enclosing,
		ParamSignature: params,
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
	})
}

func (w *goWalker) handleFunctionDecl(n *sitter.Node) {
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
		// Go's `block` node spans `{ ... }` inclusively;
		// reuse the brace-stripping helper from
		// parser_treesitter.go so BodySource / byte offsets
		// match the brace-stripped semantics the rest of the
		// pipeline expects (per evaluator finding #2 the
		// dispatcher's logical-line counter is sensitive to
		// brace inflation).
		method.BodySource, method.BodyStartByte, method.BodyEndByte =
			tsStripBraceSpan(w.src, body)
		method.BodyStartLine = int(body.StartPoint().Row) + 1
		method.BodyEndLine = int(body.EndPoint().Row) + 1
		method.Calls = uniqueStringsInsert(walkGoBareCalls(body, w.src))
	}
	w.methods = append(w.methods, method)
}

func (w *goWalker) handleMethodDecl(n *sitter.Node) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	receiverNode := n.ChildByFieldName("receiver")
	recvType, recvPtr, recvName := goExtractReceiver(receiverNode, w.src)
	if recvType == "" {
		// Malformed receiver -- skip rather than emit a
		// half-built method node.
		return
	}
	// EnclosingClass is the BARE class name regardless of
	// pointer-ness; the alias formula
	// `EnclosingClass + "." + name` (parser.go::MethodDecl)
	// must produce the un-starred string. The `*` marker
	// lives on QualifiedName only.
	enclosing := recvType
	qualified := recvType + "." + name
	var aliases []string
	meta := map[string]any{"receiver_type": recvType}
	if recvName != "" {
		// Bound receiver variable name (`r` in `func (r Foo) Bar`).
		// Architecture Section 4.4.3 catalogues `receiver`
		// as the bound identifier so consumers can match
		// it against in-body `r.field` accesses.
		meta["receiver"] = recvName
	}
	if recvPtr {
		// Operator-pinned pointer-receiver marker per
		// architecture Section 4.5: the `*` prefix on
		// QualifiedName distinguishes pointer-receiver
		// methods from value-receiver methods that share
		// the same simple name. ReceiverAliases ensures
		// sibling value-receiver call sites
		// (`r.Bar()` where `r Foo`) still resolve to the
		// pointer-receiver Node when no value-receiver
		// collision exists (the dispatcher's Pass 2b
		// registers the Node id under both the primary
		// key and each alias).
		qualified = "*" + recvType + "." + name
		aliases = []string{recvType + "." + name}
		meta["receiver_ptr"] = true
	}
	params := ""
	if p := n.ChildByFieldName("parameters"); p != nil {
		params = trimParens(p.Content(w.src))
	}
	body := n.ChildByFieldName("body")
	method := MethodDecl{
		QualifiedName:   qualified,
		EnclosingClass:  enclosing,
		ParamSignature:  params,
		StartLine:       int(n.StartPoint().Row) + 1,
		EndLine:         int(n.EndPoint().Row) + 1,
		ReceiverAliases: aliases,
		LangMeta:        meta,
	}
	if body != nil {
		method.BodySource, method.BodyStartByte, method.BodyEndByte =
			tsStripBraceSpan(w.src, body)
		method.BodyStartLine = int(body.StartPoint().Row) + 1
		method.BodyEndLine = int(body.EndPoint().Row) + 1
		method.Calls = uniqueStringsInsert(walkGoBareCalls(body, w.src))
		if recvName != "" {
			method.ReceiverCalls = uniqueStringsInsert(walkGoReceiverCalls(body, recvName, w.src))
			method.MemberAccesses = walkGoReceiverMemberAccesses(body, recvName, w.src)
		}
	}
	w.methods = append(w.methods, method)
}

func (w *goWalker) handleImportDecl(n *sitter.Node) {
	// `import_declaration` body is either a single
	// `import_spec` or an `import_spec_list` with N
	// `import_spec` children.
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		switch c.Type() {
		case goNodeImportSpec:
			w.recordImportSpec(c)
		case goNodeImportSpecList:
			for j := uint32(0); j < c.NamedChildCount(); j++ {
				spec := c.NamedChild(int(j))
				if spec.Type() == goNodeImportSpec {
					w.recordImportSpec(spec)
				}
			}
		}
	}
}

func (w *goWalker) recordImportSpec(spec *sitter.Node) {
	pathNode := spec.ChildByFieldName("path")
	if pathNode == nil {
		return
	}
	module := goUnquoteString(pathNode.Content(w.src))
	imp := Import{
		Module: module,
		// Per-spec line so grouped imports
		// `import ( "a"; "b" )` report distinct line numbers
		// per entry rather than collapsing onto the `import (`
		// line.
		Line: int(spec.StartPoint().Row) + 1,
	}
	if name := spec.ChildByFieldName("name"); name != nil {
		switch name.Type() {
		case goNodeDot:
			imp.LangMeta = map[string]any{"dot_import": true}
		case goNodeBlankIdentifier:
			imp.LangMeta = map[string]any{"blank_import": true}
		default:
			imp.Alias = name.Content(w.src)
		}
	}
	w.imports = append(w.imports, imp)
}

// goExtractReceiver returns (typeName, isPointer, receiverName).
// Tree-sitter shape for `func (r *Foo) Bar()`:
//
//	parameter_list
//	  parameter_declaration
//	    name: identifier "r"
//	    type: pointer_type
//	      type_identifier "Foo"
//
// For `func (Foo) Bar()` the receiver is unnamed -- name is
// empty and the type child sits at parameter_declaration's
// "type" field. For `func (r pkg.Foo) M()` the type is a
// qualified_type; we use the raw source as the type name in
// that case so cross-package references at least keep the
// dotted form. For `func (*Foo) M()` the type is a
// pointer_type with no name.
func goExtractReceiver(n *sitter.Node, src []byte) (typeName string, isPointer bool, receiverName string) {
	if n == nil {
		return "", false, ""
	}
	var paramDecl *sitter.Node
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c.Type() == goNodeParamDecl {
			paramDecl = c
			break
		}
	}
	if paramDecl == nil {
		return "", false, ""
	}
	if nameField := paramDecl.ChildByFieldName("name"); nameField != nil {
		receiverName = nameField.Content(src)
	}
	typeNode := paramDecl.ChildByFieldName("type")
	if typeNode == nil {
		// Fallback: paramDecl with no `type:` field --
		// scan named children for the first type-like
		// node. Tree-sitter sometimes elides the field
		// label on unnamed receivers like `func (Foo) M()`.
		for i := uint32(0); i < paramDecl.NamedChildCount(); i++ {
			c := paramDecl.NamedChild(int(i))
			switch c.Type() {
			case goNodeTypeIdentifier, goNodePointerType, goNodeQualifiedType:
				typeNode = c
			}
			if typeNode != nil {
				break
			}
		}
	}
	if typeNode == nil {
		return "", false, receiverName
	}
	if typeNode.Type() == goNodePointerType {
		isPointer = true
		// Inner type is the only named child.
		if typeNode.NamedChildCount() > 0 {
			typeNode = typeNode.NamedChild(0)
		} else {
			return "", true, receiverName
		}
	}
	typeName = strings.TrimSpace(typeNode.Content(src))
	return typeName, isPointer, receiverName
}

func goCollectStructEmbeds(structType *sitter.Node, src []byte) []string {
	var fields *sitter.Node
	for i := uint32(0); i < structType.NamedChildCount(); i++ {
		c := structType.NamedChild(int(i))
		if c.Type() == goNodeFieldDeclList {
			fields = c
			break
		}
	}
	if fields == nil {
		return nil
	}
	var out []string
	for i := uint32(0); i < fields.NamedChildCount(); i++ {
		fd := fields.NamedChild(int(i))
		if fd.Type() != goNodeFieldDecl {
			continue
		}
		// An embedded field declaration has a `type` child
		// but no `field_identifier` "name" children. A
		// regular field declaration has one or more
		// field_identifier name children before the type.
		hasFieldIdent := false
		for j := uint32(0); j < fd.NamedChildCount(); j++ {
			if fd.NamedChild(int(j)).Type() == goNodeFieldIdentifier {
				hasFieldIdent = true
				break
			}
		}
		if hasFieldIdent {
			continue
		}
		typeNode := fd.ChildByFieldName("type")
		if typeNode == nil && fd.NamedChildCount() > 0 {
			typeNode = fd.NamedChild(0)
		}
		if typeNode == nil {
			continue
		}
		out = append(out, goEmbedTypeName(typeNode, src))
	}
	return out
}

func goCollectInterfaceEmbeds(interfaceType *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < interfaceType.NamedChildCount(); i++ {
		c := interfaceType.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case goNodeTypeElem:
			// Modern grammar wraps each embedded type in a
			// `type_elem` node, e.g.
			//   interface { io.Reader; Closer }
			// produces two `type_elem` children, each with a
			// single type child (qualified_type / type_identifier).
			// Type-set unions (Go 1.18+ generics) produce a
			// single `type_elem` containing multiple type
			// children; we extract each as a separate embed so
			// the constraint surface is preserved.
			for j := uint32(0); j < c.NamedChildCount(); j++ {
				inner := c.NamedChild(int(j))
				if inner == nil {
					continue
				}
				switch inner.Type() {
				case goNodeTypeIdentifier, goNodeQualifiedType, goNodePointerType:
					out = append(out, goEmbedTypeName(inner, src))
				}
			}
		case goNodeTypeIdentifier, goNodeQualifiedType:
			// Fallback for older grammar revisions that did
			// not yet wrap interface embeds in `type_elem`.
			out = append(out, goEmbedTypeName(c, src))
		}
	}
	return out
}

// goEmbedTypeName returns the embedded type's display name.
// For `*Foo` it returns `*Foo` (preserving the pointer
// marker so consumers can distinguish value vs pointer
// embedding); for `pkg.Foo` and bare `Foo` it returns the
// source verbatim.
func goEmbedTypeName(n *sitter.Node, src []byte) string {
	if n.Type() == goNodePointerType {
		if inner := n.NamedChild(0); inner != nil {
			return "*" + goEmbedTypeName(inner, src)
		}
	}
	return strings.TrimSpace(n.Content(src))
}

// goUnquoteString decodes a Go string literal (interpreted
// or raw). Import paths are conventionally ASCII identifier
// segments so escapes are uncommon, but
// `strconv.Unquote` handles edge cases (e.g. backtick raw
// strings) more reliably than a naive trim. Falls back to a
// plain trim on decode error so malformed source still
// produces SOMETHING the dispatcher can log.
func goUnquoteString(s string) string {
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	if len(s) >= 2 && (s[0] == '"' || s[0] == '`') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// walkGoBareCalls visits every call_expression under body
// and records the bare-name callee. Selector calls
// (`r.Bar()`, `pkg.Func()`) are excluded -- receiver-calls
// have a dedicated walker (walkGoReceiverCalls) and
// cross-package calls are out of scope until the cross-file
// resolver story.
func walkGoBareCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != goNodeCallExpression {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil {
			return true
		}
		if fn.Type() == goNodeIdentifier {
			out = append(out, fn.Content(src))
		}
		return true
	})
	return out
}

// walkGoReceiverCalls extracts `<recvName>.<Name>(` call
// targets. The dispatcher resolves these against the
// enclosing receiver type without ambiguity (per evaluator
// finding #5).
func walkGoReceiverCalls(body *sitter.Node, recvName string, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != goNodeCallExpression {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil || fn.Type() != goNodeSelectorExpression {
			return true
		}
		operand := fn.ChildByFieldName("operand")
		field := fn.ChildByFieldName("field")
		if operand == nil || field == nil {
			return true
		}
		if operand.Type() != goNodeIdentifier || operand.Content(src) != recvName {
			return true
		}
		// Tree-sitter Go emits the selector RHS as
		// `field_identifier` for method/field selection on a
		// value; `identifier` shows up in edge cases. Accept
		// both so we don't miss legitimate receiver calls.
		if field.Type() == goNodeFieldIdentifier || field.Type() == goNodeIdentifier {
			out = append(out, field.Content(src))
		}
		return true
	})
	return out
}

// walkGoReceiverMemberAccesses returns the per-method list
// of `<recvName>.<field>` references with IsWrite set when
// the access appears on the LHS of an assignment_statement
// (or a short_var_declaration RHS-style assignment).
// Classification rule mirrors walkTSMemberAccesses and
// walkPySelfAccesses.
func walkGoReceiverMemberAccesses(body *sitter.Node, recvName string, src []byte) []MemberAccess {
	writes := map[string]struct{}{}
	seen := map[string]bool{}
	var reads []string
	walkChildren(body, func(node *sitter.Node) bool {
		switch node.Type() {
		case goNodeAssignment:
			lhs := node.ChildByFieldName("left")
			if lhs != nil {
				for _, name := range goSelectorWritesOn(lhs, recvName, src) {
					writes[name] = struct{}{}
					if !seen[name] {
						seen[name] = true
						reads = append(reads, name)
					}
				}
			}
		case goNodeSelectorExpression:
			if name, ok := goIsReceiverSelector(node, recvName, src); ok {
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

// goSelectorWritesOn returns the receiver-qualified field
// names written by an assignment LHS. Go's `left` field on
// an assignment_statement is an `expression_list` whose
// children may be plural (`a.x, a.y = 1, 2`), so we walk
// the list and pluck each receiver-qualified selector.
func goSelectorWritesOn(lhs *sitter.Node, recvName string, src []byte) []string {
	var out []string
	if name, ok := goIsReceiverSelector(lhs, recvName, src); ok {
		out = append(out, name)
		return out
	}
	for i := uint32(0); i < lhs.NamedChildCount(); i++ {
		if name, ok := goIsReceiverSelector(lhs.NamedChild(int(i)), recvName, src); ok {
			out = append(out, name)
		}
	}
	return out
}

func goIsReceiverSelector(n *sitter.Node, recvName string, src []byte) (string, bool) {
	if n == nil || n.Type() != goNodeSelectorExpression {
		return "", false
	}
	operand := n.ChildByFieldName("operand")
	field := n.ChildByFieldName("field")
	if operand == nil || field == nil {
		return "", false
	}
	if operand.Type() != goNodeIdentifier || operand.Content(src) != recvName {
		return "", false
	}
	if field.Type() != goNodeFieldIdentifier && field.Type() != goNodeIdentifier {
		return "", false
	}
	return field.Content(src), true
}
