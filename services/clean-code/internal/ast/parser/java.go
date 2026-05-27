//go:build !cgo

package parser

import (
	"context"
	"regexp"
	"strings"
)

func init() { registerInDefault(LanguageJava, func() Parser { return &javaParser{} }) }

// javaParser is the v1 lexer fallback for `.java` files when
// CGO is not available. Walks the source line by line + brace
// depth to extract package / class / interface / method scopes.
// The default CI build uses CGO=1 and routes through
// `tree_sitter_java.go` (real tree-sitter-java grammar).
type javaParser struct{}

func (j *javaParser) Language() string { return LanguageJava }

var (
	javaPackageRe   = regexp.MustCompile(`^package\s+([\w.]+)\s*;`)
	javaImportRe    = regexp.MustCompile(`^import\s+(static\s+)?([\w.\*]+)\s*;`)
	javaClassRe     = regexp.MustCompile(`(?:^|\s)(?:public\s+|private\s+|protected\s+|static\s+|final\s+|abstract\s+|sealed\s+|non-sealed\s+)*class\s+([A-Za-z_$][\w$]*)`)
	javaInterfaceRe = regexp.MustCompile(`(?:^|\s)(?:public\s+|private\s+|protected\s+|abstract\s+|sealed\s+|non-sealed\s+)*interface\s+([A-Za-z_$][\w$]*)`)
	javaEnumRe      = regexp.MustCompile(`(?:^|\s)(?:public\s+|private\s+|protected\s+|static\s+|final\s+)*enum\s+([A-Za-z_$][\w$]*)`)
	javaMethodRe    = regexp.MustCompile(`^(?:public\s+|private\s+|protected\s+|static\s+|final\s+|abstract\s+|synchronized\s+|native\s+|default\s+|@[\w.]+\s+)*(?:<[^>]+>\s+)?(?:[\w<>,?\[\]\s]+?\s+)?([A-Za-z_$][\w$]*)\s*\(`)
)

type javaFrame struct {
	id            string
	kind          ScopeKind
	braceDepthAt  int
	qualifiedName string
}

