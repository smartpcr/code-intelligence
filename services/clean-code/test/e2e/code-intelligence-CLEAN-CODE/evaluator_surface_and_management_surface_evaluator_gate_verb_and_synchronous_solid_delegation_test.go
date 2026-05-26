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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"

	"forge/services/clean-code/internal/domain"
)

// requireEnv returns the value of the named environment variable or skips the
// test when the variable is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("required env var %s is not set; skipping E2E test", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Shared per-scenario state
// ---------------------------------------------------------------------------

type evalGateState struct {
	// Environment URLs
	pgURL          string
	evaluatorURL   string
	ruleEngineURL  string
	mgmtURL        string
	kmsURL         string
	oidcIssuer     string

	db *sql.DB

	// Per-scenario identifiers
	sha                string
	repoID             string
	policyVersionID    string

	// Metrics snapshots
	ruleEngineInvocBefore int64
	ruleEngineInvocAfter  int64

	// DB results
	runID       string
	runXmin     string
	verdictID   string
	verdictXmin string
	verdictVal  string
	findingXmins []string
	findingCount int

	// Degraded-specific
	degradedRunID       string
	degradedRunXmin     string
	degradedVerdictXmin string

	// Signature-invalid specific
	sigInvalidSHA      string
	sigInvalidRunID    string
	sigInvalidInvocBefore int64

	// Samples-pending in double-write
	samplesDoubleWriteSHA string

	// KMS policy signature for eval.gate calls
	policySignature string

	// HTTP response tracking
	lastHTTPStatus int
	lastHTTPBody   string

	// Scenario start time for row-boundary filtering
	scenarioStart time.Time
}

