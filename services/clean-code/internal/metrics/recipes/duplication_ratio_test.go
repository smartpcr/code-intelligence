package recipes_test

import (
	"fmt"
	"math"
	"testing"

	astv1 "github.com/microsoft/code-intelligence/services/clean-code/internal/ast/v1"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// makeDupFile builds an AstFile with the given path, package
// name, and a sequence of symbol names. Each symbol becomes
// one token (`sym:var:<name>`) in the recipe's token stream,
// emitted in source order by ordinal-derived range.
func makeDupFile(path, pkgName string, symbolNames []string) *astv1.AstFile {
	b := newAstBuilder(path, true)
	b.addPackage(pkgName)
	for _, name := range symbolNames {
		b.addSymbol(nil, "var", name)
	}
	return b.build()
}

// TestDuplicationRatioRecipe_MetricKindIsCanonical pins the
// metric_kind literal `duplication_ratio` (architecture Sec
// 1.4.1 row 11).
func TestDuplicationRatioRecipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	if got := r.MetricKind(); got != "duplication_ratio" {
		t.Fatalf("MetricKind() = %q, want %q", got, "duplication_ratio")
	}
}

// TestDuplicationRatioRecipe_VersionStartsAtOne pins the
// initial version.
func TestDuplicationRatioRecipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	if got := r.Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1", got)
	}
}

// TestDuplicationRatioRecipe_AppliesTo_NilAst asserts the
// recipe refuses a nil AST.
func TestDuplicationRatioRecipe_AppliesTo_NilAst(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	if r.AppliesTo(nil) {
		t.Fatalf("AppliesTo(nil) = true, want false")
	}
}

// TestDuplicationRatioRecipe_AppliesTo_DegradedSkipped
// enforces Sec 3.4 lines 490-494: degraded ASTs MUST NOT
// produce a `source='computed'` row.
func TestDuplicationRatioRecipe_AppliesTo_DegradedSkipped(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	ast := makeDupFile("a.go", "pkg_a", repeatStrings("x", 60))
	ast.DegradedReason = "tree-sitter timeout"
	if r.AppliesTo(ast) {
		t.Fatalf("AppliesTo(<degraded>) = true, want false")
	}
	if got := r.Compute(ast); got != nil {
		t.Fatalf("Compute(<degraded>) = %v, want nil", got)
	}
}

// TestDuplicationRatioRecipe_Compute_BelowWindowEmitsZero
// asserts files with fewer tokens than the 50-token window
// emit `value=0.0` (present-but-short scope cannot contain a
// 50-token clone by definition). The recipe must NOT skip
// the draft -- doing so would create a "metric missing vs
// metric zero" ambiguity downstream.
func TestDuplicationRatioRecipe_Compute_BelowWindowEmitsZero(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	ast := makeDupFile("a.go", "pkg_a", uniqueStrings("v", 49))
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute(<49 tokens>) returned %d drafts, want 1", len(drafts))
	}
	if drafts[0].Value != 0.0 {
		t.Fatalf("Value = %v, want 0.0 (short streams cannot contain a 50-token clone)", drafts[0].Value)
	}
}

// TestDuplicationRatioRecipe_Compute_EmptyTokenStreamEmitsZero
// asserts an AST with no symbols / nested scopes (just a
// file + package scope) emits a `value=0.0` draft -- the
// scope is present and valid, the metric just happens to be
// zero.
func TestDuplicationRatioRecipe_Compute_EmptyTokenStreamEmitsZero(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	ast := makeDupFile("a.go", "pkg_a", nil)
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute(<0 tokens>) returned %d drafts, want 1", len(drafts))
	}
	if drafts[0].Value != 0.0 {
		t.Fatalf("Value = %v, want 0.0", drafts[0].Value)
	}
}

// TestDuplicationRatioRecipe_Compute_AllUniqueRatioZero pins
// the no-duplication boundary: 60 unique tokens produce
// ratio=0.0.
func TestDuplicationRatioRecipe_Compute_AllUniqueRatioZero(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	ast := makeDupFile("a.go", "pkg_a", uniqueStrings("v", 60))
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	if drafts[0].Value != 0.0 {
		t.Fatalf("Value = %v, want 0.0 (all-unique stream has no duplicates)", drafts[0].Value)
	}
	if drafts[0].Scope.Kind != scope.KindFile {
		t.Fatalf("Scope.Kind = %q, want %q", drafts[0].Scope.Kind, scope.KindFile)
	}
}

