package mgmtapi

// Behavioural unit tests for the Stage 7.2 `mgmt.ingest_spans`
// verb. Driven via httptest.ResponseRecorder + go-sqlmock so the
// full auth -> validate -> repo-exists -> forwarder -> respond
// pipeline runs without a live PostgreSQL or live Span Ingestor.
//
// All test payloads use the CANONICAL OTLP/HTTP JSON shape:
// `{repo_id, resourceSpans:[{resource:{...}, scopeSpans:[{spans:[{traceId,...}]}]}]}`
// with attributes as `[{key, value:{stringValue|intValue|boolValue|...}}]`.
// That matches the wire-format the merged docs require
// (architecture.md §6.2 / §3.3) and that
// `internal/spaningestor/otlphttp.go` already consumes.
//
// The matrix maps 1:1 onto implementation-plan.md Stage 7.2
// test scenarios and the evaluator's iter-1 follow-ups:
//
//   * invalid OTel field rejected  -> missing trace_id -> 400
//       TestIngestSpans_missingTraceID_returns400
//   * outcome field rejected       -> outcome present -> 400
//       TestIngestSpans_outcomeField_returns400_dropsBatch
//       TestIngestSpans_correctedActionField_returns400_dropsBatch
//       TestIngestSpans_correctedActionCamelCase_returns400
//
// Plus the typed-error matrix the brief implies:
//
//   * malformed body / missing repo_id -> 400
//   * unknown repo_id                  -> 404
//   * unwired forwarder                -> 501
//   * Span Ingestor backpressure       -> 503 + Retry-After
//   * other forwarder error            -> 500
//   * happy path                       -> 202 + accepted_spans count
//   * forbidden field detection precise (NOT a substring match
//     on the body bytes; an attribute named "outcome" is allowed)
//   * AnyValue attribute union decoded (string / int / bool /
//     double) and stringified consistently with `otlphttp.go`
//   * 401 on missing auth
//   * 405 on wrong method
//   * metric counter buckets land on the correct status label

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

const (
	testSpansRepoID = "11111111-2222-3333-4444-555555555555"
	// W3C-shaped trace/span ids: 32 / 16 lowercase hex chars,
	// not all zero.
	testTraceID  = "0123456789abcdef0123456789abcdef"
	testSpanID   = "0123456789abcdef"
	testParentID = "fedcba9876543210"
)

// fakeSpanForwarder records what the handler hands it and
// returns the configured error (or nil). Safe for concurrent
// use; the handler is documented as safe for concurrent use so
// tests should not assume single-threaded forwarder calls.
type fakeSpanForwarder struct {
	mu     sync.Mutex
	calls  []fakeForwardCall
	retErr error
}

type fakeForwardCall struct {
	RepoID string
	Spans  []ForwardedSpan
}

func (f *fakeSpanForwarder) ForwardSpans(_ context.Context, repoID string, spans []ForwardedSpan) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := make([]ForwardedSpan, len(spans))
	copy(cp, spans)
	f.calls = append(f.calls, fakeForwardCall{RepoID: repoID, Spans: cp})
	return f.retErr
}

func (f *fakeSpanForwarder) Calls() []fakeForwardCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeForwardCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// newSpansHandler wires a Handler with sqlmock + a fake
// forwarder + a fresh metrics ledger so the test can inspect
// every observable.
func newSpansHandler(t *testing.T, fwd SpanForwarder) (*Handler, sqlmock.Sqlmock, *IngestSpansMetrics, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	metrics := NewIngestSpansMetrics()
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:             silentLogger(),
			SecretGen:          fixedSecretGen(),
			SpanForwarder:      fwd,
			IngestSpansMetrics: metrics,
		},
	)
	return h, mock, metrics, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

func expectSpansLoadRepo(mock sqlmock.Sqlmock, repoID, repoURL, branch, headSHA string) {
	mock.ExpectQuery(`SELECT url, default_branch, current_head_sha\s+FROM repo\s+WHERE repo_id = \$1::uuid`).
		WithArgs(repoID).
		WillReturnRows(sqlmock.NewRows([]string{"url", "default_branch", "current_head_sha"}).
			AddRow(repoURL, branch, headSHA))
}

