package repo_indexer_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/repo_indexer"
)

// hmacTestSecret is the shared secret the HMAC-verified
// tests sign and verify against. Pinned at the test layer
// only.
var hmacTestSecret = []byte("repo-indexer-test-secret")

// newHandlerNoHMAC builds a [repo_indexer.WebhookHandler]
// bound to an in-memory writer with HMAC verification
// DISABLED. The composition root MUST use the HMAC-enabled
// constructor in production; tests use this shape to keep
// payload-shape assertions independent of signature crafting.
func newHandlerNoHMAC(t *testing.T) (*repo_indexer.WebhookHandler, *repo_indexer.InMemoryCatalogWriter) {
	t.Helper()
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewWebhookHandler(idx, nil)
	return h, writer
}

// newHandlerWithHMAC builds a [repo_indexer.WebhookHandler]
// with HMAC-SHA256 verification enabled under
// `hmacTestSecret`.
func newHandlerWithHMAC(t *testing.T) (*repo_indexer.WebhookHandler, *repo_indexer.InMemoryCatalogWriter) {
	t.Helper()
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	h := repo_indexer.NewWebhookHandlerWithHMAC(idx, hmacTestSecret, nil)
	return h, writer
}

// goodWebhookPayload returns a JSON-encoded WebhookPayload
// for a happy-path POST.
func goodWebhookPayload(t *testing.T, sha string) []byte {
	t.Helper()
	p := repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         sha,
		ParentSHA:   "",
		CommittedAt: fixedCommittedAt(),
		Ref:         "refs/heads/main",
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

// TestWebhook_HappyPathInsertsPendingAndRegistered pins the
// canonical Stage 3.1 scenario for the webhook surface:
// a well-formed POST INSERTs the commit row and the
// registered event, returns 200 OK with a structured
// envelope.
func TestWebhook_HappyPathInsertsPendingAndRegistered(t *testing.T) {
	h, writer := newHandlerNoHMAC(t)

	body := goodWebhookPayload(t, validSHA('a'))
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d; want 200. body=%s", rr.Code, rr.Body.String())
	}
	var resp repo_indexer.Response
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, rr.Body.String())
	}
	if !resp.CommitInserted {
		t.Errorf("response CommitInserted=false; want true")
	}
	if !resp.EventInserted {
		t.Errorf("response EventInserted=false; want true (first SHA per repo)")
	}

	// Writer-side assertions: commit lands with pending status,
	// registered event lands once.
	commits := writer.Commits()
	if len(commits) != 1 {
		t.Fatalf("Commits=%d; want 1", len(commits))
	}
	if commits[0].ScanStatus != repo_indexer.ScanStatusPending {
		t.Errorf("ScanStatus=%q; want %q", commits[0].ScanStatus, repo_indexer.ScanStatusPending)
	}
	events := writer.Events()
	if len(events) != 1 {
		t.Fatalf("Events=%d; want 1", len(events))
	}
	if events[0].Kind != "registered" {
		t.Errorf("event Kind=%q; want %q", events[0].Kind, "registered")
	}
}

// TestWebhook_DuplicateDeliveryIsNoOp pins the
// duplicate-webhook semantic at the HTTP layer: a re-POST of
// the same body returns 200 with CommitInserted=false
// EventInserted=false and does not append a second row.
func TestWebhook_DuplicateDeliveryIsNoOp(t *testing.T) {
	h, writer := newHandlerNoHMAC(t)
	body := goodWebhookPayload(t, validSHA('a'))

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("call %d: status=%d; want 200", i, rr.Code)
		}
	}

	if got := len(writer.Commits()); got != 1 {
		t.Errorf("Commits=%d; want 1 (duplicate POST must not append)", got)
	}
	if got := len(writer.Events()); got != 1 {
		t.Errorf("Events=%d; want 1 (duplicate POST must not append a second registered)", got)
	}
}

// TestWebhook_RejectsNonPOST pins the method guard.
func TestWebhook_RejectsNonPOST(t *testing.T) {
	h, _ := newHandlerNoHMAC(t)
	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(m, repo_indexer.Path, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("method=%s: status=%d; want 405", m, rr.Code)
		}
	}
}

// TestWebhook_RejectsWrongContentType pins the 415 guard.
func TestWebhook_RejectsWrongContentType(t *testing.T) {
	h, _ := newHandlerNoHMAC(t)
	body := goodWebhookPayload(t, validSHA('a'))
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status=%d; want 415", rr.Code)
	}
	var eb repo_indexer.ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &eb); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if eb.Code != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("code=%q; want UNSUPPORTED_MEDIA_TYPE", eb.Code)
	}
}

// TestWebhook_AcceptsCharsetParam pins the lenient parse:
// `application/json; charset=utf-8` is accepted.
func TestWebhook_AcceptsCharsetParam(t *testing.T) {
	h, _ := newHandlerNoHMAC(t)
	body := goodWebhookPayload(t, validSHA('a'))
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status=%d; want 200", rr.Code)
	}
}

// TestWebhook_RejectsMalformedJSON pins the 400 guard.
func TestWebhook_RejectsMalformedJSON(t *testing.T) {
	h, _ := newHandlerNoHMAC(t)
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d; want 400", rr.Code)
	}
}

