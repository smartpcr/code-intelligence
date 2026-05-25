package webhook_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/churn"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/ingest/webhook"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/materialisers"
)

// fixedRepoID is a stable repo_id literal so the deterministic
// AutoMapScopeResolver in scaffold mode mints the same scope_id
// for every test run.
var fixedRepoID = uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555"))

// fixedNow is the deterministic clock the materialiser captures
// for window math; chosen so every payload row is in-window.
func fixedNow() time.Time {
	return time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)
}

// validSHA returns a canonical 40-char hex SHA built from the
// repeated `c` rune. The materialiser's per-row SHA contract
// requires hex only (evaluator iter-4 #3) so test payloads use
// these `aaa...`, `bbb...` patterns.
func validSHA(c byte) string {
	return strings.Repeat(string(c), 40)
}

// newHandlerWithIngestor constructs a [webhook.ChurnIngestHandler]
// bound to a [metric_ingestor.Ingestor] backed by the production
// scaffold pieces ([churn.AutoMapScopeResolver],
// [metric_ingestor.InMemoryMetricSampleWriter],
// [metric_ingestor.NoopFoundationRecipeDispatcher]). Returns the
// handler + the writer so a test can both POST and inspect the
// records persisted by `Ingestor.Run`.
func newHandlerWithIngestor(t *testing.T) (*webhook.ChurnIngestHandler, *metric_ingestor.InMemoryMetricSampleWriter) {
	t.Helper()
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)
	h := webhook.NewChurnIngestHandler(ing, nil)
	return h, writer
}

// goodPayloadJSON returns a serialised churn payload with two
// in-window rows touching two different files. Used as the
// happy-path body for the round-trip test.
func goodPayloadJSON(t *testing.T) []byte {
	t.Helper()
	p := churn.Payload{
		RepoID: fixedRepoID,
		Rows: []churn.PayloadRow{
			{SHA: validSHA('a'), FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-24 * time.Hour)},
			{SHA: validSHA('b'), FilePath: "internal/bar.go", ModifiedAt: fixedNow().Add(-48 * time.Hour)},
		},
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return body
}

// TestChurnWebhook_HappyPath asserts a well-formed POST flows
// END-TO-END through the production wiring:
// HTTP -> Handler -> Ingestor.Run -> ChurnSweep -> Materialiser
// -> InMemoryMetricSampleWriter. The same-ScanRun integration
// is reachable from a real HTTP request (evaluator iter-4
// #1 + #2 structural fix).
func TestChurnWebhook_HappyPath(t *testing.T) {
	t.Parallel()
	h, writer := newHandlerWithIngestor(t)

	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(goodPayloadJSON(t)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	h.ChurnWebhook(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp webhook.Response
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.ScanRunID == uuid.Nil {
		t.Errorf("response.scan_run_id = zero UUID; want a minted v7 UUID")
	}
	if resp.FoundationDispatched {
		t.Errorf("response.foundation_dispatched = true; want false (external_per_row never dispatches foundation)")
	}
	if resp.ChurnSamplesWritten != 2 {
		t.Errorf("response.churn_samples_written = %d; want 2 (two distinct file scopes)", resp.ChurnSamplesWritten)
	}
	if resp.ChurnRowsHydrated != 2 {
		t.Errorf("response.churn_rows_hydrated = %d; want 2", resp.ChurnRowsHydrated)
	}
	records := writer.Records()
	if len(records) != 2 {
		t.Fatalf("writer.Records() len = %d; want 2", len(records))
	}
	// The producer_run_id stamped on the written record MUST
	// equal the scan_run_id returned to the caller (the
	// same-ScanRun invariant).
	for _, r := range records {
		if r.ProducerRunID != resp.ScanRunID {
			t.Errorf("record.ProducerRunID = %s; want %s (same-ScanRun invariant)", r.ProducerRunID, resp.ScanRunID)
		}
		if r.MetricKind != materialisers.MetricKind {
			t.Errorf("record.MetricKind = %q; want %q", r.MetricKind, materialisers.MetricKind)
		}
	}
}

// TestChurnWebhook_RejectsNonPost pins the canonical 405.
func TestChurnWebhook_RejectsNonPost(t *testing.T) {
	t.Parallel()
	h, _ := newHandlerWithIngestor(t)
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		req := httptest.NewRequest(method, webhook.Path, nil)
		rr := httptest.NewRecorder()
		h.ChurnWebhook(rr, req)
		if rr.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status=%d; want 405", method, rr.Code)
		}
		body := mustDecodeError(t, rr.Body)
		if body.Code != "METHOD_NOT_ALLOWED" {
			t.Errorf("%s: error code = %q; want METHOD_NOT_ALLOWED", method, body.Code)
		}
	}
}

