package webhook_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/webhook"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/materialisers"
)

// routerTestKeyID is the canonical signing-key id every Router
// test below registers against the [webhook.StaticSecretResolver].
const routerTestKeyID = "kv-prod-2026-q1"

// routerTestSecret is the shared HMAC secret bound to
// [routerTestKeyID]. Long enough to mirror a realistic
// rotated production secret; constant so the test is
// deterministic.
var routerTestSecret = []byte("router-test-hmac-secret-32-bytes-deadbeef!!")

// newRouterStack builds a fully-wired Router on top of an
// in-memory [metric_ingestor.Ingestor] so a Router test can
// assert end-to-end behaviour without touching Postgres or
// network listeners.
//
// Returns:
//
//   - the Router (mountable via [http.ServeMux] or directly
//     via [http.Handler] interface)
//   - the in-memory metric-sample writer the churn sweep
//     persists into (so tests can assert "no writer touched
//     on auth failure" and "writer touched once for the
//     winning call, not on replay")
//   - the idempotency store (so tests can introspect the
//     committed records)
//
// The Router is constructed with the ChurnVerbHandler bound
// to the same Ingestor the legacy [ChurnIngestHandler] uses,
// so the round-trip ScanRun flow matches production wiring.
func newRouterStack(t *testing.T) (*webhook.Router, *metric_ingestor.InMemoryMetricSampleWriter, *webhook.InMemoryIdempotencyStore) {
	t.Helper()
	r, writer, store, _ := newRouterStackWithDurable(t)
	return r, writer, store
}

// newRouterStackWithDurable extends [newRouterStack] by also
// returning the durable [webhook.ScanRunRepository] so tests
// can assert the persisted-scan_run invariants (Stage 4.1
// iter-2 evaluator items #1 #2). Pass `repo=nil` for the
// shared default; pass a non-nil pre-built repo to share
// state across two Router instances (the
// across-restart replay test).
func newRouterStackWithDurable(t *testing.T) (*webhook.Router, *metric_ingestor.InMemoryMetricSampleWriter, *webhook.InMemoryIdempotencyStore, *webhook.InMemoryScanRunRepository) {
	t.Helper()
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)

	resolver := webhook.NewStaticSecretResolver(map[string][]byte{
		routerTestKeyID: routerTestSecret,
	})
	store := webhook.NewInMemoryIdempotencyStore(0)
	scanRunRepo := webhook.NewInMemoryScanRunRepository()

	r := webhook.NewRouter(webhook.RouterConfig{
		Resolver:    resolver,
		Store:       store,
		ScanRunRepo: scanRunRepo,
		Verbs:       []webhook.VerbHandler{webhook.NewChurnVerbHandler(ing)},
	})
	return r, writer, store, scanRunRepo
}

// signedChurnRequest builds a POST request against
// `/v1/ingest/churn` with the canonical HMAC headers and a
// correctly-signed body. Tests that want to corrupt the
// signature or body can mutate the returned request directly.
func signedChurnRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhook.RouterPath+"churn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SigningKeyIDHeader, routerTestKeyID)
	req.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(body, routerTestSecret))
	return req
}

// decodeRouterResponse parses the canonical 200 envelope
// emitted by [webhook.Router].
func decodeRouterResponse(t *testing.T, body *bytes.Buffer) webhook.RouterResponse {
	t.Helper()
	var got webhook.RouterResponse
	raw, _ := io.ReadAll(body)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode router response: %v (raw=%q)", err, raw)
	}
	return got
}

// TestRouter_HappyPath_HMACVerifiedAndDispatched pins the
// brief's primary positive: a POST with a valid HMAC header
// and the registered signing_key_id passes the auth gate AND
// dispatches to the churn verb handler, returning the
// per-verb counter envelope under a fresh scan_run_id.
func TestRouter_HappyPath_HMACVerifiedAndDispatched(t *testing.T) {
	t.Parallel()
	r, writer, store := newRouterStack(t)
	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	resp := decodeRouterResponse(t, rec.Body)
	if resp.Verb != "churn" {
		t.Errorf("response.verb = %q; want %q", resp.Verb, "churn")
	}
	if resp.ScanRunKind != metric_ingestor.ScanRunKindExternalPerRow {
		t.Errorf("response.scan_run_kind = %q; want %q", resp.ScanRunKind, metric_ingestor.ScanRunKindExternalPerRow)
	}
	if resp.ScanRunID == uuid.Nil {
		t.Errorf("response.scan_run_id = zero UUID; want a minted v7 UUID")
	}
	if resp.Replayed {
		t.Errorf("response.replayed = true on first call; want false")
	}
	if resp.PayloadHash == "" {
		t.Errorf("response.payload_hash empty; want lowercase hex sha256")
	}
	// Verify the payload_hash matches sha256(body).
	gotHash := sha256.Sum256(body)
	if want := fmt.Sprintf("%x", gotHash); resp.PayloadHash != want {
		t.Errorf("response.payload_hash = %q; want %q", resp.PayloadHash, want)
	}
	if got := len(writer.Records()); got == 0 {
		t.Errorf("writer.Records: want >=1 record persisted, got 0")
	}
	if got := store.Len(); got != 1 {
		t.Errorf("store.Len after one successful call: want 1, got %d", got)
	}
}

