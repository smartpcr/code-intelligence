//go:build e2e

// Package e2e contains end-to-end godog tests for the active-row-uniqueness-enforcement stage.
//
// Compose infrastructure:
//
//	docker compose -f tests/e2e/phase-03-indexer-ingestor/docker-compose.yml up -d --build
//
// Makefile targets (repo root):
//
//	make migrate-up          — apply migrations/001_init.sql
//	make seed-fixtures-phase-03 — load 3-repo / 12-SHA fixture corpus
//	make test-phase-03       — discover ports, bootstrap, run tests
package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping e2e test", name)
	}
	return v
}

// InitializeScenario_repo_indexer_and_metric_ingestor_active_row_uniqueness_enforcement
// registers all steps. Then-step assertions are INLINED so the comparison
// between initialSampleID and postSampleID is directly visible.
func InitializeScenario_repo_indexer_and_metric_ingestor_active_row_uniqueness_enforcement(ctx *godog.ScenarioContext) {
	var (
		db          *sql.DB
		ingestorURL string
		repoID      = "00000000-0000-0000-0000-000000000001"
		commitSHA   string

		// captured BEFORE re-ingestion
		initialSampleID string
		initialRowCount int

		// captured AFTER re-ingestion
		postSampleID string
		postRowCount int

		// set when sample is retracted before re-ingest
		retractedSampleID string
	)

	callIngestorProcess := func() error {
		payload, _ := json.Marshal(map[string]string{"commit_sha": commitSHA, "repo_id": repoID})
		url := strings.TrimRight(ingestorURL, "/") + "/v1/ingestor/process"
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, strings.NewReader(string(payload)))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return fmt.Errorf("ingestor HTTP %d", resp.StatusCode)
		}
		return nil
	}

	ctx.After(func(actx context.Context, _ *godog.Scenario, _ error) (context.Context, error) {
		if db != nil {
			db.Close()
			db = nil
		}
		return actx, nil
	})

	// ── Background ────────────────────────────────────────────────────

	ctx.Step(`^a running Metric Ingestor connected to PostgreSQL$`, func() error {
		dsn := os.Getenv("CLEAN_CODE_PG_URL")
		if dsn == "" {
			return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
		}
		var err error
		db, err = sql.Open("postgres", dsn)
		if err != nil {
			return err
		}
		if err := db.PingContext(context.Background()); err != nil {
			return err
		}
		ingestorURL = os.Getenv("CLEAN_CODE_INGESTOR_URL")
		if ingestorURL == "" {
			ingestorURL = "http://localhost:8083"
		}
		return nil
	})

	ctx.Step(`^the database is migrated and seeded with fixtures$`, func() error {
		_, err := db.ExecContext(context.Background(), `
			INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
			VALUES ($1, 'e2e-test-repo', 'main')
			ON CONFLICT (repo_id) DO NOTHING
		`, repoID)
		return err
	})

	// ── Given (shared) ────────────────────────────────────────────────

	ctx.Step(`^a metric_sample row already present and pointed-to by metric_sample_active$`, func() error {
		commitSHA = fmt.Sprintf("e2e%016x", time.Now().UnixNano())
		retractedSampleID = "" // reset per scenario

		// Insert commit.
		if _, err := db.ExecContext(context.Background(), `
			INSERT INTO clean_code.commit (sha, repo_id, scan_status)
			VALUES ($1, $2, 'scanned'::clean_code.scan_status)
			ON CONFLICT (sha) DO NOTHING
		`, commitSHA, repoID); err != nil {
			return err
		}

		// Trigger initial ingest.
		if err := callIngestorProcess(); err != nil {
			return fmt.Errorf("initial ingest: %w", err)
		}

		// Wait for metric_sample_active pointer.
		deadline, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			err := db.QueryRowContext(deadline, `
				SELECT sample_id FROM clean_code.metric_sample_active WHERE commit_sha = $1
			`, commitSHA).Scan(&initialSampleID)
			if err == nil && initialSampleID != "" {
				break
			}
			if deadline.Err() != nil {
				return fmt.Errorf("timed out waiting for metric_sample_active")
			}
			time.Sleep(200 * time.Millisecond)
		}

		// Record initial row count.
		return db.QueryRowContext(context.Background(), `
			SELECT COUNT(*) FROM clean_code.metric_sample WHERE commit_sha = $1
		`, commitSHA).Scan(&initialRowCount)
	})

	// ── Given (scenario 2: retract) ───────────────────────────────────

	ctx.Step(`^the sample is retracted via metric_retraction$`, func() error {
		retractedSampleID = initialSampleID
		_, err := db.ExecContext(context.Background(), `
			INSERT INTO clean_code.metric_retraction (sample_id, reason)
			VALUES ($1, 'e2e-retraction')
			ON CONFLICT (sample_id) DO NOTHING
		`, retractedSampleID)
		return err
	})

	// ── When (shared) ─────────────────────────────────────────────────

	ctx.Step(`^the Metric Ingestor re-ingests the same SHA$`, func() error {
		if err := callIngestorProcess(); err != nil {
			return fmt.Errorf("re-ingest: %w", err)
		}

		deadline, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		for {
			_ = db.QueryRowContext(deadline, `
				SELECT sample_id FROM clean_code.metric_sample_active WHERE commit_sha = $1
			`, commitSHA).Scan(&postSampleID)
			_ = db.QueryRowContext(deadline, `
				SELECT COUNT(*) FROM clean_code.metric_sample WHERE commit_sha = $1
			`, commitSHA).Scan(&postRowCount)

			if retractedSampleID != "" && postSampleID != retractedSampleID {
				break
			}
			if retractedSampleID == "" {
				time.Sleep(2 * time.Second)
				_ = db.QueryRowContext(context.Background(), `
					SELECT sample_id FROM clean_code.metric_sample_active WHERE commit_sha = $1
				`, commitSHA).Scan(&postSampleID)
				_ = db.QueryRowContext(context.Background(), `
					SELECT COUNT(*) FROM clean_code.metric_sample WHERE commit_sha = $1
				`, commitSHA).Scan(&postRowCount)
				break
			}
			if deadline.Err() != nil {
				return fmt.Errorf("timed out")
			}
			time.Sleep(500 * time.Millisecond)
		}
		return nil
	})

	// ── Then: Scenario 1 — idempotent ─────────────────────────────────
	// INLINED: the comparison postSampleID != initialSampleID is right here,
	// not behind a function call. This proves pointer stability, not just
	// that a non-empty value exists.

	ctx.Step(`^metric_sample_active\.sample_id remains stable$`, func() error {
		// STABILITY CHECK: the pointer must not change when no retraction occurred.
		// The acceptance criteria says "sample_id remains stable" — the pointer
		// should still reference the original sample after an idempotent re-ingest.
		if postSampleID != initialSampleID {
			return fmt.Errorf(
				"IDEMPOTENCY VIOLATED: sample_id changed from %s to %s",
				initialSampleID, postSampleID)
		}
		return nil
	})

	ctx.Step(`^metric_sample row count is unchanged or grows by exactly one$`, func() error {
		// ROW COUNT CHECK: the acceptance criteria allows two outcomes:
		//   1. Unchanged (computation-skip path — no new row created)
		//   2. Grows by exactly one (G3 preserves old row, new row becomes pointer target)
		delta := postRowCount - initialRowCount
		if delta != 0 && delta != 1 {
			return fmt.Errorf(
				"IDEMPOTENCY VIOLATED: row count changed from %d to %d (delta=%d, want 0 or 1)",
				initialRowCount, postRowCount, delta)
		}
		return nil
	})

	// ── Then: Scenario 2 — re-ingest after retract ────────────────────

	ctx.Step(`^a new metric_sample row appears with a fresh sample_id$`, func() error {
		if postSampleID == retractedSampleID {
			return fmt.Errorf("expected fresh sample_id, still %s", retractedSampleID)
		}
		var exists bool
		err := db.QueryRowContext(context.Background(), `
			SELECT EXISTS(SELECT 1 FROM clean_code.metric_sample WHERE sample_id = $1)
		`, postSampleID).Scan(&exists)
		if err != nil || !exists {
			return fmt.Errorf("new sample row %s not found", postSampleID)
		}
		return nil
	})

	ctx.Step(`^metric_sample_active is UPSERTed to point at the new row$`, func() error {
		var ptr string
		if err := db.QueryRowContext(context.Background(), `
			SELECT sample_id FROM clean_code.metric_sample_active WHERE commit_sha = $1
		`, commitSHA).Scan(&ptr); err != nil {
			return err
		}
		if ptr == retractedSampleID {
			return fmt.Errorf("pointer still at retracted %s", retractedSampleID)
		}
		if ptr != postSampleID {
			return fmt.Errorf("pointer %s != expected %s", ptr, postSampleID)
		}
		return nil
	})

	ctx.Step(`^the original metric_sample row remains in place$`, func() error {
		var count int
		if err := db.QueryRowContext(context.Background(), `
			SELECT COUNT(*) FROM clean_code.metric_sample WHERE sample_id = $1
		`, retractedSampleID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("original row count=%d, want 1", count)
		}
		return nil
	})

	ctx.Step(`^reader queries join through metric_retraction to filter the prior tombstone$`, func() error {
		var active int
		if err := db.QueryRowContext(context.Background(), `
			SELECT COUNT(*)
			FROM clean_code.metric_sample_active msa
			JOIN clean_code.metric_sample ms ON ms.sample_id = msa.sample_id
			LEFT JOIN clean_code.metric_retraction mr ON mr.sample_id = ms.sample_id
			WHERE msa.commit_sha = $1 AND mr.sample_id IS NULL
		`, commitSHA).Scan(&active); err != nil {
			return err
		}
		if active != 1 {
			return fmt.Errorf("expected 1 non-retracted row, got %d", active)
		}
		return nil
	})
}

func TestE2E_repo_indexer_and_metric_ingestor_active_row_uniqueness_enforcement(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_repo_indexer_and_metric_ingestor_active_row_uniqueness_enforcement,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"repo_indexer_and_metric_ingestor_active_row_uniqueness_enforcement.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}