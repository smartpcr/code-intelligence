//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
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

// ---------------------------------------------------------------------------
// shared state for retraction-and-rescan scenarios
// ---------------------------------------------------------------------------

type retractionRescanState struct {
	db       *sql.DB
	repoID   string
	sha      string
	sampleID string
}

func newRetractionRescanState() *retractionRescanState {
	return &retractionRescanState{
		repoID: "00000000-0000-0000-0000-000000000001",
		sha:    "e2eretract0cafe1234",
	}
}

func (s *retractionRescanState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *retractionRescanState) aRunningMetricIngestorConnectedToPostgreSQL() error {
	dsn := os.Getenv("CLEAN_CODE_PG_URL")
	if dsn == "" {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return fmt.Errorf("pinging postgres: %w", err)
	}
	s.db = db
	return nil
}

func (s *retractionRescanState) theDatabaseIsMigratedAndSeededWithAnActiveSample() error {
	ctx := context.Background()

	// Ensure repo fixture exists.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ($1, 'e2e-retraction-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`, s.repoID)
	if err != nil {
		return fmt.Errorf("ensuring repo fixture: %w", err)
	}

	// Ensure commit fixture exists.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO clean_code.commit (repo_id, sha, scan_status)
		VALUES ($1, $2, 'scanned')
		ON CONFLICT (sha) DO NOTHING
	`, s.repoID, s.sha)
	if err != nil {
		return fmt.Errorf("ensuring commit fixture: %w", err)
	}

	// Insert an active metric sample and capture its ID.
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO clean_code.metric_sample (repo_id, sha, metric_name, metric_value)
		VALUES ($1, $2, 'cyclomatic_complexity', 4.2)
		RETURNING sample_id
	`, s.repoID, s.sha).Scan(&s.sampleID)
	if err != nil {
		return fmt.Errorf("inserting metric_sample: %w", err)
	}

	// Mark it as the active sample.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO clean_code.metric_sample_active (repo_id, sha, sample_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (repo_id, sha) DO UPDATE SET sample_id = EXCLUDED.sample_id
	`, s.repoID, s.sha, s.sampleID)
	if err != nil {
		return fmt.Errorf("inserting metric_sample_active: %w", err)
	}

	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *retractionRescanState) mgmtRetractSampleIsInvokedWithReason(reason string) error {
	ctx := context.Background()

	// Invoke mgmt.retract_sample stored procedure / management function.
	_, err := s.db.ExecContext(ctx, `
		SELECT clean_code.mgmt_retract_sample($1, $2)
	`, s.sampleID, reason)
	if err != nil {
		return fmt.Errorf("invoking mgmt.retract_sample: %w", err)
	}
	return nil
}

func (s *retractionRescanState) mgmtRescanIsInvokedWithTheRepoIDAndSha() error {
	ctx := context.Background()

	// Invoke mgmt.rescan management function.
	_, err := s.db.ExecContext(ctx, `
		SELECT clean_code.mgmt_rescan($1, $2)
	`, s.repoID, s.sha)
	if err != nil {
		return fmt.Errorf("invoking mgmt.rescan: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — retraction scenario
// ---------------------------------------------------------------------------

func (s *retractionRescanState) aMetricRetractionRowAppearsWithReason(expectedReason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var reason string
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT reason FROM clean_code.metric_retraction
			WHERE sample_id = $1
		`, s.sampleID).Scan(&reason)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for metric_retraction row for sample_id=%s: %w", s.sampleID, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if reason != expectedReason {
		return fmt.Errorf("expected retraction reason=%q, got %q", expectedReason, reason)
	}
	return nil
}

func (s *retractionRescanState) aScanRunWithKindAndStatusIsRecorded(expectedKind, expectedStatus string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var kind, status string
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT kind, status FROM clean_code.scan_run
			WHERE repo_id = $1 AND sha = $2 AND kind = $3
			ORDER BY created_at DESC
			LIMIT 1
		`, s.repoID, s.sha, expectedKind).Scan(&kind, &status)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for scan_run(kind=%q) for repo_id=%s sha=%s: %w",
				expectedKind, s.repoID, s.sha, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if status != expectedStatus {
		return fmt.Errorf("expected scan_run status=%q, got %q", expectedStatus, status)
	}
	return nil
}

func (s *retractionRescanState) theMetricSampleActivePointerRowRemainsInPlace() error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample_active
		WHERE repo_id = $1 AND sha = $2 AND sample_id = $3
	`, s.repoID, s.sha, s.sampleID).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying metric_sample_active: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("expected metric_sample_active pointer to remain (count=1), got count=%d", count)
	}
	return nil
}

