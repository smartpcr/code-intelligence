//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

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

// openDB opens a PostgreSQL connection using the given DSN and verifies
// connectivity.
func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return db, nil
}

// isPgPermissionDenied checks for a PostgreSQL "insufficient_privilege" error
// (SQLSTATE 42501).
func isPgPermissionDenied(err error) bool {
	var pgErr *pq.Error
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42501"
	}
	return false
}

// ---------------------------------------------------------------------------
// scenario state — tick-writes-snapshots
// ---------------------------------------------------------------------------

type aggregatorTickState struct {
	db            *sql.DB
	aggregatorURL string
	repoIDs       []string
}

func newAggregatorTickState(dsn, aggregatorURL string) (*aggregatorTickState, error) {
	db, err := openDB(dsn)
	if err != nil {
		return nil, err
	}
	return &aggregatorTickState{db: db, aggregatorURL: aggregatorURL}, nil
}

func (s *aggregatorTickState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// cleanup removes test data written during the scenario.
func (s *aggregatorTickState) cleanup() {
	if s.db == nil {
		return
	}
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.cross_repo_percentile WHERE metric_kind = 'lcom4' AND scope_kind = 'class'`)
	_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.repo_metric_snapshot WHERE metric_kind = 'lcom4' AND scope_kind = 'class'`)
	for _, rid := range s.repoIDs {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.metric_sample_active WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.metric_sample WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.scope_binding WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.scan_run WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.commit WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.repo WHERE repo_id = $1`, rid)
	}
}

// ensureMetricKind upserts the lcom4 metric_kind catalog entry.
func (s *aggregatorTickState) ensureMetricKind() error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_kind (metric_kind, metric_version, display_name, unit, direction)
		VALUES ('lcom4', 1, 'LCOM4', 'ratio', 'lower_is_better')
		ON CONFLICT (metric_kind, metric_version) DO NOTHING
	`)
	return err
}

// aRunningAggregatorConnectedToPostgreSQL verifies that the aggregator
// service is reachable via its healthz endpoint.
func (s *aggregatorTickState) aRunningAggregatorConnectedToPostgreSQL() error {
	healthURL := strings.TrimRight(s.aggregatorURL, "/") + "/healthz"
	client := &http.Client{Timeout: 10 * time.Second}

	// Retry for up to 30s in case the aggregator is still starting.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("creating healthz request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("aggregator at %s did not become healthy within 30s", s.aggregatorURL)
		}
		time.Sleep(time.Second)
	}
}

