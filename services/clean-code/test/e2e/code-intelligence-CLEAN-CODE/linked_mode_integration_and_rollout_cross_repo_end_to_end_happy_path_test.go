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

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

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

// openDB opens a PostgreSQL connection using the given DSN and verifies
// connectivity.
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

// ---------------------------------------------------------------------------
// scenario state
// ---------------------------------------------------------------------------

type crossRepoE2EState struct {
	db             *sql.DB
	gatewayURL     string
	repoIDs        []string
	crossRepoResp  map[string]interface{}
	evalGateResp   map[string]interface{}
	staleCrossRepo map[string]interface{}
	staleEvalGate  map[string]interface{}
	isStale        bool
}

func newCrossRepoE2EState(dsn, gatewayURL string) (*crossRepoE2EState, error) {
	db, err := openDB(dsn)
	if err != nil {
		return nil, err
	}
	return &crossRepoE2EState{db: db, gatewayURL: gatewayURL}, nil
}

func (s *crossRepoE2EState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

func (s *crossRepoE2EState) cleanup() {
	if s.db == nil {
		return
	}
	ctx := context.Background()

	// Delete cross_repo_percentile rows scoped to test-owned repos only.
	// histogram_json has shape {"entries": [{"repo_id": "...", ...}, ...]},
	// so we match rows where any histogram entry references a test repo_id.
	if len(s.repoIDs) > 0 {
		_, _ = s.db.ExecContext(ctx, `
			DELETE FROM clean_code.cross_repo_percentile
			WHERE metric_kind = 'lcom4' AND scope_kind = 'class'
			  AND EXISTS (
			    SELECT 1 FROM jsonb_array_elements(histogram_json->'entries') AS e
			    WHERE e->>'repo_id' = ANY($1::text[])
			  )
		`, pq.Array(s.repoIDs))
	}

	for _, rid := range s.repoIDs {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.repo_metric_snapshot WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.metric_sample_active WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.metric_sample WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.scope_binding WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.scan_run WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.commit WHERE repo_id = $1`, rid)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM clean_code.repo WHERE repo_id = $1`, rid)
	}
}

