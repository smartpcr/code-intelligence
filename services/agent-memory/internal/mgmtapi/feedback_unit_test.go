package mgmtapi

// Behavioural unit tests for the Stage 7.3 `mgmt.feedback`
// verb, driven via httptest.ResponseRecorder + go-sqlmock so
// the full auth -> validate -> DB -> respond pipeline runs
// without a live PostgreSQL.
//
// Mirrors the existing handler_unit_test.go conventions:
// silent slog logger, fakeResolver indirection, sqlmock with
// QueryMatcherRegexp, deterministic test constants.
//
// The matrix maps 1:1 onto the implementation-plan.md Stage
// 7.3 test scenarios plus the e2e-scenarios.md §11 wire-level
// validation matrix:
//
//   * corrected_action required on human_corrected
//       TestFeedback_humanCorrected_missingCorrectedAction_returns400
//   * corrected_action forbidden on other outcomes
//       TestFeedback_nonCorrected_withCorrectedAction_returns400 (all four)
//   * feedback yields EpisodeUpdate
//       TestFeedback_humanCorrected_writesFeedbackEpisodeAndUpdate
//       TestFeedback_success_acknowledgement_writesFeedbackEpisodeAndUpdate
//
// Plus the typed-error matrix the brief implies:
//   * missing outcome -> 400
//   * invalid outcome -> 400
//   * invalid parent_id UUID -> 400
//   * parent Episode not found -> 404
//   * parent kind != 'agent' -> 400
//   * corrected_action explicit null treated as omitted (both branches)
//   * corrected_action scalar/array rejected
//   * malformed JSON -> 400
//   * DB outage -> 500
//   * 401 on missing auth (auth middleware coverage on feedback path)
//   * 405 / 404 envelopes consistent with the rest of the surface

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	testParentEpisodeID   = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testFeedbackEpisodeID = "ffffffff-eeee-dddd-cccc-bbbbbbbbbbbb"
	testParentRepoID      = "11111111-2222-3333-4444-555555555555"
)

// feedbackPath returns the conventional path for a given
// parent id, mirroring how the existing tests build
// /v1/repos/{id}/ingest URLs.
func feedbackPath(parentID string) string {
	return RouteEpisodes + "/" + parentID + feedbackSuffix
}

// expectFeedbackLoadParent queues the parent-Episode lookup
// the executeFeedback transaction issues first. The returned
// (repo_id, kind) row drives subsequent execution; pass kind=""
// to simulate sql.ErrNoRows.
func expectFeedbackLoadParent(mock sqlmock.Sqlmock, parentID, repoID, kind string) {
	q := mock.ExpectQuery(`SELECT repo_id::text, kind::text\s+FROM episode\s+WHERE episode_id = \$1::uuid`).
		WithArgs(parentID)
	if kind == "" {
		q.WillReturnError(errSQLNoRowsFromExpect)
		return
	}
	q.WillReturnRows(sqlmock.NewRows([]string{"repo_id", "kind"}).AddRow(repoID, kind))
}

// errSQLNoRowsFromExpect is the sentinel we feed into sqlmock
// when we want the parent-load query to return "no row". We
// cannot use sql.ErrNoRows directly with QueryRow().Scan in
// sqlmock's Rows shape, but ExpectQuery().WillReturnError
// honours sql.ErrNoRows because the handler's
// `errors.Is(err, sql.ErrNoRows)` branch fires before the
// Scan.
var errSQLNoRowsFromExpect = sql.ErrNoRows

