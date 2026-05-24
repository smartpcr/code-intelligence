//go:build cgo

package parser

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/python"
)

func init() { registerInDefault(LanguagePython, func() Parser { return &pythonParser{} }) }

// pythonParser is the v1 tree-sitter-backed adapter for Python
// sources (`.py`, `.pyi`, `#!/usr/bin/env python*` scripts).
// It wraps the bundled `tree-sitter-python` grammar via
// `github.com/smacker/go-tree-sitter`. The grammar produces a
// concrete syntax tree; we walk it and emit the canonical
// `AstFile` shape (scopes/symbols/edges) that recipes consume.
//
// A non-CGO build of the same package falls back to
// `python.go` (lexer-based) so cross-compile / sandboxed
// builds without a C toolchain still satisfy the `Parser`
// interface, just at reduced fidelity.
type pythonParser struct{}

func (p *pythonParser) Language() string { return LanguagePython }

func (p *pythonParser) Parse(ctx context.Context, path string, content []byte) (*AstFile, error) {
	if err := validateContent(path, content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ts := sitter.NewParser()
	defer ts.Close()
	ts.SetLanguage(python.GetLanguage())
	tree, err := ts.ParseCtx(ctx, nil, content)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	root := tree.RootNode()

	lineCount, byteCount := lineCounts(content)
	b := newScopeBuilder(path, lineCount, byteCount)
	module := strings.TrimSuffix(b.fileScope.Name, ".py")
	module = strings.TrimSuffix(module, ".pyi")
	b.fileScope.QualifiedName = module
	b.fileScope.Attrs = map[string]string{"module": module}

	walker := &pythonWalker{b: b, src: content, module: module}
	walker.walk(root, b.fileScope.ScopeId, module)

	out := b.build(LanguagePython, path, content)
	if root.HasError() {
		out.DegradedReason = "tree_sitter_parse_error"
	}
	return out, nil
}

type pythonWalker struct {
	b      *scopeBuilder
	src    []byte
	module string
}

func (w *pythonWalker) walk(n *sitter.Node, parentScopeID, parentQualified string) {
	if n == nil {
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(int(i))
		if child == nil {
			continue
		}
		switch child.Type() {
		case "import_statement", "import_from_statement":
			w.handleImport(child)
		case "class_definition":
			w.handleClass(child, parentScopeID, parentQualified, "")
		case "function_definition", "async_function_definition":
			w.handleFunction(child, parentScopeID, parentQualified, "")
		case "decorated_definition":
			w.handleDecorated(child, parentScopeID, parentQualified)
		default:
			// Recurse so nested decls (within if-blocks or
			// try-blocks at module level) still surface.
			w.walk(child, parentScopeID, parentQualified)
		}
	}
}

func (w *pythonWalker) handleImport(n *sitter.Node) {
	module := ""
	if from := findChildByFieldName(n, "module_name"); from != nil {
		module = strings.TrimSpace(nodeText(from, w.src))
	}
	// Collect all `dotted_name` / `aliased_import` children.
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "dotted_name", "aliased_import":
			name := strings.TrimSpace(nodeText(c, w.src))
			if c.Type() == "aliased_import" {
				if orig := findChildByFieldName(c, "name"); orig != nil {
					name = strings.TrimSpace(nodeText(orig, w.src))
				}
			}
			if name == "" || (module != "" && name == module) {
				continue
			}
			display := name
			if module != "" {
				display = module + "." + name
			}
			symID := w.b.addSymbol(&AstSymbol{
				Name:    display,
				Kind:    "import",
				ScopeId: w.b.fileScope.ScopeId,
				Range:   nodeRange(n),
			})
			target := display
			if module != "" {
				target = module
			}
			w.b.addEdge(&AstEdge{
				Kind:  "imports",
				From:  scopeRef(w.b.fileScope.ScopeId),
				To:    externalScopeRef(target),
				Attrs: map[string]string{"symbol_id": symID, "language": LanguagePython},
			})
		}
	}
	// `import x, y` has direct `dotted_name` children with no
	// module_name; the loop above handles those too.
}

