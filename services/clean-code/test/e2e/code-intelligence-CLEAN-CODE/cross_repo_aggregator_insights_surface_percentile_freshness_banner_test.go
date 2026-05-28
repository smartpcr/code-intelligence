//go:build e2e

package e2e

import (
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
	"github.com/lib/pq"
)

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
// helpers
// ---------------------------------------------------------------------------

func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return db, nil
}

func httpGetJSON(url string) (map[string]interface{}, int, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, 0, fmt.Errorf("GET %s failed: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response body: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("unmarshalling JSON: %w (body: %s)", err, string(body))
	}
	return result, resp.StatusCode, nil
}

func httpPostJSON(url string, payload interface{}) (map[string]interface{}, int, error) {
	var bodyReader io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, 0, fmt.Errorf("marshalling payload: %w", err)
		}
		bodyReader = strings.NewReader(string(data))
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Post(url, "application/json", bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("POST %s failed: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response body: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("unmarshalling JSON: %w (body: %s)", err, string(body))
	}
	return result, resp.StatusCode, nil
}

// ---------------------------------------------------------------------------
// scenario state
// ---------------------------------------------------------------------------

type freshnessBannerState struct {
	db           *sql.DB
	mgmtURL      string
	evaluatorURL string

	// The metric_kind and scope_kind used in test rows.
	metricKind string
	scopeKind  string

	// Freshness window (seconds) read from env or defaulted.
	freshnessWindowSeconds int

	// Response from mgmt.read.cross_repo.
	crossRepoResponse map[string]interface{}

	// Gate code-path test state.
	gateScenarioStart time.Time
	gateTestSHAs      []string
}

func newFreshnessBannerState(pgDSN, mgmtURL, evaluatorURL string, freshnessWindow int) (*freshnessBannerState, error) {
	db, err := openDB(pgDSN)
	if err != nil {
		return nil, err
	}
	return &freshnessBannerState{
		db:                     db,
		mgmtURL:                strings.TrimRight(mgmtURL, "/"),
		evaluatorURL:           strings.TrimRight(evaluatorURL, "/"),
		metricKind:             "lcom4",
		scopeKind:              "class",
		freshnessWindowSeconds: freshnessWindow,
	}, nil
}

func (s *freshnessBannerState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

func (s *freshnessBannerState) cleanup() {
	if s.db == nil {
		return
	}
	ctx := context.Background()
	_, _ = s.db.ExecContext(ctx,
		`DELETE FROM clean_code.cross_repo_percentile WHERE metric_kind = $1 AND scope_kind = $2`,
		s.metricKind, s.scopeKind)
}

// ---------------------------------------------------------------------------
// Background step
// ---------------------------------------------------------------------------

func (s *freshnessBannerState) aRunningManagementSurfaceConnectedToPostgreSQL() error {
	healthURL := s.mgmtURL + "/healthz"
	client := &http.Client{Timeout: 10 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("creating healthz request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("management surface at %s did not become healthy within 30s", s.mgmtURL)
		}
		time.Sleep(time.Second)
	}
}

// ---------------------------------------------------------------------------
// Scenario: stale-percentile-banner-on-insights
// ---------------------------------------------------------------------------

func (s *freshnessBannerState) aCrossRepoPercentileRowWithBuiltAtOlderThanFreshnessWindow() error {
	s.cleanup()

	staleTime := time.Now().UTC().Add(-time.Duration(s.freshnessWindowSeconds+300) * time.Second)

	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.cross_repo_percentile
			(metric_kind, scope_kind, p50, p90, p99, histogram_json, built_at)
		VALUES ($1, $2, 5.0, 12.0, 25.0, '{"buckets":[0,5,10],"counts":[3,5,2]}'::jsonb, $3)
		ON CONFLICT (metric_kind, scope_kind)
		DO UPDATE SET p50 = 5.0, p90 = 12.0, p99 = 25.0,
			histogram_json = '{"buckets":[0,5,10],"counts":[3,5,2]}'::jsonb,
			built_at = $3
	`, s.metricKind, s.scopeKind, staleTime)
	if err != nil {
		return fmt.Errorf("inserting stale cross_repo_percentile row: %w", err)
	}
	return nil
}

func (s *freshnessBannerState) aCrossRepoPercentileRowWithBuiltAtWithinFreshnessWindow() error {
	s.cleanup()

	freshTime := time.Now().UTC().Add(-10 * time.Second)

	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.cross_repo_percentile
			(metric_kind, scope_kind, p50, p90, p99, histogram_json, built_at)
		VALUES ($1, $2, 6.5, 14.2, 28.7, '{"buckets":[0,5,10,15],"counts":[12,45,30,8]}'::jsonb, $3)
		ON CONFLICT (metric_kind, scope_kind)
		DO UPDATE SET p50 = 6.5, p90 = 14.2, p99 = 28.7,
			histogram_json = '{"buckets":[0,5,10,15],"counts":[12,45,30,8]}'::jsonb,
			built_at = $3
	`, s.metricKind, s.scopeKind, freshTime)
	if err != nil {
		return fmt.Errorf("inserting fresh cross_repo_percentile row: %w", err)
	}
	return nil
}

