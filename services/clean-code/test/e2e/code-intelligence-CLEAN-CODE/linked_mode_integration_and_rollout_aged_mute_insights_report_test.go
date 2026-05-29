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
// Shared state for aged-mute-insights-report scenarios
// ---------------------------------------------------------------------------

type agedMuteState struct {
	db         *sql.DB
	pgURL      string
	stewardURL string
	mgmtURL    string

	// aged-mute-listed-not-enforced
	listedScope  string
	listedRuleID string
	reportResp   *http.Response
	reportBody   []byte

	// unmute-removes-from-report
	unmuteScope  string
	unmuteRuleID string
	postReport   []byte
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *agedMuteState) ensureDB() error {
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

func (s *agedMuteState) ensureStewardURL() {
	if s.stewardURL != "" {
		return
	}
	s.stewardURL = os.Getenv("CLEAN_CODE_POLICY_STEWARD_URL")
	if s.stewardURL == "" {
		s.stewardURL = "http://localhost:8082"
	}
}

func (s *agedMuteState) ensureMgmtURL() {
	if s.mgmtURL != "" {
		return
	}
	s.mgmtURL = os.Getenv("CLEAN_CODE_MGMT_URL")
	if s.mgmtURL == "" {
		s.mgmtURL = s.stewardURL
		s.ensureStewardURL()
		s.mgmtURL = s.stewardURL
	}
}

func (s *agedMuteState) httpJSON(method, url string, body interface{}) (*http.Response, []byte, error) {
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

func (s *agedMuteState) stewardPost(path string, body interface{}) (*http.Response, []byte, error) {
	s.ensureStewardURL()
	return s.httpJSON(http.MethodPost, s.stewardURL+path, body)
}

func (s *agedMuteState) stewardGet(path string) (*http.Response, []byte, error) {
	s.ensureStewardURL()
	return s.httpJSON(http.MethodGet, s.stewardURL+path, nil)
}

// uniqueScope returns a unique scope string for test isolation.
func uniqueScope() string {
	return fmt.Sprintf("test-scope-%d", time.Now().UnixNano())
}

// ---------------------------------------------------------------------------
// Scenario: aged-mute-listed-not-enforced
//
// Creates an override via the steward API, backdates its created_at
// in the DB, then queries the aged-mutes report to verify the
// override appears AND the mute is still active (no automatic flip).
// ---------------------------------------------------------------------------

func (s *agedMuteState) anOverrideWithMuteEqualToAndCreatedAtDaysAgo(muteVal string, days int) error {
	s.listedScope = uniqueScope()
	s.listedRuleID = "SOLID-AGED-001"

	mute := muteVal == "true"
	payload := map[string]interface{}{
		"scope":   s.listedScope,
		"rule_id": s.listedRuleID,
		"mute":    mute,
	}
	resp, body, err := s.stewardPost("/v1/mgmt.override", payload)
	if err != nil {
		return fmt.Errorf("POST override: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("override returned %d: %s", resp.StatusCode, string(body))
	}

	// Backdate the row in the DB to simulate age.
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx,
		`UPDATE mgmt_override
		 SET created_at = NOW() - ($3 || ' days')::interval
		 WHERE scope = $1 AND rule_id = $2`,
		s.listedScope, s.listedRuleID, days)
	if err != nil {
		return fmt.Errorf("backdating override row: %w", err)
	}
	return nil
}

func (s *agedMuteState) theAgedMutesReportRuns() error {
	resp, body, err := s.stewardGet("/v1/insights/aged-mutes")
	if err != nil {
		return fmt.Errorf("GET aged-mutes report: %w", err)
	}
	s.reportResp = resp
	s.reportBody = body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("aged-mutes report returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (s *agedMuteState) theOverrideAppearsInTheReportResponse() error {
	if s.reportResp == nil {
		return fmt.Errorf("no report response captured")
	}

	// Parse the report as a JSON array of items containing scope and rule_id.
	var items []struct {
		Scope  string `json:"scope"`
		RuleID string `json:"rule_id"`
	}
	if err := json.Unmarshal(s.reportBody, &items); err != nil {
		// Try wrapping in an object with an "items" key.
		var wrapper struct {
			Items []struct {
				Scope  string `json:"scope"`
				RuleID string `json:"rule_id"`
			} `json:"items"`
		}
		if err2 := json.Unmarshal(s.reportBody, &wrapper); err2 != nil {
			return fmt.Errorf("parsing report body: %w (body: %s)", err, string(s.reportBody))
		}
		items = wrapper.Items
	}

	for _, item := range items {
		if item.Scope == s.listedScope && item.RuleID == s.listedRuleID {
			return nil
		}
	}
	return fmt.Errorf("override (scope=%s, rule_id=%s) not found in aged-mutes report (body: %s)",
		s.listedScope, s.listedRuleID, string(s.reportBody))
}

func (s *agedMuteState) itRemainsTheActiveMuteWithValue(expected string) error {
	// Query the evaluator or steward to verify the mute is still active.
	path := fmt.Sprintf("/v1/evaluate/mute?scope=%s&rule_id=%s",
		s.listedScope, s.listedRuleID)
	resp, body, err := s.stewardGet(path)
	if err != nil {
		return fmt.Errorf("GET evaluate/mute: %w", err)
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
	expectedBool := expected == "true"
	if result.Mute != expectedBool {
		return fmt.Errorf("expected mute=%s, got mute=%v (body: %s)",
			expected, result.Mute, string(body))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: unmute-removes-from-report
//
// Creates an aged mute override, verifies it appears in the report,
// then appends a mute=false override and verifies the scope/rule
// pair is no longer in the report.
// ---------------------------------------------------------------------------

func (s *agedMuteState) anAgedMuteOverrideForAKnownScopeAndRule() error {
	s.unmuteScope = uniqueScope()
	s.unmuteRuleID = "SOLID-AGED-002"

	payload := map[string]interface{}{
		"scope":   s.unmuteScope,
		"rule_id": s.unmuteRuleID,
		"mute":    true,
	}
	resp, body, err := s.stewardPost("/v1/mgmt.override", payload)
	if err != nil {
		return fmt.Errorf("POST override: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("override returned %d: %s", resp.StatusCode, string(body))
	}

	// Backdate to make it "aged".
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = s.db.ExecContext(ctx,
		`UPDATE mgmt_override
		 SET created_at = NOW() - INTERVAL '100 days'
		 WHERE scope = $1 AND rule_id = $2`,
		s.unmuteScope, s.unmuteRuleID)
	if err != nil {
		return fmt.Errorf("backdating override row: %w", err)
	}
	return nil
}

func (s *agedMuteState) theOperatorAppendsAnOverrideWithMuteEqualToViaMgmtOverride(muteVal string) error {
	s.ensureStewardURL()
	mute := muteVal == "true"
	payload := map[string]interface{}{
		"scope":   s.unmuteScope,
		"rule_id": s.unmuteRuleID,
		"mute":    mute,
	}
	resp, body, err := s.stewardPost("/v1/mgmt.override", payload)
	if err != nil {
		return fmt.Errorf("POST unmute override: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("unmute override returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (s *agedMuteState) theNextAgedMutesReportOmitsTheScopeAndRulePair() error {
	resp, body, err := s.stewardGet("/v1/insights/aged-mutes")
	if err != nil {
		return fmt.Errorf("GET aged-mutes report: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("aged-mutes report returned %d: %s", resp.StatusCode, string(body))
	}
	s.postReport = body

	// Parse the report and ensure our scope/rule is NOT present.
	var items []struct {
		Scope  string `json:"scope"`
		RuleID string `json:"rule_id"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		var wrapper struct {
			Items []struct {
				Scope  string `json:"scope"`
				RuleID string `json:"rule_id"`
			} `json:"items"`
		}
		if err2 := json.Unmarshal(body, &wrapper); err2 != nil {
			return fmt.Errorf("parsing report body: %w (body: %s)", err, string(body))
		}
		items = wrapper.Items
	}

	for _, item := range items {
		if item.Scope == s.unmuteScope && item.RuleID == s.unmuteRuleID {
			return fmt.Errorf("override (scope=%s, rule_id=%s) should NOT appear in aged-mutes report after unmute (body: %s)",
				s.unmuteScope, s.unmuteRuleID, string(body))
		}
	}
	return nil
}

// ======================================================================
// Godog wiring
// ======================================================================

func InitializeScenario_linked_mode_integration_and_rollout_aged_mute_insights_report(ctx *godog.ScenarioContext) {
	s := &agedMuteState{}

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if s.db != nil {
			s.db.Close()
		}
		return ctx, nil
	})

	// aged-mute-listed-not-enforced
	ctx.Step(`^an override with mute equal to "([^"]*)" and created_at "(\d+)" days ago$`, s.anOverrideWithMuteEqualToAndCreatedAtDaysAgo)
	ctx.Step(`^the aged-mutes report runs$`, s.theAgedMutesReportRuns)
	ctx.Step(`^the override appears in the report response$`, s.theOverrideAppearsInTheReportResponse)
	ctx.Step(`^it remains the active mute with value "([^"]*)"$`, s.itRemainsTheActiveMuteWithValue)

	// unmute-removes-from-report
	ctx.Step(`^an aged mute override for a known scope and rule$`, s.anAgedMuteOverrideForAKnownScopeAndRule)
	ctx.Step(`^the operator appends an override with mute equal to "([^"]*)" via mgmt\.override$`, s.theOperatorAppendsAnOverrideWithMuteEqualToViaMgmtOverride)
	ctx.Step(`^the next aged-mutes report omits the scope and rule pair$`, s.theNextAgedMutesReportOmitsTheScopeAndRulePair)
}

func TestE2E_linked_mode_integration_and_rollout_aged_mute_insights_report(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_linked_mode_integration_and_rollout_aged_mute_insights_report,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"linked_mode_integration_and_rollout_aged_mute_insights_report.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}