package mgmtapi

// Behavioural unit tests for the Stage 7.1 Management API
// handler, driven via httptest.ResponseRecorder + go-sqlmock so
// the full auth -> validate -> resolve -> DB -> respond
// pipeline runs without a live PostgreSQL.
//
// The matrix maps 1:1 onto the implementation-plan.md Stage 7.1
// test scenarios:
//
//   * register issues HMAC secret once
//       TestRegister_freshRepo_returnsSecret_andIngestJob
//       TestRegister_existingRepo_omitsSecret
//   * ingest_delta is idempotent
//       TestIngestDelta_repeatedCall_returnsSameJobID_noSecondRepoEvent
//   * missing OIDC token rejected
//       TestAuth_missingHeader_returns401_noDBAccess
//       TestAuth_invalidToken_returns401_noDBAccess
//
// Plus the typed-error matrix the brief implies and the
// rubber-duck pass surfaced:
//
//   * malformed JSON -> 400
//   * missing required field -> 400
//   * invalid SHA shape -> 400
//   * invalid repo_id UUID -> 400 / 404
//   * resolver outage -> 502
//   * DB outage -> 500
//   * 405 / 404 envelopes uniform

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// Constants reused across tests. The repo id is a real UUID
// shape so the handler's regex pre-check passes.
const (
	testRepoID  = "11111111-2222-3333-4444-555555555555"
	testToken   = "dev-token-not-for-production"
	testHeadSHA = "abcdefabcdefabcdefabcdefabcdefabcdef0001"
	testFromSHA = "abcdefabcdefabcdefabcdefabcdefabcdef0002"
	testToSHA   = "abcdefabcdefabcdefabcdefabcdefabcdef0003"
	testRepoURL = "https://git.example/acme/svc"
	testBranch  = "main"
	testSecret  = "deadbeef" + // 64-char hex (matches HMACSecretBytes*2)
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
)

// silentLogger discards log output so the test runner stays
// quiet. We don't want to assert log contents in every test
// — handler_unit_test focuses on HTTP / DB behaviour.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fixedSecretGen returns a SecretGen closure that always
// produces `testSecret`. Lets every test that exercises the
// register path assert on a deterministic body.
func fixedSecretGen() func() (string, error) {
	return func() (string, error) { return testSecret, nil }
}

// fakeResolver returns a [HeadResolver] that responds with
// `sha` and `err` for every call. Wrapping it in
// resolverFunc keeps the test code free of an explicit type
// declaration per test.
func fakeResolver(sha string, err error) HeadResolver {
	return resolverFunc(func(_ context.Context, _, _ string) (string, error) {
		return sha, err
	})
}

// newTestHandler wires a Handler with sqlmock, a fixed
// secret, a StaticBearerVerifier accepting `testToken`, and a
// resolver returning `testHeadSHA`. Cleanup verifies sqlmock
// expectations and closes the DB.
func newTestHandler(t *testing.T) (*Handler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:    silentLogger(),
			SecretGen: fixedSecretGen(),
		},
	)
	return h, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// newTestHandlerWithResolver is like newTestHandler but lets
// the test choose the resolver behaviour.
func newTestHandlerWithResolver(t *testing.T, r HeadResolver) (*Handler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		r,
		Options{
			Logger:    silentLogger(),
			SecretGen: fixedSecretGen(),
		},
	)
	return h, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// authedRequest builds a POST request with the test bearer
// token. Pass `sendAuth=false` to OMIT the Authorization
// header (so tests can assert the 401 path without rebuilding
// the helper).
func authedRequest(t *testing.T, sendAuth bool, path string, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, path, &buf)
	r.Header.Set("Content-Type", "application/json")
	if sendAuth {
		r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	}
	return r
}

// rawRequest builds a request with the raw body bytes (so
// tests can send malformed JSON, oversized bodies, etc).
func rawRequest(t *testing.T, sendAuth bool, method, path string, body []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if sendAuth {
		r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	}
	return r
}

