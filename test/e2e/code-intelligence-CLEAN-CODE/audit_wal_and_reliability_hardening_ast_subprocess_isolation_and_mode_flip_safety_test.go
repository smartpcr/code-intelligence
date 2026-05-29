//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set – skipping e2e test", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// typed error response from the evaluator service
// ---------------------------------------------------------------------------

// parseErrorResponse models the structured error returned by the evaluator's
// /api/v1/parser/parse endpoint when the subprocess fails.
type parseErrorResponse struct {
	ErrorType string `json:"error_type"` // e.g. "ParserOOM", "ParserTimeout"
	Message   string `json:"message"`
	ExitCode  int    `json:"exit_code"`
}

// ---------------------------------------------------------------------------
// shared state for the scenarios
// ---------------------------------------------------------------------------

type astSubprocessState struct {
	pgDSN         string
	otelEndpoint  string
	db            *sql.DB
	httpClient    *http.Client
	evaluatorURL  string
	ruleEngineURL string

	// subprocess-oom scenario
	parseHTTPStatus int
	parsedError     *parseErrorResponse
	hostHealthy     bool

	// mode-flip scenario
	repoID          string
	oldMode         string // captured before the flip
	inflightScanIDs []string
	scanResults     []scanResult
	nextScanMode    string

	mu sync.Mutex
}

type scanResult struct {
	ScanID string `json:"scan_id"`
	Mode   string `json:"mode"`
	Status string `json:"status"`
}

