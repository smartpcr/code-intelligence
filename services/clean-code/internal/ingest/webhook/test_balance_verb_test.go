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
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/test_balance"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ingest/webhook"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// testBalanceScopeResolver is a deterministic in-memory
// [test_balance.ScopeResolver] so the verb test exercises the
// full writer-side path WITHOUT a PG dependency. Mints
// UUIDv5(repoID + "|" + QualifiedName).
type testBalanceScopeResolver struct{}

var testBalanceVerbResolverNS = uuid.Must(uuid.FromString("a1b2c3d4-0000-0000-0000-000000000003"))

func (testBalanceScopeResolver) ResolveScopeIDs(_ context.Context, repoID uuid.UUID, refs []recipes.ScopeRef, _ string) ([]uuid.UUID, error) {
	out := make([]uuid.UUID, len(refs))
	for i, ref := range refs {
		out[i] = uuid.NewV5(testBalanceVerbResolverNS, repoID.String()+"|"+ref.QualifiedName)
	}
	return out, nil
}

// newTestBalanceVerb constructs a [webhook.TestBalanceVerbHandler]
// backed by an in-memory writer + scope resolver so the verb
// test can both dispatch and inspect persisted records.
func newTestBalanceVerb(t *testing.T) (*webhook.TestBalanceVerbHandler, *metric_ingestor.InMemoryMetricSampleWriter) {
	t.Helper()
	writer := metric_ingestor.NewInMemoryMetricSampleWriter()
	tbw := test_balance.NewWriter(writer, testBalanceScopeResolver{})
	return webhook.NewTestBalanceVerbHandler(tbw), writer
}

// goodTestBalanceMetadata returns the canonical metadata the
// Router-supplied (RepoID, SHA) tuple the verb's Handle
// consumes after ExtractMetadata has already parsed the
// request headers.
func goodTestBalanceMetadata(sha byte) webhook.VerbPayloadMetadata {
	return webhook.VerbPayloadMetadata{
		RepoID: fixedRepoID,
		SHA:    validSHA(sha),
	}
}

// goodTestBalanceBareArrayJSON returns the documented bare
// row-array body shape (`e2e-scenarios.md:648`,
// `implementation-plan.md:396,400`):
// `[{"scope_id":"S1","attempt_count":3,"pass_count":3},
//   {"scope_id":"S2","attempt_count":2,"pass_count":1}]`
func goodTestBalanceBareArrayJSON(t *testing.T) []byte {
	t.Helper()
	rows := test_balance.Payload{
		{ScopeID: "S1", AttemptCount: 3, PassCount: 3},
		{ScopeID: "S2", AttemptCount: 2, PassCount: 1},
	}
	body, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return body
}

// goodHeaders returns the canonical
// (X-Forge-Repo-ID, X-Forge-SHA) header pair the verb's
// ExtractMetadata consumes.
func goodHeaders(sha byte) http.Header {
	h := http.Header{}
	h.Set(webhook.RepoIDHeader, fixedRepoID.String())
	h.Set(webhook.SHAHeader, validSHA(sha))
	return h
}

// TestTestBalanceVerbHandler_Identity pins the canonical
// metadata the Router consumes at registration: the verb is
// `test_balance`, the content-type is `application/json`, the
// scan_run.kind is `external_single`, and the SHA binding is
// `single`.
func TestTestBalanceVerbHandler_Identity(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	if got := h.Verb(); got != "test_balance" {
		t.Errorf("Verb() = %q; want %q", got, "test_balance")
	}
	if got := h.ContentType(); got != "application/json" {
		t.Errorf("ContentType() = %q; want %q", got, "application/json")
	}
	if got := h.ScanRunKind(); got != metric_ingestor.ScanRunKindExternalSingle {
		t.Errorf("ScanRunKind() = %q; want %q", got, metric_ingestor.ScanRunKindExternalSingle)
	}
	if got := h.SHABinding(); got != metric_ingestor.SHABindingSingle {
		t.Errorf("SHABinding() = %q; want %q", got, metric_ingestor.SHABindingSingle)
	}
}

