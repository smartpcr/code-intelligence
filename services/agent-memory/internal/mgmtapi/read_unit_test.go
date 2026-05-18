package mgmtapi

// Stage 7.5 sqlmock-driven unit tests for the operator read
// endpoints.
//
// The matrix targets the behaviour the implementation-plan
// §7.5 brief and the architecture §6.2.3 / §6.3 contract pin:
//
//   * mgmt.read.repos       -- list with LATERAL join to latest ingest_jobs;
//                              repo_id filter passes through; limit clamps.
//   * mgmt.read.commits     -- repo_id REQUIRED; ?since= flag honoured.
//   * mgmt.read.episodes    -- ?since= REQUIRED (risk §9.2 partition
//                              pruning); current_status falls back to
//                              the parent outcome when no
//                              EpisodeUpdate exists.
//   * mgmt.read.observations-- loads parent Episode.created_at
//                              first (partition prune), then queries
//                              observation.
//   * mgmt.read.context     -- LIMIT 2 corruption guard surfaces 500
//                              on duplicate rows; retired Node id
//                              surfaces `retired_at_sha`.
//   * mgmt.read.concepts    -- LATERAL latest-version join; promoted=
//                              filter honoured.
//   * mgmt.read.concept_supports -- concept_id REQUIRED.
//   * mgmt.read.graph_node  -- current view anti-joins
//                              edge_retirement AND
//                              node_retirement (architecture
//                              §1.3 G5). SHA-pinned view
//                              walks repo_commit.parent_sha
//                              from ?sha=, returns 400
//                              invalid_sha / 404 unknown_sha /
//                              404 node_not_at_sha / 200 with
//                              tombstone badge at descendant /
//                              200 no badge at retirement
//                              boundary (e2e §200-202).
//   * mgmt.read.trace_observation -- next_offset emitted when fetch+1 rows;
//                              404 when no aggregate row.
//   * cross-cutting         -- 405 with Allow header carries both verbs
//                              on /v1/repos; 401 still wins over routing.
//                              Every successful body carries
//                              degraded=false top-level.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// ---------------------------------------------------------------
// Test wiring helpers (read-side variants)
// ---------------------------------------------------------------

// newReadTestHandler constructs a Handler wired to sqlmock and
// a frozen clock. The frozen clock pins
// `parseSinceParam(... , Nh)` to a deterministic value across
// the test suite, which the matcher-friendly `ExpectQuery` can
// then assert on.
func newReadTestHandler(t *testing.T, frozen time.Time) (*Handler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
			SecretGen: fixedSecretGen(),
			Clock:     func() time.Time { return frozen },
		},
	)
	return h, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// readReq builds a GET request to `path` with the test bearer
// token. Pass `sendAuth=false` to omit the Authorization
// header.
func readReq(t *testing.T, sendAuth bool, path string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if sendAuth {
		r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	}
	return r
}

// decodeReadBody decodes a successful read response body and
// returns both the typed payload and the top-level `degraded`
// fields. The custom MarshalJSON on DegradedEnvelope flattens
// the fields into the payload object, so the decode is into
// a generic map first.
func decodeReadBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v body=%q", err, rec.Body.String())
	}
	if v, ok := out["degraded"]; !ok || v != false {
		t.Fatalf("expected top-level degraded=false; got %#v body=%q", v, rec.Body.String())
	}
	return out
}

// decodeErrorBody decodes a non-2xx response into ErrorEnvelope.
func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) ErrorEnvelope {
	t.Helper()
	var env ErrorEnvelope
	if err := json.NewDecoder(rec.Body).Decode(&env); err != nil {
		t.Fatalf("decode error: %v body=%q", err, rec.Body.String())
	}
	return env
}

// ---------------------------------------------------------------
// Cross-cutting: method dispatch + auth
// ---------------------------------------------------------------

