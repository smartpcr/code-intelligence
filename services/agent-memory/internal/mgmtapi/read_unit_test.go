package mgmtapi

// Behavioural unit tests for the Stage 7.5 operator read
// endpoints. The tests are split between this file (fast,
// in-process, sqlmock + httptest) and read_integration_test.go
// (live PostgreSQL, skipped without AGENT_MEMORY_PG_URL).
//
// Unit-test coverage focuses on
//
//   - input-validation short-circuits that MUST happen BEFORE
//     any DB call (the Stage 7.5 brief pins this for
//     `mgmt.read.episodes` so the partition pruner is never
//     bypassed; the rubber-duck pass surfaced the same
//     "verify before DB" rule for SHA / UUID validators);
//   - the §6.2.3 helper matrix (parseSinceParam,
//     parseLimitParam, parseCSVEnumList) — these are pure
//     functions and the closed-set rejections are the only
//     defence against an operator query running a typed-error
//     against the DB;
//   - the §6.3 degraded envelope shape per endpoint (the
//     write-side TestDegradedEnvelope_marshalsFlat covers the
//     marshaller; this file covers the per-verb
//     `degraded:false` default and the "any-repo-degraded
//     aggregates" rule for /v1/repos);
//   - the route table (GET vs POST dispatch under /v1/).
//
// Behaviour that genuinely needs the DB plan (CTE joins for
// `current_status`, `WITH ORDINALITY` hydration for retired
// node cards, SHA point-in-time visibility, trace_observation
// log tail) is covered in read_integration_test.go. The
// rubber-duck pass flagged sqlmock-on-CTE as too brittle to
// rely on for that depth of coverage; the integration pack is
// the source of truth there.

import (
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// authedGet builds a GET request authenticated with the test
// bearer token. Mirrors `authedRequest` (POST) but for the
// read surface — keeps the test bodies focused on assertions.
func authedGet(t *testing.T, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	return r
}

// -----------------------------------------------------------
// parseSinceParam matrix
// -----------------------------------------------------------

func TestParseSinceParam(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		raw      string
		required bool
		wantOK   bool
		wantZero bool
		// when !wantZero, target offset (negative) from `now`.
		wantOffset time.Duration
	}{
		{"empty-optional", "", false, true, true, 0},
		{"empty-required", "", true, false, true, 0},
		{"30s", "30s", false, true, false, -30 * time.Second},
		{"5m", "5m", false, true, false, -5 * time.Minute},
		{"12h", "12h", false, true, false, -12 * time.Hour},
		{"7d", "7d", false, true, false, -7 * 24 * time.Hour},
		{"2w", "2w", false, true, false, -14 * 24 * time.Hour},
		{"rfc3339", "2026-05-10T00:00:00Z", false, true, false, 0},
		{"zero-d-rejected", "0d", false, false, true, 0},
		{"negative-rejected", "-1d", false, false, true, 0},
		{"fractional-rejected", "1.5d", false, false, true, 0},
		{"garbage", "tomorrow", false, false, true, 0},
		{"missing-unit", "5", false, false, true, 0},
		{"unknown-unit", "5y", false, false, true, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok, msg := parseSinceParam(tc.raw, now, tc.required, "since")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (msg=%q)", ok, tc.wantOK, msg)
			}
			if !tc.wantOK {
				if msg == "" {
					t.Error("expected operator-facing message on failure")
				}
				return
			}
			if tc.wantZero {
				if !got.IsZero() {
					t.Errorf("expected zero time, got %v", got)
				}
				return
			}
			if tc.name == "rfc3339" {
				want := time.Date(2026, 5, 10, 0, 0, 0, 0, time.UTC)
				if !got.Equal(want) {
					t.Errorf("rfc3339 parsed = %v, want %v", got, want)
				}
				return
			}
			want := now.Add(tc.wantOffset)
			if !got.Equal(want) {
				t.Errorf("duration cutoff = %v, want %v", got, want)
			}
		})
	}
}

