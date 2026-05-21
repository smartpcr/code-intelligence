package storage

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// SchemaName is the single PostgreSQL schema the CLEAN-CODE
// service owns (tech-spec C9 / Sec 8.1.3). Every Catalog,
// Lifecycle, Measurement, Policy, Audit, and Refactor table
// lives under this name. Keeping it as a constant (rather than
// re-deriving it from a config field) means a `grep -rnF
// "clean_code"` finds every reference in the Go code with one
// pass.
const SchemaName = "clean_code"

// MigrationDirEnv is the env var name that tests and CLI helpers
// can set to override the migration directory location (e.g. when
// running from a temp directory). Empty by default; the
// `MigrationDir()` helper falls back to walking up from the
// caller's source file when this is unset.
const MigrationDirEnv = "CLEAN_CODE_MIGRATIONS_DIR"

// Migration is a single discovered migration file pair. Each
// stage emits one `<version>_<name>.up.sql` and one matching
// `<version>_<name>.down.sql`; the loader requires both halves
// be present so an operator running `make migrate-down` after
// `make migrate-up` lands in a deterministic empty-schema state.
type Migration struct {
	// Version is the numeric (plus optional letter-suffix) prefix
	// of the filename, e.g. "0001" or "0006a". Lexicographic sort
	// on Version defines apply order; the leading zero pad makes
	// "0006a" sort after "0006" by plain string comparison.
	Version string
	// Name is the human-readable suffix after the version prefix,
	// e.g. "catalog_lifecycle". Stripped of the ".up.sql" or
	// ".down.sql" extension.
	Name string
	// UpPath is the absolute path to the `.up.sql` file.
	UpPath string
	// DownPath is the absolute path to the `.down.sql` file.
	DownPath string
}

// versionRe matches the leading numeric (plus optional letter
// suffix) of a migration filename. "0001_catalog_lifecycle.up.sql"
// -> ("0001", "catalog_lifecycle", "up"). The letter suffix lets
// us slip an extra migration between two already-deployed ones
// without re-numbering everything downstream, matching the
// agent-memory convention.
var versionRe = regexp.MustCompile(`^(\d+[a-z]?)_(.+)\.(up|down)\.sql$`)

// DiscoverMigrations walks `dir` and returns the ordered list of
// Migrations whose `.up.sql` and `.down.sql` halves are both
// present. An error is returned if any version has an orphan
// half (an up without a down, or vice versa) so an incomplete
// migration pair fails fast instead of silently skipping its
// down half during a reset.
func DiscoverMigrations(dir string) ([]Migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("storage: read %s: %w", dir, err)
	}
	type halves struct {
		name string
		up   string
		down string
	}
	byVersion := map[string]*halves{}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		matches := versionRe.FindStringSubmatch(ent.Name())
		if matches == nil {
			continue
		}
		version, name, half := matches[1], matches[2], matches[3]
		h, ok := byVersion[version]
		if !ok {
			h = &halves{name: name}
			byVersion[version] = h
		}
		if h.name != name {
			return nil, fmt.Errorf(
				"storage: version %q has mismatched names %q vs %q "+
					"(both halves must share the same name)",
				version, h.name, name,
			)
		}
		abs := filepath.Join(dir, ent.Name())
		switch half {
		case "up":
			if h.up != "" {
				return nil, fmt.Errorf(
					"storage: version %q has two .up.sql files: %s and %s",
					version, h.up, abs,
				)
			}
			h.up = abs
		case "down":
			if h.down != "" {
				return nil, fmt.Errorf(
					"storage: version %q has two .down.sql files: %s and %s",
					version, h.down, abs,
				)
			}
			h.down = abs
		}
	}
	out := make([]Migration, 0, len(byVersion))
	for version, h := range byVersion {
		if h.up == "" {
			return nil, fmt.Errorf(
				"storage: version %q has a .down.sql but no .up.sql",
				version,
			)
		}
		if h.down == "" {
			return nil, fmt.Errorf(
				"storage: version %q has a .up.sql but no .down.sql "+
					"(every migration MUST be reversible -- "+
					"implementation-plan Stage 1.2 catalog-up-down scenario)",
				version,
			)
		}
		out = append(out, Migration{
			Version:  version,
			Name:     h.name,
			UpPath:   h.up,
			DownPath: h.down,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// MigrationDir returns the absolute path to the service's
// `migrations/` directory.
//
// Resolution order:
//  1. If `CLEAN_CODE_MIGRATIONS_DIR` is set in the environment,
//     return its value (after `filepath.Abs`) unchanged.
//  2. Otherwise walk upward from `startDir` looking for a
//     directory that contains a `migrations` subdirectory AND a
//     `go.mod` file (the service root). This makes the helper
//     work both from test files under `internal/storage/...` and
//     from arbitrary cwds.
//
// An error is returned when neither path resolves.
func MigrationDir(startDir string) (string, error) {
	if override := strings.TrimSpace(os.Getenv(MigrationDirEnv)); override != "" {
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", fmt.Errorf("storage: %s=%q: %w", MigrationDirEnv, override, err)
		}
		return abs, nil
	}
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", fmt.Errorf("storage: abs %q: %w", startDir, err)
	}
	for {
		_, modErr := os.Stat(filepath.Join(dir, "go.mod"))
		migDir := filepath.Join(dir, "migrations")
		migInfo, migErr := os.Stat(migDir)
		if modErr == nil && migErr == nil && migInfo.IsDir() {
			return migDir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf(
				"storage: could not locate go.mod + migrations/ "+
					"by walking up from %q (set %s to override)",
				startDir, MigrationDirEnv,
			)
		}
		dir = parent
	}
}
