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
	// Stage 1.3 (implementation-plan lines 84-97) adds the
	// 0002_measurement pair. Pinning the second slot here means a
	// regression that loses the measurement migration fails fast
	// without needing a live PostgreSQL handle.
	if len(migs) < 2 {
		t.Fatalf("DiscoverMigrations returned %d migrations; "+
			"expected at least the Stage 1.2 + Stage 1.3 pairs", len(migs))
	}
	second := migs[1]
	if second.Version != "0002" {
		t.Errorf("second.Version = %q, want %q", second.Version, "0002")
	}
	if second.Name != "measurement" {
		t.Errorf("second.Name = %q, want %q", second.Name, "measurement")
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

// TestMeasurementUpSQLBodyMentionsExpectedObjects sanity-checks
// the Stage 1.3 0002_measurement.up.sql declares the seven
// Measurement tables (scope_binding, metric_sample,
// metric_retraction, metric_sample_active, repo_metric_snapshot,
// cross_repo_percentile, portfolio_snapshot) and the four ENUM
// types (metric_sample_pack, metric_sample_source,
// degraded_reason, scope_kind), plus the canonical enum label
// sets pinned by architecture Sec 5.2.1 / Sec 1.4.1 / Sec 8.2 /
// Sec 5.2.3. Like its Stage 1.2 sibling above, this is a
// string-match test that catches drift in the SQL itself
// (e.g. someone renames `degraded_reason` to `degraded_cause`)
// without needing a live PostgreSQL handle.
func TestMeasurementUpSQLBodyMentionsExpectedObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0002_measurement.up.sql"))
	if err != nil {
		t.Fatalf("read up.sql: %v", err)
	}
	// See TestCatalogUpSQLBodyMentionsExpectedObjects for the
	// CRLF-vs-LF rationale.
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	wantSubstrs := []string{
		// ENUM declarations (closed sets per arch Sec 5.2.1 / Sec 1.4.1).
		"CREATE TYPE clean_code.metric_sample_pack",
		"CREATE TYPE clean_code.metric_sample_source",
		"CREATE TYPE clean_code.degraded_reason",
		"CREATE TYPE clean_code.scope_kind",
		// Tables in dependency order.
		"CREATE TABLE clean_code.scope_binding",
		"CREATE TABLE clean_code.metric_sample",
		"CREATE TABLE clean_code.metric_retraction",
		"CREATE TABLE clean_code.metric_sample_active",
		"CREATE TABLE clean_code.repo_metric_snapshot",
		"CREATE TABLE clean_code.cross_repo_percentile",
		"CREATE TABLE clean_code.portfolio_snapshot",
		// Canonical column names from arch Sec 5.2.1: the run
		// attribution column is `producer_run_id` (NOT
		// `scan_run_id`), the timestamp is `created_at` (NOT
		// `computed_at`).
		"producer_run_id  uuid",
		"created_at       timestamptz",
		// Active-pointer table required objects (tech-spec lines
		// 1107-1119, locked-decision pin Sec 10A).
		"PRIMARY KEY (repo_id, sha, scope_id, metric_kind, metric_version)",
		"metric_sample_active_sample_id_uniq",
		"metric_sample_active_sample_consistent_fk",
		// Active-row target uniqueness on metric_sample (the
		// composite FK target the active pointer references).
		"metric_sample_active_target_uniq",
		// `degraded` default + CHECK invariant (arch Sec 5.2.1
		// line 903-904).
		"degraded         boolean                              NOT NULL\n                     DEFAULT false",
		"metric_sample_degraded_reason_consistent",
		// metric_kind composite FK (0001 line 263-265 forecast).
		"metric_kind_natural_key_uniq",
		"metric_sample_metric_kind_fk",
		// ScopeBinding natural-key UNIQUE for G2 stable-across-SHAs.
		"scope_binding_natural_key_uniq",
		// Hot-read covering index for SHA-pinned reads (tech-spec
		// Sec 7.1.b lines 1150-1160).
		"metric_sample_repo_sha_idx",
		// sample_date_bucket STORED GENERATED column (tech-spec
		// Sec 7.1.b line 1087, partitioning-deferral pin). The
		// expression uses (created_at AT TIME ZONE 'UTC') to
		// make date_trunc IMMUTABLE so PostgreSQL accepts it
		// inside GENERATED ALWAYS AS (the bare timestamptz
		// overload is STABLE and would be rejected).
		"sample_date_bucket date                               GENERATED ALWAYS AS",
		"(date_trunc('month', (created_at AT TIME ZONE 'UTC'))::date)",
		"STORED",
		// `value` is nullable, gated by the row-level
		// `metric_sample_value_present_unless_degraded`
		// CHECK so NULL is permitted iff degraded=true
		// (implementation-plan.md lines 23, 674, 683 +
		// architecture Sec 3.10 step 4 / tech-spec Sec 4.2
		// system-tier fail-safe contract). The column-level
		// `NOT NULL` MUST NOT re-appear: a regression that
		// reintroduces it would block Stage 7.2 from writing
		// degraded system-tier rows.
		"value            double precision,",
		"metric_sample_value_present_unless_degraded",
		"value IS NOT NULL OR degraded = true",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(sql, want) {
			t.Errorf("0002 up.sql missing required substring %q", want)
		}
	}
	// Canonical enum label sets (architecture Sec 5.2.1 line
	// 901-902 + Sec 1.4.1, Sec 8.2 / tech-spec C21, Sec 5.2.3
	// line 1046).
	wantLabelSets := []string{
		// pack: base | solid | system | ingested
		"'base',\n    'solid',\n    'system',\n    'ingested'",
		// source: computed | derived | ingested
		"'computed',\n    'derived',\n    'ingested'",
		// degraded_reason closed set (Sec 8.2 / C21).
		"'xrepo_edges_unavailable',\n    'samples_pending',\n    'policy_signature_invalid',\n    'percentile_stale'",
		// scope_kind: repo | package | file | class | interface | method | block.
		"'repo',\n    'package',\n    'file',\n    'class',\n    'interface',\n    'method',\n    'block'",
	}
	for _, want := range wantLabelSets {
		if !strings.Contains(sql, want) {
			t.Errorf("0002 up.sql missing canonical enum label set:\n----\n%s\n----", want)
		}
	}
	// Negative substrings -- columns explicitly forbidden by the
	// Stage 1.3 brief. We match the canonical column-definition
	// shape (`<name> <type>`) rather than bare-substring so a
	// legitimate FK reference like `REFERENCES ... (scan_run_id)`
	// doesn't trip the assertion. The non-canonical metric_sample
	// shape `scan_run_id uuid` / `computed_at timestamptz` /
	// `unit text` is what we're guarding against.
	rejectSubstrs := []string{
		// `unit` belongs on metric_kind (arch Sec 5.1.3 line
		// 874), NOT on metric_sample.
		"\n    unit             text",
		// The run attribution column is `producer_run_id`
		// (arch Sec 5.2.1 line 905), NOT `scan_run_id` as a
		// metric_sample column.
		"\n    scan_run_id      uuid",
		// The timestamp column is `created_at`
		// (arch Sec 5.2.1 line 907), NOT `computed_at`.
		"\n    computed_at      timestamptz",
		// `policy_version_id` lives on `finding` (arch Sec 5.4.1)
		// and `hot_spot` (arch Sec 5.5.1), NOT on metric_sample.
		"\n    policy_version_id",
		// The `value` column MUST NOT be declared with a
		// column-level NOT NULL: implementation-plan.md lines
		// 23, 674, 683 (system-tier fail-safe at architecture
		// Sec 3.10 step 4 / tech-spec Sec 4.2) require that
		// the Cross-Repo Aggregator write degraded system-tier
		// rows with `value=NULL`. Nullability is enforced at
		// the row level by `metric_sample_value_present_unless
		// _degraded` CHECK so the happy path is still guarded
		// against accidental NULLs.
		"value            double precision                     NOT NULL",
	}
	for _, bad := range rejectSubstrs {
		if strings.Contains(sql, bad) {
			t.Errorf("0002 up.sql contains forbidden substring %q "+
				"(see Stage 1.3 implementation-plan line 88)", bad)
		}
	}
}

