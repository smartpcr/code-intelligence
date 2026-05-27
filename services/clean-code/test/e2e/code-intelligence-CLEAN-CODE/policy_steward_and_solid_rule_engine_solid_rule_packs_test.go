//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
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
// Canonical metric kinds — the exhaustive allowlist from the acceptance
// scenario.  Any metric_kind reference NOT in this set is a failure.
// ---------------------------------------------------------------------------

var solidCanonicalMetricKinds = map[string]bool{
	"lcom4":                        true,
	"fan_in":                       true,
	"fan_out":                      true,
	"depth_of_inheritance":         true,
	"interface_width":              true,
	"coupling_between_objects":     true,
	"modification_count_in_window": true,
}

// solidPrincipleNames is the set of five SOLID principle short-names
// that must each map to exactly one rule_pack row.
var solidPrincipleNames = map[string]bool{
	"srp": true,
	"ocp": true,
	"lsp": true,
	"isp": true,
	"dip": true,
}

// ---------------------------------------------------------------------------
// API response types — parsed from the steward's JSON response
// ---------------------------------------------------------------------------

// solidRulePackEntry matches the steward's /v1/rule_packs response shape.
// The `metric_kinds` and `inputs` fields carry the server-parsed predicate
// metadata so the test never re-parses predicate text locally.
type solidRulePackEntry struct {
	Pack        string   `json:"pack"`
	Name        string   `json:"name"`
	Predicate   string   `json:"predicate"`
	RulePackID  string   `json:"rule_pack_id"`
	MetricKinds []string `json:"metric_kinds"` // parsed by steward
	Inputs      []string `json:"inputs"`       // parsed by steward
	ParseError  string   `json:"parse_error"`  // non-empty ⇒ parse failed
}

// ---------------------------------------------------------------------------
// Shared state
// ---------------------------------------------------------------------------

type solidRulePackState struct {
	stewardURL string

	// all solid packs loaded from steward
	rulePacksFound []solidRulePackEntry

	// Scenario: solid-rulepacks-only-canonical-kinds
	allMetricKinds []string // union across all packs, from server

	// Scenario: ocp-uses-fan-in
	ocpPack *solidRulePackEntry
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *solidRulePackState) ensureStewardURL() {
	if s.stewardURL != "" {
		return
	}
	s.stewardURL = os.Getenv("CLEAN_CODE_POLICY_STEWARD_URL")
	if s.stewardURL == "" {
		s.stewardURL = "http://localhost:8082"
	}
}

