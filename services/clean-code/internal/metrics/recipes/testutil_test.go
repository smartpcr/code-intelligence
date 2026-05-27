package recipes_test

import (
	astv1 "github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/v1"
)

// astBuilder is a tiny helper used by the recipe tests to
// hand-build canonical `*astv1.AstFile` values without
// touching a real parser. The Stage 2.1 parser fleet does
// not yet emit `ScopeKindBlock` decision-point children, so
// every recipe test that needs decision blocks builds them
// synthetically here -- the recipe's algorithm is exercised
// against the SHAPE the future parser MUST emit (architecture
// Sec 4.5 -- "recipes consume *astv1.AstFile").
type astBuilder struct {
	file         *astv1.AstFile
	nextOrdinal  int
	currentFile  *astv1.AstScope
	scopesByName map[string]*astv1.AstScope
}

// newAstBuilder seeds the builder with the decision-blocks
// capability flag stamped on the file-level attrs. Tests that
// want to assert capability gating (AppliesTo=false on
// today's parser output) pass `withCapability=false`.
func newAstBuilder(path string, withCapability bool) *astBuilder {
	b := &astBuilder{
		nextOrdinal:  0,
		scopesByName: map[string]*astv1.AstScope{},
		file: &astv1.AstFile{
			Language: "go",
			Path:     path,
			Attrs:    map[string]string{},
		},
	}
	if withCapability {
		b.file.Attrs["decision_blocks"] = "true"
	}
	file := &astv1.AstScope{
		ScopeKind:     astv1.ScopeKind_SCOPE_KIND_FILE,
		Name:          path,
		QualifiedName: path,
		Range: &astv1.AstRange{
			StartByte: 0,
			EndByte:   1,
			StartLine: 1,
			EndLine:   1,
			StartCol:  1,
			EndCol:    1,
		},
	}
	b.currentFile = file
	b.assignID(file)
	b.file.Scopes = append(b.file.Scopes, file)
	return b
}

// assignID stamps a parser-style `local:N` placeholder on a
// scope and bumps the builder's ordinal counter so subsequent
// scopes get unique IDs.
func (b *astBuilder) assignID(s *astv1.AstScope) {
	s.ScopeId = parserLocalID(b.nextOrdinal)
	b.nextOrdinal++
}

// parserLocalID mirrors `parser.localID` (which is package-
// private). The exact format is not load-bearing for the
// recipe algorithm -- it only matters that IDs are unique --
// but mirroring keeps the test fixtures legible.
func parserLocalID(n int) string {
	return "local:" + itoa(n)
}