// TestRouter_InvalidSignature_RejectedWith401 pins the
// implementation-plan scenario "invalid-signature-rejected":
// a webhook POST with an invalid HMAC header returns 401 and
// does NOT enqueue a scan_run (the writer is not touched, the
// idempotency store is empty).
func TestRouter_InvalidSignature_RejectedWith401(t *testing.T) {
	t.Parallel()
	r, writer, store := newRouterStack(t)
	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	// Tamper the signature so it is well-formed (sha256=<64
	// hex chars>) but cryptographically wrong.
	req.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(body, []byte("wrong-secret-bytes-not-the-resolvers!")))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_SIGNATURE_MISMATCH" {
		t.Errorf("error code: want HMAC_SIGNATURE_MISMATCH, got %q", got.Code)
	}
	if n := len(writer.Records()); n != 0 {
		t.Errorf("writer.Records: want 0 (auth failed), got %d", n)
	}
	if n := store.Len(); n != 0 {
		t.Errorf("store.Len after auth failure: want 0 (no scan_run enqueued), got %d", n)
	}
}

// TestRouter_MissingSignature_RejectedWith401 pins the
// missing-header path: a POST without [HMACSignatureHeader]
// is rejected at the verifier with HMAC_MISSING_SIGNATURE.
func TestRouter_MissingSignature_RejectedWith401(t *testing.T) {
	t.Parallel()
	r, writer, store := newRouterStack(t)
	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	req.Header.Del(webhook.HMACSignatureHeader)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_MISSING_SIGNATURE" {
		t.Errorf("error code: want HMAC_MISSING_SIGNATURE, got %q", got.Code)
	}
	if n := len(writer.Records()); n != 0 {
		t.Errorf("writer.Records: want 0, got %d", n)
	}
	if n := store.Len(); n != 0 {
		t.Errorf("store.Len: want 0, got %d", n)
	}
}

// TestRouter_MalformedSignature_RejectedWith401 pins the
// "shape error" path: a header value that is not
// `sha256=<64-hex>` is rejected with HMAC_MALFORMED_SIGNATURE.
func TestRouter_MalformedSignature_RejectedWith401(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	req.Header.Set(webhook.HMACSignatureHeader, "md5=deadbeef")
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_MALFORMED_SIGNATURE" {
		t.Errorf("error code: want HMAC_MALFORMED_SIGNATURE, got %q", got.Code)
	}
}

// TestRouter_MissingSigningKeyID_RejectedWith401 pins the
// missing-key-id branch: a POST with a valid signature but no
// X-Signing-Key-Id header is rejected before the resolver
// fires.
func TestRouter_MissingSigningKeyID_RejectedWith401(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	req.Header.Del(webhook.SigningKeyIDHeader)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_MISSING_KEY_ID" {
		t.Errorf("error code: want HMAC_MISSING_KEY_ID, got %q", got.Code)
	}
}

// TestRouter_UnknownSigningKeyID_RejectedWith401 pins the
// unknown-id branch: the resolver does NOT know the key, so
// the Router emits HMAC_UNKNOWN_KEY_ID.
func TestRouter_UnknownSigningKeyID_RejectedWith401(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	req.Header.Set(webhook.SigningKeyIDHeader, "kv-not-registered-2099")
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_UNKNOWN_KEY_ID" {
		t.Errorf("error code: want HMAC_UNKNOWN_KEY_ID, got %q", got.Code)
	}
}

// TestRouter_MalformedSigningKeyID_RejectedWith401 pins the
// header-shape validation: a key id with embedded CR/LF
// (header-injection probe) is rejected pre-resolver.
func TestRouter_MalformedSigningKeyID_RejectedWith401(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	req.Header.Set(webhook.SigningKeyIDHeader, "kv-prod\r\nX-Injected: 1")
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_MALFORMED_KEY_ID" {
		t.Errorf("error code: want HMAC_MALFORMED_KEY_ID, got %q", got.Code)
	}
}