// expectRegisterInserts queues the four-statement register
// transaction expected on a brand-new repo.
func expectRegisterInserts(mock sqlmock.Sqlmock, repoID, repoURL, branch, headSHA, secret string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO repo \(url, default_branch, current_head_sha\)`).
		WithArgs(repoURL, branch, headSHA).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id"}).AddRow(repoID))
	mock.ExpectExec(`INSERT INTO repo_webhook_secret`).
		WithArgs(repoID, secret).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO repo_event \(repo_id, kind, from_sha, to_sha\)`).
		WithArgs(repoID, headSHA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`INSERT INTO ingest_jobs \(repo_id, mode, from_sha, to_sha\)`).
		WithArgs(repoID, headSHA).
		WillReturnRows(sqlmock.NewRows([]string{"job_id"}).AddRow("job-uuid-1"))
	mock.ExpectCommit()
}

// expectRegisterIdempotent queues the no-op path: the INSERT
// returns no rows (conflict on url), the handler SELECTs the
// existing repo_id, and the tx commits without writing the
// secret / event / job rows.
func expectRegisterIdempotent(mock sqlmock.Sqlmock, repoID, repoURL, branch, headSHA string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO repo \(url, default_branch, current_head_sha\)`).
		WithArgs(repoURL, branch, headSHA).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT repo_id::text, default_branch, current_head_sha\s+FROM repo`).
		WithArgs(repoURL).
		WillReturnRows(
			sqlmock.NewRows([]string{"repo_id", "default_branch", "current_head_sha"}).
				AddRow(repoID, branch, headSHA))
	mock.ExpectCommit()
}

