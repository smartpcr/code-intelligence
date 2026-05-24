//go:build cgo

package parser

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/java"
)

func init() { registerInDefault(LanguageJava, func() Parser { return &javaParser{} }) }

// javaParser is the v1 tree-sitter-backed adapter for `.java`
// sources. It wraps the bundled `tree-sitter-java` grammar via
// `github.com/smacker/go-tree-sitter` and emits the canonical
// `AstFile` shape (package + class/interface/method scopes,
// import + extends + implements edges) so clean-code recipes
// can read a single shape regardless of source language.
type javaParser struct{}

func (j *javaParser) Language() string { return LanguageJava }

func (j *javaParser) Parse(ctx context.Context, path string, content []byte) (*AstFile, error) {
	if err := validateContent(path, content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ts := sitter.NewParser()
	defer ts.Close()
	ts.SetLanguage(java.GetLanguage())
	tree, err := ts.ParseCtx(ctx, nil, content)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	root := tree.RootNode()

	lineCount, byteCount := lineCounts(content)
	b := newScopeBuilder(path, lineCount, byteCount)

	// Find and emit the package scope if present.
	pkg := ""
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		c := root.NamedChild(int(i))
		if c == nil {
			continue
		}
		if c.Type() == "package_declaration" {
			for j := uint32(0); j < c.NamedChildCount(); j++ {
				name := c.NamedChild(int(j))
				if name != nil && (name.Type() == "scoped_identifier" || name.Type() == "identifier") {
					pkg = strings.TrimSpace(nodeText(name, content))
					break
				}
			}
			break
		}
	}
	if pkg != "" {
		b.fileScope.Attrs = map[string]string{"package": pkg}
		b.fileScope.QualifiedName = joinQualified(pkg, b.fileScope.Name)
		pkgScope := &AstScope{
			ScopeKind:     ScopeKindPackage,
			Name:          pkg,
			QualifiedName: pkg,
			Range:         nodeRange(root),
			Attrs:         map[string]string{"language": LanguageJava},
		}
		pkgScopeID := b.addScope(pkgScope)
		b.fileScope.ParentScopeId = pkgScopeID
	}

	walker := &javaWalker{b: b, src: content}
	walker.walk(root, b.fileScope.ScopeId, b.fileScope.QualifiedName)

	out := b.build(LanguageJava, path, content)
	if root.HasError() {
		out.DegradedReason = "tree_sitter_parse_error"
	}
	return out, nil
}

type javaWalker struct {
	b   *scopeBuilder
	src []byte
}

func (w *javaWalker) walk(n *sitter.Node, parentScopeID, parentQualified string) {
	if n == nil {
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(int(i))
		if child == nil {
			continue
		}
		switch child.Type() {
		case "import_declaration":
			w.handleImport(child)
		case "class_declaration", "record_declaration", "enum_declaration":
			w.handleClass(child, parentScopeID, parentQualified)
		case "interface_declaration", "annotation_type_declaration":
			w.handleInterface(child, parentScopeID, parentQualified)
		}
	}
}

func (w *javaWalker) handleImport(n *sitter.Node) {
	target := ""
	isStatic := false
	for i := uint32(0); i < n.ChildCount(); i++ {
		c := n.Child(int(i))
		if c == nil {
			continue
		}
		if !c.IsNamed() {
			if strings.TrimSpace(nodeText(c, w.src)) == "static" {
				isStatic = true
			}
			continue
		}
		switch c.Type() {
		case "scoped_identifier", "identifier":
			target = strings.TrimSpace(nodeText(c, w.src))
		case "asterisk":
			if target != "" {
				target += ".*"
			}
		}
	}
	if target == "" {
		// Fallback: parse the raw text between `import` and `;`.
		raw := strings.TrimSpace(nodeText(n, w.src))
		raw = strings.TrimPrefix(raw, "import")
		raw = strings.TrimPrefix(strings.TrimSpace(raw), "static")
		target = strings.TrimSpace(strings.TrimSuffix(raw, ";"))
	}
	symID := w.b.addSymbol(&AstSymbol{
		Name:    target,
		Kind:    "import",
		ScopeId: w.b.fileScope.ScopeId,
		Range:   nodeRange(n),
		Attrs:   map[string]string{"static": boolStr(isStatic)},
	})
	w.b.addEdge(&AstEdge{
		Kind:  "imports",
		From:  scopeRef(w.b.fileScope.ScopeId),
		To:    externalScopeRef(target),
		Attrs: map[string]string{"symbol_id": symID, "language": LanguageJava},
	})
}