func (s *solidRulePackState) stewardGet(path string) (int, []byte, error) {
	s.ensureStewardURL()
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(s.stewardURL + path)
	if err != nil {
		return 0, nil, fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

func (s *solidRulePackState) stewardPost(path string, payload interface{}) (int, []byte, error) {
	s.ensureStewardURL()
	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, fmt.Errorf("marshalling request: %w", err)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(s.stewardURL+path, "application/json", bytes.NewReader(jsonBody))
	if err != nil {
		return 0, nil, fmt.Errorf("POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body, nil
}

// loadSolidPacks fetches rule_packs with pack='solid' from the steward,
// caching the result for subsequent steps.
func (s *solidRulePackState) loadSolidPacks() error {
	if len(s.rulePacksFound) > 0 {
		return nil
	}
	statusCode, body, err := s.stewardGet("/v1/rule_packs?pack=solid")
	if err != nil {
		return fmt.Errorf("querying rule_packs: %w", err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("GET /v1/rule_packs?pack=solid returned %d: %s",
			statusCode, string(body))
	}

	// Try wrapped {"rule_packs":[…]} first, fall back to bare array.
	var resp struct {
		RulePacks []solidRulePackEntry `json:"rule_packs"`
	}
	if err := json.Unmarshal(body, &resp); err == nil && len(resp.RulePacks) > 0 {
		s.rulePacksFound = resp.RulePacks
	} else {
		var packs []solidRulePackEntry
		if err2 := json.Unmarshal(body, &packs); err2 != nil {
			return fmt.Errorf("decoding rule_packs response: %w (body: %s)", err2, string(body))
		}
		s.rulePacksFound = packs
	}

	// If the steward didn't return parsed metric_kinds / inputs inline,
	// call the per-pack parse endpoint to get the server-parsed metadata.
	for i := range s.rulePacksFound {
		rp := &s.rulePacksFound[i]
		if len(rp.MetricKinds) == 0 && rp.Predicate != "" {
			if err := s.enrichFromParseEndpoint(rp); err != nil {
				return err
			}
		}
	}
	return nil
}

// enrichFromParseEndpoint calls the steward's predicate parse endpoint
// to retrieve the server-authoritative metric_kinds and inputs for a
// single rule pack, so we never fall back to local regex extraction.
func (s *solidRulePackState) enrichFromParseEndpoint(rp *solidRulePackEntry) error {
	payload := map[string]string{"predicate": rp.Predicate}
	statusCode, body, err := s.stewardPost("/v1/predicates/parse", payload)
	if err != nil {
		return fmt.Errorf("parsing predicate for %q: %w", rp.Name, err)
	}
	if statusCode < 200 || statusCode >= 300 {
		return fmt.Errorf("POST /v1/predicates/parse for %q returned %d: %s",
			rp.Name, statusCode, string(body))
	}

	var parsed struct {
		MetricKinds []string `json:"metric_kinds"`
		Inputs      []string `json:"inputs"`
		ParseError  string   `json:"parse_error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("decoding parse response for %q: %w (body: %s)",
			rp.Name, err, string(body))
	}
	rp.MetricKinds = parsed.MetricKinds
	rp.Inputs = parsed.Inputs
	rp.ParseError = parsed.ParseError
	return nil
}

// findOCP locates the OCP rule pack by name.
func (s *solidRulePackState) findOCP() (*solidRulePackEntry, error) {
	for i, rp := range s.rulePacksFound {
		lower := strings.ToLower(rp.Name)
		if lower == "ocp" || strings.Contains(lower, "open-closed") ||
			strings.Contains(lower, "open_closed") ||
			strings.HasPrefix(lower, "ocp-") || strings.HasPrefix(lower, "ocp_") {
			return &s.rulePacksFound[i], nil
		}
	}
	names := make([]string, len(s.rulePacksFound))
	for i, rp := range s.rulePacksFound {
		names[i] = rp.Name
	}
	return nil, fmt.Errorf("OCP rule pack not found among %d packs: %v",
		len(s.rulePacksFound), names)
}

// normalizePrincipleKey extracts the SOLID principle key from a rule
// pack name.  e.g. "srp-cohesion" → "srp", "ocp" → "ocp".
func normalizePrincipleKey(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	for key := range solidPrincipleNames {
		if lower == key || strings.HasPrefix(lower, key+"-") ||
			strings.HasPrefix(lower, key+"_") {
			return key
		}
	}
	// Fallback: try matching full principle names
	fullNames := map[string]string{
		"single-responsibility": "srp", "single_responsibility": "srp",
		"open-closed": "ocp", "open_closed": "ocp",
		"liskov": "lsp",
		"interface-segregation": "isp", "interface_segregation": "isp",
		"dependency-inversion": "dip", "dependency_inversion": "dip",
	}
	for pattern, key := range fullNames {
		if strings.Contains(lower, pattern) {
			return key
		}
	}
	return lower
}

// ======================================================================
// Background
// ======================================================================

func (s *solidRulePackState) thePolicyStewardIsReachable() error {
	s.ensureStewardURL()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(s.stewardURL + "/healthz")
	if err != nil {
		return fmt.Errorf("steward not reachable at %s: %w", s.stewardURL, err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("steward health check returned %d", resp.StatusCode)
	}
	return nil
}

// ======================================================================
// Scenario: solid-rulepacks-load
// ======================================================================

func (s *solidRulePackState) theFiveSOLIDRulepackFilesFor(nameList string) error {
	// Parse the expected principle names from the step argument.
	// e.g. `"srp", "ocp", "lsp", "isp", "dip"`
	parts := strings.Split(nameList, ",")
	if len(parts) != 5 {
		return fmt.Errorf("expected 5 principle names, got %d from %q", len(parts), nameList)
	}
	for _, p := range parts {
		trimmed := strings.Trim(strings.TrimSpace(p), `"`)
		if !solidPrincipleNames[trimmed] {
			return fmt.Errorf("unexpected principle name %q", trimmed)
		}
	}
	return nil
}

func (s *solidRulePackState) thePolicyStewardLoadsThem() error {
	return s.loadSolidPacks()
}

func (s *solidRulePackState) exactlyNRulePackRowsExistWithPack(expected int, pack string) error {
	count := 0
	for _, rp := range s.rulePacksFound {
		if strings.EqualFold(rp.Pack, pack) {
			count++
		}
	}
	if count != expected {
		return fmt.Errorf("expected exactly %d rule_packs with pack=%q, got %d",
			expected, pack, count)
	}
	return nil
}

func (s *solidRulePackState) eachRulePackMapsToOneOf(nameList string) error {
	// Build expected set from the step argument.
	parts := strings.Split(nameList, ",")
	expected := make(map[string]bool, len(parts))
	for _, p := range parts {
		expected[strings.Trim(strings.TrimSpace(p), `"`)] = true
	}

	// Check that every rule_pack maps to exactly one expected principle.
	covered := make(map[string]bool)
	for _, rp := range s.rulePacksFound {
		key := normalizePrincipleKey(rp.Name)
		if !expected[key] {
			return fmt.Errorf("rule_pack %q (normalized: %q) does not map to any expected SOLID principle %v",
				rp.Name, key, parts)
		}
		covered[key] = true
	}

	// Verify all five principles are covered.
	for e := range expected {
		if !covered[e] {
			return fmt.Errorf("SOLID principle %q has no corresponding rule_pack row", e)
		}
	}
	return nil
}

func (s *solidRulePackState) everyRulePackHasNonEmptyPredicateThatParsesWithoutError() error {
	for _, rp := range s.rulePacksFound {
		if strings.TrimSpace(rp.Predicate) == "" {
			return fmt.Errorf("rule_pack %q has an empty predicate", rp.Name)
		}
		if rp.ParseError != "" {
			return fmt.Errorf("rule_pack %q has a parse error: %s", rp.Name, rp.ParseError)
		}
	}
	return nil
}

// ======================================================================
// Scenario: solid-rulepacks-only-canonical-kinds
// ======================================================================

func (s *solidRulePackState) theLoadedSOLIDRulePacks() error {
	return s.loadSolidPacks()
}

func (s *solidRulePackState) theStewardReturnsTheParsedMetricKindReferencesForEachPredicate() error {
	seen := make(map[string]bool)
	for _, rp := range s.rulePacksFound {
		for _, mk := range rp.MetricKinds {
			seen[mk] = true
		}
		// Also include inputs; they are metric_kind references too.
		for _, inp := range rp.Inputs {
			seen[inp] = true
		}
	}
	var kinds []string
	for k := range seen {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	s.allMetricKinds = kinds
	return nil
}

func (s *solidRulePackState) everyMetricKindIsOneOf(allowedCSV string) error {
	allowed := make(map[string]bool)
	for _, k := range strings.Split(allowedCSV, ",") {
		allowed[strings.TrimSpace(k)] = true
	}

	if len(s.allMetricKinds) == 0 {
		return fmt.Errorf("no metric_kind references were returned by the steward for any predicate")
	}

	var bad []string
	for _, mk := range s.allMetricKinds {
		if !allowed[mk] {
			bad = append(bad, mk)
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("non-canonical metric_kinds found: %v (allowed: %s)", bad, allowedCSV)
	}
	return nil
}

func (s *solidRulePackState) noNonCanonicalAliasAppearsInAnyPredicate() error {
	// Double-check: iterate every pack's MetricKinds field (server-parsed)
	// and reject anything outside the canonical set.
	var violations []string
	for _, rp := range s.rulePacksFound {
		for _, mk := range rp.MetricKinds {
			if !solidCanonicalMetricKinds[mk] {
				violations = append(violations, fmt.Sprintf("%s (in %q)", mk, rp.Name))
			}
		}
	}
	if len(violations) > 0 {
		return fmt.Errorf("non-canonical aliases found in parsed predicates: %v", violations)
	}
	return nil
}

// ======================================================================
// Scenario: ocp-uses-fan-in
// ======================================================================

func (s *solidRulePackState) theLoadedOCPRulepack() error {
	if err := s.loadSolidPacks(); err != nil {
		return err
	}
	ocp, err := s.findOCP()
	if err != nil {
		return err
	}
	s.ocpPack = ocp
	return nil
}

func (s *solidRulePackState) theStewardReturnsItsParsedInputSet() error {
	if s.ocpPack == nil {
		return fmt.Errorf("OCP rulepack not loaded")
	}
	if len(s.ocpPack.Inputs) == 0 && len(s.ocpPack.MetricKinds) == 0 {
		return fmt.Errorf("OCP rulepack has no parsed inputs or metric_kinds from steward")
	}
	return nil
}

func (s *solidRulePackState) theInputsAreExactlyFanInAndModificationCountInWindow() error {
	// Use the server-parsed Inputs field; fall back to MetricKinds.
	inputs := s.ocpPack.Inputs
	if len(inputs) == 0 {
		inputs = s.ocpPack.MetricKinds
	}

	expected := []string{"fan_in", "modification_count_in_window"}
	sort.Strings(expected)

	actual := make([]string, len(inputs))
	copy(actual, inputs)
	sort.Strings(actual)

	if len(actual) != len(expected) {
		return fmt.Errorf("expected OCP inputs %v, got %v", expected, actual)
	}
	for i := range expected {
		if actual[i] != expected[i] {
			return fmt.Errorf("expected OCP inputs %v, got %v", expected, actual)
		}
	}
	return nil
}

func (s *solidRulePackState) theInputDepthOfInheritanceIsNotPresent() error {
	inputs := s.ocpPack.Inputs
	if len(inputs) == 0 {
		inputs = s.ocpPack.MetricKinds
	}
	for _, inp := range inputs {
		if inp == "depth_of_inheritance" {
			return fmt.Errorf("OCP rulepack must NOT reference depth_of_inheritance " +
				"(iter-3 drift guard, item 14), but it was found in the parsed input set")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_policy_steward_and_solid_rule_engine_solid_rule_packs(ctx *godog.ScenarioContext) {
	s := &solidRulePackState{}

	// Background
	ctx.Step(`^the Policy Steward is reachable$`, s.thePolicyStewardIsReachable)

	// Scenario: solid-rulepacks-load
	ctx.Step(`^the five SOLID rulepack files for "([^"]*)"$`,
		s.theFiveSOLIDRulepackFilesFor)
	ctx.Step(`^the Policy Steward loads them$`,
		s.thePolicyStewardLoadsThem)
	ctx.Step(`^exactly (\d+) rule_pack rows exist with pack "([^"]*)"$`,
		s.exactlyNRulePackRowsExistWithPack)
	ctx.Step(`^each rule_pack maps to one of (.+)$`,
		s.eachRulePackMapsToOneOf)
	ctx.Step(`^every rule_pack has a non-empty predicate that parses without error$`,
		s.everyRulePackHasNonEmptyPredicateThatParsesWithoutError)

	// Scenario: solid-rulepacks-only-canonical-kinds
	ctx.Step(`^the loaded SOLID rule_packs$`,
		s.theLoadedSOLIDRulePacks)
	ctx.Step(`^the steward returns the parsed metric_kind references for each predicate$`,
		s.theStewardReturnsTheParsedMetricKindReferencesForEachPredicate)
	ctx.Step(`^every metric_kind is one of "([^"]*)"$`,
		s.everyMetricKindIsOneOf)
	ctx.Step(`^no non-canonical alias appears in any predicate$`,
		s.noNonCanonicalAliasAppearsInAnyPredicate)

	// Scenario: ocp-uses-fan-in
	ctx.Step(`^the loaded OCP rulepack$`,
		s.theLoadedOCPRulepack)
	ctx.Step(`^the steward returns its parsed input set$`,
		s.theStewardReturnsItsParsedInputSet)
	ctx.Step(`^the inputs are exactly "fan_in" and "modification_count_in_window"$`,
		s.theInputsAreExactlyFanInAndModificationCountInWindow)
	ctx.Step(`^the input "depth_of_inheritance" is not present$`,
		s.theInputDepthOfInheritanceIsNotPresent)
}

func TestE2E_policy_steward_and_solid_rule_engine_solid_rule_packs(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_policy_steward_and_solid_rule_engine_solid_rule_packs,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"policy_steward_and_solid_rule_engine_solid_rule_packs.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
