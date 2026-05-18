package mgmtapi

// Behavioural unit tests for the Stage 7.2 `mgmt.ingest_spans`
// HTTP verb. Iter-2 rewrite: the SpansBatch is now atomic
// (one body per Forward call, no per-repo splitting), the
// forwarded body is the ORIGINAL validated bytes (no
// re-serialization), the forbidden-field check runs at
// EVERY level (root/resource/resource-attrs/scope/scope-attrs/
// span/span-attrs), unknown service is a 400 reject (not 202
// with drop count), and a /metrics endpoint exposes the
// mandated `mgmt_ingest_spans_total` counter in Prometheus
// text-format.
//
// Plan-mandated scenarios (implementation-plan.md §7.2):
//   * invalid OTel field rejected (missing trace_id)
//     -> 400 with `trace_id required`, forwarder NOT called,
//        metric `rejected_validation` += 1.
//   * outcome field rejected -> 400 with §6.2.2 reference,
//     batch dropped, metric `rejected_forbidden_field` += 1.
//
// Rubber-duck-mandated extras:
//   * forbidden field at ROOT / RESOURCE / SCOPE levels
//     also rejected.
//   * forbidden attribute key at RESOURCE attributes.
//   * non-string OTLP attribute values (intValue) preserved
//     on the forwarded body byte-for-byte.
//   * unknown service -> 400 unknown_service (not 202 +
//     drop count).
//   * content-type != application/json -> 415.
//   * forwarder called exactly ONCE per POST with the
//     original body bytes.
//   * Prometheus text format is stable, sorted, and well-
//     formed.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// fakeSpanForwarder is a SpanForwarder double that records
// every Forward call. Set `err` to make Forward return that
// error.
type fakeSpanForwarder struct {
	mu    sync.Mutex
	calls []SpansBatch
	err   error
}

func (f *fakeSpanForwarder) Forward(_ context.Context, batch SpansBatch) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Defensive copy so callers can mutate the batch
	// without disturbing the recorded history.
	bodyCopy := append([]byte(nil), batch.Body...)
	f.calls = append(f.calls, SpansBatch{
		Body:        bodyCopy,
		ContentType: batch.ContentType,
	})
	return f.err
}

func (f *fakeSpanForwarder) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSpanForwarder) lastBatch() (SpansBatch, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return SpansBatch{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// staticServiceMap returns a ServiceNameToRepoID closure
// that maps from the provided map; unknown names return "".
func staticServiceMap(m map[string]string) ServiceNameToRepoID {
	return func(name string) string {
		return m[name]
	}
}

// spansTestRig wires a Handler with sqlmock (no DB
// expectations queued; the spans verb does not hit the DB),
// the StaticBearer test verifier, a fake forwarder, the
// supplied service map, and a DefaultSpanMetrics for
// assertion.
type spansTestRig struct {
	h         *Handler
	forwarder *fakeSpanForwarder
	metrics   *DefaultSpanMetrics
	cleanup   func()
}

func newSpansTestHandler(t *testing.T, services map[string]string) spansTestRig {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	fwd := &fakeSpanForwarder{}
	metrics := NewDefaultSpanMetrics()
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:        silentLogger(),
			SpanForwarder: fwd,
			SpanMetrics:   metrics,
			SpanLookup:    staticServiceMap(services),
		},
	)
	return spansTestRig{
		h:         h,
		forwarder: fwd,
		metrics:   metrics,
		cleanup: func() {
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("unexpected sqlmock activity: %v", err)
			}
			_ = db.Close()
		},
	}
}

// authedSpansRequest builds an authed POST /v1/spans request
// with `body` as the raw JSON bytes.
func authedSpansRequest(t *testing.T, body []byte) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, RouteSpans, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	return r
}

// otelSpanJSON renders one valid OTLP/HTTP span JSON object
// with overridable fields.
func otelSpanJSON(t *testing.T, override map[string]any) []byte {
	t.Helper()
	base := map[string]any{
		"traceId":           "11111111111111111111111111111111",
		"spanId":            "2222222222222222",
		"parentSpanId":      "3333333333333333",
		"name":              "GET /api",
		"startTimeUnixNano": "1000",
		"endTimeUnixNano":   "2000",
	}
	for k, v := range override {
		if v == nil {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	b, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal span: %v", err)
	}
	return b
}

// otelPayload wraps `spans` for a single service.name in an
// OTLP/HTTP ExportTraceServiceRequest envelope.
func otelPayload(t *testing.T, serviceName string, spans ...[]byte) []byte {
	t.Helper()
	rawSpans := make([]json.RawMessage, 0, len(spans))
	for _, s := range spans {
		rawSpans = append(rawSpans, s)
	}
	doc := map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{
					"attributes": []any{
						map[string]any{
							"key":   "service.name",
							"value": map[string]any{"stringValue": serviceName},
						},
					},
				},
				"scopeSpans": []any{
					map[string]any{
						"spans": rawSpans,
					},
				},
			},
		},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// otelPayloadMultiResource builds a payload with N resource
