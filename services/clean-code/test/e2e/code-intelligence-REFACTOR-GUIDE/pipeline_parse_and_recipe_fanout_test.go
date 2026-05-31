//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="pipeline_parse_and_recipe_fanout_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/scopebinding"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/walk"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// ---------------------------------------------------------------------------
// Per-scenario state
// ---------------------------------------------------------------------------

type parseAndRecipeFanoutState struct {
	fixtureRoot string
	result      *orchestrator.Result
	resultErr   error
	table       *scopebinding.Table

	// Scenario-specific fields
	knownLineCount float64
	goRelPath      string // repo-relative forward-slash path of the Go fixture
	panicRelPath   string // repo-relative path of the file wired to the panicking parser
	cleanRelPath   string // repo-relative path of a second file (for survival check)
}

func newParseAndRecipeFanoutState() *parseAndRecipeFanoutState {
	return &parseAndRecipeFanoutState{
		table: scopebinding.NewTable(),
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (s *parseAndRecipeFanoutState) createFixtureRoot() error {
	if s.fixtureRoot != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "parse-fanout-e2e-*")
	if err != nil {
		return fmt.Errorf("create fixture root: %w", err)
	}
	s.fixtureRoot = dir
	return nil
}

func (s *parseAndRecipeFanoutState) writeFixtureFile(relPath, content string) (string, error) {
	if err := s.createFixtureRoot(); err != nil {
		return "", err
	}
	abs := filepath.Join(s.fixtureRoot, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", fmt.Errorf("mkdir %q: %w", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		return "", err
	}
	return filepath.ToSlash(relPath), nil
}

func (s *parseAndRecipeFanoutState) cleanup() {
	if s.fixtureRoot != "" {
		_ = os.RemoveAll(s.fixtureRoot)
	}
}

func (s *parseAndRecipeFanoutState) repoContext() repocontext.RepoContext {
	return repocontext.RepoContext{
		RootPath:   s.fixtureRoot,
		RepoID:     repocontext.MintRepoID(s.fixtureRoot),
		HeadSHA:    repocontext.HeadSHAWorkingCopySentinel,
		ModulePath: "",
		IsGitRepo:  false,
	}
}

func (s *parseAndRecipeFanoutState) runOrchestrator(opts orchestrator.Options) error {
	if opts.ScopeBindings == nil {
		opts.ScopeBindings = s.table
	}
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	o := orchestrator.New(opts)
	s.result, s.resultErr = o.Run(context.Background(), s.repoContext(), s.fixtureRoot)
	return nil
}

// ---------------------------------------------------------------------------
// panicking parser test double — selective: only panics for a specific file
// ---------------------------------------------------------------------------

type selectivePanickingParser struct {
	panicPath    string // repo-relative path that triggers the panic
	realRegistry *parser.Registry
}

func (p *selectivePanickingParser) Language() string { return "go" }

func (p *selectivePanickingParser) Parse(ctx context.Context, path string, content []byte) (*parser.AstFile, error) {
	if path == p.panicPath {
		panic("e2e: synthetic parser panic for path=" + path)
	}
	return p.realRegistry.Parse(ctx, path, content)
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *parseAndRecipeFanoutState) aFixtureRepoWithOneFileEachOfFourLanguages() error {
	if _, err := s.writeFixtureFile("src/main.go", "package main\n\nfunc main() {}\n"); err != nil {
		return err
	}
	if _, err := s.writeFixtureFile("src/main.py", "def main():\n    pass\n"); err != nil {
		return err
	}
	if _, err := s.writeFixtureFile("src/main.ts", "function main(): void {}\n"); err != nil {
		return err
	}
	if _, err := s.writeFixtureFile("src/Main.java", "package com.example;\n\npublic class Main {}\n"); err != nil {
		return err
	}
	return nil
}

func (s *parseAndRecipeFanoutState) aFixtureGoFileWithExactly6Lines() error {
	// 6 physical lines: the trailing newline creates line 6 (the parser's
	// file scope Range is 1-6, so loc = 6).
	// Line 1: package foo
	// Line 2: (blank)
	// Line 3: func Bar() {
	// Line 4:     return
	// Line 5: }
	// Line 6: (trailing newline creates this empty line)
	content := "package foo\n\nfunc Bar() {\n\treturn\n}\n"
	relPath, err := s.writeFixtureFile("pkg/bar.go", content)
	if err != nil {
		return err
	}
	s.goRelPath = relPath
	s.knownLineCount = 6
	return nil
}

func (s *parseAndRecipeFanoutState) aFixtureGoFileWithBranches() error {
	content := strings.Join([]string{
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
	}, "\n")
	relPath, err := s.writeFixtureFile("pkg/branchy.go", content)
	if err != nil {
		return err
	}
	s.goRelPath = relPath
	return nil
}

func (s *parseAndRecipeFanoutState) aFixtureGoFileWithFunctionFooSpanningLines3To5() error {
	// Line 1: package mypkg
	// Line 2: (blank)
	// Line 3: func Foo() {
	// Line 4:     return
	// Line 5: }
	content := "package mypkg\n\nfunc Foo() {\n\treturn\n}\n"
	relPath, err := s.writeFixtureFile("mypkg/foo.go", content)
	if err != nil {
		return err
	}
	s.goRelPath = relPath
	return nil
}

func (s *parseAndRecipeFanoutState) aFixtureWhereOnlyBoomGoTriggersAPanicAndCleanGoParsesNormally() error {
	panicRel, err := s.writeFixtureFile("pkg/boom.go", "package boom\n\nfunc Boom() {}\n")
	if err != nil {
		return err
	}
	s.panicRelPath = panicRel
	cleanRel, err := s.writeFixtureFile("pkg/clean.go", "package boom\n\nfunc Clean() {}\n")
	if err != nil {
		return err
	}
	s.cleanRelPath = cleanRel
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *parseAndRecipeFanoutState) theOrchestratorRunsTheParseStage() error {
	return s.runOrchestrator(orchestrator.Options{})
}

func (s *parseAndRecipeFanoutState) recipesRun() error {
	return s.runOrchestrator(orchestrator.Options{})
}

func (s *parseAndRecipeFanoutState) recipesRunAgainstTheBranchyFile() error {
	return s.runOrchestrator(orchestrator.Options{})
}

func (s *parseAndRecipeFanoutState) parseAndRecipeFanOutCompletes() error {
	return s.runOrchestrator(orchestrator.Options{})
}

func (s *parseAndRecipeFanoutState) theOrchestratorRunsWithTheSelectivePanickingParser() error {
	realReg := parser.DefaultRegistry()
	selectiveParser := &selectivePanickingParser{
		panicPath:    s.panicRelPath,
		realRegistry: realReg,
	}
	reg := parser.NewRegistry()
	if err := reg.Register("go", func() parser.Parser { return selectiveParser }); err != nil {
		return fmt.Errorf("register selective panicking parser: %w", err)
	}
	return s.runOrchestrator(orchestrator.Options{
		Workers: 1,
		Parsers: reg,
	})
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *parseAndRecipeFanoutState) fourAstFileRowsAreCollected() error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	if got := len(s.result.Files); got != 4 {
		paths := make([]string, len(s.result.Files))
		for i, f := range s.result.Files {
			paths[i] = f.GetPath()
		}
		return fmt.Errorf("expected 4 AstFile rows, got %d (paths=%v)", got, paths)
	}
	return nil
}

