//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="pipeline_repo_walker_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/walk"
)

// repoWalkerState holds per-scenario state for repo walker e2e scenarios.
type repoWalkerState struct {
	// fixtureRoot is a temp directory acting as the repo root.
	fixtureRoot string

	// walkedFiles collected from the walker's file channel.
	walkedFiles []walk.WalkedFile
	// walkSkips collected from the walker's skip channel.
	walkSkips []walk.WalkSkip
	// walkErrors collected from the walker's error channel.
	walkErrors []error

	// walkedFilesRun2 captures a second traversal for deterministic
	// ordering verification.
	walkedFilesRun2 []walk.WalkedFile

	// readCalled tracks whether the ReadFileFn was invoked for
	// oversize files (size cap scenario).
	readCalled map[string]bool

	// walker allows per-scenario customisation (e.g. overriding
	// ReadFileFn for the size-cap scenario).
	walker *walk.DefaultWalker

	// missingRoot is a path that intentionally does not exist.
	missingRoot string
}

func newRepoWalkerState() *repoWalkerState {
	return &repoWalkerState{
		readCalled: make(map[string]bool),
		walker:     walk.NewDefaultWalker(),
	}
}

// drainWalker concurrently drains all three walker channels per the
// channel contract and populates the state fields.
func (s *repoWalkerState) drainWalker(root string) error {
	files, skips, errs := s.walker.Walk(context.Background(), root)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for f := range files {
			s.walkedFiles = append(s.walkedFiles, f)
		}
	}()
	go func() {
		defer wg.Done()
		for sk := range skips {
			s.walkSkips = append(s.walkSkips, sk)
		}
	}()
	go func() {
		defer wg.Done()
		for e := range errs {
			s.walkErrors = append(s.walkErrors, e)
		}
	}()
	wg.Wait()
	return nil
}

// drainWalkerInto is like drainWalker but populates a separate slice.
func (s *repoWalkerState) drainWalkerInto(root string) ([]walk.WalkedFile, error) {
	files, skips, errs := s.walker.Walk(context.Background(), root)
	var result []walk.WalkedFile
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for f := range files {
			result = append(result, f)
		}
	}()
	go func() {
		defer wg.Done()
		for range skips {
		}
	}()
	go func() {
		defer wg.Done()
		for range errs {
		}
	}()
	wg.Wait()
	return result, nil
}

// --- helpers ---

// createFixtureRoot creates a fresh temp directory as the fixture repo root.
func (s *repoWalkerState) createFixtureRoot() error {
	if s.fixtureRoot != "" {
		return nil
	}
	dir, err := os.MkdirTemp("", "walker-e2e-*")
	if err != nil {
		return fmt.Errorf("create fixture root: %w", err)
	}
	s.fixtureRoot = dir
	return nil
}

// writeFixtureFile creates a file relative to fixtureRoot with the given content.
func (s *repoWalkerState) writeFixtureFile(relPath, content string) error {
	if err := s.createFixtureRoot(); err != nil {
		return err
	}
	abs := filepath.Join(s.fixtureRoot, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return fmt.Errorf("mkdir %q: %w", filepath.Dir(abs), err)
	}
	return os.WriteFile(abs, []byte(content), 0o644)
}

// cleanup removes the temp fixture directory.
func (s *repoWalkerState) cleanup() {
	if s.fixtureRoot != "" {
		_ = os.RemoveAll(s.fixtureRoot)
	}
}

// --- Given steps ---

func (s *repoWalkerState) aFixtureRepoContaining(path string) error {
	return s.writeFixtureFile(path, "// placeholder\n")
}

func (s *repoWalkerState) aFixtureRepoWhoseGitignoreLists(pattern string) error {
	return s.writeFixtureFile(".gitignore", pattern+"\n")
}

func (s *repoWalkerState) theFixtureRepoContains(path string) error {
	return s.writeFixtureFile(path, "secret content\n")
}

