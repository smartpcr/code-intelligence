//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
// Shared state for optional-agent-memory-linked-mode-adapter scenarios
// ---------------------------------------------------------------------------

type linkedModeAdapterState struct {
	aggregatorURL    string
	agentMemoryURL   string
	agentMemoryAlive bool

	composeMetric string
	composeResp   *http.Response
	composeBody   []byte
	composeResult map[string]interface{}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *linkedModeAdapterState) ensureAggregatorURL() {
	if s.aggregatorURL != "" {
		return
	}
	s.aggregatorURL = os.Getenv("CLEAN_CODE_AGGREGATOR_URL")
	if s.aggregatorURL == "" {
		s.aggregatorURL = "http://localhost:8085"
	}
}

func (s *linkedModeAdapterState) ensureAgentMemoryURL() {
	if s.agentMemoryURL != "" {
		return
	}
	s.agentMemoryURL = os.Getenv("CLEAN_CODE_AGENT_MEMORY_URL")
	if s.agentMemoryURL == "" {
		s.agentMemoryURL = "http://localhost:8090"
	}
}

func (s *linkedModeAdapterState) httpJSON(method, url string, body interface{}) (*http.Response, []byte, error) {
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

// ---------------------------------------------------------------------------
// Scenario: linked-mode-uses-edges
// ---------------------------------------------------------------------------

func (s *linkedModeAdapterState) linkedModeEnabledWithReachableAgentMemory() error {
	s.ensureAggregatorURL()
	s.ensureAgentMemoryURL()
	s.agentMemoryAlive = true

	// Configure the aggregator to use linked mode with the reachable agent-memory URL.
	payload := map[string]interface{}{
		"linked_mode":      true,
		"agent_memory_url": s.agentMemoryURL,
	}
	resp, body, err := s.httpJSON(http.MethodPut, s.aggregatorURL+"/v1/config/linked-mode", payload)
	if err != nil {
		return fmt.Errorf("PUT linked-mode config: %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("linked-mode config returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (s *linkedModeAdapterState) theAggregatorComposesMetric(metric string) error {
	s.ensureAggregatorURL()
	s.composeMetric = metric

	payload := map[string]interface{}{
		"metric": metric,
	}
	resp, body, err := s.httpJSON(http.MethodPost, s.aggregatorURL+"/v1/compose", payload)
	if err != nil {
		return fmt.Errorf("POST compose: %w", err)
	}
	s.composeResp = resp
	s.composeBody = body
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("compose returned %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parsing compose response: %w (body: %s)", err, string(body))
	}
	s.composeResult = result
	return nil
}

func (s *linkedModeAdapterState) xrepoEdgesAreFactoredIntoTheResult() error {
	if s.composeResult == nil {
		return fmt.Errorf("no compose result captured")
	}

	// Verify that the result contains xrepo edge data.
	xrepoEdges, ok := s.composeResult["xrepo_edges_included"]
	if !ok {
		return fmt.Errorf("compose result missing 'xrepo_edges_included' field (body: %s)", string(s.composeBody))
	}
	included, ok := xrepoEdges.(bool)
	if !ok {
		return fmt.Errorf("xrepo_edges_included is not a bool: %v", xrepoEdges)
	}
	if !included {
		return fmt.Errorf("expected xrepo_edges_included=true, got false (body: %s)", string(s.composeBody))
	}
	return nil
}

func (s *linkedModeAdapterState) theOutputHasDegradedEqualTo(expected string) error {
	if s.composeResult == nil {
		return fmt.Errorf("no compose result captured")
	}

	degradedVal, ok := s.composeResult["degraded"]
	if !ok {
		return fmt.Errorf("compose result missing 'degraded' field (body: %s)", string(s.composeBody))
	}
	degradedBool, ok := degradedVal.(bool)
	if !ok {
		return fmt.Errorf("degraded is not a bool: %v", degradedVal)
	}
	expectedBool := expected == "true"
	if degradedBool != expectedBool {
		return fmt.Errorf("expected degraded=%s, got %v (body: %s)", expected, degradedBool, string(s.composeBody))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: linked-mode-unreachable-degrades
// ---------------------------------------------------------------------------

func (s *linkedModeAdapterState) linkedModeEnabledWithUnreachableAgentMemory() error {
	s.ensureAggregatorURL()
	s.agentMemoryAlive = false

	// Point the aggregator at an unreachable agent-memory URL.
	unreachableURL := "http://127.0.0.1:1" // port 1 is almost certainly unreachable
	payload := map[string]interface{}{
		"linked_mode":      true,
		"agent_memory_url": unreachableURL,
	}
	resp, body, err := s.httpJSON(http.MethodPut, s.aggregatorURL+"/v1/config/linked-mode", payload)
	if err != nil {
		return fmt.Errorf("PUT linked-mode config (unreachable): %w", err)
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("linked-mode config returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (s *linkedModeAdapterState) theOutputHasDegradedReasonEqualTo(expected string) error {
	if s.composeResult == nil {
		return fmt.Errorf("no compose result captured")
	}

	reasonVal, ok := s.composeResult["degraded_reason"]
	if !ok {
		return fmt.Errorf("compose result missing 'degraded_reason' field (body: %s)", string(s.composeBody))
	}
	reason, ok := reasonVal.(string)
	if !ok {
		return fmt.Errorf("degraded_reason is not a string: %v", reasonVal)
	}
	if reason != expected {
		return fmt.Errorf("expected degraded_reason=%q, got %q (body: %s)", expected, reason, string(s.composeBody))
	}
	return nil
}

// ======================================================================
// Godog wiring
// ======================================================================

func InitializeScenario_linked_mode_integration_and_rollout_optional_agent_memory_linked_mode_adapter(ctx *godog.ScenarioContext) {
	s := &linkedModeAdapterState{}

	// Restore the aggregator's linked-mode configuration after each scenario
	// so that mutations (e.g. pointing at an unreachable agent-memory URL)
	// do not leak into subsequent tests sharing the same aggregator instance.
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.ensureAggregatorURL()
		payload := map[string]interface{}{
			"linked_mode": false,
		}
		_, _, resetErr := s.httpJSON(http.MethodPut, s.aggregatorURL+"/v1/config/linked-mode", payload)
		if resetErr != nil {
			return ctx, fmt.Errorf("teardown: failed to reset linked-mode config: %w", resetErr)
		}
		return ctx, nil
	})

	// linked-mode-uses-edges
	ctx.Step(`^linked mode is enabled with a reachable agent-memory service$`, s.linkedModeEnabledWithReachableAgentMemory)
	ctx.Step(`^the aggregator composes "([^"]*)"$`, s.theAggregatorComposesMetric)
	ctx.Step(`^xrepo edges are factored into the result$`, s.xrepoEdgesAreFactoredIntoTheResult)
	ctx.Step(`^the output has degraded equal to "([^"]*)"$`, s.theOutputHasDegradedEqualTo)

	// linked-mode-unreachable-degrades
	ctx.Step(`^linked mode is enabled with an unreachable agent-memory service$`, s.linkedModeEnabledWithUnreachableAgentMemory)
	ctx.Step(`^the output has degraded_reason equal to "([^"]*)"$`, s.theOutputHasDegradedReasonEqualTo)
}

func TestE2E_linked_mode_integration_and_rollout_optional_agent_memory_linked_mode_adapter(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_AGGREGATOR_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_linked_mode_integration_and_rollout_optional_agent_memory_linked_mode_adapter,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"linked_mode_integration_and_rollout_optional_agent_memory_linked_mode_adapter.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}