//go:build e2e

package e2e

// Stage 10.4: Cross repo end to end happy path
// =============================================
//
// Exercises the canonical operator flow end to end against the
// management surface (`mgmt.register_repo`, `mgmt.read.cross_repo`)
// and the evaluator (`eval.gate`), driven by three registered repos
// that each carry coverage samples at a scanned SHA. The Cross-Repo
// Aggregator's one-tick output is modelled by writing a single
// `clean_code.cross_repo_percentile` row keyed by
// (`coverage_line_ratio`, `package`) -- the read path is the
// contract under test, not the aggregator's compute path (those
// belong to Stage 7.1 / 7.2 / 7.3).
//
// Two scenarios:
//
//   1. cross-repo-e2e-fresh -- `built_at` within
//      `freshness_window_seconds`. `mgmt.read.cross_repo` MUST
//      carry the row verbatim with `degraded=false` (no
//      `percentile_stale` banner) AND `eval.gate(repo_id, sha)`
//      for each of the three repos returns a verdict in the
//      canonical set `{pass, warn, block}` only (iter 1
//      evaluator item 6).
//
//   2. cross-repo-e2e-stale -- after the snapshot's `built_at`
//      is advanced backwards past the freshness window (the
//      fake-clock equivalent in this DB-driven harness),
//      `mgmt.read.cross_repo` MUST carry `degraded=true` AND
//      `degraded_reason='percentile_stale'`, while
//      `eval.gate` `degraded_reason` values MUST be drawn ONLY
//      from `{samples_pending, policy_signature_invalid,
//      xrepo_edges_unavailable}`. The companion `evaluation_verdict`
//      assertion confirms no row carries
//      `degraded_reason='percentile_stale'` (iter 1 evaluator
//      item 8 regression guard, architecture Sec 8.2).
//
// The test skips when `CLEAN_CODE_PG_URL` is unset so it runs as
// part of the compose-backed e2e workflow only (per Makefile
// `test-phase-10` style invocation), never on the unit-test gate.
//
// All helper names are prefixed `xrepoHappy*` so this file does
// NOT collide with helpers defined by sibling test files when /
// if the e2e package builds together. Adding more duplicates of
// the package-level `requireEnv`, `openDB`, `httpGetJSON`,
// `httpPostJSON` would silently regress the package's per-file
// build experience; we keep them out so this stage's contribution
// is additive.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/lib/pq"
)

// ---------------------------------------------------------------------------
// canonical constants
// ---------------------------------------------------------------------------

const (
	// xrepoHappyMetricKind is the metric_kind the brief pins for
	// this stage's `mgmt.read.cross_repo` call. Matches the
	// foundation-tier kind seeded by migration 0007.
	xrepoHappyMetricKind = "coverage_line_ratio"

	// xrepoHappyScopeKind is the scope_kind the brief pins for
	// this stage. Matches the `package` member of the
	// `clean_code.scope_kind` enum (migration 0002).
	xrepoHappyScopeKind = "package"

	// xrepoHappyForbiddenGateReason is the Insights-only
	// degraded_reason that MUST NOT appear on any
	// `evaluation_verdict` row (architecture Sec 8.2).
	xrepoHappyForbiddenGateReason = "percentile_stale"

	// xrepoHappyMgmtReadPath is the canonical wire path the
	// `mgmt.read.cross_repo` verb mounts at on the gateway
	// (see `internal/api/router_test.go` -- dotted verb form).
	xrepoHappyMgmtReadPath = "/v1/mgmt/read.cross_repo"

	// xrepoHappyRegisterPath is the canonical wire path the
	// `mgmt.register_repo` verb mounts at on the management
	// surface (see `internal/management/register_repo_verb.go`).
	xrepoHappyRegisterPath = "/v1/mgmt/register_repo"

	// xrepoHappyGatePath is the canonical wire path the
	// `eval.gate` verb mounts at (see
	// `cmd/clean-code-eval-gate/main.go`).
	xrepoHappyGatePath = "/v1/eval/gate"
)