// -----------------------------------------------------------
// parseLimitParam matrix
// -----------------------------------------------------------

func TestParseLimitParam(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw     string
		want    int
		wantErr bool
	}{
		{"", defaultReadLimit, false},
		{"1", 1, false},
		{"42", 42, false},
		{"200", 200, false},
		{"1000", 1000, false},
		{"2000", maxReadLimit, false}, // capped
		{"0", 0, true},
		{"-3", 0, true},
		{"abc", 0, true},
	}
	for _, tc := range cases {
		got, ok, msg := parseLimitParam(tc.raw)
		if (ok != !tc.wantErr) {
			t.Errorf("parseLimitParam(%q) ok=%v wantErr=%v msg=%q",
				tc.raw, ok, tc.wantErr, msg)
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("parseLimitParam(%q) = %d, want %d", tc.raw, got, tc.want)
		}
	}
}

// -----------------------------------------------------------
// parseCSVEnumList matrix
// -----------------------------------------------------------

func TestParseCSVEnumList(t *testing.T) {
	t.Parallel()
	allowed := []string{"success", "failure", "human_corrected"}
	cases := []struct {
		raw     string
		want    []string
		wantErr bool
	}{
		{"", nil, false},
		{"success", []string{"success"}, false},
		{"success,failure", []string{"success", "failure"}, false},
		{"  success , human_corrected  ", []string{"success", "human_corrected"}, false},
		{"success,,failure", []string{"success", "failure"}, false},
		{"bogus", nil, true},
		{"success,bogus", nil, true},
	}
	for _, tc := range cases {
		got, ok, msg := parseCSVEnumList(tc.raw, allowed, "outcome_in")
		if ok != !tc.wantErr {
			t.Errorf("parseCSVEnumList(%q) ok=%v wantErr=%v msg=%q",
				tc.raw, ok, tc.wantErr, msg)
		}
		if !tc.wantErr {
			if len(got) != len(tc.want) {
				t.Errorf("parseCSVEnumList(%q) got %v, want %v", tc.raw, got, tc.want)
				continue
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("parseCSVEnumList(%q)[%d] = %q, want %q",
						tc.raw, i, got[i], tc.want[i])
				}
			}
		}
	}
}

// -----------------------------------------------------------
// /v1/episodes — `since` is required (Stage 7.5 scenario 1)
// -----------------------------------------------------------

// The Stage 7.5 brief and risk §9.2 pin this: a missing
// `since` MUST short-circuit before the DB so the partition
// pruner is never bypassed by a full-table scan. The
// sqlmock cleanup hook asserts no DB calls fired (any
// unexpected SQL fails ExpectationsWereMet).
func TestReadEpisodes_missingSince_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadEpisodes))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body: %v -- %s", err, w.Body.String())
	}
	if env.Code != "since_required" {
		t.Errorf("code = %q, want since_required", env.Code)
	}
}

// `since` with a malformed shorthand also returns 400. The
// regex rejects negative / fractional / zero values; this is
// the path the operator hits when they hand-craft a URL.
func TestReadEpisodes_invalidSince_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadEpisodes+"?since=tomorrow"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// `repo_id` query parameter MUST be a UUID. Pre-validation
// keeps a malformed query out of the planner.
func TestReadEpisodes_invalidRepoID_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadEpisodes+"?since=1d&repo_id=not-a-uuid"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// `outcome_in` is a closed set; the parser rejects anything
// not in episodeOutcomes BEFORE the DB call.
func TestReadEpisodes_invalidOutcome_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t,
		RouteReadEpisodes+"?since=1d&outcome_in=success,bogus"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// /v1/observations — episode_id is required and UUID-shaped
// -----------------------------------------------------------

func TestReadObservations_missingEpisodeID_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadObservations))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

func TestReadObservations_invalidEpisodeID_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadObservations+"?episode_id=not-a-uuid"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// /v1/context/{id} — UUID shape + path parsing
// -----------------------------------------------------------