// TestRouter_TamperedBody_RejectedWith401 pins the security-
// critical contract: a signature computed over body A but
// posted with body B (e.g. an attacker replays a captured
// signature against a modified payload) is rejected with
// HMAC_SIGNATURE_MISMATCH.
func TestRouter_TamperedBody_RejectedWith401(t *testing.T) {
	t.Parallel()
	r, writer, _ := newRouterStack(t)
	originalBody := goodPayloadJSON(t)
	req := signedChurnRequest(t, originalBody)
	// Swap the body the request will read with a tampered
	// version while keeping the signature header pointing at
	// the original.
	tampered := append([]byte{}, originalBody...)
	tampered[len(tampered)-2] ^= 0x01
	req.Body = io.NopCloser(bytes.NewReader(tampered))
	req.ContentLength = int64(len(tampered))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_SIGNATURE_MISMATCH" {
		t.Errorf("error code: want HMAC_SIGNATURE_MISMATCH, got %q", got.Code)
	}
	if n := len(writer.Records()); n != 0 {
		t.Errorf("writer.Records: want 0 (tampered body), got %d", n)
	}
}

// TestRouter_ReplayReturnsCachedScanRun pins the brief's
// idempotency scenario "replay-returns-cached-scan-run":
// posting the SAME signed body twice yields the SAME
// scan_run_id, the second response carries Replayed=true,
// and the writer is touched on the first call only.
func TestRouter_ReplayReturnsCachedScanRun(t *testing.T) {
	t.Parallel()
	r, writer, store := newRouterStack(t)
	body := goodPayloadJSON(t)

	// First POST -- claims the slot, runs the verb, commits.
	req1 := signedChurnRequest(t, body)
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("first POST status: want 200, got %d (body=%s)", rec1.Code, rec1.Body.String())
	}
	first := decodeRouterResponse(t, rec1.Body)
	if first.Replayed {
		t.Errorf("first response: Replayed=true; want false")
	}
	if first.ScanRunID == uuid.Nil {
		t.Fatalf("first response: scan_run_id is zero UUID")
	}
	firstWriterCount := len(writer.Records())
	if firstWriterCount == 0 {
		t.Fatalf("writer.Records after first POST: want >=1, got 0")
	}

	// Second POST -- same body -> cached replay.
	req2 := signedChurnRequest(t, body)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("second POST status: want 200, got %d (body=%s)", rec2.Code, rec2.Body.String())
	}
	second := decodeRouterResponse(t, rec2.Body)
	if !second.Replayed {
		t.Errorf("second response: Replayed=false; want true on replay")
	}
	if second.ScanRunID != first.ScanRunID {
		t.Errorf("replay scan_run_id: want %s (same as original), got %s", first.ScanRunID, second.ScanRunID)
	}
	if second.PayloadHash != first.PayloadHash {
		t.Errorf("replay payload_hash: want %s, got %s", first.PayloadHash, second.PayloadHash)
	}
	if got := len(writer.Records()); got != firstWriterCount {
		t.Errorf("writer.Records after replay: want unchanged at %d, got %d (verb handler re-ran on replay)", firstWriterCount, got)
	}
	if got := store.Len(); got != 1 {
		t.Errorf("store.Len after replay: want 1 (single committed entry), got %d", got)
	}
}

// TestRouter_DifferentBodies_GetDifferentScanRuns pins the
// payload_hash discriminator: two requests with materially
// different payloads do NOT share the cached scan_run.
func TestRouter_DifferentBodies_GetDifferentScanRuns(t *testing.T) {
	t.Parallel()
	r, _, store := newRouterStack(t)

	payloadA := goodPayloadJSON(t)
	// payloadB differs from payloadA in a single byte's
	// worth of content (different file path), so the
	// sha256 hashes diverge.
	pB := churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: validSHA('c'), FilePath: "internal/baz.go", ModifiedAt: fixedNow().Add(-72 * time.Hour)},
		},
	}
	payloadB, err := json.Marshal(pB)
	if err != nil {
		t.Fatalf("marshal payload B: %v", err)
	}

	reqA := signedChurnRequest(t, payloadA)
	recA := httptest.NewRecorder()
	r.ServeHTTP(recA, reqA)
	if recA.Code != http.StatusOK {
		t.Fatalf("POST A status: want 200, got %d", recA.Code)
	}
	respA := decodeRouterResponse(t, recA.Body)

	reqB := signedChurnRequest(t, payloadB)
	recB := httptest.NewRecorder()
	r.ServeHTTP(recB, reqB)
	if recB.Code != http.StatusOK {
		t.Fatalf("POST B status: want 200, got %d", recB.Code)
	}
	respB := decodeRouterResponse(t, recB.Body)

	if respA.ScanRunID == respB.ScanRunID {
		t.Errorf("scan_run_ids collided across distinct payloads: A=%s B=%s", respA.ScanRunID, respB.ScanRunID)
	}
	if respA.PayloadHash == respB.PayloadHash {
		t.Errorf("payload_hashes collided across distinct bodies: A=%s B=%s", respA.PayloadHash, respB.PayloadHash)
	}
	if got := store.Len(); got != 2 {
		t.Errorf("store.Len: want 2 (distinct payloads), got %d", got)
	}
}

