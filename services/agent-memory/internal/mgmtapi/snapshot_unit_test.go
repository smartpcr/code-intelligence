package mgmtapi

// Behavioural unit tests for the Stage 7.4 `mgmt.snapshot`
// verb, driven via httptest.ResponseRecorder + go-sqlmock so
// the full auth -> validate -> loadRepo -> snapshotter ->
// respond pipeline runs without a live PostgreSQL.
//
// The matrix maps 1:1 onto the implementation-plan.md Stage
// 7.4 test scenarios plus the typed-error matrix the brief
// implies:
//
//   * happy path -> 202 + counts envelope
//       TestSnapshot_happy_returns202_andCounts
//   * dedupe-only call returns 202 with skipped counts
//       TestSnapshot_skippedOnly_returns202_andSkippedCounts
//   * unknown repo -> 404
//       TestSnapshot_unknownRepo_returns404
//   * malformed UUID -> 400
//       TestSnapshot_badRepoID_returns400
//   * unwired snapshotter -> 503
//       TestSnapshot_noSnapshotter_returns503
//   * snapshotter sentinel-from-implementation -> 404
//       TestSnapshot_snapshotterRepoNotFound_returns404
//   * snapshotter generic error -> 500
//       TestSnapshot_snapshotterError_returns500
//   * missing auth header -> 401
//       TestSnapshot_noAuth_returns401
//   * wrong method -> 405
//       TestSnapshot_wrongMethod_returns405
//   * extra path segment -> 404
//       TestSnapshot_extraSegments_returns404

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// snapshotPath returns the conventional path for a given
// repo id; mirrors feedbackPath / RouteRepos+"/{id}/ingest".
func snapshotPath(repoID string) string {
	return RouteRepos + "/" + repoID + snapshotSuffix
}

// stubSnapshotter is the in-process implementation tests use
// to drive the handler. The closure-shape makes per-test
// behaviour selection a one-liner.
type stubSnapshotter struct {
	calls int
	fn    func(ctx context.Context, repoID string) (SnapshotResult, error)
}

func (s *stubSnapshotter) Snapshot(ctx context.Context, repoID string) (SnapshotResult, error) {
	s.calls++
	if s.fn == nil {
		return SnapshotResult{}, errors.New("stubSnapshotter: fn not set")
	}
	return s.fn(ctx, repoID)
}

// newSnapshotHandler wires a Handler with sqlmock, a stub
// snapshotter, fixed bearer auth, and a silent logger. The
// snapshotter is returned to the caller so the test can
// assert on `calls` after ServeHTTP.
func newSnapshotHandler(t *testing.T, stub *stubSnapshotter) (*Handler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	opts := Options{
		Logger:    silentLogger(),
		SecretGen: fixedSecretGen(),
	}
	if stub != nil {
		opts.Snapshotter = stub
	}
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		opts,
	)
	return h, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// expectSnapshotLoadRepo queues the repo-lookup that the
// handler issues before delegating to the Snapshotter. Pass
// found=false to make it return sql.ErrNoRows.
func expectSnapshotLoadRepo(mock sqlmock.Sqlmock, repoID string, found bool) {
	q := mock.ExpectQuery(`SELECT url, default_branch, current_head_sha\s+FROM repo\s+WHERE repo_id = \$1::uuid`).
		WithArgs(repoID)
	if !found {
		q.WillReturnError(sql.ErrNoRows)
		return
	}
	q.WillReturnRows(sqlmock.NewRows([]string{"url", "default_branch", "current_head_sha"}).
		AddRow(testRepoURL, testBranch, testHeadSHA))
}

// -----------------------------------------------------------
// Scenario: happy path returns 202 + counts
// -----------------------------------------------------------