// TestMeasurementDownSQLDropsMeasurementObjects sanity-checks the
// Stage 1.3 down half drops exactly the objects the up half
// created -- no more, no less. A regression that drops the
// `clean_code` schema CASCADE here would silently take 0001's
// tables with it on a partial reset.
func TestMeasurementDownSQLDropsMeasurementObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0002_measurement.down.sql"))
	if err != nil {
		t.Fatalf("read down.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	// Every table created in up must be dropped in down.
	wantDrops := []string{
		"DROP TABLE IF EXISTS clean_code.portfolio_snapshot",
		"DROP TABLE IF EXISTS clean_code.cross_repo_percentile",
		"DROP TABLE IF EXISTS clean_code.repo_metric_snapshot",
		"DROP TABLE IF EXISTS clean_code.metric_sample_active",
		"DROP TABLE IF EXISTS clean_code.metric_retraction",
		"DROP TABLE IF EXISTS clean_code.metric_sample",
		"DROP TABLE IF EXISTS clean_code.scope_binding",
		// ALTER TABLE reversal of the metric_kind composite UNIQUE.
		"DROP CONSTRAINT IF EXISTS metric_kind_natural_key_uniq",
		// ENUM types last.
		"DROP TYPE IF EXISTS clean_code.scope_kind",
		"DROP TYPE IF EXISTS clean_code.degraded_reason",
		"DROP TYPE IF EXISTS clean_code.metric_sample_source",
		"DROP TYPE IF EXISTS clean_code.metric_sample_pack",
	}
	for _, want := range wantDrops {
		if !strings.Contains(sql, want) {
			t.Errorf("0002 down.sql missing required statement %q", want)
		}
	}
	// Guard against an accidental schema-CASCADE that would also
	// take 0001's tables down.
	if strings.Contains(sql, "DROP SCHEMA") {
		t.Errorf("0002 down.sql contains `DROP SCHEMA` -- this "+
			"would also drop 0001's Catalog/Lifecycle tables; "+
			"only the 0001 down half should DROP SCHEMA CASCADE")
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

	// Stage 1.3 ("Measurement schema and active row index") test
	// scenarios. The subtests share the same live PostgreSQL
	// handle so we exercise the canonical row-level semantics
	// of the Measurement sub-store without paying for a second
	// up/down cycle. See implementation-plan.md lines 102-106 for
	// the canonical scenario IDs.
	t.Run("active-row-quintuple-uniqueness", func(t *testing.T) {
		assertActiveRowQuintupleUniqueness(t, psql, url)
	})
	t.Run("pack-source-enum-rejects-invalid", func(t *testing.T) {
		assertPackSourceEnumRejectsInvalid(t, psql, url)
	})
	t.Run("degraded-defaults-false", func(t *testing.T) {
		assertDegradedDefaultsFalse(t, psql, url)
	})
	t.Run("degraded-reason-rejections", func(t *testing.T) {
		assertDegradedReasonRejections(t, psql, url)
	})
	t.Run("value-nullable-only-when-degraded", func(t *testing.T) {
		assertValueNullableOnlyWhenDegraded(t, psql, url)
	})
	t.Run("scope-binding-stable-across-shas", func(t *testing.T) {
		assertScopeBindingStableAcrossShas(t, psql, url)
	})
	t.Run("sample-date-bucket-generated", func(t *testing.T) {
		assertSampleDateBucketGenerated(t, psql, url)
	})
	t.Run("cross-repo-percentile-shape", func(t *testing.T) {
		assertCrossRepoPercentileShape(t, psql, url)
	})
	t.Run("repo-metric-snapshot-shape", func(t *testing.T) {
		assertRepoMetricSnapshotShape(t, psql, url)
	})
	t.Run("portfolio-snapshot-shape", func(t *testing.T) {
		assertPortfolioSnapshotShape(t, psql, url)
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
	//
	// The scan_status is read from the INSERT's RETURNING clause
	// rather than via a JOIN on `clean_code.commit` because in
	// PostgreSQL's WITH-data-modifying semantics every statement
	// (including the outer SELECT) executes against the same
	// pre-CTE snapshot, so a `JOIN clean_code.commit c USING
	// (...)` against the just-inserted (repo_id, sha) returns
	// zero rows. RETURNING sees the post-INSERT row including
	// any DEFAULTs materialised by the column-default expression,
	// so it is the canonical way to verify a DEFAULT lands.
	const sql = `
WITH new_repo AS (
    INSERT INTO clean_code.repo (display_name, default_branch)
    VALUES ('default-pending-scratch', 'main')
    RETURNING repo_id
), inserted_commit AS (
    INSERT INTO clean_code.commit (repo_id, sha, committed_at)
    SELECT repo_id, 'sha-default-pending', now() FROM new_repo
    RETURNING scan_status
)
SELECT scan_status::text FROM inserted_commit;`
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
	// Options BEFORE the URL positional. Some psql 16 builds
	// (notably the Windows installer's psql.exe) treat any
	// argument that follows the connection-string positional
	// as a connection-string positional itself (the dbname /
	// username overrides), which silently turns `-v
	// ON_ERROR_STOP=1` and `-c <sql>` into ignored extras and
	// breaks the test in non-obvious ways. POSIX psql accepts
	// either ordering, so URL-last is the portable choice.
	cmd := exec.CommandContext(ctx, psql, "-v", "ON_ERROR_STOP=1", "-f", path, url)
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
	// Options BEFORE the URL positional (see runPSQLFile comment).
	cmd := exec.CommandContext(ctx, psql, "-v", "ON_ERROR_STOP=1", "-c", sql, url)
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
	// Options BEFORE the URL positional (see runPSQLFile comment).
	cmd := exec.CommandContext(ctx, psql, "-v", "ON_ERROR_STOP=1",
		"-At", "-c", sql, url)
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
	cmd := exec.CommandContext(ctx, psql, "-v", "ON_ERROR_STOP=1",
		"-At", "-c", query, url)
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

// seedMeasurementFKChain inserts the minimum FK-parent rows the
// Measurement subtests need before they can INSERT into
// `metric_sample` / `metric_sample_active`. Every parent row
// carries a `label`-derived natural key so concurrent subtests
// running against the same shared schema do not collide on
// `repo.display_name` or `metric_kind.metric_kind` (the latter
// is the PK of the catalog table; reusing it between subtests
// would trip a PK conflict on the second seed).
//
// The returned `measurementSeed` carries the IDs the calling
// subtest needs to bind FK columns; everything else (kind name,
// canonical signature, etc.) is derived deterministically from
// the same `label` so the SQL stays grep-able.
//
// Seed + assertion are issued as SEPARATE `psql -c` invocations
// because `psql -c "stmt1; stmt2; ..."` wraps the batch in one
// implicit transaction (no explicit BEGIN/COMMIT in the input);
// a negative-assertion subtest that issues "seed + bad INSERT"
// in one call would lose the seed rows when the bad INSERT
// rolls the whole transaction back, breaking later subtests
// that need those seed rows. Keeping seed in its own call
// commits before the negative-path assertion runs.
type measurementSeed struct {
	repoID         string
	scanRunID      string
	scopeID        string
	metricKind     string
	metricVersion  int
	sha            string
	canonicalSig   string
	firstSeenSHA   string
}

func seedMeasurementFKChain(t *testing.T, psql, url, label string) measurementSeed {
	t.Helper()
	// Deterministic UUID literals. Embedding the label keeps a
	// failing run grep-able (`select * from clean_code.repo
	// where display_name like '%active-row-uniq%'`).
	repoID := "11111111-1111-1111-1111-" + paddedHex(label, 12, 0)
	scanRunID := "22222222-2222-2222-2222-" + paddedHex(label, 12, 0)
	scopeID := "33333333-3333-3333-3333-" + paddedHex(label, 12, 0)
	kind := "metric_kind_" + sanitizeLabel(label)
	const version = 1
	sha := "sha-" + sanitizeLabel(label)
	canonicalSig := "sig-" + sanitizeLabel(label)
	firstSeenSHA := "first-sha-" + sanitizeLabel(label)
	seedSQL := fmt.Sprintf(`
INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
VALUES ('%s'::uuid, '%s-scratch', 'main');
INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('%s', %d, 'foundation', 'base', 'count', 'test seed for %s');
INSERT INTO clean_code.scan_run (scan_run_id, repo_id, kind, to_sha)
VALUES ('%s'::uuid, '%s'::uuid, 'full', '%s');
INSERT INTO clean_code.scope_binding
       (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
VALUES ('%s'::uuid, '%s'::uuid, 'method', '%s', '%s');`,
		repoID, label,
		kind, version, label,
		scanRunID, repoID, sha,
		scopeID, repoID, canonicalSig, firstSeenSHA,
	)
	if err := runPSQLInline(t, psql, url, seedSQL); err != nil {
		t.Fatalf("seedMeasurementFKChain(%q): %v", label, err)
	}
	return measurementSeed{
		repoID:        repoID,
		scanRunID:     scanRunID,
		scopeID:       scopeID,
		metricKind:    kind,
		metricVersion: version,
		sha:           sha,
		canonicalSig:  canonicalSig,
		firstSeenSHA:  firstSeenSHA,
	}
}

// sanitizeLabel rewrites a subtest label into a string safe to
// embed inside `metric_kind` PK / canonical signature columns:
// hyphens are not legal in PostgreSQL identifiers (which is
// fine for column-value strings, but we keep the naming
// uniform with the underscore-style identifier names elsewhere
// in the schema).
func sanitizeLabel(label string) string {
	return strings.ReplaceAll(label, "-", "_")
}

// paddedHex returns a 12-character hex-ish suffix derived from
// the label. We use `index` to offset the bytes when a subtest
// needs MULTIPLE UUID literals (e.g. two sample IDs). The
// implementation is deliberately not cryptographic; it just
// produces a stable, distinct, 12-char string for a given
// `(label, index)` pair so the UUID literals stay legible in
// failing test logs.
func paddedHex(label string, width, index int) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, width)
	for i := 0; i < width; i++ {
		// Mix the label byte with the index so different
		// callers (index=0, index=1, ...) produce different
		// suffixes without re-deriving from a hash.
		var b byte
		if i < len(label) {
			b = label[i]
		} else {
			b = byte(i)
		}
		b ^= byte(index)
		out[i] = hexDigits[int(b)%len(hexDigits)]
	}
	return string(out)
}

// assertActiveRowQuintupleUniqueness verifies the Stage 1.3
// "active-row-quintuple-uniqueness" scenario
// (implementation-plan.md line 102):
//
//	Given a `metric_sample_active` row already present for the
//	quintuple (repo, sha, scope, metric_kind, metric_version),
//	When a second INSERT into `metric_sample_active` for the
//	same quintuple runs, Then it fails with a PRIMARY KEY
//	violation; an `INSERT ... ON CONFLICT (...) DO UPDATE SET
//	sample_id = EXCLUDED.sample_id` succeeds (UPSERT/repointing
//	is the only allowed way to change the pointer target).
//
// We seed TWO `metric_sample` rows (sample_A and sample_B)
// for the same quintuple. The first active-pointer INSERT
// targets sample_A; the duplicate INSERT targets sample_B and
// must fail; the ON CONFLICT DO UPDATE then swaps the pointer
// to sample_B. We finally verify the pointer's `sample_id` is
// indeed sample_B so a future regression that no-ops the
// upsert (e.g. `DO NOTHING` instead of `DO UPDATE`) fails
// loudly here.
func assertActiveRowQuintupleUniqueness(t *testing.T, psql, url string) {
	t.Helper()
	seed := seedMeasurementFKChain(t, psql, url, "active-row-uniq")
	sampleA := "44444444-4444-4444-4444-" + paddedHex("active-row-uniq", 12, 0)
	sampleB := "44444444-4444-4444-4444-" + paddedHex("active-row-uniq", 12, 1)
	// Insert two metric_sample rows + the initial active pointer
	// targeting sample_A. Issued in one batch because every
	// statement here is on the "happy path" -- a failure on any
	// of them means the seed is broken and the t.Fatal below
	// reports the offending SQL.
	insertSamplesSQL := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample
       (sample_id, repo_id, sha, scope_id, metric_kind,
        metric_version, value, pack, source, producer_run_id)
VALUES
  ('%s'::uuid, '%s'::uuid, '%s', '%s'::uuid, '%s', %d,
   1.0, 'base', 'computed', '%s'::uuid),
  ('%s'::uuid, '%s'::uuid, '%s', '%s'::uuid, '%s', %d,
   2.0, 'base', 'computed', '%s'::uuid);
INSERT INTO clean_code.metric_sample_active
       (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
VALUES ('%s'::uuid, '%s', '%s'::uuid, '%s', %d, '%s'::uuid);`,
		sampleA, seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, seed.scanRunID,
		sampleB, seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, seed.scanRunID,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, sampleA,
	)
	if err := runPSQLInline(t, psql, url, insertSamplesSQL); err != nil {
		t.Fatalf("seed metric_sample + initial pointer: %v", err)
	}
	// Negative: a second INSERT with the same quintuple but a
	// different sample_id (sample_B) must fail with PK
	// violation on `metric_sample_active_pkey`.
	dupSQL := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample_active
       (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
VALUES ('%s'::uuid, '%s', '%s'::uuid, '%s', %d, '%s'::uuid);`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, sampleB,
	)
	if err := runPSQLInline(t, psql, url, dupSQL); err == nil {
		t.Fatalf("duplicate INSERT into metric_sample_active for the " +
			"same quintuple succeeded; expected PRIMARY KEY violation")
	}
	// Positive: the canonical ON CONFLICT DO UPDATE re-pointer
	// pattern must succeed and swap the pointer to sample_B.
	upsertSQL := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample_active
       (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
VALUES ('%s'::uuid, '%s', '%s'::uuid, '%s', %d, '%s'::uuid)
ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version)
DO UPDATE SET sample_id = EXCLUDED.sample_id;`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, sampleB,
	)
	if err := runPSQLInline(t, psql, url, upsertSQL); err != nil {
		t.Fatalf("INSERT ... ON CONFLICT DO UPDATE re-pointer: %v", err)
	}
	// Verify the pointer's sample_id was actually swapped to
	// sample_B. A regression that uses `DO NOTHING` instead of
	// `DO UPDATE` would silently no-op; this read catches that.
	checkSQL := fmt.Sprintf(`
SELECT sample_id::text
  FROM clean_code.metric_sample_active
 WHERE repo_id = '%s'::uuid
   AND sha = '%s'
   AND scope_id = '%s'::uuid
   AND metric_kind = '%s'
   AND metric_version = %d;`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind, seed.metricVersion,
	)
	out, err := runPSQLQuery(t, psql, url, checkSQL)
	if err != nil {
		t.Fatalf("read pointer after upsert: %v", err)
	}
	if got := strings.TrimSpace(out); got != sampleB {
		t.Fatalf("after ON CONFLICT DO UPDATE, pointer.sample_id = %q; "+
			"want %q (the upsert must SWAP the pointer to sample_B, "+
			"not no-op)", got, sampleB)
	}
}

// assertPackSourceEnumRejectsInvalid verifies the Stage 1.3
// "pack-source-enum-rejects-invalid" scenario
// (implementation-plan.md line 103): an INSERT into
// `metric_sample` with `pack='unknown'` or `source='external'`
// is rejected by PostgreSQL's enum input check.
//
// Each negative INSERT uses VALID values for every other column
// so the only possible failure mode is the enum check on the
// targeted column. This guards against the test silently
// passing for the wrong reason (e.g. a missing FK seed making
// the INSERT fail before reaching the enum check).
func assertPackSourceEnumRejectsInvalid(t *testing.T, psql, url string) {
	t.Helper()
	seed := seedMeasurementFKChain(t, psql, url, "pack-source-enum")
	cases := []struct {
		column   string
		badValue string
		sqlFmt   string
	}{
		{
			column:   "pack",
			badValue: "unknown",
			sqlFmt: `
INSERT INTO clean_code.metric_sample
       (repo_id, sha, scope_id, metric_kind, metric_version,
        value, pack, source, producer_run_id)
VALUES ('%s'::uuid, '%s', '%s'::uuid, '%s', %d,
        1.0, '%s', 'computed', '%s'::uuid);`,
		},
		{
			column:   "source",
			badValue: "external",
			sqlFmt: `
INSERT INTO clean_code.metric_sample
       (repo_id, sha, scope_id, metric_kind, metric_version,
        value, pack, source, producer_run_id)
VALUES ('%s'::uuid, '%s', '%s'::uuid, '%s', %d,
        1.0, 'base', '%s', '%s'::uuid);`,
		},
	}
	for _, c := range cases {
		sql := fmt.Sprintf(c.sqlFmt,
			seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
			seed.metricVersion, c.badValue, seed.scanRunID,
		)
		if err := runPSQLInline(t, psql, url, sql); err == nil {
			t.Errorf("INSERT with %s=%q succeeded; "+
				"expected enum check rejection", c.column, c.badValue)
		}
	}
}

// assertDegradedDefaultsFalse verifies the Stage 1.3
// "degraded-defaults-false" scenario (implementation-plan.md
// line 104): an INSERT into `metric_sample` without a
// `degraded` value materialises with `degraded=false` (the
// column DEFAULT) and `degraded_reason IS NULL`. The
// `metric_sample_degraded_reason_consistent` CHECK requires
// degraded_reason IS NULL whenever degraded=false, so this
// also verifies the CHECK accepts the DEFAULT shape.
func assertDegradedDefaultsFalse(t *testing.T, psql, url string) {
	t.Helper()
	seed := seedMeasurementFKChain(t, psql, url, "degraded-default")
	insertSQL := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample
       (repo_id, sha, scope_id, metric_kind, metric_version,
        value, pack, source, producer_run_id)
VALUES ('%s'::uuid, '%s', '%s'::uuid, '%s', %d,
        1.0, 'base', 'computed', '%s'::uuid);`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, seed.scanRunID,
	)
	if err := runPSQLInline(t, psql, url, insertSQL); err != nil {
		t.Fatalf("INSERT omitting `degraded`: %v", err)
	}
	readSQL := fmt.Sprintf(`
SELECT degraded::text || '|' || COALESCE(degraded_reason::text, '<null>')
  FROM clean_code.metric_sample
 WHERE repo_id = '%s'::uuid
   AND sha = '%s'
   AND scope_id = '%s'::uuid
   AND metric_kind = '%s'
   AND metric_version = %d;`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind, seed.metricVersion,
	)
	out, err := runPSQLQuery(t, psql, url, readSQL)
	if err != nil {
		t.Fatalf("read back degraded/degraded_reason: %v", err)
	}
	got := strings.TrimSpace(out)
	const want = "false|<null>"
	if got != want {
		t.Fatalf("(degraded|degraded_reason) after INSERT without "+
			"value = %q; want %q (DEFAULT must materialise "+
			"`degraded=false` and `degraded_reason IS NULL`)", got, want)
	}
}

// assertScopeBindingStableAcrossShas verifies the Stage 1.3
// "scope-binding-stable-across-shas" scenario
// (implementation-plan.md line 105). The architecture-level
// stability is enforced by the writer's deterministic UUIDv5
// recipe over (repo_id, scope_kind, canonical_signature,
// first_seen_sha). At the DB level the matching defensive
// constraints are:
//
//   - PRIMARY KEY on `scope_id`: two writers that derive the
//     same UUIDv5 cannot both insert (the second fails with PK
//     violation; the writer ON CONFLICT DO NOTHINGs in
//     practice).
//
//   - UNIQUE on (repo_id, scope_kind, canonical_signature,
//     first_seen_sha): a writer-bug that derives TWO different
//     UUIDv5s for the same natural-key tuple is also caught.
//
// We exercise BOTH constraints, taking care to isolate which
// constraint fires. To prove the natural-key UNIQUE actually
// fires (independent of the PK), the second INSERT uses a
// DIFFERENT scope_id but the SAME natural-key tuple. To prove
// the PK fires (independent of the natural-key UNIQUE), a
// third INSERT reuses the SAME scope_id but CHANGES the
// canonical_signature so the natural-key UNIQUE no longer
// matches.
func assertScopeBindingStableAcrossShas(t *testing.T, psql, url string) {
	t.Helper()
	// We need a repo + the first scope_binding row. Don't call
	// seedMeasurementFKChain here because we want full control
	// over the scope_binding insert shape.
	const label = "scope-binding-stable"
	repoID := "11111111-1111-1111-1111-" + paddedHex(label, 12, 0)
	scopeIDA := "33333333-3333-3333-3333-" + paddedHex(label, 12, 0)
	scopeIDB := "33333333-3333-3333-3333-" + paddedHex(label, 12, 1)
	const (
		canonicalSig = "sig-stable-across-shas"
		firstSeenSHA = "first-sha-stable"
		altSig       = "sig-stable-alt"
	)
	repoSeed := fmt.Sprintf(`
INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
VALUES ('%s'::uuid, '%s-scratch', 'main');
INSERT INTO clean_code.scope_binding
       (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
VALUES ('%s'::uuid, '%s'::uuid, 'method', '%s', '%s');`,
		repoID, label, scopeIDA, repoID, canonicalSig, firstSeenSHA,
	)
	if err := runPSQLInline(t, psql, url, repoSeed); err != nil {
		t.Fatalf("seed scope-binding-stable repo + first row: %v", err)
	}
	// Natural-key UNIQUE: same natural key, DIFFERENT scope_id.
	// Only the natural-key UNIQUE can fire here because the PK
	// is over a different scope_id literal.
	dupNaturalKey := fmt.Sprintf(`
INSERT INTO clean_code.scope_binding
       (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
VALUES ('%s'::uuid, '%s'::uuid, 'method', '%s', '%s');`,
		scopeIDB, repoID, canonicalSig, firstSeenSHA,
	)
	if err := runPSQLInline(t, psql, url, dupNaturalKey); err == nil {
		t.Errorf("INSERT into scope_binding with same natural key but "+
			"different scope_id (%s vs %s) succeeded; expected "+
			"scope_binding_natural_key_uniq violation", scopeIDB, scopeIDA)
	}
	// PRIMARY KEY: same scope_id, DIFFERENT natural key (the
	// `canonical_signature` is changed). Only the PK can fire
	// here because the natural-key tuple no longer matches the
	// first row.
	dupPK := fmt.Sprintf(`
INSERT INTO clean_code.scope_binding
       (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
VALUES ('%s'::uuid, '%s'::uuid, 'method', '%s', '%s');`,
		scopeIDA, repoID, altSig, firstSeenSHA,
	)
	if err := runPSQLInline(t, psql, url, dupPK); err == nil {
		t.Errorf("INSERT into scope_binding with duplicate scope_id but "+
			"different natural key succeeded; expected "+
			"scope_binding_pkey violation")
	}
	// Sanity: exactly ONE row remains for the original natural
	// key (the writer's idempotent path lands here).
	countSQL := fmt.Sprintf(`
SELECT count(*)::text
  FROM clean_code.scope_binding
 WHERE repo_id = '%s'::uuid
   AND scope_kind = 'method'
   AND canonical_signature = '%s'
   AND first_seen_sha = '%s';`,
		repoID, canonicalSig, firstSeenSHA,
	)
	out, err := runPSQLQuery(t, psql, url, countSQL)
	if err != nil {
		t.Fatalf("count scope_binding rows: %v", err)
	}
	if got := strings.TrimSpace(out); got != "1" {
		t.Fatalf("scope_binding row count for the natural key = %q; "+
			"want %q (constraints must keep the writer idempotent)", got, "1")
	}
}

// assertCrossRepoPercentileShape verifies the Stage 1.3
// "cross-repo-percentile-shape" scenario (implementation-plan.md
// line 106): the `cross_repo_percentile` table has exactly the
// columns `percentile_id, metric_kind, scope_kind,
// histogram_json, p50, p90, p99, built_at` -- no extras (e.g.
// no `p95.system` materialised as a row, no per-tier columns)
// and no missing canonical columns.
//
// Reads `information_schema.columns` in ordinal order so the
// failure log shows columns in CREATE TABLE sequence (matches
// architecture Sec 5.2.5 lines 1067-1078 listing).
func assertCrossRepoPercentileShape(t *testing.T, psql, url string) {
	t.Helper()
	const query = `
SELECT string_agg(column_name, ',' ORDER BY ordinal_position)
  FROM information_schema.columns
 WHERE table_schema = 'clean_code'
   AND table_name   = 'cross_repo_percentile';`
	out, err := runPSQLQuery(t, psql, url, query)
	if err != nil {
		t.Fatalf("read cross_repo_percentile shape: %v", err)
	}
	got := strings.TrimSpace(out)
	const want = "percentile_id,metric_kind,scope_kind,histogram_json,p50,p90,p99,built_at"
	if got != want {
		t.Fatalf("cross_repo_percentile column shape mismatch:\n"+
			"  got:  %s\n  want: %s\n"+
			"architecture Sec 5.2.5 pins exactly these eight columns "+
			"in this order; any extra or missing column is a "+
			"canon-drift regression.", got, want)
	}
}

// assertRepoMetricSnapshotShape verifies the canonical column
// list (name + type, in ordinal order) of `repo_metric_snapshot`
// (architecture Sec 5.2.4). The brief explicitly pins the
// `count` column name (NOT `sample_count`); the type assertion
// catches a silent `bigint` -> `integer` drift that a name-only
// query would miss.
//
// `format_type(atttypid, atttypmod)` is used over
// `information_schema.columns.data_type` because the latter
// returns `USER-DEFINED` for enum columns (e.g. `scope_kind`),
// which would not distinguish `scope_kind` (the canonical enum)
// from a hypothetical regression that changes the type to a
// different user-defined type.
func assertRepoMetricSnapshotShape(t *testing.T, psql, url string) {
	t.Helper()
	const query = `
SELECT string_agg(
         a.attname || ':' || format_type(a.atttypid, a.atttypmod),
         ',' ORDER BY a.attnum)
  FROM pg_catalog.pg_attribute a
  JOIN pg_catalog.pg_class c     ON c.oid = a.attrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'clean_code'
   AND c.relname = 'repo_metric_snapshot'
   AND a.attnum  > 0
   AND NOT a.attisdropped;`
	out, err := runPSQLQuery(t, psql, url, query)
	if err != nil {
		t.Fatalf("read repo_metric_snapshot shape: %v", err)
	}
	got := strings.TrimSpace(out)
	const want = "snapshot_id:uuid," +
		"repo_id:uuid," +
		"metric_kind:text," +
		"scope_kind:clean_code.scope_kind," +
		"count:bigint," +
		"mean:double precision," +
		"p50:double precision," +
		"p90:double precision," +
		"p99:double precision," +
		"built_at:timestamp with time zone"
	if got != want {
		t.Fatalf("repo_metric_snapshot column shape mismatch:\n"+
			"  got:  %s\n  want: %s\n"+
			"architecture Sec 5.2.4 pins exactly these ten columns "+
			"in this order with these types; the Stage 1.3 brief "+
			"specifically pins `count bigint` (NOT `sample_count` "+
			"and NOT `integer`).", got, want)
	}
}

// assertPortfolioSnapshotShape verifies the canonical column
// list (name + type, in ordinal order) of `portfolio_snapshot`
// (architecture Sec 5.2.6). The Stage 1.3 brief explicitly
// pins the operator-pinned aggregate shape into
// `aggregate_json jsonb` (NOT split into typed columns like
// `sum_value` / `weighted_mean`); the type assertion guards
// against a silent `jsonb` -> `json` drift that would lose
// indexability.
func assertPortfolioSnapshotShape(t *testing.T, psql, url string) {
	t.Helper()
	const query = `
SELECT string_agg(
         a.attname || ':' || format_type(a.atttypid, a.atttypmod),
         ',' ORDER BY a.attnum)
  FROM pg_catalog.pg_attribute a
  JOIN pg_catalog.pg_class c     ON c.oid = a.attrelid
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = 'clean_code'
   AND c.relname = 'portfolio_snapshot'
   AND a.attnum  > 0
   AND NOT a.attisdropped;`
	out, err := runPSQLQuery(t, psql, url, query)
	if err != nil {
		t.Fatalf("read portfolio_snapshot shape: %v", err)
	}
	got := strings.TrimSpace(out)
	const want = "portfolio_snapshot_id:uuid," +
		"metric_kind:text," +
		"scope_kind:clean_code.scope_kind," +
		"repo_count:integer," +
		"aggregate_json:jsonb," +
		"built_at:timestamp with time zone"
	if got != want {
		t.Fatalf("portfolio_snapshot column shape mismatch:\n"+
			"  got:  %s\n  want: %s\n"+
			"architecture Sec 5.2.6 pins exactly these six columns "+
			"in this order with these types; aggregate_json MUST "+
			"remain jsonb (not json) so the Insights queries can "+
			"index by aggregate-key.", got, want)
	}
}

// assertSampleDateBucketGenerated verifies the Stage 1.3
// tech-spec Sec 7.1.b sample_date_bucket column is materialised
// as a STORED GENERATED column whose expression uses the
// IMMUTABLE `(created_at AT TIME ZONE 'UTC')` cast (not the
// bare timestamptz overload, which is STABLE and rejected by
// PostgreSQL inside GENERATED ALWAYS AS).
//
// We assert FOUR things in one query:
//
//	(1) the column exists at all;
//	(2) its data_type is `date` (not `timestamp` or `text`);
//	(3) is_generated = 'ALWAYS' (not a writer-supplied column);
//	(4) the generation_expression mentions BOTH `date_trunc('month'`
//	    AND `AT TIME ZONE 'UTC'` -- a regression that drops the
//	    UTC cast would compile fine on PG <= 12 but break under
//	    a future PG upgrade or a tz-config change.
//
// We also verify end-to-end semantics: a sample inserted with
// `created_at = 2025-11-30 14:30:00+00` (= 23:30 JST, well into
// December in JST) lands in the November bucket because the
// UTC cast pins the bucket to UTC-month, not local-month.
func assertSampleDateBucketGenerated(t *testing.T, psql, url string) {
	t.Helper()
	const metaQuery = `
SELECT column_name || '|' ||
       data_type || '|' ||
       is_generated || '|' ||
       generation_expression
  FROM information_schema.columns
 WHERE table_schema = 'clean_code'
   AND table_name   = 'metric_sample'
   AND column_name  = 'sample_date_bucket';`
	out, err := runPSQLQuery(t, psql, url, metaQuery)
	if err != nil {
		t.Fatalf("read sample_date_bucket metadata: %v", err)
	}
	got := strings.TrimSpace(out)
	if got == "" {
		t.Fatalf("sample_date_bucket column is missing from " +
			"clean_code.metric_sample; tech-spec Sec 7.1.b line " +
			"1087 + Sec 7.1.b partitioning-deferral pin require " +
			"it to be present as a STORED GENERATED column.")
	}
	parts := strings.SplitN(got, "|", 4)
	if len(parts) != 4 {
		t.Fatalf("unexpected sample_date_bucket meta shape: %q "+
			"(want 4 pipe-separated fields)", got)
	}
	colName, dataType, isGen, genExpr := parts[0], parts[1], parts[2], parts[3]
	if colName != "sample_date_bucket" {
		t.Errorf("column_name = %q; want \"sample_date_bucket\"", colName)
	}
	if dataType != "date" {
		t.Errorf("data_type = %q; want \"date\" (tech-spec line "+
			"1087 pins `date`, NOT `timestamp` / `text`)", dataType)
	}
	if isGen != "ALWAYS" {
		t.Errorf("is_generated = %q; want \"ALWAYS\" (tech-spec "+
			"line 1087 pins GENERATED ALWAYS AS, NOT a writer-"+
			"supplied column)", isGen)
	}
	// The expression must include both `date_trunc('month'` and
	// `AT TIME ZONE 'UTC'`. A regression that drops the UTC cast
	// would make the expression STABLE (rejected by PG inside
	// GENERATED ALWAYS AS) -- but we still defend against a
	// regression where someone tries to "simplify" the
	// expression to `created_at::date` which is local-tz-
	// dependent.
	if !strings.Contains(genExpr, "date_trunc") {
		t.Errorf("generation_expression %q missing `date_trunc` "+
			"(tech-spec line 1088 pins the month-trunc shape)", genExpr)
	}
	if !strings.Contains(genExpr, "AT TIME ZONE 'UTC'") {
		t.Errorf("generation_expression %q missing the `AT TIME "+
			"ZONE 'UTC'` cast (Stage 1.3 partitioning-deferral pin: "+
			"the cast is required to make the expression IMMUTABLE "+
			"so PostgreSQL accepts it inside GENERATED ALWAYS AS)", genExpr)
	}
	// End-to-end UTC-bucket semantics. Insert a sample whose
	// created_at is on the UTC side of midnight (~14:30 UTC =
	// 23:30 JST = early next-day in JST). The bucket must be
	// the UTC month (the day the UTC timestamp falls in), not
	// the local-tz month.
	seed := seedMeasurementFKChain(t, psql, url, "sample-date-bucket")
	insertSQL := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample
       (sample_id, repo_id, sha, scope_id, metric_kind,
        metric_version, value, pack, source, producer_run_id,
        created_at)
VALUES ('aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee'::uuid,
        '%s'::uuid, '%s', '%s'::uuid, '%s', %d,
        1.0, 'base', 'computed', '%s'::uuid,
        '2025-11-30 14:30:00+00'::timestamptz);`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, seed.scanRunID,
	)
	if err := runPSQLInline(t, psql, url, insertSQL); err != nil {
		t.Fatalf("INSERT for bucket end-to-end check: %v", err)
	}
	const bucketReadSQL = `
SELECT sample_date_bucket::text
  FROM clean_code.metric_sample
 WHERE sample_id = 'aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee'::uuid;`
	bucketOut, err := runPSQLQuery(t, psql, url, bucketReadSQL)
	if err != nil {
		t.Fatalf("read back sample_date_bucket: %v", err)
	}
	gotBucket := strings.TrimSpace(bucketOut)
	const wantBucket = "2025-11-01"
	if gotBucket != wantBucket {
		t.Fatalf("sample_date_bucket for 2025-11-30 14:30 UTC = "+
			"%q; want %q (the bucket MUST be the UTC month, "+
			"not the local-tz month -- a regression that drops "+
			"the `AT TIME ZONE 'UTC'` cast would land this in "+
			"the December bucket under JST)", gotBucket, wantBucket)
	}
}

// assertDegradedReasonRejections verifies the Stage 1.3
// `metric_sample.degraded_reason` enum closed-set AND the
// `metric_sample_degraded_reason_consistent` CHECK constraint
// reject the three forbidden input shapes:
//
//	(a) degraded=false WITH a non-NULL degraded_reason
//	    -- the CHECK invariant "set iff degraded=true" fails
//	(b) degraded=true WITH a NULL degraded_reason
//	    -- the CHECK invariant "set iff degraded=true" fails
//	(c) degraded=true WITH a non-canonical reason value
//	    -- the closed-set enum cast fails before CHECK runs
//
// And accept the one legitimate degraded shape as a positive
// control:
//
//	(d) degraded=true WITH 'samples_pending' (canonical reason)
//	    -- both CHECK and enum cast pass; INSERT succeeds.
//
// All four cases use VALID values for every other column so
// the only possible failure mode is the targeted constraint.
// The positive control prevents a too-strict CHECK regression
// (e.g. someone "tightens" the CHECK to also reject `true`
// rows) from passing all three negatives silently.
func assertDegradedReasonRejections(t *testing.T, psql, url string) {
	t.Helper()
	seed := seedMeasurementFKChain(t, psql, url, "degraded-reason-rejections")
	// shaSuffix lets each case use a distinct sha+sample_id pair
	// so a positive INSERT from case (d) doesn't collide with
	// a leftover row from case (a/b/c) (which all failed and
	// rolled back, but we keep them isolated for clarity).
	negatives := []struct {
		shaSuffix     string
		degraded      string
		reason        string // SQL literal, e.g. 'samples_pending' or NULL
		wantSubstr    string
		caseLabel     string
	}{
		{
			shaSuffix:  "neg-a",
			degraded:   "false",
			reason:     "'samples_pending'",
			wantSubstr: "metric_sample_degraded_reason_consistent",
			caseLabel:  "(a) degraded=false WITH samples_pending",
		},
		{
			shaSuffix:  "neg-b",
			degraded:   "true",
			reason:     "NULL",
			wantSubstr: "metric_sample_degraded_reason_consistent",
			caseLabel:  "(b) degraded=true WITH NULL reason",
		},
		{
			shaSuffix:  "neg-c",
			degraded:   "true",
			reason:     "'bogus_reason'",
			wantSubstr: "invalid input value for enum",
			caseLabel:  "(c) degraded=true WITH non-canonical 'bogus_reason'",
		},
	}
	for _, n := range negatives {
		sql := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample
       (repo_id, sha, scope_id, metric_kind, metric_version,
        value, pack, source, producer_run_id, degraded, degraded_reason)
VALUES ('%s'::uuid, '%s-%s', '%s'::uuid, '%s', %d,
        1.0, 'base', 'computed', '%s'::uuid, %s, %s);`,
			seed.repoID, seed.sha, n.shaSuffix, seed.scopeID,
			seed.metricKind, seed.metricVersion, seed.scanRunID,
			n.degraded, n.reason,
		)
		err := runPSQLInline(t, psql, url, sql)
		if err == nil {
			t.Errorf("%s succeeded; expected failure mentioning %q",
				n.caseLabel, n.wantSubstr)
			continue
		}
		if !strings.Contains(err.Error(), n.wantSubstr) {
			t.Errorf("%s failed but the error did not mention %q; "+
				"got: %v (this guards against the failure being "+
				"caused by an unrelated constraint, e.g. a FK)",
				n.caseLabel, n.wantSubstr, err)
		}
	}
	// Positive control: degraded=true + 'samples_pending' MUST
	// succeed. Without this guard, a too-strict CHECK that
	// rejected all degraded rows would pass (a), (b), (c) above
	// and silently break the writer's legitimate degraded path.
	positiveSQL := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample
       (repo_id, sha, scope_id, metric_kind, metric_version,
        value, pack, source, producer_run_id, degraded, degraded_reason)
VALUES ('%s'::uuid, '%s-pos', '%s'::uuid, '%s', %d,
        1.0, 'base', 'computed', '%s'::uuid, true, 'samples_pending');`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, seed.scanRunID,
	)
	if err := runPSQLInline(t, psql, url, positiveSQL); err != nil {
		t.Fatalf("(d) positive control (degraded=true, "+
			"degraded_reason='samples_pending') failed: %v -- a "+
			"too-strict CHECK regression would land here. The "+
			"legitimate writer path through degraded=true MUST "+
			"succeed with any canonical degraded_reason value.", err)
	}
}

// assertValueNullableOnlyWhenDegraded verifies the Stage 1.3
// `metric_sample_value_present_unless_degraded` CHECK that binds
// `value` nullability to `degraded`:
//
//	(a) degraded=true  + value=NULL  -- INSERT succeeds (Stage 7.2
//	    system-tier fail-safe path per implementation-plan.md
//	    lines 23, 674, 683 + architecture Sec 3.10 step 4 / tech-
//	    spec Sec 4.2).
//	(b) degraded=false + value=NULL  -- CHECK rejects the row.
//	    Happy-path foundation/ingested rows MUST always carry a
//	    non-null value.
//	(c) degraded=true  + value=1.0   -- INSERT succeeds. This
//	    positive control prevents a misread regression that
//	    accidentally inverts the CHECK direction.
//
// Without this test, a regression that re-adds the column-level
// `NOT NULL` would silently break Stage 7.2's degraded system-
// tier writer path; the static body-substring guard in
// `TestMeasurementUpSQLBodyMentionsExpectedObjects` catches the
// schema-text shape, and this live test catches the behavioural
// shape.
func assertValueNullableOnlyWhenDegraded(t *testing.T, psql, url string) {
	t.Helper()
	seed := seedMeasurementFKChain(t, psql, url, "value-nullable-only-when-degraded")
	// (a) degraded=true + value=NULL: the canonical system-tier
	// fail-safe write. Must succeed; the row is the audit trail
	// of the missing input at that SHA.
	insertA := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample
       (repo_id, sha, scope_id, metric_kind, metric_version,
        value, pack, source, producer_run_id, degraded, degraded_reason)
VALUES ('%s'::uuid, '%s-a', '%s'::uuid, '%s', %d,
        NULL, 'system', 'derived', '%s'::uuid, true, 'samples_pending');`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, seed.scanRunID,
	)
	if err := runPSQLInline(t, psql, url, insertA); err != nil {
		t.Fatalf("(a) degraded=true + value=NULL failed: %v -- "+
			"this is the Stage 7.2 system-tier fail-safe write "+
			"path; a regression that re-adds column-level NOT "+
			"NULL on `value` would surface here. See "+
			"implementation-plan.md lines 23, 674, 683.", err)
	}
	// Confirm the row landed and value is genuinely NULL (not 0,
	// not NaN). A regression that wrote a 0 coerced value would
	// pass the INSERT but corrupt the observability signal.
	const readSQL = `
SELECT value IS NULL
  FROM clean_code.metric_sample
 WHERE repo_id = '%s'::uuid AND sha = '%s-a';`
	readOut, err := runPSQLQuery(t, psql, url, fmt.Sprintf(readSQL,
		seed.repoID, seed.sha))
	if err != nil {
		t.Fatalf("read back NULL value: %v", err)
	}
	if got := strings.TrimSpace(readOut); got != "t" {
		t.Fatalf("(a) inserted value IS NULL = %q; want \"t\" "+
			"-- the value must persist as a true SQL NULL, not "+
			"be silently coerced to 0 or NaN", got)
	}
	// (b) degraded=false + value=NULL: must fail on the new
	// CHECK constraint. Every other column is valid so the only
	// possible failure mode is the value-present-unless-degraded
	// invariant.
	insertB := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample
       (repo_id, sha, scope_id, metric_kind, metric_version,
        value, pack, source, producer_run_id, degraded, degraded_reason)
