package repocontext_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
)

// TestMintRepoID_Deterministic locks in the [arch Sec 1.4 G2]
// stability invariant: two calls to [MintRepoID] in the same
// process for the same root path return identical bytes.
// The e2e scenario [impl-plan Stage 1.2] / "stable repo id"
// asserts the same behaviour across the binary surface.
func TestMintRepoID_Deterministic(t *testing.T) {
	t.Parallel()
	const root = "/tmp/foo"
	first := repocontext.MintRepoID(root)
	second := repocontext.MintRepoID(root)
	if first != second {
		t.Fatalf("MintRepoID(%q) is non-deterministic: %s != %s", root, first, second)
	}
	if first == uuid.Nil {
		t.Fatalf("MintRepoID(%q) returned uuid.Nil", root)
	}
}

// TestMintRepoID_VariantIsV5 asserts the returned UUID's
// version nibble is 5 per RFC 4122. The e2e Phase 1 scenario
// "MintRepoID is deterministic across re-runs" requires the
// variant bits to identify it as RFC 4122 v5.
func TestMintRepoID_VariantIsV5(t *testing.T) {
	t.Parallel()
	id := repocontext.MintRepoID("/tmp/foo")
	// Per RFC 4122, the high nibble of byte 6 encodes the
	// version. For UUID-v5 this nibble MUST be 0x5.
	if got := id[6] >> 4; got != 0x5 {
		t.Fatalf("MintRepoID returned a non-v5 UUID; version nibble = %#x (want 0x5)", got)
	}
	// And the high two bits of byte 8 MUST be 0b10 to mark
	// the RFC 4122 variant.
	if got := id[8] >> 6; got != 0x2 {
		t.Fatalf("MintRepoID returned a non-RFC4122 variant; variant bits = %#x (want 0x2)", got)
	}
}

// TestMintRepoID_GoldenForFixedPath pins the UUID-v5 bytes
// for a fixed root so any future change to the namespace
// prefix or the slash-normalisation breaks loud rather than
// silent. Equivalent to the agent-memory "golden namespace"
// pattern in `services/clean-code/internal/ast/scope/identity_test.go`.
func TestMintRepoID_GoldenForFixedPath(t *testing.T) {
	t.Parallel()
	got := repocontext.MintRepoID("/tmp/foo").String()
	// Recompute via gofrs/uuid directly so the golden is
	// derivable from first principles; mismatch indicates
	// an unintended change to the pre-image construction.
	// The prefix is concatenated to the normalised root,
	// so an absolute POSIX root yields a double slash in
	// the pre-image (`"cleanc.local-repo/" + "/tmp/foo"`
	// = `"cleanc.local-repo//tmp/foo"`). This MUST be the
	// stable shape -- changing it bumps every existing
	// repo's identity.
	const preimage = repocontext.RepoIDNamespaceNamePrefix + "/tmp/foo"
	want := uuid.NewV5(uuid.NamespaceURL, preimage).String()
	if got != want {
		t.Fatalf("MintRepoID(/tmp/foo) = %s; want %s (pre-image %q)", got, want, preimage)
	}
}

// TestMintRepoID_ForwardSlashNormalisation pins the Windows
// path / forward-slash equivalence from e2e Phase 1:
// `C:\Users\dev\repo` and `C:/Users/dev/repo` must yield
// the same UUID byte-for-byte.
func TestMintRepoID_ForwardSlashNormalisation(t *testing.T) {
	t.Parallel()
	// On Linux runners filepath.Clean does NOT rewrite
	// backslash to slash, so the two inputs naturally
	// diverge UNLESS the implementation normalises. We
	// therefore drive both shapes through MintRepoID and
	// require equality on every host OS.
	winLike := repocontext.MintRepoID(`C:\Users\dev\repo`)
	posixLike := repocontext.MintRepoID(`C:/Users/dev/repo`)
	if winLike != posixLike {
		// On non-Windows hosts filepath.Clean treats `\`
		// as a literal byte, so this assertion can only
		// pass when MintRepoID itself normalises. The
		// e2e scenario requires the same behaviour from
		// the host doing the analysis.
		if runtime.GOOS != "windows" {
			t.Logf("non-Windows host: relying on MintRepoID's own normalisation, not filepath.Clean")
		}
		t.Fatalf("MintRepoID Windows / forward-slash normalisation failed:\n  C:\\Users\\dev\\repo  = %s\n  C:/Users/dev/repo    = %s", winLike, posixLike)
	}
	// Both forms also equal the UUID for the result of
	// filepath.ToSlash applied externally.
	external := repocontext.MintRepoID(filepath.ToSlash(`C:\Users\dev\repo`))
	if external != winLike {
		t.Fatalf("MintRepoID disagrees with filepath.ToSlash pre-normalisation: %s vs %s", external, winLike)
	}
}