// TestChurnWebhook_RejectsNonJSON pins the 415.
func TestChurnWebhook_RejectsNonJSON(t *testing.T) {
	t.Parallel()
	h, _ := newHandlerWithIngestor(t)
	req := httptest.NewRequest(http.MethodPost, webhook.Path, strings.NewReader("not json"))
	req.Header.Set("Content-Type", "text/plain")
	rr := httptest.NewRecorder()
	h.ChurnWebhook(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status=%d; want 415", rr.Code)
	}
	body := mustDecodeError(t, rr.Body)
	if body.Code != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("error code = %q; want UNSUPPORTED_MEDIA_TYPE", body.Code)
	}
}

// TestChurnWebhook_RejectsMalformedJSON pins the 400.
func TestChurnWebhook_RejectsMalformedJSON(t *testing.T) {
	t.Parallel()
	h, _ := newHandlerWithIngestor(t)
	req := httptest.NewRequest(http.MethodPost, webhook.Path, strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ChurnWebhook(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d; want 400", rr.Code)
	}
}

// TestChurnWebhook_RejectsInvalidSHA pins the iter-4 #3 contract:
// a malformed (non-40-hex) SHA in the payload MUST surface as a
// 400 with the canonical INVALID_SHA code so a CI publisher can
// detect the typo without parsing prose.
func TestChurnWebhook_RejectsInvalidSHA(t *testing.T) {
	t.Parallel()
	h, writer := newHandlerWithIngestor(t)

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
	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ChurnWebhook(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d; want 400 (invalid SHA)", rr.Code)
	}
	errBody := mustDecodeError(t, rr.Body)
	if errBody.Code != "INVALID_SHA" {
		t.Errorf("error code = %q; want INVALID_SHA", errBody.Code)
	}
	if got := len(writer.Records()); got != 0 {
		t.Errorf("writer.Records() len = %d; want 0 (validation runs before writer)", got)
	}
}

// TestChurnWebhook_RejectsEmptyRepoID pins the EMPTY_REPO_ID code.
func TestChurnWebhook_RejectsEmptyRepoID(t *testing.T) {
	t.Parallel()
	h, _ := newHandlerWithIngestor(t)

	p := churn.Payload{
		RepoID: uuid.Nil,
		Rows: []churn.PayloadRow{
			{SHA: validSHA('a'), FilePath: "internal/foo.go", ModifiedAt: fixedNow().Add(-24 * time.Hour)},
		},
	}
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ChurnWebhook(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("status=%d; want 400 (zero repo_id)", rr.Code)
	}
	errBody := mustDecodeError(t, rr.Body)
	if errBody.Code != "EMPTY_REPO_ID" {
		t.Errorf("error code = %q; want EMPTY_REPO_ID", errBody.Code)
	}
}

// TestChurnWebhook_PayloadTooLarge pins the 413.
func TestChurnWebhook_PayloadTooLarge(t *testing.T) {
	t.Parallel()
	h, _ := newHandlerWithIngestor(t)
	// One byte past the limit; the body content does not have
	// to be valid JSON because MaxBytesReader trips first.
	body := bytes.Repeat([]byte("a"), webhook.MaxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ChurnWebhook(rr, req)

	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status=%d; want 413", rr.Code)
	}
}

// TestChurnWebhook_WriterFailureSurfacesAs500 wires a custom
// writer that fails on WriteBatch so the handler's
// classifyError mapping for [metric_ingestor.ErrWriterFailure]
// is exercised in the wired path.
func TestChurnWebhook_WriterFailureSurfacesAs500(t *testing.T) {
	t.Parallel()
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	writer.FailNext(errors.New("disk full"))
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)
	h := webhook.NewChurnIngestHandler(ing, nil)

	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(goodPayloadJSON(t)))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	h.ChurnWebhook(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status=%d; want 500 (writer failure)", rr.Code)
	}
	errBody := mustDecodeError(t, rr.Body)
	if errBody.Code != "WRITER_FAILURE" {
		t.Errorf("error code = %q; want WRITER_FAILURE", errBody.Code)
	}
}

