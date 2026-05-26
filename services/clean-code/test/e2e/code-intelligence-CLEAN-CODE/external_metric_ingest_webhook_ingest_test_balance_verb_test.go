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

// ---------------------------------------------------------------------------
// shared test-balance state
// ---------------------------------------------------------------------------

type testBalanceState struct {
	db         *sql.DB
	webhookURL string
	hmacSecret string

	// Per-scenario tracking.
	scanRunID      string
	lastStatusCode int
	uploadedScopes []scopeRow
}

type scopeRow struct {
	ScopeID      string
	AttemptCount int
	PassCount    int
}

func newTestBalanceState() *testBalanceState {
	return &testBalanceState{}
}

func (s *testBalanceState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *testBalanceState) aRunningWebhookServiceConnectedToPostgreSQL() error {
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

func (s *testBalanceState) theDatabaseIsMigratedAndRepoDIsSeeded() error {
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
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *testBalanceState) aJSONTestBalancePayloadIsUploadedWithScopes(table *godog.Table) error {
	s.uploadedScopes = nil

	for _, row := range table.Rows[1:] { // skip header
		att, err := strconv.Atoi(row.Cells[1].Value)
		if err != nil {
			return fmt.Errorf("parsing attempt_count %q: %w", row.Cells[1].Value, err)
		}
		pc, err := strconv.Atoi(row.Cells[2].Value)
		if err != nil {
			return fmt.Errorf("parsing pass_count %q: %w", row.Cells[2].Value, err)
		}
		s.uploadedScopes = append(s.uploadedScopes, scopeRow{
			ScopeID:      row.Cells[0].Value,
			AttemptCount: att,
			PassCount:    pc,
		})
	}

	// Build the JSON payload.
	type entry struct {
		ScopeID      string `json:"scope_id"`
		AttemptCount int    `json:"attempt_count"`
		PassCount    int    `json:"pass_count"`
	}
	entries := make([]entry, len(s.uploadedScopes))
	for i, sc := range s.uploadedScopes {
		entries[i] = entry{ScopeID: sc.ScopeID, AttemptCount: sc.AttemptCount, PassCount: sc.PassCount}
	}

	payload := map[string]interface{}{
		"repository": "repo-d",
		"sha":        "dddd0001",
		"ref":        "refs/heads/main",
		"scopes":     entries,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}

	return s.doSignedIngestPost("/v1/ingest/test_balance", "application/json", body)
}

func (s *testBalanceState) aJUnitXMLBodyIsPOSTedTo(path string) error {
	xmlBody := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<testsuites><testsuite name="s" tests="1"><testcase name="tc1"/></testsuite></testsuites>`)

	return s.doSignedIngestPost(path, "application/xml", xmlBody)
}

func (s *testBalanceState) doSignedIngestPost(path, contentType string, body []byte) error {
	sig := computeHMACSHA256ForTestBalance([]byte(s.hmacSecret), body)

	url := s.webhookURL + path
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)

	client := &http.Client{Timeout: 15 * time.Second}
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
		s.scanRunID = result.ScanRunID
	}
	return nil
}

// computeHMACSHA256ForTestBalance returns the hex-encoded HMAC-SHA256.
// Unique name avoids collision with sibling stage helpers in the same package.
func computeHMACSHA256ForTestBalance(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *testBalanceState) eachScopeIDHasExactlyOneMetricSampleWithMetricKind(kind string) error {
	if s.scanRunID == "" {
		return fmt.Errorf("no scan_run_id captured from upload response")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Poll briefly to allow async processing to complete.
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		lastErr = nil
		for _, sc := range s.uploadedScopes {
			if sc.AttemptCount == 0 {
				continue // zero-attempt scopes are expected to produce no rows
			}
			var count int
			err := s.db.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM clean_code.metric_sample
				WHERE scan_run_id = $1
				  AND scope_id    = $2
				  AND metric_kind = $3
			`, s.scanRunID, sc.ScopeID, kind).Scan(&count)
			if err != nil {
				lastErr = fmt.Errorf("querying metric_sample for scope %s: %w", sc.ScopeID, err)
				break
			}
			if count != 1 {
				lastErr = fmt.Errorf("expected 1 metric_sample with kind=%s for scope=%s, got %d",
					kind, sc.ScopeID, count)
				break
			}
		}
		if lastErr == nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return lastErr
}

