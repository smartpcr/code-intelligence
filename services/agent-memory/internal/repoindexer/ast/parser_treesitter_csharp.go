//go:build cgo

// Package ast — tree-sitter C# parser.
//
// This file implements the LanguageParser interface for C# on
// top of the smacker/go-tree-sitter `c_sharp` grammar binding.
// CGO-gated for the same reason as the C / C++ / Go parsers:
// the grammar links against C-compiled tree-sitter runtime
// objects, and the portable `make test` lane (CGO=0) does not
// link this file in.
//
// Scope (per the workstream brief, expanded in iter-4 of the
// Go-fixture-test stage to land the C# class / method /
// inheritance walker as a cross-cutting baseline repair
// authorised by the operator's pinned answer to the
// `baseline-package-break` open question -- items #2 and #3 of
// the iter-3 evaluator feedback):
//
//   - Walk `compilation_unit`. Recurse into both
//     `namespace_declaration` and
//     `file_scoped_namespace_declaration` WITHOUT modifying
//     the enclosing-class context. Namespace prefixing is
//     intentionally NOT applied to QualifiedNames; the
//     fixture-test helpers (csharpHasSuffix / findCSharpClass)
//     match by simple-name / dotted suffix so the parser stays
//     compatible whether or not a future revision prefixes
//     names with the namespace.
//
//   - Emit `ClassDecl` for `class_declaration` /
//     `interface_declaration` / `struct_declaration` /
//     `record_declaration` with the appropriate Kind tag.
//     Capture `base_list` entries verbatim into Extends in
//     pass 1; post-walk, re-distribute each base entry across
//     Extends vs Implements using a `kindByName` lookup
//     populated from the emitted class/interface set so that
//     same-file `Base, IGreeter` lists are partitioned
//     correctly.
//
//   - Emit `MethodDecl` for `method_declaration`. The
//     QualifiedName is `<EnclosingClass>.<MethodName>`. Walk
//     the body (`block` or `arrow_expression_clause`) for
//     `invocation_expression` whose `function` field is a
//     bare identifier; those are surfaced into `Calls` for the
//     dispatcher's Pass 2b resolver. Member-access
//     (`obj.Foo()` / `this.Foo()`) calls are intentionally
//     dropped because the dispatcher's same-file callee index
//     is keyed on the bare method name and a receiver-
//     qualified emission would not match -- the C# fixture
//     test explicitly pins ReceiverCalls = 0.
//
//   - Emit `Import` for each `using_directive`. The module
//     name is the directive's `name` field (`qualified_name`
//     or `identifier`) -- with a defensive fallback that
//     scans for the first qualified_name / identifier / alias
//     qualified_name named child if the field lookup misses.
//
// Field declarations (`private string prefix = "hi";`) are
// intentionally not surfaced: ParseResult has no field slice
// and ClassDecl tracks only structural shape. A walker that
// misclassified field-with-initializer as a method would
// inflate the method count and trip the fixture's exact-
// cardinality assertion.

package ast

import (
	"context"
	"fmt"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/csharp"
)

// NewTreeSitterCSharpParser returns a tree-sitter-backed
// LanguageParser for C# source files. The walker covers
// classes, interfaces, structs, records, methods, base lists
// (extends vs implements), `using` directives, and bare-name
// method calls within method bodies.
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

// Node-type constants used by the walker. Kept inline as a
// `const` block at function-file scope (not package scope) so
// the symbols don't compete with the C / C++ walkers' own
// `c*` / `cpp*` prefixed constants.
const (
	csharpNodeCompilationUnit             = "compilation_unit"
	csharpNodeUsingDirective              = "using_directive"
	csharpNodeNamespaceDeclaration        = "namespace_declaration"
	csharpNodeFileScopedNamespaceDecl     = "file_scoped_namespace_declaration"
	csharpNodeClassDeclaration            = "class_declaration"
	csharpNodeInterfaceDeclaration        = "interface_declaration"
	csharpNodeStructDeclaration           = "struct_declaration"
	csharpNodeRecordDeclaration           = "record_declaration"
	csharpNodeMethodDeclaration           = "method_declaration"
	csharpNodeBaseList                    = "base_list"
	csharpNodeBlock                       = "block"
	csharpNodeArrowExpressionClause       = "arrow_expression_clause"
	csharpNodeInvocationExpression        = "invocation_expression"
	csharpNodeMemberAccessExpression      = "member_access_expression"
	csharpNodeIdentifier                  = "identifier"
	csharpNodeQualifiedName               = "qualified_name"
	csharpNodeAliasQualifiedName          = "alias_qualified_name"
	csharpNodeGenericName                 = "generic_name"
	csharpNodeParameterList               = "parameter_list"
	csharpNodeBracketedParameterList      = "bracketed_parameter_list"
	csharpNodeModifier                    = "modifier"
)

