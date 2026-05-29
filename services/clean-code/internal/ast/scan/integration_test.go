package scan_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"forge/services/clean-code/internal/ast/parser"
	"forge/services/clean-code/internal/ast/scan"
	"forge/services/clean-code/internal/metrics/recipes"
)

// writeFile is a tiny test helper that creates parent
// directories and writes the file in one call so the
// integration tests can stay readable.
func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %q: %v", path, err)
	}
}

// TestIntegration_ScanToRecipeEndToEnd is the iter-6 item 1
// proof-of-wiring test. It exercises the FULL production
// chain end-to-end:
//
//  1. A temp project with a real `go.mod` and two cyclically
//     importing packages, where the import targets are
//     MODULE-QUALIFIED (`github.com/org/repo/internal/<x>`)
//     rather than scan-relative paths.
//
//  2. The real Go parser (from `parser.DefaultRegistry`)
//     parses each file. Each AstFile carries import edges
//     keyed by the full module-qualified target string -- NOT
//     by the scan-relative directory name. Without module-
//     path canonicalisation the edges would be orphaned
//     (they don't match any in-project file's qualifiedName
//     and don't match any in-project directory either).
//
//  3. `scan.AnnotateProjectAsts(root, asts)` reads
//     `<root>/go.mod`, extracts the module path, and stamps
//     `Attrs[parser.AttrModulePath]` on every AstFile.
//
//  4. `recipes.NewCycleMemberRecipe().ComputeProject(asts)`
//     uses the stamped module path to canonicalise each
//     import target: it strips the module prefix
//     `github.com/org/repo/` and re-runs the exact-dir
//     lookup against the residual `internal/foo` /
//     `internal/bar`. With the lookup succeeding the cycle
//     is detected and value=1 drafts are emitted.
//
// Without iter-6's scan-package wiring the chain breaks at
// step 3 (no production code populates `Attrs[AttrModulePath]`,
// so the resolver has no authoritative module metadata, so
// step 4 returns value=0 for every scope). This test is the
// definitive evidence that the iter-6 production helper does
// what the evaluator asked for.
func TestIntegration_ScanToRecipeEndToEnd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// go.mod declares the module path. The recipe's tier-3
	// canonicalisation reads this via the AstFile's
	// `Attrs[AttrModulePath]` -- but only the scan-layer
	// helper populates that attr. Until iter-6 the attr was
	// only reachable from tests via `setModulePath`.
	writeFile(t, filepath.Join(root, "go.mod"),
		"module github.com/org/repo\n\ngo 1.21\n")
	// Two packages that import each other via the FULL
	// module-qualified path. This is the shape the Go parser
	// emits in production -- not the scan-relative form the
	// recipe's exact-dir tier alone would catch.
	writeFile(t, filepath.Join(root, "internal", "foo", "foo.go"),
		`package foo

import "github.com/org/repo/internal/bar"

func CallBar() { bar.Hello() }
`)
	writeFile(t, filepath.Join(root, "internal", "bar", "bar.go"),
		`package bar

import "github.com/org/repo/internal/foo"

func Hello() {}
func CallFoo() { foo.CallBar() }
`)

	p, err := parser.DefaultRegistry().For("go")
	if err != nil {
		t.Fatalf("parser.DefaultRegistry().For(go): %v", err)
	}
	ctx := context.Background()
	parseRel := func(rel string) *parser.AstFile {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		content, readErr := os.ReadFile(abs)
		if readErr != nil {
			t.Fatalf("read %s: %v", abs, readErr)
		}
		ast, parseErr := p.Parse(ctx, rel, content)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", rel, parseErr)
		}
		return ast
	}
	asts := []*parser.AstFile{
		parseRel("internal/foo/foo.go"),
		parseRel("internal/bar/bar.go"),
	}

	// Before annotation: the recipe should NOT detect the
	// cycle, because the resolver has no module metadata and
	// the import targets carry the unstripped module prefix
	// that won't match either qualifiedName or scan-relative
	// directory.
	preDrafts := recipes.NewCycleMemberRecipe().ComputeProject(asts)
	preCycle := 0
	for _, d := range preDrafts {
		if d.Value == 1.0 {
			preCycle++
		}
	}
	if preCycle != 0 {
		t.Fatalf("pre-annotate: %d value=1 drafts, want 0 (resolver should fail closed without module metadata)", preCycle)
	}

	// Annotate via the production helper (the iter-6 item 1
	// wiring under test).
	mp, annotErr := scan.AnnotateProjectAsts(root, asts)
	if annotErr != nil {
		t.Fatalf("scan.AnnotateProjectAsts: %v", annotErr)
	}
	if mp != "github.com/org/repo" {
		t.Fatalf("detected module path = %q, want %q", mp, "github.com/org/repo")
	}
	for i, ast := range asts {
		if got := ast.GetAttrs()[parser.AttrModulePath]; got != "github.com/org/repo" {
			t.Fatalf("ast[%d] module_path = %q, want %q (scan.AnnotateProjectAsts did not stamp)", i, got, "github.com/org/repo")
		}
	}

	// After annotation: the recipe's tier-3 canonicalisation
	// strips the module prefix and the residual paths match
	// the in-project directories. The cycle is detected.
	postDrafts := recipes.NewCycleMemberRecipe().ComputeProject(asts)
	if len(postDrafts) != 4 {
		t.Fatalf("post-annotate: %d drafts, want 4 (2 file + 2 package, all value=1)", len(postDrafts))
	}
	for _, d := range postDrafts {
		if d.Value != 1.0 {
			t.Fatalf("post-annotate draft.Value = %v, want 1.0 (cycle bar<->foo via module-path canonicalisation); draft = %+v", d.Value, d)
		}
		if cid := d.Attrs[recipes.AttrCycleID]; cid != "scc:bar,foo" {
			t.Fatalf("cycle_id = %q, want %q", cid, "scc:bar,foo")
		}
	}
}

