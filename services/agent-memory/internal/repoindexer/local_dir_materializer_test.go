package repoindexer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// TestLocalDirMaterializer_nonGitDirYieldsStableMTimeSHA covers
// brief case (a) and implementation-plan scenario
// `local-non-git-sha`: scanning a plain on-disk directory (no
// `.git/`) resolves the SHA to EXACTLY
// `fingerprint.MTimeTreeSHA(rootDir, defaultExcludeDirs)`. The
// equality assertion pins the contract so accidental changes to
// either the exclude set or the hash helper break the test.
func TestLocalDirMaterializer_nonGitDirYieldsStableMTimeSHA(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := writeTextFile(filepath.Join(root, "src", "main.go"), "package x\n"); err != nil {
		t.Fatalf("seed src/main.go: %v", err)
	}
	if err := writeTextFile(filepath.Join(root, "README.md"), "hello\n"); err != nil {
		t.Fatalf("seed README.md: %v", err)
	}

	m := &LocalDirMaterializer{}

	ws1, err := m.Materialize(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Materialize #1: %v", err)
	}
	defer ws1.Close()

	// EXACT equality with the pinned helper output -- this is
	// what implementation-plan.md:310 requires.
	absRoot, _ := filepath.Abs(root)
	wantSHA, err := fingerprint.MTimeTreeSHA(absRoot, defaultExcludeDirs)
	if err != nil {
		t.Fatalf("MTimeTreeSHA: %v", err)
	}
	if ws1.SHA() != wantSHA {
		t.Errorf("SHA: got %q, want fingerprint.MTimeTreeSHA(root, defaultExcludeDirs)=%q",
			ws1.SHA(), wantSHA)
	}
	if len(ws1.SHA()) != 32 {
		t.Errorf("MTimeTreeSHA length: got %d, want 32 (sha=%q)", len(ws1.SHA()), ws1.SHA())
	}

	ws2, err := m.Materialize(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Materialize #2: %v", err)
	}
	defer ws2.Close()
	if ws1.SHA() != ws2.SHA() {
		t.Errorf("SHA must be stable across runs on unchanged tree:\n  #1=%s\n  #2=%s",
			ws1.SHA(), ws2.SHA())
	}

	wantPrefix := "file://"
	if !strings.HasPrefix(ws1.URL(), wantPrefix) {
		t.Errorf("URL prefix: got %q, want prefix %q", ws1.URL(), wantPrefix)
	}
	// URL must use forward slashes regardless of host OS.
	if strings.ContainsRune(ws1.URL(), '\\') {
		t.Errorf("URL must use forward slashes: %q", ws1.URL())
	}
	if runtime.GOOS == "windows" {
		// e.g. file:///c:/...   drive letter must be lowercase.
		// Expect a colon at position 9 (file:/// + 'x' + ':').
		if len(ws1.URL()) < 11 || ws1.URL()[9] != ':' {
			t.Errorf("Windows URL shape unexpected: %q", ws1.URL())
		} else {
			drive := ws1.URL()[8]
			if drive < 'a' || drive > 'z' {
				t.Errorf("Windows drive letter must be lowercase in URL: %q", ws1.URL())
			}
		}
	}
}

