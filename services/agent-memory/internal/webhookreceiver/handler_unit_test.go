package webhookreceiver

// Behavioural unit tests for Handler. Driven through
// `httptest.ResponseRecorder` + `go-sqlmock` so the full
// request -> verify -> enqueue -> respond pipeline runs without
// any live PostgreSQL dependency. The live-DB integration test
// in handler_integration_test.go covers schema / role-grant
// drift on top of these unit tests.
//
// Coverage matrix (mirrors the Stage 3.5 acceptance scenarios):
//
//   * Invalid signature rejected -- TestServeHTTP_invalidSignature_returns401_noRowsWritten
//   * Valid push enqueues delta job -- TestServeHTTP_validPush_writesRepoEventAndIngestJob
//
// Plus correctness coverage for the failure modes the brief
// implicitly requires:
//
//   * Missing signature header -- TestServeHTTP_missingSignatureHeader_returns401
//   * Malformed signature header -- TestServeHTTP_malformedSignatureHeader_returns401
//   * Unknown repo_id -- TestServeHTTP_unknownRepo_returns401_noRowsWritten
//   * Non-UUID repo_id -- TestServeHTTP_invalidUUIDPath_returns401
//   * Wrong HTTP method -- TestServeHTTP_wrongMethod_returns405
//   * Off-route path -- TestServeHTTP_offRoute_returns404
//   * Closed-set kind enforcement -- TestServeHTTP_invalidKind_returns400_afterAuth
//   * Missing to_sha -- TestServeHTTP_missingToSHA_returns400
//   * Oversized body -- TestServeHTTP_bodyTooLarge_returns413
//   * Idempotent re-delivery -- TestServeHTTP_duplicatePush_dedupesJobID

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	testRepoID  = "11111111-2222-3333-4444-555555555555"
	testSecret  = "shhh-this-is-test-only-not-prod"
	testFromSHA = "0000000000000000000000000000000000000001"
	testToSHA   = "0000000000000000000000000000000000000002"
)

// newMockHandler returns a Handler wired to a sqlmock-backed
// *sql.DB and a silent logger. The cleanup verifies all
// expectations were met and closes the DB.
func newMockHandler(t *testing.T) (*Handler, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherRegexp,
	))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	h := NewHandler(db, Options{
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

// signPayload computes the GitHub-style HMAC header value for
// `body` under `secret`. Mirrors the format the handler verifies
// against.
func signPayload(t *testing.T, secret string, body []byte) string {
	t.Helper()
	mac := hmac.New(sha256.New, []byte(secret))
	_, err := mac.Write(body)
	if err != nil {
		t.Fatalf("hmac.Write: %v", err)
	}
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// makeRequest builds an authenticated POST request for
// `/webhook/{repoID}` with `body` and the supplied signature
// header value. Pass an empty `sig` to OMIT the signature
// header entirely (different from passing a malformed value).
func makeRequest(t *testing.T, repoID, sig string, body []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, RoutePrefix+repoID, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if sig != "" {
		r.Header.Set(DefaultSignatureHeader, sig)
	}
	return r
}

// expectSecretLookup queues a SELECT on `repo_webhook_secret`
// for `repoID`. `secret` is the value returned; pass an empty
// string to simulate "no row" (sql.ErrNoRows). The query is
// matched against a regex so it stays robust to whitespace
// variations.
func expectSecretLookup(mock sqlmock.Sqlmock, repoID, secret string) {
	q := mock.ExpectQuery(`SELECT webhook_secret FROM repo_webhook_secret WHERE repo_id`).
		WithArgs(repoID)
	if secret == "" {
		q.WillReturnError(sql.ErrNoRows)
	} else {
		q.WillReturnRows(sqlmock.NewRows([]string{"webhook_secret"}).AddRow(secret))
	}
}

// expectEnqueue queues the two INSERTs the happy-path enqueue
// runs inside a single tx. `eventID` / `jobID` / `jobState` are
// returned by the RETURNING clauses. `fromSHA` is the value
// the handler will pass through; pass an empty string to assert
// the handler converts it to SQL NULL.
func expectEnqueue(mock sqlmock.Sqlmock, repoID, fromSHA, toSHA, kind, eventID, jobID, jobState string) {
	mock.ExpectBegin()
	var fromArg driverArg = fromSHA
	if fromSHA == "" {
		fromArg = nil
	}
	mock.ExpectQuery(`INSERT INTO repo_event`).
		WithArgs(repoID, kind, fromArg, toSHA).
		WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow(eventID))
	mock.ExpectQuery(`INSERT INTO ingest_jobs`).
		WithArgs(repoID, fromArg, toSHA).
		WillReturnRows(sqlmock.NewRows([]string{"job_id", "status"}).AddRow(jobID, jobState))
	mock.ExpectCommit()
}

// driverArg lets us pass either a string or nil as a
// sqlmock.WithArgs entry without sprinkling `any` everywhere.
type driverArg = any

// ----- happy-path scenarios -------------------------------------

func TestServeHTTP_validPush_writesRepoEventAndIngestJob(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{
		Kind:    "push",
		FromSHA: testFromSHA,
		ToSHA:   testToSHA,
	})
	sig := signPayload(t, testSecret, body)

	expectSecretLookup(mock, testRepoID, testSecret)
	expectEnqueue(mock, testRepoID, testFromSHA, testToSHA, "push",
		"event-uuid-1", "job-uuid-1", "pending")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusAccepted, w.Body.String())
	}
	var resp Response
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v -- body=%q", err, w.Body.String())
	}
	if resp.EventID != "event-uuid-1" || resp.JobID != "job-uuid-1" || resp.JobState != "pending" {
		t.Fatalf("response = %+v, want event_id=event-uuid-1 job_id=job-uuid-1 job_state=pending", resp)
	}
	if got := w.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", got)
	}
}