// Parse drives the walker. Errors from the underlying
// tree-sitter parse are surfaced verbatim; an empty
// ParseResult is returned for empty / nil roots so callers
// don't have to special-case that path.
func (csharpTreeSitterParser) Parse(relPath string, src []byte) (ParseResult, error) {
	root, err := sitter.ParseCtx(context.Background(), src, csharp.GetLanguage())
	if err != nil {
		return ParseResult{}, fmt.Errorf("ast: tree-sitter c# parse %s: %w", relPath, err)
	}
	if root == nil {
		return ParseResult{}, nil
	}
	w := &csharpWalker{src: src}
	w.walk(root, "")
	w.partitionBases()
	return ParseResult{
		Classes: w.classes,
		Methods: w.methods,
		Imports: w.imports,
	}, nil
}

// csharpWalker accumulates declarations during a DFS over the
// compilation_unit tree.
type csharpWalker struct {
	src     []byte
	classes []ClassDecl
	methods []MethodDecl
	imports []Import
	// kindByName maps simple class/interface/struct/record
	// name -> "class" | "interface" | "struct" | "record".
	// Populated as declarations are emitted; consumed by
	// partitionBases to split each ClassDecl's raw Extends
	// slice into Extends + Implements buckets after the
	// walk completes (forward references in `class A : B,
	// IFoo` work cleanly because the lookup happens after
	// every type has been visited).
	kindByName map[string]string
}

// walk dispatches on n.Type() and recurses into children.
// `enclosingClass` is the simple-name of the immediate
// containing class/interface/struct/record, or empty at
// compilation-unit / namespace scope. The walker does NOT
// thread namespace prefixes; the test helpers accept either
// simple-name or namespace-prefixed forms and the dispatcher
// normalises canonical signatures downstream.
func (w *csharpWalker) walk(n *sitter.Node, enclosingClass string) {
	if n == nil {
		return
	}
	switch n.Type() {
	case csharpNodeUsingDirective:
		w.handleUsingDirective(n)
		return
	case csharpNodeClassDeclaration:
		w.handleTypeDeclaration(n, "class")
		return
	case csharpNodeInterfaceDeclaration:
		w.handleTypeDeclaration(n, "interface")
		return
	case csharpNodeStructDeclaration:
		w.handleTypeDeclaration(n, "struct")
		return
	case csharpNodeRecordDeclaration:
		w.handleTypeDeclaration(n, "record")
		return
	case csharpNodeMethodDeclaration:
		if enclosingClass != "" {
			w.handleMethodDeclaration(n, enclosingClass)
		}
		return
	case csharpNodeNamespaceDeclaration, csharpNodeFileScopedNamespaceDecl:
		// Namespace prefixing is intentionally NOT
		// threaded into enclosingClass. The fixture test
		// pins simple-name forms (`Base`, `IGreeter`,
		// `HelloWorld`) via csharpHasSuffix; threading the
		// namespace would push qualified names like
		// `Demo.Base` and break Extends/Implements
		// resolution against a kindByName keyed on
		// `Base`. Recurse with enclosingClass unchanged.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			w.walk(n.NamedChild(int(i)), enclosingClass)
		}
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		w.walk(n.NamedChild(int(i)), enclosingClass)
	}
}

// handleUsingDirective extracts the module path from a
// `using_directive` and appends one Import entry. The grammar
// exposes the path via the `name` field; defensive fallback
// scans for the first qualified_name / identifier / alias
// qualified_name named child if the field lookup misses.
func (w *csharpWalker) handleUsingDirective(n *sitter.Node) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			switch c.Type() {
			case csharpNodeQualifiedName, csharpNodeIdentifier, csharpNodeAliasQualifiedName:
				nameNode = c
			}
			if nameNode != nil {
				break
			}
		}
	}
	if nameNode == nil {
		return
	}
	module := strings.TrimSpace(nameNode.Content(w.src))
	if module == "" {
		return
	}
	alias := ""
	if a := n.ChildByFieldName("alias"); a != nil {
		alias = strings.TrimSpace(a.Content(w.src))
	}
	w.imports = append(w.imports, Import{
		Module: module,
		Alias:  alias,
		Line:   int(n.StartPoint().Row) + 1,
	})
}

