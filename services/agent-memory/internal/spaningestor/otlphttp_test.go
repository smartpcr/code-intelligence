package spaningestor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

const repoUUIDForOTLP = "11111111-2222-3333-4444-555555555555"

// staticServiceMap implements ServiceNameToRepoID for tests.
func staticServiceMap(m map[string]string) ServiceNameToRepoID {
	return func(name string) string { return m[name] }
}

// buildOTLPRequestBody renders a minimal-but-valid OTLP/HTTP
// JSON ExportTraceServiceRequest body with one resource span
// carrying one span.
func buildOTLPRequestBody(t *testing.T, serviceName, traceID, spanID string, attrs map[string]string) []byte {
	t.Helper()
	resourceAttrs := []map[string]any{
		{"key": "service.name", "value": map[string]any{"stringValue": serviceName}},
	}
	spanAttrs := make([]map[string]any, 0, len(attrs))
	for k, v := range attrs {
		spanAttrs = append(spanAttrs, map[string]any{
			"key": k, "value": map[string]any{"stringValue": v},
		})
	}
	payload := map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{"attributes": resourceAttrs},
				"scopeSpans": []map[string]any{
					{
						"spans": []map[string]any{
							{
								"traceId":           traceID,
								"spanId":            spanID,
								"startTimeUnixNano": "1700000000000000000",
								"endTimeUnixNano":   "1700000000010000000", // +10 ms
								"name":              "doStuff",
								"attributes":        spanAttrs,
							},
						},
					},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return body
}

func newTestOTLPReceiver(t *testing.T, ing *Ingestor, services map[string]string) *OTLPReceiver {
	t.Helper()
	return NewOTLPReceiver(ing, staticServiceMap(services), OTLPConfig{}, discardLogger())
}

// TestOTLPReceiver_acceptsJSONAndEnqueues confirms a
// well-formed OTLP/HTTP JSON POST is parsed, the
// service.name → repo_id mapping is applied, and a SpanBatch
// of the correct shape lands in the Ingestor's queue.
func TestOTLPReceiver_acceptsJSONAndEnqueues(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	rcv := newTestOTLPReceiver(t, ing, map[string]string{"my-service": repoUUIDForOTLP})

	body := buildOTLPRequestBody(t, "my-service", "trace-1", "span-1", map[string]string{
		"code.namespace": "pkg.svc", "code.function": "Callee",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}

	// Drain and process the enqueued batch so we can confirm
	// it round-tripped through the resolver.
	select {
	case batch := <-ing.queue:
		if batch.RepoID != repoUUIDForOTLP {
			t.Errorf("batch.RepoID = %q, want %q", batch.RepoID, repoUUIDForOTLP)
		}
		if got := len(batch.Spans); got != 1 {
			t.Fatalf("spans = %d, want 1", got)
		}
		sp := batch.Spans[0]
		if sp.TraceID != "trace-1" || sp.SpanID != "span-1" {
			t.Errorf("span identity = (%q,%q)", sp.TraceID, sp.SpanID)
		}
		if got := sp.Attributes["code.function"]; got != "Callee" {
			t.Errorf("code.function attr = %q, want Callee", got)
		}
		// 10ms duration synthesized in buildOTLPRequestBody.
		if sp.DurationMs <= 0 {
			t.Errorf("DurationMs = %f, want > 0", sp.DurationMs)
		}
	default:
		t.Fatalf("queue empty; expected a batch")
	}
}

// TestOTLPReceiver_unknownServiceDropped confirms a span
// whose service.name is not in the lookup map is dropped
// silently (no batch enqueued), and the response is still
// 200 OK with `{}` (the OTLP success envelope) — the spec
// forbids 4xx on a well-formed empty result.
func TestOTLPReceiver_unknownServiceDropped(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	rcv := newTestOTLPReceiver(t, ing, map[string]string{"known-service": repoUUIDForOTLP})

	body := buildOTLPRequestBody(t, "unknown-service", "trace-x", "span-x", nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 on unknown service", rr.Code)
	}
	if len(ing.queue) != 0 {
		t.Errorf("queue depth = %d, want 0 (span should be dropped)", len(ing.queue))
	}
}

// TestOTLPReceiver_protobufAcceptedAndDecoded confirms that
// Content-Type: application/x-protobuf is accepted and the
// payload routes through the shared protobuf decoder. Evaluator
// iter-1 #1: the OTLP/HTTP receiver MUST accept the protobuf
// encoding to be spec-compliant (gRPC OTLP lands in otlpgrpc.go).
func TestOTLPReceiver_protobufAcceptedAndDecoded(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	ing := NewIngestor(resolver, newFakeTraceWriter(), nil, Config{QueueDepth: 4}, discardLogger())
	rcv := newTestOTLPReceiver(t, ing, map[string]string{"x": repoUUIDForOTLP})

	// Build a minimal valid protobuf ExportTraceServiceRequest
	// (zero ResourceSpans). The receiver should accept it with
	// 200 OK because the body parses cleanly even though no
	// spans land on the queue.
	payload := &coltracepb.ExportTraceServiceRequest{}
	body, err := proto.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200; body=%q", rr.Code, rr.Body.String())
	}
}

// TestOTLPReceiver_backpressureRespondsWith503 confirms that
// when the Ingestor's queue is full, the receiver returns 503
// with a Retry-After header so the OTel collector backs off.
func TestOTLPReceiver_backpressureRespondsWith503(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 1}, discardLogger())
	// Pre-fill the queue so the next Enqueue returns ErrQueueFull.
	if err := ing.Enqueue(SpanBatch{RepoID: repoUUIDForOTLP, Spans: []ObservationSpan{{
		Span: Span{RepoID: repoUUIDForOTLP, TraceID: "t", SpanID: "pre"},
	}}}); err != nil {
		t.Fatalf("pre-enqueue: %v", err)
	}
	rcv := newTestOTLPReceiver(t, ing, map[string]string{"svc": repoUUIDForOTLP})

	body := buildOTLPRequestBody(t, "svc", "trace-2", "span-2", nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rr.Code)
	}
	if ra := rr.Header().Get("Retry-After"); ra == "" {
		t.Errorf("Retry-After header missing on 503")
	}
}

