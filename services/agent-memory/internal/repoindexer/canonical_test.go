package repoindexer

import (
	"strings"
	"testing"
)

// canonical_test.go pins the EXACT byte output of every canonical-
// signature helper. These helpers were promoted out of worker.go
// so external packages (graphsink backends, the diagram projector)
// can mint identities that match the production Postgres write
// path. Any change to their output shifts the
// `node.canonical_signature` for every node minted by every
// future scan -- which means the same input scanned twice with
// different versions of these functions would split node
// identity. Pinning the bytes here turns that risk into a hard
// test failure rather than a silent re-identification of the
// graph.
//
// The two `Test*_disambiguatePkgAndFile` and `Test*_rootCollapses`
// cases below restate the previous worker_unit_test.go vectors
// against the exported names; the remaining cases add the
// byte-exact golden assertions.

// TestCanonicalRepoSig_bytePinned pins the contract that the
// repo signature is exactly the URL the caller passed in -- no
// canonicalisation, no trailing-slash strip, no lower-casing.
// Callers (worker.runFull, deltaProcessRenamed, the future
// SQLite/memory sinks) rely on this so that a repo registered as
// `https://example.test/repo` and re-scanned with the same URL
// hits the same canonical_signature byte-for-byte.
func TestCanonicalRepoSig_bytePinned(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, in, want string
	}{
		{"https url", "https://example.test/repo", "https://example.test/repo"},
		{"git url", "git@github.com:smartpcr/code-intelligence.git", "git@github.com:smartpcr/code-intelligence.git"},
		{"file url", "file:///tmp/repo", "file:///tmp/repo"},
		{"empty url", "", ""},
		{"trailing slash preserved", "https://example.test/repo/", "https://example.test/repo/"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := CanonicalRepoSig(c.in); got != c.want {
				t.Errorf("CanonicalRepoSig(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestCanonicalPackageDir_rootCollapses is the restated worker
// test: path.Dir's "." sentinel for repo-root files MUST collapse
// to "" so the resulting canonical signature reads
// `<url>::pkg::` rather than `<url>::pkg::.`. The "/" branch
// guards the rooted-input case (`/x`) where path.Dir returns
// `/`; we collapse it the same way so a single-component path
// with or without a leading slash yields the same root package.
func TestCanonicalPackageDir_rootCollapses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want string
	}{
		{"README.md", ""},
		{"a/b.go", "a"},
		{"a/b/c.go", "a/b"},
		// Rooted single-component path: path.Dir returns "/",
		// which the helper collapses to "".
		{"/foo.go", ""},
		// Deeper rooted path: the leading "/" is preserved by
		// path.Dir, so the helper preserves it too.
		{"/a/b.go", "/a"},
		// Empty input: path.Dir("") == ".", collapsed to "".
		{"", ""},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			if got := CanonicalPackageDir(c.in); got != c.want {
				t.Errorf("CanonicalPackageDir(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestCanonicalPackageSig_bytePinned pins the
// `<repo url>::pkg::<dir>` format byte-for-byte. The literal
// `::pkg::` separator is load-bearing: shifting it would
// silently re-identify every Package Node in every scan, and
// the SQLite/memory sinks would no longer match the Postgres
// canonical_signature for the same input.
func TestCanonicalPackageSig_bytePinned(t *testing.T) {
	t.Parallel()
	const url = "https://example.test/repo"
	cases := []struct {
		name, dir, want string
	}{
		{"root pkg", "", "https://example.test/repo::pkg::"},
		{"top level", "pkg", "https://example.test/repo::pkg::pkg"},
		{"nested", "a/b/c", "https://example.test/repo::pkg::a/b/c"},
		{"dir named like file", "foo.go", "https://example.test/repo::pkg::foo.go"},
		{"empty url", "", "::pkg::"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repoURL := url
			if c.name == "empty url" {
				repoURL = ""
			}
			if got := CanonicalPackageSig(repoURL, c.dir); got != c.want {
				t.Errorf("CanonicalPackageSig(%q, %q) = %q, want %q",
					repoURL, c.dir, got, c.want)
			}
		})
	}
}

// TestCanonicalFileSig_bytePinned pins the
// `<repo url>::file::<rel path>` format byte-for-byte. Mirrors
// the package-sig pin above: the `::file::` separator MUST stay
// distinct from `::pkg::` so a directory named `foo.go` cannot
// collide with a file named `foo.go`.
func TestCanonicalFileSig_bytePinned(t *testing.T) {
	t.Parallel()
	const url = "https://example.test/repo"
	cases := []struct {
		name, rel, want string
	}{
		{"root file", "README.md", "https://example.test/repo::file::README.md"},
		{"nested file", "pkg/foo.go", "https://example.test/repo::file::pkg/foo.go"},
		{"deep file", "a/b/c/d.txt", "https://example.test/repo::file::a/b/c/d.txt"},
		{"file with spaces", "docs/My File.md", "https://example.test/repo::file::docs/My File.md"},
		{"empty rel", "", "https://example.test/repo::file::"},
		{"empty url", "", "::file::"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			repoURL := url
			rel := c.rel
			if c.name == "empty url" {
				repoURL = ""
				rel = ""
			}
			if got := CanonicalFileSig(repoURL, rel); got != c.want {
				t.Errorf("CanonicalFileSig(%q, %q) = %q, want %q",
					repoURL, rel, got, c.want)
			}
		})
	}
}

// TestCanonicalSignatures_disambiguatePkgAndFile is the restated
// worker test: `::pkg::` and `::file::` MUST stay distinct so a
// directory and a file with the same path segment cannot collide
// on canonical_signature (which is the dedupe key in the
// `(repo_id, kind, canonical_signature)` graphwriter UNIQUE
// INDEX).
func TestCanonicalSignatures_disambiguatePkgAndFile(t *testing.T) {
	t.Parallel()
	const url = "https://example.test/repo"
	pkgSig := CanonicalPackageSig(url, "foo.go")
	fileSig := CanonicalFileSig(url, "foo.go")
	if pkgSig == fileSig {
		t.Errorf("pkg and file signatures must not collide: %s", pkgSig)
	}
	if !strings.Contains(pkgSig, "::pkg::") {
		t.Errorf("pkg signature missing ::pkg:: separator: %s", pkgSig)
	}
	if !strings.Contains(fileSig, "::file::") {
		t.Errorf("file signature missing ::file:: separator: %s", fileSig)
	}
	// Byte-exact pin so a future edit cannot silently change the
	// separator without tripping a test failure.
	if want := "https://example.test/repo::pkg::foo.go"; pkgSig != want {
		t.Errorf("pkg signature shifted: got %q, want %q", pkgSig, want)
	}
	if want := "https://example.test/repo::file::foo.go"; fileSig != want {
		t.Errorf("file signature shifted: got %q, want %q", fileSig, want)
	}
}

// TestCanonicalHelpers_runFullParity walks the exact call shape
// worker.runFull uses (CanonicalRepoSig at the repo node,
// CanonicalPackageDir + CanonicalPackageSig at each package
// node, CanonicalFileSig at each file node) for a small fixture
// tree and pins every byte output. This is the parity contract
// the SQLite/memory graphsink backends will be tested against:
// a future "scan to SQLite, then scan to Postgres" parity test
// must produce these exact canonical_signature strings for the
// same input.
func TestCanonicalHelpers_runFullParity(t *testing.T) {
	t.Parallel()
	const url = "https://example.test/repo"
	// Repo Node.
	if got, want := CanonicalRepoSig(url), "https://example.test/repo"; got != want {
		t.Errorf("repo: got %q, want %q", got, want)
	}
	// Fixture tree:
	//   README.md                  (root package, root file)
	//   pkg/foo.go                 (pkg package, pkg/foo.go file)
	//   pkg/sub/bar.go             (pkg/sub package, pkg/sub/bar.go file)
	files := []struct {
		rel, wantPkgDir, wantPkgSig, wantFileSig string
	}{
		{
			rel:         "README.md",
			wantPkgDir:  "",
			wantPkgSig:  "https://example.test/repo::pkg::",
			wantFileSig: "https://example.test/repo::file::README.md",
		},
		{
			rel:         "pkg/foo.go",
			wantPkgDir:  "pkg",
			wantPkgSig:  "https://example.test/repo::pkg::pkg",
			wantFileSig: "https://example.test/repo::file::pkg/foo.go",
		},
		{
			rel:         "pkg/sub/bar.go",
			wantPkgDir:  "pkg/sub",
			wantPkgSig:  "https://example.test/repo::pkg::pkg/sub",
			wantFileSig: "https://example.test/repo::file::pkg/sub/bar.go",
		},
	}
	for _, f := range files {
		f := f
		t.Run(f.rel, func(t *testing.T) {
			t.Parallel()
			gotDir := CanonicalPackageDir(f.rel)
			if gotDir != f.wantPkgDir {
				t.Errorf("CanonicalPackageDir(%q) = %q, want %q",
					f.rel, gotDir, f.wantPkgDir)
			}
			if got := CanonicalPackageSig(url, gotDir); got != f.wantPkgSig {
				t.Errorf("CanonicalPackageSig(%q, %q) = %q, want %q",
					url, gotDir, got, f.wantPkgSig)
			}
			if got := CanonicalFileSig(url, f.rel); got != f.wantFileSig {
				t.Errorf("CanonicalFileSig(%q, %q) = %q, want %q",
					url, f.rel, got, f.wantFileSig)
			}
		})
	}
}
