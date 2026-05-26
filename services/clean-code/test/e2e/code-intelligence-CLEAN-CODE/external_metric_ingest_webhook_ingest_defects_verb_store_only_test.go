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
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// requireEnv returns the value of the named environment variable or skips the
// test when the variable is unset / empty.  One copy per package.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping e2e test", name)
	}
	return v
}

// computeHMACSHA256Defects returns the hex-encoded HMAC-SHA256 of body using secret.
func computeHMACSHA256Defects(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// sha256HexDefects returns the hex-encoded SHA-256 digest of data.
func sha256HexDefects(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// shared state for defects verb store-only scenarios
// ---------------------------------------------------------------------------

type defectsVerbState struct {
	db          *sql.DB
	webhookURL  string
	hmacSecret  string
	payload     []byte
	payloadHash string

	metricSampleCountBefore int
	lastStatusCode          int
	firstScanRunID          string
	lastScanRunID           string
	currentScanRunID        string
}

func newDefectsVerbState() *defectsVerbState {
	return &defectsVerbState{}
}

func (s *defectsVerbState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *defectsVerbState) aRunningWebhookServiceConnectedToPostgreSQL() error {
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

func (s *defectsVerbState) theDatabaseIsMigratedAndRepoDIsSeeded() error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.repo
		WHERE mode = 'external'
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("checking repo-d seed: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("repo-d seed data not found; run 'make seed-repo-d' first")
	}

	// Snapshot metric_sample count before the test for the "unchanged" assertion.
	err = s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
	`).Scan(&s.metricSampleCountBefore)
	if err != nil {
		return fmt.Errorf("counting metric_sample rows: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *defectsVerbState) aValidDefectsWebhookPOSTIsSentForSHA(sha string) error {
	payload := map[string]interface{}{
		"repository": "repo-d",
		"sha":        sha,
		"ref":        "refs/heads/main",
		"kind":       "defects",
		"defects": []map[string]interface{}{
			{
				"file":     "src/main.go",
				"line":     42,
				"rule":     "SA1000",
				"severity": "warning",
				"message":  "test defect entry",
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}
	s.payload = body
	s.payloadHash = sha256HexDefects(body)

	// Clean any previous test state for this payload hash.
	_, _ = s.db.ExecContext(context.Background(),
		`DELETE FROM clean_code.scan_run WHERE payload_hash = $1`, s.payloadHash)

	return s.doSignedPostDefects(body)
}

func (s *defectsVerbState) doSignedPostDefects(body []byte) error {
	sig := computeHMACSHA256Defects([]byte(s.hmacSecret), body)

	url := s.webhookURL + "/v1/webhook/defects"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader(body))
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

	var result struct {
		ScanRunID string `json:"scan_run_id"`
	}
	if err := json.Unmarshal(respBody, &result); err == nil && result.ScanRunID != "" {
		s.lastScanRunID = result.ScanRunID
		s.currentScanRunID = result.ScanRunID
	}
	return nil
}

func (s *defectsVerbState) theSameDefectsPayloadIsPOSTedAgainWithAValidSignature() error {
	s.lastScanRunID = ""
	return s.doSignedPostDefects(s.payload)
}

// ---------------------------------------------------------------------------
// Then steps – defects-v1-writes-no-metric
// ---------------------------------------------------------------------------

func (s *defectsVerbState) aScanRunRowExistsWithKindAndStatus(wantKind, wantStatus string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var kind, status string
	err := s.db.QueryRowContext(ctx, `
		SELECT kind, status FROM clean_code.scan_run
		WHERE payload_hash = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, s.payloadHash).Scan(&kind, &status)
	if err == sql.ErrNoRows {
		return fmt.Errorf("no scan_run row found for payload_hash=%s", s.payloadHash)
	}
	if err != nil {
		return fmt.Errorf("querying scan_run: %w", err)
	}
	if kind != wantKind {
		return fmt.Errorf("scan_run kind: want %q, got %q", wantKind, kind)
	}
	if status != wantStatus {
		return fmt.Errorf("scan_run status: want %q, got %q", wantStatus, status)
	}
	return nil
}

func (s *defectsVerbState) theMetricSampleRowCountIsUnchanged() error {
	var countAfter int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
	`).Scan(&countAfter)
	if err != nil {
		return fmt.Errorf("counting metric_sample rows after: %w", err)
	}
	if countAfter != s.metricSampleCountBefore {
		return fmt.Errorf("metric_sample row count changed: before=%d, after=%d",
			s.metricSampleCountBefore, countAfter)
	}
	return nil
}

func (s *defectsVerbState) noMetricKindRowExistsForThatScanRun(forbiddenKind string) error {
	if s.currentScanRunID == "" {
		return fmt.Errorf("no scan_run_id captured; cannot check metric_kind")
	}
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
		WHERE scan_run_id = $1 AND metric_kind = $2
	`, s.currentScanRunID, forbiddenKind).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying metric_sample for kind=%q: %w", forbiddenKind, err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 metric_sample rows with kind=%q for scan_run_id=%s, got %d",
			forbiddenKind, s.currentScanRunID, count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps – defects-idempotent
// ---------------------------------------------------------------------------

func (s *defectsVerbState) theResponseStatusCodeIs2xxAndAScanRunIDIsReturned() error {
	if s.lastStatusCode < 200 || s.lastStatusCode >= 300 {
		return fmt.Errorf("expected 2xx, got %d", s.lastStatusCode)
	}
	if s.lastScanRunID == "" {
		return fmt.Errorf("expected a scan_run_id in response body, got none")
	}
	s.firstScanRunID = s.lastScanRunID
	return nil
}

func (s *defectsVerbState) theSameScanRunIDIsReturned() error {
	if s.lastScanRunID == "" {
		return fmt.Errorf("no scan_run_id returned on replay")
	}
	if s.lastScanRunID != s.firstScanRunID {
		return fmt.Errorf("expected scan_run_id=%q (original), got %q",
			s.firstScanRunID, s.lastScanRunID)
	}
	return nil
}

func (s *defectsVerbState) noSecondScanRunRowIsAppendedForThatPayloadHash() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM clean_code.scan_run
		WHERE payload_hash = $1
	`, s.payloadHash).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying scan_run count: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("expected exactly 1 scan_run row for payload_hash=%s, got %d",
			s.payloadHash, count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_external_metric_ingest_webhook_ingest_defects_verb_store_only(ctx *godog.ScenarioContext) {
	var state *defectsVerbState

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		state = newDefectsVerbState()
		return bctx, nil
	})

	ctx.After(func(actx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if state != nil {
			state.close()
		}
		return actx, nil
	})

	// Given
	ctx.Step(`^a running webhook service connected to PostgreSQL$`, func() error {
		return state.aRunningWebhookServiceConnectedToPostgreSQL()
	})
	ctx.Step(`^the database is migrated and repo-d is seeded$`, func() error {
		return state.theDatabaseIsMigratedAndRepoDIsSeeded()
	})

	// When
	ctx.Step(`^a valid defects webhook POST is sent for SHA "([^"]*)"$`, func(sha string) error {
		return state.aValidDefectsWebhookPOSTIsSentForSHA(sha)
	})
	ctx.Step(`^the same defects payload is POSTed again with a valid signature$`, func() error {
		return state.theSameDefectsPayloadIsPOSTedAgainWithAValidSignature()
	})

	// Then – defects-v1-writes-no-metric
	ctx.Step(`^a scan_run row exists with kind "([^"]*)" and status "([^"]*)"$`, func(kind, status string) error {
		return state.aScanRunRowExistsWithKindAndStatus(kind, status)
	})
	ctx.Step(`^the metric_sample row count is unchanged$`, func() error {
		return state.theMetricSampleRowCountIsUnchanged()
	})
	ctx.Step(`^no metric_kind "([^"]*)" row exists for that scan_run$`, func(kind string) error {
		return state.noMetricKindRowExistsForThatScanRun(kind)
	})

	// Then – defects-idempotent
	ctx.Step(`^the response status code is 2xx and a scan_run_id is returned$`, func() error {
		return state.theResponseStatusCodeIs2xxAndAScanRunIDIsReturned()
	})
	ctx.Step(`^the same scan_run_id is returned$`, func() error {
		return state.theSameScanRunIDIsReturned()
	})
	ctx.Step(`^no second scan_run row is appended for that payload hash$`, func() error {
		return state.noSecondScanRunRowIsAppendedForThatPayloadHash()
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_external_metric_ingest_webhook_ingest_defects_verb_store_only(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")
	requireEnv(t, "CLEAN_CODE_WEBHOOK_HMAC_SECRET")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_external_metric_ingest_webhook_ingest_defects_verb_store_only,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"external_metric_ingest_webhook_ingest_defects_verb_store_only.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}