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
	// src is .../test/e2e/code-intelligence-CLEAN-CODE/<file>.go
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

// isConstraintViolation checks whether a PostgreSQL error matches the given
// SQLSTATE code (e.g. "22P02" for invalid_text_representation, "23514" for
// check_violation).
func isConstraintViolation(err error, codes ...string) bool {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		for _, c := range codes {
			if pgErr.Code == pq.ErrorCode(c) {
				return true
			}
		}
	}
	return false
}

// isEnumOrCheckViolation returns true when the error is either an
// invalid_text_representation (22P02 — enum cast failure) or a
// check_violation (23514 — CHECK constraint).
func isEnumOrCheckViolation(err error) bool {
	return isConstraintViolation(err, "22P02", "23514")
}

// ---------------------------------------------------------------------------
// shared state for a single scenario run
// ---------------------------------------------------------------------------

type policyMigrationState struct {
	db            *sql.DB
	lastInsertErr error
	columns       []string // populated by "listing columns of …"

	// For policy-activation-latest-row-wins
	latestPolicyID string
	activePolicyID string
}

func newPolicyMigrationState(dsn string) (*policyMigrationState, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &policyMigrationState{db: db}, nil
}

func (s *policyMigrationState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

func (s *policyMigrationState) cleanup() {
	if s.db != nil {
		_, _ = s.db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS clean_code CASCADE")
	}
}

// ensureMigrateUp drops existing schema and runs migrate-up fresh.
func (s *policyMigrationState) ensureMigrateUp() error {
	s.cleanup()
	out, err := runMake("migrate-up")
	if err != nil {
		return fmt.Errorf("make migrate-up failed: %w\noutput: %s", err, out)
	}
	return nil
}

// ---------------------------------------------------------------------------
// step implementations — evaluation_verdict
// ---------------------------------------------------------------------------

func (s *policyMigrationState) theEvaluationVerdictTableExistsAfterMigrateUp() error {
	return s.ensureMigrateUp()
}

// verdictSeq is a monotonic counter to generate unique rows per scenario.
var verdictSeq int

func (s *policyMigrationState) anINSERTSuppliesVerdict(verdict string) error {
	verdictSeq++

	// Ensure parent rows exist for FK references.
	_, _ = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ('00000000-0000-0000-0000-000000000001', 'test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`)

	_, s.lastInsertErr = s.db.ExecContext(context.Background(), fmt.Sprintf(`
		INSERT INTO clean_code.evaluation_verdict (repo_id, verdict)
		VALUES ('00000000-0000-0000-0000-000000000001', '%s')
	`, verdict))
	return nil
}

func (s *policyMigrationState) postgreSQLRejectsTheVerdictInsert() error {
	if s.lastInsertErr == nil {
		return fmt.Errorf("expected INSERT to be rejected, but it succeeded")
	}
	if !isEnumOrCheckViolation(s.lastInsertErr) {
		return fmt.Errorf("expected enum/check constraint violation, got: %v", s.lastInsertErr)
	}
	return nil
}

func (s *policyMigrationState) postgreSQLAcceptsTheVerdictInsert() error {
	if s.lastInsertErr != nil {
		return fmt.Errorf("expected INSERT to succeed, but got: %v", s.lastInsertErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// step implementations — degraded_reason
// ---------------------------------------------------------------------------

var degradedReasonSeq int

func (s *policyMigrationState) anINSERTSuppliesDegradedReason(reason string) error {
	degradedReasonSeq++

	_, _ = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ('00000000-0000-0000-0000-000000000001', 'test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`)

	_, s.lastInsertErr = s.db.ExecContext(context.Background(), fmt.Sprintf(`
		INSERT INTO clean_code.evaluation_verdict (repo_id, verdict, degraded_reason)
		VALUES ('00000000-0000-0000-0000-000000000001', 'pass', '%s')
	`, reason))
	return nil
}

func (s *policyMigrationState) postgreSQLRejectsTheDegradedReasonInsert() error {
	if s.lastInsertErr == nil {
		return fmt.Errorf("expected degraded_reason INSERT to be rejected, but it succeeded")
	}
	if !isEnumOrCheckViolation(s.lastInsertErr) {
		return fmt.Errorf("expected enum/check constraint violation for degraded_reason, got: %v", s.lastInsertErr)
	}
	return nil
}

func (s *policyMigrationState) postgreSQLAcceptsTheDegradedReasonInsert() error {
	if s.lastInsertErr != nil {
		return fmt.Errorf("expected degraded_reason INSERT to succeed, but got: %v", s.lastInsertErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// step implementations — finding delta
// ---------------------------------------------------------------------------

var findingDeltaSeq int

func (s *policyMigrationState) theFindingTableExistsAfterMigrateUp() error {
	return s.ensureMigrateUp()
}

func (s *policyMigrationState) anINSERTSuppliesDelta(delta string) error {
	findingDeltaSeq++

	_, _ = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ('00000000-0000-0000-0000-000000000001', 'test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`)

	_, s.lastInsertErr = s.db.ExecContext(context.Background(), fmt.Sprintf(`
		INSERT INTO clean_code.finding (repo_id, delta)
		VALUES ('00000000-0000-0000-0000-000000000001', '%s')
	`, delta))
	return nil
}

func (s *policyMigrationState) postgreSQLRejectsTheDeltaInsert() error {
	if s.lastInsertErr == nil {
		return fmt.Errorf("expected delta INSERT to be rejected, but it succeeded")
	}
	if !isEnumOrCheckViolation(s.lastInsertErr) {
		return fmt.Errorf("expected enum/check constraint violation for delta, got: %v", s.lastInsertErr)
	}
	return nil
}

func (s *policyMigrationState) postgreSQLAcceptsTheDeltaInsert() error {
	if s.lastInsertErr != nil {
		return fmt.Errorf("expected delta INSERT to succeed, but got: %v", s.lastInsertErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// step implementations — column presence/absence
// ---------------------------------------------------------------------------

func (s *policyMigrationState) theOverrideTableExistsAfterMigrateUp() error {
	return s.ensureMigrateUp()
}

func (s *policyMigrationState) theRefactorTaskTableExistsAfterMigrateUp() error {
	return s.ensureMigrateUp()
}

func (s *policyMigrationState) listingColumnsOf(qualifiedTable string) error {
	parts := strings.SplitN(qualifiedTable, ".", 2)
	if len(parts) != 2 {
		return fmt.Errorf("expected schema.table, got %q", qualifiedTable)
	}
	schema, table := parts[0], parts[1]

	rows, err := s.db.QueryContext(context.Background(), `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = $1 AND table_name = $2
		ORDER BY ordinal_position
	`, schema, table)
	if err != nil {
		return fmt.Errorf("querying columns: %w", err)
	}
	defer rows.Close()

	s.columns = nil
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return fmt.Errorf("scanning column name: %w", err)
		}
		s.columns = append(s.columns, col)
	}
	if len(s.columns) == 0 {
		return fmt.Errorf("table %s has no columns (does it exist?)", qualifiedTable)
	}
	return rows.Err()
}

func (s *policyMigrationState) theColumnDoesNotExist(column string) error {
	for _, c := range s.columns {
		if c == column {
			return fmt.Errorf("column %q exists but should not", column)
		}
	}
	return nil
}

// theOnlyColumnsPresentAre asserts that the table has exactly the listed columns
// (comma-separated) in ordinal order — no more, no fewer.
func (s *policyMigrationState) theOnlyColumnsPresentAre(csv string) error {
	expected := strings.Split(csv, ",")
	if len(s.columns) != len(expected) {
		return fmt.Errorf("expected %d columns %v, got %d columns %v",
			len(expected), expected, len(s.columns), s.columns)
	}
	for i, col := range expected {
		if s.columns[i] != col {
			return fmt.Errorf("column %d: expected %q, got %q (full: %v)",
				i, col, s.columns[i], s.columns)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// step implementations — policy_activation latest-row-wins
// ---------------------------------------------------------------------------

func (s *policyMigrationState) thePolicyActivationTableExistsAfterMigrateUp() error {
	return s.ensureMigrateUp()
}

func (s *policyMigrationState) twoPolicyActivationRowsInsertedForSamePolicyChain() error {
	_, _ = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ('00000000-0000-0000-0000-000000000001', 'test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`)

	// Insert first activation (older).
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.policy_activation (policy_chain_id, repo_id, created_at)
		VALUES ('chain-A', '00000000-0000-0000-0000-000000000001', now() - interval '1 hour')
	`)
	if err != nil {
		return fmt.Errorf("inserting first policy_activation: %w", err)
	}

	// Insert second activation (newer) — this should be the winner.
	_, err = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.policy_activation (policy_chain_id, repo_id, created_at)
		VALUES ('chain-A', '00000000-0000-0000-0000-000000000001', now())
	`)
	if err != nil {
		return fmt.Errorf("inserting second policy_activation: %w", err)
	}

	// Record the latest created_at row's id.
	err = s.db.QueryRowContext(context.Background(), `
		SELECT policy_activation_id::text FROM clean_code.policy_activation
		WHERE policy_chain_id = 'chain-A'
		ORDER BY created_at DESC
		LIMIT 1
	`).Scan(&s.latestPolicyID)
	if err != nil {
		return fmt.Errorf("querying latest policy_activation: %w", err)
	}

	return nil
}

func (s *policyMigrationState) theRowWithMAXCreatedAtIsTheActivePolicy() error {
	// Simulate the evaluator's resolution query: MAX(created_at) wins.
	err := s.db.QueryRowContext(context.Background(), `
		SELECT policy_activation_id::text FROM clean_code.policy_activation
		WHERE policy_chain_id = 'chain-A'
		  AND created_at = (
			SELECT MAX(created_at) FROM clean_code.policy_activation
			WHERE policy_chain_id = 'chain-A'
		  )
	`).Scan(&s.activePolicyID)
	if err != nil {
		return fmt.Errorf("resolving active policy: %w", err)
	}
	if s.activePolicyID != s.latestPolicyID {
		return fmt.Errorf("expected active policy %s, got %s", s.latestPolicyID, s.activePolicyID)
	}
	return nil
}

// noPartialUniqueIndexExistsOnPolicyActivation queries pg_indexes to confirm
// that no partial unique index (WHERE clause) exists on policy_activation.
func (s *policyMigrationState) noPartialUniqueIndexExistsOnPolicyActivation() error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*)
		FROM pg_indexes
		WHERE schemaname = 'clean_code'
		  AND tablename  = 'policy_activation'
		  AND indexdef LIKE '%UNIQUE%'
		  AND indexdef LIKE '%WHERE%'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying pg_indexes: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("found %d partial unique index(es) on policy_activation; expected 0", count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_foundation_and_schema_policy_and_audit_and_refactor_schema_migrations
// registers all Given/When/Then steps for the policy-and-audit-and-refactor-schema-migrations
// stage.
func InitializeScenario_foundation_and_schema_policy_and_audit_and_refactor_schema_migrations(ctx *godog.ScenarioContext) {
	var state *policyMigrationState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		dsn := os.Getenv("CLEAN_CODE_PG_URL")
		if dsn == "" {
			return ctx, fmt.Errorf("CLEAN_CODE_PG_URL is not set")
		}
		var err error
		state, err = newPolicyMigrationState(dsn)
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

	// verdict-enum-only-canonical
	ctx.Step(`^the evaluation_verdict table exists after migrate-up$`, func() error {
		return state.theEvaluationVerdictTableExistsAfterMigrateUp()
	})
	ctx.Step(`^an INSERT supplies verdict '([^']*)'$`, func(v string) error {
		return state.anINSERTSuppliesVerdict(v)
	})
	ctx.Step(`^PostgreSQL rejects the verdict insert$`, func() error {
		return state.postgreSQLRejectsTheVerdictInsert()
	})
	ctx.Step(`^PostgreSQL accepts the verdict insert$`, func() error {
		return state.postgreSQLAcceptsTheVerdictInsert()
	})

	// override-no-expires-column
	ctx.Step(`^the override table exists after migrate-up$`, func() error {
		return state.theOverrideTableExistsAfterMigrateUp()
	})

	// degraded-reason-closed-set
	ctx.Step(`^an INSERT supplies degraded_reason '([^']*)'$`, func(reason string) error {
		return state.anINSERTSuppliesDegradedReason(reason)
	})
	ctx.Step(`^PostgreSQL rejects the degraded_reason insert$`, func() error {
		return state.postgreSQLRejectsTheDegradedReasonInsert()
	})
	ctx.Step(`^PostgreSQL accepts the degraded_reason insert$`, func() error {
		return state.postgreSQLAcceptsTheDegradedReasonInsert()
	})

	// finding-delta-canonical
	ctx.Step(`^the finding table exists after migrate-up$`, func() error {
		return state.theFindingTableExistsAfterMigrateUp()
	})
	ctx.Step(`^an INSERT supplies delta '([^']*)'$`, func(delta string) error {
		return state.anINSERTSuppliesDelta(delta)
	})
	ctx.Step(`^PostgreSQL rejects the delta insert$`, func() error {
		return state.postgreSQLRejectsTheDeltaInsert()
	})
	ctx.Step(`^PostgreSQL accepts the delta insert$`, func() error {
		return state.postgreSQLAcceptsTheDeltaInsert()
	})

	// refactor-task-no-status-column
	ctx.Step(`^the refactor_task table exists after migrate-up$`, func() error {
		return state.theRefactorTaskTableExistsAfterMigrateUp()
	})

	// shared column-checking steps
	ctx.Step(`^listing columns of (.+)$`, func(table string) error {
		return state.listingColumnsOf(table)
	})
	ctx.Step(`^the column "([^"]*)" does not exist$`, func(col string) error {
		return state.theColumnDoesNotExist(col)
	})
	ctx.Step(`^the only columns present are "([^"]*)"$`, func(csv string) error {
		return state.theOnlyColumnsPresentAre(csv)
	})

	// policy-activation-latest-row-wins
	ctx.Step(`^the policy_activation table exists after migrate-up$`, func() error {
		return state.thePolicyActivationTableExistsAfterMigrateUp()
	})
	ctx.Step(`^two policy_activation rows are inserted for the same policy chain$`, func() error {
		return state.twoPolicyActivationRowsInsertedForSamePolicyChain()
	})
	ctx.Step(`^the row with MAX\(created_at\) is the active policy$`, func() error {
		return state.theRowWithMAXCreatedAtIsTheActivePolicy()
	})
	ctx.Step(`^no partial unique index exists on clean_code\.policy_activation$`, func() error {
		return state.noPartialUniqueIndexExistsOnPolicyActivation()
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_foundation_and_schema_policy_and_audit_and_refactor_schema_migrations(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_foundation_and_schema_policy_and_audit_and_refactor_schema_migrations,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"foundation_and_schema_policy_and_audit_and_refactor_schema_migrations.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}