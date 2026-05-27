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

// ---------- shared types ----------

// predicateDSLState carries state across Given/When/Then steps for
// the predicate-dsl-evaluator scenarios.
type predicateDSLState struct {
	ruleEngineURL string

	// dsl-rejects-unknown-metric-kind
	unknownMetricKind string
	predicateExpr     string
	parseStatusCode   int
	parseRespBody     string

	// dsl-deterministic
	deterministicPredicate string
	metricSample           map[string]interface{}
	evalResults            []bool
}

// ---------- helpers ----------

// postPredicate sends a predicate expression to the rule-engine's
// parse / validate endpoint and captures the response.
func (s *predicateDSLState) postPredicate(predicate string) (int, string, error) {
	body := map[string]interface{}{
		"predicate": predicate,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return 0, "", fmt.Errorf("marshalling predicate request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, s.ruleEngineURL+"/v1/predicates/validate", bytes.NewReader(jsonBody))
	if err != nil {
		return 0, "", fmt.Errorf("creating validate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("POST /v1/predicates/validate: %w", err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", fmt.Errorf("reading validate response: %w", err)
	}
	return resp.StatusCode, string(respBytes), nil
}

// evalPredicate sends a predicate expression and a MetricSample to
// the rule-engine's evaluate endpoint and returns the boolean result.
func (s *predicateDSLState) evalPredicate(predicate string, sample map[string]interface{}) (bool, error) {
	body := map[string]interface{}{
		"predicate":     predicate,
		"metric_sample": sample,
	}
	jsonBody, err := json.Marshal(body)
	if err != nil {
		return false, fmt.Errorf("marshalling evaluate request: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, s.ruleEngineURL+"/v1/predicates/evaluate", bytes.NewReader(jsonBody))
	if err != nil {
		return false, fmt.Errorf("creating evaluate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false, fmt.Errorf("POST /v1/predicates/evaluate: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBytes, _ := io.ReadAll(resp.Body)
		return false, fmt.Errorf("evaluate returned %d: %s", resp.StatusCode, string(respBytes))
	}

	var result struct {
		Result bool `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, fmt.Errorf("decoding evaluate response: %w", err)
	}
	return result.Result, nil
}

// ---------- Scenario: dsl-rejects-unknown-metric-kind ----------

func (s *predicateDSLState) aPredicateReferencingMetricKind(metricKind string) error {
	s.ruleEngineURL = os.Getenv("CLEAN_CODE_RULE_ENGINE_URL")
	if s.ruleEngineURL == "" {
		s.ruleEngineURL = "http://localhost:8083"
	}

	// Capture the metric kind from the Given step so the Then step can
	// assert against the same value that was supplied by the feature
	// file, rather than a hard-coded literal.
	s.unknownMetricKind = metricKind

	// Build a predicate expression that references the given (invalid) metric_kind.
	s.predicateExpr = fmt.Sprintf("metric_kind == '%s' && value > 100", metricKind)
	return nil
}

func (s *predicateDSLState) theParserRuns() error {
	statusCode, respBody, err := s.postPredicate(s.predicateExpr)
	if err != nil {
		return fmt.Errorf("running parser: %w", err)
	}
	s.parseStatusCode = statusCode
	s.parseRespBody = respBody
	return nil
}

func (s *predicateDSLState) itReturnsAValidationErrorNamingTheUnknownMetricKind() error {
	// Expect a 4xx status indicating validation failure.
	if s.parseStatusCode < 400 || s.parseStatusCode >= 500 {
		return fmt.Errorf("expected 4xx validation error, got %d; body: %s", s.parseStatusCode, s.parseRespBody)
	}

	if s.unknownMetricKind == "" {
		return fmt.Errorf("unknownMetricKind was not captured from the Given step; ensure the Given step runs before this Then step")
	}

	// The response body should mention the unknown metric_kind that was
	// supplied by the Given step. Compare case-insensitively because the
	// service may normalise case when echoing the offending value.
	lower := strings.ToLower(s.parseRespBody)
	expected := strings.ToLower(s.unknownMetricKind)
	if !strings.Contains(lower, expected) {
		return fmt.Errorf("expected response to name the unknown metric_kind %q, got: %s", s.unknownMetricKind, s.parseRespBody)
	}

	// Should also indicate it's a validation / unknown-metric error.
	errorIndicators := []string{"unknown", "invalid", "not recognized", "unsupported", "validation"}
	found := false
	for _, indicator := range errorIndicators {
		if strings.Contains(lower, indicator) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("expected a validation/unknown error message, got: %s", s.parseRespBody)
	}
	return nil
}

// ---------- Scenario: dsl-deterministic ----------

func (s *predicateDSLState) theSamePredicateAndTheSameMetricSampleInput() error {
	s.ruleEngineURL = os.Getenv("CLEAN_CODE_RULE_ENGINE_URL")
	if s.ruleEngineURL == "" {
		s.ruleEngineURL = "http://localhost:8083"
	}

	// Use a well-known canonical predicate and sample.
	s.deterministicPredicate = "metric_kind == 'cyclomatic_complexity' && value > 10"
	s.metricSample = map[string]interface{}{
		"metric_kind": "cyclomatic_complexity",
		"value":       25,
		"scope":       "function",
		"repo":        "repo-a",
	}
	s.evalResults = nil
	return nil
}

func (s *predicateDSLState) evaluatedTwice() error {
	for i := 0; i < 2; i++ {
		result, err := s.evalPredicate(s.deterministicPredicate, s.metricSample)
		if err != nil {
			return fmt.Errorf("evaluation run %d: %w", i+1, err)
		}
		s.evalResults = append(s.evalResults, result)
	}
	return nil
}

func (s *predicateDSLState) itReturnsTheSameBooleanResult() error {
	if len(s.evalResults) != 2 {
		return fmt.Errorf("expected 2 evaluation results, got %d", len(s.evalResults))
	}
	if s.evalResults[0] != s.evalResults[1] {
		return fmt.Errorf("determinism violated: first=%v, second=%v", s.evalResults[0], s.evalResults[1])
	}
	return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_policy_steward_and_solid_rule_engine_predicate_dsl_evaluator(ctx *godog.ScenarioContext) {
	s := &predicateDSLState{}

	// dsl-rejects-unknown-metric-kind
	ctx.Step(`^a predicate referencing metric_kind "([^"]*)"$`, s.aPredicateReferencingMetricKind)
	ctx.Step(`^the parser runs$`, s.theParserRuns)
	ctx.Step(`^it returns a validation error naming the unknown metric_kind$`, s.itReturnsAValidationErrorNamingTheUnknownMetricKind)

	// dsl-deterministic
	ctx.Step(`^the same predicate and the same MetricSample input$`, s.theSamePredicateAndTheSameMetricSampleInput)
	ctx.Step(`^evaluated twice$`, s.evaluatedTwice)
	ctx.Step(`^it returns the same boolean result$`, s.itReturnsTheSameBooleanResult)
}

func TestE2E_policy_steward_and_solid_rule_engine_predicate_dsl_evaluator(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_policy_steward_and_solid_rule_engine_predicate_dsl_evaluator,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"policy_steward_and_solid_rule_engine_predicate_dsl_evaluator.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