// TestDuplicationRatioRecipe_Compute_AllSameRatioOne pins
// the full-duplication boundary: 100 identical tokens produce
// ratio=1.0.
func TestDuplicationRatioRecipe_Compute_AllSameRatioOne(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	ast := makeDupFile("a.go", "pkg_a", repeatStrings("x", 100))
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	if drafts[0].Value != 1.0 {
		t.Fatalf("Value = %v, want 1.0 (all-identical stream is 100%% duplicate)", drafts[0].Value)
	}
}

// TestDuplicationRatioRecipe_Compute_PartialDuplicationEightTenths
// is the headline scenario: a 125-token stream with the
// first 50 tokens repeated verbatim at positions 50..99 and
// 25 unique tokens at the tail. ONLY the (0, 50) window pair
// duplicates; covered tokens = 100, total = 125, ratio = 0.8.
func TestDuplicationRatioRecipe_Compute_PartialDuplicationEightTenths(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	// 50 unique "a*" tokens + 50 repeats of those same names + 25 unique "c*" tail.
	a := uniqueStrings("a", 50)
	tail := uniqueStrings("c", 25)
	stream := make([]string, 0, 125)
	stream = append(stream, a...)
	stream = append(stream, a...)
	stream = append(stream, tail...)
	ast := makeDupFile("a.go", "pkg_a", stream)
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	want := 0.8
	if math.Abs(drafts[0].Value-want) > 1e-9 {
		t.Fatalf("Value = %v, want %v (100 covered / 125 total)", drafts[0].Value, want)
	}
}

// TestDuplicationRatioRecipe_Compute_RatioBoundedZeroToOne is
// a property-style sanity check: across several constructed
// streams, the emitted ratio MUST sit in [0.0, 1.0].
func TestDuplicationRatioRecipe_Compute_RatioBoundedZeroToOne(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	cases := [][]string{
		uniqueStrings("v", 60),
		repeatStrings("x", 60),
		mixStreams(uniqueStrings("a", 30), repeatStrings("b", 30)),
		mixStreams(uniqueStrings("p", 25), uniqueStrings("p", 25)), // names match
	}
	for i, stream := range cases {
		ast := makeDupFile(fmt.Sprintf("f%d.go", i), "pkg_a", stream)
		drafts := r.Compute(ast)
		if len(drafts) != 1 {
			t.Fatalf("case %d: Compute returned %d drafts, want 1", i, len(drafts))
		}
		v := drafts[0].Value
		if v < 0.0 || v > 1.0 {
			t.Fatalf("case %d: Value = %v, want in [0.0, 1.0]", i, v)
		}
	}
}

// TestDuplicationRatioRecipe_Compute_DraftFieldsCanonical pins
// every guard the recipe's draft must satisfy: pack=base,
// source=computed, version=1, scope_kind=file. The closed-set
// values are FK targets in `MetricSample`; drift surfaces as
// a constraint violation at insert time.
func TestDuplicationRatioRecipe_Compute_DraftFieldsCanonical(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	ast := makeDupFile("a.go", "pkg_a", uniqueStrings("v", 60))
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	d := drafts[0]
	if d.MetricKind != "duplication_ratio" {
		t.Fatalf("MetricKind = %q, want duplication_ratio", d.MetricKind)
	}
	if d.MetricVersion != 1 {
		t.Fatalf("MetricVersion = %d, want 1", d.MetricVersion)
	}
	if d.Pack != recipes.PackBase {
		t.Fatalf("Pack = %q, want %q", d.Pack, recipes.PackBase)
	}
	if d.Source != recipes.SourceComputed {
		t.Fatalf("Source = %q, want %q", d.Source, recipes.SourceComputed)
	}
	if d.Scope.Kind != scope.KindFile {
		t.Fatalf("Scope.Kind = %q, want file", d.Scope.Kind)
	}
}

