package recipes_test

import (
	astv1 "github.com/microsoft/code-intelligence/services/clean-code/internal/ast/v1"
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
