package recipes_test

import (
	"context"
	"sort"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	astv1 "github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/v1"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/scope"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// makePkgFile is a tiny helper that builds a fresh AstFile
// with the given path, package qualifiedName, and import
// target list. The decision_blocks capability is irrelevant
// for cycle_member (it doesn't walk method bodies) so we
// always pass true to keep the builder happy.
func makePkgFile(path, pkgName string, imports ...string) *astv1.AstFile {
	b := newAstBuilder(path, true)
	b.addPackage(pkgName)
	for _, imp := range imports {
		b.addImportEdge(imp)
	}
	return b.build()
}

// TestCycleMemberRecipe_MetricKindIsCanonical pins the
// recipe's metric_kind to the architecture Sec 1.4.1 row 10
// literal `cycle_member`. A typo here surfaces as an FK
// violation on `MetricSample.metric_kind` at insert time.
func TestCycleMemberRecipe_MetricKindIsCanonical(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	if got := r.MetricKind(); got != "cycle_member" {
		t.Fatalf("MetricKind() = %q, want %q", got, "cycle_member")
	}
}

// TestCycleMemberRecipe_VersionStartsAtOne pins the initial
// recipe version.
func TestCycleMemberRecipe_VersionStartsAtOne(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	if got := r.Version(); got != 1 {
		t.Fatalf("Version() = %d, want 1", got)
	}
}

// TestCycleMemberRecipe_AppliesTo_NilAst asserts the recipe
// refuses a nil AST input.
func TestCycleMemberRecipe_AppliesTo_NilAst(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	if r.AppliesTo(nil) {
		t.Fatalf("AppliesTo(nil) = true, want false")
	}
}

// TestCycleMemberRecipe_AppliesTo_DegradedSkipped enforces
// architecture Sec 3.4 lines 490-494: degraded ASTs MUST NOT
// produce a `source='computed'` row.
func TestCycleMemberRecipe_AppliesTo_DegradedSkipped(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	ast := makePkgFile("a.go", "pkg_a")
	ast.DegradedReason = "tree-sitter timeout"
	if r.AppliesTo(ast) {
		t.Fatalf("AppliesTo(<degraded>) = true, want false")
	}
}

// TestCycleMemberRecipe_Compute_ReturnsNil pins the per-file
// no-op: a single AstFile cannot determine cycle membership,
// so Compute MUST return nil. The substantive emission path
// is ComputeProject.
func TestCycleMemberRecipe_Compute_ReturnsNil(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	ast := makePkgFile("a.go", "pkg_a", "pkg_b")
	if got := r.Compute(ast); got != nil {
		t.Fatalf("Compute(single ast) = %v drafts, want nil", got)
	}
}

// TestCycleMemberRecipe_ComputeProject_FullyAcyclicAllValueZero
// pins the iter-5 always-emit contract: when the project has
// zero non-trivial SCCs, the recipe STILL emits one draft per
// valid file scope and one per package identity, all with
// `value=0` and EMPTY attrs.
//
// The iter-4 "fully-acyclic returns nil" shortcut was
// REMOVED (iter-5 evaluator item 4): downstream queries
// need to distinguish "scope absent" from "scope present
// and not in any cycle". e2e-scenarios.md:418-422 was
// reconciled in iter-6 (iter-5 evaluator item 3) to spell
// out "zero rows with value=1.0 AND every scope emits a
// value=0.0 row" so the literal e2e text now agrees with
// the brief's universal `value=0 otherwise` contract.
func TestCycleMemberRecipe_ComputeProject_FullyAcyclicAllValueZero(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		makePkgFile("a/a.go", "pkg_a", "pkg_b"),
		makePkgFile("b/b.go", "pkg_b"), // no imports back to a
	}
	drafts := r.ComputeProject(asts)
	// 2 file-scope + 2 package-scope = 4 drafts, all value=0
	// with NO cycle_id attr.
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(fully-acyclic) = %d drafts, want 4 (2 file + 2 package, all value=0; iter-5 always-emit)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// assertAllNonCycle is a tiny test helper that asserts EVERY
// draft in `drafts` carries Value==0 AND empty Attrs (no
// AttrCycleID, no other key). Used by the many
// fully-acyclic / external-import / ambiguous-resolver
// tests to keep the iter-5 always-emit shape locked.
func assertAllNonCycle(t *testing.T, drafts []recipes.MetricSampleDraft) {
	t.Helper()
	for i, d := range drafts {
		if d.Value != 0.0 {
			t.Fatalf("draft[%d] Value = %v, want 0.0 (non-cycle scope, iter-5 always-emit); draft = %+v", i, d.Value, d)
		}
		if len(d.Attrs) != 0 {
			t.Fatalf("draft[%d] Attrs = %v, want empty (non-cycle scope has no cycle_id); draft = %+v", i, d.Attrs, d)
		}
		if cid, ok := d.Attrs[recipes.AttrCycleID]; ok {
			t.Fatalf("draft[%d] cycle_id = %q present, want absent (non-cycle scope)", i, cid)
		}
	}
}

// TestCycleMemberRecipe_ComputeProject_MixedCycleAndAcyclic
// pins the iter-5 always-emit contract (brief / impl-plan
// Stage 2.5 Test Scenario "file D outside the cycle emits
// value=0"): in a MIXED project where SOME packages are in
// cycles, the recipe emits ONE draft per (file, package) at
// file scope AND one per package at package scope. Cycle
// participants carry value=1 + attrs[cycle_id]; non-
// participants carry value=0 + empty attrs.
//
// See [recipes.CycleMemberRecipe] doc "Emission contract"
// for the full reconciliation of brief / impl-plan / arch /
// e2e. The fully-acyclic case (still emits value=0 rows per
// the iter-5 always-emit contract) is pinned by the SIBLING
// test
// [TestCycleMemberRecipe_ComputeProject_FullyAcyclicAllValueZero].
func TestCycleMemberRecipe_ComputeProject_MixedCycleAndAcyclic(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		makePkgFile("a/a.go", "pkg_a", "pkg_b"),
		makePkgFile("b/b.go", "pkg_b", "pkg_c"),
		makePkgFile("c/c.go", "pkg_c", "pkg_a"),
		makePkgFile("d/d.go", "pkg_d"), // outside the cycle
	}
	drafts := r.ComputeProject(asts)
	// 4 file-scope (A/B/C/D) + 4 package-scope = 8.
	if len(drafts) != 8 {
		t.Fatalf("ComputeProject(mixed) = %d drafts, want 8 (4 file + 4 package, hybrid contract)", len(drafts))
	}

	// Inspect the D drafts: file scope AND package scope
	// both have Value=0.0, NO cycle_id attr.
	var dFile, dPkg *recipes.MetricSampleDraft
	for i := range drafts {
		d := &drafts[i]
		switch {
		case d.Scope.Kind == scope.KindFile && d.Scope.Path == "d/d.go":
			dFile = d
		case d.Scope.Kind == scope.KindPackage && d.Scope.QualifiedName == "pkg_d":
			dPkg = d
		}
	}
	if dFile == nil {
		t.Fatalf("no file-scope draft for d/d.go (hybrid contract requires value=0 row for outside-cycle scope)")
	}
	if dFile.Value != 0.0 {
		t.Fatalf("d/d.go file Value = %v, want 0.0 (outside the cycle, brief: \"value=0 otherwise\")", dFile.Value)
	}
	if cid, ok := dFile.Attrs[recipes.AttrCycleID]; ok && cid != "" {
		t.Fatalf("d/d.go file cycle_id = %q, want empty (D is outside any SCC)", cid)
	}
	if dPkg == nil {
		t.Fatalf("no package-scope draft for pkg_d (hybrid contract requires value=0 row for outside-cycle scope)")
	}
	if dPkg.Value != 0.0 {
		t.Fatalf("pkg_d package Value = %v, want 0.0 (outside the cycle, brief: \"value=0 otherwise\")", dPkg.Value)
	}
	if cid, ok := dPkg.Attrs[recipes.AttrCycleID]; ok && cid != "" {
		t.Fatalf("pkg_d package cycle_id = %q, want empty (D is outside any SCC)", cid)
	}

	// Spot-check one cycle participant.
	var aFile *recipes.MetricSampleDraft
	for i := range drafts {
		d := &drafts[i]
		if d.Scope.Kind == scope.KindFile && d.Scope.Path == "a/a.go" {
			aFile = d
		}
	}
	if aFile == nil {
		t.Fatalf("no file-scope draft for a/a.go")
	}
	if aFile.Value != 1.0 {
		t.Fatalf("a/a.go file Value = %v, want 1.0 (cycle participant)", aFile.Value)
	}
	if cid := aFile.Attrs[recipes.AttrCycleID]; cid != "scc:pkg_a,pkg_b,pkg_c" {
		t.Fatalf("a/a.go cycle_id = %q, want %q", cid, "scc:pkg_a,pkg_b,pkg_c")
	}
}

