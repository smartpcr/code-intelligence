//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
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
// shared state for metric-ingestor-and-scanrun-state-machine scenarios
// ---------------------------------------------------------------------------

type metricIngestorState struct {
	db          *sql.DB
	ingestorURL string
	commitSHA   string
	repoID      string

	// mu protects observedStatuses for concurrent access from the poller goroutine.
	mu sync.Mutex

	// observedStatuses tracks the scan_status values seen during polling,
	// in order, for the happy-path transition assertion.
	observedStatuses []string

	// lastError captures the error returned by an operation so a subsequent
	// Then step can assert on it.
	lastError error

	// lastHTTPStatus captures the HTTP status code from the last API call
	// so Then steps can assert the specific rejection code.
	lastHTTPStatus int
}

func newMetricIngestorState() *metricIngestorState {
	return &metricIngestorState{
		repoID: "00000000-0000-0000-0000-000000000001",
	}
}

func (s *metricIngestorState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *metricIngestorState) aRunningMetricIngestorConnectedToPostgreSQL() error {
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

	s.ingestorURL = os.Getenv("CLEAN_CODE_INGESTOR_URL")
	if s.ingestorURL == "" {
		s.ingestorURL = "http://localhost:8083"
	}
	return nil
}

func (s *metricIngestorState) theDatabaseIsMigratedAndSeededWithFixtures() error {
	// Ensure the repo fixture row exists (mirrors seed-fixtures-phase-03).
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
		VALUES ($1, 'e2e-test-repo', 'main')
		ON CONFLICT (repo_id) DO NOTHING
	`, s.repoID)
	if err != nil {
		return fmt.Errorf("ensuring repo fixture: %w", err)
	}
	return nil
}

func (s *metricIngestorState) aCommitExistsWithScanStatus(status string) error {
	// Generate a unique SHA per scenario run to avoid collisions.
	s.commitSHA = fmt.Sprintf("e2e%016x", time.Now().UnixNano())

	// Clean up any leftover data from previous runs.
	_, _ = s.db.ExecContext(context.Background(),
		`DELETE FROM clean_code.scan_run WHERE commit_sha = $1`, s.commitSHA)
	_, _ = s.db.ExecContext(context.Background(),
		`DELETE FROM clean_code.commit WHERE sha = $1`, s.commitSHA)

	// Insert a commit row in the requested initial state.
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.commit (sha, repo_id, scan_status)
		VALUES ($1, $2, $3::clean_code.scan_status)
	`, s.commitSHA, s.repoID, status)
	if err != nil {
		return fmt.Errorf("inserting commit with scan_status=%q: %w", status, err)
	}

	s.observedStatuses = []string{status}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *metricIngestorState) theMetricIngestorProcessesTheCommitSuccessfully() error {
	payload := map[string]interface{}{
		"commit_sha": s.commitSHA,
		"repo_id":    s.repoID,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling ingestor payload: %w", err)
	}

	triggerURL := strings.TrimRight(s.ingestorURL, "/") + "/v1/ingestor/process"

	// Start a concurrent DB poller BEFORE sending the HTTP request so we
	// can observe the intermediate "scanning" state while the ingestor is
	// processing (the ingestor commits scanning to DB before doing work).
	pollCtx, pollCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pollCancel()

	var pollWg sync.WaitGroup
	var pollErr error
	pollWg.Add(1)
	go func() {
		defer pollWg.Done()
		lastSeen := "pending"
		for {
			var current string
			qErr := s.db.QueryRowContext(pollCtx, `
				SELECT scan_status::text FROM clean_code.commit WHERE sha = $1
			`, s.commitSHA).Scan(&current)
			if qErr == nil && current != lastSeen {
				s.mu.Lock()
				s.observedStatuses = append(s.observedStatuses, current)
				s.mu.Unlock()
				lastSeen = current
			}

			if lastSeen == "scanned" {
				return
			}
			if pollCtx.Err() != nil {
				pollErr = fmt.Errorf("timed out waiting for scan_status=scanned (last=%s, observed=%v)",
					lastSeen, s.observedStatuses)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// Now fire the HTTP request while the poller is running.
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, triggerURL, strings.NewReader(string(body)))
	if err != nil {
		pollCancel()
		pollWg.Wait()
		return fmt.Errorf("creating ingestor request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		pollCancel()
		pollWg.Wait()
		return fmt.Errorf("sending ingestor trigger: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		pollCancel()
		pollWg.Wait()
		return fmt.Errorf("ingestor trigger returned HTTP %d", resp.StatusCode)
	}

	// Wait for the poller to see "scanned".
	pollWg.Wait()
	if pollErr != nil {
		return pollErr
	}
	return nil
}

func (s *metricIngestorState) theMetricIngestorProcessesARecipeThatPanics() error {
	// Trigger the ingestor with a recipe name that causes a real panic()
	// inside the ingestor's runRecipe function. The ingestor catches it
	// via recover() and transitions the commit to failed.
	payload := map[string]interface{}{
		"commit_sha": s.commitSHA,
		"repo_id":    s.repoID,
		"recipe":     "__panic_test__",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling ingestor payload: %w", err)
	}

	triggerURL := strings.TrimRight(s.ingestorURL, "/") + "/v1/ingestor/process"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, triggerURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("creating ingestor request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("sending panic trigger: %w", err)
	}
	defer resp.Body.Close()

	// The ingestor may return a 500 for a panic — that is the expected
	// behaviour; we only care about the DB state afterwards.

	// Poll for the commit to reach the failed state.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		var current string
		err := s.db.QueryRowContext(ctx, `
			SELECT scan_status::text FROM clean_code.commit WHERE sha = $1
		`, s.commitSHA).Scan(&current)
		if err == nil && current == "failed" {
			return nil
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for scan_status=failed: %w", ctx.Err())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *metricIngestorState) theScanRunWriterIsAskedToInsertKind(kind string) error {
	if s.commitSHA == "" {
		return fmt.Errorf("commitSHA is empty — the 'a commit exists with scan_status' step must run first")
	}

	// Attempt to insert a scan_run with an invalid kind value via the
	// ingestor API. The application-level enum guard MUST reject this with
	// an HTTP 400 or 422 before it reaches PostgreSQL.
	payload := map[string]interface{}{
		"commit_sha": s.commitSHA,
		"repo_id":    s.repoID,
		"kind":       kind,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshalling scan_run payload: %w", err)
	}

	triggerURL := strings.TrimRight(s.ingestorURL, "/") + "/v1/ingestor/scan-run"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, triggerURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("creating scan-run request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		// A network error does NOT prove the enum guard rejected the kind —
		// it could be a connectivity issue. Fail the step explicitly.
		return fmt.Errorf("scan-run request failed with network error (does not prove enum rejection): %w", err)
	}
	defer resp.Body.Close()

	s.lastHTTPStatus = resp.StatusCode

	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnprocessableEntity {
		// The ingestor correctly rejected the invalid kind before PostgreSQL.
		s.lastError = fmt.Errorf("scan-run rejected with HTTP %d", resp.StatusCode)
		return nil
	}

	// Any other status (including 2xx or 500) means the enum guard did NOT
	// catch the invalid kind at the application level.
	s.lastError = nil
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *metricIngestorState) theCommitScanStatusTransitionsThrough(expected string) error {
	expectedParts := strings.Split(expected, ", ")

	if len(s.observedStatuses) < len(expectedParts) {
		return fmt.Errorf("expected status transitions %v but observed only %v",
			expectedParts, s.observedStatuses)
	}

	// Verify the observed transitions contain the expected sequence in order.
	j := 0
	for i := 0; i < len(s.observedStatuses) && j < len(expectedParts); i++ {
		if s.observedStatuses[i] == expectedParts[j] {
			j++
		}
	}
	if j != len(expectedParts) {
		return fmt.Errorf("expected ordered transitions %v but observed %v",
			expectedParts, s.observedStatuses)
	}
	return nil
}

func (s *metricIngestorState) aSingleScanRunWithStatusIsAppended(expectedStatus string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var count int
	var status string
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(MIN(status::text), '')
			FROM clean_code.scan_run
			WHERE commit_sha = $1
		`, s.commitSHA).Scan(&count, &status)
		if err == nil && count > 0 {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for scan_run for sha=%s", s.commitSHA)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if count != 1 {
		return fmt.Errorf("expected exactly 1 scan_run for sha=%s, got %d", s.commitSHA, count)
	}
	if status != expectedStatus {
		return fmt.Errorf("expected scan_run status=%q, got %q", expectedStatus, status)
	}
	return nil
}

func (s *metricIngestorState) theCommitScanStatusIs(expected string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var actual string
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT scan_status::text FROM clean_code.commit WHERE sha = $1
		`, s.commitSHA).Scan(&actual)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for commit sha=%s: %w", s.commitSHA, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if actual != expected {
		return fmt.Errorf("expected scan_status=%q, got %q", expected, actual)
	}
	return nil
}

func (s *metricIngestorState) itReturnsAnHTTP400Or422ErrorRejectingTheInvalidKind() error {
	if s.lastError == nil {
		return fmt.Errorf("expected the ingestor to reject the invalid scan_run kind, but no error was returned (HTTP %d)", s.lastHTTPStatus)
	}
	if s.lastHTTPStatus != http.StatusBadRequest && s.lastHTTPStatus != http.StatusUnprocessableEntity {
		return fmt.Errorf("expected HTTP 400 or 422 rejecting invalid kind, but got HTTP %d", s.lastHTTPStatus)
	}
	return nil
}

func (s *metricIngestorState) noScanRunRowWithKindExists(kind string) error {
	var count int
	err := s.db.QueryRowContext(context.Background(), `
		SELECT COUNT(*) FROM clean_code.scan_run
		WHERE commit_sha = $1 AND kind::text = $2
	`, s.commitSHA, kind).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying scan_run for kind=%q: %w", kind, err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 scan_run rows with kind=%q, got %d", kind, count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_repo_indexer_and_metric_ingestor_metric_ingestor_and_scanrun_state_machine
// registers all Given/When/Then steps for the metric-ingestor-and-scanrun-state-machine stage.
func InitializeScenario_repo_indexer_and_metric_ingestor_metric_ingestor_and_scanrun_state_machine(ctx *godog.ScenarioContext) {
	var state *metricIngestorState

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		state = newMetricIngestorState()
		return bctx, nil
	})

	ctx.After(func(actx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if state != nil {
			state.close()
		}
		return actx, nil
	})

	// --- Given ---
	ctx.Step(`^a running Metric Ingestor connected to PostgreSQL$`, func() error {
		return state.aRunningMetricIngestorConnectedToPostgreSQL()
	})
	ctx.Step(`^the database is migrated and seeded with fixtures$`, func() error {
		return state.theDatabaseIsMigratedAndSeededWithFixtures()
	})
	ctx.Step(`^a commit exists with scan_status "([^"]*)"$`, func(status string) error {
		return state.aCommitExistsWithScanStatus(status)
	})

	// --- When ---
	ctx.Step(`^the Metric Ingestor processes the commit successfully$`, func() error {
		return state.theMetricIngestorProcessesTheCommitSuccessfully()
	})
	ctx.Step(`^the Metric Ingestor processes a recipe that panics$`, func() error {
		return state.theMetricIngestorProcessesARecipeThatPanics()
	})
	ctx.Step(`^the ScanRun writer is asked to insert kind "([^"]*)"$`, func(kind string) error {
		return state.theScanRunWriterIsAskedToInsertKind(kind)
	})

	// --- Then ---
	ctx.Step(`^the commit scan_status transitions through "([^"]*)"$`, func(expected string) error {
		return state.theCommitScanStatusTransitionsThrough(expected)
	})
	ctx.Step(`^a single scan_run with status "([^"]*)" is appended for that commit$`, func(status string) error {
		return state.aSingleScanRunWithStatusIsAppended(status)
	})
	ctx.Step(`^the commit scan_status is "([^"]*)"$`, func(expected string) error {
		return state.theCommitScanStatusIs(expected)
	})
	ctx.Step(`^it returns an HTTP 400 or 422 error rejecting the invalid kind$`, func() error {
		return state.itReturnsAnHTTP400Or422ErrorRejectingTheInvalidKind()
	})
	ctx.Step(`^no scan_run row with kind "([^"]*)" exists$`, func(kind string) error {
		return state.noScanRunRowWithKindExists(kind)
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_repo_indexer_and_metric_ingestor_metric_ingestor_and_scanrun_state_machine(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_repo_indexer_and_metric_ingestor_metric_ingestor_and_scanrun_state_machine,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"repo_indexer_and_metric_ingestor_metric_ingestor_and_scanrun_state_machine.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}