func TestSnapshot_happy_returns202_andCounts(t *testing.T) {
	t.Parallel()
	stub := &stubSnapshotter{
		fn: func(_ context.Context, repoID string) (SnapshotResult, error) {
			if repoID != testRepoID {
				t.Fatalf("snapshotter got repoID=%q, want %q", repoID, testRepoID)
			}
			return SnapshotResult{
				SnapshotID:          "snap-abc",
				ModelVersion:        "stub@v1",
				MethodsEnqueued:     7,
				BlocksEnqueued:      3,
				ConceptsEnqueued:    2,
				MethodBlocksSkipped: 1,
				ConceptsSkipped:     0,
			}, nil
		},
	}
	h, mock, cleanup := newSnapshotHandler(t, stub)
	defer cleanup()

	expectSnapshotLoadRepo(mock, testRepoID, true)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusAccepted, w.Body.String())
	}
	if stub.calls != 1 {
		t.Errorf("snapshotter calls = %d, want 1", stub.calls)
	}

	var resp SnapshotResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v. body=%q", err, w.Body.String())
	}
	if resp.SnapshotID != "snap-abc" {
		t.Errorf("snapshot_id = %q, want snap-abc", resp.SnapshotID)
	}
	if resp.ModelVersion != "stub@v1" {
		t.Errorf("model_version = %q, want stub@v1", resp.ModelVersion)
	}
	if resp.MethodsEnqueued != 7 {
		t.Errorf("methods_enqueued = %d, want 7", resp.MethodsEnqueued)
	}
	if resp.BlocksEnqueued != 3 {
		t.Errorf("blocks_enqueued = %d, want 3", resp.BlocksEnqueued)
	}
	if resp.ConceptsEnqueued != 2 {
		t.Errorf("concepts_enqueued = %d, want 2", resp.ConceptsEnqueued)
	}
	if resp.TargetsSkipped != 1 {
		t.Errorf("targets_skipped = %d, want 1", resp.TargetsSkipped)
	}
}

// -----------------------------------------------------------
// Scenario: dedupe-only call still 202, surfaces skipped count
// -----------------------------------------------------------

func TestSnapshot_skippedOnly_returns202_andSkippedCounts(t *testing.T) {
	t.Parallel()
	stub := &stubSnapshotter{
		fn: func(_ context.Context, _ string) (SnapshotResult, error) {
			return SnapshotResult{
				SnapshotID:          "snap-dedupe",
				ModelVersion:        "stub@v1",
				MethodBlocksSkipped: 4,
				ConceptsSkipped:     2,
			}, nil
		},
	}
	h, mock, cleanup := newSnapshotHandler(t, stub)
	defer cleanup()

	expectSnapshotLoadRepo(mock, testRepoID, true)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusAccepted, w.Body.String())
	}
	var resp SnapshotResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.MethodsEnqueued != 0 || resp.BlocksEnqueued != 0 || resp.ConceptsEnqueued != 0 {
		t.Errorf("expected zero enqueue counts on dedupe-only, got %+v", resp)
	}
	if resp.TargetsSkipped != 6 {
		t.Errorf("targets_skipped = %d, want 6 (4 method-block + 2 concept)", resp.TargetsSkipped)
	}
}

// -----------------------------------------------------------
// Scenario: unknown repo -> 404 (handler loadRepo path)
// -----------------------------------------------------------

func TestSnapshot_unknownRepo_returns404(t *testing.T) {
	t.Parallel()
	stub := &stubSnapshotter{
		fn: func(_ context.Context, _ string) (SnapshotResult, error) {
			t.Fatalf("snapshotter must not be called when repo is unknown")
			return SnapshotResult{}, nil
		},
	}
	h, mock, cleanup := newSnapshotHandler(t, stub)
	defer cleanup()

	expectSnapshotLoadRepo(mock, testRepoID, false)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusNotFound, w.Body.String())
	}
	if stub.calls != 0 {
		t.Errorf("snapshotter calls = %d, want 0", stub.calls)
	}
	assertErrorCode(t, w.Body.Bytes(), "repo_not_found")
}

// -----------------------------------------------------------
// Scenario: bad repo_id UUID -> 400, no DB read
// -----------------------------------------------------------

func TestSnapshot_badRepoID_returns400(t *testing.T) {
	t.Parallel()
	stub := &stubSnapshotter{
		fn: func(_ context.Context, _ string) (SnapshotResult, error) {
			t.Fatalf("snapshotter must not be called for invalid repo_id")
			return SnapshotResult{}, nil
		},
	}
	h, _, cleanup := newSnapshotHandler(t, stub)
	defer cleanup()
	// Note: expect no loadRepo call -- the UUID regex pre-check rejects.

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath("not-a-uuid"), nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if stub.calls != 0 {
		t.Errorf("snapshotter calls = %d, want 0", stub.calls)
	}
	assertErrorCode(t, w.Body.Bytes(), "invalid_repo_id")
}

// -----------------------------------------------------------
// Scenario: missing Snapshotter -> 503
// -----------------------------------------------------------

func TestSnapshot_noSnapshotter_returns503(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newSnapshotHandler(t, nil)
	defer cleanup()

	// Handler still does loadRepo first (so a missing
	// snapshot wiring and a missing repo report distinct
	// errors -- the operator who hits 503 knows the repo
	// existed, just the feature wasn't wired).
	expectSnapshotLoadRepo(mock, testRepoID, true)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d. body=%q",
			w.Code, http.StatusServiceUnavailable, w.Body.String())
	}
	assertErrorCode(t, w.Body.Bytes(), "snapshot_unavailable")
}