// TestOTLPReceiver_malformedJSONIs400 confirms bad JSON
// surfaces as a 400, not a 500.
func TestOTLPReceiver_malformedJSONIs400(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	ing := NewIngestor(resolver, newFakeTraceWriter(), nil, Config{QueueDepth: 4}, discardLogger())
	rcv := newTestOTLPReceiver(t, ing, map[string]string{"svc": repoUUIDForOTLP})

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("not json"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
}

// TestOTLPReceiver_methodNotAllowed confirms GET on /v1/traces
// is 405, not 200, so accidental browser hits are obvious.
func TestOTLPReceiver_methodNotAllowed(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	ing := NewIngestor(resolver, newFakeTraceWriter(), nil, Config{QueueDepth: 4}, discardLogger())
	rcv := newTestOTLPReceiver(t, ing, map[string]string{"svc": repoUUIDForOTLP})

	req := httptest.NewRequest(http.MethodGet, "/v1/traces", nil)
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
}

// TestOTLPStringInt_unmarshalsBothShapes confirms the
// custom unmarshaler tolerates BOTH the spec-required
// stringified uint64 and the raw-number variant some SDKs
// emit.
func TestOTLPStringInt_unmarshalsBothShapes(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{`"12345"`, 12345},
		{`12345`, 12345},
		{`""`, 0},
		{`0`, 0},
	}
	for _, tc := range cases {
		var v otlpStringInt
		if err := json.Unmarshal([]byte(tc.in), &v); err != nil {
			t.Errorf("Unmarshal(%s): %v", tc.in, err)
			continue
		}
		if uint64(v) != tc.want {
			t.Errorf("Unmarshal(%s) = %d, want %d", tc.in, uint64(v), tc.want)
		}
	}
	// Negative: garbage should return an error.
	var v otlpStringInt
	if err := json.Unmarshal([]byte(`"abc"`), &v); err == nil {
		t.Errorf("Unmarshal garbage: want error, got nil")
	}
}

// ensure http import compiles after edits; trivial use.
var _ = fmt.Sprintf
var _ = context.Background

// -----------------------------------------------------------
// Routing precedence (mgmt-api replay support)
//
// The HTTP receiver must honor an explicit `repo_id` override
// from EITHER the X-Mgmt-Repo-ID header OR a `mgmt.repo_id`
// resource attribute, so the mgmt-api forwarder's batches can
// be routed without operator-side service.name registry
// configuration. The service.name lookup remains the fallback.
// -----------------------------------------------------------

const altRepoUUIDForOTLP = "22222222-3333-4444-5555-666666666666"