func TestReadContext_invalidContextID_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadContextPrefix+"not-a-uuid"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// /v1/commits — repo_id required and UUID-shaped
// -----------------------------------------------------------

func TestReadCommits_missingRepoID_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadCommits))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

func TestReadCommits_invalidRepoID_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadCommits+"?repo_id=not-a-uuid"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// /v1/concept_supports — concept_id required and UUID-shaped
// -----------------------------------------------------------

func TestReadConceptSupports_missingConceptID_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadConceptSupports))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// /v1/concepts — `promoted` is a closed set
// -----------------------------------------------------------

func TestReadConcepts_invalidPromoted_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadConcepts+"?promoted=maybe"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// /v1/graph_node/{id} — UUID shape + SHA shape pre-validation
// -----------------------------------------------------------

func TestReadGraphNode_invalidNodeID_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadGraphNodePrefix+"not-a-uuid"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

func TestReadGraphNode_invalidSHA_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t,
		RouteReadGraphNodePrefix+testRepoID+"?sha=NOT-HEX"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// /v1/trace_observation/{edge_id} — UUID shape + `before`
// shape (the `before` parser rejects non-RFC3339 values)
// -----------------------------------------------------------

func TestReadTraceObservation_invalidEdgeID_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadTraceObsPrefix+"not-a-uuid"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

func TestReadTraceObservation_invalidBefore_returns400_noDBCall(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t,
		RouteReadTraceObsPrefix+testRepoID+"?before=tomorrow"))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// /v1/repos happy path with sqlmock — verifies aggregate
// `degraded=true` when any repo's `repo_health.degraded`
// is true, and the per-row passthrough.
// -----------------------------------------------------------

// reposQueryRegex matches the CTE produced by handleReadRepos.
// We anchor on the unique join chain ("FROM repo r LEFT JOIN
// latest_job j" + "LEFT JOIN repo_health h") so a future tweak
// to the column list doesn't silently break the test.
const reposQueryRegex = `FROM repo r\s+LEFT JOIN latest_job j\s+ON.*LEFT JOIN repo_health h`

func TestReadRepos_aggregateDegraded_wireShape(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	now := time.Now()
	mock.ExpectQuery(reposQueryRegex).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "url", "default_branch", "current_head_sha",
			"language_hints", "created_at",
			"ingest_status", "degraded", "degraded_reason",
		}).
			AddRow(testRepoID, testRepoURL, "main", testHeadSHA,
				stringSliceToArrayLiteral([]string{"go"}), now,
				"indexed", true, "span_ingestor_backpressure").
			AddRow("99999999-8888-7777-6666-555555555555",
				"https://git.example/other", "main", testHeadSHA,
				stringSliceToArrayLiteral(nil), now,
				"indexed", false, ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadRepos))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%q", w.Code, w.Body.String())
	}
	var got struct {
		Repos          []RepoRow `json:"repos"`
		Degraded       bool      `json:"degraded"`
		DegradedReason string    `json:"degraded_reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v -- %s", err, w.Body.String())
	}
	if !got.Degraded {
		t.Errorf("top-level degraded = false; want true (one repo is degraded). body=%s", w.Body.String())
	}
	if got.DegradedReason != "span_ingestor_backpressure" {
		t.Errorf("degraded_reason = %q, want span_ingestor_backpressure", got.DegradedReason)
	}
	if len(got.Repos) != 2 {
		t.Fatalf("repos len = %d, want 2", len(got.Repos))
	}
	if !got.Repos[0].Degraded || got.Repos[0].DegradedReason != "span_ingestor_backpressure" {
		t.Errorf("repo[0] degraded fields wrong: %+v", got.Repos[0])
	}
	if got.Repos[1].Degraded {
		t.Errorf("repo[1] should be healthy: %+v", got.Repos[1])
	}
}

// When ALL repos are healthy the top-level `degraded` is
// false AND `degraded_reason` is omitted from the JSON
// (omitempty). Pairs with TestDegradedEnvelope_marshalsFlat
// (which covers the marshaller in isolation) — this one
// covers the per-endpoint behaviour.
func TestReadRepos_allHealthy_omitsDegradedReason(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	now := time.Now()
	mock.ExpectQuery(reposQueryRegex).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "url", "default_branch", "current_head_sha",
			"language_hints", "created_at",
			"ingest_status", "degraded", "degraded_reason",
		}).
			AddRow(testRepoID, testRepoURL, "main", testHeadSHA,
				stringSliceToArrayLiteral([]string{"go"}), now,
				"indexed", false, ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadRepos))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%q", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"degraded":false`) {
		t.Errorf("expected `degraded:false` in body, got %s", body)
	}
	if strings.Contains(body, "degraded_reason") {
		t.Errorf("expected `degraded_reason` to be omitted, got %s", body)
	}
}

