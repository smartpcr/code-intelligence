//go:build !cgo

package parser

import (
	"context"
	"regexp"
	"strings"
)

func init() { registerInDefault(LanguageTypeScript, func() Parser { return &tsParser{} }) }

// tsParser is the v1 lexer fallback for TypeScript / JavaScript
// files (`.ts`, `.tsx`, `.mts`, `.cts`, `.js`, `.jsx`, `.mjs`,
// `.cjs`) when CGO is not available. It walks the source line
// by line and tracks brace nesting to assemble class / interface /
// method scopes. The default CI build uses CGO=1 and routes
// through `tree_sitter_typescript.go` (real
// tree-sitter-typescript grammar).
type tsParser struct{}

func (t *tsParser) Language() string { return LanguageTypeScript }

var (
	tsClassRe     = regexp.MustCompile(`^(?:export\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	tsInterfaceRe = regexp.MustCompile(`^(?:export\s+)?interface\s+([A-Za-z_$][\w$]*)`)
	tsFunctionRe  = regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)\s*[<(]`)
	tsMethodRe    = regexp.MustCompile(`^(?:public\s+|private\s+|protected\s+|static\s+|async\s+|readonly\s+|override\s+|abstract\s+)*([A-Za-z_$][\w$]*)\s*[<(]`)
	tsImportRe    = regexp.MustCompile(`^import\s+(.+?)\s+from\s+['"]([^'"]+)['"]`)
	tsTypeAliasRe = regexp.MustCompile(`^(?:export\s+)?type\s+([A-Za-z_$][\w$]*)\s*=`)
)

// tsFrame tracks an open class / interface / method body while
// we walk the source.
type tsFrame struct {
	id            string
	kind          ScopeKind
	braceDepthAt  int
	qualifiedName string
}

