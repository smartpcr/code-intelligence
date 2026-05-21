package storage

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// envTestPGURL is the libpq DSN the round-trip test connects to
// when present. Matches the canonical `CLEAN_CODE_PG_URL` env
// var the Makefile and `e2e-scenarios.md` already use, so a
// single export wires up `make test` + the round-trip helper.
// When unset, the round-trip subtests skip cleanly so a
// developer laptop without a postgres handle still gets `go test
// ./...` exit 0.
const envTestPGURL = "CLEAN_CODE_PG_URL"

// roundTripTimeout caps any single psql invocation. Bumped to
// 30s because the CASCADE drop on a freshly-populated schema can
// briefly hit autovacuum contention on cold CI runners.
const roundTripTimeout = 30 * time.Second

// callerDir returns the directory of THIS test file, computed
// from runtime.Caller. We use it to resolve the service-root
// `migrations/` directory without depending on the test runner's
// cwd (which differs between `go test ./...` invoked from the
// service root vs from the repo root).
func callerDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed; cannot locate test file")
	}
	return filepath.Dir(file)
}

// TestMigrationDir_resolvesRelativeToServiceRoot is a structural
// guard: the helper must find the same `migrations/` directory
// regardless of which sub-package the test runs from. Without
// this, a future caller in `internal/whatever/` could silently
// resolve to a sibling `migrations/` and apply the wrong DDL.
func TestMigrationDir_resolvesRelativeToServiceRoot(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	// The resolved dir must end in `/migrations` (or `\migrations`
	// on Windows) and its parent must contain go.mod.
	if filepath.Base(dir) != "migrations" {
		t.Errorf("MigrationDir base = %q, want %q", filepath.Base(dir), "migrations")
	}
	parent := filepath.Dir(dir)
	if _, err := os.Stat(filepath.Join(parent, "go.mod")); err != nil {
		t.Errorf("MigrationDir parent %q does not contain go.mod: %v", parent, err)
	}
}

// TestMigrationDir_honoursOverrideEnv verifies operators can
// re-target the helper via CLEAN_CODE_MIGRATIONS_DIR. The
// docker compose harness uses this to mount alternate migration
// fixtures in tests that need a partially-migrated schema.
func TestMigrationDir_honoursOverrideEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(MigrationDirEnv, dir)
	got, err := MigrationDir("/should/not/matter")
	if err != nil {
		t.Fatalf("MigrationDir override: %v", err)
	}
	wantAbs, _ := filepath.Abs(dir)
	if got != wantAbs {
		t.Errorf("MigrationDir override = %q, want %q", got, wantAbs)
	}
}

// TestDiscoverMigrations_findsStage12Pair is the load-bearing
// assertion for the Stage 1.2 implementation step "Add
// migrations/0001_catalog_lifecycle.up.sql ... and matching
// 0001_catalog_lifecycle.down.sql". A test failure here means
// the directory layout regressed below the structural minimum
// the Stage 1.2 brief locks in.
func TestDiscoverMigrations_findsStage12Pair(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	migs, err := DiscoverMigrations(dir)
	if err != nil {
		t.Fatalf("DiscoverMigrations(%q): %v", dir, err)
	}
	if len(migs) == 0 {
		t.Fatalf("DiscoverMigrations(%q) returned zero migrations; "+
			"expected at least the Stage 1.2 catalog_lifecycle pair", dir)
	}
	first := migs[0]
	if first.Version != "0001" {
		t.Errorf("first.Version = %q, want %q", first.Version, "0001")
	}
	if first.Name != "catalog_lifecycle" {
		t.Errorf("first.Name = %q, want %q", first.Name, "catalog_lifecycle")
	}
	for _, m := range migs {
		if _, err := os.Stat(m.UpPath); err != nil {
			t.Errorf("up path %q: %v", m.UpPath, err)
		}
		if _, err := os.Stat(m.DownPath); err != nil {
			t.Errorf("down path %q: %v", m.DownPath, err)
		}
	}
}

