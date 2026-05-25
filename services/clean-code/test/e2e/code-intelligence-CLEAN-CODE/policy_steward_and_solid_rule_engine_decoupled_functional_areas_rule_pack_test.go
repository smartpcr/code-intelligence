//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
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
// Shared state
// ---------------------------------------------------------------------------

type decouplingRulePackState struct {
	stewardURL    string
	ruleEngineURL string

	// Scenario: decoupling-loads
	rulePacksFound []rulePackEntry

	// Scenario: cycles-rule-fires-on-cycle-member
	metricSample map[string]interface{}
	evalResult   *bool
}

type rulePackEntry struct {
	Pack       string `json:"pack"`
	Name       string `json:"name"`
	Predicate  string `json:"predicate"`
	RulePackID string `json:"rule_pack_id"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *decouplingRulePackState) ensureStewardURL() {
	if s.stewardURL != "" {
		return
	}
	s.stewardURL = os.Getenv("CLEAN_CODE_POLICY_STEWARD_URL")
	if s.stewardURL == "" {
		s.stewardURL = "http://localhost:8082"
	}
}

func (s *decouplingRulePackState) ensureRuleEngineURL() {
	if s.ruleEngineURL != "" {
		return
	}
	s.ruleEngineURL = os.Getenv("CLEAN_CODE_RULE_ENGINE_URL")
	if s.ruleEngineURL == "" {
		s.ruleEngineURL = "http://localhost:8083"
	}
}

// stewardGet sends a GET request to the policy-steward HTTP API.
func (s *decouplingRulePackState) stewardGet(path string) (int, []byte, error) {
	s.ensureStewardURL()
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodGet, s.stewardURL+path, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("creating GET request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// stewardPost sends a JSON POST to the policy-steward HTTP API.
func (s *decouplingRulePackState) stewardPost(path string, payload interface{}) (int, []byte, error) {
	s.ensureStewardURL()
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("marshalling request: %w", err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodPost, s.stewardURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return 0, nil, fmt.Errorf("creating POST request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// ruleEnginePost sends a JSON POST to the rule-engine HTTP API.
func (s *decouplingRulePackState) ruleEnginePost(path string, payload interface{}) (int, []byte, error) {
	s.ensureRuleEngineURL()
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("marshalling request: %w", err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest(http.MethodPost, s.ruleEngineURL+path, bytes.NewReader(jsonBody))
	if err != nil {
		return 0, nil, fmt.Errorf("creating POST request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// ======================================================================
// Scenario: decoupling-loads
// ======================================================================

func (s *decouplingRulePackState) theThreeDecouplingRulepackFiles() error {
	s.ensureStewardURL()
	return nil
}

func (s *decouplingRulePackState) theStewardLoadsThem() error {
	// Query the steward for rule packs with pack='decoupling'.
	// The steward should have loaded these during startup from the
	// three decoupling rulepack files registered by seed-phase-05.
	statusCode, body, err := s.stewardGet("/v1/rule_packs?pack=decoupling")
	if err != nil {
		return fmt.Errorf("querying rule_packs: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("GET /v1/rule_packs?pack=decoupling returned %d: %s",
			statusCode, string(body))
	}

	// Parse the response to extract rule pack entries.
	var resp struct {
		RulePacks []rulePackEntry `json:"rule_packs"`
	}
	// Try wrapped response first, fall back to bare array.
	if err := json.Unmarshal(body, &resp); err == nil && len(resp.RulePacks) > 0 {
		s.rulePacksFound = resp.RulePacks
	} else {
		var packs []rulePackEntry
		if err2 := json.Unmarshal(body, &packs); err2 != nil {
			return fmt.Errorf("decoding rule_packs response: %w (body: %s)", err2, string(body))
		}
		s.rulePacksFound = packs
	}
	return nil
}

func (s *decouplingRulePackState) rulePacksExistWithParsedPredicates() error {
	if len(s.rulePacksFound) == 0 {
		return fmt.Errorf("expected decoupling rule_packs, but none were returned")
	}

	// Verify each rule pack has a non-empty predicate (proving parse succeeded).
	for _, rp := range s.rulePacksFound {
		if strings.TrimSpace(rp.Predicate) == "" {
			return fmt.Errorf("rule_pack %q (pack=%q) has an empty predicate — parsing may have failed",
				rp.Name, rp.Pack)
		}
		lowerPack := strings.ToLower(rp.Pack)
		if lowerPack != "decoupling" {
			return fmt.Errorf("expected pack='decoupling', got pack=%q for rule %q",
				rp.Pack, rp.Name)
		}
	}
	return nil
}

// ======================================================================
// Scenario: cycles-rule-fires-on-cycle-member
// ======================================================================

func (s *decouplingRulePackState) aMetricSampleWithMetricKindAndValue(metricKind string, value int) error {
	s.ensureRuleEngineURL()
	s.metricSample = map[string]interface{}{
		"metric_kind": metricKind,
		"value":       value,
		"scope":       "module",
		"repo":        "repo-a",
	}
	return nil
}

func (s *decouplingRulePackState) thePredicateEvaluates() error {
	// Build the predicate that the cycle-member rule would use:
	// it fires when metric_kind is 'cycle_member' and value >= 1.
	predicate := "metric_kind == 'cycle_member' && value >= 1"

	body := map[string]interface{}{
		"predicate":     predicate,
		"metric_sample": s.metricSample,
	}
	statusCode, respBody, err := s.ruleEnginePost("/v1/predicates/evaluate", body)
	if err != nil {
		return fmt.Errorf("evaluating predicate: %w", err)
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("POST /v1/predicates/evaluate returned %d: %s",
			statusCode, string(respBody))
	}

	var result struct {
		Result bool `json:"result"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("decoding evaluate response: %w (body: %s)", err, string(respBody))
	}
	s.evalResult = &result.Result
	return nil
}

func (s *decouplingRulePackState) itReturnsTrue() error {
	if s.evalResult == nil {
		return fmt.Errorf("predicate was not evaluated — evalResult is nil")
	}
	if !*s.evalResult {
		return fmt.Errorf("expected predicate to return true for cycle_member with value=1, got false")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_policy_steward_and_solid_rule_engine_decoupled_functional_areas_rule_pack(ctx *godog.ScenarioContext) {
	s := &decouplingRulePackState{}

	// Scenario: decoupling-loads
	ctx.Step(`^the three decoupling rulepack files$`, s.theThreeDecouplingRulepackFiles)
	ctx.Step(`^the Steward loads them$`, s.theStewardLoadsThem)
	ctx.Step(`^"pack='decoupling'" rule_packs exist with parsed predicates$`, s.rulePacksExistWithParsedPredicates)

	// Scenario: cycles-rule-fires-on-cycle-member
	ctx.Step(`^a metric_sample with metric_kind "([^"]*)" and value (\d+)$`, s.aMetricSampleWithMetricKindAndValue)
	ctx.Step(`^the predicate evaluates$`, s.thePredicateEvaluates)
	ctx.Step(`^it returns true$`, s.itReturnsTrue)
}

func TestE2E_policy_steward_and_solid_rule_engine_decoupled_functional_areas_rule_pack(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_policy_steward_and_solid_rule_engine_decoupled_functional_areas_rule_pack,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"policy_steward_and_solid_rule_engine_decoupled_functional_areas_rule_pack.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
