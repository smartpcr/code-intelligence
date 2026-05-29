//go:build e2e

package e2e

import (
	"context"
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

// requireEnv returns the value of the named environment variable or skips the
// test when the variable is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("required env var %s is not set", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Shared test state scoped to a single scenario.
// ---------------------------------------------------------------------------

type otelTelemetryState struct {
	evaluatorURL      string
	otelQueryURL      string
	aggregatorMetrics string

	// State captured between Given and When/Then.
	gateVerdict string
	traceID     string
	spanAttrs   map[string]string
	metricsBody string
}

func newOtelTelemetryState() *otelTelemetryState {
	evaluatorURL := os.Getenv("CLEAN_CODE_EVALUATOR_URL")
	if evaluatorURL == "" {
		evaluatorURL = "http://localhost:8081"
	}
	otelQueryURL := os.Getenv("CLEAN_CODE_OTEL_QUERY_URL")
	if otelQueryURL == "" {
		otelQueryURL = "http://localhost:16686"
	}
	aggregatorMetrics := os.Getenv("CLEAN_CODE_AGGREGATOR_METRICS_URL")
	if aggregatorMetrics == "" {
		aggregatorMetrics = "http://localhost:9090/metrics"
	}
	return &otelTelemetryState{
		evaluatorURL:      evaluatorURL,
		otelQueryURL:      otelQueryURL,
		aggregatorMetrics: aggregatorMetrics,
		spanAttrs:         make(map[string]string),
	}
}

// ---------------------------------------------------------------------------
// Step implementations – Scenario: gate-span-carries-verdict-tag
// ---------------------------------------------------------------------------

func (s *otelTelemetryState) evalGateReturning(operation, verdict string) error {
	s.gateVerdict = verdict

	// Invoke the evaluator gate endpoint to produce a span with the given verdict.
	reqBody := fmt.Sprintf(`{"operation":"%s","force_verdict":"%s"}`, operation, verdict)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.evaluatorURL+"/api/v1/gate", strings.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build gate request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("call eval.gate: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("eval.gate returned %d: %s", resp.StatusCode, string(body))
	}

	// Extract the trace ID from the response so we can query for the span.
	var gateResp struct {
		TraceID string `json:"trace_id"`
		Verdict string `json:"verdict"`
	}
	if err := json.Unmarshal(body, &gateResp); err != nil {
		return fmt.Errorf("parse gate response: %w", err)
	}
	s.traceID = gateResp.TraceID

	return nil
}

func (s *otelTelemetryState) otlpReceivesTheSpan() error {
	// Allow time for the span to be exported and indexed.
	time.Sleep(3 * time.Second)

	// Query the trace backend (Jaeger-compatible API) for the span.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var lastErr error
	// Poll for up to 30 seconds waiting for the span to appear.
	for attempt := 0; attempt < 10; attempt++ {
		url := fmt.Sprintf("%s/api/traces/%s", s.otelQueryURL, s.traceID)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return fmt.Errorf("build trace query: %w", err)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			attrs, err := extractSpanAttributes(body, "eval.gate")
			if err != nil {
				lastErr = err
				time.Sleep(2 * time.Second)
				continue
			}
			s.spanAttrs = attrs
			return nil
		}

		lastErr = fmt.Errorf("trace query returned %d: %s", resp.StatusCode, string(body))
		time.Sleep(2 * time.Second)
	}

	return fmt.Errorf("span for trace %s not found after polling: %v", s.traceID, lastErr)
}