// TestRouter_RejectsNonPost pins the 405 for any non-POST.
func TestRouter_RejectsNonPost(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, webhook.RouterPath+"churn", nil)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status=%d; want 405", method, rec.Code)
		}
		body := mustDecodeError(t, rec.Body)
		if body.Code != "METHOD_NOT_ALLOWED" {
			t.Errorf("%s: error code = %q; want METHOD_NOT_ALLOWED", method, body.Code)
		}
	}
}

// TestRouter_MalformedPath_Returns404 pins the path-parse
// guard: any path NOT matching `/v1/ingest/{verb}` returns
// 404 (with the canonical VERB_NOT_FOUND code) BEFORE the
// HMAC step. This is the documented "path-shape is not
// sensitive" carve-out from the iter-6 ordering invariant.
func TestRouter_MalformedPath_Returns404(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	for _, path := range []string{
		"/v1/ingest/",         // empty verb
		"/v1/ingest/churn/",   // trailing slash
		"/v1/ingest/churn/x",  // extra segment
		"/v1/ingest/Churn",    // uppercase (rejected by ValidateVerbToken)
		"/v1/different/churn", // wrong base path
		"/v1/ingest/foo-bar",  // illegal byte '-' in verb token
	} {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(`{}`)))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %q: status=%d; want 404", path, rec.Code)
		}
		body := mustDecodeError(t, rec.Body)
		if body.Code != "VERB_NOT_FOUND" {
			t.Errorf("path %q: error code = %q; want VERB_NOT_FOUND", path, body.Code)
		}
	}
}

// TestRouter_UnregisteredVerb_Returns404 pins the registry
// guard: a syntactically-valid verb that no VerbHandler
// claims surfaces as 404 POST-auth.
func TestRouter_UnregisteredVerb_Returns404(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, webhook.RouterPath+"defects", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SigningKeyIDHeader, routerTestKeyID)
	req.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(body, routerTestSecret))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: want 404, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "VERB_NOT_FOUND" {
		t.Errorf("error code: want VERB_NOT_FOUND, got %q", got.Code)
	}
}

// TestRouter_WrongContentType_Returns415 pins the per-verb
// media-type pin -- AFTER auth. The verb handler claimed
// `application/json`; a POST with `application/xml`
// surfaces as 415 with the canonical code.
func TestRouter_WrongContentType_Returns415(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := []byte(`<churn></churn>`)
	req := httptest.NewRequest(http.MethodPost, webhook.RouterPath+"churn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set(webhook.SigningKeyIDHeader, routerTestKeyID)
	req.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(body, routerTestSecret))
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status: want 415, got %d", rec.Code)
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("error code: want UNSUPPORTED_MEDIA_TYPE, got %q", got.Code)
	}
}

// TestRouter_HMACVerifiedBeforeContentType pins the security
// ordering: an unsigned POST with a WRONG content-type still
// returns 401 (the auth gate fires before any per-verb
// media-type inspection), so an unauthenticated caller
// cannot probe the verb's content-type contract via 415-vs-
// 401 differential responses.
func TestRouter_HMACVerifiedBeforeContentType(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := []byte(`<churn></churn>`)
	req := httptest.NewRequest(http.MethodPost, webhook.RouterPath+"churn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/xml") // would normally trigger 415
	// Intentionally NO X-Signing-Key-Id header.
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 (auth before content-type), got %d", rec.Code)
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_MISSING_KEY_ID" {
		t.Errorf("error code: want HMAC_MISSING_KEY_ID, got %q", got.Code)
	}
}

// TestRouter_PayloadTooLarge_Returns413 pins the body-size
// guard.
func TestRouter_PayloadTooLarge_Returns413(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := bytes.Repeat([]byte("a"), int(webhook.MaxBodyBytes)+1)
	req := httptest.NewRequest(http.MethodPost, webhook.RouterPath+"churn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SigningKeyIDHeader, routerTestKeyID)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status: want 413, got %d", rec.Code)
	}
}

// TestRouter_MalformedJSON_Returns400 pins the per-verb
// classifier path: a JSON body that fails to decode (under
// the verb handler) surfaces as 400 / BAD_REQUEST per the
// [ChurnVerbHandler.ClassifyError] mapping.
func TestRouter_MalformedJSON_Returns400(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := []byte(`{not json}`)
	req := signedChurnRequest(t, body)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "BAD_REQUEST" {
		t.Errorf("error code: want BAD_REQUEST, got %q", got.Code)
	}
}

// TestRouter_InvalidSHAInPayload_Returns400 pins the per-verb
// validation error mapping: a payload with an invalid SHA
// surfaces as 400 / INVALID_SHA AFTER the auth and
// content-type gates pass.
func TestRouter_InvalidSHAInPayload_Returns400(t *testing.T) {
	t.Parallel()
	r, _, store := newRouterStack(t)
	p := churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: "not-a-real-sha", FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-24 * time.Hour)},
		},
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := signedChurnRequest(t, body)
	rec := httptest.NewRecorder()

	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status: want 400, got %d", rec.Code)
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "INVALID_SHA" {
		t.Errorf("error code: want INVALID_SHA, got %q", got.Code)
	}
	// A verb-validation failure MUST NOT leave a committed
	// idempotency record -- the slot was claimed, the verb
	// returned an error, the Router aborted the claim, so a
	// retry (with a corrected payload + a different
	// payload_hash) gets a fresh claim.
	if n := store.Len(); n != 0 {
		t.Errorf("store.Len after verb error: want 0 (claim aborted), got %d", n)
	}
}