// ensureMetricKind upserts the lcom4 metric_kind catalog entry.
func (s *crossRepoE2EState) ensureMetricKind() error {
	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.metric_kind (metric_kind, metric_version, display_name, unit, direction)
		VALUES ('lcom4', 1, 'LCOM4', 'ratio', 'lower_is_better')
		ON CONFLICT (metric_kind, metric_version) DO NOTHING
	`)
	return err
}

// ---------------------------------------------------------------------------
// step implementations — Background
// ---------------------------------------------------------------------------

// threeRegisteredReposWithCoverageUploads creates three repos, each with an
// active metric_sample row (simulating coverage upload).
func (s *crossRepoE2EState) threeRegisteredReposWithCoverageUploads() error {
	if err := s.ensureMetricKind(); err != nil {
		return fmt.Errorf("ensuring metric_kind: %w", err)
	}

	ctx := context.Background()
	s.repoIDs = make([]string, 3)

	for i := 0; i < 3; i++ {
		repoID := fmt.Sprintf("e2e00000-0000-0000-0000-%012d", i+1)
		s.repoIDs[i] = repoID
		sha := fmt.Sprintf("e2e-sha-%d", i+1)
		value := float64(i+1) * 2.0

		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO clean_code.repo (repo_id, display_name, default_branch)
			VALUES ($1, $2, 'main')
			ON CONFLICT (repo_id) DO NOTHING
		`, repoID, fmt.Sprintf("e2e-cross-repo-%d", i+1)); err != nil {
			return fmt.Errorf("inserting repo %d: %w", i+1, err)
		}

		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO clean_code.commit (repo_id, sha, committed_at, scan_status)
			VALUES ($1, $2, now(), 'pending')
			ON CONFLICT DO NOTHING
		`, repoID, sha); err != nil {
			return fmt.Errorf("inserting commit %d: %w", i+1, err)
		}

		var scanRunID string
		if err := s.db.QueryRowContext(ctx, `
			INSERT INTO clean_code.scan_run (repo_id, kind, to_sha, status)
			VALUES ($1, 'full', $2, 'running')
			RETURNING scan_run_id
		`, repoID, sha).Scan(&scanRunID); err != nil {
			return fmt.Errorf("inserting scan_run %d: %w", i+1, err)
		}

		var scopeID string
		if err := s.db.QueryRowContext(ctx, `
			INSERT INTO clean_code.scope_binding (repo_id, sha, scope_kind, scope_path, language)
			VALUES ($1, $2, 'class', 'com.example.Service', 'java')
			RETURNING scope_id
		`, repoID, sha).Scan(&scopeID); err != nil {
			return fmt.Errorf("inserting scope_binding %d: %w", i+1, err)
		}

		var sampleID string
		if err := s.db.QueryRowContext(ctx, `
			INSERT INTO clean_code.metric_sample
				(repo_id, sha, scope_id, metric_kind, metric_version, value, pack, source, producer_run_id)
			VALUES ($1, $2, $3, 'lcom4', 1, $4, 'base', 'computed', $5)
			RETURNING sample_id
		`, repoID, sha, scopeID, value, scanRunID).Scan(&sampleID); err != nil {
			return fmt.Errorf("inserting metric_sample %d: %w", i+1, err)
		}

		if _, err := s.db.ExecContext(ctx, `
			INSERT INTO clean_code.metric_sample_active
				(repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
			VALUES ($1, $2, $3, 'lcom4', 1, $4)
			ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version)
			DO UPDATE SET sample_id = EXCLUDED.sample_id
		`, repoID, sha, scopeID, sampleID); err != nil {
			return fmt.Errorf("upserting metric_sample_active %d: %w", i+1, err)
		}
	}

	return nil
}

// oneAggregatorTickHasCompleted fires the aggregator tick endpoint to
// produce repo_metric_snapshot and cross_repo_percentile rows.
func (s *crossRepoE2EState) oneAggregatorTickHasCompleted() error {
	aggregatorURL := os.Getenv("CLEAN_CODE_AGGREGATOR_URL")
	if aggregatorURL == "" {
		aggregatorURL = "http://localhost:8085"
	}

	tickURL := strings.TrimRight(aggregatorURL, "/") + "/v1/aggregator/tick"
	client := &http.Client{Timeout: 60 * time.Second}

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, tickURL, nil)
	if err != nil {
		return fmt.Errorf("creating tick request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("invoking aggregator tick: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("aggregator tick returned HTTP %d", resp.StatusCode)
	}

	// Wait for async writes to settle.
	time.Sleep(time.Second)
	return nil
}

// ---------------------------------------------------------------------------
// step implementations — read paths
// ---------------------------------------------------------------------------

// httpGetJSON issues a GET request and decodes the response as JSON.
func httpGetJSON(url string) (map[string]interface{}, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("GET %s returned HTTP %d: %s", url, resp.StatusCode, string(body))
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decoding JSON response: %w", err)
	}
	return result, nil
}

// theE2EScriptAssertsOnTheReadPaths calls mgmt.read.cross_repo and
// eval.gate to populate the scenario state for subsequent Then steps.
func (s *crossRepoE2EState) theE2EScriptAssertsOnTheReadPaths() error {
	baseURL := strings.TrimRight(s.gatewayURL, "/")

	// Read cross_repo percentiles via the management read surface.
	crossRepoURL := baseURL + "/v1/mgmt/read/cross_repo?metric_kind=lcom4&scope_kind=class"
	crossRepoResp, err := httpGetJSON(crossRepoURL)
	if err != nil {
		return fmt.Errorf("reading cross_repo: %w", err)
	}

	// Call the evaluator gate.
	evalGateURL := baseURL + "/v1/eval/gate?metric_kind=lcom4&scope_kind=class"
	evalGateResp, err := httpGetJSON(evalGateURL)
	if err != nil {
		return fmt.Errorf("calling eval.gate: %w", err)
	}

	if s.isStale {
		s.staleCrossRepo = crossRepoResp
		s.staleEvalGate = evalGateResp
	} else {
		s.crossRepoResp = crossRepoResp
		s.evalGateResp = evalGateResp
	}

	return nil
}

// ---------------------------------------------------------------------------
// step implementations — fresh scenario assertions
// ---------------------------------------------------------------------------

func (s *crossRepoE2EState) theCrossRepoResponseHasPopulatedPercentileColumns() error {
	for _, key := range []string{"p50", "p90", "p99"} {
		val, ok := s.crossRepoResp[key]
		if !ok || val == nil {
			return fmt.Errorf("cross_repo response missing or null field: %s", key)
		}
		// Percentile values should be numeric and non-zero.
		numVal, ok := val.(float64)
		if !ok {
			return fmt.Errorf("cross_repo field %s is not a number: %v", key, val)
		}
		if numVal == 0 {
			return fmt.Errorf("cross_repo field %s is zero", key)
		}
	}
	return nil
}

func (s *crossRepoE2EState) theCrossRepoResponseHasDegradedEqualToFalse() error {
	degraded, ok := s.crossRepoResp["degraded"]
	if !ok {
		return fmt.Errorf("cross_repo response missing 'degraded' field")
	}
	if degradedBool, ok := degraded.(bool); ok {
		if degradedBool {
			return fmt.Errorf("expected degraded=false, got true")
		}
		return nil
	}
	return fmt.Errorf("expected degraded to be a boolean, got %T: %v", degraded, degraded)
}

// canonicalVerdicts is the closed set of verdicts the evaluator gate may
// return.  Any value outside this set is treated as a test failure.
var canonicalVerdicts = map[string]bool{
	"pass": true,
	"fail": true,
	"warn": true,
}

func (s *crossRepoE2EState) evalGateReturnsACanonicalVerdict() error {
	verdict, ok := s.evalGateResp["verdict"]
	if !ok || verdict == nil {
		return fmt.Errorf("eval.gate response missing 'verdict' field")
	}
	verdictStr, ok := verdict.(string)
	if !ok {
		return fmt.Errorf("expected verdict to be a string, got %T: %v", verdict, verdict)
	}
	if !canonicalVerdicts[verdictStr] {
		return fmt.Errorf("eval.gate returned non-canonical verdict %q; expected one of pass/fail/warn", verdictStr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// step implementations — stale scenario
// ---------------------------------------------------------------------------

func (s *crossRepoE2EState) theFakeClockIsAdvancedPastFreshnessWindowSeconds() error {
	// Advance the freshness window by updating built_at on all
	// cross_repo_percentile rows to a timestamp far in the past, simulating
	// a stale aggregator snapshot.
	ctx := context.Background()
	_, err := s.db.ExecContext(ctx, `
		UPDATE clean_code.cross_repo_percentile
		SET built_at = now() - INTERVAL '48 hours'
		WHERE metric_kind = 'lcom4' AND scope_kind = 'class'
	`)
	if err != nil {
		return fmt.Errorf("advancing fake clock on cross_repo_percentile: %w", err)
	}

	s.isStale = true
	return nil
}

func (s *crossRepoE2EState) mgmtReadCrossRepoCarriesPercentileStale() error {
	staleFlag, ok := s.staleCrossRepo["percentile_stale"]
	if !ok {
		return fmt.Errorf("mgmt.read.cross_repo response missing 'percentile_stale' field")
	}
	if staleBool, ok := staleFlag.(bool); ok {
		if !staleBool {
			return fmt.Errorf("expected percentile_stale=true, got false")
		}
		return nil
	}
	return fmt.Errorf("expected percentile_stale to be a boolean, got %T: %v", staleFlag, staleFlag)
}

func (s *crossRepoE2EState) evalGateNeverEmitsPercentileStale() error {
	// The evaluator gate response must NOT contain the percentile_stale key
	// at all — not as true, not as false.  Its mere presence is a regression
	// (iter-1 evaluator item 8): the eval surface must never leak internal
	// staleness markers to consumers.
	if _, present := s.staleEvalGate["percentile_stale"]; present {
		return fmt.Errorf("eval.gate response contains 'percentile_stale' key (value=%v); "+
			"the field must be absent entirely — regression on evaluator item 8",
			s.staleEvalGate["percentile_stale"])
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path
// registers all Given/When/Then steps for the cross-repo end-to-end happy
// path stage.
func InitializeScenario_linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path(ctx *godog.ScenarioContext) {
	var state *crossRepoE2EState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		pgDSN := os.Getenv("CLEAN_CODE_PG_URL")
		if pgDSN == "" {
			return ctx, fmt.Errorf("CLEAN_CODE_PG_URL is not set")
		}

		gatewayURL := os.Getenv("CLEAN_CODE_GATEWAY_URL")
		if gatewayURL == "" {
			gatewayURL = "http://localhost:8080"
		}

		var err error
		state, err = newCrossRepoE2EState(pgDSN, gatewayURL)
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
	ctx.Step(`^three registered repos with coverage uploads$`, func() error {
		return state.threeRegisteredReposWithCoverageUploads()
	})
	ctx.Step(`^one aggregator tick has completed$`, func() error {
		return state.oneAggregatorTickHasCompleted()
	})

	// Shared When
	ctx.Step(`^the e2e script asserts on the read paths$`, func() error {
		return state.theE2EScriptAssertsOnTheReadPaths()
	})

	// cross-repo-e2e-fresh Then
	ctx.Step(`^the cross_repo response has populated percentile columns$`, func() error {
		return state.theCrossRepoResponseHasPopulatedPercentileColumns()
	})
	ctx.Step(`^the cross_repo response has degraded equal to false$`, func() error {
		return state.theCrossRepoResponseHasDegradedEqualToFalse()
	})
	ctx.Step(`^eval\.gate returns a canonical verdict$`, func() error {
		return state.evalGateReturnsACanonicalVerdict()
	})

	// cross-repo-e2e-stale Given
	ctx.Step(`^the fake clock is advanced past freshness_window_seconds$`, func() error {
		return state.theFakeClockIsAdvancedPastFreshnessWindowSeconds()
	})

	// cross-repo-e2e-stale Then
	ctx.Step(`^mgmt\.read\.cross_repo carries percentile_stale$`, func() error {
		return state.mgmtReadCrossRepoCarriesPercentileStale()
	})
	ctx.Step(`^eval\.gate never emits percentile_stale$`, func() error {
		return state.evalGateNeverEmitsPercentileStale()
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}