// TestNewChurnIngestHandler_PanicsOnNilIngestor pins the
// composition-root wiring guard.
func TestNewChurnIngestHandler_PanicsOnNilIngestor(t *testing.T) {
	t.Parallel()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("NewChurnIngestHandler(nil, nil) did not panic")
		}
	}()
	_ = webhook.NewChurnIngestHandler(nil, nil)
}

// TestChurnWebhook_DeterministicScopeID_RepeatedPostsCollapse
// proves the iter-5 scaffold-resolver swap: two identical POSTs
// produce records with the SAME ScopeID (the active-row
// uniqueness invariant requires identity stability across
// calls). The previous MapScopeResolver required pre-
// registration, so the webhook would fail on the first call;
// the AutoMapScopeResolver mints UUIDv5 deterministically.
func TestChurnWebhook_DeterministicScopeID_RepeatedPostsCollapse(t *testing.T) {
	t.Parallel()
	h, writer := newHandlerWithIngestor(t)
	body := goodPayloadJSON(t)

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		h.ChurnWebhook(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("POST %d: status=%d body=%s", i, rr.Code, rr.Body.String())
		}
	}

	records := writer.Records()
	if len(records) != 4 {
		t.Fatalf("writer.Records() len = %d; want 4 (2 scopes * 2 POSTs)", len(records))
	}
	// Two scopes; each scope's records across the two POSTs MUST
	// share the SAME ScopeID. Otherwise the active-row uniqueness
	// invariant cannot hold across calls.
	byPath := map[string][]uuid.UUID{}
	for _, r := range records {
		byPath[fmt.Sprintf("%s:%s", r.MetricKind, r.ScopeID)] = append(byPath[fmt.Sprintf("%s:%s", r.MetricKind, r.ScopeID)], r.ScopeID)
	}
	// We expect exactly 2 distinct ScopeIDs across the 4
	// records (foo.go and bar.go), each appearing TWICE.
	distinct := map[uuid.UUID]int{}
	for _, r := range records {
		distinct[r.ScopeID]++
	}
	if len(distinct) != 2 {
		t.Errorf("distinct scope IDs = %d; want 2 (foo + bar, deterministic via UUIDv5)", len(distinct))
	}
	for sid, count := range distinct {
		if count != 2 {
			t.Errorf("scope %s appeared %d times; want 2 (identity must be stable across POSTs)", sid, count)
		}
	}
}

// mustDecodeError decodes the JSON ErrorBody from an httptest
// response or fails the test.
func mustDecodeError(t *testing.T, body *bytes.Buffer) webhook.ErrorBody {
	t.Helper()
	var got webhook.ErrorBody
	raw, _ := io.ReadAll(body)
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal ErrorBody: %v (raw=%q)", err, raw)
	}
	return got
}

// Make sure the package imports `context` and `time` are
// used at least once (the helper imports above are real
// production-side imports, but the linter wants every import
// referenced -- a benign asssertion below.).
var _ = context.Background
var _ = time.Now

