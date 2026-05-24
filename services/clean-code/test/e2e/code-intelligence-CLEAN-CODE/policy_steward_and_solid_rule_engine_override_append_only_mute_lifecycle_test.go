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
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// requireEnv returns the value of the named environment variable,
// calling t.Skip when unset or empty.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Shared state for override-append-only-mute-lifecycle scenarios
// ---------------------------------------------------------------------------

type overrideMuteState struct {
	db            *sql.DB
	pgURL         string
	stewardURL    string
	ruleEngineURL string

	// override-no-expires-field
	expiresScope     string
	expiresRuleID    string
	rowCountBefore   int
	overrideResp     *http.Response
	overrideRespBody []byte
	overrideErr      error

	// latest-row-wins
	latestScope  string
	latestRuleID string
	evalResp     *http.Response
	evalRespBody []byte

	// no-ttl-enforcement
	ttlScope  string
	ttlRuleID string
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *overrideMuteState) ensureDB() error {
	if s.db != nil {
		return nil
	}
	s.pgURL = os.Getenv("CLEAN_CODE_PG_URL")
	if s.pgURL == "" {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	db, err := sql.Open("postgres", s.pgURL)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}
	db.SetMaxOpenConns(5)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("pinging postgres: %w", err)
	}
	s.db = db
	return nil
}

func (s *overrideMuteState) ensureStewardURL() {
	if s.stewardURL != "" {
		return
	}
	s.stewardURL = os.Getenv("CLEAN_CODE_POLICY_STEWARD_URL")
	if s.stewardURL == "" {
		s.stewardURL = "http://localhost:8082"
	}
}

func (s *overrideMuteState) ensureRuleEngineURL() {
	if s.ruleEngineURL != "" {
		return
	}
	s.ruleEngineURL = os.Getenv("CLEAN_CODE_RULE_ENGINE_URL")
	if s.ruleEngineURL == "" {
		s.ruleEngineURL = "http://localhost:8083"
	}
}

func (s *overrideMuteState) httpJSON(method, url string, body interface{}) (*http.Response, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		jsonBody, err := json.Marshal(body)
		if err != nil {
			return nil, nil, fmt.Errorf("marshalling request: %w", err)
		}
		reqBody = bytes.NewReader(jsonBody)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, respBody, nil
}

func (s *overrideMuteState) stewardPost(path string, body interface{}) (*http.Response, []byte, error) {
	s.ensureStewardURL()
	return s.httpJSON(http.MethodPost, s.stewardURL+path, body)
}

func (s *overrideMuteState) ruleEngineGet(path string) (*http.Response, []byte, error) {
	s.ensureRuleEngineURL()
	return s.httpJSON(http.MethodGet, s.ruleEngineURL+path, nil)
}

// uniqueScope returns a unique scope string for test isolation.
func uniqueScope() string {
	return fmt.Sprintf("test-scope-%d", time.Now().UnixNano())
}