// TestCycleMemberRecipe_ComputeProject_ThreeCycleEmitsAllParticipants
// constructs a three-package cycle A -> B -> C -> A and
// asserts the recipe emits cycle_member=1 at file scope for
// each file AND at package scope for each package, all
// sharing the same cycle_id.
func TestCycleMemberRecipe_ComputeProject_ThreeCycleEmitsAllParticipants(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		makePkgFile("a/a.go", "pkg_a", "pkg_b"),
		makePkgFile("b/b.go", "pkg_b", "pkg_c"),
		makePkgFile("c/c.go", "pkg_c", "pkg_a"),
	}
	drafts := r.ComputeProject(asts)
	// 3 file-scope drafts + 3 package-scope drafts.
	if len(drafts) != 6 {
		t.Fatalf("ComputeProject(3-cycle) = %d drafts, want 6", len(drafts))
	}

	fileCount, pkgCount := 0, 0
	cycleIDs := map[string]bool{}
	for _, d := range drafts {
		if d.MetricKind != "cycle_member" {
			t.Fatalf("draft.MetricKind = %q, want cycle_member", d.MetricKind)
		}
		if d.MetricVersion != 1 {
			t.Fatalf("draft.MetricVersion = %d, want 1", d.MetricVersion)
		}
		if d.Pack != recipes.PackBase {
			t.Fatalf("draft.Pack = %q, want %q", d.Pack, recipes.PackBase)
		}
		if d.Source != recipes.SourceComputed {
			t.Fatalf("draft.Source = %q, want %q", d.Source, recipes.SourceComputed)
		}
		if d.Value != 1.0 {
			t.Fatalf("draft.Value = %v, want 1.0 (cycle_member is `1 iff` per architecture Sec 1.4.1)", d.Value)
		}
		switch d.Scope.Kind {
		case scope.KindFile:
			fileCount++
		case scope.KindPackage:
			pkgCount++
		default:
			t.Fatalf("draft.Scope.Kind = %q, want file or package", d.Scope.Kind)
		}
		cid := d.Attrs[recipes.AttrCycleID]
		if cid == "" {
			t.Fatalf("draft.Attrs[%q] is empty, want SCC identifier", recipes.AttrCycleID)
		}
		cycleIDs[cid] = true
	}
	if fileCount != 3 {
		t.Fatalf("file-scope draft count = %d, want 3", fileCount)
	}
	if pkgCount != 3 {
		t.Fatalf("package-scope draft count = %d, want 3", pkgCount)
	}
	if len(cycleIDs) != 1 {
		t.Fatalf("distinct cycle_id values = %d, want 1 (all participants share the same SCC)", len(cycleIDs))
	}
	for cid := range cycleIDs {
		if cid != "scc:pkg_a,pkg_b,pkg_c" {
			t.Fatalf("cycle_id = %q, want %q (sorted, comma-joined)", cid, "scc:pkg_a,pkg_b,pkg_c")
		}
	}
}