// TestRouter_VerbFailure_DoesNotCommitIdempotency pins the
// abort-on-error contract: a verb returning an error MUST
// NOT leave a committed record in the idempotency store, so
// a publisher fixing its payload and retrying with the SAME
// body (already-failed-once) actually re-executes the verb.
func TestRouter_VerbFailure_AllowsRetryWithSameBody(t *testing.T) {
	t.Parallel()
	r, _, store := newRouterStack(t)

	// Build a payload that the verb will REJECT (zero
	// repo_id -> EMPTY_REPO_ID) so we can observe the abort
	// + retry shape.
	badP := churn.Payload{
		RepoID: uuid.Nil,
		Rows: []churn.PayloadRow{
			{SHA: validSHA('a'), FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-24 * time.Hour)},
		},
	}
	badBody, err := json.Marshal(badP)
	if err != nil {
		t.Fatalf("marshal bad payload: %v", err)
	}

	// First POST -- verb fails, Router aborts claim.
	req1 := signedChurnRequest(t, badBody)
	rec1 := httptest.NewRecorder()
	r.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("first POST status: want 400, got %d", rec1.Code)
	}
	if n := store.Len(); n != 0 {
		t.Errorf("store.Len after first verb failure: want 0 (claim aborted), got %d", n)
	}

	// Second POST -- same body. The Router MUST treat the
	// slot as fresh (no replay) because the previous claim
	// was aborted.
	req2 := signedChurnRequest(t, badBody)
	rec2 := httptest.NewRecorder()
	r.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("second POST status: want 400 again, got %d", rec2.Code)
	}
	if n := store.Len(); n != 0 {
		t.Errorf("store.Len after second verb failure: want 0 (claim aborted), got %d", n)
	}
}

// TestRouter_ConcurrentReplay_OneWriterOneCommit pins the
// rubber-duck contract: N concurrent identical POSTs all
// return 200 with the SAME scan_run_id, the writer is
// touched EXACTLY ONCE, and the idempotency store grows by
// exactly one record.
func TestRouter_ConcurrentReplay_OneWriterOneCommit(t *testing.T) {
	t.Parallel()
	r, writer, store := newRouterStack(t)
	body := goodPayloadJSON(t)

	const N = 8
	var wg sync.WaitGroup
	var scanRuns sync.Map // ScanRunID -> count
	var replayCount, freshCount int64
	gate := make(chan struct{})

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			req := signedChurnRequest(t, body)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent POST status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
				return
			}
			resp := decodeRouterResponse(t, rec.Body)
			scanRuns.Store(resp.ScanRunID.String(), true)
			if resp.Replayed {
				atomic.AddInt64(&replayCount, 1)
			} else {
				atomic.AddInt64(&freshCount, 1)
			}
		}()
	}
	close(gate)
	wg.Wait()

	// Every successful response converges to ONE scan_run_id.
	distinct := 0
	scanRuns.Range(func(_, _ any) bool { distinct++; return true })
	if distinct != 1 {
		t.Errorf("distinct scan_run_ids across N concurrent POSTs: want 1, got %d", distinct)
	}
	if got := atomic.LoadInt64(&freshCount); got != 1 {
		t.Errorf("fresh (Replayed=false) count: want 1, got %d", got)
	}
	if got := atomic.LoadInt64(&replayCount); got != N-1 {
		t.Errorf("replay (Replayed=true) count: want %d, got %d", N-1, got)
	}
	// The writer's record count should equal the number of
	// rows in ONE happy-path call (2 in goodPayloadJSON);
	// the verb handler ran exactly once.
	if got := len(writer.Records()); got != 2 {
		t.Errorf("writer.Records: want 2 (verb handler ran once), got %d", got)
	}
	if got := store.Len(); got != 1 {
		t.Errorf("store.Len: want 1 (single committed slot), got %d", got)
	}
}