func (s *retractionRescanState) shaPinnedReaderJoinsThroughMetricRetractionCorrectlyFilterOutTheRetractedSample() error {
	// A SHA-pinned reader joins metric_sample_active with metric_retraction
	// to exclude retracted samples. After retraction, the join should return
	// zero rows for this sample.
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample_active msa
		JOIN clean_code.metric_sample ms ON ms.sample_id = msa.sample_id
		WHERE msa.repo_id = $1
		  AND msa.sha = $2
		  AND NOT EXISTS (
			SELECT 1 FROM clean_code.metric_retraction mr
			WHERE mr.sample_id = msa.sample_id
		  )
	`, s.repoID, s.sha).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying SHA-pinned reader join: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected SHA-pinned reader to filter out retracted sample (count=0), got count=%d", count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — rescan scenario
// ---------------------------------------------------------------------------

func (s *retractionRescanState) aServiceInternalRescanRequestIsLoggedForThatRepoAndSha() error {
	// The mgmt_rescan function records a service-internal rescan request in
	// clean_code.rescan_request. This table is the internal queue consumed by
	// the ingestor — no external RepoEvent is emitted (verified separately).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var count int
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM clean_code.rescan_request
			WHERE repo_id = $1 AND sha = $2
		`, s.repoID, s.sha).Scan(&count)
		if err == nil && count > 0 {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for rescan_request row for repo_id=%s sha=%s", s.repoID, s.sha)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if count < 1 {
		return fmt.Errorf("expected at least 1 rescan_request row for repo_id=%s sha=%s, got %d", s.repoID, s.sha, count)
	}
	return nil
}

func (s *retractionRescanState) aScanRunWithKindAndStatusIsObservable(expectedKind, expectedStatus string) error {
	return s.aScanRunWithKindAndStatusIsRecorded(expectedKind, expectedStatus)
}

func (s *retractionRescanState) noRescanIntentRepoEventKindIsEmitted() error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.repo_event
		WHERE repo_id = $1
		  AND commit_sha = $2
		  AND kind::text = 'rescan_intent'
	`, s.repoID, s.sha).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying repo_event for rescan_intent: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected zero rescan_intent repo_events, got %d", count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_repo_indexer_and_metric_ingestor_retraction_and_rescan_flow
// registers all Given/When/Then steps for the retraction-and-rescan-flow stage.
func InitializeScenario_repo_indexer_and_metric_ingestor_retraction_and_rescan_flow(ctx *godog.ScenarioContext) {
	var state *retractionRescanState

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		state = newRetractionRescanState()
		return bctx, nil
	})

	ctx.After(func(actx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if state != nil {
			state.close()
		}
		return actx, nil
	})

	// Given
	ctx.Step(`^a running metric ingestor connected to PostgreSQL$`, func() error {
		return state.aRunningMetricIngestorConnectedToPostgreSQL()
	})
	ctx.Step(`^the database is migrated and seeded with an active sample$`, func() error {
		return state.theDatabaseIsMigratedAndSeededWithAnActiveSample()
	})

	// When — retraction
	ctx.Step(`^mgmt\.retract_sample is invoked with reason "([^"]*)"$`, func(reason string) error {
		return state.mgmtRetractSampleIsInvokedWithReason(reason)
	})

	// When — rescan
	ctx.Step(`^mgmt\.rescan is invoked with the repo_id and sha$`, func() error {
		return state.mgmtRescanIsInvokedWithTheRepoIDAndSha()
	})

	// Then — retraction
	ctx.Step(`^a metric_retraction row appears with reason "([^"]*)"$`, func(reason string) error {
		return state.aMetricRetractionRowAppearsWithReason(reason)
	})
	ctx.Step(`^a scan_run with kind "([^"]*)" and status "([^"]*)" is recorded$`, func(kind, status string) error {
		return state.aScanRunWithKindAndStatusIsRecorded(kind, status)
	})
	ctx.Step(`^the metric_sample_active pointer row remains in place$`, func() error {
		return state.theMetricSampleActivePointerRowRemainsInPlace()
	})
	ctx.Step(`^SHA-pinned reader joins through metric_retraction correctly filter out the retracted sample$`, func() error {
		return state.shaPinnedReaderJoinsThroughMetricRetractionCorrectlyFilterOutTheRetractedSample()
	})

	// Then — rescan
	ctx.Step(`^a service-internal rescan request is logged for that repo and sha$`, func() error {
		return state.aServiceInternalRescanRequestIsLoggedForThatRepoAndSha()
	})
	ctx.Step(`^a scan_run with kind "([^"]*)" and status "([^"]*)" is observable$`, func(kind, status string) error {
		return state.aScanRunWithKindAndStatusIsObservable(kind, status)
	})
	ctx.Step(`^no rescan_intent RepoEvent kind is emitted$`, func() error {
		return state.noRescanIntentRepoEventKindIsEmitted()
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_repo_indexer_and_metric_ingestor_retraction_and_rescan_flow(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_repo_indexer_and_metric_ingestor_retraction_and_rescan_flow,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"repo_indexer_and_metric_ingestor_retraction_and_rescan_flow.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}