// TestRead_authMissing_returns401_noDBAccess asserts the
// auth middleware fires before route() so a GET to a read
// path with no bearer token returns 401 without touching the
// DB. The sqlmock cleanup catches an accidental DB call via
// ExpectationsWereMet -- there are no expectations queued so
// any query fails the test.
func TestRead_authMissing_returns401_noDBAccess(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, false, "/v1/repos"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401; got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRead_unsupportedMethod_returns405_dualVerbAllowHeader
// asserts that PUT to a Stage 7.5 dual-verb route (/v1/repos)
// surfaces Allow: GET, POST. PUT to a single-verb route
// (/v1/commits) surfaces Allow: GET. PUT to a write-only
// sub-resource (/v1/repos/{id}/ingest) surfaces Allow: POST.
func TestRead_unsupportedMethod_returns405_dualVerbAllowHeader(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	cases := []struct {
		path  string
		allow string
	}{
		{"/v1/repos", "GET, POST"},
		{"/v1/episodes", "GET, POST"},
		{"/v1/commits", http.MethodGet},
		{"/v1/observations", http.MethodGet},
		{"/v1/repos/" + testRepoID + "/ingest", http.MethodPost},
		{"/v1/episodes/" + testRepoID + "/feedback", http.MethodPost},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, c.path, nil)
			req.Header.Set(AuthorizationHeader, "Bearer "+testToken)
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("expected 405; got %d: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("Allow"); got != c.allow {
				t.Fatalf("Allow header: got %q want %q", got, c.allow)
			}
		})
	}
}

// ---------------------------------------------------------------
// mgmt.read.repos
// ---------------------------------------------------------------

func TestReadRepos_happyPath_returnsListWithLatestJob(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	h, mock, cleanup := newReadTestHandler(t, frozen)
	defer cleanup()

	mock.ExpectQuery(`SELECT r\.repo_id::text.*FROM repo r.*LEFT JOIN LATERAL.*FROM ingest_jobs`).
		WithArgs("", readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "url", "default_branch", "current_head_sha", "created_at",
			"latest_job_id", "latest_status", "latest_mode", "latest_updated_at",
		}).AddRow(
			testRepoID, testRepoURL, testBranch, testHeadSHA, frozen.Add(-time.Hour),
			"22222222-2222-2222-2222-222222222222", "done", "full", frozen.Add(-30*time.Minute),
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/repos"))
	body := decodeReadBody(t, rec)
	repos, ok := body["repos"].([]any)
	if !ok || len(repos) != 1 {
		t.Fatalf("expected 1 repo; got %#v", body["repos"])
	}
	r := repos[0].(map[string]any)
	if r["repo_id"] != testRepoID {
		t.Errorf("repo_id mismatch: %v", r["repo_id"])
	}
	if r["latest_ingest_status"] != "done" {
		t.Errorf("latest_ingest_status mismatch: %v", r["latest_ingest_status"])
	}
}

func TestReadRepos_invalidLimit_returns400(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	for _, raw := range []string{"0", "-5", "abc"} {
		t.Run("limit="+raw, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, readReq(t, true, "/v1/repos?limit="+raw))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400; got %d: %s", rec.Code, rec.Body.String())
			}
			env := decodeErrorBody(t, rec)
			if env.Code != "invalid_request" {
				t.Fatalf("unexpected code %q", env.Code)
			}
		})
	}
}

func TestReadRepos_invalidRepoID_returns400(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/repos?repo_id=not-a-uuid"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReadRepos_dbFailure_returns500(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	mock.ExpectQuery(`FROM repo r`).
		WillReturnError(errors.New("connection refused"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/repos"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500; got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------
// mgmt.read.commits
// ---------------------------------------------------------------

func TestReadCommits_requiresRepoID(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/commits"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", rec.Code)
	}
	env := decodeErrorBody(t, rec)
	if !strings.Contains(env.Message, "repo_id") {
		t.Errorf("expected message to mention repo_id; got %q", env.Message)
	}
}

func TestReadCommits_happyPath(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	h, mock, cleanup := newReadTestHandler(t, frozen)
	defer cleanup()

	mock.ExpectQuery(`SELECT sha.*FROM repo_commit`).
		WithArgs(testRepoID, sqlmock.AnyArg(), false, readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{"sha", "parent_sha", "committed_at", "index_status"}).
			AddRow(testHeadSHA, "", frozen.Add(-time.Hour), "indexed").
			AddRow(testFromSHA, testHeadSHA, frozen.Add(-2*time.Hour), "indexed"))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/commits?repo_id="+testRepoID))
	body := decodeReadBody(t, rec)
	commits, ok := body["commits"].([]any)
	if !ok || len(commits) != 2 {
		t.Fatalf("expected 2 commits; got %#v", body["commits"])
	}
}

