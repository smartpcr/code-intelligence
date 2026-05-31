package fingerprint

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", abs, err)
	}
	return abs
}

// TestMTimeTreeSHA_StableOnNoop covers scenario (a): two
// back-to-back calls with no file changes between them return
// the identical 32-char hex string.
func TestMTimeTreeSHA_StableOnNoop(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", "alpha")
	writeFile(t, root, "sub/b.txt", "bravo")
	writeFile(t, root, "sub/c/d.txt", "delta")

	first, err := MTimeTreeSHA(root, nil)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if len(first) != 32 {
		t.Fatalf("expected 32-char hex, got %d chars: %q", len(first), first)
	}
	for _, r := range first {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			t.Fatalf("non-lowercase-hex character %q in %q", r, first)
		}
	}

	second, err := MTimeTreeSHA(root, nil)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if first != second {
		t.Fatalf("expected stable digest on no-op walk, got %q vs %q", first, second)
	}
}

// TestMTimeTreeSHA_ChangesOnMtimeBump covers scenario (b): when
// any file's mtime moves, the returned string differs.
func TestMTimeTreeSHA_ChangesOnMtimeBump(t *testing.T) {
	root := t.TempDir()
	a := writeFile(t, root, "a.txt", "alpha")
	writeFile(t, root, "b.txt", "bravo")

	before, err := MTimeTreeSHA(root, nil)
	if err != nil {
		t.Fatalf("before: %v", err)
	}

	// Bump a.txt's mtime forward by an hour; size and contents
	// stay identical so only the mtime delta drives the change.
	newTime := time.Now().Add(time.Hour).Truncate(time.Second)
	if err := os.Chtimes(a, newTime, newTime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	after, err := MTimeTreeSHA(root, nil)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("expected digest to change after mtime bump, both = %q", before)
	}
}

// TestMTimeTreeSHA_ExcludesAreSkipped covers scenario (c):
// excluded directories contribute zero bytes, verified by
// removing the excluded dir and re-hashing -- the digest is
// identical.
func TestMTimeTreeSHA_ExcludesAreSkipped(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "src/main.go", "package main")
	writeFile(t, root, ".git/HEAD", "ref: refs/heads/main")
	writeFile(t, root, ".git/objects/aa/bb", "blob")
	writeFile(t, root, "node_modules/pkg/index.js", "module.exports = 1")

	excluded, err := MTimeTreeSHA(root, []string{".git", "node_modules"})
	if err != nil {
		t.Fatalf("with excludes: %v", err)
	}

	// Hash WITHOUT excludes must differ -- the excluded files
	// would have contributed bytes.
	withGit, err := MTimeTreeSHA(root, nil)
	if err != nil {
		t.Fatalf("without excludes: %v", err)
	}
	if withGit == excluded {
		t.Fatalf("expected excluded vs unfiltered digests to differ; both = %q", excluded)
	}

	// Physically remove the excluded dirs and re-hash with no
	// exclude list. If the exclude implementation is correct,
	// the digest must equal the earlier excluded run.
	if err := os.RemoveAll(filepath.Join(root, ".git")); err != nil {
		t.Fatalf("rm .git: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(root, "node_modules")); err != nil {
		t.Fatalf("rm node_modules: %v", err)
	}
	after, err := MTimeTreeSHA(root, nil)
	if err != nil {
		t.Fatalf("after rm: %v", err)
	}
	if after != excluded {
		t.Fatalf("expected exclude-set to behave like removal: excluded=%q after-rm=%q", excluded, after)
	}
}

// TestMTimeTreeSHA_MissingRoot covers scenario (d): a path that
// does not exist must return a non-nil error and the empty
// string.
func TestMTimeTreeSHA_MissingRoot(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does", "not", "exist")
	hash, err := MTimeTreeSHA(missing, nil)
	if err == nil {
		t.Fatalf("expected error for missing root, got nil (hash=%q)", hash)
	}
	if hash != "" {
		t.Fatalf("expected empty hash for missing root, got %q", hash)
	}
	if !strings.Contains(err.Error(), "MTimeTreeSHA") {
		t.Fatalf("expected error to mention helper name, got %v", err)
	}
}

// TestMTimeTreeSHA_NotADirectory ensures pointing at a regular
// file (not a directory) is reported as an error rather than
// silently returning the empty-tree digest.
func TestMTimeTreeSHA_NotADirectory(t *testing.T) {
	root := t.TempDir()
	file := writeFile(t, root, "x.txt", "x")
	hash, err := MTimeTreeSHA(file, nil)
	if err == nil {
		t.Fatalf("expected error when root is a file, got hash=%q", hash)
	}
	if hash != "" {
		t.Fatalf("expected empty hash, got %q", hash)
	}
}

// TestMTimeTreeSHA_ChangesOnSizeOrContent ensures the digest
// also reacts to size changes (size is part of the pre-image
// per architecture S4.3). This is a regression guard against an
// implementation that only mixes mtime into the hash.
func TestMTimeTreeSHA_ChangesOnSizeOrContent(t *testing.T) {
	root := t.TempDir()
	p := writeFile(t, root, "a.txt", "alpha")

	before, err := MTimeTreeSHA(root, nil)
	if err != nil {
		t.Fatalf("before: %v", err)
	}

	// Grow the file; this changes both size and mtime, but
	// either alone is sufficient for the digest to shift.
	if err := os.WriteFile(p, []byte("alpha+extra"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	after, err := MTimeTreeSHA(root, nil)
	if err != nil {
		t.Fatalf("after: %v", err)
	}
	if before == after {
		t.Fatalf("expected digest change after content rewrite; both = %q", before)
	}
}

// TestMTimeTreeSHA_EmptyDirectory documents the behaviour for an
// empty tree: a valid 32-char hex string derived from the empty
// SHA-256 pre-image, returned without error.
func TestMTimeTreeSHA_EmptyDirectory(t *testing.T) {
	root := t.TempDir()
	hash, err := MTimeTreeSHA(root, nil)
	if err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	if len(hash) != 32 {
		t.Fatalf("expected 32-char hex, got %q", hash)
	}
	// SHA-256 of the empty string truncated to 16 bytes.
	const emptySha256First16Hex = "e3b0c44298fc1c149afbf4c8996fb924"
	if hash != emptySha256First16Hex {
		t.Fatalf("expected %q, got %q", emptySha256First16Hex, hash)
	}
}