func (s *freshnessBannerState) theMgmtReadCrossRepoEndpointIsCalled() error {
	url := fmt.Sprintf("%s/api/v1/cross-repo?metric_kind=%s&scope_kind=%s",
		s.mgmtURL, s.metricKind, s.scopeKind)

	result, statusCode, err := httpGetJSON(url)
	if err != nil {
		return err
	}
	if statusCode != http.StatusOK {
		return fmt.Errorf("expected HTTP 200, got %d", statusCode)
	}
	s.crossRepoResponse = result
	return nil
}

func (s *freshnessBannerState) theResponseEnvelopeCarriesDegradedEqualTo(expected string) error {
	if s.crossRepoResponse == nil {
		return fmt.Errorf("no cross_repo response available")
	}

	degradedVal, exists := s.crossRepoResponse["degraded"]
	if !exists {
		return fmt.Errorf("response does not contain 'degraded' field; response: %v", s.crossRepoResponse)
	}

	degradedBool, ok := degradedVal.(bool)
	if !ok {
		return fmt.Errorf("'degraded' field is not a boolean: %v", degradedVal)
	}

	expectedBool := expected == "true"
	if degradedBool != expectedBool {
		return fmt.Errorf("expected degraded=%v, got %v", expectedBool, degradedBool)
	}
	return nil
}

func (s *freshnessBannerState) theResponseEnvelopeCarriesDegradedReasonEqualTo(expected string) error {
	if s.crossRepoResponse == nil {
		return fmt.Errorf("no cross_repo response available")
	}

	reasonVal, exists := s.crossRepoResponse["degraded_reason"]
	if !exists {
		return fmt.Errorf("response does not contain 'degraded_reason' field; response: %v", s.crossRepoResponse)
	}

	reasonStr, ok := reasonVal.(string)
	if !ok {
		return fmt.Errorf("'degraded_reason' field is not a string: %v", reasonVal)
	}

	if reasonStr != expected {
		return fmt.Errorf("expected degraded_reason=%q, got %q", expected, reasonStr)
	}
	return nil
}