// TestWebhook_RejectsUnknownFields pins
// DisallowUnknownFields semantics: a publisher posting an
// extra key gets a structured 400 rather than a silent
// drop. This protects future-self from a publisher sending
// the legacy `register` kind or a misspelled column name.
func TestWebhook_RejectsUnknownFields(t *testing.T) {
	h, _ := newHandlerNoHMAC(t)
	body := []byte(`{"repo_id":"11111111-2222-3333-4444-555555555555","sha":"` + validSHA('a') + `","committed_at":"2026-05-24T12:00:00Z","extra":"oops"}`)
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d; want 400 (extra fields must be rejected)", rr.Code)
	}
}

// TestWebhook_RejectsValidationErrorWithStructuredCode pins
// the canonical code surface for upstream consumers.
func TestWebhook_RejectsValidationErrorWithStructuredCode(t *testing.T) {
	h, _ := newHandlerNoHMAC(t)

	// Missing repo_id -> EMPTY_REPO_ID.
	body := []byte(`{"sha":"` + validSHA('a') + `","committed_at":"2026-05-24T12:00:00Z"}`)
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status=%d; want 400", rr.Code)
	}
	var eb repo_indexer.ErrorBody
	if err := json.Unmarshal(rr.Body.Bytes(), &eb); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if eb.Code != "EMPTY_REPO_ID" {
		t.Errorf("code=%q; want EMPTY_REPO_ID", eb.Code)
	}
}

// TestWebhook_HMACMissingSignatureRejects pins the
// authenticated-only surface: a POST without
// X-Hub-Signature-256 is 401.
func TestWebhook_HMACMissingSignatureRejects(t *testing.T) {
	h, writer := newHandlerWithHMAC(t)
	body := goodWebhookPayload(t, validSHA('a'))
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d; want 401", rr.Code)
	}
	if got := len(writer.Commits()); got != 0 {
		t.Errorf("Commits=%d; want 0 (unauthenticated POST must not touch the writer)", got)
	}
}

// TestWebhook_HMACValidSignatureAccepts pins the
// happy-path authenticated POST.
func TestWebhook_HMACValidSignatureAccepts(t *testing.T) {
	h, writer := newHandlerWithHMAC(t)
	body := goodWebhookPayload(t, validSHA('a'))
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(repo_indexer.HMACSignatureHeader, repo_indexer.SignHMAC(body, hmacTestSecret))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d; want 200 (body=%s)", rr.Code, rr.Body.String())
	}
	if got := len(writer.Commits()); got != 1 {
		t.Errorf("Commits=%d; want 1", got)
	}
}

// TestWebhook_HMACBadSignatureRejects pins the
// signature-mismatch path.
func TestWebhook_HMACBadSignatureRejects(t *testing.T) {
	h, writer := newHandlerWithHMAC(t)
	body := goodWebhookPayload(t, validSHA('a'))
	// Sign a DIFFERENT body so the digest mismatches.
	wrong := repo_indexer.SignHMAC([]byte("different body"), hmacTestSecret)
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(repo_indexer.HMACSignatureHeader, wrong)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d; want 401", rr.Code)
	}
	if got := len(writer.Commits()); got != 0 {
		t.Errorf("Commits=%d; want 0 (invalid-signature POST must not touch the writer)", got)
	}
}

// TestWebhook_PayloadTooLargeRejects pins the 1 MiB ceiling.
func TestWebhook_PayloadTooLargeRejects(t *testing.T) {
	h, _ := newHandlerNoHMAC(t)
	// Build a JSON body larger than MaxBodyBytes by padding
	// the Ref field with junk text.
	pad := strings.Repeat("X", repo_indexer.MaxBodyBytes+1024)
	p := repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         validSHA('a'),
		CommittedAt: fixedCommittedAt(),
		Ref:         pad,
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, repo_indexer.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d; want 413", rr.Code)
	}
}

// TestNewWebhookHandler_PanicsOnNilIndexer pins the
// composition-root safety check.
func TestNewWebhookHandler_PanicsOnNilIndexer(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewWebhookHandler(nil, nil) did not panic")
		}
	}()
	_ = repo_indexer.NewWebhookHandler(nil, nil)
}

// TestNewWebhookHandlerWithHMAC_PanicsOnEmptySecret pins
// the production-only safety check that prevents an HMAC-
// less webhook from being mounted via the wrong constructor.
func TestNewWebhookHandlerWithHMAC_PanicsOnEmptySecret(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("NewWebhookHandlerWithHMAC(_, empty, _) did not panic")
		}
	}()
	writer := repo_indexer.NewInMemoryCatalogWriter()
	idx := repo_indexer.NewIndexer(writer, nil)
	_ = repo_indexer.NewWebhookHandlerWithHMAC(idx, nil, nil)
}

// TestWebhookPayload_RoundTripsCanonicalJSON pins the wire
// contract: a payload marshalled to JSON and back yields an
// identical struct, with the past-tense canonical fields
// preserved.
func TestWebhookPayload_RoundTripsCanonicalJSON(t *testing.T) {
	original := repo_indexer.WebhookPayload{
		RepoID:      fixedRepoID,
		SHA:         validSHA('a'),
		ParentSHA:   validSHA('b'),
		CommittedAt: fixedCommittedAt(),
		Ref:         "refs/heads/main",
	}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got repo_indexer.WebhookPayload
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RepoID != original.RepoID || got.SHA != original.SHA || got.ParentSHA != original.ParentSHA || got.Ref != original.Ref {
		t.Errorf("round-trip mismatch:\n got=%#v\nwant=%#v", got, original)
	}
	// Compare CommittedAt at UTC nanosecond precision.
	if !got.CommittedAt.Equal(original.CommittedAt.In(time.UTC)) {
		t.Errorf("CommittedAt mismatch: got=%v want=%v", got.CommittedAt, original.CommittedAt)
	}
}
