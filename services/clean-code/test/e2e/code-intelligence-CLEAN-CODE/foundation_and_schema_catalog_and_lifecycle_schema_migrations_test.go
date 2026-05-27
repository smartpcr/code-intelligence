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

// runMake executes a make target in the module root directory. It inherits
// the current environment (including CLEAN_CODE_PG_URL) so the Makefile
// can connect to PostgreSQL.
func runMake(target string) (string, error) {
	cmd := exec.Command("make", target)
	cmd.Dir = moduleRoot()
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// isEnumConstraintViolation checks whether a PostgreSQL error is specifically
// an invalid_text_representation error (SQLSTATE 22P02), which is the error
// PostgreSQL raises when an invalid value is supplied for an ENUM type.
func isEnumConstraintViolation(err error) bool {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		// 22P02 = invalid_text_representation — raised when a string literal
		// cannot be coerced to the target enum type.
		return pgErr.Code == "22P02"
	}
	return false
}

// ---------------------------------------------------------------------------
// shared state for a single scenario run
// ---------------------------------------------------------------------------

type catalogMigrationState struct {
	db              *sql.DB
	migrateUpErr    error
	migrateUpOut    string
	migrateDownErr  error
	migrateDownOut  string
	lastInsertErr   error
}

func newCatalogMigrationState(dsn string) (*catalogMigrationState, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &catalogMigrationState{db: db}, nil
}