// TestCatalogUpSQLBodyMentionsExpectedObjects sanity-checks that
// the .up.sql file the round-trip test will apply actually
// declares the five Catalog/Lifecycle tables + canonical ENUM
// label sets. This is intentionally a string-match test: a
// failure here surfaces drift in the SQL files themselves
// (e.g. someone renamed `commit_scan_status` to
// `commit_status`) without needing a live PostgreSQL handle,
// so the regression is caught on every laptop `go test ./...`
// pass, not just in CI's integration job.
func TestCatalogUpSQLBodyMentionsExpectedObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0001_catalog_lifecycle.up.sql"))
	if err != nil {
		t.Fatalf("read up.sql: %v", err)
	}
	// Normalise CRLF -> LF so the substring + multi-line label-set
	// matchers below behave identically on a Windows checkout
	// (where the file may carry CRLF until git's `text eol=lf`
	// normalisation runs) and a Linux checkout.
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	wantSubstrs := []string{
		// Schema.
		"CREATE SCHEMA IF NOT EXISTS clean_code",
		// ENUM declarations.
		"CREATE TYPE clean_code.repo_mode",
		"CREATE TYPE clean_code.commit_scan_status",
		"CREATE TYPE clean_code.metric_kind_tier",
		"CREATE TYPE clean_code.repo_event_kind",
		"CREATE TYPE clean_code.scan_run_kind",
		"CREATE TYPE clean_code.scan_run_sha_binding",
		"CREATE TYPE clean_code.scan_run_status",
		// Tables.
		"CREATE TABLE clean_code.repo",
		"CREATE TABLE clean_code.commit",
		"CREATE TABLE clean_code.metric_kind",
		"CREATE TABLE clean_code.repo_event",
		"CREATE TABLE clean_code.scan_run",
		// Commit.scan_status pin: DEFAULT 'pending' per arch Sec 5.1.2.
		"scan_status  clean_code.commit_scan_status NOT NULL DEFAULT 'pending'",
		// Sole-writer COMMENT on Commit.scan_status (Metric Ingestor).
		"Metric Ingestor is the SOLE writer",
		// Required indexes (implementation-plan Stage 1.2 indexes step).
		"repo_default_branch_head_idx",
		"scan_run_repo_started_idx",
		"repo_event_repo_created_idx",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(sql, want) {
			t.Errorf("up.sql missing required substring %q", want)
		}
	}
	// Canonical enum label sets (architecture Sec 5.1.2 / Sec 5.7,
	// e2e-scenarios "ScanRun and Commit status enums are tight").
	wantLabelSets := []string{
		"'pending',\n    'scanning',\n    'scanned',\n    'failed'",
		"'running',\n    'succeeded',\n    'failed'",
		"'registered',\n    'retired',\n    'retract_intent',\n    'mode_changed'",
		"'full',\n    'delta',\n    'external_single',\n    'external_per_row',\n    'retract'",
		"'single',\n    'per_row'",
	}
	for _, want := range wantLabelSets {
		if !strings.Contains(sql, want) {
			t.Errorf("up.sql missing canonical enum label set:\n----\n%s\n----", want)
		}
	}
}

// TestCatalogDownSQLDropsSchemaCascade verifies the down half
// implements the Stage 1.2 "drops the `clean_code` schema cleanly
// so local-dev resets are deterministic" contract. A `DROP SCHEMA
// ... CASCADE` is the minimal correct shape; this test pins it so
// a refactor that breaks it (e.g. switching to per-object DROPs
// that miss a type) gets caught immediately.
func TestCatalogDownSQLDropsSchemaCascade(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0001_catalog_lifecycle.down.sql"))
	if err != nil {
		t.Fatalf("read down.sql: %v", err)
	}
	// See TestCatalogUpSQLBodyMentionsExpectedObjects for the
	// CRLF-vs-LF rationale.
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	if !strings.Contains(sql, "DROP SCHEMA IF EXISTS clean_code CASCADE") {
		t.Errorf("down.sql missing `DROP SCHEMA IF EXISTS clean_code CASCADE`")
	}
}