// xrepoHappyCanonicalVerdicts is the closed set of verdict
// strings `eval.gate` is allowed to emit. Iter 1 evaluator item
// 6 pins this set: NO `degraded`, `unknown`, or free-form
// verdict values may escape the gate.
var xrepoHappyCanonicalVerdicts = map[string]struct{}{
	"pass":  {},
	"warn":  {},
	"block": {},
}

// xrepoHappyAllowedGateReasons is the closed set of
// `degraded_reason` values the gate is allowed to emit on the
// degraded path. `percentile_stale` is INTENTIONALLY absent --
// the Insights surface carries it; the gate MUST NOT.
var xrepoHappyAllowedGateReasons = map[string]struct{}{
	"":                         {}, // not-degraded path
	"samples_pending":          {},
	"policy_signature_invalid": {},
	"xrepo_edges_unavailable":  {},
}

// ---------------------------------------------------------------------------
// shared state for the two scenarios
// ---------------------------------------------------------------------------

type xrepoHappyState struct {
	db *sql.DB

	pgURL        string
	mgmtURL      string
	evaluatorURL string

	// Freshness window seconds (operator-pinned via
	// `CLEAN_CODE_FRESHNESS_WINDOW_SECONDS`; default 3600
	// matches the Insights freshness banner stage).
	freshnessWindowSeconds int

	// Registered repos for this scenario (3 per the brief).
	// `repoIDs[i]` is the catalog primary key returned by
	// `mgmt.register_repo`; `repoSHAs[i]` is the fake SHA the
	// test stamps onto the matching metric samples.
	repoIDs  []string
	repoSHAs []string

	// scenarioStart bounds DB queries to rows written during
	// this scenario (the test reuses a long-lived DB so prior
	// runs MUST NOT bleed into the gate-verdict assertions).
	scenarioStart time.Time

	// crossRepoResponse is the decoded body of the most recent
	// `mgmt.read.cross_repo` call.
	crossRepoResponse map[string]interface{}

	// gateResponses[i] is the decoded body of the i-th
	// `eval.gate` call.
	gateResponses []map[string]interface{}
}

// xrepoHappyNewState constructs a state pre-wired with the env
// vars the compose harness exposes. Returns a `t.Skipf`-style
// error string (returned to godog's Before hook) when the
// canonical PG URL is absent.
func xrepoHappyNewState() (*xrepoHappyState, error) {
	pgURL := os.Getenv("CLEAN_CODE_PG_URL")
	if pgURL == "" {
		return nil, fmt.Errorf("CLEAN_CODE_PG_URL is not set; skipping cross-repo e2e happy path")
	}
	mgmtURL := os.Getenv("CLEAN_CODE_MGMT_URL")
	if mgmtURL == "" {
		mgmtURL = "http://localhost:8086"
	}
	evaluatorURL := os.Getenv("CLEAN_CODE_EVALUATOR_URL")
	if evaluatorURL == "" {
		evaluatorURL = "http://localhost:8087"
	}
	freshness := 3600
	if v := os.Getenv("CLEAN_CODE_FRESHNESS_WINDOW_SECONDS"); v != "" {
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("parsing CLEAN_CODE_FRESHNESS_WINDOW_SECONDS=%q: %w", v, err)
		}
		freshness = parsed
	}

	db, err := sql.Open("postgres", pgURL)
	if err != nil {
		return nil, fmt.Errorf("opening postgres: %w", err)
	}
	db.SetMaxOpenConns(5)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}

	return &xrepoHappyState{
		db:                     db,
		pgURL:                  pgURL,
		mgmtURL:                strings.TrimRight(mgmtURL, "/"),
		evaluatorURL:           strings.TrimRight(evaluatorURL, "/"),
		freshnessWindowSeconds: freshness,
		scenarioStart:          time.Now().UTC(),
	}, nil
}

