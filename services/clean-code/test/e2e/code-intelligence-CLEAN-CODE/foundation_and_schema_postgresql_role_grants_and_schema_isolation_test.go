//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// requireEnv returns the value of the named environment variable or skips the
// test when the variable is unset / empty.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping e2e test", name)
	}
	return v
}

// moduleRoot returns the absolute path to the Go module root
// (services/clean-code/) relative to this source file.
func moduleRoot() string {
	_, src, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(src), "..", "..", "..")
}

// runMake executes a make target in the module root directory.
func runMake(target string) (string, error) {
	cmd := exec.Command("make", target)
	cmd.Dir = moduleRoot()
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// isPermissionDenied returns true when the PostgreSQL error indicates
// insufficient privilege (SQLSTATE 42501).
func isPermissionDenied(err error) bool {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42501"
	}
	return false
}

// ---------------------------------------------------------------------------
// role definitions per architecture G1 + tech-spec Sec 7.2
// ---------------------------------------------------------------------------

// writerRoles lists every application-level writer role.
var writerRoles = []string{
	"clean_code_metric_ingestor",
	"clean_code_aggregator",
	"clean_code_evaluator",
	"clean_code_solid_batch",
	"clean_code_wal_reconciler",
}

// nonAuditWriterRoles are roles that must NOT write audit tables.
var nonAuditWriterRoles = []string{
	"clean_code_metric_ingestor",
	"clean_code_aggregator",
	"clean_code_policy_steward",
	"clean_code_refactor_planner",
	"clean_code_repo_indexer",
	"clean_code_management",
}

// auditRoles are the three roles allowed to write audit tables.
var auditRoles = []string{
	"clean_code_evaluator",
	"clean_code_solid_batch",
	"clean_code_wal_reconciler",
}

// auditTables are the three audit tables per tech-spec Sec 7.2.
var auditTables = []string{
	"evaluation_run",
	"evaluation_verdict",
	"finding",
}

// measurementTables are tables shared by the ingestor + aggregator.
var measurementTables = []string{
	"metric_sample",
	"metric_retraction",
	"metric_sample_active",
}

// roleOwnership maps each writer role to the tables it may INSERT into.
var roleOwnership = map[string][]string{
	"clean_code_metric_ingestor": {"metric_sample", "metric_retraction", "metric_sample_active"},
	"clean_code_aggregator":     {"metric_sample", "metric_retraction", "metric_sample_active"},
	"clean_code_evaluator":      {"evaluation_run", "evaluation_verdict", "finding"},
	"clean_code_solid_batch":    {"evaluation_run", "evaluation_verdict", "finding"},
	"clean_code_wal_reconciler": {"evaluation_run", "evaluation_verdict", "finding"},
}

// allWriterTables returns the union of all tables that any writer role owns.
func allWriterTables() []string {
	seen := map[string]bool{}
	var result []string
	for _, tables := range roleOwnership {
		for _, t := range tables {
			if !seen[t] {
				seen[t] = true
				result = append(result, t)
			}
		}
	}
	return result
}

