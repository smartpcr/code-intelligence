package metric_ingestor_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
)

func TestDirectoryAstFileSource_MissingRoot(t *testing.T) {
	t.Parallel()
	src := &metric_ingestor.DirectoryAstFileSource{}
	_, err := src.Files(context.Background(), metric_ingestor.ScanRunContext{
		ID: mustNewV7(t), RepoID: mustNewV4(t),
	})
	if !errors.Is(err, metric_ingestor.ErrDirectoryAstSourceMissingRoot) {
		t.Errorf("Files(empty root) err=%v, want errors.Is ErrDirectoryAstSourceMissingRoot", err)
	}
}

// TestDirectoryAstFileSource_MissingCommitRoot pins the
// iter-3 evaluator item 2 semantics: a missing per-commit
// root is a HARD failure, not a silent "zero files" success.
// The state machine transitions the commit to `failed`
// (one of the four canonical terminal states) rather than
// smuggling a bogus `scanned` terminal state past the gate.
//
// iter-4 evaluator item 2 (recovery): the Metric Ingestor is
// the SOLE writer of `commit.scan_status`, and operators MUST
// NOT manually mutate the column to re-queue a `failed`
// commit -- `failed->pending` is REJECTED by
// [repo_indexer.ValidateTransition]. The structural recovery
// surface is the [AstSourceAvailability] pre-flight probe
// (see [WithStateMachineSourceProbe]): when the source
// reports the SHA is not yet materialised, the state machine
// leaves the commit `pending` (no canonical transition occurs)
// and the next sweep tick retries.
func TestDirectoryAstFileSource_MissingCommitRoot(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	src := &metric_ingestor.DirectoryAstFileSource{Root: tmp}
	files, err := src.Files(context.Background(), metric_ingestor.ScanRunContext{
		ID:     mustNewV7(t),
		RepoID: mustNewV4(t),
		Kind:   metric_ingestor.ScanRunKindFull,
		SHA:    "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
	})
	if err == nil {
		t.Fatalf("Files(missing commit root): err=nil, want errors.Is ErrCommitRootNotMaterialised")
	}
	if !errors.Is(err, metric_ingestor.ErrCommitRootNotMaterialised) {
		t.Errorf("Files(missing commit root): err=%v, want errors.Is ErrCommitRootNotMaterialised", err)
	}
	if files != nil {
		t.Errorf("Files(missing commit root): files=%v, want nil", files)
	}
}