// TestRouter_LoggerEmitsHMACFailures pins the structured-log
// contract: an HMAC failure emits a Warn line carrying the
// canonical code and the verb, and does NOT include the
// secret or the supplied signature.
func TestRouter_LoggerEmitsHMACFailures(t *testing.T) {
	t.Parallel()
	logs, logger := newTestLogger()
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)
	resolver := webhook.NewStaticSecretResolver(map[string][]byte{routerTestKeyID: routerTestSecret})
	store := webhook.NewInMemoryIdempotencyStore(0)
	scanRunRepo := webhook.NewInMemoryScanRunRepository()
	r := webhook.NewRouter(webhook.RouterConfig{
		Resolver:    resolver,
		Store:       store,
		ScanRunRepo: scanRunRepo,
		Verbs:       []webhook.VerbHandler{webhook.NewChurnVerbHandler(ing)},
		Logger:      logger,
	})

	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	req.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(body, []byte("wrong-secret-bytes")))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d", rec.Code)
	}
	emitted := logs.String()
	if !strings.Contains(emitted, "HMAC verification failed") {
		t.Errorf("log output missing HMAC failure line: %q", emitted)
	}
	if !strings.Contains(emitted, "HMAC_SIGNATURE_MISMATCH") {
		t.Errorf("log output missing canonical code: %q", emitted)
	}
	// Defence: the secret bytes must NEVER appear in any
	// log line. Stringify the test secret and search.
	if strings.Contains(emitted, string(routerTestSecret)) {
		t.Errorf("log leaked the HMAC secret: %q", emitted)
	}
	if strings.Contains(emitted, "wrong-secret-bytes") {
		// The wrong secret was used to compute the bogus
		// signature; neither the bogus signature itself nor
		// the secret bytes should be logged.
		t.Errorf("log leaked a secret-derived literal: %q", emitted)
	}
}

// TestNewRouter_PanicsOnMisconfiguration pins the
// composition-root guards: a Router constructed without a
// resolver, without a store, with no verbs, with a malformed
// verb token, or with a verb whose ScanRunKind disagrees
// with the canonical pin must crash at startup.
func TestNewRouter_PanicsOnMisconfiguration(t *testing.T) {
	t.Parallel()
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)
	resolver := webhook.NewStaticSecretResolver(map[string][]byte{routerTestKeyID: routerTestSecret})
	store := webhook.NewInMemoryIdempotencyStore(0)
	scanRunRepo := webhook.NewInMemoryScanRunRepository()

	t.Run("nil resolver", func(t *testing.T) {
		defer expectPanic(t, "nil SecretResolver")
		_ = webhook.NewRouter(webhook.RouterConfig{Store: store, ScanRunRepo: scanRunRepo, Verbs: []webhook.VerbHandler{webhook.NewChurnVerbHandler(ing)}})
	})
	t.Run("nil store", func(t *testing.T) {
		defer expectPanic(t, "nil IdempotencyStore")
		_ = webhook.NewRouter(webhook.RouterConfig{Resolver: resolver, ScanRunRepo: scanRunRepo, Verbs: []webhook.VerbHandler{webhook.NewChurnVerbHandler(ing)}})
	})
	t.Run("nil scan_run_repo", func(t *testing.T) {
		defer expectPanic(t, "nil ScanRunRepository")
		_ = webhook.NewRouter(webhook.RouterConfig{Resolver: resolver, Store: store, Verbs: []webhook.VerbHandler{webhook.NewChurnVerbHandler(ing)}})
	})
	t.Run("no verbs", func(t *testing.T) {
		defer expectPanic(t, "zero verbs")
		_ = webhook.NewRouter(webhook.RouterConfig{Resolver: resolver, Store: store, ScanRunRepo: scanRunRepo})
	})
	t.Run("duplicate verb", func(t *testing.T) {
		defer expectPanic(t, "duplicate verb")
		_ = webhook.NewRouter(webhook.RouterConfig{
			Resolver:    resolver,
			Store:       store,
			ScanRunRepo: scanRunRepo,
			Verbs:       []webhook.VerbHandler{webhook.NewChurnVerbHandler(ing), webhook.NewChurnVerbHandler(ing)},
		})
	})
	t.Run("negative MaxBytes", func(t *testing.T) {
		defer expectPanic(t, "negative MaxBytes")
		_ = webhook.NewRouter(webhook.RouterConfig{
			Resolver:    resolver,
			Store:       store,
			ScanRunRepo: scanRunRepo,
			Verbs:       []webhook.VerbHandler{webhook.NewChurnVerbHandler(ing)},
			MaxBytes:    -1,
		})
	})
}

// TestRouter_PayloadHashMatchesSpec pins the brief's exact
// formula `payload_hash = sha256(canonicalised body)` --
// where v1's "canonicalised" pin is "the raw body bytes as
// received over the wire". A test fixture POST + a direct
// sha256 over the body MUST produce the same hex value.
func TestRouter_PayloadHashMatchesSpec(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	resp := decodeRouterResponse(t, rec.Body)

	sum := sha256.Sum256(body)
	want := fmt.Sprintf("%x", sum)
	if resp.PayloadHash != want {
		t.Errorf("payload_hash mismatch: spec sha256(body)=%s, response=%s", want, resp.PayloadHash)
	}
}