// ---------------------------------------------------------------
// mgmt.read.episodes
// ---------------------------------------------------------------

func TestReadEpisodes_sinceMissing_returns400(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/episodes"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", rec.Code)
	}
	env := decodeErrorBody(t, rec)
	if env.Code != "since_required" {
		t.Fatalf("expected code since_required; got %q", env.Code)
	}
}

func TestReadEpisodes_invalidSince_returns400(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	for _, raw := range []string{"3600", "0d", "-5h", "notatime", "1y"} {
		t.Run("since="+raw, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, readReq(t, true, "/v1/episodes?since="+raw))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("expected 400; got %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestReadEpisodes_currentStatusFallsBackToOutcome_whenNoUpdate(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	h, mock, cleanup := newReadTestHandler(t, frozen)
	defer cleanup()

	episodeID := "33333333-3333-3333-3333-333333333333"
	groupID := "44444444-4444-4444-4444-444444444444"
	contextID := "55555555-5555-5555-5555-555555555555"

	// LATERAL produces no eu row: COALESCE returns the
	// parent outcome and the row's status timestamp is NULL.
	mock.ExpectQuery(`FROM episode e.*LEFT JOIN LATERAL.*FROM episode_update`).
		WillReturnRows(sqlmock.NewRows([]string{
			"episode_id", "episode_group_id", "repo_id",
			"session_id", "trace_id", "kind", "outcome",
			"parent_episode_id", "context_id",
			"degraded", "degraded_reason",
			"created_at", "current_status", "status_at",
		}).AddRow(
			episodeID, groupID, testRepoID,
			"sess-1", "trace-1", "agent", "success",
			"", contextID,
			false, "",
			frozen.Add(-time.Hour), "success", nil,
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/episodes?since=24h"))
	body := decodeReadBody(t, rec)
	eps, ok := body["episodes"].([]any)
	if !ok || len(eps) != 1 {
		t.Fatalf("expected 1 episode; got %#v", body["episodes"])
	}
	e := eps[0].(map[string]any)
	if e["current_status"] != "success" || e["outcome"] != "success" {
		t.Errorf("current_status should match outcome when no update: %#v", e)
	}
}

func TestReadEpisodes_currentStatusReflectsLatestUpdate(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	h, mock, cleanup := newReadTestHandler(t, frozen)
	defer cleanup()

	episodeID := "33333333-3333-3333-3333-333333333333"
	groupID := "44444444-4444-4444-4444-444444444444"
	contextID := "55555555-5555-5555-5555-555555555555"

	mock.ExpectQuery(`FROM episode e.*LEFT JOIN LATERAL.*FROM episode_update`).
		WillReturnRows(sqlmock.NewRows([]string{
			"episode_id", "episode_group_id", "repo_id",
			"session_id", "trace_id", "kind", "outcome",
			"parent_episode_id", "context_id",
			"degraded", "degraded_reason",
			"created_at", "current_status", "status_at",
		}).AddRow(
			episodeID, groupID, testRepoID,
			"sess-1", "trace-1", "agent", "failure",
			"", contextID,
			false, "",
			frozen.Add(-time.Hour), "human_corrected", frozen.Add(-30*time.Minute),
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/episodes?since=2024-05-01T00:00:00Z"))
	body := decodeReadBody(t, rec)
	eps := body["episodes"].([]any)
	e := eps[0].(map[string]any)
	if e["current_status"] != "human_corrected" {
		t.Errorf("expected current_status=human_corrected; got %v", e["current_status"])
	}
	if e["outcome"] != "failure" {
		t.Errorf("parent outcome should be preserved as failure; got %v", e["outcome"])
	}
}

func TestReadEpisodes_invalidOutcomeIn_returns400(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/episodes?since=24h&outcome_in=bogus"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", rec.Code)
	}
}

// ---------------------------------------------------------------
// mgmt.read.observations
// ---------------------------------------------------------------

func TestReadObservations_requiresEpisodeID(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/observations"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", rec.Code)
	}
}

func TestReadObservations_unknownEpisode_returns404(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	mock.ExpectQuery(`SELECT created_at FROM episode WHERE episode_id`).
		WithArgs(testRepoID).
		WillReturnError(noRowsErr())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/observations?episode_id="+testRepoID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeErrorBody(t, rec)
	if env.Code != "episode_not_found" {
		t.Fatalf("unexpected code %q", env.Code)
	}
}

func TestReadObservations_happyPath_partitionPrunePredicateUsed(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	h, mock, cleanup := newReadTestHandler(t, frozen)
	defer cleanup()

	parentCreatedAt := frozen.Add(-time.Hour)
	episodeID := testRepoID

	mock.ExpectQuery(`SELECT created_at FROM episode WHERE episode_id`).
		WithArgs(episodeID).
		WillReturnRows(sqlmock.NewRows([]string{"created_at"}).AddRow(parentCreatedAt))

	mock.ExpectQuery(`FROM observation\s+WHERE episode_id = .* AND created_at >=`).
		WithArgs(episodeID, parentCreatedAt).
		WillReturnRows(sqlmock.NewRows([]string{
			"observation_id", "role", "node_id", "edge_id", "concept_id", "degraded_recall_context_id", "weight", "created_at",
		}).AddRow(
			"66666666-6666-6666-6666-666666666666", "node_hit",
			"77777777-7777-7777-7777-777777777777", "", "", "",
			1.5, parentCreatedAt.Add(time.Minute),
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/observations?episode_id="+episodeID))
	body := decodeReadBody(t, rec)
	obs := body["observations"].([]any)
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation; got %#v", obs)
	}
	o := obs[0].(map[string]any)
	if o["role"] != "node_hit" {
		t.Errorf("role mismatch: %v", o["role"])
	}
}

// ---------------------------------------------------------------
// mgmt.read.context
// ---------------------------------------------------------------

func TestReadContext_invalidPathID_returns400(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/context/not-a-uuid"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", rec.Code)
	}
}

func TestReadContext_emptyTail_returns404(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/context/"))
	// No trailing id at all -> 404 (the trailing-slash form
	// has no id segment).
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d", rec.Code)
	}
}

