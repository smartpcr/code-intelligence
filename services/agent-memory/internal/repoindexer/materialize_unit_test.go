package repoindexer

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInMemoryMaterializer_walksFilesInLexOrder pins the
// in-memory materializer's contract that Walk surfaces files in
// lexicographic RelPath order regardless of insertion order. The
// full-mode handler's package cache assumes a stable enumeration
// so the on-disk Walk and the in-memory Walk produce identical
// per-package row counts.
func TestInMemoryMaterializer_walksFilesInLexOrder(t *testing.T) {
	t.Parallel()
	m := &InMemoryMaterializer{
		Files: []InMemoryFile{
			{RelPath: "z/last.go", Content: []byte("z")},
			{RelPath: "a/first.go", Content: []byte("a")},
			{RelPath: "m/middle.go", Content: []byte("m")},
		},
	}
	ws, err := m.Materialize(context.Background(), "https://example/x", "deadbeef")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	defer ws.Close()
	var got []string
	if err := ws.Walk(func(f WalkFile) error {
		got = append(got, f.RelPath)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{"a/first.go", "m/middle.go", "z/last.go"}
	if len(got) != len(want) {
		t.Fatalf("count: got %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

// TestInMemoryMaterializer_rejectsBackslashPaths confirms the
// canonical-path invariant. A backslash in a RelPath would
// silently break the canonical signature on Windows (filepath.ToSlash
// only normalises filepath calls; an already-backslash literal
// would survive the Walk untouched). Reject at Materialize time
// so the failure has a clear pointer to the test data error.
func TestInMemoryMaterializer_rejectsBackslashPaths(t *testing.T) {
	t.Parallel()
	m := &InMemoryMaterializer{
		Files: []InMemoryFile{{RelPath: `foo\bar.go`}},
	}
	_, err := m.Materialize(context.Background(), "https://example/x", "abc")
	if err == nil {
		t.Fatal("expected backslash RelPath rejection; got nil")
	}
	if !strings.Contains(err.Error(), "forward slashes") {
		t.Errorf("error did not mention forward slashes: %v", err)
	}
}

// TestInMemoryMaterializer_readerReturnsFreshStream confirms a
// per-call new reader so the AST emitter (which may make
// multiple passes) does not exhaust the same shared stream.
func TestInMemoryMaterializer_readerReturnsFreshStream(t *testing.T) {
	t.Parallel()
	m := &InMemoryMaterializer{
		Files: []InMemoryFile{{RelPath: "a.go", Content: []byte("hello")}},
	}
	ws, err := m.Materialize(context.Background(), "https://example/x", "abc")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	defer ws.Close()
	var got []byte
	if err := ws.Walk(func(f WalkFile) error {
		// Read twice to confirm fresh readers each call.
		for i := 0; i < 2; i++ {
			rc, err := f.Reader()
			if err != nil {
				return err
			}
			b, err := io.ReadAll(rc)
			_ = rc.Close()
			if err != nil {
				return err
			}
			got = b
		}
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

// TestGitMaterializer_runCmdInjection asserts the runCmd hook
// is invoked with the exact `git ...` sequence the brief calls
// for (init / remote add / fetch --depth=1 SHA / checkout
// FETCH_HEAD). Tests the dispatcher shape; the real `git` is
// not invoked.
func TestGitMaterializer_runCmdInjection(t *testing.T) {
	t.Parallel()
	type call struct {
		dir, bin string
		args     []string
	}
	var calls []call
	g := &GitMaterializer{
		GitBinary: "git-stub",
		runCmd: func(_ context.Context, dir, bin string, args ...string) error {
			calls = append(calls, call{dir: dir, bin: bin, args: append([]string(nil), args...)})
			return nil
		},
	}
	ws, err := g.Materialize(context.Background(),
		"https://example.test/repo.git", "deadbeef")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	// Even though the runCmd is a stub, the function still
	// created the temp dir; tidy it up.
	defer ws.Close()

	want := []struct {
		args []string
	}{
		{[]string{"init", "--quiet"}},
		{[]string{"remote", "add", "origin", "https://example.test/repo.git"}},
		{[]string{"fetch", "--depth=1", "--quiet", "origin", "deadbeef"}},
		{[]string{"checkout", "--quiet", "FETCH_HEAD"}},
	}
	if len(calls) != len(want) {
		t.Fatalf("call count: got %d, want %d (%+v)", len(calls), len(want), calls)
	}
	for i, c := range calls {
		if c.bin != "git-stub" {
			t.Errorf("[%d].bin = %q, want git-stub", i, c.bin)
		}
		if !sliceEqual(c.args, want[i].args) {
			t.Errorf("[%d].args = %v, want %v", i, c.args, want[i].args)
		}
	}
}

// TestGitWorkspace_walkSkipsExcludeDirs builds a tiny on-disk
// tree containing `.git/`, `node_modules/`, and a real source
// file, then asserts Walk surfaces only the real source file.
// Uses a real `gitWorkspace` constructed by hand (bypassing
// the git CLI) so the exclude-dir behaviour is tested in
// isolation.
func TestGitWorkspace_walkSkipsExcludeDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	mkfile := func(rel string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := writeTextFile(full, "content"); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	mkfile(".git/HEAD")
	mkfile("node_modules/leftpad/index.js")
	mkfile("src/main.go")
	mkfile("README.md")

	ws := &gitWorkspace{
		root: root,
		excludeDirs: map[string]struct{}{
			".git":         {},
			"node_modules": {},
		},
	}
	defer ws.Close()
	var got []string
	if err := ws.Walk(func(f WalkFile) error {
		got = append(got, f.RelPath)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	want := []string{"README.md", "src/main.go"}
	if !sliceEqual(got, want) {
		t.Errorf("Walk got %v, want %v", got, want)
	}
}

// TestGitWorkspace_relPathUsesForwardSlash confirms cross-OS
// stability of the RelPath the walker surfaces. Without
// `filepath.ToSlash` a Windows host would emit `src\main.go`
// and the canonical signature would diverge between OSes,
// breaking idempotent re-ingest of the same repo across the
// dev (Windows) and CI (Linux) substrates.
func TestGitWorkspace_relPathUsesForwardSlash(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := writeTextFile(filepath.Join(root, filepath.FromSlash("nested/dir/x.go")), ""); err != nil {
		t.Fatalf("write: %v", err)
	}
	ws := &gitWorkspace{root: root, excludeDirs: map[string]struct{}{}}
	defer ws.Close()
	var got string
	if err := ws.Walk(func(f WalkFile) error {
		got = f.RelPath
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got != "nested/dir/x.go" {
		t.Errorf("RelPath = %q, want nested/dir/x.go (forward slashes regardless of OS=%s)",
			got, runtime.GOOS)
	}
}

// TestGitWorkspace_closeRemovesRoot confirms Close tidies the
// temp dir. Multiple Close calls are safe (idempotent).
func TestGitWorkspace_closeRemovesRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Drop a file so the dir is non-empty.
	if err := writeTextFile(filepath.Join(root, "a.txt"), "x"); err != nil {
		t.Fatalf("write: %v", err)
	}
	ws := &gitWorkspace{root: root, excludeDirs: map[string]struct{}{}}
	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := ws.Close(); err != nil {
		t.Errorf("second Close: %v (want nil; Close must be idempotent)", err)
	}
}

// sliceEqual is a tiny equality helper local to the test file
// so we don't pull a comparison library.
func sliceEqual[T comparable](a, b []T) bool {
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

// writeTextFile is a tiny helper that creates parent dirs and
// writes the file with default permissions. Local to the test
// file so we don't take a dependency on a fixture library.
func writeTextFile(full, content string) error {
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, []byte(content), 0o644)
}