func expectSpansLoadRepoNotFound(mock sqlmock.Sqlmock, repoID string) {
	mock.ExpectQuery(`SELECT url, default_branch, current_head_sha\s+FROM repo\s+WHERE repo_id = \$1::uuid`).
		WithArgs(repoID).
		WillReturnError(sql.ErrNoRows)
}

// validSpanOTLP returns a canonical OTLP-shaped span object the
// handler accepts. Tests mutate via mergeMap to exercise
// negative paths.
func validSpanOTLP() map[string]any {
	return map[string]any{
		"traceId":           testTraceID,
		"spanId":            testSpanID,
		"parentSpanId":      testParentID,
		"name":              "GET /widgets",
		"startTimeUnixNano": "1700000000000000000",
		"endTimeUnixNano":   "1700000000123456000",
		"attributes": []any{
			map[string]any{"key": "http.method", "value": map[string]any{"stringValue": "GET"}},
			map[string]any{"key": "http.status", "value": map[string]any{"intValue": "200"}},
			map[string]any{"key": "code.function", "value": map[string]any{"stringValue": "GetWidgets"}},
		},
	}
}

func mergeMap(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		if v == nil {
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// otlpBody is a convenience for building the OTLP envelope
// `{repo_id, resourceSpans:[{resource, scopeSpans:[{spans}]}]}`
// around a list of span objects.
func otlpBody(repoID string, spans []map[string]any) map[string]any {
	resourceAttrs := []any{
		map[string]any{
			"key":   "service.name",
			"value": map[string]any{"stringValue": "agent-memory:test"},
		},
	}
	return map[string]any{
		"repo_id": repoID,
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{"attributes": resourceAttrs},
				"scopeSpans": []any{
					map[string]any{"spans": spans},
				},
			},
		},
	}
}

// postSpans builds an authed POST /v1/spans request with `body`
// JSON-encoded.
func postSpans(t *testing.T, body any) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	r := httptest.NewRequest(http.MethodPost, RouteSpans, &buf)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	return r
}

// -----------------------------------------------------------
// Happy path
// -----------------------------------------------------------

func TestIngestSpans_validBatch_returns202_forwardsToIngestor(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, metrics, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		validSpanOTLP(),
		mergeMap(validSpanOTLP(), map[string]any{
			"spanId":       "fedcba9876543210",
			"parentSpanId": testSpanID,
		}),
	})))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	var resp SpanIngestResponse
	mustDecode(t, w.Body.Bytes(), &resp)
	if resp.RepoID != testSpansRepoID {
		t.Errorf("repo_id = %q, want %q", resp.RepoID, testSpansRepoID)
	}
	if resp.AcceptedSpans != 2 {
		t.Errorf("accepted_spans = %d, want 2", resp.AcceptedSpans)
	}
	if resp.Degraded {
		t.Errorf("degraded = true, want false on happy path")
	}

	calls := fwd.Calls()
	if len(calls) != 1 {
		t.Fatalf("forwarder calls = %d, want 1", len(calls))
	}
	if calls[0].RepoID != testSpansRepoID {
		t.Errorf("forwarder repo_id = %q, want %q", calls[0].RepoID, testSpansRepoID)
	}
	if len(calls[0].Spans) != 2 {
		t.Errorf("forwarder span count = %d, want 2", len(calls[0].Spans))
	}
	if calls[0].Spans[0].TraceID != testTraceID {
		t.Errorf("forwarder span[0].trace_id = %q, want %q",
			calls[0].Spans[0].TraceID, testTraceID)
	}
	if calls[0].Spans[0].StartTimeUnixNano != 1700000000000000000 {
		t.Errorf("forwarder span[0].start = %d, want 1700000000000000000",
			calls[0].Spans[0].StartTimeUnixNano)
	}
	// AnyValue attribute decoding: stringValue + intValue
	// should both end up as strings in the resolver view.
	if got := calls[0].Spans[0].Attributes["http.method"]; got != "GET" {
		t.Errorf("attr http.method = %q, want GET", got)
	}
	if got := calls[0].Spans[0].Attributes["http.status"]; got != "200" {
		t.Errorf("attr http.status = %q, want 200 (intValue stringified)", got)
	}

	if got := metrics.Count(IngestSpansStatusAccepted, testSpansRepoID); got != 1 {
		t.Errorf("metric accepted{%s} = %d, want 1", testSpansRepoID, got)
	}
}

