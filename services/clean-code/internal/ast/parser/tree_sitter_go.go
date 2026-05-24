//go:build cgo

package parser

import (
	"context"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	tsgo "github.com/smacker/go-tree-sitter/golang"
)

func init() { registerInDefault(LanguageGo, func() Parser { return &goParser{} }) }

// goParser is the v1 tree-sitter-backed adapter for `*.go`
// sources. It wraps the bundled `tree-sitter-go` grammar
// (via `github.com/smacker/go-tree-sitter/golang`) and emits
// the canonical `AstFile` shape -- package + struct/interface
// + method scopes, imports + extends + embeds + implements
// edges -- so every recipe consumes the same shape regardless
// of language.
//
// A `//go:build !cgo` fallback in `go.go` exists for builds
// without a C toolchain; both implementations satisfy the
// same `parser_test.go` and `edges_test.go` assertions.
type goParser struct{}

func (g *goParser) Language() string { return LanguageGo }

func (g *goParser) Parse(ctx context.Context, path string, content []byte) (*AstFile, error) {
	if err := validateContent(path, content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	ts := sitter.NewParser()
	defer ts.Close()
	ts.SetLanguage(tsgo.GetLanguage())
	tree, err := ts.ParseCtx(ctx, nil, content)
	if err != nil {
		return nil, err
	}
	defer tree.Close()
	root := tree.RootNode()

	lineCount, byteCount := lineCounts(content)
	b := newScopeBuilder(path, lineCount, byteCount)

	// Extract package name and emit a package scope above the
	// file scope (mirrors the stdlib parser path so the same
	// assertions hold under both build tags).
	pkgName := ""
	var packageClauseNode *sitter.Node
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		c := root.NamedChild(int(i))
		if c == nil {
			continue
		}
		if c.Type() == "package_clause" {
			packageClauseNode = c
			for j := uint32(0); j < c.NamedChildCount(); j++ {
				sub := c.NamedChild(int(j))
				if sub == nil {
					continue
				}
				if sub.Type() == "package_identifier" || sub.Type() == "identifier" {
					pkgName = strings.TrimSpace(nodeText(sub, content))
					break
				}
			}
			break
		}
	}
	if pkgName != "" {
		b.fileScope.Attrs = map[string]string{"package": pkgName}
		b.fileScope.QualifiedName = joinQualified(pkgName, b.fileScope.Name)
		// Pin the package scope's range to the `package_clause`
		// token (not the whole file) so callers using
		// `AstScope.Range` to distinguish "inside the package
		// scope" from "inside the file scope" get a meaningful
		// answer -- and so the cgo path matches the precise,
		// narrow range emitted by the `!cgo` fallback
		// (`astRangeFromPos(fset, file.Package, file.Package)`
		// in `go.go`).
		pkgRange := nodeRange(root)
		if packageClauseNode != nil {
			pkgRange = nodeRange(packageClauseNode)
		}
		pkgScope := &AstScope{
			ScopeKind:     ScopeKindPackage,
			Name:          pkgName,
			QualifiedName: pkgName,
			Range:         pkgRange,
			Attrs:         map[string]string{"language": LanguageGo},
		}
		pkgScopeID := b.addScope(pkgScope)
		b.fileScope.ParentScopeId = pkgScopeID
	}

	walker := &goTSWalker{b: b, src: content}
	// Pass 1: emit imports + type declarations so receiver
	// methods (Pass 2) can find their parent struct/interface.
	walker.walkImports(root)
	walker.walkTypes(root, b.fileScope.ScopeId, b.fileScope.QualifiedName)
	// Pass 2: function + method declarations.
	walker.walkFunctions(root, b.fileScope.ScopeId, b.fileScope.QualifiedName)

	out := b.build(LanguageGo, path, content)
	if root.HasError() {
		out.DegradedReason = "tree_sitter_parse_error"
	}
	return out, nil
}

type goTSWalker struct {
	b               *scopeBuilder
	src             []byte
	typeScopeByName map[string]string
}

func (w *goTSWalker) walkImports(root *sitter.Node) {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		c := root.NamedChild(int(i))
		if c == nil || c.Type() != "import_declaration" {
			continue
		}
		for j := uint32(0); j < c.NamedChildCount(); j++ {
			sub := c.NamedChild(int(j))
			if sub == nil {
				continue
			}
			switch sub.Type() {
			case "import_spec":
				w.emitImport(sub)
			case "import_spec_list":
				for k := uint32(0); k < sub.NamedChildCount(); k++ {
					spec := sub.NamedChild(int(k))
					if spec != nil && spec.Type() == "import_spec" {
						w.emitImport(spec)
					}
				}
			}
		}
	}
}