func (w *javaWalker) handleClass(n *sitter.Node, parentScopeID, parentQualified string) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		return
	}
	attrs := javaCollectModifierAttrs(n, w.src)
	if n.Type() == "enum_declaration" {
		attrs["java_type"] = "enum"
	} else if n.Type() == "record_declaration" {
		attrs["java_type"] = "record"
	}
	var extends, implements []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "superclass":
			extends = append(extends, javaTypeNames(c, w.src)...)
		case "super_interfaces":
			implements = append(implements, javaTypeNames(c, w.src)...)
		}
	}
	if len(extends) > 0 {
		attrs["extends"] = strings.Join(extends, ",")
	}
	if len(implements) > 0 {
		attrs["implements"] = strings.Join(implements, ",")
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
	if body == nil {
		// `class_body` is also exposed as a named child without
		// being the value of the `body` field on some grammar
		// revisions; locate it manually.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c == nil {
				continue
			}
			t := c.Type()
			if t == "class_body" || t == "enum_body" || t == "record_body" {
				body = c
				break
			}
		}
	}
	if body != nil {
		javaWalkClassBody(w, body, id, scope.QualifiedName, ScopeKindClass)
	}
}

func (w *javaWalker) handleInterface(n *sitter.Node, parentScopeID, parentQualified string) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		return
	}
	attrs := javaCollectModifierAttrs(n, w.src)
	var extends []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		if c.Type() == "extends_interfaces" {
			extends = append(extends, javaTypeNames(c, w.src)...)
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
	if body == nil {
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c != nil && c.Type() == "interface_body" {
				body = c
				break
			}
		}
	}
	if body != nil {
		javaWalkClassBody(w, body, id, scope.QualifiedName, ScopeKindInterface)
	}
}

func javaWalkClassBody(w *javaWalker, body *sitter.Node, parentID, parentQualified string, parentKind ScopeKind) {
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		c := body.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "method_declaration", "constructor_declaration":
			javaEmitMethod(w, c, parentID, parentQualified, parentKind)
		case "class_declaration", "record_declaration":
			w.handleClass(c, parentID, parentQualified)
		case "interface_declaration":
			w.handleInterface(c, parentID, parentQualified)
		case "field_declaration":
			javaEmitField(w, c, parentID)
		case "enum_constant":
			name := ""
			if id := findChildByFieldName(c, "name"); id != nil {
				name = strings.TrimSpace(nodeText(id, w.src))
			}
			if name != "" {
				w.b.addSymbol(&AstSymbol{
					Name:    name,
					Kind:    "enum_constant",
					ScopeId: parentID,
					Range:   nodeRange(c),
				})
			}
		}
	}
}