// groups, one per (serviceName -> spans) entry.
func otelPayloadMultiResource(t *testing.T, groups map[string][][]byte) []byte {
	t.Helper()
	resources := make([]any, 0, len(groups))
	for service, spans := range groups {
		rawSpans := make([]json.RawMessage, 0, len(spans))
		for _, s := range spans {
			rawSpans = append(rawSpans, s)
		}
		resources = append(resources, map[string]any{
			"resource": map[string]any{
				"attributes": []any{
					map[string]any{
						"key":   "service.name",
						"value": map[string]any{"stringValue": service},
					},
				},
			},
			"scopeSpans": []any{
				map[string]any{
					"spans": rawSpans,
				},
			},
		})
	}
	doc := map[string]any{"resourceSpans": resources}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// -----------------------------------------------------------
// Plan-mandated scenario: invalid OTel field rejected
// -----------------------------------------------------------

func TestIngestSpans_missingTraceID_returns400_metricIncremented(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	bad := otelSpanJSON(t, map[string]any{"traceId": nil})
	body := otelPayload(t, "svc-a", bad)

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "trace_id required") {
		t.Errorf("body = %q, want substring 'trace_id required'", w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0 (batch must be dropped)", rig.forwarder.callCount())
	}
	// Validation failures attribute to repo_id="" since
	// we short-circuited before resolving the service.
	if got := rig.metrics.Snapshot()[""][SpanStatusRejectedValidation]; got != 1 {
		t.Errorf("metric (empty)/rejected_validation = %d, want 1; full=%+v",
			got, rig.metrics.Snapshot())
	}
}

func TestIngestSpans_emptyTraceIDString_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	bad := otelSpanJSON(t, map[string]any{"traceId": ""})
	body := otelPayload(t, "svc-a", bad)

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "trace_id required") {
		t.Errorf("body = %q, want substring 'trace_id required'", w.Body.String())
	}
}

// -----------------------------------------------------------
// Plan-mandated scenario: outcome field rejected
// -----------------------------------------------------------

func TestIngestSpans_outcomeField_returns400_with622Reference(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	bad := otelSpanJSON(t, map[string]any{"outcome": "success"})
	body := otelPayload(t, "svc-a", bad)

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	bodyStr := w.Body.String()
	if !strings.Contains(bodyStr, "§6.2.2") {
		t.Errorf("body = %q, want substring '§6.2.2'", bodyStr)
	}
	if !strings.Contains(bodyStr, "outcome") {
		t.Errorf("body = %q, want substring 'outcome'", bodyStr)
	}
	if !strings.Contains(bodyStr, "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", bodyStr)
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0 (batch must be dropped)", rig.forwarder.callCount())
	}
	if got := rig.metrics.Snapshot()[""][SpanStatusRejectedForbiddenField]; got != 1 {
		t.Errorf("metric (empty)/rejected_forbidden_field = %d, want 1; full=%+v",
			got, rig.metrics.Snapshot())
	}
}

func TestIngestSpans_correctedActionField_returns400_with622Reference(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	bad := otelSpanJSON(t, map[string]any{"corrected_action": "retry"})
	body := otelPayload(t, "svc-a", bad)

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "§6.2.2") {
		t.Errorf("body = %q, want substring '§6.2.2'", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "corrected_action") {
		t.Errorf("body = %q, want substring 'corrected_action'", w.Body.String())
	}
}

// Forbidden span ATTRIBUTE key (vs. top-level field).
func TestIngestSpans_outcomeAttribute_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	bad := otelSpanJSON(t, map[string]any{
		"attributes": []any{
			map[string]any{
				"key":   "outcome",
				"value": map[string]any{"stringValue": "success"},
			},
		},
	})
	body := otelPayload(t, "svc-a", bad)

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "§6.2.2") {
		t.Errorf("body = %q, want substring '§6.2.2'", w.Body.String())
	}
}

// -----------------------------------------------------------
// Rubber-duck #4 / evaluator #5: forbidden field MUST be
// caught at root, resource, resource-attrs, scope, and
// scope-attrs levels too (not only at span/span-attrs).
// -----------------------------------------------------------

func TestIngestSpans_outcomeAtRootLevel_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()
	// Build a payload with a root-level `outcome` key.
	doc := map[string]any{
		"outcome": "success",
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{
					"attributes": []any{
						map[string]any{
							"key":   "service.name",
							"value": map[string]any{"stringValue": "svc-a"},
						},
					},
				},
				"scopeSpans": []any{
					map[string]any{
						"spans": []any{json.RawMessage(otelSpanJSON(t, nil))},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(doc)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
}

func TestIngestSpans_correctedActionAtResourceLevel_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()
	doc := map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{
					"corrected_action": "ignore",
					"attributes": []any{
						map[string]any{
							"key":   "service.name",
							"value": map[string]any{"stringValue": "svc-a"},
						},
					},
				},
				"scopeSpans": []any{
					map[string]any{
						"spans": []any{json.RawMessage(otelSpanJSON(t, nil))},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(doc)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
}

func TestIngestSpans_outcomeAsResourceAttribute_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()
	doc := map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{
					"attributes": []any{
						map[string]any{
							"key":   "service.name",
							"value": map[string]any{"stringValue": "svc-a"},
						},
						map[string]any{
							"key":   "outcome",
							"value": map[string]any{"stringValue": "ok"},
						},
					},
				},
				"scopeSpans": []any{
					map[string]any{
						"spans": []any{json.RawMessage(otelSpanJSON(t, nil))},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(doc)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
}

func TestIngestSpans_outcomeAtScopeLevel_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()
	doc := map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{
					"attributes": []any{
						map[string]any{
							"key":   "service.name",
							"value": map[string]any{"stringValue": "svc-a"},
						},
					},
				},
				"scopeSpans": []any{
					map[string]any{
						"outcome": "ok",
						"spans":   []any{json.RawMessage(otelSpanJSON(t, nil))},
					},
				},
			},
		},
	}
	body, _ := json.Marshal(doc)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
}