// fiveReposWithActiveSamples creates five repos, each with one active
// metric_sample row for the given metric_kind.
func (s *aggregatorTickState) fiveReposWithActiveSamples(metricKind string) error {
	if err := s.ensureMetricKind(); err != nil {
		return fmt.Errorf("ensuring metric_kind: %w", err)
	}

	ctx := context.Background()
	s.repoIDs = make([]string, 5)

	for i := 0; i < 5; i++ {
		repoID := fmt.Sprintf("a0000000-0000-0000-0000-%012d", i+1)
		s.repoIDs[i] = repoID
		sha := fmt.Sprintf("sha-%d", i+1)
		value := float64(i+1) * 1.5

		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
			VALUES ($1, $2, 'main')
			ON CONFLICT (repo_id) DO NOTHING
		`, repoID, fmt.Sprintf("test-repo-%d", i+1)); err != nil {
			return fmt.Errorf("inserting repo %d: %w", i+1, err)
		}

		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO clean_code.commit (repo_id, sha, committed_at, scan_status)
			VALUES ($1, $2, now(), 'pending')
			ON CONFLICT DO NOTHING
		`, repoID, sha); err != nil {
			return fmt.Errorf("inserting commit %d: %w", i+1, err)
		}

		var scanRunID string
		if err := s.db.QueryRowContext(ctx, `
			INSERT INTO clean_code.scan_run (repo_id, kind, to_sha, status)
			VALUES ($1, 'full', $2, 'running')
			RETURNING scan_run_id
		`, repoID, sha).Scan(&scanRunID); err != nil {
			return fmt.Errorf("inserting scan_run %d: %w", i+1, err)
		}

		var scopeID string
		if err := s.db.QueryRowContext(ctx, `
			INSERT INTO clean_code.scope_binding (repo_id, sha, scope_kind, scope_path, language)
			VALUES ($1, $2, 'class', 'com.example.Foo', 'java')
			RETURNING scope_id
		`, repoID, sha).Scan(&scopeID); err != nil {
			return fmt.Errorf("inserting scope_binding %d: %w", i+1, err)
		}

		var sampleID string
		if err := s.db.QueryRowContext(ctx, `
			INSERT INTO clean_code.metric_sample
				(repo_id, sha, scope_id, metric_kind, metric_version, value, pack, source, producer_run_id)
			VALUES ($1, $2, $3, $4, 1, $5, 'base', 'computed', $6)
			RETURNING sample_id
		`, repoID, sha, scopeID, metricKind, value, scanRunID).Scan(&sampleID); err != nil {
			return fmt.Errorf("inserting metric_sample %d: %w", i+1, err)
		}

		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO clean_code.metric_sample_active
				(repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
			VALUES ($1, $2, $3, $4, 1, $5)
			ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version)
			DO UPDATE SET sample_id = EXCLUDED.sample_id
		`, repoID, sha, scopeID, metricKind, sampleID); err != nil {
			return fmt.Errorf("upserting metric_sample_active %d: %w", i+1, err)
		}
	}

	return nil
}

// theAggregatorTickEndpointIsInvoked fires an HTTP POST to the real
// aggregator service's tick endpoint, triggering the cadence loop to
// compute repo_metric_snapshot and cross_repo_percentile rows.
func (s *aggregatorTickState) theAggregatorTickEndpointIsInvoked() error {
	tickURL := strings.TrimRight(s.aggregatorURL, "/") + "/v1/aggregator/tick"
	client := &http.Client{Timeout: 60 * time.Second}

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, tickURL, nil)
	if err != nil {
		return fmt.Errorf("creating tick request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("invoking aggregator tick endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("aggregator tick returned HTTP %d", resp.StatusCode)
	}

	// Allow a brief settling window for async writes to land.
	time.Sleep(500 * time.Millisecond)
	return nil
}

func (s *aggregatorTickState) repoMetricSnapshotHasNRows(metricKind string) error {
	var count int
	// Poll for up to 15s in case the aggregator writes asynchronously.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM clean_code.repo_metric_snapshot
			WHERE metric_kind = $1 AND scope_kind = 'class'
		`, metricKind).Scan(&count)
		if err != nil {
			return fmt.Errorf("querying repo_metric_snapshot count: %w", err)
		}
		if count >= 5 {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("expected 5 repo_metric_snapshot rows, got %d after timeout", count)
		}
		time.Sleep(500 * time.Millisecond)
	}

	if count != 5 {
		return fmt.Errorf("expected 5 repo_metric_snapshot rows, got %d", count)
	}
	return nil
}