// TestCycleMemberRecipe_ComputeProject_NeverEmitsModuleKind
// is the closed-set guard: the brief explicitly forbids
// `scope_kind='module'` (architecture Sec 1.4.1 row 10 pins
// `file, package` -- "NOT module"). The canonical enum has no
// `module` value at all; emitting one would panic at
// [newDraft]. This test asserts the affirmative: ONLY the
// canonical two kinds appear.
func TestCycleMemberRecipe_ComputeProject_NeverEmitsModuleKind(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		makePkgFile("a/a.go", "pkg_a", "pkg_b"),
		makePkgFile("b/b.go", "pkg_b", "pkg_a"),
	}
	drafts := r.ComputeProject(asts)
	if len(drafts) == 0 {
		t.Fatalf("ComputeProject(2-cycle) = 0 drafts, want > 0")
	}
	for _, d := range drafts {
		if d.Scope.Kind != scope.KindFile && d.Scope.Kind != scope.KindPackage {
			t.Fatalf("scope_kind = %q, want file or package (NOT module/function/...)", d.Scope.Kind)
		}
	}
}

// TestCycleMemberRecipe_ComputeProject_SelfLoopIsCycle pins
// the singleton-with-self-loop edge case: package A that
// imports itself is a non-trivial SCC.
func TestCycleMemberRecipe_ComputeProject_SelfLoopIsCycle(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		makePkgFile("a/a.go", "pkg_a", "pkg_a"),
	}
	drafts := r.ComputeProject(asts)
	if len(drafts) != 2 {
		t.Fatalf("ComputeProject(self-loop) = %d drafts, want 2 (file + package)", len(drafts))
	}
	for _, d := range drafts {
		if d.Attrs[recipes.AttrCycleID] != "scc:pkg_a" {
			t.Fatalf("self-loop cycle_id = %q, want %q", d.Attrs[recipes.AttrCycleID], "scc:pkg_a")
		}
	}
}