// expectIngestDeltaNew queues the four-statement enqueue path
// for the FIRST identical ingest_delta call.
func expectIngestDeltaNew(mock sqlmock.Sqlmock, repoID, fromSHA, toSHA, jobID, jobState string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO ingest_jobs \(repo_id, mode, from_sha, to_sha\)`).
		WithArgs(repoID, fromSHA, toSHA).
		WillReturnRows(sqlmock.NewRows([]string{"job_id", "status"}).AddRow(jobID, jobState))
	mock.ExpectExec(`INSERT INTO repo_event \(repo_id, kind, from_sha, to_sha\)`).
		WithArgs(repoID, fromSHA, toSHA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
}

// expectIngestDeltaDeduped queues the conflict path for a
// SECOND identical ingest_delta call: the INSERT collides,
// the handler SELECTs the existing job, and the tx commits
// WITHOUT a new repo_event row.
func expectIngestDeltaDeduped(mock sqlmock.Sqlmock, repoID, fromSHA, toSHA, jobID, jobState string) {
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO ingest_jobs \(repo_id, mode, from_sha, to_sha\)`).
		WithArgs(repoID, fromSHA, toSHA).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT job_id::text, status::text\s+FROM ingest_jobs`).
		WithArgs(repoID, fromSHA, toSHA).
		WillReturnRows(sqlmock.NewRows([]string{"job_id", "status"}).AddRow(jobID, jobState))
	mock.ExpectCommit()
}

// -----------------------------------------------------------
// register: scenario "register issues HMAC secret once"
// -----------------------------------------------------------

func TestRegister_freshRepo_returnsSecret_andIngestJob(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	expectRegisterInserts(mock, testRepoID, testRepoURL, testBranch, testHeadSHA, testSecret)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, RouteRepos, RegisterRequest{
		RepoURL: testRepoURL, DefaultBranch: testBranch,
	}))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusCreated, w.Body.String())
	}
	var resp RegisterResponse
	mustDecode(t, w.Body.Bytes(), &resp)
	if resp.RepoID != testRepoID {
		t.Errorf("repo_id = %q, want %q", resp.RepoID, testRepoID)
	}
	if resp.WebhookSecret != testSecret {
		t.Errorf("webhook_secret = %q, want %q (first registration must reveal secret)",
			resp.WebhookSecret, testSecret)
	}
	if resp.Status != "registered" {
		t.Errorf("status = %q, want registered", resp.Status)
	}
	if resp.IngestJobID != "job-uuid-1" {
		t.Errorf("ingest_job_id = %q, want job-uuid-1", resp.IngestJobID)
	}
	if resp.CurrentHeadSHA != testHeadSHA {
		t.Errorf("current_head_sha = %q, want %q", resp.CurrentHeadSHA, testHeadSHA)
	}
}

func TestRegister_existingRepo_omitsSecret(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	expectRegisterIdempotent(mock, testRepoID, testRepoURL, testBranch, testHeadSHA)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, RouteRepos, RegisterRequest{
		RepoURL: testRepoURL, DefaultBranch: testBranch,
	}))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusOK, w.Body.String())
	}
	var resp RegisterResponse
	mustDecode(t, w.Body.Bytes(), &resp)
	if resp.RepoID != testRepoID {
		t.Errorf("repo_id = %q, want %q", resp.RepoID, testRepoID)
	}
	if resp.WebhookSecret != "" {
		t.Errorf("webhook_secret = %q, want empty (existing repo never reveals secret)",
			resp.WebhookSecret)
	}
	if resp.Status != "exists" {
		t.Errorf("status = %q, want exists", resp.Status)
	}
	// The JSON output must not even contain the key on the
	// existing-repo path — `omitempty` is normative for
	// the secret-revealed-once invariant.
	if strings.Contains(w.Body.String(), "webhook_secret") {
		t.Errorf("response body contains webhook_secret key on existing-repo path: %q",
			w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ingest_job_id") {
		t.Errorf("response body contains ingest_job_id key on existing-repo path: %q",
			w.Body.String())
	}
}

// -----------------------------------------------------------
// ingest_delta: scenario "ingest_delta is idempotent"
// -----------------------------------------------------------

func TestIngestDelta_repeatedCall_returnsSameJobID_noSecondRepoEvent(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	// Call 1: new row, RepoEvent written.
	expectIngestDeltaNew(mock, testRepoID, testFromSHA, testToSHA, "job-uuid-delta", "pending")
	// Call 2: conflict, RepoEvent skipped, same job_id
	// returned.
	expectIngestDeltaDeduped(mock, testRepoID, testFromSHA, testToSHA, "job-uuid-delta", "pending")

	deltaPath := RouteRepos + "/" + testRepoID + ingestDeltaSuffix

	for i, expectedDedupe := range []bool{false, true} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, authedRequest(t, true, deltaPath, IngestDeltaRequest{
			FromSHA: testFromSHA, ToSHA: testToSHA,
		}))
		if w.Code != http.StatusAccepted {
			t.Fatalf("call %d: status = %d, want 202. body=%q", i, w.Code, w.Body.String())
		}
		var resp IngestDeltaResponse
		mustDecode(t, w.Body.Bytes(), &resp)
		if resp.IngestJobID != "job-uuid-delta" {
			t.Errorf("call %d: ingest_job_id = %q, want job-uuid-delta (idempotency broken)",
				i, resp.IngestJobID)
		}
		if resp.Deduped != expectedDedupe {
			t.Errorf("call %d: deduped = %v, want %v", i, resp.Deduped, expectedDedupe)
		}
		if resp.Mode != "delta" {
			t.Errorf("call %d: mode = %q, want delta", i, resp.Mode)
		}
	}
}

// -----------------------------------------------------------
// auth: scenario "missing OIDC token rejected"
// -----------------------------------------------------------

func TestAuth_missingHeader_returns401_noDBAccess(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	// No DB expectations queued — auth must short-circuit
	// the request BEFORE the body is read. If any DB call
	// fires, ExpectationsWereMet will fail the test.

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, false /* no auth header */, RouteRepos, RegisterRequest{
		RepoURL: testRepoURL, DefaultBranch: testBranch,
	}))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body=%q", w.Code, w.Body.String())
	}
	wantChallenge := `Bearer realm="agent-memory mgmt-api"`
	if got := w.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, wantChallenge) {
		t.Errorf("WWW-Authenticate = %q, want prefix %q", got, wantChallenge)
	}
	_ = mock
}

func TestAuth_invalidToken_returns401_noDBAccess(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	r := httptest.NewRequest(http.MethodPost, RouteRepos, bytes.NewReader([]byte(`{}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(AuthorizationHeader, "Bearer wrong-token-but-right-length-padding")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Header().Get("WWW-Authenticate"), `error="invalid_token"`) {
		t.Errorf("WWW-Authenticate = %q, want substring error=\"invalid_token\"",
			w.Header().Get("WWW-Authenticate"))
	}
	_ = mock
}

