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
// Shared state for solid-rule-engine-batch-worker-and-synchronous-mode
// ---------------------------------------------------------------------------

type solidRuleEngineState struct {
	db            *sql.DB
	pgURL         string
	ruleEngineURL string
	stewardURL    string

	// finding-emitted-on-rule-hit
	metricSampleID string
	scopeID        string
	sha            string
	findingRow     map[string]interface{}

	// muted-scope-skipped
	mutedScope       string
	mutedRuleID      string
	mutedSHA         string
	mutedRunSyncResp *runSyncResponse

	// delta-newly-failing
	deltaScopeID string
	deltaRuleID  string
	shaA         string
	shaB         string
	shaBFinding  map[string]interface{}

	// sync-mode
	syncScopeID      string
	syncSHA          string
	syncRunSyncResp  *runSyncResponse
	syncRunID        string
	syncVerdictID    string
	syncFindings     []map[string]interface{}
	syncVerdictValue string
}

// runSyncResponse models the JSON returned by POST /v1/run-sync.
type runSyncResponse struct {
	EvaluationRunID string `json:"evaluation_run_id"`
	Verdict         string `json:"verdict"`
	FindingCount    int    `json:"finding_count"`
	StatusCode      int    `json:"-"`
	RawBody         string `json:"-"`
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *solidRuleEngineState) ensureDB() error {
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

func (s *solidRuleEngineState) ensureRuleEngineURL() {
	if s.ruleEngineURL != "" {
		return
	}
	s.ruleEngineURL = os.Getenv("CLEAN_CODE_RULE_ENGINE_URL")
	if s.ruleEngineURL == "" {
		s.ruleEngineURL = "http://localhost:8083"
	}
}

func (s *solidRuleEngineState) ensureStewardURL() {
	if s.stewardURL != "" {
		return
	}
	s.stewardURL = os.Getenv("CLEAN_CODE_POLICY_STEWARD_URL")
	if s.stewardURL == "" {
		s.stewardURL = "http://localhost:8082"
	}
}

func uniqueScopeID() string {
	return fmt.Sprintf("test-scope-%d", time.Now().UnixNano())
}

func uniqueSHA() string {
	return fmt.Sprintf("abc%d", time.Now().UnixNano())
}

// callRunSync invokes POST /v1/run-sync on the rule-engine service.
// This is the synchronous evaluation API that commits evaluation_run,
// evaluation_verdict, and finding rows in a single transaction.
func (s *solidRuleEngineState) callRunSync(scopeID, sha, caller string) (*runSyncResponse, error) {
	s.ensureRuleEngineURL()

	payload := map[string]interface{}{
		"scope_id": scopeID,
		"sha":      sha,
		"caller":   caller,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshalling run-sync request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.ruleEngineURL+"/v1/run-sync", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating run-sync request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /v1/run-sync: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	result := &runSyncResponse{
		StatusCode: resp.StatusCode,
		RawBody:    string(respBody),
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("POST /v1/run-sync returned %d: %s",
			resp.StatusCode, string(respBody))
	}

	if err := json.Unmarshal(respBody, result); err != nil {
		return result, fmt.Errorf("parsing run-sync response: %w (body: %s)",
			err, string(respBody))
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Scenario: finding-emitted-on-rule-hit
// ---------------------------------------------------------------------------

func (s *solidRuleEngineState) aSHAWithMetricSampleOfKindAndValueExceedingThreshold(kind string, value int, threshold int) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	s.scopeID = uniqueScopeID()
	s.sha = uniqueSHA()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	err := s.db.QueryRowContext(ctx,
		`INSERT INTO metric_sample (scope_id, sha, metric_kind, value, created_at)
		 VALUES ($1, $2, $3, $4, NOW())
		 RETURNING id`,
		s.scopeID, s.sha, kind, value).Scan(&s.metricSampleID)
	if err != nil {
		return fmt.Errorf("inserting metric_sample: %w", err)
	}
	return nil
}

func (s *solidRuleEngineState) theRuleEngineRuns() error {
	// Trigger evaluation via the RunSync HTTP API so we exercise the real
	// service path rather than just polling the DB passively.
	result, err := s.callRunSync(s.scopeID, s.sha, "batch_worker")
	if err != nil {
		return fmt.Errorf("triggering rule engine via RunSync: %w", err)
	}
	if result.EvaluationRunID == "" {
		return fmt.Errorf("RunSync returned empty evaluation_run_id (body: %s)", result.RawBody)
	}

	// Now fetch the finding from the DB to populate findingRow for Then steps.
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	row := s.db.QueryRowContext(ctx,
		`SELECT id, rule_id, severity, delta, policy_version_id, metric_sample_ids
		 FROM finding
		 WHERE scope_id = $1 AND sha = $2
		 LIMIT 1`,
		s.scopeID, s.sha)

	var id, ruleID, severity, delta string
	var policyVersionID sql.NullString
	var metricSampleIDs []byte
	err = row.Scan(&id, &ruleID, &severity, &delta, &policyVersionID, &metricSampleIDs)
	if err != nil {
		return fmt.Errorf("finding row not present after RunSync for scope=%s sha=%s: %w",
			s.scopeID, s.sha, err)
	}
	s.findingRow = map[string]interface{}{
		"id":                id,
		"rule_id":           ruleID,
		"severity":          severity,
		"delta":             delta,
		"policy_version_id": policyVersionID.String,
		"metric_sample_ids": string(metricSampleIDs),
	}
	return nil
}

func (s *solidRuleEngineState) aFindingWithRuleIDAndSeverityAndDeltaExists(ruleID, severity, delta string) error {
	if s.findingRow == nil {
		return fmt.Errorf("no finding row found for scope=%s sha=%s", s.scopeID, s.sha)
	}
	if got := s.findingRow["rule_id"]; got != ruleID {
		return fmt.Errorf("expected rule_id=%q, got %q", ruleID, got)
	}
	if got := s.findingRow["severity"]; got != severity {
		return fmt.Errorf("expected severity=%q, got %q", severity, got)
	}
	if got := s.findingRow["delta"]; got != delta {
		return fmt.Errorf("expected delta=%q, got %q", delta, got)
	}
	return nil
}

func (s *solidRuleEngineState) theFindingHasAPolicyVersionIDPinned() error {
	if s.findingRow == nil {
		return fmt.Errorf("no finding row captured")
	}
	pvid, ok := s.findingRow["policy_version_id"].(string)
	if !ok || pvid == "" {
		return fmt.Errorf("expected policy_version_id to be set, got %v", s.findingRow["policy_version_id"])
	}
	return nil
}

func (s *solidRuleEngineState) theFindingHasMetricSampleIDsJSONBReferencingTriggeringSample() error {
	if s.findingRow == nil {
		return fmt.Errorf("no finding row captured")
	}
	raw, ok := s.findingRow["metric_sample_ids"].(string)
	if !ok || raw == "" {
		return fmt.Errorf("metric_sample_ids is empty or not a string")
	}
	var ids []interface{}
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return fmt.Errorf("parsing metric_sample_ids JSONB: %w (raw: %s)", err, raw)
	}
	found := false
	for _, id := range ids {
		idStr := fmt.Sprintf("%v", id)
		if idStr == s.metricSampleID {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("metric_sample_ids %s does not contain triggering sample %s", raw, s.metricSampleID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: muted-scope-skipped
//
// Strategy: Insert override + metric_sample, then call RunSync on the muted
// scope. RunSync forces synchronous processing so we KNOW the engine ran.
// After it returns we verify zero findings AND that the run completed
// (proving the engine saw and skipped the muted scope, not that it never ran).
// ---------------------------------------------------------------------------

func (s *solidRuleEngineState) anOverrideWithScopeAndRuleIDAndMuteTrueAsLatestRow(scope, ruleID string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	s.mutedScope = scope
	s.mutedRuleID = ruleID

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO mgmt_override (scope, rule_id, mute, created_at)
		 VALUES ($1, $2, true, NOW())`,
		scope, ruleID)
	if err != nil {
		return fmt.Errorf("inserting mute override: %w", err)
	}
	return nil
}

func (s *solidRuleEngineState) aMetricSampleExistsForThatMutedScopeExceedingTheThreshold() error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	s.mutedSHA = uniqueSHA()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := s.db.ExecContext(ctx,
		`INSERT INTO metric_sample (scope_id, sha, metric_kind, value, created_at)
		 VALUES ($1, $2, 'lcom4', 15, NOW())`,
		s.mutedScope, s.mutedSHA)
	if err != nil {
		return fmt.Errorf("inserting metric_sample for muted scope: %w", err)
	}
	return nil
}

func (s *solidRuleEngineState) theRuleEngineEvaluatesThatScopeViaRunSync() error {
	// Call RunSync so we get synchronous confirmation that the engine
	// processed this scope. This eliminates false-pass from the batch
	// worker simply not having run yet.
	result, err := s.callRunSync(s.mutedScope, s.mutedSHA, "eval_gate")
	if err != nil {
		return fmt.Errorf("RunSync on muted scope failed: %w", err)
	}
	s.mutedRunSyncResp = result
	return nil
}

func (s *solidRuleEngineState) noFindingRowIsAppendedForThatScopeAndRule() error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM finding WHERE scope_id = $1 AND rule_id = $2`,
		s.mutedScope, s.mutedRuleID).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying findings for muted scope: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 findings for muted scope %s/%s, got %d",
			s.mutedScope, s.mutedRuleID, count)
	}
	return nil
}

func (s *solidRuleEngineState) theEvaluationRunForTheMutedScopeCompletedSuccessfully() error {
	if s.mutedRunSyncResp == nil {
		return fmt.Errorf("RunSync response was not captured for muted scope")
	}
	if s.mutedRunSyncResp.EvaluationRunID == "" {
		return fmt.Errorf("RunSync returned empty evaluation_run_id; engine may not have processed the scope (body: %s)",
			s.mutedRunSyncResp.RawBody)
	}
	// Verify the evaluation_run row exists in the DB — proves the engine ran.
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM evaluation_run WHERE id = $1`,
		s.mutedRunSyncResp.EvaluationRunID).Scan(&id)
	if err != nil {
		return fmt.Errorf("evaluation_run %s not found in DB after RunSync: %w",
			s.mutedRunSyncResp.EvaluationRunID, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: delta-newly-failing
// ---------------------------------------------------------------------------

func (s *solidRuleEngineState) theSameScopeAndRuleEvaluatedAtSHAAWithSeverity(severity string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	s.deltaScopeID = uniqueScopeID()
	s.deltaRuleID = "solid.srp"
	s.shaA = uniqueSHA()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Seed metric_sample for SHA A and run via RunSync so the finding is
	// created through the real service path.
	value := 12
	if severity == "block" {
		value = 25
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO metric_sample (scope_id, sha, metric_kind, value, created_at)
		 VALUES ($1, $2, 'lcom4', $3, NOW())`,
		s.deltaScopeID, s.shaA, value)
	if err != nil {
		return fmt.Errorf("inserting SHA A metric_sample: %w", err)
	}

	// Evaluate SHA A via RunSync.
	result, err := s.callRunSync(s.deltaScopeID, s.shaA, "batch_worker")
	if err != nil {
		return fmt.Errorf("RunSync for SHA A: %w", err)
	}
	if result.EvaluationRunID == "" {
		return fmt.Errorf("RunSync for SHA A returned empty run ID (body: %s)", result.RawBody)
	}
	return nil
}

func (s *solidRuleEngineState) theSameScopeAndRuleEvaluatedAtSHABWithSeverity(severity string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	s.shaB = uniqueSHA()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	value := 12
	if severity == "block" {
		value = 25
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO metric_sample (scope_id, sha, metric_kind, value, created_at)
		 VALUES ($1, $2, 'lcom4', $3, NOW())`,
		s.deltaScopeID, s.shaB, value)
	if err != nil {
		return fmt.Errorf("inserting SHA B metric_sample: %w", err)
	}
	return nil
}

func (s *solidRuleEngineState) theWorkerWritesTheSHABFinding() error {
	// Evaluate SHA B through RunSync so the worker computes delta against
	// the existing SHA A finding for the same scope+rule.
	result, err := s.callRunSync(s.deltaScopeID, s.shaB, "batch_worker")
	if err != nil {
		return fmt.Errorf("RunSync for SHA B: %w", err)
	}
	if result.EvaluationRunID == "" {
		return fmt.Errorf("RunSync for SHA B returned empty run ID (body: %s)", result.RawBody)
	}

	// Fetch the SHA B finding from the DB.
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	row := s.db.QueryRowContext(ctx,
		`SELECT id, rule_id, severity, delta
		 FROM finding
		 WHERE scope_id = $1 AND sha = $2
		 LIMIT 1`,
		s.deltaScopeID, s.shaB)

	var id, ruleID, sev, delta string
	if err := row.Scan(&id, &ruleID, &sev, &delta); err != nil {
		return fmt.Errorf("SHA B finding not found after RunSync for scope=%s sha=%s: %w",
			s.deltaScopeID, s.shaB, err)
	}
	s.shaBFinding = map[string]interface{}{
		"id":       id,
		"rule_id":  ruleID,
		"severity": sev,
		"delta":    delta,
	}
	return nil
}

func (s *solidRuleEngineState) theSHABFindingHasDelta(expected string) error {
	if s.shaBFinding == nil {
		return fmt.Errorf("no SHA B finding captured")
	}
	got := s.shaBFinding["delta"]
	if got != expected {
		return fmt.Errorf("expected delta=%q, got %q", expected, got)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: sync-mode-writes-run-verdict-and-findings
//
// Strategy: Seed a metric_sample then call POST /v1/run-sync (the real
// RuleEngine.RunSync API). The endpoint must commit evaluation_run,
// evaluation_verdict, and finding rows atomically. After the HTTP call
// returns we query the DB to verify all three row types exist and
// reference the same run.
// ---------------------------------------------------------------------------

func (s *solidRuleEngineState) ruleEngineRunSyncCalledWithValidInputs() error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	s.syncScopeID = uniqueScopeID()
	s.syncSHA = uniqueSHA()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Seed a metric_sample so RunSync has data to evaluate.
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO metric_sample (scope_id, sha, metric_kind, value, created_at)
		 VALUES ($1, $2, 'lcom4', 12, NOW())`,
		s.syncScopeID, s.syncSHA)
	if err != nil {
		return fmt.Errorf("seeding metric_sample for sync test: %w", err)
	}
	return nil
}

func (s *solidRuleEngineState) itReturns() error {
	// Call RunSync via the HTTP API — the actual synchronous evaluation path.
	result, err := s.callRunSync(s.syncScopeID, s.syncSHA, "eval_gate")
	if err != nil {
		return fmt.Errorf("RunSync API call failed: %w", err)
	}
	if result.EvaluationRunID == "" {
		return fmt.Errorf("RunSync returned empty evaluation_run_id (status=%d, body=%s)",
			result.StatusCode, result.RawBody)
	}
	s.syncRunID = result.EvaluationRunID
	s.syncRunSyncResp = result
	return nil
}

func (s *solidRuleEngineState) exactlyOneEvaluationRunWithCallerExists(caller string) error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_run
		 WHERE id = $1 AND caller = $2`,
		s.syncRunID, caller).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying evaluation_run: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("expected exactly 1 evaluation_run with id=%s caller=%s, got %d",
			s.syncRunID, caller, count)
	}
	return nil
}

func (s *solidRuleEngineState) exactlyOneEvaluationVerdictReferencingThatRunExists() error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM evaluation_verdict
		 WHERE evaluation_run_id = $1`,
		s.syncRunID).Scan(&count)
	if err != nil {
		return fmt.Errorf("querying evaluation_verdict: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("expected exactly 1 evaluation_verdict for run %s, got %d",
			s.syncRunID, count)
	}

	// Capture the verdict ID and value for later steps.
	err = s.db.QueryRowContext(ctx,
		`SELECT id, verdict FROM evaluation_verdict
		 WHERE evaluation_run_id = $1`,
		s.syncRunID).Scan(&s.syncVerdictID, &s.syncVerdictValue)
	if err != nil {
		return fmt.Errorf("fetching verdict details: %w", err)
	}
	return nil
}