// TestMintRepoID_TrailingSlashNormalisation ensures a trailing
// separator is collapsed before hashing so `/tmp/foo` and
// `/tmp/foo/` map to the same RepoID.
func TestMintRepoID_TrailingSlashNormalisation(t *testing.T) {
	t.Parallel()
	bare := repocontext.MintRepoID("/tmp/foo")
	withSlash := repocontext.MintRepoID("/tmp/foo/")
	if bare != withSlash {
		t.Fatalf("trailing-slash normalisation failed: %s != %s", bare, withSlash)
	}
}

// TestNormalisePath_HostIndependent locks the canonical
// pre-image bytes for both POSIX and Windows-shaped inputs
// to the exact strings the [MintRepoID] hash MUST see on
// every host OS. It guards against a future regression where
// [NormalisePath] silently delegates to [filepath.Clean] /
// [filepath.ToSlash] -- both of which are no-ops on `\` when
// invoked from a non-Windows host -- and therefore diverges
// from the cross-OS stability invariant in `architecture.md`
// Sec 4.1.
//
// Unlike [TestMintRepoID_ForwardSlashNormalisation] this test
// asserts on the byte string itself (not just the resulting
// UUID), so a future refactor that swaps in a different
// equally-deterministic-but-different normalisation surfaces
// loudly here rather than silently re-minting every repo id.
func TestNormalisePath_HostIndependent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"posix bare", "/tmp/foo", "/tmp/foo"},
		{"posix trailing slash", "/tmp/foo/", "/tmp/foo"},
		{"posix dot segment", "/tmp/./foo", "/tmp/foo"},
		{"posix dotdot segment", "/tmp/foo/../bar", "/tmp/bar"},
		{"windows backslash", `C:\Users\dev\repo`, "C:/Users/dev/repo"},
		{"windows forward slash", "C:/Users/dev/repo", "C:/Users/dev/repo"},
		{"windows trailing backslash", `C:\Users\dev\repo\`, "C:/Users/dev/repo"},
		{"mixed separators", `C:\Users/dev\repo`, "C:/Users/dev/repo"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := repocontext.NormalisePath(tc.in); got != tc.want {
				t.Fatalf("NormalisePath(%q) on %s = %q; want %q",
					tc.in, runtime.GOOS, got, tc.want)
			}
		})
	}
}

// TestDetectHeadSHA_NonGitFallback asserts the
// `("working-copy", false)` shape for a directory without
// `.git`. Mirrors the e2e Phase 1 scenario "HEAD SHA fallback
// to working-copy for non-git roots".
func TestDetectHeadSHA_NonGitFallback(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sha, isGit := repocontext.DetectHeadSHA(dir)
	if sha != repocontext.HeadSHAWorkingCopySentinel {
		t.Fatalf("DetectHeadSHA on non-git dir returned %q; want %q", sha, repocontext.HeadSHAWorkingCopySentinel)
	}
	if isGit {
		t.Fatalf("IsGitRepo flag should be false for a non-git dir")
	}
}

// TestDetectHeadSHA_RealGitRepo asserts the success path for
// a real `git init`-ed directory with one commit. Mirrors
// the e2e Phase 1 scenario "Git working copy yields the real
// HEAD SHA". Skipped when `git` is not on PATH so the unit
// test suite remains runnable on hosts without git installed
// (production CI always has git per repo README).
func TestDetectHeadSHA_RealGitRepo(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed on this host; skipping live-repo test")
	}
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	mustGit(t, dir, "config", "user.email", "test@example.invalid")
	mustGit(t, dir, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	mustGit(t, dir, "add", "README.md")
	mustGit(t, dir, "commit", "-q", "-m", "init")

	sha, isGit := repocontext.DetectHeadSHA(dir)
	if !isGit {
		t.Fatalf("IsGitRepo should be true for an initialised git repo")
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(sha) {
		t.Fatalf("DetectHeadSHA returned %q; want a 40-char hex SHA", sha)
	}
	// Cross-check against `git rev-parse HEAD` directly.
	want := strings.TrimSpace(string(mustGitOut(t, dir, "rev-parse", "HEAD")))
	if sha != want {
		t.Fatalf("DetectHeadSHA = %q; git rev-parse HEAD = %q", sha, want)
	}
}

// TestDetectHeadSHA_GitInitNoCommits exercises the "`.git`
// exists but `git rev-parse HEAD` errors" branch: a freshly
// initialised repo with no commits MUST still report
// IsGitRepo=true (so `--with-churn` is gated correctly) but
// fall back to the working-copy sentinel for the SHA.
func TestDetectHeadSHA_GitInitNoCommits(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed; skipping no-commits test")
	}
	dir := t.TempDir()
	mustGit(t, dir, "init", "-q")
	sha, isGit := repocontext.DetectHeadSHA(dir)
	if !isGit {
		t.Fatalf("IsGitRepo should be true for an initialised repo even with zero commits")
	}
	if sha != repocontext.HeadSHAWorkingCopySentinel {
		t.Fatalf("DetectHeadSHA on no-commits repo returned %q; want %q", sha, repocontext.HeadSHAWorkingCopySentinel)
	}
}

// TestDetectModulePath_Go covers the Go branch: a go.mod
// declaring `module github.com/example/foo` MUST resolve to
// the exact module path.
func TestDetectModulePath_Go(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	contents := "module github.com/example/foo\n\ngo 1.22\n"
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	got := repocontext.DetectModulePath(dir, "go")
	if got != "github.com/example/foo" {
		t.Fatalf("DetectModulePath(go) = %q; want %q", got, "github.com/example/foo")
	}
}

// TestDetectModulePath_TypeScript covers the TS branch via
// a package.json with `name`.
func TestDetectModulePath_TypeScript(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	contents := `{"name": "@example/foo", "version": "1.0.0"}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write package.json: %v", err)
	}
	got := repocontext.DetectModulePath(dir, "typescript")
	if got != "@example/foo" {
		t.Fatalf("DetectModulePath(typescript) = %q; want %q", got, "@example/foo")
	}
}

