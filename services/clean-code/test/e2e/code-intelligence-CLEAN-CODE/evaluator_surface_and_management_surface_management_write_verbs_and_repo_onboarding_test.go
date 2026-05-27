//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
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
// test when the variable is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("required env var %s is not set; skipping E2E test", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Per-scenario state
// ---------------------------------------------------------------------------

type mgmtWriteState struct {
	pgURL     string
	mgmtURL   string
	authToken string

	db *sql.DB

	// register-repo-idempotent scenario
	registeredRepoID string
	registeredURL    string
	secondRepoID     string

	// set-mode-emits-event scenario
	modeRepoID          string
	initialMode         string
	eventCountBeforeSet int64
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *mgmtWriteState) generateUniqueID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// doRequest is the single HTTP helper; it attaches the OIDC bearer token
// (from CLEAN_CODE_OIDC_DEV_TOKEN) when available so that authenticated
// mgmt-surface endpoints accept the call.
func (s *mgmtWriteState) doRequest(method, url string, payload interface{}) ([]byte, int, error) {
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("marshalling request body: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("creating %s request: %w", method, err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.authToken)
	}

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("%s %s failed: %w", method, url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response body: %w", err)
	}
	return respBody, resp.StatusCode, nil
}

func (s *mgmtWriteState) httpPost(url string, payload interface{}) ([]byte, int, error) {
	return s.doRequest(http.MethodPost, url, payload)
}

func (s *mgmtWriteState) httpGet(url string) ([]byte, int, error) {
	return s.doRequest(http.MethodGet, url, nil)
}

func (s *mgmtWriteState) httpPut(url string, payload interface{}) ([]byte, int, error) {
	return s.doRequest(http.MethodPut, url, payload)
}

// registerRepo calls the mgmt.register_repo endpoint and returns the repo_id.
func (s *mgmtWriteState) registerRepo(repoURL string) (string, error) {
	payload := map[string]string{"url": repoURL}
	body, statusCode, err := s.httpPost(fmt.Sprintf("%s/api/v1/repos", s.mgmtURL), payload)
	if err != nil {
		return "", err
	}
	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		return "", fmt.Errorf("expected 200 or 201, got %d: %s", statusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("unmarshalling register_repo response: %w", err)
	}

	repoID, ok := result["repo_id"].(string)
	if !ok {
		// Try "id" as alternative field name
		repoID, ok = result["id"].(string)
		if !ok {
			return "", fmt.Errorf("response missing repo_id/id field: %s", string(body))
		}
	}
	return repoID, nil
}

// ---------------------------------------------------------------------------
// Scenario: register-repo-idempotent
// ---------------------------------------------------------------------------

func (s *mgmtWriteState) aRepoIsAlreadyRegisteredWithURL(repoURL string) error {
	s.registeredURL = repoURL
	repoID, err := s.registerRepo(repoURL)
	if err != nil {
		return fmt.Errorf("initial repo registration failed: %w", err)
	}
	s.registeredRepoID = repoID
	return nil
}

func (s *mgmtWriteState) mgmtRegisterRepoIsCalledWithTheSameURL(repoURL string) error {
	repoID, err := s.registerRepo(repoURL)
	if err != nil {
		return fmt.Errorf("second repo registration failed: %w", err)
	}
	s.secondRepoID = repoID
	return nil
}

func (s *mgmtWriteState) theExistingRepoIDIsReturned() error {
	if s.secondRepoID != s.registeredRepoID {
		return fmt.Errorf("expected same repo_id %s, got %s", s.registeredRepoID, s.secondRepoID)
	}
	return nil
}

func (s *mgmtWriteState) noDuplicateRepoRowAppearsForURL(repoURL string) error {
	if s.db == nil {
		return fmt.Errorf("db not initialised; cannot verify duplicate repo rows")
	}
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM repo WHERE url = $1`, repoURL).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying repo count: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("expected exactly 1 repo row for URL %s, got %d", repoURL, count)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: set-mode-emits-event
// ---------------------------------------------------------------------------

func (s *mgmtWriteState) aRepoRegisteredAtMode(mode string) error {
	if s.db == nil {
		return fmt.Errorf("db not initialised; cannot snapshot repo_event count")
	}
	s.initialMode = mode
	// Register a unique repo for this scenario
	uniqueURL := fmt.Sprintf("https://github.com/acme/mode-test-%d", time.Now().UnixNano())
	repoID, err := s.registerRepo(uniqueURL)
	if err != nil {
		return fmt.Errorf("registering repo for mode test: %w", err)
	}
	s.modeRepoID = repoID

	// Ensure the repo starts at the specified mode. If the default is not the
	// desired initial mode, set it explicitly.
	payload := map[string]string{"mode": mode}
	_, statusCode, err := s.httpPut(
		fmt.Sprintf("%s/api/v1/repos/%s/mode", s.mgmtURL, s.modeRepoID), payload)
	if err != nil {
		return fmt.Errorf("setting initial mode: %w", err)
	}
	// Accept 200, 204, or 201 as success for the initial mode set
	if statusCode != http.StatusOK && statusCode != http.StatusNoContent && statusCode != http.StatusCreated {
		return fmt.Errorf("setting initial mode returned %d", statusCode)
	}

	// Snapshot the current event count AFTER initial setup so that the Then
	// step can verify a NEW event was appended by the When step (not by this
	// Given step's own set_mode call).
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM repo_event WHERE repo_id = $1 AND kind = 'mode_changed'`,
		s.modeRepoID).Scan(&s.eventCountBeforeSet)
	if err != nil {
		return fmt.Errorf("snapshotting repo_event count: %w", err)
	}

	return nil
}

