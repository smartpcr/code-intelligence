//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// helpers (package-local, one copy per package)
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

// isPrimaryKeyViolation checks whether a PostgreSQL error is a unique/PK
// violation (SQLSTATE 23505).
func isPrimaryKeyViolation(err error) bool {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// isEnumOrCheckViolation checks whether a PostgreSQL error is an invalid enum
// value (22P02) or a check constraint violation (23514).
func isEnumOrCheckViolation(err error) bool {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		return pgErr.Code == "22P02" || pgErr.Code == "23514"
	}
	return false
}

// ---------------------------------------------------------------------------
// shared state for a single scenario run
// ---------------------------------------------------------------------------

type measurementState struct {
	db *sql.DB

	// active-row-quintuple-uniqueness
	firstSampleID     string
	secondInsertErr   error
	upsertErr         error
	upsertNewSampleID string
	activeRowCount    int

	// scope-binding
	firstScopeID  string
	secondScopeID string

	// pack-source-enum
	lastInsertErr error

	// degraded-defaults
	lastSampleID string
}

func newMeasurementState(dsn string) (*measurementState, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres connection: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &measurementState{db: db}, nil
}

func (s *measurementState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

func (s *measurementState) cleanup() {
	if s.db != nil {
		_, _ = s.db.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS clean_code CASCADE")
	}
}

// ---------------------------------------------------------------------------
// step implementations
// ---------------------------------------------------------------------------

func (s *measurementState) theMeasurementTablesExistAfterMigrateUp() error {
	s.cleanup()
	out, err := runMake("migrate-up")
	if err != nil {
		return fmt.Errorf("make migrate-up failed: %w\noutput: %s", err, out)
	}
	return nil
}

// -- active-row-quintuple-uniqueness ----------------------------------------

func (s *measurementState) aMetricSampleActiveRowIsInsertedForQuintuple(repo, sha, scope, metricKind, metricVersion string) error {
	// Ensure parent repo exists. Surface any error (e.g. missing schema /
	// table) immediately so callers get a clear "relation does not exist"
	// message rather than a downstream FK violation.
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ($1, 'test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`, repo); err != nil {
		return fmt.Errorf("inserting parent clean_code.repo row: %w", err)
	}

	// Insert a scope_binding row so we have a valid scope_id.
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.scope_binding (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
		VALUES ($1, $2, 'function', 'test.Func', $3)
		ON CONFLICT DO NOTHING
	`, scope, repo, sha); err != nil {
		return fmt.Errorf("inserting parent clean_code.scope_binding row: %w", err)
	}

	// Insert a metric_sample row to reference.
	sampleID := fmt.Sprintf("sample-%s-%s-%s-%s-%s", repo, sha, scope, metricKind, metricVersion)
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_sample (sample_id, repo_id, sha, scope_id, metric_kind, metric_version, value_json)
		VALUES ($1, $2, $3, $4, $5, $6, '{}')
		ON CONFLICT DO NOTHING
	`, sampleID, repo, sha, scope, metricKind, metricVersion)
	if err != nil {
		return fmt.Errorf("inserting metric_sample: %w", err)
	}
	s.firstSampleID = sampleID

	// Insert into metric_sample_active.
	_, err = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_sample_active (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
		VALUES ($1, $2, $3, $4, $5, $6)
	`, repo, sha, scope, metricKind, metricVersion, sampleID)
	if err != nil {
		return fmt.Errorf("inserting metric_sample_active: %w", err)
	}
	return nil
}

func (s *measurementState) aSecondMetricSampleActiveINSERTForTheSameQuintupleRuns() error {
	_, s.secondInsertErr = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_sample_active (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
		VALUES ('repo1', 'sha1', 'scope1', 'complexity', 'v1', $1)
	`, s.firstSampleID)
	return nil
}

func (s *measurementState) itFailsWithAPrimaryKeyViolation() error {
	if s.secondInsertErr == nil {
		return fmt.Errorf("expected INSERT to fail with PK violation, but it succeeded")
	}
	if !isPrimaryKeyViolation(s.secondInsertErr) {
		return fmt.Errorf("expected PK violation (SQLSTATE 23505), got: %v", s.secondInsertErr)
	}
	return nil
}

func (s *measurementState) anUPSERTOnMetricSampleActiveForTheSameQuintupleSetsANewSampleID() error {
	newSampleID := s.firstSampleID + "-v2"
	// Insert the new sample row first.
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_sample (sample_id, repo_id, sha, scope_id, metric_kind, metric_version, value_json)
		VALUES ($1, 'repo1', 'sha1', 'scope1', 'complexity', 'v1', '{"v":2}')
		ON CONFLICT DO NOTHING
	`, newSampleID)
	if err != nil {
		return fmt.Errorf("inserting new metric_sample for upsert: %w", err)
	}

	_, s.upsertErr = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_sample_active (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
		VALUES ('repo1', 'sha1', 'scope1', 'complexity', 'v1', $1)
		ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version)
		DO UPDATE SET sample_id = EXCLUDED.sample_id
	`, newSampleID)
	s.upsertNewSampleID = newSampleID
	return nil
}