func (s *freshnessBannerState) theResponseEnvelopeDoesNotContainADegradedReasonField() error {
	if s.crossRepoResponse == nil {
		return fmt.Errorf("no cross_repo response available")
	}

	_, exists := s.crossRepoResponse["degraded_reason"]
	if exists {
		return fmt.Errorf("response should not contain 'degraded_reason' field, but it does: %v",
			s.crossRepoResponse["degraded_reason"])
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: gate-never-emits-percentile-stale
//
// The acceptance criterion requires that across ANY code path through
// eval.gate, the degraded_reason is never "percentile_stale". We exercise
// multiple eval.gate paths (clean-pass, samples_pending degraded,
// signature_invalid degraded) and then query evaluation_verdict rows to
// assert that none carry degraded_reason = 'percentile_stale'.
// ---------------------------------------------------------------------------

// callEvalGate posts to the evaluator gate endpoint and returns the HTTP
// status code and decoded JSON response body.
func callEvalGate(evaluatorURL, sha, repoID, policyVersionID string, extras map[string]interface{}) (int, map[string]interface{}, error) {
	payload := map[string]interface{}{
		"sha":               sha,
		"repo_id":           repoID,
		"policy_version_id": policyVersionID,
	}
	for k, v := range extras {
		payload[k] = v
	}

	result, statusCode, err := httpPostJSON(evaluatorURL+"/v1/eval/gate", payload)
	return statusCode, result, err
}

func (s *freshnessBannerState) anEvalGateServiceConnectedToPostgreSQL() error {
	// Verify evaluator is healthy.
	healthURL := s.evaluatorURL + "/healthz"
	client := &http.Client{Timeout: 10 * time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("creating healthz request: %w", err)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return nil
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("evaluator at %s did not become healthy within 30s", s.evaluatorURL)
		}
		time.Sleep(time.Second)
	}
}

func (s *freshnessBannerState) evalGateIsCalledThroughEveryDegradedCodePath() error {
	// Record scenario start time to scope DB queries.
	s.gateScenarioStart = time.Now().UTC()

	// Look up a seeded repo and active policy version from the DB so that
	// eval.gate has real FK references to work with.
	var repoID, policyVersionID string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT repo_id FROM clean_code.repo LIMIT 1`).Scan(&repoID)
	if err != nil {
		// Fall back to non-schema-qualified table name.
		err = s.db.QueryRowContext(context.Background(),
			`SELECT id FROM repository LIMIT 1`).Scan(&repoID)
		if err != nil {
			return fmt.Errorf("querying seeded repo: %w", err)
		}
	}

	err = s.db.QueryRowContext(context.Background(),
		`SELECT id FROM clean_code.policy_version WHERE active = true LIMIT 1`).Scan(&policyVersionID)
	if err != nil {
		err = s.db.QueryRowContext(context.Background(),
			`SELECT id FROM policy_version WHERE active = true LIMIT 1`).Scan(&policyVersionID)
		if err != nil {
			return fmt.Errorf("querying active policy_version: %w", err)
		}
	}

	// Path 1: clean-pass — provide a SHA that has metric samples.
	sha1 := fmt.Sprintf("freshness-gate-clean-%d", time.Now().UnixNano())
	s.gateTestSHAs = append(s.gateTestSHAs, sha1)
	seedMetricSamplesForGate(s.db, repoID, sha1)
	if _, _, err = callEvalGate(s.evaluatorURL, sha1, repoID, policyVersionID, nil); err != nil {
		return fmt.Errorf("eval.gate call failed for clean-pass path (sha=%s): %w", sha1, err)
	}

	// Path 2: samples_pending degraded — use a SHA with no samples.
	sha2 := fmt.Sprintf("freshness-gate-degraded-%d", time.Now().UnixNano())
	s.gateTestSHAs = append(s.gateTestSHAs, sha2)
	if _, _, err = callEvalGate(s.evaluatorURL, sha2, repoID, policyVersionID, nil); err != nil {
		return fmt.Errorf("eval.gate call failed for samples_pending degraded path (sha=%s): %w", sha2, err)
	}

	// Path 3: signature_invalid degraded — pass an invalid policy signature.
	sha3 := fmt.Sprintf("freshness-gate-siginv-%d", time.Now().UnixNano())
	s.gateTestSHAs = append(s.gateTestSHAs, sha3)
	if _, _, err = callEvalGate(s.evaluatorURL, sha3, repoID, policyVersionID,
		map[string]interface{}{"signature": "INVALID-SIGNATURE"}); err != nil {
		return fmt.Errorf("eval.gate call failed for signature_invalid degraded path (sha=%s): %w", sha3, err)
	}

	return nil
}

// seedMetricSamplesForGate inserts minimal metric samples so that eval.gate
// treats the SHA as having samples present (non-degraded).
func seedMetricSamplesForGate(db *sql.DB, repoID, sha string) {
	ctx := context.Background()
	// Try schema-qualified first, then fall back.
	_, err := db.ExecContext(ctx, `
		INSERT INTO clean_code.metric_sample
			(repo_id, sha, scope_id, metric_kind, metric_version, value, pack, source, producer_run_id)
		VALUES ($1, $2, '00000000-0000-0000-0000-000000000001', 'lcom4', 1, 3.0, 'base', 'computed', '00000000-0000-0000-0000-000000000001')
		ON CONFLICT DO NOTHING
	`, repoID, sha)
	if err != nil {
		// Fall back to simpler schema.
		_, _ = db.ExecContext(ctx, `
			INSERT INTO metric_sample (repo_id, sha, metric_name, value, created_at)
			VALUES ($1, $2, 'cyclomatic_complexity', 5.0, NOW())
			ON CONFLICT DO NOTHING
		`, repoID, sha)
	}
}

func (s *freshnessBannerState) noneOfTheEvaluationVerdictRowsContainDegradedReason(forbidden string) error {
	if len(s.gateTestSHAs) == 0 {
		return fmt.Errorf("no gate test SHAs recorded; eval.gate paths were not exercised")
	}

	ctx := context.Background()

	// Query evaluation_verdict rows written after scenario start for the
	// SHAs we tested, checking if any carry the forbidden degraded_reason.
	// Try schema-qualified first, then fall back.
	query := `
		SELECT ev.degraded_reason
		FROM clean_code.evaluation_verdict ev
		JOIN clean_code.evaluation_run er ON ev.evaluation_run_id = er.id
		WHERE er.sha = ANY($1)
		  AND er.created_at >= $2
		  AND ev.degraded_reason = $3
	`
	rows, err := s.db.QueryContext(ctx, query, pq.Array(s.gateTestSHAs), s.gateScenarioStart, forbidden)
	if err != nil {
		// Fall back to non-schema-qualified.
		query = `
			SELECT ev.degraded_reason
			FROM evaluation_verdict ev
			JOIN evaluation_run er ON ev.evaluation_run_id = er.id
			WHERE er.sha = ANY($1)
			  AND er.created_at >= $2
			  AND ev.degraded_reason = $3
		`
		rows, err = s.db.QueryContext(ctx, query, pq.Array(s.gateTestSHAs), s.gateScenarioStart, forbidden)
		if err != nil {
			return fmt.Errorf("querying evaluation_verdict for forbidden degraded_reason: %w", err)
		}
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating evaluation_verdict rows: %w", err)
	}

	if count > 0 {
		return fmt.Errorf("found %d evaluation_verdict row(s) with degraded_reason=%q across eval.gate code paths; expected zero", count, forbidden)
	}

	// Also verify that at least some verdict rows were written, confirming
	// that the eval.gate paths were actually exercised.
	var totalVerdicts int
	verifyQuery := `
		SELECT COUNT(*)
		FROM clean_code.evaluation_verdict ev
		JOIN clean_code.evaluation_run er ON ev.evaluation_run_id = er.id
		WHERE er.sha = ANY($1)
		  AND er.created_at >= $2
	`
	err = s.db.QueryRowContext(ctx, verifyQuery, pq.Array(s.gateTestSHAs), s.gateScenarioStart).Scan(&totalVerdicts)
	if err != nil {
		verifyQuery = `
			SELECT COUNT(*)
			FROM evaluation_verdict ev
			JOIN evaluation_run er ON ev.evaluation_run_id = er.id
			WHERE er.sha = ANY($1)
			  AND er.created_at >= $2
		`
		err = s.db.QueryRowContext(ctx, verifyQuery, pq.Array(s.gateTestSHAs), s.gateScenarioStart).Scan(&totalVerdicts)
		if err != nil {
			return fmt.Errorf("verifying verdict rows were written: %w", err)
		}
	}

	if totalVerdicts == 0 {
		return fmt.Errorf("no evaluation_verdict rows found for tested SHAs %v; eval.gate paths may not have written verdicts", s.gateTestSHAs)
	}

	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_cross_repo_aggregator_insights_surface_percentile_freshness_banner
// registers all Given/When/Then steps for the
// insights-surface-percentile-freshness-banner stage.
func InitializeScenario_cross_repo_aggregator_insights_surface_percentile_freshness_banner(ctx *godog.ScenarioContext) {
	var state *freshnessBannerState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		pgDSN := os.Getenv("CLEAN_CODE_PG_URL")
		if pgDSN == "" {
			return ctx, fmt.Errorf("CLEAN_CODE_PG_URL is not set")
		}

		mgmtURL := os.Getenv("CLEAN_CODE_MGMT_URL")
		if mgmtURL == "" {
			mgmtURL = "http://localhost:8086"
		}

		evaluatorURL := os.Getenv("CLEAN_CODE_EVALUATOR_URL")
		if evaluatorURL == "" {
			evaluatorURL = "http://localhost:8087"
		}

		freshnessWindow := 3600 // default 1 hour
		if v := os.Getenv("CLEAN_CODE_FRESHNESS_WINDOW_SECONDS"); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &freshnessWindow); err != nil {
				return ctx, fmt.Errorf("parsing CLEAN_CODE_FRESHNESS_WINDOW_SECONDS: %w", err)
			}
		}

		var err error
		state, err = newFreshnessBannerState(pgDSN, mgmtURL, evaluatorURL, freshnessWindow)
		if err != nil {
			return ctx, err
		}

		return ctx, nil
	})

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if state != nil {
			state.cleanup()
			state.close()
		}
		return ctx, nil
	})

	// Background
	ctx.Step(`^a running management surface connected to PostgreSQL$`, func() error {
		return state.aRunningManagementSurfaceConnectedToPostgreSQL()
	})

	// stale-percentile-banner-on-insights
	ctx.Step(`^a cross_repo_percentile row with built_at older than the freshness window$`, func() error {
		return state.aCrossRepoPercentileRowWithBuiltAtOlderThanFreshnessWindow()
	})

	// fresh-percentile-no-banner
	ctx.Step(`^a cross_repo_percentile row with built_at within the freshness window$`, func() error {
		return state.aCrossRepoPercentileRowWithBuiltAtWithinFreshnessWindow()
	})

	// shared When
	ctx.Step(`^the mgmt\.read\.cross_repo endpoint is called$`, func() error {
		return state.theMgmtReadCrossRepoEndpointIsCalled()
	})

	// shared Then — degraded flag
	ctx.Step(`^the response envelope carries degraded equal to (true|false)$`, func(val string) error {
		return state.theResponseEnvelopeCarriesDegradedEqualTo(val)
	})

	// stale Then — degraded_reason
	ctx.Step(`^the response envelope carries degraded_reason equal to "([^"]*)"$`, func(reason string) error {
		return state.theResponseEnvelopeCarriesDegradedReasonEqualTo(reason)
	})

	// fresh Then — no degraded_reason
	ctx.Step(`^the response envelope does not contain a degraded_reason field$`, func() error {
		return state.theResponseEnvelopeDoesNotContainADegradedReasonField()
	})

	// gate-never-emits-percentile-stale
	ctx.Step(`^an eval\.gate service connected to PostgreSQL$`, func() error {
		return state.anEvalGateServiceConnectedToPostgreSQL()
	})
	ctx.Step(`^eval\.gate is called through every degraded code path$`, func() error {
		return state.evalGateIsCalledThroughEveryDegradedCodePath()
	})
	ctx.Step(`^none of the evaluation_verdict rows contain degraded_reason "([^"]*)"$`, func(reason string) error {
		return state.noneOfTheEvaluationVerdictRowsContainDegradedReason(reason)
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_cross_repo_aggregator_insights_surface_percentile_freshness_banner(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_cross_repo_aggregator_insights_surface_percentile_freshness_banner,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"cross_repo_aggregator_insights_surface_percentile_freshness_banner.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}