// -----------------------------------------------------------
// Atomicity & lossless forwarding (evaluator #1, #2, #3)
// -----------------------------------------------------------

// Multi-repo POST is forwarded as ONE call with the ORIGINAL
// body bytes (no re-serialization, no per-repo splitting).
func TestIngestSpans_happyPath_multiRepo_forwardedOnce(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{
		"svc-a": "repo-1",
		"svc-b": "repo-2",
	})
	defer rig.cleanup()

	spanA := otelSpanJSON(t, nil)
	spanB := otelSpanJSON(t, map[string]any{
		"traceId": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	})
	body := otelPayloadMultiResource(t, map[string][][]byte{
		"svc-a": {spanA},
		"svc-b": {spanB},
	})

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	var resp IngestSpansResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp.AcceptedSpans != 2 {
		t.Errorf("accepted_spans = %d, want 2", resp.AcceptedSpans)
	}
	// Atomicity: forwarder called EXACTLY once with the
	// inbound body (one POST, one body, no splitting).
	if rig.forwarder.callCount() != 1 {
		t.Fatalf("forwarder calls = %d, want exactly 1 (atomic forwarding)",
			rig.forwarder.callCount())
	}
	last, ok := rig.forwarder.lastBatch()
	if !ok {
		t.Fatal("forwarder never received the batch")
	}
	if last.ContentType != "application/json" {
		t.Errorf("content_type = %q, want application/json", last.ContentType)
	}
	// Forwarded body MUST equal the inbound body
	// byte-for-byte (no re-serialization).
	if !bytes.Equal(last.Body, body) {
		t.Errorf("forwarded body differs from inbound body.\nwant=%s\ngot =%s",
			string(body), string(last.Body))
	}
	snap := rig.metrics.Snapshot()
	if snap["repo-1"][SpanStatusAccepted] != 1 {
		t.Errorf("repo-1 accepted = %d, want 1; snap=%+v", snap["repo-1"][SpanStatusAccepted], snap)
	}
	if snap["repo-2"][SpanStatusAccepted] != 1 {
		t.Errorf("repo-2 accepted = %d, want 1; snap=%+v", snap["repo-2"][SpanStatusAccepted], snap)
	}
}

// Lossless preservation: a span with an INT attribute (not a
// string) must be forwarded byte-for-byte without losing the
// intValue.
func TestIngestSpans_intValueAttribute_preservedInForwardedBody(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	// Build span manually so we can include a typed
	// intValue attribute that the iter-1 stringValue-only
	// re-serializer would have dropped.
	spanWithIntAttr := map[string]any{
		"traceId":           "11111111111111111111111111111111",
		"spanId":            "2222222222222222",
		"parentSpanId":      "3333333333333333",
		"name":              "GET /api",
		"startTimeUnixNano": "1000",
		"endTimeUnixNano":   "2000",
		"attributes": []any{
			map[string]any{
				"key":   "http.status_code",
				"value": map[string]any{"intValue": "200"},
			},
			map[string]any{
				"key":   "http.success",
				"value": map[string]any{"boolValue": true},
			},
		},
	}
	spanBytes, _ := json.Marshal(spanWithIntAttr)
	body := otelPayload(t, "svc-a", spanBytes)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	last, _ := rig.forwarder.lastBatch()
	if !bytes.Equal(last.Body, body) {
		t.Errorf("forwarded body differs from inbound; intValue/boolValue likely dropped.\nwant=%s\ngot =%s",
			string(body), string(last.Body))
	}
	// Belt-and-suspenders: literal substring check for
	// `intValue` so a refactor that re-introduces lossy
	// re-serialization fails this test.
	if !bytes.Contains(last.Body, []byte(`"intValue":"200"`)) {
		t.Errorf("forwarded body missing intValue:200 — re-serialization regression")
	}
	if !bytes.Contains(last.Body, []byte(`"boolValue":true`)) {
		t.Errorf("forwarded body missing boolValue:true — re-serialization regression")
	}
}

// Lossless preservation: service.name must NOT be replaced
// by repo_id in the forwarded body (iter-1 regression).
func TestIngestSpans_serviceName_notRewrittenToRepoID(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()
	body := otelPayload(t, "svc-a", otelSpanJSON(t, nil))
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	last, _ := rig.forwarder.lastBatch()
	if !bytes.Contains(last.Body, []byte(`"stringValue":"svc-a"`)) {
		t.Errorf("forwarded body missing original service.name 'svc-a'.\nbody=%s", string(last.Body))
	}
	if bytes.Contains(last.Body, []byte(`"stringValue":"repo-1"`)) {
		t.Errorf("forwarded body contains repo-1 — handler is illegally rewriting service.name.\nbody=%s",
			string(last.Body))
	}
}

