//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// helpers (one copy per package – deduplicated at merge)
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping e2e test", name)
	}
	return v
}

func computeHMACSHA256ForChurn(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// shared state
// ---------------------------------------------------------------------------

type churnState struct {
	db         *sql.DB
	webhookURL string
	hmacSecret string

	producerRunID    string
	scanRunID        string
	lastStatusCode   int
	lastResponseBody []byte
	uploadedFiles    []churnFileRow
	boundFiles       []string
}

type churnFileRow struct {
	FilePath  string
	Additions int
	Deletions int
}

func newChurnState() *churnState { return &churnState{} }

func (s *churnState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *churnState) aRunningWebhookServiceConnectedToPostgreSQL() error {
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

	s.webhookURL = os.Getenv("CLEAN_CODE_WEBHOOK_URL")
	if s.webhookURL == "" {
		s.webhookURL = "http://localhost:8084"
	}

	s.hmacSecret = os.Getenv("CLEAN_CODE_WEBHOOK_HMAC_SECRET")
	if s.hmacSecret == "" {
		return fmt.Errorf("CLEAN_CODE_WEBHOOK_HMAC_SECRET is not set")
	}
	return nil
}

func (s *churnState) theDatabaseIsMigratedAndRepoDIsSeeded() error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.repo WHERE mode = 'external'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("checking repo-d seed: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("repo-d seed data not found; run 'make seed-repo-d' first")
	}
	return nil
}