// expectFeedbackInsertEpisode queues the feedback-Episode
// INSERT and returns the assigned id via RETURNING. The
// caller asserts argument shape (corrected_action nullability
// in particular) by passing the expected args slice.
func expectFeedbackInsertEpisode(
	mock sqlmock.Sqlmock,
	repoID, parentID, outcome string,
	correctedAction any, // string or nil
	feedbackEpisodeID string,
) {
	// We match the args structurally: repo_id, session_id,
	// trace_id, parent_id, action_json, outcome,
	// corrected_action. We DO NOT pin session_id / trace_id
	// to specific values -- the handler mints those server-
	// side. sqlmock.AnyArg() handles the wildcard.
	mock.ExpectQuery(`INSERT INTO episode\s*\(\s*episode_group_id,\s*repo_id,\s*session_id,\s*trace_id,\s*kind,\s*parent_episode_id,\s*action,\s*outcome,\s*corrected_action\s*\)`).
		WithArgs(
			repoID,
			sqlmock.AnyArg(), // session_id
			sqlmock.AnyArg(), // trace_id
			parentID,
			feedbackEpisodeActionJSON,
			outcome,
			correctedAction,
		).
		WillReturnRows(sqlmock.NewRows([]string{"episode_id"}).AddRow(feedbackEpisodeID))
}

// expectFeedbackInsertUpdate queues the EpisodeUpdate INSERT.
// `note` is the expected driver.Value: pass nil for SQL NULL
// (empty operator note) or a Go string for a populated note.
// The handler binds the note via sql.NullString whose
// driver.Valuer implementation collapses Valid=false into nil
// and Valid=true into the raw string -- sqlmock matches on
// the post-Valuer value, not the NullString wrapper.
func expectFeedbackInsertUpdate(mock sqlmock.Sqlmock, parentID, outcome string, note any) {
	mock.ExpectExec(`INSERT INTO episode_update\s*\(episode_id, new_outcome, note, actor\)`).
		WithArgs(parentID, outcome, note).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// -----------------------------------------------------------
// Scenario: feedback yields EpisodeUpdate -- happy path
// -----------------------------------------------------------

func TestFeedback_humanCorrected_writesFeedbackEpisodeAndUpdate(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	expectFeedbackLoadParent(mock, testParentEpisodeID, testParentRepoID, "agent")
	expectFeedbackInsertEpisode(mock, testParentRepoID, testParentEpisodeID, "human_corrected",
		`{"op":"replay","with":"helper"}`,
		testFeedbackEpisodeID)
	// `note` is populated -> driver receives a non-NULL sql.NullString.
	expectFeedbackInsertUpdate(mock, testParentEpisodeID, "human_corrected",
		"re-route via retry helper")
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	body := FeedbackRequest{
		Outcome:         "human_corrected",
		CorrectedAction: json.RawMessage(`{"op":"replay","with":"helper"}`),
		Note:            "re-route via retry helper",
	}
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID), body))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp FeedbackResponse
	mustDecode(t, w.Body.Bytes(), &resp)
	if resp.FeedbackEpisodeID != testFeedbackEpisodeID {
		t.Errorf("feedback_episode_id = %q, want %q", resp.FeedbackEpisodeID, testFeedbackEpisodeID)
	}
}

func TestFeedback_success_acknowledgement_writesFeedbackEpisodeAndUpdate(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	expectFeedbackLoadParent(mock, testParentEpisodeID, testParentRepoID, "agent")
	// corrected_action omitted -> driver receives nil (SQL NULL).
	expectFeedbackInsertEpisode(mock, testParentRepoID, testParentEpisodeID, "success", nil, testFeedbackEpisodeID)
	// note populated for traceability.
	expectFeedbackInsertUpdate(mock, testParentEpisodeID, "success",
		"looks right after re-read")
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	body := FeedbackRequest{
		Outcome: "success",
		Note:    "looks right after re-read",
	}
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID), body))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp FeedbackResponse
	mustDecode(t, w.Body.Bytes(), &resp)
	if resp.FeedbackEpisodeID != testFeedbackEpisodeID {
		t.Errorf("feedback_episode_id = %q, want %q", resp.FeedbackEpisodeID, testFeedbackEpisodeID)
	}
}

func TestFeedback_emptyNote_writesNullNote(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	expectFeedbackLoadParent(mock, testParentEpisodeID, testParentRepoID, "agent")
	expectFeedbackInsertEpisode(mock, testParentRepoID, testParentEpisodeID, "failure", nil, testFeedbackEpisodeID)
	// Empty note -> nil arg -> SQL NULL.
	expectFeedbackInsertUpdate(mock, testParentEpisodeID, "failure", nil)
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{Outcome: "failure"}))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusCreated, w.Body.String())
	}
}