func (s *parseAndRecipeFanoutState) zeroWalkSkipRowsWithReasonUnsupportedLanguage() error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	for _, sk := range s.result.Skips {
		if sk.Reason == walk.SkipReasonUnsupportedLanguage {
			return fmt.Errorf("unexpected unsupported_language skip for %q", sk.Path)
		}
	}
	return nil
}

func (s *parseAndRecipeFanoutState) aMetricSampleDraftWithLocAndValueExactly6() error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	var locDrafts []recipes.MetricSampleDraft
	for _, d := range s.result.Drafts {
		if d.MetricKind == "loc" && d.Scope.Path == s.goRelPath {
			locDrafts = append(locDrafts, d)
		}
	}
	if len(locDrafts) == 0 {
		var allKinds []string
		for _, d := range s.result.Drafts {
			allKinds = append(allKinds, d.MetricKind)
		}
		return fmt.Errorf("no loc draft emitted for %q; saw metric_kinds=%v", s.goRelPath, allKinds)
	}
	for _, d := range locDrafts {
		if d.Value != float64(s.knownLineCount) {
			return fmt.Errorf("loc draft for %q: Value = %v, want exactly %v", s.goRelPath, d.Value, s.knownLineCount)
		}
	}
	return nil
}

func (s *parseAndRecipeFanoutState) recipeAppliesToReturnsFalseForCycloOnEveryParsedAstFile() error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	if len(s.result.Files) == 0 {
		return fmt.Errorf("no AstFiles parsed — cannot verify AppliesTo gate")
	}
	cycloRecipe := recipes.NewCycloRecipe()
	for _, ast := range s.result.Files {
		if cycloRecipe.AppliesTo(ast) {
			return fmt.Errorf("CycloRecipe.AppliesTo returned true for %q — Stage 2.1 parsers MUST NOT stamp decision_blocks", ast.GetPath())
		}
	}
	return nil
}