func TestReadContext_duplicateRows_returns500_corruptionGuard(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	ctxID := "88888888-8888-8888-8888-888888888888"

	mock.ExpectQuery(`FROM recall_context_log\s+WHERE context_id`).
		WithArgs(ctxID).
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "repo_id", "verb",
			"query_json", "reranker_model_version",
			"served_under_degraded", "created_at",
			"node_ids", "edge_ids", "concept_ids",
		}).AddRow(
			ctxID, testRepoID, "recall",
			`{}`, "rerank-v1",
			false, time.Now(),
			"{}", "{}", "{}",
		).AddRow(
			ctxID, testRepoID, "recall",
			`{}`, "rerank-v1",
			false, time.Now(),
			"{}", "{}", "{}",
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/context/"+ctxID))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500; got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------
// mgmt.read.concepts
// ---------------------------------------------------------------

func TestReadConcepts_happyPath_includesLatestVersion(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	h, mock, cleanup := newReadTestHandler(t, frozen)
	defer cleanup()

	conceptID := "99999999-9999-9999-9999-999999999999"

	mock.ExpectQuery(`FROM concept c\s+LEFT JOIN LATERAL.*FROM concept_version`).
		WithArgs(false, false, readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "created_at",
			"version_index", "confidence", "confidence_band",
			"support_count", "negative_count", "promoted", "version_created_at",
		}).AddRow(
			conceptID, "circuit_breaker", "use a circuit breaker", frozen.Add(-2*time.Hour),
			3, 0.82, "high", 12, 1, true, frozen.Add(-time.Hour),
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/concepts"))
	body := decodeReadBody(t, rec)
	cs := body["concepts"].([]any)
	if len(cs) != 1 {
		t.Fatalf("expected 1 concept; got %#v", cs)
	}
	c := cs[0].(map[string]any)
	if c["latest_promoted"] != true {
		t.Errorf("expected latest_promoted=true; got %v", c["latest_promoted"])
	}
}

func TestReadConcepts_promotedFilterPassesThrough(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	mock.ExpectQuery(`FROM concept c`).
		WithArgs(true, true, readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"concept_id", "name", "description_md", "created_at",
			"version_index", "confidence", "confidence_band",
			"support_count", "negative_count", "promoted", "version_created_at",
		}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/concepts?promoted=true"))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200; got %d: %s", rec.Code, rec.Body.String())
	}
}

