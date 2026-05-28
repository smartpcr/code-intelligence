//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/lib/pq" // PostgreSQL driver
)

// requireEnv returns the value of the named environment variable or skips the
// test when the variable is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("required env var %s is not set", name)
	}
	return v
}

// resolveComposeFile returns the absolute path to the docker-compose file.
// It checks COMPOSE_FILE env var first, then walks up from the current
// working directory to find the repo root containing the compose file.
func resolveComposeFile() string {
	const relPath = "tests/e2e/phase-09-audit-wal/docker-compose.yml"

	// If COMPOSE_FILE is set and absolute, use it directly.
	if cf := os.Getenv("COMPOSE_FILE"); cf != "" {
		if filepath.IsAbs(cf) {
			return cf
		}
	}

	// Walk up from cwd to find the repo root that contains the compose file.
	// Go test sets cwd to the package directory, so we need to ascend.
	dir, _ := os.Getwd()
	for {
		candidate := filepath.Join(dir, relPath)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Fallback: return the relative path and let docker compose fail with a
	// clear error if the file truly doesn't exist.
	return relPath
}

// ---------------------------------------------------------------------------
// Shared test state scoped to a single scenario.
// ---------------------------------------------------------------------------

type auditWalReconcilerState struct {
	pgURL string
	db    *sql.DB

	// WAL frame payload stashed between Given and When/Then.
	targetTable string
	walRowID    string
	walCaller   string

	// Results captured during When steps.
	permissionErr error
	insertResults map[string]error
}

func newAuditWalReconcilerState(pgURL string) *auditWalReconcilerState {
	return &auditWalReconcilerState{
		pgURL:         pgURL,
		insertResults: make(map[string]error),
	}
}

func (s *auditWalReconcilerState) openDB() error {
	if s.db != nil {
		return nil
	}
	db, err := sql.Open("postgres", s.pgURL)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	db.SetMaxOpenConns(5)
	db.SetConnMaxLifetime(30 * time.Second)
	s.db = db
	return nil
}

func (s *auditWalReconcilerState) closeDB() {
	if s.db != nil {
		s.db.Close()
		s.db = nil
	}
}

// ---------------------------------------------------------------------------
// Step implementations – Scenario: reconciler-replays-missing-rows
// ---------------------------------------------------------------------------

func (s *auditWalReconcilerState) aWALFrameForARowMissingFrom(table string) error {
	if err := s.openDB(); err != nil {
		return err
	}
	s.targetTable = table
	s.walRowID = fmt.Sprintf("wal-replay-test-%d", time.Now().UnixNano())

	// Ensure the row does NOT exist yet.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var exists bool
	query := fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM %s WHERE id = $1)`, pq.QuoteIdentifier(s.targetTable))
	if err := s.db.QueryRowContext(ctx, query, s.walRowID).Scan(&exists); err != nil {
		return fmt.Errorf("check existence in %s: %w", s.targetTable, err)
	}
	if exists {
		return fmt.Errorf("row %s already exists in %s", s.walRowID, s.targetTable)
	}

	// Write a WAL frame that the reconciler will pick up.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO wal_inbox (target_table, row_id, payload, created_at)
		 VALUES ($1, $2, $3, NOW())`,
		s.targetTable, s.walRowID, fmt.Sprintf(`{"id":"%s","detail":"e2e-replay-test"}`, s.walRowID))
	return err
}

func (s *auditWalReconcilerState) theReconcilerRuns() error {
	if err := s.openDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Trigger the reconciler by calling its replay function.
	_, err := s.db.ExecContext(ctx, `SELECT reconciler_replay()`)
	return err
}

func (s *auditWalReconcilerState) theRowIsINSERTedAndReadsReturnIt(table string) error {
	if err := s.openDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var exists bool
	query := fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM %s WHERE id = $1)`, pq.QuoteIdentifier(table))
	if err := s.db.QueryRowContext(ctx, query, s.walRowID).Scan(&exists); err != nil {
		return fmt.Errorf("read from %s: %w", table, err)
	}
	if !exists {
		return fmt.Errorf("expected row %s in %s after reconciler replay, but it was not found", s.walRowID, table)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Step implementations – Scenario: reconciler-preserves-caller
// ---------------------------------------------------------------------------

func (s *auditWalReconcilerState) aWALFrameForTableWithCaller(table, caller string) error {
	if err := s.openDB(); err != nil {
		return err
	}
	s.targetTable = table
	s.walCaller = caller
	s.walRowID = fmt.Sprintf("wal-caller-test-%d", time.Now().UnixNano())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO wal_inbox (target_table, row_id, payload, created_at)
		 VALUES ($1, $2, $3, NOW())`,
		s.targetTable, s.walRowID,
		fmt.Sprintf(`{"id":"%s","caller":"%s","detail":"caller-preservation-test"}`, s.walRowID, s.walCaller))
	return err
}

func (s *auditWalReconcilerState) theReconcilerReplaysAfterRestart() error {
	// Restart the wal-reconciler container to simulate a process crash/restart,
	// then wait for the reconciler to replay pending WAL frames.
	composeFile := resolveComposeFile()

	// docker compose restart wal-reconciler
	cmd := exec.Command("docker", "compose", "-f", composeFile, "restart", "wal-reconciler")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("restart wal-reconciler: %s: %w", string(out), err)
	}

	// Wait for the reconciler to come back healthy and process pending frames.
	time.Sleep(5 * time.Second)

	// Trigger replay to ensure pending WAL frames are processed.
	return s.theReconcilerRuns()
}