func (s *parseAndRecipeFanoutState) zeroMetricSampleDraftRowsForCyclo() error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	for _, d := range s.result.Drafts {
		if d.MetricKind == "cyclo" {
			return fmt.Errorf("unexpected cyclo draft for %q (Stage 2.1 parsers do not emit decision_blocks)", d.Scope.Path)
		}
	}
	return nil
}

func (s *parseAndRecipeFanoutState) scopeBindingTableContainsFooMethodBindingWithExactLines() error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	if s.table.Len() == 0 {
		return fmt.Errorf("scope binding table empty after run")
	}

	var methodBindings []scopebinding.ScopeBinding
	for key, id := range s.result.ScopeIDs {
		if key.Path != s.goRelPath {
			continue
		}
		b, ok := s.table.Get(id)
		if !ok {
			continue
		}
		if b.ScopeKind == "method" {
			methodBindings = append(methodBindings, b)
		}
	}
	if len(methodBindings) == 0 {
		return fmt.Errorf("no method-kind binding for %q in table", s.goRelPath)
	}

	for _, b := range methodBindings {
		// Signature must end with "Foo()" per BuildMethod format:
		// <repoURL>::method::<relPath>#<qualifiedName>(<params>)
		// The qualifiedName includes the package prefix (e.g. "mypkg.foo.go.Foo"),
		// so the full signature ends with ".Foo()" or "#Foo()".
		if !strings.HasSuffix(b.Signature, "Foo()") {
			continue
		}
		// Verify the full signature structure
		if !strings.Contains(b.Signature, "::method::"+s.goRelPath+"#") {
			return fmt.Errorf("method binding signature %q missing ::method::%s# segment", b.Signature, s.goRelPath)
		}
		if !strings.HasPrefix(b.Signature, orchestrator.SyntheticRepoURLPrefix) {
			return fmt.Errorf("method binding signature %q missing %q prefix", b.Signature, orchestrator.SyntheticRepoURLPrefix)
		}
		if b.FilePath != s.goRelPath {
			return fmt.Errorf("method binding FilePath = %q, want %q", b.FilePath, s.goRelPath)
		}
		// StartLine and EndLine must enclose the function body (lines 3-5)
		if b.StartLine != 3 {
			return fmt.Errorf("method binding StartLine = %d, want 3 (func Foo() is on line 3)", b.StartLine)
		}
		if b.EndLine != 5 {
			return fmt.Errorf("method binding EndLine = %d, want 5 (closing brace is on line 5)", b.EndLine)
		}
		return nil // found the Foo binding, all checks pass
	}

	sigs := make([]string, 0, len(methodBindings))
	for _, b := range methodBindings {
		sigs = append(sigs, fmt.Sprintf("%s [%d-%d]", b.Signature, b.StartLine, b.EndLine))
	}
	return fmt.Errorf("no method binding with signature ending in Foo() in %q; saw: %v", s.goRelPath, sigs)
}

func (s *parseAndRecipeFanoutState) aWalkSkipWithParserPanicForBoomGo() error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	for _, sk := range s.result.Skips {
		if sk.Reason == orchestrator.SkipReasonParserPanic && sk.Path == s.panicRelPath {
			return nil
		}
	}
	var found []string
	for _, sk := range s.result.Skips {
		found = append(found, fmt.Sprintf("{Path:%q Reason:%q}", sk.Path, sk.Reason))
	}
	return fmt.Errorf("no WalkSkip{Path:%q, Reason:%q} found; skips: %s", s.panicRelPath, orchestrator.SkipReasonParserPanic, strings.Join(found, ", "))
}