// countOverrideRows counts ALL rows for a given scope+rule_id (no column filter).
func (s *overrideMuteState) countOverrideRows(scope, ruleID string) (int, error) {
	if err := s.ensureDB(); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM mgmt_override WHERE scope = $1 AND rule_id = $2`,
		scope, ruleID).Scan(&count)
	return count, err
}

// ---------------------------------------------------------------------------
// Scenario: override-no-expires-field
//
// Uses a unique scope so the row-count comparison is isolated.
// Snapshots total row count BEFORE the request, then verifies count
// is unchanged AFTER — catches both "row with expires_at" and "row
// inserted with the field silently dropped".
// ---------------------------------------------------------------------------

func (s *overrideMuteState) aMgmtOverrideRequestWithExpiresAtSetTo(field, value string) error {
	s.expiresScope = uniqueScope()
	s.expiresRuleID = "SOLID-001"

	// Snapshot the row count before the request so we can detect any append.
	before, err := s.countOverrideRows(s.expiresScope, s.expiresRuleID)
	if err != nil {
		return fmt.Errorf("snapshot row count: %w", err)
	}
	s.rowCountBefore = before

	payload := map[string]interface{}{
		"scope":   s.expiresScope,
		"rule_id": s.expiresRuleID,
		"mute":    true,
		field:     value,
	}
	resp, body, err := s.stewardPost("/v1/mgmt.override", payload)
	s.overrideResp = resp
	s.overrideRespBody = body
	s.overrideErr = err
	return nil
}

func (s *overrideMuteState) theVerbRuns() error {
	if s.overrideErr != nil {
		return fmt.Errorf("verb request failed: %w", s.overrideErr)
	}
	return nil
}

func (s *overrideMuteState) itReturnsAValidationErrorNamingFieldAsUnsupportedInV1(field string) error {
	if s.overrideResp == nil {
		return fmt.Errorf("no response received")
	}
	if s.overrideResp.StatusCode < 400 || s.overrideResp.StatusCode >= 500 {
		return fmt.Errorf("expected 4xx status, got %d; body: %s",
			s.overrideResp.StatusCode, string(s.overrideRespBody))
	}
	bodyStr := strings.ToLower(string(s.overrideRespBody))
	fieldLower := strings.ToLower(field)
	if !strings.Contains(bodyStr, fieldLower) {
		return fmt.Errorf("expected response body to mention %q, got: %s",
			field, string(s.overrideRespBody))
	}
	return nil
}

func (s *overrideMuteState) noOverrideRowIsAppended() error {
	// Count ALL rows for this scope+rule — no column filter — so we catch
	// both "row with expires_at" and "row silently inserted without it".
	after, err := s.countOverrideRows(s.expiresScope, s.expiresRuleID)
	if err != nil {
		return fmt.Errorf("querying mgmt_override: %w", err)
	}
	if after != s.rowCountBefore {
		return fmt.Errorf("expected row count to stay at %d, but found %d after the rejected request",
			s.rowCountBefore, after)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: latest-row-wins
//
// Both override rows are created through the policy-steward API
// (POST /v1/mgmt.override). The active mute is read through the
// rule-engine evaluator API (GET /v1/evaluate/mute), exercising
// the full service stack rather than querying the DB directly.
// ---------------------------------------------------------------------------

func (s *overrideMuteState) twoOverrideRowsForSameScopeAndRuleWithMuteValuesThenValue(first, second string) error {
	s.latestScope = uniqueScope()
	s.latestRuleID = "SOLID-002"

	firstMute := first == "true"
	secondMute := second == "true"

	// First override via steward API.
	payload1 := map[string]interface{}{
		"scope":   s.latestScope,
		"rule_id": s.latestRuleID,
		"mute":    firstMute,
	}
	resp1, body1, err := s.stewardPost("/v1/mgmt.override", payload1)
	if err != nil {
		return fmt.Errorf("POST first override: %w", err)
	}
	if resp1.StatusCode >= 300 {
		return fmt.Errorf("first override returned %d: %s", resp1.StatusCode, string(body1))
	}

	// Brief pause so second row gets a later created_at.
	time.Sleep(50 * time.Millisecond)

	// Second override via steward API.
	payload2 := map[string]interface{}{
		"scope":   s.latestScope,
		"rule_id": s.latestRuleID,
		"mute":    secondMute,
	}
	resp2, body2, err := s.stewardPost("/v1/mgmt.override", payload2)
	if err != nil {
		return fmt.Errorf("POST second override: %w", err)
	}
	if resp2.StatusCode >= 300 {
		return fmt.Errorf("second override returned %d: %s", resp2.StatusCode, string(body2))
	}
	return nil
}

func (s *overrideMuteState) theEvaluatorReadsTheActiveMute() error {
	// Query the rule-engine evaluator API for the active mute state of
	// this scope+rule. This exercises the full service stack instead of
	// directly querying the DB.
	path := fmt.Sprintf("/v1/evaluate/mute?scope=%s&rule_id=%s",
		s.latestScope, s.latestRuleID)
	resp, body, err := s.ruleEngineGet(path)
	if err != nil {
		return fmt.Errorf("GET evaluate/mute: %w", err)
	}
	s.evalResp = resp
	s.evalRespBody = body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("evaluate/mute returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (s *overrideMuteState) itSeesMuteEquals(expected string) error {
	if s.evalResp == nil {
		return fmt.Errorf("evaluator response was not captured")
	}
	// Parse the evaluator JSON response to extract the mute field.
	var result struct {
		Mute bool `json:"mute"`
	}
	if err := json.Unmarshal(s.evalRespBody, &result); err != nil {
		return fmt.Errorf("parsing evaluator response: %w (body: %s)", err, string(s.evalRespBody))
	}
	expectedBool := expected == "true"
	if result.Mute != expectedBool {
		return fmt.Errorf("expected mute=%s from evaluator, got mute=%v (body: %s)",
			expected, result.Mute, string(s.evalRespBody))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: no-ttl-enforcement
//
// Creates the override via the steward API, then backdates it in
// the DB to simulate age. The "time advances" step is a deliberate
// wait, not a no-op — it gives any hypothetical TTL reaper time to
// act. The assertion reads the active mute through the rule-engine
// evaluator API to verify the row is still active end-to-end.
// ---------------------------------------------------------------------------

func (s *overrideMuteState) anOverrideRowOlderThanDays(days int) error {
	s.ttlScope = uniqueScope()
	s.ttlRuleID = "SOLID-003"

	// Create the override through the steward API (true E2E path).
	payload := map[string]interface{}{
		"scope":   s.ttlScope,
		"rule_id": s.ttlRuleID,
		"mute":    true,
	}
	resp, body, err := s.stewardPost("/v1/mgmt.override", payload)
	if err != nil {
		return fmt.Errorf("POST override for TTL test: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("override returned %d: %s", resp.StatusCode, string(body))
	}

	// Backdate the row's created_at in the DB to simulate an old override.
	// Use make_interval with a bound parameter rather than string-interpolating
	// the day count — keeps the interval arithmetic fully parameterised and
	// avoids teaching a copy-paste SQL-injection-prone pattern.
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx,
		`UPDATE mgmt_override
		 SET created_at = NOW() - make_interval(days => $3)
		 WHERE scope = $1 AND rule_id = $2`,
		s.ttlScope, s.ttlRuleID, days)
	if err != nil {
		return fmt.Errorf("backdating override row: %w", err)
	}
	return nil
}

func (s *overrideMuteState) timeAdvancesAndNoScheduledJobRuns() error {
	// Wait briefly to give any hypothetical TTL reaper time to act.
	// If no TTL enforcement exists (correct v1 behavior), the row
	// will still be present after the wait.
	time.Sleep(2 * time.Second)
	return nil
}

func (s *overrideMuteState) theRowRemainsTheActiveStateViaLatestRowWins() error {
	// Query the rule-engine evaluator API to confirm the old row is
	// still the active override — exercising the full service stack.
	path := fmt.Sprintf("/v1/evaluate/mute?scope=%s&rule_id=%s",
		s.ttlScope, s.ttlRuleID)
	resp, body, err := s.ruleEngineGet(path)
	if err != nil {
		return fmt.Errorf("GET evaluate/mute for TTL check: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("evaluate/mute returned %d: %s", resp.StatusCode, string(body))
	}
	var result struct {
		Mute bool `json:"mute"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parsing evaluator response: %w (body: %s)", err, string(body))
	}
	if !result.Mute {
		return fmt.Errorf("expected mute=true from evaluator for old row, got false (body: %s)",
			string(body))
	}
	return nil
}