// TestRoundTrip_upDownLeavesSchemaEmpty is the integration test
// for implementation-plan Stage 1.2 scenario "catalog-up-down":
//
//	Given an empty PostgreSQL 16 instance
//	When `make migrate-up` then `make migrate-down` runs
//	Then both succeed and `\dt clean_code.*` returns zero rows
//	after the second run.
//
// The test also folds in the three other Stage 1.2 test
// scenarios that require a live PostgreSQL handle:
//
//   - scan-status-enum-rejects-invalid
//     (`commit.scan_status='garbage'` / `='complete'` fail)
//   - scan-status-default-pending
//     (INSERT without scan_status materialises 'pending')
//   - scan-run-status-enum
//     (`scan_run.status='orphaned'` / `='complete'` fail)
//
// Running them inside the same up/down round-trip keeps the live
// PostgreSQL provisioning cost in CI down to a single test
// invocation (vs four separate ones).
//
// Skip / fail policy: a missing `CLEAN_CODE_PG_URL` env var is a
// developer-laptop scenario (no live PG handle available) and
// SKIPs cleanly. A SET env var with a missing `psql` binary is a
// misconfigured CI image and FAILs the test loudly -- this catches
// the silent-skip risk where an integration job appears green
// without ever exercising migrations.
func TestRoundTrip_upDownLeavesSchemaEmpty(t *testing.T) {
	url := strings.TrimSpace(os.Getenv(envTestPGURL))
	if url == "" {
		t.Skipf("skipping: %s is unset; round-trip requires a live PostgreSQL", envTestPGURL)
	}
	psql, err := exec.LookPath("psql")
	if err != nil {
		t.Fatalf("%s is set but `psql` is not on PATH: %v -- a misconfigured "+
			"CI image must NOT silently skip this test", envTestPGURL, err)
	}
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	migs, err := DiscoverMigrations(dir)
	if err != nil {
		t.Fatalf("DiscoverMigrations: %v", err)
	}
	if len(migs) == 0 {
		t.Fatal("DiscoverMigrations returned zero migrations")
	}
	// Pre-condition: make sure the schema is gone (a previous
	// failed test run could have left it half-built).
	if err := runPSQLInline(t, psql, url, "DROP SCHEMA IF EXISTS clean_code CASCADE;"); err != nil {
		t.Fatalf("precondition cleanup: %v", err)
	}

	// Apply up in lexical order. A failure mid-pass leaves the
	// schema half-built; the test cleanup below drops whatever
	// the up half managed to create.
	t.Cleanup(func() {
		_ = runPSQLInline(t, psql, url, "DROP SCHEMA IF EXISTS clean_code CASCADE;")
	})
	for _, m := range migs {
		if err := runPSQLFile(t, psql, url, m.UpPath); err != nil {
			t.Fatalf("migrate-up %s: %v", m.UpPath, err)
		}
	}
	// Sanity: after migrate-up at least one table must exist in
	// the clean_code schema. Guards against a no-op up file that
	// silently passes the down half too.
	if got, err := countCleanCodeTables(t, psql, url); err != nil {
		t.Fatalf("count after up: %v", err)
	} else if got == 0 {
		t.Fatalf("after migrate-up: clean_code schema has zero tables; want >= 1")
	}

	// Run the remaining three Stage 1.2 test scenarios while the
	// schema is live. Each is wrapped in its own subtest so a
	// single regression names the offending scenario by ID.
	t.Run("scan-status-default-pending", func(t *testing.T) {
		assertCommitScanStatusDefaultsPending(t, psql, url)
	})
	t.Run("scan-status-enum-rejects-invalid", func(t *testing.T) {
		assertCommitScanStatusEnumRejects(t, psql, url, "garbage")
		assertCommitScanStatusEnumRejects(t, psql, url, "complete")
	})
	t.Run("scan-run-status-enum", func(t *testing.T) {
		assertScanRunStatusEnumRejects(t, psql, url, "orphaned")
		assertScanRunStatusEnumRejects(t, psql, url, "complete")
	})
	t.Run("primary-keys-match-spec", func(t *testing.T) {
		assertPrimaryKeysMatchSpec(t, psql, url)
	})
	t.Run("foreign-keys-match-spec", func(t *testing.T) {
		assertForeignKeysMatchSpec(t, psql, url)
	})

	// Revert in reverse lexical order. Per the Makefile
	// `migrate-down` target convention (`ls *.down.sql | sort -r`).
	for i := len(migs) - 1; i >= 0; i-- {
		if err := runPSQLFile(t, psql, url, migs[i].DownPath); err != nil {
			t.Fatalf("migrate-down %s: %v", migs[i].DownPath, err)
		}
	}
	// Stage 1.2 catalog-up-down assertion: `\dt clean_code.*`
	// returns zero rows. Since DROP SCHEMA CASCADE removes the
	// schema entirely, the table count is computed against
	// `information_schema.tables WHERE table_schema =
	// 'clean_code'`, which is 0 when the schema is absent.
	got, err := countCleanCodeTables(t, psql, url)
	if err != nil {
		t.Fatalf("count after down: %v", err)
	}
	if got != 0 {
		t.Fatalf("after migrate-down: clean_code schema has %d tables; want 0", got)
	}
}

