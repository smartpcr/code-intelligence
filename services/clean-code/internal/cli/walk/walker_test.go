package walk_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/walk"
)

// collect drains all three channels concurrently until each
// closes. The walker contract requires the consumer to drain
// all three; collecting in parallel mirrors what the
// orchestrator does in production. Nil channels are
// permitted -- tests that don't care about one of the
// streams may pass nil and the corresponding drain goroutine
// is skipped (ranging over a nil channel would block forever).
func collect(t *testing.T, files <-chan walk.WalkedFile, skips <-chan walk.WalkSkip, errs <-chan error) ([]walk.WalkedFile, []walk.WalkSkip, []error) {
	t.Helper()
	var (
		wg     sync.WaitGroup
		got    []walk.WalkedFile
		gotSk  []walk.WalkSkip
		gotErr []error
	)
	if files != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range files {
				got = append(got, f)
			}
		}()
	}
	if skips != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range skips {
				gotSk = append(gotSk, s)
			}
		}()
	}
	if errs != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for e := range errs {
				gotErr = append(gotErr, e)
			}
		}()
	}
	wg.Wait()
	return got, gotSk, gotErr
}

// writeFile writes content to root/relPath, creating parent
// directories as needed. Fails the test on IO error.
func writeFile(t *testing.T, root, relPath, content string) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdirall %q: %v", filepath.Dir(abs), err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %q: %v", abs, err)
	}
}

// pathsOf returns the RepoRelPath of each WalkedFile in
// the order emitted; the walker's contract is lexicographic
// ordering by RepoRelPath.
func pathsOf(in []walk.WalkedFile) []string {
	out := make([]string, len(in))
	for i, f := range in {
		out[i] = f.RepoRelPath
	}
	return out
}

// hasSkip reports whether any [walk.WalkSkip] in `skips`
// matches the (path, reason) pair. Used because helper
// scenarios may legitimately produce other unrelated skips
// (e.g. `unsupported_language` for an incidental README) that
// would make a strict slice-equality assertion brittle.
func hasSkip(skips []walk.WalkSkip, path, reason string) bool {
	for _, s := range skips {
		if s.Path == path && s.Reason == reason {
			return true
		}
	}
	return false
}

// countSkips returns the number of skips with the given reason.
func countSkips(skips []walk.WalkSkip, reason string) int {
	n := 0
	for _, s := range skips {
		if s.Reason == reason {
			n++
		}
	}
	return n
}

// TestWalk_SkipDirectories_AllBaseline asserts that every
// directory name in [walk.DefaultSkipDirs] is honoured: no
// descendant is emitted, and exactly one WalkSkip with
// `directory_skip` is emitted per skip directory.
//
// E2E scenario: "Walker honours hard-coded skip directories"
// (`e2e-scenarios.md` impl-plan Stage 2.1; arch Sec 3.1).
func TestWalk_SkipDirectories_AllBaseline(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// One source file per skip directory, plus a sentinel
	// at the root so the walker has something to emit.
	for _, dir := range walk.DefaultSkipDirs {
		writeFile(t, root, dir+"/foo.go", "package x")
	}
	writeFile(t, root, "keep.go", "package x\n")

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if names := pathsOf(got); !equalStrings(names, []string{"keep.go"}) {
		t.Errorf("emitted files = %v; want [keep.go]", names)
	}
	for _, dir := range walk.DefaultSkipDirs {
		if !hasSkip(gotSk, dir, walk.SkipReasonDirectory) {
			t.Errorf("missing WalkSkip{Reason:directory_skip, Path:%q}; got %v", dir, gotSk)
		}
	}
	// Exactly one directory_skip per skip directory.
	if got, want := countSkips(gotSk, walk.SkipReasonDirectory), len(walk.DefaultSkipDirs); got != want {
		t.Errorf("directory_skip count = %d; want %d (got skips: %v)", got, want, gotSk)
	}
}

// TestWalk_Gitignore_HonoursRootFile asserts a .gitignore at
// the repo root causes the listed file to surface as a
// `WalkSkip{Reason: "gitignore"}` and zero WalkedFile rows.
//
// E2E scenario: ".gitignore matches are surfaced as walk skips"
// (e2e Stage 2.1).
func TestWalk_Gitignore_HonoursRootFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, ".gitignore", "secret.go\n")
	writeFile(t, root, "secret.go", "package x\n")
	writeFile(t, root, "ok.go", "package x\n")

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if names := pathsOf(got); !equalStrings(names, []string{"ok.go"}) {
		t.Errorf("emitted files = %v; want [ok.go]", names)
	}
	if !hasSkip(gotSk, "secret.go", walk.SkipReasonGitignore) {
		t.Errorf("missing gitignore skip for secret.go; got %v", gotSk)
	}
}