func (w *goTSWalker) emitImport(spec *sitter.Node) {
	path := ""
	alias := ""
	for i := uint32(0); i < spec.NamedChildCount(); i++ {
		c := spec.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "interpreted_string_literal", "raw_string_literal":
			path = stripQuotes(strings.TrimSpace(nodeText(c, w.src)))
		case "package_identifier", "identifier":
			alias = strings.TrimSpace(nodeText(c, w.src))
		case "blank_identifier":
			alias = "_"
		case "dot":
			alias = "."
		}
	}
	if path == "" {
		return
	}
	name := path
	if alias != "" {
		name = alias
	}
	symID := w.b.addSymbol(&AstSymbol{
		Name:    name,
		Kind:    "import",
		ScopeId: w.b.fileScope.ScopeId,
		Range:   nodeRange(spec),
		Attrs:   map[string]string{"path": path},
	})
	w.b.addEdge(&AstEdge{
		Kind:  "imports",
		From:  scopeRef(w.b.fileScope.ScopeId),
		To:    externalScopeRef(path),
		Attrs: map[string]string{"symbol_id": symID, "language": LanguageGo},
	})
}

func (w *goTSWalker) walkTypes(root *sitter.Node, parentID, parentQualified string) {
	if w.typeScopeByName == nil {
		w.typeScopeByName = map[string]string{}
	}
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		c := root.NamedChild(int(i))
		if c == nil || c.Type() != "type_declaration" {
			continue
		}
		for j := uint32(0); j < c.NamedChildCount(); j++ {
			spec := c.NamedChild(int(j))
			if spec == nil {
				continue
			}
			switch spec.Type() {
			case "type_spec":
				w.emitTypeSpec(spec, parentID, parentQualified, false)
			case "type_alias":
				w.emitTypeSpec(spec, parentID, parentQualified, true)
			}
		}
	}
}

func (w *goTSWalker) emitTypeSpec(spec *sitter.Node, parentID, parentQualified string, isAlias bool) {
	nameNode := findChildByFieldName(spec, "name")
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nodeText(nameNode, w.src))
	if name == "" {
		return
	}
	typeNode := findChildByFieldName(spec, "type")
	kind := ScopeKindClass
	attrs := map[string]string{}
	var embeds []string
	if isAlias {
		attrs["go_type"] = "alias"
	} else if typeNode != nil {
		switch typeNode.Type() {
		case "struct_type":
			attrs["go_type"] = "struct"
			embeds = goExtractStructEmbeds(typeNode, w.src)
		case "interface_type":
			kind = ScopeKindInterface
			attrs["go_type"] = "interface"
			embeds = goExtractInterfaceEmbeds(typeNode, w.src)
		default:
			attrs["go_type"] = "alias"
		}
	}
	if len(embeds) > 0 {
		attrs["embeds"] = strings.Join(embeds, ",")
	}
	scope := &AstScope{
		ScopeKind:     kind,
		Name:          name,
		QualifiedName: joinQualified(parentQualified, name),
		Range:         nodeRange(spec),
		ParentScopeId: parentID,
		Attrs:         attrs,
	}
	id := w.b.addScope(scope)
	w.typeScopeByName[name] = id
	for _, e := range embeds {
		e = strings.TrimSpace(strings.TrimPrefix(e, "*"))
		if e == "" {
			continue
		}
		edgeKind := "extends"
		if kind == ScopeKindClass {
			edgeKind = "embeds"
		}
		w.b.addEdge(&AstEdge{
			Kind: edgeKind,
			From: scopeRef(id),
			To:   externalScopeRef(e),
		})
	}
}

func (w *goTSWalker) walkFunctions(root *sitter.Node, fileParentID, fileQualified string) {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		c := root.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "function_declaration":
			w.emitFunc(c, fileParentID, fileQualified, "", "")
		case "method_declaration":
			recvType, recvName := goReceiverType(c, w.src)
			parentID := fileParentID
			if id, ok := w.typeScopeByName[recvType]; ok {
				parentID = id
			}
			w.emitFunc(c, parentID, fileQualified, recvType, recvName)
		}
	}
}

func (w *goTSWalker) emitFunc(fn *sitter.Node, parentID, fileQualified, recvType, recvName string) {
	nameNode := findChildByFieldName(fn, "name")
	if nameNode == nil {
		return
	}
	name := strings.TrimSpace(nodeText(nameNode, w.src))
	if name == "" {
		return
	}
	params := goExtractParameters(findChildByFieldName(fn, "parameters"), w.src)
	attrs := map[string]string{}
	if recvName != "" {
		attrs["receiver_name"] = recvName
	}
	if recvType != "" {
		attrs["receiver_type"] = recvType
	}
	if results := findChildByFieldName(fn, "result"); results != nil {
		attrs["returns"] = strings.TrimSpace(nodeText(results, w.src))
	}
	qname := name
	if recvType != "" {
		qname = recvType + "." + name
	}
	scope := &AstScope{
		ScopeKind:     ScopeKindMethod,
		Name:          name,
		QualifiedName: joinQualified(fileQualified, qname),
		Parameters:    params,
		Range:         nodeRange(fn),
		ParentScopeId: parentID,
		Attrs:         attrs,
	}
	w.b.addScope(scope)
}