// owns returns true when the role is allowed to INSERT into the table.
func owns(role, table string) bool {
	for _, t := range roleOwnership[role] {
		if t == table {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// shared state
// ---------------------------------------------------------------------------

type roleIsolationState struct {
	adminDB *sql.DB // connected as superuser / admin

	// Scenario: role-isolation-matrix
	disallowedResults []insertResult // disallowed attempts
	allowedResults    []insertResult // allowed attempts

	// Scenario: audit-tables-three-writer-grant
	auditResults      map[string][]insertResult // role -> results
	nonAuditResults   []insertResult
	updateDeleteErrs  []error

	// Scenario: aggregator-also-writes-active-pointer
	lastInsertErr error
}

type insertResult struct {
	role  string
	table string
	err   error
}

func newRoleIsolationState(dsn string) (*roleIsolationState, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &roleIsolationState{
		adminDB:      db,
		auditResults: make(map[string][]insertResult),
	}, nil
}

func (s *roleIsolationState) close() {
	if s.adminDB != nil {
		s.adminDB.Close()
	}
}

func (s *roleIsolationState) cleanup() {
	if s.adminDB != nil {
		_, _ = s.adminDB.ExecContext(context.Background(),
			"DROP SCHEMA IF EXISTS clean_code CASCADE")
	}
}

// execAsRole runs a SQL statement within a transaction that has SET ROLE to
// the specified role. This lets us test per-role permissions from a single
// superuser connection.
func (s *roleIsolationState) execAsRole(role, sql string, args ...interface{}) error {
	tx, err := s.adminDB.BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(context.Background(),
		fmt.Sprintf("SET LOCAL ROLE %s", pq.QuoteIdentifier(role))); err != nil {
		return fmt.Errorf("set role %s: %w", role, err)
	}

	_, err = tx.ExecContext(context.Background(), sql, args...)
	if err != nil {
		return err
	}

	// Roll back so rows don't accumulate and contaminate other steps.
	return tx.Rollback()
}

// ---------------------------------------------------------------------------
// step: schema + roles exist after migrate-up
// ---------------------------------------------------------------------------

func (s *roleIsolationState) schemaExistsAfterMigrateUpWithAllRolesProvisioned() error {
	s.cleanup()
	out, err := runMake("migrate-up")
	if err != nil {
		return fmt.Errorf("make migrate-up failed: %w\noutput: %s", err, out)
	}

	// Ensure parent rows exist for FK references used in INSERT probes.
	_, _ = s.adminDB.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ('00000000-0000-0000-0000-000000000001', 'e2e-test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`)

	return nil
}

// ---------------------------------------------------------------------------
// Scenario: role-isolation-matrix  —  disallowed INSERTs
// ---------------------------------------------------------------------------

// buildInsertSQL returns a minimal INSERT statement for a given table.
func buildInsertSQL(table string) string {
	switch table {
	case "metric_sample":
		return `INSERT INTO clean_code.metric_sample (repo_id, metric_name, value)
			VALUES ('00000000-0000-0000-0000-000000000001', 'e2e_probe', 0)`
	case "metric_retraction":
		return `INSERT INTO clean_code.metric_retraction (repo_id, metric_name, reason)
			VALUES ('00000000-0000-0000-0000-000000000001', 'e2e_probe', 'e2e')`
	case "metric_sample_active":
		return `INSERT INTO clean_code.metric_sample_active (repo_id, metric_name, sample_id)
			VALUES ('00000000-0000-0000-0000-000000000001', 'e2e_probe',
				'00000000-0000-0000-0000-000000000099')`
	case "evaluation_run":
		return `INSERT INTO clean_code.evaluation_run (repo_id)
			VALUES ('00000000-0000-0000-0000-000000000001')`
	case "evaluation_verdict":
		return `INSERT INTO clean_code.evaluation_verdict (repo_id, verdict)
			VALUES ('00000000-0000-0000-0000-000000000001', 'pass')`
	case "finding":
		return `INSERT INTO clean_code.finding (repo_id, delta)
			VALUES ('00000000-0000-0000-0000-000000000001', 'new')`
	default:
		return fmt.Sprintf("INSERT INTO clean_code.%s DEFAULT VALUES",
			strings.ReplaceAll(table, "'", ""))
	}
}

func (s *roleIsolationState) eachWriterRoleAttemptsInsertOutsideOwnership() error {
	s.disallowedResults = nil
	s.allowedResults = nil

	tables := allWriterTables()
	for _, role := range writerRoles {
		for _, table := range tables {
			err := s.execAsRole(role, buildInsertSQL(table))
			r := insertResult{role: role, table: table, err: err}
			if owns(role, table) {
				s.allowedResults = append(s.allowedResults, r)
			} else {
				s.disallowedResults = append(s.disallowedResults, r)
			}
		}
	}
	return nil
}

func (s *roleIsolationState) permissionDeniedForEveryDisallowedInsert() error {
	for _, r := range s.disallowedResults {
		if r.err == nil {
			return fmt.Errorf("role %s was allowed to INSERT into %s (should be denied)",
				r.role, r.table)
		}
		if !isPermissionDenied(r.err) {
			return fmt.Errorf("role %s on table %s: expected permission denied, got: %v",
				r.role, r.table, r.err)
		}
	}
	return nil
}

func (s *roleIsolationState) eachWriterRoleSucceedsOnOwnedTables() error {
	for _, r := range s.allowedResults {
		if r.err != nil {
			return fmt.Errorf("role %s should be allowed to INSERT into %s, got: %v",
				r.role, r.table, r.err)
		}
	}
	return nil
}

func (s *roleIsolationState) ingestorCanInsertMeasurementTables() error {
	for _, table := range measurementTables {
		err := s.execAsRole("clean_code_metric_ingestor", buildInsertSQL(table))
		if err != nil {
			return fmt.Errorf("ingestor INSERT into %s failed: %v", table, err)
		}
	}
	return nil
}

func (s *roleIsolationState) aggregatorCanInsertMeasurementTables() error {
	for _, table := range measurementTables {
		err := s.execAsRole("clean_code_aggregator", buildInsertSQL(table))
		if err != nil {
			return fmt.Errorf("aggregator INSERT into %s failed: %v", table, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: audit-tables-three-writer-grant
// ---------------------------------------------------------------------------

func (s *roleIsolationState) roleAttemptsInsertOnAuditTables(role string) error {
	s.auditResults[role] = nil
	for _, table := range auditTables {
		err := s.execAsRole(role, buildInsertSQL(table))
		s.auditResults[role] = append(s.auditResults[role],
			insertResult{role: role, table: table, err: err})
	}
	return nil
}

func (s *roleIsolationState) allThreeAuditInsertsByRoleSucceed(roleShort string) error {
	// Map short names from Gherkin to full role names.
	roleMap := map[string]string{
		"evaluator":      "clean_code_evaluator",
		"solid_batch":    "clean_code_solid_batch",
		"wal_reconciler": "clean_code_wal_reconciler",
	}
	role := roleMap[roleShort]
	if role == "" {
		return fmt.Errorf("unknown role short name %q", roleShort)
	}
	for _, r := range s.auditResults[role] {
		if r.err != nil {
			return fmt.Errorf("role %s INSERT into %s should succeed, got: %v",
				r.role, r.table, r.err)
		}
	}
	return nil
}

func (s *roleIsolationState) anyNonAuditWriterRoleAttemptsInsertOnAuditTables() error {
	s.nonAuditResults = nil
	for _, role := range nonAuditWriterRoles {
		for _, table := range auditTables {
			err := s.execAsRole(role, buildInsertSQL(table))
			s.nonAuditResults = append(s.nonAuditResults,
				insertResult{role: role, table: table, err: err})
		}
	}
	return nil
}

func (s *roleIsolationState) permissionDeniedForEveryNonAuditRoleOnAuditTables() error {
	for _, r := range s.nonAuditResults {
		if r.err == nil {
			return fmt.Errorf("role %s was allowed to INSERT into audit table %s (should be denied)",
				r.role, r.table)
		}
		if !isPermissionDenied(r.err) {
			return fmt.Errorf("role %s on audit table %s: expected permission denied, got: %v",
				r.role, r.table, r.err)
		}
	}
	return nil
}

func (s *roleIsolationState) updateAndDeleteRevokedIncludingPublicOnAuditTables() error {
	s.updateDeleteErrs = nil

	// Collect all named roles to test (writer roles + non-audit roles).
	allRoles := make(map[string]bool)
	for _, r := range writerRoles {
		allRoles[r] = true
	}
	for _, r := range nonAuditWriterRoles {
		allRoles[r] = true
	}

	for role := range allRoles {
		for _, table := range auditTables {
			// Test UPDATE
			err := s.execAsRole(role, fmt.Sprintf(
				"UPDATE clean_code.%s SET repo_id = repo_id WHERE FALSE", table))
			if err != nil {
				if !isPermissionDenied(err) {
					return fmt.Errorf("role %s UPDATE on %s: expected permission denied, got: %v",
						role, table, err)
				}
			} else {
				return fmt.Errorf("role %s was allowed to UPDATE audit table %s (should be denied)",
					role, table)
			}

			// Test DELETE
			err = s.execAsRole(role, fmt.Sprintf(
				"DELETE FROM clean_code.%s WHERE FALSE", table))
			if err != nil {
				if !isPermissionDenied(err) {
					return fmt.Errorf("role %s DELETE on %s: expected permission denied, got: %v",
						role, table, err)
				}
			} else {
				return fmt.Errorf("role %s was allowed to DELETE from audit table %s (should be denied)",
					role, table)
			}
		}
	}

	// Explicitly verify PUBLIC has no UPDATE or DELETE on audit tables.
	// We query pg_class + aclexplode to confirm PUBLIC (grantee = 0) has
	// neither UPDATE nor DELETE privilege.
	for _, table := range auditTables {
		var count int
		err := s.adminDB.QueryRowContext(context.Background(), `
			SELECT COUNT(*)
			FROM (
				SELECT (aclexplode(relacl)).grantee AS grantee,
				       (aclexplode(relacl)).privilege_type AS priv
				FROM pg_class c
				JOIN pg_namespace n ON n.oid = c.relnamespace
				WHERE n.nspname = 'clean_code' AND c.relname = $1
			) acl
			WHERE acl.grantee = 0
			  AND acl.priv IN ('UPDATE', 'DELETE')
		`, table).Scan(&count)
		if err != nil {
			return fmt.Errorf("querying PUBLIC ACL on %s: %v", table, err)
		}
		if count > 0 {
			return fmt.Errorf("PUBLIC has UPDATE or DELETE privilege on audit table %s (%d grants found)",
				table, count)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Scenario: aggregator-also-writes-active-pointer
// ---------------------------------------------------------------------------

func (s *roleIsolationState) aggregatorInsertsRowWithPackSystemIntoMetricSample() error {
	s.lastInsertErr = s.execAsRole("clean_code_aggregator", `
		INSERT INTO clean_code.metric_sample (repo_id, metric_name, value, pack)
		VALUES ('00000000-0000-0000-0000-000000000001', 'e2e_agg_probe', 42, 'system')
	`)
	return nil
}

func (s *roleIsolationState) aggregatorMetricSampleInsertSucceeds() error {
	if s.lastInsertErr != nil {
		return fmt.Errorf("aggregator INSERT into metric_sample with pack=system failed: %v",
			s.lastInsertErr)
	}
	return nil
}

func (s *roleIsolationState) aggregatorUpsertsMetricSampleActivePointer() error {
	s.lastInsertErr = s.execAsRole("clean_code_aggregator", `
		INSERT INTO clean_code.metric_sample_active (repo_id, metric_name, sample_id)
		VALUES ('00000000-0000-0000-0000-000000000001', 'e2e_agg_probe',
			'00000000-0000-0000-0000-000000000099')
		ON CONFLICT (repo_id, metric_name) DO UPDATE SET sample_id = EXCLUDED.sample_id
	`)
	return nil
}

func (s *roleIsolationState) aggregatorMetricSampleActiveUpsertSucceeds() error {
	if s.lastInsertErr != nil {
		return fmt.Errorf("aggregator upsert into metric_sample_active failed: %v",
			s.lastInsertErr)
	}
	return nil
}

// aggregatorAttemptsInsertWithPackBaseIntoMetricSample tests the application-layer
// per-metric_kind partitioning check: aggregator must NOT insert pack='base' rows.
// The schema enforces this via a CHECK constraint or trigger that rejects
// pack='base' writes from the aggregator role context.
func (s *roleIsolationState) aggregatorAttemptsInsertWithPackBaseIntoMetricSample() error {
	s.lastInsertErr = s.execAsRole("clean_code_aggregator", `
		INSERT INTO clean_code.metric_sample (repo_id, metric_name, value, pack)
		VALUES ('00000000-0000-0000-0000-000000000001', 'e2e_agg_base_probe', 1, 'base')
	`)
	return nil
}

func (s *roleIsolationState) applicationLayerRejectsBasePackInsert() error {
	if s.lastInsertErr == nil {
		return fmt.Errorf("aggregator INSERT with pack='base' should be rejected by " +
			"application-layer per-metric_kind partitioning check, but it succeeded")
	}
	// Accept either a check-constraint violation (23514), a trigger-raised
	// exception (P0001), or a permission denied (42501) — the implementation
	// may use any of these enforcement mechanisms.
	var pgErr *pq.Error
	if errors.As(s.lastInsertErr, &pgErr) {
		switch pgErr.Code {
		case "23514", "P0001", "42501":
			return nil
		}
	}
	return fmt.Errorf("aggregator pack='base' INSERT rejected with unexpected error "+
		"(expected check_violation/raise_exception/permission_denied): %v", s.lastInsertErr)
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_foundation_and_schema_postgresql_role_grants_and_schema_isolation(ctx *godog.ScenarioContext) {
	var state *roleIsolationState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		dsn := os.Getenv("CLEAN_CODE_PG_URL")
		if dsn == "" {
			return ctx, fmt.Errorf("CLEAN_CODE_PG_URL is not set")
		}
		var err error
		state, err = newRoleIsolationState(dsn)
		if err != nil {
			return ctx, err
		}
		return ctx, nil
	})

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if state != nil {
			state.cleanup()
			state.close()
		}
		return ctx, nil
	})

	// Shared Given
	ctx.Step(`^the clean_code schema exists after migrate-up with all roles provisioned$`,
		func() error { return state.schemaExistsAfterMigrateUpWithAllRolesProvisioned() })

	// role-isolation-matrix
	ctx.Step(`^each writer role attempts INSERT on a table outside its writer-ownership$`,
		func() error { return state.eachWriterRoleAttemptsInsertOutsideOwnership() })
	ctx.Step(`^PostgreSQL returns permission denied for every disallowed INSERT$`,
		func() error { return state.permissionDeniedForEveryDisallowedInsert() })
	ctx.Step(`^each writer role succeeds on its owned tables$`,
		func() error { return state.eachWriterRoleSucceedsOnOwnedTables() })
	ctx.Step(`^the ingestor role can INSERT into metric_sample and metric_retraction and metric_sample_active$`,
		func() error { return state.ingestorCanInsertMeasurementTables() })
	ctx.Step(`^the aggregator role can INSERT into metric_sample and metric_retraction and metric_sample_active$`,
		func() error { return state.aggregatorCanInsertMeasurementTables() })

	// audit-tables-three-writer-grant
	ctx.Step(`^the clean_code_evaluator role attempts INSERT on evaluation_run and evaluation_verdict and finding$`,
		func() error { return state.roleAttemptsInsertOnAuditTables("clean_code_evaluator") })
	ctx.Step(`^all three audit INSERTs by the evaluator succeed$`,
		func() error { return state.allThreeAuditInsertsByRoleSucceed("evaluator") })
	ctx.Step(`^the clean_code_solid_batch role attempts INSERT on evaluation_run and evaluation_verdict and finding$`,
		func() error { return state.roleAttemptsInsertOnAuditTables("clean_code_solid_batch") })
	ctx.Step(`^all three audit INSERTs by the solid_batch succeed$`,
		func() error { return state.allThreeAuditInsertsByRoleSucceed("solid_batch") })
	ctx.Step(`^the clean_code_wal_reconciler role attempts INSERT on evaluation_run and evaluation_verdict and finding$`,
		func() error { return state.roleAttemptsInsertOnAuditTables("clean_code_wal_reconciler") })
	ctx.Step(`^all three audit INSERTs by the wal_reconciler succeed$`,
		func() error { return state.allThreeAuditInsertsByRoleSucceed("wal_reconciler") })
	ctx.Step(`^any non-audit writer role attempts INSERT on the three audit tables$`,
		func() error { return state.anyNonAuditWriterRoleAttemptsInsertOnAuditTables() })
	ctx.Step(`^PostgreSQL returns permission denied for every non-audit role on audit tables$`,
		func() error { return state.permissionDeniedForEveryNonAuditRoleOnAuditTables() })
	ctx.Step(`^UPDATE and DELETE are revoked from all roles including PUBLIC on the three audit tables$`,
		func() error { return state.updateAndDeleteRevokedIncludingPublicOnAuditTables() })

	// aggregator-also-writes-active-pointer
	ctx.Step(`^the aggregator role INSERTs a row with pack 'system' into metric_sample$`,
		func() error { return state.aggregatorInsertsRowWithPackSystemIntoMetricSample() })
	ctx.Step(`^the aggregator metric_sample INSERT succeeds$`,
		func() error { return state.aggregatorMetricSampleInsertSucceeds() })
	ctx.Step(`^the aggregator role upserts the matching metric_sample_active pointer$`,
		func() error { return state.aggregatorUpsertsMetricSampleActivePointer() })
	ctx.Step(`^the aggregator metric_sample_active upsert succeeds$`,
		func() error { return state.aggregatorMetricSampleActiveUpsertSucceeds() })
	ctx.Step(`^the aggregator role attempts INSERT with pack 'base' into metric_sample$`,
		func() error { return state.aggregatorAttemptsInsertWithPackBaseIntoMetricSample() })
	ctx.Step(`^the application-layer per-metric_kind partitioning check rejects the base pack INSERT$`,
		func() error { return state.applicationLayerRejectsBasePackInsert() })
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_foundation_and_schema_postgresql_role_grants_and_schema_isolation(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_foundation_and_schema_postgresql_role_grants_and_schema_isolation,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"foundation_and_schema_postgresql_role_grants_and_schema_isolation.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}