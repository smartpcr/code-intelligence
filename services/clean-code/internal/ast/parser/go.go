//go:build !cgo

package parser

import (
	"context"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"strings"
)

func init() { registerInDefault(LanguageGo, func() Parser { return &goParser{} }) }

// goParser is the v1 `!cgo` fallback adapter for `*.go` files.
// It uses Go's standard `go/parser` package which is full-
// fidelity at the scope level (file/package/interface/method)
// and requires no C toolchain. Under `//go:build cgo` the
// `tree_sitter_go.go` adapter is used instead -- the stdlib
// parser is retained here so cross-compile and sandboxed
// builds without a C compiler still satisfy the `Parser`
// interface and the same edge / parenting test assertions.
type goParser struct{}

func (g *goParser) Language() string { return LanguageGo }

func (g *goParser) Parse(ctx context.Context, path string, content []byte) (*AstFile, error) {
	if err := validateContent(path, content); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	fset := token.NewFileSet()
	file, parseErr := goparser.ParseFile(fset, path, content, goparser.ParseComments|goparser.SkipObjectResolution)

	lineCount, byteCount := lineCounts(content)
	b := newScopeBuilder(path, lineCount, byteCount)

	if file == nil {
		// Even on a hard parse error, emit a degraded `AstFile`
		// with the file scope populated so downstream stages can
		// distinguish "parser exploded" from "parser silently
		// returned nil" (tech-spec Sec 9.13 / Sec 9.14).
		out := b.build(LanguageGo, path, content)
		if parseErr != nil {
			out.DegradedReason = "go_parser_error"
		} else {
			out.DegradedReason = "go_parser_returned_nil"
		}
		return out, nil
	}

	pkgName := ""
	if file.Name != nil {
		pkgName = file.Name.Name
	}
	if pkgName != "" {
		b.fileScope.Attrs = map[string]string{"package": pkgName}
		b.fileScope.QualifiedName = joinQualified(pkgName, b.fileScope.Name)
	}

	pkgScopeID := ""
	if pkgName != "" {
		pkgScope := &AstScope{
			ScopeKind:     ScopeKindPackage,
			Name:          pkgName,
			QualifiedName: pkgName,
			Range:         astRangeFromPos(fset, file.Package, file.Package),
			Attrs:         map[string]string{"language": LanguageGo},
		}
		pkgScopeID = b.addScope(pkgScope)
		b.fileScope.ParentScopeId = pkgScopeID
	}

	// Imports as symbols hanging off the file scope -- recipes
	// like fan_out consume these.
	for _, imp := range file.Imports {
		impPath := ""
		if imp.Path != nil {
			impPath = strings.Trim(imp.Path.Value, "\"")
		}
		name := impPath
		if imp.Name != nil {
			name = imp.Name.Name
		}
		symID := b.addSymbol(&AstSymbol{
			Name:    name,
			Kind:    "import",
			ScopeId: b.fileScope.ScopeId,
			Range:   astRangeFromPos(fset, imp.Pos(), imp.End()),
			Attrs:   map[string]string{"path": impPath},
		})
		// Emit a typed edge from this file to the imported
		// package so downstream recipes (fan_out, cycles) can
		// walk imports without re-parsing symbols.
		b.addEdge(&AstEdge{
			Kind:  "imports",
			From:  scopeRef(b.fileScope.ScopeId),
			To:    externalScopeRef(impPath),
			Attrs: map[string]string{"symbol_id": symID, "language": LanguageGo},
		})
	}

	// Two-pass walk: emit every type declaration first so
	// receiver methods can parent themselves under the struct /
	// interface scope rather than under the file scope (Go
	// allows function decls before their receiver type appears
	// textually).
	typeScopeByName := map[string]string{}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name == nil {
				continue
			}
			kind := ScopeKindClass
			attrs := map[string]string{}
			switch tn := ts.Type.(type) {
			case *ast.InterfaceType:
				kind = ScopeKindInterface
				attrs["go_type"] = "interface"
				// Embedded interface members become an
				// `extends` edge later.
				if tn.Methods != nil {
					var embeds []string
					for _, m := range tn.Methods.List {
						if len(m.Names) == 0 {
							embeds = append(embeds, typeString(m.Type))
						}
					}
					if len(embeds) > 0 {
						attrs["embeds"] = strings.Join(embeds, ",")
					}
				}
			case *ast.StructType:
				attrs["go_type"] = "struct"
				if tn.Fields != nil {
					var embeds []string
					for _, f := range tn.Fields.List {
						if len(f.Names) == 0 {
							embeds = append(embeds, typeString(f.Type))
						}
					}
					if len(embeds) > 0 {
						attrs["embeds"] = strings.Join(embeds, ",")
					}
				}
			default:
				attrs["go_type"] = "alias"
			}
			scope := &AstScope{
				ScopeKind:     kind,
				Name:          ts.Name.Name,
				QualifiedName: joinQualified(b.fileScope.QualifiedName, ts.Name.Name),
				Range:         astRangeFromPos(fset, ts.Pos(), ts.End()),
				ParentScopeId: b.fileScope.ScopeId,
				Attrs:         attrs,
			}
			id := b.addScope(scope)
			typeScopeByName[ts.Name.Name] = id

			// Emit `extends` edges for interface/struct
			// embedding so recipes can compute depth-of-
			// inheritance / fan-in without re-walking types.
			if embeds, ok := attrs["embeds"]; ok && embeds != "" {
				for _, e := range strings.Split(embeds, ",") {
					e = strings.TrimSpace(stripReceiverPointer(e))
					if e == "" {
						continue
					}
					edgeKind := "extends"
					if kind == ScopeKindInterface {
						edgeKind = "extends"
					}
					if kind == ScopeKindClass {
						// Embedded interface in a struct -> implements.
						edgeKind = "embeds"
					}
					b.addEdge(&AstEdge{
						Kind: edgeKind,
						From: scopeRef(id),
						To:   externalScopeRef(e),
					})
				}
			}
		}
	}

	for _, decl := range file.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name == nil {
			continue
		}
		recv := ""
		recvType := ""
		parentID := b.fileScope.ScopeId
		if fd.Recv != nil && len(fd.Recv.List) > 0 {
			recvType = stripReceiverPointer(typeString(fd.Recv.List[0].Type))
			if len(fd.Recv.List[0].Names) > 0 {
				recv = fd.Recv.List[0].Names[0].Name
			}
			if id, ok := typeScopeByName[recvType]; ok {
				parentID = id
			}
		}
		params := goFuncParameters(fd.Type)
		scope := &AstScope{
			ScopeKind:     ScopeKindMethod,
			Name:          fd.Name.Name,
			QualifiedName: joinQualified(b.fileScope.QualifiedName, methodQualifiedName(recvType, fd.Name.Name)),
			Parameters:    params,
			Range:         astRangeFromPos(fset, fd.Pos(), fd.End()),
			ParentScopeId: parentID,
			Attrs:         goFuncAttrs(fd, recv, recvType),
		}
		b.addScope(scope)
	}

	return b.build(LanguageGo, path, content), nil
}