func TestIngestSpans_numericTimestamp_accepted(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := otelSpanJSON(t, map[string]any{
		"startTimeUnixNano": 1000,
		"endTimeUnixNano":   2000,
	})
	body := otelPayload(t, "svc-a", span)

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// Forwarder error handling
// -----------------------------------------------------------

func TestIngestSpans_forwarderNotConfigured_returns503(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unexpected sqlmock activity: %v", err)
		}
		_ = db.Close()
	}()

	metrics := NewDefaultSpanMetrics()
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:      silentLogger(),
			SpanMetrics: metrics,
			SpanLookup:  staticServiceMap(map[string]string{"svc-a": "repo-1"}),
			// SpanForwarder intentionally nil
		},
	)
	body := otelPayload(t, "svc-a", otelSpanJSON(t, nil))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forwarder_not_configured") {
		t.Errorf("body = %q, want code 'forwarder_not_configured'", w.Body.String())
	}
	if got := metrics.Snapshot()["repo-1"][SpanStatusForwarderNotConfigured]; got != 1 {
		t.Errorf("metric repo-1/forwarder_not_configured = %d, want 1; full=%+v",
			got, metrics.Snapshot())
	}
}

func TestIngestSpans_forwarderTransientError_returns502(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	rig.forwarder.err = errors.New("connection refused")
	defer rig.cleanup()

	body := otelPayload(t, "svc-a", otelSpanJSON(t, nil))
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forward_failed") {
		t.Errorf("body = %q, want code 'forward_failed'", w.Body.String())
	}
	if got := rig.metrics.Snapshot()["repo-1"][SpanStatusForwardFailed]; got != 1 {
		t.Errorf("metric repo-1/forward_failed = %d, want 1; full=%+v",
			got, rig.metrics.Snapshot())
	}
}

// -----------------------------------------------------------
// Unknown service handling: iter-2 changed this to 400
// (was 202 with drop count in iter-1). Aligns with the
// doc comment in cmd/mgmt-api/main.go and the architecture's
// fail-fast batch semantic.
// -----------------------------------------------------------

func TestIngestSpans_unknownService_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{}) // empty -> always unknown
	defer rig.cleanup()

	body := otelPayload(t, "svc-x", otelSpanJSON(t, nil))
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unknown_service") {
		t.Errorf("body = %q, want code 'unknown_service'", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "svc-x") {
		t.Errorf("body = %q, want service name 'svc-x' in error", w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0", rig.forwarder.callCount())
	}
	if got := rig.metrics.Snapshot()[""][SpanStatusUnknownService]; got != 1 {
		t.Errorf("metric (empty)/unknown_service = %d, want 1; full=%+v",
			got, rig.metrics.Snapshot())
	}
}

// Mixed known + unknown services in same POST: the WHOLE
// batch is rejected (fail-fast atomic semantic).
func TestIngestSpans_mixedKnownAndUnknownService_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"}) // svc-b unknown
	defer rig.cleanup()

	body := otelPayloadMultiResource(t, map[string][][]byte{
		"svc-a": {otelSpanJSON(t, nil)},
		"svc-b": {otelSpanJSON(t, nil)},
	})
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0 (one unknown service must abort the whole batch)",
			rig.forwarder.callCount())
	}
}

// Empty resource group (zero spans) with unknown service.name
// should NOT be a 400 — there's no data to attribute, so the
// stricter "any resource with unknown service" check would
// be overly aggressive (per rubber-duck #6).
func TestIngestSpans_emptyResourceGroupUnknownService_returns202(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	// resourceSpans has one entry with svc-x (unmapped),
	// but it carries ZERO spans.
	doc := map[string]any{
		"resourceSpans": []any{
			map[string]any{
				"resource": map[string]any{
					"attributes": []any{
						map[string]any{
							"key":   "service.name",
							"value": map[string]any{"stringValue": "svc-x"},
						},
					},
				},
				"scopeSpans": []any{
					map[string]any{"spans": []any{}},
				},
			},
		},
	}
	body, _ := json.Marshal(doc)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (empty resource group is a no-op). body=%q",
			w.Code, w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0 (no spans to forward)", rig.forwarder.callCount())
	}
}

// -----------------------------------------------------------
// Content-type guard (rubber-duck #2)
// -----------------------------------------------------------

func TestIngestSpans_textPlainContentType_returns415(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	body := otelPayload(t, "svc-a", otelSpanJSON(t, nil))
	r := httptest.NewRequest(http.MethodPost, RouteSpans, bytes.NewReader(body))
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, r)

	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status = %d, want 415. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "unsupported_media_type") {
		t.Errorf("body = %q, want code 'unsupported_media_type'", w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0 (415 must short-circuit)", rig.forwarder.callCount())
	}
}