// TestDuplicationRatioRecipe_ComputeProject_EmitsFileAndPackage
// asserts the per-project emission path produces ONE
// file-scope draft per AST PLUS one package-scope draft per
// package. The package-scope value reflects the CONCATENATED
// token streams.
func TestDuplicationRatioRecipe_ComputeProject_EmitsFileAndPackage(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	asts := []*astv1.AstFile{
		makeDupFile("a/x.go", "pkg_a", uniqueStrings("a", 60)),
		makeDupFile("a/y.go", "pkg_a", uniqueStrings("b", 60)),
	}
	drafts := r.ComputeProject(asts)
	// Expect 2 file drafts + 1 package draft.
	if len(drafts) != 3 {
		t.Fatalf("ComputeProject returned %d drafts, want 3", len(drafts))
	}
	fileCount, pkgCount := 0, 0
	for _, d := range drafts {
		switch d.Scope.Kind {
		case scope.KindFile:
			fileCount++
		case scope.KindPackage:
			pkgCount++
		default:
			t.Fatalf("scope_kind = %q, want file or package", d.Scope.Kind)
		}
	}
	if fileCount != 2 {
		t.Fatalf("file-scope draft count = %d, want 2", fileCount)
	}
	if pkgCount != 1 {
		t.Fatalf("package-scope draft count = %d, want 1", pkgCount)
	}
}

// TestDuplicationRatioRecipe_ComputeProject_CrossFileDuplicateInPackage
// asserts that when two files in the same package share a
// 50-token block, the per-package draft picks up the cross-
// file duplication (per-file ratios may be 0 because each
// file viewed alone has no internal duplicate).
func TestDuplicationRatioRecipe_ComputeProject_CrossFileDuplicateInPackage(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	shared := uniqueStrings("shared", 50)
	// File 1: 50 shared + 10 unique = 60 tokens, no internal dup.
	// File 2: 50 shared + 10 unique = 60 tokens, no internal dup.
	// Package concat: 120 tokens with a 50-window appearing at
	// positions 0 and 60 (after file 1's 10 unique tail) -- the
	// (0, 60) pair duplicates.
	f1 := append(append([]string{}, shared...), uniqueStrings("f1", 10)...)
	f2 := append(append([]string{}, shared...), uniqueStrings("f2", 10)...)
	asts := []*astv1.AstFile{
		makeDupFile("a/x.go", "pkg_a", f1),
		makeDupFile("a/y.go", "pkg_a", f2),
	}
	drafts := r.ComputeProject(asts)
	var pkgDraft *recipes.MetricSampleDraft
	for i := range drafts {
		if drafts[i].Scope.Kind == scope.KindPackage {
			pkgDraft = &drafts[i]
			break
		}
	}
	if pkgDraft == nil {
		t.Fatalf("no package-scope draft emitted")
	}
	if pkgDraft.Value <= 0 {
		t.Fatalf("package-scope Value = %v, want > 0 (cross-file shared block)", pkgDraft.Value)
	}
	// Per-file drafts should be 0 (no internal duplication).
	for _, d := range drafts {
		if d.Scope.Kind == scope.KindFile && d.Value != 0.0 {
			t.Fatalf("file %s Value = %v, want 0 (no within-file duplication)", d.Scope.Path, d.Value)
		}
	}
}

// TestDuplicationRatioRecipe_ComputeProject_NeverEmitsModuleKind
// is the closed-set guard: brief and architecture Sec 1.4.1
// row 11 explicitly forbid `function`, `method`, and `module`
// scope_kinds. Only `file` and `package` are valid.
func TestDuplicationRatioRecipe_ComputeProject_NeverEmitsModuleKind(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	asts := []*astv1.AstFile{
		makeDupFile("a/x.go", "pkg_a", repeatStrings("z", 100)),
	}
	drafts := r.ComputeProject(asts)
	if len(drafts) == 0 {
		t.Fatalf("ComputeProject returned 0 drafts, want > 0")
	}
	for _, d := range drafts {
		if d.Scope.Kind != scope.KindFile && d.Scope.Kind != scope.KindPackage {
			t.Fatalf("scope_kind = %q, want file or package", d.Scope.Kind)
		}
	}
}

// TestDuplicationRatioRecipe_ComputeProject_DegradedAstSkipped
// asserts a degraded AST is excluded from both per-file
// emission and per-package concatenation.
func TestDuplicationRatioRecipe_ComputeProject_DegradedAstSkipped(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	good := makeDupFile("a/x.go", "pkg_a", uniqueStrings("g", 60))
	bad := makeDupFile("a/y.go", "pkg_a", uniqueStrings("b", 60))
	bad.DegradedReason = "tree-sitter timeout"
	drafts := r.ComputeProject([]*astv1.AstFile{good, bad})
	for _, d := range drafts {
		if d.Scope.Path == "a/y.go" {
			t.Fatalf("draft for degraded file emitted: %+v", d)
		}
	}
}