// TestIntegration_ScanWithoutGoMod_FallsBackClosed pins the
// evaluator's item-2 false-positive concern at the
// production-wiring layer: a project with NO go.mod whose
// import target's tail happens to match a local directory
// MUST NOT fabricate a cycle. The scan helper finds no
// go.mod, returns ("", nil) without erroring, and the
// recipe falls back to exact-dir / exact-qn matching
// (which correctly drops the external import because the
// full path `github.com/other/repo/internal/foo` does NOT
// equal any in-project directory).
func TestIntegration_ScanWithoutGoMod_FallsBackClosed(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// NO go.mod in root. Two local packages, one importing
	// an external repo whose path tail accidentally matches
	// the other local package name.
	writeFile(t, filepath.Join(root, "internal", "foo", "foo.go"),
		"package foo\n\nfunc Hello() {}\n")
	writeFile(t, filepath.Join(root, "internal", "bar", "bar.go"),
		`package bar

import "github.com/other/repo/internal/foo"

func CallFoo() { foo.Hello() }
`)

	p, err := parser.DefaultRegistry().For("go")
	if err != nil {
		t.Fatalf("parser.DefaultRegistry().For(go): %v", err)
	}
	ctx := context.Background()
	parseRel := func(rel string) *parser.AstFile {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		content, readErr := os.ReadFile(abs)
		if readErr != nil {
			t.Fatalf("read %s: %v", abs, readErr)
		}
		ast, parseErr := p.Parse(ctx, rel, content)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", rel, parseErr)
		}
		return ast
	}
	asts := []*parser.AstFile{
		parseRel("internal/foo/foo.go"),
		parseRel("internal/bar/bar.go"),
	}

	// Annotate -- with no go.mod the helper returns empty
	// module path and stamps nothing.
	mp, annotErr := scan.AnnotateProjectAsts(root, asts)
	if annotErr != nil {
		t.Fatalf("AnnotateProjectAsts: %v", annotErr)
	}
	if mp != "" {
		t.Fatalf("mp = %q, want empty (no go.mod)", mp)
	}
	for i, ast := range asts {
		if _, present := ast.GetAttrs()[parser.AttrModulePath]; present {
			t.Fatalf("ast[%d] got module_path stamp despite no go.mod", i)
		}
	}

	// Recipe must NOT fabricate the cycle: no value=1 rows
	// (resolver has no module metadata, exact-dir lookup
	// fails for `github.com/other/repo/internal/foo`).
	drafts := recipes.NewCycleMemberRecipe().ComputeProject(asts)
	if len(drafts) != 4 {
		t.Fatalf("ComputeProject = %d drafts, want 4 (2 file + 2 package, all value=0)", len(drafts))
	}
	for _, d := range drafts {
		if d.Value != 0.0 {
			t.Fatalf("draft.Value = %v, want 0.0 (external import must NOT be canonicalised to local pkg); draft = %+v", d.Value, d)
		}
	}
}