// ---------------------------------------------------------------
// mgmt.read.concept_supports
// ---------------------------------------------------------------

func TestReadConceptSupports_requiresConceptID(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/concept_supports"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", rec.Code)
	}
}

func TestReadConceptSupports_happyPath(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	conceptID := "99999999-9999-9999-9999-999999999999"
	supportID := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	versionID := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"

	mock.ExpectQuery(`FROM concept_support`).
		WithArgs(conceptID, "", readDefaultLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"support_id", "concept_id", "concept_version_id",
			"repo_id", "node_id", "episode_id",
			"polarity", "created_at",
		}).AddRow(
			supportID, conceptID, versionID,
			testRepoID, "", "",
			"positive", time.Now(),
		))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/concept_supports?concept_id="+conceptID))
	body := decodeReadBody(t, rec)
	if body["concept_id"] != conceptID {
		t.Errorf("concept_id mismatch: %v", body["concept_id"])
	}
}

// ---------------------------------------------------------------
// mgmt.read.graph_node
// ---------------------------------------------------------------

func TestReadGraphNode_invalidShaShape_returns400(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/"+testRepoID+"?sha=not-a-sha"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", rec.Code)
	}
	env := decodeErrorBody(t, rec)
	if env.Code != "invalid_sha" {
		t.Fatalf("expected invalid_sha; got %q", env.Code)
	}
}

func TestReadGraphNode_unknownSha_returns404(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	nodeID := testRepoID
	// Step 1: load node + retirement (no retirement here).
	mock.ExpectQuery(`FROM node n\s+LEFT JOIN node_retirement`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "kind", "canonical_signature", "from_sha",
			"parent_node_id", "attrs_json", "retired_at_sha",
		}).AddRow(
			testRepoID, "method", "Foo::bar()", testFromSHA,
			"", `{}`, "",
		))
	// Step 2: ancestor CTE -- target_known=false (no ancestor row).
	mock.ExpectQuery(`WITH RECURSIVE ancestors`).
		WithArgs(testRepoID, testHeadSHA, testFromSHA, "").
		WillReturnRows(sqlmock.NewRows([]string{
			"target_known", "from_in_ancestors", "retire_in_ancestors",
		}).AddRow(false, false, false))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/"+nodeID+"?sha="+testHeadSHA))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeErrorBody(t, rec)
	if env.Code != "unknown_sha" {
		t.Fatalf("expected unknown_sha; got %q", env.Code)
	}
}

func TestReadGraphNode_nodeNotAtSha_returns404(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	nodeID := testRepoID
	mock.ExpectQuery(`FROM node n\s+LEFT JOIN node_retirement`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "kind", "canonical_signature", "from_sha",
			"parent_node_id", "attrs_json", "retired_at_sha",
		}).AddRow(
			testRepoID, "method", "Foo::bar()", testFromSHA,
			"", `{}`, "",
		))
	// Target known but node.from_sha not in ancestors.
	mock.ExpectQuery(`WITH RECURSIVE ancestors`).
		WithArgs(testRepoID, testHeadSHA, testFromSHA, "").
		WillReturnRows(sqlmock.NewRows([]string{
			"target_known", "from_in_ancestors", "retire_in_ancestors",
		}).AddRow(true, false, false))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/"+nodeID+"?sha="+testHeadSHA))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeErrorBody(t, rec)
	if env.Code != "node_not_at_sha" {
		t.Fatalf("expected node_not_at_sha; got %q", env.Code)
	}
}