func TestAuth_caseInsensitiveBearer(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	expectRegisterInserts(mock, testRepoID, testRepoURL, testBranch, testHeadSHA, testSecret)

	r := authedRequest(t, true, RouteRepos, RegisterRequest{
		RepoURL: testRepoURL, DefaultBranch: testBranch,
	})
	// Lower-case scheme name (RFC 6750 §2.1: case-insensitive).
	r.Header.Set(AuthorizationHeader, "bearer "+testToken)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// validation matrix
// -----------------------------------------------------------

func TestRegister_malformedJSON_returns400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, rawRequest(t, true, http.MethodPost, RouteRepos, []byte(`{"repo_url":}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "invalid_json" {
		t.Errorf("code = %q, want invalid_json", env.Code)
	}
}

func TestRegister_missingRepoURL_returns400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, RouteRepos, RegisterRequest{DefaultBranch: testBranch}))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "repo_url") {
		t.Errorf("body = %q, want substring repo_url", w.Body.String())
	}
}

func TestRegister_missingBranch_returns400(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, RouteRepos, RegisterRequest{RepoURL: testRepoURL}))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "default_branch") {
		t.Errorf("body = %q, want substring default_branch", w.Body.String())
	}
}

func TestRegister_resolverOutage_returns502_noDBWrites(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandlerWithResolver(t,
		fakeResolver("", ErrHeadResolverUnavailable))
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, RouteRepos, RegisterRequest{
		RepoURL: testRepoURL, DefaultBranch: testBranch,
	}))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502. body=%q", w.Code, w.Body.String())
	}
	_ = mock // No DB ops queued — outage must short-circuit before begin tx.
}

func TestRegister_resolverUnknownRef_returns400(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandlerWithResolver(t,
		fakeResolver("", ErrHeadResolverUnknownRef))
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, RouteRepos, RegisterRequest{
		RepoURL: testRepoURL, DefaultBranch: "bogus-branch",
	}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	_ = mock
}

func TestIngest_invalidRepoID_returns400(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/not-a-uuid"+ingestSuffix,
		IngestRequest{SHA: testHeadSHA}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	_ = mock
}

func TestIngest_unknownRepoID_returns404(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT url, default_branch, current_head_sha\s+FROM repo`).
		WithArgs(testRepoID).
		WillReturnError(sql.ErrNoRows)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/"+testRepoID+ingestSuffix,
		IngestRequest{SHA: testHeadSHA}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body=%q", w.Code, w.Body.String())
	}
}

func TestIngest_invalidSHA_returns400(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	// Item 4 of the iter-1 evaluator feedback: the SHA
	// shape MUST be validated BEFORE any DB read so a
	// malformed operator input can never be masked by a
	// transient DB outage as a 500. No loadRepo expectation
	// is queued — if a SELECT runs the sqlmock
	// ExpectationsWereMet check at cleanup will fail this
	// test (and the change in handler.go is therefore
	// gated by behaviour, not just by code review).

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/"+testRepoID+ingestSuffix,
		IngestRequest{SHA: "NOT-A-VALID-SHA"}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	_ = mock
}