// methodQualifiedName joins a receiver type and a function name
// into the qualified-name suffix Stage 2.2 will consume. For
// free functions (no receiver) the receiver part is empty.
func methodQualifiedName(receiverType, funcName string) string {
	if receiverType == "" {
		return funcName
	}
	return receiverType + "." + funcName
}

// goFuncParameters extracts the parameter type list from a
// `*ast.FuncType` as the slice of surface-syntax types Stage
// 2.2 will join into a `canonical_signature`.
func goFuncParameters(ft *ast.FuncType) []string {
	if ft == nil || ft.Params == nil {
		return nil
	}
	out := make([]string, 0, len(ft.Params.List))
	for _, field := range ft.Params.List {
		ts := typeString(field.Type)
		switch n := len(field.Names); {
		case n == 0:
			out = append(out, ts)
		default:
			for i := 0; i < n; i++ {
				out = append(out, ts)
			}
		}
	}
	return out
}

// goFuncAttrs builds the `Attrs` map for a function scope.
func goFuncAttrs(d *ast.FuncDecl, recv, recvType string) map[string]string {
	out := map[string]string{}
	if recv != "" {
		out["receiver_name"] = recv
	}
	if recvType != "" {
		out["receiver_type"] = recvType
	}
	if d.Type != nil && d.Type.Results != nil {
		var rs []string
		for _, r := range d.Type.Results.List {
			rs = append(rs, typeString(r.Type))
		}
		if len(rs) > 0 {
			out["returns"] = strings.Join(rs, ",")
		}
	}
	return out
}

// stripReceiverPointer turns `*Foo` into `Foo` so the receiver
// type used for `canonical_signature` matches the type-decl
// scope's `Name`.
func stripReceiverPointer(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "*")
}

// typeString renders an `ast.Expr` representing a type back
// into Go surface syntax. Implementation is intentionally
// minimal -- it handles the shapes the v1 recipes touch
// (identifiers, qualified identifiers, pointers, slices,
// generics, ellipsis) and falls back to `<expr>` for everything
// else.
func typeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.ArrayType:
		return "[]" + typeString(t.Elt)
	case *ast.MapType:
		return "map[" + typeString(t.Key) + "]" + typeString(t.Value)
	case *ast.ChanType:
		return "chan " + typeString(t.Value)
	case *ast.Ellipsis:
		return "..." + typeString(t.Elt)
	case *ast.IndexExpr:
		return typeString(t.X) + "[" + typeString(t.Index) + "]"
	case *ast.IndexListExpr:
		parts := make([]string, 0, len(t.Indices))
		for _, idx := range t.Indices {
			parts = append(parts, typeString(idx))
		}
		return typeString(t.X) + "[" + strings.Join(parts, ",") + "]"
	case *ast.FuncType:
		return "func"
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{}"
	default:
		return "<expr>"
	}
}

// astRangeFromPos converts a `(start, end)` Go-token-position
// pair into an `*AstRange`. Falls back to a synthetic zero
// range when positions are invalid.
func astRangeFromPos(fset *token.FileSet, start, end token.Pos) *AstRange {
	if !start.IsValid() {
		return &AstRange{}
	}
	startPos := fset.Position(start)
	endPos := startPos
	if end.IsValid() {
		endPos = fset.Position(end)
	}
	startByte := startPos.Offset
	if startByte < 0 {
		startByte = 0
	}
	endByte := endPos.Offset
	if endByte < startByte {
		endByte = startByte
	}
	return astRangeAt(startByte, endByte, startPos.Line, endPos.Line, startPos.Column, endPos.Column)
}