// -----------------------------------------------------------
// Scenario: corrected_action REQUIRED on human_corrected
// -----------------------------------------------------------

func TestFeedback_humanCorrected_missingCorrectedAction_returns400(t *testing.T) {
	t.Parallel()
	// No DB expectations: the validation gate rejects before
	// any tx begins; sqlmock.ExpectationsWereMet would catch
	// a stray INSERT.
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{Outcome: "human_corrected"}))

	assertFeedbackValidationError(t, w, http.StatusBadRequest, "corrected_action_required")
}

func TestFeedback_humanCorrected_explicitNullCorrectedAction_returns400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{
			Outcome:         "human_corrected",
			CorrectedAction: json.RawMessage(`null`),
		}))

	assertFeedbackValidationError(t, w, http.StatusBadRequest, "corrected_action_required")
}

// -----------------------------------------------------------
// Scenario: corrected_action FORBIDDEN on other outcomes
// -----------------------------------------------------------

func TestFeedback_nonCorrected_withCorrectedAction_returns400(t *testing.T) {
	t.Parallel()
	for _, out := range []string{"success", "failure", "refused", "degraded"} {
		out := out
		t.Run(out, func(t *testing.T) {
			t.Parallel()
			h, _, cleanup := newTestHandler(t)
			defer cleanup()

			w := httptest.NewRecorder()
			h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
				FeedbackRequest{
					Outcome:         out,
					CorrectedAction: json.RawMessage(`{"some":"action"}`),
				}))

			assertFeedbackValidationError(t, w, http.StatusBadRequest, "corrected_action_forbidden")
		})
	}
}

func TestFeedback_nonCorrected_explicitNullCorrectedAction_accepted(t *testing.T) {
	t.Parallel()
	// Explicit JSON null is semantically equivalent to
	// omission per §6.2.2 ("must be omitted"). The handler
	// should accept the request and write the rows.
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	expectFeedbackLoadParent(mock, testParentEpisodeID, testParentRepoID, "agent")
	expectFeedbackInsertEpisode(mock, testParentRepoID, testParentEpisodeID, "success", nil, testFeedbackEpisodeID)
	expectFeedbackInsertUpdate(mock, testParentEpisodeID, "success", nil)
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{
			Outcome:         "success",
			CorrectedAction: json.RawMessage(`null`),
		}))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusCreated, w.Body.String())
	}
}

// -----------------------------------------------------------
// outcome closed-set validation
// -----------------------------------------------------------

func TestFeedback_missingOutcome_returns400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{}))

	assertFeedbackValidationError(t, w, http.StatusBadRequest, "invalid_request")
}

func TestFeedback_unknownOutcome_returns400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{Outcome: "great"}))

	assertFeedbackValidationError(t, w, http.StatusBadRequest, "invalid_outcome")
}

// -----------------------------------------------------------
// corrected_action shape validation (object-only)
// -----------------------------------------------------------

func TestFeedback_correctedActionScalar_returns400(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{`"a-string"`, `42`, `true`, `["a","b"]`} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			t.Parallel()
			h, _, cleanup := newTestHandler(t)
			defer cleanup()
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
				FeedbackRequest{
					Outcome:         "human_corrected",
					CorrectedAction: json.RawMessage(raw),
				}))
			assertFeedbackValidationError(t, w, http.StatusBadRequest, "invalid_corrected_action")
		})
	}
}

func TestFeedback_correctedActionMalformedJSON_returns400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()
	w := httptest.NewRecorder()
	// The OUTER body uses an explicit raw JSON so the body
	// itself stays parseable but the corrected_action field
	// holds a structurally-broken object.
	body := []byte(`{"outcome":"human_corrected","corrected_action":{"foo":}}`)
	h.ServeHTTP(w, rawRequest(t, true, http.MethodPost, feedbackPath(testParentEpisodeID), body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	// The outer json.Unmarshal fails first (the whole body
	// is invalid because of the trailing `}` placement),
	// surfacing invalid_json. That's still a clean 400.
	if env.Code != "invalid_json" && env.Code != "invalid_corrected_action" {
		t.Errorf("code = %q, want invalid_json or invalid_corrected_action", env.Code)
	}
}

// -----------------------------------------------------------
// parent_id path validation / lookup
// -----------------------------------------------------------

func TestFeedback_invalidParentID_returns400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath("not-a-uuid"),
		FeedbackRequest{Outcome: "success"}))

	assertFeedbackValidationError(t, w, http.StatusBadRequest, "invalid_parent_id")
}