// TestWalk_Gitignore_HonoursNestedFile asserts a .gitignore
// loaded mid-traversal applies to descendant entries via the
// per-pattern domain check.
func TestWalk_Gitignore_HonoursNestedFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/.gitignore", "private.go\n")
	writeFile(t, root, "pkg/private.go", "package x\n")
	writeFile(t, root, "pkg/public.go", "package x\n")
	writeFile(t, root, "other/private.go", "package x\n") // not gitignored, different subtree

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	names := pathsOf(got)
	want := []string{"other/private.go", "pkg/public.go"}
	if !equalStrings(names, want) {
		t.Errorf("emitted files = %v; want %v", names, want)
	}
	if !hasSkip(gotSk, "pkg/private.go", walk.SkipReasonGitignore) {
		t.Errorf("missing gitignore skip for pkg/private.go; got %v", gotSk)
	}
	// other/private.go must NOT be skipped by gitignore;
	// it lives outside the .gitignore's domain.
	for _, s := range gotSk {
		if s.Path == "other/private.go" && s.Reason == walk.SkipReasonGitignore {
			t.Errorf("unexpected gitignore skip outside the pattern's domain: %v", s)
		}
	}
}

// TestWalk_Gitignore_HonoursInfoExclude asserts that
// .git/info/exclude is consulted with an empty domain so its
// rules apply repo-wide.
func TestWalk_Gitignore_HonoursInfoExclude(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, ".git/info/exclude", "drop.go\n")
	writeFile(t, root, "drop.go", "package x\n")
	writeFile(t, root, "keep.go", "package x\n")

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	// Even though .git/ is itself skipped as a directory,
	// the exclude file was read at walk start.
	if names := pathsOf(got); !equalStrings(names, []string{"keep.go"}) {
		t.Errorf("emitted files = %v; want [keep.go]", names)
	}
	if !hasSkip(gotSk, "drop.go", walk.SkipReasonGitignore) {
		t.Errorf("missing gitignore skip for drop.go from .git/info/exclude; got %v", gotSk)
	}
}

// TestWalk_Gitignore_CommentsAndBlankLinesIgnored asserts the
// gitignore parser strips `#` comments and blank lines so they
// do not become bogus patterns.
func TestWalk_Gitignore_CommentsAndBlankLinesIgnored(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// The blank and comment lines must NOT be interpreted as
	// patterns matching `secret.go` or `ok.go`.
	writeFile(t, root, ".gitignore", "\n# this is a comment\n\nsecret.go\n# trailing comment\n")
	writeFile(t, root, "secret.go", "package x\n")
	writeFile(t, root, "ok.go", "package x\n")

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if names := pathsOf(got); !equalStrings(names, []string{"ok.go"}) {
		t.Errorf("emitted files = %v; want [ok.go]", names)
	}
	if !hasSkip(gotSk, "secret.go", walk.SkipReasonGitignore) {
		t.Errorf("missing gitignore skip for secret.go; got %v", gotSk)
	}
}

// TestWalk_SizeCap_OversizeFileIsSkippedWithoutRead asserts
// the 2 MiB cap is enforced via stat alone: a file strictly
// larger than the cap emits `size_cap` AND the file's bytes
// are NEVER read. The "never read" half is verified by
// installing a [ReadFileFn] override that panics if called for
// the oversize path.
//
// E2E scenario: "Per-file size cap enforced at 2 MiB"
// (`e2e-scenarios.md` Stage 2.1; tech-spec Sec 8.3).
func TestWalk_SizeCap_OversizeFileIsSkippedWithoutRead(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// 3 MiB sparse file: write a single byte at offset
	// 3 MiB - 1, so the on-disk size exceeds the cap but
	// disk usage stays tiny.
	abs := filepath.Join(root, "large.go")
	f, err := os.Create(abs)
	if err != nil {
		t.Fatalf("create large.go: %v", err)
	}
	if _, err := f.WriteAt([]byte{0}, 3*1024*1024-1); err != nil {
		t.Fatalf("seek-write large.go: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close large.go: %v", err)
	}
	writeFile(t, root, "small.go", "package x\n")

	var readCalls []string
	w := &walk.DefaultWalker{
		ReadFileFn: func(p string) ([]byte, error) {
			readCalls = append(readCalls, p)
			if strings.HasSuffix(filepath.ToSlash(p), "/large.go") {
				t.Fatalf("ReadFile must not be called for oversize file: %s", p)
			}
			return os.ReadFile(p)
		},
	}
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if !hasSkip(gotSk, "large.go", walk.SkipReasonSizeCap) {
		t.Errorf("missing size_cap skip for large.go; got %v", gotSk)
	}
	if names := pathsOf(got); !equalStrings(names, []string{"small.go"}) {
		t.Errorf("emitted files = %v; want [small.go]", names)
	}
	// Defensive: even with the panic guard, assert that
	// the recorded read calls don't include large.go.
	for _, p := range readCalls {
		if strings.HasSuffix(filepath.ToSlash(p), "/large.go") {
			t.Errorf("ReadFile recorded for oversize path: %s", p)
		}
	}
}

