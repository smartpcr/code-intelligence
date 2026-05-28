//go:build cgo

// Package ast C# tree-sitter parser.
//
// This file implements the LanguageParser interface for C#
// (`.cs`, `.csx`) on top of the smacker/go-tree-sitter
// bindings (tree-sitter-c-sharp grammar). It is the
// CGO-enabled counterpart to the TypeScript / Python
// tree-sitter implementations in parser_treesitter.go.
//
// Extraction scope (per the story brief):
//
//   - compilation_unit -> top-level usings + namespaces +
//     class/interface/struct/record/enum declarations.
//   - namespace_declaration / file_scoped_namespace_declaration:
//     walk the name into a dotted string and recurse into the
//     contained declarations carrying the namespace through
//     `LangMeta["namespace"]` so downstream consumers can
//     filter by namespace without re-parsing the canonical
//     signature.
//   - class_declaration, interface_declaration,
//     struct_declaration, record_declaration, enum_declaration:
//     each becomes a ClassDecl whose `Kind` field carries
//     `"class" | "interface" | "struct" | "record" | "enum"`.
//     The `partial` modifier surfaces on
//     `LangMeta["partial"] = true`; the architecture catalogue
//     (Section 4.4.3) reserves `namespace` and `partial` as
//     well-known C# keys so downstream tooling can rely on
//     their presence.
//   - method_declaration / constructor_declaration: extracted
//     as MethodDecl. Modifiers (public, static, async, ...)
//     are persisted on `Modifiers`. The body is descended for
//     bare-name and `this.*` call sites + member accesses,
//     mirroring the TS/Python walkers.
//   - using_directive: extracted as Import. The dotted name
//     becomes `Module`; `using static System.Math;` flags
//     `LangMeta["is_static"] = true`; `using F = System.IO.File;`
//     populates `Alias`.
//
// Inheritance / interface implementation:
//
//   The C# grammar does not separate `extends` from
//   `implements`; both live inside the same `base_list` node.
//   This parser realises the partition contract from
//   `tech-spec.md` Section 5.3 ("C# base-list partition rule")
//   by walking the file twice:
//
//     Pass A scans every type declaration and builds a
//       `localKind` map (simple type name -> "class" /
//       "interface" / "struct" / "record" / "enum") -- the
//       "file's local symbol table" the architecture
//       references.
//
//     Pass B emits each `ClassDecl` with `Extends` and
//       `Implements` already partitioned. For a class
//       declaration the position-0 base routes by
//       `localKind[entry]` (class -> Extends, interface ->
//       Implements, struct/record/enum -> dropped); later
//       positions land in Implements defensively because C#
//       permits at most one base class. Interface
//       declarations route every entry to Extends
//       (super-interfaces); struct / record declarations
//       route every entry to Implements (value types have no
//       base class in C#).
//
//   The verbatim list is always retained on
//   `LangMeta["base_raw"]` so the future cross-file resolver
//   can re-partition with project-wide kind information.
//   Cross-file unresolved entries default to Extends at
//   position 0 (most likely the cross-file base class) and
//   Implements at later positions; the dispatcher's Pass 2a
//   drops the edge per the C4 unknown-target rule when the
//   target cannot be resolved.

package ast

import (
	"context"
	"fmt"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/csharp"
)

// NewTreeSitterCSharpParser returns a LanguageParser that uses
// the upstream tree-sitter C# grammar to extract classes,
// methods, and using directives from `.cs` / `.csx` sources.
//
// The returned instance is stateless and safe for concurrent
// reuse across files (the underlying tree-sitter parser is
// constructed per Parse call so cursor state never leaks
// between callers).
func NewTreeSitterCSharpParser() LanguageParser {
	return csharpTreeSitterParser{}
}

// =====================================================================
// C# tree-sitter implementation
// =====================================================================

type csharpTreeSitterParser struct{}

func (csharpTreeSitterParser) Language() string { return "csharp" }

// Extensions claims `.cs` (C# source) and `.csx` (C# script).
// The dispatcher routes both through the same grammar; script
// files use the same syntax tree shape as compilation units
// from the parser's perspective.
func (csharpTreeSitterParser) Extensions() []string { return []string{".cs", ".csx"} }