func TestFeedback_parentNotFound_returns404(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	expectFeedbackLoadParent(mock, testParentEpisodeID, "", "")
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{Outcome: "success"}))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "episode_not_found" {
		t.Errorf("code = %q, want episode_not_found", env.Code)
	}
}

func TestFeedback_parentKindFeedback_returns400(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	expectFeedbackLoadParent(mock, testParentEpisodeID, testParentRepoID, "feedback")
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{Outcome: "human_corrected", CorrectedAction: json.RawMessage(`{"x":1}`)}))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "invalid_parent_kind" {
		t.Errorf("code = %q, want invalid_parent_kind", env.Code)
	}
}

func TestFeedback_parentKindSyntheticPositive_returns400(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	expectFeedbackLoadParent(mock, testParentEpisodeID, testParentRepoID, "synthetic_positive")
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{Outcome: "success"}))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "invalid_parent_kind" {
		t.Errorf("code = %q, want invalid_parent_kind", env.Code)
	}
}

// -----------------------------------------------------------
// envelope / route hygiene
// -----------------------------------------------------------

func TestFeedback_malformedJSON_returns400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, rawRequest(t, true, http.MethodPost,
		feedbackPath(testParentEpisodeID), []byte(`{"outcome":}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "invalid_json" {
		t.Errorf("code = %q, want invalid_json", env.Code)
	}
}

func TestFeedback_dbOutage_returns500_noLeak(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	expectFeedbackLoadParent(mock, testParentEpisodeID, testParentRepoID, "agent")
	mock.ExpectQuery(`INSERT INTO episode\s*\(\s*episode_group_id,\s*repo_id,`).
		WillReturnError(errSimulatedOutage)
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, feedbackPath(testParentEpisodeID),
		FeedbackRequest{Outcome: "human_corrected", CorrectedAction: json.RawMessage(`{"x":1}`)}))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "internal_error" {
		t.Errorf("code = %q, want internal_error", env.Code)
	}
	// The error message MUST NOT leak the underlying driver
	// error -- it should be a generic operator-facing string.
	if strings.Contains(env.Message, errSimulatedOutage.Error()) {
		t.Errorf("error message leaked driver detail: %q", env.Message)
	}
}

func TestFeedback_missingAuth_returns401_noDBAccess(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	// No DB expectations queued; auth must short-circuit
	// BEFORE any DB call.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, false /* no auth header */, feedbackPath(testParentEpisodeID),
		FeedbackRequest{Outcome: "success"}))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body=%q", w.Code, w.Body.String())
	}
}

func TestFeedback_wrongMethod_returns405(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	r := httptest.NewRequest(http.MethodGet, feedbackPath(testParentEpisodeID), nil)
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405. body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow header = %q, want POST", got)
	}
}

func TestFeedback_unknownEpisodeSuffix_returns404(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteEpisodes+"/"+testParentEpisodeID+"/elaborate",
		FeedbackRequest{Outcome: "success"}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// extract / validator helpers
// -----------------------------------------------------------

func TestExtractEpisodeFeedbackPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, wantParentID, wantSuffix string
		wantOK                         bool
	}{
		{"/v1/episodes/" + testParentEpisodeID + "/feedback", testParentEpisodeID, feedbackSuffix, true},
		{"/v1/episodes/" + testParentEpisodeID, "", "", false},
		{"/v1/episodes//feedback", "", "", false},
		{"/v1/episodes/" + testParentEpisodeID + "/foo/feedback", "", "", false},
		{"/v1/episodes/" + testParentEpisodeID + "/feedback/extra", "", "", false},
	}
	for _, tc := range cases {
		parentID, suffix, ok := extractEpisodeFeedbackPath(tc.path)
		if parentID != tc.wantParentID || suffix != tc.wantSuffix || ok != tc.wantOK {
			t.Errorf("extractEpisodeFeedbackPath(%q) = (%q, %q, %v); want (%q, %q, %v)",
				tc.path, parentID, suffix, ok, tc.wantParentID, tc.wantSuffix, tc.wantOK)
		}
	}
}

func TestCorrectedActionPresent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  json.RawMessage
		want bool
	}{
		{"nil", nil, false},
		{"empty", json.RawMessage{}, false},
		{"whitespace", json.RawMessage("  \t\n "), false},
		{"explicit null", json.RawMessage("null"), false},
		{"padded null", json.RawMessage("  null  "), false},
		{"empty object", json.RawMessage("{}"), true},
		{"object", json.RawMessage(`{"op":"x"}`), true},
		{"array", json.RawMessage(`["x"]`), true},
		{"string", json.RawMessage(`"x"`), true},
	}
	for _, tc := range cases {
		if got := correctedActionPresent(tc.raw); got != tc.want {
			t.Errorf("correctedActionPresent(%q) = %v, want %v", string(tc.raw), got, tc.want)
		}
	}
}

func TestValidateFeedbackRequest_matrix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		req        FeedbackRequest
		wantOK     bool
		wantCode   string
	}{
		{"hc with object", FeedbackRequest{Outcome: "human_corrected", CorrectedAction: json.RawMessage(`{"x":1}`)}, true, ""},
		{"hc missing ca", FeedbackRequest{Outcome: "human_corrected"}, false, "corrected_action_required"},
		{"hc explicit null ca", FeedbackRequest{Outcome: "human_corrected", CorrectedAction: json.RawMessage(`null`)}, false, "corrected_action_required"},
		{"hc scalar ca", FeedbackRequest{Outcome: "human_corrected", CorrectedAction: json.RawMessage(`"x"`)}, false, "invalid_corrected_action"},
		{"hc array ca", FeedbackRequest{Outcome: "human_corrected", CorrectedAction: json.RawMessage(`[]`)}, false, "invalid_corrected_action"},
		{"success no ca", FeedbackRequest{Outcome: "success"}, true, ""},
		{"success null ca", FeedbackRequest{Outcome: "success", CorrectedAction: json.RawMessage(`null`)}, true, ""},
		{"success with ca", FeedbackRequest{Outcome: "success", CorrectedAction: json.RawMessage(`{"x":1}`)}, false, "corrected_action_forbidden"},
		{"failure with ca", FeedbackRequest{Outcome: "failure", CorrectedAction: json.RawMessage(`{"x":1}`)}, false, "corrected_action_forbidden"},
		{"refused with ca", FeedbackRequest{Outcome: "refused", CorrectedAction: json.RawMessage(`{"x":1}`)}, false, "corrected_action_forbidden"},
		{"degraded with ca", FeedbackRequest{Outcome: "degraded", CorrectedAction: json.RawMessage(`{"x":1}`)}, false, "corrected_action_forbidden"},
		{"missing outcome", FeedbackRequest{}, false, "invalid_request"},
		{"unknown outcome", FeedbackRequest{Outcome: "great"}, false, "invalid_outcome"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			req := tc.req // copy; validateFeedbackRequest mutates req.Outcome
			code, _, ok := validateFeedbackRequest(&req)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (code=%q)", ok, tc.wantOK, code)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}

// -----------------------------------------------------------
// shared test sentinels
// -----------------------------------------------------------

// errSimulatedOutage is the sentinel we feed into sqlmock to
// simulate a transient DB failure. The handler MUST log it
// internally but return a generic operator-facing error.
var errSimulatedOutage = errors.New("simulated pq connection refused")

// assertFeedbackValidationError is a small helper that
// collapses the "status + JSON envelope code" assertion shape
// repeated across the validation tests. Keeps the test bodies
// focused on the input under test.
func assertFeedbackValidationError(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, wantStatus, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != wantCode {
		t.Errorf("code = %q, want %q. body=%q", env.Code, wantCode, w.Body.String())
	}
	if !env.Error {
		t.Errorf("envelope.error = false; want true on a 4xx response")
	}
}