// extractSpanAttributes parses a Jaeger trace response and returns the tag
// key-value pairs for the first span matching the given operation name.
func extractSpanAttributes(body []byte, operationName string) (map[string]string, error) {
	var result struct {
		Data []struct {
			Spans []struct {
				OperationName string `json:"operationName"`
				Tags          []struct {
					Key   string      `json:"key"`
					Value interface{} `json:"value"`
				} `json:"tags"`
			} `json:"spans"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("parse trace response: %w", err)
	}

	for _, trace := range result.Data {
		for _, span := range trace.Spans {
			if span.OperationName == operationName {
				attrs := make(map[string]string)
				for _, tag := range span.Tags {
					attrs[tag.Key] = fmt.Sprintf("%v", tag.Value)
				}
				return attrs, nil
			}
		}
	}

	return nil, fmt.Errorf("no span with operation %q found in trace response", operationName)
}

func (s *otelTelemetryState) theSpanCarriesTagEqualTo(key, expected string) error {
	actual, ok := s.spanAttrs[key]
	if !ok {
		return fmt.Errorf("span attribute %q not found; available attributes: %v", key, mapKeys(s.spanAttrs))
	}
	if actual != expected {
		return fmt.Errorf("span attribute %q: expected %q, got %q", key, expected, actual)
	}
	return nil
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ---------------------------------------------------------------------------
// Step implementations – Scenario: prometheus-counter-shape
// ---------------------------------------------------------------------------

func (s *otelTelemetryState) theAggregatorRunsOneTick() error {
	// Trigger one aggregator tick by calling its run endpoint.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.evaluatorURL+"/api/v1/aggregator/tick", nil)
	if err != nil {
		return fmt.Errorf("build tick request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("trigger aggregator tick: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("aggregator tick returned %d: %s", resp.StatusCode, string(body))
	}

	// Allow time for the metric to be recorded.
	time.Sleep(2 * time.Second)
	return nil
}

func (s *otelTelemetryState) metricsIsScraped() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.aggregatorMetrics, nil)
	if err != nil {
		return fmt.Errorf("build metrics request: %w", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("scrape /metrics: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read metrics body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("/metrics returned %d: %s", resp.StatusCode, string(body))
	}

	s.metricsBody = string(body)
	return nil
}

func (s *otelTelemetryState) metricExistsWithExpectedBucketLabels(metricName string) error {
	if s.metricsBody == "" {
		return fmt.Errorf("metrics body is empty; was /metrics scraped?")
	}

	// Strictly require histogram TYPE declaration — summaries are not accepted.
	typeDecl := fmt.Sprintf("# TYPE %s histogram", metricName)
	if !strings.Contains(s.metricsBody, typeDecl) {
		return fmt.Errorf("metric %q must be declared as histogram (expected %q in /metrics output)", metricName, typeDecl)
	}

	// Require at least one _bucket line with a le="..." label (histogram buckets).
	bucketPrefix := metricName + `_bucket{le="`
	if !strings.Contains(s.metricsBody, bucketPrefix) {
		return fmt.Errorf("no histogram bucket lines with le labels found for metric %q (expected lines matching %s)", metricName, bucketPrefix)
	}

	// Require the +Inf bucket (every Prometheus histogram must expose it).
	infBucket := metricName + `_bucket{le="+Inf"}`
	if !strings.Contains(s.metricsBody, infBucket) {
		return fmt.Errorf("missing +Inf bucket for metric %q (expected %s)", metricName, infBucket)
	}

	// Require _sum and _count companion metrics (mandatory for histograms).
	sumMetric := metricName + "_sum"
	countMetric := metricName + "_count"
	if !strings.Contains(s.metricsBody, sumMetric) {
		return fmt.Errorf("companion metric %q not found in /metrics output", sumMetric)
	}
	if !strings.Contains(s.metricsBody, countMetric) {
		return fmt.Errorf("companion metric %q not found in /metrics output", countMetric)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Scenario initializer & test entrypoint
// ---------------------------------------------------------------------------

func InitializeScenario_audit_wal_and_reliability_hardening_otel_telemetry_across_all_surfaces(ctx *godog.ScenarioContext) {
	state := newOtelTelemetryState()

	// Scenario: gate-span-carries-verdict-tag
	ctx.Step(`^"([^"]*)" returning "([^"]*)"$`, state.evalGateReturning)
	ctx.Step(`^OTLP receives the span$`, state.otlpReceivesTheSpan)
	ctx.Step(`^the span carries "([^"]*)" equal to "([^"]*)"$`, state.theSpanCarriesTagEqualTo)

	// Scenario: prometheus-counter-shape
	ctx.Step(`^the aggregator runs one tick$`, state.theAggregatorRunsOneTick)
	ctx.Step(`^"/metrics" is scraped$`, state.metricsIsScraped)
	ctx.Step(`^"([^"]*)" exists with the expected bucket labels$`, state.metricExistsWithExpectedBucketLabels)
}

func TestE2E_audit_wal_and_reliability_hardening_otel_telemetry_across_all_surfaces(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_OTEL_ENDPOINT")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_audit_wal_and_reliability_hardening_otel_telemetry_across_all_surfaces,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"audit_wal_and_reliability_hardening_otel_telemetry_across_all_surfaces.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("e2e suite failed")
	}
}