// TestReadGraphNode_shaPinned_aliveAtRetirementBoundary asserts
// the e2e-scenarios.md §201 invariant: at sha == retired_at_sha,
// the node is still alive and the card carries NO badge.
func TestReadGraphNode_shaPinned_aliveAtRetirementBoundary(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	nodeID := testRepoID
	// Node has a retirement; retired_at_sha == testFromSHA.
	mock.ExpectQuery(`FROM node n\s+LEFT JOIN node_retirement`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "kind", "canonical_signature", "from_sha",
			"parent_node_id", "attrs_json", "retired_at_sha",
		}).AddRow(
			testRepoID, "method", "Foo::bar()", testFromSHA,
			"", `{}`, testFromSHA,
		))
	// Request sha = retired_at_sha (= from_sha here). Both from and
	// retire are in ancestors; the handler must NOT set the badge
	// because retired_at_sha == requested sha.
	mock.ExpectQuery(`WITH RECURSIVE ancestors`).
		WithArgs(testRepoID, testFromSHA, testFromSHA, testFromSHA).
		WillReturnRows(sqlmock.NewRows([]string{
			"target_known", "from_in_ancestors", "retire_in_ancestors",
		}).AddRow(true, true, true))
	// SHA-pinned neighbor query -- empty results for both directions.
	mock.ExpectQuery(`WITH RECURSIVE ancestors.*FROM edge e.*neighbor\.node_id = e\.dst_node_id`).
		WithArgs(nodeID, testRepoID, testFromSHA, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))
	mock.ExpectQuery(`WITH RECURSIVE ancestors.*FROM edge e.*neighbor\.node_id = e\.src_node_id`).
		WithArgs(nodeID, testRepoID, testFromSHA, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/"+nodeID+"?sha="+testFromSHA))
	body := decodeReadBody(t, rec)
	if _, ok := body["retired_at_sha"]; ok {
		t.Errorf("expected NO retired_at_sha badge at retirement boundary; got %v",
			body["retired_at_sha"])
	}
}

// TestReadGraphNode_shaPinned_tombstonedAtDescendant asserts
// e2e-scenarios.md §200: at sha that is a descendant of
// retired_at_sha, the card carries the tombstone badge.
func TestReadGraphNode_shaPinned_tombstonedAtDescendant(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	nodeID := testRepoID
	mock.ExpectQuery(`FROM node n\s+LEFT JOIN node_retirement`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "kind", "canonical_signature", "from_sha",
			"parent_node_id", "attrs_json", "retired_at_sha",
		}).AddRow(
			testRepoID, "method", "Foo::bar()", testFromSHA,
			"", `{}`, testFromSHA,
		))
	// Request sha = testHeadSHA (descendant of testFromSHA). All
	// three booleans are true; retired_at_sha != requested sha
	// so the handler MUST set the badge.
	mock.ExpectQuery(`WITH RECURSIVE ancestors`).
		WithArgs(testRepoID, testHeadSHA, testFromSHA, testFromSHA).
		WillReturnRows(sqlmock.NewRows([]string{
			"target_known", "from_in_ancestors", "retire_in_ancestors",
		}).AddRow(true, true, true))
	mock.ExpectQuery(`WITH RECURSIVE ancestors.*FROM edge e.*neighbor\.node_id = e\.dst_node_id`).
		WithArgs(nodeID, testRepoID, testHeadSHA, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))
	mock.ExpectQuery(`WITH RECURSIVE ancestors.*FROM edge e.*neighbor\.node_id = e\.src_node_id`).
		WithArgs(nodeID, testRepoID, testHeadSHA, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/"+nodeID+"?sha="+testHeadSHA))
	body := decodeReadBody(t, rec)
	if body["retired_at_sha"] != testFromSHA {
		t.Errorf("expected retired_at_sha=%s badge at descendant; got %v",
			testFromSHA, body["retired_at_sha"])
	}
}