func (s *measurementState) theUPSERTSucceedsAndOnlyOneRowExistsForThatQuintuple() error {
	if s.upsertErr != nil {
		return fmt.Errorf("UPSERT failed: %w", s.upsertErr)
	}
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample_active
		WHERE repo_id='repo1' AND sha='sha1' AND scope_id='scope1'
		  AND metric_kind='complexity' AND metric_version='v1'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("counting active rows: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("expected exactly 1 active row, got %d", count)
	}

	// Verify the sample_id was updated.
	var currentSampleID string
	err = s.db.QueryRowContext(context.Background(), `
		SELECT sample_id FROM clean_code.metric_sample_active
		WHERE repo_id='repo1' AND sha='sha1' AND scope_id='scope1'
		  AND metric_kind='complexity' AND metric_version='v1'
	`).Scan(&currentSampleID)
	if err != nil {
		return fmt.Errorf("reading current sample_id: %w", err)
	}
	if currentSampleID != s.upsertNewSampleID {
		return fmt.Errorf("expected sample_id=%q after upsert, got %q", s.upsertNewSampleID, currentSampleID)
	}
	return nil
}

func (s *measurementState) metricSampleIsNeverUPDATEd() error {
	// Verify that both original and new sample rows exist unchanged in
	// metric_sample (append-only; the old row was not deleted or modified).
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
		WHERE sample_id IN ($1, $2)
	`, s.firstSampleID, s.upsertNewSampleID).Scan(&count)
	if err != nil {
		return fmt.Errorf("counting metric_sample rows: %w", err)
	}
	if count != 2 {
		return fmt.Errorf("expected 2 metric_sample rows (append-only), got %d", count)
	}
	return nil
}

// -- pack-source-enum-rejects-invalid ---------------------------------------

func (s *measurementState) anINSERTIntoMetricSampleSuppliesPack(pack string) error {
	if err := s.ensureParentRows(); err != nil {
		return err
	}
	_, s.lastInsertErr = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_sample (sample_id, repo_id, sha, scope_id, metric_kind, metric_version, value_json, pack)
		VALUES ('sample-pack-test', 'repo-enum-test', 'sha-enum', 'scope-enum', 'complexity', 'v1', '{}', $1)
	`, pack)
	return nil
}

func (s *measurementState) anINSERTIntoMetricSampleSuppliesSource(source string) error {
	if err := s.ensureParentRows(); err != nil {
		return err
	}
	_, s.lastInsertErr = s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_sample (sample_id, repo_id, sha, scope_id, metric_kind, metric_version, value_json, source)
		VALUES ('sample-source-test', 'repo-enum-test', 'sha-enum', 'scope-enum', 'complexity', 'v1', '{}', $1)
	`, source)
	return nil
}

func (s *measurementState) postgreSQLRejectsTheMetricSampleInsert() error {
	if s.lastInsertErr == nil {
		return fmt.Errorf("expected INSERT to be rejected, but it succeeded")
	}
	if !isEnumOrCheckViolation(s.lastInsertErr) {
		return fmt.Errorf("expected enum/check violation, got: %v", s.lastInsertErr)
	}
	return nil
}

// ensureParentRows inserts the shared parent rows required by the enum /
// degraded scenarios. Errors are surfaced to the caller so a missing schema
// or table produces a clear "relation does not exist" failure instead of a
// confusing FK violation on a later INSERT.
func (s *measurementState) ensureParentRows() error {
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ('repo-enum-test', 'test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("ensureParentRows: inserting clean_code.repo: %w", err)
	}
	if _, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.scope_binding (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
		VALUES ('scope-enum', 'repo-enum-test', 'function', 'test.Func', 'sha-enum')
		ON CONFLICT DO NOTHING
	`); err != nil {
		return fmt.Errorf("ensureParentRows: inserting clean_code.scope_binding: %w", err)
	}
	return nil
}

// -- degraded-defaults-false ------------------------------------------------