// Numbers (not strings) for timestamps -- some OTel SDKs do this.
func TestIngestSpans_numericTimestamps_accepted(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{
			"startTimeUnixNano": float64(1700000000000000000),
			"endTimeUnixNano":   float64(1700000000123456000),
		}),
	})))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	calls := fwd.Calls()
	if len(calls) != 1 || calls[0].Spans[0].StartTimeUnixNano == 0 {
		t.Fatalf("numeric timestamps not parsed: %+v", calls)
	}
}

// AnyValue union: every supported variant decodes to the
// resolver's `map[string]string` shape consistently with
// otlphttp.go's stringifyAnyValue. This is the explicit
// evaluator-item-3 coverage.
func TestIngestSpans_anyValueAttributes_stringified(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	span := mergeMap(validSpanOTLP(), map[string]any{
		"attributes": []any{
			map[string]any{"key": "string_attr", "value": map[string]any{"stringValue": "hello"}},
			map[string]any{"key": "int_attr", "value": map[string]any{"intValue": "42"}},
			map[string]any{"key": "int_attr_raw", "value": map[string]any{"intValue": float64(7)}},
			map[string]any{"key": "bool_attr_true", "value": map[string]any{"boolValue": true}},
			map[string]any{"key": "bool_attr_false", "value": map[string]any{"boolValue": false}},
			map[string]any{"key": "double_attr", "value": map[string]any{"doubleValue": 3.14}},
			map[string]any{"key": "empty_attr", "value": map[string]any{}},
			map[string]any{"key": "bytes_attr", "value": map[string]any{"bytesValue": "AQID"}},
		},
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{span})))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	calls := fwd.Calls()
	if len(calls) != 1 {
		t.Fatalf("forwarder calls = %d, want 1", len(calls))
	}
	attrs := calls[0].Spans[0].Attributes
	want := map[string]string{
		"string_attr":     "hello",
		"int_attr":        "42",
		"int_attr_raw":    "7",
		"bool_attr_true":  "true",
		"bool_attr_false": "false",
		"double_attr":     "3.14",
		"empty_attr":      "",
		"bytes_attr":      "AQID",
	}
	for k, v := range want {
		if got := attrs[k]; got != v {
			t.Errorf("attr %q = %q, want %q", k, got, v)
		}
	}
}

// Spans split across multiple resourceSpans / scopeSpans
// entries are flattened. Verifies the iteration covers the
// full nested OTLP hierarchy, not just the first entry.
func TestIngestSpans_multipleResourceSpansAndScopes_allFlattened(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	span := func(id string) map[string]any {
		return mergeMap(validSpanOTLP(), map[string]any{"spanId": id})
	}
	body := map[string]any{
		"repo_id": testSpansRepoID,
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{"attributes": []any{}},
				"scopeSpans": []any{
					map[string]any{"spans": []map[string]any{span("0000000000000001"), span("0000000000000002")}},
					map[string]any{"spans": []map[string]any{span("0000000000000003")}},
				},
			},
			map[string]any{
				"resource": map[string]any{"attributes": []any{}},
				"scopeSpans": []any{
					map[string]any{"spans": []map[string]any{span("0000000000000004")}},
				},
			},
		},
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, body))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	calls := fwd.Calls()
	if len(calls) != 1 || len(calls[0].Spans) != 4 {
		t.Fatalf("forwarder calls = %+v, want one call of 4 spans", calls)
	}
}

// -----------------------------------------------------------
// §7.2 Scenario 1: missing trace_id -> 400 with "trace_id
// required" + no rows written
// -----------------------------------------------------------

