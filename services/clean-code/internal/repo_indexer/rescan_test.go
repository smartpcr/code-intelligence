package repo_indexer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"forge/services/clean-code/internal/repo_indexer"
)

// TestRescanHandler_HappyPathInsertsPendingAndRegistered
// pins the canonical CLI rescan trigger flow:
//
//   - POST /v1/indexer/rescan with a valid JSON body
//   - Indexer dispatches the SAME OnNewSHA path the webhook
//     uses
//   - 200 OK with `{commit_inserted: true, event_inserted: true}`
//   - The in-memory writer materialises one commit with
//     `scan_status=pending` and one `registered` repo_event
func TestRescanHandler_HappyPathInsertsPendingAndRegistered(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandler(idx, nil)

	body := mustMarshal(t, repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         validSHA('a'),
		ParentSHA:   validSHA('b'),
		CommittedAt: fixedCommittedAt(),
		Ref:         "refs/heads/main",
	})
	req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q); want 200", rec.Code, rec.Body.String())
	}
	var resp repo_indexer.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v (body %q)", err, rec.Body.String())
	}
	if !resp.CommitInserted {
		t.Errorf("response.commit_inserted = false; want true")
	}
	if !resp.EventInserted {
		t.Errorf("response.event_inserted = false; want true")
	}

	commits := writer.Commits()
	if len(commits) != 1 {
		t.Fatalf("writer.Commits len = %d; want 1", len(commits))
	}
	if commits[0].ScanStatus != repo_indexer.ScanStatusPending {
		t.Errorf("inserted commit scan_status = %q; want %q", commits[0].ScanStatus, repo_indexer.ScanStatusPending)
	}
	events := writer.Events()
	if len(events) != 1 {
		t.Fatalf("writer.Events len = %d; want 1", len(events))
	}
	if events[0].Kind != "registered" {
		t.Errorf("event kind = %q; want %q", events[0].Kind, "registered")
	}
}

// TestRescanHandler_DuplicateRescanIsNoOp confirms the
// idempotent contract: an operator can re-run the rescan
// command without spawning duplicate commit rows or
// registered events.
func TestRescanHandler_DuplicateRescanIsNoOp(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandler(idx, nil)

	payload := repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         validSHA('c'),
		CommittedAt: fixedCommittedAt(),
	}

	for i := 0; i < 2; i++ {
		body := mustMarshal(t, payload)
		req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d: status = %d (body %q); want 200", i, rec.Code, rec.Body.String())
		}
	}

	if got := len(writer.Commits()); got != 1 {
		t.Errorf("commits = %d; want 1 (rescan must be idempotent)", got)
	}
	if got := len(writer.Events()); got != 1 {
		t.Errorf("events = %d; want 1 (rescan must not re-emit registered)", got)
	}
}

// TestRescanHandler_RejectsNonPOST guards against an
// operator's CLI sending GET / DELETE / PUT.
func TestRescanHandler_RejectsNonPOST(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandler(idx, nil)

	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		method := method
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(method, repo_indexer.RescanPath, nil)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("status = %d; want 405", rec.Code)
			}
		})
	}
}

// TestRescanHandler_RejectsWrongContentType pins the
// Content-Type guard.
func TestRescanHandler_RejectsWrongContentType(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandler(idx, nil)

	body := mustMarshal(t, repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         validSHA('d'),
		CommittedAt: fixedCommittedAt(),
	})
	req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d; want 415", rec.Code)
	}
}

// TestRescanHandler_RejectsMalformedJSON pins the
// JSON-decode guard.
func TestRescanHandler_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandler(idx, nil)

	req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath,
		strings.NewReader(`{not-json`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d; want 400", rec.Code)
	}
}

// TestRescanHandler_RejectsValidationErrorWithStructuredCode
// pins that an invalid SHA produces a 400 with the
// `INVALID_SHA` structured code.
func TestRescanHandler_RejectsValidationErrorWithStructuredCode(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandler(idx, nil)

	body := mustMarshal(t, repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         "not-a-sha",
		CommittedAt: fixedCommittedAt(),
	})
	req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", rec.Code)
	}
	var errBody repo_indexer.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if errBody.Code != "INVALID_SHA" {
		t.Errorf("error.code = %q; want INVALID_SHA", errBody.Code)
	}
}

// TestRescanHandler_WriterFailureSurfacesAs500 pins that a
// writer-side error wraps to a 500 + WRITER_FAILURE code.
func TestRescanHandler_WriterFailureSurfacesAs500(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	writer.FailNext(errors.New("simulated storage outage"))
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandler(idx, nil)

	body := mustMarshal(t, repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         validSHA('e'),
		CommittedAt: fixedCommittedAt(),
	})
	req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", rec.Code)
	}
	var errBody repo_indexer.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if errBody.Code != "WRITER_FAILURE" {
		t.Errorf("error.code = %q; want WRITER_FAILURE", errBody.Code)
	}
}

// TestRescanHandler_PayloadTooLargeRejects pins the 1 MiB
// body-size guard, mirroring the webhook surface.
func TestRescanHandler_PayloadTooLargeRejects(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandler(idx, nil)

	big := make([]byte, repo_indexer.MaxBodyBytes+1024)
	for i := range big {
		big[i] = 'a'
	}
	req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath, bytes.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d; want 413", rec.Code)
	}
}

// TestNewRescanHandler_PanicsOnNilIndexer pins the
// constructor's fail-loud contract.
func TestNewRescanHandler_PanicsOnNilIndexer(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewRescanHandler(nil, nil): want panic; got none")
		}
	}()
	_ = repo_indexer.NewRescanHandler(nil, nil)
}