func TestIngestSpans_jsonContentTypeWithCharset_accepted(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	body := otelPayload(t, "svc-a", otelSpanJSON(t, nil))
	r := httptest.NewRequest(http.MethodPost, RouteSpans, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json; charset=utf-8")
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (charset param must be tolerated). body=%q",
			w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// Auth + bad-request paths
// -----------------------------------------------------------

func TestIngestSpans_missingAuth_returns401(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	body := otelPayload(t, "svc-a", otelSpanJSON(t, nil))
	r := httptest.NewRequest(http.MethodPost, RouteSpans, bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, r)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401. body=%q", w.Code, w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0 (auth fail must short-circuit)", rig.forwarder.callCount())
	}
}

func TestIngestSpans_emptyBody_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
}

func TestIngestSpans_malformedJSON_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, []byte(`{not_json`)))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "invalid_json") {
		t.Errorf("body = %q, want code 'invalid_json'", w.Body.String())
	}
}

func TestIngestSpans_bodyTooLarge_returns413(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unexpected sqlmock activity: %v", err)
		}
		_ = db.Close()
	}()
	fwd := &fakeSpanForwarder{}
	metrics := NewDefaultSpanMetrics()
	h := NewHandler(db,
		&StaticBearerVerifier{Secret: testToken, Subject: "test-op"},
		fakeResolver(testHeadSHA, nil),
		Options{
			Logger:            silentLogger(),
			SpanForwarder:     fwd,
			SpanMetrics:       metrics,
			SpanLookup:        staticServiceMap(map[string]string{"svc-a": "repo-1"}),
			MaxSpansBodyBytes: 64,
		},
	)
	body := otelPayload(t, "svc-a", otelSpanJSON(t, nil))
	if len(body) <= 64 {
		t.Fatalf("test fixture too small (%d bytes); want > 64", len(body))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413. body=%q", w.Code, w.Body.String())
	}
	if fwd.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0", fwd.callCount())
	}
}

// -----------------------------------------------------------
// Route mounting through the real composition mux
// -----------------------------------------------------------