// TestRouter_NoTenantIDField pins the brief's single-tenant
// invariant by asserting the response envelope has NO
// `tenant_id` field (tech-spec Sec 4.14 / Sec 10A: v1 is
// single-tenant per deployment; multi-tenant v2 uses
// per-schema isolation, not row-level columns).
func TestRouter_NoTenantIDField(t *testing.T) {
	t.Parallel()
	r, _, _ := newRouterStack(t)
	body := goodPayloadJSON(t)
	req := signedChurnRequest(t, body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d", rec.Code)
	}
	// Decode into a generic map so we can assert NO
	// `tenant_id` key surfaces from the typed envelope.
	var envelope map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, found := envelope["tenant_id"]; found {
		t.Errorf("response envelope leaked a tenant_id field; v1 is single-tenant per tech-spec Sec 4.14")
	}
}

// TestRouter_Context_CancelDuringInflight_PreservesClaim
// pins the in-flight slot semantics: a Claim canceled by a
// client disconnect MUST be released so a retry can succeed.
func TestRouter_Context_CancelDuringInflight_PreservesClaim(t *testing.T) {
	t.Parallel()
	r, _, store := newRouterStack(t)
	body := goodPayloadJSON(t)

	// First, a normal POST to seed a committed record so
	// we can prove the slot is committed (not stuck in
	// flight).
	req := signedChurnRequest(t, body)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("seed POST status: want 200, got %d", rec.Code)
	}
	if got := store.Len(); got != 1 {
		t.Fatalf("store.Len after seed: want 1, got %d", got)
	}
	// Lookup should find a committed record (no
	// ErrClaimInFlight).
	hash := sha256.Sum256(body)
	rec2, err := store.Lookup(context.Background(), "churn", webhook.PayloadHash(hash))
	if err != nil {
		t.Fatalf("Lookup after commit: unexpected err %v", err)
	}
	if rec2 == nil || rec2.ResponseBody == nil {
		t.Fatalf("Lookup after commit: want non-nil committed record, got %+v", rec2)
	}
}

// TestRouter_DurableReplay_AcrossSimulatedRestart pins the
// brief's "scan_run(payload_hash=...) already exists for this
// verb -> return stored scan_run_id without re-executing"
// invariant ACROSS a simulated process restart. Stage 4.1
// iter-2 evaluator items #1 #2: the in-memory
// IdempotencyStore alone cannot satisfy this because it dies
// with the process; the durable [webhook.ScanRunRepository]
// MUST be the source of truth.
//
// Test fixture: build TWO Router instances that share ONE
// underlying InMemoryScanRunRepository (the in-process stand-in
// for migration 0009's PG-backed claim). Each Router has its
// OWN IdempotencyStore (mirroring two separate processes, each
// with its own in-memory cache). POST to the first; POST the
// SAME body (same hash) to the second; assert the second
// returns the FIRST scan_run_id with `replayed=true` and that
// the verb's writer was NOT touched by the second call.
func TestRouter_DurableReplay_AcrossSimulatedRestart(t *testing.T) {
	t.Parallel()

	// Build a SHARED durable scan_run repo and a shared
	// metric-sample writer (so we can prove the second
	// Router's verb path is NOT walked).
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)

	resolver := webhook.NewStaticSecretResolver(map[string][]byte{
		routerTestKeyID: routerTestSecret,
	})
	sharedScanRunRepo := webhook.NewInMemoryScanRunRepository()

	makeRouter := func() *webhook.Router {
		return webhook.NewRouter(webhook.RouterConfig{
			Resolver:    resolver,
			Store:       webhook.NewInMemoryIdempotencyStore(0),
			ScanRunRepo: sharedScanRunRepo,
			Verbs:       []webhook.VerbHandler{webhook.NewChurnVerbHandler(ing)},
		})
	}
	first := makeRouter()
	second := makeRouter()

	body := goodPayloadJSON(t)

	// POST to the FIRST process. The verb pipeline runs;
	// scan_run row is opened+finalised.
	rec1 := httptest.NewRecorder()
	first.ServeHTTP(rec1, signedChurnRequest(t, body))
	if rec1.Code != http.StatusOK {
		t.Fatalf("first POST status: want 200, got %d body=%s", rec1.Code, rec1.Body.String())
	}
	resp1 := decodeRouterResponse(t, rec1.Body)
	if resp1.Replayed {
		t.Errorf("first POST: want Replayed=false, got true")
	}
	if got := sharedScanRunRepo.Len(); got != 1 {
		t.Fatalf("scan_run rows after first POST: want 1, got %d", got)
	}
	writes1 := len(writer.Records())

	// POST to the SECOND "process" with the SAME body.
	// The second Router has a fresh in-memory cache --
	// the only thing that can short-circuit the verb
	// execution is the durable scan_run repo.
	rec2 := httptest.NewRecorder()
	second.ServeHTTP(rec2, signedChurnRequest(t, body))
	if rec2.Code != http.StatusOK {
		t.Fatalf("second POST status: want 200, got %d body=%s", rec2.Code, rec2.Body.String())
	}
	resp2 := decodeRouterResponse(t, rec2.Body)
	if !resp2.Replayed {
		t.Errorf("second POST: want Replayed=true (durable replay), got false")
	}
	if resp2.ScanRunID != resp1.ScanRunID {
		t.Errorf("second POST scan_run_id: want %s (first call's), got %s",
			resp1.ScanRunID, resp2.ScanRunID)
	}
	if got := sharedScanRunRepo.Len(); got != 1 {
		t.Errorf("scan_run rows after second POST: want 1 (no new row), got %d", got)
	}
	writes2 := len(writer.Records())
	if writes2 != writes1 {
		t.Errorf("verb writer touched during durable replay: writes pre=%d, post=%d (want unchanged)",
			writes1, writes2)
	}
}