func (s *mgmtWriteState) mgmtSetModeIsCalledWithModeForThatRepo(newMode string) error {
	payload := map[string]string{"mode": newMode}
	body, statusCode, err := s.httpPut(
		fmt.Sprintf("%s/api/v1/repos/%s/mode", s.mgmtURL, s.modeRepoID), payload)
	if err != nil {
		return fmt.Errorf("set_mode call failed: %w", err)
	}
	if statusCode != http.StatusOK && statusCode != http.StatusNoContent {
		return fmt.Errorf("expected 200 or 204, got %d: %s", statusCode, string(body))
	}
	return nil
}

func (s *mgmtWriteState) aRepoEventWithKindIsAppended(expectedKind string) error {
	if s.db == nil {
		return fmt.Errorf("db not initialised; cannot verify repo_event")
	}
	var countAfter int64
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM repo_event WHERE repo_id = $1 AND kind = $2`,
		s.modeRepoID, expectedKind).Scan(&countAfter)
	if err != nil {
		return fmt.Errorf("querying repo_event: %w", err)
	}
	// Verify that the When step appended at least one NEW event beyond what
	// existed after the Given step completed.
	if countAfter <= s.eventCountBeforeSet {
		return fmt.Errorf(
			"expected new repo_event(kind=%q) after set_mode; count before=%d, count after=%d",
			expectedKind, s.eventCountBeforeSet, countAfter)
	}
	return nil
}

func (s *mgmtWriteState) subsequentMgmtReadRepoReturnsMode(expectedMode string) error {
	body, statusCode, err := s.httpGet(
		fmt.Sprintf("%s/api/v1/repos/%s", s.mgmtURL, s.modeRepoID))
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("expected 200, got %d: %s", statusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("unmarshalling repo response: %w", err)
	}

	mode, ok := result["mode"].(string)
	if !ok {
		return fmt.Errorf("response missing mode field: %s", string(body))
	}
	if mode != expectedMode {
		return fmt.Errorf("expected mode=%q, got %q", expectedMode, mode)
	}
	return nil
}

// ---------------------------------------------------------------------------
// State factory
// ---------------------------------------------------------------------------

func newMgmtWriteStateFromEnv() *mgmtWriteState {
	pgURL := os.Getenv("CLEAN_CODE_PG_URL")
	mgmtURL := os.Getenv("CLEAN_CODE_MGMT_URL")

	// make tokens mints OIDC bearer tokens per role; use the dev token for
	// write-verb calls against the authenticated mgmt-surface.
	authToken := os.Getenv("CLEAN_CODE_OIDC_DEV_TOKEN")
	if authToken == "" {
		// Fallback: some CI setups export a generic token name.
		authToken = os.Getenv("CLEAN_CODE_OIDC_TOKEN")
	}

	s := &mgmtWriteState{
		pgURL:     pgURL,
		mgmtURL:   mgmtURL,
		authToken: authToken,
	}

	if pgURL != "" {
		db, err := sql.Open("postgres", pgURL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: sql.Open failed for CLEAN_CODE_PG_URL: %v\n", err)
		} else {
			db.SetMaxOpenConns(5)
			db.SetConnMaxLifetime(2 * time.Minute)
			s.db = db
		}
	}

	return s
}

// ---------------------------------------------------------------------------
// Scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_evaluator_surface_and_management_surface_management_write_verbs_and_repo_onboarding(ctx *godog.ScenarioContext) {
	s := newMgmtWriteStateFromEnv()

	// Release the database connection pool after each scenario.
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if s.db != nil {
			s.db.Close()
		}
		return ctx, nil
	})

	// Scenario: register-repo-idempotent
	ctx.Step(`^a repo is already registered with URL "([^"]*)"$`, s.aRepoIsAlreadyRegisteredWithURL)
	ctx.Step(`^mgmt\.register_repo is called with the same URL "([^"]*)"$`, s.mgmtRegisterRepoIsCalledWithTheSameURL)
	ctx.Step(`^the existing repo_id is returned$`, s.theExistingRepoIDIsReturned)
	ctx.Step(`^no duplicate repo row appears for URL "([^"]*)"$`, s.noDuplicateRepoRowAppearsForURL)

	// Scenario: set-mode-emits-event
	ctx.Step(`^a repo registered at mode "([^"]*)"$`, s.aRepoRegisteredAtMode)
	ctx.Step(`^mgmt\.set_mode is called with mode "([^"]*)" for that repo$`, s.mgmtSetModeIsCalledWithModeForThatRepo)
	ctx.Step(`^a repo_event with kind "([^"]*)" is appended$`, s.aRepoEventWithKindIsAppended)
	ctx.Step(`^subsequent mgmt\.read\.repo returns mode "([^"]*)"$`, s.subsequentMgmtReadRepoReturnsMode)
}

// ---------------------------------------------------------------------------
// Test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_evaluator_surface_and_management_surface_management_write_verbs_and_repo_onboarding(t *testing.T) {
	// Ensure required env vars are present; skip gracefully if not.
	requireEnv(t, "CLEAN_CODE_PG_URL")
	requireEnv(t, "CLEAN_CODE_MGMT_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_evaluator_surface_and_management_surface_management_write_verbs_and_repo_onboarding,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"evaluator_surface_and_management_surface_management_write_verbs_and_repo_onboarding.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run E2E tests")
	}
}