// assertCommitScanStatusDefaultsPending verifies the Stage 1.2
// "scan-status-default-pending" scenario: an INSERT into
// `clean_code.commit` that omits the scan_status column produces
// a row with `scan_status='pending'` via the column DEFAULT.
func assertCommitScanStatusDefaultsPending(t *testing.T, psql, url string) {
	t.Helper()
	// Seed a repo to satisfy the FK + a fresh commit; query back
	// the inserted scan_status. We isolate the rows in their own
	// scratch repo so the assertion does not depend on prior
	// inserts from other subtests.
	const sql = `
WITH new_repo AS (
    INSERT INTO clean_code.repo (display_name, default_branch)
    VALUES ('default-pending-scratch', 'main')
    RETURNING repo_id
), inserted_commit AS (
    INSERT INTO clean_code.commit (repo_id, sha, committed_at)
    SELECT repo_id, 'sha-default-pending', now() FROM new_repo
    RETURNING repo_id, sha
)
SELECT c.scan_status::text
  FROM clean_code.commit c
  JOIN inserted_commit i USING (repo_id, sha);`
	out, err := runPSQLQuery(t, psql, url, sql)
	if err != nil {
		t.Fatalf("inserting row with omitted scan_status: %v", err)
	}
	if got := strings.TrimSpace(out); got != "pending" {
		t.Fatalf("scan_status after INSERT without value = %q, want %q", got, "pending")
	}
	// Cleanup so a re-run is idempotent under -count=N.
	_ = runPSQLInline(t, psql, url,
		"DELETE FROM clean_code.commit USING clean_code.repo r "+
			"WHERE clean_code.commit.repo_id = r.repo_id "+
			"AND r.display_name = 'default-pending-scratch';")
	_ = runPSQLInline(t, psql, url,
		"DELETE FROM clean_code.repo WHERE display_name = 'default-pending-scratch';")
}

// assertCommitScanStatusEnumRejects verifies the Stage 1.2
// "scan-status-enum-rejects-invalid" scenario: an INSERT into
// `clean_code.commit` with a non-canonical scan_status value
// fails with PostgreSQL's enum input check.
func assertCommitScanStatusEnumRejects(t *testing.T, psql, url, badValue string) {
	t.Helper()
	// Use the same scratch repo shape as above but inline the
	// invalid scan_status to provoke the rejection. We rely on
	// statement-level rollback (each runPSQLInline opens its own
	// implicit transaction) so the FK seed inside the same
	// statement is reverted along with the failing INSERT.
	sql := fmt.Sprintf(`
WITH new_repo AS (
    INSERT INTO clean_code.repo (display_name, default_branch)
    VALUES ('reject-scratch-%s', 'main')
    RETURNING repo_id
)
INSERT INTO clean_code.commit (repo_id, sha, committed_at, scan_status)
SELECT repo_id, 'sha-reject-%s', now(), '%s'
  FROM new_repo;`, badValue, badValue, badValue)
	if err := runPSQLInline(t, psql, url, sql); err == nil {
		t.Fatalf("INSERT with scan_status=%q succeeded; expected enum check rejection", badValue)
	}
}

// assertScanRunStatusEnumRejects verifies the Stage 1.2
// "scan-run-status-enum" scenario: an INSERT into
// `clean_code.scan_run` with a non-canonical status fails.
func assertScanRunStatusEnumRejects(t *testing.T, psql, url, badValue string) {
	t.Helper()
	sql := fmt.Sprintf(`
WITH new_repo AS (
    INSERT INTO clean_code.repo (display_name, default_branch)
    VALUES ('scan-run-reject-%s', 'main')
    RETURNING repo_id
)
INSERT INTO clean_code.scan_run (repo_id, kind, status)
SELECT repo_id, 'full', '%s' FROM new_repo;`, badValue, badValue)
	if err := runPSQLInline(t, psql, url, sql); err == nil {
		t.Fatalf("INSERT with scan_run.status=%q succeeded; expected enum check rejection", badValue)
	}
}