// TestDetectModulePath_Python covers PEP 621: a `[project]`
// table with `name = "example-foo"` MUST resolve to the
// exact string. Verifies the scanner also skips entries
// under unrelated tables (`[tool.poetry]`).
func TestDetectModulePath_Python(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	contents := "[build-system]\nrequires = [\"setuptools\"]\n\n[project]\nname = \"example-foo\"\nversion = \"0.1.0\"\n\n[tool.poetry]\nname = \"poetry-ghost\"\n"
	if err := os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(contents), 0o644); err != nil {
		t.Fatalf("write pyproject.toml: %v", err)
	}
	got := repocontext.DetectModulePath(dir, "python")
	if got != "example-foo" {
		t.Fatalf("DetectModulePath(python) = %q; want %q", got, "example-foo")
	}
}

// TestDetectModulePath_Java covers the first-`.java`-file
// strategy: the top-level `package x.y.z;` of the earliest
// source file the walker encounters wins.
func TestDetectModulePath_Java(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src", "main", "java", "com", "example", "foo")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	java := "// header comment\npackage com.example.foo;\n\nclass Foo { }\n"
	if err := os.WriteFile(filepath.Join(src, "Foo.java"), []byte(java), 0o644); err != nil {
		t.Fatalf("write Foo.java: %v", err)
	}
	got := repocontext.DetectModulePath(dir, "java")
	if got != "com.example.foo" {
		t.Fatalf("DetectModulePath(java) = %q; want %q", got, "com.example.foo")
	}
}

// TestDetectModulePath_Missing returns the empty string when
// the relevant manifest is absent. The CLI treats an empty
// ModulePath as "skip intra-repo import resolution"; the
// orchestrator MUST NOT panic on missing manifests.
func TestDetectModulePath_Missing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, lang := range []string{"go", "typescript", "python", "java", "ruby"} {
		got := repocontext.DetectModulePath(dir, lang)
		if got != "" {
			t.Errorf("DetectModulePath(%q) on empty dir = %q; want \"\"", lang, got)
		}
	}
}

// TestNormalisePath sanity-checks the helper used by
// downstream `--diagnostics` consumers.
func TestNormalisePath(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"/tmp/foo", "/tmp/foo"},
		{"/tmp/foo/", "/tmp/foo"},
		{"/tmp/foo/../foo", "/tmp/foo"},
	}
	for _, tc := range cases {
		if got := repocontext.NormalisePath(tc.in); got != tc.want {
			t.Errorf("NormalisePath(%q) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func mustGitOut(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}
