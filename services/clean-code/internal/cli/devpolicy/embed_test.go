// Embed-surface tests for [rulepacks.EmbeddedFS] -- the binary-baked
// rule pack `embed.FS` declared by `services/clean-code/policy/rulepacks/embedded_fs.go`.
// These tests pin the same three contracts the CLI's devpolicy loader
// relies on:
//
//  1. The directive `//go:embed solid/*.yaml decoupling/*.yaml`
//     successfully resolved at build time -- i.e. the YAML files
//     under both family subdirectories are actually reachable
//     through the embedded `fs.FS` at runtime.
//  2. Representative files round-trip through the embed surface
//     and carry the canonical comment-header marker each YAML file
//     in this tree starts with (`# clean-code Stage X.Y`).
//  3. The top level of the embedded FS contains ONLY the two
//     family subdirectories (`solid/`, `decoupling/`); any new
//     top-level entry forces an explicit review of the
//     `//go:embed` pattern in `embedded_fs.go`.
//
// These tests live in `devpolicy` (not `policy/rulepacks`) because
// `devpolicy` is the Stage 1.1 consumer of [rulepacks.EmbeddedFS]
// via the alias in `embed.go`; the embed-surface contract is what
// the loader depends on, so the pinning test belongs next to the
// loader. The tests exercise `rulepacks.EmbeddedFS` directly (not
// the devpolicy alias) so any future refactor of `embed.go` cannot
// silently drift the surface this test guards.
//
// These tests do NOT assert the YAML schema (the per-family
// `solid_test.go` / `decoupling_test.go` files already pin
// the schema). They guard ONLY the embed surface.
package devpolicy

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/policy/rulepacks"
)

// TestEmbeddedFS_ContainsBothFamilies walks the entire embedded
// FS and asserts that at least one `.yaml` file is present under
// each of `solid/` and `decoupling/`. A miss on either side
// means the `//go:embed` pattern silently degraded (e.g.
// someone renamed a directory or moved the file out of the
// rulepacks tree); the build would still succeed but the CLI
// loader would walk an empty source.
func TestEmbeddedFS_ContainsBothFamilies(t *testing.T) {
	t.Parallel()

	var (
		solidYAMLs      []string
		decouplingYAMLs []string
	)
	err := fs.WalkDir(rulepacks.EmbeddedFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		switch {
		case strings.HasPrefix(path, "solid/"):
			solidYAMLs = append(solidYAMLs, path)
		case strings.HasPrefix(path, "decoupling/"):
			decouplingYAMLs = append(decouplingYAMLs, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WalkDir(rulepacks.EmbeddedFS): %v", err)
	}
	if len(solidYAMLs) == 0 {
		t.Errorf("rulepacks.EmbeddedFS contains no solid/*.yaml files (got 0) -- //go:embed pattern likely broken")
	}
	if len(decouplingYAMLs) == 0 {
		t.Errorf("rulepacks.EmbeddedFS contains no decoupling/*.yaml files (got 0) -- //go:embed pattern likely broken")
	}
	t.Logf("rulepacks.EmbeddedFS: %d solid/*.yaml + %d decoupling/*.yaml files reachable",
		len(solidYAMLs), len(decouplingYAMLs))
}

// TestEmbeddedFS_ReadsRepresentativeFiles opens one known file
// from each family subtree, asserts the read succeeds and the
// bytes are non-empty, and asserts the file's canonical
// comment-header marker is present (each YAML file in this tree
// starts with `# clean-code Stage X.Y`). This catches the case
// where the embed directive resolved but the embedded bytes are
// stale / wrong.
func TestEmbeddedFS_ReadsRepresentativeFiles(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path           string
		expectedMarker string
	}{
		{path: "solid/srp.yaml", expectedMarker: "clean-code Stage"},
		{path: "decoupling/cycles.yaml", expectedMarker: "clean-code Stage"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()
			data, err := fs.ReadFile(rulepacks.EmbeddedFS, tc.path)
			if err != nil {
				t.Fatalf("ReadFile(rulepacks.EmbeddedFS, %q): %v", tc.path, err)
			}
			if len(data) == 0 {
				t.Fatalf("ReadFile(rulepacks.EmbeddedFS, %q): returned 0 bytes", tc.path)
			}
			if !strings.Contains(string(data), tc.expectedMarker) {
				t.Errorf("ReadFile(rulepacks.EmbeddedFS, %q): missing canonical marker %q in first 200 bytes: %q",
					tc.path, tc.expectedMarker, firstN(string(data), 200))
			}
		})
	}
}

// TestEmbeddedFS_NoUnexpectedTopLevelEntries asserts that the
// top level of the embedded FS contains ONLY the two family
// subdirectories (`solid/`, `decoupling/`). A new top-level
// entry typically means a stray file was added to
// `services/clean-code/policy/rulepacks/` without updating the
// `//go:embed` pattern -- this test forces an explicit review
// of the embed surface when that happens.
func TestEmbeddedFS_NoUnexpectedTopLevelEntries(t *testing.T) {
	t.Parallel()

	entries, err := fs.ReadDir(rulepacks.EmbeddedFS, ".")
	if err != nil {
		t.Fatalf("ReadDir(rulepacks.EmbeddedFS, %q): %v", ".", err)
	}
	want := map[string]bool{"solid": true, "decoupling": true}
	for _, e := range entries {
		if !want[e.Name()] {
			t.Errorf("rulepacks.EmbeddedFS root contains unexpected entry %q -- review //go:embed pattern in embedded_fs.go", e.Name())
		}
		delete(want, e.Name())
	}
	for missing := range want {
		t.Errorf("rulepacks.EmbeddedFS root missing expected directory %q", missing)
	}
}

// firstN returns the first n bytes of s as a string, without
// panicking on short input. Used only for error messages; not
// part of any public contract.
func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