func TestIngestSpans_routeMountedTrailingSlash(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	body := otelPayload(t, "svc-a", otelSpanJSON(t, nil))
	r := httptest.NewRequest(http.MethodPost, RouteSpans+"/", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(AuthorizationHeader, "Bearer "+testToken)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (trailing slash must dispatch the same handler). body=%q",
			w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// SpanForwarder direct tests
// -----------------------------------------------------------

func TestNotConfiguredForwarder_returnsSentinel(t *testing.T) {
	t.Parallel()
	err := notConfiguredForwarder{}.Forward(context.Background(), SpansBatch{})
	if !errors.Is(err, ErrForwarderNotConfigured) {
		t.Fatalf("err = %v, want ErrForwarderNotConfigured", err)
	}
}

func TestHTTPSpanForwarder_postsBodyAndContentType(t *testing.T) {
	t.Parallel()
	var seen *http.Request
	var seenBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r
		seenBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := &HTTPSpanForwarder{URL: srv.URL + "/v1/traces"}
	batch := SpansBatch{
		Body:        []byte(`{"resourceSpans":[]}`),
		ContentType: "application/json",
	}
	if err := f.Forward(context.Background(), batch); err != nil {
		t.Fatalf("Forward err = %v, want nil", err)
	}
	if seen == nil {
		t.Fatal("downstream never received the request")
	}
	if seen.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", seen.Header.Get("Content-Type"))
	}
	if string(seenBody) != `{"resourceSpans":[]}` {
		t.Errorf("body = %q, want full forwarded payload", string(seenBody))
	}
}

// Atomicity: a single Forward call MUST produce exactly ONE
// HTTP POST. Iter-1 had a per-batch loop that could partially
// accept.
func TestHTTPSpanForwarder_onePOSTPerForward(t *testing.T) {
	t.Parallel()
	var postCount int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		postCount++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	f := &HTTPSpanForwarder{URL: srv.URL + "/v1/traces"}
	for i := 0; i < 5; i++ {
		if err := f.Forward(context.Background(), SpansBatch{
			Body:        []byte(`{"resourceSpans":[]}`),
			ContentType: "application/json",
		}); err != nil {
			t.Fatalf("Forward err = %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 5 {
		t.Errorf("downstream POST count = %d, want 5 (exactly 1 POST per Forward)", postCount)
	}
}

func TestHTTPSpanForwarder_nonStatus2xx_returnsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	f := &HTTPSpanForwarder{URL: srv.URL + "/v1/traces"}
	err := f.Forward(context.Background(), SpansBatch{
		Body: []byte(`{}`), ContentType: "application/json",
	})
	if err == nil {
		t.Fatal("err = nil, want non-nil for 500 downstream")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want status code in message", err)
	}
}

func TestHTTPSpanForwarder_emptyURL_returnsNotConfigured(t *testing.T) {
	t.Parallel()
	f := &HTTPSpanForwarder{}
	err := f.Forward(context.Background(), SpansBatch{Body: []byte("{}")})
	if !errors.Is(err, ErrForwarderNotConfigured) {
		t.Fatalf("err = %v, want ErrForwarderNotConfigured", err)
	}
}

// -----------------------------------------------------------
// DefaultSpanMetrics + WritePrometheus
// -----------------------------------------------------------

func TestDefaultSpanMetrics_incrementsAndSnapshots(t *testing.T) {
	t.Parallel()
	m := NewDefaultSpanMetrics()
	m.IncIngestSpansTotal("repo-1", SpanStatusAccepted, 3)
	m.IncIngestSpansTotal("repo-1", SpanStatusAccepted, 2)
	m.IncIngestSpansTotal("repo-2", SpanStatusForwardFailed, 1)
	m.IncIngestSpansTotal("", SpanStatusUnknownService, 4)

	got := m.Snapshot()
	if got["repo-1"][SpanStatusAccepted] != 5 {
		t.Errorf("repo-1/accepted = %d, want 5", got["repo-1"][SpanStatusAccepted])
	}
	if got["repo-2"][SpanStatusForwardFailed] != 1 {
		t.Errorf("repo-2/forward_failed = %d, want 1", got["repo-2"][SpanStatusForwardFailed])
	}
	if got[""][SpanStatusUnknownService] != 4 {
		t.Errorf("(empty)/unknown_service = %d, want 4", got[""][SpanStatusUnknownService])
	}
	got["repo-1"][SpanStatusAccepted] = 999
	if m.Snapshot()["repo-1"][SpanStatusAccepted] != 5 {
		t.Errorf("snapshot mutation leaked into live counters; got %d, want 5",
			m.Snapshot()["repo-1"][SpanStatusAccepted])
	}
}

func TestDefaultSpanMetrics_WritePrometheus_emptyHasHeader(t *testing.T) {
	t.Parallel()
	m := NewDefaultSpanMetrics()
	var buf bytes.Buffer
	if _, err := m.WritePrometheus(&buf); err != nil {
		t.Fatalf("WritePrometheus err = %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "# HELP mgmt_ingest_spans_total") {
		t.Errorf("missing HELP line; got:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE mgmt_ingest_spans_total counter") {
		t.Errorf("missing TYPE line; got:\n%s", out)
	}
}

func TestDefaultSpanMetrics_WritePrometheus_stableSortedOutput(t *testing.T) {
	t.Parallel()
	m := NewDefaultSpanMetrics()
	m.IncIngestSpansTotal("repo-z", SpanStatusAccepted, 1)
	m.IncIngestSpansTotal("repo-a", SpanStatusForwardFailed, 2)
	m.IncIngestSpansTotal("repo-a", SpanStatusAccepted, 7)
	m.IncIngestSpansTotal("", SpanStatusUnknownService, 5)

	var a, b bytes.Buffer
	_, _ = m.WritePrometheus(&a)
	_, _ = m.WritePrometheus(&b)
	if a.String() != b.String() {
		t.Errorf("WritePrometheus output not deterministic.\nA:\n%s\nB:\n%s", a.String(), b.String())
	}
	// repo-a appears before repo-z; empty repo_id (sorts first).
	idxEmpty := strings.Index(a.String(), `repo_id=""`)
	idxA := strings.Index(a.String(), `repo_id="repo-a"`)
	idxZ := strings.Index(a.String(), `repo_id="repo-z"`)
	if !(idxEmpty < idxA && idxA < idxZ) {
		t.Errorf("output not sorted by repo_id; got:\n%s", a.String())
	}
}

func TestDefaultSpanMetrics_WritePrometheus_escapesLabelValues(t *testing.T) {
	t.Parallel()
	m := NewDefaultSpanMetrics()
	// Repo IDs are normally UUIDs but be defensive against
	// a misconfigured operator planting a quote in the
	// service map.
	m.IncIngestSpansTotal(`weird"id`, SpanStatusAccepted, 1)
	var buf bytes.Buffer
	_, _ = m.WritePrometheus(&buf)
	if !strings.Contains(buf.String(), `repo_id="weird\"id"`) {
		t.Errorf("quote not escaped in label value; got:\n%s", buf.String())
	}
}

// -----------------------------------------------------------
// Evaluator iter-2 #1: forbidden field inside span events /
// links / event-attrs / link-attrs must also be rejected.
// -----------------------------------------------------------

// spanWithSubObjects builds a valid span with an `events` or
// `links` array tacked on.
func spanWithSubObjects(t *testing.T, subKey string, entries []map[string]any) []byte {
	t.Helper()
	base := map[string]any{
		"traceId":           "11111111111111111111111111111111",
		"spanId":            "2222222222222222",
		"parentSpanId":      "3333333333333333",
		"name":              "GET /api",
		"startTimeUnixNano": "1000",
		"endTimeUnixNano":   "2000",
	}
	arr := make([]any, 0, len(entries))
	for _, e := range entries {
		arr = append(arr, e)
	}
	base[subKey] = arr
	b, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal span: %v", err)
	}
	return b
}

func TestIngestSpans_outcomeAsEventObjectField_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := spanWithSubObjects(t, "events", []map[string]any{
		{
			"timeUnixNano": "1500",
			"name":         "boom",
			"outcome":      "success", // forbidden on the event object itself
		},
	})
	body := otelPayload(t, "svc-a", span)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "events[0]") {
		t.Errorf("body = %q, want location 'events[0]'", w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0", rig.forwarder.callCount())
	}
}

func TestIngestSpans_outcomeAsEventAttribute_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := spanWithSubObjects(t, "events", []map[string]any{
		{
			"timeUnixNano": "1500",
			"name":         "boom",
			"attributes": []any{
				map[string]any{
					"key":   "outcome",
					"value": map[string]any{"stringValue": "success"},
				},
			},
		},
	})
	body := otelPayload(t, "svc-a", span)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "events[0].attributes") {
		t.Errorf("body = %q, want location 'events[0].attributes'", w.Body.String())
	}
}