// TestCycleMemberRecipe_ComputeProject_DegradedSkipped asserts
// degraded ASTs are excluded from the import graph; the
// remaining files still form a graph and emit normally
// (iter-5 always-emit: with the degraded file dropped, the
// only file still in the project is pkg_a -- it emits one
// file + one package draft, both value=0).
func TestCycleMemberRecipe_ComputeProject_DegradedSkipped(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		makePkgFile("a/a.go", "pkg_a", "pkg_b"),
		makePkgFile("b/b.go", "pkg_b", "pkg_a"),
	}
	asts[1].DegradedReason = "parser timeout"
	drafts := r.ComputeProject(asts)
	// pkg_b is dropped from the graph, so the cycle breaks.
	// pkg_a remains: 1 file + 1 package = 2 drafts, all
	// value=0 (no cycle in the surviving graph).
	if len(drafts) != 2 {
		t.Fatalf("ComputeProject(with-degraded) = %d drafts, want 2 (degraded file dropped; pkg_a still emits value=0)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_EmptyInput pins the
// nil-empty contract.
func TestCycleMemberRecipe_ComputeProject_EmptyInput(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	if got := r.ComputeProject(nil); got != nil {
		t.Fatalf("ComputeProject(nil) = %v, want nil", got)
	}
	if got := r.ComputeProject([]*astv1.AstFile{}); got != nil {
		t.Fatalf("ComputeProject([]) = %v, want nil", got)
	}
}

// TestCycleMemberRecipe_ComputeProject_ExternalImportsIgnored
// asserts the recipe drops imports targeting packages OUTSIDE
// the project (i.e. not in the input AstFile set). Each in-
// project package still emits value=0 drafts (iter-5 always-
// emit) since the project has no cycles.
func TestCycleMemberRecipe_ComputeProject_ExternalImportsIgnored(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	// pkg_a imports an external package + pkg_b; pkg_b
	// imports the same external + nothing back to pkg_a.
	asts := []*astv1.AstFile{
		makePkgFile("a/a.go", "pkg_a", "context", "pkg_b"),
		makePkgFile("b/b.go", "pkg_b", "context"),
	}
	drafts := r.ComputeProject(asts)
	// 2 file + 2 package = 4 drafts, all value=0.
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(no in-project cycle) = %d drafts, want 4 (2 file + 2 package, all value=0)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_ExternalBasenameCollisionNoFabrication
// asserts that an external import whose basename matches an
// in-project package's qualifiedName does NOT fabricate a
// cycle: the recipe matches edge targets by EXACT qualifiedName
// only, never by suffix or basename. pkg_a imports
// `github.com/external/pkg_b` (external) -- the project's
// `pkg_b` declares itself as `pkg_b` (no path prefix), so the
// exact-match check fails and the edge is dropped. With the
// previous suffix-match fallback this would have been a
// fabricated edge -> fabricated cycle.
//
// iter-5 always-emit: the recipe STILL emits one file + one
// package draft per surviving package (all value=0) so
// downstream queries see the scopes.
func TestCycleMemberRecipe_ComputeProject_ExternalBasenameCollisionNoFabrication(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		// pkg_a imports the EXTERNAL `github.com/external/pkg_b`.
		makePkgFile("a/a.go", "pkg_a", "github.com/external/pkg_b"),
		// In-project pkg_b imports pkg_a (back-edge).
		makePkgFile("b/b.go", "pkg_b", "pkg_a"),
	}
	drafts := r.ComputeProject(asts)
	// pkg_a -> github.com/external/pkg_b is dropped (not in
	// pkgFiles), so the only edge in the graph is
	// pkg_b -> pkg_a. No cycle. iter-5 always-emit: 4 drafts
	// all value=0.
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(external basename collision) = %d drafts, want 4 (2 file + 2 package, all value=0; recipe must NOT fabricate cycles via suffix-match)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_DeterministicOrder
// asserts emission order is reproducible across runs (G2):
// file-scope drafts in lexicographic path order, then
// package-scope drafts in lexicographic package-name order.
func TestCycleMemberRecipe_ComputeProject_DeterministicOrder(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		makePkgFile("c/c.go", "pkg_c", "pkg_a"),
		makePkgFile("a/a.go", "pkg_a", "pkg_b"),
		makePkgFile("b/b.go", "pkg_b", "pkg_c"),
	}
	first := r.ComputeProject(asts)
	second := r.ComputeProject(asts)
	if len(first) != len(second) {
		t.Fatalf("two runs returned different draft counts: %d vs %d", len(first), len(second))
	}
	paths := make([]string, 0)
	for _, d := range first {
		if d.Scope.Kind == scope.KindFile {
			paths = append(paths, d.Scope.Path)
		}
	}
	expected := []string{"a/a.go", "b/b.go", "c/c.go"}
	if !equalStringSlices(paths, expected) {
		t.Fatalf("file-scope path order = %v, want %v (lexicographic)", paths, expected)
	}
	pkgs := make([]string, 0)
	for _, d := range first {
		if d.Scope.Kind == scope.KindPackage {
			pkgs = append(pkgs, d.Scope.QualifiedName)
		}
	}
	expectedPkgs := []string{"pkg_a", "pkg_b", "pkg_c"}
	if !equalStringSlices(pkgs, expectedPkgs) {
		t.Fatalf("package-scope order = %v, want %v (lexicographic)", pkgs, expectedPkgs)
	}
	// Also assert the two runs are byte-identical.
	for i := range first {
		if first[i].Scope.Path != second[i].Scope.Path ||
			first[i].Scope.Kind != second[i].Scope.Kind ||
			first[i].Attrs[recipes.AttrCycleID] != second[i].Attrs[recipes.AttrCycleID] {
			t.Fatalf("draft %d differs across runs (G2 determinism violation)", i)
		}
	}
}

// equalStringSlices is a tiny helper kept local to the
// cycle_member tests (no stdlib `slices.Equal` import to keep
// the test file's Go version requirement minimal).
func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCycleMemberRecipe_ComputeProject_TwoSeparateCycles
// asserts the recipe finds multiple independent SCCs in one
// project and stamps each participant with its own cycle_id.
func TestCycleMemberRecipe_ComputeProject_TwoSeparateCycles(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		makePkgFile("a/a.go", "pkg_a", "pkg_b"),
		makePkgFile("b/b.go", "pkg_b", "pkg_a"),
		makePkgFile("c/c.go", "pkg_c", "pkg_d"),
		makePkgFile("d/d.go", "pkg_d", "pkg_c"),
	}
	drafts := r.ComputeProject(asts)
	cycleIDs := map[string]bool{}
	for _, d := range drafts {
		cycleIDs[d.Attrs[recipes.AttrCycleID]] = true
	}
	want := map[string]bool{
		"scc:pkg_a,pkg_b": true,
		"scc:pkg_c,pkg_d": true,
	}
	if len(cycleIDs) != 2 {
		t.Fatalf("distinct cycle_id count = %d, want 2 (two independent SCCs)", len(cycleIDs))
	}
	for cid := range want {
		if !cycleIDs[cid] {
			t.Fatalf("expected cycle_id %q not present in %v", cid, sortedKeys(cycleIDs))
		}
	}
}

// sortedKeys returns the sorted keys of a `map[string]bool`
// for stable error messages.
func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// makePkgFileAtPath is a fixture helper for the iter-3
// dir-index canonicalisation tests. It builds an AstFile
// where the package's qualifiedName (e.g. `b`) differs from
// the file's directory path (e.g. `myproj/b`) -- the exact
// shape real Go modules produce when import edges target
// the full module-qualified path while package scopes carry
// only the declared basename.
func makePkgFileAtPath(path, pkgName string, imports ...string) *astv1.AstFile {
	b := newAstBuilder(path, true)
	b.addPackage(pkgName)
	for _, imp := range imports {
		b.addImportEdge(imp)
	}
	return b.build()
}

// TestCycleMemberRecipe_ComputeProject_GoStyleImportPathsResolvedViaDirIndex
// pins iter-3 item 2 (dir-index canonicalisation): real Go
// modules emit import edges with `qualified:<full module
// path>` targets (e.g. `qualified:myproj/b`) while the
// package scope's qualifiedName carries only the declared
// basename (`b`). The recipe's two-tier resolver matches
// these via the file-directory index when the file is stored
// at the corresponding directory (e.g. `myproj/b/b.go`).
func TestCycleMemberRecipe_ComputeProject_GoStyleImportPathsResolvedViaDirIndex(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	// File paths carry the full module-relative prefix; the
	// package qualifiedName is just the declared name; the
	// import target is the full module-qualified path.
	asts := []*astv1.AstFile{
		makePkgFileAtPath("myproj/a/a.go", "a", "myproj/b"),
		makePkgFileAtPath("myproj/b/b.go", "b", "myproj/a"),
	}
	drafts := r.ComputeProject(asts)
	// 2 file + 2 package drafts, all value=1, all in the
	// `scc:a,b` cycle. Without the dir-index, this graph
	// would have been seen as fully acyclic (target
	// `myproj/b` does not exact-match qualifiedName `b`).
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(go-style paths) = %d drafts, want 4 (2 file + 2 package)", len(drafts))
	}
	for _, d := range drafts {
		if d.Value != 1.0 {
			t.Fatalf("draft.Value = %v, want 1.0; draft = %+v", d.Value, d)
		}
		if cid := d.Attrs[recipes.AttrCycleID]; cid != "scc:a,b" {
			t.Fatalf("cycle_id = %q, want %q", cid, "scc:a,b")
		}
	}
}

