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
// calling t.Skip when unset or empty. This is preferred over silently
// falling back to localhost defaults because it makes a missing CI
// configuration obvious in the test output instead of producing
// confusing connection errors against http://localhost.
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
	loadResp       *http.Response
	loadRespBody   string
	rulePacksFound []rulePackEntry

	// Scenario: cycles-rule-fires-on-cycle-member
	metricSample       map[string]interface{}
	cycleMemberRule    *rulePackEntry
	evaluatedPredicate string
	evalResult         *bool
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

// stewardGet sends a GET request to the policy-steward HTTP API.
// stewardURL is populated in TestE2E_* via requireEnv before the godog
// suite runs, so by the time any step calls this helper the URL is set.
func (s *decouplingRulePackState) stewardGet(path string) (int, []byte, error) {
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

// fetchDecouplingRulePacks queries the steward for all rule packs in
// the "decoupling" pack and populates s.rulePacksFound. It tolerates
// both wrapped ({"rule_packs":[...]}) and bare-array response shapes.
func (s *decouplingRulePackState) fetchDecouplingRulePacks() error {
	statusCode, body, err := s.stewardGet("/v1/rule_packs?pack=decoupling")
	if err != nil {
		return fmt.Errorf("querying rule_packs: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("GET /v1/rule_packs?pack=decoupling returned %d: %s",
			statusCode, string(body))
	}

	var resp struct {
		RulePacks []rulePackEntry `json:"rule_packs"`
	}
	if err := json.Unmarshal(body, &resp); err == nil && len(resp.RulePacks) > 0 {
		s.rulePacksFound = resp.RulePacks
		return nil
	}
	var packs []rulePackEntry
	if err2 := json.Unmarshal(body, &packs); err2 != nil {
		return fmt.Errorf("decoding rule_packs response: %w (body: %s)", err2, string(body))
	}
	s.rulePacksFound = packs
	return nil
}

// findCycleMemberRule locates the cycle-member rule inside the loaded
// decoupling rule pack. It matches by rule name first (which is the
// canonical identifier) and falls back to scanning predicates for the
// `cycle_member` metric_kind literal so the test is robust to small
// naming variations across rule pack revisions.
func findCycleMemberRule(packs []rulePackEntry) *rulePackEntry {
	for i := range packs {
		lowerName := strings.ToLower(packs[i].Name)
		if strings.Contains(lowerName, "cycle_member") || strings.Contains(lowerName, "cycle-member") {
			return &packs[i]
		}
	}
	for i := range packs {
		if strings.Contains(packs[i].Predicate, "'cycle_member'") ||
			strings.Contains(packs[i].Predicate, "\"cycle_member\"") {
			return &packs[i]
		}
	}
	return nil
}

// ======================================================================
// Scenario: decoupling-loads
// ======================================================================

func (s *decouplingRulePackState) theThreeDecouplingRulepackFiles() error {
	return nil
}

func (s *decouplingRulePackState) theStewardLoadsThem() error {
	// Query the steward for rule packs with pack='decoupling'.
	// The steward should have loaded these during startup from the
	// three decoupling rulepack files registered by seed-phase-05.
	return s.fetchDecouplingRulePacks()
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
	s.metricSample = map[string]interface{}{
		"metric_kind": metricKind,
		"value":       value,
		"scope":       "module",
		"repo":        "repo-a",
	}
	return nil
}

func (s *decouplingRulePackState) thePredicateEvaluates() error {
	// Godog instantiates a fresh decouplingRulePackState per scenario, so
	// `rulePacksFound` is empty here even though the `decoupling-loads`
	// scenario also populates it. We re-fetch from the steward so that
	// this step uses the ACTUAL predicate the rule pack registered —
	// without this, hardcoding the predicate would still pass even if
	// the rule pack failed to load or stored a broken predicate.
	if len(s.rulePacksFound) == 0 {
		if err := s.fetchDecouplingRulePacks(); err != nil {
			return fmt.Errorf("fetching decoupling rule packs for cycle-member predicate: %w", err)
		}
	}
	if len(s.rulePacksFound) == 0 {
		return fmt.Errorf("steward returned no decoupling rule_packs; cannot verify cycle-member rule")
	}

	s.cycleMemberRule = findCycleMemberRule(s.rulePacksFound)
	if s.cycleMemberRule == nil {
		names := make([]string, 0, len(s.rulePacksFound))
		for _, rp := range s.rulePacksFound {
			names = append(names, rp.Name)
		}
		return fmt.Errorf("could not find cycle-member rule in decoupling rule pack; %d rule(s) loaded: %v",
			len(s.rulePacksFound), names)
	}
	predicate := strings.TrimSpace(s.cycleMemberRule.Predicate)
	if predicate == "" {
		return fmt.Errorf("cycle-member rule %q has an empty predicate", s.cycleMemberRule.Name)
	}
	s.evaluatedPredicate = predicate

	reqBody := map[string]interface{}{
		"predicate":     predicate,
		"metric_sample": s.metricSample,
	}
	statusCode, respBody, err := s.ruleEnginePost("/v1/predicates/evaluate", reqBody)
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
		return fmt.Errorf("expected predicate %q (from cycle-member rule %q) to return true for cycle_member with value=1, got false",
			s.evaluatedPredicate,
			func() string {
				if s.cycleMemberRule != nil {
					return s.cycleMemberRule.Name
				}
				return "<unknown>"
			}())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_policy_steward_and_solid_rule_engine_decoupled_functional_areas_rule_pack(ctx *godog.ScenarioContext, stewardURL, ruleEngineURL string) {
	s := &decouplingRulePackState{
		stewardURL:    stewardURL,
		ruleEngineURL: ruleEngineURL,
	}

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
	// Resolve service URLs up front. requireEnv calls t.Skip with a
	// clear message when an env var is unset, so a misconfigured CI
	// run surfaces as a visible skip instead of silently running
	// against http://localhost:8082/8083 and producing confusing
	// connection-refused failures.
	stewardURL := requireEnv(t, "CLEAN_CODE_POLICY_STEWARD_URL")
	ruleEngineURL := requireEnv(t, "CLEAN_CODE_RULE_ENGINE_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: func(ctx *godog.ScenarioContext) {
			InitializeScenario_policy_steward_and_solid_rule_engine_decoupled_functional_areas_rule_pack(ctx, stewardURL, ruleEngineURL)
		},
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