func TestServeHTTP_validMerge_acceptsMergeKind(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{
		Kind:    "merge",
		FromSHA: testFromSHA,
		ToSHA:   testToSHA,
	})
	sig := signPayload(t, testSecret, body)

	expectSecretLookup(mock, testRepoID, testSecret)
	expectEnqueue(mock, testRepoID, testFromSHA, testToSHA, "merge",
		"event-uuid-2", "job-uuid-2", "pending")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusAccepted, w.Body.String())
	}
}

func TestServeHTTP_validPushWithoutFromSHA_passesNullToDB(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{
		Kind:  "push",
		ToSHA: testToSHA,
	})
	sig := signPayload(t, testSecret, body)

	expectSecretLookup(mock, testRepoID, testSecret)
	// Empty FromSHA must be passed as SQL NULL -- expectEnqueue
	// helper materialises that as `nil` in WithArgs.
	expectEnqueue(mock, testRepoID, "", testToSHA, "push",
		"event-uuid-3", "job-uuid-3", "pending")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusAccepted, w.Body.String())
	}
}

// ----- authentication-failure scenarios -------------------------

func TestServeHTTP_invalidSignature_returns401_noRowsWritten(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{
		Kind:    "push",
		FromSHA: testFromSHA,
		ToSHA:   testToSHA,
	})
	// HMAC is computed over a DIFFERENT body, so verification
	// must fail and NO db writes may be issued.
	tamperedSig := signPayload(t, testSecret, []byte(`{"kind":"push","to_sha":"different"}`))

	expectSecretLookup(mock, testRepoID, testSecret)
	// No ExpectBegin / ExpectExec -- if the handler tries any
	// DB write after the verify failure, sqlmock will surface it
	// via ExpectationsWereMet's "unexpected exec" reporting.

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, tamperedSig, body))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

func TestServeHTTP_missingSignatureHeader_returns401(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{Kind: "push", ToSHA: testToSHA})

	// The handler reads the body BEFORE looking up the secret,
	// but the verifier short-circuits on a missing header
	// without ever calling lookupSecret. That means we must NOT
	// queue a secret-lookup expectation here -- otherwise
	// ExpectationsWereMet will fail the test.

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, "", body))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusUnauthorized, w.Body.String())
	}
	_ = mock // explicit "no expectations queued" suffices
}

func TestServeHTTP_malformedSignatureHeader_returns401(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name, header string
	}{
		{"missing prefix", hex.EncodeToString([]byte("0123456789abcdef"))},
		{"wrong algo", "sha1=0123"},
		{"empty hex", "sha256="},
		{"non-hex", "sha256=not-hex-at-all"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, mock, cleanup := newMockHandler(t)
			defer cleanup()

			body := mustMarshalJSON(t, Payload{Kind: "push", ToSHA: testToSHA})

			w := httptest.NewRecorder()
			h.ServeHTTP(w, makeRequest(t, testRepoID, tc.header, body))

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
			}
			_ = mock // verifier short-circuits before secret lookup
		})
	}
}

func TestServeHTTP_unknownRepo_returns401_noRowsWritten(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{Kind: "push", ToSHA: testToSHA})
	sig := signPayload(t, testSecret, body)

	// Empty secret simulates sql.ErrNoRows -- the repo isn't
	// registered. Handler MUST NOT try to write any rows.
	expectSecretLookup(mock, testRepoID, "")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