// TestIntegration_AnnotateAstsByNearestGoMod_StampsPerModule
// pins the iter-6 multi-module stamping contract: a scan
// root with TWO nested modules stamps each AstFile against
// ITS module path (NOT the parent's). The recipe's longest-
// prefix module-path matcher then has the right per-file
// metadata for each module's internal imports.
//
// Note on scope: cross-module cycle detection in a single
// ComputeProject call would require carrying the per-file
// module ROOT DIRECTORY (not just the module path) so the
// resolver can combine `stripped` with the module's
// workspace-relative root to find the canonical directory.
// That is future work; the recipe is presently a single-
// module engine and the canonical scan-layer pattern is
// "run ComputeProject once per module". This test verifies
// only the STAMPING contract (which is what iter-6 item 1
// asked for); per-module ComputeProject behaviour is covered
// by [TestIntegration_ScanToRecipeEndToEnd].
func TestIntegration_AnnotateAstsByNearestGoMod_StampsPerModule(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "modA", "go.mod"),
		"module github.com/org/modA\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "modA", "foo", "foo.go"),
		"package foo\n\nfunc Hello() {}\n")
	writeFile(t, filepath.Join(root, "modA", "bar", "bar.go"),
		"package bar\n\nfunc Hello() {}\n")
	writeFile(t, filepath.Join(root, "modB", "go.mod"),
		"module github.com/org/modB\n\ngo 1.21\n")
	writeFile(t, filepath.Join(root, "modB", "qux", "qux.go"),
		"package qux\n\nfunc Hello() {}\n")

	p, err := parser.DefaultRegistry().For("go")
	if err != nil {
		t.Fatalf("parser.DefaultRegistry().For(go): %v", err)
	}
	ctx := context.Background()
	parseRel := func(rel string) *parser.AstFile {
		t.Helper()
		abs := filepath.Join(root, filepath.FromSlash(rel))
		content, readErr := os.ReadFile(abs)
		if readErr != nil {
			t.Fatalf("read %s: %v", abs, readErr)
		}
		ast, parseErr := p.Parse(ctx, rel, content)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", rel, parseErr)
		}
		return ast
	}
	asts := []*parser.AstFile{
		parseRel("modA/foo/foo.go"),
		parseRel("modA/bar/bar.go"),
		parseRel("modB/qux/qux.go"),
	}

	if err := scan.AnnotateAstsByNearestGoMod(root, asts); err != nil {
		t.Fatalf("AnnotateAstsByNearestGoMod: %v", err)
	}
	wantMP := map[string]string{
		"modA/foo/foo.go": "github.com/org/modA",
		"modA/bar/bar.go": "github.com/org/modA",
		"modB/qux/qux.go": "github.com/org/modB",
	}
	for _, ast := range asts {
		got := ast.GetAttrs()[parser.AttrModulePath]
		want := wantMP[ast.GetPath()]
		if got != want {
			t.Fatalf("ast %s: module_path = %q, want %q (multi-module attribution broken)", ast.GetPath(), got, want)
		}
	}
}