func (j *javaParser) Parse(ctx context.Context, path string, content []byte) (*AstFile, error) {
	if err := validateContent(path, content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lineCount, byteCount := lineCounts(content)
	b := newScopeBuilder(path, lineCount, byteCount)
	pkg := ""

	cursor := newLineCursor(content)
	// Pre-scan for the `package` line so we can stamp the file
	// scope's qualified name properly. We then re-iterate from
	// the start for the actual scope walk.
	for cursor.next() {
		stripped := strings.TrimSpace(stripComment(cursor.currentLine, "//"))
		if m := javaPackageRe.FindStringSubmatch(stripped); m != nil {
			pkg = m[1]
			break
		}
		if stripped != "" && !strings.HasPrefix(stripped, "/*") && !strings.HasPrefix(stripped, "*") {
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
			Range:         astRangeAt(0, 0, 1, 1, 1, 1),
			Attrs:         map[string]string{"language": LanguageJava},
		}
		pkgScopeID := b.addScope(pkgScope)
		b.fileScope.ParentScopeId = pkgScopeID
	}

	cursor = newLineCursor(content)
	stack := []javaFrame{{id: b.fileScope.ScopeId, kind: ScopeKindFile, braceDepthAt: 0, qualifiedName: b.fileScope.QualifiedName}}
	braceDepth := 0
	for cursor.next() {
		raw := stripComment(cursor.currentLine, "//")
		stripped := strings.TrimSpace(raw)
		if stripped == "" || strings.HasPrefix(stripped, "*") || strings.HasPrefix(stripped, "/*") {
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		// Pop closed frames.
		for len(stack) > 1 && braceDepth < stack[len(stack)-1].braceDepthAt {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]

		if m := javaImportRe.FindStringSubmatch(stripped); m != nil {
			symID := b.addSymbol(&AstSymbol{
				Name:    m[2],
				Kind:    "import",
				ScopeId: b.fileScope.ScopeId,
				Range:   cursor.lineRange(0, len(cursor.currentLine)),
				Attrs:   map[string]string{"static": boolStr(m[1] != "")},
			})
			b.addEdge(&AstEdge{
				Kind:  "imports",
				From:  scopeRef(b.fileScope.ScopeId),
				To:    externalScopeRef(m[2]),
				Attrs: map[string]string{"symbol_id": symID, "language": LanguageJava},
			})
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		if m := javaClassRe.FindStringSubmatch(stripped); m != nil {
			scope := &AstScope{
				ScopeKind:     ScopeKindClass,
				Name:          m[1],
				QualifiedName: joinQualified(parent.qualifiedName, m[1]),
				Range:         cursor.lineRange(0, len(cursor.currentLine)),
				ParentScopeId: parent.id,
				Attrs:         javaClassAttrs(stripped),
			}
			id := b.addScope(scope)
			javaEmitInheritanceEdges(b, id, scope.Attrs)
			depthEntering := braceDepth + countBraces(cursor.currentLine)
			stack = append(stack, javaFrame{id: id, kind: ScopeKindClass, braceDepthAt: depthEntering, qualifiedName: scope.QualifiedName})
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		if m := javaInterfaceRe.FindStringSubmatch(stripped); m != nil {
			scope := &AstScope{
				ScopeKind:     ScopeKindInterface,
				Name:          m[1],
				QualifiedName: joinQualified(parent.qualifiedName, m[1]),
				Range:         cursor.lineRange(0, len(cursor.currentLine)),
				ParentScopeId: parent.id,
				Attrs:         javaClassAttrs(stripped),
			}
			id := b.addScope(scope)
			javaEmitInheritanceEdges(b, id, scope.Attrs)
			depthEntering := braceDepth + countBraces(cursor.currentLine)
			stack = append(stack, javaFrame{id: id, kind: ScopeKindInterface, braceDepthAt: depthEntering, qualifiedName: scope.QualifiedName})
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		if m := javaEnumRe.FindStringSubmatch(stripped); m != nil {
			scope := &AstScope{
				ScopeKind:     ScopeKindClass,
				Name:          m[1],
				QualifiedName: joinQualified(parent.qualifiedName, m[1]),
				Range:         cursor.lineRange(0, len(cursor.currentLine)),
				ParentScopeId: parent.id,
				Attrs:         map[string]string{"java_type": "enum"},
			}
			id := b.addScope(scope)
			depthEntering := braceDepth + countBraces(cursor.currentLine)
			stack = append(stack, javaFrame{id: id, kind: ScopeKindClass, braceDepthAt: depthEntering, qualifiedName: scope.QualifiedName})
			braceDepth += countBraces(cursor.currentLine)
			continue
		}

		// Method recognition: must be inside a class / interface
		// body to count.
		if parent.kind == ScopeKindClass || parent.kind == ScopeKindInterface {
			if m := javaMethodRe.FindStringSubmatch(stripped); m != nil && javaLooksLikeMethod(stripped) {
				params, _ := extractParenList(stripped)
				scope := &AstScope{
					ScopeKind:     ScopeKindMethod,
					Name:          m[1],
					QualifiedName: joinQualified(parent.qualifiedName, m[1]),
					Parameters:    javaParameterTypes(params),
					Range:         cursor.lineRange(0, len(cursor.currentLine)),
					ParentScopeId: parent.id,
					Attrs:         javaMethodAttrs(stripped, parent.kind),
				}
				id := b.addScope(scope)
				// Abstract / interface methods end in `;` (no
				// body); they MUST NOT push a brace frame
				// otherwise siblings in the same interface get
				// silently parented under the first method
				// (architecture review #5 from iter-1).
				lineBraces := countBraces(cursor.currentLine)
				if lineBraces > 0 {
					depthEntering := braceDepth + lineBraces
					stack = append(stack, javaFrame{id: id, kind: ScopeKindMethod, braceDepthAt: depthEntering, qualifiedName: scope.QualifiedName})
				}
				braceDepth += lineBraces
				continue
			}
		}

		braceDepth += countBraces(cursor.currentLine)
	}

	return b.build(LanguageJava, path, content), nil
}

func javaClassAttrs(line string) map[string]string {
	out := map[string]string{}
	if strings.Contains(line, " extends ") {
		out["extends"] = extractAfterKeyword(line, "extends")
	}
	if strings.Contains(line, " implements ") {
		out["implements"] = extractAfterKeyword(line, "implements")
	}
	for _, kw := range []string{"public", "private", "protected", "abstract", "final", "static", "sealed"} {
		if strings.Contains(" "+line+" ", " "+kw+" ") {
			out[kw] = "true"
		}
	}
	return out
}

// javaEmitInheritanceEdges turns `extends`/`implements` clauses
// from a class/interface declaration's attrs into AstEdges so
// recipes can compute depth-of-inheritance and coupling-between-
// objects without re-tokenising the source.
func javaEmitInheritanceEdges(b *scopeBuilder, scopeID string, attrs map[string]string) {
	if ext, ok := attrs["extends"]; ok && ext != "" {
		for _, ref := range splitTopLevelCommas(ext) {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			b.addEdge(&AstEdge{Kind: "extends", From: scopeRef(scopeID), To: externalScopeRef(ref)})
		}
	}
	if impls, ok := attrs["implements"]; ok && impls != "" {
		for _, ref := range splitTopLevelCommas(impls) {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			b.addEdge(&AstEdge{Kind: "implements", From: scopeRef(scopeID), To: externalScopeRef(ref)})
		}
	}
}

func javaMethodAttrs(line string, parentKind ScopeKind) map[string]string {
	out := map[string]string{}
	for _, kw := range []string{"public", "private", "protected", "static", "abstract", "final", "synchronized", "default", "native"} {
		if strings.Contains(" "+line+" ", " "+kw+" ") {
			out[kw] = "true"
		}
	}
	if parentKind == ScopeKindInterface {
		out["interface_member"] = "true"
	}
	if strings.Contains(line, " throws ") {
		out["throws"] = extractAfterKeyword(line, "throws")
	}
	return out
}

// javaParameterTypes extracts parameter types from a Java
// method parameter list. Each parameter is `Type name` (with
// possible `final`/annotations prefix); we keep the type
// portion (everything up to the last identifier).
func javaParameterTypes(params string) []string {
	parts := splitTopLevelCommas(params)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = strings.TrimPrefix(p, "final ")
		// Strip annotations like `@Nullable Foo bar`.
		for strings.HasPrefix(p, "@") {
			if sp := strings.IndexByte(p, ' '); sp >= 0 {
				p = strings.TrimSpace(p[sp+1:])
			} else {
				break
			}
		}
		if sp := strings.LastIndexByte(p, ' '); sp >= 0 {
			out = append(out, strings.TrimSpace(p[:sp]))
		} else {
			out = append(out, p)
		}
	}
	return out
}

// javaLooksLikeMethod filters out field declarations, control
// flow lines, and `try`/`catch` blocks that match the broad
// method regex.
func javaLooksLikeMethod(line string) bool {
	if strings.HasSuffix(line, ";") && !strings.HasSuffix(line, ");") {
		// Almost certainly a field declaration with `(` from
		// a method-call initialiser.
		return false
	}
	for _, kw := range []string{"return", "if", "for", "while", "switch", "throw", "try", "catch", "synchronized"} {
		if strings.HasPrefix(line, kw+"(") || strings.HasPrefix(line, kw+" (") || strings.HasPrefix(line, kw+" {") {
			return false
		}
	}
	return true
}