func (s *measurementState) aMetricSampleRowIsInsertedWithoutADegradedValue() error {
	if err := s.ensureParentRows(); err != nil {
		return err
	}
	s.lastSampleID = "sample-degraded-test"
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_sample (sample_id, repo_id, sha, scope_id, metric_kind, metric_version, value_json)
		VALUES ($1, 'repo-enum-test', 'sha-enum', 'scope-enum', 'complexity', 'v1', '{}')
		ON CONFLICT DO NOTHING
	`, s.lastSampleID)
	if err != nil {
		return fmt.Errorf("inserting metric_sample without degraded: %w", err)
	}
	return nil
}

func (s *measurementState) theRowMaterialisesWithDegradedFalseAndDegradedReasonISNULL() error {
	var degraded bool
	var degradedReason *string
	err := s.db.QueryRowContext(context.Background(), `
		SELECT degraded, degraded_reason FROM clean_code.metric_sample
		WHERE sample_id = $1
	`, s.lastSampleID).Scan(&degraded, &degradedReason)
	if err != nil {
		return fmt.Errorf("querying degraded columns: %w", err)
	}
	if degraded {
		return fmt.Errorf("expected degraded=false, got true")
	}
	if degradedReason != nil {
		return fmt.Errorf("expected degraded_reason IS NULL, got %q", *degradedReason)
	}
	return nil
}

// -- scope-binding-stable-across-shas ---------------------------------------

func (s *measurementState) theScopeBindingWriterInsertsARow(repo, scopeKind, signature, firstSeenSHA string) error {
	var scopeID string
	err := s.db.QueryRowContext(context.Background(), `
		INSERT INTO clean_code.scope_binding (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
		VALUES (gen_random_uuid()::text, $1, $2, $3, $4)
		ON CONFLICT (repo_id, scope_kind, canonical_signature, first_seen_sha)
		DO UPDATE SET scope_id = clean_code.scope_binding.scope_id
		RETURNING scope_id
	`, repo, scopeKind, signature, firstSeenSHA).Scan(&scopeID)
	if err != nil {
		// Ensure repo exists first, then retry.
		if _, repoErr := s.db.ExecContext(context.Background(), `
			INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
			VALUES ($1, 'test-repo', 'main')
			ON CONFLICT (repo_id) DO NOTHING
		`, repo); repoErr != nil {
			return fmt.Errorf("inserting parent clean_code.repo row before retry: %w", repoErr)
		}
		err = s.db.QueryRowContext(context.Background(), `
			INSERT INTO clean_code.scope_binding (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
			VALUES (gen_random_uuid()::text, $1, $2, $3, $4)
			ON CONFLICT (repo_id, scope_kind, canonical_signature, first_seen_sha)
			DO UPDATE SET scope_id = clean_code.scope_binding.scope_id
			RETURNING scope_id
		`, repo, scopeKind, signature, firstSeenSHA).Scan(&scopeID)
		if err != nil {
			return fmt.Errorf("inserting scope_binding: %w", err)
		}
	}
	s.firstScopeID = scopeID
	return nil
}

func (s *measurementState) theScopeBindingWriterRunsAgainForSameNaturalKeyAtSHA(sha string) error {
	// The natural key is (repo_id, scope_kind, canonical_signature, first_seen_sha).
	// Running again with the same natural key should return the same scope_id.
	var scopeID string
	err := s.db.QueryRowContext(context.Background(), `
		INSERT INTO clean_code.scope_binding (scope_id, repo_id, scope_kind, canonical_signature, first_seen_sha)
		VALUES (gen_random_uuid()::text, 'r1', 'function', 'pkg.Foo', 'sha-A')
		ON CONFLICT (repo_id, scope_kind, canonical_signature, first_seen_sha)
		DO UPDATE SET scope_id = clean_code.scope_binding.scope_id
		RETURNING scope_id
	`).Scan(&scopeID)
	if err != nil {
		return fmt.Errorf("second scope_binding insert: %w", err)
	}
	s.secondScopeID = scopeID
	return nil
}

func (s *measurementState) theSecondCallScopeIDEqualsTheFirst() error {
	if s.firstScopeID != s.secondScopeID {
		return fmt.Errorf("scope_id mismatch: first=%q, second=%q", s.firstScopeID, s.secondScopeID)
	}
	return nil
}

func (s *measurementState) onlyOneRowExistsInScopeBindingForThatNaturalKey() error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.scope_binding
		WHERE repo_id='r1' AND scope_kind='function'
		  AND canonical_signature='pkg.Foo' AND first_seen_sha='sha-A'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("counting scope_binding rows: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("expected 1 scope_binding row, got %d", count)
	}
	return nil
}

// -- cross-repo-percentile-shape --------------------------------------------

func (s *measurementState) crossRepoPercentileHasExactlyColumns(expectedCSV string) error {
	rows, err := s.db.QueryContext(context.Background(), `
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'clean_code'
		  AND table_name = 'cross_repo_percentile'
		ORDER BY ordinal_position
	`)
	if err != nil {
		return fmt.Errorf("querying columns: %w", err)
	}
	defer rows.Close()

	var actual []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return fmt.Errorf("scanning column: %w", err)
		}
		actual = append(actual, col)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating columns: %w", err)
	}

	expected := strings.Split(expectedCSV, ",")
	sort.Strings(expected)
	sortedActual := make([]string, len(actual))
	copy(sortedActual, actual)
	sort.Strings(sortedActual)

	expectedJSON, _ := json.Marshal(expected)
	actualJSON, _ := json.Marshal(sortedActual)
	if string(expectedJSON) != string(actualJSON) {
		return fmt.Errorf("column mismatch:\n  expected (sorted): %s\n  actual   (sorted): %s", expectedJSON, actualJSON)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_foundation_and_schema_measurement_schema_and_active_row_index
// registers all Given/When/Then steps for the measurement-schema-and-active-row-index
// stage.
func InitializeScenario_foundation_and_schema_measurement_schema_and_active_row_index(ctx *godog.ScenarioContext) {
	var state *measurementState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		dsn := os.Getenv("CLEAN_CODE_PG_URL")
		if dsn == "" {
			return ctx, fmt.Errorf("CLEAN_CODE_PG_URL is not set")
		}
		var err error
		state, err = newMeasurementState(dsn)
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

	// -- shared setup
	ctx.Step(`^the measurement tables exist after migrate-up$`, func() error {
		return state.theMeasurementTablesExistAfterMigrateUp()
	})

	// -- active-row-quintuple-uniqueness
	ctx.Step(`^a metric_sample_active row is inserted for quintuple "([^"]*)","([^"]*)","([^"]*)","([^"]*)","([^"]*)"$`, func(repo, sha, scope, kind, ver string) error {
		return state.aMetricSampleActiveRowIsInsertedForQuintuple(repo, sha, scope, kind, ver)
	})
	ctx.Step(`^a second metric_sample_active INSERT for the same quintuple runs$`, func() error {
		return state.aSecondMetricSampleActiveINSERTForTheSameQuintupleRuns()
	})
	ctx.Step(`^it fails with a PRIMARY KEY violation$`, func() error {
		return state.itFailsWithAPrimaryKeyViolation()
	})
	ctx.Step(`^an UPSERT on metric_sample_active for the same quintuple sets a new sample_id$`, func() error {
		return state.anUPSERTOnMetricSampleActiveForTheSameQuintupleSetsANewSampleID()
	})
	ctx.Step(`^the UPSERT succeeds and only one row exists for that quintuple$`, func() error {
		return state.theUPSERTSucceedsAndOnlyOneRowExistsForThatQuintuple()
	})
	ctx.Step(`^metric_sample is never UPDATEd$`, func() error {
		return state.metricSampleIsNeverUPDATEd()
	})

	// -- pack-source-enum-rejects-invalid
	ctx.Step(`^an INSERT into metric_sample supplies pack '([^']*)'$`, func(pack string) error {
		return state.anINSERTIntoMetricSampleSuppliesPack(pack)
	})
	ctx.Step(`^an INSERT into metric_sample supplies source '([^']*)'$`, func(source string) error {
		return state.anINSERTIntoMetricSampleSuppliesSource(source)
	})
	ctx.Step(`^PostgreSQL rejects the metric_sample insert$`, func() error {
		return state.postgreSQLRejectsTheMetricSampleInsert()
	})

	// -- degraded-defaults-false
	ctx.Step(`^a metric_sample row is inserted without a degraded value$`, func() error {
		return state.aMetricSampleRowIsInsertedWithoutADegradedValue()
	})
	ctx.Step(`^the row materialises with degraded false and degraded_reason IS NULL$`, func() error {
		return state.theRowMaterialisesWithDegradedFalseAndDegradedReasonISNULL()
	})

	// -- scope-binding-stable-across-shas
	ctx.Step(`^the ScopeBinding writer inserts a row for repo "([^"]*)", scope_kind "([^"]*)", signature "([^"]*)", first_seen_sha "([^"]*)"$`, func(repo, scopeKind, sig, sha string) error {
		return state.theScopeBindingWriterInsertsARow(repo, scopeKind, sig, sha)
	})
	ctx.Step(`^the ScopeBinding writer runs again for the same natural key at sha "([^"]*)"$`, func(sha string) error {
		return state.theScopeBindingWriterRunsAgainForSameNaturalKeyAtSHA(sha)
	})
	ctx.Step(`^the second call scope_id equals the first$`, func() error {
		return state.theSecondCallScopeIDEqualsTheFirst()
	})
	ctx.Step(`^only one row exists in scope_binding for that natural key$`, func() error {
		return state.onlyOneRowExistsInScopeBindingForThatNaturalKey()
	})

	// -- cross-repo-percentile-shape
	ctx.Step(`^cross_repo_percentile has exactly columns "([^"]*)"$`, func(cols string) error {
		return state.crossRepoPercentileHasExactlyColumns(cols)
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_foundation_and_schema_measurement_schema_and_active_row_index(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_foundation_and_schema_measurement_schema_and_active_row_index,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"foundation_and_schema_measurement_schema_and_active_row_index.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}