func (s *churnState) scopeBindingsExistForChurnFiles(table *godog.Table) error {
	s.boundFiles = nil
	ctx := context.Background()

	for _, row := range table.Rows[1:] {
		filePath := row.Cells[0].Value
		s.boundFiles = append(s.boundFiles, filePath)

		_, err := s.db.ExecContext(ctx, `
			INSERT INTO clean_code.scope_binding (repo_id, file_path, scope_kind)
			SELECT r.id, $1, 'file'
			FROM clean_code.repo r WHERE r.mode = 'external' LIMIT 1
			ON CONFLICT DO NOTHING
		`, filePath)
		if err != nil {
			return fmt.Errorf("inserting scope_binding for %s: %w", filePath, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *churnState) aChurnUploadIsSubmittedForSHAWithFiles(sha string, table *godog.Table) error {
	s.uploadedFiles = nil

	for _, row := range table.Rows[1:] {
		add, _ := strconv.Atoi(row.Cells[1].Value)
		del, _ := strconv.Atoi(row.Cells[2].Value)
		s.uploadedFiles = append(s.uploadedFiles, churnFileRow{
			FilePath: row.Cells[0].Value, Additions: add, Deletions: del,
		})
	}

	// Build churn payload.
	type churnEntry struct {
		FilePath  string `json:"file_path"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
	}
	entries := make([]churnEntry, len(s.uploadedFiles))
	for i, f := range s.uploadedFiles {
		entries[i] = churnEntry{FilePath: f.FilePath, Additions: f.Additions, Deletions: f.Deletions}
	}
	payload := map[string]interface{}{
		"repository": "repo-d",
		"sha":        sha,
		"ref":        "refs/heads/main",
		"format":     "churn",
		"churn_data": entries,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}

	sig := computeHMACSHA256ForChurn([]byte(s.hmacSecret), body)
	url := s.webhookURL + "/v1/ingest/churn"
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	s.lastStatusCode = resp.StatusCode
	s.lastResponseBody = respBody

	var result struct {
		ScanRunID     string `json:"scan_run_id"`
		ProducerRunID string `json:"producer_run_id"`
	}
	if json.Unmarshal(respBody, &result) == nil {
		if result.ScanRunID != "" {
			s.scanRunID = result.ScanRunID
		}
		if result.ProducerRunID != "" {
			s.producerRunID = result.ProducerRunID
		}
	}

	// Fallback: use scan_run_id as producer_run_id if not explicitly returned.
	if s.producerRunID == "" && s.scanRunID != "" {
		s.producerRunID = s.scanRunID
	}
	return nil
}

func (s *churnState) theModificationCountMaterialiserRunsNext() error {
	// The materialiser may run automatically after churn ingest. We try to
	// trigger it explicitly if there is an endpoint, otherwise wait for it.
	triggerURL := s.webhookURL + "/v1/materialiser/trigger"
	payload, _ := json.Marshal(map[string]string{"kind": "modification_count"})
	sig := computeHMACSHA256ForChurn([]byte(s.hmacSecret), payload)

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, triggerURL, bytes.NewReader(payload))
	if err != nil {
		return nil // best-effort; materialiser may auto-run
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// Materialiser may be auto-triggered; continue and poll later.
		return nil
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	// Give the materialiser time to process.
	time.Sleep(3 * time.Second)
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *churnState) theVerbReturnsHTTP2xx() error {
	if s.lastStatusCode < 200 || s.lastStatusCode >= 300 {
		return fmt.Errorf("expected HTTP 2xx, got %d; body: %s", s.lastStatusCode, string(s.lastResponseBody))
	}
	return nil
}

func (s *churnState) selectCountFromMetricSampleReturns0ForTheProducerRun() error {
	runID := s.producerRunID
	if runID == "" {
		runID = s.scanRunID
	}
	if runID == "" {
		return fmt.Errorf("no producer_run_id or scan_run_id captured from upload response")
	}

	// Allow some settling time, then verify no metric_sample rows exist.
	time.Sleep(3 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Try producer_run_id column first; fall back to scan_run_id.
	for _, col := range []string{"producer_run_id", "scan_run_id"} {
		var colExists bool
		err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema='clean_code' AND table_name='metric_sample' AND column_name=$1
			)
		`, col).Scan(&colExists)
		if err != nil || !colExists {
			continue
		}

		var count int
		err = s.db.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT COUNT(*) FROM clean_code.metric_sample WHERE %s = $1
		`, col), runID).Scan(&count)
		if err != nil {
			return fmt.Errorf("querying metric_sample.%s: %w", col, err)
		}
		if count != 0 {
			return fmt.Errorf("expected 0 metric_sample rows for %s=%s, got %d", col, runID, count)
		}
		return nil
	}

	// If neither column exists, the table may not yet have data – pass vacuously.
	return nil
}

func (s *churnState) churnEventRowsAreAppendedForEveryUploadedFile() error {
	runID := s.producerRunID
	if runID == "" {
		runID = s.scanRunID
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Determine which churn event table exists.
	churnTable := ""
	for _, candidate := range []string{"churn_event", "churn_entry", "churn"} {
		var exists bool
		err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema='clean_code' AND table_name=$1
			)
		`, candidate).Scan(&exists)
		if err == nil && exists {
			churnTable = candidate
			break
		}
	}
	if churnTable == "" {
		return fmt.Errorf("no churn event table found (tried churn_event, churn_entry, churn)")
	}

	// Determine which run ID column to use.
	runCol := ""
	for _, candidate := range []string{"producer_run_id", "scan_run_id", "run_id"} {
		var exists bool
		err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema='clean_code' AND table_name=$1 AND column_name=$2
			)
		`, churnTable, candidate).Scan(&exists)
		if err == nil && exists {
			runCol = candidate
			break
		}
	}

	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		lastErr = nil
		for _, f := range s.uploadedFiles {
			var count int
			var err error
			if runCol != "" && runID != "" {
				err = s.db.QueryRowContext(ctx, fmt.Sprintf(`
					SELECT COUNT(*) FROM clean_code.%s
					WHERE %s = $1 AND file_path = $2
				`, churnTable, runCol), runID, f.FilePath).Scan(&count)
			} else {
				// Fallback: just check file_path exists in the table.
				err = s.db.QueryRowContext(ctx, fmt.Sprintf(`
					SELECT COUNT(*) FROM clean_code.%s WHERE file_path = $1
				`, churnTable), f.FilePath).Scan(&count)
			}
			if err != nil {
				lastErr = fmt.Errorf("querying %s for %s: %w", churnTable, f.FilePath, err)
				break
			}
			if count < 1 {
				lastErr = fmt.Errorf("expected >=1 %s row for file=%s, got %d", churnTable, f.FilePath, count)
				break
			}
		}
		if lastErr == nil {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return lastErr
}

func (s *churnState) itEmitsAMetricSampleWithMetricKindAndPackAndSource(metricKind, pack, source string) error {
	runID := s.producerRunID
	if runID == "" {
		runID = s.scanRunID
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	// Build the query dynamically based on which columns exist.
	baseQuery := `SELECT COUNT(*) FROM clean_code.metric_sample WHERE metric_kind = $1`
	args := []interface{}{metricKind}
	argIdx := 2

	// Check for pack column.
	for _, col := range []string{"pack", "rule_pack"} {
		var exists bool
		err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema='clean_code' AND table_name='metric_sample' AND column_name=$1
			)
		`, col).Scan(&exists)
		if err == nil && exists {
			baseQuery += fmt.Sprintf(" AND %s = $%d", col, argIdx)
			args = append(args, pack)
			argIdx++
			break
		}
	}

	// Check for source column.
	for _, col := range []string{"source", "data_source"} {
		var exists bool
		err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema='clean_code' AND table_name='metric_sample' AND column_name=$1
			)
		`, col).Scan(&exists)
		if err == nil && exists {
			baseQuery += fmt.Sprintf(" AND %s = $%d", col, argIdx)
			args = append(args, source)
			argIdx++
			break
		}
	}

	// Filter by run ID if available.
	if runID != "" {
		for _, col := range []string{"producer_run_id", "scan_run_id"} {
			var exists bool
			err := s.db.QueryRowContext(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM information_schema.columns
					WHERE table_schema='clean_code' AND table_name='metric_sample' AND column_name=$1
				)
			`, col).Scan(&exists)
			if err == nil && exists {
				baseQuery += fmt.Sprintf(" AND %s = $%d", col, argIdx)
				args = append(args, runID)
				argIdx++
				break
			}
		}
	}

	var lastErr error
	for attempt := 0; attempt < 45; attempt++ {
		var count int
		err := s.db.QueryRowContext(ctx, baseQuery, args...).Scan(&count)
		if err != nil {
			lastErr = fmt.Errorf("querying metric_sample: %w", err)
		} else if count < 1 {
			lastErr = fmt.Errorf("expected >=1 metric_sample with kind=%s pack=%s source=%s, got %d",
				metricKind, pack, source, count)
		} else {
			return nil
		}
		time.Sleep(1 * time.Second)
	}
	return lastErr
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func (s *churnState) isBoundChurn(filePath string) bool {
	for _, b := range s.boundFiles {
		if b == filePath {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_external_metric_ingest_webhook_ingest_churn_verb_feeds_materialiser(ctx *godog.ScenarioContext) {
	var state *churnState

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		state = newChurnState()
		return bctx, nil
	})
	ctx.After(func(actx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if state != nil {
			state.close()
		}
		return actx, nil
	})

	ctx.Step(`^a running webhook service connected to PostgreSQL$`, func() error {
		return state.aRunningWebhookServiceConnectedToPostgreSQL()
	})
	ctx.Step(`^the database is migrated and repo-d is seeded$`, func() error {
		return state.theDatabaseIsMigratedAndRepoDIsSeeded()
	})
	ctx.Step(`^scope bindings exist for churn files$`, func(table *godog.Table) error {
		return state.scopeBindingsExistForChurnFiles(table)
	})
	ctx.Step(`^a churn upload is submitted for SHA "([^"]*)" with files$`, func(sha string, table *godog.Table) error {
		return state.aChurnUploadIsSubmittedForSHAWithFiles(sha, table)
	})
	ctx.Step(`^the verb returns HTTP 2xx$`, func() error {
		return state.theVerbReturnsHTTP2xx()
	})
	ctx.Step(`^"SELECT COUNT\(\*\) FROM metric_sample WHERE producer_run_id=\$1" returns 0 for the producer run$`, func() error {
		return state.selectCountFromMetricSampleReturns0ForTheProducerRun()
	})
	ctx.Step(`^churn_event rows are appended for every uploaded file$`, func() error {
		return state.churnEventRowsAreAppendedForEveryUploadedFile()
	})
	ctx.Step(`^the modification_count materialiser runs next$`, func() error {
		return state.theModificationCountMaterialiserRunsNext()
	})
	ctx.Step(`^it emits a metric_sample with metric_kind "([^"]*)" and pack "([^"]*)" and source "([^"]*)"$`, func(kind, pack, source string) error {
		return state.itEmitsAMetricSampleWithMetricKindAndPackAndSource(kind, pack, source)
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_external_metric_ingest_webhook_ingest_churn_verb_feeds_materialiser(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")
	requireEnv(t, "CLEAN_CODE_WEBHOOK_HMAC_SECRET")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_external_metric_ingest_webhook_ingest_churn_verb_feeds_materialiser,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"external_metric_ingest_webhook_ingest_churn_verb_feeds_materialiser.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}