// TestTestBalanceVerbHandler_ExtractMetadata_FromHeaders
// pins the (RepoID, SHA) tuple the Router consumes from the
// REQUEST HEADERS (iter-2 fix: the test_balance body is a
// bare row array; the body MUST NOT carry the (repo, sha)
// metadata).
func TestTestBalanceVerbHandler_ExtractMetadata_FromHeaders(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	md, err := h.ExtractMetadata(context.Background(), goodHeaders('d'), []byte(`[]`))
	if err != nil {
		t.Fatalf("ExtractMetadata: %v", err)
	}
	if md.RepoID != fixedRepoID {
		t.Errorf("RepoID = %s; want %s", md.RepoID, fixedRepoID)
	}
	if md.SHA != validSHA('d') {
		t.Errorf("SHA = %q; want %q", md.SHA, validSHA('d'))
	}
}

// TestTestBalanceVerbHandler_ExtractMetadata_EmptyRepoIDHeader
// pins the 400 path when the publisher forgets X-Forge-Repo-ID.
func TestTestBalanceVerbHandler_ExtractMetadata_EmptyRepoIDHeader(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	hdr := http.Header{}
	hdr.Set(webhook.SHAHeader, validSHA('d'))
	_, err := h.ExtractMetadata(context.Background(), hdr, []byte(`[]`))
	if err == nil {
		t.Fatalf("ExtractMetadata: want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "EMPTY_REPO_ID" {
		t.Errorf("ClassifyError = (%d, %q); want (400, EMPTY_REPO_ID)", status, code)
	}
}

// TestTestBalanceVerbHandler_ExtractMetadata_EmptySHAHeader
// pins the 400 path when the publisher forgets X-Forge-SHA.
func TestTestBalanceVerbHandler_ExtractMetadata_EmptySHAHeader(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	hdr := http.Header{}
	hdr.Set(webhook.RepoIDHeader, fixedRepoID.String())
	_, err := h.ExtractMetadata(context.Background(), hdr, []byte(`[]`))
	if err == nil {
		t.Fatalf("ExtractMetadata: want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "EMPTY_SHA" {
		t.Errorf("ClassifyError = (%d, %q); want (400, EMPTY_SHA)", status, code)
	}
}

// TestTestBalanceVerbHandler_ExtractMetadata_InvalidRepoIDHeader
// pins the 400 path when the X-Forge-Repo-ID header is not a
// valid UUID.
func TestTestBalanceVerbHandler_ExtractMetadata_InvalidRepoIDHeader(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	hdr := http.Header{}
	hdr.Set(webhook.RepoIDHeader, "not-a-uuid")
	hdr.Set(webhook.SHAHeader, validSHA('d'))
	_, err := h.ExtractMetadata(context.Background(), hdr, []byte(`[]`))
	if err == nil {
		t.Fatalf("ExtractMetadata: want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "INVALID_REPO_ID" {
		t.Errorf("ClassifyError = (%d, %q); want (400, INVALID_REPO_ID)", status, code)
	}
}

// TestTestBalanceVerbHandler_ExtractMetadata_InvalidSHAHeader
// pins the 400 path when X-Forge-SHA is non-empty but not a
// 40-char hex string.
func TestTestBalanceVerbHandler_ExtractMetadata_InvalidSHAHeader(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	hdr := http.Header{}
	hdr.Set(webhook.RepoIDHeader, fixedRepoID.String())
	hdr.Set(webhook.SHAHeader, "deadbeef")
	_, err := h.ExtractMetadata(context.Background(), hdr, []byte(`[]`))
	if err == nil {
		t.Fatalf("ExtractMetadata: want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "INVALID_SHA" {
		t.Errorf("ClassifyError = (%d, %q); want (400, INVALID_SHA)", status, code)
	}
}

// TestTestBalanceVerbHandler_HappyPath_AcceptsBareArrayBody
// pins the documented body shape (e2e-scenarios.md:648,
// implementation-plan.md:396,400): the body is a bare JSON
// row-array, NOT an envelope. Iter-1 incorrectly rejected
// this shape; iter-2 accepts it.
func TestTestBalanceVerbHandler_HappyPath_AcceptsBareArrayBody(t *testing.T) {
	t.Parallel()
	h, writer := newTestBalanceVerb(t)
	body := goodTestBalanceBareArrayJSON(t)
	scanRunID := uuid.Must(uuid.NewV7())

	res, err := h.Handle(context.Background(), goodTestBalanceMetadata('d'), body, scanRunID)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.ScanRunID != scanRunID {
		t.Errorf("result.ScanRunID = %s; want %s", res.ScanRunID, scanRunID)
	}
	if res.FoundationDispatched {
		t.Errorf("FoundationDispatched = true; want false (external_single test_balance is store-only)")
	}
	records := writer.Records()
	if len(records) != 2 {
		t.Fatalf("writer.Records: want 2 (S1+S2), got %d", len(records))
	}
	for _, r := range records {
		if r.ProducerRunID != scanRunID {
			t.Errorf("record.ProducerRunID = %s; want %s (same-ScanRun invariant)", r.ProducerRunID, scanRunID)
		}
		if r.MetricKind != test_balance.MetricKind {
			t.Errorf("record.MetricKind = %q; want %q", r.MetricKind, test_balance.MetricKind)
		}
		if r.SHA != validSHA('d') {
			t.Errorf("record.SHA = %q; want %q (header-provided SHA)", r.SHA, validSHA('d'))
		}
		if r.RepoID != fixedRepoID {
			t.Errorf("record.RepoID = %s; want %s (header-provided RepoID)", r.RepoID, fixedRepoID)
		}
	}
	var detail struct {
		SamplesWritten int `json:"test_balance_samples_written"`
		RowsSkipped    int `json:"test_balance_rows_skipped"`
	}
	if err := json.Unmarshal(res.Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v (raw=%q)", err, res.Detail)
	}
	if detail.SamplesWritten != 2 || detail.RowsSkipped != 0 {
		t.Errorf("detail = %+v; want {SamplesWritten:2, RowsSkipped:0}", detail)
	}
}

// TestTestBalanceVerbHandler_Handle_ReportsSkippedRows pins
// the skip-on-zero detail surface: a bare-array body with 1
// valid row + 1 attempt=0 row reports {Samples:1, Skipped:1}.
func TestTestBalanceVerbHandler_Handle_ReportsSkippedRows(t *testing.T) {
	t.Parallel()
	h, writer := newTestBalanceVerb(t)
	body := []byte(`[
		{"scope_id":"alive","attempt_count":4,"pass_count":2},
		{"scope_id":"untested","attempt_count":0,"pass_count":0}
	]`)
	res, err := h.Handle(context.Background(), goodTestBalanceMetadata('e'), body, uuid.Must(uuid.NewV7()))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(writer.Records()); got != 1 {
		t.Errorf("Records = %d; want 1 (skip-on-zero)", got)
	}
	var detail struct {
		SamplesWritten int `json:"test_balance_samples_written"`
		RowsSkipped    int `json:"test_balance_rows_skipped"`
	}
	if err := json.Unmarshal(res.Detail, &detail); err != nil {
		t.Fatalf("decode detail: %v", err)
	}
	if detail.SamplesWritten != 1 || detail.RowsSkipped != 1 {
		t.Errorf("detail = %+v; want {SamplesWritten:1, RowsSkipped:1}", detail)
	}
}

// TestTestBalanceVerbHandler_ClassifyError_KnownSentinels
// pins the per-verb error-to-status mapping the Router
// consumes via [webhook.VerbErrorClassifier].
func TestTestBalanceVerbHandler_ClassifyError_KnownSentinels(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	cases := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   string
	}{
		{"EmptyRows", test_balance.ErrEmptyRows, http.StatusBadRequest, "EMPTY_ROWS"},
		{"EmptyScopeID", test_balance.ErrEmptyScopeID, http.StatusBadRequest, "EMPTY_SCOPE_ID"},
		{"NegativeAttemptCount", test_balance.ErrNegativeAttemptCount, http.StatusBadRequest, "NEGATIVE_ATTEMPT_COUNT"},
		{"NegativePassCount", test_balance.ErrNegativePassCount, http.StatusBadRequest, "NEGATIVE_PASS_COUNT"},
		{"ScopeResolutionFailed", test_balance.ErrScopeResolutionFailed, http.StatusInternalServerError, "SCOPE_RESOLUTION_FAILED"},
		{"ZeroRepoID", metric_ingestor.ErrZeroRepoID, http.StatusBadRequest, "EMPTY_REPO_ID"},
		{"InvalidScanRunKind", metric_ingestor.ErrInvalidScanRunKind, http.StatusInternalServerError, "INVALID_SCAN_RUN_KIND"},
		{"WriterFailure", metric_ingestor.ErrWriterFailure, http.StatusInternalServerError, "WRITER_FAILURE"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			status, code := h.ClassifyError(tc.err)
			if status != tc.wantStatus {
				t.Errorf("status = %d; want %d", status, tc.wantStatus)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q; want %q", code, tc.wantCode)
			}
		})
	}
}

// TestTestBalanceVerbHandler_ClassifyError_DefersUnknownToRouter
// pins the "(0, '')" contract for errors the verb does NOT
// own: the Router falls back to its generic 500.
func TestTestBalanceVerbHandler_ClassifyError_DefersUnknownToRouter(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	status, code := h.ClassifyError(errors.New("a brand-new error type"))
	if status != 0 || code != "" {
		t.Errorf("unknown error: want (0, \"\"), got (%d, %q)", status, code)
	}
}

// TestTestBalanceVerbHandler_RejectsBadJSON pins the
// JSON-decode-failure path. A malformed body returns a
// wrapped sentinel the verb's ClassifyError maps to 400.
func TestTestBalanceVerbHandler_RejectsBadJSON(t *testing.T) {
	t.Parallel()
	h, writer := newTestBalanceVerb(t)
	_, err := h.Handle(context.Background(), goodTestBalanceMetadata('d'), []byte("{not json"), uuid.Must(uuid.NewV7()))
	if err == nil {
		t.Fatalf("Handle: want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "BAD_REQUEST" {
		t.Errorf("ClassifyError(bad JSON) = (%d, %q); want (400, %q)", status, code, "BAD_REQUEST")
	}
	if got := len(writer.Records()); got != 0 {
		t.Errorf("Records = %d; want 0 (decode failed)", got)
	}
}

// TestTestBalanceVerbHandler_RejectsUnknownFields pins the
// DisallowUnknownFields invariant on the row decoder.
func TestTestBalanceVerbHandler_RejectsUnknownFields(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	body := []byte(`[{"scope_id":"S1","attempt_count":1,"pass_count":1,"surprise":42}]`)
	_, err := h.Handle(context.Background(), goodTestBalanceMetadata('d'), body, uuid.Must(uuid.NewV7()))
	if err == nil {
		t.Fatalf("Handle with unknown field: want error, got nil")
	}
	status, _ := h.ClassifyError(err)
	if status != http.StatusBadRequest {
		t.Errorf("ClassifyError(unknown field): status = %d; want 400", status)
	}
}

// TestTestBalanceVerbHandler_RejectsEmptyArray pins the
// `EMPTY_ROWS` contract: an empty `[]` body is a no-op the
// publisher should fix, and surfaces as 400 EMPTY_ROWS.
func TestTestBalanceVerbHandler_RejectsEmptyArray(t *testing.T) {
	t.Parallel()
	h, _ := newTestBalanceVerb(t)
	_, err := h.Handle(context.Background(), goodTestBalanceMetadata('d'), []byte(`[]`), uuid.Must(uuid.NewV7()))
	if err == nil {
		t.Fatalf("Handle([]): want error, got nil")
	}
	status, code := h.ClassifyError(err)
	if status != http.StatusBadRequest || code != "EMPTY_ROWS" {
		t.Errorf("ClassifyError([]) = (%d, %q); want (400, EMPTY_ROWS)", status, code)
	}
}

// signedTestBalanceRequest builds an HTTP POST with the
// canonical (header + body) HMAC signature the Router
// expects for the `test_balance` verb (Stage 4.3 iter-3
// fix: HMAC covers
// `body || 0x00 || normalised RepoID || 0x00 || normalised SHA`
// so an attacker cannot retarget a signed body to a
// different (repo, sha) by swapping headers). The headers
// are set BEFORE the canonical bytes are computed so the
// signature binds the EXACT header values the Router sees.
func signedTestBalanceRequest(t *testing.T, body []byte, repoID, sha string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/ingest/test_balance", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(webhook.SigningKeyIDHeader, routerTestKeyID)
	req.Header.Set(webhook.RepoIDHeader, repoID)
	req.Header.Set(webhook.SHAHeader, sha)
	canonical := webhook.BuildTestBalanceCanonicalRequest(req.Header, body)
	req.Header.Set(webhook.HMACSignatureHeader, webhook.SignHMAC(canonical, routerTestSecret))
	return req
}

// TestRouter_TestBalance_RejectsJUnitXMLBody pins the
// `test-balance-rejects-junit-xml` e2e scenario
// (implementation-plan.md:407): posting a JUnit-XML body to
// `/v1/ingest/test_balance` MUST surface as `415 Unsupported
// Media Type` from the Router's per-verb content-type check.
// The Router enforces the pin BEFORE invoking the verb
// handler -- the verb only declares `ContentType() ==
// "application/json"`; the Router does the matching.
func TestRouter_TestBalance_RejectsJUnitXMLBody(t *testing.T) {
	t.Parallel()
	tbHandler, _ := newTestBalanceVerb(t)
	router := newRouterWithVerbsForTestBalance(t, []webhook.VerbHandler{tbHandler})

	body := []byte(`<?xml version="1.0"?><testsuite tests="3" failures="0"></testsuite>`)
	req := signedTestBalanceRequest(t, body, fixedRepoID.String(), validSHA('x'))
	req.Header.Set("Content-Type", "application/xml")
	// signedTestBalanceRequest above set application/json;
	// overwrite AFTER signing so HMAC still covers the
	// canonical bytes (Content-Type is NOT in canonical).
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d; want %d (415 Unsupported Media Type)", got, http.StatusUnsupportedMediaType)
	}
	var got webhook.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Code != "UNSUPPORTED_MEDIA_TYPE" {
		t.Errorf("error code: want UNSUPPORTED_MEDIA_TYPE, got %q", got.Code)
	}
}

// TestRouter_TestBalance_HappyPath_BareArrayBody_AcceptsAndPersists
// pins the iter-2 contract end-to-end: a Router-routed POST
// with the documented bare-array body + X-Forge-Repo-ID /
// X-Forge-SHA headers + HMAC over canonical bytes passes the
// HMAC + content-type + idempotency gates and lands rows in
// the writer.
func TestRouter_TestBalance_HappyPath_BareArrayBody_AcceptsAndPersists(t *testing.T) {
	t.Parallel()
	tbHandler, writer := newTestBalanceVerb(t)
	router := newRouterWithVerbsForTestBalance(t, []webhook.VerbHandler{tbHandler})

	body := goodTestBalanceBareArrayJSON(t)
	req := signedTestBalanceRequest(t, body, fixedRepoID.String(), validSHA('f'))
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusOK {
		respBody, _ := io.ReadAll(rec.Body)
		t.Fatalf("status = %d; want 200; body=%s", got, respBody)
	}
	records := writer.Records()
	if len(records) != 2 {
		t.Fatalf("writer.Records: want 2, got %d", len(records))
	}
	for _, r := range records {
		if r.SHA != validSHA('f') {
			t.Errorf("record.SHA = %q; want %q", r.SHA, validSHA('f'))
		}
	}
}

// TestRouter_TestBalance_CrossTargetCollision_DistinctScanRuns
// is the Stage 4.3 iter-2 evaluator #1 regression test: two
// POSTs with byte-identical bare-array bodies but DIFFERENT
// `X-Forge-SHA` headers MUST land in DIFFERENT scan_run rows
// (no idempotency replay) and persist BOTH sets of rows.
//
// Before iter-3 the Router computed `payload_hash =
// sha256(body)` only, so the second call replayed the first
// call's response and the second SHA's writes were silently
// dropped. After iter-3 the per-verb [CanonicalRequest]
// folds the (repo, sha) header tuple into the canonical
// bytes so `payload_hash` differs per logical target.
func TestRouter_TestBalance_CrossTargetCollision_DistinctScanRuns(t *testing.T) {
	t.Parallel()
	tbHandler, writer := newTestBalanceVerb(t)
	router := newRouterWithVerbsForTestBalance(t, []webhook.VerbHandler{tbHandler})

	body := goodTestBalanceBareArrayJSON(t)

	send := func(shaByte byte) (*webhook.RouterResponse, *httptest.ResponseRecorder) {
		req := signedTestBalanceRequest(t, body, fixedRepoID.String(), validSHA(shaByte))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			respBody, _ := io.ReadAll(rec.Body)
			t.Fatalf("status = %d (sha=%c); want 200; body=%s", rec.Code, shaByte, respBody)
		}
		resp := decodeRouterResponse(t, rec.Body)
		return &resp, rec
	}
	resp1, _ := send('a')
	resp2, _ := send('b')

	if resp1.ScanRunID == resp2.ScanRunID {
		t.Fatalf("scan_run_id collision: both calls returned %s -- payload_hash did NOT fold header SHA", resp1.ScanRunID)
	}
	if resp1.Replayed || resp2.Replayed {
		t.Errorf("replayed flags = (%v, %v); want (false, false) -- the calls target DIFFERENT (repo, sha) and must NOT replay", resp1.Replayed, resp2.Replayed)
	}
	records := writer.Records()
	if len(records) != 4 {
		t.Fatalf("writer.Records: want 4 (2 calls x 2 rows), got %d -- second call's writes were dropped", len(records))
	}
	var aCount, bCount int
	for _, r := range records {
		switch r.SHA {
		case validSHA('a'):
			aCount++
		case validSHA('b'):
			bCount++
		default:
			t.Errorf("unexpected record SHA: %q", r.SHA)
		}
	}
	if aCount != 2 || bCount != 2 {
		t.Errorf("per-SHA counts = (a=%d, b=%d); want (2, 2)", aCount, bCount)
	}
}

// TestRouter_TestBalance_HMACBindsHeaderTuple is the Stage
// 4.3 iter-2 evaluator #2 regression test: an attacker who
// captures a valid HMAC signature over canonical_A
// (body + header tuple A) MUST NOT be able to replay it
// against canonical_B by swapping the SHA header. The
// recomputed canonical differs so the HMAC verification
// fails with 401 HMAC_SIGNATURE_MISMATCH.
//
// The test signs the canonical bytes for SHA 'a' then
// mutates the SHA header to 'b' BEFORE sending. Under the
// pre-iter-3 body-only HMAC contract this attack would
// succeed (the body unchanged); under iter-3 it fails.
func TestRouter_TestBalance_HMACBindsHeaderTuple(t *testing.T) {
	t.Parallel()
	tbHandler, writer := newTestBalanceVerb(t)
	router := newRouterWithVerbsForTestBalance(t, []webhook.VerbHandler{tbHandler})

	body := goodTestBalanceBareArrayJSON(t)
	// Sign canonical bytes for target (fixedRepoID, sha='a').
	req := signedTestBalanceRequest(t, body, fixedRepoID.String(), validSHA('a'))
	// MITM-style header swap: caller-controlled retarget.
	req.Header.Set(webhook.SHAHeader, validSHA('b'))

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusUnauthorized {
		respBody, _ := io.ReadAll(rec.Body)
		t.Fatalf("status = %d; want 401 (HMAC must bind the header tuple); body=%s", got, respBody)
	}
	var got webhook.ErrorBody
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if got.Code != "HMAC_SIGNATURE_MISMATCH" {
		t.Errorf("error code = %q; want HMAC_SIGNATURE_MISMATCH", got.Code)
	}
	if n := len(writer.Records()); n != 0 {
		t.Errorf("writer.Records = %d; want 0 (HMAC rejected, no writes should land)", n)
	}
}

// TestRouter_TestBalance_PayloadHashFoldsHeaders pins the
// `payload_hash = sha256(canonical)` invariant the iter-3
// fix introduces: the response envelope's `payload_hash`
// MUST equal sha256 over the canonical bytes the publisher
// signed (NOT over the body alone). A test that builds the
// canonical bytes via [webhook.BuildTestBalanceCanonicalRequest]
// and hashes them locally must match what the Router
// echoes.
func TestRouter_TestBalance_PayloadHashFoldsHeaders(t *testing.T) {
	t.Parallel()
	tbHandler, _ := newTestBalanceVerb(t)
	router := newRouterWithVerbsForTestBalance(t, []webhook.VerbHandler{tbHandler})

	body := goodTestBalanceBareArrayJSON(t)
	req := signedTestBalanceRequest(t, body, fixedRepoID.String(), validSHA('c'))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		respBody, _ := io.ReadAll(rec.Body)
		t.Fatalf("status = %d; want 200; body=%s", rec.Code, respBody)
	}
	resp := decodeRouterResponse(t, rec.Body)

	canonical := webhook.BuildTestBalanceCanonicalRequest(req.Header, body)
	wantHash := sha256.Sum256(canonical)
	wantHex := fmt.Sprintf("%x", wantHash)
	if resp.PayloadHash != wantHex {
		t.Errorf("payload_hash mismatch:\n want sha256(canonical) = %s\n got %s -- Router did NOT hash canonical bytes", wantHex, resp.PayloadHash)
	}

	bodyOnly := sha256.Sum256(body)
	bodyOnlyHex := fmt.Sprintf("%x", bodyOnly)
	if resp.PayloadHash == bodyOnlyHex {
		t.Errorf("payload_hash == sha256(body) -- iter-3 fix regressed; header tuple is no longer folded")
	}
}