func (s *aggregatorTickState) crossRepoPercentileHasOneRow() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var count int
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM clean_code.cross_repo_percentile
			WHERE metric_kind = 'lcom4' AND scope_kind = 'class'
		`).Scan(&count)
		if err != nil {
			return fmt.Errorf("counting cross_repo_percentile: %w", err)
		}
		if count >= 1 {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("expected at least 1 cross_repo_percentile row, got %d after timeout", count)
		}
		time.Sleep(500 * time.Millisecond)
	}

	var (
		p50, p90     sql.NullFloat64
		p99          sql.NullFloat64
		histogramRaw sql.NullString
		builtAt      sql.NullString
	)
	err := s.db.QueryRowContext(context.Background(), `
		SELECT p50, p90, p99, histogram_json::text, built_at::text
		FROM clean_code.cross_repo_percentile
		WHERE metric_kind = 'lcom4' AND scope_kind = 'class'
		ORDER BY built_at DESC
		LIMIT 1
	`).Scan(&p50, &p90, &p99, &histogramRaw, &builtAt)
	if err != nil {
		return fmt.Errorf("reading cross_repo_percentile: %w", err)
	}

	if !p50.Valid {
		return fmt.Errorf("p50 is NULL")
	}
	if !p90.Valid {
		return fmt.Errorf("p90 is NULL")
	}
	if !p99.Valid {
		return fmt.Errorf("p99 is NULL")
	}
	if !histogramRaw.Valid || histogramRaw.String == "" {
		return fmt.Errorf("histogram_json is NULL or empty")
	}
	if !builtAt.Valid || builtAt.String == "" {
		return fmt.Errorf("built_at is NULL or empty")
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario state — aggregator-is-sole-writer
// ---------------------------------------------------------------------------

type soleWriterState struct {
	aggregatorDB  *sql.DB
	nonAggDB      *sql.DB
	lastInsertErr error
}

func newSoleWriterState(aggregatorDSN, nonAggDSN string) (*soleWriterState, error) {
	aggDB, err := openDB(aggregatorDSN)
	if err != nil {
		return nil, fmt.Errorf("opening aggregator DSN: %w", err)
	}
	nonAggDB, err := openDB(nonAggDSN)
	if err != nil {
		aggDB.Close()
		return nil, fmt.Errorf("opening non-aggregator DSN: %w", err)
	}
	return &soleWriterState{aggregatorDB: aggDB, nonAggDB: nonAggDB}, nil
}

func (s *soleWriterState) close() {
	if s.aggregatorDB != nil {
		s.aggregatorDB.Close()
	}
	if s.nonAggDB != nil {
		s.nonAggDB.Close()
	}
}

func (s *soleWriterState) cleanup() {
	if s.aggregatorDB != nil {
		ctx := context.Background()
		_, _ = s.aggregatorDB.ExecContext(ctx,
			`DELETE FROM clean_code.cross_repo_percentile WHERE metric_kind = 'e2e_sole_writer_test'`)
	}
}

func (s *soleWriterState) aNonAggregatorDatabaseRole() error {
	// The non-aggregator connection is already configured via
	// CLEAN_CODE_PG_NONAGG_URL; verify it is alive.
	return s.nonAggDB.PingContext(context.Background())
}

func (s *soleWriterState) itAttemptsINSERTIntoCrossRepoPercentile() error {
	_, s.lastInsertErr = s.nonAggDB.ExecContext(context.Background(), `
		INSERT INTO clean_code.cross_repo_percentile
			(metric_kind, scope_kind, histogram_json, p50, p90, p99)
		VALUES ('e2e_sole_writer_test', 'class', '{"test":true}'::jsonb, 1.0, 2.0, 3.0)
	`)
	return nil
}

func (s *soleWriterState) postgresqlReturnsPermissionDenied() error {
	if s.lastInsertErr == nil {
		return fmt.Errorf("expected permission denied error, but INSERT succeeded")
	}
	if !isPgPermissionDenied(s.lastInsertErr) {
		return fmt.Errorf("expected permission denied (SQLSTATE 42501), got: %v", s.lastInsertErr)
	}
	return nil
}

func (s *soleWriterState) theAggregatorRoleAttemptsINSERT() error {
	_, s.lastInsertErr = s.aggregatorDB.ExecContext(context.Background(), `
		INSERT INTO clean_code.cross_repo_percentile
			(metric_kind, scope_kind, histogram_json, p50, p90, p99)
		VALUES ('e2e_sole_writer_test', 'class', '{"test":true}'::jsonb, 1.0, 2.0, 3.0)
	`)
	return nil
}

func (s *soleWriterState) theAggregatorINSERTSucceeds() error {
	if s.lastInsertErr != nil {
		return fmt.Errorf("expected aggregator INSERT to succeed, got: %v", s.lastInsertErr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_cross_repo_aggregator_aggregator_cadence_loop_and_snapshot_writers
// registers all Given/When/Then steps for the
// aggregator-cadence-loop-and-snapshot-writers stage.
func InitializeScenario_cross_repo_aggregator_aggregator_cadence_loop_and_snapshot_writers(ctx *godog.ScenarioContext) {
	var tickState *aggregatorTickState
	var writerState *soleWriterState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		aggDSN := os.Getenv("CLEAN_CODE_PG_URL")
		if aggDSN == "" {
			return ctx, fmt.Errorf("CLEAN_CODE_PG_URL is not set")
		}

		aggregatorURL := os.Getenv("CLEAN_CODE_AGGREGATOR_URL")
		if aggregatorURL == "" {
			aggregatorURL = "http://localhost:8085"
		}

		var err error
		tickState, err = newAggregatorTickState(aggDSN, aggregatorURL)
		if err != nil {
			return ctx, err
		}

		nonAggDSN := os.Getenv("CLEAN_CODE_PG_NONAGG_URL")
		if nonAggDSN != "" {
			writerState, err = newSoleWriterState(aggDSN, nonAggDSN)
			if err != nil {
				tickState.close()
				return ctx, err
			}
		}

		return ctx, nil
	})

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if tickState != nil {
			tickState.cleanup()
			tickState.close()
		}
		if writerState != nil {
			writerState.cleanup()
			writerState.close()
		}
		return ctx, nil
	})

	// Background
	ctx.Step(`^a running Cross-Repo Aggregator connected to PostgreSQL$`, func() error {
		return tickState.aRunningAggregatorConnectedToPostgreSQL()
	})

	// tick-writes-snapshots
	ctx.Step(`^five repos with active metric_sample rows for "([^"]*)"$`, func(metricKind string) error {
		return tickState.fiveReposWithActiveSamples(metricKind)
	})
	ctx.Step(`^the aggregator tick endpoint is invoked$`, func() error {
		return tickState.theAggregatorTickEndpointIsInvoked()
	})
	ctx.Step(`^repo_metric_snapshot has five rows for metric_kind "([^"]*)"$`, func(metricKind string) error {
		return tickState.repoMetricSnapshotHasNRows(metricKind)
	})
	ctx.Step(`^cross_repo_percentile has one row with non-null p50 p90 p99 histogram_json built_at$`, func() error {
		return tickState.crossRepoPercentileHasOneRow()
	})

	// aggregator-is-sole-writer
	ctx.Step(`^a non-aggregator database role$`, func() error {
		if writerState == nil {
			return fmt.Errorf("CLEAN_CODE_PG_NONAGG_URL is not set; cannot test sole-writer scenario")
		}
		return writerState.aNonAggregatorDatabaseRole()
	})
	ctx.Step(`^it attempts INSERT into cross_repo_percentile$`, func() error {
		return writerState.itAttemptsINSERTIntoCrossRepoPercentile()
	})
	ctx.Step(`^PostgreSQL returns permission denied$`, func() error {
		return writerState.postgresqlReturnsPermissionDenied()
	})
	ctx.Step(`^the aggregator role attempts INSERT into cross_repo_percentile$`, func() error {
		return writerState.theAggregatorRoleAttemptsINSERT()
	})
	ctx.Step(`^the aggregator INSERT succeeds$`, func() error {
		return writerState.theAggregatorINSERTSucceeds()
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_cross_repo_aggregator_aggregator_cadence_loop_and_snapshot_writers(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_cross_repo_aggregator_aggregator_cadence_loop_and_snapshot_writers,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"cross_repo_aggregator_aggregator_cadence_loop_and_snapshot_writers.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}