func (s *auditWalReconcilerState) theReplayedRowsCallerColumnEquals(expectedCaller string) error {
	if err := s.openDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var caller string
	query := fmt.Sprintf(`SELECT caller FROM %s WHERE id = $1`, pq.QuoteIdentifier(s.targetTable))
	if err := s.db.QueryRowContext(ctx, query, s.walRowID).Scan(&caller); err != nil {
		return fmt.Errorf("read caller from %s: %w", s.targetTable, err)
	}
	if caller != expectedCaller {
		return fmt.Errorf("expected caller %q but got %q", expectedCaller, caller)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Step implementations – Scenario: reconciler-cannot-write-non-audit
// ---------------------------------------------------------------------------

func (s *auditWalReconcilerState) theRoleBoundToASession(role string) error {
	if err := s.openDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := s.db.ExecContext(ctx, fmt.Sprintf("SET ROLE %s", pq.QuoteIdentifier(role)))
	if err != nil {
		return fmt.Errorf("SET ROLE %s: %w", role, err)
	}
	return nil
}

func (s *auditWalReconcilerState) theReconcilerAttemptsINSERTInto(table string) error {
	if err := s.openDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rowID := fmt.Sprintf("perm-test-%d", time.Now().UnixNano())
	// Use a minimal but schema-valid INSERT for metric_sample (non-audit table).
	_, s.permissionErr = s.db.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO %s (id, created_at) VALUES ($1, NOW())`, pq.QuoteIdentifier(table)), rowID)
	return nil
}

func (s *auditWalReconcilerState) postgreSQLReturnsPermissionDenied() error {
	if s.permissionErr == nil {
		return fmt.Errorf("expected permission denied error, but INSERT succeeded")
	}
	errMsg := strings.ToLower(s.permissionErr.Error())
	// PostgreSQL error code 42501 = insufficient_privilege; also match the
	// human-readable "permission denied" text that lib/pq surfaces.
	if !strings.Contains(errMsg, "permission denied") && !strings.Contains(errMsg, "42501") {
		return fmt.Errorf("expected PostgreSQL permission denied (42501), got: %v", s.permissionErr)
	}
	return nil
}

func (s *auditWalReconcilerState) insertIntoTableSucceeds(table string) error {
	if err := s.openDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rowID := fmt.Sprintf("audit-perm-test-%d", time.Now().UnixNano())
	// Use full-row INSERT matching audit table schemas so NOT NULL constraints
	// don't mask the permission check we're actually testing.
	var err error
	switch table {
	case "evaluation_run":
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO evaluation_run (id, caller, status, created_at)
			 VALUES ($1, 'e2e-perm-test', 'pending', NOW())`, rowID)
	case "evaluation_verdict":
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO evaluation_verdict (id, run_id, verdict, created_at)
			 VALUES ($1, 'e2e-stub-run', 'pass', NOW())`, rowID)
	case "finding":
		_, err = s.db.ExecContext(ctx,
			`INSERT INTO finding (id, run_id, rule_id, severity, message, created_at)
			 VALUES ($1, 'e2e-stub-run', 'e2e-rule', 'info', 'permission test', NOW())`, rowID)
	default:
		_, err = s.db.ExecContext(ctx,
			fmt.Sprintf(`INSERT INTO %s (id, created_at) VALUES ($1, NOW())`, pq.QuoteIdentifier(table)), rowID)
	}
	if err != nil {
		s.insertResults[table] = err
		return fmt.Errorf("expected INSERT into %s to succeed, got: %w", table, err)
	}
	s.insertResults[table] = nil
	return nil
}

// ---------------------------------------------------------------------------
// Scenario initializer & test entrypoint
// ---------------------------------------------------------------------------

func InitializeScenario_audit_wal_and_reliability_hardening_audit_wal_reconciler_replay_only(ctx *godog.ScenarioContext) {
	pgURL := os.Getenv("CLEAN_CODE_PG_URL")
	if pgURL == "" {
		pgURL = "postgres://clean_code_admin:e2e_test_password@localhost:5432/clean_code?sslmode=disable"
	}

	state := newAuditWalReconcilerState(pgURL)

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		state.closeDB()
		return ctx, nil
	})

	// Scenario: reconciler-replays-missing-rows
	ctx.Step(`^a WAL frame for a row missing from "([^"]*)"$`, state.aWALFrameForARowMissingFrom)
	ctx.Step(`^the reconciler runs$`, state.theReconcilerRuns)
	ctx.Step(`^the row is INSERTed into "([^"]*)" and reads return it$`, state.theRowIsINSERTedAndReadsReturnIt)

	// Scenario: reconciler-preserves-caller
	ctx.Step(`^a WAL frame for "([^"]*)" with caller "([^"]*)"$`, state.aWALFrameForTableWithCaller)
	ctx.Step(`^the reconciler replays after restart$`, state.theReconcilerReplaysAfterRestart)
	ctx.Step(`^the replayed row's caller column equals "([^"]*)"$`, state.theReplayedRowsCallerColumnEquals)

	// Scenario: reconciler-cannot-write-non-audit
	ctx.Step(`^the "([^"]*)" role bound to a session$`, state.theRoleBoundToASession)
	ctx.Step(`^the reconciler attempts INSERT into "([^"]*)"$`, state.theReconcilerAttemptsINSERTInto)
	ctx.Step(`^PostgreSQL returns permission denied$`, state.postgreSQLReturnsPermissionDenied)
	ctx.Step(`^INSERT into "([^"]*)" succeeds$`, state.insertIntoTableSucceeds)
}

func TestE2E_audit_wal_and_reliability_hardening_audit_wal_reconciler_replay_only(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_audit_wal_and_reliability_hardening_audit_wal_reconciler_replay_only,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"audit_wal_and_reliability_hardening_audit_wal_reconciler_replay_only.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("e2e suite failed")
	}
}