func (csharpTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, csharp.GetLanguage())
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter csharp parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	w := csharpWalker{
		src:       src,
		localKind: classifyCSharpLocalKinds(root, src),
	}
	w.walkTop(root)
	return ParseResult{
		Classes: w.classes,
		Methods: w.methods,
		Imports: w.imports,
	}, nil
}

const (
	csNodeCompilationUnit         = "compilation_unit"
	csNodeNamespaceDecl           = "namespace_declaration"
	csNodeFileScopedNamespaceDecl = "file_scoped_namespace_declaration"
	csNodeClassDecl               = "class_declaration"
	csNodeInterfaceDecl           = "interface_declaration"
	csNodeStructDecl              = "struct_declaration"
	csNodeRecordDecl              = "record_declaration"
	csNodeRecordStructDecl        = "record_struct_declaration"
	csNodeEnumDecl                = "enum_declaration"
	csNodeDeclarationList         = "declaration_list"
	csNodeMethodDecl              = "method_declaration"
	csNodeConstructorDecl         = "constructor_declaration"
	csNodeDestructorDecl          = "destructor_declaration"
	csNodeOperatorDecl            = "operator_declaration"
	csNodeUsingDirective          = "using_directive"
	csNodeQualifiedName           = "qualified_name"
	csNodeIdentifier              = "identifier"
	csNodeBaseList                = "base_list"
	csNodeBlock                   = "block"
	csNodeArrowExpressionClause   = "arrow_expression_clause"
	csNodeModifier                = "modifier"
	csNodeInvocationExpr          = "invocation_expression"
	csNodeMemberAccessExpr        = "member_access_expression"
	csNodeAssignmentExpr          = "assignment_expression"
	csNodeThisExpression          = "this_expression"
	csNodeGenericName             = "generic_name"
	csNodeTypeArgumentList        = "type_argument_list"
	csNodeTypeParameterList       = "type_parameter_list"
	csNodePartial                 = "partial"
)

type csharpWalker struct {
	src []byte
	// localKind is the same-file "local symbol table" the
	// tech-spec partition rule references: simple type name
	// -> declaring kind ("class" / "interface" / "struct" /
	// "record" / "enum"). Populated by classifyCSharpLocalKinds
	// during Pass A so handleClass (Pass B) can partition each
	// base_list entry into Extends or Implements.
	localKind map[string]string
	classes   []ClassDecl
	methods   []MethodDecl
	imports   []Import
}

// classifyCSharpLocalKinds is the parser's Pass A walker: it
// scans every type declaration in the file once and returns a
// map from simple type name to declaring kind. The result is
// the same-file local symbol table that the base-list
// partition rule (tech-spec Section 5.3) uses to route each
// base into Extends vs Implements.
//
// Same-simple-name collisions (e.g., two `Foo` classes in
// different namespaces in the same file) overwrite; the
// tech-spec acknowledges cross-file kind resolution is future
// work and `LangMeta["base_raw"]` preserves the verbatim
// entries for that resolver.
func classifyCSharpLocalKinds(root *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case csNodeClassDecl, csNodeInterfaceDecl, csNodeStructDecl,
			csNodeRecordDecl, csNodeRecordStructDecl, csNodeEnumDecl:
			if nameNode := n.ChildByFieldName("name"); nameNode != nil {
				simple := strings.TrimSpace(nameNode.Content(src))
				if simple != "" {
					out[simple] = csKindFor(n.Type())
				}
			}
		}
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			walk(n.NamedChild(int(i)))
		}
	}
	walk(root)
	return out
}

func (w *csharpWalker) walkTop(root *sitter.Node) {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.visitTopLevel(root.NamedChild(int(i)), "", "")
	}
}

