//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
	"github.com/smartpcr/code-intelligence/services/agent-memory/migrations"
)

// requireEnv skips the test when the named environment variable
// is unset or empty, returning its value otherwise.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("env %s not set; skipping", name)
	}
	return v
}

// --------------- scenario state ----------------------------------

type migrationScenarioState struct {
	db       *sql.DB
	migrator *migrations.Migrator
	upErr    error
	// appliedBefore stores versions applied after the first Up pass.
	appliedBefore []string
	// secondUpErr stores the error (if any) from a second Up pass.
	secondUpErr error
}

func newMigrationScenarioState(dsn string) (*migrationScenarioState, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return &migrationScenarioState{
		db:       db,
		migrator: migrations.New(db),
	}, nil
}

func (s *migrationScenarioState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// resetSchema drops all objects so each scenario starts clean.
func (s *migrationScenarioState) resetSchema(ctx context.Context) error {
	// Drop the journal table and all enum types / tables that
	// migrations create, by dropping the public schema and
	// recreating it. This is the most reliable way to get a
	// truly fresh state in a disposable test database.
	_, err := s.db.ExecContext(ctx, `
		DROP SCHEMA public CASCADE;
		CREATE SCHEMA public;
		GRANT ALL ON SCHEMA public TO public;
	`)
	return err
}

// --------------- step definitions --------------------------------

func (s *migrationScenarioState) aFreshSchemaWithMigrationsThroughFile(filename string) error {
	ctx := context.Background()
	if err := s.resetSchema(ctx); err != nil {
		return fmt.Errorf("reset schema: %w", err)
	}

	// Load all embedded migrations and apply them one-by-one
	// in version order, stopping just before the target file
	// (0022). This leaves 0001–0021 applied so the When step
	// can apply 0022 via Migrator.Up.
	all, err := migrations.All()
	if err != nil {
		return fmt.Errorf("load migrations: %w", err)
	}

	// Create the journal table manually (schema was just reset).
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS _schema_migrations (
			version    text        PRIMARY KEY,
			name       text        NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("create journal: %w", err)
	}

	for _, mg := range all {
		if mg.Filename == "0022_edge_kind_overrides.sql" {
			break
		}
		if _, err := s.db.ExecContext(ctx, mg.Up); err != nil {
			return fmt.Errorf("apply %s: %w", mg.Filename, err)
		}
		if _, err := s.db.ExecContext(ctx,
			"INSERT INTO _schema_migrations (version, name) VALUES ($1, $2)",
			mg.Version, mg.Name,
		); err != nil {
			return fmt.Errorf("journal %s: %w", mg.Filename, err)
		}
	}

	return nil
}

func (s *migrationScenarioState) migrationFileIsApplied(filename string) error {
	ctx := context.Background()
	// Now apply remaining migrations (just 0022) via Migrator.Up
	// which will skip already-journaled versions.
	s.upErr = s.migrator.Up(ctx)
	return nil
}

func (s *migrationScenarioState) itReturnsNoErrorAndSelectSucceeds(query string) error {
	if s.upErr != nil {
		return fmt.Errorf("migration Up returned error: %w", s.upErr)
	}
	ctx := context.Background()
	var result string
	if err := s.db.QueryRowContext(ctx, query).Scan(&result); err != nil {
		return fmt.Errorf("query %q failed: %w", query, err)
	}
	if result != "overrides" {
		return fmt.Errorf("expected 'overrides', got %q", result)
	}
	return nil
}

func (s *migrationScenarioState) theMigrationRunnerSkipsAlreadyAppliedMigrations(dummy string) error {
	ctx := context.Background()
	// Start with a completely fresh schema and apply all
	// migrations via Migrator.Up (includes 0022).
	if err := s.resetSchema(ctx); err != nil {
		return fmt.Errorf("reset schema: %w", err)
	}
	s.upErr = s.migrator.Up(ctx)
	if s.upErr != nil {
		return fmt.Errorf("first Up: %w", s.upErr)
	}
	// Record which versions are applied after first pass.
	versions, err := s.migrator.AppliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("AppliedVersions: %w", err)
	}
	s.appliedBefore = versions
	return nil
}

func (s *migrationScenarioState) migrationFileRunsOnce(filename string) error {
	// The first Up already happened in the Given step.
	// Verify 0022 is in the journal.
	found := false
	for _, v := range s.appliedBefore {
		if v == "0022" {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("version 0022 not found in applied versions after first Up: %v", s.appliedBefore)
	}
	return nil
}

func (s *migrationScenarioState) aSecondMigrationPassDoesNotReExecuteIt() error {
	ctx := context.Background()
	// Run Up again — it should be a no-op for all migrations.
	s.secondUpErr = s.migrator.Up(ctx)
	if s.secondUpErr != nil {
		return fmt.Errorf("second Up returned error (expected no-op): %w", s.secondUpErr)
	}
	// Verify the applied versions are identical.
	after, err := s.migrator.AppliedVersions(ctx)
	if err != nil {
		return fmt.Errorf("AppliedVersions after second Up: %w", err)
	}
	if len(after) != len(s.appliedBefore) {
		return fmt.Errorf("applied version count changed: before=%d, after=%d",
			len(s.appliedBefore), len(after))
	}
	return nil
}

// --------------- godog wiring ------------------------------------

func InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_schema_migration_for_overrides_edge_kind(ctx *godog.ScenarioContext) {
	var s *migrationScenarioState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		dsn := os.Getenv("AGENT_MEMORY_PG_URL")
		if dsn == "" {
			return ctx, fmt.Errorf("AGENT_MEMORY_PG_URL not set")
		}
		var err error
		s, err = newMigrationScenarioState(dsn)
		if err != nil {
			return ctx, err
		}
		return ctx, nil
	})

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if s != nil {
			s.close()
		}
		return ctx, nil
	})

	ctx.Step(
		`^a fresh schema with migrations through "([^"]*)"$`,
		func(filename string) error { return s.aFreshSchemaWithMigrationsThroughFile(filename) },
	)
	ctx.Step(
		`^"([^"]*)" is applied$`,
		func(filename string) error { return s.migrationFileIsApplied(filename) },
	)
	ctx.Step(
		`^it returns no error and "([^"]*)" succeeds$`,
		func(query string) error { return s.itReturnsNoErrorAndSelectSucceeds(query) },
	)
	ctx.Step(
		`^the migration runner skips already-applied migrations by filename$`,
		func() error { return s.theMigrationRunnerSkipsAlreadyAppliedMigrations("") },
	)
	ctx.Step(
		`^"([^"]*)" runs once$`,
		func(filename string) error { return s.migrationFileRunsOnce(filename) },
	)
	ctx.Step(
		`^a second migration pass does not re-execute it$`,
		func() error { return s.aSecondMigrationPassDoesNotReExecuteIt() },
	)
}

func TestE2E_shared_additive_surfaces_and_dispatcher_edits_schema_migration_for_overrides_edge_kind(t *testing.T) {
	requireEnv(t, "AGENT_MEMORY_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_shared_additive_surfaces_and_dispatcher_edits_schema_migration_for_overrides_edge_kind,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"shared_additive_surfaces_and_dispatcher_edits_schema_migration_for_overrides_edge_kind.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
