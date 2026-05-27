//go:build !cgo

package parser

import (
	"context"
	"regexp"
	"strings"
)

func init() { registerInDefault(LanguagePython, func() Parser { return &pythonParser{} }) }

// pythonParser is the v1 lexer fallback for `.py`, `.pyi`, and
// `#!/usr/bin/env python*` scripts when CGO is not available.
// It tracks indentation to derive parent/child relationships
// and emits the canonical scope/symbol/edge shape that
// `tree_sitter_python.go` produces under `//go:build cgo`.
//
// The lexer adapter is the supported fallback for CGO-disabled
// builds (e.g. cross-compile, sandboxed containers without a C
// toolchain). The default code path in CI runs with CGO=1 and
// uses the real tree-sitter grammar.
type pythonParser struct{}

func (p *pythonParser) Language() string { return LanguagePython }

var (
	pyClassRe = regexp.MustCompile(`^class\s+([A-Za-z_][A-Za-z0-9_]*)\s*(?:\(([^)]*)\))?\s*:`)
	pyFuncRe  = regexp.MustCompile(`^(?:async\s+)?def\s+([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
	pyDecRe   = regexp.MustCompile(`^@([A-Za-z_][A-Za-z0-9_.]*)`)
	pyImpRe   = regexp.MustCompile(`^(?:from\s+([A-Za-z_][A-Za-z0-9_.]*)\s+)?import\s+(.+)$`)
)

// pyScopeFrame tracks an open Python scope while we walk the
// file. `indent` is the column at which the declaration was
// found; `id` is the synthetic intra-file ID assigned by the
// scopeBuilder.
type pyScopeFrame struct {
	id            string
	indent        int
	qualifiedName string
}

func (p *pythonParser) Parse(ctx context.Context, path string, content []byte) (*AstFile, error) {
	if err := validateContent(path, content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	lineCount, byteCount := lineCounts(content)
	b := newScopeBuilder(path, lineCount, byteCount)
	module := strings.TrimSuffix(b.fileScope.Name, ".py")
	module = strings.TrimSuffix(module, ".pyi")
	b.fileScope.QualifiedName = module
	b.fileScope.Attrs = map[string]string{"module": module}

	cursor := newLineCursor(content)
	stack := []pyScopeFrame{{id: b.fileScope.ScopeId, indent: -1, qualifiedName: module}}
	pendingDecorators := []string{}
	for cursor.next() {
		raw := stripComment(cursor.currentLine, "#")
		stripped, leading := strings.TrimSpace(raw), leadingIndent(cursor.currentLine)
		if stripped == "" {
			continue
		}

		// Pop frames whose indent is >= current. Top-level
		// declarations have leading == 0 so we pop every
		// non-file frame.
		for len(stack) > 1 && leading <= stack[len(stack)-1].indent {
			stack = stack[:len(stack)-1]
		}
		parent := stack[len(stack)-1]

		if m := pyDecRe.FindStringSubmatch(stripped); m != nil {
			pendingDecorators = append(pendingDecorators, m[1])
			continue
		}

		if m := pyClassRe.FindStringSubmatch(stripped); m != nil {
			name := m[1]
			bases := strings.TrimSpace(m[2])
			kind := ScopeKindClass
			attrs := map[string]string{}
			if bases != "" {
				attrs["bases"] = bases
				if looksLikeABC(bases) {
					kind = ScopeKindInterface
				}
			}
			if len(pendingDecorators) > 0 {
				attrs["decorators"] = strings.Join(pendingDecorators, ",")
				pendingDecorators = pendingDecorators[:0]
			}
			scope := &AstScope{
				ScopeKind:     kind,
				Name:          name,
				QualifiedName: joinQualified(parent.qualifiedName, name),
				Range:         cursor.lineRange(leading, len(cursor.currentLine)),
				ParentScopeId: parent.id,
				Attrs:         attrs,
			}
			id := b.addScope(scope)
			// Emit extends edges for declared base classes so
			// inheritance recipes can walk Python class
			// hierarchies without re-tokenising.
			if bases != "" {
				for _, base := range splitTopLevelCommas(bases) {
					base = strings.TrimSpace(base)
					if base == "" || base == "object" {
						continue
					}
					edgeKind := "extends"
					if kind == ScopeKindClass && looksLikeABC(base) {
						edgeKind = "implements"
					}
					b.addEdge(&AstEdge{
						Kind: edgeKind,
						From: scopeRef(id),
						To:   externalScopeRef(base),
					})
				}
			}
			stack = append(stack, pyScopeFrame{id: id, indent: leading, qualifiedName: scope.QualifiedName})
			continue
		}

		if m := pyFuncRe.FindStringSubmatch(stripped); m != nil {
			name := m[1]
			params, _ := extractParenList(stripped)
			paramList := splitTopLevelCommas(params)
			attrs := map[string]string{}
			if strings.HasPrefix(stripped, "async") {
				attrs["async"] = "true"
			}
			if len(pendingDecorators) > 0 {
				attrs["decorators"] = strings.Join(pendingDecorators, ",")
				pendingDecorators = pendingDecorators[:0]
			}
			scope := &AstScope{
				ScopeKind:     ScopeKindMethod,
				Name:          name,
				QualifiedName: joinQualified(parent.qualifiedName, name),
				Parameters:    paramList,
				Range:         cursor.lineRange(leading, len(cursor.currentLine)),
				ParentScopeId: parent.id,
				Attrs:         attrs,
			}
			id := b.addScope(scope)
			stack = append(stack, pyScopeFrame{id: id, indent: leading, qualifiedName: scope.QualifiedName})
			continue
		}

		if m := pyImpRe.FindStringSubmatch(stripped); m != nil {
			from := m[1]
			imports := splitTopLevelCommas(m[2])
			for _, name := range imports {
				name = strings.TrimSpace(name)
				if name == "" {
					continue
				}
				display := name
				if from != "" {
					display = from + "." + name
				}
				symID := b.addSymbol(&AstSymbol{
					Name:    display,
					Kind:    "import",
					ScopeId: b.fileScope.ScopeId,
					Range:   cursor.lineRange(leading, len(cursor.currentLine)),
				})
				target := display
				if from != "" {
					target = from
				}
				b.addEdge(&AstEdge{
					Kind:  "imports",
					From:  scopeRef(b.fileScope.ScopeId),
					To:    externalScopeRef(target),
					Attrs: map[string]string{"symbol_id": symID, "language": LanguagePython},
				})
			}
			continue
		}

		// Reset pending decorators on any non-class/non-func
		// line so a stray `@something` doesn't bleed into the
		// next declaration.
		if len(pendingDecorators) > 0 && !strings.HasPrefix(stripped, "@") {
			pendingDecorators = pendingDecorators[:0]
		}
	}

	return b.build(LanguagePython, path, content), nil
}

// leadingIndent returns the byte-count of leading spaces/tabs
// on `line`.
func leadingIndent(line string) int {
	for i := 0; i < len(line); i++ {
		if line[i] != ' ' && line[i] != '\t' {
			return i
		}
	}
	return len(line)
}