func (s *parseAndRecipeFanoutState) cleanGoAppearsInTheParsedAstFileResults() error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator returned error: %w", s.resultErr)
	}
	for _, f := range s.result.Files {
		if f.GetPath() == s.cleanRelPath {
			return nil
		}
	}
	paths := make([]string, len(s.result.Files))
	for i, f := range s.result.Files {
		paths[i] = f.GetPath()
	}
	return fmt.Errorf("clean.go (%q) not found in parsed AstFile results (got %v) — the worker did NOT survive the boom.go panic", s.cleanRelPath, paths)
}

func (s *parseAndRecipeFanoutState) theOrchestratorExitsCleanly() error {
	if s.resultErr != nil {
		return fmt.Errorf("orchestrator MUST return cleanly on per-file parser panics, got error: %v", s.resultErr)
	}
	if s.result == nil {
		return fmt.Errorf("orchestrator returned nil result")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_pipeline_parse_and_recipe_fanout(ctx *godog.ScenarioContext) {
	s := newParseAndRecipeFanoutState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a fixture repo with one file each of Go, Python, TypeScript, and Java$`, s.aFixtureRepoWithOneFileEachOfFourLanguages)
	ctx.Step(`^a fixture Go file with exactly 6 lines$`, s.aFixtureGoFileWithExactly6Lines)
	ctx.Step(`^a fixture Go file with branches$`, s.aFixtureGoFileWithBranches)
	ctx.Step(`^a fixture Go file with a function "Foo" spanning lines 3 to 5$`, s.aFixtureGoFileWithFunctionFooSpanningLines3To5)
	ctx.Step(`^a fixture where only "boom\.go" triggers a panic in the parser stub and "clean\.go" parses normally$`, s.aFixtureWhereOnlyBoomGoTriggersAPanicAndCleanGoParsesNormally)

	// When
	ctx.Step(`^the orchestrator runs the parse stage$`, s.theOrchestratorRunsTheParseStage)
	ctx.Step(`^recipes run$`, s.recipesRun)
	ctx.Step(`^recipes run against the branchy file$`, s.recipesRunAgainstTheBranchyFile)
	ctx.Step(`^parse and recipe fan-out completes$`, s.parseAndRecipeFanOutCompletes)
	ctx.Step(`^the orchestrator runs with the selective panicking parser$`, s.theOrchestratorRunsWithTheSelectivePanickingParser)

	// Then
	ctx.Step(`^four AstFile rows are collected$`, s.fourAstFileRowsAreCollected)
	ctx.Step(`^zero WalkSkip rows with reason "unsupported_language" are emitted$`, s.zeroWalkSkipRowsWithReasonUnsupportedLanguage)
	ctx.Step(`^a MetricSampleDraft with MetricKind "loc" and Value exactly 6 is collected$`, s.aMetricSampleDraftWithLocAndValueExactly6)
	ctx.Step(`^Recipe\.AppliesTo returns false for the cyclo recipe on every parsed AstFile$`, s.recipeAppliesToReturnsFalseForCycloOnEveryParsedAstFile)
	ctx.Step(`^zero MetricSampleDraft rows for metric_kind "cyclo" are emitted$`, s.zeroMetricSampleDraftRowsForCyclo)
	ctx.Step(`^the scope binding table contains a method binding whose Signature ends with "Foo\(\)" and whose StartLine is 3 and EndLine is 5$`, s.scopeBindingTableContainsFooMethodBindingWithExactLines)
	ctx.Step(`^a WalkSkip with reason "parser_panic" is emitted for "boom\.go"$`, s.aWalkSkipWithParserPanicForBoomGo)
	ctx.Step(`^"clean\.go" appears in the parsed AstFile results$`, s.cleanGoAppearsInTheParsedAstFileResults)
	ctx.Step(`^the orchestrator exits cleanly$`, s.theOrchestratorExitsCleanly)
}

func TestE2E_pipeline_parse_and_recipe_fanout(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_pipeline_parse_and_recipe_fanout,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"pipeline_parse_and_recipe_fanout.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}

// requireEnv skips the test when the named environment variable is unset.
func requireEnv_pipeline_parse_and_recipe_fanout(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("env %q not set", name)
	}
	return v
}

// Ensure uuid import is used (the type appears in orchestrator.Result.ScopeIDs key comparisons).
var _ = uuid.Nil