func (s *solidRuleEngineState) atLeastOneFindingRowReferencingThatRunExists() error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, rule_id, severity, delta
		 FROM finding
		 WHERE evaluation_run_id = $1`,
		s.syncRunID)
	if err != nil {
		return fmt.Errorf("querying findings for run %s: %w", s.syncRunID, err)
	}
	defer rows.Close()

	s.syncFindings = nil
	for rows.Next() {
		var id, ruleID, severity, delta string
		if err := rows.Scan(&id, &ruleID, &severity, &delta); err != nil {
			return fmt.Errorf("scanning finding: %w", err)
		}
		s.syncFindings = append(s.syncFindings, map[string]interface{}{
			"id":       id,
			"rule_id":  ruleID,
			"severity": severity,
			"delta":    delta,
		})
	}
	if len(s.syncFindings) == 0 {
		return fmt.Errorf("expected at least 1 finding referencing run %s, got 0", s.syncRunID)
	}
	return nil
}

func (s *solidRuleEngineState) allRowsWereCommittedInTheSameTransaction() error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Atomicity proof via Postgres `xmin`: every row stores the xid of the
	// transaction that inserted it. Rows inserted in the *same* transaction
	// share an identical xmin; three sequential commits would produce three
	// distinct xmins. Comparing xmin across evaluation_run, evaluation_verdict,
	// and every finding for this run is therefore a strong proxy for "all
	// rows were committed in the same transaction" — much stronger than just
	// checking the rows exist.
	//
	// Caveat: VACUUM FREEZE could in theory rewrite xmin to FrozenTransactionId,
	// but that only happens on long-lived rows well past freeze thresholds,
	// not on rows we just inserted seconds earlier in this test.
	var runXmin, verdictXmin string
	err := s.db.QueryRowContext(ctx,
		`SELECT xmin::text FROM evaluation_run WHERE id = $1`,
		s.syncRunID).Scan(&runXmin)
	if err != nil {
		return fmt.Errorf("reading evaluation_run xmin for run %s: %w", s.syncRunID, err)
	}
	if runXmin == "" || runXmin == "0" {
		return fmt.Errorf("evaluation_run xmin is empty/zero for run %s (unexpected)", s.syncRunID)
	}

	err = s.db.QueryRowContext(ctx,
		`SELECT xmin::text FROM evaluation_verdict WHERE evaluation_run_id = $1`,
		s.syncRunID).Scan(&verdictXmin)
	if err != nil {
		return fmt.Errorf("reading evaluation_verdict xmin for run %s: %w", s.syncRunID, err)
	}
	if verdictXmin != runXmin {
		return fmt.Errorf("evaluation_verdict xmin=%s != evaluation_run xmin=%s "+
			"(rows were NOT committed in the same transaction)",
			verdictXmin, runXmin)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, xmin::text FROM finding WHERE evaluation_run_id = $1`,
		s.syncRunID)
	if err != nil {
		return fmt.Errorf("reading finding xmins for run %s: %w", s.syncRunID, err)
	}
	defer rows.Close()

	var findingCount int
	for rows.Next() {
		var findingID, findingXmin string
		if err := rows.Scan(&findingID, &findingXmin); err != nil {
			return fmt.Errorf("scanning finding xmin row: %w", err)
		}
		if findingXmin != runXmin {
			return fmt.Errorf("finding %s xmin=%s != evaluation_run xmin=%s "+
				"(rows were NOT committed in the same transaction)",
				findingID, findingXmin, runXmin)
		}
		findingCount++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating finding xmins: %w", err)
	}
	if findingCount < 1 {
		return fmt.Errorf("expected at least 1 finding for run %s, got 0", s.syncRunID)
	}
	return nil
}