func (s *catalogMigrationState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// cleanup drops the clean_code schema so each scenario starts clean.
func (s *catalogMigrationState) cleanup() {
	if s.db != nil {
		_, _ = s.db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS clean_code CASCADE")
	}
}

// ---------------------------------------------------------------------------
// step implementations
// ---------------------------------------------------------------------------

func (s *catalogMigrationState) anEmptyPostgreSQL16Instance() error {
	s.cleanup()
	return nil
}

func (s *catalogMigrationState) makeTargetRuns(target string) error {
	// Strip quotes and the "make " prefix to get just the target name.
	// The Gherkin step passes e.g. "make migrate-up".
	target = strings.TrimPrefix(target, "make ")

	out, err := runMake(target)
	switch target {
	case "migrate-up":
		s.migrateUpOut = out
		s.migrateUpErr = err
	case "migrate-down":
		s.migrateDownOut = out
		s.migrateDownErr = err
	default:
		return fmt.Errorf("unknown make target %q", target)
	}
	return nil
}

func (s *catalogMigrationState) bothMigrationsSucceed() error {
	if s.migrateUpErr != nil {
		return fmt.Errorf("make migrate-up failed: %w\noutput: %s", s.migrateUpErr, s.migrateUpOut)
	}
	if s.migrateDownErr != nil {
		return fmt.Errorf("make migrate-down failed: %w\noutput: %s", s.migrateDownErr, s.migrateDownOut)
	}
	return nil
}

func (s *catalogMigrationState) listingTablesInCleanCodeReturnsZeroRows() error {
	query := `
		SELECT COUNT(*)
		FROM information_schema.tables
		WHERE table_schema = 'clean_code'
	`
	var count int
	if err := s.db.QueryRowContext(context.Background(), query).Scan(&count); err != nil {
		return fmt.Errorf("querying information_schema.tables: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 tables in clean_code schema after migrate-down, got %d", count)
	}
	return nil
}

func (s *catalogMigrationState) theCommitTableExistsAfterMigrateUp() error {
	s.cleanup()
	out, err := runMake("migrate-up")
	if err != nil {
		return fmt.Errorf("make migrate-up failed during setup: %w\noutput: %s", err, out)
	}
	return nil
}

func (s *catalogMigrationState) anINSERTSuppliesScanStatus(status string) error {
	// Ensure parent repo row exists.
	_, _ = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ('00000000-0000-0000-0000-000000000001', 'test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`)

	_, s.lastInsertErr = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.commit (repo_id, sha, committed_at, scan_status)
		VALUES ('00000000-0000-0000-0000-000000000001', $1, now(), $2)
	`, fmt.Sprintf("deadbeef-%s", status), status)
	return nil
}

func (s *catalogMigrationState) postgreSQLRejectsItWithAnEnumConstraintViolation() error {
	if s.lastInsertErr == nil {
		return fmt.Errorf("expected INSERT to be rejected with enum constraint violation, but it succeeded")
	}
	if !isEnumConstraintViolation(s.lastInsertErr) {
		return fmt.Errorf("expected PostgreSQL enum constraint violation (SQLSTATE 22P02), got: %v", s.lastInsertErr)
	}
	return nil
}

func (s *catalogMigrationState) aCommitRowIsInsertedWithoutAScanStatusValue() error {
	// Ensure parent repo exists.
	_, _ = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ('00000000-0000-0000-0000-000000000001', 'test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`)

	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.commit (repo_id, sha, committed_at)
		VALUES ('00000000-0000-0000-0000-000000000001', 'abc123-default-test', now())
	`)
	if err != nil {
		return fmt.Errorf("inserting commit without scan_status: %w", err)
	}
	return nil
}

func (s *catalogMigrationState) theRowMaterialisesWithScanStatus(expected string) error {
	var actual string
	err := s.db.QueryRowContext(context.Background(), `
		SELECT scan_status::text FROM clean_code.commit
		WHERE sha = 'abc123-default-test'
	`).Scan(&actual)
	if err != nil {
		return fmt.Errorf("querying scan_status: %w", err)
	}
	if actual != expected {
		return fmt.Errorf("expected scan_status=%q, got %q", expected, actual)
	}
	return nil
}

func (s *catalogMigrationState) theScanRunTableExistsAfterMigrateUp() error {
	s.cleanup()
	out, err := runMake("migrate-up")
	if err != nil {
		return fmt.Errorf("make migrate-up failed during setup: %w\noutput: %s", err, out)
	}
	return nil
}

func (s *catalogMigrationState) anINSERTSuppliesScanRunStatus(status string) error {
	// Ensure parent repo exists.
	_, _ = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ('00000000-0000-0000-0000-000000000001', 'test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`)

	_, s.lastInsertErr = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.scan_run (repo_id, kind, to_sha, status)
		VALUES ('00000000-0000-0000-0000-000000000001', 'full', 'abc123', $1)
	`, status)
	return nil
}

func (s *catalogMigrationState) postgreSQLRejectsTheScanRunInsertWithEnumViolation() error {
	if s.lastInsertErr == nil {
		return fmt.Errorf("expected scan_run INSERT to be rejected with enum constraint violation, but it succeeded")
	}
	if !isEnumConstraintViolation(s.lastInsertErr) {
		return fmt.Errorf("expected PostgreSQL enum constraint violation (SQLSTATE 22P02), got: %v", s.lastInsertErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_foundation_and_schema_catalog_and_lifecycle_schema_migrations
// registers all Given/When/Then steps for the catalog-and-lifecycle-schema-migrations
// stage.
func InitializeScenario_foundation_and_schema_catalog_and_lifecycle_schema_migrations(ctx *godog.ScenarioContext) {
	var state *catalogMigrationState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		dsn := os.Getenv("CLEAN_CODE_PG_URL")
		if dsn == "" {
			return ctx, fmt.Errorf("CLEAN_CODE_PG_URL is not set")
		}
		var err error
		state, err = newCatalogMigrationState(dsn)
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

	// catalog-up-down — uses `make migrate-up` / `make migrate-down`
	ctx.Step(`^an empty PostgreSQL 16 instance$`, func() error { return state.anEmptyPostgreSQL16Instance() })
	ctx.Step(`^"([^"]*)" runs$`, func(target string) error { return state.makeTargetRuns(target) })
	ctx.Step(`^both migrations succeed$`, func() error { return state.bothMigrationsSucceed() })
	ctx.Step(`^listing tables in clean_code returns zero rows$`, func() error {
		return state.listingTablesInCleanCodeReturnsZeroRows()
	})

	// scan-status-enum-rejects-invalid
	ctx.Step(`^the commit table exists after migrate-up$`, func() error {
		return state.theCommitTableExistsAfterMigrateUp()
	})
	ctx.Step(`^an INSERT supplies scan_status '([^']*)'$`, func(status string) error {
		return state.anINSERTSuppliesScanStatus(status)
	})
	ctx.Step(`^PostgreSQL rejects it with an enum constraint violation$`, func() error {
		return state.postgreSQLRejectsItWithAnEnumConstraintViolation()
	})

	// scan-status-default-pending
	ctx.Step(`^a commit row is inserted without a scan_status value$`, func() error {
		return state.aCommitRowIsInsertedWithoutAScanStatusValue()
	})
	ctx.Step(`^the row materialises with scan_status '([^']*)'$`, func(expected string) error {
		return state.theRowMaterialisesWithScanStatus(expected)
	})

	// scan-run-status-enum
	ctx.Step(`^the scan_run table exists after migrate-up$`, func() error {
		return state.theScanRunTableExistsAfterMigrateUp()
	})
	ctx.Step(`^an INSERT supplies scan_run status '([^']*)'$`, func(status string) error {
		return state.anINSERTSuppliesScanRunStatus(status)
	})
	ctx.Step(`^PostgreSQL rejects the scan_run insert with an enum constraint violation$`, func() error {
		return state.postgreSQLRejectsTheScanRunInsertWithEnumViolation()
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_foundation_and_schema_catalog_and_lifecycle_schema_migrations(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_foundation_and_schema_catalog_and_lifecycle_schema_migrations,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"foundation_and_schema_catalog_and_lifecycle_schema_migrations.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}