// visitTopLevel dispatches by node type. `namespace` is the
// accumulated dotted namespace inherited from any enclosing
// namespace_declaration scopes; `outer` is the dotted
// QualifiedName of the enclosing class (or "" at file scope).
func (w *csharpWalker) visitTopLevel(n *sitter.Node, namespace, outer string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case csNodeNamespaceDecl, csNodeFileScopedNamespaceDecl:
		w.handleNamespace(n, namespace, outer)
	case csNodeClassDecl, csNodeInterfaceDecl, csNodeStructDecl,
		csNodeRecordDecl, csNodeRecordStructDecl, csNodeEnumDecl:
		w.handleClass(n, namespace, outer)
	case csNodeMethodDecl, csNodeOperatorDecl:
		// Top-level method (script files / global statements):
		// emit as a free function.
		w.handleMethod(n, namespace, "")
	case csNodeUsingDirective:
		w.handleUsing(n)
	}
}

// handleNamespace dotted-joins the inherited namespace with
// the declared one and recurses into the body. Both
// `namespace Foo.Bar { ... }` and `namespace Foo.Bar;` (file-
// scoped) routes through here -- their children differ only
// in whether they're inside a declaration_list or hang off the
// namespace node directly.
func (w *csharpWalker) handleNamespace(n *sitter.Node, outerNs, outerClass string) {
	declared := w.namespaceName(n)
	ns := outerNs
	if declared != "" {
		if ns == "" {
			ns = declared
		} else {
			ns = ns + "." + declared
		}
	}

	// File-scoped: subsequent declarations are direct named
	// children of the file_scoped_namespace_declaration.
	// Classic: declarations live inside a `body` field
	// (declaration_list).
	body := n.ChildByFieldName("body")
	if body != nil {
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			w.visitTopLevel(body.NamedChild(int(i)), ns, outerClass)
		}
		return
	}
	// File-scoped: skip the `name` field and recurse into the
	// rest.
	nameField := n.ChildByFieldName("name")
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nameField {
			continue
		}
		w.visitTopLevel(c, ns, outerClass)
	}
}

// namespaceName extracts the dotted namespace name. Handles
// `name:` field being either an identifier or a qualified_name.
func (w *csharpWalker) namespaceName(n *sitter.Node) string {
	name := n.ChildByFieldName("name")
	if name == nil {
		return ""
	}
	return w.dottedName(name)
}

// dottedName flattens a qualified_name / identifier / generic_name
// chain into a dotted string. Generic type arguments are
// stripped (only the type's base name is retained).
func (w *csharpWalker) dottedName(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	switch n.Type() {
	case csNodeIdentifier:
		return n.Content(w.src)
	case csNodeQualifiedName:
		qualifier := n.ChildByFieldName("qualifier")
		name := n.ChildByFieldName("name")
		left := w.dottedName(qualifier)
		right := w.dottedName(name)
		if left == "" {
			return right
		}
		if right == "" {
			return left
		}
		return left + "." + right
	case csNodeGenericName:
		if nm := n.ChildByFieldName("name"); nm != nil {
			return nm.Content(w.src)
		}
		// Fallback: first named child is usually the
		// identifier even without a name field.
		if n.NamedChildCount() > 0 {
			return n.NamedChild(0).Content(w.src)
		}
		return ""
	default:
		// Be tolerant of alias_qualified_name etc. -- collect
		// identifier leaves in source order.
		var parts []string
		var walk func(*sitter.Node)
		walk = func(node *sitter.Node) {
			if node == nil {
				return
			}
			if node.Type() == csNodeTypeArgumentList || node.Type() == csNodeTypeParameterList {
				return
			}
			if node.Type() == csNodeIdentifier {
				parts = append(parts, node.Content(w.src))
				return
			}
			for i := uint32(0); i < node.NamedChildCount(); i++ {
				walk(node.NamedChild(int(i)))
			}
		}
		walk(n)
		return strings.Join(parts, ".")
	}
}