// ======================================================================
// Godog wiring
// ======================================================================

func InitializeScenario_policy_steward_and_solid_rule_engine_override_append_only_mute_lifecycle(ctx *godog.ScenarioContext) {
	s := &overrideMuteState{}

	// override-no-expires-field
	ctx.Step(`^a "mgmt\.override" request with "([^"]*)" set to "([^"]*)"$`, s.aMgmtOverrideRequestWithExpiresAtSetTo)
	ctx.Step(`^the verb runs$`, s.theVerbRuns)
	ctx.Step(`^it returns a validation error naming "([^"]*)" as unsupported in v1$`, s.itReturnsAValidationErrorNamingFieldAsUnsupportedInV1)
	ctx.Step(`^no override row is appended$`, s.noOverrideRowIsAppended)

	// latest-row-wins
	ctx.Step(`^two override rows for the same scope and rule with "mute" values "([^"]*)" then "([^"]*)"$`, s.twoOverrideRowsForSameScopeAndRuleWithMuteValuesThenValue)
	ctx.Step(`^the evaluator reads the active mute$`, s.theEvaluatorReadsTheActiveMute)
	ctx.Step(`^it sees mute equals "([^"]*)"$`, s.itSeesMuteEquals)

	// no-ttl-enforcement
	ctx.Step(`^an override row older than (\d+) days$`, s.anOverrideRowOlderThanDays)
	ctx.Step(`^time advances and no scheduled job runs$`, s.timeAdvancesAndNoScheduledJobRuns)
	ctx.Step(`^the row remains the active state via latest-row-wins$`, s.theRowRemainsTheActiveStateViaLatestRowWins)
}

func TestE2E_policy_steward_and_solid_rule_engine_override_append_only_mute_lifecycle(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_policy_steward_and_solid_rule_engine_override_append_only_mute_lifecycle,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"policy_steward_and_solid_rule_engine_override_append_only_mute_lifecycle.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
