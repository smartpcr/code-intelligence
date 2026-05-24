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