// assertPrimaryKeysMatchSpec verifies the Stage 1.2
// "primary-keys-match-spec" scenario: every Catalog/Lifecycle
// table carries the architecture-Sec-5.1 mandated PK column set
// (and ONLY that column set). String-matching the up.sql is not
// enough; this subtest queries `pg_constraint` via
// `pg_get_constraintdef` and pins the canonical PK shape.
//
// architecture Sec 5.1 PK contract:
//   - clean_code.repo         PK = (repo_id)
//   - clean_code.commit       PK = (repo_id, sha)        -- composite
//   - clean_code.metric_kind  PK = (metric_kind)
//   - clean_code.repo_event   PK = (event_id)
//   - clean_code.scan_run     PK = (scan_run_id)
func assertPrimaryKeysMatchSpec(t *testing.T, psql, url string) {
	t.Helper()
	cases := []struct {
		table   string
		wantDef string
	}{
		{"repo", "PRIMARY KEY (repo_id)"},
		{"commit", "PRIMARY KEY (repo_id, sha)"},
		{"metric_kind", "PRIMARY KEY (metric_kind)"},
		{"repo_event", "PRIMARY KEY (event_id)"},
		{"scan_run", "PRIMARY KEY (scan_run_id)"},
	}
	for _, c := range cases {
		// `pg_get_constraintdef` returns the textual reproduction
		// of the constraint (`PRIMARY KEY (...)` in column order).
		// `quote_ident` keeps PostgreSQL's tokeniser happy when the
		// table name is `commit` (a non-reserved keyword that still
		// requires the schema prefix to disambiguate from COMMIT).
		query := fmt.Sprintf(`
SELECT pg_get_constraintdef(c.oid)
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
  JOIN pg_namespace n ON n.oid = t.relnamespace
 WHERE n.nspname = 'clean_code'
   AND t.relname = %s
   AND c.contype = 'p'`, pgLiteral(c.table))
		out, err := runPSQLQuery(t, psql, url, query)
		if err != nil {
			t.Fatalf("PK introspection for clean_code.%s: %v", c.table, err)
		}
		got := strings.TrimSpace(out)
		if got != c.wantDef {
			t.Errorf("clean_code.%s primary key = %q; want %q",
				c.table, got, c.wantDef)
		}
	}
}

// assertForeignKeysMatchSpec verifies the Stage 1.2
// "foreign-keys-match-spec" scenario: every FK in the
// Catalog/Lifecycle tables references the canonical parent
// column with the architecture-mandated ON DELETE behaviour.
// String-matching the up.sql cannot catch a structural break
// like "FK created but ON DELETE clause missing" or "FK
// references a sibling column instead of `repo.repo_id`".
//
// architecture Sec 5.1 FK contract (ON DELETE RESTRICT
// throughout -- the lifecycle tables are append-only and we
// never want a repo deletion to silently nuke its history):
//   - clean_code.commit.repo_id      -> clean_code.repo(repo_id)
//   - clean_code.repo_event.repo_id  -> clean_code.repo(repo_id)
//   - clean_code.scan_run.repo_id    -> clean_code.repo(repo_id)
//
// `clean_code.repo` itself has no FK (it is the root of the
// lifecycle graph). `clean_code.metric_kind` has no FK either
// (its rows are catalogue seeds, not per-repo data).
func assertForeignKeysMatchSpec(t *testing.T, psql, url string) {
	t.Helper()
	cases := []struct {
		table   string
		wantFKs []string
	}{
		{"repo", nil},
		{"commit", []string{
			"FOREIGN KEY (repo_id) REFERENCES clean_code.repo(repo_id) ON DELETE RESTRICT",
		}},
		{"metric_kind", nil},
		{"repo_event", []string{
			"FOREIGN KEY (repo_id) REFERENCES clean_code.repo(repo_id) ON DELETE RESTRICT",
		}},
		{"scan_run", []string{
			"FOREIGN KEY (repo_id) REFERENCES clean_code.repo(repo_id) ON DELETE RESTRICT",
		}},
	}
	for _, c := range cases {
		// Sort the result so the assertion is order-independent
		// (PostgreSQL is free to return constraints in any order).
		query := fmt.Sprintf(`
SELECT pg_get_constraintdef(c.oid)
  FROM pg_constraint c
  JOIN pg_class t ON t.oid = c.conrelid
  JOIN pg_namespace n ON n.oid = t.relnamespace
 WHERE n.nspname = 'clean_code'
   AND t.relname = %s
   AND c.contype = 'f'
 ORDER BY pg_get_constraintdef(c.oid)`, pgLiteral(c.table))
		out, err := runPSQLQuery(t, psql, url, query)
		if err != nil {
			t.Fatalf("FK introspection for clean_code.%s: %v", c.table, err)
		}
		got := splitNonEmpty(strings.TrimSpace(out))
		want := append([]string(nil), c.wantFKs...)
		sort.Strings(want)
		sort.Strings(got)
		if !equalStringSlices(got, want) {
			t.Errorf("clean_code.%s foreign keys = %#v; want %#v",
				c.table, got, want)
		}
	}
}

