//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type mtimeTreeSHAState struct {
	root string

	// stable-on-noop
	firstHash  string
	secondHash string

	// mtime-bump-changes-hash
	preUpdateHash  string
	postUpdateHash string
	targetFile     string

	// exclude-applied
	excludedHash    string
	afterRemoveHash string

	// missing-root-errors
	missingPath string
	errResult   error
	hashResult  string
}

func (s *mtimeTreeSHAState) writeFile(rel, content string) {
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		panic(fmt.Sprintf("mkdir %s: %v", filepath.Dir(abs), err))
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		panic(fmt.Sprintf("write %s: %v", abs, err))
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *mtimeTreeSHAState) aDirectoryTreeWithThreeFiles() error {
	s.root = os.TempDir() + "/mtimetreesha-stable-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	s.writeFile("a.txt", "alpha")
	s.writeFile("sub/b.txt", "bravo")
	s.writeFile("sub/c/d.txt", "delta")
	return nil
}

func (s *mtimeTreeSHAState) aDirectoryTreeWithTwoFiles() error {
	s.root = os.TempDir() + "/mtimetreesha-mtime-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	s.writeFile("a.txt", "alpha")
	s.writeFile("b.txt", "bravo")
	s.targetFile = filepath.Join(s.root, "a.txt")
	return nil
}

func (s *mtimeTreeSHAState) aDirectoryTreeWithSrcAndGit() error {
	s.root = os.TempDir() + "/mtimetreesha-exclude-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return err
	}
	s.writeFile("src/main.go", "package main")
	s.writeFile(".git/HEAD", "ref: refs/heads/main")
	s.writeFile(".git/objects/aa/bb", "blob")
	return nil
}

func (s *mtimeTreeSHAState) aPathThatDoesNotExist() error {
	s.root = os.TempDir() + "/mtimetreesha-missing-" + fmt.Sprintf("%d", time.Now().UnixNano())
	s.missingPath = filepath.Join(s.root, "does", "not", "exist")
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *mtimeTreeSHAState) mtimeTreeSHACalledTwiceNoChanges() error {
	var err error
	s.firstHash, err = fingerprint.MTimeTreeSHA(s.root, nil)
	if err != nil {
		return fmt.Errorf("first call: %w", err)
	}
	s.secondHash, err = fingerprint.MTimeTreeSHA(s.root, nil)
	if err != nil {
		return fmt.Errorf("second call: %w", err)
	}
	return nil
}

func (s *mtimeTreeSHAState) oneFileMtimeUpdated() error {
	var err error
	s.preUpdateHash, err = fingerprint.MTimeTreeSHA(s.root, nil)
	if err != nil {
		return fmt.Errorf("pre-update hash: %w", err)
	}
	newTime := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := os.Chtimes(s.targetFile, newTime, newTime); err != nil {
		return fmt.Errorf("chtimes: %w", err)
	}
	return nil
}

func (s *mtimeTreeSHAState) mtimeTreeSHARecomputed() error {
	var err error
	s.postUpdateHash, err = fingerprint.MTimeTreeSHA(s.root, nil)
	if err != nil {
		return fmt.Errorf("post-update hash: %w", err)
	}
	return nil
}

func (s *mtimeTreeSHAState) mtimeTreeSHARunsWithExcludesGit() error {
	var err error
	s.excludedHash, err = fingerprint.MTimeTreeSHA(s.root, []string{".git"})
	if err != nil {
		return fmt.Errorf("with excludes: %w", err)
	}
	return nil
}

func (s *mtimeTreeSHAState) mtimeTreeSHARuns() error {
	s.hashResult, s.errResult = fingerprint.MTimeTreeSHA(s.missingPath, nil)
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *mtimeTreeSHAState) bothCallsReturnIdentical32CharHex() error {
	if len(s.firstHash) != 32 {
		return fmt.Errorf("expected 32-char hex, got %d chars: %q", len(s.firstHash), s.firstHash)
	}
	if s.firstHash != s.secondHash {
		return fmt.Errorf("expected identical hashes, got %q vs %q", s.firstHash, s.secondHash)
	}
	return nil
}

