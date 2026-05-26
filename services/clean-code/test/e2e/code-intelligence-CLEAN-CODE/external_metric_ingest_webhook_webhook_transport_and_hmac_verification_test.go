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

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

// computeHMACSHA256 returns the hex-encoded HMAC-SHA256 of body using secret.
func computeHMACSHA256(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// sha256Hex returns the hex-encoded SHA-256 digest of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// shared webhook state
// ---------------------------------------------------------------------------

type webhookHMACState struct {
	db          *sql.DB
	webhookURL  string
	hmacSecret  string
	payload     []byte
	payloadHash string

	lastStatusCode int
	firstScanRunID string
	lastScanRunID  string
}

func newWebhookHMACState() *webhookHMACState {
	return &webhookHMACState{}
}

func (s *webhookHMACState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *webhookHMACState) aRunningWebhookServiceConnectedToPostgreSQL() error {
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

func (s *webhookHMACState) theDatabaseIsMigratedAndRepoDIsSeeded() error {
	// Verify repo-d seed data exists (created by `make seed-repo-d`).
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
// When/Then steps – invalid-signature-rejected
// ---------------------------------------------------------------------------

func (s *webhookHMACState) aWebhookPOSTIsSentWithAnInvalidHMACHeader() error {
	payload := map[string]interface{}{
		"repository": "repo-d",
		"sha":        "dddd0001",
		"ref":        "refs/heads/main",
		"metrics":    []interface{}{},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}
	s.payload = body
	s.payloadHash = sha256Hex(body)

	// Deliberately compute a wrong signature.
	badSig := computeHMACSHA256([]byte("wrong-secret-value"), body)

	url := s.webhookURL + "/v1/webhook/metrics"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+badSig)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()
	io.ReadAll(resp.Body)

	s.lastStatusCode = resp.StatusCode
	return nil
}

func (s *webhookHMACState) theResponseStatusCodeIs(expected int) error {
	if s.lastStatusCode != expected {
		return fmt.Errorf("expected HTTP %d, got %d", expected, s.lastStatusCode)
	}
	return nil
}

func (s *webhookHMACState) noScanRunRowExistsForThatPayload() error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.scan_run
		WHERE payload_hash = $1
	`, s.payloadHash).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying scan_run: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 scan_run rows for payload_hash=%s, got %d", s.payloadHash, count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// When/Then steps – replay-returns-cached-scan-run
// ---------------------------------------------------------------------------

func (s *webhookHMACState) aValidWebhookPOSTIsSentForSHA(sha string) error {
	payload := map[string]interface{}{
		"repository": "repo-d",
		"sha":        sha,
		"ref":        "refs/heads/main",
		"metrics":    []interface{}{},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}
	s.payload = body
	s.payloadHash = sha256Hex(body)

	// Clean any previous test state for this payload hash.
	_, _ = s.db.ExecContext(context.Background(),
		`DELETE FROM clean_code.scan_run WHERE payload_hash = $1`, s.payloadHash)

	return s.doSignedPost(body)
}

func (s *webhookHMACState) doSignedPost(body []byte) error {
	sig := computeHMACSHA256([]byte(s.hmacSecret), body)

	url := s.webhookURL + "/v1/webhook/metrics"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	s.lastStatusCode = resp.StatusCode

	// Extract scan_run_id from response JSON.
	var result struct {
		ScanRunID string `json:"scan_run_id"`
	}
	if err := json.Unmarshal(respBody, &result); err == nil && result.ScanRunID != "" {
		s.lastScanRunID = result.ScanRunID
	}
	return nil
}

func (s *webhookHMACState) theResponseStatusCodeIs2xxAndAScanRunIDIsReturned() error {
	if s.lastStatusCode < 200 || s.lastStatusCode >= 300 {
		return fmt.Errorf("expected 2xx, got %d", s.lastStatusCode)
	}
	if s.lastScanRunID == "" {
		return fmt.Errorf("expected a scan_run_id in response body, got none")
	}
	s.firstScanRunID = s.lastScanRunID
	return nil
}

func (s *webhookHMACState) theSamePayloadBodyIsPOSTedAgainWithAValidSignature() error {
	s.lastScanRunID = ""
	return s.doSignedPost(s.payload)
}

func (s *webhookHMACState) theResponseReturnsTheOriginalScanRunID() error {
	if s.lastScanRunID == "" {
		return fmt.Errorf("no scan_run_id returned on replay")
	}
	if s.lastScanRunID != s.firstScanRunID {
		return fmt.Errorf("expected scan_run_id=%q (original), got %q", s.firstScanRunID, s.lastScanRunID)
	}
	return nil
}

func (s *webhookHMACState) onlyOneScanRunRowExistsForThatPayloadHash() error {
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
		return fmt.Errorf("expected exactly 1 scan_run row for payload_hash=%s, got %d", s.payloadHash, count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_external_metric_ingest_webhook_webhook_transport_and_hmac_verification
// registers all Given/When/Then steps for the webhook-transport-and-hmac-verification stage.
func InitializeScenario_external_metric_ingest_webhook_webhook_transport_and_hmac_verification(ctx *godog.ScenarioContext) {
	var state *webhookHMACState

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		state = newWebhookHMACState()
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

	// When/Then – invalid-signature-rejected
	ctx.Step(`^a webhook POST is sent with an invalid HMAC header$`, func() error {
		return state.aWebhookPOSTIsSentWithAnInvalidHMACHeader()
	})
	ctx.Step(`^the response status code is (\d+)$`, func(code int) error {
		return state.theResponseStatusCodeIs(code)
	})
	ctx.Step(`^no scan_run row exists for that payload$`, func() error {
		return state.noScanRunRowExistsForThatPayload()
	})

	// When/Then – replay-returns-cached-scan-run
	ctx.Step(`^a valid webhook POST is sent for SHA "([^"]*)"$`, func(sha string) error {
		return state.aValidWebhookPOSTIsSentForSHA(sha)
	})
	ctx.Step(`^the response status code is 2xx and a scan_run_id is returned$`, func() error {
		return state.theResponseStatusCodeIs2xxAndAScanRunIDIsReturned()
	})
	ctx.Step(`^the same payload body is POSTed again with a valid signature$`, func() error {
		return state.theSamePayloadBodyIsPOSTedAgainWithAValidSignature()
	})
	ctx.Step(`^the response returns the original scan_run_id$`, func() error {
		return state.theResponseReturnsTheOriginalScanRunID()
	})
	ctx.Step(`^only one scan_run row exists for that payload hash$`, func() error {
		return state.onlyOneScanRunRowExistsForThatPayloadHash()
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_external_metric_ingest_webhook_webhook_transport_and_hmac_verification(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")
	requireEnv(t, "CLEAN_CODE_WEBHOOK_HMAC_SECRET")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_external_metric_ingest_webhook_webhook_transport_and_hmac_verification,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"external_metric_ingest_webhook_webhook_transport_and_hmac_verification.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}