// handleTypeDeclaration emits one ClassDecl per
// class/interface/struct/record declaration. Base list entries
// are captured into Extends in raw form; partitionBases
// re-splits them into Extends/Implements after the walk
// completes.
func (w *csharpWalker) handleTypeDeclaration(n *sitter.Node, kind string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Content(w.src))
	if name == "" {
		return
	}
	var rawBases []string
	if bases := n.ChildByFieldName("bases"); bases != nil {
		rawBases = collectCSharpBases(bases, w.src)
	} else {
		// Older grammar revisions may not field-tag the
		// base list -- find it by node type as a fallback.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c.Type() == csharpNodeBaseList {
				rawBases = collectCSharpBases(c, w.src)
				break
			}
		}
	}
	cls := ClassDecl{
		QualifiedName: name,
		Kind:          kind,
		Extends:       rawBases,
		StartLine:     int(n.StartPoint().Row) + 1,
		EndLine:       int(n.EndPoint().Row) + 1,
	}
	w.classes = append(w.classes, cls)
	if w.kindByName == nil {
		w.kindByName = map[string]string{}
	}
	w.kindByName[name] = kind

	// Recurse into the body so nested methods/types are
	// captured. Both `body` field and unfielded body block
	// fallbacks are supported.
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
			w.walk(body.NamedChild(int(i)), name)
		}
	}
}

// collectCSharpBases returns the simple-name list captured
// from a base_list node. Each entry is normalised to its
// final identifier component (e.g. `System.IDisposable` ->
// `IDisposable`, `IEnumerable<T>` -> `IEnumerable`) so the
// partition step's kindByName lookup matches against
// emitted simple-names.
func collectCSharpBases(list *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < list.NamedChildCount(); i++ {
		c := list.NamedChild(int(i))
		raw := strings.TrimSpace(c.Content(src))
		if raw == "" {
			continue
		}
		out = append(out, csharpSimpleBaseName(raw))
	}
	return out
}