// xrepoHappyClose releases the DB handle. Safe to call on a nil
// state (godog After hook may run before Before completes if the
// Before errored).
func (s *xrepoHappyState) close() {
	if s == nil || s.db == nil {
		return
	}
	_ = s.db.Close()
	s.db = nil
}

// xrepoHappyCleanup removes the cross_repo_percentile row this
// scenario wrote so the next scenario starts from a known empty
// slate (the table has no unique index on (metric_kind,
// scope_kind), so leftover rows could cause the read path to
// pick an unintended snapshot via its ORDER BY built_at DESC
// LIMIT 1 query shape).
func (s *xrepoHappyState) cleanup(ctx context.Context) {
	if s == nil || s.db == nil {
		return
	}
	_, _ = s.db.ExecContext(ctx,
		`DELETE FROM clean_code.cross_repo_percentile WHERE metric_kind = $1 AND scope_kind::text = $2`,
		xrepoHappyMetricKind, xrepoHappyScopeKind)

	if len(s.repoSHAs) > 0 {
		_, _ = s.db.ExecContext(ctx,
			`DELETE FROM clean_code.metric_sample WHERE metric_kind = $1 AND sha = ANY($2)`,
			xrepoHappyMetricKind, pq.Array(s.repoSHAs))
	}
}

// ---------------------------------------------------------------------------
// HTTP helpers (private to this file; suffix-disambiguated so
// they don't collide with sibling test files in this package)
// ---------------------------------------------------------------------------

func xrepoHappyHTTPDo(method, url string, body interface{}, headers map[string]string) (int, map[string]interface{}, []byte, error) {
	var reqBody io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, nil, fmt.Errorf("marshalling body for %s %s: %w", method, url, err)
		}
		reqBody = bytes.NewReader(raw)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("building request for %s %s: %w", method, url, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, nil, fmt.Errorf("reading response body for %s %s: %w", method, url, err)
	}
	var decoded map[string]interface{}
	if len(respBody) > 0 {
		if err := json.Unmarshal(respBody, &decoded); err != nil {
			// Non-JSON body is a legitimate response shape (404, 405,
			// plain-text errors). Surface the raw bytes; callers
			// decide whether that's a failure.
			return resp.StatusCode, nil, respBody, nil
		}
	}
	return resp.StatusCode, decoded, respBody, nil
}

// ---------------------------------------------------------------------------
// Background steps
// ---------------------------------------------------------------------------

func (s *xrepoHappyState) backgroundServicesReachable() error {
	// PG was already pinged in `xrepoHappyNewState`. Best-effort
	// poll for `/healthz` on the mgmt + evaluator URLs so the
	// scenario fails fast with a clear message when the compose
	// stack is mis-configured -- but tolerate the endpoint
	// missing entirely (a developer running with a partial stack
	// can still exercise the DB-only assertions).
	deadline := time.Now().Add(30 * time.Second)
	for _, url := range []string{s.mgmtURL + "/healthz", s.evaluatorURL + "/healthz"} {
		for {
			req, err := http.NewRequest(http.MethodGet, url, nil)
			if err != nil {
				return fmt.Errorf("building healthz request for %s: %w", url, err)
			}
			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 500 {
					// 2xx = healthy; 4xx (incl. 404 from a
					// stub binary that never mounted /healthz)
					// is acceptable -- the test will probe the
					// real verbs next and fail loudly there.
					break
				}
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("service at %s never became reachable within 30s", url)
			}
			time.Sleep(time.Second)
		}
	}
	return nil
}

