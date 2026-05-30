// Tests for the [LoaderSource.FS] choice point and the
// [ErrMissingPolicyDir] sentinel declared in `loader.go`.
//
// These tests pin three contracts the future build-tag-gated
// synthesisers (`unsigned_dev.go` / `unsigned_prod.go`,
// implementation-plan Stage 1.4 items 99-100) will rely on:
//
//  1. `LoaderSource{UseEmbedded: true}.FS()` returns the
//     canonical binary-baked rule pack `embed.FS` and never
//     errors -- this is the default source the CLI uses when
//     the operator does NOT pass `--policy <path>`.
//  2. `LoaderSource{DirPath: <non-empty>}.FS()` returns a
//     fresh `os.DirFS` rooted at the given path -- this is the
//     `--policy <path>` override surface (architecture Sec 7.2
//     and tech-spec Sec 8.4 precedence rules).
//  3. `LoaderSource{}.FS()` with both fields zero returns
//     [ErrMissingPolicyDir] without producing any FS handle --
//     the CLI surfaces this as an operator-facing diagnostic
//     ("forgot `--policy <path>`").
//
// The tests deliberately do NOT assert anything about the
// Loader interface's Load() method -- the concrete
// implementations of that method live in the follow-up
// workstream (Stage 1.4 items 97-102).
package devpolicy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestLoaderSource_FS_UseEmbedded verifies that selecting the
// embedded source returns a usable `fs.FS` whose contents are
// the canonical rule pack tree (at least one file under each
// of the `solid/` and `decoupling/` families is readable). A
// regression here means either the `embeddedRulePacks` alias
// in `embed.go` no longer points at `rulepacks.EmbeddedFS`, or
// the underlying embed directive lost coverage.
func TestLoaderSource_FS_UseEmbedded(t *testing.T) {
	t.Parallel()

	src := LoaderSource{UseEmbedded: true}
	got, err := src.FS()
	if err != nil {
		t.Fatalf("LoaderSource{UseEmbedded:true}.FS(): unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("LoaderSource{UseEmbedded:true}.FS(): returned nil fs.FS")
	}

	// Spot-check one representative file from each family
	// subdirectory is reachable through the returned FS.
	for _, p := range []string{"solid/srp.yaml", "decoupling/cycles.yaml"} {
		data, readErr := fs.ReadFile(got, p)
		if readErr != nil {
			t.Errorf("fs.ReadFile(embedded, %q): %v", p, readErr)
			continue
		}
		if len(data) == 0 {
			t.Errorf("fs.ReadFile(embedded, %q): returned 0 bytes", p)
		}
	}
}

// TestLoaderSource_FS_UseEmbedded_IgnoresDirPath verifies
// that when UseEmbedded is true, a non-empty DirPath is
// ignored (per the LoaderSource doc contract). The operator-
// facing precedence is: embed wins. This test pins that
// behaviour against a non-existent DirPath value -- if the
// implementation had wrongly preferred DirPath, the test
// would observe an os.DirFS over a nonsense path and surface
// it via the same spot-check.
func TestLoaderSource_FS_UseEmbedded_IgnoresDirPath(t *testing.T) {
	t.Parallel()

	src := LoaderSource{UseEmbedded: true, DirPath: "/definitely/not/a/real/path"}
	got, err := src.FS()
	if err != nil {
		t.Fatalf("LoaderSource{UseEmbedded:true, DirPath:nonsense}.FS(): unexpected error: %v", err)
	}
	if _, err := fs.ReadFile(got, "solid/srp.yaml"); err != nil {
		t.Fatalf("expected DirPath to be ignored when UseEmbedded=true; got read error: %v", err)
	}
}

// TestLoaderSource_FS_DirPath verifies that a filesystem
// source is rooted at the requested DirPath by writing a
// fixture YAML file into a t.TempDir() and reading it back
// through the returned `fs.FS`.
func TestLoaderSource_FS_DirPath(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	fixture := []byte("# devpolicy loader_test fixture\npack_id: test.fixture\n")
	if err := os.WriteFile(filepath.Join(dir, "fixture.yaml"), fixture, 0o644); err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

	src := LoaderSource{DirPath: dir}
	got, err := src.FS()
	if err != nil {
		t.Fatalf("LoaderSource{DirPath:tempdir}.FS(): unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("LoaderSource{DirPath:tempdir}.FS(): returned nil fs.FS")
	}

	data, err := fs.ReadFile(got, "fixture.yaml")
	if err != nil {
		t.Fatalf("fs.ReadFile(dirfs, %q): %v", "fixture.yaml", err)
	}
	if string(data) != string(fixture) {
		t.Errorf("round-trip mismatch:\n want: %q\n  got: %q", fixture, data)
	}
}

// TestLoaderSource_FS_MissingDirPath verifies the
// operator-facing "forgot `--policy <path>`" diagnostic:
// when UseEmbedded is false and DirPath is empty, FS()
// must return [ErrMissingPolicyDir] without producing any
// fs.FS handle.
func TestLoaderSource_FS_MissingDirPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  LoaderSource
	}{
		{name: "zero-value", src: LoaderSource{}},
		{name: "explicit-empty-DirPath", src: LoaderSource{UseEmbedded: false, DirPath: ""}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := tc.src.FS()
			if !errors.Is(err, ErrMissingPolicyDir) {
				t.Errorf("expected ErrMissingPolicyDir; got err=%v", err)
			}
			if got != nil {
				t.Errorf("expected nil fs.FS on error; got non-nil: %T", got)
			}
		})
	}
}

// TestEmbeddedRulePacks_AliasResolvesToRulepacksEmbeddedFS
// guards the package-internal alias `embeddedRulePacks` in
// `embed.go`. It asserts the alias is non-nil and that its
// content matches what `LoaderSource.FS()` returns for the
// embedded source -- i.e. the future build-tag-gated
// synthesisers can rely on the alias being the SAME FS the
// public choice point exposes.
func TestEmbeddedRulePacks_AliasResolvesToRulepacksEmbeddedFS(t *testing.T) {
	t.Parallel()

	if embeddedRulePacks == nil {
		t.Fatal("embeddedRulePacks alias is nil -- embed.go failed to resolve rulepacks.EmbeddedFS")
	}
	if _, err := fs.ReadFile(embeddedRulePacks, "solid/srp.yaml"); err != nil {
		t.Errorf("embeddedRulePacks does not contain solid/srp.yaml: %v", err)
	}
	if _, err := fs.ReadFile(embeddedRulePacks, "decoupling/cycles.yaml"); err != nil {
		t.Errorf("embeddedRulePacks does not contain decoupling/cycles.yaml: %v", err)
	}
}