// TestNewTestBalanceVerbHandler_PanicsOnNilWriter pins the
// wiring guard.
func TestNewTestBalanceVerbHandler_PanicsOnNilWriter(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewTestBalanceVerbHandler(nil) did not panic")
		}
	}()
	_ = webhook.NewTestBalanceVerbHandler(nil)
}

// newRouterWithVerbsForTestBalance builds a minimal Router
// stack wired to the supplied verb handlers, with the same
// HMAC secret resolver + in-memory idempotency / scan_run
// stores used elsewhere in the webhook test suite. Distinct
// from [newRouterStack] (which only wires the churn handler)
// so the test_balance Router-level scenarios don't have to
// drag the churn ingestor's dependencies in.
func newRouterWithVerbsForTestBalance(t *testing.T, verbs []webhook.VerbHandler) *webhook.Router {
	t.Helper()
	resolver := webhook.NewStaticSecretResolver(map[string][]byte{
		routerTestKeyID: routerTestSecret,
	})
	store := webhook.NewInMemoryIdempotencyStore(0)
	scanRunRepo := webhook.NewInMemoryScanRunRepository()
	return webhook.NewRouter(webhook.RouterConfig{
		Resolver:    resolver,
		Store:       store,
		ScanRunRepo: scanRunRepo,
		Verbs:       verbs,
	})
}

// Compile-time assertion the test exercises the
// [webhook.VerbErrorClassifier] interface.
var _ webhook.VerbErrorClassifier = (*webhook.TestBalanceVerbHandler)(nil)

// suppress unused-import warning.
var _ = strings.HasPrefix
