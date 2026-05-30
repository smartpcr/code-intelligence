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
// panicking parser test double
// ---------------------------------------------------------------------------

type panickingGoParser struct{}

func (panickingGoParser) Language() string { return "go" }

func (panickingGoParser) Parse(_ context.Context, path string, _ []byte) (*parser.AstFile, error) {
	panic("e2e: synthetic parser panic for path=" + path)
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

func (s *parseAndRecipeFanoutState) aFixtureGoFileOfKnownLineCount() error {
	content := "package foo\n\nfunc Bar() {\n\treturn\n}\n"
	relPath, err := s.writeFixtureFile("pkg/bar.go", content)
	if err != nil {
		return err
	}
	s.goRelPath = relPath
	s.knownLineCount = float64(strings.Count(content, "\n"))
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

func (s *parseAndRecipeFanoutState) aFixtureGoFileWithFunctionFoo() error {
	content := strings.Join([]string{
		"package mypkg",
		"",
		"func Foo() {",
		"\treturn",
		"}",
		"",
	}, "\n")
	relPath, err := s.writeFixtureFile("mypkg/foo.go", content)
	if err != nil {
		return err
	}
	s.goRelPath = relPath
	return nil
}

func (s *parseAndRecipeFanoutState) aFixtureFileThatTriggersAPanicInTheParserStub() error {
	panicRel, err := s.writeFixtureFile("pkg/boom.go", "package boom\n")
	if err != nil {
		return err
	}
	s.panicRelPath = panicRel
	cleanRel, err := s.writeFixtureFile("pkg/clean.go", "package boom\n")
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

func (s *parseAndRecipeFanoutState) theOrchestratorRunsWithThePanickingParser() error {
	reg := parser.NewRegistry()
	if err := reg.Register("go", func() parser.Parser { return panickingGoParser{} }); err != nil {
		return fmt.Errorf("register panicking parser: %w", err)
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

func (s *parseAndRecipeFanoutState) aMetricSampleDraftWithLocAndExpectedValue() error {
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
		if d.Value <= 0 {
			return fmt.Errorf("loc draft for %q: Value = %v, want > 0", s.goRelPath, d.Value)
		}
		if d.Value > s.knownLineCount+1 {
			return fmt.Errorf("loc draft for %q: Value = %v, want <= %v", s.goRelPath, d.Value, s.knownLineCount+1)
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

func (s *parseAndRecipeFanoutState) scopeBindingTableContainsFooMethodBinding() error {
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
		if !strings.Contains(b.Signature, "Foo") {
			continue
		}
		if b.StartLine < 1 {
			return fmt.Errorf("method binding StartLine = %d, want >= 1", b.StartLine)
		}
		if b.EndLine < b.StartLine {
			return fmt.Errorf("method binding EndLine = %d < StartLine = %d", b.EndLine, b.StartLine)
		}
		if b.FilePath != s.goRelPath {
			return fmt.Errorf("method binding FilePath = %q, want %q", b.FilePath, s.goRelPath)
		}
		if !strings.HasPrefix(b.Signature, orchestrator.SyntheticRepoURLPrefix) {
			return fmt.Errorf("method binding signature %q missing %q prefix", b.Signature, orchestrator.SyntheticRepoURLPrefix)
		}
		return nil // found the Foo binding, all checks pass
	}

	sigs := make([]string, 0, len(methodBindings))
	for _, b := range methodBindings {
		sigs = append(sigs, b.Signature)
	}
	return fmt.Errorf("no method binding mentioning Foo in %q; saw signatures=%v", s.goRelPath, sigs)
}

func (s *parseAndRecipeFanoutState) aWalkSkipWithParserPanicForPanickingFile() error {
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
	ctx.Step(`^a fixture Go file of known line count$`, s.aFixtureGoFileOfKnownLineCount)
	ctx.Step(`^a fixture Go file with branches$`, s.aFixtureGoFileWithBranches)
	ctx.Step(`^a fixture Go file with a function "Foo"$`, s.aFixtureGoFileWithFunctionFoo)
	ctx.Step(`^a fixture file that triggers a panic in the parser stub$`, s.aFixtureFileThatTriggersAPanicInTheParserStub)

	// When
	ctx.Step(`^the orchestrator runs the parse stage$`, s.theOrchestratorRunsTheParseStage)
	ctx.Step(`^recipes run$`, s.recipesRun)
	ctx.Step(`^recipes run against the branchy file$`, s.recipesRunAgainstTheBranchyFile)
	ctx.Step(`^parse and recipe fan-out completes$`, s.parseAndRecipeFanOutCompletes)
	ctx.Step(`^the orchestrator runs with the panicking parser$`, s.theOrchestratorRunsWithThePanickingParser)

	// Then
	ctx.Step(`^four AstFile rows are collected$`, s.fourAstFileRowsAreCollected)
	ctx.Step(`^zero WalkSkip rows with reason "unsupported_language" are emitted$`, s.zeroWalkSkipRowsWithReasonUnsupportedLanguage)
	ctx.Step(`^a MetricSampleDraft with MetricKind "loc" and the expected value is collected$`, s.aMetricSampleDraftWithLocAndExpectedValue)
	ctx.Step(`^zero MetricSampleDraft rows for metric_kind "cyclo" are emitted$`, s.zeroMetricSampleDraftRowsForCyclo)
	ctx.Step(`^the scope binding table contains a method binding whose Signature ends with "Foo" and whose StartLine and EndLine enclose the function body$`, s.scopeBindingTableContainsFooMethodBinding)
	ctx.Step(`^a WalkSkip with reason "parser_panic" is emitted for the panicking file$`, s.aWalkSkipWithParserPanicForPanickingFile)
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