// itoa is a tiny strconv-free integer-to-string for the
// `local:N` formatter. Avoiding `strconv` keeps the test
// helper free of stdlib alias drift.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := false
	if n < 0 {
		negative = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// addMethod appends a method scope to the file. The optional
// parentID lets a test attach a method to a class / interface
// rather than the file (the cyclo recipe descends from method
// scopes; the parent does not matter).
func (b *astBuilder) addMethod(name string, parentID string) *astv1.AstScope {
	if parentID == "" {
		parentID = b.currentFile.GetScopeId()
	}
	m := &astv1.AstScope{
		ScopeKind:     astv1.ScopeKind_SCOPE_KIND_METHOD,
		Name:          name,
		QualifiedName: b.currentFile.GetQualifiedName() + "." + name,
		ParentScopeId: parentID,
		Range:         freshRange(b.nextOrdinal),
	}
	b.assignID(m)
	b.file.Scopes = append(b.file.Scopes, m)
	b.scopesByName[name] = m
	return m
}

// addBlock appends a `SCOPE_KIND_BLOCK` decision-point child
// under `parent`. `decisionKind` is stamped onto the block's
// `attrs["decision_kind"]` so the recipe's lookup hits the
// per-kind contribution row in `decisionTable`. An empty
// `decisionKind` produces a plain block (no contribution).
func (b *astBuilder) addBlock(parent *astv1.AstScope, decisionKind string) *astv1.AstScope {
	blk := &astv1.AstScope{
		ScopeKind:     astv1.ScopeKind_SCOPE_KIND_BLOCK,
		Name:          decisionKind,
		QualifiedName: parent.GetQualifiedName() + "#block",
		ParentScopeId: parent.GetScopeId(),
		Range:         freshRange(b.nextOrdinal),
		Attrs:         map[string]string{},
	}
	if decisionKind != "" {
		blk.Attrs["decision_kind"] = decisionKind
	}
	b.assignID(blk)
	b.file.Scopes = append(b.file.Scopes, blk)
	return blk
}

// freshRange returns a unique `*AstRange` for a scope so the
// stable-sort key in `recipes.sortByRange` produces a
// deterministic source order. The N-th scope sits at line
// `100+N` -- well past line 1 (which the file scope owns).
func freshRange(ordinal int) *astv1.AstRange {
	return &astv1.AstRange{
		StartLine: uint32(100 + ordinal),
		EndLine:   uint32(100 + ordinal),
		StartByte: uint32(ordinal),
		EndByte:   uint32(ordinal + 1),
		StartCol:  1,
		EndCol:    1,
	}
}

// build returns the assembled `*AstFile`.
func (b *astBuilder) build() *astv1.AstFile { return b.file }

// addPackage attaches a `SCOPE_KIND_PACKAGE` scope to the
// builder's file and rewires the file scope's
// `parent_scope_id` to point at it. This mirrors the Stage
// 2.1 parser fleet's emission shape -- every `AstFile` ships
// a package scope that parents the file scope (see
// `internal/ast/parser/go.go` package-scope construction).
//
// The package's `name` and `qualifiedName` are both set to
// `pkgName` so the cycle_member recipe's package-name index
// matches an `imports`-edge target of `qualified:<pkgName>`.
func (b *astBuilder) addPackage(pkgName string) *astv1.AstScope {
	pkg := &astv1.AstScope{
		ScopeKind:     astv1.ScopeKind_SCOPE_KIND_PACKAGE,
		Name:          pkgName,
		QualifiedName: pkgName,
		Range: &astv1.AstRange{
			StartByte: 0,
			EndByte:   1,
			StartLine: 1,
			EndLine:   1,
			StartCol:  1,
			EndCol:    1,
		},
	}
	b.assignID(pkg)
	b.file.Scopes = append(b.file.Scopes, pkg)
	b.currentFile.ParentScopeId = pkg.GetScopeId()
	return pkg
}

// addSymbol appends a `*AstSymbol` to the builder's file with
// the given `kind` and `name`, attached to `parent` (or the
// file scope when `parent` is nil). Used by duplication_ratio
// tests to inflate the structural-token stream past the
// 50-token window threshold.
func (b *astBuilder) addSymbol(parent *astv1.AstScope, kind, name string) *astv1.AstSymbol {
	if parent == nil {
		parent = b.currentFile
	}
	sym := &astv1.AstSymbol{
		Name:    name,
		Kind:    kind,
		ScopeId: parent.GetScopeId(),
		Range:   freshRange(b.nextOrdinal),
	}
	// Give every symbol a unique synthetic id derived from
	// the ordinal counter so a test that bulk-adds symbols
	// still produces unique entries.
	sym.SymbolId = parserLocalID(b.nextOrdinal) + ":sym"
	b.nextOrdinal++
	b.file.Symbols = append(b.file.Symbols, sym)
	return sym
}

// addImportEdge appends an `"imports"`-kind `AstEdge` whose
// source is the builder's file scope and whose target is the
// `qualified:<target>` form the parser uses (see
// `internal/ast/parser/internal.go:externalScopeRef`).
func (b *astBuilder) addImportEdge(target string) *astv1.AstEdge {
	edge := &astv1.AstEdge{
		Kind: "imports",
		From: &astv1.AstRef{
			Kind: astv1.AstRefKind_AST_REF_KIND_SCOPE,
			Id:   b.currentFile.GetScopeId(),
		},
		To: &astv1.AstRef{
			Kind: astv1.AstRefKind_AST_REF_KIND_SCOPE,
			Id:   "qualified:" + target,
		},
	}
	b.file.Edges = append(b.file.Edges, edge)
	return edge
}

// setModulePath stamps `Attrs[module_path]` on the builder's
// file so the cycle_member recipe can canonicalise module-
// qualified import targets (e.g. strip `github.com/org/repo/`
// from `github.com/org/repo/internal/foo` before looking up
// `internal/foo` in the dir-index). Used by the iter-5
// module-path canonicalisation tests.
func (b *astBuilder) setModulePath(modulePath string) {
	b.file.Attrs["module_path"] = modulePath
}