func TestServeHTTP_invalidUUIDPath_returns401(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{Kind: "push", ToSHA: testToSHA})
	sig := signPayload(t, testSecret, body)

	// The handler doesn't pre-validate UUID shape; it lets the
	// DB cast `$1::uuid` fail and treats the resulting error
	// as "no such repo". Simulate the pq error message that
	// triggers that classification.
	mock.ExpectQuery(`SELECT webhook_secret FROM repo_webhook_secret WHERE repo_id`).
		WithArgs("not-a-uuid").
		WillReturnError(errors.New(`pq: invalid input syntax for type uuid: "not-a-uuid"`))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, "not-a-uuid", sig, body))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusUnauthorized, w.Body.String())
	}
}

// ----- payload-validation scenarios -----------------------------

func TestServeHTTP_invalidKind_returns400_afterAuth(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{
		Kind:  "register", // legitimate enum label, but only valid via mgmt.*
		ToSHA: testToSHA,
	})
	sig := signPayload(t, testSecret, body)

	expectSecretLookup(mock, testRepoID, testSecret)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusBadRequest, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "kind must be one of") {
		t.Errorf("body = %q, want kind-list hint", w.Body.String())
	}
}

func TestServeHTTP_missingToSHA_returns400(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{Kind: "push"})
	sig := signPayload(t, testSecret, body)

	expectSecretLookup(mock, testRepoID, testSecret)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestServeHTTP_invalidJSON_returns400(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := []byte("not-json{")
	sig := signPayload(t, testSecret, body)

	expectSecretLookup(mock, testRepoID, testSecret)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

// ----- request-shape scenarios ----------------------------------

func TestServeHTTP_wrongMethod_returns405(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newMockHandler(t)
	defer cleanup()

	r := httptest.NewRequest(http.MethodGet, RoutePrefix+testRepoID, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusMethodNotAllowed)
	}
	if got := w.Header().Get("Allow"); got != http.MethodPost {
		t.Errorf("Allow header = %q, want %q", got, http.MethodPost)
	}
}

func TestServeHTTP_offRoute_returns404(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newMockHandler(t)
	defer cleanup()

	cases := []string{"/", "/healthz", "/api/whatever", "/webhook"}
	for _, p := range cases {
		r := httptest.NewRequest(http.MethodPost, p, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotFound {
			t.Errorf("path %q: status = %d, want %d", p, w.Code, http.StatusNotFound)
		}
	}
}

func TestServeHTTP_nestedPath_returns401(t *testing.T) {
	t.Parallel()
	h, _, cleanup := newMockHandler(t)
	defer cleanup()

	// `/webhook/<uuid>/foo` is technically under RoutePrefix
	// but has an extra segment; extractRepoID rejects it and
	// the handler returns 401 (uniform unauthenticated reply).
	r := httptest.NewRequest(http.MethodPost,
		RoutePrefix+testRepoID+"/foo", bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

func TestServeHTTP_bodyTooLarge_returns413(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	// Tight cap so we don't allocate megabytes in a unit test.
	h := NewHandler(db, Options{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxBodyBytes: 8,
	})

	body := bytes.Repeat([]byte("a"), 128) // >> the 8-byte cap
	sig := signPayload(t, testSecret, body)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	// http.MaxBytesReader returns *http.MaxBytesError to the
	// caller; the handler is required to translate that into
	// the exact 413 documented in doc.go. Don't accept any
	// other 4xx -- a regression to 400 / 401 would silently
	// mis-shape operator alerting.
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d (StatusRequestEntityTooLarge). body=%q",
			w.Code, http.StatusRequestEntityTooLarge, w.Body.String())
	}
}

// ----- idempotency / dedupe scenario ----------------------------

func TestServeHTTP_duplicatePush_returnsExistingJobID(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{
		Kind:    "push",
		FromSHA: testFromSHA,
		ToSHA:   testToSHA,
	})
	sig := signPayload(t, testSecret, body)

	// Two requests; the second hits the ON CONFLICT path and
	// gets the same job_id back -- repo_event still produces
	// a fresh row (audit log shape).
	for i, eventID := range []string{"event-uuid-A", "event-uuid-B"} {
		expectSecretLookup(mock, testRepoID, testSecret)
		expectEnqueue(mock, testRepoID, testFromSHA, testToSHA, "push",
			eventID, "job-uuid-X" /* same */, "pending")

		w := httptest.NewRecorder()
		h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

		if w.Code != http.StatusAccepted {
			t.Fatalf("iter %d: status = %d, want %d. body=%q", i, w.Code,
				http.StatusAccepted, w.Body.String())
		}
		var resp Response
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("iter %d: decode response: %v", i, err)
		}
		if resp.JobID != "job-uuid-X" {
			t.Errorf("iter %d: job_id = %q, want %q (dedupe broken)",
				i, resp.JobID, "job-uuid-X")
		}
	}
}