func TestIngestSpans_missingTraceID_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, metrics, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{"traceId": nil}),
	})))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if !env.Error {
		t.Errorf("error=false in envelope: %s", w.Body.String())
	}
	if !strings.Contains(env.Message, "trace_id required") {
		t.Errorf("message = %q, want substring 'trace_id required'", env.Message)
	}
	if len(fwd.Calls()) != 0 {
		t.Errorf("forwarder calls = %d, want 0 on validation failure", len(fwd.Calls()))
	}
	if got := metrics.Count(IngestSpansStatusValidationError, testSpansRepoID); got != 1 {
		t.Errorf("metric validation_error{%s} = %d, want 1", testSpansRepoID, got)
	}
	if got := metrics.Count(IngestSpansStatusAccepted, testSpansRepoID); got != 0 {
		t.Errorf("metric accepted{%s} = %d, want 0 on validation failure", testSpansRepoID, got)
	}
}

func TestIngestSpans_emptyTraceID_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{"traceId": ""}),
	})))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "trace_id required") {
		t.Errorf("body = %q, want 'trace_id required'", w.Body.String())
	}
}

// -----------------------------------------------------------
// §7.2 Scenario 2: outcome / corrected_action field present ->
// 400 with §6.2.2 reference + batch dropped
// -----------------------------------------------------------

func TestIngestSpans_outcomeField_returns400_dropsBatch(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, metrics, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	// First span carries the forbidden top-level outcome field.
	// Second span is clean; entire batch must still be rejected.
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{"outcome": "success"}),
		validSpanOTLP(),
	})))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "forbidden_field" {
		t.Errorf("code = %q, want forbidden_field", env.Code)
	}
	if !strings.Contains(env.Message, "§6.2.2") {
		t.Errorf("message = %q, want §6.2.2 reference", env.Message)
	}
	if !strings.Contains(env.Message, "outcome") {
		t.Errorf("message = %q, want 'outcome' in message", env.Message)
	}
	if len(fwd.Calls()) != 0 {
		t.Errorf("forwarder calls = %d, want 0 on forbidden_field", len(fwd.Calls()))
	}
	if got := metrics.Count(IngestSpansStatusForbiddenField, testSpansRepoID); got != 1 {
		t.Errorf("metric forbidden_field{%s} = %d, want 1", testSpansRepoID, got)
	}
}

func TestIngestSpans_correctedActionField_returns400_dropsBatch(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{
			"corrected_action": map[string]any{"op": "noop"},
		}),
	})))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != "forbidden_field" {
		t.Errorf("code = %q, want forbidden_field", env.Code)
	}
	if !strings.Contains(env.Message, "corrected_action") {
		t.Errorf("message = %q, want 'corrected_action' in message", env.Message)
	}
}

// Coverage: forbidden-field detection also catches the OTel
// lowerCamelCase spelling `correctedAction`, since the canonical
// OTLP wire shape would naturally emit camelCase.
func TestIngestSpans_correctedActionCamelCase_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{
			"correctedAction": "noop",
		}),
	})))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "correctedAction") {
		t.Errorf("body = %q, want 'correctedAction' in message", w.Body.String())
	}
}

// An ATTRIBUTE keyed `outcome` is NOT a top-level outcome field.
// §6.2.2 forbids the SPAN-LEVEL field; an attribute may carry
// any operator-chosen key. This proves the forbidden-key
// detection is precise (no substring match on body bytes).
func TestIngestSpans_attributeNamedOutcome_allowed(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	span := mergeMap(validSpanOTLP(), map[string]any{
		"attributes": []any{
			map[string]any{"key": "outcome", "value": map[string]any{"stringValue": "success"}},
			map[string]any{"key": "corrected_action", "value": map[string]any{"stringValue": "noop"}},
		},
	})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{span})))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. attribute-named-outcome must NOT be rejected. body=%q",
			w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// Other validation surface
// -----------------------------------------------------------

func TestIngestSpans_missingRepoID_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody("", []map[string]any{validSpanOTLP()})))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "repo_id") {
		t.Errorf("body = %q, want 'repo_id'", w.Body.String())
	}
}

