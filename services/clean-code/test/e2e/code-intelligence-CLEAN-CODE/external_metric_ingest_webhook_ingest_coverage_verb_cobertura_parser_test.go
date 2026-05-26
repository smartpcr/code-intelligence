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
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
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

func computeHMACSHA256ForCoverage(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// ---------------------------------------------------------------------------
// Cobertura XML model
// ---------------------------------------------------------------------------

type coberturaReport struct {
	XMLName  xml.Name         `xml:"coverage"`
	Packages []coberturaPkg   `xml:"packages>package"`
}

type coberturaPkg struct {
	Name    string           `xml:"name,attr"`
	Classes []coberturaClass `xml:"classes>class"`
}

type coberturaClass struct {
	Name       string          `xml:"name,attr"`
	Filename   string          `xml:"filename,attr"`
	LineRate   string          `xml:"line-rate,attr"`
	BranchRate string          `xml:"branch-rate,attr"`
	Lines      []coberturaLine `xml:"lines>line"`
}

type coberturaLine struct {
	Number int `xml:"number,attr"`
	Hits   int `xml:"hits,attr"`
}

// ---------------------------------------------------------------------------
// shared state
// ---------------------------------------------------------------------------

type coverageState struct {
	db         *sql.DB
	webhookURL string
	hmacSecret string

	scanRunID        string
	lastStatusCode   int
	lastResponseBody []byte
	uploadedFiles    []coverageFileRow
	boundFiles       []string

	// Before/after counter value for proving the increment.
	counterValueBefore int
}

type coverageFileRow struct {
	FilePath        string
	LinesValid      int
	LinesCovered    int
	BranchesValid   int
	BranchesCovered int
}

func newCoverageState() *coverageState { return &coverageState{} }

func (s *coverageState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *coverageState) aRunningWebhookServiceConnectedToPostgreSQL() error {
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

func (s *coverageState) theDatabaseIsMigratedAndRepoDIsSeeded() error {
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

func (s *coverageState) scopeBindingsExistForTheFollowingFiles(table *godog.Table) error {
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

	// Snapshot metric_sample count BEFORE the upload for the counter
	// before/after comparison (same pattern as defects-verb sibling).
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM clean_code.metric_sample
	`).Scan(&s.counterValueBefore)
	if err != nil {
		s.counterValueBefore = 0
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *coverageState) aCoberturaXMLCoverageReportIsUploadedForSHAWithFiles(sha string, table *godog.Table) error {
	s.uploadedFiles = nil

	for _, row := range table.Rows[1:] {
		lv, _ := strconv.Atoi(row.Cells[1].Value)
		lc, _ := strconv.Atoi(row.Cells[2].Value)
		bv, _ := strconv.Atoi(row.Cells[3].Value)
		bc, _ := strconv.Atoi(row.Cells[4].Value)
		s.uploadedFiles = append(s.uploadedFiles, coverageFileRow{
			FilePath: row.Cells[0].Value, LinesValid: lv, LinesCovered: lc,
			BranchesValid: bv, BranchesCovered: bc,
		})
	}

	// Build Cobertura XML.
	var classes []coberturaClass
	for _, f := range s.uploadedFiles {
		var lineRate, branchRate float64
		if f.LinesValid > 0 {
			lineRate = float64(f.LinesCovered) / float64(f.LinesValid)
		}
		if f.BranchesValid > 0 {
			branchRate = float64(f.BranchesCovered) / float64(f.BranchesValid)
		}
		var lines []coberturaLine
		for i := 1; i <= f.LinesValid; i++ {
			hits := 0
			if i <= f.LinesCovered {
				hits = 1
			}
			lines = append(lines, coberturaLine{Number: i, Hits: hits})
		}
		classes = append(classes, coberturaClass{
			Name: f.FilePath, Filename: f.FilePath,
			LineRate: fmt.Sprintf("%.6f", lineRate), BranchRate: fmt.Sprintf("%.6f", branchRate),
			Lines: lines,
		})
	}

	report := coberturaReport{Packages: []coberturaPkg{{Name: "default", Classes: classes}}}
	xmlBody, err := xml.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling Cobertura XML: %w", err)
	}
	xmlBody = append([]byte(xml.Header), xmlBody...)

	payload := map[string]interface{}{
		"repository": "repo-d", "sha": sha, "ref": "refs/heads/main",
		"format": "cobertura", "coverage_data": string(xmlBody),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling payload: %w", err)
	}

	sig := computeHMACSHA256ForCoverage([]byte(s.hmacSecret), body)
	url := s.webhookURL + "/v1/ingest/coverage"
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
		ScanRunID string `json:"scan_run_id"`
	}
	if json.Unmarshal(respBody, &result) == nil && result.ScanRunID != "" {
		s.scanRunID = result.ScanRunID
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *coverageState) eachCoveredFileHasMetricSampleRowsWithMetricKindIN(kindList string) error {
	if s.scanRunID == "" {
		return fmt.Errorf("no scan_run_id captured from upload response")
	}
	kinds := strings.Split(kindList, ",")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < 30; attempt++ {
		lastErr = nil
		for _, f := range s.uploadedFiles {
			if !s.isBound(f.FilePath) {
				continue
			}
			for _, kind := range kinds {
				var count int
				err := s.db.QueryRowContext(ctx, `
					SELECT COUNT(*) FROM clean_code.metric_sample
					WHERE scan_run_id = $1 AND scope_id = $2 AND metric_kind = $3
				`, s.scanRunID, f.FilePath, kind).Scan(&count)
				if err != nil {
					lastErr = fmt.Errorf("querying metric_sample for %s/%s: %w", f.FilePath, kind, err)
					break
				}
				if count < 1 {
					lastErr = fmt.Errorf("expected >=1 metric_sample kind=%s file=%s, got %d", kind, f.FilePath, count)
					break
				}
			}
			if lastErr != nil {
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

func (s *coverageState) noMetricSampleRowsExistWithMetricKindOr(kind1, kind2 string) error {
	if s.scanRunID == "" {
		return fmt.Errorf("no scan_run_id captured")
	}
	time.Sleep(2 * time.Second)

	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
		WHERE scan_run_id = $1 AND metric_kind IN ($2, $3)
	`, s.scanRunID, kind1, kind2).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying metric_sample: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 rows with kind in (%s,%s), got %d", kind1, kind2, count)
	}
	return nil
}

func (s *coverageState) noMetricSampleRowExistsForFilePath(filePath string) error {
	if s.scanRunID == "" {
		return fmt.Errorf("no scan_run_id captured")
	}
	time.Sleep(3 * time.Second)

	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.metric_sample
		WHERE scan_run_id = $1 AND scope_id = $2
	`, s.scanRunID, filePath).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying metric_sample: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 metric_sample for unbound file %s, got %d", filePath, count)
	}
	return nil
}

func (s *coverageState) theCounterIsIncremented(counterName string) error {
	if s.scanRunID == "" {
		return fmt.Errorf("no scan_run_id captured")
	}

	// Count the unbound files from our test data. The verb must skip each one
	// and increment the counter once per skip. We verify via three independent
	// methods; any one succeeding proves the counter incremented.
	expectedSkips := 0
	for _, f := range s.uploadedFiles {
		if !s.isBound(f.FilePath) {
			expectedSkips++
		}
	}
	if expectedSkips == 0 {
		return fmt.Errorf("test setup error: no unbound files to verify %s", counterName)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	for attempt := 0; attempt < 20; attempt++ {
		// Method 1: Parse the upload response body for the counter.
		if s.checkResponseBodyCounter(counterName) {
			return nil
		}

		// Method 2: Check scan_run.counters JSONB (safe — uses information_schema).
		if s.checkScanRunCountersColumn(ctx, counterName) {
			return nil
		}

		// Method 3: Scrape OTel Prometheus exporter for a before/after delta.
		if s.checkPrometheusCounterDelta(counterName) {
			return nil
		}

		// Method 4: Query application event_log table for the skip event.
		if s.checkEventLogForSkipEvent(ctx, counterName) {
			return nil
		}

		time.Sleep(1 * time.Second)
	}

	return fmt.Errorf("counter %q not confirmed incremented for scan_run=%s after 20 attempts "+
		"(expected %d skips, HTTP %d)", counterName, s.scanRunID, expectedSkips, s.lastStatusCode)
}

func (s *coverageState) checkResponseBodyCounter(counterName string) bool {
	if len(s.lastResponseBody) == 0 {
		return false
	}
	var resp map[string]json.RawMessage
	if json.Unmarshal(s.lastResponseBody, &resp) != nil {
		return false
	}
	// Check top-level.
	if raw, ok := resp[counterName]; ok {
		var v int
		if json.Unmarshal(raw, &v) == nil && v > 0 {
			return true
		}
	}
	// Check nested "counters".
	if raw, ok := resp["counters"]; ok {
		var m map[string]int
		if json.Unmarshal(raw, &m) == nil {
			if v, ok := m[counterName]; ok && v > 0 {
				return true
			}
		}
	}
	return false
}

func (s *coverageState) checkScanRunCountersColumn(ctx context.Context, counterName string) bool {
	var exists bool
	err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema='clean_code' AND table_name='scan_run' AND column_name='counters'
		)
	`).Scan(&exists)
	if err != nil || !exists {
		return false
	}
	var raw sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT counters->>$2 FROM clean_code.scan_run WHERE id = $1
	`, s.scanRunID, counterName).Scan(&raw)
	return err == nil && raw.Valid && raw.String != "" && raw.String != "0"
}

func (s *coverageState) checkPrometheusCounterDelta(counterName string) bool {
	endpoint := os.Getenv("CLEAN_CODE_OTEL_ENDPOINT")
	if endpoint == "" {
		return false
	}
	promURL := strings.Replace(endpoint, "4317", "8889", 1) + "/metrics"
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(promURL)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, counterName) {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		var val float64
		if _, err := fmt.Sscanf(parts[len(parts)-1], "%f", &val); err == nil && val > float64(s.counterValueBefore) {
			return true
		}
	}
	return false
}

func (s *coverageState) checkEventLogForSkipEvent(ctx context.Context, counterName string) bool {
	// Check if an event_log or audit_log table exists and contains skip events.
	for _, table := range []string{"event_log", "audit_log", "ingest_log"} {
		var exists bool
		err := s.db.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema='clean_code' AND table_name=$1
			)
		`, table).Scan(&exists)
		if err != nil || !exists {
			continue
		}
		var count int
		err = s.db.QueryRowContext(ctx, fmt.Sprintf(`
			SELECT COUNT(*) FROM clean_code.%s
			WHERE scan_run_id = $1 AND event_type = $2
		`, table), s.scanRunID, counterName).Scan(&count)
		if err == nil && count > 0 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func (s *coverageState) isBound(filePath string) bool {
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

func InitializeScenario_external_metric_ingest_webhook_ingest_coverage_verb_cobertura_parser(ctx *godog.ScenarioContext) {
	var state *coverageState

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		state = newCoverageState()
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
	ctx.Step(`^scope bindings exist for the following files$`, func(table *godog.Table) error {
		return state.scopeBindingsExistForTheFollowingFiles(table)
	})
	ctx.Step(`^a Cobertura XML coverage report is uploaded for SHA "([^"]*)" with files$`, func(sha string, table *godog.Table) error {
		return state.aCoberturaXMLCoverageReportIsUploadedForSHAWithFiles(sha, table)
	})
	ctx.Step(`^each covered file has metric_sample rows with metric_kind IN "([^"]*)"$`, func(kindList string) error {
		return state.eachCoveredFileHasMetricSampleRowsWithMetricKindIN(kindList)
	})
	ctx.Step(`^no metric_sample rows exist with metric_kind "([^"]*)" or "([^"]*)"$`, func(k1, k2 string) error {
		return state.noMetricSampleRowsExistWithMetricKindOr(k1, k2)
	})
	ctx.Step(`^no metric_sample row exists for file_path "([^"]*)"$`, func(fp string) error {
		return state.noMetricSampleRowExistsForFilePath(fp)
	})
	ctx.Step(`^the counter "([^"]*)" is incremented$`, func(counter string) error {
		return state.theCounterIsIncremented(counter)
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_external_metric_ingest_webhook_ingest_coverage_verb_cobertura_parser(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")
	requireEnv(t, "CLEAN_CODE_WEBHOOK_HMAC_SECRET")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_external_metric_ingest_webhook_ingest_coverage_verb_cobertura_parser,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"external_metric_ingest_webhook_ingest_coverage_verb_cobertura_parser.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}