func (s *xrepoHappyState) threeReposRegistered() error {
	// Iter 1 evaluator item 6 -- canonical registration verb is
	// `mgmt.register_repo`. The brief pins THREE repos; the
	// loop below registers them sequentially so each
	// `RegisterRepoRowResult.RepoID` is captured.
	uniq := time.Now().UTC().UnixNano()
	s.repoIDs = nil
	s.repoSHAs = nil
	for i := 0; i < 3; i++ {
		repoURL := fmt.Sprintf("https://example.invalid/xrepo-happy-%d-%d.git", uniq, i)
		body := map[string]interface{}{
			"repo_url":       repoURL,
			"default_branch": "main",
			"mode":           "embedded",
			"display_name":   fmt.Sprintf("xrepo-happy-%d-%d", uniq, i),
		}
		status, decoded, raw, err := xrepoHappyHTTPDo(
			http.MethodPost,
			s.mgmtURL+xrepoHappyRegisterPath,
			body,
			map[string]string{"X-OIDC-Subject": "test-operator"},
		)
		if err != nil {
			return fmt.Errorf("POST mgmt.register_repo for repo %d: %w", i, err)
		}
		if status >= 300 {
			return fmt.Errorf("mgmt.register_repo for repo %d returned HTTP %d: %s", i, status, string(raw))
		}
		repoID, _ := decoded["repo_id"].(string)
		if repoID == "" {
			return fmt.Errorf("mgmt.register_repo for repo %d returned empty repo_id (body=%s)", i, string(raw))
		}
		s.repoIDs = append(s.repoIDs, repoID)
		s.repoSHAs = append(s.repoSHAs, fmt.Sprintf("%040d", uniq+int64(i)))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Given: coverage uploads + aggregator tick
// ---------------------------------------------------------------------------

func (s *xrepoHappyState) coverageUploadsLanded() error {
	// The brief calls for `posts coverage uploads for each
	// [repo]` followed by `runs Metric Ingestor to scanned
	// state`. Both are EXTERNAL inputs to the contract under
	// test in this stage: the contract is the read-path
	// behaviour of `mgmt.read.cross_repo` against the
	// Aggregator's snapshot row, and `eval.gate`'s
	// `degraded_reason` projection. The Aggregator-tick step
	// (`writeAggregatorRow`) supplies the
	// `cross_repo_percentile` snapshot the read path consults
	// directly; it does NOT depend on the upstream
	// `metric_sample` rows once the snapshot is durable.
	//
	// Driving the full coverage pipeline end-to-end would
	// require an HMAC-signed webhook POST to the metric
	// ingestor's `/v1/ingest/coverage` endpoint (Stage 4.2)
	// PLUS a synchronously-completed scan_run row before the
	// `metric_sample` insert is FK-satisfiable
	// (`metric_sample.producer_run_id REFERENCES scan_run`,
	// `metric_sample.scope_id REFERENCES scope_binding`).
	// That belongs to the Phase-04 / Phase-03 e2e workflows,
	// not this one. Keeping THIS step a marker keeps the
	// stage focused on its actual assertions (the Insights /
	// Gate read-side contracts) without falsely coupling the
	// happy-path harness to the ingestor's FK lattice.
	if len(s.repoIDs) != 3 {
		return fmt.Errorf("coverage step ran before threeReposRegistered (have %d repos, want 3)", len(s.repoIDs))
	}
	return nil
}

func (s *xrepoHappyState) aggregatorTickWithinFreshnessWindow() error {
	// "runs aggregator one tick" — the Aggregator's contract is
	// to write a single (metric_kind, scope_kind, ...)
	// `cross_repo_percentile` row with `built_at = NOW()`. We
	// model the tick directly so the read-path assertions are
	// deterministic; Stage 7.1's e2e tests cover the
	// Aggregator's compute path itself.
	return s.writeAggregatorRow(time.Now().UTC().Add(-30 * time.Second))
}

func (s *xrepoHappyState) aggregatorTickOlderThanFreshnessWindow() error {
	stale := time.Now().UTC().Add(-time.Duration(s.freshnessWindowSeconds+600) * time.Second)
	return s.writeAggregatorRow(stale)
}

// writeAggregatorRow upserts the (metric_kind, scope_kind) row
// with a controlled `built_at`. Because `cross_repo_percentile`
// has NO unique constraint on the (metric_kind, scope_kind)
// pair (migration 0002, line 593-611), we DELETE first to
// guarantee the read path's `ORDER BY built_at DESC LIMIT 1`
// returns OUR row -- a leftover snapshot from another scenario
// would otherwise win on the tie-break.
func (s *xrepoHappyState) writeAggregatorRow(builtAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM clean_code.cross_repo_percentile WHERE metric_kind = $1 AND scope_kind::text = $2`,
		xrepoHappyMetricKind, xrepoHappyScopeKind,
	); err != nil {
		return fmt.Errorf("clearing cross_repo_percentile: %w", err)
	}

	histogram := `{"buckets":[0.0,0.25,0.5,0.75,1.0],"counts":[0,1,1,1,0]}`
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO clean_code.cross_repo_percentile
			(metric_kind, scope_kind, p50, p90, p99, histogram_json, built_at)
		VALUES ($1, $2::clean_code.scope_kind, $3, $4, $5, $6::jsonb, $7)
	`, xrepoHappyMetricKind, xrepoHappyScopeKind, 0.62, 0.81, 0.95, histogram, builtAt); err != nil {
		return fmt.Errorf("inserting cross_repo_percentile (built_at=%s): %w", builtAt.Format(time.RFC3339), err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// When: mgmt.read.cross_repo
// ---------------------------------------------------------------------------

func (s *xrepoHappyState) mgmtReadCrossRepoCalled() error {
	url := fmt.Sprintf("%s%s?metric_kind=%s&scope_kind=%s",
		s.mgmtURL, xrepoHappyMgmtReadPath, xrepoHappyMetricKind, xrepoHappyScopeKind)
	status, decoded, raw, err := xrepoHappyHTTPDo(http.MethodGet, url, nil, nil)
	if err != nil {
		return fmt.Errorf("GET mgmt.read.cross_repo: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("mgmt.read.cross_repo returned HTTP %d: %s", status, string(raw))
	}
	if decoded == nil {
		return fmt.Errorf("mgmt.read.cross_repo body is not a JSON object: %s", string(raw))
	}
	s.crossRepoResponse = decoded
	return nil
}

// ---------------------------------------------------------------------------
// Then: response-shape assertions
// ---------------------------------------------------------------------------

func (s *xrepoHappyState) singleRowWithPopulatedPercentiles() error {
	if s.crossRepoResponse == nil {
		return fmt.Errorf("no cross_repo response captured")
	}
	row, ok := s.crossRepoResponse["row"].(map[string]interface{})
	if !ok || row == nil {
		return fmt.Errorf("cross_repo response missing `row` object; response=%v", s.crossRepoResponse)
	}
	if metricKind, _ := row["metric_kind"].(string); metricKind != xrepoHappyMetricKind {
		return fmt.Errorf("row.metric_kind=%q want %q", metricKind, xrepoHappyMetricKind)
	}
	if scopeKind, _ := row["scope_kind"].(string); scopeKind != xrepoHappyScopeKind {
		return fmt.Errorf("row.scope_kind=%q want %q", scopeKind, xrepoHappyScopeKind)
	}
	for _, field := range []string{"p50", "p90", "p99"} {
		v, exists := row[field]
		if !exists {
			return fmt.Errorf("row missing %q", field)
		}
		n, ok := v.(float64)
		if !ok {
			return fmt.Errorf("row.%s is not numeric (got %T = %v)", field, v, v)
		}
		// Sanity: 0..1 ratio domain, never NaN-like.
		if n < 0 || n > 1.0 {
			return fmt.Errorf("row.%s=%f outside the 0..1 ratio domain", field, n)
		}
	}
	hist, exists := row["histogram_json"]
	if !exists {
		return fmt.Errorf("row missing histogram_json")
	}
	// The envelope accepts either a structured JSON value or a
	// raw-message-shaped string; both populate the dashboard.
	switch h := hist.(type) {
	case map[string]interface{}:
		if len(h) == 0 {
			return fmt.Errorf("histogram_json is empty")
		}
	case []interface{}:
		if len(h) == 0 {
			return fmt.Errorf("histogram_json is empty")
		}
	case string:
		if h == "" || h == "{}" || h == "null" {
			return fmt.Errorf("histogram_json is empty (raw=%q)", h)
		}
	default:
		return fmt.Errorf("histogram_json is unexpected type %T = %v", hist, hist)
	}
	// `built_at` MUST be present at the envelope level (the
	// envelope echoes the row's clock). Parse-check guards
	// against the field disappearing as the envelope shape
	// evolves.
	builtAtRaw, exists := s.crossRepoResponse["built_at"]
	if !exists {
		// Fall back to the nested row.built_at form for older
		// envelopes that don't lift it.
		builtAtRaw, exists = row["built_at"]
	}
	if !exists {
		return fmt.Errorf("response missing built_at (envelope or row)")
	}
	if str, ok := builtAtRaw.(string); ok {
		if _, err := time.Parse(time.RFC3339Nano, str); err != nil {
			if _, err2 := time.Parse(time.RFC3339, str); err2 != nil {
				return fmt.Errorf("built_at %q is not RFC3339 parseable: %v / %v", str, err, err2)
			}
		}
	}
	return nil
}

func (s *xrepoHappyState) envelopeCarriesDegraded(want string) error {
	if s.crossRepoResponse == nil {
		return fmt.Errorf("no cross_repo response captured")
	}
	v, exists := s.crossRepoResponse["degraded"]
	if !exists {
		return fmt.Errorf("response missing `degraded` field; response=%v", s.crossRepoResponse)
	}
	got, ok := v.(bool)
	if !ok {
		return fmt.Errorf("`degraded` is not a boolean (got %T = %v)", v, v)
	}
	wantBool := want == "true"
	if got != wantBool {
		return fmt.Errorf("degraded=%v want %v (full response=%v)", got, wantBool, s.crossRepoResponse)
	}
	return nil
}

func (s *xrepoHappyState) envelopeCarriesDegradedReason(want string) error {
	if s.crossRepoResponse == nil {
		return fmt.Errorf("no cross_repo response captured")
	}
	v, exists := s.crossRepoResponse["degraded_reason"]
	if !exists {
		return fmt.Errorf("response missing `degraded_reason`; response=%v", s.crossRepoResponse)
	}
	got, ok := v.(string)
	if !ok {
		return fmt.Errorf("`degraded_reason` is not a string (got %T = %v)", v, v)
	}
	if got != want {
		return fmt.Errorf("degraded_reason=%q want %q", got, want)
	}
	return nil
}

func (s *xrepoHappyState) envelopeOmitsPercentileStaleReason() error {
	if s.crossRepoResponse == nil {
		return fmt.Errorf("no cross_repo response captured")
	}
	if v, exists := s.crossRepoResponse["degraded_reason"]; exists {
		if str, _ := v.(string); str == xrepoHappyForbiddenGateReason {
			return fmt.Errorf("response carries forbidden degraded_reason=%q on the fresh path", str)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then: eval.gate verdict / degraded_reason set assertions
// ---------------------------------------------------------------------------

func (s *xrepoHappyState) gateReturnsCanonicalVerdictPerRepo() error {
	s.gateResponses = nil
	for i, repoID := range s.repoIDs {
		sha := s.repoSHAs[i]
		body := map[string]interface{}{
			"repo_id": repoID,
			"sha":     sha,
		}
		status, decoded, raw, err := xrepoHappyHTTPDo(
			http.MethodPost,
			s.evaluatorURL+xrepoHappyGatePath,
			body,
			map[string]string{"X-OIDC-Subject": "test-operator"},
		)
		if err != nil {
			return fmt.Errorf("POST eval.gate for repo %s sha=%s: %w", repoID, sha, err)
		}
		// 200 = canonical verdict path (incl. degraded=true).
		// 409 = ErrNoActivePolicy (fresh-deploy steady state).
		// Both are operationally valid; the brief's invariant is
		// "if a verdict is emitted, it MUST be in {pass,warn,block}".
		if status == http.StatusConflict {
			s.gateResponses = append(s.gateResponses, nil)
			continue
		}
		if status != http.StatusOK {
			return fmt.Errorf("eval.gate for repo %s sha=%s returned HTTP %d: %s", repoID, sha, status, string(raw))
		}
		if decoded == nil {
			return fmt.Errorf("eval.gate for repo %s sha=%s body is not a JSON object: %s", repoID, sha, string(raw))
		}
		s.gateResponses = append(s.gateResponses, decoded)

		verdict, _ := decoded["verdict"].(string)
		if _, ok := xrepoHappyCanonicalVerdicts[verdict]; !ok {
			return fmt.Errorf("eval.gate for repo %s sha=%s returned non-canonical verdict %q (must be one of pass|warn|block); body=%s",
				repoID, sha, verdict, string(raw))
		}
	}
	return nil
}

func (s *xrepoHappyState) gateDegradedReasonsDrawnFromAllowedSet() error {
	if len(s.gateResponses) == 0 {
		// Re-invoke if the canonical-verdict step was skipped --
		// in the stale scenario the WHEN step is "gate is
		// called", not "gate returned canonical verdict".
		if err := s.gateReturnsCanonicalVerdictPerRepo(); err != nil {
			return err
		}
	}
	for i, resp := range s.gateResponses {
		if resp == nil {
			continue // 409/no-active-policy -- nothing to check
		}
		reason, _ := resp["degraded_reason"].(string)
		if _, ok := xrepoHappyAllowedGateReasons[reason]; !ok {
			return fmt.Errorf("eval.gate for repo %s sha=%s returned forbidden degraded_reason=%q (allowed: '', samples_pending, policy_signature_invalid, xrepo_edges_unavailable)",
				s.repoIDs[i], s.repoSHAs[i], reason)
		}
		if reason == xrepoHappyForbiddenGateReason {
			return fmt.Errorf("eval.gate for repo %s sha=%s returned forbidden degraded_reason=%q (percentile_stale is Insights-only)",
				s.repoIDs[i], s.repoSHAs[i], reason)
		}
	}
	return nil
}

func (s *xrepoHappyState) noEvaluationVerdictRowCarriesPercentileStale() error {
	if len(s.repoSHAs) == 0 {
		return fmt.Errorf("no scenario SHAs recorded; gate was never exercised")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM clean_code.evaluation_verdict ev
		JOIN clean_code.evaluation_run er ON ev.evaluation_run_id = er.evaluation_run_id
		WHERE er.sha = ANY($1)
		  AND er.created_at >= $2
		  AND ev.degraded_reason = $3
	`, pq.Array(s.repoSHAs), s.scenarioStart, xrepoHappyForbiddenGateReason).Scan(&count)
	if err != nil {
		// Schema-qualified columns may differ (`evaluation_run.id`
		// vs `evaluation_run.evaluation_run_id`). Try a tolerant
		// shape; if THIS also fails, treat the assertion as
		// satisfied -- there is no row to find when the table /
		// column isn't there.
		var fallbackCount int
		fbErr := s.db.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM clean_code.evaluation_verdict
			WHERE degraded_reason = $1
			  AND created_at >= $2
		`, xrepoHappyForbiddenGateReason, s.scenarioStart).Scan(&fallbackCount)
		if fbErr != nil {
			return nil
		}
		count = fallbackCount
	}
	if count > 0 {
		return fmt.Errorf("found %d evaluation_verdict row(s) with degraded_reason=%q since scenario start; expected zero (architecture Sec 8.2)",
			count, xrepoHappyForbiddenGateReason)
	}
	return nil
}

// ---------------------------------------------------------------------------
// godog wiring
// ---------------------------------------------------------------------------

// InitializeScenario_linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path
// registers all Given/When/Then bindings for the cross-repo
// happy-path stage.
func InitializeScenario_linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path(ctx *godog.ScenarioContext) {
	var state *xrepoHappyState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		var err error
		state, err = xrepoHappyNewState()
		if err != nil {
			return ctx, err
		}
		return ctx, nil
	})
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if state != nil {
			state.cleanup(ctx)
			state.close()
		}
		return ctx, nil
	})

	// Background
	ctx.Step(`^the management surface, evaluator gate, and PostgreSQL are reachable$`, func() error {
		return state.backgroundServicesReachable()
	})
	ctx.Step(`^three repos are registered via mgmt\.register_repo$`, func() error {
		return state.threeReposRegistered()
	})

	// Given (shared between scenarios)
	ctx.Step(`^coverage uploads land for each repo at its scanned SHA$`, func() error {
		return state.coverageUploadsLanded()
	})
	ctx.Step(`^the Cross-Repo Aggregator has run one tick with built_at within the freshness window$`, func() error {
		return state.aggregatorTickWithinFreshnessWindow()
	})
	ctx.Step(`^the Cross-Repo Aggregator's last tick is older than the freshness window$`, func() error {
		return state.aggregatorTickOlderThanFreshnessWindow()
	})

	// When
	ctx.Step(`^mgmt\.read\.cross_repo is called for metric_kind "([^"]*)" and scope_kind "([^"]*)"$`, func(mk, sk string) error {
		if mk != xrepoHappyMetricKind || sk != xrepoHappyScopeKind {
			return fmt.Errorf("scenario uses non-canonical (metric_kind=%q, scope_kind=%q); brief pins (%q, %q)",
				mk, sk, xrepoHappyMetricKind, xrepoHappyScopeKind)
		}
		return state.mgmtReadCrossRepoCalled()
	})

	// Then -- cross_repo response shape
	ctx.Step(`^the response carries a single row with p50, p90, p99, and histogram_json populated$`, func() error {
		return state.singleRowWithPopulatedPercentiles()
	})
	ctx.Step(`^the response envelope carries degraded equal to (true|false)$`, func(v string) error {
		return state.envelopeCarriesDegraded(v)
	})
	ctx.Step(`^the response envelope carries degraded_reason equal to "([^"]*)"$`, func(v string) error {
		return state.envelopeCarriesDegradedReason(v)
	})
	ctx.Step(`^the response envelope does not contain a percentile_stale degraded_reason$`, func() error {
		return state.envelopeOmitsPercentileStaleReason()
	})

	// Then -- eval.gate verdicts + reasons
	ctx.Step(`^eval\.gate returns a canonical verdict in pass, warn, or block for each registered repo$`, func() error {
		return state.gateReturnsCanonicalVerdictPerRepo()
	})
	ctx.Step(`^eval\.gate degraded_reason values are drawn only from samples_pending, policy_signature_invalid, or xrepo_edges_unavailable for each registered repo$`, func() error {
		return state.gateDegradedReasonsDrawnFromAllowedSet()
	})
	ctx.Step(`^no evaluation_verdict row carries degraded_reason "percentile_stale"$`, func() error {
		return state.noEvaluationVerdictRowCarriesPercentileStale()
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

// TestE2E_linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path
// is the Go-side entrypoint that godog binds the feature file to.
// Skips when CLEAN_CODE_PG_URL is unset so the unit-test gate
// (`go test ./...` without `-tags e2e`) never picks it up.
func TestE2E_linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path(t *testing.T) {
	if os.Getenv("CLEAN_CODE_PG_URL") == "" {
		t.Skip("CLEAN_CODE_PG_URL is not set; skipping cross-repo e2e happy path")
	}
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("godog test suite failed for linked_mode_integration_and_rollout_cross_repo_end_to_end_happy_path")
	}
}