func TestIngestSpans_malformedRepoID_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody("not-a-uuid", []map[string]any{validSpanOTLP()})))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestIngestSpans_emptyResourceSpans_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, map[string]any{
		"repo_id":       testSpansRepoID,
		"resourceSpans": []any{},
	}))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "resourceSpans") {
		t.Errorf("body = %q, want 'resourceSpans'", w.Body.String())
	}
}

func TestIngestSpans_emptyScopeSpans_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	// resourceSpans present but no actual span objects.
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{})))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "at least one span required") {
		t.Errorf("body = %q, want 'at least one span required'", w.Body.String())
	}
}

func TestIngestSpans_oversizedBatch_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	spans := make([]map[string]any, IngestSpansMaxBatch+1)
	for i := range spans {
		spans[i] = mergeMap(validSpanOTLP(), map[string]any{
			"spanId": fmt.Sprintf("%016x", i+1),
		})
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, spans)))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "batch_too_large") {
		t.Errorf("body = %q, want 'batch_too_large'", w.Body.String())
	}
}

func TestIngestSpans_invalidTraceIDLength_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{"traceId": "abc"}),
	})))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "32 hex") {
		t.Errorf("body = %q, want '32 hex'", w.Body.String())
	}
}

func TestIngestSpans_nonHexTraceID_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{
			"traceId": strings.Repeat("z", 32),
		}),
	})))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

func TestIngestSpans_allZeroTraceID_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{
			"traceId": strings.Repeat("0", 32),
		}),
	})))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 on all-zero W3C invalid id", w.Code)
	}
}

func TestIngestSpans_endBeforeStart_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{
			"startTimeUnixNano": "1700000000123456000",
			"endTimeUnixNano":   "1700000000000000000",
		}),
	})))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if !strings.Contains(env.Message, ">= start_time_unix_nano") {
		t.Errorf("message = %q, want '>= start_time_unix_nano'", env.Message)
	}
}

func TestIngestSpans_uppercaseTraceID_normalizedToLower(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	upper := strings.ToUpper(testTraceID)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{
		mergeMap(validSpanOTLP(), map[string]any{"traceId": upper}),
	})))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	calls := fwd.Calls()
	if len(calls) != 1 || calls[0].Spans[0].TraceID != strings.ToLower(upper) {
		t.Fatalf("trace_id not lower-cased: %+v", calls)
	}
}

func TestIngestSpans_malformedJSON_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	r := httptest.NewRequest(http.MethodPost, RouteSpans, bytes.NewReader([]byte("{not json")))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// -----------------------------------------------------------
// Repo existence
// -----------------------------------------------------------

func TestIngestSpans_unknownRepo_returns404(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, metrics, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepoNotFound(mock, testSpansRepoID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{validSpanOTLP()})))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if len(fwd.Calls()) != 0 {
		t.Errorf("forwarder calls = %d, want 0 on unknown repo", len(fwd.Calls()))
	}
	if got := metrics.Count(IngestSpansStatusRepoNotFound, testSpansRepoID); got != 1 {
		t.Errorf("metric repo_not_found{%s} = %d, want 1", testSpansRepoID, got)
	}
}

// -----------------------------------------------------------
// Forwarder coverage
// -----------------------------------------------------------

func TestIngestSpans_forwarderUnset_returns501(t *testing.T) {
	t.Parallel()
	// No forwarder supplied.
	h, _, metrics, cleanup := newSpansHandler(t, nil)
	defer cleanup()

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{validSpanOTLP()})))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501. body=%q", w.Code, w.Body.String())
	}
	// The error code MUST be `span_forwarder_unavailable` so
	// the operator-facing string matches the cmd/mgmt-api doc
	// comment and the monitoring runbook key. A drift between
	// the two would mean the alert rules fire on the wrong
	// label and the operator sees a generic 501 with no
	// runbook context.
	var body struct {
		Code string `json:"code"`
	}
	mustDecode(t, w.Body.Bytes(), &body)
	if got, want := body.Code, IngestSpansForwarderUnavailableCode; got != want {
		t.Errorf("error code = %q, want %q", got, want)
	}
	if got, want := body.Code, "span_forwarder_unavailable"; got != want {
		t.Errorf("error code (literal) = %q, want %q", got, want)
	}
	if got := metrics.Count(IngestSpansStatusForwarderUnset, metricUnknownRepo); got != 1 {
		t.Errorf("metric forwarder_unset{unknown} = %d, want 1", got)
	}
}