func TestReadGraphNode_invalidPathID_returns400(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/not-a-uuid"))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400; got %d", rec.Code)
	}
}

func TestReadGraphNode_notFound_returns404(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	mock.ExpectQuery(`FROM node n\s+LEFT JOIN node_retirement`).
		WithArgs(testRepoID).
		WillReturnError(noRowsErr())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/"+testRepoID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestReadGraphNode_currentView_neighborsAntiJoinRetired asserts
// the evaluator item 3 fix: the default (no ?sha=) neighbor
// query MUST anti-join `edge_retirement` and `node_retirement`
// so retired edges and edges pointing at retired neighbors do
// not appear in the head-state view (architecture §1.3 G5).
func TestReadGraphNode_currentView_neighborsAntiJoinRetired(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	nodeID := testRepoID
	mock.ExpectQuery(`FROM node n\s+LEFT JOIN node_retirement`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "kind", "canonical_signature", "from_sha",
			"parent_node_id", "attrs_json", "retired_at_sha",
		}).AddRow(
			testRepoID, "method", "Foo::bar()", testHeadSHA,
			"", `{}`, "",
		))
	// Outgoing: assert the anti-join WHERE clauses are present.
	mock.ExpectQuery(`FROM edge e\s+LEFT JOIN node neighbor ON neighbor\.node_id = e\.dst_node_id\s+LEFT JOIN edge_retirement er ON er\.edge_id = e\.edge_id\s+LEFT JOIN node_retirement nr ON nr\.node_id = e\.dst_node_id\s+WHERE e\.src_node_id = \$1::uuid\s+AND er\.edge_id IS NULL\s+AND nr\.node_id IS NULL`).
		WithArgs(nodeID, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))
	// Incoming: same shape, mirrored columns.
	mock.ExpectQuery(`FROM edge e\s+LEFT JOIN node neighbor ON neighbor\.node_id = e\.src_node_id\s+LEFT JOIN edge_retirement er ON er\.edge_id = e\.edge_id\s+LEFT JOIN node_retirement nr ON nr\.node_id = e\.src_node_id\s+WHERE e\.dst_node_id = \$1::uuid\s+AND er\.edge_id IS NULL\s+AND nr\.node_id IS NULL`).
		WithArgs(nodeID, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/"+nodeID))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200; got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReadGraphNode_retired_surfacesRetiredAtSHA(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()

	nodeID := testRepoID

	mock.ExpectQuery(`FROM node n\s+LEFT JOIN node_retirement`).
		WithArgs(nodeID).
		WillReturnRows(sqlmock.NewRows([]string{
			"repo_id", "kind", "canonical_signature", "from_sha",
			"parent_node_id", "attrs_json", "retired_at_sha",
		}).AddRow(
			testRepoID, "method", "Foo::bar()", testHeadSHA,
			"", `{"visibility":"public"}`, testFromSHA,
		))
	// outgoing (current view, anti-joined)
	mock.ExpectQuery(`FROM edge e\s+LEFT JOIN node neighbor ON neighbor\.node_id = e\.dst_node_id`).
		WithArgs(nodeID, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))
	// incoming
	mock.ExpectQuery(`FROM edge e\s+LEFT JOIN node neighbor ON neighbor\.node_id = e\.src_node_id`).
		WithArgs(nodeID, graphNeighborLimit).
		WillReturnRows(sqlmock.NewRows([]string{
			"edge_id", "kind", "neighbor_node_id", "neighbor_canonical_signature",
			"neighbor_kind", "edge_retired_at_sha", "missing",
		}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node/"+nodeID))
	body := decodeReadBody(t, rec)
	if body["retired_at_sha"] != testFromSHA {
		t.Errorf("expected retired_at_sha=%s; got %v", testFromSHA, body["retired_at_sha"])
	}
}

// TestReadGraphNode_barePath_returns404 asserts the evaluator
// item 5 fix: a GET to the bare /v1/graph_node (no id segment)
// returns a typed JSON 404 from the handler -- not a ServeMux
// 301 redirect. The cleanup helper's
// sqlmock.ExpectationsWereMet() catches any accidental DB call.
func TestReadGraphNode_barePath_returns404(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/graph_node"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeErrorBody(t, rec)
	if env.Code != "node_id_required" {
		t.Fatalf("expected node_id_required; got %q", env.Code)
	}
}

func TestReadContext_barePath_returns404(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/context"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeErrorBody(t, rec)
	if env.Code != "context_id_required" {
		t.Fatalf("expected context_id_required; got %q", env.Code)
	}
}

func TestReadTraceObservation_barePath_returns404(t *testing.T) {
	h, _, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/trace_observation"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d: %s", rec.Code, rec.Body.String())
	}
	env := decodeErrorBody(t, rec)
	if env.Code != "edge_id_required" {
		t.Fatalf("expected edge_id_required; got %q", env.Code)
	}
}