// TestRescanPath_IsDistinctFromWebhookPath pins that the
// rescan trigger is mounted at a SEPARATE path from the
// Git webhook so operators can rate-limit / authorise the
// two surfaces independently.
func TestRescanPath_IsDistinctFromWebhookPath(t *testing.T) {
	t.Parallel()

	if repo_indexer.RescanPath == repo_indexer.Path {
		t.Errorf("RescanPath (%q) must differ from webhook Path (%q)",
			repo_indexer.RescanPath, repo_indexer.Path)
	}
	// Pin the canonical literal so a rename of the
	// constant value must touch this test (and downstream
	// CLI docs).
	if repo_indexer.RescanPath != "/v1/indexer/rescan" {
		t.Errorf("RescanPath = %q; want /v1/indexer/rescan", repo_indexer.RescanPath)
	}
}

// mustMarshal is a small helper that marshals `v` to JSON
// and fails the test on encoding error. Defined in this
// file (not the shared handler_test.go) so the rescan
// tests stay self-contained.
func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	return body
}

// rescanHMACTestSecret is the canonical test HMAC secret
// reused by every HMAC-enabled rescan test. 32 ASCII bytes
// satisfies the iter-2 minimum-length config guard.
var rescanHMACTestSecret = []byte("rescan-test-hmac-secret-32-bytes!")

// TestNewRescanHandlerWithHMAC_PanicsOnNilIndexer pins the
// HMAC-enabled constructor's fail-loud contract for the
// nil-indexer case.
func TestNewRescanHandlerWithHMAC_PanicsOnNilIndexer(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewRescanHandlerWithHMAC(nil, secret, nil): want panic; got none")
		}
	}()
	_ = repo_indexer.NewRescanHandlerWithHMAC(nil, rescanHMACTestSecret, nil)
}

// TestNewRescanHandlerWithHMAC_PanicsOnEmptySecret pins
// the HMAC-enabled constructor's fail-loud contract for
// the empty-secret case -- a wiring bug that must not
// silently degrade to HMAC-less behaviour.
func TestNewRescanHandlerWithHMAC_PanicsOnEmptySecret(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewRescanHandlerWithHMAC(idx, empty, nil): want panic; got none")
		}
	}()
	_ = repo_indexer.NewRescanHandlerWithHMAC(idx, nil, nil)
}

// TestRescanHandler_HMAC_AcceptsSignedRequest pins that a
// request carrying a valid HMAC-SHA256 signature over the
// body reaches the indexer and INSERTs the canonical pending
// commit + registered repo_event.
func TestRescanHandler_HMAC_AcceptsSignedRequest(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandlerWithHMAC(idx, rescanHMACTestSecret, nil)

	body := mustMarshal(t, repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         validSHA('a'),
		CommittedAt: fixedCommittedAt(),
	})
	sig := repo_indexer.SignHMAC(body, rescanHMACTestSecret)

	req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(repo_indexer.HMACSignatureHeader, sig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %q); want 200", rec.Code, rec.Body.String())
	}
	if got := len(writer.Commits()); got != 1 {
		t.Errorf("commits = %d; want 1", got)
	}
	if got := len(writer.Events()); got != 1 {
		t.Errorf("events = %d; want 1", got)
	}
}

// TestRescanHandler_HMAC_RejectsMissingHeader pins the 401
// path: a request to the HMAC-enabled rescan that omits
// `X-Hub-Signature-256` is rejected with
// `HMAC_MISSING_SIGNATURE` and the writer is NEVER touched.
func TestRescanHandler_HMAC_RejectsMissingHeader(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandlerWithHMAC(idx, rescanHMACTestSecret, nil)

	body := mustMarshal(t, repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         validSHA('b'),
		CommittedAt: fixedCommittedAt(),
	})
	req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// No HMAC header.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	var errBody repo_indexer.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if errBody.Code != "HMAC_MISSING_SIGNATURE" {
		t.Errorf("error.code = %q; want HMAC_MISSING_SIGNATURE", errBody.Code)
	}
	if got := len(writer.Commits()); got != 0 {
		t.Errorf("commits = %d; want 0 (auth must short-circuit before Indexer.OnNewSHA)", got)
	}
}

// TestRescanHandler_HMAC_RejectsTamperedSignature pins the
// canonical signature-mismatch path: a request carrying a
// well-formed `sha256=...` header whose digest does NOT
// match the body is rejected with
// `HMAC_SIGNATURE_MISMATCH`.
func TestRescanHandler_HMAC_RejectsTamperedSignature(t *testing.T) {
	t.Parallel()

	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewRescanHandlerWithHMAC(idx, rescanHMACTestSecret, nil)

	body := mustMarshal(t, repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         validSHA('c'),
		CommittedAt: fixedCommittedAt(),
	})
	// Sign a DIFFERENT body so the digest is well-formed but
	// does not match the request body bytes.
	wrongSig := repo_indexer.SignHMAC([]byte("not the body"), rescanHMACTestSecret)

	req := httptest.NewRequest(http.MethodPost, repo_indexer.RescanPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(repo_indexer.HMACSignatureHeader, wrongSig)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	var errBody repo_indexer.ErrorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &errBody); err != nil {
		t.Fatalf("unmarshal error body: %v", err)
	}
	if errBody.Code != "HMAC_SIGNATURE_MISMATCH" {
		t.Errorf("error.code = %q; want HMAC_SIGNATURE_MISMATCH", errBody.Code)
	}
}