// buildOTLPBodyWithResourceAttrs lets a test override the
// resource attributes (e.g. add `mgmt.repo_id`) without the
// service.name baseline that `buildOTLPRequestBody` hard-codes.
func buildOTLPBodyWithResourceAttrs(t *testing.T, resourceAttrs []map[string]any, traceID, spanID string) []byte {
	t.Helper()
	payload := map[string]any{
		"resourceSpans": []map[string]any{
			{
				"resource": map[string]any{"attributes": resourceAttrs},
				"scopeSpans": []map[string]any{
					{
						"spans": []map[string]any{
							{
								"traceId":           traceID,
								"spanId":            spanID,
								"startTimeUnixNano": "1700000000000000000",
								"endTimeUnixNano":   "1700000000010000000",
								"name":              "doStuff",
							},
						},
					},
				},
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return body
}

func TestOTLPReceiver_mgmtRepoIDHeader_overridesServiceLookup(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	// Receiver knows nothing about the service name — without
	// the header the batch would be dropped as unknown.
	rcv := newTestOTLPReceiver(t, ing, map[string]string{})

	body := buildOTLPRequestBody(t, "unknown-svc", "trace-h", "span-h", nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(MgmtRepoIDHeader, repoUUIDForOTLP)
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	select {
	case batch := <-ing.queue:
		if batch.RepoID != repoUUIDForOTLP {
			t.Errorf("batch.RepoID = %q, want %q (from header)", batch.RepoID, repoUUIDForOTLP)
		}
	default:
		t.Fatalf("queue empty; expected a batch routed via X-Mgmt-Repo-ID")
	}
}

func TestOTLPReceiver_mgmtRepoIDResourceAttr_overridesServiceLookup(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	rcv := newTestOTLPReceiver(t, ing, map[string]string{})

	body := buildOTLPBodyWithResourceAttrs(t, []map[string]any{
		{"key": "service.name", "value": map[string]any{"stringValue": "unknown-svc"}},
		{"key": MgmtRepoIDResourceAttr, "value": map[string]any{"stringValue": repoUUIDForOTLP}},
	}, "trace-a", "span-a")

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	select {
	case batch := <-ing.queue:
		if batch.RepoID != repoUUIDForOTLP {
			t.Errorf("batch.RepoID = %q, want %q (from resource attr)", batch.RepoID, repoUUIDForOTLP)
		}
	default:
		t.Fatalf("queue empty; expected a batch routed via mgmt.repo_id attr")
	}
}

func TestOTLPReceiver_mgmtRepoIDHeader_precedesResourceAttr(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	rcv := newTestOTLPReceiver(t, ing, map[string]string{})

	// Resource attr says altRepoUUID; header says repoUUID.
	// Header MUST win (explicit operator override).
	body := buildOTLPBodyWithResourceAttrs(t, []map[string]any{
		{"key": "service.name", "value": map[string]any{"stringValue": "unknown-svc"}},
		{"key": MgmtRepoIDResourceAttr, "value": map[string]any{"stringValue": altRepoUUIDForOTLP}},
	}, "trace-p", "span-p")

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(MgmtRepoIDHeader, repoUUIDForOTLP)
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	select {
	case batch := <-ing.queue:
		if batch.RepoID != repoUUIDForOTLP {
			t.Errorf("batch.RepoID = %q, want %q (header wins)", batch.RepoID, repoUUIDForOTLP)
		}
	default:
		t.Fatalf("queue empty; expected a batch routed via header")
	}
}

func TestOTLPReceiver_mgmtReplayServiceNamePrefix_resolvesRepoID(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	rcv := newTestOTLPReceiver(t, ing, map[string]string{})

	body := buildOTLPRequestBody(t,
		MgmtReplayServiceNamePrefix+repoUUIDForOTLP,
		"trace-pfx", "span-pfx", nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	select {
	case batch := <-ing.queue:
		if batch.RepoID != repoUUIDForOTLP {
			t.Errorf("batch.RepoID = %q, want %q (from service.name prefix)",
				batch.RepoID, repoUUIDForOTLP)
		}
	default:
		t.Fatalf("queue empty; expected a batch routed via service.name prefix")
	}
}

func TestOTLPReceiver_malformedMgmtRepoIDHeader_falls_back(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	// service.name DOES resolve, so a malformed header must
	// fall through to the next hook rather than poison routing.
	rcv := newTestOTLPReceiver(t, ing, map[string]string{"good-svc": repoUUIDForOTLP})

	body := buildOTLPRequestBody(t, "good-svc", "trace-mf", "span-mf", nil)
	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(MgmtRepoIDHeader, "not-a-uuid")
	rr := httptest.NewRecorder()
	rcv.handleTraces(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	select {
	case batch := <-ing.queue:
		if batch.RepoID != repoUUIDForOTLP {
			t.Errorf("batch.RepoID = %q, want %q (fallback to service.name)",
				batch.RepoID, repoUUIDForOTLP)
		}
	default:
		t.Fatalf("queue empty; expected fallback routing to succeed")
	}
}

func TestValidatedRepoID(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"   ", ""},
		{"not-a-uuid", ""},
		{repoUUIDForOTLP, repoUUIDForOTLP},
		{strings.ToUpper(repoUUIDForOTLP), repoUUIDForOTLP},
		{"  " + repoUUIDForOTLP + "  ", repoUUIDForOTLP},
		// Length-correct hex but wrong group sizes — rejected.
		{"111111112222333344445555555555555555", ""},
	}
	for _, tc := range cases {
		if got := validatedRepoID(tc.in); got != tc.want {
			t.Errorf("validatedRepoID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
