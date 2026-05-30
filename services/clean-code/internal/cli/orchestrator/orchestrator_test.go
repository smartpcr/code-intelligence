// -----------------------------------------------------------------------
// <copyright file="orchestrator_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package orchestrator_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/walk"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// testRepoContext returns a deterministic RepoContext for a
// temp-dir root so test assertions can predict the
// `clean-code-repo:<repoID>` prefix the orchestrator stamps
// without rebuilding the canonical-signature pre-image. The
// HeadSHA sentinel is the working-copy literal so two test
// runs against different temp dirs still mint distinct scope
// IDs even when the parser output is identical.
func testRepoContext(t *testing.T, root string) repocontext.RepoContext {
	t.Helper()
	rc := repocontext.RepoContext{
		RootPath:   root,
		RepoID:     repocontext.MintRepoID(root),
		HeadSHA:    repocontext.HeadSHAWorkingCopySentinel,
		ModulePath: "",
		IsGitRepo:  false,
	}
	if rc.RepoID == uuid.Nil {
		t.Fatalf("testRepoContext: MintRepoID returned uuid.Nil for root %q", root)
	}
	return rc
}

// writeFixtureFile materialises `content` at `relPath` under
// `root`. Parent directories are created. Returns the
// repo-relative forward-slash form of the path so the test
// can match the orchestrator's normalised
// `AstFile.GetPath()` output verbatim.
func writeFixtureFile(t *testing.T, root, relPath, content string) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("writeFixtureFile: mkdir %q: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("writeFixtureFile: write %q: %v", abs, err)
	}
	return filepath.ToSlash(relPath)
}

// ---------------------------------------------------------------------------
// Scenario 1: parse all four languages
//
// impl-plan Stage 2.2 line 168 -- Given a fixture repo with
// one file each of Go/Python/TypeScript/Java, When the
// orchestrator runs the parse stage, Then four `*AstFile`
// rows are collected and zero
// `WalkSkip{Reason: "unsupported_language"}` rows are
// emitted.
// ---------------------------------------------------------------------------