func TestIngestSpans_backpressure_returns503_withRetryAfter(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{retErr: ErrSpanIngestorBackpressure}
	h, mock, metrics, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{validSpanOTLP()})))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("Retry-After header missing on 503")
	}
	var env ErrorEnvelope
	mustDecode(t, w.Body.Bytes(), &env)
	if env.Code != SpanIngestorBackpressureReason {
		t.Errorf("code = %q, want %q", env.Code, SpanIngestorBackpressureReason)
	}
	if got := metrics.Count(IngestSpansStatusBackpressure, testSpansRepoID); got != 1 {
		t.Errorf("metric backpressure{%s} = %d, want 1", testSpansRepoID, got)
	}
}

func TestIngestSpans_forwarderError_returns500(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{retErr: errors.New("upstream blew up")}
	h, mock, metrics, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, []map[string]any{validSpanOTLP()})))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if got := metrics.Count(IngestSpansStatusForwarderError, testSpansRepoID); got != 1 {
		t.Errorf("metric forwarder_error{%s} = %d, want 1", testSpansRepoID, got)
	}
}

// -----------------------------------------------------------
// Auth / method gating (verifies the spans verb shares the
// existing middleware behaviour)
// -----------------------------------------------------------

func TestIngestSpans_missingAuth_returns401(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	r := httptest.NewRequest(http.MethodPost, RouteSpans, bytes.NewReader([]byte(`{}`)))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body=%q", w.Code, w.Body.String())
	}
	if len(fwd.Calls()) != 0 {
		t.Errorf("forwarder calls = %d, want 0 when auth fails before route", len(fwd.Calls()))
	}
}

func TestIngestSpans_wrongMethod_returns405(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	r := httptest.NewRequest(http.MethodGet, RouteSpans, nil)
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

// -----------------------------------------------------------
// Metrics ledger
// -----------------------------------------------------------

func TestIngestSpansMetrics_Snapshot_independentBuckets(t *testing.T) {
	t.Parallel()
	m := NewIngestSpansMetrics()
	m.Inc(IngestSpansStatusAccepted, "repo-a")
	m.Inc(IngestSpansStatusAccepted, "repo-a")
	m.Inc(IngestSpansStatusAccepted, "repo-b")
	m.Inc(IngestSpansStatusValidationError, "repo-a")

	snap := m.Snapshot()
	if snap[IngestSpansStatusAccepted]["repo-a"] != 2 {
		t.Errorf("accepted{repo-a} = %d, want 2", snap[IngestSpansStatusAccepted]["repo-a"])
	}
	if snap[IngestSpansStatusAccepted]["repo-b"] != 1 {
		t.Errorf("accepted{repo-b} = %d, want 1", snap[IngestSpansStatusAccepted]["repo-b"])
	}
	if snap[IngestSpansStatusValidationError]["repo-a"] != 1 {
		t.Errorf("validation_error{repo-a} = %d, want 1", snap[IngestSpansStatusValidationError]["repo-a"])
	}
}

func TestIngestSpansMetrics_nilReceiverIsNoop(t *testing.T) {
	t.Parallel()
	var m *IngestSpansMetrics
	m.Inc("x", "y")
	if got := m.Count("x", "y"); got != 0 {
		t.Errorf("Count on nil = %d, want 0", got)
	}
	if snap := m.Snapshot(); len(snap) != 0 {
		t.Errorf("Snapshot on nil = %v, want empty", snap)
	}
}

// -----------------------------------------------------------
// Defensive: large body cap.
// -----------------------------------------------------------

func TestIngestSpans_largeBody_underCap_accepted(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	// 100 spans with ~10 KiB padding each ≈ 1 MiB body.
	// Well under the 4 MiB cap; well above the legacy 64 KiB
	// global default.
	pad := strings.Repeat("a", 10*1024)
	spans := make([]map[string]any, 100)
	for i := range spans {
		spans[i] = mergeMap(validSpanOTLP(), map[string]any{
			"spanId": fmt.Sprintf("%016x", i+1),
			"attributes": []any{
				map[string]any{"key": "padding", "value": map[string]any{"stringValue": pad}},
			},
		})
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, otlpBody(testSpansRepoID, spans)))
	if w.Code != http.StatusAccepted {
		body := w.Body.String()
		if len(body) > 200 {
			body = body[:200]
		}
		t.Fatalf("status = %d, want 202. body=%q", w.Code, body)
	}
	if calls := fwd.Calls(); len(calls) != 1 || len(calls[0].Spans) != 100 {
		t.Fatalf("forwarder calls = %+v, want one call of 100 spans", calls)
	}
}