// goReceiverType extracts the receiver type from a
// `method_declaration` node's `receiver` parameter_list. Returns
// ("", "") if no receiver is present.
func goReceiverType(fn *sitter.Node, src []byte) (string, string) {
	recv := findChildByFieldName(fn, "receiver")
	if recv == nil {
		return "", ""
	}
	for i := uint32(0); i < recv.NamedChildCount(); i++ {
		c := recv.NamedChild(int(i))
		if c == nil || c.Type() != "parameter_declaration" {
			continue
		}
		name := ""
		typeText := ""
		if n := findChildByFieldName(c, "name"); n != nil {
			name = strings.TrimSpace(nodeText(n, src))
		}
		if t := findChildByFieldName(c, "type"); t != nil {
			typeText = strings.TrimSpace(nodeText(t, src))
		}
		typeText = strings.TrimPrefix(typeText, "*")
		// Strip generic instantiation `Foo[T]` -> `Foo`.
		if idx := strings.IndexByte(typeText, '['); idx > 0 {
			typeText = typeText[:idx]
		}
		return typeText, name
	}
	return "", ""
}

// goExtractParameters walks a `parameter_list` and returns the
// surface-syntax type for each parameter slot (one entry per
// declared name; bare-type fields produce one entry).
func goExtractParameters(plist *sitter.Node, src []byte) []string {
	if plist == nil {
		return nil
	}
	var out []string
	for i := uint32(0); i < plist.NamedChildCount(); i++ {
		c := plist.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "parameter_declaration":
			typeText := ""
			if t := findChildByFieldName(c, "type"); t != nil {
				typeText = strings.TrimSpace(nodeText(t, src))
			}
			// Count how many names share this type (e.g. `a, b int`
			// is two slots).
			nameCount := 0
			for j := uint32(0); j < c.NamedChildCount(); j++ {
				sub := c.NamedChild(int(j))
				if sub != nil && sub.Type() == "identifier" {
					nameCount++
				}
			}
			if nameCount == 0 {
				nameCount = 1
			}
			for k := 0; k < nameCount; k++ {
				out = append(out, typeText)
			}
		case "variadic_parameter_declaration":
			typeText := ""
			if t := findChildByFieldName(c, "type"); t != nil {
				typeText = "..." + strings.TrimSpace(nodeText(t, src))
			}
			out = append(out, typeText)
		}
	}
	return out
}

// goExtractStructEmbeds returns the type names of every
// anonymous field in a struct (embedding). Named fields and
// methods are not included.
func goExtractStructEmbeds(structNode *sitter.Node, src []byte) []string {
	var fieldList *sitter.Node
	for i := uint32(0); i < structNode.NamedChildCount(); i++ {
		c := structNode.NamedChild(int(i))
		if c != nil && c.Type() == "field_declaration_list" {
			fieldList = c
			break
		}
	}
	if fieldList == nil {
		return nil
	}
	var out []string
	for i := uint32(0); i < fieldList.NamedChildCount(); i++ {
		field := fieldList.NamedChild(int(i))
		if field == nil || field.Type() != "field_declaration" {
			continue
		}
		hasName := false
		for j := uint32(0); j < field.NamedChildCount(); j++ {
			sub := field.NamedChild(int(j))
			if sub != nil && sub.Type() == "field_identifier" {
				hasName = true
				break
			}
		}
		if hasName {
			continue
		}
		if t := findChildByFieldName(field, "type"); t != nil {
			out = append(out, strings.TrimSpace(nodeText(t, src)))
		}
	}
	return out
}

// goExtractInterfaceEmbeds returns the type names of every
// embedded interface (a `type_elem` or bare type identifier
// inside the interface body). Declared method elements are
// skipped.
func goExtractInterfaceEmbeds(ifaceNode *sitter.Node, src []byte) []string {
	var out []string
	for i := uint32(0); i < ifaceNode.NamedChildCount(); i++ {
		c := ifaceNode.NamedChild(int(i))
		if c == nil {
			continue
		}
		switch c.Type() {
		case "type_elem", "type_identifier", "qualified_type":
			out = append(out, strings.TrimSpace(nodeText(c, src)))
		}
	}
	return out
}