VALUES ('%s'::uuid, '%s-b', '%s'::uuid, '%s', %d,
        NULL, 'base', 'computed', '%s'::uuid, false, NULL);`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, seed.scanRunID,
	)
	err = runPSQLInline(t, psql, url, insertB)
	if err == nil {
		t.Errorf("(b) degraded=false + value=NULL succeeded; "+
			"expected CHECK violation. The "+
			"metric_sample_value_present_unless_degraded "+
			"constraint is missing or inverted.")
	} else if !strings.Contains(err.Error(),
		"metric_sample_value_present_unless_degraded") {
		t.Errorf("(b) degraded=false + value=NULL failed but the "+
			"error did not mention "+
			"metric_sample_value_present_unless_degraded; "+
			"got: %v (this guards against the failure being "+
			"caused by an unrelated constraint)", err)
	}
	// (c) degraded=true + value=1.0: positive control that the
	// happy-path degraded write with an actual partial value
	// also succeeds. Without this, a regression that flips the
	// CHECK to require value IS NULL when degraded=true would
	// pass (a) and (b) above silently.
	insertC := fmt.Sprintf(`
INSERT INTO clean_code.metric_sample
       (repo_id, sha, scope_id, metric_kind, metric_version,
        value, pack, source, producer_run_id, degraded, degraded_reason)
VALUES ('%s'::uuid, '%s-c', '%s'::uuid, '%s', %d,
        1.0, 'system', 'derived', '%s'::uuid, true, 'xrepo_edges_unavailable');`,
		seed.repoID, seed.sha, seed.scopeID, seed.metricKind,
		seed.metricVersion, seed.scanRunID,
	)
	if err := runPSQLInline(t, psql, url, insertC); err != nil {
		t.Fatalf("(c) positive control (degraded=true + "+
			"value=1.0 + reason='xrepo_edges_unavailable') "+
			"failed: %v -- a CHECK regression that requires "+
			"value IS NULL when degraded=true would land here. "+
			"Both NULL and non-NULL value MUST be acceptable "+
			"under degraded=true per implementation-plan.md "+
			"line 674 (\"best-effort partial computation or "+
			"NULL\").", err)
	}
}