// -----------------------------------------------------------
// repo_id resolution (header / resource-attr / wrapper)
//
// Evaluator iter-2 item #2: a canonical OTLP
// `ExportTraceServiceRequest` from a Collector forwarder MUST
// be accepted as-is by POST /v1/spans even though it has no
// top-level `repo_id` wrapper. The handler resolves the target
// via X-Mgmt-Repo-ID header or `mgmt.repo_id` resource attr.
// -----------------------------------------------------------

// canonicalOTLPNoWrapper builds the exact wire payload a stock
// OTel Collector would POST (no `repo_id` envelope, just
// `resourceSpans`). The supplied resourceAttrs replace the
// default service.name attr so the test can choose how the
// caller is asserting routing.
func canonicalOTLPNoWrapper(resourceAttrs []any, spans []map[string]any) map[string]any {
	return map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{"attributes": resourceAttrs},
				"scopeSpans": []any{
					map[string]any{"spans": spans},
				},
			},
		},
	}
}

func TestIngestSpans_canonicalOTLPNoWrapper_headerRepoID_accepted(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()
	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	body := canonicalOTLPNoWrapper(
		[]any{
			// Real-collector style: only service.name. The
			// header overrides routing without needing a
			// service-name registry entry on the receiver.
			map[string]any{
				"key":   "service.name",
				"value": map[string]any{"stringValue": "checkout-svc"},
			},
		},
		[]map[string]any{validSpanOTLP()},
	)
	req := postSpans(t, body)
	req.Header.Set(MgmtRepoIDHeader, testSpansRepoID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	calls := fwd.Calls()
	if len(calls) != 1 {
		t.Fatalf("forwarder calls = %d, want 1", len(calls))
	}
	if calls[0].RepoID != testSpansRepoID {
		t.Errorf("forwarder repoID = %q, want %q", calls[0].RepoID, testSpansRepoID)
	}
}

func TestIngestSpans_canonicalOTLPNoWrapper_resourceAttrRepoID_accepted(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()
	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	body := canonicalOTLPNoWrapper(
		[]any{
			map[string]any{
				"key":   "service.name",
				"value": map[string]any{"stringValue": "checkout-svc"},
			},
			map[string]any{
				"key":   MgmtRepoIDResourceAttr,
				"value": map[string]any{"stringValue": testSpansRepoID},
			},
		},
		[]map[string]any{validSpanOTLP()},
	)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	calls := fwd.Calls()
	if len(calls) != 1 || calls[0].RepoID != testSpansRepoID {
		t.Fatalf("forwarder calls = %+v, want one call to %s", calls, testSpansRepoID)
	}
}

func TestIngestSpans_repoIDResolution_headerOverridesWrapperAndAttr(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()
	// Header value wins, so the mocked DB lookup is by that
	// repo only.
	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	// Different non-target IDs in the wrapper + attr so we can
	// verify the header takes precedence (a typo in either
	// downstream hook must NOT mis-route).
	otherID := "abcdef01-2345-4789-89ab-cdef01234567"
	body := otlpBody(otherID, []map[string]any{validSpanOTLP()})
	body["resourceSpans"].([]any)[0].(map[string]any)["resource"] = map[string]any{
		"attributes": []any{
			map[string]any{
				"key":   MgmtRepoIDResourceAttr,
				"value": map[string]any{"stringValue": otherID},
			},
		},
	}
	req := postSpans(t, body)
	req.Header.Set(MgmtRepoIDHeader, testSpansRepoID)

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	calls := fwd.Calls()
	if len(calls) != 1 || calls[0].RepoID != testSpansRepoID {
		t.Fatalf("forwarder calls = %+v, want one call to %s (header wins)", calls, testSpansRepoID)
	}
}

func TestIngestSpans_noRepoIDHook_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	// Body without `repo_id` wrapper and without
	// `mgmt.repo_id` attr; no header set.
	body := canonicalOTLPNoWrapper(
		[]any{
			map[string]any{
				"key":   "service.name",
				"value": map[string]any{"stringValue": "checkout-svc"},
			},
		},
		[]map[string]any{validSpanOTLP()},
	)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var decoded struct {
		Code string `json:"code"`
	}
	mustDecode(t, w.Body.Bytes(), &decoded)
	if decoded.Code != IngestSpansRepoIDRequiredCode {
		t.Errorf("error code = %q, want %q", decoded.Code, IngestSpansRepoIDRequiredCode)
	}
}