// TestCycleMemberRecipe_ComputeProject_GoStyleImports_NoFabricationOnExternal
// is the iter-3 follow-up to the iter-2 external-basename
// regression test. The dir-index canonicalisation MUST NOT
// resolve external imports to in-project packages even when
// the imports' basename matches an in-project package's
// declared name. Concretely: pkg_a imports
// `github.com/external/pkg_b` (NOT an in-project file path)
// + in-project pkg_b imports pkg_a (back-edge). The dir-index
// has no entry for `github.com/external/pkg_b`; the bare-name
// match also misses (no in-project package is named
// `github.com/external/pkg_b`). The graph is fully acyclic.
//
// iter-5 always-emit: still emits 4 drafts (2 file + 2
// package) all value=0.
func TestCycleMemberRecipe_ComputeProject_GoStyleImports_NoFabricationOnExternal(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		makePkgFileAtPath("myproj/a/a.go", "pkg_a", "github.com/external/pkg_b"),
		makePkgFileAtPath("myproj/b/b.go", "pkg_b", "myproj/a"),
	}
	drafts := r.ComputeProject(asts)
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(external import basename collision) = %d drafts, want 4 (dir-index must NOT fabricate cycles via tail-segment match; iter-5 always-emit emits value=0 for surviving acyclic graph)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_RealGoParserCycle is
// the iter-3 item 4 integration test. It uses the actual
// `parser.DefaultRegistry()` Go parser (registered via
// `init()` in `internal/ast/parser/go.go`) to produce two
// AstFiles with real import edges and asserts cycle_member
// detects the cycle. Without the dir-index canonicalisation,
// the Go parser's `qualified:<full module path>` targets
// would not match the package scope qualifiedName (declared
// basename), and the cycle would be missed.
//
// Pinned by evaluator iter-2 item 4: "Add an integration-
// style test using the actual Go parser output for an import
// cycle, not only `makePkgFile` with already-canonical import
// targets, so the exact-match regression in item 2 is caught
// before production."
func TestCycleMemberRecipe_ComputeProject_RealGoParserCycle(t *testing.T) {
	t.Parallel()
	p, err := parser.DefaultRegistry().For("go")
	if err != nil {
		t.Fatalf("parser.DefaultRegistry().For(go): %v", err)
	}
	// Two real .go source bodies, each importing the other
	// via a module-qualified path. The Go parser produces
	// import edges of the form `qualified:myproj/<other>`.
	sourceA := []byte(`package a

import (
	"myproj/b"
)

func CallB() { b.Hello() }
`)
	sourceB := []byte(`package b

import (
	"myproj/a"
)

func Hello() {}
func CallA() { a.CallB() }
`)
	ctx := context.Background()
	astA, parseErrA := p.Parse(ctx, "myproj/a/a.go", sourceA)
	if parseErrA != nil {
		t.Fatalf("parse a: %v", parseErrA)
	}
	astB, parseErrB := p.Parse(ctx, "myproj/b/b.go", sourceB)
	if parseErrB != nil {
		t.Fatalf("parse b: %v", parseErrB)
	}
	// Sanity check: the parser emitted import edges with
	// the full module-qualified target -- the EXACT shape
	// the exact-match-only resolver from iter-2 would have
	// failed to canonicalise.
	gotTargets := map[string]bool{}
	for _, edge := range astA.GetEdges() {
		if edge.GetKind() == "imports" {
			gotTargets[edge.GetTo().GetId()] = true
		}
	}
	if !gotTargets["qualified:myproj/b"] {
		t.Fatalf("expected import edge target 'qualified:myproj/b' in astA; got %v", gotTargets)
	}
	r := recipes.NewCycleMemberRecipe()
	drafts := r.ComputeProject([]*astv1.AstFile{astA, astB})
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(real go parser, cyclic) = %d drafts, want 4 (2 file + 2 package, all value=1)", len(drafts))
	}
	// Every emitted draft is a cycle participant.
	for _, d := range drafts {
		if d.Value != 1.0 {
			t.Fatalf("draft.Value = %v, want 1.0 (recipe must emit ONLY value=1 rows); draft = %+v", d.Value, d)
		}
		if cid := d.Attrs[recipes.AttrCycleID]; cid != "scc:a,b" {
			t.Fatalf("cycle_id = %q, want %q (real go parser cycle a<->b)", cid, "scc:a,b")
		}
	}
	// Spot-check kinds: 2 file + 2 package.
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
	if fileCount != 2 || pkgCount != 2 {
		t.Fatalf("kind split: file=%d, package=%d; want 2/2", fileCount, pkgCount)
	}
}