// handleClass emits a ClassDecl + recurses into the body to
// extract methods and nested types. `outer` is the
// QualifiedName of the enclosing class (empty at file/namespace
// scope); the namespace lives on `LangMeta["namespace"]`.
func (w *csharpWalker) handleClass(n *sitter.Node, namespace, outer string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	qualified := name
	if outer != "" {
		qualified = outer + "." + name
	}
	kind := csKindFor(n.Type())
	cls := ClassDecl{
		QualifiedName: qualified,
		Kind:          kind,
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
	}

	modifiers := collectCSharpModifiers(n, w.src)
	meta := map[string]any{}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	for _, m := range modifiers {
		if m == csNodePartial {
			meta["partial"] = true
			break
		}
	}

	// Inheritance / interface partition (tech-spec Section 5.3).
	// The C# `base_list` mixes class + interface entries with
	// no syntactic distinction. Pass A built w.localKind so
	// Pass B here can route each entry into Extends or
	// Implements per the declaring kind decision matrix. The
	// verbatim list is always retained on
	// `LangMeta["base_raw"]` for the future cross-file
	// resolver to re-partition with project-wide kind info.
	if bases := findCSharpBaseList(n); bases != nil {
		raw := collectCSharpTypeNames(bases, w.src)
		if len(raw) > 0 {
			rawCopy := make([]string, len(raw))
			copy(rawCopy, raw)
			meta["base_raw"] = rawCopy
			cls.Extends, cls.Implements = partitionCSharpBases(kind, raw, w.localKind)
		}
	}

	if len(meta) > 0 {
		cls.LangMeta = meta
	}

	w.classes = append(w.classes, cls)

	body := w.classBody(n)
	if body != nil {
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			member := body.NamedChild(int(i))
			switch member.Type() {
			case csNodeMethodDecl, csNodeOperatorDecl:
				w.handleMethod(member, namespace, qualified)
			case csNodeConstructorDecl, csNodeDestructorDecl:
				w.handleConstructor(member, namespace, qualified, name)
			case csNodeClassDecl, csNodeInterfaceDecl, csNodeStructDecl,
				csNodeRecordDecl, csNodeRecordStructDecl, csNodeEnumDecl:
				w.handleClass(member, namespace, qualified)
			}
		}
	}
}

// classBody returns the declaration_list child of a class /
// struct / interface / record / enum declaration. Most C#
// types expose it via the `body` field; some grammar versions
// place it as the last declaration_list child without a field.
func (w *csharpWalker) classBody(n *sitter.Node) *sitter.Node {
	if b := n.ChildByFieldName("body"); b != nil {
		return b
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c.Type() == csNodeDeclarationList {
			return c
		}
	}
	return nil
}

func csKindFor(nodeType string) string {
	switch nodeType {
	case csNodeInterfaceDecl:
		return "interface"
	case csNodeStructDecl:
		return "struct"
	case csNodeRecordDecl, csNodeRecordStructDecl:
		return "record"
	case csNodeEnumDecl:
		return "enum"
	default:
		return "class"
	}
}

// partitionCSharpBases applies the tech-spec Section 5.3
// decision matrix to a `base_list` raw entry sequence. The
// declaring kind determines which slots are Extends vs
// Implements; localKind (built by classifyCSharpLocalKinds)
// resolves each entry against the same-file local symbol
// table.
//
// Rules per declaring kind:
//
//   - "class": position 0 dispatches on localKind[entry]:
//     "class" -> Extends; "interface" -> Implements;
//     "struct" / "record" / "enum" -> dropped (a class
//     cannot extend a value type in C#); unresolved -> Extends
//     (C# permits at most one base class which must appear
//     first; an unresolved position-0 name is most likely the
//     cross-file base). Position 1+ goes to Implements
//     defensively (a class has exactly one base class in C#;
//     anything else in the list is an interface or invalid).
//   - "interface": every entry -> Extends (an interface's
//     base list is all super-interfaces).
//   - "struct" / "record": every entry -> Implements (value
//     types have no base class in C#; the entire list is
//     implemented interfaces).
//   - "enum": both empty (enum base type is the underlying
//     integer type, not an inheritance edge; it persists on
//     `base_raw` for completeness).
//
// The verbatim list is preserved on `LangMeta["base_raw"]`
// regardless of partition by the caller (`handleClass`).
func partitionCSharpBases(declKind string, raw []string, localKind map[string]string) (extends, implements []string) {
	switch declKind {
	case "interface":
		out := make([]string, len(raw))
		copy(out, raw)
		return out, nil
	case "struct", "record":
		out := make([]string, len(raw))
		copy(out, raw)
		return nil, out
	case "enum":
		return nil, nil
	case "class":
		for i, entry := range raw {
			switch localKind[entry] {
			case "interface":
				implements = append(implements, entry)
			case "class":
				if i == 0 {
					extends = append(extends, entry)
				} else {
					// Invalid C# (class has exactly one base
					// class, must be position 0); preserve
					// defensively in Implements so the
					// dispatcher can still emit an edge and
					// the verbatim list survives on base_raw.
					implements = append(implements, entry)
				}
			case "struct", "record", "enum":
				// A class cannot extend a value type; drop
				// from both partitions. base_raw retains the
				// verbatim entry for a future project-wide
				// resolver to re-partition.
			case "":
				// Unresolved (cross-file). Position 0 is most
				// likely the cross-file base class per C#'s
				// at-most-one-base-class rule; later positions
				// default to Implements defensively. The
				// dispatcher's Pass 2a drops the edge on a
				// classNodeID miss (C4), so a mis-classified
				// cross-file interface produces no false edge.
				if i == 0 {
					extends = append(extends, entry)
				} else {
					implements = append(implements, entry)
				}
			}
		}
		return extends, implements
	default:
		return nil, nil
	}
}