func TestIngestSpans_repoIDConflictAcrossResourceSpans_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	otherID := "11111111-2222-4333-9444-555555555555"
	// Two ResourceSpans entries with conflicting attrs and
	// no header / wrapper to win first; the handler must
	// reject up front to avoid splitting the batch silently.
	body := map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{"attributes": []any{
					map[string]any{
						"key":   MgmtRepoIDResourceAttr,
						"value": map[string]any{"stringValue": testSpansRepoID},
					},
				}},
				"scopeSpans": []any{map[string]any{
					"spans": []map[string]any{validSpanOTLP()},
				}},
			},
			map[string]any{
				"resource": map[string]any{"attributes": []any{
					map[string]any{
						"key":   MgmtRepoIDResourceAttr,
						"value": map[string]any{"stringValue": otherID},
					},
				}},
				"scopeSpans": []any{map[string]any{
					"spans": []map[string]any{validSpanOTLP()},
				}},
			},
		},
	}

	w := httptest.NewRecorder()
	h.ServeHTTP(w, postSpans(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	var decoded struct {
		Code string `json:"code"`
	}
	mustDecode(t, w.Body.Bytes(), &decoded)
	if decoded.Code != IngestSpansRepoIDConflictCode {
		t.Errorf("error code = %q, want %q", decoded.Code, IngestSpansRepoIDConflictCode)
	}
}

func TestIngestSpans_malformedHeaderRepoID_returns400(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, _, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()

	body := otlpBody(testSpansRepoID, []map[string]any{validSpanOTLP()})
	req := postSpans(t, body)
	req.Header.Set(MgmtRepoIDHeader, "not-a-uuid")

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), MgmtRepoIDHeader) {
		t.Errorf("body = %q, want to mention %q", w.Body.String(), MgmtRepoIDHeader)
	}
}

func TestIngestSpans_uppercaseHeaderRepoID_normalized(t *testing.T) {
	t.Parallel()
	fwd := &fakeSpanForwarder{}
	h, mock, _, cleanup := newSpansHandler(t, fwd)
	defer cleanup()
	expectSpansLoadRepo(mock, testSpansRepoID, testRepoURL, testBranch, testHeadSHA)

	body := canonicalOTLPNoWrapper(
		[]any{
			map[string]any{
				"key":   "service.name",
				"value": map[string]any{"stringValue": "checkout-svc"},
			},
		},
		[]map[string]any{validSpanOTLP()},
	)
	req := postSpans(t, body)
	req.Header.Set(MgmtRepoIDHeader, strings.ToUpper(testSpansRepoID))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	calls := fwd.Calls()
	if len(calls) != 1 || calls[0].RepoID != testSpansRepoID {
		t.Fatalf("forwarder calls = %+v, want one call to %s (lower-cased)", calls, testSpansRepoID)
	}
}