func TestIngestSpans_correctedActionAsLinkObjectField_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := spanWithSubObjects(t, "links", []map[string]any{
		{
			"traceId":          "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"spanId":           "bbbbbbbbbbbbbbbb",
			"corrected_action": "retry", // forbidden on the link object itself
		},
	})
	body := otelPayload(t, "svc-a", span)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "links[0]") {
		t.Errorf("body = %q, want location 'links[0]'", w.Body.String())
	}
}

func TestIngestSpans_outcomeAsLinkAttribute_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := spanWithSubObjects(t, "links", []map[string]any{
		{
			"traceId": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"spanId":  "bbbbbbbbbbbbbbbb",
			"attributes": []any{
				map[string]any{
					"key":   "outcome",
					"value": map[string]any{"stringValue": "ok"},
				},
			},
		},
	})
	body := otelPayload(t, "svc-a", span)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "links[0].attributes") {
		t.Errorf("body = %q, want location 'links[0].attributes'", w.Body.String())
	}
}

// Defensive: events / links arrays that are present but
// empty / null must NOT regress accepted paths.
func TestIngestSpans_emptyEventsAndLinks_accepted(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	// Both fields present, both empty.
	span := map[string]any{
		"traceId":           "11111111111111111111111111111111",
		"spanId":            "2222222222222222",
		"parentSpanId":      "3333333333333333",
		"name":              "noop",
		"startTimeUnixNano": "1000",
		"endTimeUnixNano":   "2000",
		"events":            []any{},
		"links":             []any{},
	}
	spanBytes, _ := json.Marshal(span)
	body := otelPayload(t, "svc-a", spanBytes)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
}

// Clean event/link payloads with non-string attribute values
// also pass through (regression guard for the walker only
// inspecting attribute KEYS).
func TestIngestSpans_eventAttributesWithIntValue_acceptedAndPreserved(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := spanWithSubObjects(t, "events", []map[string]any{
		{
			"timeUnixNano": "1500",
			"name":         "retry",
			"attributes": []any{
				map[string]any{
					"key":   "retry.count",
					"value": map[string]any{"intValue": "3"},
				},
			},
		},
	})
	body := otelPayload(t, "svc-a", span)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202. body=%q", w.Code, w.Body.String())
	}
	last, _ := rig.forwarder.lastBatch()
	if !bytes.Contains(last.Body, []byte(`"intValue":"3"`)) {
		t.Errorf("forwarded body missing event intValue:3 — re-serialization regression.\nbody=%s",
			string(last.Body))
	}
}

// -----------------------------------------------------------
// Evaluator iter-2 #2: unknown_service metric must increment
// by the resource group's span count, not by 1.
// -----------------------------------------------------------

func TestIngestSpans_unknownService_metricCountsAllSpans(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{}) // empty -> svc-x is unknown
	defer rig.cleanup()

	// 5 spans in one unknown resource group; metric should
	// be incremented by 5, not 1.
	spanA := otelSpanJSON(t, nil)
	spanB := otelSpanJSON(t, map[string]any{"traceId": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"})
	spanC := otelSpanJSON(t, map[string]any{"traceId": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"})
	spanD := otelSpanJSON(t, map[string]any{"traceId": "cccccccccccccccccccccccccccccccc"})
	spanE := otelSpanJSON(t, map[string]any{"traceId": "dddddddddddddddddddddddddddddddd"})
	body := otelPayload(t, "svc-x", spanA, spanB, spanC, spanD, spanE)

	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if got := rig.metrics.Snapshot()[""][SpanStatusUnknownService]; got != 5 {
		t.Errorf("metric (empty)/unknown_service = %d, want 5 (must equal the resource group's span count, not 1); snap=%+v",
			got, rig.metrics.Snapshot())
	}
}

// -----------------------------------------------------------
// Evaluator iter-3 #1: deep forbidden-field scan catches
// nested OTLP shapes the structured walker doesn't traverse
// (`span.status.outcome`, nested `kvlistValue` KeyValue keys).
// -----------------------------------------------------------

// span.status is an OTel `{code, message}` object. The
// structured walker doesn't descend into it, but the deep
// scan must catch an `outcome` field smuggled there.
func TestIngestSpans_outcomeInsideSpanStatus_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := map[string]any{
		"traceId":           "11111111111111111111111111111111",
		"spanId":            "2222222222222222",
		"parentSpanId":      "3333333333333333",
		"name":              "GET /api",
		"startTimeUnixNano": "1000",
		"endTimeUnixNano":   "2000",
		"status": map[string]any{
			"code":    1,
			"message": "ok",
			"outcome": "success", // forbidden, nested in unwalked status object
		},
	}
	spanBytes, _ := json.Marshal(span)
	body := otelPayload(t, "svc-a", spanBytes)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "status.outcome") {
		t.Errorf("body = %q, want path containing 'status.outcome'", w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0 (forbidden field must drop the batch)", rig.forwarder.callCount())
	}
}

// span.attributes[].value.kvlistValue.values[].key="outcome"
// — nested KeyValue inside a kvlist attribute. The structured
// walker doesn't descend into typed-value unions, so only the
// deep scan catches this.
func TestIngestSpans_outcomeInsideNestedKVListValue_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := map[string]any{
		"traceId":           "11111111111111111111111111111111",
		"spanId":            "2222222222222222",
		"parentSpanId":      "3333333333333333",
		"name":              "GET /api",
		"startTimeUnixNano": "1000",
		"endTimeUnixNano":   "2000",
		"attributes": []any{
			map[string]any{
				"key": "http.request",
				"value": map[string]any{
					"kvlistValue": map[string]any{
						"values": []any{
							map[string]any{
								"key":   "outcome", // forbidden, nested in kvlistValue
								"value": map[string]any{"stringValue": "success"},
							},
						},
					},
				},
			},
		},
	}
	spanBytes, _ := json.Marshal(span)
	body := otelPayload(t, "svc-a", spanBytes)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "forbidden_field") {
		t.Errorf("body = %q, want code 'forbidden_field'", w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0 (nested kvlistValue must drop the batch)",
			rig.forwarder.callCount())
	}
}