func (s *repoWalkerState) aFixtureRepoWithAMiBGoFileNamed(sizeMiB int, name string) error {
	if err := s.createFixtureRoot(); err != nil {
		return err
	}
	abs := filepath.Join(s.fixtureRoot, name)
	content := make([]byte, int64(sizeMiB)*1024*1024)
	for i := range content {
		content[i] = '/'
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		return fmt.Errorf("write oversized file: %w", err)
	}

	// Override ReadFileFn to track read calls and panic on the oversize file.
	s.walker.ReadFileFn = func(absPath string) ([]byte, error) {
		rel, _ := filepath.Rel(s.fixtureRoot, absPath)
		rel = filepath.ToSlash(rel)
		s.readCalled[rel] = true
		if rel == name {
			return nil, fmt.Errorf("ReadFileFn called for oversize file %q — should never happen", name)
		}
		return os.ReadFile(absPath)
	}

	// Override StatFn to return the actual file size.
	s.walker.StatFn = func(absPath string, d fs.DirEntry) (int64, error) {
		info, err := d.Info()
		if err != nil {
			return 0, err
		}
		return info.Size(), nil
	}

	return nil
}

func (s *repoWalkerState) aPathThatDoesNotExist() error {
	dir, err := os.MkdirTemp("", "walker-e2e-missing-*")
	if err != nil {
		return err
	}
	s.missingRoot = filepath.Join(dir, "no-such-dir")
	// Remove the parent to be safe; the child never existed.
	_ = os.RemoveAll(dir)
	return nil
}

func (s *repoWalkerState) aFixtureRepoWithFiles(fileList string) error {
	names := strings.Split(fileList, ";")
	for _, name := range names {
		name = strings.TrimSpace(name)
		if err := s.writeFixtureFile(name, fmt.Sprintf("package main // %s\n", name)); err != nil {
			return err
		}
	}
	return nil
}

// --- When steps ---

func (s *repoWalkerState) theWalkerTraverses() error {
	return s.drainWalker(s.fixtureRoot)
}

func (s *repoWalkerState) walkRuns() error {
	return s.drainWalker(s.missingRoot)
}

func (s *repoWalkerState) theWalkerTraversesTwice() error {
	if err := s.drainWalker(s.fixtureRoot); err != nil {
		return err
	}
	run2, err := s.drainWalkerInto(s.fixtureRoot)
	if err != nil {
		return err
	}
	s.walkedFilesRun2 = run2
	return nil
}

// --- Then steps ---

func (s *repoWalkerState) doesNotAppearInWalkedFile(path string) error {
	for _, f := range s.walkedFiles {
		if f.RepoRelPath == path {
			return fmt.Errorf("expected %q to NOT appear in WalkedFile, but it did", path)
		}
	}
	return nil
}

func (s *repoWalkerState) aWalkSkipWithReasonIsEmittedFor(reason, path string) error {
	for _, sk := range s.walkSkips {
		if sk.Reason == reason && sk.Path == path {
			return nil
		}
	}
	var found []string
	for _, sk := range s.walkSkips {
		found = append(found, fmt.Sprintf("{Path:%q Reason:%q}", sk.Path, sk.Reason))
	}
	return fmt.Errorf("no WalkSkip{Path:%q, Reason:%q} found; skips: %s", path, reason, strings.Join(found, ", "))
}

func (s *repoWalkerState) producesAWalkSkipWithReason(path, reason string) error {
	return s.aWalkSkipWithReasonIsEmittedFor(reason, path)
}

func (s *repoWalkerState) zeroWalkedFileRowsExistFor(path string) error {
	return s.doesNotAppearInWalkedFile(path)
}

func (s *repoWalkerState) itEmitsAWalkSkipWithReasonFor(reason, path string) error {
	return s.aWalkSkipWithReasonIsEmittedFor(reason, path)
}

func (s *repoWalkerState) theFileBytesAreNeverRead() error {
	for path, called := range s.readCalled {
		if called && strings.HasSuffix(path, "huge.go") {
			return fmt.Errorf("ReadFileFn was called for oversized file %q", path)
		}
	}
	return nil
}

func (s *repoWalkerState) theErrorChannelYieldsErrRootNotFound() error {
	for _, e := range s.walkErrors {
		if errors.Is(e, walk.ErrRootNotFound) {
			return nil
		}
	}
	return fmt.Errorf("expected ErrRootNotFound on error channel; got %v", s.walkErrors)
}