// TestLocalDirMaterializer_acceptsFileURLInput pins the
// implementation-plan.md:310 scenario shape `Materialize("file:///path", "")`:
// the materializer MUST accept a `file://` URL as `rootDir`,
// decode it back to a filesystem path, and behave identically to
// the plain-path call.
func TestLocalDirMaterializer_acceptsFileURLInput(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := writeTextFile(filepath.Join(root, "a.go"), "package a\n"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	absRoot, _ := filepath.Abs(root)
	fileURL := synthesizeFileURL(absRoot) // exact shape the CLI will hand us

	m := &LocalDirMaterializer{}
	ws, err := m.Materialize(context.Background(), fileURL, "")
	if err != nil {
		t.Fatalf("Materialize(file://): %v", err)
	}
	defer ws.Close()

	// Walk must surface the same single file -- proof the URL
	// was decoded and rooted at the right directory.
	var got []string
	if err := ws.Walk(func(f WalkFile) error {
		got = append(got, f.RelPath)
		return nil
	}); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !sliceEqual(got, []string{"a.go"}) {
		t.Errorf("Walk on file:// URL got %v, want [a.go]", got)
	}

	// And the synthesised URL on the workspace round-trips:
	// scanning via `file://X` produces a workspace whose URL()
	// equals synthesizeFileURL(X) -- decoding was lossless.
	if ws.URL() != fileURL {
		t.Errorf("URL round-trip: got %q, want %q", ws.URL(), fileURL)
	}
}

// TestLocalDirMaterializer_gitDirYieldsRevParseHEAD covers brief
// case (b): when `.git/` is present the materializer invokes
// `git rev-parse HEAD` and adopts its output as the SHA. We
// inject `runGitCmd` so the test doesn't need a real git binary
// or a real repository.
func TestLocalDirMaterializer_gitDirYieldsRevParseHEAD(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// `.git/` must exist (as a directory) for the materializer
	// to take the git-rev-parse branch.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := writeTextFile(filepath.Join(root, "main.go"), "package x\n"); err != nil {
		t.Fatalf("seed main.go: %v", err)
	}

	const headSHA = "0123456789abcdef0123456789abcdef01234567"
	var seenDir, seenBin string
	var seenArgs []string
	m := &LocalDirMaterializer{
		GitBinary: "git-stub",
		runGitCmd: func(_ context.Context, dir, bin string, args ...string) (string, error) {
			seenDir = dir
			seenBin = bin
			seenArgs = append([]string(nil), args...)
			// Mimic real git output which includes a trailing newline.
			return headSHA + "\n", nil
		},
	}

	ws, err := m.Materialize(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	defer ws.Close()
	if ws.SHA() != headSHA {
		t.Errorf("SHA: got %q, want %q", ws.SHA(), headSHA)
	}
	wantArgs := []string{"rev-parse", "HEAD"}
	if !sliceEqual(seenArgs, wantArgs) {
		t.Errorf("runGitCmd args: got %v, want %v", seenArgs, wantArgs)
	}
	if seenBin != "git-stub" {
		t.Errorf("runGitCmd bin: got %q, want git-stub", seenBin)
	}
	// The git command must run inside the local dir (resolved to
	// absolute) so `rev-parse HEAD` targets the right repo.
	absRoot, _ := filepath.Abs(root)
	if seenDir != absRoot {
		t.Errorf("runGitCmd dir: got %q, want %q", seenDir, absRoot)
	}
}

// TestLocalDirMaterializer_gitFileTriggersRevParse covers the
// linked-worktree / submodule case: `.git` may be a FILE (a
// `gitdir:` pointer) rather than a directory. The materializer
// must still invoke `git rev-parse HEAD` instead of falling
// through to the mtime fingerprint, since git itself understands
// both forms.
func TestLocalDirMaterializer_gitFileTriggersRevParse(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Worktree-style `.git` is a regular file containing a
	// `gitdir:` pointer. Content is not parsed by the
	// materializer -- existence + non-dir is enough.
	if err := os.WriteFile(filepath.Join(root, ".git"),
		[]byte("gitdir: /elsewhere/.git/worktrees/wt\n"), 0o644); err != nil {
		t.Fatalf("write .git file: %v", err)
	}
	if err := writeTextFile(filepath.Join(root, "main.go"), "package x\n"); err != nil {
		t.Fatalf("seed main.go: %v", err)
	}

	const headSHA = "abcdef0123456789abcdef0123456789abcdef01"
	called := false
	m := &LocalDirMaterializer{
		runGitCmd: func(_ context.Context, _, _ string, _ ...string) (string, error) {
			called = true
			return headSHA + "\n", nil
		},
	}
	ws, err := m.Materialize(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	defer ws.Close()
	if !called {
		t.Fatal("git rev-parse HEAD must be invoked when .git is a file (linked worktree); was not called")
	}
	if ws.SHA() != headSHA {
		t.Errorf("SHA: got %q, want %q", ws.SHA(), headSHA)
	}
}

// TestLocalDirMaterializer_operatorSuppliedSHAOverrides covers
// brief case (c): a non-empty `sha` argument wins over both git
// rev-parse and the MTimeTreeSHA fallback. The injected
// `runGitCmd` would fail loudly if invoked -- proving the
// override short-circuits SHA synthesis entirely.
func TestLocalDirMaterializer_operatorSuppliedSHAOverrides(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Seed BOTH a `.git/` (so the git branch would normally fire)
	// AND a file (so the mtime branch would also have content).
	// The override must bypass both.
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("mkdir .git: %v", err)
	}
	if err := writeTextFile(filepath.Join(root, "main.go"), "package x\n"); err != nil {
		t.Fatalf("seed main.go: %v", err)
	}
	m := &LocalDirMaterializer{
		runGitCmd: func(_ context.Context, _, _ string, _ ...string) (string, error) {
			t.Fatalf("runGitCmd must not be invoked when sha is overridden")
			return "", nil
		},
	}
	const override = "feedfacecafebabefeedfacecafebabefeedface"
	ws, err := m.Materialize(context.Background(), root, override)
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	defer ws.Close()
	if ws.SHA() != override {
		t.Errorf("SHA: got %q, want override %q", ws.SHA(), override)
	}
}

// TestLocalDirMaterializer_walkSkipsDefaultExcludeDirs covers
// brief case (d): the workspace's Walk respects
// `defaultExcludeDirs` (same set the GitMaterializer uses) so
// language-irrelevant trees (.git, node_modules, vendor, target,
// __pycache__, ...) don't leak into the AST dispatcher.
func TestLocalDirMaterializer_walkSkipsDefaultExcludeDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// One file inside each default-excluded dir + two real ones.
	excluded := []string{
		".git/HEAD",
		".hg/store",
		".svn/entries",
		"node_modules/leftpad/index.js",
		"vendor/lib/v.go",
		"target/debug/x.o",
		"bin/app",
		"obj/Debug/x.dll",
		"__pycache__/m.pyc",
		".venv/lib/python.so",
		".tox/py3/x",
	}
	for _, rel := range excluded {
		if err := writeTextFile(filepath.Join(root, filepath.FromSlash(rel)), "x"); err != nil {
			t.Fatalf("seed %s: %v", rel, err)
		}
	}
	if err := writeTextFile(filepath.Join(root, "README.md"), "ok"); err != nil {
		t.Fatalf("seed README.md: %v", err)
	}
	if err := writeTextFile(filepath.Join(root, "src", "main.go"), "package x\n"); err != nil {
		t.Fatalf("seed src/main.go: %v", err)
	}

	m := &LocalDirMaterializer{}
	ws, err := m.Materialize(context.Background(), root, "operator-sha")
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
	want := []string{"README.md", "src/main.go"}
	if !sliceEqual(got, want) {
		t.Errorf("Walk visited %v, want %v (default excludes must be skipped)", got, want)
	}
}