// hmacTestSecret is the shared secret every HMAC test below
// signs/verifies with. Long enough to mirror a realistic
// rotated production secret; constant so the test is
// deterministic.
var hmacTestSecret = []byte("clean-coded-test-hmac-secret-32-bytes!")

// newHandlerWithHMAC builds a handler that enforces HMAC
// verification on every request, plus the
// [metric_ingestor.InMemoryMetricSampleWriter] backing it so
// tests can assert the writer is NOT touched on auth failure.
func newHandlerWithHMAC(t *testing.T, secret []byte) (*webhook.ChurnIngestHandler, *metric_ingestor.InMemoryMetricSampleWriter) {
	t.Helper()
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)
	h := webhook.NewChurnIngestHandlerWithHMAC(ing, secret, nil)
	return h, writer
}

// TestChurnWebhook_HMAC_ValidSignature pins that a request
// signed under the SAME secret the handler was constructed
// with succeeds end-to-end (writer touched, 200 returned).
// This is the positive case for the iter-6 #2 fix.
func TestChurnWebhook_HMAC_ValidSignature(t *testing.T) {
	t.Parallel()
	h, writer := newHandlerWithHMAC(t, hmacTestSecret)
	body := goodPayloadJSON(t)
	sig := webhook.SignHMAC(body, hmacTestSecret)

	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.HMACSignatureHeader, sig)
	rec := httptest.NewRecorder()

	h.ChurnWebhook(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: want 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	if got := len(writer.Records()); got == 0 {
		t.Fatalf("writer.Records: want >=1 record persisted, got 0")
	}
}

// TestChurnWebhook_HMAC_MissingSignature pins that an HMAC-
// enabled handler rejects a POST that lacks the signature
// header with 401 + HMAC_MISSING_SIGNATURE -- crucially, the
// writer is NOT touched and the JSON decode never runs (the
// content-type and body shape are irrelevant past auth).
func TestChurnWebhook_HMAC_MissingSignature(t *testing.T) {
	t.Parallel()
	h, writer := newHandlerWithHMAC(t, hmacTestSecret)
	body := goodPayloadJSON(t)

	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// Intentionally NO X-Hub-Signature-256 header.
	rec := httptest.NewRecorder()

	h.ChurnWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_MISSING_SIGNATURE" {
		t.Fatalf("error code: want HMAC_MISSING_SIGNATURE, got %q", got.Code)
	}
	if n := len(writer.Records()); n != 0 {
		t.Fatalf("writer.Records: want 0 (auth failed), got %d", n)
	}
}

// TestChurnWebhook_HMAC_InvalidSignature pins that a header
// whose digest is correct hex but mismatches the computed
// digest is rejected with 401 + HMAC_SIGNATURE_MISMATCH.
func TestChurnWebhook_HMAC_InvalidSignature(t *testing.T) {
	t.Parallel()
	h, writer := newHandlerWithHMAC(t, hmacTestSecret)
	body := goodPayloadJSON(t)
	// Sign with the wrong secret -> the digest is well-formed
	// but the verifier rejects it.
	bogusSig := webhook.SignHMAC(body, []byte("wrong-secret-bytes-not-the-handlers!"))

	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.HMACSignatureHeader, bogusSig)
	rec := httptest.NewRecorder()

	h.ChurnWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_SIGNATURE_MISMATCH" {
		t.Fatalf("error code: want HMAC_SIGNATURE_MISMATCH, got %q", got.Code)
	}
	if n := len(writer.Records()); n != 0 {
		t.Fatalf("writer.Records: want 0 (auth failed), got %d", n)
	}
}

// TestChurnWebhook_HMAC_MalformedSignature pins that a header
// without the `sha256=` prefix is rejected with 401 +
// HMAC_MALFORMED_SIGNATURE.
func TestChurnWebhook_HMAC_MalformedSignature(t *testing.T) {
	t.Parallel()
	h, _ := newHandlerWithHMAC(t, hmacTestSecret)
	body := goodPayloadJSON(t)

	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.HMACSignatureHeader, "md5=00000000")
	rec := httptest.NewRecorder()

	h.ChurnWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_MALFORMED_SIGNATURE" {
		t.Fatalf("error code: want HMAC_MALFORMED_SIGNATURE, got %q", got.Code)
	}
}