// ---------------------------------------------------------------
// mgmt.read.trace_observation
// ---------------------------------------------------------------

func TestReadTraceObservation_notFound_returns404(t *testing.T) {
	h, mock, cleanup := newReadTestHandler(t, time.Now())
	defer cleanup()
	mock.ExpectQuery(`FROM trace_observation\s+WHERE edge_id`).
		WithArgs(testRepoID).
		WillReturnError(noRowsErr())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, "/v1/trace_observation/"+testRepoID))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404; got %d", rec.Code)
	}
}

func TestReadTraceObservation_emitsNextOffsetWhenMore(t *testing.T) {
	frozen := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	h, mock, cleanup := newReadTestHandler(t, frozen)
	defer cleanup()

	edgeID := testRepoID

	mock.ExpectQuery(`FROM trace_observation\s+WHERE edge_id`).
		WithArgs(edgeID).
		WillReturnRows(sqlmock.NewRows([]string{
			"observation_count", "p50_latency_ms", "p95_latency_ms", "latest_span_ref", "last_observed_at",
		}).AddRow(int64(1234), 12.5, 99.0, "trace-A/span-B", frozen.Add(-time.Minute)))

	// Limit = 2 means we ask for 3 rows; return 3 so
	// next_offset is emitted.
	tailRows := sqlmock.NewRows([]string{
		"span_log_id", "trace_id", "span_id", "started_at", "duration_ms",
	}).
		AddRow("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1", "trace-A", "span-1", frozen.Add(-time.Minute), 10.0).
		AddRow("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa2", "trace-B", "span-2", frozen.Add(-2*time.Minute), 11.0).
		AddRow("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa3", "trace-C", "span-3", frozen.Add(-3*time.Minute), 12.0)
	mock.ExpectQuery(`FROM trace_observation_log\s+WHERE edge_id`).
		WithArgs(edgeID, sqlmock.AnyArg(), false, 3, 0).
		WillReturnRows(tailRows)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, readReq(t, true, fmt.Sprintf("/v1/trace_observation/%s?limit=2", edgeID)))
	body := decodeReadBody(t, rec)
	tail := body["tail"].([]any)
	if len(tail) != 2 {
		t.Fatalf("expected 2 tail rows; got %d", len(tail))
	}
	if body["next_offset"] != float64(2) {
		t.Errorf("expected next_offset=2; got %v", body["next_offset"])
	}
}

// ---------------------------------------------------------------
// helpers
// ---------------------------------------------------------------

// noRowsErr returns the canonical no-rows sentinel. sqlmock
// surfaces a WillReturnError straight through QueryRowContext
// + Scan, so the handler's errors.Is(err, sql.ErrNoRows)
// check fires when we return this value.
func noRowsErr() error { return sql.ErrNoRows }

// _ = context.Background -- silence import of context (kept
// for symmetry with helpers in feedback_unit_test.go).
var _ = context.Background
