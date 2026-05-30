//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="foundations_repo_context_and_scope_binding_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"
	"github.com/microsoft/cleancode-service/internal/cli/repocontext"
	"github.com/microsoft/cleancode-service/internal/cli/scopebinding"
)

// requireEnv skips the test when the named environment variable is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

// repoContextState holds per-scenario state for repo-context
// and scope-binding scenarios.
type repoContextState struct {
	rootPath string
	tmpDir   string

	// MintRepoID results
	repoID1 uuid.UUID
	repoID2 uuid.UUID

	// DetectHeadSHA results
	headSHA   string
	isGitRepo bool

	// DetectModulePath results
	modulePath string

	// ScopeBinding round-trip
	store          *scopebinding.Store
	insertedBinding scopebinding.ScopeBinding
	retrievedBinding scopebinding.ScopeBinding
	retrieveErr     error
}

func newRepoContextState() *repoContextState {
	return &repoContextState{
		store: scopebinding.NewStore(),
	}
}

// --- Given steps ---

func (s *repoContextState) anAbsoluteRootPath(path string) {
	s.rootPath = path
}

func (s *repoContextState) aDirectoryThatIsNotAGitRepo() error {
	dir, err := os.MkdirTemp("", "not-a-git-repo-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	s.tmpDir = dir
	s.rootPath = dir
	return nil
}

func (s *repoContextState) aGoModFileContaining(moduleDecl string) error {
	dir, err := os.MkdirTemp("", "gomod-test-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	s.tmpDir = dir
	s.rootPath = dir

	goModContent := moduleDecl + "\n\ngo 1.22.0\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(goModContent), 0644); err != nil {
		return fmt.Errorf("failed to write go.mod: %w", err)
	}
	return nil
}

func (s *repoContextState) aScopeBindingInsertedWithScopeID(scopeID string) {
	s.insertedBinding = scopebinding.ScopeBinding{
		ScopeID:   scopeID,
		ScopeKind: "class",
		FilePath:  "src/main/java/com/example/Foo.java",
		StartLine: 10,
		EndLine:   50,
		Signature: "com.example.Foo",
	}
	s.store.Put(s.insertedBinding)
}

// --- When steps ---

func (s *repoContextState) mintRepoIDIsCalledTwice() {
	s.repoID1 = repocontext.MintRepoID(s.rootPath)
	s.repoID2 = repocontext.MintRepoID(s.rootPath)
}

func (s *repoContextState) detectHeadSHARuns() {
	sha, isGit := repocontext.DetectHeadSHA(s.rootPath)
	s.headSHA = sha
	s.isGitRepo = isGit
}

func (s *repoContextState) detectModulePathRuns() {
	s.modulePath = repocontext.DetectModulePath(s.rootPath, "go")
}

func (s *repoContextState) getScopeBindingRuns(scopeID string) {
	s.retrievedBinding, s.retrieveErr = s.store.Get(scopeID)
}

// --- Then steps ---

func (s *repoContextState) bothCallsReturnTheSameUUIDv5() error {
	if s.repoID1 == uuid.Nil {
		return fmt.Errorf("first MintRepoID returned nil UUID")
	}
	if s.repoID1 != s.repoID2 {
		return fmt.Errorf("MintRepoID not deterministic: %q != %q", s.repoID1, s.repoID2)
	}
	// UUID-v5 version check: byte 6 upper nibble must be 0x50 (version 5).
	if s.repoID1[6]>>4 != 5 {
		return fmt.Errorf("expected UUID version 5, got version %d (byte 6 = 0x%02x)", s.repoID1[6]>>4, s.repoID1[6])
	}
	// RFC 4122 variant check: byte 8 upper two bits must be 10 (0x80..0xBF).
	if s.repoID1[8]&0xC0 != 0x80 {
		return fmt.Errorf("expected RFC 4122 variant (0x80..0xBF), got byte 8 = 0x%02x", s.repoID1[8])
	}
	return nil
}

func (s *repoContextState) itReturnsWorkingCopyAndNotGitRepo() error {
	if s.headSHA != repocontext.HeadSHAWorkingCopySentinel {
		return fmt.Errorf("expected HeadSHA=%q, got %q", repocontext.HeadSHAWorkingCopySentinel, s.headSHA)
	}
	if s.isGitRepo {
		return fmt.Errorf("expected IsGitRepo=false, got true")
	}
	return nil
}

func (s *repoContextState) itReturnsModulePath(expected string) error {
	if s.modulePath != expected {
		return fmt.Errorf("expected module path %q, got %q", expected, s.modulePath)
	}
	return nil
}

func (s *repoContextState) itReturnsTheSameStructIntact() error {
	if s.retrieveErr != nil {
		return fmt.Errorf("Get returned error: %w", s.retrieveErr)
	}
	b := s.retrievedBinding
	e := s.insertedBinding
	if b.FilePath != e.FilePath {
		return fmt.Errorf("FilePath mismatch: %q != %q", b.FilePath, e.FilePath)
	}
	if b.StartLine != e.StartLine {
		return fmt.Errorf("StartLine mismatch: %d != %d", b.StartLine, e.StartLine)
	}
	if b.EndLine != e.EndLine {
		return fmt.Errorf("EndLine mismatch: %d != %d", b.EndLine, e.EndLine)
	}
	if b.Signature != e.Signature {
		return fmt.Errorf("Signature mismatch: %q != %q", b.Signature, e.Signature)
	}
	return nil
}

// cleanup removes any temp dirs created during the scenario.
func (s *repoContextState) cleanup() {
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir)
	}
}

func InitializeScenario_foundations_repo_context_and_scope_binding(ctx *godog.ScenarioContext) {
	s := newRepoContextState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^an absolute root path "([^"]*)"$`, s.anAbsoluteRootPath)
	ctx.Step(`^a directory that is not a git repo$`, s.aDirectoryThatIsNotAGitRepo)
	ctx.Step(`^a go\.mod file containing "([^"]*)"$`, s.aGoModFileContaining)
	ctx.Step(`^a ScopeBinding inserted with ScopeID = "([^"]*)"$`, s.aScopeBindingInsertedWithScopeID)

	// When
	ctx.Step(`^MintRepoID is called twice$`, s.mintRepoIDIsCalledTwice)
	ctx.Step(`^DetectHeadSHA runs$`, s.detectHeadSHARuns)
	ctx.Step(`^DetectModulePath\(root, "go"\) runs$`, s.detectModulePathRuns)
	ctx.Step(`^Get\("([^"]*)"\) runs$`, s.getScopeBindingRuns)

	// Then
	ctx.Step(`^both calls return the same UUID-v5 value byte-for-byte$`, s.bothCallsReturnTheSameUUIDv5)
	ctx.Step(`^it returns the literal string "working-copy" and IsGitRepo == false$`, s.itReturnsWorkingCopyAndNotGitRepo)
	ctx.Step(`^it returns "([^"]*)"$`, s.itReturnsModulePath)
	ctx.Step(`^it returns the same struct with FilePath, StartLine, EndLine, and Signature intact$`, s.itReturnsTheSameStructIntact)
}

func TestE2E_foundations_repo_context_and_scope_binding(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_foundations_repo_context_and_scope_binding,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"foundations_repo_context_and_scope_binding.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}