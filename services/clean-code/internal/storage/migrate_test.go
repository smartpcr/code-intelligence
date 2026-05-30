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

// stripSQLComments removes PostgreSQL `--`-style line comments
// from a SQL source so substring searches for disallowed object
// names don't accidentally match explanatory prose that documents
// the rejection (e.g. "the values `regression | improvement | flat`
// were considered and explicitly rejected"). Block comments are
// not used in the migration files, so they need no handling.
func stripSQLComments(sql string) string {
	out := make([]string, 0, 256)
	for _, line := range strings.Split(sql, "\n") {
		idx := strings.Index(line, "--")
		if idx >= 0 {
			line = line[:idx]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// stripSQLStringLiterals removes the contents of every single-
// quoted SQL string literal from `sql`, leaving the surrounding
// DDL intact. This is the second half of the "code-only view"
// the canon-guard absence tests use: a forbidden column name like
// `signed_at` MUST NOT appear in an actual column declaration,
// but it MAY appear inside a COMMENT ON ... 'string-literal'
// statement that documents WHY the architecture canon rejects
// the column. Stripping the literal payload (but keeping the
// surrounding quotes as `''`) lets the canon-guard search the
// DDL without false positives from explanatory prose.
//
// The implementation tracks a simple in-string flag and honours
// the PostgreSQL/SQL standard `''` escape (two adjacent quotes
// inside a literal collapse to a single quote rather than ending
// the literal). Dollar-quoted strings are NOT used anywhere in
// the migration files, so they need no handling.
func stripSQLStringLiterals(sql string) string {
	var b strings.Builder
	b.Grow(len(sql))
	inStr := false
	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		if !inStr {
			if c == '\'' {
				inStr = true
				b.WriteRune(c)
			} else {
				b.WriteRune(c)
			}
			continue
		}
		// inside a string literal
		if c == '\'' {
			// `''` inside a literal is an escaped quote.
			if i+1 < len(runes) && runes[i+1] == '\'' {
				i++ // skip the second quote, stay inside
				continue
			}
			inStr = false
			b.WriteRune(c)
			continue
		}
		// drop the literal payload
	}
	return b.String()
}

// sqlCodeOnly returns `sql` with both `--` line comments and
// single-quoted string-literal payloads stripped. The result is
// the SQL DDL body alone, suitable for canon-guard substring
// searches that must not be confused by explanatory prose.
//
// Stripping ORDER MATTERS: string literals are stripped FIRST so
// that a `--` token inside a string (e.g. an explanatory aside
// "tech-spec Sec 4.14 single-tenant -- there is NO ...") cannot
// fool the comment stripper into cutting the line, which would
// then leave an unterminated literal whose continuation lines
// get treated as DDL. Doing literals first collapses those
// strings to `''`, after which the comment stripper sees only
// the inter-statement `--` lines (which are always real comments).
func sqlCodeOnly(sql string) string {
	return stripSQLComments(stripSQLStringLiterals(sql))
}

// TestDiscoverMigrations_findsStage14Pair is the load-bearing
// assertion for the Stage 1.4 implementation step "Add
// migrations/0003_policy_audit_refactor.up.sql ... and matching
// 0003_policy_audit_refactor.down.sql". A test failure here means
// the directory layout regressed below the structural minimum
// the Stage 1.4 brief locks in. (Stage 1.3 -- the 0002 pair --
// lands in a sibling workstream; this test does not assume 0002
// is present.)
func TestDiscoverMigrations_findsStage14Pair(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	migs, err := DiscoverMigrations(dir)
	if err != nil {
		t.Fatalf("DiscoverMigrations(%q): %v", dir, err)
	}
	var got *Migration
	for i := range migs {
		if migs[i].Version == "0003" {
			got = &migs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("DiscoverMigrations(%q) missing the Stage 1.4 "+
			"0003_policy_audit_refactor pair", dir)
	}
	if got.Name != "policy_audit_refactor" {
		t.Errorf("Stage 1.4 migration name = %q, want %q",
			got.Name, "policy_audit_refactor")
	}
	if _, err := os.Stat(got.UpPath); err != nil {
		t.Errorf("up path %q: %v", got.UpPath, err)
	}
	if _, err := os.Stat(got.DownPath); err != nil {
		t.Errorf("down path %q: %v", got.DownPath, err)
	}
}

// TestPolicyAuditRefactorUpSQLBodyMentionsExpectedObjects pins the
// structural shape of the Stage 1.4 `0003_policy_audit_refactor.up.sql`
// migration against the architecture canon (Sec 5.3 -- 5.5) and the
// implementation-plan Stage 1.4 brief (lines 111-122).
//
// String-matching at the SQL-source level keeps these structural
// invariants enforced on every laptop `go test ./...` pass without
// requiring a live PostgreSQL handle. The contract this test pins:
//
//   - Twelve tables: rule, rule_pack, policy_version, policy_activation,
//     threshold, override (Policy); evaluation_run, evaluation_verdict,
//     finding (Audit); hot_spot, refactor_plan, refactor_task (Refactor).
//   - No invented tables (rule_pack_revision, policy_override,
//     audit_event, audit_anchor, effort_estimate) -- iter-1 evaluator
//     item 1 rejected these.
//   - Six ENUM types covering the closed-set columns the architecture
//     pins as enums.
//   - Canonical enum label sets exactly match the architecture canon
//     (no `regression|improvement|flat` on finding_delta; no
//     `fail|gated` on evaluation_verdict_value; no `extract_function`
//     / `introduce_interface` / etc. on refactor_task_kind).
//   - PolicyVersion / PolicyActivation column shapes match the brief
//     verbatim -- absences (signed_at, signing_key_id, scope,
//     deactivated_at) are pinned via `!strings.Contains` checks on
//     the code-only view of the SQL.
//   - Override CHECK constraint requires `reason` when `mute=true`
//     (architecture Sec 5.3.6 line 1169) and there is NO `expires_at`
//     column (tech-spec Sec 10A latest-row-wins).
//   - EvaluationVerdict CHECK constrains `degraded_reason` to the
//     four-value architecture Sec 8.2 closed set.
//   - Audit table COMMENTs name the Audit WAL (architecture Sec 7.10)
//     as the durability mechanism per the implementation-plan brief.
//   - RefactorTask has NO `status` and NO `expected_metric_delta`
//     column (architecture Sec 5.5.3 / e2e-scenarios canon-guard).
func TestPolicyAuditRefactorUpSQLBodyMentionsExpectedObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0003_policy_audit_refactor.up.sql"))
	if err != nil {
		t.Fatalf("read up.sql: %v", err)
	}
	// Normalise CRLF -> LF so multi-line label-set matchers behave
	// identically on Windows and Linux checkouts (mirrors the
	// Stage 1.2 test above).
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")

	wantSubstrs := []string{
		// ENUM declarations (architecture Sec 5.3 -- 5.5 closed sets).
		"CREATE TYPE clean_code.rule_severity",
		"CREATE TYPE clean_code.finding_delta",
		"CREATE TYPE clean_code.evaluation_run_caller",
		"CREATE TYPE clean_code.evaluation_verdict_value",
		"CREATE TYPE clean_code.refactor_task_kind",
		"CREATE TYPE clean_code.threshold_op",
		// Policy sub-store tables (architecture Sec 5.3).
		"CREATE TABLE clean_code.rule_pack",
		"CREATE TABLE clean_code.rule",
		"CREATE TABLE clean_code.policy_version",
		"CREATE TABLE clean_code.policy_activation",
		"CREATE TABLE clean_code.threshold",
		"CREATE TABLE clean_code.override",
		// Audit sub-store tables (architecture Sec 5.4).
		"CREATE TABLE clean_code.evaluation_run",
		"CREATE TABLE clean_code.evaluation_verdict",
		"CREATE TABLE clean_code.finding",
		// Refactor sub-store tables (architecture Sec 5.5).
		"CREATE TABLE clean_code.hot_spot",
		"CREATE TABLE clean_code.refactor_plan",
		"CREATE TABLE clean_code.refactor_task",
		// Hot-read indexes pinned by the implementation-plan brief.
		"policy_activation_created_at_idx",
		"override_rule_created_idx",
		"evaluation_verdict_run_idx",
		"finding_run_idx",
		"finding_repo_sha_idx",
		"hot_spot_repo_sha_idx",
		"refactor_task_plan_idx",
		// PolicyVersion column shape (architecture Sec 5.3.3
		// lines 1126-1132 + Stage 1.4 brief lines 112-113).
		"policy_version_id  uuid         PRIMARY KEY DEFAULT gen_random_uuid()",
		"rule_refs          jsonb        NOT NULL",
		"threshold_refs     jsonb        NOT NULL",
		"refactor_weights   jsonb        NOT NULL",
		"signature          bytea        NOT NULL",
		// PolicyActivation column shape (architecture Sec 5.3.4
		// + Stage 1.4 brief line 113).
		"activation_id      uuid         PRIMARY KEY DEFAULT gen_random_uuid()",
		"activated_by       text         NOT NULL",
		// Override CHECK constraint enforcing
		// "Required when mute=true" (architecture Sec 5.3.6 line 1169).
		"override_reason_required_when_muted",
		"mute = false OR reason IS NOT NULL",
		// EvaluationVerdict CHECK enforcing the architecture Sec 8.2
		// closed degraded_reason set (Stage 1.4 brief line 130 pin).
		"evaluation_verdict_degraded_reason_canonical",
		// Threshold scope_kind closed set enforced via CHECK
		// (cross-stage enum sharing per header note).
		"threshold_scope_kind_canonical",
		// Override.actor_id (NOT `created_by`, per Stage 1.4
		// brief line 114).
		"actor_id      text         NOT NULL",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(sql, want) {
			t.Errorf("up.sql missing required substring %q", want)
		}
	}

	// Stage 1.4 brief line 121 mandates table COMMENTs on
	// `evaluation_run`, `evaluation_verdict`, `finding` that name
	// the Audit WAL as their durability mechanism. The COMMENT ON
	// statements wrap their prose across SQL string-literal
	// concatenations, so we just pin the two key phrases as
	// independent substrings rather than a single contiguous one.
	wantAuditCommentSubstrs := []string{
		"Audit WAL", // architecture Sec 7.1
		"Sec 7.10",  // architecture Sec 7.10 (immutability anchor)
	}
	for _, want := range wantAuditCommentSubstrs {
		if !strings.Contains(sql, want) {
			t.Errorf("up.sql missing required Audit WAL COMMENT phrase %q "+
				"(Stage 1.4 brief line 121)", want)
		}
	}

	// Canonical enum label sets (architecture Sec 5.3 -- 5.5).
	// Drift-detection pins on the exact ordering + spelling the
	// architecture canon mandates; reordering or relabelling any
	// of these fails this assertion. Includes the canon-guard
	// rejections from iter-1 / iter-3 evaluator items
	// (e2e-scenarios canonical-name pins).
	wantLabelSets := []string{
		// finding_delta: canonical `new | newly_failing |
		// unchanged | resolved` (NOT `regression | improvement |
		// flat`). Iter-3 drift item 7 canon-guard.
		"'new',\n    'newly_failing',\n    'unchanged',\n    'resolved'",
		// evaluation_verdict_value: canonical `pass | warn |
		// block` (NOT `fail | gated`). Iter-1 evaluator item 6.
		"'pass',\n    'warn',\n    'block'",
		// rule_severity: shared with `finding.severity` per
		// architecture Sec 5.3.1 + Sec 5.4.1.
		"'info',\n    'warn',\n    'block'",
		// evaluation_run_caller: architecture Sec 5.4.2 line 1201.
		"'eval_gate',\n    'batch_refresh'",
		// refactor_task_kind: five canonical values per
		// architecture Sec 5.5.3 line 1247 (the implementation-
		// plan brief LIMITS v1 to these five; iter-1 evaluator
		// item rejected `extract_function`/`introduce_interface`
		// /etc.).
		"'split_class',\n    'extract_method',\n    'invert_dependency',\n    'break_cycle',\n    'consolidate_duplication'",
		// threshold_op: five-value relational operator set per
		// architecture Sec 5.3.5 line 1157.
		"'gt',\n    'ge',\n    'lt',\n    'le',\n    'eq'",
	}
	for _, want := range wantLabelSets {
		if !strings.Contains(sql, want) {
			t.Errorf("up.sql missing canonical enum label set:\n----\n%s\n----", want)
		}
	}

	// Canon-guard absences: substrings that MUST NOT appear in
	// the DDL portion of up.sql (we strip `--` comments AND
	// single-quoted string literal payloads before searching so
	// explanatory COMMENT ON ... 'this column does NOT exist'
	// prose does not trigger false positives). A regression here
	// is a structural drift away from the architecture canon, so
	// the assertion is mandatory.
	codeOnly := sqlCodeOnly(sql)
	dontWantSubstrs := []string{
		// Iter-1 evaluator item 1: invented tables.
		"CREATE TABLE clean_code.rule_pack_revision",
		"CREATE TABLE clean_code.policy_override",
		"CREATE TABLE clean_code.audit_event",
		"CREATE TABLE clean_code.audit_anchor",
		"CREATE TABLE clean_code.effort_estimate",
		// Stage 1.4 brief line 112: PolicyVersion absences.
		"signed_at",
		"signing_key_id",
		"rulepack_set_hash",
		// Stage 1.4 brief line 113: PolicyActivation absences
		// (no `deactivated_at`; no `scope` column either, but
		// `scope` alone is too noisy as a substring -- the
		// `scope_kind`, `scope_id`, `scope_filter` columns on
		// other tables legitimately contain it).
		"deactivated_at",
		// Stage 1.4 brief line 114: Override absences.
		"expires_at",
		// Stage 1.4 brief line 118: RefactorTask absences.
		"expected_metric_delta",
		// finding_delta canon-guard (architecture Sec 5.4.1 line
		// 1189 + Stage 1.4 brief line 116): the values
		// `regression | improvement | flat` are explicitly NOT
		// canonical. The trailing quote pins the literal form
		// inside the CREATE TYPE label list.
		"'regression'",
		"'improvement'",
		// evaluation_verdict_value canon-guard: `fail` and
		// `gated` are NOT canonical (iter-1 evaluator item 6).
		"'fail',",
		"'gated',",
		// refactor_task_kind canon-guard: the rejected forms
		// listed in the Stage 1.4 brief line 118.
		"'extract_function'",
		"'introduce_interface'",
		"'reduce_inheritance'",
		"'reduce_coupling'",
		"'reduce_lcom'",
		"'reduce_duplication'",
	}
	for _, dont := range dontWantSubstrs {
		if strings.Contains(codeOnly, dont) {
			t.Errorf("up.sql declares disallowed substring %q in DDL "+
				"(after stripping comments + string literals); this "+
				"regresses the Stage 1.4 canon-guard", dont)
		}
	}

	// PolicyActivation must NOT have a partial unique index pinning
	// "at most one currently active row" -- the architecture canon
	// uses latest-row-wins, not partial uniqueness (Stage 1.4 brief
	// line 113). The check uses the code-only view so a CREATE
	// UNIQUE INDEX ... WHERE in a COMMENT does not trigger.
	lines := strings.Split(codeOnly, "\n")
	for i, line := range lines {
		if !strings.Contains(line, "CREATE UNIQUE INDEX") {
			continue
		}
		// Look ahead within the next ~5 lines for `WHERE` +
		// `policy_activation` so the heuristic only fires on a
		// real partial unique on the activation table.
		end := i + 6
		if end > len(lines) {
			end = len(lines)
		}
		window := strings.Join(lines[i:end], "\n")
		if strings.Contains(window, "policy_activation") &&
			strings.Contains(window, "WHERE") {
			t.Errorf("up.sql declares a partial UNIQUE INDEX on " +
				"policy_activation: the architecture canon (Sec " +
				"5.3.4) uses latest-row-wins, NOT partial-unique")
		}
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

// TestPolicyAuditRefactorDownSQLDropsObjects pins the per-object
// DROP shape of `0003_policy_audit_refactor.down.sql`. Unlike the
// Stage 1.2 down half (which does `DROP SCHEMA ... CASCADE` because
// it owns the schema), Stage 1.4 only drops its own twelve tables
// + six ENUMs so a partial rollback can leave Stage 1.2 catalog
// tables in place. The drop order MUST be the reverse of CREATE
// order so FK constraints unwind cleanly without CASCADE.
func TestPolicyAuditRefactorDownSQLDropsObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0003_policy_audit_refactor.down.sql"))
	if err != nil {
		t.Fatalf("read down.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")

	wantSubstrs := []string{
		// Refactor sub-store DROPs (reverse of CREATE order).
		"DROP TABLE IF EXISTS clean_code.refactor_task",
		"DROP TABLE IF EXISTS clean_code.refactor_plan",
		"DROP TABLE IF EXISTS clean_code.hot_spot",
		// Audit sub-store DROPs.
		"DROP TABLE IF EXISTS clean_code.finding",
		"DROP TABLE IF EXISTS clean_code.evaluation_verdict",
		"DROP TABLE IF EXISTS clean_code.evaluation_run",
		// Policy sub-store DROPs.
		"DROP TABLE IF EXISTS clean_code.override",
		"DROP TABLE IF EXISTS clean_code.threshold",
		"DROP TABLE IF EXISTS clean_code.policy_activation",
		"DROP TABLE IF EXISTS clean_code.policy_version",
		"DROP TABLE IF EXISTS clean_code.rule",
		"DROP TABLE IF EXISTS clean_code.rule_pack",
		// ENUM type DROPs.
		"DROP TYPE IF EXISTS clean_code.threshold_op",
		"DROP TYPE IF EXISTS clean_code.refactor_task_kind",
		"DROP TYPE IF EXISTS clean_code.evaluation_verdict_value",
		"DROP TYPE IF EXISTS clean_code.evaluation_run_caller",
		"DROP TYPE IF EXISTS clean_code.finding_delta",
		"DROP TYPE IF EXISTS clean_code.rule_severity",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(sql, want) {
			t.Errorf("down.sql missing required substring %q", want)
		}
	}

	// Stage 1.4 must NOT do a `DROP SCHEMA ... CASCADE` -- that
	// belongs to Stage 1.2's down half. Stage 1.4 only owns its
	// own twelve tables + six ENUMs; dropping the schema would
	// blow away Stage 1.2's catalog tables and break a partial
	// rollback to the Stage 1.2 state. We check the code-only
	// view so a `-- DROP SCHEMA ... CASCADE was considered and
	// rejected because ...` comment does not trip the assertion.
	if strings.Contains(sqlCodeOnly(sql), "DROP SCHEMA") {
		t.Errorf("down.sql contains DDL `DROP SCHEMA` -- Stage 1.4 must " +
			"only drop its own tables + types, not the whole schema")
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

// TestDiscoverMigrations_findsStage15Pair is the load-bearing
// assertion for the Stage 1.5 implementation step "Add
// migrations/0004_roles.up.sql ... and matching
// 0004_roles.down.sql". A test failure here means the directory
// layout regressed below the structural minimum the Stage 1.5
// brief locks in (the role grants migration disappeared, was
// renamed, or lost its down half).
func TestDiscoverMigrations_findsStage15Pair(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	migs, err := DiscoverMigrations(dir)
	if err != nil {
		t.Fatalf("DiscoverMigrations(%q): %v", dir, err)
	}
	var got *Migration
	for i := range migs {
		if migs[i].Version == "0004" {
			got = &migs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("DiscoverMigrations(%q) missing the Stage 1.5 "+
			"0004_roles pair", dir)
	}
	if got.Name != "roles" {
		t.Errorf("Stage 1.5 migration name = %q, want %q",
			got.Name, "roles")
	}
	if _, err := os.Stat(got.UpPath); err != nil {
		t.Errorf("up path %q: %v", got.UpPath, err)
	}
	if _, err := os.Stat(got.DownPath); err != nil {
		t.Errorf("down path %q: %v", got.DownPath, err)
	}
}

// TestRolesUpSQLBodyMentionsExpectedObjects pins the structural
// shape of the Stage 1.5 `0004_roles.up.sql` migration against
// tech-spec Sec 7.2 lines 1232-1261 (canonical role names) and
// the C5 ACL table at lines 690-696. The contract this test
// pins:
//
//   - All nine canonical roles are created
//     (`clean_code_repo_indexer`, `clean_code_metric_ingestor`,
//     `clean_code_xrepo_aggregator`, `clean_code_policy_steward`,
//     `clean_code_solid_batch`, `clean_code_evaluator`,
//     `clean_code_wal_reconciler`, `clean_code_refactor_planner`,
//     `clean_code_management`).
//   - No alias names (`clean_code_aggregator`,
//     `clean_code_rule_engine`, `clean_code_audit_replayer`)
//     appear in the DDL -- they are NEVER created in v1.
//   - CREATE ROLE statements use the race-safe DO-block pattern
//     catching `duplicate_object` (no plain `CREATE ROLE` lines
//     that would fail under concurrent test schemas).
//   - The two-writer carve-out grants (tech-spec Sec 7.2 lines
//     1212-1248) are issued to BOTH `clean_code_metric_ingestor`
//     and `clean_code_xrepo_aggregator` on `metric_sample`,
//     `metric_retraction`, and `metric_sample_active`.
//   - Metric Ingestor receives column-level UPDATE on
//     `commit.scan_status` (sole writer per arch Sec 1.5.1 row 1).
//   - Repo Indexer receives INSERT on `commit` (initial row only)
//     and INSERT on `repo_event`.
//   - Policy Steward receives INSERT on each of the six Policy
//     tables (`rule`, `rule_pack`, `policy_version`,
//     `policy_activation`, `threshold`, `override`).
//   - The three Audit tables (`evaluation_run`,
//     `evaluation_verdict`, `finding`) carry INSERT grants for
//     all three writer roles in parallel.
//   - G3 / C2 / C25 immutability REVOKEs are present
//     (`metric_sample` no UPDATE/DELETE, retraction +
//     active no DELETE, snapshot tables no UPDATE/DELETE,
//     Audit tables no UPDATE/DELETE).
//   - Refactor Planner receives INSERT on the three Refactor
//     tables (`hot_spot`, `refactor_plan`, `refactor_task`).
func TestRolesUpSQLBodyMentionsExpectedObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0004_roles.up.sql"))
	if err != nil {
		t.Fatalf("read up.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	// `code` is the SQL with `--` line comments + single-quoted
	// string-literal payloads stripped. Forbidden-alias absence
	// checks run against `code` so a documentary mention of a
	// rejected alias inside a `--` rationale comment does not
	// trigger a false positive.
	code := sqlCodeOnly(sql)

	wantSubstrs := []string{
		// All nine canonical roles created with the race-safe
		// DO-block pattern (tech-spec Sec 7.2 lines 1232-1261).
		"CREATE ROLE clean_code_repo_indexer NOLOGIN",
		"CREATE ROLE clean_code_metric_ingestor NOLOGIN",
		"CREATE ROLE clean_code_xrepo_aggregator NOLOGIN",
		"CREATE ROLE clean_code_policy_steward NOLOGIN",
		"CREATE ROLE clean_code_solid_batch NOLOGIN",
		"CREATE ROLE clean_code_evaluator NOLOGIN",
		"CREATE ROLE clean_code_wal_reconciler NOLOGIN",
		"CREATE ROLE clean_code_refactor_planner NOLOGIN",
		"CREATE ROLE clean_code_management NOLOGIN",
		// Race-safe shape: every CREATE ROLE catches
		// `duplicate_object` so concurrent test schemas don't
		// abort the migration on the loser-of-the-race.
		"EXCEPTION WHEN duplicate_object THEN NULL",
		// Schema USAGE for every writer role.
		"GRANT USAGE ON SCHEMA clean_code TO clean_code_repo_indexer",
		"GRANT USAGE ON SCHEMA clean_code TO clean_code_management",
		// ----- Catalog / Lifecycle (C5 row 1) -----
		// Management: BOTH INSERT and UPDATE are column-level on
		// the Management-owned columns ONLY. INSERT names
		// (repo_id, display_name, mode, default_branch); UPDATE
		// names (display_name, mode, default_branch).
		// `default_branch_head` is excluded from BOTH grants per
		// architecture Sec 5.1.1 line 854 / Sec 1.5.1 row 1 pin
		// (Repo Indexer is the sole writer of that column);
		// `created_at` is excluded so Management cannot backdate.
		"GRANT INSERT (repo_id, display_name, mode, default_branch) ON clean_code.repo       TO clean_code_management",
		"GRANT UPDATE (display_name, mode, default_branch)          ON clean_code.repo       TO clean_code_management",
		"GRANT INSERT                                               ON clean_code.repo_event TO clean_code_management",
		// Repo Indexer: column-level INSERT on `commit` excludes
		// `scan_status` per architecture Sec 5.1.2 line 864 / Sec
		// 1.5.1 row 1 -- the schema-level DEFAULT supplies
		// 'pending'. Indexer also UPDATEs `repo.default_branch_head`
		// only (Management-owned columns are off-limits).
		"GRANT INSERT (repo_id, sha, parent_sha, committed_at) ON clean_code.commit      TO clean_code_repo_indexer",
		"GRANT INSERT                                          ON clean_code.repo_event  TO clean_code_repo_indexer",
		"GRANT UPDATE (default_branch_head)                    ON clean_code.repo        TO clean_code_repo_indexer",
		// Metric Ingestor sole UPDATE of commit.scan_status (column-level).
		"GRANT UPDATE (scan_status) ON clean_code.commit   TO clean_code_metric_ingestor",
		"GRANT INSERT, UPDATE       ON clean_code.scan_run TO clean_code_metric_ingestor",
		// Policy Steward owns the MetricKind catalogue rows.
		"GRANT INSERT ON clean_code.metric_kind TO clean_code_policy_steward",
		// ----- Measurement -- foundation tier (C5 row 2) -----
		"GRANT INSERT, SELECT         ON clean_code.scope_binding        TO clean_code_metric_ingestor",
		"GRANT INSERT, SELECT         ON clean_code.metric_sample        TO clean_code_metric_ingestor",
		"GRANT INSERT, SELECT         ON clean_code.metric_retraction    TO clean_code_metric_ingestor",
		"GRANT INSERT, SELECT, UPDATE ON clean_code.metric_sample_active TO clean_code_metric_ingestor",
		// ----- Measurement -- system-tier carve-out (C5 row 3) -----
		"GRANT INSERT, SELECT         ON clean_code.metric_sample          TO clean_code_xrepo_aggregator",
		"GRANT INSERT, SELECT         ON clean_code.metric_retraction      TO clean_code_xrepo_aggregator",
		"GRANT INSERT, SELECT, UPDATE ON clean_code.metric_sample_active   TO clean_code_xrepo_aggregator",
		"GRANT INSERT, SELECT         ON clean_code.repo_metric_snapshot   TO clean_code_xrepo_aggregator",
		"GRANT INSERT, SELECT         ON clean_code.cross_repo_percentile  TO clean_code_xrepo_aggregator",
		"GRANT INSERT, SELECT         ON clean_code.portfolio_snapshot     TO clean_code_xrepo_aggregator",
		// G3 / C2 row immutability REVOKEs (tech-spec Sec 7.2
		// lines 1362-1367 verbatim).
		"REVOKE UPDATE, DELETE ON clean_code.metric_sample          FROM PUBLIC, clean_code_metric_ingestor, clean_code_xrepo_aggregator",
		"REVOKE DELETE         ON clean_code.metric_retraction      FROM PUBLIC, clean_code_metric_ingestor, clean_code_xrepo_aggregator",
		"REVOKE DELETE         ON clean_code.metric_sample_active   FROM PUBLIC, clean_code_metric_ingestor, clean_code_xrepo_aggregator",
		"REVOKE UPDATE, DELETE ON clean_code.repo_metric_snapshot   FROM PUBLIC, clean_code_xrepo_aggregator",
		"REVOKE UPDATE, DELETE ON clean_code.cross_repo_percentile  FROM PUBLIC, clean_code_xrepo_aggregator",
		"REVOKE UPDATE, DELETE ON clean_code.portfolio_snapshot     FROM PUBLIC, clean_code_xrepo_aggregator",
		// ----- Policy (C5 row 4) -----
		"GRANT INSERT ON clean_code.rule              TO clean_code_policy_steward",
		"GRANT INSERT ON clean_code.rule_pack         TO clean_code_policy_steward",
		"GRANT INSERT ON clean_code.policy_version    TO clean_code_policy_steward",
		"GRANT INSERT ON clean_code.policy_activation TO clean_code_policy_steward",
		"GRANT INSERT ON clean_code.threshold         TO clean_code_policy_steward",
		"GRANT INSERT ON clean_code.override          TO clean_code_policy_steward",
		// ----- Audit / verdict (C5 row 5; tech-spec Sec 7.2
		// lines 1374-1377 -- three writers in parallel) -----
		"GRANT INSERT, SELECT ON clean_code.evaluation_run     TO clean_code_evaluator",
		"GRANT INSERT, SELECT ON clean_code.evaluation_run     TO clean_code_solid_batch",
		"GRANT INSERT, SELECT ON clean_code.evaluation_run     TO clean_code_wal_reconciler",
		"GRANT INSERT, SELECT ON clean_code.evaluation_verdict TO clean_code_evaluator",
		"GRANT INSERT, SELECT ON clean_code.evaluation_verdict TO clean_code_solid_batch",
		"GRANT INSERT, SELECT ON clean_code.evaluation_verdict TO clean_code_wal_reconciler",
		"GRANT INSERT, SELECT ON clean_code.finding            TO clean_code_evaluator",
		"GRANT INSERT, SELECT ON clean_code.finding            TO clean_code_solid_batch",
		"GRANT INSERT, SELECT ON clean_code.finding            TO clean_code_wal_reconciler",
		"REVOKE UPDATE, DELETE ON clean_code.evaluation_run     FROM PUBLIC, clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler",
		"REVOKE UPDATE, DELETE ON clean_code.evaluation_verdict FROM PUBLIC, clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler",
		"REVOKE UPDATE, DELETE ON clean_code.finding            FROM PUBLIC, clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler",
		// ----- Refactor (C5 row 6) -----
		"GRANT INSERT ON clean_code.hot_spot       TO clean_code_refactor_planner",
		"GRANT INSERT ON clean_code.refactor_plan  TO clean_code_refactor_planner",
		"GRANT INSERT ON clean_code.refactor_task  TO clean_code_refactor_planner",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(sql, want) {
			t.Errorf("0004 up.sql missing required substring %q", want)
		}
	}

	// Forbidden alias absence: tech-spec Sec 7.2 lines 1232-1261
	// pins the canonical role names ONLY. The three names below
	// are NEVER created in v1; checking against `code` (with
	// comments + string literals stripped) keeps explanatory
	// prose in COMMENTs / rationale lines from triggering a false
	// positive while still catching an accidental
	// `CREATE ROLE clean_code_aggregator` in the DDL itself.
	notWantSubstrs := []string{
		"clean_code_aggregator",
		"clean_code_rule_engine",
		"clean_code_audit_replayer",
	}
	for _, notWant := range notWantSubstrs {
		if strings.Contains(code, notWant) {
			t.Errorf("0004 up.sql DDL contains forbidden alias %q -- "+
				"only the nine canonical role names per tech-spec "+
				"Sec 7.2 lines 1232-1261 are permitted", notWant)
		}
	}

	// The down half MUST NOT use a plain CREATE ROLE (race
	// hazard under concurrent test schemas -- see the file
	// header rationale). Every CREATE ROLE in the DDL must be
	// wrapped in a DO block. We check by counting -- the only
	// way to safely have N CREATE ROLE lines in the file is via
	// N DO blocks each catching duplicate_object.
	//
	// `sqlNoComments` strips `--` line comments only (NOT string
	// literals); we deliberately avoid the full `sqlCodeOnly`
	// view here because that view's literal-stripper enters
	// "in-string" state on the first single quote and exits on
	// the next single quote, which an apostrophe-in-comment
	// (e.g. "Management's delegated") would unbalance, silently
	// swallowing DDL lines between two apostrophes. Comment
	// stripping alone is sufficient because the count targets
	// (`CREATE ROLE`, `EXCEPTION WHEN duplicate_object`) never
	// appear inside SQL string literals in this migration.
	sqlNoComments := stripSQLComments(sql)
	createRoleCount := strings.Count(sqlNoComments, "CREATE ROLE")
	doBlockCount := strings.Count(sqlNoComments, "EXCEPTION WHEN duplicate_object THEN NULL")
	if createRoleCount != doBlockCount {
		t.Errorf("0004 up.sql: CREATE ROLE count = %d, DO-block "+
			"duplicate_object handler count = %d (every CREATE "+
			"ROLE must be wrapped in a race-safe DO block per "+
			"the file header)",
			createRoleCount, doBlockCount)
	}
	if createRoleCount != 9 {
		t.Errorf("0004 up.sql: CREATE ROLE count = %d, want 9 "+
			"(the nine canonical roles per tech-spec Sec 7.2 "+
			"lines 1232-1261)", createRoleCount)
	}
}

// TestRolesDownSQLRevokesGrants verifies the Stage 1.5 down half
// REVOKEs every grant the up half issued and does NOT DROP the
// nine writer roles. Roles are cluster-wide; a DROP ROLE in this
// migration would (a) fail under SQLSTATE 2BP01 when sibling
// tenants hold grants and (b) take the roles away from concurrent
// test schemas that are still using them.
func TestRolesDownSQLRevokesGrants(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0004_roles.down.sql"))
	if err != nil {
		t.Fatalf("read down.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	code := sqlCodeOnly(sql)

	wantRevokes := []string{
		// Symmetric REVOKE for each up-half GRANT (we only
		// pin the load-bearing ones -- the full set lives in
		// the up.sql test).
		"REVOKE INSERT ON clean_code.hot_spot       FROM clean_code_refactor_planner",
		"REVOKE INSERT ON clean_code.evaluation_run     FROM clean_code_evaluator",
		"REVOKE INSERT ON clean_code.evaluation_run     FROM clean_code_solid_batch",
		"REVOKE INSERT ON clean_code.evaluation_run     FROM clean_code_wal_reconciler",
		"REVOKE INSERT ON clean_code.finding            FROM clean_code_wal_reconciler",
		"REVOKE INSERT ON clean_code.rule_pack         FROM clean_code_policy_steward",
		"REVOKE INSERT ON clean_code.policy_version    FROM clean_code_policy_steward",
		"REVOKE INSERT ON clean_code.metric_kind FROM clean_code_policy_steward",
		"REVOKE INSERT         ON clean_code.metric_sample        FROM clean_code_metric_ingestor",
		"REVOKE INSERT         ON clean_code.metric_sample          FROM clean_code_xrepo_aggregator",
		"REVOKE INSERT, UPDATE ON clean_code.metric_sample_active   FROM clean_code_xrepo_aggregator",
		"REVOKE INSERT, UPDATE ON clean_code.metric_sample_active FROM clean_code_metric_ingestor",
		"REVOKE UPDATE (scan_status) ON clean_code.commit   FROM clean_code_metric_ingestor",
		"REVOKE INSERT (repo_id, sha, parent_sha, committed_at) ON clean_code.commit      FROM clean_code_repo_indexer",
		"REVOKE INSERT                                          ON clean_code.repo_event  FROM clean_code_repo_indexer",
		"REVOKE UPDATE (default_branch_head)                    ON clean_code.repo        FROM clean_code_repo_indexer",
		"REVOKE INSERT (repo_id, display_name, mode, default_branch) ON clean_code.repo       FROM clean_code_management",
		"REVOKE UPDATE (display_name, mode, default_branch)          ON clean_code.repo       FROM clean_code_management",
		"REVOKE INSERT                                               ON clean_code.repo_event FROM clean_code_management",
		// Symmetric schema-USAGE REVOKE for every role.
		"REVOKE USAGE ON SCHEMA clean_code FROM clean_code_repo_indexer",
		"REVOKE USAGE ON SCHEMA clean_code FROM clean_code_management",
	}
	for _, want := range wantRevokes {
		if !strings.Contains(sql, want) {
			t.Errorf("0004 down.sql missing required REVOKE %q", want)
		}
	}

	// Down half MUST NOT drop roles (cluster-wide -- see the
	// file header rationale). A regression here would break
	// concurrent test schemas that share the cluster.
	if strings.Contains(code, "DROP ROLE") {
		t.Errorf("0004 down.sql contains `DROP ROLE` -- roles are " +
			"cluster-wide and a drop would fail (SQLSTATE 2BP01) " +
			"when sibling tenants hold grants. The down half MUST " +
			"REVOKE only.")
	}
	// Down half MUST NOT drop the schema CASCADE (that's
	// 0001_catalog_lifecycle.down.sql's job; 0004 unwinds only
	// its own grants).
	if strings.Contains(code, "DROP SCHEMA") {
		t.Errorf("0004 down.sql contains `DROP SCHEMA` -- only the " +
			"0001 down half should DROP SCHEMA CASCADE; 0004 unwinds " +
			"only the role grants it added.")
	}
}

// TestDiscoverMigrations_findsStage51Pair pins the Stage 5.1
// implementation step "Add migrations/0005_policy_signing_keys.up.sql
// ... and matching 0005_policy_signing_keys.down.sql".
func TestDiscoverMigrations_findsStage51Pair(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	migs, err := DiscoverMigrations(dir)
	if err != nil {
		t.Fatalf("DiscoverMigrations(%q): %v", dir, err)
	}
	var got *Migration
	for i := range migs {
		if migs[i].Version == "0005" {
			got = &migs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("DiscoverMigrations(%q) missing the Stage 5.1 "+
			"0005_policy_signing_keys pair", dir)
	}
	if got.Name != "policy_signing_keys" {
		t.Errorf("Stage 5.1 migration name = %q, want %q",
			got.Name, "policy_signing_keys")
	}
	if _, err := os.Stat(got.UpPath); err != nil {
		t.Errorf("up path %q: %v", got.UpPath, err)
	}
	if _, err := os.Stat(got.DownPath); err != nil {
		t.Errorf("down path %q: %v", got.DownPath, err)
	}
}

// TestPolicySigningKeysUpSQLBodyMentionsExpectedObjects pins the
// structural shape of the Stage 5.1 `0005_policy_signing_keys.up.sql`
// migration against tech-spec Sec 8.4 (key storage canon) +
// Sec 7.2 / C5 (writer isolation). The contract this test
// pins:
//
//   - Exactly one table created: `clean_code.policy_signing_keys`.
//   - Columns: `key_id`, `fingerprint`, `public_key`,
//     `key_handle`, `valid_from`, `algorithm`.
//   - PRIVATE-KEY ABSENCE: the migration MUST NOT include
//     `private_key`, `secret_key`, `sealed_key`, `signing_secret`,
//     `wrapped_key`, or any other column that could hold the
//     Ed25519 private bytes -- tech-spec Sec 8.4 line 944
//     explicitly forbids storing the private key here.
//   - VALID_UNTIL ABSENCE: the upper-bound of a key's validity
//     is DERIVED at read time per the in-package Manager (see
//     `computeValidUntil` in `internal/policy/keys/manager.go`).
//     A `valid_until` column would invite an UPDATE on rotation,
//     violating G3.
//   - CHECK constraints enforce: fingerprint = 64 lowercase hex
//     chars, public_key length = 32 bytes (Ed25519), algorithm
//     in {ed25519}.
//   - PolicyVersion / PolicyActivation column shape from 0003
//     remains UNCHANGED (this migration does not add a
//     `signing_key_id` FK to policy_version per implementation-
//     plan canon line 112).
//   - Privilege grants: clean_code_policy_steward gets
//     INSERT+SELECT; every other writer role gets SELECT only;
//     UPDATE / DELETE is REVOKEd from PUBLIC and every writer
//     role per G3.
//   - The canonical `(valid_from DESC, key_id DESC)` index is
//     declared so the Evaluator's per-`eval.gate` lookup is a
//     single index range scan.
func TestPolicySigningKeysUpSQLBodyMentionsExpectedObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0005_policy_signing_keys.up.sql"))
	if err != nil {
		t.Fatalf("read up.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	code := sqlCodeOnly(sql)

	wantSubstrs := []string{
		// Table + columns.
		"CREATE TABLE clean_code.policy_signing_keys",
		"key_id        uuid           PRIMARY KEY DEFAULT gen_random_uuid()",
		"fingerprint   text           NOT NULL UNIQUE",
		"public_key    bytea          NOT NULL",
		"key_handle    text           NOT NULL",
		"valid_from    timestamptz    NOT NULL DEFAULT now()",
		"algorithm     text           NOT NULL DEFAULT 'ed25519'",
		// CHECK constraints.
		"CONSTRAINT policy_signing_keys_fingerprint_shape CHECK",
		"^[0-9a-f]{64}$",
		"CONSTRAINT policy_signing_keys_public_key_ed25519_len CHECK",
		"octet_length(public_key) = 32",
		"CONSTRAINT policy_signing_keys_algorithm_canonical CHECK",
		"algorithm IN ('ed25519')",
		// Canonical lookup index.
		"CREATE INDEX policy_signing_keys_valid_from_idx",
		"(valid_from DESC, key_id DESC)",
		// Grants -- writer isolation per C5.
		"GRANT INSERT, SELECT ON clean_code.policy_signing_keys TO clean_code_policy_steward",
		"GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_evaluator",
		"GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_wal_reconciler",
		"GRANT SELECT ON clean_code.policy_signing_keys TO clean_code_management",
		// G3: append-only.
		"REVOKE UPDATE, DELETE ON clean_code.policy_signing_keys FROM PUBLIC",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(sql, want) {
			t.Errorf("0005 up.sql missing required substring %q", want)
		}
	}

	// Forbidden-column absence checks run against the
	// code-only view so documentary mentions inside `--`
	// comments (e.g. "The PRIVATE key NEVER lives in this
	// table") do not trip false positives.
	forbiddenColumns := []string{
		"private_key",
		"secret_key",
		"sealed_key",
		"signing_secret",
		"wrapped_key",
		// `valid_until` is derived in code, NOT stored
		// (storing it would require an UPDATE on rotation,
		// violating G3).
		"valid_until",
		// No `retired_at` either -- retirement is implicit
		// from the next key's `valid_from + overlap`.
		"retired_at",
	}
	for _, want := range forbiddenColumns {
		if strings.Contains(code, want) {
			t.Errorf("0005 up.sql contains forbidden column reference %q "+
				"(tech-spec Sec 8.4: private keys live in the operator's "+
				"secret manager; valid_until/retired_at are derived at read time)",
				want)
		}
	}

	// PolicyVersion / PolicyActivation MUST NOT gain a
	// signing_key_id FK in this stage (implementation-plan
	// canon line 112).
	if strings.Contains(code, "policy_version") && strings.Contains(code, "signing_key_id") {
		t.Errorf("0005 up.sql ties policy_version to signing_key_id; " +
			"the implementation-plan canon line 112 forbids that column.")
	}
}

// TestPolicySigningKeysDownSQLDropsOwnObjects pins the down
// half: drop the Stage-5.1 index + table only, leave 0004's
// roles and the schema intact.
func TestPolicySigningKeysDownSQLDropsOwnObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0005_policy_signing_keys.down.sql"))
	if err != nil {
		t.Fatalf("read down.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	wantDrops := []string{
		"DROP INDEX IF EXISTS clean_code.policy_signing_keys_valid_from_idx",
		"DROP TABLE IF EXISTS clean_code.policy_signing_keys",
	}
	for _, want := range wantDrops {
		if !strings.Contains(sql, want) {
			t.Errorf("0005 down.sql missing required DROP %q", want)
		}
	}
	// Must NOT drop roles or the schema.
	code := sqlCodeOnly(sql)
	if strings.Contains(code, "DROP ROLE") {
		t.Errorf("0005 down.sql contains DROP ROLE -- roles are " +
			"created by 0004 and are cluster-wide; 0005 down must " +
			"only unwind its own objects.")
	}
	if strings.Contains(code, "DROP SCHEMA") {
		t.Errorf("0005 down.sql contains DROP SCHEMA -- only " +
			"0001_catalog_lifecycle.down.sql should DROP SCHEMA.")
	}
}

// TestDiscoverMigrations_findsStage32Pair pins discovery of the
// Stage 3.2 `0006_repo_url` pair. iter-7 evaluator follow-up
// asked for direct migration round-trip coverage of the
// WRITE-ONCE trigger; this discovery test is the structural
// pre-condition: if the pair stops being discovered, every
// subsequent assertion below fails noisily rather than silently
// degrading to "schema is fine, just missing the column".
func TestDiscoverMigrations_findsStage32Pair(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	migs, err := DiscoverMigrations(dir)
	if err != nil {
		t.Fatalf("DiscoverMigrations(%q): %v", dir, err)
	}
	var got *Migration
	for i := range migs {
		if migs[i].Version == "0006" {
			got = &migs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("DiscoverMigrations(%q) missing the Stage 3.2 "+
			"0006_repo_url pair", dir)
	}
	if got.Name != "repo_url" {
		t.Errorf("Stage 3.2 migration name = %q, want %q",
			got.Name, "repo_url")
	}
	if _, err := os.Stat(got.UpPath); err != nil {
		t.Errorf("up path %q: %v", got.UpPath, err)
	}
	if _, err := os.Stat(got.DownPath); err != nil {
		t.Errorf("down path %q: %v", got.DownPath, err)
	}
}

// TestRepoURLUpSQLBodyMentionsExpectedObjects pins the
// structural shape of the Stage 3.2
// `0006_repo_url.up.sql` migration so a regression that
// silently drops the WRITE-ONCE trigger (iter-7 evaluator
// item 4) or a column-level grant (iter-7 evaluator item 2)
// surfaces in `go test ./...` -- BEFORE any developer ever
// gets a chance to run the live-PostgreSQL round-trip.
//
// The contract this test pins:
//
//   - The repo_url column is added as `text NULL` (back-compat
//     with pre-0006 rows; NULL is the "not yet back-filled"
//     state the application-side `PGRepoURLLookup` surfaces as
//     `ErrRepoURLLookupNotFound`).
//   - Management gets column-level INSERT + UPDATE grants on
//     `repo_url` (iter-6 evaluator item 1 -- canonical writer
//     for the column). The UPDATE grant is INTENTIONALLY
//     present despite the WRITE-ONCE policy because a DBA-run
//     back-fill UPDATE must work without superuser escalation;
//     the trigger below is the actual immutability guard.
//   - A `BEFORE UPDATE OF repo_url` trigger exists, backed by
//     a PL/pgSQL function that rejects post-set changes with
//     SQLSTATE 23514 (check_violation). This is the iter-7
//     evaluator item 4 defence-in-depth fix.
//   - The trigger function's guard is `IS DISTINCT FROM` (not
//     plain `!=`) so a NULL OLD value is treated as a back-fill
//     transition, and a `SET repo_url = repo_url` no-op is
//     allowed (some ORMs emit these).
//   - The function is `LANGUAGE plpgsql` (required for the
//     `IF ... RAISE` control flow).
func TestRepoURLUpSQLBodyMentionsExpectedObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0006_repo_url.up.sql"))
	if err != nil {
		t.Fatalf("read up.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	code := sqlCodeOnly(sql)

	wantSubstrs := []string{
		// Column shape.
		"ALTER TABLE clean_code.repo",
		"ADD COLUMN IF NOT EXISTS repo_url text NULL",
		// Column-level grants -- Management is the canonical
		// writer per C5 row 1; the iter-7 evaluator item 2 fix.
		"GRANT INSERT (repo_url) ON clean_code.repo TO clean_code_management",
		"GRANT UPDATE (repo_url) ON clean_code.repo TO clean_code_management",
		// WRITE-ONCE trigger function (iter-7 evaluator item 4).
		"CREATE OR REPLACE FUNCTION clean_code.tg_repo_url_write_once()",
		"RETURNS trigger",
		"LANGUAGE plpgsql",
		// The guard MUST use IS DISTINCT FROM so NULL handling
		// (back-fill) and the SET repo_url = repo_url no-op both
		// pass through. A plain `!=` would crash on NULLs and
		// drop a redundant set with an error.
		"IS DISTINCT FROM OLD.repo_url",
		// SQLSTATE 23514 (check_violation) is the canonical
		// shape; the application's PG driver maps this to
		// pq.Error.Code "23514" so callers can branch on it.
		"ERRCODE = '23514'",
		// Trigger wiring -- BEFORE UPDATE OF repo_url so the
		// firing surface is narrowed to the column we care
		// about (not every UPDATE on `repo`).
		"DROP TRIGGER IF EXISTS tg_repo_url_write_once ON clean_code.repo",
		"CREATE TRIGGER tg_repo_url_write_once",
		"BEFORE UPDATE OF repo_url ON clean_code.repo",
		"FOR EACH ROW",
		"EXECUTE FUNCTION clean_code.tg_repo_url_write_once()",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(sql, want) {
			t.Errorf("0006 up.sql missing required substring %q", want)
		}
	}

	// Forbidden patterns -- run against the code-only view so
	// documentary mentions inside `--` comments do not trip
	// false positives.
	forbiddenCodePatterns := []struct {
		pat    string
		reason string
	}{
		// Repo Indexer must NEVER gain INSERT/UPDATE on
		// repo_url -- Management owns this column (C5 row 1).
		// A future migration that accidentally extends the
		// grant trips this guard.
		{
			pat: "GRANT INSERT (repo_url) ON clean_code.repo TO clean_code_repo_indexer",
			reason: "Repo Indexer is NOT the writer for repo_url; " +
				"Management owns this column (C5 row 1).",
		},
		{
			pat: "GRANT UPDATE (repo_url) ON clean_code.repo TO clean_code_repo_indexer",
			reason: "Repo Indexer is NOT the writer for repo_url; " +
				"Management owns this column (C5 row 1).",
		},
		// Tightening to NOT NULL is reserved for the Stage 1.2
		// follow-up workstream (after a one-off back-fill).
		// Doing it in this migration would error on every
		// existing repo row in a populated catalog.
		{
			pat: "ALTER COLUMN repo_url SET NOT NULL",
			reason: "Tightening repo_url to NOT NULL is reserved " +
				"for the Stage 1.2 follow-up (after a DBA back-fill); " +
				"doing it here would error on every pre-0006 row.",
		},
	}
	for _, fp := range forbiddenCodePatterns {
		if strings.Contains(code, fp.pat) {
			t.Errorf("0006 up.sql contains forbidden pattern %q -- %s",
				fp.pat, fp.reason)
		}
	}
}

// TestRepoURLDownSQLDropsTriggerAndColumn pins the down half:
// the rollback MUST drop both the WRITE-ONCE trigger AND the
// function (PostgreSQL would cascade the trigger when the
// column drops, but naming both keeps the rollback idempotent
// and explicit). It MUST NOT touch the schema or roles --
// those belong to 0001 / 0004 respectively.
func TestRepoURLDownSQLDropsTriggerAndColumn(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "0006_repo_url.down.sql"))
	if err != nil {
		t.Fatalf("read down.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	wantDrops := []string{
		// Trigger + function dropped BEFORE the column so a
		// half-applied rollback (trigger gone, column still
		// there) leaves the rest of the down idempotent.
		"DROP TRIGGER IF EXISTS tg_repo_url_write_once ON clean_code.repo",
		"DROP FUNCTION IF EXISTS clean_code.tg_repo_url_write_once()",
		"ALTER TABLE clean_code.repo",
		"DROP COLUMN IF EXISTS repo_url",
	}
	for _, want := range wantDrops {
		if !strings.Contains(sql, want) {
			t.Errorf("0006 down.sql missing required DROP %q", want)
		}
	}
	// Must NOT drop roles or the schema -- those belong to
	// 0004 / 0001 respectively. Run against the code-only
	// view so documentary mentions inside `--` comments do
	// not trip false positives.
	code := sqlCodeOnly(sql)
	if strings.Contains(code, "DROP ROLE") {
		t.Errorf("0006 down.sql contains DROP ROLE -- roles are " +
			"created by 0004 and are cluster-wide; 0006 down must " +
			"only unwind its own objects.")
	}
	if strings.Contains(code, "DROP SCHEMA") {
		t.Errorf("0006 down.sql contains DROP SCHEMA -- only " +
			"0001_catalog_lifecycle.down.sql should DROP SCHEMA.")
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

	// Stage 1.4 live-PostgreSQL canon-guards.
	//
	// These subtests prove the architecture-Sec-5.3..5.5 canon
	// at the database level rather than via SQL source-text
	// matching (which the structural tests above already
	// cover). Each subtest seeds its own scratch repo +
	// policy_version + evaluation_run and tears them down on
	// completion so re-runs under `go test -count=N` stay
	// idempotent. They land here -- inside the env-gated
	// round-trip -- so a developer laptop without
	// `CLEAN_CODE_PG_URL` still gets `go test ./...` exit 0.
	t.Run("verdict-enum-only-canonical", func(t *testing.T) {
		assertVerdictEnumOnlyCanonical(t, psql, url)
	})
	t.Run("degraded-reason-closed-set", func(t *testing.T) {
		assertDegradedReasonClosedSet(t, psql, url)
	})
	t.Run("finding-delta-canonical", func(t *testing.T) {
		assertFindingDeltaCanonical(t, psql, url)
	})
	t.Run("refactor-task-no-status-column", func(t *testing.T) {
		assertRefactorTaskNoStatusColumn(t, psql, url)
	})
	t.Run("policy-activation-latest-row-wins", func(t *testing.T) {
		assertPolicyActivationLatestRowWins(t, psql, url)
	})
	t.Run("override-no-expires-column", func(t *testing.T) {
		assertOverrideNoExpiresColumn(t, psql, url)
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
	t.Run("policy-signing-keys-roundtrip", func(t *testing.T) {
		assertPolicySigningKeysRoundTrip(t, psql, url)
	})
	t.Run("policy-signing-keys-constraints", func(t *testing.T) {
		assertPolicySigningKeysConstraints(t, psql, url)
	})
	t.Run("policy-signing-keys-grants", func(t *testing.T) {
		assertPolicySigningKeysGrants(t, psql, url)
	})

	// Stage 3.2 ("Metric Ingestor and ScanRun state machine")
	// live-PG canon-guards for the iter-7 evaluator follow-up
	// (non-blocking item: migration round-trip coverage for
	// 0006_repo_url trigger/grants). These run inside the same
	// up/down round-trip so the WRITE-ONCE behavior is pinned
	// against the actual installed trigger, not just SQL
	// source-text.
	t.Run("repo-url-write-once-trigger", func(t *testing.T) {
		assertRepoURLWriteOnceTrigger(t, psql, url)
	})
	t.Run("repo-url-management-grants", func(t *testing.T) {
		assertRepoURLManagementGrants(t, psql, url)
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

// assertRepoURLWriteOnceTrigger pins the runtime behavior of
// the `tg_repo_url_write_once` trigger installed by
// 0006_repo_url.up.sql. The structural test
// (TestRepoURLUpSQLBodyMentionsExpectedObjects) pins the SQL
// source-text shape; this assertion pins what the trigger
// actually DOES against a live PostgreSQL:
//
//   - A row INSERTed with NULL repo_url stays NULL.
//   - The NULL -> non-NULL transition is allowed (one-shot
//     back-fill of pre-0006 rows).
//   - The redundant `SET repo_url = repo_url` no-op is allowed
//     (some ORMs emit these on partial-column updates; the
//     trigger's IS DISTINCT FROM guard short-circuits).
//   - Any change to a non-NULL repo_url is rejected with
//     SQLSTATE 23514 (check_violation). The rejection surfaces
//     the trigger's RAISE EXCEPTION message text.
//
// Each subtest seeds its own scratch repo and tears it down on
// completion so re-runs under `go test -count=N` stay
// idempotent.
//
// iter-7 evaluator non-blocking item: direct migration
// round-trip coverage for 0006_repo_url trigger enforcement
// beyond static SQL review.
func assertRepoURLWriteOnceTrigger(t *testing.T, psql, url string) {
	t.Helper()
	// Use a fixed scratch display_name so the test cleanup
	// can target this test's rows without affecting sibling
	// subtests.
	const scratch = "repo-url-write-once-scratch"
	const originalURL = "https://example.com/org/original-repo"
	const replacementURL = "https://example.com/org/attacker-repo"

	// Best-effort precondition cleanup so a prior failed run
	// doesn't leave a row that would skew the test.
	_ = runPSQLInline(t, psql, url, fmt.Sprintf(
		"DELETE FROM clean_code.repo WHERE display_name = %s;",
		pgLiteral(scratch)))
	// Final cleanup so re-runs under -count=N stay idempotent
	// AND so the round-trip's `drop schema` cleanup is a no-op
	// even if this assertion fails mid-way.
	t.Cleanup(func() {
		_ = runPSQLInline(t, psql, url, fmt.Sprintf(
			"DELETE FROM clean_code.repo WHERE display_name = %s;",
			pgLiteral(scratch)))
	})

	// Step 1: INSERT with NULL repo_url -- pre-0006-style row.
	// No trigger fire (the trigger is BEFORE UPDATE, not
	// BEFORE INSERT).
	if err := runPSQLInline(t, psql, url, fmt.Sprintf(
		"INSERT INTO clean_code.repo (display_name, default_branch, repo_url) "+
			"VALUES (%s, 'main', NULL);", pgLiteral(scratch))); err != nil {
		t.Fatalf("INSERT with NULL repo_url: %v", err)
	}

	// Step 2: NULL -> non-NULL back-fill -- ALLOWED.
	if err := runPSQLInline(t, psql, url, fmt.Sprintf(
		"UPDATE clean_code.repo SET repo_url = %s WHERE display_name = %s;",
		pgLiteral(originalURL), pgLiteral(scratch))); err != nil {
		t.Fatalf("UPDATE NULL -> %q: %v (expected to succeed -- "+
			"the trigger MUST allow back-fill of pre-0006 rows)", originalURL, err)
	}
	// Sanity-check the value actually landed.
	got, err := runPSQLQuery(t, psql, url, fmt.Sprintf(
		"SELECT repo_url FROM clean_code.repo WHERE display_name = %s;",
		pgLiteral(scratch)))
	if err != nil {
		t.Fatalf("SELECT repo_url after back-fill: %v", err)
	}
	if strings.TrimSpace(got) != originalURL {
		t.Fatalf("repo_url after back-fill = %q, want %q",
			strings.TrimSpace(got), originalURL)
	}

	// Step 3: SET repo_url = repo_url (no-op) -- ALLOWED.
	// Some ORMs emit redundant assignments on partial-column
	// updates; the trigger MUST short-circuit on
	// `NEW.repo_url IS DISTINCT FROM OLD.repo_url` = false.
	if err := runPSQLInline(t, psql, url, fmt.Sprintf(
		"UPDATE clean_code.repo SET repo_url = repo_url "+
			"WHERE display_name = %s;", pgLiteral(scratch))); err != nil {
		t.Fatalf("UPDATE SET repo_url = repo_url (no-op): %v "+
			"(expected to succeed -- IS DISTINCT FROM guards the no-op)", err)
	}

	// Step 4: post-set change -- REJECTED with SQLSTATE 23514.
	// runPSQLInline returns a non-nil error; we surface its
	// combined output so the test log shows the RAISE EXCEPTION
	// message text.
	err = runPSQLInline(t, psql, url, fmt.Sprintf(
		"UPDATE clean_code.repo SET repo_url = %s WHERE display_name = %s;",
		pgLiteral(replacementURL), pgLiteral(scratch)))
	if err == nil {
		t.Fatalf("UPDATE %q -> %q: err=nil; want SQLSTATE 23514 "+
			"check_violation -- the WRITE-ONCE trigger MUST reject "+
			"post-set changes", originalURL, replacementURL)
	}
	// The error wraps psql's combined stderr/stdout. We look
	// for either the SQLSTATE code (matches `psql:...: ERROR:
	// ...` formatted output) OR the trigger's literal
	// message text ("is WRITE-ONCE"). Both signals are
	// present in the same line of psql output, so requiring
	// either keeps the assertion robust against psql output
	// format variations across versions.
	errText := err.Error()
	if !strings.Contains(errText, "23514") && !strings.Contains(errText, "WRITE-ONCE") {
		t.Fatalf("UPDATE rejection error did not mention SQLSTATE "+
			"23514 or 'WRITE-ONCE'; got: %v", err)
	}

	// Step 5: confirm the value did NOT change.
	got, err = runPSQLQuery(t, psql, url, fmt.Sprintf(
		"SELECT repo_url FROM clean_code.repo WHERE display_name = %s;",
		pgLiteral(scratch)))
	if err != nil {
		t.Fatalf("SELECT repo_url after rejected UPDATE: %v", err)
	}
	if strings.TrimSpace(got) != originalURL {
		t.Fatalf("repo_url after rejected UPDATE = %q, want %q "+
			"(trigger rejection MUST leave the value unchanged)",
			strings.TrimSpace(got), originalURL)
	}
}

// assertRepoURLManagementGrants pins the runtime privilege
// behavior of the column-level grants added by
// 0006_repo_url.up.sql. The structural test
// (TestRepoURLUpSQLBodyMentionsExpectedObjects) pins the
// presence of the GRANT statements; this assertion pins what
// they actually authorise / forbid against a live PostgreSQL:
//
//   - The Management role can INSERT a row WITH repo_url
//     supplied (canonical registration path -- iter-7
//     evaluator item 2 fix).
//   - The Management role can SELECT the column (every writer
//     role inherits SELECT via the cross-sub-store grant in
//     0004_roles.up.sql:227-260).
//   - The Repo Indexer role CANNOT INSERT into clean_code.repo
//     at all -- repo registration is Management-owned per C5
//     row 1.
//   - The Metric Ingestor role CANNOT UPDATE repo_url -- the
//     column is Management-owned even though Metric Ingestor
//     can SELECT it for the canonical-signature stamp.
//
// iter-7 evaluator non-blocking item: round-trip coverage of
// 0006_repo_url grants beyond static SQL review.
func assertRepoURLManagementGrants(t *testing.T, psql, url string) {
	t.Helper()
	const scratch = "repo-url-grants-scratch"
	const operatorURL = "https://example.com/org/grants-test"

	// Best-effort precondition cleanup; final cleanup wired
	// to t.Cleanup so failures mid-way still tidy up.
	_ = runPSQLInline(t, psql, url, fmt.Sprintf(
		"DELETE FROM clean_code.repo WHERE display_name = %s;",
		pgLiteral(scratch)))
	t.Cleanup(func() {
		_ = runPSQLInline(t, psql, url, fmt.Sprintf(
			"DELETE FROM clean_code.repo WHERE display_name = %s;",
			pgLiteral(scratch)))
	})

	// (1) Management CAN insert WITH repo_url. We wrap in
	// BEGIN..COMMIT with SET LOCAL ROLE so the grant matrix
	// is exercised exactly. SET LOCAL constrains the role
	// switch to the transaction; COMMIT resets it.
	managementInsert := fmt.Sprintf(`BEGIN;
SET LOCAL ROLE clean_code_management;
INSERT INTO clean_code.repo (display_name, default_branch, repo_url)
  VALUES (%s, 'main', %s);
COMMIT;`, pgLiteral(scratch), pgLiteral(operatorURL))
	if err := runPSQLInline(t, psql, url, managementInsert); err != nil {
		t.Fatalf("Management INSERT WITH repo_url: %v "+
			"(C5 row 1 + iter-7 evaluator item 2 require this to succeed)", err)
	}

	// (2) Management CAN SELECT repo_url. The cross-sub-store
	// SELECT grant in 0004_roles.up.sql covers all columns;
	// this is a sanity check that 0006 did not regress it.
	managementSelect := fmt.Sprintf(`BEGIN;
SET LOCAL ROLE clean_code_management;
SELECT repo_url FROM clean_code.repo WHERE display_name = %s;
COMMIT;`, pgLiteral(scratch))
	if err := runPSQLInline(t, psql, url, managementSelect); err != nil {
		t.Fatalf("Management SELECT repo_url: %v "+
			"(every writer role MUST be able to SELECT repo_url)", err)
	}

	// (3) Repo Indexer CANNOT INSERT into clean_code.repo at
	// all -- C5 row 1 reserves repo registration for
	// Management. The expected failure mode is SQLSTATE 42501
	// (insufficient_privilege).
	repoIndexerInsert := fmt.Sprintf(`BEGIN;
SET LOCAL ROLE clean_code_repo_indexer;
INSERT INTO clean_code.repo (display_name, default_branch, repo_url)
  VALUES ('repo-indexer-attempt-%s', 'main', %s);
COMMIT;`, scratch, pgLiteral(operatorURL))
	err := runPSQLInline(t, psql, url, repoIndexerInsert)
	if err == nil {
		t.Fatalf("Repo Indexer INSERT into clean_code.repo: err=nil; " +
			"want SQLSTATE 42501 -- Repo Indexer must NOT be able " +
			"to register repos (C5 row 1 owner is Management).")
	}
	if !strings.Contains(err.Error(), pgPermissionDeniedSQLState) &&
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Repo Indexer INSERT rejection did not mention "+
			"SQLSTATE %s or 'permission denied'; got: %v",
			pgPermissionDeniedSQLState, err)
	}

	// (4) Metric Ingestor CANNOT UPDATE repo_url (even though
	// the trigger would allow a NULL->non-NULL back-fill;
	// the grant is the outer guard). Expected failure mode is
	// again SQLSTATE 42501.
	metricIngestorUpdate := fmt.Sprintf(`BEGIN;
SET LOCAL ROLE clean_code_metric_ingestor;
UPDATE clean_code.repo SET repo_url = %s WHERE display_name = %s;
COMMIT;`, pgLiteral("https://example.com/org/metric-ingestor-attempt"), pgLiteral(scratch))
	err = runPSQLInline(t, psql, url, metricIngestorUpdate)
	if err == nil {
		t.Fatalf("Metric Ingestor UPDATE repo_url: err=nil; " +
			"want SQLSTATE 42501 -- the column is Management-owned.")
	}
	if !strings.Contains(err.Error(), pgPermissionDeniedSQLState) &&
		!strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("Metric Ingestor UPDATE rejection did not mention "+
			"SQLSTATE %s or 'permission denied'; got: %v",
			pgPermissionDeniedSQLState, err)
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

// ---------------------------------------------------------------------------
// Stage 1.4 live-PostgreSQL canon-guard helpers
// ---------------------------------------------------------------------------
//
// These helpers back the five Stage 1.4 subtests of
// TestRoundTrip_upDownLeavesSchemaEmpty. They each (a) seed a
// repo + policy_version + evaluation_run triple identified by a
// per-subtest `tag`, (b) exercise the canon-guard via INSERTs
// or catalog queries, then (c) tear the seeded rows back down
// in reverse-FK order so re-runs under `go test -count=N` stay
// idempotent.
//
// The seeded `signature` column uses `decode('00', 'hex')` for
// a single-byte payload -- the column is BYTEA NOT NULL and the
// canon-guard does not exercise signature verification (that is
// the Evaluator Surface's job at gate time).

// seedStage14EvaluationRun creates a (repo, policy_version,
// evaluation_run) triple identified by `tag` and returns the
// evaluation_run_id text + the repo_id text + the
// policy_version_id text. The triple is sufficient to satisfy
// the FK chain feeding `evaluation_verdict` and `finding`
// INSERTs.
func seedStage14EvaluationRun(t *testing.T, psql, url, tag string) (runID, repoID, policyVersionID string) {
	t.Helper()
	// Single statement -- if any leg fails the whole CTE chain
	// rolls back via PostgreSQL statement-level transaction
	// semantics, so a partial seed cannot leak.
	sql := fmt.Sprintf(`
WITH new_repo AS (
    INSERT INTO clean_code.repo (display_name, default_branch)
    VALUES ('stage14-%s', 'main')
    RETURNING repo_id
), new_pv AS (
    INSERT INTO clean_code.policy_version
        (name, rule_refs, threshold_refs, refactor_weights, signature)
    VALUES (
        'stage14-%s',
        '[]'::jsonb,
        '[]'::jsonb,
        '{"alpha":1,"beta":1,"gamma":1,"delta":1,"effort_model_version":"v0","window_days":90}'::jsonb,
        decode('00', 'hex')
    )
    RETURNING policy_version_id
), new_er AS (
    INSERT INTO clean_code.evaluation_run
        (repo_id, sha, policy_version_id, caller)
    SELECT r.repo_id, 'sha-stage14-%s', p.policy_version_id, 'eval_gate'
    FROM new_repo r, new_pv p
    RETURNING evaluation_run_id, repo_id, policy_version_id
)
SELECT evaluation_run_id::text || '|' || repo_id::text || '|' || policy_version_id::text
  FROM new_er;`, tag, tag, tag)
	out, err := runPSQLQuery(t, psql, url, sql)
	if err != nil {
		t.Fatalf("seed stage14 fixture %q: %v", tag, err)
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		t.Fatalf("seed stage14 fixture %q returned malformed ids: %q", tag, out)
	}
	return parts[0], parts[1], parts[2]
}

// cleanupStage14Tag deletes every scratch row attributed to the
// supplied `tag`. The order is reverse-FK so RESTRICT
// constraints fire only on operator/test bugs, not on the
// teardown path:
//
//	verdict -> finding -> evaluation_run -> rule -> policy_version -> repo
//
// Errors are swallowed so a partial seed (already-failed test)
// still cleans up whatever it can.
func cleanupStage14Tag(t *testing.T, psql, url, tag string) {
	t.Helper()
	repoName := fmt.Sprintf("stage14-%s", tag)
	pvName := fmt.Sprintf("stage14-%s", tag)
	ruleID := fmt.Sprintf("stage14-%s", tag)
	stmts := []string{
		fmt.Sprintf(`DELETE FROM clean_code.evaluation_verdict v
USING clean_code.evaluation_run e, clean_code.repo r
 WHERE v.evaluation_run_id = e.evaluation_run_id
   AND e.repo_id = r.repo_id
   AND r.display_name = '%s';`, repoName),
		fmt.Sprintf(`DELETE FROM clean_code.finding f
USING clean_code.evaluation_run e, clean_code.repo r
 WHERE f.evaluation_run_id = e.evaluation_run_id
   AND e.repo_id = r.repo_id
   AND r.display_name = '%s';`, repoName),
		fmt.Sprintf(`DELETE FROM clean_code.evaluation_run e
USING clean_code.repo r
 WHERE e.repo_id = r.repo_id
   AND r.display_name = '%s';`, repoName),
		fmt.Sprintf(`DELETE FROM clean_code.rule
 WHERE rule_id = '%s';`, ruleID),
		fmt.Sprintf(`DELETE FROM clean_code.policy_version
 WHERE name = '%s';`, pvName),
		fmt.Sprintf(`DELETE FROM clean_code.repo
 WHERE display_name = '%s';`, repoName),
	}
	for _, s := range stmts {
		_ = runPSQLInline(t, psql, url, s)
	}
}

// assertVerdictEnumOnlyCanonical pins the
// implementation-plan.md:128 "verdict-enum-only-canonical"
// scenario. It (a) seeds a parent evaluation_run, (b) attempts
// INSERTs with the brief-rejected verdict labels `fail` and
// `gated` and asserts each fails with PostgreSQL's ENUM input
// rejection (SQLSTATE 22P02), and (c) inserts each canonical
// label `pass`, `warn`, `block` and asserts each succeeds.
func assertVerdictEnumOnlyCanonical(t *testing.T, psql, url string) {
	t.Helper()
	const tag = "verdict-enum"
	cleanupStage14Tag(t, psql, url, tag)
	t.Cleanup(func() { cleanupStage14Tag(t, psql, url, tag) })
	runID, _, _ := seedStage14EvaluationRun(t, psql, url, tag)

	rejected := []string{"fail", "gated"}
	for _, bad := range rejected {
		insert := fmt.Sprintf(
			`INSERT INTO clean_code.evaluation_verdict (evaluation_run_id, verdict) `+
				`VALUES ('%s', '%s');`, runID, bad)
		err := runPSQLInline(t, psql, url, insert)
		if err == nil {
			t.Fatalf("INSERT with verdict=%q succeeded; "+
				"want PostgreSQL ENUM rejection (SQLSTATE 22P02) "+
				"per implementation-plan.md:128", bad)
		}
		if !strings.Contains(err.Error(), "22P02") &&
			!strings.Contains(err.Error(), "invalid input value for enum") {
			t.Fatalf("INSERT with verdict=%q failed but not via ENUM "+
				"input check; got %v -- a different failure mode "+
				"would not prove the canon-guard", bad, err)
		}
	}

	accepted := []string{"pass", "warn", "block"}
	for _, good := range accepted {
		insert := fmt.Sprintf(
			`INSERT INTO clean_code.evaluation_verdict (evaluation_run_id, verdict) `+
				`VALUES ('%s', '%s');`, runID, good)
		if err := runPSQLInline(t, psql, url, insert); err != nil {
			t.Fatalf("INSERT with verdict=%q failed: %v -- the three "+
				"canonical values MUST succeed per architecture "+
				"Sec 5.4.3 line 1210", good, err)
		}
	}
}

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

// assertDegradedReasonClosedSet pins the
// implementation-plan.md:130 "degraded-reason-closed-set"
// scenario. It asserts the CHECK constraint
// `evaluation_verdict_degraded_reason_canonical` rejects
// out-of-band reasons (SQLSTATE 23514) and accepts each of the
// four canonical values + NULL.
func assertDegradedReasonClosedSet(t *testing.T, psql, url string) {
	t.Helper()
	const tag = "degraded-reason"
	cleanupStage14Tag(t, psql, url, tag)
	t.Cleanup(func() { cleanupStage14Tag(t, psql, url, tag) })
	runID, _, _ := seedStage14EvaluationRun(t, psql, url, tag)

	// Out-of-band reasons MUST fail the CHECK constraint.
	rejected := []string{"other", "unknown", "regression"}
	for _, bad := range rejected {
		insert := fmt.Sprintf(
			`INSERT INTO clean_code.evaluation_verdict `+
				`(evaluation_run_id, verdict, degraded, degraded_reason) `+
				`VALUES ('%s', 'warn', true, '%s');`, runID, bad)
		err := runPSQLInline(t, psql, url, insert)
		if err == nil {
			t.Fatalf("INSERT with degraded_reason=%q succeeded; "+
				"want CHECK rejection (SQLSTATE 23514) per "+
				"implementation-plan.md:130", bad)
		}
		if !strings.Contains(err.Error(), "23514") &&
			!strings.Contains(err.Error(), "check constraint") {
			t.Fatalf("INSERT with degraded_reason=%q failed but not "+
				"via CHECK constraint; got %v", bad, err)
		}
	}

	// The four canonical values from architecture Sec 8.2 /
	// tech-spec Sec 7.7 C21 MUST all succeed.
	accepted := []string{
		"xrepo_edges_unavailable",
		"samples_pending",
		"policy_signature_invalid",
		"percentile_stale",
	}
	for _, good := range accepted {
		insert := fmt.Sprintf(
			`INSERT INTO clean_code.evaluation_verdict `+
				`(evaluation_run_id, verdict, degraded, degraded_reason) `+
				`VALUES ('%s', 'warn', true, '%s');`, runID, good)
		if err := runPSQLInline(t, psql, url, insert); err != nil {
			t.Fatalf("INSERT with degraded_reason=%q failed: %v -- "+
				"the four canonical values MUST succeed per "+
				"architecture Sec 8.2", good, err)
		}
	}

	// NULL MUST also be accepted (the column is nullable per
	// architecture Sec 5.4.3 line 1212 `text?`).
	insertNull := fmt.Sprintf(
		`INSERT INTO clean_code.evaluation_verdict `+
			`(evaluation_run_id, verdict, degraded) `+
			`VALUES ('%s', 'pass', false);`, runID)
	if err := runPSQLInline(t, psql, url, insertNull); err != nil {
		t.Fatalf("INSERT omitting degraded_reason failed: %v -- "+
			"the column is nullable per architecture Sec 5.4.3 "+
			"line 1212", err)
	}
}

// assertFindingDeltaCanonical pins the
// implementation-plan.md:131 "finding-delta-canonical" scenario.
// `clean_code.finding.delta` is of type
// `clean_code.finding_delta` ENUM with the four canonical labels
// `new | newly_failing | unchanged | resolved`. The brief-
// rejected labels `regression | improvement | flat` must each
// fail PostgreSQL's ENUM input check (SQLSTATE 22P02).
func assertFindingDeltaCanonical(t *testing.T, psql, url string) {
	t.Helper()
	const tag = "finding-delta"
	cleanupStage14Tag(t, psql, url, tag)
	t.Cleanup(func() { cleanupStage14Tag(t, psql, url, tag) })
	runID, repoID, pvID := seedStage14EvaluationRun(t, psql, url, tag)

	// Seed a `rule` row to satisfy `finding.(rule_id, rule_version)`
	// composite FK to `rule(rule_id, version)`.
	seedRule := fmt.Sprintf(
		`INSERT INTO clean_code.rule `+
			`(rule_id, version, pack_id, predicate_dsl, severity_default, description_md) `+
			`VALUES ('stage14-%s', 1, 'test', 'true', 'info', 'fixture');`, tag)
	if err := runPSQLInline(t, psql, url, seedRule); err != nil {
		t.Fatalf("seed stage14 rule: %v", err)
	}

	finding := func(delta string) string {
		return fmt.Sprintf(`INSERT INTO clean_code.finding `+
			`(evaluation_run_id, repo_id, sha, scope_id, `+
			` rule_id, rule_version, policy_version_id, severity, delta) `+
			`VALUES ('%s', '%s', 'sha-stage14-%s', gen_random_uuid(), `+
			`        'stage14-%s', 1, '%s', 'info', '%s');`,
			runID, repoID, tag, tag, pvID, delta)
	}

	rejected := []string{"regression", "improvement", "flat"}
	for _, bad := range rejected {
		err := runPSQLInline(t, psql, url, finding(bad))
		if err == nil {
			t.Fatalf("INSERT with delta=%q succeeded; want PostgreSQL "+
				"ENUM rejection (SQLSTATE 22P02) per "+
				"implementation-plan.md:131", bad)
		}
		if !strings.Contains(err.Error(), "22P02") &&
			!strings.Contains(err.Error(), "invalid input value for enum") {
			t.Fatalf("INSERT with delta=%q failed but not via ENUM "+
				"input check; got %v", bad, err)
		}
	}

	accepted := []string{"new", "newly_failing", "unchanged", "resolved"}
	for _, good := range accepted {
		if err := runPSQLInline(t, psql, url, finding(good)); err != nil {
			t.Fatalf("INSERT with delta=%q failed: %v -- the four "+
				"canonical values MUST succeed per architecture "+
				"Sec 5.4.1 line 1189", good, err)
		}
	}
}

// assertRefactorTaskNoStatusColumn pins the
// implementation-plan.md:132 "refactor-task-no-status-column"
// scenario. The architecture Sec 5.5.3 RefactorTask field table
// explicitly omits `status` and `expected_metric_delta`; both
// columns MUST be absent from the live schema. Querying the
// SQL source-text is insufficient -- the migration could carry
// a comment line containing the forbidden token without
// actually creating the column, or vice versa. So we hit
// `information_schema.columns` against the live database.
func assertRefactorTaskNoStatusColumn(t *testing.T, psql, url string) {
	t.Helper()
	// Sanity: the table must exist. A vacuously-true absence
	// check on a missing table would mask a migration regression.
	assertTableExists(t, psql, url, "refactor_task")

	for _, col := range []string{"status", "expected_metric_delta"} {
		q := fmt.Sprintf(
			`SELECT count(*) FROM information_schema.columns `+
				`WHERE table_schema = 'clean_code' `+
				`  AND table_name = 'refactor_task' `+
				`  AND column_name = '%s';`, col)
		got, err := runPSQLQuery(t, psql, url, q)
		if err != nil {
			t.Fatalf("information_schema query for refactor_task.%s: %v", col, err)
		}
		if strings.TrimSpace(got) != "0" {
			t.Fatalf("clean_code.refactor_task.%s is PRESENT in the "+
				"live schema (count=%s); architecture Sec 5.5.3 "+
				"forbids it per implementation-plan.md:132",
				col, strings.TrimSpace(got))
		}
	}
}

// assertPolicyActivationLatestRowWins pins the
// implementation-plan.md:133 "policy-activation-latest-row-wins"
// scenario via three structural absence checks against the
// live schema:
//
//  1. `policy_activation` has no `scope` column -- activation is
//     global per deployment in v1 (tech-spec Sec 4.14 single-
//     tenant).
//  2. `policy_activation` has no `deactivated_at` column --
//     deactivation is recorded by appending a new activation
//     row, not by mutating an existing one (G3 + G5).
//  3. `policy_activation` carries no PARTIAL UNIQUE index (one
//     with a non-null `pg_index.indpred`). A partial-unique on
//     "currently active" would require a mutable column to
//     flip, violating G3.
//
// Each check hits a different PostgreSQL catalog so a missing
// table cannot vacuously satisfy the negative assertions.
func assertPolicyActivationLatestRowWins(t *testing.T, psql, url string) {
	t.Helper()
	assertTableExists(t, psql, url, "policy_activation")

	for _, col := range []string{"scope", "deactivated_at"} {
		q := fmt.Sprintf(
			`SELECT count(*) FROM information_schema.columns `+
				`WHERE table_schema = 'clean_code' `+
				`  AND table_name = 'policy_activation' `+
				`  AND column_name = '%s';`, col)
		got, err := runPSQLQuery(t, psql, url, q)
		if err != nil {
			t.Fatalf("information_schema query for policy_activation.%s: %v", col, err)
		}
		if strings.TrimSpace(got) != "0" {
			t.Fatalf("clean_code.policy_activation.%s is PRESENT in "+
				"the live schema (count=%s); architecture Sec 5.3.4 "+
				"forbids it per implementation-plan.md:133",
				col, strings.TrimSpace(got))
		}
	}

	// Partial UNIQUE index detection via pg_index.indpred.
	// `indisunique = true AND indpred IS NOT NULL` identifies
	// "unique WHERE <predicate>" indexes -- the PRIMARY KEY
	// index is unique but `indpred IS NULL`, so it does not
	// match. This is more robust than a textual LIKE on
	// `pg_indexes.indexdef` (which would break if PostgreSQL
	// adjusted the SHOW output format).
	const partialUniqueQ = `
SELECT count(*)
FROM pg_index i
JOIN pg_class idx ON idx.oid = i.indexrelid
JOIN pg_class tbl ON tbl.oid = i.indrelid
JOIN pg_namespace ns ON ns.oid = tbl.relnamespace
WHERE ns.nspname = 'clean_code'
  AND tbl.relname = 'policy_activation'
  AND i.indisunique
  AND i.indpred IS NOT NULL;`
	got, err := runPSQLQuery(t, psql, url, partialUniqueQ)
	if err != nil {
		t.Fatalf("pg_index query for policy_activation partial unique: %v", err)
	}
	if strings.TrimSpace(got) != "0" {
		t.Fatalf("clean_code.policy_activation carries %s partial UNIQUE "+
			"index(es); architecture Sec 5.3.4 forbids them "+
			"(latest-row-wins is the contract) per "+
			"implementation-plan.md:133", strings.TrimSpace(got))
	}
}

// assertOverrideNoExpiresColumn pins the
// implementation-plan.md:129 "override-no-expires-column"
// scenario. The architecture Sec 5.3.6 Override field table
// (and tech-spec Sec 10A "v1 has no TTL") explicitly omit
// `expires_at`; mute lifecycle is latest-row-wins via appended
// `mute=false` rows, not a TTL column. An accidental
// `expires_at` column would silently re-introduce TTL
// semantics, so we assert absence against the live schema --
// not just the SQL source text.
//
// Existence pre-check on the `override` table itself prevents
// a missing-table regression from vacuously satisfying the
// zero-row assertion.
func assertOverrideNoExpiresColumn(t *testing.T, psql, url string) {
	t.Helper()
	assertTableExists(t, psql, url, "override")

	const q = `SELECT count(*) FROM information_schema.columns ` +
		`WHERE table_schema = 'clean_code' ` +
		`  AND table_name = 'override' ` +
		`  AND column_name = 'expires_at';`
	got, err := runPSQLQuery(t, psql, url, q)
	if err != nil {
		t.Fatalf("information_schema query for override.expires_at: %v", err)
	}
	if strings.TrimSpace(got) != "0" {
		t.Fatalf("clean_code.override.expires_at is PRESENT in the "+
			"live schema (count=%s); architecture Sec 5.3.6 + "+
			"tech-spec Sec 10A forbid it per implementation-"+
			"plan.md:129 (iter 1 evaluator item 5)",
			strings.TrimSpace(got))
	}
}

// assertTableExists is the existence pre-check used by the
// structural Stage 1.4 absence subtests. It catches the
// "vacuously true" risk: a SELECT-zero-rows assertion on a
// MISSING table would silently pass even if the migration
// failed to create the table at all.
func assertTableExists(t *testing.T, psql, url, table string) {
	t.Helper()
	q := fmt.Sprintf(
		`SELECT count(*) FROM information_schema.tables `+
			`WHERE table_schema = 'clean_code' `+
			`  AND table_name = '%s' `+
			`  AND table_type = 'BASE TABLE';`, table)
	got, err := runPSQLQuery(t, psql, url, q)
	if err != nil {
		t.Fatalf("information_schema query for clean_code.%s: %v", table, err)
	}
	if strings.TrimSpace(got) != "1" {
		t.Fatalf("clean_code.%s is NOT a base table in the live "+
			"schema (count=%s); migration regression before the "+
			"absence check would be vacuously true",
			table, strings.TrimSpace(got))
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

// ---------------------------------------------------------------------------
// Stage 5.1 policy_signing_keys live-PG canon-guards
// ---------------------------------------------------------------------------

// assertPolicySigningKeysRoundTrip exercises the basic
// INSERT/SELECT contract against
// `clean_code.policy_signing_keys`. Seeds a valid row, reads it
// back via `policy.keys.list_active` projection columns,
// asserts the round-trip matches.
func assertPolicySigningKeysRoundTrip(t *testing.T, psql, url string) {
	t.Helper()
	// Cleanup any prior row from a re-run before seeding.
	if err := runPSQLInline(t, psql, url,
		"DELETE FROM clean_code.policy_signing_keys WHERE fingerprint LIKE 'aa%';"); err != nil {
		t.Fatalf("pre-test cleanup: %v", err)
	}

	const seed = `
INSERT INTO clean_code.policy_signing_keys
    (key_id, fingerprint, public_key, key_handle)
VALUES (
    'aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa',
    'aa11220000000000000000000000000000000000000000000000000000000000',
    decode('0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20', 'hex'),
    'local-v1:test-handle-roundtrip'
);`
	if err := runPSQLInline(t, psql, url, seed); err != nil {
		t.Fatalf("seed valid policy_signing_keys row: %v", err)
	}

	// The four columns the `policy.keys.list_active` verb
	// projects: key_id, fingerprint, valid_from, valid_until.
	// `valid_until` is DERIVED so the query proves the column
	// is intentionally absent (we cast valid_from to text via
	// to_char so any timestamptz mis-format surfaces here).
	out, err := runPSQLQuery(t, psql, url,
		"SELECT key_id::text || '|' || fingerprint || '|' || (valid_from IS NOT NULL)::text "+
			"FROM clean_code.policy_signing_keys "+
			"WHERE fingerprint = 'aa11220000000000000000000000000000000000000000000000000000000000';")
	if err != nil {
		t.Fatalf("read-back round-trip row: %v", err)
	}
	out = strings.TrimSpace(out)
	expectedKeyID := "aaaaaaaa-aaaa-4aaa-aaaa-aaaaaaaaaaaa"
	expectedFingerprint := "aa11220000000000000000000000000000000000000000000000000000000000"
	if !strings.Contains(out, expectedKeyID) {
		t.Errorf("round-trip key_id missing; got %q", out)
	}
	if !strings.Contains(out, expectedFingerprint) {
		t.Errorf("round-trip fingerprint missing; got %q", out)
	}
	if !strings.Contains(out, "t") {
		t.Errorf("round-trip valid_from is NULL; got %q", out)
	}

	// The index on (valid_from DESC, key_id DESC) must be
	// present -- the active-set lookup query relies on it.
	// `policy.keys.list_active` runs this exact ordering on
	// every read.
	idxOut, err := runPSQLQuery(t, psql, url,
		"SELECT indexname FROM pg_indexes "+
			"WHERE schemaname='clean_code' AND tablename='policy_signing_keys' "+
			"AND indexname='policy_signing_keys_valid_from_idx';")
	if err != nil {
		t.Fatalf("query indexes: %v", err)
	}
	if !strings.Contains(strings.TrimSpace(idxOut), "policy_signing_keys_valid_from_idx") {
		t.Errorf("missing index `policy_signing_keys_valid_from_idx`; got %q", idxOut)
	}

	// `valid_until` MUST NOT be a column on the table -- the
	// tech-spec Sec 8.4 + Stage 5.1 design DERIVES it at read
	// time from the successor row. If a future migration adds
	// the column, the in-process derivation path
	// (Manager.computeValidUntil) becomes ambiguous.
	colOut, err := runPSQLQuery(t, psql, url,
		"SELECT column_name FROM information_schema.columns "+
			"WHERE table_schema='clean_code' AND table_name='policy_signing_keys' "+
			"AND column_name IN ('valid_until','retired_at','private_key','secret_key','sealed_key','signing_secret','wrapped_key');")
	if err != nil {
		t.Fatalf("query forbidden columns: %v", err)
	}
	if strings.TrimSpace(colOut) != "" {
		t.Errorf("forbidden column(s) present on policy_signing_keys: %q -- the Stage 5.1 contract requires valid_until DERIVED + no private-key column", colOut)
	}

	// Cleanup so re-runs stay idempotent.
	if err := runPSQLInline(t, psql, url,
		"DELETE FROM clean_code.policy_signing_keys WHERE fingerprint = 'aa11220000000000000000000000000000000000000000000000000000000000';"); err != nil {
		t.Fatalf("post-test cleanup: %v", err)
	}
}

// assertPolicySigningKeysConstraints walks every CHECK
// constraint declared on the table. Each negative case names
// the constraint expected to fire so a constraint-name drift
// surfaces here rather than at the Manager layer (where the
// SQLSTATE -> sentinel mapping in `internal/policy/keys/sql_store.go`
// keys off the same names).
func assertPolicySigningKeysConstraints(t *testing.T, psql, url string) {
	t.Helper()
	// Cleanup before each negative case.
	pre := func() {
		_ = runPSQLInline(t, psql, url,
			"DELETE FROM clean_code.policy_signing_keys WHERE fingerprint LIKE 'bb%' OR fingerprint LIKE 'cc%' OR fingerprint LIKE 'dd%';")
	}

	// 1. fingerprint shape violation: too short, non-hex,
	//    uppercase -- all rejected by
	//    `policy_signing_keys_fingerprint_shape`.
	for _, bad := range []string{
		"AA11220000000000000000000000000000000000000000000000000000000000", // uppercase
		"bb",      // too short
		"bbxxzz1100000000000000000000000000000000000000000000000000000000", // non-hex
	} {
		pre()
		insert := fmt.Sprintf(`INSERT INTO clean_code.policy_signing_keys (fingerprint, public_key, key_handle) VALUES ('%s', decode('0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20','hex'), 'h');`, bad)
		err := runPSQLInline(t, psql, url, insert)
		if err == nil {
			t.Errorf("bad fingerprint %q: INSERT succeeded; want CHECK violation", bad)
			continue
		}
		if !strings.Contains(err.Error(), "policy_signing_keys_fingerprint_shape") {
			t.Errorf("bad fingerprint %q: error did not name constraint; got %v", bad, err)
		}
	}

	// 2. public_key length violation: 31 bytes / 33 bytes ->
	//    `policy_signing_keys_public_key_ed25519_len`.
	for _, bad := range []string{
		"0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f",     // 31 bytes
		"0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f2021", // 33 bytes
	} {
		pre()
		insert := fmt.Sprintf(`INSERT INTO clean_code.policy_signing_keys (fingerprint, public_key, key_handle) VALUES ('cc11220000000000000000000000000000000000000000000000000000000000', decode('%s','hex'), 'h');`, bad)
		err := runPSQLInline(t, psql, url, insert)
		if err == nil {
			t.Errorf("bad public_key (hex=%s): INSERT succeeded; want CHECK violation", bad)
			continue
		}
		if !strings.Contains(err.Error(), "policy_signing_keys_public_key_ed25519_len") {
			t.Errorf("bad public_key length: error did not name constraint; got %v", err)
		}
	}

	// 3. algorithm closed-set violation:
	//    `policy_signing_keys_algorithm_canonical`.
	pre()
	insert := `INSERT INTO clean_code.policy_signing_keys (fingerprint, public_key, key_handle, algorithm) VALUES ('dd11220000000000000000000000000000000000000000000000000000000000', decode('0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20','hex'), 'h', 'dilithium3');`
	err := runPSQLInline(t, psql, url, insert)
	if err == nil {
		t.Error("algorithm='dilithium3': INSERT succeeded; want CHECK violation")
	} else if !strings.Contains(err.Error(), "policy_signing_keys_algorithm_canonical") {
		t.Errorf("bad algorithm: error did not name constraint; got %v", err)
	}

	// 4. fingerprint UNIQUE violation -- two rows with the
	//    same fingerprint must collide on the UNIQUE
	//    constraint (which `internal/policy/keys/sql_store.go`
	//    maps to ErrDuplicateKey via SQLSTATE 23505).
	pre()
	if err := runPSQLInline(t, psql, url,
		`INSERT INTO clean_code.policy_signing_keys (fingerprint, public_key, key_handle) VALUES ('bbcafe0000000000000000000000000000000000000000000000000000000000', decode('0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20','hex'), 'h');`); err != nil {
		t.Fatalf("seed for UNIQUE test: %v", err)
	}
	dupErr := runPSQLInline(t, psql, url,
		`INSERT INTO clean_code.policy_signing_keys (fingerprint, public_key, key_handle) VALUES ('bbcafe0000000000000000000000000000000000000000000000000000000000', decode('2120191817161514131211100f0e0d0c0b0a09080706050403020100ffeeddcc','hex'), 'h');`)
	if dupErr == nil {
		t.Error("duplicate fingerprint: INSERT succeeded; want UNIQUE violation")
	} else if !strings.Contains(strings.ToLower(dupErr.Error()), "policy_signing_keys_fingerprint_key") &&
		!strings.Contains(strings.ToLower(dupErr.Error()), "duplicate key") {
		t.Errorf("duplicate fingerprint: error did not look like UNIQUE violation; got %v", dupErr)
	}
	pre()
}

// assertPolicySigningKeysGrants checks the privilege contract
// the migration declares against
// `information_schema.role_table_grants`. The Policy Steward
// MUST hold INSERT + SELECT; every other writer role MUST
// hold SELECT and NOT INSERT (writer isolation per tech-spec
// Sec 7.2 / C5). UPDATE and DELETE MUST be absent from every
// grantee (append-only G3).
func assertPolicySigningKeysGrants(t *testing.T, psql, url string) {
	t.Helper()
	// Aggregate grants into a single concatenated string so a
	// single round-trip captures the full ACL.
	out, err := runPSQLQuery(t, psql, url,
		"SELECT grantee || '|' || privilege_type FROM information_schema.role_table_grants "+
			"WHERE table_schema='clean_code' AND table_name='policy_signing_keys' "+
			"ORDER BY grantee, privilege_type;")
	if err != nil {
		t.Fatalf("query role_table_grants: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	has := make(map[string]bool, len(lines))
	for _, line := range lines {
		has[strings.TrimSpace(line)] = true
	}

	mustHave := []string{
		"clean_code_policy_steward|INSERT",
		"clean_code_policy_steward|SELECT",
		"clean_code_repo_indexer|SELECT",
		"clean_code_metric_ingestor|SELECT",
		"clean_code_xrepo_aggregator|SELECT",
		"clean_code_solid_batch|SELECT",
		"clean_code_evaluator|SELECT",
		"clean_code_wal_reconciler|SELECT",
		"clean_code_refactor_planner|SELECT",
		"clean_code_management|SELECT",
	}
	for _, g := range mustHave {
		if !has[g] {
			t.Errorf("missing required grant %q; actual grants:\n%s", g, out)
		}
	}

	mustNotHave := []string{
		// Append-only G3: UPDATE and DELETE banned for every
		// grantee.
		"clean_code_policy_steward|UPDATE",
		"clean_code_policy_steward|DELETE",
		"clean_code_repo_indexer|INSERT",
		"clean_code_repo_indexer|UPDATE",
		"clean_code_repo_indexer|DELETE",
		"clean_code_metric_ingestor|INSERT",
		"clean_code_xrepo_aggregator|INSERT",
		"clean_code_solid_batch|INSERT",
		"clean_code_evaluator|INSERT",
		"clean_code_evaluator|UPDATE",
		"clean_code_evaluator|DELETE",
		"clean_code_wal_reconciler|INSERT",
		"clean_code_refactor_planner|INSERT",
		"clean_code_management|INSERT",
		"clean_code_management|UPDATE",
		"clean_code_management|DELETE",
	}
	for _, g := range mustNotHave {
		if has[g] {
			t.Errorf("forbidden grant present %q; tech-spec Sec 7.2 writer isolation breached. Full grants:\n%s", g, out)
		}
	}
}


// TestDiscoverMigrations_findsStage32SeedPair pins the
// iter-18 fix for the production catalog-seed role-grant
// mismatch (the iter-17 evaluator's item 1). The Metric
// Ingestor connection cannot INSERT into
// `clean_code.metric_kind` (grant lives on
// `clean_code_policy_steward` per
// `migrations/0004_roles.up.sql:350-355`), so the seven
// foundation rows are seeded by a schema-owner migration
// instead of by application startup.
//
// This test pins discovery of the 0007 pair (up + down)
// so a future refactor that orphans one half surfaces in
// `go test ./...` BEFORE the round-trip integration test
// even has a chance to run.
func TestDiscoverMigrations_findsStage32SeedPair(t *testing.T) {
t.Parallel()
dir, err := MigrationDir(callerDir(t))
if err != nil {
t.Fatalf("MigrationDir: %v", err)
}
migs, err := DiscoverMigrations(dir)
if err != nil {
t.Fatalf("DiscoverMigrations(%q): %v", dir, err)
}
var got *Migration
for i := range migs {
if migs[i].Version == "0007" {
got = &migs[i]
break
}
}
if got == nil {
t.Fatalf("DiscoverMigrations(%q) missing the Stage 3.2 "+
"0007_seed_foundation_metric_kinds pair", dir)
}
if got.Name != "seed_foundation_metric_kinds" {
t.Errorf("Stage 3.2 seed migration name = %q, want %q",
got.Name, "seed_foundation_metric_kinds")
}
if _, err := os.Stat(got.UpPath); err != nil {
t.Errorf("up path %q: %v", got.UpPath, err)
}
if _, err := os.Stat(got.DownPath); err != nil {
t.Errorf("down path %q: %v", got.DownPath, err)
}
}

// TestSeedFoundationMetricKindsUpSQLBodyMentionsExpectedObjects
// pins the structural shape of
// `0007_seed_foundation_metric_kinds.up.sql`. The seven
// foundation kinds + their (kind, version) pairs MUST be
// present so the metric_sample FK
// (`migrations/0002_measurement.up.sql:348-350`) is always
// satisfied for a fresh schema. A regression that drops
// one of the seven (or bumps a version without also
// bumping the in-process producer at
// `internal/metric_ingestor/metric_kind_catalog.go`)
// surfaces here.
func TestSeedFoundationMetricKindsUpSQLBodyMentionsExpectedObjects(t *testing.T) {
t.Parallel()
dir, err := MigrationDir(callerDir(t))
if err != nil {
t.Fatalf("MigrationDir: %v", err)
}
body, err := os.ReadFile(filepath.Join(dir,
"0007_seed_foundation_metric_kinds.up.sql"))
if err != nil {
t.Fatalf("read up.sql: %v", err)
}
sql := strings.ReplaceAll(string(body), "\r\n", "\n")

wantSubstrs := []string{
// Idempotence -- per the table COMMENT at
// `0001_catalog_lifecycle.up.sql:283-286`, a
// Steward-curated row MUST NOT be overwritten by
// re-running the seed migration. ON CONFLICT DO
// NOTHING preserves Steward precedence; the
// in-process VerifyMetricKindCatalog (called at
// startup) surfaces (kind, version) drift between
// the Steward row and the in-process producer.
"ON CONFLICT (metric_kind) DO NOTHING",
// All seven foundation kinds. Six come from the
// recipe registry at
// `internal/metrics/recipes/registry.go:124-135`;
// the seventh is the modification_count_in_window
// materialiser at
// `internal/metrics/materialisers/modification_count.go`.
"'cyclo'",
"'cognitive_complexity'",
"'loc'",
"'lcom4'",
"'fan_in'",
"'fan_out'",
"'modification_count_in_window'",
// Wrapped in a transaction so a partial seed is
// impossible -- either all seven land or none do.
"BEGIN",
"COMMIT",
// Targets the canonical catalog table -- a typo
// against `metric_kinds` (plural) would silently
// land in a non-existent table and crash with a
// 42P01 (undefined_table).
"INSERT INTO clean_code.metric_kind",
}
for _, want := range wantSubstrs {
if !strings.Contains(sql, want) {
t.Errorf("0007 up.sql missing required substring %q", want)
}
}
}

// TestSeedFoundationMetricKindsDownSQLDeletesSeededRows
// pins the rollback shape. The down migration MUST DELETE
// exactly the rows the up migration INSERTed (and no
// others). FK ON DELETE RESTRICT from `metric_sample`
// blocks the DELETE once metric_sample rows reference a
// kind -- the comment in the down file documents this.
func TestSeedFoundationMetricKindsDownSQLDeletesSeededRows(t *testing.T) {
t.Parallel()
dir, err := MigrationDir(callerDir(t))
if err != nil {
t.Fatalf("MigrationDir: %v", err)
}
body, err := os.ReadFile(filepath.Join(dir,
"0007_seed_foundation_metric_kinds.down.sql"))
if err != nil {
t.Fatalf("read down.sql: %v", err)
}
sql := strings.ReplaceAll(string(body), "\r\n", "\n")
code := sqlCodeOnly(sql)

if !strings.Contains(code, "DELETE FROM clean_code.metric_kind") {
t.Errorf("0007 down.sql missing DELETE FROM clean_code.metric_kind")
}
// The down must enumerate every kind the up seeded;
// a partial down would leave orphan rows the next up
// would silently skip via ON CONFLICT DO NOTHING.
// Check against the raw SQL (sqlCodeOnly strips string
// literals; the kind names ARE string literals).
wantKinds := []string{
"'cyclo'",
"'cognitive_complexity'",
"'loc'",
"'lcom4'",
"'fan_in'",
"'fan_out'",
"'modification_count_in_window'",
}
for _, want := range wantKinds {
if !strings.Contains(sql, want) {
t.Errorf("0007 down.sql missing kind %q", want)
}
}
// Roles + schema belong to 0004 / 0001 respectively;
// 0007 down must only unwind its own rows.
if strings.Contains(code, "DROP ROLE") {
t.Errorf("0007 down.sql contains DROP ROLE")
}
if strings.Contains(code, "DROP SCHEMA") {
t.Errorf("0007 down.sql contains DROP SCHEMA")
}
if strings.Contains(code, "DROP TABLE") {
t.Errorf("0007 down.sql contains DROP TABLE -- " +
"only delete rows, not the table.")
}
}

// TestDiscoverMigrations_findsStage22SolidScopeTreeSeedPair
// pins discovery of the
// `0013_seed_solid_scope_tree_metric_kinds` pair. This
// migration is the catalog companion to Stage 2.2's parse +
// recipe fan-out: when iter-2 of that workstream registered
// the three SOLID-pack scope-tree recipes (`interface_width`,
// `depth_of_inheritance`, `coupling_between_objects`) in
// `internal/metrics/recipes/registry.go::DefaultRegistry`,
// the metric-ingestor startup probe
// `verifyMetricKindCatalog`
// (`cmd/clean-code-metric-ingestor/main.go:369-373`) -- which
// derives expected rows from `recipes.DefaultRegistry()` via
// `metric_ingestor.MetricKindCatalogRowsForRegistry` --
// began demanding three additional `metric_kind` catalog
// rows that the prior seed migration
// (`0007_seed_foundation_metric_kinds.up.sql`) did NOT
// supply. The 0013 pair closes that gap. Without this test
// a future refactor that orphans one half (or renames the
// migration) ships a binary that fails readiness on a fresh
// schema -- and only an integration-level deploy catches it.
func TestDiscoverMigrations_findsStage22SolidScopeTreeSeedPair(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	migs, err := DiscoverMigrations(dir)
	if err != nil {
		t.Fatalf("DiscoverMigrations(%q): %v", dir, err)
	}
	var got *Migration
	for i := range migs {
		if migs[i].Version == "0013" {
			got = &migs[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("DiscoverMigrations(%q) missing the Stage 2.2 "+
			"0013_seed_solid_scope_tree_metric_kinds pair", dir)
	}
	if got.Name != "seed_solid_scope_tree_metric_kinds" {
		t.Errorf("Stage 2.2 seed migration name = %q, want %q",
			got.Name, "seed_solid_scope_tree_metric_kinds")
	}
	if _, err := os.Stat(got.UpPath); err != nil {
		t.Errorf("up path %q: %v", got.UpPath, err)
	}
	if _, err := os.Stat(got.DownPath); err != nil {
		t.Errorf("down path %q: %v", got.DownPath, err)
	}
}

// TestSeedSolidScopeTreeMetricKindsUpSQLBodyMentionsExpectedObjects
// pins the structural shape of
// `0013_seed_solid_scope_tree_metric_kinds.up.sql`. The
// three SOLID-pack scope-tree kinds + their (kind, version)
// pairs MUST be present at `metric_version = 1` so the
// metric_sample FK
// (`migrations/0002_measurement.up.sql:348-350`) is always
// satisfied for samples emitted by the Stage 2.2 recipes on
// a fresh schema. A regression that drops one of the three
// (or bumps a version without also bumping the in-process
// producer at
// `internal/metric_ingestor/metric_kind_catalog.go`)
// surfaces here BEFORE the binary even tries to boot against
// a freshly-migrated database.
func TestSeedSolidScopeTreeMetricKindsUpSQLBodyMentionsExpectedObjects(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir,
		"0013_seed_solid_scope_tree_metric_kinds.up.sql"))
	if err != nil {
		t.Fatalf("read up.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")

	wantSubstrs := []string{
		// Idempotence -- per the table COMMENT at
		// `0001_catalog_lifecycle.up.sql:283-286`, a
		// Steward-curated row MUST NOT be overwritten by
		// re-running the seed migration. ON CONFLICT DO
		// NOTHING preserves Steward precedence; the
		// in-process VerifyMetricKindCatalog (called at
		// startup) surfaces (kind, version) drift between
		// the Steward row and the in-process producer.
		"ON CONFLICT (metric_kind) DO NOTHING",
		// The three SOLID-pack scope-tree kinds Stage 2.2
		// adds to `recipes.DefaultRegistry()`
		// (`internal/metrics/recipes/registry.go::DefaultRegistry`).
		"'interface_width'",
		"'depth_of_inheritance'",
		"'coupling_between_objects'",
		// All three rows must seed at metric_version = 1 to
		// match the recipe `Version()` constants the
		// in-process catalog producer derives at
		// `internal/metric_ingestor/metric_kind_catalog.go::MetricKindCatalogRowsForRegistry`.
		// A future bump that ships the recipe without
		// bumping the seed (or vice versa) surfaces as a
		// startup `ErrMetricKindCatalogVersionMismatch`.
		"'interface_width', 1",
		"'depth_of_inheritance', 1",
		"'coupling_between_objects', 1",
		// All three rows belong to the SOLID pack at the
		// foundation tier per `packToTier`
		// (`internal/metric_ingestor/metric_kind_catalog.go:151-160`).
		"'foundation', 'solid'",
		// Wrapped in a transaction so a partial seed is
		// impossible -- either all three land or none do.
		"BEGIN",
		"COMMIT",
		// Targets the canonical catalog table -- a typo
		// against `metric_kinds` (plural) would silently
		// land in a non-existent table and crash with a
		// 42P01 (undefined_table).
		"INSERT INTO clean_code.metric_kind",
	}
	for _, want := range wantSubstrs {
		if !strings.Contains(sql, want) {
			t.Errorf("0013 up.sql missing required substring %q", want)
		}
	}
}

// TestSeedSolidScopeTreeMetricKindsDownSQLDeletesSeededRows
// pins the rollback shape. The down migration MUST DELETE
// exactly the rows the up migration INSERTed (and no
// others). FK ON DELETE RESTRICT from `metric_sample`
// blocks the DELETE once metric_sample rows reference a
// kind -- the comment in the down file documents this. A
// Steward-curated row bumped to `metric_version >= 2` MUST
// be preserved by the `(metric_kind, metric_version)` tuple
// predicate -- mirroring the precedent set by 0007's down.
func TestSeedSolidScopeTreeMetricKindsDownSQLDeletesSeededRows(t *testing.T) {
	t.Parallel()
	dir, err := MigrationDir(callerDir(t))
	if err != nil {
		t.Fatalf("MigrationDir: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir,
		"0013_seed_solid_scope_tree_metric_kinds.down.sql"))
	if err != nil {
		t.Fatalf("read down.sql: %v", err)
	}
	sql := strings.ReplaceAll(string(body), "\r\n", "\n")
	code := sqlCodeOnly(sql)

	if !strings.Contains(code, "DELETE FROM clean_code.metric_kind") {
		t.Errorf("0013 down.sql missing DELETE FROM clean_code.metric_kind")
	}
	// The down must enumerate every kind the up seeded;
	// a partial down would leave orphan rows the next up
	// would silently skip via ON CONFLICT DO NOTHING.
	// Check against the raw SQL (sqlCodeOnly strips string
	// literals; the kind names ARE string literals).
	wantKinds := []string{
		"'interface_width'",
		"'depth_of_inheritance'",
		"'coupling_between_objects'",
	}
	for _, want := range wantKinds {
		if !strings.Contains(sql, want) {
			t.Errorf("0013 down.sql missing kind %q", want)
		}
	}
	// The tuple predicate must scope to v=1 specifically so
	// a Steward-customized row at v>=2 is preserved. A
	// regression that uses `WHERE metric_kind IN (...)`
	// without the version constraint would silently delete
	// Steward customizations.
	if !strings.Contains(code, "metric_version") {
		t.Errorf("0013 down.sql missing metric_version tuple predicate -- " +
			"a Steward-curated row at v>=2 would be silently deleted")
	}
	// Roles + schema belong to 0004 / 0001 respectively;
	// 0013 down must only unwind its own rows.
	if strings.Contains(code, "DROP ROLE") {
		t.Errorf("0013 down.sql contains DROP ROLE")
	}
	if strings.Contains(code, "DROP SCHEMA") {
		t.Errorf("0013 down.sql contains DROP SCHEMA")
	}
	if strings.Contains(code, "DROP TABLE") {
		t.Errorf("0013 down.sql contains DROP TABLE -- " +
			"only delete rows, not the table.")
	}
}