// TestDirectoryAstFileSource_WalksLayoutAndSortsOutput
// pins the canonical layout convention
// `<Root>/<repo_id>/<sha>/` and the path-sorted output
// invariant (G2 idempotency: re-runs at the same SHA emit
// identical drafts).
//
// The on-disk layout:
//
//	<tmp>/<repo_id>/<sha>/main.go      (parseable Go)
//	<tmp>/<repo_id>/<sha>/lib/b.go     (parseable Go)
//	<tmp>/<repo_id>/<sha>/lib/a.go     (parseable Go)
//	<tmp>/<repo_id>/<sha>/README.md    (unsupported -> skipped)
func TestDirectoryAstFileSource_WalksLayoutAndSortsOutput(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repoID := uuid.Must(uuid.FromString("ccccdddd-1111-2222-3333-444444444444"))
	sha := "1234567890123456789012345678901234567890"
	commitRoot := filepath.Join(tmp, repoID.String(), sha)
	if err := os.MkdirAll(filepath.Join(commitRoot, "lib"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustWriteFile(t, filepath.Join(commitRoot, "main.go"), goSampleMain)
	mustWriteFile(t, filepath.Join(commitRoot, "lib/a.go"), goSampleA)
	mustWriteFile(t, filepath.Join(commitRoot, "lib/b.go"), goSampleB)
	mustWriteFile(t, filepath.Join(commitRoot, "README.md"), []byte("# README"))

	src := &metric_ingestor.DirectoryAstFileSource{Root: tmp}
	files, err := src.Files(context.Background(), metric_ingestor.ScanRunContext{
		ID:     mustNewV7(t),
		RepoID: repoID,
		SHA:    sha, // ScanRunContext has no SHA today; the source reads from claim-side
		Kind:   metric_ingestor.ScanRunKindFull,
	})
	if err != nil {
		t.Fatalf("Files: err=%v, want nil", err)
	}
	if got := len(files); got != 3 {
		t.Fatalf("Files = %d, want 3 (main.go, lib/a.go, lib/b.go; README.md is unsupported)", got)
	}
	// Path-sorted: lib/a.go, lib/b.go, main.go.
	wantPaths := []string{"lib/a.go", "lib/b.go", "main.go"}
	for i, want := range wantPaths {
		if got := files[i].GetPath(); got != want {
			t.Errorf("files[%d].Path = %q, want %q", i, got, want)
		}
	}
}

// TestDirectoryAstFileSource_SkipPatterns covers the
// glob-skip mechanism used to exclude vendor/ etc.
func TestDirectoryAstFileSource_SkipPatterns(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repoID := mustNewV4(t)
	sha := "abcdef0123456789abcdef0123456789abcdef01"
	commitRoot := filepath.Join(tmp, repoID.String(), sha)
	if err := os.MkdirAll(filepath.Join(commitRoot, "vendor"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustWriteFile(t, filepath.Join(commitRoot, "main.go"), goSampleMain)
	mustWriteFile(t, filepath.Join(commitRoot, "vendor/lib.go"), goSampleA)

	src := &metric_ingestor.DirectoryAstFileSource{
		Root:         tmp,
		SkipPatterns: []string{"vendor/*"},
	}
	files, err := src.Files(context.Background(), metric_ingestor.ScanRunContext{
		ID: mustNewV7(t), RepoID: repoID, SHA: sha,
	})
	if err != nil {
		t.Fatalf("Files: err=%v, want nil", err)
	}
	if got := len(files); got != 1 {
		t.Fatalf("Files with vendor skip = %d, want 1 (only main.go)", got)
	}
	if got := files[0].GetPath(); got != "main.go" {
		t.Errorf("files[0].Path = %q, want main.go", got)
	}
}

// TestDirectoryAstFileSource_MaxFileBytes drops oversized
// files silently (with a skip count). Useful for guarding
// against pathological checkouts that include large
// generated assets.
func TestDirectoryAstFileSource_MaxFileBytes(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repoID := mustNewV4(t)
	sha := "fedcba9876543210fedcba9876543210fedcba98"
	commitRoot := filepath.Join(tmp, repoID.String(), sha)
	if err := os.MkdirAll(commitRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	mustWriteFile(t, filepath.Join(commitRoot, "small.go"), goSampleMain)
	// A "big" file that exceeds the 50-byte cap below.
	mustWriteFile(t, filepath.Join(commitRoot, "big.go"), make([]byte, 1024))

	src := &metric_ingestor.DirectoryAstFileSource{Root: tmp, MaxFileBytes: 50}
	files, err := src.Files(context.Background(), metric_ingestor.ScanRunContext{
		ID: mustNewV7(t), RepoID: repoID, SHA: sha,
	})
	if err != nil {
		t.Fatalf("Files: err=%v, want nil", err)
	}
	// Only small.go fits under MaxFileBytes (big.go is 1024 bytes).
	if got := len(files); got != 1 {
		t.Fatalf("Files with MaxFileBytes=50 = %d, want 1", got)
	}
}

func mustWriteFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func mustNewV4(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("uuid.NewV4: %v", err)
	}
	return id
}

func mustNewV7(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	return id
}

// goSampleMain / goSampleA / goSampleB are minimal valid
// Go source bodies used by the directory-source tests.
var (
	goSampleMain = []byte(`package main

func main() {
	println("hello")
}
`)

	goSampleA = []byte(`package lib

func A() int { return 1 }
`)

	goSampleB = []byte(`package lib

func B() int { return 2 }
`)
)