// TestWalk_SizeCap_AtCapIsAdmitted asserts the cap is
// inclusive: a file of exactly [walk.MaxFileSizeBytes] passes.
//
// E2E scenario: "File at exactly the 2 MiB cap is admitted"
// (e2e Stage 2.1).
func TestWalk_SizeCap_AtCapIsAdmitted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	abs := filepath.Join(root, "edge.go")
	buf := make([]byte, walk.MaxFileSizeBytes)
	// Real text content so DetectLanguage returns `go`.
	copy(buf, "package x\n")
	if err := os.WriteFile(abs, buf, 0o644); err != nil {
		t.Fatalf("write edge.go: %v", err)
	}

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if countSkips(gotSk, walk.SkipReasonSizeCap) != 0 {
		t.Errorf("expected 0 size_cap skips for at-cap file; got %v", gotSk)
	}
	if names := pathsOf(got); !equalStrings(names, []string{"edge.go"}) {
		t.Errorf("emitted files = %v; want [edge.go]", names)
	}
	if got[0].SizeBytes != walk.MaxFileSizeBytes {
		t.Errorf("SizeBytes = %d; want %d", got[0].SizeBytes, walk.MaxFileSizeBytes)
	}
}

// TestWalk_RootNotFound_EmitsSentinel asserts a non-existent
// root surfaces [walk.ErrRootNotFound] on the error channel
// AND the file/skip channels close with zero rows.
//
// E2E scenario: "Missing root path exits with code 2"
// (e2e Stage 2.1; tech-spec Sec 8.6).
func TestWalk_RootNotFound_EmitsSentinel(t *testing.T) {
	t.Parallel()
	// A path that cannot exist on either platform.
	root := filepath.Join(t.TempDir(), "definitely-not-a-real-directory-12345")

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(got) != 0 {
		t.Errorf("expected 0 files; got %v", got)
	}
	if len(gotSk) != 0 {
		t.Errorf("expected 0 skips; got %v", gotSk)
	}
	if len(gotErr) != 1 {
		t.Fatalf("expected exactly 1 error; got %v", gotErr)
	}
	if !isErrRootNotFound(gotErr[0]) {
		t.Errorf("expected ErrRootNotFound; got %v", gotErr[0])
	}
}

// TestWalk_RootIsFile_EmitsError asserts that pointing the
// walker at a regular file (not a directory) is a fatal walk
// error (orchestrator maps to exit code 2).
func TestWalk_RootIsFile_EmitsError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "not-a-dir.go")
	if err := os.WriteFile(root, []byte("package x"), 0o644); err != nil {
		t.Fatalf("write %q: %v", root, err)
	}

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(got) != 0 {
		t.Errorf("expected 0 files; got %v", got)
	}
	if len(gotSk) != 0 {
		t.Errorf("expected 0 skips; got %v", gotSk)
	}
	if len(gotErr) != 1 {
		t.Fatalf("expected exactly 1 error; got %v", gotErr)
	}
	if !strings.Contains(gotErr[0].Error(), "not a directory") {
		t.Errorf("error message missing 'not a directory': %v", gotErr[0])
	}
}

