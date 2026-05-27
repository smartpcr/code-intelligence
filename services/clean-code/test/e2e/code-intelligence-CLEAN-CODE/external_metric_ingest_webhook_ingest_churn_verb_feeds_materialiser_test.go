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

// computeHMACSHA256Churn returns the hex-encoded HMAC-SHA256 of body using secret.
// One copy per package; the defects file holds the canonical helper used by other
// scenarios. Naming is per-feature so the linker does not collide.
func computeHMACSHA256Churn(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// sha256HexChurn returns the hex-encoded SHA-256 digest of data.
func sha256HexChurn(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// ---------------------------------------------------------------------------
// shared state for churn-verb-feeds-materialiser scenarios
// ---------------------------------------------------------------------------

// churnVerbState holds the per-scenario state shared across
// Given / When / Then steps for the churn feature. Mirrors the
// shape of [defectsVerbState] so operators see the same triage
// fields when a scenario fails (db handle, payload + hash,
// last response, last scan_run_id).
type churnVerbState struct {
	db           *sql.DB
	webhookURL   string
	hmacSecret   string
	signingKeyID string
	payload      []byte
	payloadHash  string

	metricSampleCountBefore int
	churnEventCountBefore   int
	lastStatusCode          int
	firstScanRunID          string
	lastScanRunID           string
	currentScanRunID        string
}

func newChurnVerbState() *churnVerbState {
	return &churnVerbState{}
}

func (s *churnVerbState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *churnVerbState) aRunningWebhookServiceConnectedToPostgreSQLChurn() error {
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
	// Stage-6 secret-resolver Router requires a non-empty
	// signing_key_id on every POST. The CI workflow exports
	// `CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID` (and
	// `CLEAN_CODE_ENABLE_EXTERNAL_INGEST_WEBHOOK=true`)
	// BEFORE `docker compose up` so the binary mounts the
	// production Router via `mountIngestRouter`
	// (`cmd/clean-code-metric-ingestor/main.go:577-621`).
	// The same env var is also passed through the compose
	// `webhook` service env block so the binary's
	// `StaticSecretResolver` is seeded with the
	// (key_id, secret) pair the test signs with.
	s.signingKeyID = os.Getenv("CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID")
	if s.signingKeyID == "" {
		return fmt.Errorf("CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID is not set (required by the production external-ingest Router; see cmd/clean-code-metric-ingestor/main.go:584-586)")
	}
	return nil
}

func (s *churnVerbState) theDatabaseIsMigratedAndRepoDIsSeededChurn() error {
	// Canonical repo modes per architecture Sec 5.1.1 line
	// 852 and the `clean_code.repo_mode` ENUM in
	// `migrations/0001_catalog_lifecycle.up.sql:75-78`:
	// `embedded` (default) or `linked`. The seed inserts the
	// repo-d row using one of these two values; matching on
	// the closed enum set (rather than a stale `'external'`
	// literal) survives a seed-mode swap and aligns with
	// `internal/management/repo_store.go:66-74`.
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.repo
		WHERE mode IN ('embedded', 'linked')
	`).Scan(&count)
	if err != nil {
		return fmt.Errorf("checking repo-d seed: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("repo-d seed data not found; run 'make seed-repo-d' first")
	}

	// Snapshot metric_sample row count before the upload so the
	// "unchanged" assertion can compare AFTER the verb runs.
	err = s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
	`).Scan(&s.metricSampleCountBefore)
	if err != nil {
		return fmt.Errorf("counting metric_sample rows: %w", err)
	}

	// Snapshot churn_event row count so the "one or more
	// appended" assertion can derive the delta.
	err = s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.churn_event
	`).Scan(&s.churnEventCountBefore)
	if err != nil {
		return fmt.Errorf("counting churn_event rows: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *churnVerbState) aValidChurnWebhookPOSTIsSentForSHA(sha string) error {
	// Canonical churn payload shape per [churn.Payload] in
	// `internal/ingest/churn/churn.go:176-187`: `{repo_id,
	// rows:[{sha, file_path, modified_at[, author]}]}`. The
	// verb's decoder runs `DisallowUnknownFields`
	// (`churn_verb.go:199-205`), so the wire test MUST match
	// the canonical field set exactly -- any drift (e.g. the
	// brief-draft `kind` / `window_days` / `files` fields)
	// triggers a 400 before any churn_event row is written.
	//
	// The scenario's `sha` argument becomes the per-row sha
	// the materialiser dedupes by (architecture Sec 4.4 line
	// 781: "each row has its own SHA").
	modifiedAt := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	payload := map[string]interface{}{
		"repo_id": "11111111-2222-3333-4444-555555555555",
		"rows": []map[string]interface{}{
			{
				"sha":         sha,
				"file_path":   "internal/foo.go",
				"modified_at": modifiedAt,
			},
			{
				"sha":         sha,
				"file_path":   "internal/bar.go",
				"modified_at": modifiedAt,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}
	s.payload = body
	s.payloadHash = sha256HexChurn(body)

	// Wipe prior scan_run rows for this exact payload hash so
	// the idempotency test sees a clean slate.
	_, _ = s.db.ExecContext(context.Background(),
		`DELETE FROM clean_code.scan_run WHERE payload_hash = $1`, s.payloadHash)

	return s.doSignedPostChurn(body)
}

func (s *churnVerbState) doSignedPostChurn(body []byte) error {
	sig := computeHMACSHA256Churn([]byte(s.hmacSecret), body)

	url := s.webhookURL + "/v1/ingest/churn"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hub-Signature-256", "sha256="+sig)
	// Stage-6 secret-resolver Router REQUIRES the canonical
	// `X-Signing-Key-Id` header (=
	// `webhook.SigningKeyIDHeader` at
	// `internal/ingest/webhook/secret_resolver.go:34`); a
	// missing or empty value triggers a 401 +
	// `HMAC_MISSING_KEY_ID` before the body is HMAC-verified
	// (router_test.go:557-575 pins the order). The CI
	// workflow exports
	// `CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID` before compose-up
	// and the binary's `StaticSecretResolver` is seeded with
	// the (key_id, secret) pair via
	// `cmd/clean-code-metric-ingestor/main.go:618-621`.
	req.Header.Set("X-Signing-Key-Id", s.signingKeyID)

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	s.lastStatusCode = resp.StatusCode

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response body: %w", err)
	}

	var result struct {
		ScanRunID string `json:"scan_run_id"`
	}
	if err := json.Unmarshal(respBody, &result); err == nil && result.ScanRunID != "" {
		if s.firstScanRunID == "" {
			s.firstScanRunID = result.ScanRunID
		}
		s.lastScanRunID = result.ScanRunID
		s.currentScanRunID = result.ScanRunID
	}
	return nil
}

func (s *churnVerbState) theSameChurnPayloadIsPOSTedAgainWithAValidSignature() error {
	return s.doSignedPostChurn(s.payload)
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *churnVerbState) aScanRunRowExistsWithKindAndStatus(kind, status string) error {
	var got struct {
		Kind   string
		Status string
	}
	err := s.db.QueryRowContext(context.Background(), `
		SELECT kind, status FROM clean_code.scan_run
		WHERE payload_hash = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, s.payloadHash).Scan(&got.Kind, &got.Status)
	if err != nil {
		return fmt.Errorf("looking up scan_run: %w", err)
	}
	if got.Kind != kind {
		return fmt.Errorf("scan_run.kind=%q; want %q", got.Kind, kind)
	}
	if got.Status != status {
		return fmt.Errorf("scan_run.status=%q; want %q", got.Status, status)
	}
	return nil
}

func (s *churnVerbState) theMetricSampleRowCountIsUnchanged() error {
	var after int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
	`).Scan(&after)
	if err != nil {
		return fmt.Errorf("counting metric_sample rows: %w", err)
	}
	if after != s.metricSampleCountBefore {
		return fmt.Errorf("metric_sample count=%d (was %d); churn verb MUST NOT write metric_sample rows directly",
			after, s.metricSampleCountBefore)
	}
	return nil
}

func (s *churnVerbState) oneOrMoreChurnEventRowsExistForThatScanRun() error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.churn_event
		WHERE scan_run_id = $1
	`, s.currentScanRunID).Scan(&count)
	if err != nil {
		return fmt.Errorf("counting churn_event rows: %w", err)
	}
	if count == 0 {
		return fmt.Errorf("churn_event count for scan_run_id=%s is 0; want >0", s.currentScanRunID)
	}
	return nil
}

func (s *churnVerbState) theResponseStatusCodeIs2xxAndAScanRunIDIsReturned() error {
	if s.lastStatusCode < 200 || s.lastStatusCode >= 300 {
		return fmt.Errorf("status=%d; want 2xx", s.lastStatusCode)
	}
	if s.lastScanRunID == "" {
		return fmt.Errorf("no scan_run_id in response body")
	}
	return nil
}

func (s *churnVerbState) theSameScanRunIDIsReturned() error {
	if s.firstScanRunID == "" || s.lastScanRunID == "" {
		return fmt.Errorf("missing scan_run_id state: first=%q last=%q",
			s.firstScanRunID, s.lastScanRunID)
	}
	if s.firstScanRunID != s.lastScanRunID {
		return fmt.Errorf("scan_run_id changed across replay: first=%s last=%s",
			s.firstScanRunID, s.lastScanRunID)
	}
	return nil
}

func (s *churnVerbState) noSecondScanRunRowIsAppendedForThatPayloadHash() error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.scan_run
		WHERE payload_hash = $1
	`, s.payloadHash).Scan(&count)
	if err != nil {
		return fmt.Errorf("counting scan_run rows for payload_hash: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("scan_run count for payload_hash=%s is %d; want 1 (idempotent replay)",
			s.payloadHash, count)
	}
	return nil
}

func (s *churnVerbState) noDuplicateChurnEventRowsAreAppended() error {
	var dups int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM (
			SELECT file_path, sha, COUNT(*) AS n
			FROM clean_code.churn_event
			WHERE scan_run_id = $1
			GROUP BY file_path, sha
			HAVING COUNT(*) > 1
		) sub
	`, s.currentScanRunID).Scan(&dups)
	if err != nil {
		return fmt.Errorf("counting duplicate churn_event rows: %w", err)
	}
	if dups != 0 {
		return fmt.Errorf("found %d duplicate (file_path, sha) groupings in churn_event; want 0", dups)
	}
	return nil
}

// theStagedChurnEventRowsCarryTheMaterialiserShape asserts the
// staged `churn_event` rows have the schema the
// `modification_count_in_window` materialiser depends on:
// each row carries a non-empty (repo_id, sha, file_path,
// modified_at). This is the contract handoff between the
// churn verb (this workstream) and the future
// modification-count sweeper (a separate workstream). When the
// sweeper lands it will SELECT exactly these four columns.
func (s *churnVerbState) theStagedChurnEventRowsCarryTheMaterialiserShape() error {
	var orphans int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.churn_event
		WHERE scan_run_id = $1
		  AND (repo_id IS NULL
		    OR sha IS NULL OR sha = ''
		    OR file_path IS NULL OR file_path = ''
		    OR modified_at IS NULL)
	`, s.currentScanRunID).Scan(&orphans)
	if err != nil {
		return fmt.Errorf("checking churn_event materialiser-shape: %w", err)
	}
	if orphans != 0 {
		return fmt.Errorf("found %d churn_event rows missing required materialiser fields (repo_id/sha/file_path/modified_at)", orphans)
	}
	return nil
}