// ----- enqueue failure -> 500 ----------------------------------

func TestServeHTTP_dbFailureOnEventInsert_returns500(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{Kind: "push", FromSHA: testFromSHA, ToSHA: testToSHA})
	sig := signPayload(t, testSecret, body)

	expectSecretLookup(mock, testRepoID, testSecret)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO repo_event`).
		WithArgs(testRepoID, "push", testFromSHA, testToSHA).
		WillReturnError(errors.New("db boom"))
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d. body=%q", w.Code, http.StatusInternalServerError, w.Body.String())
	}
	// Ensure the raw error text does NOT leak to the caller.
	if strings.Contains(w.Body.String(), "db boom") {
		t.Errorf("response body leaks raw error: %q", w.Body.String())
	}
}

// TestServeHTTP_dbFailureOnIngestJobsInsert_returns500 covers
// the second leg of the enqueue transaction: the repo_event
// INSERT succeeded but the ingest_jobs upsert returns an error.
// The handler MUST roll back so the audit row does not survive
// without its paired job row, MUST return 500, MUST NOT leak the
// driver-level error text, and MUST NOT emit a success-shaped
// JSON body (event_id / job_id fields).
func TestServeHTTP_dbFailureOnIngestJobsInsert_returns500(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{Kind: "push", FromSHA: testFromSHA, ToSHA: testToSHA})
	sig := signPayload(t, testSecret, body)

	expectSecretLookup(mock, testRepoID, testSecret)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO repo_event`).
		WithArgs(testRepoID, "push", testFromSHA, testToSHA).
		WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow("event-uuid-A"))
	mock.ExpectQuery(`INSERT INTO ingest_jobs`).
		WithArgs(testRepoID, testFromSHA, testToSHA).
		WillReturnError(errors.New("upsert exploded"))
	mock.ExpectRollback()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d. body=%q",
			w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "upsert exploded") {
		t.Errorf("response body leaks raw error: %q", w.Body.String())
	}
	// Critically: the response body must NOT look like the
	// success envelope. A future refactor that accidentally
	// writes the Response struct on the failure path would
	// be caught here.
	if strings.Contains(w.Body.String(), "event-uuid-A") {
		t.Errorf("response leaks pre-rollback event_id: %q", w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"job_id"`) ||
		strings.Contains(w.Body.String(), `"event_id"`) {
		t.Errorf("response shaped like success envelope on failure path: %q", w.Body.String())
	}
}

// TestServeHTTP_dbFailureOnCommit_returns500 covers the final
// leg: both INSERTs succeeded but the COMMIT itself fails (e.g.
// SERIALIZATION_FAILURE, network hiccup, server crash between
// the last write and the commit ACK). The handler MUST return
// 500 and MUST NOT report the (now-rolled-back) row IDs to the
// caller, because the rows do not exist post-rollback.
func TestServeHTTP_dbFailureOnCommit_returns500(t *testing.T) {
	t.Parallel()
	h, mock, cleanup := newMockHandler(t)
	defer cleanup()

	body := mustMarshalJSON(t, Payload{Kind: "push", FromSHA: testFromSHA, ToSHA: testToSHA})
	sig := signPayload(t, testSecret, body)

	expectSecretLookup(mock, testRepoID, testSecret)
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO repo_event`).
		WithArgs(testRepoID, "push", testFromSHA, testToSHA).
		WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow("event-uuid-B"))
	mock.ExpectQuery(`INSERT INTO ingest_jobs`).
		WithArgs(testRepoID, testFromSHA, testToSHA).
		WillReturnRows(sqlmock.NewRows([]string{"job_id", "status"}).
			AddRow("job-uuid-B", "pending"))
	mock.ExpectCommit().WillReturnError(errors.New("commit unavailable"))
	// NOTE: after a failed Commit the transaction is marked
	// done at the database/sql layer; the deferred
	// tx.Rollback() in handler.go returns sql.ErrTxDone
	// WITHOUT issuing a ROLLBACK to the driver. We therefore
	// do NOT queue an ExpectRollback here -- mock would flag
	// it as unmet.

	w := httptest.NewRecorder()
	h.ServeHTTP(w, makeRequest(t, testRepoID, sig, body))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d. body=%q",
			w.Code, http.StatusInternalServerError, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "commit unavailable") {
		t.Errorf("response body leaks raw error: %q", w.Body.String())
	}
	// The row IDs the RETURNING clauses produced are stale --
	// the rows do not exist after the commit failure. The
	// handler MUST NOT advertise them.
	if strings.Contains(w.Body.String(), "event-uuid-B") ||
		strings.Contains(w.Body.String(), "job-uuid-B") {
		t.Errorf("response leaks rolled-back row id: %q", w.Body.String())
	}
}