// -----------------------------------------------------------
// /v1/repos — filter parameter is applied
// -----------------------------------------------------------

func TestReadRepos_filterParamPassedToSQL(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	now := time.Now()
	mock.ExpectQuery(reposQueryRegex+`.*WHERE r\.url ILIKE`).
		WithArgs("%acme%").
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "url", "default_branch", "current_head_sha",
			"language_hints", "created_at",
			"ingest_status", "degraded", "degraded_reason",
		}).
			AddRow(testRepoID, "https://git.example/acme/svc", "main", testHeadSHA,
				stringSliceToArrayLiteral([]string{}), now,
				"indexed", false, ""))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, RouteReadRepos+"?filter=acme"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// route dispatch: GET unknown route → 404, GET wrong method
// dispatch via routeWrite returns 404 (not 405) because Stage
// 7.5 surfaces GET as a valid method. The 405 path is the
// "neither GET nor POST" case, already covered by
// TestRoute_wrongMethod_returns405.
// -----------------------------------------------------------

func TestReadRoute_unknownPath_returns404(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedGet(t, "/v1/unknown-verb"))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode body: %v -- %s", err, w.Body.String())
	}
	if env.Code != "not_found" {
		t.Errorf("code = %q, want not_found", env.Code)
	}
}

// Stage 7.5 + Stage 7.1 coexistence: POST /v1/repos still
// reaches the register surface. This is the regression test
// the rubber-duck pass asked for.
func TestReadRoute_postReposStillReachesRegister(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()
	expectRegisterInserts(mock, testRepoID, testRepoURL, testBranch, testHeadSHA, testSecret)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, RouteRepos, RegisterRequest{
		RepoURL: testRepoURL, DefaultBranch: testBranch,
	}))
	// Register returns 201 Created on the fresh-repo path
	// (see handleRegister); the test asserts that the POST
	// did NOT get short-circuited into GET dispatch by the
	// Stage 7.5 routing change in route().
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (register). body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// helpers
// -----------------------------------------------------------

// stringSliceToArrayLiteral builds the PostgreSQL array
// literal sqlmock hands back for a `text[]` column. lib/pq's
// pq.StringArray scanner consumes literals of the form
// `{a,b}` / `{}`; passing a []string directly would be
// scanned as bytes and fail.
func stringSliceToArrayLiteral(parts []string) string {
	if len(parts) == 0 {
		return "{}"
	}
	// Each element is quoted to handle commas / braces in
	// hypothetical inputs; the test inputs are plain
	// identifiers but the quoting matches the lib/pq scanner
	// contract.
	quoted := make([]string, len(parts))
	for i, p := range parts {
		quoted[i] = `"` + strings.ReplaceAll(p, `"`, `\"`) + `"`
	}
	return "{" + strings.Join(quoted, ",") + "}"
}

// _ keep hex.EncodeToString referenced so a future helper
// that constructs concept fingerprints can drop it in
// without a fresh import.
var _ = hex.EncodeToString