func (w *pythonWalker) handleClass(n *sitter.Node, parentScopeID, parentQualified, decorators string) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		return
	}
	bases := []string{}
	if sl := findChildByFieldName(n, "superclasses"); sl != nil {
		for j := uint32(0); j < sl.NamedChildCount(); j++ {
			c := sl.NamedChild(int(j))
			if c == nil {
				continue
			}
			t := strings.TrimSpace(nodeText(c, w.src))
			if t != "" {
				bases = append(bases, t)
			}
		}
	}
	attrs := map[string]string{}
	kind := ScopeKindClass
	if len(bases) > 0 {
		attrs["bases"] = strings.Join(bases, ",")
		if looksLikeABC(strings.Join(bases, ",")) {
			kind = ScopeKindInterface
		}
	}
	if decorators != "" {
		attrs["decorators"] = decorators
	}
	scope := &AstScope{
		ScopeKind:     kind,
		Name:          name,
		QualifiedName: joinQualified(parentQualified, name),
		Range:         nodeRange(n),
		ParentScopeId: parentScopeID,
		Attrs:         attrs,
	}
	id := w.b.addScope(scope)
	for _, base := range bases {
		if base == "object" {
			continue
		}
		edgeKind := "extends"
		if kind == ScopeKindClass && looksLikeABC(base) {
			edgeKind = "implements"
		}
		w.b.addEdge(&AstEdge{
			Kind: edgeKind,
			From: scopeRef(id),
			To:   externalScopeRef(base),
		})
	}
	if body := findChildByFieldName(n, "body"); body != nil {
		w.walk(body, id, scope.QualifiedName)
	}
}

func (w *pythonWalker) handleFunction(n *sitter.Node, parentScopeID, parentQualified, decorators string) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		return
	}
	params := pythonParameterTypes(findChildByFieldName(n, "parameters"), w.src)
	attrs := map[string]string{}
	if n.Type() == "async_function_definition" {
		attrs["async"] = "true"
	}
	if decorators != "" {
		attrs["decorators"] = decorators
	}
	scope := &AstScope{
		ScopeKind:     ScopeKindMethod,
		Name:          name,
		QualifiedName: joinQualified(parentQualified, name),
		Parameters:    params,
		Range:         nodeRange(n),
		ParentScopeId: parentScopeID,
		Attrs:         attrs,
	}
	id := w.b.addScope(scope)
	if body := findChildByFieldName(n, "body"); body != nil {
		w.walk(body, id, scope.QualifiedName)
	}
}

func (w *pythonWalker) handleDecorated(n *sitter.Node, parentScopeID, parentQualified string) {
	var decorators []string
	var target *sitter.Node
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "decorator":
			decorators = append(decorators, strings.TrimSpace(strings.TrimPrefix(nodeText(c, w.src), "@")))
		case "class_definition", "function_definition", "async_function_definition":
			target = c
		}
	}
	if target == nil {
		return
	}
	joined := strings.Join(decorators, ",")
	switch target.Type() {
	case "class_definition":
		w.handleClass(target, parentScopeID, parentQualified, joined)
	case "function_definition", "async_function_definition":
		w.handleFunction(target, parentScopeID, parentQualified, joined)
	}
}

// pythonParameterTypes turns a tree-sitter `parameters` node
// into a slice of parameter labels suitable for Stage 2.2 to
// compose into a canonical signature. We keep the annotation
// when present, the identifier when not (mirrors the
// lexer-fallback behaviour).
func pythonParameterTypes(params *sitter.Node, src []byte) []string {
	if params == nil {
		return nil
	}
	var out []string
	for i := uint32(0); i < params.NamedChildCount(); i++ {
		c := params.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier":
			out = append(out, nodeText(c, src))
		case "typed_parameter", "typed_default_parameter":
			if ann := findChildByFieldName(c, "type"); ann != nil {
				out = append(out, strings.TrimSpace(nodeText(ann, src)))
			} else if name := findChildByFieldName(c, "name"); name != nil {
				out = append(out, strings.TrimSpace(nodeText(name, src)))
			} else {
				out = append(out, strings.TrimSpace(nodeText(c, src)))
			}
		case "default_parameter":
			if name := findChildByFieldName(c, "name"); name != nil {
				out = append(out, strings.TrimSpace(nodeText(name, src)))
			}
		case "list_splat_pattern", "dictionary_splat_pattern":
			out = append(out, strings.TrimSpace(nodeText(c, src)))
		}
	}
	return out
}