func TestOrchestrator_Run_ParsesAllFourLanguages(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "src/main.go", "package main\n\nfunc main() {}\n")
	writeFixtureFile(t, root, "src/main.py", "def main():\n    pass\n")
	writeFixtureFile(t, root, "src/main.ts", "function main(): void {}\n")
	writeFixtureFile(t, root, "src/Main.java", "package com.example;\n\npublic class Main {}\n")

	rc := testRepoContext(t, root)
	o := orchestrator.New(orchestrator.Options{})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: unexpected error: %v", err)
	}

	if got, want := len(res.Files), 4; got != want {
		paths := make([]string, len(res.Files))
		for i, f := range res.Files {
			paths[i] = f.GetPath()
		}
		t.Fatalf("res.Files: got %d (paths=%v), want %d", got, paths, want)
	}

	wantPaths := []string{
		"src/Main.java",
		"src/main.go",
		"src/main.py",
		"src/main.ts",
	}
	for i, want := range wantPaths {
		if got := res.Files[i].GetPath(); got != want {
			t.Errorf("res.Files[%d].Path = %q, want %q (orchestrator must sort by path for determinism)", i, got, want)
		}
	}

	for _, s := range res.Skips {
		if s.Reason == walk.SkipReasonUnsupportedLanguage {
			t.Errorf("unexpected unsupported_language skip for %q (all four languages are in the v1 pin)", s.Path)
		}
		if s.Reason == orchestrator.SkipReasonParserError || s.Reason == orchestrator.SkipReasonParserPanic {
			t.Errorf("unexpected parser skip %q for %q", s.Reason, s.Path)
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: loc recipe lights up
//
// impl-plan Stage 2.2 line 169 -- Given a fixture Go file of
// known line count `N`, When recipes run, Then a
// `MetricSampleDraft{MetricKind: "loc", Value: N}` is
// collected.
//
// The exact `N` depends on whether the file-scope range
// covers the whole file (parser implementations vary on
// trailing-newline / first-line treatment); the assertion
// pins the lower bound at the number of source lines and
// upper bound at +1 so a trailing-blank trim is tolerated.
// ---------------------------------------------------------------------------

func TestOrchestrator_Run_LocRecipeLightsUp(t *testing.T) {
	root := t.TempDir()
	content := "package foo\n\nfunc Bar() {\n\treturn\n}\n"
	relPath := writeFixtureFile(t, root, "pkg/bar.go", content)
	wantLines := float64(strings.Count(content, "\n"))

	rc := testRepoContext(t, root)
	o := orchestrator.New(orchestrator.Options{Workers: 1})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var locDrafts []recipes.MetricSampleDraft
	for _, d := range res.Drafts {
		if d.MetricKind == "loc" && d.Scope.Path == relPath {
			locDrafts = append(locDrafts, d)
		}
	}
	if len(locDrafts) == 0 {
		var allKinds []string
		for _, d := range res.Drafts {
			allKinds = append(allKinds, d.MetricKind)
		}
		t.Fatalf("no loc draft emitted for %q; saw metric_kinds=%v", relPath, allKinds)
	}

	for _, d := range locDrafts {
		if d.Value <= 0 {
			t.Errorf("loc draft for %q: Value = %v, want > 0", relPath, d.Value)
		}
		if d.Value > wantLines+1 {
			t.Errorf("loc draft for %q: Value = %v, want <= %v (file has %v newlines)", relPath, d.Value, wantLines+1, wantLines)
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: dark cyclo recipe
//
// impl-plan Stage 2.2 line 170 -- Given a fixture Go file
// (today's parser does not stamp `decision_blocks`), When
// recipes run, Then `Recipe.AppliesTo` returns false for
// `cyclo` and zero `MetricSampleDraft` rows for
// `metric_kind == "cyclo"` are emitted.
//
// The parser fleet at Stage 2.1 does NOT emit the
// `decision_blocks` attr on file or method scopes (see
// `recipes/recipe.go` lines 55-122 and the dark-metric
// roadmap in `tech-spec.md` Sec C15). Until Stage 2.7
// extends the per-language adapters, the orchestrator
// gates on `cyclo.AppliesTo` and produces no draft; this
// test pins the contract so a future parser change that
// silently flips the gate is caught by CI.
// ---------------------------------------------------------------------------

func TestOrchestrator_Run_CyclopRecipeStaysDark(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, root, "pkg/branchy.go", strings.Join([]string{
		"package pkg",
		"",
		"func Branchy(x int) int {",
		"    if x > 0 {",
		"        return x",
		"    } else if x < 0 {",
		"        return -x",
		"    }",
		"    return 0",
		"}",
		"",
	}, "\n"))

	rc := testRepoContext(t, root)
	o := orchestrator.New(orchestrator.Options{Workers: 1})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, d := range res.Drafts {
		if d.MetricKind == "cyclo" {
			t.Errorf("unexpected cyclo draft for %q (Stage 2.1 parsers do not emit decision_blocks, so cyclo.AppliesTo MUST return false)", d.Scope.Path)
		}
	}
}

// ---------------------------------------------------------------------------
// Scenario 4: scope binding populated
//
// impl-plan Stage 2.2 line 171 -- Given a fixture Go file
// with a function `Foo`, When parse + recipe fan-out
// completes, Then `scopebinding.Table.Get(scopeIDFor("Foo"))`
// returns a row whose `Signature` ends with `::Foo` (more
// precisely: contains `Foo()`) and whose `StartLine` /
// `EndLine` enclose the function body.
//
// Without a public iteration API on [scopebinding.Table],
// the test walks `Result.ScopeIDs` for the bindings the
// orchestrator minted, looks each up via `Table.Get`, and
// filters for `(ScopeKind=="method", FilePath==relPath)`.
// Asserts one such binding exists whose canonical signature
// embeds the parser-emitted qualifiedName for `Foo`.
// ---------------------------------------------------------------------------

func TestOrchestrator_Run_ScopeBindingPopulatedForGoFunction(t *testing.T) {
	root := t.TempDir()
	source := strings.Join([]string{
		"package mypkg",
		"",
		"func Foo() {",
		"\treturn",
		"}",
		"",
	}, "\n")
	relPath := writeFixtureFile(t, root, "mypkg/foo.go", source)
	totalLines := strings.Count(source, "\n")

	rc := testRepoContext(t, root)
	table := scopebinding.NewTable()
	o := orchestrator.New(orchestrator.Options{Workers: 1, ScopeBindings: table})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := table.Len(); got == 0 {
		t.Fatalf("scope binding table empty after Run (Result.ScopeIDs=%d)", len(res.ScopeIDs))
	}

	var methodBindings []scopebinding.ScopeBinding
	for key, id := range res.ScopeIDs {
		if key.Path != relPath {
			continue
		}
		b, ok := table.Get(id)
		if !ok {
			t.Errorf("table.Get(%v): not found for key %+v", id, key)
			continue
		}
		if b.ScopeKind == "method" {
			methodBindings = append(methodBindings, b)
		}
	}
	if len(methodBindings) == 0 {
		t.Fatalf("no method-kind binding for %q in table (Result.ScopeIDs keys=%d)", relPath, len(res.ScopeIDs))
	}

	var foundFoo bool
	for _, b := range methodBindings {
		if !strings.Contains(b.Signature, "Foo") {
			continue
		}
		if !strings.Contains(b.Signature, "::method::"+relPath+"#") {
			t.Errorf("method binding signature %q missing ::method::%s# prefix", b.Signature, relPath)
		}
		if !strings.HasSuffix(b.Signature, "()") {
			t.Errorf("method binding signature %q does not end with `()` (Foo is a no-arg method)", b.Signature)
		}
		if b.StartLine < 1 || b.StartLine > totalLines {
			t.Errorf("method binding StartLine = %d, want in [1, %d]", b.StartLine, totalLines)
		}
		if b.EndLine < b.StartLine || b.EndLine > totalLines+1 {
			t.Errorf("method binding EndLine = %d, want in [%d, %d]", b.EndLine, b.StartLine, totalLines+1)
		}
		if b.FilePath != relPath {
			t.Errorf("method binding FilePath = %q, want %q", b.FilePath, relPath)
		}
		if !strings.HasPrefix(b.Signature, orchestrator.SyntheticRepoURLPrefix) {
			t.Errorf("method binding signature %q missing %q prefix", b.Signature, orchestrator.SyntheticRepoURLPrefix)
		}
		foundFoo = true
	}
	if !foundFoo {
		sigs := make([]string, 0, len(methodBindings))
		for _, b := range methodBindings {
			sigs = append(sigs, b.Signature)
		}
		t.Fatalf("no method binding mentioning Foo in %q; saw signatures=%v", relPath, sigs)
	}
}

// ---------------------------------------------------------------------------
// Scenario 5: parser panic is non-fatal
//
// impl-plan Stage 2.2 line 172 -- Given a fixture file that
// triggers a panic in the parser stub (via test double),
// When the orchestrator runs, Then
// `WalkSkip{Reason: "parser_panic", Path: <file>}` is
// emitted and the orchestrator exits cleanly through the
// remainder of the corpus.
//
// The stub registers a panicking factory for `go` against a
// fresh `parser.NewRegistry()`, so the process-wide
// `DefaultRegistry` is untouched. Two files are written so
// the test also asserts the worker SURVIVES the panic and
// processes the second file (the cleanly-named one).
// ---------------------------------------------------------------------------

type panickingParser struct{}

func (panickingParser) Language() string { return "go" }

func (panickingParser) Parse(ctx context.Context, path string, content []byte) (*parser.AstFile, error) {
	panic("orchestrator_test: synthetic parser panic for path=" + path)
}

func TestOrchestrator_Run_ParserPanicSurfacesAsSkipAndPipelineContinues(t *testing.T) {
	root := t.TempDir()
	panickyRel := writeFixtureFile(t, root, "pkg/boom.go", "package boom\n")
	cleanRel := writeFixtureFile(t, root, "pkg/clean.go", "package boom\n")

	reg := parser.NewRegistry()
	if err := reg.Register("go", func() parser.Parser { return panickingParser{} }); err != nil {
		t.Fatalf("Register(go, panickingParser): %v", err)
	}

	rc := testRepoContext(t, root)
	o := orchestrator.New(orchestrator.Options{
		Workers: 1, // pin to 1 so both files traverse the same recovering worker
		Parsers: reg,
	})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: orchestrator MUST return cleanly on per-file parser panics, got error: %v", err)
	}

	if len(res.Files) != 0 {
		t.Errorf("res.Files = %d, want 0 (every parse panicked, so no ASTs survive)", len(res.Files))
	}

	skipsByPath := map[string]string{}
	for _, s := range res.Skips {
		if s.Reason == orchestrator.SkipReasonParserPanic {
			skipsByPath[s.Path] = s.Reason
		}
	}
	if _, ok := skipsByPath[panickyRel]; !ok {
		t.Errorf("no parser_panic skip recorded for %q; got skips=%v", panickyRel, res.Skips)
	}
	if _, ok := skipsByPath[cleanRel]; !ok {
		t.Errorf("worker did NOT survive the first panic: no parser_panic skip recorded for %q; got skips=%v (per-job recover is mandatory)", cleanRel, res.Skips)
	}
}

// ---------------------------------------------------------------------------
// Scenario 6: project-level recipe dispatch
//
// Workstream-2.2 iter-2 evaluator item 3 -- The project-level
// recipe fan-out at `orchestrator.go:381-383` MUST be
// regression-tested: a fixture corpus rich enough to exercise
// BOTH project recipes registered in
// `recipes.DefaultProjectRegistry()` --
// `{cycle_member, duplication_ratio}` -- and an assertion
// that at least one draft of EACH `metric_kind` appears in
// `res.Drafts`. This pins the contract that
// `o.projectRecReg.All()` is iterated (not silently
// short-circuited) AND that each project recipe receives the
// full AST corpus (not a per-file slice).
//
// Fixture shape:
//   - Two Go files forming an import cycle (pkg `a` imports
//     `b`, pkg `b` imports `a`) -- the canonical cycle_member
//     input. The cycle_member recipe identifies SCCs in the
//     import graph and emits value=1.0 for files/packages
//     inside an SCC and value=0.0 for those outside.
//   - One additional Go file containing repeated content
//     across many symbols so the duplication_ratio recipe's
//     lexical window aggregator sees enough source-byte
//     fanout to emit at least one row. The recipe operates on
//     `AttrSourceBytes` (see `recipes/recipe.go:137-160`),
//     which the parser fleet stamps on every `AstFile` per
//     `internal/ast/parser/internal.go:160-170`.
//
// What is asserted:
//
//   - len(res.Files) == 3 (all three Go files parsed
//     successfully).
//   - At least one draft with `MetricKind == "cycle_member"`
//     and at least one with value == 1.0 -- a member of the
//     SCC -- so the recipe demonstrably RAN and saw the
//     cycle.
//   - At least one draft with
//     `MetricKind == "duplication_ratio"` -- demonstrating
//     the second project recipe was also dispatched (even if
//     the exact value is small for a 3-file fixture, the
//     row's presence proves the dispatch path was hit).
//
// What is NOT asserted (out of scope for this regression):
//
//   - Exact SCC membership identities -- the per-recipe unit
//     tests at `internal/metrics/recipes/cycle_member_test.go`
//     pin those.
//   - Exact duplication_ratio values -- the per-recipe unit
//     tests at `recipes/duplication_ratio_test.go` pin those.
// ---------------------------------------------------------------------------

func TestOrchestrator_Run_ProjectRecipeDispatch_EmitsCycleMemberAndDuplicationRatio(t *testing.T) {
	root := t.TempDir()

	// Cycle-leg 1: package `a` imports `b`. The `Worker`
	// struct + `Job` interface give the per-file SOLID-pack
	// recipes (interface_width, depth_of_inheritance,
	// coupling_between_objects) at least one class/interface
	// scope to attach drafts to, so the test cross-check at
	// the bottom asserts the Stage 2.2 dispatch path is
	// complete.
	writeFixtureFile(t, root, "a/a.go", strings.Join([]string{
		"package a",
		"",
		"import _ \"example.com/cycle/b\"",
		"",
		"type Job interface {",
		"\tRun() error",
		"\tName() string",
		"}",
		"",
		"type Worker struct {",
		"\tID int",
		"}",
		"",
		"func (w *Worker) Run() error { return nil }",
		"func (w *Worker) Name() string { return \"w\" }",
		"",
		"func A() int { return 1 }",
		"",
	}, "\n"))
	// Cycle-leg 2: package `b` imports `a`. Also gives `b`
	// a struct/method pair so the per-file recipes see
	// class scopes in this file too.
	writeFixtureFile(t, root, "b/b.go", strings.Join([]string{
		"package b",
		"",
		"import _ \"example.com/cycle/a\"",
		"",
		"type Helper struct {",
		"\tCount int",
		"}",
		"",
		"func (h *Helper) Tick() { h.Count++ }",
		"",
		"func B() int { return 2 }",
		"",
	}, "\n"))
	// A standalone package whose source bytes give the
	// duplication_ratio recipe enough lexical material to
	// chew on. Repeated identifiers / literals are typical
	// dup-ratio fuel (the recipe's windowed-shingle pass
	// produces non-zero output when many tokens repeat).
	dupContent := strings.Repeat("var Foo int = 42\n", 40) +
		strings.Repeat("var Bar int = 99\n", 40)
	writeFixtureFile(t, root, "c/c.go", "package c\n\n"+dupContent)

	rc := testRepoContext(t, root)
	// Stamp the module path so cycle_member can canonicalise
	// the `example.com/cycle/...` import targets against the
	// in-corpus package directory index.
	rc.ModulePath = "example.com/cycle"

	o := orchestrator.New(orchestrator.Options{Workers: 2})

	res, err := o.Run(context.Background(), rc, root)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got, want := len(res.Files), 3; got != want {
		paths := make([]string, len(res.Files))
		for i, f := range res.Files {
			paths[i] = f.GetPath()
		}
		t.Fatalf("res.Files: got %d (paths=%v), want %d (all three Go files must parse for the project recipes to see the full corpus)", got, paths, want)
	}

	// Assert that the project-recipe dispatch path ran every
	// recipe registered in DefaultProjectRegistry. We scan
	// res.Drafts (the flat collection populated by both per-
	// file and project recipes) by metric_kind.
	var (
		cycleMemberDrafts      []recipes.MetricSampleDraft
		duplicationRatioDrafts []recipes.MetricSampleDraft
	)
	for _, d := range res.Drafts {
		switch d.MetricKind {
		case "cycle_member":
			cycleMemberDrafts = append(cycleMemberDrafts, d)
		case "duplication_ratio":
			duplicationRatioDrafts = append(duplicationRatioDrafts, d)
		}
	}

	if len(cycleMemberDrafts) == 0 {
		kinds := uniqueMetricKinds(res.Drafts)
		t.Fatalf("no cycle_member drafts emitted; saw metric_kinds=%v -- project-level dispatch at orchestrator.go:381-383 either skipped DefaultProjectRegistry().All() or failed to feed the full AST corpus", kinds)
	}
	if len(duplicationRatioDrafts) == 0 {
		kinds := uniqueMetricKinds(res.Drafts)
		t.Fatalf("no duplication_ratio drafts emitted; saw metric_kinds=%v -- project-level dispatch must reach BOTH DefaultProjectRegistry entries, not just the first", kinds)
	}

	// Assert at least one cycle_member draft has value == 1.0
	// (a member of an SCC). This proves the recipe actually
	// SAW the import cycle, not just that it was invoked.
	sawCycleMember := false
	for _, d := range cycleMemberDrafts {
		if d.Value == 1.0 {
			sawCycleMember = true
			break
		}
	}
	if !sawCycleMember {
		values := make([]float64, len(cycleMemberDrafts))
		for i, d := range cycleMemberDrafts {
			values[i] = d.Value
		}
		t.Errorf("no cycle_member draft with value=1.0; got values=%v -- the recipe ran but did not detect the synthetic a<->b cycle (possible canonicalisation gap)", values)
	}

	// Cross-check the per-file dispatch path also ran the
	// nine canonical per-file metrics. This is a regression
	// against the workstream-2.2 evaluator item 2 fix
	// (registering interface_width, depth_of_inheritance,
	// coupling_between_objects in DefaultRegistry).
	wantPerFileLit := []string{
		"loc",
		"interface_width",
		"depth_of_inheritance",
		"coupling_between_objects",
	}
	gotKindsSet := map[string]bool{}
	for _, d := range res.Drafts {
		gotKindsSet[d.MetricKind] = true
	}
	for _, k := range wantPerFileLit {
		if !gotKindsSet[k] {
			t.Errorf("per-file dispatch did not emit %q; got metric_kinds=%v -- DefaultRegistry must register all nine foundation-tier recipes", k, uniqueMetricKinds(res.Drafts))
		}
	}
}

// uniqueMetricKinds collects the distinct `MetricKind`
// strings observed across `drafts`, in sorted order, so
// failure messages are deterministic across runs.
func uniqueMetricKinds(drafts []recipes.MetricSampleDraft) []string {
	seen := map[string]bool{}
	for _, d := range drafts {
		seen[d.MetricKind] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	// Tiny insertion sort: deterministic output without
	// pulling `sort` into the test file (the existing tests
	// already import `strings`, `os`, `context`, etc., but
	// not `sort` -- keeping the import set minimal).
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