func (s *solidRuleEngineState) theVerdictColumnMatchesSeverityRollupOfUnmutedFindings() error {
	if err := s.ensureDB(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Compute expected rollup from the findings actually stored.
	var maxSevNum int
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(
			CASE severity
				WHEN 'block' THEN 4
				WHEN 'warn'  THEN 3
				WHEN 'info'  THEN 2
				WHEN 'pass'  THEN 1
				ELSE 0
			END), 0)
		 FROM finding
		 WHERE evaluation_run_id = $1`,
		s.syncRunID).Scan(&maxSevNum)
	if err != nil {
		return fmt.Errorf("computing severity rollup: %w", err)
	}

	severityMap := map[int]string{4: "block", 3: "warn", 2: "info", 1: "pass", 0: "pass"}
	expectedVerdict := severityMap[maxSevNum]

	if s.syncVerdictValue != expectedVerdict {
		return fmt.Errorf("expected verdict=%q (rollup of findings), got %q",
			expectedVerdict, s.syncVerdictValue)
	}
	return nil
}

// ======================================================================
// Godog wiring
// ======================================================================

func InitializeScenario_policy_steward_and_solid_rule_engine_solid_rule_engine_batch_worker_and_synchronous_mode(ctx *godog.ScenarioContext) {
	s := &solidRuleEngineState{}

	// finding-emitted-on-rule-hit
	ctx.Step(`^a SHA with a metric_sample of kind "([^"]*)" and value (\d+) exceeding the SRP threshold of (\d+)$`,
		s.aSHAWithMetricSampleOfKindAndValueExceedingThreshold)
	ctx.Step(`^the rule engine runs$`, s.theRuleEngineRuns)
	ctx.Step(`^a finding with rule_id "([^"]*)" and severity "([^"]*)" and delta "([^"]*)" exists$`,
		s.aFindingWithRuleIDAndSeverityAndDeltaExists)
	ctx.Step(`^the finding has a policy_version_id pinned$`, s.theFindingHasAPolicyVersionIDPinned)
	ctx.Step(`^the finding has metric_sample_ids JSONB referencing the triggering sample$`,
		s.theFindingHasMetricSampleIDsJSONBReferencingTriggeringSample)

	// muted-scope-skipped
	ctx.Step(`^an override with scope "([^"]*)" and rule_id "([^"]*)" and mute true as the latest row$`,
		s.anOverrideWithScopeAndRuleIDAndMuteTrueAsLatestRow)
	ctx.Step(`^a metric_sample exists for that muted scope exceeding the threshold$`,
		s.aMetricSampleExistsForThatMutedScopeExceedingTheThreshold)
	ctx.Step(`^the rule engine evaluates that scope via RunSync$`,
		s.theRuleEngineEvaluatesThatScopeViaRunSync)
	ctx.Step(`^no finding row is appended for that scope and rule$`,
		s.noFindingRowIsAppendedForThatScopeAndRule)
	ctx.Step(`^the evaluation_run for the muted scope completed successfully$`,
		s.theEvaluationRunForTheMutedScopeCompletedSuccessfully)

	// delta-newly-failing
	ctx.Step(`^the same scope and rule evaluated at SHA A with severity "([^"]*)"$`,
		s.theSameScopeAndRuleEvaluatedAtSHAAWithSeverity)
	ctx.Step(`^the same scope and rule evaluated at SHA B with severity "([^"]*)"$`,
		s.theSameScopeAndRuleEvaluatedAtSHABWithSeverity)
	ctx.Step(`^the worker writes the SHA B finding$`, s.theWorkerWritesTheSHABFinding)
	ctx.Step(`^the SHA B finding has delta "([^"]*)"$`, s.theSHABFindingHasDelta)

	// sync-mode-writes-run-verdict-and-findings
	ctx.Step(`^RuleEngine\.RunSync called with valid inputs$`, s.ruleEngineRunSyncCalledWithValidInputs)
	ctx.Step(`^it returns$`, s.itReturns)
	ctx.Step(`^exactly one evaluation_run with caller "([^"]*)" exists$`,
		s.exactlyOneEvaluationRunWithCallerExists)
	ctx.Step(`^exactly one evaluation_verdict referencing that run exists$`,
		s.exactlyOneEvaluationVerdictReferencingThatRunExists)
	ctx.Step(`^at least one finding row referencing that run exists$`,
		s.atLeastOneFindingRowReferencingThatRunExists)
	ctx.Step(`^all rows were committed in the same transaction$`,
		s.allRowsWereCommittedInTheSameTransaction)
	ctx.Step(`^the verdict column matches the severity rollup of unmuted findings$`,
		s.theVerdictColumnMatchesSeverityRollupOfUnmutedFindings)
}

func TestE2E_policy_steward_and_solid_rule_engine_solid_rule_engine_batch_worker_and_synchronous_mode(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_policy_steward_and_solid_rule_engine_solid_rule_engine_batch_worker_and_synchronous_mode,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"policy_steward_and_solid_rule_engine_solid_rule_engine_batch_worker_and_synchronous_mode.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}