// TestChurnWebhook_HMAC_TamperedBody pins that signing one
// body and posting a DIFFERENT body (e.g. an attacker
// replays a signature against a modified payload) is rejected
// with 401 + HMAC_SIGNATURE_MISMATCH. The writer is not
// touched even though the SIGNED body would have been a valid
// payload.
func TestChurnWebhook_HMAC_TamperedBody(t *testing.T) {
	t.Parallel()
	h, writer := newHandlerWithHMAC(t, hmacTestSecret)
	signedBody := goodPayloadJSON(t)
	// Sign one body, post another. The handler will compute
	// the digest over `sentBody` and find it doesn't match.
	sig := webhook.SignHMAC(signedBody, hmacTestSecret)
	sentBody := append([]byte{}, signedBody...)
	sentBody[len(sentBody)-2] = sentBody[len(sentBody)-2] ^ 0x01 // flip a bit

	req := httptest.NewRequest(http.MethodPost, webhook.Path, bytes.NewReader(sentBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.HMACSignatureHeader, sig)
	rec := httptest.NewRecorder()

	h.ChurnWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_SIGNATURE_MISMATCH" {
		t.Fatalf("error code: want HMAC_SIGNATURE_MISMATCH, got %q", got.Code)
	}
	if n := len(writer.Records()); n != 0 {
		t.Fatalf("writer.Records: want 0 (auth failed), got %d", n)
	}
}

// TestChurnWebhook_HMAC_VerifiedBeforeContentType pins the
// security-critical ordering invariant: an unauthenticated
// caller cannot probe the Content-Type contract by inspecting
// the difference between a 401 (auth) and a 415 (wrong
// media-type). When HMAC verification is enabled, an
// unsigned request with a WRONG content-type STILL returns
// 401 -- the content-type branch is unreachable pre-auth.
func TestChurnWebhook_HMAC_VerifiedBeforeContentType(t *testing.T) {
	t.Parallel()
	h, _ := newHandlerWithHMAC(t, hmacTestSecret)

	req := httptest.NewRequest(http.MethodPost, webhook.Path, strings.NewReader("not-json"))
	req.Header.Set("Content-Type", "text/plain") // would normally trigger 415
	// No signature -> the verifier short-circuits to 401
	// BEFORE the content-type check runs.
	rec := httptest.NewRecorder()

	h.ChurnWebhook(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: want 401 (auth before content-type), got %d", rec.Code)
	}
	got := mustDecodeError(t, rec.Body)
	if got.Code != "HMAC_MISSING_SIGNATURE" {
		t.Fatalf("error code: want HMAC_MISSING_SIGNATURE, got %q", got.Code)
	}
}

// TestNewChurnIngestHandlerWithHMAC_PanicsOnEmptySecret pins
// the constructor's "no silent fallback to HMAC-less" guard.
// A caller passing nil/empty must crash at startup, not
// silently get an unauthenticated handler.
func TestNewChurnIngestHandlerWithHMAC_PanicsOnEmptySecret(t *testing.T) {
	t.Parallel()
	mat := materialisers.NewMaterialiserWithClock(materialisers.DefaultWindowDays, fixedNow)
	hyd := churn.NewHydrator(churn.NewAutoMapScopeResolver())
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	sweep := metric_ingestor.NewChurnSweep(mat, hyd, writer)
	ing := metric_ingestor.NewIngestor(metric_ingestor.NoopFoundationRecipeDispatcher{}, sweep)
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewChurnIngestHandlerWithHMAC: want panic on empty secret, got none")
		}
	}()
	_ = webhook.NewChurnIngestHandlerWithHMAC(ing, nil, nil)
}

// Ensure unused imports stay used after the HMAC additions.
var _ = errors.Is
var _ = fmt.Sprintf