// pgLiteral wraps a Go string in single quotes for embedding
// in a SQL query, doubling any embedded single quote. The test
// table names are static literals so this is paranoia, but the
// helper makes future additions safe by construction.
func pgLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// splitNonEmpty splits the raw psql -At output (one row per
// line) into a slice, dropping empty lines that come from
// trailing newlines or zero-row result sets.
func splitNonEmpty(s string) []string {
	out := make([]string, 0)
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// equalStringSlices reports whether a and b are element-wise
// equal. Both slices are assumed pre-sorted by the caller.
func equalStringSlices(a, b []string) bool {
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

// `psql -v ON_ERROR_STOP=1 -f <path>`. The ON_ERROR_STOP flag
// matches the Makefile shape so an individual statement failure
// aborts the whole file (rather than psql plowing through and
// returning exit 0).
func runPSQLFile(t *testing.T, psql, url, path string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), roundTripTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, psql, url, "-v", "ON_ERROR_STOP=1", "-f", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return wrapPSQL(path, out, err)
	}
	return nil
}

// runPSQLInline executes a literal SQL string against `url` via
// `psql -v ON_ERROR_STOP=1 -c <sql>`. Used for the per-test
// pre-condition cleanup and the post-down table-count probe.
func runPSQLInline(t *testing.T, psql, url, sql string) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), roundTripTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, psql, url, "-v", "ON_ERROR_STOP=1", "-c", sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return wrapPSQL(sql, out, err)
	}
	return nil
}

// runPSQLQuery executes a literal SQL string and returns the
// raw stdout (tuples-only, unaligned via -At so the caller can
// parse it without trimming the psql output formatting). Errors
// surface as the wrapped psqlError shape so the failing SQL is
// visible in the test log.
func runPSQLQuery(t *testing.T, psql, url, sql string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), roundTripTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, psql, url, "-v", "ON_ERROR_STOP=1",
		"-At", "-c", sql)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", wrapPSQL(sql, out, err)
	}
	return string(out), nil
}

// countCleanCodeTables returns the number of base tables visible
// in the `clean_code` schema. Implemented via psql so the test
// does not need a Go PostgreSQL driver.
func countCleanCodeTables(t *testing.T, psql, url string) (int, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), roundTripTimeout)
	defer cancel()
	const query = "SELECT count(*) FROM information_schema.tables " +
		"WHERE table_schema = 'clean_code' AND table_type = 'BASE TABLE'"
	cmd := exec.CommandContext(ctx, psql, url, "-v", "ON_ERROR_STOP=1",
		"-At", "-c", query)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return 0, wrapPSQL(query, out, err)
	}
	got := strings.TrimSpace(string(out))
	switch got {
	case "0":
		return 0, nil
	default:
		n := 0
		for _, c := range got {
			if c < '0' || c > '9' {
				return 0, wrapPSQL(query, out, nil)
			}
			n = n*10 + int(c-'0')
		}
		return n, nil
	}
}

func wrapPSQL(what string, out []byte, err error) error {
	if err == nil {
		return &psqlError{what: what, stdout: string(out)}
	}
	return &psqlError{what: what, stdout: string(out), inner: err}
}

type psqlError struct {
	what   string
	stdout string
	inner  error
}

func (e *psqlError) Error() string {
	if e.inner != nil {
		return "psql " + e.what + ": " + e.inner.Error() + ": " + e.stdout
	}
	return "psql " + e.what + ": unexpected output: " + e.stdout
}

func (e *psqlError) Unwrap() error { return e.inner }