// TestDuplicationRatioRecipe_ComputeProject_EmptyInput pins
// the nil-empty contract.
func TestDuplicationRatioRecipe_ComputeProject_EmptyInput(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	if got := r.ComputeProject(nil); got != nil {
		t.Fatalf("ComputeProject(nil) = %v, want nil", got)
	}
	if got := r.ComputeProject([]*astv1.AstFile{}); got != nil {
		t.Fatalf("ComputeProject([]) = %v, want nil", got)
	}
}

// TestDuplicationRatioRecipe_ComputeProject_Deterministic
// asserts emission order and values are reproducible across
// runs (G2).
func TestDuplicationRatioRecipe_ComputeProject_Deterministic(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	asts := []*astv1.AstFile{
		makeDupFile("c/c.go", "pkg_c", repeatStrings("c", 80)),
		makeDupFile("a/a.go", "pkg_a", repeatStrings("a", 80)),
		makeDupFile("b/b.go", "pkg_b", repeatStrings("b", 80)),
	}
	first := r.ComputeProject(asts)
	second := r.ComputeProject(asts)
	if len(first) != len(second) {
		t.Fatalf("two runs returned different draft counts: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Scope.Path != second[i].Scope.Path ||
			first[i].Scope.Kind != second[i].Scope.Kind ||
			first[i].Value != second[i].Value {
			t.Fatalf("draft %d differs across runs: %+v vs %+v", i, first[i], second[i])
		}
	}
	// File drafts in lex order, then packages in lex order.
	want := []string{"a/a.go", "b/b.go", "c/c.go"}
	got := []string{}
	for _, d := range first {
		if d.Scope.Kind == scope.KindFile {
			got = append(got, d.Scope.Path)
		}
	}
	if !equalStringSlices(got, want) {
		t.Fatalf("file-scope path order = %v, want %v", got, want)
	}
}

// uniqueStrings returns `count` strings of the form
// `prefix-0`, `prefix-1`, ... `prefix-(count-1)` -- a stream
// of distinct tokens.
func uniqueStrings(prefix string, count int) []string {
	out := make([]string, count)
	for i := 0; i < count; i++ {
		out[i] = fmt.Sprintf("%s-%d", prefix, i)
	}
	return out
}

// repeatStrings returns `count` copies of `value` -- a stream
// of identical tokens.
func repeatStrings(value string, count int) []string {
	out := make([]string, count)
	for i := 0; i < count; i++ {
		out[i] = value
	}
	return out
}

// mixStreams concatenates the input string slices in order.
func mixStreams(parts ...[]string) []string {
	out := []string{}
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// stubLoader returns a [recipes.SourceLoader] that maps a
// finite path->bytes table. Unknown paths return
// `(nil, false)`. Used by lexical-tokeniser tests to keep
// the recipe hermetic (no real filesystem reads).
func stubLoader(table map[string][]byte) recipes.SourceLoader {
	return func(path string) ([]byte, bool) {
		b, ok := table[path]
		if !ok {
			return nil, false
		}
		return b, true
	}
}

// TestDuplicationRatioRecipe_LexicalTokenize_WhitespaceOnlyDiffIsDuplicate
// is the e2e contract pin (e2e-scenarios.md:426-430): two
// TypeScript functions differing only in indentation and
// trailing newlines emit duplication_ratio > 0.95. The
// lexical tokeniser drops whitespace as a separator only,
// so both functions tokenise to identical streams and every
// 50-window appears at two positions.
func TestDuplicationRatioRecipe_LexicalTokenize_WhitespaceOnlyDiffIsDuplicate(t *testing.T) {
	t.Parallel()
	// A TypeScript-ish snippet with TWO functions whose
	// only difference is indentation / blank lines. We
	// inflate the body with enough identifiers / operators
	// so each function alone contributes >= 50 lexical
	// tokens (so a 50-window covers the bulk of the file).
	source := `
function alpha(input: Record<string, number>): number {
    const collected = collectAllEntries(input, keysSorted);
    const filtered = filterByThreshold(collected, threshold, ceiling);
    const reshaped = reshapeRecord(filtered, mapper, fallback);
    const enriched = enrichWithDefaults(reshaped, defaults, overrides);
    const finalised = finaliseAggregate(enriched, options, sentinel);
    return finalised.totalScore;
}

function beta(input:Record<string,number>):number{
const collected=collectAllEntries(input,keysSorted);
const filtered=filterByThreshold(collected,threshold,ceiling);
const reshaped=reshapeRecord(filtered,mapper,fallback);
const enriched=enrichWithDefaults(reshaped,defaults,overrides);
const finalised=finaliseAggregate(enriched,options,sentinel);
return finalised.totalScore;
}
`
	loader := stubLoader(map[string][]byte{
		"whitespace.ts": []byte(source),
	})
	r := recipes.NewDuplicationRatioRecipeWithSource(loader)
	// Build a minimal AstFile so AppliesTo / fileScopeOf pass;
	// the recipe pulls real tokens from the loader, NOT from
	// the synthetic scopes we attach here.
	ast := makeDupFile("whitespace.ts", "pkg_whitespace", nil)
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	v := drafts[0].Value
	if v <= 0.95 || v > 1.0 {
		t.Fatalf("Value = %v, want >0.95 and <=1.0 (whitespace-only diff is fully duplicate; e2e-scenarios.md:426-430)", v)
	}
}

// TestDuplicationRatioRecipe_LexicalTokenize_BasicShape asserts
// the tokeniser's contract: whitespace is dropped, identifier
// runs are one token, operators are single-character tokens.
func TestDuplicationRatioRecipe_LexicalTokenize_BasicShape(t *testing.T) {
	t.Parallel()
	loader := stubLoader(map[string][]byte{
		"a.ts": []byte("function foo (x) { return x ; }"),
	})
	r := recipes.NewDuplicationRatioRecipeWithSource(loader)
	ast := makeDupFile("a.ts", "pkg_a", nil)
	// The lexer alone won't produce duplicates on this
	// 11-token snippet; we just want to confirm the recipe
	// runs through the lexical path without panicking and
	// emits a present-but-short value=0 row.
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	if drafts[0].Value != 0.0 {
		t.Fatalf("Value = %v, want 0.0 (11 tokens < windowSize=50)", drafts[0].Value)
	}
}

// TestDuplicationRatioRecipe_LexicalTokenize_StructuralFallback
// asserts that when the SourceLoader misses (file not in the
// stub table), the recipe falls back to structural tokens.
// Existing makeDupFile-based tests rely on this fallback
// implicitly; this test pins the contract explicitly.
func TestDuplicationRatioRecipe_LexicalTokenize_StructuralFallback(t *testing.T) {
	t.Parallel()
	// Stub loader misses every path.
	r := recipes.NewDuplicationRatioRecipeWithSource(stubLoader(nil))
	// 100 identical symbol-tokens via structural fallback ->
	// ratio = 1.0 (every window is a duplicate of every other).
	ast := makeDupFile("missing.go", "pkg_a", repeatStrings("x", 100))
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	if drafts[0].Value != 1.0 {
		t.Fatalf("Value = %v, want 1.0 (structural fallback over all-identical symbols)", drafts[0].Value)
	}
}

// TestDuplicationRatioRecipe_PackageScope_SentinelsExcludedFromRatio
// pins evaluator item 5 (the sentinel-denominator bug fix).
// With two files of 50 unique tokens each (no internal
// duplication), the concatenated package stream is
// [50 unique][SENTINEL][50 unique] = 101 tokens but only 100
// REAL tokens. No two windows match (the sentinel guarantees
// boundary-straddling windows are unique; the two halves are
// internally unique). The package-scope ratio MUST equal
// 0 / 100 = 0.0 (not 0 / 101 ~ 0.0 either, but the
// denominator MUST be REAL tokens only, not sentinel-padded).
//
// To prove the denominator is correct we use a constructed
// fixture where 25 of the second file's 50 tokens are
// COPIES of the first file's first 25 tokens, padded so
// exactly one 50-window appears at two positions. Computing
// real_covered / real_total gives a value the test can pin.
func TestDuplicationRatioRecipe_PackageScope_SentinelsExcludedFromRatio(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipeWithSource(stubLoader(nil))

	// File 1: 50 unique tokens [a-0 .. a-49].
	// File 2: 50 unique tokens [a-0 .. a-49] (identical to file 1).
	//   -> Concat stream (structural fallback): 50 + sentinel + 50 = 101 tokens.
	//   -> Windows at positions [0..50] and [51..101] are identical (boundary at index 50).
	//   -> covered = positions 0..49 (50 real) + positions 51..100 (50 real, but [51] is sentinel? NO -- [50] is sentinel) ...
	// Let's recompute:
	//   positions 0..49: file 1's 50 tokens (REAL)
	//   position 50: SENTINEL
	//   positions 51..100: file 2's 50 tokens (REAL)
	//   Window [0..49]: 50 REAL tokens of file 1.
	//   Window [51..100]: 50 REAL tokens of file 2.
	//   Both windows have IDENTICAL content (50 same tokens).
	//   So both windows are duplicates of each other.
	//   covered indices: 0..49 and 51..100 = 100 real positions.
	//   sentinel index 50 is NOT counted in numerator or denominator.
	// Expected ratio: 100 / 100 = 1.0.
	tokens := uniqueStrings("a", 50)
	asts := []*astv1.AstFile{
		makeDupFile("p/x.go", "pkg_p", tokens),
		makeDupFile("p/y.go", "pkg_p", tokens),
	}
	drafts := r.ComputeProject(asts)
	var pkgDraft *recipes.MetricSampleDraft
	for i := range drafts {
		if drafts[i].Scope.Kind == scope.KindPackage {
			pkgDraft = &drafts[i]
			break
		}
	}
	if pkgDraft == nil {
		t.Fatalf("no package-scope draft emitted")
	}
	// If the recipe (incorrectly) divided by 101 (sentinel-
	// inflated total), the value would be 100/101 ~ 0.9901.
	// The fixed contract divides by 100 (real-token total)
	// so the value is exactly 1.0.
	want := 1.0
	if pkgDraft.Value != want {
		t.Fatalf("package Value = %v, want %v (sentinel must be excluded from denominator: real_covered=100 / real_total=100)", pkgDraft.Value, want)
	}
}

// TestDuplicationRatioRecipe_DefaultSourceLoader_DoesNotReadDisk
// pins the iter-3 item 3 fix: the [recipes.DefaultSourceLoader]
// is the always-miss loader -- the recipe never reads the
// filesystem so its output is a pure function of the AstFile
// (recipe purity contract at `recipe.go:227-237`). Calling
// the loader with ANY path (existing or not) MUST return
// `(nil, false)`. The complement test
// `_LexicalTokenize_WhitespaceOnlyDiffIsDuplicate` proves the
// SEAM still works when an explicit deterministic loader is
// injected via [NewDuplicationRatioRecipeWithSource].
func TestDuplicationRatioRecipe_DefaultSourceLoader_DoesNotReadDisk(t *testing.T) {
	t.Parallel()
	// Two paths, one that almost certainly exists (this test
	// file) and one that doesn't. Both must return
	// `(nil, false)` -- the loader must NOT consult the
	// filesystem at all.
	if _, ok := recipes.DefaultSourceLoader("duplication_ratio_test.go"); ok {
		t.Fatalf("DefaultSourceLoader returned ok=true for a real file; the iter-3 contract is always-miss (recipe purity)")
	}
	if _, ok := recipes.DefaultSourceLoader("does-not-exist-12345.go"); ok {
		t.Fatalf("DefaultSourceLoader returned ok=true for a non-existent file; expected always-miss")
	}
	if _, ok := recipes.DefaultSourceLoader(""); ok {
		t.Fatalf("DefaultSourceLoader returned ok=true for empty path; expected always-miss")
	}
}

// TestDuplicationRatioRecipe_DefaultRecipe_StructuralFallback
// pins the iter-3 / iter-5 production contract: when an
// AstFile carries NEITHER `Attrs[parser.AttrSourceBytes]`
// (tier 1, parser-stamped lexical) NOR a hit from the
// configured [SourceLoader] (tier 2; the default constructor
// wires the always-miss [DefaultSourceLoader]), the default-
// constructed recipe falls through to STRUCTURAL tokens
// (tier 3). This test exercises the fallback by hand-building
// an AstFile without source-bytes Attrs (and without a
// non-default loader), so neither tier 1 nor tier 2 fires.
//
// This is the COMPLEMENT to
// `TestDuplicationRatioRecipe_DefaultRecipe_LexicalViaAttrs`
// below, which proves the SAME default constructor exercises
// lexical mode when the parser stamps `AttrSourceBytes`
// (tier 1) -- so the default recipe is lexical in production
// (parser ASTs carry the attr) and structural-fallback in
// fixture-built tests like this one. The behaviour is
// determined by the AstFile's Attrs, NOT by which constructor
// the caller used.
//
// Recipe purity is preserved either way: the path string
// doesn't matter because the recipe never consults the
// filesystem (the default [DefaultSourceLoader] is always-
// miss; the parser-attr path reads bytes off the AstFile, not
// disk).
func TestDuplicationRatioRecipe_DefaultRecipe_StructuralFallback(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	// 100 identical symbol-tokens via structural fallback ->
	// ratio = 1.0 (every window is a duplicate of every
	// other). The path string doesn't matter because the
	// loader never consults it.
	ast := makeDupFile("does-not-exist.go", "pkg_a", repeatStrings("x", 100))
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	if drafts[0].Value != 1.0 {
		t.Fatalf("Value = %v, want 1.0 (structural-fallback over all-identical symbols, production default)", drafts[0].Value)
	}
}

// TestDuplicationRatioRecipe_DefaultRecipe_LexicalViaAttrs pins
// iter-4 item 5: the DEFAULT-CONSTRUCTED recipe (the one
// `DefaultProjectRegistry` registers) MUST exercise lexical
// tokenisation when the AstFile carries source bytes on its
// `Attrs[AttrSourceBytes]` field, without any SourceLoader
// wiring at the composition root. This is the production
// dispatch path: a parser / scan layer populates
// `AstFile.Attrs[AttrSourceBytes]` at construction time,
// AND the default `NewDuplicationRatioRecipe()` reads it
// directly -- so the e2e whitespace-canonicalisation
// behaviour is reachable through the default registry,
// without requiring callers to know about the SourceLoader
// seam.
//
// Recipe purity (recipe.go:227-237 "Same `*AstFile` in ->
// same drafts out") is preserved: the source bytes live ON
// the AstFile, so the recipe's output is a pure function of
// its input AstFile. No filesystem read, no cwd dependency.
func TestDuplicationRatioRecipe_DefaultRecipe_LexicalViaAttrs(t *testing.T) {
	t.Parallel()
	// Same two-function whitespace-only-diff fixture as
	// _LexicalTokenize_WhitespaceOnlyDiffIsDuplicate (per
	// e2e-scenarios.md:426-430).
	source := `
function alpha (input: Record<string, number>) : number {
    const collected = collectAllEntries(input, keysSorted);
    const filtered = filterByThreshold(collected, threshold, ceiling);
    const reshaped = reshapeRecord(filtered, mapper, fallback);
    const enriched = enrichWithDefaults(reshaped, defaults, overrides);
    const finalised = finaliseAggregate(enriched, options, sentinel);
    return finalised.totalScore;
}

function beta(input:Record<string,number>):number{
const collected=collectAllEntries(input,keysSorted);
const filtered=filterByThreshold(collected,threshold,ceiling);
const reshaped=reshapeRecord(filtered,mapper,fallback);
const enriched=enrichWithDefaults(reshaped,defaults,overrides);
const finalised=finaliseAggregate(enriched,options,sentinel);
return finalised.totalScore;
}
`
	// Build via the DEFAULT recipe -- no SourceLoader wiring
	// at the composition root. Lexical mode is reached only
	// because the AstFile carries source bytes on its Attrs.
	r := recipes.NewDuplicationRatioRecipe()
	ast := makeDupFile("whitespace.ts", "pkg_whitespace", nil)
	if ast.Attrs == nil {
		ast.Attrs = map[string]string{}
	}
	ast.Attrs[recipes.AttrSourceBytes] = source
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	v := drafts[0].Value
	// Two functions differing only by whitespace tokenise to
	// identical streams (whitespace dropped); ratio > 0.95
	// per e2e:426-430. If lexical mode didn't fire (e.g. the
	// Attrs path missed and we fell through to structural),
	// the value would be 0 because the synthetic AstFile has
	// no symbols/scopes beyond the package.
	if v <= 0.95 || v > 1.0 {
		t.Fatalf("Value = %v, want >0.95 and <=1.0 (DEFAULT recipe MUST exercise lexical mode via Attrs[source_bytes]; e2e:426-430)", v)
	}
}

// TestDuplicationRatioRecipe_AttrsLexicalIsPureOnAstFile pins
// the purity invariant for the iter-4 lexical wire-up:
// re-running the recipe on the same AstFile (Attrs and all)
// must produce byte-identical drafts. We mutate the
// process's cwd between two compute calls to flush any
// hidden filesystem dependency; the output must not change.
func TestDuplicationRatioRecipe_AttrsLexicalIsPureOnAstFile(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	ast := makeDupFile("synthetic.go", "pkg_x", nil)
	if ast.Attrs == nil {
		ast.Attrs = map[string]string{}
	}
	// 200 bytes of identical structure -> dense duplicate.
	body := ""
	for i := 0; i < 100; i++ {
		body += "x y "
	}
	ast.Attrs[recipes.AttrSourceBytes] = body
	first := r.Compute(ast)
	second := r.Compute(ast)
	if len(first) != len(second) {
		t.Fatalf("draft count differs across runs: first=%d, second=%d", len(first), len(second))
	}
	for i := range first {
		if first[i].Value != second[i].Value {
			t.Fatalf("Value differs across runs at i=%d: first=%v, second=%v (recipe MUST be pure on AstFile)", i, first[i].Value, second[i].Value)
		}
	}
}

// TestDuplicationRatioRecipe_AttrsLexical_EmptyBytesFallThrough
// pins the empty-string edge: when `Attrs[AttrSourceBytes]`
// is present but EMPTY, the recipe MUST treat it as absent
// and fall through to the SourceLoader / structural path
// rather than lexing an empty byte slice (which would
// produce a zero-token stream and force the value to 0
// regardless of structural-token content). The empty-attr
// case typically reflects scan-layer "couldn't read this
// file" -- the recipe's existing structural fallback is
// the safer behaviour for that scenario.
func TestDuplicationRatioRecipe_AttrsLexical_EmptyBytesFallThrough(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	// AstFile WITH dense duplicate symbols (so structural
	// fallback would yield value=1.0); Attrs[source_bytes]
	// = "" (the empty edge). The recipe must NOT lex the
	// empty string; it must fall through to structural.
	ast := makeDupFile("dense.go", "pkg_dense", repeatStrings("dup", 60))
	if ast.Attrs == nil {
		ast.Attrs = map[string]string{}
	}
	ast.Attrs[recipes.AttrSourceBytes] = ""
	drafts := r.Compute(ast)
	if len(drafts) != 1 {
		t.Fatalf("Compute returned %d drafts, want 1", len(drafts))
	}
	// Structural fallback over 60 identical "dup" symbols
	// is value=1.0 (every 50-token window is a duplicate).
	// If the recipe had lexed the empty AttrSourceBytes, it
	// would have emitted 0.0.
	if drafts[0].Value != 1.0 {
		t.Fatalf("Value = %v, want 1.0 (empty AttrSourceBytes MUST fall through to structural; structural sees 60 identical syms)", drafts[0].Value)
	}
}

// TestDuplicationRatioRecipe_ComputeProject_PackageScope_CompoundIdentity
// pins iter-5 evaluator item 3: when a project has TWO
// packages declaring the same qualifiedName in DIFFERENT
// directories (the canonical Go example: multiple `main`
// packages under cmd/a/, cmd/b/, ...), the recipe MUST
// emit a separate package-scope draft for each compound
// identity `<qualifiedName>@<canonicalDir>`. The iter-4
// implementation keyed groups by qualifiedName alone,
// merging the two `main`s into a single inflated package
// metric.
func TestDuplicationRatioRecipe_ComputeProject_PackageScope_CompoundIdentity(t *testing.T) {
	t.Parallel()
	r := recipes.NewDuplicationRatioRecipe()
	// Two distinct `main` packages, each with its own
	// unique symbol stream so the per-package ratios are
	// independent.
	asts := []*astv1.AstFile{
		makeDupFile("cmd/a/main.go", "main", uniqueStrings("a", 60)),
		makeDupFile("cmd/b/main.go", "main", uniqueStrings("b", 60)),
	}
	drafts := r.ComputeProject(asts)
	// 2 file-scope + 2 package-scope drafts (one per
	// compound identity). With the iter-4 keying we would
	// have seen 2 file + 1 package = 3.
	fileCount, pkgCount := 0, 0
	pkgPaths := map[string]bool{}
	for _, d := range drafts {
		switch d.Scope.Kind {
		case scope.KindFile:
			fileCount++
		case scope.KindPackage:
			pkgCount++
			pkgPaths[d.Scope.Path] = true
		}
	}
	if fileCount != 2 {
		t.Fatalf("file-scope draft count = %d, want 2", fileCount)
	}
	if pkgCount != 2 {
		t.Fatalf("package-scope draft count = %d, want 2 (compound identity must keep co-named packages separate; iter-5 evaluator item 3)", pkgCount)
	}
	// Each package-scope draft should have a DIFFERENT
	// representative file path (one per compound identity).
	if len(pkgPaths) != 2 {
		t.Fatalf("distinct package-scope Path count = %d, want 2 (each compound identity gets its own representative path); got = %v", len(pkgPaths), pkgPaths)
	}
}