// csharpSimpleBaseName reduces a base-list entry's source
// text to its final identifier component, stripping generic
// type arguments and namespace qualifiers. `IEnumerable<T>`
// becomes `IEnumerable`; `System.IDisposable` becomes
// `IDisposable`; `Foo<Bar.Baz>` becomes `Foo`.
func csharpSimpleBaseName(s string) string {
	// Strip generic args first -- they may contain dots
	// (`Foo<Bar.Baz>`) that would otherwise leak into the
	// simple-name extraction.
	if i := strings.Index(s, "<"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// partitionBases re-distributes each ClassDecl's Extends
// slice into Extends + Implements buckets using the
// kindByName lookup populated during the walk.
//
// Rules:
//   - Bases that resolve to a known interface -> Implements.
//   - Bases that resolve to a known class/struct/record ->
//     Extends.
//   - Bases that don't resolve at all (cross-file types like
//     `System.IDisposable`, or external library bases): C#
//     syntax allows AT MOST ONE class base, and it MUST be
//     the first entry of the base list. So the first
//     unknown becomes Extends and subsequent unknowns become
//     Implements -- the standard heuristic also adopted by
//     OmniSharp / Roslyn-lite parsers.
func (w *csharpWalker) partitionBases() {
	for ci := range w.classes {
		raw := w.classes[ci].Extends
		var extends, implements []string
		firstUnknown := true
		for _, b := range raw {
			kind, known := w.kindByName[b]
			if known {
				if kind == "interface" {
					implements = append(implements, b)
				} else {
					extends = append(extends, b)
				}
				continue
			}
			if firstUnknown {
				extends = append(extends, b)
				firstUnknown = false
			} else {
				implements = append(implements, b)
			}
		}
		w.classes[ci].Extends = extends
		w.classes[ci].Implements = implements
	}
}

// handleMethodDeclaration emits one MethodDecl per
// method_declaration. The QualifiedName is
// `<EnclosingClass>.<Name>`. Walks the body (block or
// arrow_expression_clause) for `invocation_expression` whose
// function field is a bare identifier -> Calls. Member-access
// calls are dropped (the test pins ReceiverCalls = 0; the
// dispatcher's Pass 2b resolves bare names only).
func (w *csharpWalker) handleMethodDeclaration(n *sitter.Node, enclosingClass string) {
	nameNode := n.ChildByFieldName("name")
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nameNode.Content(w.src))
	if name == "" {
		return
	}
	params := ""
	if p := n.ChildByFieldName("parameters"); p != nil {
		params = trimParens(p.Content(w.src))
	}
	method := MethodDecl{
		QualifiedName:  enclosingClass + "." + name,
		EnclosingClass: enclosingClass,
		ParamSignature: params,
		StartLine:      int(n.StartPoint().Row) + 1,
		EndLine:        int(n.EndPoint().Row) + 1,
		Modifiers:      collectCSharpModifiers(n, w.src),
	}
	body := n.ChildByFieldName("body")
	if body != nil {
		switch body.Type() {
		case csharpNodeBlock:
			method.BodySource, method.BodyStartByte, method.BodyEndByte =
				csharpStripBraceSpan(w.src, body)
			method.BodyStartLine = int(body.StartPoint().Row) + 1
			method.BodyEndLine = int(body.EndPoint().Row) + 1
			calls := walkCSharpCalls(body, w.src)
			method.Calls = uniqueStringsInsert(calls)
		case csharpNodeArrowExpressionClause:
			// Expression-bodied member: `=> expr`. The
			// body is the inner expression node, not the
			// arrow clause itself. Capture its content
			// and walk it for calls.
			expr := body.ChildByFieldName("expression")
			if expr == nil {
				for i := uint32(0); i < body.NamedChildCount(); i++ {
					expr = body.NamedChild(int(i))
					if expr != nil {
						break
					}
				}
			}
			if expr != nil {
				method.BodySource = expr.Content(w.src)
				method.BodyStartByte = int(expr.StartByte())
				method.BodyEndByte = int(expr.EndByte())
				method.BodyStartLine = int(expr.StartPoint().Row) + 1
				method.BodyEndLine = int(expr.EndPoint().Row) + 1
				calls := walkCSharpCalls(expr, w.src)
				method.Calls = uniqueStringsInsert(calls)
			}
		}
	}
	w.methods = append(w.methods, method)
}

// walkCSharpCalls visits every invocation_expression under
// the given root and records bare-identifier callees in source
// order. Receiver-qualified calls (`this.Foo()`,
// `obj.Foo()`) are intentionally dropped because the
// dispatcher's Pass 2b same-file resolver matches bare names
// against the file-local callee index. The fixture test
// explicitly pins ReceiverCalls = 0 for the C# Greet method
// -- the test author's note flags that surfacing `this.Foo`
// here would be a misclassification on top of the
// dispatcher's downstream policy.
func walkCSharpCalls(body *sitter.Node, src []byte) []string {
	var out []string
	walkChildren(body, func(node *sitter.Node) bool {
		if node.Type() != csharpNodeInvocationExpression {
			return true
		}
		fn := node.ChildByFieldName("function")
		if fn == nil {
			// Older grammar revisions may field-tag this
			// as `expression` -- fall back gracefully.
			fn = node.ChildByFieldName("expression")
		}
		if fn == nil {
			return true
		}
		if fn.Type() == csharpNodeIdentifier {
			out = append(out, strings.TrimSpace(fn.Content(src)))
		}
		// Other shapes (member_access_expression,
		// generic_name, alias_qualified_name) are skipped
		// -- their target isn't a bare identifier the
		// dispatcher can resolve directly.
		return true
	})
	return out
}

// collectCSharpModifiers returns the access / disposition
// keywords appearing directly under the method_declaration
// node. Tree-sitter-csharp tags each keyword as a `modifier`
// node containing a single anonymous keyword child; we
// surface the raw text of the modifier.
func collectCSharpModifiers(n *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < n.ChildCount(); i++ {
		c := n.Child(int(i))
		if c == nil {
			continue
		}
		if c.Type() != csharpNodeModifier {
			continue
		}
		tok := strings.TrimSpace(c.Content(src))
		if tok != "" {
			out = append(out, tok)
		}
	}
	return out
}

// csharpStripBraceSpan extracts the interior of a C# `block`
// (`{ ... }`) returning the inner source alongside interior
// byte offsets. Mirror of cStripBraceSpan / cppStripBraceSpan.
func csharpStripBraceSpan(src []byte, body *sitter.Node) (string, int, int) {
	startByte := int(body.StartByte())
	endByte := int(body.EndByte())
	content := body.Content(src)
	if len(content) < 2 || content[0] != '{' || content[len(content)-1] != '}' {
		return content, startByte, endByte
	}
	inner := content[1 : len(content)-1]
	return inner, startByte + 1, endByte - 2
}