// Regression: a literal STRING value "outcome" is NOT a
// forbidden key and must be accepted. The deep scan only
// rejects:
//
//	(a) JSON object keys equal to a forbidden name, OR
//	(b) the string value of a KeyValue's `key` field.
//
// It does NOT reject arbitrary string values appearing in
// `stringValue`, span `name`, etc.
func TestIngestSpans_literalStringValueOutcome_accepted(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := map[string]any{
		"traceId":           "11111111111111111111111111111111",
		"spanId":            "2222222222222222",
		"parentSpanId":      "3333333333333333",
		"name":              "outcome", // the word as a span name is fine
		"startTimeUnixNano": "1000",
		"endTimeUnixNano":   "2000",
		"attributes": []any{
			map[string]any{
				"key":   "result.label",
				"value": map[string]any{"stringValue": "outcome"}, // as a value, fine
			},
		},
	}
	spanBytes, _ := json.Marshal(span)
	body := otelPayload(t, "svc-a", spanBytes)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (string value 'outcome' must NOT be rejected). body=%q",
			w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// Evaluator iter-3 #2: W3C/OTel sentinel rejection — the
// all-zero trace_id and all-zero span_id must be rejected.
// -----------------------------------------------------------

func TestIngestSpans_allZeroTraceID_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	// 32 zeros — passes the hex shape check but is the
	// W3C "invalid trace" sentinel.
	bad := otelSpanJSON(t, map[string]any{
		"traceId": "00000000000000000000000000000000",
	})
	body := otelPayload(t, "svc-a", bad)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "trace_id") {
		t.Errorf("body = %q, want substring 'trace_id'", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "all zeros") {
		t.Errorf("body = %q, want substring 'all zeros'", w.Body.String())
	}
	if rig.forwarder.callCount() != 0 {
		t.Errorf("forwarder calls = %d, want 0", rig.forwarder.callCount())
	}
}

func TestIngestSpans_allZeroSpanID_returns400(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	// 16 zeros — W3C "no span" sentinel, invalid for the
	// span_id field itself.
	bad := otelSpanJSON(t, map[string]any{
		"spanId": "0000000000000000",
	})
	body := otelPayload(t, "svc-a", bad)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400. body=%q", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "span_id") {
		t.Errorf("body = %q, want substring 'span_id'", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "all zeros") {
		t.Errorf("body = %q, want substring 'all zeros'", w.Body.String())
	}
}

// parent_span_id all-zero IS allowed — it legitimately
// indicates a root span (no parent). Regression guard.
func TestIngestSpans_allZeroParentSpanID_accepted(t *testing.T) {
	t.Parallel()
	rig := newSpansTestHandler(t, map[string]string{"svc-a": "repo-1"})
	defer rig.cleanup()

	span := otelSpanJSON(t, map[string]any{
		"parentSpanId": "0000000000000000",
	})
	body := otelPayload(t, "svc-a", span)
	w := httptest.NewRecorder()
	rig.h.ServeHTTP(w, authedSpansRequest(t, body))
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (all-zero parent_span_id = root span; must be accepted). body=%q",
			w.Code, w.Body.String())
	}
}

// -----------------------------------------------------------
// Internal helpers
// -----------------------------------------------------------
func debugPrintMetrics(t *testing.T, m *DefaultSpanMetrics) { //nolint:unused
	t.Helper()
	for repo, by := range m.Snapshot() {
		for status, n := range by {
			fmt.Printf("metric repo=%q status=%q = %d\n", repo, status, n)
		}
	}
}

// compile-time references so unused imports don't break the
// build when a test is disabled mid-iteration.
var (
	_ = sql.ErrNoRows
)