func javaEmitMethod(w *javaWalker, n *sitter.Node, parentID, parentQualified string, parentKind ScopeKind) {
	name := ""
	if id := findChildByFieldName(n, "name"); id != nil {
		name = strings.TrimSpace(nodeText(id, w.src))
	}
	if name == "" {
		// constructor_declaration has no name field; the
		// declared type is the identifier we want.
		text := nodeText(n, w.src)
		// Strip modifiers and find the first identifier.
		fields := strings.Fields(text)
		for _, f := range fields {
			if f == "public" || f == "private" || f == "protected" || f == "static" || f == "final" || f == "abstract" {
				continue
			}
			// `<TypeArgs>` block from generic constructors.
			if strings.HasPrefix(f, "<") {
				continue
			}
			if paren := strings.IndexByte(f, '('); paren >= 0 {
				f = f[:paren]
			}
			if f != "" {
				name = f
				break
			}
		}
	}
	params := javaParameterTypesNode(findChildByFieldName(n, "parameters"), w.src)
	attrs := javaCollectModifierAttrs(n, w.src)
	if parentKind == ScopeKindInterface {
		attrs["interface_member"] = "true"
	}
	if n.Type() == "constructor_declaration" {
		attrs["constructor"] = "true"
	}
	if throws := findChildByFieldName(n, "throws"); throws != nil {
		attrs["throws"] = strings.TrimSpace(nodeText(throws, w.src))
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

func javaEmitField(w *javaWalker, n *sitter.Node, parentID string) {
	declarators := []string{}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		if c.Type() == "variable_declarator" {
			if id := findChildByFieldName(c, "name"); id != nil {
				declarators = append(declarators, strings.TrimSpace(nodeText(id, w.src)))
			}
		}
	}
	for _, d := range declarators {
		if d == "" {
			continue
		}
		w.b.addSymbol(&AstSymbol{
			Name:    d,
			Kind:    "field",
			ScopeId: parentID,
			Range:   nodeRange(n),
		})
	}
}

// javaCollectModifierAttrs scans the `modifiers` child of a
// declaration node and stamps each Java modifier keyword as
// a string attr. Annotations are stored under
// `attrs["annotations"]` as a comma-separated list.
func javaCollectModifierAttrs(n *sitter.Node, src []byte) map[string]string {
	out := map[string]string{}
	mods := findChildByFieldName(n, "modifiers")
	if mods == nil {
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			c := n.NamedChild(int(i))
			if c != nil && c.Type() == "modifiers" {
				mods = c
				break
			}
		}
	}
	if mods == nil {
		return out
	}
	var annotations []string
	for i := uint32(0); i < mods.ChildCount(); i++ {
		c := mods.Child(int(i))
		if c == nil {
			continue
		}
		if !c.IsNamed() {
			kw := strings.TrimSpace(nodeText(c, src))
			switch kw {
			case "public", "private", "protected", "static", "final", "abstract",
				"synchronized", "native", "default", "sealed", "transient", "volatile":
				out[kw] = "true"
			}
			continue
		}
		if c.Type() == "annotation" || c.Type() == "marker_annotation" {
			annotations = append(annotations, strings.TrimSpace(nodeText(c, src)))
		}
	}
	if len(annotations) > 0 {
		out["annotations"] = strings.Join(annotations, ",")
	}
	return out
}

// javaTypeNames extracts type names from `superclass`,
// `super_interfaces`, or `extends_interfaces` child nodes.
// Strips generic argument lists (`Foo<T>` -> `Foo`).
func javaTypeNames(n *sitter.Node, src []byte) []string {
	if n == nil {
		return nil
	}
	var out []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		c := n.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_identifier", "scoped_type_identifier", "identifier":
			out = append(out, strings.TrimSpace(nodeText(c, src)))
		case "generic_type":
			if base := findChildByFieldName(c, "type"); base != nil {
				out = append(out, strings.TrimSpace(nodeText(base, src)))
			} else {
				text := strings.TrimSpace(nodeText(c, src))
				if i := strings.IndexByte(text, '<'); i > 0 {
					text = text[:i]
				}
				out = append(out, text)
			}
		case "type_list", "interface_type_list":
			out = append(out, javaTypeNames(c, src)...)
		}
	}
	if len(out) == 0 {
		// Fallback: parse text after `extends`/`implements`.
		raw := strings.TrimSpace(nodeText(n, src))
		raw = strings.TrimPrefix(raw, "extends")
		raw = strings.TrimPrefix(strings.TrimSpace(raw), "implements")
		for _, part := range strings.Split(raw, ",") {
			p := strings.TrimSpace(part)
			if i := strings.IndexByte(p, '<'); i > 0 {
				p = p[:i]
			}
			if p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

// javaParameterTypesNode walks a tree-sitter `formal_parameters`
// node and produces the canonical parameter slice (types
// only, mirroring the lexer-fallback behaviour).
func javaParameterTypesNode(params *sitter.Node, src []byte) []string {
	if params == nil {
		return nil
	}
	var out []string
	for i := uint32(0); i < params.NamedChildCount(); i++ {
		c := params.NamedChild(int(i))
		if c == nil {
			continue
		}
		if c.Type() != "formal_parameter" && c.Type() != "spread_parameter" {
			continue
		}
		if t := findChildByFieldName(c, "type"); t != nil {
			out = append(out, strings.TrimSpace(nodeText(t, src)))
			continue
		}
		// fallback: drop the last identifier and keep the rest
		// as the type.
		text := strings.TrimSpace(nodeText(c, src))
		if sp := strings.LastIndexByte(text, ' '); sp >= 0 {
			out = append(out, strings.TrimSpace(text[:sp]))
		} else {
			out = append(out, text)
		}
	}
	return out
}