// ---------------------------------------------------------------------------
// godog wiring
// ---------------------------------------------------------------------------

// TestE2E_external_metric_ingest_webhook_ingest_churn_verb_feeds_materialiser
// is the entry point for the
// `external_metric_ingest_webhook_ingest_churn_verb_feeds_materialiser`
// e2e feature. The name follows the closed
// `TestE2E_<feature>` convention every other e2e in this
// directory uses, so the CI workflows can invoke it via
// `-run TestE2E_external_metric_ingest_webhook_ingest_churn_verb_feeds_materialiser`.
//
// Compose-up requires (set by the CI workflows BEFORE
// `docker compose up`):
//
//   - CLEAN_CODE_PG_URL                       = `postgres://...` to the e2e DB
//   - CLEAN_CODE_WEBHOOK_URL                  = base URL of the metric-ingestor (default :8084)
//   - CLEAN_CODE_WEBHOOK_HMAC_SECRET          = the shared HMAC secret matching the binary's config
//   - CLEAN_CODE_ENABLE_EXTERNAL_INGEST_WEBHOOK = "true" so the binary mounts the Stage-6 Router
//   - CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID       = the publisher key id (sent as `X-Signing-Key-Id`)
//
// The phase-04 compose `webhook` service passes both new
// env vars through to the container (`${VAR:-}` default
// keeps backward-compat for sibling jobs that do NOT mount
// the production Router).
//
// When any of these is missing the test skips (or fails
// in setup with a clear error), mirroring the
// other feature tests in this directory.
func TestE2E_external_metric_ingest_webhook_ingest_churn_verb_feeds_materialiser(t *testing.T) {
	if os.Getenv("CLEAN_CODE_PG_URL") == "" {
		t.Skip("CLEAN_CODE_PG_URL is not set; skipping churn-verb e2e")
	}
	state := newChurnVerbState()
	t.Cleanup(state.close)

	suite := godog.TestSuite{
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			sc.Step(`^a running webhook service connected to PostgreSQL$`, state.aRunningWebhookServiceConnectedToPostgreSQLChurn)
			sc.Step(`^the database is migrated and repo-d is seeded$`, state.theDatabaseIsMigratedAndRepoDIsSeededChurn)
			sc.Step(`^a valid churn webhook POST is sent for SHA "([^"]+)"$`, state.aValidChurnWebhookPOSTIsSentForSHA)
			sc.Step(`^a scan_run row exists with kind "([^"]+)" and status "([^"]+)"$`, state.aScanRunRowExistsWithKindAndStatus)
			sc.Step(`^the metric_sample row count is unchanged$`, state.theMetricSampleRowCountIsUnchanged)
			sc.Step(`^one or more churn_event rows exist for that scan_run$`, state.oneOrMoreChurnEventRowsExistForThatScanRun)
			sc.Step(`^the staged churn_event rows carry the materialiser shape$`, state.theStagedChurnEventRowsCarryTheMaterialiserShape)
			sc.Step(`^the response status code is 2xx and a scan_run_id is returned$`, state.theResponseStatusCodeIs2xxAndAScanRunIDIsReturned)
			sc.Step(`^the same churn payload is POSTed again with a valid signature$`, state.theSameChurnPayloadIsPOSTedAgainWithAValidSignature)
			sc.Step(`^the same scan_run_id is returned$`, state.theSameScanRunIDIsReturned)
			sc.Step(`^no second scan_run row is appended for that payload hash$`, state.noSecondScanRunRowIsAppendedForThatPayloadHash)
			sc.Step(`^no duplicate churn_event rows are appended for the same \(file_path, sha\)$`, state.noDuplicateChurnEventRowsAreAppended)
		},
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"external_metric_ingest_webhook_ingest_churn_verb_feeds_materialiser.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero godog suite status; see scenario output above")
	}
}