// TestLocalDirMaterializer_closeDoesNotDeleteRoot pins the
// safety property: Close on a localDirWorkspace must NOT remove
// the user's source tree. Bug-of-omission risk is high because
// the type embeds gitWorkspace whose Close DOES delete.
func TestLocalDirMaterializer_closeDoesNotDeleteRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	keep := filepath.Join(root, "keep.txt")
	if err := writeTextFile(keep, "still here"); err != nil {
		t.Fatalf("seed keep.txt: %v", err)
	}
	m := &LocalDirMaterializer{}
	ws, err := m.Materialize(context.Background(), root, "sha")
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("source file gone after Close: %v -- LocalDirMaterializer must not delete user dir", err)
	}
	// Idempotent.
	if err := ws.Close(); err != nil {
		t.Errorf("second Close: %v (want nil)", err)
	}
}

// TestLocalDirMaterializer_emptyRootDirRejected guards the
// "no rootDir" boundary; without it a CLI typo would silently
// scan the current working directory.
func TestLocalDirMaterializer_emptyRootDirRejected(t *testing.T) {
	t.Parallel()
	m := &LocalDirMaterializer{}
	_, err := m.Materialize(context.Background(), "", "sha")
	if err == nil {
		t.Fatal("expected error for empty rootDir; got nil")
	}
	if !strings.Contains(err.Error(), "empty rootDir") {
		t.Errorf("error message: %v", err)
	}
}

// TestLocalDirMaterializer_notADirectoryRejected covers the
// other input-validation boundary: pointing the materializer at
// a regular file must fail fast with a clear message rather than
// degrading to an empty walk.
func TestLocalDirMaterializer_notADirectoryRejected(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	if err := writeTextFile(file, "x"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	m := &LocalDirMaterializer{}
	_, err := m.Materialize(context.Background(), file, "sha")
	if err == nil {
		t.Fatal("expected error for non-directory rootDir; got nil")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("error message: %v", err)
	}
}

// TestDecodeFileURL_roundTrips covers the small URL <-> path
// helper directly so we catch shape regressions independent of
// the full Materialize path. Covers POSIX, Windows, and
// passthrough-of-plain-paths.
func TestDecodeFileURL_roundTrips(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		input string
		want  string
		onOS  string // "" for all
	}{
		{name: "plain-passthrough-posix", input: "/foo/bar", want: "/foo/bar"},
		{name: "plain-passthrough-windows", input: `C:\code\repo`, want: `C:\code\repo`},
		{name: "posix-file-url", input: "file:///foo/bar", want: "/foo/bar", onOS: "linux"},
		{name: "windows-file-url", input: "file:///c:/code/repo", want: "c:/code/repo", onOS: "windows"},
		{name: "windows-uppercase-drive", input: "file:///C:/code/repo", want: "C:/code/repo", onOS: "windows"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.onOS != "" && runtime.GOOS != tc.onOS {
				t.Skipf("os-specific: %s", tc.onOS)
			}
			got, err := decodeFileURL(tc.input)
			if err != nil {
				t.Fatalf("decodeFileURL(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("decodeFileURL(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestDecodeFileURL_rejectsRemoteHost guards against the
// `file://server/share/path` shape silently being treated as a
// local path. UNC-style remote file URLs are out of scope for
// the local-dir materializer.
func TestDecodeFileURL_rejectsRemoteHost(t *testing.T) {
	t.Parallel()
	_, err := decodeFileURL("file://remote-host/share/repo")
	if err == nil {
		t.Fatal("expected error for remote host in file URL; got nil")
	}
}