func newAstSubprocessState() *astSubprocessState {
	return &astSubprocessState{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// ---------------------------------------------------------------------------
// Scenario: subprocess-oom-returns-error
// ---------------------------------------------------------------------------

func (s *astSubprocessState) aParserSubprocessThatAllocatesBeyondItsRlimit(ctx context.Context) error {
	s.pgDSN = os.Getenv("CLEAN_CODE_PG_URL")
	if s.pgDSN == "" {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	s.evaluatorURL = os.Getenv("EVALUATOR_URL")
	if s.evaluatorURL == "" {
		s.evaluatorURL = "http://localhost:8090"
	}

	db, err := sql.Open("postgres", s.pgDSN)
	if err != nil {
		return fmt.Errorf("connecting to postgres: %w", err)
	}
	s.db = db

	// Configure the parser subprocess with a very low memory rlimit so the
	// next parse will exceed it and be killed with SIGKILL / OOM.
	payload := map[string]interface{}{
		"action":       "configure_rlimit",
		"memory_bytes": 1024, // artificially tiny
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.evaluatorURL+"/api/v1/parser/configure", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building configure request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending configure request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("configure request returned %d", resp.StatusCode)
	}
	return nil
}

func (s *astSubprocessState) theParseRuns(ctx context.Context) error {
	payload := map[string]interface{}{
		"action": "parse",
		"source": "// intentionally large allocation trigger\npackage oom\n",
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.evaluatorURL+"/api/v1/parser/parse", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building parse request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// A network-level error (host crashed) is captured; we still want
		// to attempt the health-check in the Then step.
		s.parsedError = &parseErrorResponse{
			ErrorType: "NetworkError",
			Message:   err.Error(),
		}
		return nil
	}
	defer resp.Body.Close()
	s.parseHTTPStatus = resp.StatusCode

	var errResp parseErrorResponse
	if decErr := json.NewDecoder(resp.Body).Decode(&errResp); decErr != nil {
		return fmt.Errorf("decoding parse response: %w", decErr)
	}
	s.parsedError = &errResp
	return nil
}

func (s *astSubprocessState) theHostProcessReceivesATypedErrorAndTheHostRemainsRunning(
	ctx context.Context, expectedErrorType string,
) error {
	// 1. Verify we received a structured error with the exact typed error_type.
	if s.parsedError == nil {
		return fmt.Errorf("expected a typed error response but got none")
	}
	if s.parsedError.ErrorType != expectedErrorType {
		return fmt.Errorf(
			"expected error_type=%q but got error_type=%q (message: %s)",
			expectedErrorType, s.parsedError.ErrorType, s.parsedError.Message,
		)
	}

	// 2. Verify the host is still alive (no panic/crash) via the health endpoint.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		s.evaluatorURL+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("building health request: %w", err)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("host health check failed — host may have crashed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("host health check returned HTTP %d", resp.StatusCode)
	}
	s.hostHealthy = true
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: mode-flip-drains-scans
// ---------------------------------------------------------------------------

func (s *astSubprocessState) twoInFlightScansForRepoID(ctx context.Context, repoID string) error {
	s.repoID = repoID
	s.ruleEngineURL = os.Getenv("RULE_ENGINE_URL")
	if s.ruleEngineURL == "" {
		s.ruleEngineURL = "http://localhost:8091"
	}
	s.pgDSN = os.Getenv("CLEAN_CODE_PG_URL")
	if s.pgDSN == "" {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	if s.db == nil {
		db, err := sql.Open("postgres", s.pgDSN)
		if err != nil {
			return fmt.Errorf("connecting to postgres: %w", err)
		}
		s.db = db
	}

	// Start two scans concurrently.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var startErrors []error

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			scanID := fmt.Sprintf("scan-%s-%d", repoID, idx)
			payload := map[string]interface{}{
				"repo_id": repoID,
				"scan_id": scanID,
			}
			body, _ := json.Marshal(payload)
			req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost,
				s.ruleEngineURL+"/api/v1/scans/start", bytes.NewReader(body))
			if reqErr != nil {
				mu.Lock()
				startErrors = append(startErrors, reqErr)
				mu.Unlock()
				return
			}
			req.Header.Set("Content-Type", "application/json")

			resp, doErr := s.httpClient.Do(req)
			if doErr != nil {
				mu.Lock()
				startErrors = append(startErrors, doErr)
				mu.Unlock()
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				mu.Lock()
				startErrors = append(startErrors, fmt.Errorf("scan start returned %d", resp.StatusCode))
				mu.Unlock()
				return
			}
			mu.Lock()
			s.inflightScanIDs = append(s.inflightScanIDs, scanID)
			mu.Unlock()
		}(i)
	}
	wg.Wait()

	if len(startErrors) > 0 {
		return fmt.Errorf("errors starting scans: %v", startErrors)
	}
	if len(s.inflightScanIDs) != 2 {
		return fmt.Errorf("expected 2 scans started, got %d", len(s.inflightScanIDs))
	}

	// Confirm both scans are actually in-flight (status == "running") and
	// capture the old mode so we can assert against it later.
	for _, scanID := range s.inflightScanIDs {
		sr, err := s.fetchScan(ctx, scanID)
		if err != nil {
			return fmt.Errorf("verifying in-flight status for %s: %w", scanID, err)
		}
		if sr.Status != "running" {
			return fmt.Errorf("scan %s is not in-flight (status=%q); expected running", scanID, sr.Status)
		}
		if s.oldMode == "" {
			s.oldMode = sr.Mode
		}
	}
	if s.oldMode == "" {
		return fmt.Errorf("could not determine old mode from in-flight scans")
	}
	return nil
}

func (s *astSubprocessState) mgmtSetModeIsCalledWithRepoIDAndMode(
	ctx context.Context, repoID, mode string,
) error {
	// Re-confirm scans are still in-flight at the moment we flip the mode.
	for _, scanID := range s.inflightScanIDs {
		sr, err := s.fetchScan(ctx, scanID)
		if err != nil {
			return fmt.Errorf("re-checking in-flight status for %s: %w", scanID, err)
		}
		if sr.Status != "running" {
			return fmt.Errorf(
				"scan %s finished before set_mode was called (status=%q); "+
					"test cannot prove drain behaviour", scanID, sr.Status)
		}
	}

	payload := map[string]interface{}{
		"repo_id": repoID,
		"mode":    mode,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.ruleEngineURL+"/api/v1/mgmt/set_mode", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building set_mode request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending set_mode request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("set_mode returned %d", resp.StatusCode)
	}
	return nil
}

func (s *astSubprocessState) bothScansCompleteUnderTheOldModeAndTheNextScanStartsUnder(
	ctx context.Context, expectedNewMode string,
) error {
	// Poll until both in-flight scans reach "completed" (max 60s).
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		allDone := true
		s.scanResults = nil
		for _, scanID := range s.inflightScanIDs {
			sr, err := s.fetchScan(ctx, scanID)
			if err != nil {
				return err
			}
			s.scanResults = append(s.scanResults, sr)
			if sr.Status != "completed" {
				allDone = false
			}
		}
		if allDone {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// Assert each completed scan ran under the OLD mode (captured in Given).
	for _, sr := range s.scanResults {
		if sr.Status != "completed" {
			return fmt.Errorf("scan %s did not complete within 60s (status=%s)", sr.ScanID, sr.Status)
		}
		if sr.Mode != s.oldMode {
			return fmt.Errorf(
				"scan %s completed under mode %q, expected old mode %q",
				sr.ScanID, sr.Mode, s.oldMode)
		}
		if sr.Mode == expectedNewMode {
			return fmt.Errorf(
				"scan %s completed under the NEW mode %q — drain did not work",
				sr.ScanID, expectedNewMode)
		}
	}

	// Start a post-flip scan and verify it picks up the new mode.
	postFlipID := fmt.Sprintf("scan-%s-post-flip", s.repoID)
	payload := map[string]interface{}{
		"repo_id": s.repoID,
		"scan_id": postFlipID,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.ruleEngineURL+"/api/v1/scans/start", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()

	// Poll until the post-flip scan has a mode assigned.
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		sr, err := s.fetchScan(ctx, postFlipID)
		if err != nil {
			return err
		}
		if sr.Mode != "" {
			s.nextScanMode = sr.Mode
			break
		}
		time.Sleep(2 * time.Second)
	}

	if s.nextScanMode != expectedNewMode {
		return fmt.Errorf("expected next scan mode %q, got %q", expectedNewMode, s.nextScanMode)
	}
	return nil
}

// fetchScan retrieves the current state of a scan from the rule-engine API.
func (s *astSubprocessState) fetchScan(ctx context.Context, scanID string) (scanResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/api/v1/scans/%s", s.ruleEngineURL, scanID), nil)
	if err != nil {
		return scanResult{}, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return scanResult{}, err
	}
	defer resp.Body.Close()
	var sr scanResult
	if decErr := json.NewDecoder(resp.Body).Decode(&sr); decErr != nil {
		return scanResult{}, decErr
	}
	return sr, nil
}

// ---------------------------------------------------------------------------
// godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_audit_wal_and_reliability_hardening_ast_subprocess_isolation_and_mode_flip_safety(ctx *godog.ScenarioContext) {
	s := newAstSubprocessState()

	// Close the database connection after each scenario to avoid leaking
	// connections across scenarios in CI.
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if s.db != nil {
			s.db.Close()
			s.db = nil
		}
		return ctx, nil
	})

	// subprocess-oom-returns-error
	ctx.Step(`^a parser subprocess that allocates beyond its rlimit$`,
		s.aParserSubprocessThatAllocatesBeyondItsRlimit)
	ctx.Step(`^the parse runs$`,
		s.theParseRuns)
	ctx.Step(`^the host process receives a typed "([^"]*)" error and the host remains running$`,
		s.theHostProcessReceivesATypedErrorAndTheHostRemainsRunning)

	// mode-flip-drains-scans
	ctx.Step(`^two in-flight scans for repo_id "([^"]*)"$`,
		s.twoInFlightScansForRepoID)
	ctx.Step(`^mgmt set_mode is called with repo_id "([^"]*)" and mode "([^"]*)"$`,
		s.mgmtSetModeIsCalledWithRepoIDAndMode)
	ctx.Step(`^both scans complete under the old mode and the next scan starts under "([^"]*)"$`,
		s.bothScansCompleteUnderTheOldModeAndTheNextScanStartsUnder)
}

func TestE2E_audit_wal_and_reliability_hardening_ast_subprocess_isolation_and_mode_flip_safety(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_audit_wal_and_reliability_hardening_ast_subprocess_isolation_and_mode_flip_safety,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"audit_wal_and_reliability_hardening_ast_subprocess_isolation_and_mode_flip_safety.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run e2e tests")
	}
}