// ----- pure-function helpers -----------------------------------

func TestExtractRepoID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		path, want string
		ok         bool
	}{
		{RoutePrefix + testRepoID, testRepoID, true},
		{RoutePrefix + testRepoID + "/", testRepoID, true},
		{RoutePrefix + testRepoID + "/foo", "", false},
		{RoutePrefix, "", false},
		{"/not-webhook/" + testRepoID, "", false},
	}
	for _, tc := range cases {
		got, ok := extractRepoID(tc.path)
		if ok != tc.ok || got != tc.want {
			t.Errorf("extractRepoID(%q) = (%q, %v), want (%q, %v)",
				tc.path, got, ok, tc.want, tc.ok)
		}
	}
}

func TestPayloadValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		p       Payload
		wantErr string
	}{
		{"push ok", Payload{Kind: "push", FromSHA: testFromSHA, ToSHA: testToSHA}, ""},
		{"merge ok", Payload{Kind: "merge", FromSHA: testFromSHA, ToSHA: testToSHA}, ""},
		{"push without from_sha ok", Payload{Kind: "push", ToSHA: testToSHA}, ""},
		{"empty kind", Payload{ToSHA: testToSHA}, "kind must be one of"},
		{"register kind", Payload{Kind: "register", ToSHA: testToSHA}, "kind must be one of"},
		{"manual kind", Payload{Kind: "manual", ToSHA: testToSHA}, "kind must be one of"},
		{"missing to_sha", Payload{Kind: "push"}, "to_sha is required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.p.validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("validate: %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validate err = %v, want substring %q", err, tc.wantErr)
			}
		})
	}
}

func TestVerifySignature_constantTimeEqualLengths(t *testing.T) {
	t.Parallel()
	// Belt-and-braces test that an attacker-supplied signature of
	// a DIFFERENT byte length than the real HMAC does not panic
	// or short-circuit early -- hmac.Equal must compare them
	// without revealing the length difference.
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	h := NewHandler(db, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	body := []byte(`{"kind":"push","to_sha":"x"}`)
	expectSecretLookup(mock, testRepoID, testSecret)

	// "sha256=" + 1 byte hex = length-1 HMAC; the real one is 32 bytes.
	ok := h.verifySignature(context.Background(), testRepoID, "sha256=ab", body)
	if ok {
		t.Fatal("verifySignature accepted a 1-byte HMAC; should reject")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet sqlmock expectations: %v", err)
	}
}

// ----- helpers -------------------------------------------------

func mustMarshalJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return b
}

// reHex matches a 64-char lower-case hex string -- handy for
// asserting against UUIDs / SHAs in log output.
var reHex = regexp.MustCompile(`^[0-9a-f]+$`)

func TestSignPayload_helperShape(t *testing.T) {
	// Sanity check on the test helper itself: deterministic
	// hex output, sha256= prefix, 64-char hex tail.
	got := signPayload(t, "k", []byte("v"))
	if !strings.HasPrefix(got, "sha256=") {
		t.Fatalf("signPayload missing prefix: %q", got)
	}
	tail := strings.TrimPrefix(got, "sha256=")
	if len(tail) != 64 || !reHex.MatchString(tail) {
		t.Fatalf("signPayload tail %q is not a 64-char hex string", tail)
	}
	// Direct sanity-check against crypto/hmac.
	mac := hmac.New(sha256.New, []byte("k"))
	_, _ = mac.Write([]byte("v"))
	if want := "sha256=" + hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Fatalf("signPayload = %q, want %q", got, want)
	}
}

// Ensure that the test harness body matches what the handler
// reads from the wire. Belt-and-braces against io.NopCloser /
// http.NoBody quirks in newer net/http versions.
func TestMakeRequest_bodyRoundTrips(t *testing.T) {
	body := []byte(`{"hi":"there"}`)
	r := makeRequest(t, testRepoID, "sha256=00", body)
	got, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body round-trip mismatch: got %q want %q", got, body)
	}
}