func (s *repoWalkerState) theFileChannelClosesWithZeroRows() error {
	if len(s.walkedFiles) != 0 {
		return fmt.Errorf("expected zero WalkedFile rows, got %d", len(s.walkedFiles))
	}
	return nil
}

func (s *repoWalkerState) bothRunsEmitWalkedFileInIdenticalOrder(expectedCSV string) error {
	names := strings.Split(expectedCSV, ";")
	for i, n := range names {
		names[i] = strings.TrimSpace(n)
	}

	if len(s.walkedFiles) != len(names) {
		return fmt.Errorf("run 1: expected %d files, got %d (%v)", len(names), len(s.walkedFiles), pathsOf(s.walkedFiles))
	}
	if len(s.walkedFilesRun2) != len(names) {
		return fmt.Errorf("run 2: expected %d files, got %d (%v)", len(names), len(s.walkedFilesRun2), pathsOf(s.walkedFilesRun2))
	}

	for i, want := range names {
		got1 := s.walkedFiles[i].RepoRelPath
		got2 := s.walkedFilesRun2[i].RepoRelPath
		if got1 != want {
			return fmt.Errorf("run 1 index %d: expected %q, got %q", i, want, got1)
		}
		if got2 != want {
			return fmt.Errorf("run 2 index %d: expected %q, got %q", i, want, got2)
		}
	}
	return nil
}

// pathsOf returns RepoRelPath values for debug output.
func pathsOf(files []walk.WalkedFile) []string {
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.RepoRelPath
	}
	return out
}

// --- Scenario initializer ---

func InitializeScenario_pipeline_repo_walker(ctx *godog.ScenarioContext) {
	s := newRepoWalkerState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a fixture repo containing "([^"]*)"$`, s.aFixtureRepoContaining)
	ctx.Step(`^a fixture repo whose "\.gitignore" lists "([^"]*)"$`, s.aFixtureRepoWhoseGitignoreLists)
	ctx.Step(`^the fixture repo contains "([^"]*)"$`, s.theFixtureRepoContains)
	ctx.Step(`^a fixture repo with a (\d+) MiB "\.go" file named "([^"]*)"$`, s.aFixtureRepoWithAMiBGoFileNamed)
	ctx.Step(`^a path that does not exist$`, s.aPathThatDoesNotExist)
	ctx.Step(`^a fixture repo with files "([^"]*)"$`, s.aFixtureRepoWithFiles)

	// When
	ctx.Step(`^the walker traverses$`, s.theWalkerTraverses)
	ctx.Step(`^Walk runs$`, s.walkRuns)
	ctx.Step(`^the walker traverses twice$`, s.theWalkerTraversesTwice)

	// Then
	ctx.Step(`^"([^"]*)" does not appear in WalkedFile$`, s.doesNotAppearInWalkedFile)
	ctx.Step(`^a WalkSkip with reason "([^"]*)" is emitted for "([^"]*)"$`, s.aWalkSkipWithReasonIsEmittedFor)
	ctx.Step(`^"([^"]*)" produces a WalkSkip with reason "([^"]*)"$`, s.producesAWalkSkipWithReason)
	ctx.Step(`^zero WalkedFile rows exist for "([^"]*)"$`, s.zeroWalkedFileRowsExistFor)
	ctx.Step(`^it emits a WalkSkip with reason "([^"]*)" for "([^"]*)"$`, s.itEmitsAWalkSkipWithReasonFor)
	ctx.Step(`^the file bytes are never read$`, s.theFileBytesAreNeverRead)
	ctx.Step(`^the error channel yields ErrRootNotFound$`, s.theErrorChannelYieldsErrRootNotFound)
	ctx.Step(`^the file channel closes with zero rows$`, s.theFileChannelClosesWithZeroRows)
	ctx.Step(`^both runs emit WalkedFile in identical order "([^"]*)"$`, s.bothRunsEmitWalkedFileInIdenticalOrder)
}

func TestE2E_pipeline_repo_walker(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_pipeline_repo_walker,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"pipeline_repo_walker.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