func TestIngest_defaultSHA_resolvesViaResolver(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	// loadRepo returns cached head and url/branch.
	mock.ExpectQuery(`SELECT url, default_branch, current_head_sha\s+FROM repo`).
		WithArgs(testRepoID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "default_branch", "current_head_sha"}).
			AddRow(testRepoURL, testBranch, testHeadSHA))

	// Enqueue uses the resolver-returned SHA (testHeadSHA
	// per newTestHandler).
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO ingest_jobs \(repo_id, mode, from_sha, to_sha\)`).
		WithArgs(testRepoID, testHeadSHA).
		WillReturnRows(sqlmock.NewRows([]string{"job_id", "status"}).AddRow("ji-1", "pending"))
	mock.ExpectExec(`INSERT INTO repo_event \(repo_id, kind, from_sha, to_sha\)`).
		WithArgs(testRepoID, testHeadSHA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/"+testRepoID+ingestSuffix,
		IngestRequest{} /* no SHA */))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
}

// Item 5 of the iter-1 evaluator feedback: when ingest is
// called WITHOUT a SHA and the resolver is unavailable, the
// handler MUST surface 502 — it MUST NOT silently fall back
// to the cached `repo.current_head_sha`. Stage 7.1 write
// verbs have no degraded-mode contract.
func TestIngest_defaultSHA_resolverOutage_returns502_noEnqueue(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandlerWithResolver(t,
		fakeResolver("", ErrHeadResolverUnavailable))
	defer cleanup()

	// loadRepo runs (we need the repo's url/branch to ask
	// the resolver) but no INSERT must follow on outage.
	mock.ExpectQuery(`SELECT url, default_branch, current_head_sha\s+FROM repo`).
		WithArgs(testRepoID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "default_branch", "current_head_sha"}).
			AddRow(testRepoURL, testBranch, testHeadSHA))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/"+testRepoID+ingestSuffix,
		IngestRequest{} /* no SHA */))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502. body=%q", w.Code, w.Body.String())
	}
}

// Item 5 corollary: a resolver returning a non-hex value is
// also a 502 (we cannot trust it as a real HEAD SHA).
func TestIngest_defaultSHA_resolverReturnsGarbage_returns502(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandlerWithResolver(t,
		fakeResolver("not-a-sha", nil))
	defer cleanup()

	mock.ExpectQuery(`SELECT url, default_branch, current_head_sha\s+FROM repo`).
		WithArgs(testRepoID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "default_branch", "current_head_sha"}).
			AddRow(testRepoURL, testBranch, testHeadSHA))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/"+testRepoID+ingestSuffix,
		IngestRequest{} /* no SHA */))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502. body=%q", w.Code, w.Body.String())
	}
}

// Item 3 corollary: 401 responses MUST be JSON, not text/plain.
// The middleware's previous http.Error path emitted
// `text/plain; charset=utf-8`; the JSON envelope path emits
// `application/json; charset=utf-8` with the canonical
// ErrorEnvelope body.
func TestAuth_401_bodyIsJSONEnvelope(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	// Wrong token -> 401 invalid_token branch.
	r := httptest.NewRequest(http.MethodPost, RouteRepos, bytes.NewReader([]byte(`{}`)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(AuthorizationHeader, "Bearer this-is-the-wrong-secret-padding")
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body=%q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
	var env ErrorEnvelope
	if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
		t.Fatalf("decode body as ErrorEnvelope: %v. raw=%q", err, w.Body.String())
	}
	if !env.Error || env.Code == "" {
		t.Errorf("envelope.Error=%v code=%q message=%q, want all set",
			env.Error, env.Code, env.Message)
	}
	if env.Code != "invalid_token" {
		t.Errorf("envelope.Code = %q, want invalid_token", env.Code)
	}
	_ = mock
}

func TestIngestDelta_missingSHA_returns400(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/"+testRepoID+ingestDeltaSuffix,
		IngestDeltaRequest{ToSHA: testToSHA} /* from_sha missing */))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	_ = mock
}

func TestIngestDelta_sameSHA_returns400(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/"+testRepoID+ingestDeltaSuffix,
		IngestDeltaRequest{FromSHA: testToSHA, ToSHA: testToSHA}))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	_ = mock
}

func TestIngestDelta_foreignKeyViolation_returns404(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO ingest_jobs`).
		WithArgs(testRepoID, testFromSHA, testToSHA).
		WillReturnError(errors.New(`pq: insert or update on table "ingest_jobs" violates foreign key constraint "ingest_jobs_repo_id_fkey"`))
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/"+testRepoID+ingestDeltaSuffix,
		IngestDeltaRequest{FromSHA: testFromSHA, ToSHA: testToSHA}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body=%q", w.Code, w.Body.String())
	}
}