func (s *testBalanceState) noMetricSampleRowsExistWithMetricKindOr(kind1, kind2 string) error {
	if s.scanRunID == "" {
		return fmt.Errorf("no scan_run_id captured from upload response")
	}

	// Brief pause to ensure async processing had time to complete. This makes
	// the negative assertion robust in isolation (e.g. if scenario step order
	// changes) instead of relying on a preceding polling Then step to absorb
	// the webhook's async work. Mirrors noMetricSampleRowIsWrittenForScopeID.
	time.Sleep(2 * time.Second)

	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
		WHERE scan_run_id = $1
		  AND metric_kind IN ($2, $3)
	`, s.scanRunID, kind1, kind2).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying metric_sample: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 metric_sample rows with kind in (%s, %s), got %d", kind1, kind2, count)
	}
	return nil
}

func (s *testBalanceState) theResponseStatusCodeIsValue(expected int) error {
	if s.lastStatusCode != expected {
		return fmt.Errorf("expected HTTP %d, got %d", expected, s.lastStatusCode)
	}
	return nil
}

func (s *testBalanceState) theEmittedPassFirstTryRatioForIsBetweenZeroAndOne(scopeID string) error {
	if s.scanRunID == "" {
		return fmt.Errorf("no scan_run_id captured from upload response")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var value float64
	var found bool
	for attempt := 0; attempt < 10; attempt++ {
		err := s.db.QueryRowContext(ctx, `
			SELECT value FROM clean_code.metric_sample
			WHERE scan_run_id = $1
			  AND scope_id    = $2
			  AND metric_kind = 'pass_first_try_ratio'
		`, s.scanRunID, scopeID).Scan(&value)
		if err == nil {
			found = true
			break
		}
		if err != sql.ErrNoRows {
			return fmt.Errorf("querying metric_sample: %w", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if !found {
		return fmt.Errorf("no pass_first_try_ratio metric_sample found for scope=%s", scopeID)
	}
	if value < 0 || value > 1 {
		return fmt.Errorf("expected ratio in [0,1], got %f for scope=%s", value, scopeID)
	}
	return nil
}

func (s *testBalanceState) noMetricSampleRowIsWrittenForScopeID(scopeID string) error {
	if s.scanRunID == "" {
		return fmt.Errorf("no scan_run_id captured from upload response")
	}

	// Brief pause to ensure processing had time to complete.
	time.Sleep(2 * time.Second)

	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
		WHERE scan_run_id = $1
		  AND scope_id    = $2
	`, s.scanRunID, scopeID).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying metric_sample: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 metric_sample rows for scope=%s (attempt_count=0), got %d", scopeID, count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_external_metric_ingest_webhook_ingest_test_balance_verb
// registers all Given/When/Then steps for the ingest-test-balance-verb stage.
func InitializeScenario_external_metric_ingest_webhook_ingest_test_balance_verb(ctx *godog.ScenarioContext) {
	var state *testBalanceState

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		state = newTestBalanceState()
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
	ctx.Step(`^a JSON test_balance payload is uploaded with scopes$`, func(table *godog.Table) error {
		return state.aJSONTestBalancePayloadIsUploadedWithScopes(table)
	})
	ctx.Step(`^a JUnit-XML body is POSTed to "([^"]*)"$`, func(path string) error {
		return state.aJUnitXMLBodyIsPOSTedTo(path)
	})

	// Then
	ctx.Step(`^each scope_id has exactly one metric_sample with metric_kind "([^"]*)"$`, func(kind string) error {
		return state.eachScopeIDHasExactlyOneMetricSampleWithMetricKind(kind)
	})
	ctx.Step(`^no metric_sample rows exist with metric_kind "([^"]*)" or "([^"]*)"$`, func(k1, k2 string) error {
		return state.noMetricSampleRowsExistWithMetricKindOr(k1, k2)
	})
	ctx.Step(`^the response status code is (\d+)$`, func(code int) error {
		return state.theResponseStatusCodeIsValue(code)
	})
	ctx.Step(`^the emitted pass_first_try_ratio for "([^"]*)" is between 0 and 1$`, func(scopeID string) error {
		return state.theEmittedPassFirstTryRatioForIsBetweenZeroAndOne(scopeID)
	})
	ctx.Step(`^no metric_sample row is written for scope_id "([^"]*)"$`, func(scopeID string) error {
		return state.noMetricSampleRowIsWrittenForScopeID(scopeID)
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_external_metric_ingest_webhook_ingest_test_balance_verb(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")
	requireEnv(t, "CLEAN_CODE_WEBHOOK_HMAC_SECRET")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_external_metric_ingest_webhook_ingest_test_balance_verb,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"external_metric_ingest_webhook_ingest_test_balance_verb.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}