// TestWalk_DeterministicOrder asserts re-runs against the same
// fixture produce identical WalkedFile orderings. The walker
// inherits per-directory lexicographic ordering from
// [filepath.WalkDir].
//
// E2E scenario: "Walker emits files in deterministic
// lexicographic order" (e2e Stage 2.1; tech-spec C11).
func TestWalk_DeterministicOrder(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "b.go", "package x\n")
	writeFile(t, root, "a.go", "package x\n")
	writeFile(t, root, "c.go", "package x\n")
	writeFile(t, root, "sub/m.go", "package x\n")
	writeFile(t, root, "sub/n.go", "package x\n")

	w := walk.NewDefaultWalker()

	files1, skips1, errs1 := w.Walk(context.Background(), root)
	got1, _, gotErr1 := collect(t, files1, skips1, errs1)
	if len(gotErr1) != 0 {
		t.Fatalf("unexpected errors on first run: %v", gotErr1)
	}

	files2, skips2, errs2 := w.Walk(context.Background(), root)
	got2, _, gotErr2 := collect(t, files2, skips2, errs2)
	if len(gotErr2) != 0 {
		t.Fatalf("unexpected errors on second run: %v", gotErr2)
	}

	names1 := pathsOf(got1)
	names2 := pathsOf(got2)
	want := []string{"a.go", "b.go", "c.go", "sub/m.go", "sub/n.go"}
	if !equalStrings(names1, want) {
		t.Errorf("run 1 emitted files = %v; want %v", names1, want)
	}
	if !equalStrings(names2, want) {
		t.Errorf("run 2 emitted files = %v; want %v", names2, want)
	}
}

// TestWalk_FourLanguages_AllEmitted asserts the walker emits
// a WalkedFile for each of the v1 pinned languages (Go,
// Python, TypeScript, Java) with the correct Language field
// set.
//
// E2E scenario: "Four-language parse fan-out" (e2e Stage 2.2;
// arch Sec 3.2) — the walker half.
func TestWalk_FourLanguages_AllEmitted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "a.go", "package x\n")
	writeFile(t, root, "b.py", "pass\n")
	writeFile(t, root, "c.ts", "export const x = 1;\n")
	writeFile(t, root, "d.java", "class D {}\n")

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	wantLangs := map[string]string{
		"a.go":   "go",
		"b.py":   "python",
		"c.ts":   "typescript",
		"d.java": "java",
	}
	if len(got) != len(wantLangs) {
		t.Fatalf("emitted %d files; want %d (%v)", len(got), len(wantLangs), pathsOf(got))
	}
	for _, f := range got {
		want, ok := wantLangs[f.RepoRelPath]
		if !ok {
			t.Errorf("unexpected file %q", f.RepoRelPath)
			continue
		}
		if f.Language != want {
			t.Errorf("file %q Language = %q; want %q", f.RepoRelPath, f.Language, want)
		}
	}
	if countSkips(gotSk, walk.SkipReasonUnsupportedLanguage) != 0 {
		t.Errorf("expected zero unsupported_language skips; got %v", gotSk)
	}
}

// TestWalk_UnsupportedLanguage_Emitted asserts files whose
// extension is outside the v1 pinned set emit
// `unsupported_language` skips. C# and Rust are the canonical
// fixture pair from the e2e scenarios.
//
// E2E scenario: "Non-v1 language file is skipped" (e2e Stage
// 2.2).
func TestWalk_UnsupportedLanguage_Emitted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "Program.cs", "class Program {}\n")
	writeFile(t, root, "main.rs", "fn main() {}\n")

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 WalkedFile rows; got %v", pathsOf(got))
	}
	if !hasSkip(gotSk, "Program.cs", walk.SkipReasonUnsupportedLanguage) {
		t.Errorf("missing unsupported_language skip for Program.cs; got %v", gotSk)
	}
	if !hasSkip(gotSk, "main.rs", walk.SkipReasonUnsupportedLanguage) {
		t.Errorf("missing unsupported_language skip for main.rs; got %v", gotSk)
	}
	if got, want := countSkips(gotSk, walk.SkipReasonUnsupportedLanguage), 2; got != want {
		t.Errorf("unsupported_language skip count = %d; want %d", got, want)
	}
}