func (t *tsParser) Parse(ctx context.Context, path string, content []byte) (*AstFile, error) {
	if err := validateContent(path, content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

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

	cursor := newLineCursor(content)
	stack := []tsFrame{{id: b.fileScope.ScopeId, kind: ScopeKindFile, braceDepthAt: 0, qualifiedName: moduleName}}
	braceDepth := 0

	for cursor.next() {
		raw := stripComment(cursor.currentLine, "//")
		stripped := strings.TrimSpace(raw)
		if stripped == "" {
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		parent := stack[len(stack)-1]
		// Pop frames whose body has closed (braceDepth <
		// frame.braceDepthAt would never happen; depth == frame
		// means body is closed BEFORE this line).
		for len(stack) > 1 && braceDepth < stack[len(stack)-1].braceDepthAt {
			stack = stack[:len(stack)-1]
			parent = stack[len(stack)-1]
		}

		if m := tsImportRe.FindStringSubmatch(stripped); m != nil {
			symID := b.addSymbol(&AstSymbol{
				Name:    strings.TrimSpace(m[1]),
				Kind:    "import",
				ScopeId: b.fileScope.ScopeId,
				Range:   cursor.lineRange(0, len(cursor.currentLine)),
				Attrs:   map[string]string{"from": m[2]},
			})
			b.addEdge(&AstEdge{
				Kind:  "imports",
				From:  scopeRef(b.fileScope.ScopeId),
				To:    externalScopeRef(m[2]),
				Attrs: map[string]string{"symbol_id": symID, "language": LanguageTypeScript},
			})
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		if m := tsClassRe.FindStringSubmatch(stripped); m != nil {
			attrs := map[string]string{}
			if strings.Contains(stripped, "extends ") {
				attrs["extends"] = extractAfterKeyword(stripped, "extends")
			}
			if strings.Contains(stripped, "implements ") {
				attrs["implements"] = extractAfterKeyword(stripped, "implements")
			}
			if strings.Contains(stripped, "abstract ") {
				attrs["abstract"] = "true"
			}
			scope := &AstScope{
				ScopeKind:     ScopeKindClass,
				Name:          m[1],
				QualifiedName: joinQualified(parent.qualifiedName, m[1]),
				Range:         cursor.lineRange(0, len(cursor.currentLine)),
				ParentScopeId: parent.id,
				Attrs:         attrs,
			}
			id := b.addScope(scope)
			if ext, ok := attrs["extends"]; ok {
				for _, ref := range splitTopLevelCommas(ext) {
					ref = strings.TrimSpace(ref)
					if ref == "" {
						continue
					}
					b.addEdge(&AstEdge{Kind: "extends", From: scopeRef(id), To: externalScopeRef(ref)})
				}
			}
			if impls, ok := attrs["implements"]; ok {
				for _, ref := range splitTopLevelCommas(impls) {
					ref = strings.TrimSpace(ref)
					if ref == "" {
						continue
					}
					b.addEdge(&AstEdge{Kind: "implements", From: scopeRef(id), To: externalScopeRef(ref)})
				}
			}
			depthEntering := braceDepth + countBraces(cursor.currentLine)
			stack = append(stack, tsFrame{id: id, kind: ScopeKindClass, braceDepthAt: depthEntering, qualifiedName: scope.QualifiedName})
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		if m := tsInterfaceRe.FindStringSubmatch(stripped); m != nil {
			attrs := map[string]string{}
			if strings.Contains(stripped, "extends ") {
				attrs["extends"] = extractAfterKeyword(stripped, "extends")
			}
			scope := &AstScope{
				ScopeKind:     ScopeKindInterface,
				Name:          m[1],
				QualifiedName: joinQualified(parent.qualifiedName, m[1]),
				Range:         cursor.lineRange(0, len(cursor.currentLine)),
				ParentScopeId: parent.id,
				Attrs:         attrs,
			}
			id := b.addScope(scope)
			if ext, ok := attrs["extends"]; ok {
				for _, ref := range splitTopLevelCommas(ext) {
					ref = strings.TrimSpace(ref)
					if ref == "" {
						continue
					}
					b.addEdge(&AstEdge{Kind: "extends", From: scopeRef(id), To: externalScopeRef(ref)})
				}
			}
			depthEntering := braceDepth + countBraces(cursor.currentLine)
			stack = append(stack, tsFrame{id: id, kind: ScopeKindInterface, braceDepthAt: depthEntering, qualifiedName: scope.QualifiedName})
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		if m := tsTypeAliasRe.FindStringSubmatch(stripped); m != nil {
			b.addSymbol(&AstSymbol{
				Name:    m[1],
				Kind:    "type_alias",
				ScopeId: parent.id,
				Range:   cursor.lineRange(0, len(cursor.currentLine)),
			})
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		if m := tsFunctionRe.FindStringSubmatch(stripped); m != nil {
			params, _ := extractParenList(stripped)
			scope := &AstScope{
				ScopeKind:     ScopeKindMethod,
				Name:          m[1],
				QualifiedName: joinQualified(parent.qualifiedName, m[1]),
				Parameters:    tsParameterTypes(params),
				Range:         cursor.lineRange(0, len(cursor.currentLine)),
				ParentScopeId: parent.id,
				Attrs:         tsFunctionAttrs(stripped),
			}
			id := b.addScope(scope)
			depthEntering := braceDepth + countBraces(cursor.currentLine)
			stack = append(stack, tsFrame{id: id, kind: ScopeKindMethod, braceDepthAt: depthEntering, qualifiedName: scope.QualifiedName})
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		// Inside a class / interface body, treat top-level
		// method declarations as method scopes.
		if (parent.kind == ScopeKindClass || parent.kind == ScopeKindInterface) && tsLooksLikeMethod(stripped) {
			if m := tsMethodRe.FindStringSubmatch(stripped); m != nil {
				params, _ := extractParenList(stripped)
				attrs := tsFunctionAttrs(stripped)
				if parent.kind == ScopeKindInterface {
					attrs["interface_member"] = "true"
				}
				scope := &AstScope{
					ScopeKind:     ScopeKindMethod,
					Name:          m[1],
					QualifiedName: joinQualified(parent.qualifiedName, m[1]),
					Parameters:    tsParameterTypes(params),
					Range:         cursor.lineRange(0, len(cursor.currentLine)),
					ParentScopeId: parent.id,
					Attrs:         attrs,
				}
				id := b.addScope(scope)
				// A method-with-body opens a new brace frame;
				// an interface signature like `foo(): void;`
				// has no body and MUST NOT push, otherwise
				// subsequent methods on the same interface are
				// silently parented under the first (architecture
				// review #5 from iter-1).
				lineBraces := countBraces(cursor.currentLine)
				if lineBraces > 0 {
					depthEntering := braceDepth + lineBraces
					stack = append(stack, tsFrame{id: id, kind: ScopeKindMethod, braceDepthAt: depthEntering, qualifiedName: scope.QualifiedName})
				}
				braceDepth += lineBraces
				continue
			}
		}

		braceDepth += countBraces(cursor.currentLine)
	}

	return b.build(LanguageTypeScript, path, content), nil
}

// countBraces returns net `{` minus `}` outside strings.
func countBraces(line string) int {
	in := false
	var quote byte
	delta := 0
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if in {
			if ch == '\\' {
				i++
				continue
			}
			if ch == quote {
				in = false
			}
			continue
		}
		switch ch {
		case '"', '\'', '`':
			in = true
			quote = ch
		case '{':
			delta++
		case '}':
			delta--
		}
	}
	return delta
}

// extractAfterKeyword returns the substring following the first
// occurrence of `<keyword> ` in `line`, trimmed at the first
// `{`. Used to capture `extends`/`implements` clauses.
func extractAfterKeyword(line, keyword string) string {
	idx := strings.Index(line, keyword+" ")
	if idx < 0 {
		return ""
	}
	tail := line[idx+len(keyword)+1:]
	if brace := strings.IndexByte(tail, '{'); brace >= 0 {
		tail = tail[:brace]
	}
	return strings.TrimSpace(tail)
}

// tsParameterTypes turns a TS parameter list (the bytes
// between parens) into a slice of types. Each parameter looks
// like `name: Type` or just `name`; we keep the type when
// present, the name when not.
func tsParameterTypes(params string) []string {
	parts := splitTopLevelCommas(params)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i := strings.IndexByte(p, ':'); i >= 0 {
			out = append(out, strings.TrimSpace(p[i+1:]))
		} else {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

// tsFunctionAttrs picks out the small set of modifiers Stage
// 2.4 wants on method scopes.
func tsFunctionAttrs(line string) map[string]string {
	out := map[string]string{}
	for _, kw := range []string{"async", "static", "public", "private", "protected", "override", "readonly", "abstract"} {
		if strings.HasPrefix(line, kw+" ") || strings.Contains(line, " "+kw+" ") {
			out[kw] = "true"
		}
	}
	return out
}

// tsLooksLikeMethod skips lines that are clearly fields,
// imports, statements -- not class members.
func tsLooksLikeMethod(line string) bool {
	if strings.HasPrefix(line, "//") || strings.HasPrefix(line, "/*") {
		return false
	}
	if strings.HasPrefix(line, "return") || strings.HasPrefix(line, "if") ||
		strings.HasPrefix(line, "for") || strings.HasPrefix(line, "while") ||
		strings.HasPrefix(line, "switch") || strings.HasPrefix(line, "throw") ||
		strings.HasPrefix(line, "let ") || strings.HasPrefix(line, "const ") ||
		strings.HasPrefix(line, "var ") {
		return false
	}
	// A method declaration line ends with `{`, `;`, or has
	// `(` before any `=`.
	paren := strings.IndexByte(line, '(')
	eq := strings.IndexByte(line, '=')
	if paren < 0 {
		return false
	}
	if eq >= 0 && eq < paren {
		return false
	}
	return true
}