// TestCycleMemberRecipe_ComputeProject_RealGoParserAcyclic
// is the negative complement of the integration test above:
// real Go files with a non-cyclic import relationship emit
// ZERO value=1 `cycle_member` rows. Per iter-6 reconciled
// e2e-scenarios.md:418-422 every valid file / package
// scope still emits a value=0 row with empty attrs (the
// brief's universal `value=0 otherwise` contract).
func TestCycleMemberRecipe_ComputeProject_RealGoParserAcyclic(t *testing.T) {
	t.Parallel()
	p, err := parser.DefaultRegistry().For("go")
	if err != nil {
		t.Fatalf("parser.DefaultRegistry().For(go): %v", err)
	}
	sourceA := []byte(`package a

import (
	"myproj/b"
)

func CallB() { b.Hello() }
`)
	sourceB := []byte(`package b

func Hello() {}
`)
	ctx := context.Background()
	astA, parseErrA := p.Parse(ctx, "myproj/a/a.go", sourceA)
	if parseErrA != nil {
		t.Fatalf("parse a: %v", parseErrA)
	}
	astB, parseErrB := p.Parse(ctx, "myproj/b/b.go", sourceB)
	if parseErrB != nil {
		t.Fatalf("parse b: %v", parseErrB)
	}
	r := recipes.NewCycleMemberRecipe()
	drafts := r.ComputeProject([]*astv1.AstFile{astA, astB})
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(real go parser, acyclic) = %d drafts, want 4 (2 file + 2 package, all value=0; iter-5 always-emit)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_SameNamePackagesInDifferentDirsAreSeparateNodes
// pins iter-4 item 4 (package identity collision defense):
// real Go projects can have multiple `main` packages (one
// per `cmd/<name>/main.go`) plus multiple `internal`
// packages across subtrees. The recipe MUST treat such co-
// named packages as DISTINCT SCC graph nodes (compound
// identity `<qualifiedName>@<canonical-dir>`); aliasing them
// would fabricate cycles where independent main packages
// share util dependencies. Here we set up two `main`
// packages (`cmd/a/main.go` and `cmd/b/main.go`) each
// importing a shared `util` package -- there is NO cycle.
//
// iter-5 always-emit: 3 files + 3 distinct package
// identities (main@cmd/a, main@cmd/b, util@myproj/util) =
// 6 drafts, all value=0.
func TestCycleMemberRecipe_ComputeProject_SameNamePackagesInDifferentDirsAreSeparateNodes(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		// Two distinct `main` packages, each in its own
		// directory; each imports `myproj/util`. The Go
		// idiom for cmd/X is `package main`.
		makePkgFileAtPath("myproj/cmd/a/main.go", "main", "myproj/util"),
		makePkgFileAtPath("myproj/cmd/b/main.go", "main", "myproj/util"),
		// The shared util package -- no back-edges, so the
		// graph is acyclic.
		makePkgFileAtPath("myproj/util/util.go", "util"),
	}
	drafts := r.ComputeProject(asts)
	// 3 file drafts + 3 distinct package identities
	// (main@cmd/a, main@cmd/b, util@util) = 6 drafts, all
	// value=0.
	if len(drafts) != 6 {
		t.Fatalf("ComputeProject(co-named acyclic) = %d drafts, want 6 (3 file + 3 package identities, all value=0; compound identity keeps the two main packages distinct)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_SameNamePackagesNoFabricatedCycle
// pins the absence of fabricated cycles when two co-named
// packages have different import sets. Without compound
// identity, the recipe would alias `main@cmd/a` and
// `main@cmd/b` to the same node, and the union of their
// import edges {util, lib} -> back-edge from `lib` to
// `main` would create a fabricated cycle. With compound
// identity, the two `main`s are distinct nodes and the
// back-edge from `lib` to `main@cmd/a` does NOT close a
// cycle through `main@cmd/b`.
func TestCycleMemberRecipe_ComputeProject_SameNamePackagesNoFabricatedCycle(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		// `main` at cmd/a imports `lib` (sets up the
		// suspected cycle).
		makePkgFileAtPath("myproj/cmd/a/main.go", "main", "myproj/lib"),
		// `main` at cmd/b imports `util` only.
		makePkgFileAtPath("myproj/cmd/b/main.go", "main", "myproj/util"),
		// `lib` imports `main` at cmd/b -- this WOULD
		// close a fabricated cycle if both `main`s were
		// aliased. The import target is `myproj/cmd/b`
		// (the directory of the `main` we want to point
		// at), but Go semantics forbid importing `main`
		// packages -- this fixture exists purely to drive
		// the aliasing scenario.
		makePkgFileAtPath("myproj/lib/lib.go", "lib", "myproj/cmd/b"),
		makePkgFileAtPath("myproj/util/util.go", "util"),
	}
	drafts := r.ComputeProject(asts)
	// The graph (with compound identity) is:
	//   main@cmd/a -> lib@lib
	//   main@cmd/b -> util@util
	//   lib@lib    -> main@cmd/b   (back-edge to a SPECIFIC main)
	// This is acyclic: lib points at main@cmd/b which has
	// no edges back through any path to lib. Without
	// compound identity, lib -> main (aliased) -> lib
	// would be a fabricated 2-cycle. iter-5 always-emit:
	// 4 files + 4 distinct package identities = 8 drafts,
	// all value=0.
	if len(drafts) != 8 {
		t.Fatalf("ComputeProject(co-named with back-edge) = %d drafts, want 8 (4 file + 4 package identities, all value=0; compound identity must prevent fabricated cycle)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_ModulePathCanonicalisationResolvesImports
// pins iter-5 item 2 (module-path canonicalisation for
// module-qualified imports whose prefix differs from the
// scan-relative path). Setup: files are stored at scan-
// relative paths like `internal/foo/foo.go` but the Go
// module path is `github.com/org/repo`, so the actual
// import target is `github.com/org/repo/internal/foo`. The
// module path is stamped on each AstFile via
// `Attrs[AttrModulePath]`. The resolver's tier-3 module-
// path canonicalisation strips the module prefix and re-
// runs the exact-dir lookup on the residual `internal/foo`.
//
// Replaces the iter-4 multi-segment-suffix tier (REMOVED in
// iter-5 because suffix matching is unsafe without
// authoritative module metadata; iter-5 evaluator item 2).
func TestCycleMemberRecipe_ComputeProject_ModulePathCanonicalisationResolvesImports(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	bFoo := newAstBuilder("internal/foo/foo.go", true)
	bFoo.addPackage("foo")
	bFoo.addImportEdge("github.com/org/repo/internal/bar")
	bFoo.setModulePath("github.com/org/repo")
	bBar := newAstBuilder("internal/bar/bar.go", true)
	bBar.addPackage("bar")
	bBar.addImportEdge("github.com/org/repo/internal/foo")
	bBar.setModulePath("github.com/org/repo")
	asts := []*astv1.AstFile{bFoo.build(), bBar.build()}
	drafts := r.ComputeProject(asts)
	// Module-path canonicalisation strips
	// `github.com/org/repo/` and the residual `internal/foo`
	// / `internal/bar` resolve via exact-dir match, forming
	// the cycle foo <-> bar. Expect 2 file + 2 package
	// drafts, all value=1.
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(module-path canonicalisation) = %d drafts, want 4 (cycle bar<->foo)", len(drafts))
	}
	for _, d := range drafts {
		if d.Value != 1.0 {
			t.Fatalf("draft.Value = %v, want 1.0 (module-path canonicalisation should resolve module-qualified imports to cycle); draft = %+v", d.Value, d)
		}
		if cid := d.Attrs[recipes.AttrCycleID]; cid != "scc:bar,foo" {
			t.Fatalf("cycle_id = %q, want %q", cid, "scc:bar,foo")
		}
	}
}

// TestCycleMemberRecipe_ComputeProject_ModulePathBoundaryMatch
// pins iter-5 item 2 boundary safety: the module-path
// canonicalisation tier MUST use a path-BOUNDARY match
// (`target == m` OR `strings.HasPrefix(target, m + "/")`),
// NOT raw HasPrefix. Without the boundary, module
// `github.com/org/repo` would falsely match an external
// import `github.com/org/repo2/foo` (raw HasPrefix is true).
// With the boundary, the trailing `2` breaks the match and
// the external import is dropped.
func TestCycleMemberRecipe_ComputeProject_ModulePathBoundaryMatch(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	bFoo := newAstBuilder("internal/foo/foo.go", true)
	bFoo.addPackage("foo")
	// External import whose path starts with the module
	// path but is NOT in this module: target =
	// `github.com/org/repo2/foo` vs module
	// `github.com/org/repo`. Raw HasPrefix is true; boundary
	// match is FALSE (no trailing `/` after the module).
	bFoo.addImportEdge("github.com/org/repo2/foo")
	bFoo.setModulePath("github.com/org/repo")
	asts := []*astv1.AstFile{bFoo.build()}
	drafts := r.ComputeProject(asts)
	// 1 file + 1 package = 2 drafts, both value=0 (external
	// import dropped via boundary safety).
	if len(drafts) != 2 {
		t.Fatalf("ComputeProject(module-path boundary) = %d drafts, want 2 (1 file + 1 package, all value=0)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_ExternalImportWithLocalDirectoryTailNotFabricated
// is iter-5 evaluator item 2 negative test: an external
// import `github.com/other/repo/internal/foo` whose tail
// `internal/foo` matches a local `internal/foo` package
// MUST NOT be resolved to the local package. The iter-4
// multi-segment suffix matcher would have fabricated this
// edge; the iter-5 resolver REMOVED that tier so the
// import is correctly treated as external.
func TestCycleMemberRecipe_ComputeProject_ExternalImportWithLocalDirectoryTailNotFabricated(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	// No module_path attr set, so the resolver only has the
	// exact-dir and exact-qn tiers. Local `internal/foo`
	// exists; importer targets `github.com/other/repo/internal/foo`.
	// Without authoritative module metadata, the resolver
	// MUST treat the import as external.
	asts := []*astv1.AstFile{
		makePkgFileAtPath("internal/foo/foo.go", "foo"),
		makePkgFileAtPath("internal/bar/bar.go", "bar", "github.com/other/repo/internal/foo"),
	}
	drafts := r.ComputeProject(asts)
	// 2 file + 2 package = 4 drafts, all value=0 (no
	// fabricated cycle).
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(external tail collision) = %d drafts, want 4 (all value=0; iter-5 suffix-tier removal must prevent fabrication)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_SingleSegmentSuffixDoesNotFabricate
// is a legacy guard kept after the iter-5 suffix tier
// removal: external imports whose last segment matches an
// in-project package name MUST NOT be resolved. Single-
// segment basenames are NEVER a valid resolution source
// (the resolver only honours exact-dir, exact-qn, and
// module-path canonicalisation -- none of which match here).
//
// iter-5 always-emit: 2 file + 2 package = 4 drafts, all
// value=0.
func TestCycleMemberRecipe_ComputeProject_SingleSegmentSuffixDoesNotFabricate(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	asts := []*astv1.AstFile{
		// pkg_b imports `github.com/external/foo` (last
		// segment `foo` matches in-project `foo`). The
		// resolver MUST NOT match by basename.
		makePkgFileAtPath("internal/foo/foo.go", "foo", "github.com/elsewhere/bar"),
		makePkgFileAtPath("internal/bar/bar.go", "bar", "github.com/external/foo"),
	}
	drafts := r.ComputeProject(asts)
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject(single-segment external tail) = %d drafts, want 4 (all value=0; basename match MUST NOT fabricate cycles)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_DirMatchPreferredOverQn
// pins the iter-4 resolver order swap: when an import target
// matches BOTH the dir-index (most authoritative, module-
// style) AND a unique qualifiedName, the recipe MUST resolve
// via the dir-index. This binds one-segment imports like
// `util` to the exact `util/...` directory rather than to a
// same-named package elsewhere in the tree.
//
// iter-5 always-emit: 3 file + 3 distinct package
// identities (util@util, util@vendor/util, main@cmd/x) =
// 6 drafts, all value=0.
func TestCycleMemberRecipe_ComputeProject_DirMatchPreferredOverQn(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	// Two packages BOTH named `util` -- one at the dir
	// `util/` (matches the import target exactly), one at
	// `vendor/util/`. Importer at `cmd/x/main.go` imports
	// `util`. The dir-match `dirToIdent["util"]` wins
	// over the (ambiguous) qualifiedName match.
	asts := []*astv1.AstFile{
		makePkgFileAtPath("util/util.go", "util"),
		makePkgFileAtPath("vendor/util/util.go", "util"),
		makePkgFileAtPath("cmd/x/main.go", "main", "util"),
	}
	drafts := r.ComputeProject(asts)
	// No cycle here -- main -> util/util has no back-edge,
	// vendor/util is unreached. iter-5 always-emit: 6
	// drafts all value=0.
	if len(drafts) != 6 {
		t.Fatalf("ComputeProject(dir-match preferred) = %d drafts, want 6 (all value=0; project is fully acyclic)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}

// TestCycleMemberRecipe_ComputeProject_AmbiguousBareNameImportDropped
// pins the explicit ambiguous-bare-name case for item 4:
// when an import target matches MULTIPLE qualifiedNames
// (no unique resolution) AND has no dir-index entry AND
// no module-path canonicalisation match, the resolver
// returns no match -- preventing a fabricated edge to one
// of the ambiguous candidates.
//
// iter-5 always-emit: 3 files + 3 distinct package
// identities (internal@moduleA/internal, internal@moduleB/internal,
// bar@moduleA/bar) = 6 drafts, all value=0.
func TestCycleMemberRecipe_ComputeProject_AmbiguousBareNameImportDropped(t *testing.T) {
	t.Parallel()
	r := recipes.NewCycleMemberRecipe()
	// Two distinct `internal` packages plus one importer
	// targeting the bare name `internal`. The dir-index
	// has no "internal" key (file dirs are
	// "moduleA/internal" and "moduleB/internal", not the
	// bare "internal"). qnToIdents["internal"] has two
	// entries (ambiguous). The recipe drops the import.
	asts := []*astv1.AstFile{
		makePkgFileAtPath("moduleA/internal/internal.go", "internal"),
		makePkgFileAtPath("moduleB/internal/internal.go", "internal"),
		makePkgFileAtPath("moduleA/bar/bar.go", "bar", "internal"),
	}
	drafts := r.ComputeProject(asts)
	if len(drafts) != 6 {
		t.Fatalf("ComputeProject(ambiguous bare-name import) = %d drafts, want 6 (all value=0; ambiguous qn match MUST be dropped)", len(drafts))
	}
	assertAllNonCycle(t, drafts)
}