// TestWalk_UnsupportedLanguage_NotReadFromDisk asserts the
// walker filters by extension WITHOUT calling its read hook:
// for unsupported files, neither the size cap stat nor the
// content read run.
func TestWalk_UnsupportedLanguage_NotReadFromDisk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "Program.cs", "class Program {}\n")
	writeFile(t, root, "main.go", "package x\n")

	var readCalls []string
	w := &walk.DefaultWalker{
		ReadFileFn: func(p string) ([]byte, error) {
			readCalls = append(readCalls, p)
			return os.ReadFile(p)
		},
	}
	files, _, errs := w.Walk(context.Background(), root)
	got, _, gotErr := collect(t, files, nil, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if names := pathsOf(got); !equalStrings(names, []string{"main.go"}) {
		t.Errorf("emitted files = %v; want [main.go]", names)
	}
	for _, p := range readCalls {
		if strings.HasSuffix(filepath.ToSlash(p), "/Program.cs") {
			t.Errorf("ReadFile must not be called for unsupported-language file: %s", p)
		}
	}
}

// TestWalk_ZeroByteFileSkipped asserts the walker emits an
// `empty` skip for zero-byte supported-language files; the
// downstream parser returns [parser.ErrEmptyContent] for them,
// and the architecture pins the walker as the upstream filter.
func TestWalk_ZeroByteFileSkipped(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "empty.go", "")
	writeFile(t, root, "ok.go", "package x\n")

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if names := pathsOf(got); !equalStrings(names, []string{"ok.go"}) {
		t.Errorf("emitted files = %v; want [ok.go]", names)
	}
	if !hasSkip(gotSk, "empty.go", walk.SkipReasonEmpty) {
		t.Errorf("missing empty skip for empty.go; got %v", gotSk)
	}
}

// TestWalk_WalkedFileShape asserts every field of the emitted
// WalkedFile is populated correctly: RepoRelPath is forward
// slash, AbsPath is an absolute path, Language is canonical,
// SizeBytes matches `len(Content)`.
func TestWalk_WalkedFileShape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "pkg/a.go", "package x\n")

	w := walk.NewDefaultWalker()
	files, skips, errs := w.Walk(context.Background(), root)
	got, _, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if len(got) != 1 {
		t.Fatalf("emitted %d files; want 1", len(got))
	}
	f := got[0]
	if f.RepoRelPath != "pkg/a.go" {
		t.Errorf("RepoRelPath = %q; want %q (forward slash, relative)", f.RepoRelPath, "pkg/a.go")
	}
	if !filepath.IsAbs(f.AbsPath) {
		t.Errorf("AbsPath = %q; want absolute", f.AbsPath)
	}
	if f.Language != "go" {
		t.Errorf("Language = %q; want %q", f.Language, "go")
	}
	if f.SizeBytes != int64(len(f.Content)) {
		t.Errorf("SizeBytes %d != len(Content) %d", f.SizeBytes, len(f.Content))
	}
	if string(f.Content) != "package x\n" {
		t.Errorf("Content = %q; want %q", f.Content, "package x\n")
	}
}

// TestWalk_SymlinkLoop_AncestorIsDetected verifies the v1
// best-effort symlink-loop guard surfaces a symlinked
// directory whose target is an ancestor of the link path. On
// Windows the test is skipped because non-admin symlink
// creation is not generally allowed; the guard's POSIX
// behaviour is the contract-anchoring case (the Windows half
// follows the same code path via canonical-path equality).
//
// E2E scenario: "Symlink-loop guard works on POSIX and
// Windows" (e2e Stage 2.1).
func TestWalk_SymlinkLoop_AncestorIsDetected(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("non-admin symlink creation is restricted on Windows; covered by the cross-platform e2e scenario")
	}
	root := t.TempDir()
	// Layout: root/a/keep.go and root/a/loop -> root/a
	// — `loop` symlinks back to its own parent.
	if err := os.MkdirAll(filepath.Join(root, "a"), 0o755); err != nil {
		t.Fatalf("mkdir a: %v", err)
	}
	writeFile(t, root, "a/keep.go", "package x\n")
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "a", "loop")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	w := walk.NewDefaultWalker()

	// A hard wall-time bound asserts no infinite recursion
	// occurs even if the guard misses (the WalkDir default
	// of "do not follow symlinks" is the second line of
	// defence).
	done := make(chan struct{})
	var (
		got    []walk.WalkedFile
		gotSk  []walk.WalkSkip
		gotErr []error
	)
	go func() {
		defer close(done)
		files, skips, errs := w.Walk(context.Background(), root)
		got, gotSk, gotErr = collect(t, files, skips, errs)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("walker did not finish within 5s; suspected infinite recursion")
	}

	if len(gotErr) != 0 {
		t.Fatalf("unexpected errors: %v", gotErr)
	}
	if !hasSkip(gotSk, "a/loop", walk.SkipReasonSymlinkLoop) {
		t.Errorf("missing symlink_loop skip for a/loop; got %v", gotSk)
	}
	if names := pathsOf(got); !equalStrings(names, []string{"a/keep.go"}) {
		t.Errorf("emitted files = %v; want [a/keep.go]", names)
	}
}

