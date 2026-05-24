//go:build cgo

package parser

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

func init() { registerInDefault(LanguageTypeScript, func() Parser { return &tsParser{} }) }

// tsParser is the v1 tree-sitter-backed adapter for TypeScript
// / JavaScript files (`.ts`, `.tsx`, `.mts`, `.cts`, `.js`,
// `.jsx`, `.mjs`, `.cjs`). It wraps the bundled
// `tree-sitter-typescript` grammar via
// `github.com/smacker/go-tree-sitter`. `.tsx` / `.jsx` content
// uses the `tsx` dialect; everything else uses `typescript`.
type tsParser struct{}

func (t *tsParser) Language() string { return LanguageTypeScript }

func (t *tsParser) Parse(ctx context.Context, path string, content []byte) (*AstFile, error) {
	if err := validateContent(path, content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ts := sitter.NewParser()
	defer ts.Close()
	lang := typescript.GetLanguage()
	lowerPath := strings.ToLower(path)
	if strings.HasSuffix(lowerPath, ".tsx") || strings.HasSuffix(lowerPath, ".jsx") {
		lang = tsx.GetLanguage()
	}
	ts.SetLanguage(lang)
	tree, err := ts.ParseCtx(ctx, nil, content)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	root := tree.RootNode()

	lineCount, byteCount := lineCounts(content)
	b := newScopeBuilder(path, lineCount, byteCount)
	moduleName := strings.TrimSuffix(b.fileScope.Name, ".ts")
	moduleName = strings.TrimSuffix(moduleName, ".tsx")
	moduleName = strings.TrimSuffix(moduleName, ".mts")
	moduleName = strings.TrimSuffix(moduleName, ".cts")
	moduleName = strings.TrimSuffix(moduleName, ".js")
	moduleName = strings.TrimSuffix(moduleName, ".jsx")
	moduleName = strings.TrimSuffix(moduleName, ".mjs")
	moduleName = strings.TrimSuffix(moduleName, ".cjs")
	b.fileScope.QualifiedName = moduleName
	b.fileScope.Attrs = map[string]string{"module": moduleName}

	walker := &tsWalker{b: b, src: content}
	walker.walk(root, b.fileScope.ScopeId, moduleName)

	out := b.build(LanguageTypeScript, path, content)
	if root.HasError() {
		out.DegradedReason = "tree_sitter_parse_error"
	}
	return out, nil
}

type tsWalker struct {
	b   *scopeBuilder
	src []byte
}

func (w *tsWalker) walk(n *sitter.Node, parentScopeID, parentQualified string) {
	if n == nil {
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(int(i))
		if child == nil {
			continue
		}
		switch child.Type() {
		case "import_statement":
			w.handleImport(child)
		case "export_statement":
			// Recurse so `export class Foo {}` lands.
			for j := uint32(0); j < child.NamedChildCount(); j++ {
				c := child.NamedChild(int(j))
				if c == nil {
					continue
				}
				switch c.Type() {
				case "class_declaration":
					w.handleClass(c, parentScopeID, parentQualified)
				case "interface_declaration":
					w.handleInterface(c, parentScopeID, parentQualified)
				case "function_declaration":
					w.handleFunction(c, parentScopeID, parentQualified)
				case "type_alias_declaration":
					w.handleTypeAlias(c, parentScopeID)
				}
			}
		case "class_declaration":
			w.handleClass(child, parentScopeID, parentQualified)
		case "interface_declaration":
			w.handleInterface(child, parentScopeID, parentQualified)
		case "function_declaration":
			w.handleFunction(child, parentScopeID, parentQualified)
		case "type_alias_declaration":
			w.handleTypeAlias(child, parentScopeID)
		default:
			// Recurse into namespaces / blocks so nested
			// declarations surface.
			if child.Type() == "internal_module" || child.Type() == "namespace_declaration" {
				w.walk(child, parentScopeID, parentQualified)
			}
		}
	}
}

func (w *tsWalker) handleImport(n *sitter.Node) {
	source := ""
	if src := findChildByFieldName(n, "source"); src != nil {
		source = stripQuotes(strings.TrimSpace(nodeText(src, w.src)))
	}
	if source == "" {
		// Could be a side-effect import like
		// `import "foo"`; fall back to scanning string
		// children.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c == nil {
				continue
			}
			if c.Type() == "string" {
				source = stripQuotes(strings.TrimSpace(nodeText(c, w.src)))
				break
			}
		}
	}
	display := strings.TrimSpace(nodeText(n, w.src))
	// Strip leading `import ` and trailing `;` for the symbol
	// name to mirror the lexer adapter's behaviour.
	display = strings.TrimPrefix(display, "import")
	display = strings.TrimSpace(strings.TrimSuffix(display, ";"))
	symID := w.b.addSymbol(&AstSymbol{
		Name:    display,
		Kind:    "import",
		ScopeId: w.b.fileScope.ScopeId,
		Range:   nodeRange(n),
		Attrs:   map[string]string{"from": source},
	})
	w.b.addEdge(&AstEdge{
		Kind:  "imports",
		From:  scopeRef(w.b.fileScope.ScopeId),
		To:    externalScopeRef(source),
		Attrs: map[string]string{"symbol_id": symID, "language": LanguageTypeScript},
	})
}