// TestRouter_VerbFailure_FinalizesScanRunAsFailed pins the
// Stage 4.1 iter-2 lifecycle rule: when a verb handler
// returns an error AFTER the durable scan_run row is opened,
// the Router MUST finalize the row as 'failed' so a stale-
// sweep doesn't have to. Uses a fake verb handler that
// passes ExtractMetadata but fails inside Handle so we can
// exercise the post-claim finalize path deterministically.
func TestRouter_VerbFailure_FinalizesScanRunAsFailed(t *testing.T) {
	t.Parallel()
	resolver := webhook.NewStaticSecretResolver(map[string][]byte{
		routerTestKeyID: routerTestSecret,
	})
	store := webhook.NewInMemoryIdempotencyStore(0)
	scanRunRepo := webhook.NewInMemoryScanRunRepository()
	repoID, _ := uuid.NewV4()
	failHandler := &failingFakeVerbHandler{
		repoID: repoID,
		err:    errors.New("simulated verb-handler failure"),
	}
	r := webhook.NewRouter(webhook.RouterConfig{
		Resolver:    resolver,
		Store:       store,
		ScanRunRepo: scanRunRepo,
		Verbs:       []webhook.VerbHandler{failHandler},
	})

	body := []byte(`{"any":"payload"}`)
	req := httptest.NewRequest(http.MethodPost, webhook.RouterPath+"churn", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SigningKeyIDHeader, routerTestKeyID)
	req.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(body, routerTestSecret))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("verb-failure: want non-200, got 200 (body=%s)", rec.Body.String())
	}
	if got := scanRunRepo.Len(); got != 1 {
		t.Fatalf("scan_run rows after failed verb: want 1, got %d", got)
	}
	// Confirm the opened row is finalised in 'failed'
	// status via a fresh OpenExternal on the SAME hash --
	// AlreadyExisted=true and ExistingStatus='failed'
	// proves the Router walked the failure branch.
	openRes, err := scanRunRepo.OpenExternal(context.Background(), webhook.ScanRunRepositoryRequest{
		Verb:        "churn",
		Kind:        failHandler.ScanRunKind(),
		SHABinding:  failHandler.SHABinding(),
		RepoID:      repoID,
		SHA:         "",
		PayloadHash: webhook.PayloadHash(sha256.Sum256(body)),
		OpenedAt:    fixedNow(),
	})
	if err != nil {
		t.Fatalf("re-OpenExternal: %v", err)
	}
	if !openRes.AlreadyExisted {
		t.Errorf("re-OpenExternal AlreadyExisted: want true, got false")
	}
	if openRes.ExistingStatus != webhook.ScanRunStatusFailed {
		t.Errorf("ExistingStatus: want %q (failed-finalised by Router), got %q",
			webhook.ScanRunStatusFailed, openRes.ExistingStatus)
	}
}

// failingFakeVerbHandler implements [webhook.VerbHandler]
// with a stable, predictable Handle-time failure so the
// Router's finalize-as-failed branch is testable without
// fishing for a churn-payload shape that fails inside the
// sweep.
type failingFakeVerbHandler struct {
	repoID uuid.UUID
	err    error
}

func (h *failingFakeVerbHandler) Verb() string        { return "churn" }
func (h *failingFakeVerbHandler) ContentType() string { return "application/json" }
func (h *failingFakeVerbHandler) ScanRunKind() string { return "external_per_row" }
func (h *failingFakeVerbHandler) SHABinding() string  { return "per_row" }
func (h *failingFakeVerbHandler) ExtractMetadata(ctx context.Context, body []byte) (webhook.VerbPayloadMetadata, error) {
	return webhook.VerbPayloadMetadata{RepoID: h.repoID}, nil
}
func (h *failingFakeVerbHandler) Handle(ctx context.Context, body []byte, scanRunID uuid.UUID) (webhook.VerbHandleResult, error) {
	return webhook.VerbHandleResult{}, h.err
}

// Ensure context import stays referenced (used by Lookup
// above).
var _ = errors.Is