// -----------------------------------------------------------
// Scenario: Snapshotter returns ErrSnapshotRepoNotFound -> 404
// -----------------------------------------------------------

func TestSnapshot_snapshotterRepoNotFound_returns404(t *testing.T) {
	t.Parallel()
	stub := &stubSnapshotter{
		fn: func(_ context.Context, _ string) (SnapshotResult, error) {
			// Simulates the race where the repo existed at
			// handler.loadRepo time but was deleted before the
			// snapshotter's own existence check.
			return SnapshotResult{}, ErrSnapshotRepoNotFound
		},
	}
	h, mock, cleanup := newSnapshotHandler(t, stub)
	defer cleanup()

	expectSnapshotLoadRepo(mock, testRepoID, true)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d. body=%q",
			w.Code, http.StatusNotFound, w.Body.String())
	}
	assertErrorCode(t, w.Body.Bytes(), "repo_not_found")
}

// -----------------------------------------------------------
// Scenario: Snapshotter returns generic error -> 500
// -----------------------------------------------------------

func TestSnapshot_snapshotterError_returns500(t *testing.T) {
	t.Parallel()
	stub := &stubSnapshotter{
		fn: func(_ context.Context, _ string) (SnapshotResult, error) {
			return SnapshotResult{}, errors.New("pg conn refused")
		},
	}
	h, mock, cleanup := newSnapshotHandler(t, stub)
	defer cleanup()

	expectSnapshotLoadRepo(mock, testRepoID, true)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, true, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d. body=%q",
			w.Code, http.StatusInternalServerError, w.Body.String())
	}
	assertErrorCode(t, w.Body.Bytes(), "internal_error")
}

// -----------------------------------------------------------
// Scenario: missing auth header -> 401, no DB read, no
// snapshotter call (auth middleware fires first).
// -----------------------------------------------------------

func TestSnapshot_noAuth_returns401(t *testing.T) {
	t.Parallel()
	stub := &stubSnapshotter{
		fn: func(_ context.Context, _ string) (SnapshotResult, error) {
			t.Fatalf("snapshotter must not be called without auth")
			return SnapshotResult{}, nil
		},
	}
	h, _, cleanup := newSnapshotHandler(t, stub)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedRequest(t, false, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d. body=%q",
			w.Code, http.StatusUnauthorized, w.Body.String())
	}
	if stub.calls != 0 {
		t.Errorf("snapshotter calls = %d, want 0", stub.calls)
	}
}

// -----------------------------------------------------------
// Scenario: wrong method (GET) -> 405 with Allow: POST
// -----------------------------------------------------------

func TestSnapshot_wrongMethod_returns405(t *testing.T) {
	t.Parallel()
	stub := &stubSnapshotter{}
	h, _, cleanup := newSnapshotHandler(t, stub)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, rawRequest(t, true, http.MethodGet, snapshotPath(testRepoID), nil))

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d. body=%q",
			w.Code, http.StatusMethodNotAllowed, w.Body.String())
	}
	if got := w.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow header = %q, want %q", got, http.MethodPost)
	}
	if stub.calls != 0 {
		t.Errorf("snapshotter calls = %d, want 0", stub.calls)
	}
}

// -----------------------------------------------------------
// Scenario: extra path segments -> 404 (route dispatch
// rejects shape before any verb handler runs).
// -----------------------------------------------------------

func TestSnapshot_extraSegments_returns404(t *testing.T) {
	t.Parallel()
	stub := &stubSnapshotter{}
	h, _, cleanup := newSnapshotHandler(t, stub)
	defer cleanup()

	w := httptest.NewRecorder()
	// Extra segment between repo_id and `/snapshot`.
	path := RouteRepos + "/" + testRepoID + "/foo" + snapshotSuffix
	h.ServeHTTP(w, authedRequest(t, true, path, nil))

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d. body=%q",
			w.Code, http.StatusNotFound, w.Body.String())
	}
	assertErrorCode(t, w.Body.Bytes(), "not_found")
}

// -----------------------------------------------------------
// Test helpers
// -----------------------------------------------------------

// assertErrorCode decodes an ErrorEnvelope and checks the
// `code` field. Centralised so every test reports a uniform
// failure message on shape mismatch.
func assertErrorCode(t *testing.T, body []byte, wantCode string) {
	t.Helper()
	var env ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode ErrorEnvelope: %v. body=%q", err, string(body))
	}
	if !env.Error {
		t.Errorf("error envelope `error` field = false; want true. body=%q", string(body))
	}
	if env.Code != wantCode {
		t.Errorf("error code = %q, want %q. body=%q", env.Code, wantCode, string(body))
	}
}