func (w *tsWalker) handleClass(n *sitter.Node, parentScopeID, parentQualified string) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		return
	}
	attrs := map[string]string{}
	var extends, implements []string
	// heritage clauses are `class_heritage` or `extends_clause` /
	// `implements_clause` nodes depending on grammar revision.
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "class_heritage":
			for j := uint32(0); j < c.NamedChildCount(); j++ {
				cc := c.NamedChild(int(j))
				if cc == nil {
					continue
				}
				switch cc.Type() {
				case "extends_clause", "extends_type_clause":
					extends = append(extends, tsHeritageNames(cc, w.src)...)
				case "implements_clause":
					implements = append(implements, tsHeritageNames(cc, w.src)...)
				}
			}
		case "extends_clause", "extends_type_clause":
			extends = append(extends, tsHeritageNames(c, w.src)...)
		case "implements_clause":
			implements = append(implements, tsHeritageNames(c, w.src)...)
		}
	}
	if len(extends) > 0 {
		attrs["extends"] = strings.Join(extends, ",")
	}
	if len(implements) > 0 {
		attrs["implements"] = strings.Join(implements, ",")
	}
	if tsHasAbstractModifier(n, w.src) {
		attrs["abstract"] = "true"
	}
	scope := &AstScope{
		ScopeKind:     ScopeKindClass,
		Name:          name,
		QualifiedName: joinQualified(parentQualified, name),
		Range:         nodeRange(n),
		ParentScopeId: parentScopeID,
		Attrs:         attrs,
	}
	id := w.b.addScope(scope)
	for _, e := range extends {
		w.b.addEdge(&AstEdge{Kind: "extends", From: scopeRef(id), To: externalScopeRef(e)})
	}
	for _, im := range implements {
		w.b.addEdge(&AstEdge{Kind: "implements", From: scopeRef(id), To: externalScopeRef(im)})
	}
	body := findChildByFieldName(n, "body")
	if body != nil {
		tsWalkClassBody(w, body, id, scope.QualifiedName, ScopeKindClass)
	}
}

func (w *tsWalker) handleInterface(n *sitter.Node, parentScopeID, parentQualified string) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		return
	}
	attrs := map[string]string{}
	var extends []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		if c.Type() == "extends_type_clause" || c.Type() == "extends_clause" {
			extends = append(extends, tsHeritageNames(c, w.src)...)
		}
	}
	if len(extends) > 0 {
		attrs["extends"] = strings.Join(extends, ",")
	}
	scope := &AstScope{
		ScopeKind:     ScopeKindInterface,
		Name:          name,
		QualifiedName: joinQualified(parentQualified, name),
		Range:         nodeRange(n),
		ParentScopeId: parentScopeID,
		Attrs:         attrs,
	}
	id := w.b.addScope(scope)
	for _, e := range extends {
		w.b.addEdge(&AstEdge{Kind: "extends", From: scopeRef(id), To: externalScopeRef(e)})
	}
	body := findChildByFieldName(n, "body")
	if body != nil {
		tsWalkClassBody(w, body, id, scope.QualifiedName, ScopeKindInterface)
	}
}

func (w *tsWalker) handleFunction(n *sitter.Node, parentScopeID, parentQualified string) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		return
	}
	params := tsParameterTypesNode(findChildByFieldName(n, "parameters"), w.src)
	attrs := map[string]string{}
	if tsHasModifier(n, w.src, "async") {
		attrs["async"] = "true"
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
	w.b.addScope(scope)
}

func (w *tsWalker) handleTypeAlias(n *sitter.Node, parentScopeID string) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		return
	}
	w.b.addSymbol(&AstSymbol{
		Name:    name,
		Kind:    "type_alias",
		ScopeId: parentScopeID,
		Range:   nodeRange(n),
	})
}