func TestIngestDelta_dbOutage_returns500_noLeak(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newTestHandler(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO ingest_jobs`).
		WithArgs(testRepoID, testFromSHA, testToSHA).
		WillReturnError(errors.New("connection refused"))
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true,
		RouteRepos+"/"+testRepoID+ingestDeltaSuffix,
		IngestDeltaRequest{FromSHA: testFromSHA, ToSHA: testToSHA}))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500. body=%q", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Errorf("body leaks raw driver error: %q", w.Body.String())
	}
}

func TestRoute_wrongMethod_returns405(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	// /v1/repos is dual-verb in Stage 7.5 (GET = list, POST =
	// register); use PUT to assert the 405 envelope with the
	// precise Allow header.
	r := httptest.NewRequest(http.MethodPut, RouteRepos, nil)
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405. body=%q", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Allow"); got != "GET, POST" {
		t.Errorf("Allow = %q, want %q", got, "GET, POST")
	}
}

func TestRoute_unknownPath_returns404(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newTestHandler(t)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, "/v1/repos/"+testRepoID+"/unknown", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404. body=%q", w.Code, w.Body.String())
	}
}

func TestRoute_bodyTooLarge_returns413(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken},
		fakeResolver(testHeadSHA, nil),
		Options{Logger: silentLogger(), MaxBodyBytes: 16, SecretGen: fixedSecretGen()},
	)

	big := bytes.Repeat([]byte("a"), 256)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, rawRequest(t, true, http.MethodPost, RouteRepos, big))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// pure-function helpers
// -----------------------------------------------------------

func TestExtractIngestPath(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, wantRepoID, wantSuffix string
		wantOK                       bool
	}{
		{"/v1/repos/" + testRepoID + "/ingest", testRepoID, ingestSuffix, true},
		{"/v1/repos/" + testRepoID + "/ingest_delta", testRepoID, ingestDeltaSuffix, true},
		{"/v1/repos/" + testRepoID, "", "", false},
		{"/v1/repos/" + testRepoID + "/foo/ingest", "", "", false},
		{"/v1/repos//ingest", "", "", false},
	}
	for _, tc := range cases {
		repoID, suffix, ok := extractIngestPath(tc.path)
		if repoID != tc.wantRepoID || suffix != tc.wantSuffix || ok != tc.wantOK {
			t.Errorf("extractIngestPath(%q) = (%q, %q, %v); want (%q, %q, %v)",
				tc.path, repoID, suffix, ok, tc.wantRepoID, tc.wantSuffix, tc.wantOK)
		}
	}
}

func TestNormalizeRepoURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		raw, want string
		wantErr   bool
	}{
		{"https://git.example/acme/svc", "https://git.example/acme/svc", false},
		{"  https://git.example/acme/svc  ", "https://git.example/acme/svc", false},
		{"ssh://git@git.example/acme/svc", "ssh://git@git.example/acme/svc", false},
		{"", "", true},
		{"   ", "", true},
		{"acme/svc", "", true}, // missing scheme
	}
	for _, tc := range cases {
		got, err := normalizeRepoURL(tc.raw)
		if (err != nil) != tc.wantErr {
			t.Errorf("normalizeRepoURL(%q): err=%v, wantErr=%v", tc.raw, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("normalizeRepoURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestIsHexGitSHA(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"abcdefabcdefabcdefabcdefabcdefabcdef0001", true},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 63), false},
		{strings.Repeat("a", 65), false},
		{"ABCDEFabcdefabcdefabcdefabcdefabcdef0001", false}, // upper-case rejected
		{"", false},
		{strings.Repeat("g", 40), false}, // non-hex char
	}
	for _, tc := range cases {
		if got := IsHexGitSHA(tc.in); got != tc.want {
			t.Errorf("IsHexGitSHA(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestStaticBearerVerifier(t *testing.T) {
	t.Parallel()
	v := &StaticBearerVerifier{Secret: "alpha", Subject: "ops"}
	if _, err := v.Verify(context.Background(), "alpha"); err != nil {
		t.Errorf("Verify(alpha) = %v, want nil", err)
	}
	if _, err := v.Verify(context.Background(), ""); !errors.Is(err, ErrTokenMissing) {
		t.Errorf("Verify(empty) err = %v, want ErrTokenMissing", err)
	}
	if _, err := v.Verify(context.Background(), "beta"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("Verify(beta) err = %v, want ErrTokenInvalid", err)
	}
	// Empty Secret rejects every token (fail closed).
	empty := &StaticBearerVerifier{}
	if _, err := empty.Verify(context.Background(), "anything"); !errors.Is(err, ErrTokenInvalid) {
		t.Errorf("empty Secret: Verify(anything) err = %v, want ErrTokenInvalid", err)
	}
}

func TestExtractBearer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in        string
		wantToken string
		wantOK    bool
	}{
		{"Bearer abc", "abc", true},
		{"bearer abc", "abc", true},
		{"BEARER abc", "abc", true},
		{"  Bearer abc  ", "abc", true},
		{"Bearer ", "", false},
		{"abc", "", false},
		{"Basic abc", "", false},
		{"Bearer abc def", "", false}, // embedded space
		{"", "", false},
	}
	for _, tc := range cases {
		got, ok := extractBearer(tc.in)
		if got != tc.wantToken || ok != tc.wantOK {
			t.Errorf("extractBearer(%q) = (%q, %v); want (%q, %v)",
				tc.in, got, ok, tc.wantToken, tc.wantOK)
		}
	}
}

func TestDegradedEnvelope_marshalsFlat(t *testing.T) {
	t.Parallel()
	type payload struct {
		RepoID string `json:"repo_id"`
		Count  int    `json:"count"`
	}
	env := DegradedEnvelope[payload]{
		Payload:        payload{RepoID: "abc", Count: 3},
		Degraded:       true,
		DegradedReason: "snapshot fallback",
	}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v -- %s", err, b)
	}
	if got["repo_id"] != "abc" || got["count"] != float64(3) {
		t.Errorf("payload fields missing or wrong: %v -- %s", got, b)
	}
	if got["degraded"] != true {
		t.Errorf("degraded missing: %v -- %s", got, b)
	}
	if got["degraded_reason"] != "snapshot fallback" {
		t.Errorf("degraded_reason missing: %v -- %s", got, b)
	}

	// Non-degraded path: degraded_reason key MUST be
	// omitted (`omitempty`).
	env2 := DegradedEnvelope[payload]{
		Payload: payload{RepoID: "xyz", Count: 0},
	}
	b2, err := json.Marshal(env2)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(b2), "degraded_reason") {
		t.Errorf("degraded_reason should be omitted: %s", b2)
	}
	if !strings.Contains(string(b2), `"degraded":false`) {
		t.Errorf("degraded:false missing: %s", b2)
	}
}

// mustDecode is a small helper that fails the test on a decode
// error so the body of each test stays focused on assertions.
func mustDecode(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode response: %v -- body=%q", err, raw)
	}
}