// findCSharpBaseList locates the `base_list` child (the
// `: Base, IFoo` clause after a type declaration's name).
// Some grammar versions expose it via the `bases` field;
// otherwise it's a direct named child.
func findCSharpBaseList(n *sitter.Node) *sitter.Node {
	if b := n.ChildByFieldName("bases"); b != nil {
		return b
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c.Type() == csNodeBaseList {
			return c
		}
	}
	return nil
}

// collectCSharpTypeNames returns the dotted names of each base
// in a `base_list`. Generic instantiations are reduced to the
// base type's name (matching the TS walker's `type_arguments`
// stripping so `Foo<T>` resolves to `Foo`).
func collectCSharpTypeNames(baseList *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < baseList.NamedChildCount(); i++ {
		c := baseList.NamedChild(int(i))
		name := walkerForName(src).dottedName(c)
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// walkerForName is a tiny shim so the package-level helpers
// can reuse csharpWalker.dottedName without holding state.
func walkerForName(src []byte) *csharpWalker { return &csharpWalker{src: src} }

// collectCSharpModifiers returns the lower-case modifier
// tokens declared on a class / method / constructor. Each
// `modifier` named child wraps a single keyword
// (`public`, `static`, `partial`, `async`, ...).
func collectCSharpModifiers(n *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c.Type() != csNodeModifier {
			continue
		}
		out = append(out, strings.TrimSpace(c.Content(src)))
	}
	return out
}

// handleMethod extracts a regular method declaration. Free
// methods (`enclosing == ""`) are emitted with EnclosingClass
// empty so the dispatcher treats them as free functions.
func (w *csharpWalker) handleMethod(n *sitter.Node, namespace, enclosing string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := nameNode.Content(w.src)
	qualified := name
	if enclosing != "" {
		qualified = enclosing + "." + name
	}
	w.appendMethod(n, namespace, enclosing, qualified)
}

// handleConstructor mirrors handleMethod for constructors and
// destructors. The C# grammar exposes the constructor's name
// either via the `name` field (matching the enclosing class)
// or by emitting the class name as a leaf identifier; either
// way we surface it as `<EnclosingClass>.<className>` so the
// canonical signature matches what calling-side resolvers
// would build from `new Foo(...)` constructions.
func (w *csharpWalker) handleConstructor(n *sitter.Node, namespace, enclosing, ctorName string) {
	name := ctorName
	if nm := n.ChildByFieldName("name"); nm != nil {
		name = nm.Content(w.src)
	}
	qualified := name
	if enclosing != "" {
		qualified = enclosing + "." + name
	}
	w.appendMethod(n, namespace, enclosing, qualified)
}

// appendMethod is the shared body of handleMethod /
// handleConstructor. It captures parameters, the body (block
// or arrow-expression clause), modifiers, and walks the body
// for calls + member accesses.
func (w *csharpWalker) appendMethod(n *sitter.Node, namespace, enclosing, qualified string) {
	params := ""
	if p := n.ChildByFieldName("parameters"); p != nil {
		params = trimParens(p.Content(w.src))
	}
	method := MethodDecl{
		QualifiedName:  qualified,
		EnclosingClass: enclosing,
		ParamSignature: params,
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
		Modifiers:      collectCSharpModifiers(n, w.src),
	}
	if namespace != "" {
		method.LangMeta = map[string]any{"namespace": namespace}
	}

	body := n.ChildByFieldName("body")
	if body == nil {
		body = findCSharpMethodBody(n)
	}
	if body != nil {
		switch body.Type() {
		case csNodeBlock:
			// Strip the outer `{` / `}` so CountLogicalLines
			// stays consistent across CGO=on and CGO=off
			// paths, matching the TS walker's
			// tsStripBraceSpan semantics.
			method.BodySource, method.BodyStartByte, method.BodyEndByte =
				tsStripBraceSpan(w.src, body)
		case csNodeArrowExpressionClause:
			// `=> expression;` -- keep the raw text; the
			// dispatcher's logical-line counter handles
			// single-expression bodies trivially.
			method.BodySource = body.Content(w.src)
			method.BodyStartByte = int(body.StartByte())
			method.BodyEndByte = int(body.EndByte()) - 1
		default:
			method.BodySource = body.Content(w.src)
			method.BodyStartByte = int(body.StartByte())
			method.BodyEndByte = int(body.EndByte()) - 1
		}
		method.BodyStartLine = int(body.StartPoint().Row) + 1
		method.BodyEndLine = int(body.EndPoint().Row) + 1
		method.Calls = uniqueStringsInsert(walkCSharpCalls(body, w.src))
		if enclosing != "" {
			method.ReceiverCalls = uniqueStringsInsert(walkCSharpThisCalls(body, w.src))
			method.MemberAccesses = walkCSharpThisAccesses(body, w.src)
		}
	}
	w.methods = append(w.methods, method)
}

// findCSharpMethodBody locates the body of a method when
// `body:` is absent (expression-bodied members and partial
// definitions sometimes lack the field). Prefers
// `arrow_expression_clause` then `block`.
func findCSharpMethodBody(n *sitter.Node) *sitter.Node {
	var block, arrow *sitter.Node
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		switch c.Type() {
		case csNodeBlock:
			if block == nil {
				block = c
			}
		case csNodeArrowExpressionClause:
			if arrow == nil {
				arrow = c
			}
		}
	}
	if arrow != nil {
		return arrow
	}
	return block
}