// tsWalkClassBody iterates `class_body` / `interface_body` (or
// `object_type`) children and emits method / property scopes
// under `parentID`.
func tsWalkClassBody(w *tsWalker, body *sitter.Node, parentID, parentQualified string, parentKind ScopeKind) {
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		c := body.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "method_definition", "method_signature", "abstract_method_signature":
			tsEmitMethod(w, c, parentID, parentQualified, parentKind)
		case "function_signature":
			tsEmitMethod(w, c, parentID, parentQualified, parentKind)
		case "public_field_definition", "property_signature":
			// fields are emitted as symbols so SOLID recipes
			// can count them.
			name := ""
			if n := findChildByFieldName(c, "name"); n != nil {
				name = strings.TrimSpace(nodeText(n, w.src))
			}
			if name == "" {
				continue
			}
			w.b.addSymbol(&AstSymbol{
				Name:    name,
				Kind:    "field",
				ScopeId: parentID,
				Range:   nodeRange(c),
			})
		}
	}
}

func tsEmitMethod(w *tsWalker, n *sitter.Node, parentID, parentQualified string, parentKind ScopeKind) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		// constructor / index_signature have no `name` field
		// in some grammar revisions; fall back to the node
		// text leading up to `(`.
		text := nodeText(n, w.src)
		if i := strings.IndexByte(text, '('); i > 0 {
			candidate := strings.TrimSpace(text[:i])
			// strip leading modifiers
			parts := strings.Fields(candidate)
			if len(parts) > 0 {
				name = parts[len(parts)-1]
			}
		}
		if name == "" {
			name = n.Type()
		}
	}
	params := tsParameterTypesNode(findChildByFieldName(n, "parameters"), w.src)
	attrs := map[string]string{}
	if parentKind == ScopeKindInterface {
		attrs["interface_member"] = "true"
	}
	if n.Type() == "abstract_method_signature" || tsHasModifier(n, w.src, "abstract") {
		attrs["abstract"] = "true"
	}
	if tsHasModifier(n, w.src, "async") {
		attrs["async"] = "true"
	}
	if tsHasModifier(n, w.src, "static") {
		attrs["static"] = "true"
	}
	for _, kw := range []string{"public", "private", "protected", "readonly", "override"} {
		if tsHasModifier(n, w.src, kw) {
			attrs[kw] = "true"
		}
	}
	scope := &AstScope{
		ScopeKind:     ScopeKindMethod,
		Name:          name,
		QualifiedName: joinQualified(parentQualified, name),
		Parameters:    params,
		Range:         nodeRange(n),
		ParentScopeId: parentID,
		Attrs:         attrs,
	}
	w.b.addScope(scope)
}

// tsHeritageNames extracts comma-separated heritage type names
// from an `extends_clause` / `implements_clause` /
// `extends_type_clause` node. Strips any generic argument list.
func tsHeritageNames(n *sitter.Node, src []byte) []string {
	if n == nil {
		return nil
	}
	var out []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		t := c.Type()
		if t == "extends" || t == "implements" || t == "comma" {
			continue
		}
		text := strings.TrimSpace(nodeText(c, src))
		if text == "" {
			continue
		}
		// Strip generic argument list.
		if i := strings.IndexByte(text, '<'); i > 0 {
			text = text[:i]
		}
		out = append(out, text)
	}
	return out
}

// tsHasModifier returns true if `n` has a leading anonymous
// modifier token whose source text equals `kw`. Used to detect
// `async`/`abstract`/`static`/visibility tokens that tree-
// sitter exposes as anonymous siblings rather than named
// fields.
func tsHasModifier(n *sitter.Node, src []byte, kw string) bool {
	if n == nil {
		return false
	}
	for i := uint32(0); i < n.ChildCount(); i++ {
		c := n.Child(int(i))
		if c == nil || c.IsNamed() {
			continue
		}
		if strings.TrimSpace(nodeText(c, src)) == kw {
			return true
		}
	}
	return false
}

// tsHasAbstractModifier is a shortcut so the class handler can
// short-circuit without two iterations over the same children.
func tsHasAbstractModifier(n *sitter.Node, src []byte) bool {
	return tsHasModifier(n, src, "abstract")
}

// tsParameterTypesNode walks a tree-sitter `formal_parameters`
// node and produces the canonical parameter slice (types when
// annotated, identifiers when not).
func tsParameterTypesNode(params *sitter.Node, src []byte) []string {
	if params == nil {
		return nil
	}
	var out []string
	for i := uint32(0); i < params.NamedChildCount(); i++ {
		c := params.NamedChild(int(i))
		if c == nil {
			continue
		}
		if t := findChildByFieldName(c, "type"); t != nil {
			text := strings.TrimSpace(nodeText(t, src))
			text = strings.TrimPrefix(text, ":")
			out = append(out, strings.TrimSpace(text))
			continue
		}
		if name := findChildByFieldName(c, "name"); name != nil {
			out = append(out, strings.TrimSpace(nodeText(name, src)))
			continue
		}
		out = append(out, strings.TrimSpace(nodeText(c, src)))
	}
	return out
}
