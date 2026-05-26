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

// requireEnvSweep returns the value of the named environment variable,
// calling t.Skip when unset or empty.  Each *_test.go file in the e2e
// package may carry its own copy so files stay self-contained.
func requireEnvSweep(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

// ---------- shared state ----------

type sweepState struct {
	db        *sql.DB
	scanRunID string
	commitID  string
}

func newSweepState() (*sweepState, error) {
	dsn := os.Getenv("CLEAN_CODE_PG_URL")
	if dsn == "" {
		return nil, fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &sweepState{db: db}, nil
}

func (s *sweepState) cleanup() {
	if s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.scanRunID != "" {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM scan_runs WHERE id = $1`, s.scanRunID)
	}
	if s.commitID != "" {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM commits WHERE id = $1`, s.commitID)
	}
	_ = s.db.Close()
}

// ---------- Scenario: stale-scan-run-becomes-failed ----------

func (s *sweepState) aScanRunRowWithStatusWhoseUpdatedAtIsOlderThan30Minutes(status string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	staleTime := time.Now().UTC().Add(-31 * time.Minute)
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO scan_runs (status, created_at, updated_at)
		 VALUES ($1, $2, $2)
		 RETURNING id`, status, staleTime).Scan(&s.scanRunID)
	if err != nil {
		return fmt.Errorf("inserting stale scan_run: %w", err)
	}
	return nil
}

func (s *sweepState) theSweepLoopExecutes() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The sweep is expected to run on a short interval in the compose
	// environment.  Poll the scan_run row until it leaves "running" or
	// until the context deadline.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for sweep to process scan_run %s", s.scanRunID)
		case <-ticker.C:
			var status string
			err := s.db.QueryRowContext(ctx,
				`SELECT status FROM scan_runs WHERE id = $1`, s.scanRunID).Scan(&status)
			if err != nil {
				return fmt.Errorf("querying scan_run status: %w", err)
			}
			if status != "running" {
				return nil
			}
		}
	}
}

func (s *sweepState) theScanRunRowTransitionsToStatus(expected string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM scan_runs WHERE id = $1`, s.scanRunID).Scan(&status)
	if err != nil {
		return fmt.Errorf("querying scan_run status: %w", err)
	}
	if status != expected {
		return fmt.Errorf("scan_run %s: want status %q, got %q", s.scanRunID, expected, status)
	}
	return nil
}

func (s *sweepState) theScanRunRowDoesNOTHaveStatusOrStatus(forbidden1, forbidden2 string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM scan_runs WHERE id = $1`, s.scanRunID).Scan(&status)
	if err != nil {
		return fmt.Errorf("querying scan_run status: %w", err)
	}
	if status == forbidden1 || status == forbidden2 {
		return fmt.Errorf("scan_run %s: has forbidden status %q", s.scanRunID, status)
	}
	return nil
}

// ---------- Scenario: stale-commit-becomes-failed ----------

func (s *sweepState) aCommitRowWithScanStatusLinkedToAScanRunThatWasJustMarked(scanStatus, runStatus string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// First, insert a scan_run already in the target runStatus (e.g. "failed").
	staleTime := time.Now().UTC().Add(-31 * time.Minute)
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO scan_runs (status, created_at, updated_at)
		 VALUES ($1, $2, $2)
		 RETURNING id`, runStatus, staleTime).Scan(&s.scanRunID)
	if err != nil {
		return fmt.Errorf("inserting scan_run with status %q: %w", runStatus, err)
	}

	// Then insert a commit linked to that scan_run still in "scanning".
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO commits (scan_run_id, scan_status, created_at, updated_at)
		 VALUES ($1, $2, now(), now())
		 RETURNING id`, s.scanRunID, scanStatus).Scan(&s.commitID)
	if err != nil {
		return fmt.Errorf("inserting commit with scan_status %q: %w", scanStatus, err)
	}
	return nil
}

func (s *sweepState) theSweepFinalises() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Poll the commit row until it leaves "scanning" or until timeout.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for sweep to finalise commit %s", s.commitID)
		case <-ticker.C:
			var scanStatus string
			err := s.db.QueryRowContext(ctx,
				`SELECT scan_status FROM commits WHERE id = $1`, s.commitID).Scan(&scanStatus)
			if err != nil {
				return fmt.Errorf("querying commit scan_status: %w", err)
			}
			if scanStatus != "scanning" {
				return nil
			}
		}
	}
}

func (s *sweepState) theCommitRowTransitionsToScanStatus(expected string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var scanStatus string
	err := s.db.QueryRowContext(ctx,
		`SELECT scan_status FROM commits WHERE id = $1`, s.commitID).Scan(&scanStatus)
	if err != nil {
		return fmt.Errorf("querying commit scan_status: %w", err)
	}
	if scanStatus != expected {
		return fmt.Errorf("commit %s: want scan_status %q, got %q", s.commitID, expected, scanStatus)
	}
	return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop(ctx *godog.ScenarioContext) {
	var s *sweepState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		var err error
		s, err = newSweepState()
		if err != nil {
			return ctx, fmt.Errorf("sweep state init: %w", err)
		}
		return ctx, nil
	})

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if s != nil {
			s.cleanup()
		}
		return ctx, nil
	})

	// stale-scan-run-becomes-failed
	ctx.Step(`^a scan_run row with status "([^"]*)" whose updated_at is older than 30 minutes$`,
		func(status string) error { return s.aScanRunRowWithStatusWhoseUpdatedAtIsOlderThan30Minutes(status) })
	ctx.Step(`^the sweep loop executes$`,
		func() error { return s.theSweepLoopExecutes() })
	ctx.Step(`^the scan_run row transitions to status "([^"]*)"$`,
		func(expected string) error { return s.theScanRunRowTransitionsToStatus(expected) })
	ctx.Step(`^the scan_run row does NOT have status "([^"]*)" or "([^"]*)"$`,
		func(f1, f2 string) error { return s.theScanRunRowDoesNOTHaveStatusOrStatus(f1, f2) })

	// stale-commit-becomes-failed
	ctx.Step(`^a commit row with scan_status "([^"]*)" linked to a scan_run that was just marked "([^"]*)"$`,
		func(scanStatus, runStatus string) error {
			return s.aCommitRowWithScanStatusLinkedToAScanRunThatWasJustMarked(scanStatus, runStatus)
		})
	ctx.Step(`^the sweep finalises$`,
		func() error { return s.theSweepFinalises() })
	ctx.Step(`^the commit row transitions to scan_status "([^"]*)"$`,
		func(expected string) error { return s.theCommitRowTransitionsToScanStatus(expected) })
}

func TestE2E_repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop(t *testing.T) {
	requireEnvSweep(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop.feature"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