// walkCSharpCalls collects the bare-name callees of every
// invocation_expression under body. Member-access calls
// (`x.f()`, `this.f()`) are routed to walkCSharpThisCalls
// for `this`-qualified ones; arbitrary-object calls remain
// out of scope until the cross-file resolver story.
func walkCSharpCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != csNodeInvocationExpr {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil {
			return true
		}
		if fn.Type() == csNodeIdentifier {
			out = append(out, fn.Content(src))
		}
		return true
	})
	return out
}

// walkCSharpThisCalls extracts `this.<name>(` call targets
// for receiver-qualified resolution against the enclosing
// class.
func walkCSharpThisCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != csNodeInvocationExpr {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil || fn.Type() != csNodeMemberAccessExpr {
			return true
		}
		obj := fn.ChildByFieldName("expression")
		nm := fn.ChildByFieldName("name")
		if obj == nil || nm == nil {
			return true
		}
		if obj.Type() == csNodeThisExpression && nm.Type() == csNodeIdentifier {
			out = append(out, nm.Content(src))
		}
		return true
	})
	return out
}

// walkCSharpThisAccesses returns `this.<name>` member access
// records with IsWrite set when the access appears on the
// LHS of an assignment_expression. Names are deduped with
// write-wins-over-read precedence, matching the TS / Python
// walkers.
//
// Per tech-spec Section 5.3 ("Member access" row), a
// `this.<name>` access is recorded ONLY when it sits OUTSIDE
// any invocation_expression. When the member access is the
// `function:` child of an invocation_expression (the receiver
// call form `this.Method()`), it is handled by
// walkCSharpThisCalls instead; recording it here too would
// emit a spurious read edge on the enclosing type for every
// receiver-qualified method call.
func walkCSharpThisAccesses(body *sitter.Node, src []byte) []MemberAccess {
	writes := map[string]struct{}{}
	seen := map[string]bool{}
	var reads []string
	walkChildren(body, func(node *sitter.Node) bool {
		switch node.Type() {
		case csNodeAssignmentExpr:
			lhs := node.ChildByFieldName("left")
			if name, ok := isCSharpThisMember(lhs, src); ok {
				writes[name] = struct{}{}
				if !seen[name] {
					seen[name] = true
					reads = append(reads, name)
				}
				return true
			}
		case csNodeMemberAccessExpr:
			// Skip when this member access is the `function:`
			// of an enclosing invocation_expression: that
			// shape is a ReceiverCall (`this.M()`), not a
			// MemberAccess. walkCSharpThisCalls handles it.
			if parent := node.Parent(); parent != nil && parent.Type() == csNodeInvocationExpr {
				if fn := parent.ChildByFieldName("function"); fn != nil && node.Equal(fn) {
					return true
				}
			}
			if name, ok := isCSharpThisMember(node, src); ok {
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

func isCSharpThisMember(n *sitter.Node, src []byte) (string, bool) {
	if n == nil || n.Type() != csNodeMemberAccessExpr {
		return "", false
	}
	obj := n.ChildByFieldName("expression")
	nm := n.ChildByFieldName("name")
	if obj == nil || nm == nil {
		return "", false
	}
	if obj.Type() != csNodeThisExpression || nm.Type() != csNodeIdentifier {
		return "", false
	}
	return nm.Content(src), true
}

// handleUsing extracts a `using` directive. Forms covered:
//
//   - using System;                  -> Module="System"
//   - using System.IO;               -> Module="System.IO"
//   - using static System.Math;      -> LangMeta["is_static"]=true
//   - using F = System.IO.File;      -> Alias="F", Module="System.IO.File"
//   - global using System;           -> LangMeta["is_global"]=true
//
// The grammar exposes the alias via the `alias:` field (single
// identifier on the LHS of `=`) when present. The module name
// is the dotted qualified_name / identifier child that is NOT
// the alias node. `static` / `global` appear as unnamed leaf
// children whose source text is the keyword; we scan all direct
// children for those tokens.
func (w *csharpWalker) handleUsing(n *sitter.Node) {
	imp := Import{Line: int(n.StartPoint().Row) + 1}
	var meta map[string]any

	// Pick up `static` / `global` flag tokens. These are
	// unnamed direct children whose source text equals the
	// keyword.
	for i := uint32(0); i < n.ChildCount(); i++ {
		c := n.Child(int(i))
		if c == nil || c.IsNamed() {
			continue
		}
		switch c.Content(w.src) {
		case "static":
			if meta == nil {
				meta = map[string]any{}
			}
			meta["is_static"] = true
		case "global":
			if meta == nil {
				meta = map[string]any{}
			}
			meta["is_global"] = true
		}
	}

	// Alias is exposed via the `alias:` field when the
	// directive uses the `using X = ...;` form.
	aliasNode := n.ChildByFieldName("alias")
	if aliasNode != nil {
		imp.Alias = firstIdentifier(aliasNode, w.src)
	}

	// Module is the first dotted-name child that is not the
	// alias. Walk named children in source order so we pick up
	// either a qualified_name, an identifier, an
	// alias_qualified_name, or a generic_name without making
	// brittle assumptions about which field name (`name:` vs.
	// no field) the current grammar version uses.
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil || c == aliasNode {
			continue
		}
		switch c.Type() {
		case csNodeQualifiedName, csNodeIdentifier, csNodeGenericName,
			"alias_qualified_name":
			if imp.Module == "" {
				imp.Module = w.dottedName(c)
			}
		}
	}

	if imp.Module == "" {
		return
	}
	if meta != nil {
		imp.LangMeta = meta
	}
	w.imports = append(w.imports, imp)
}

// firstIdentifier returns the source text of the first
// identifier leaf reachable from n.
func firstIdentifier(n *sitter.Node, src []byte) string {
	if n == nil {
		return ""
	}
	if n.Type() == csNodeIdentifier {
		return n.Content(src)
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if got := firstIdentifier(n.NamedChild(int(i)), src); got != "" {
			return got
		}
	}
	return ""
}