// TestWalk_ContextCancellation asserts that cancelling the
// context stops the walker promptly even when the consumer is
// slow to drain. Channels are closed; no goroutine leak.
func TestWalk_ContextCancellation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Enough files to ensure the walker is still sending
	// after the cancel arrives.
	for i := 0; i < 200; i++ {
		writeFile(t, root, fmtIndex(i), "package x\n")
	}

	w := walk.NewDefaultWalker()
	ctx, cancel := context.WithCancel(context.Background())
	files, skips, errs := w.Walk(ctx, root)
	// Cancel before draining.
	cancel()

	// Drain to channel-close. With cancellation, the send
	// helpers fall through and the goroutine returns
	// promptly. Without a leak the channels MUST close.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range files {
		}
		for range skips {
		}
		for range errs {
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("walker did not close channels after context cancellation")
	}
}

// TestWalk_ReadError_NonFatal asserts a read error on one
// file does not abort the walk; the read failure surfaces as
// `read_error` and the rest of the tree is emitted normally.
func TestWalk_ReadError_NonFatal(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFile(t, root, "bad.go", "package x\n")
	writeFile(t, root, "ok.go", "package x\n")

	w := &walk.DefaultWalker{
		ReadFileFn: func(p string) ([]byte, error) {
			if strings.HasSuffix(filepath.ToSlash(p), "/bad.go") {
				return nil, &os.PathError{Op: "read", Path: p, Err: fs.ErrPermission}
			}
			return os.ReadFile(p)
		},
	}
	files, skips, errs := w.Walk(context.Background(), root)
	got, gotSk, gotErr := collect(t, files, skips, errs)

	if len(gotErr) != 0 {
		t.Fatalf("unexpected fatal errors: %v", gotErr)
	}
	if names := pathsOf(got); !equalStrings(names, []string{"ok.go"}) {
		t.Errorf("emitted files = %v; want [ok.go]", names)
	}
	if !hasSkip(gotSk, "bad.go", walk.SkipReasonReadError) {
		t.Errorf("missing read_error skip for bad.go; got %v", gotSk)
	}
}

// TestSkipped_StableSort asserts the [walk.Skipped] helper
// sorts by (Path, Reason) without modifying the input slice.
func TestSkipped_StableSort(t *testing.T) {
	t.Parallel()
	in := []walk.WalkSkip{
		{Path: "z/a.go", Reason: walk.SkipReasonGitignore},
		{Path: "a/b.go", Reason: walk.SkipReasonSizeCap},
		{Path: "a/b.go", Reason: walk.SkipReasonGitignore},
	}
	original := append([]walk.WalkSkip(nil), in...)
	out := walk.Skipped(in)

	if !equalSkips(in, original) {
		t.Errorf("Skipped mutated input: got %v want %v", in, original)
	}
	want := []walk.WalkSkip{
		{Path: "a/b.go", Reason: walk.SkipReasonGitignore},
		{Path: "a/b.go", Reason: walk.SkipReasonSizeCap},
		{Path: "z/a.go", Reason: walk.SkipReasonGitignore},
	}
	if !equalSkips(out, want) {
		t.Errorf("Skipped result = %v; want %v", out, want)
	}
}

// --- helpers ---

func equalStrings(a, b []string) bool {
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	// The walker contract pins lexicographic ordering, so
	// raw equality is the stricter check; we apply it
	// without re-sorting `a` to keep the order-checking
	// callsites honest.
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	_ = ac
	_ = bc
	return true
}

func equalSkips(a, b []walk.WalkSkip) bool {
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

func isErrRootNotFound(err error) bool {
	for err != nil {
		if err == walk.ErrRootNotFound {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func fmtIndex(i int) string {
	// Zero-pad so lexicographic ordering matches numeric
	// ordering; not required for the cancellation test but
	// keeps the fixture readable in error logs.
	const pad = "000"
	s := pad
	for n, j := i, 0; n > 0 && j < len(pad); n, j = n/10, j+1 {
		s = s[:len(pad)-j-1] + string(rune('0'+n%10)) + s[len(pad)-j:]
	}
	return s + ".go"
}