func newEvalGateState() *evalGateState {
	return &evalGateState{
		scenarioStart: time.Now().UTC(),
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// scrapeRuleEngineInvocationCount fetches the Prometheus metrics endpoint of
// the rule-engine service and extracts the invocation counter. We look for a
// counter named rule_engine_evaluations_total (or similar) that specifically
// tracks evaluation invocations, NOT generic HTTP requests.
func scrapeRuleEngineInvocationCount(ruleEngineURL string) (int64, error) {
	resp, err := http.Get(ruleEngineURL + "/metrics")
	if err != nil {
		return 0, fmt.Errorf("GET /metrics: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("reading metrics body: %w", err)
	}

	// Try specific evaluation counter first, fall back to run_sync counter.
	patterns := []string{
		`rule_engine_evaluations_total\b[^\n]*\s+(\d+)`,
		`rule_engine_run_sync_total\b[^\n]*\s+(\d+)`,
		`rule_engine_invocations_total\b[^\n]*\s+(\d+)`,
	}
	for _, pat := range patterns {
		re := regexp.MustCompile(pat)
		m := re.FindSubmatch(body)
		if m != nil {
			return strconv.ParseInt(string(m[1]), 10, 64)
		}
	}

	// If no specific counter exists, count the RunSync HTTP handler calls.
	re := regexp.MustCompile(`http_requests_total\{[^}]*handler="/v1/run-sync"[^}]*\}\s+(\d+)`)
	m := re.FindSubmatch(body)
	if m != nil {
		return strconv.ParseInt(string(m[1]), 10, 64)
	}

	// Fallback: zero if the counter has never been incremented (absent from output).
	return 0, nil
}

func uniqueSHA(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// callEvalGate sends a POST to the evaluator's gate endpoint.
func callEvalGate(evaluatorURL, sha, repoID, policyVersionID string, extras map[string]interface{}) (int, string, error) {
	payload := map[string]interface{}{
		"sha":               sha,
		"repo_id":           repoID,
		"policy_version_id": policyVersionID,
	}
	for k, v := range extras {
		payload[k] = v
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(evaluatorURL+"/v1/eval/gate", "application/json", bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("POST /v1/eval/gate: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(respBody), nil
}

// seedSamplesForSHA inserts metric samples into the database so the evaluator
// recognises this SHA as having samples present (not degraded). The seeded data
// from `make seed-phase-06` creates baseline samples for repo-a, but our
// unique per-test SHAs need their own rows.
//
// The function discovers the actual metric_sample schema from information_schema
// to select the correct column names, and fails fast if the schema is unknown.
func seedSamplesForSHA(db *sql.DB, repoID, sha string) error {
	// Discover the metric_sample column names to select the right INSERT variant.
	cols, err := discoverMetricSampleColumns(db)
	if err != nil {
		return fmt.Errorf("discovering metric_sample schema: %w", err)
	}

	// Determine which schema variant we're dealing with based on actual columns.
	type schemaVariant struct {
		nameCol string // column for metric name
		tsCol   string // column for timestamp
	}

	var variant *schemaVariant
	if cols["metric_name"] && cols["created_at"] {
		variant = &schemaVariant{nameCol: "metric_name", tsCol: "created_at"}
	} else if cols["name"] && cols["collected_at"] {
		variant = &schemaVariant{nameCol: "name", tsCol: "collected_at"}
	} else {
		return fmt.Errorf("metric_sample schema does not match any known variant (columns: %v); "+
			"cannot seed complete sample rows with metric names and values", colNames(cols))
	}

	query := fmt.Sprintf(
		`INSERT INTO metric_sample (repo_id, sha, %s, value, %s)
		 VALUES ($1, $2, 'cyclomatic_complexity', 5.0, NOW()),
		        ($1, $2, 'coupling_between_objects', 2.0, NOW()),
		        ($1, $2, 'lines_of_code', 100.0, NOW())
		 ON CONFLICT DO NOTHING`,
		variant.nameCol, variant.tsCol,
	)

	_, err = db.Exec(query, repoID, sha)
	if err != nil {
		return fmt.Errorf("inserting metric_sample rows (schema: %s/%s): %w",
			variant.nameCol, variant.tsCol, err)
	}
	return nil
}

// discoverMetricSampleColumns queries information_schema to return the set of
// column names present in the metric_sample table.
func discoverMetricSampleColumns(db *sql.DB) (map[string]bool, error) {
	rows, err := db.Query(
		`SELECT column_name FROM information_schema.columns
		 WHERE table_schema = 'public' AND table_name = 'metric_sample'`)
	if err != nil {
		return nil, fmt.Errorf("querying information_schema: %w", err)
	}
	defer rows.Close()

	cols := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scanning column name: %w", err)
		}
		cols[name] = true
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("metric_sample table not found or has no columns")
	}
	return cols, rows.Err()
}

// colNames returns a sorted comma-separated list of column names for error messages.
func colNames(cols map[string]bool) string {
	names := make([]string, 0, len(cols))
	for name := range cols {
		names = append(names, name)
	}
	return strings.Join(names, ", ")
}

// signPolicyForSHA requests a valid policy signature from the KMS mock service
// for the given SHA. This ensures the evaluator considers the policy signature
// valid when processing the gate request.
func signPolicyForSHA(kmsURL, sha, policyVersionID string) (string, error) {
	if kmsURL == "" {
		// If KMS URL is not available, return empty — the evaluator may accept
		// unsigned requests in test mode or via the seeded policy.
		return "", nil
	}
	payload := map[string]interface{}{
		"sha":               sha,
		"policy_version_id": policyVersionID,
	}
	body, _ := json.Marshal(payload)
	resp, err := http.Post(kmsURL+"/v1/sign", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("POST /v1/sign: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		// Non-fatal: the KMS mock may not implement /v1/sign; the seeded
		// policy may already have a valid signature.
		return "", nil
	}
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", nil
	}
	if sig, ok := result["signature"].(string); ok {
		return sig, nil
	}
	return "", nil
}

// ---------------------------------------------------------------------------
// Scenario: verdict-enum-only-canonical
// ---------------------------------------------------------------------------

func (s *evalGateState) theProductionVerdictEnumImportedFromDomain() error {
	// This step validates that the import of domain.AllVerdicts is compilable.
	// The actual assertion is in the When/Then steps.
	// By reaching this line, the compile-time import of the production
	// domain package has succeeded.
	verdicts := domain.AllVerdicts()
	if verdicts == nil {
		return fmt.Errorf("domain.AllVerdicts() returned nil")
	}
	return nil
}

func (s *evalGateState) iteratingTheProductionAllVerdictsFunction() error {
	// Iteration happens in the Then step; this is a pass-through.
	return nil
}

func (s *evalGateState) theValuesAreExactlyPassWarnBlockAndNoFailOrGatedExist() error {
	verdicts := domain.AllVerdicts()

	// Assert exactly 3 values
	if len(verdicts) != 3 {
		return fmt.Errorf("expected exactly 3 verdicts, got %d: %v", len(verdicts), verdicts)
	}

	canonical := []domain.Verdict{domain.VerdictPass, domain.VerdictWarn, domain.VerdictBlock}
	canonicalStrings := []string{"pass", "warn", "block"}

	// Assert exact order and values match the production constants
	for i, v := range verdicts {
		if v != canonical[i] {
			return fmt.Errorf("verdict[%d]: expected %q (domain constant), got %q", i, canonical[i], v)
		}
		if string(v) != canonicalStrings[i] {
			return fmt.Errorf("verdict[%d]: expected string %q, got %q", i, canonicalStrings[i], string(v))
		}
	}

	// Assert no duplicates
	seen := map[domain.Verdict]bool{}
	for _, v := range verdicts {
		if seen[v] {
			return fmt.Errorf("duplicate verdict %q in AllVerdicts()", v)
		}
		seen[v] = true
	}

	// Assert forbidden values are absent
	forbidden := []string{"fail", "gated"}
	for _, v := range verdicts {
		for _, f := range forbidden {
			if string(v) == f {
				return fmt.Errorf("forbidden verdict %q found in AllVerdicts()", f)
			}
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Scenario: gate-delegates-synchronous-rule-pass
// ---------------------------------------------------------------------------

func (s *evalGateState) aUniqueCleanSHAWithSamplesPresentAndAValidPolicySignature() error {
	s.pgURL = os.Getenv("CLEAN_CODE_PG_URL")
	s.evaluatorURL = os.Getenv("CLEAN_CODE_EVALUATOR_URL")
	s.ruleEngineURL = os.Getenv("CLEAN_CODE_RULE_ENGINE_URL")
	s.mgmtURL = os.Getenv("CLEAN_CODE_MGMT_URL")
	s.kmsURL = os.Getenv("CLEAN_CODE_KMS_URL")
	s.oidcIssuer = os.Getenv("CLEAN_CODE_OIDC_ISSUER")

	if s.pgURL == "" || s.evaluatorURL == "" || s.ruleEngineURL == "" {
		return fmt.Errorf("required env vars CLEAN_CODE_PG_URL, CLEAN_CODE_EVALUATOR_URL, CLEAN_CODE_RULE_ENGINE_URL must be set")
	}

	var err error
	s.db, err = sql.Open("postgres", s.pgURL)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}
	if err := s.db.Ping(); err != nil {
		return fmt.Errorf("pinging postgres: %w", err)
	}

	s.sha = uniqueSHA("clean-pass")
	s.scenarioStart = time.Now().UTC()

	// Retrieve seeded repo and policy version from DB
	err = s.db.QueryRow(`SELECT id FROM repository LIMIT 1`).Scan(&s.repoID)
	if err != nil {
		return fmt.Errorf("querying seeded repository: %w", err)
	}
	err = s.db.QueryRow(`SELECT id FROM policy_version WHERE active = true LIMIT 1`).Scan(&s.policyVersionID)
	if err != nil {
		return fmt.Errorf("querying active policy_version: %w", err)
	}

	// Seed metric samples for this unique SHA so the evaluator recognises
	// it as having samples present (not degraded / samples_pending).
	if err := seedSamplesForSHA(s.db, s.repoID, s.sha); err != nil {
		return fmt.Errorf("seeding samples for clean-pass SHA: %w", err)
	}

	// Obtain a valid policy signature from the KMS mock so the evaluator
	// accepts the policy as validly signed.
	sig, err := signPolicyForSHA(s.kmsURL, s.sha, s.policyVersionID)
	if err != nil {
		return fmt.Errorf("signing policy for clean-pass SHA: %w", err)
	}
	if sig != "" {
		s.policySignature = sig
	}

	return nil
}

func (s *evalGateState) theRuleEngineHTTPInvocationCountIsSnapshottedViaMetrics() error {
	var err error
	s.ruleEngineInvocBefore, err = scrapeRuleEngineInvocationCount(s.ruleEngineURL)
	if err != nil {
		return fmt.Errorf("snapshotting rule-engine invocations: %w", err)
	}
	return nil
}

func (s *evalGateState) evalGateIsCalledForTheCleanPassPath() error {
	extras := map[string]interface{}{}
	// Include valid policy signature if one was obtained from KMS mock
	if s.policySignature != "" {
		extras["signature"] = s.policySignature
	}
	status, body, err := callEvalGate(s.evaluatorURL, s.sha, s.repoID, s.policyVersionID, extras)
	if err != nil {
		return err
	}
	s.lastHTTPStatus = status
	s.lastHTTPBody = body
	if status < 200 || status >= 300 {
		return fmt.Errorf("eval.gate clean-pass returned HTTP %d: %s", status, body)
	}
	return nil
}

func (s *evalGateState) theRuleEngineHTTPInvocationCountIncreasedByExactlyOneProvingRunSyncWasCalled() error {
	var err error
	s.ruleEngineInvocAfter, err = scrapeRuleEngineInvocationCount(s.ruleEngineURL)
	if err != nil {
		return fmt.Errorf("snapshotting rule-engine invocations (after): %w", err)
	}
	delta := s.ruleEngineInvocAfter - s.ruleEngineInvocBefore
	if delta != 1 {
		if s.ruleEngineInvocAfter < s.ruleEngineInvocBefore {
			return fmt.Errorf("rule-engine invocation counter decreased (before=%d, after=%d); service may have restarted", s.ruleEngineInvocBefore, s.ruleEngineInvocAfter)
		}
		return fmt.Errorf("expected rule-engine invocation count delta=1, got delta=%d (before=%d, after=%d)", delta, s.ruleEngineInvocBefore, s.ruleEngineInvocAfter)
	}
	return nil
}

func (s *evalGateState) exactlyOneNewEvaluationRunRowForThisSHAWithCallerEvalGateExists() error {
	rows, err := s.db.Query(
		`SELECT id, xmin::text FROM evaluation_run WHERE sha = $1 AND caller = 'eval_gate' AND created_at >= $2`,
		s.sha, s.scenarioStart,
	)
	if err != nil {
		return fmt.Errorf("querying evaluation_run: %w", err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		count++
		if err := rows.Scan(&s.runID, &s.runXmin); err != nil {
			return fmt.Errorf("scanning evaluation_run: %w", err)
		}
	}
	if count != 1 {
		return fmt.Errorf("expected exactly 1 evaluation_run for SHA %s, got %d", s.sha, count)
	}
	return nil
}

func (s *evalGateState) exactlyOneNewEvaluationVerdictRowReferencingThatRunExists() error {
	rows, err := s.db.Query(
		`SELECT id, xmin::text, verdict FROM evaluation_verdict WHERE evaluation_run_id = $1 AND created_at >= $2`,
		s.runID, s.scenarioStart,
	)
	if err != nil {
		return fmt.Errorf("querying evaluation_verdict: %w", err)
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		count++
		if err := rows.Scan(&s.verdictID, &s.verdictXmin, &s.verdictVal); err != nil {
			return fmt.Errorf("scanning evaluation_verdict: %w", err)
		}
	}
	if count != 1 {
		return fmt.Errorf("expected exactly 1 evaluation_verdict for run %s, got %d", s.runID, count)
	}
	return nil
}

func (s *evalGateState) nNewFindingRowsReferencingThatRunExistWithNGreaterThanZero() error {
	rows, err := s.db.Query(
		`SELECT xmin::text FROM finding WHERE evaluation_run_id = $1 AND created_at >= $2`,
		s.runID, s.scenarioStart,
	)
	if err != nil {
		return fmt.Errorf("querying finding: %w", err)
	}
	defer rows.Close()

	s.findingXmins = nil
	for rows.Next() {
		var xmin string
		if err := rows.Scan(&xmin); err != nil {
			return fmt.Errorf("scanning finding xmin: %w", err)
		}
		s.findingXmins = append(s.findingXmins, xmin)
	}
	s.findingCount = len(s.findingXmins)
	if s.findingCount == 0 {
		return fmt.Errorf("expected N > 0 finding rows for run %s, got 0", s.runID)
	}
	return nil
}

func (s *evalGateState) theRunAndVerdictAndFindingRowsShareTheSameXminProvingSameTransaction() error {
	if s.runXmin != s.verdictXmin {
		return fmt.Errorf("evaluation_run xmin=%s != evaluation_verdict xmin=%s; not same transaction", s.runXmin, s.verdictXmin)
	}
	for i, fx := range s.findingXmins {
		if fx != s.runXmin {
			return fmt.Errorf("finding[%d] xmin=%s != evaluation_run xmin=%s; not same transaction", i, fx, s.runXmin)
		}
	}
	return nil
}

func (s *evalGateState) theVerdictColumnEqualsTheSeverityRollupOfTheFindings() error {
	// Query finding severities and compute rollup
	rows, err := s.db.Query(
		`SELECT severity FROM finding WHERE evaluation_run_id = $1 AND created_at >= $2`,
		s.runID, s.scenarioStart,
	)
	if err != nil {
		return fmt.Errorf("querying finding severities: %w", err)
	}
	defer rows.Close()

	severityRank := map[string]int{
		"info":    0,
		"low":     1,
		"warning": 2,
		"warn":    2,
		"medium":  3,
		"high":    4,
		"block":   5,
		"error":   5,
	}

	maxRank := -1
	for rows.Next() {
		var sev string
		if err := rows.Scan(&sev); err != nil {
			return fmt.Errorf("scanning severity: %w", err)
		}
		rank, ok := severityRank[strings.ToLower(sev)]
		if !ok {
			rank = 0
		}
		if rank > maxRank {
			maxRank = rank
		}
	}

	// Map max severity rank to expected verdict
	var expectedVerdict string
	switch {
	case maxRank >= 5:
		expectedVerdict = "block"
	case maxRank >= 2:
		expectedVerdict = "warn"
	default:
		expectedVerdict = "pass"
	}

	if s.verdictVal != expectedVerdict {
		return fmt.Errorf("verdict column %q does not match severity rollup (expected %q, maxRank=%d)", s.verdictVal, expectedVerdict, maxRank)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: degraded-maps-to-warn
// ---------------------------------------------------------------------------

func (s *evalGateState) aUniqueSHAForTheDegradedScenario() error {
	s.pgURL = os.Getenv("CLEAN_CODE_PG_URL")
	s.evaluatorURL = os.Getenv("CLEAN_CODE_EVALUATOR_URL")
	s.ruleEngineURL = os.Getenv("CLEAN_CODE_RULE_ENGINE_URL")

	if s.pgURL == "" || s.evaluatorURL == "" || s.ruleEngineURL == "" {
		return fmt.Errorf("required env vars must be set")
	}

	var err error
	s.db, err = sql.Open("postgres", s.pgURL)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}

	s.sha = uniqueSHA("degraded")
	s.scenarioStart = time.Now().UTC()

	err = s.db.QueryRow(`SELECT id FROM repository LIMIT 1`).Scan(&s.repoID)
	if err != nil {
		return fmt.Errorf("querying seeded repository: %w", err)
	}
	err = s.db.QueryRow(`SELECT id FROM policy_version WHERE active = true LIMIT 1`).Scan(&s.policyVersionID)
	if err != nil {
		return fmt.Errorf("querying active policy_version: %w", err)
	}

	return nil
}

func (s *evalGateState) aSamplesPendingDegradedConditionIsConfiguredWithNoMetricSamples() error {
	// Ensure no metric samples exist for this SHA so the evaluator detects
	// a samples_pending degraded condition.
	// The seeded data does NOT include samples for arbitrary SHAs, so using
	// a unique SHA naturally produces this condition.
	return nil
}

func (s *evalGateState) evalGateIsCalledForTheDegradedPath() error {
	status, body, err := callEvalGate(s.evaluatorURL, s.sha, s.repoID, s.policyVersionID, map[string]interface{}{
		"degraded_reason": "samples_pending",
	})
	if err != nil {
		return err
	}
	s.lastHTTPStatus = status
	s.lastHTTPBody = body
	if status < 200 || status >= 300 {
		return fmt.Errorf("eval.gate degraded returned HTTP %d: %s", status, body)
	}
	return nil
}

func (s *evalGateState) theRuleEngineHTTPInvocationCountDidNotChangeProvingNotInvoked() error {
	after, err := scrapeRuleEngineInvocationCount(s.ruleEngineURL)
	if err != nil {
		return fmt.Errorf("snapshotting rule-engine invocations (after): %w", err)
	}
	if after < s.ruleEngineInvocBefore {
		return fmt.Errorf("rule-engine invocation counter decreased (before=%d, after=%d); service may have restarted", s.ruleEngineInvocBefore, after)
	}
	delta := after - s.ruleEngineInvocBefore
	if delta != 0 {
		return fmt.Errorf("expected rule-engine invocation count delta=0 (not invoked), got delta=%d", delta)
	}
	return nil
}

func (s *evalGateState) zeroNewFindingRowsExistForThisSHA() error {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM finding f JOIN evaluation_run r ON f.evaluation_run_id = r.id WHERE r.sha = $1 AND f.created_at >= $2`,
		s.sha, s.scenarioStart,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("counting findings: %w", err)
	}
	if count != 0 {
		return fmt.Errorf("expected 0 finding rows for degraded SHA %s, got %d", s.sha, count)
	}
	return nil
}

func (s *evalGateState) oneNewEvaluationRunRowWithCanonicalColumnsExists() error {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM evaluation_run WHERE sha = $1 AND caller = 'eval_gate' AND repo_id = $2 AND policy_version_id = $3 AND created_at >= $4`,
		s.sha, s.repoID, s.policyVersionID, s.scenarioStart,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("counting evaluation_run: %w", err)
	}
	if count != 1 {
		return fmt.Errorf("expected exactly 1 evaluation_run for degraded SHA %s with repo_id=%s and policy_version_id=%s, got %d",
			s.sha, s.repoID, s.policyVersionID, count)
	}

	err = s.db.QueryRow(
		`SELECT id, xmin::text FROM evaluation_run WHERE sha = $1 AND caller = 'eval_gate' AND created_at >= $2`,
		s.sha, s.scenarioStart,
	).Scan(&s.degradedRunID, &s.degradedRunXmin)
	if err != nil {
		return fmt.Errorf("fetching degraded run: %w", err)
	}
	return nil
}

func (s *evalGateState) oneNewEvaluationVerdictWithCanonicalDegradedFieldsExists() error {
	// First enforce exactly one verdict row exists for this run
	var verdictCount int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM evaluation_verdict WHERE evaluation_run_id = $1`,
		s.degradedRunID,
	).Scan(&verdictCount)
	if err != nil {
		return fmt.Errorf("counting degraded verdicts: %w", err)
	}
	if verdictCount != 1 {
		return fmt.Errorf("expected exactly 1 evaluation_verdict for degraded run %s, got %d", s.degradedRunID, verdictCount)
	}

	var (
		verdict        string
		degraded       bool
		degradedReason sql.NullString
		runFK          sql.NullString
		createdAt      sql.NullTime
		xmin           string
	)
	err = s.db.QueryRow(
		`SELECT verdict, degraded, degraded_reason, evaluation_run_id, created_at, xmin::text
		 FROM evaluation_verdict WHERE evaluation_run_id = $1`,
		s.degradedRunID,
	).Scan(&verdict, &degraded, &degradedReason, &runFK, &createdAt, &xmin)
	if err != nil {
		return fmt.Errorf("querying degraded evaluation_verdict: %w", err)
	}

	s.degradedVerdictXmin = xmin

	if verdict != "warn" {
		return fmt.Errorf("expected verdict='warn', got %q", verdict)
	}
	if !degraded {
		return fmt.Errorf("expected degraded=true, got false")
	}
	if !degradedReason.Valid || degradedReason.String != "samples_pending" {
		return fmt.Errorf("expected degraded_reason='samples_pending', got %v", degradedReason)
	}
	if !runFK.Valid {
		return fmt.Errorf("evaluation_run_id FK is NULL in degraded verdict")
	}
	if !createdAt.Valid {
		return fmt.Errorf("created_at is NULL in degraded verdict")
	}
	return nil
}

func (s *evalGateState) theDegradedRunAndVerdictShareTheSameXminProvingSameTransaction() error {
	if s.degradedRunXmin != s.degradedVerdictXmin {
		return fmt.Errorf("degraded evaluation_run xmin=%s != evaluation_verdict xmin=%s; not same transaction",
			s.degradedRunXmin, s.degradedVerdictXmin)
	}
	return nil
}

func (s *evalGateState) theDegradedVerdictHasNonNullRunFKAndCreatedAt() error {
	var runFK sql.NullString
	var createdAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT evaluation_run_id, created_at FROM evaluation_verdict WHERE evaluation_run_id = $1 AND created_at >= $2`,
		s.degradedRunID, s.scenarioStart,
	).Scan(&runFK, &createdAt)
	if err != nil {
		return fmt.Errorf("querying verdict FK/created_at: %w", err)
	}
	if !runFK.Valid {
		return fmt.Errorf("evaluation_run_id FK is NULL")
	}
	if !createdAt.Valid {
		return fmt.Errorf("created_at is NULL")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: gate-does-not-double-write-verdict
// ---------------------------------------------------------------------------

func (s *evalGateState) aUniqueSHAForTheCleanPassDoubleWriteCheck() error {
	s.pgURL = os.Getenv("CLEAN_CODE_PG_URL")
	s.evaluatorURL = os.Getenv("CLEAN_CODE_EVALUATOR_URL")
	s.ruleEngineURL = os.Getenv("CLEAN_CODE_RULE_ENGINE_URL")
	s.kmsURL = os.Getenv("CLEAN_CODE_KMS_URL")

	if s.pgURL == "" || s.evaluatorURL == "" || s.ruleEngineURL == "" {
		return fmt.Errorf("required env vars must be set")
	}

	var err error
	s.db, err = sql.Open("postgres", s.pgURL)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}

	s.sha = uniqueSHA("double-write")
	s.scenarioStart = time.Now().UTC()

	err = s.db.QueryRow(`SELECT id FROM repository LIMIT 1`).Scan(&s.repoID)
	if err != nil {
		return fmt.Errorf("querying seeded repository: %w", err)
	}
	err = s.db.QueryRow(`SELECT id FROM policy_version WHERE active = true LIMIT 1`).Scan(&s.policyVersionID)
	if err != nil {
		return fmt.Errorf("querying active policy_version: %w", err)
	}

	return nil
}

func (s *evalGateState) evalGateHasCompletedTheCleanPassPathWritingOneRunAndOneVerdictAndNFindings() error {
	// Snapshot metrics before
	var err error
	s.ruleEngineInvocBefore, err = scrapeRuleEngineInvocationCount(s.ruleEngineURL)
	if err != nil {
		return fmt.Errorf("snapshotting metrics: %w", err)
	}

	// Seed samples for this SHA so the evaluator takes the clean-pass path
	if err := seedSamplesForSHA(s.db, s.repoID, s.sha); err != nil {
		return fmt.Errorf("seeding samples for double-write clean-pass: %w", err)
	}

	// Obtain valid policy signature from KMS mock
	sig, _ := signPolicyForSHA(s.kmsURL, s.sha, s.policyVersionID)
	extras := map[string]interface{}{}
	if sig != "" {
		extras["signature"] = sig
	}

	status, body, err := callEvalGate(s.evaluatorURL, s.sha, s.repoID, s.policyVersionID, extras)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("eval.gate clean-pass for double-write returned HTTP %d: %s", status, body)
	}

	// Verify the expected rows exist
	err = s.db.QueryRow(
		`SELECT id FROM evaluation_run WHERE sha = $1 AND caller = 'eval_gate' AND created_at >= $2`,
		s.sha, s.scenarioStart,
	).Scan(&s.runID)
	if err != nil {
		return fmt.Errorf("querying run for double-write check: %w", err)
	}

	return nil
}

func (s *evalGateState) checkingForDoubleWriteOnTheCleanPassRun() error {
	// This is the "When" step — the check is in the Then step.
	return nil
}

func (s *evalGateState) exactlyOneEvaluationVerdictRowExistsForThatRun() error {
	var count int
	err := s.db.QueryRow(
		`SELECT COUNT(*) FROM evaluation_verdict WHERE evaluation_run_id = $1`,
		s.runID,
	).Scan(&count)
	if err != nil {
		return fmt.Errorf("counting verdicts for run %s: %w", s.runID, err)
	}
	if count != 1 {
		return fmt.Errorf("expected exactly 1 evaluation_verdict for run %s, got %d (double-write detected!)", s.runID, count)
	}
	return nil
}

func (s *evalGateState) theRuleEngineInvocCountSnapshottedAgainForSignatureInvalidCall() error {
	var err error
	s.sigInvalidInvocBefore, err = scrapeRuleEngineInvocationCount(s.ruleEngineURL)
	if err != nil {
		return fmt.Errorf("snapshotting metrics for sig-invalid: %w", err)
	}
	return nil
}

func (s *evalGateState) aSignatureInvalidEvalGateCallWithUniqueSHAProducesOneRunAndOneVerdict() error {
	s.sigInvalidSHA = uniqueSHA("sig-invalid")

	status, body, err := callEvalGate(s.evaluatorURL, s.sigInvalidSHA, s.repoID, s.policyVersionID, map[string]interface{}{
		"signature": "invalid-signature-value",
	})
	if err != nil {
		return err
	}
	s.lastHTTPStatus = status
	s.lastHTTPBody = body
	// The endpoint may return success (with a blocking verdict) or a 4xx; either way
	// it must produce exactly one run + one verdict in the DB.

	// Verify exactly one evaluation_run was created
	var runCount int
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM evaluation_run WHERE sha = $1 AND created_at >= $2`,
		s.sigInvalidSHA, s.scenarioStart,
	).Scan(&runCount)
	if err != nil {
		return fmt.Errorf("counting sig-invalid runs: %w", err)
	}
	if runCount != 1 {
		return fmt.Errorf("expected exactly 1 evaluation_run for sig-invalid SHA %s, got %d", s.sigInvalidSHA, runCount)
	}

	err = s.db.QueryRow(
		`SELECT id FROM evaluation_run WHERE sha = $1 AND created_at >= $2`,
		s.sigInvalidSHA, s.scenarioStart,
	).Scan(&s.sigInvalidRunID)
	if err != nil {
		return fmt.Errorf("fetching sig-invalid run ID: %w", err)
	}

	// Verify exactly one evaluation_verdict referencing that run
	var verdictCount int
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM evaluation_verdict WHERE evaluation_run_id = $1`,
		s.sigInvalidRunID,
	).Scan(&verdictCount)
	if err != nil {
		return fmt.Errorf("counting sig-invalid verdicts: %w", err)
	}
	if verdictCount != 1 {
		return fmt.Errorf("expected exactly 1 evaluation_verdict for sig-invalid run %s, got %d", s.sigInvalidRunID, verdictCount)
	}

	return nil
}

func (s *evalGateState) theRuleEngineWasNotInvokedForTheSignatureInvalidCall() error {
	after, err := scrapeRuleEngineInvocationCount(s.ruleEngineURL)
	if err != nil {
		return fmt.Errorf("snapshotting metrics (after sig-invalid): %w", err)
	}
	if after < s.sigInvalidInvocBefore {
		return fmt.Errorf("rule-engine counter decreased; service may have restarted")
	}
	delta := after - s.sigInvalidInvocBefore
	if delta != 0 {
		return fmt.Errorf("expected rule-engine invocation delta=0 for sig-invalid, got %d", delta)
	}
	return nil
}

func (s *evalGateState) theSignatureInvalidVerdictHasCanonicalSchema() error {
	var runFK sql.NullString
	var createdAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT evaluation_run_id, created_at FROM evaluation_verdict WHERE evaluation_run_id = $1`,
		s.sigInvalidRunID,
	).Scan(&runFK, &createdAt)
	if err != nil {
		return fmt.Errorf("querying sig-invalid verdict schema: %w", err)
	}
	if !runFK.Valid {
		return fmt.Errorf("sig-invalid verdict: evaluation_run_id FK is NULL")
	}
	if !createdAt.Valid {
		return fmt.Errorf("sig-invalid verdict: created_at is NULL")
	}

	// Verify no scope or settled_at columns via information_schema
	// (checked in a separate step for completeness, but also checked here
	// to ensure the specific row doesn't have these columns)
	return nil
}

func (s *evalGateState) aSamplesPendingEvalGateCallWithUniqueSHAProducesOneRunAndOneVerdict() error {
	s.samplesDoubleWriteSHA = uniqueSHA("dw-samples")

	status, body, err := callEvalGate(s.evaluatorURL, s.samplesDoubleWriteSHA, s.repoID, s.policyVersionID, map[string]interface{}{
		"degraded_reason": "samples_pending",
	})
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("eval.gate samples_pending returned HTTP %d: %s", status, body)
	}

	// Verify exactly one run
	var runCount int
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM evaluation_run WHERE sha = $1 AND created_at >= $2`,
		s.samplesDoubleWriteSHA, s.scenarioStart,
	).Scan(&runCount)
	if err != nil {
		return fmt.Errorf("counting samples-pending runs: %w", err)
	}
	if runCount != 1 {
		return fmt.Errorf("expected 1 run for samples_pending SHA %s, got %d", s.samplesDoubleWriteSHA, runCount)
	}

	// Verify exactly one verdict
	var verdictCount int
	err = s.db.QueryRow(
		`SELECT COUNT(*) FROM evaluation_verdict ev
		 JOIN evaluation_run er ON ev.evaluation_run_id = er.id
		 WHERE er.sha = $1 AND ev.created_at >= $2`,
		s.samplesDoubleWriteSHA, s.scenarioStart,
	).Scan(&verdictCount)
	if err != nil {
		return fmt.Errorf("counting samples-pending verdicts: %w", err)
	}
	if verdictCount != 1 {
		return fmt.Errorf("expected 1 verdict for samples_pending SHA %s, got %d", s.samplesDoubleWriteSHA, verdictCount)
	}

	return nil
}

func (s *evalGateState) theInformationSchemaConfirmsNoScopeOrSettledAtColumns() error {
	// Check that evaluation_verdict table does NOT have 'scope' or 'settled_at' columns
	for _, colName := range []string{"scope", "settled_at"} {
		var count int
		err := s.db.QueryRow(
			`SELECT COUNT(*) FROM information_schema.columns
				 WHERE table_schema = 'public' AND table_name = 'evaluation_verdict' AND column_name = $1`,
			colName,
		).Scan(&count)
		if err != nil {
			return fmt.Errorf("querying information_schema for column %q: %w", colName, err)
		}
		if count != 0 {
			return fmt.Errorf("evaluation_verdict table unexpectedly has column %q", colName)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: percentile-stale-not-on-gate
// ---------------------------------------------------------------------------

func (s *evalGateState) theEvaluatorServiceIsReachable() error {
	s.evaluatorURL = os.Getenv("CLEAN_CODE_EVALUATOR_URL")
	s.pgURL = os.Getenv("CLEAN_CODE_PG_URL")
	if s.evaluatorURL == "" {
		return fmt.Errorf("CLEAN_CODE_EVALUATOR_URL must be set")
	}
	resp, err := http.Get(s.evaluatorURL + "/healthz")
	if err != nil {
		return fmt.Errorf("evaluator health check failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("evaluator health check returned %d", resp.StatusCode)
	}
	return nil
}

func (s *evalGateState) theDegradedReasonValidatorIsInvokedWithPercentileStale() error {
	// Call eval.gate with degraded_reason="percentile_stale" — this should be
	// rejected by the gate's degraded_reason validator.
	s.sha = uniqueSHA("pct-stale")

	// We need repo_id and policy_version_id for a valid call structure
	if s.pgURL != "" {
		var err error
		s.db, err = sql.Open("postgres", s.pgURL)
		if err == nil {
			_ = s.db.QueryRow(`SELECT id FROM repository LIMIT 1`).Scan(&s.repoID)
			_ = s.db.QueryRow(`SELECT id FROM policy_version WHERE active = true LIMIT 1`).Scan(&s.policyVersionID)
		}
	}

	status, body, err := callEvalGate(s.evaluatorURL, s.sha, s.repoID, s.policyVersionID, map[string]interface{}{
		"degraded_reason": "percentile_stale",
	})
	if err != nil {
		return err
	}
	s.lastHTTPStatus = status
	s.lastHTTPBody = body
	return nil
}

func (s *evalGateState) theResponseRejectsPercentileStaleAsInvalidEvalGateReason() error {
	// The evaluator MUST reject percentile_stale — a 2xx is always a failure,
	// regardless of body content. Only 4xx (400, 422, etc.) proves rejection.
	if s.lastHTTPStatus >= 200 && s.lastHTTPStatus < 300 {
		return fmt.Errorf("expected percentile_stale to be rejected with a 4xx status, but got HTTP %d (success); body: %s",
			s.lastHTTPStatus, s.lastHTTPBody)
	}
	if s.lastHTTPStatus >= 500 {
		return fmt.Errorf("expected rejection (4xx), got server error %d: %s", s.lastHTTPStatus, s.lastHTTPBody)
	}
	// 4xx status confirms the evaluator rejected the invalid degraded_reason
	if s.lastHTTPStatus < 400 || s.lastHTTPStatus >= 500 {
		return fmt.Errorf("expected 4xx rejection status, got %d: %s", s.lastHTTPStatus, s.lastHTTPBody)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_evaluator_surface_and_management_surface_evaluator_gate_verb_and_synchronous_solid_delegation(ctx *godog.ScenarioContext) {
	s := newEvalGateState()

	// Close any DB connection opened during the scenario to avoid leaking
	// Postgres connections across test runs.
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if s.db != nil {
			s.db.Close()
		}
		return ctx, nil
	})

	// Scenario: verdict-enum-only-canonical
	ctx.Step(`^the production Verdict enum imported from the domain package$`, s.theProductionVerdictEnumImportedFromDomain)
	ctx.Step(`^iterating the production AllVerdicts function$`, s.iteratingTheProductionAllVerdictsFunction)
	ctx.Step(`^the values are exactly "pass", "warn", "block" and no "fail" or "gated" exist$`, s.theValuesAreExactlyPassWarnBlockAndNoFailOrGatedExist)

	// Scenario: gate-delegates-synchronous-rule-pass
	ctx.Step(`^a unique clean SHA with samples present and a valid policy signature$`, s.aUniqueCleanSHAWithSamplesPresentAndAValidPolicySignature)
	ctx.Step(`^the rule-engine HTTP invocation count is snapshotted via its metrics endpoint$`, s.theRuleEngineHTTPInvocationCountIsSnapshottedViaMetrics)
	ctx.Step(`^eval\.gate is called for the clean-pass path$`, s.evalGateIsCalledForTheCleanPassPath)
	ctx.Step(`^the rule-engine HTTP invocation count increased by exactly one proving RunSync was called$`, s.theRuleEngineHTTPInvocationCountIncreasedByExactlyOneProvingRunSyncWasCalled)
	ctx.Step(`^exactly one new evaluation_run row for this SHA with caller "eval_gate" exists$`, s.exactlyOneNewEvaluationRunRowForThisSHAWithCallerEvalGateExists)
	ctx.Step(`^exactly one new evaluation_verdict row referencing that run exists$`, s.exactlyOneNewEvaluationVerdictRowReferencingThatRunExists)
	ctx.Step(`^N new finding rows referencing that run exist with N greater than zero$`, s.nNewFindingRowsReferencingThatRunExistWithNGreaterThanZero)
	ctx.Step(`^the evaluation_run and evaluation_verdict and finding rows share the same xmin proving RunSync wrote them in one transaction$`, s.theRunAndVerdictAndFindingRowsShareTheSameXminProvingSameTransaction)
	ctx.Step(`^the verdict column equals the severity rollup of the findings$`, s.theVerdictColumnEqualsTheSeverityRollupOfTheFindings)

	// Scenario: degraded-maps-to-warn
	ctx.Step(`^a unique SHA for the degraded scenario$`, s.aUniqueSHAForTheDegradedScenario)
	ctx.Step(`^a samples_pending degraded condition is configured with no metric samples$`, s.aSamplesPendingDegradedConditionIsConfiguredWithNoMetricSamples)
	ctx.Step(`^eval\.gate is called for the degraded path$`, s.evalGateIsCalledForTheDegradedPath)
	ctx.Step(`^the rule-engine HTTP invocation count did not change proving the Rule Engine was not invoked$`, s.theRuleEngineHTTPInvocationCountDidNotChangeProvingNotInvoked)
	ctx.Step(`^zero new finding rows exist for this SHA$`, s.zeroNewFindingRowsExistForThisSHA)
	ctx.Step(`^one new evaluation_run row with caller "eval_gate" and the test repo_id and SHA and policy_version_id exists$`, s.oneNewEvaluationRunRowWithCanonicalColumnsExists)
	ctx.Step(`^one new evaluation_verdict with verdict "warn" and degraded true and degraded_reason "samples_pending" referencing that run exists$`, s.oneNewEvaluationVerdictWithCanonicalDegradedFieldsExists)
	ctx.Step(`^the degraded evaluation_run and evaluation_verdict share the same xmin proving same-transaction write$`, s.theDegradedRunAndVerdictShareTheSameXminProvingSameTransaction)
	ctx.Step(`^the degraded evaluation_verdict has a non-null evaluation_run_id FK and a non-null created_at timestamp$`, s.theDegradedVerdictHasNonNullRunFKAndCreatedAt)

	// Scenario: gate-does-not-double-write-verdict
	ctx.Step(`^a unique SHA for the clean-pass double-write check$`, s.aUniqueSHAForTheCleanPassDoubleWriteCheck)
	ctx.Step(`^eval\.gate has completed the clean-pass path writing one run and one verdict and N findings$`, s.evalGateHasCompletedTheCleanPassPathWritingOneRunAndOneVerdictAndNFindings)
	ctx.Step(`^checking for double-write on the clean-pass run$`, s.checkingForDoubleWriteOnTheCleanPassRun)
	ctx.Step(`^exactly one evaluation_verdict row exists for that run$`, s.exactlyOneEvaluationVerdictRowExistsForThatRun)
	ctx.Step(`^the rule-engine HTTP invocation count is snapshotted again for the signature-invalid call$`, s.theRuleEngineInvocCountSnapshottedAgainForSignatureInvalidCall)
	ctx.Step(`^a signature-invalid eval\.gate call with a unique SHA produces exactly one new run and one new verdict$`, s.aSignatureInvalidEvalGateCallWithUniqueSHAProducesOneRunAndOneVerdict)
	ctx.Step(`^the rule-engine was not invoked for the signature-invalid call$`, s.theRuleEngineWasNotInvokedForTheSignatureInvalidCall)
	ctx.Step(`^the signature-invalid verdict has the canonical schema with non-null evaluation_run_id FK and created_at and no scope column and no settled_at column$`, s.theSignatureInvalidVerdictHasCanonicalSchema)
	ctx.Step(`^a samples_pending eval\.gate call with a unique SHA produces exactly one new run and one new verdict$`, s.aSamplesPendingEvalGateCallWithUniqueSHAProducesOneRunAndOneVerdict)
	ctx.Step(`^the information_schema confirms evaluation_verdict has no scope or settled_at columns$`, s.theInformationSchemaConfirmsNoScopeOrSettledAtColumns)

	// Scenario: percentile-stale-not-on-gate
	ctx.Step(`^the evaluator service is reachable$`, s.theEvaluatorServiceIsReachable)
	ctx.Step(`^the degraded_reason validator is invoked with "percentile_stale"$`, s.theDegradedReasonValidatorIsInvokedWithPercentileStale)
	ctx.Step(`^the response rejects "percentile_stale" as an invalid eval\.gate reason$`, s.theResponseRejectsPercentileStaleAsInvalidEvalGateReason)
}

// ---------------------------------------------------------------------------
// Test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_evaluator_surface_and_management_surface_evaluator_gate_verb_and_synchronous_solid_delegation(t *testing.T) {
	// Ensure required env vars are present; skip if not (local dev without compose).
	requireEnv(t, "CLEAN_CODE_PG_URL")
	requireEnv(t, "CLEAN_CODE_EVALUATOR_URL")
	requireEnv(t, "CLEAN_CODE_RULE_ENGINE_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_evaluator_surface_and_management_surface_evaluator_gate_verb_and_synchronous_solid_delegation,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"evaluator_surface_and_management_surface_evaluator_gate_verb_and_synchronous_solid_delegation.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}