func (s *mtimeTreeSHAState) returnedStringDiffersFromPreUpdate() error {
	if s.preUpdateHash == s.postUpdateHash {
		return fmt.Errorf("expected hash to change after mtime bump, both = %q", s.preUpdateHash)
	}
	return nil
}

func (s *mtimeTreeSHAState) gitFilesContributeZeroBytes() error {
	// Verify by hashing without excludes — the result must differ,
	// proving .git files contributed bytes when not excluded.
	withGit, err := fingerprint.MTimeTreeSHA(s.root, nil)
	if err != nil {
		return fmt.Errorf("hash without excludes: %w", err)
	}
	if withGit == s.excludedHash {
		return fmt.Errorf("excluded vs unfiltered digests should differ; both = %q", s.excludedHash)
	}
	return nil
}

func (s *mtimeTreeSHAState) removingGitAndRehashingProducesIdenticalOutput() error {
	gitDir := filepath.Join(s.root, ".git")
	if err := os.RemoveAll(gitDir); err != nil {
		return fmt.Errorf("rm .git: %w", err)
	}
	var err error
	s.afterRemoveHash, err = fingerprint.MTimeTreeSHA(s.root, nil)
	if err != nil {
		return fmt.Errorf("after rm: %w", err)
	}
	if s.afterRemoveHash != s.excludedHash {
		return fmt.Errorf(
			"expected exclude-set to behave like removal: excluded=%q after-rm=%q",
			s.excludedHash, s.afterRemoveHash,
		)
	}
	return nil
}

func (s *mtimeTreeSHAState) nonNilErrorAndEmptyHash() error {
	if s.errResult == nil {
		return fmt.Errorf("expected non-nil error for missing root, got nil (hash=%q)", s.hashResult)
	}
	if s.hashResult != "" {
		return fmt.Errorf("expected empty hash for missing root, got %q", s.hashResult)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Cleanup helper
// ---------------------------------------------------------------------------

func (s *mtimeTreeSHAState) cleanup() {
	if s.root != "" {
		os.RemoveAll(s.root)
	}
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_local_materializer_and_sha_synthesis_mtimetreesha_helper(ctx *godog.ScenarioContext) {
	s := &mtimeTreeSHAState{}

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx, nil
	})

	// Given
	ctx.Given(`^a directory tree with files "a\.txt", "sub/b\.txt", and "sub/c/d\.txt"$`, s.aDirectoryTreeWithThreeFiles)
	ctx.Given(`^a directory tree with files "a\.txt" and "b\.txt"$`, s.aDirectoryTreeWithTwoFiles)
	ctx.Given(`^a directory tree with "src/main\.go" and a "\.git/" directory containing files$`, s.aDirectoryTreeWithSrcAndGit)
	ctx.Given(`^a path that does not exist$`, s.aPathThatDoesNotExist)

	// When
	ctx.When(`^MTimeTreeSHA is called twice with no file changes between calls$`, s.mtimeTreeSHACalledTwiceNoChanges)
	ctx.When(`^one file's mtime is updated via os\.Chtimes$`, s.oneFileMtimeUpdated)
	ctx.When(`^MTimeTreeSHA is recomputed$`, s.mtimeTreeSHARecomputed)
	ctx.When(`^MTimeTreeSHA runs with excludes \["\.git"\]$`, s.mtimeTreeSHARunsWithExcludesGit)
	ctx.When(`^MTimeTreeSHA runs$`, s.mtimeTreeSHARuns)

	// Then
	ctx.Then(`^both calls return the identical 32-char hex string$`, s.bothCallsReturnIdentical32CharHex)
	ctx.Then(`^the returned string differs from the pre-update value$`, s.returnedStringDiffersFromPreUpdate)
	ctx.Then(`^files under "\.git/" contribute zero bytes to the hash$`, s.gitFilesContributeZeroBytes)
	ctx.Then(`^removing "\.git/" and re-hashing without excludes produces the identical output$`, s.removingGitAndRehashingProducesIdenticalOutput)
	ctx.Then(`^a non-nil error is returned and the empty string is returned for the hash$`, s.nonNilErrorAndEmptyHash)
}

func TestE2E_local_materializer_and_sha_synthesis_mtimetreesha_helper(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"local_materializer_and_sha_synthesis_mtimetreesha_helper.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_local_materializer_and_sha_synthesis_mtimetreesha_helper,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{featurePath},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
