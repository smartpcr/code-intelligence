package mgmtapi

// Unit tests for the production [HTTPSpanForwarder] -- the
// concrete [SpanForwarder] the cmd/mgmt-api binary wires when
// AGENT_MEMORY_SPAN_INGESTOR_URL is set.
//
// The tests spin up an httptest.Server that stands in for the
// Span Ingestor's OTLP/HTTP receiver so the wire-shape and
// status-code mapping are exercised end-to-end without a live
// span-ingestor.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recvSpan mirrors the canonical OTLP/JSON span shape so the
// fake receiver can assert on the bytes the forwarder produced.
type recvSpan struct {
	TraceID           string `json:"traceId"`
	SpanID            string `json:"spanId"`
	ParentSpanID      string `json:"parentSpanId"`
	Name              string `json:"name"`
	StartTimeUnixNano string `json:"startTimeUnixNano"`
	EndTimeUnixNano   string `json:"endTimeUnixNano"`
	Attributes        []struct {
		Key   string `json:"key"`
		Value struct {
			StringValue string `json:"stringValue"`
		} `json:"value"`
	} `json:"attributes"`
}

type recvScopeSpans struct {
	Spans []recvSpan `json:"spans"`
}

type recvResourceSpans struct {
	Resource struct {
		Attributes []struct {
			Key   string `json:"key"`
			Value struct {
				StringValue string `json:"stringValue"`
			} `json:"value"`
		} `json:"attributes"`
	} `json:"resource"`
	ScopeSpans []recvScopeSpans `json:"scopeSpans"`
}

type recvExportRequest struct {
	ResourceSpans []recvResourceSpans `json:"resourceSpans"`
}

// fakeReceiver stands in for the Span Ingestor's OTLP/HTTP
// receiver. Records the most-recent request and replies with
// the configured status / body.
type fakeReceiver struct {
	mu              sync.Mutex
	lastReq         *recvExportRequest
	lastHeaderRepo  string
	lastContentType string
	lastPath        string
	lastMethod      string
	replyStatus     int
	replyBody       string
	replyHeaders    http.Header
}

func newFakeReceiver(status int) *fakeReceiver {
	return &fakeReceiver{replyStatus: status}
}

func (f *fakeReceiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.lastPath = r.URL.Path
	f.lastMethod = r.Method
	f.lastContentType = r.Header.Get("Content-Type")
	f.lastHeaderRepo = r.Header.Get("X-Mgmt-Repo-ID")
	for k, vals := range f.replyHeaders {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	body, _ := io.ReadAll(r.Body)
	var req recvExportRequest
	if err := json.Unmarshal(body, &req); err == nil {
		f.lastReq = &req
	}
	w.WriteHeader(f.replyStatus)
	if f.replyBody != "" {
		_, _ = io.WriteString(w, f.replyBody)
	}
	f.mu.Unlock()
}

func (f *fakeReceiver) LastRequest() *recvExportRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}

func newTestForwarder(t *testing.T, srv *httptest.Server) *HTTPSpanForwarder {
	t.Helper()
	fwd, err := NewHTTPSpanForwarder(HTTPSpanForwarderConfig{
		BaseURL:    srv.URL,
		HTTPClient: srv.Client(),
		Logger:     silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewHTTPSpanForwarder: %v", err)
	}
	return fwd
}

func sampleForwardedSpans() []ForwardedSpan {
	return []ForwardedSpan{
		{
			TraceID:           testTraceID,
			SpanID:            testSpanID,
			ParentSpanID:      testParentID,
			Name:              "GET /widgets",
			StartTimeUnixNano: 1700000000000000000,
			EndTimeUnixNano:   1700000000123456000,
			Attributes: map[string]string{
				"http.method": "GET",
				"http.status": "200",
			},
		},
	}
}

// -----------------------------------------------------------
// NewHTTPSpanForwarder validation
// -----------------------------------------------------------

func TestNewHTTPSpanForwarder_missingBaseURL_errors(t *testing.T) {
	t.Parallel()
	if _, err := NewHTTPSpanForwarder(HTTPSpanForwarderConfig{}); err == nil {
		t.Fatalf("err = nil, want missing-BaseURL error")
	}
}

func TestNewHTTPSpanForwarder_wrongScheme_errors(t *testing.T) {
	t.Parallel()
	if _, err := NewHTTPSpanForwarder(HTTPSpanForwarderConfig{BaseURL: "ftp://x:1234"}); err == nil {
		t.Fatalf("err = nil, want scheme error")
	}
}

func TestNewHTTPSpanForwarder_missingHost_errors(t *testing.T) {
	t.Parallel()
	if _, err := NewHTTPSpanForwarder(HTTPSpanForwarderConfig{BaseURL: "http:///foo"}); err == nil {
		t.Fatalf("err = nil, want missing-host error")
	}
}

func TestNewHTTPSpanForwarder_targetURL_joined(t *testing.T) {
	t.Parallel()
	fwd, err := NewHTTPSpanForwarder(HTTPSpanForwarderConfig{BaseURL: "http://span-ingestor:4318"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got, want := fwd.TargetURL(), "http://span-ingestor:4318/v1/traces"; got != want {
		t.Errorf("TargetURL = %q, want %q", got, want)
	}
}

func TestNewHTTPSpanForwarder_baseURLTrailingSlash_normalized(t *testing.T) {
	t.Parallel()
	fwd, err := NewHTTPSpanForwarder(HTTPSpanForwarderConfig{BaseURL: "http://x:1/"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got, want := fwd.TargetURL(), "http://x:1/v1/traces"; got != want {
		t.Errorf("TargetURL = %q, want %q (trailing slash should collapse)", got, want)
	}
}

func TestNewHTTPSpanForwarder_customPath_honoured(t *testing.T) {
	t.Parallel()
	fwd, err := NewHTTPSpanForwarder(HTTPSpanForwarderConfig{
		BaseURL: "http://x:1",
		Path:    "/internal/ingest",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got, want := fwd.TargetURL(), "http://x:1/internal/ingest"; got != want {
		t.Errorf("TargetURL = %q, want %q", got, want)
	}
}

// -----------------------------------------------------------
// ForwardSpans wire shape
// -----------------------------------------------------------

func TestHTTPSpanForwarder_postsCanonicalOTLP_includesRoutingResourceAttrs(t *testing.T) {
	t.Parallel()
	recv := newFakeReceiver(http.StatusOK)
	srv := httptest.NewServer(recv)
	defer srv.Close()
	fwd := newTestForwarder(t, srv)

	err := fwd.ForwardSpans(context.Background(), testSpansRepoID, sampleForwardedSpans())
	if err != nil {
		t.Fatalf("ForwardSpans err = %v, want nil", err)
	}

	if recv.lastPath != "/v1/traces" {
		t.Errorf("path = %q, want /v1/traces", recv.lastPath)
	}
	if recv.lastMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", recv.lastMethod)
	}
	if recv.lastContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", recv.lastContentType)
	}
	if recv.lastHeaderRepo != testSpansRepoID {
		t.Errorf("X-Mgmt-Repo-ID = %q, want %q", recv.lastHeaderRepo, testSpansRepoID)
	}

	req := recv.LastRequest()
	if req == nil {
		t.Fatalf("receiver did not parse request body")
	}
	if len(req.ResourceSpans) != 1 {
		t.Fatalf("resourceSpans count = %d, want 1", len(req.ResourceSpans))
	}
	// Routing attribute injection.
	var foundServiceName, foundMgmtRepoID bool
	for _, a := range req.ResourceSpans[0].Resource.Attributes {
		if a.Key == "service.name" && a.Value.StringValue == "mgmt-api-replay/"+testSpansRepoID {
			foundServiceName = true
		}
		if a.Key == "mgmt.repo_id" && a.Value.StringValue == testSpansRepoID {
			foundMgmtRepoID = true
		}
	}
	if !foundServiceName {
		t.Errorf("service.name resource attribute missing or wrong: %+v",
			req.ResourceSpans[0].Resource.Attributes)
	}
	if !foundMgmtRepoID {
		t.Errorf("mgmt.repo_id resource attribute missing: %+v",
			req.ResourceSpans[0].Resource.Attributes)
	}

	if len(req.ResourceSpans[0].ScopeSpans) != 1 ||
		len(req.ResourceSpans[0].ScopeSpans[0].Spans) != 1 {
		t.Fatalf("scopeSpans/spans count wrong: %+v", req.ResourceSpans[0].ScopeSpans)
	}
	span := req.ResourceSpans[0].ScopeSpans[0].Spans[0]
	if span.TraceID != testTraceID {
		t.Errorf("span traceId = %q, want %q", span.TraceID, testTraceID)
	}
	if span.StartTimeUnixNano != "1700000000000000000" {
		t.Errorf("span startTimeUnixNano = %q, want string-encoded uint64",
			span.StartTimeUnixNano)
	}
	// Attributes carried + sorted deterministically.
	got := map[string]string{}
	for _, a := range span.Attributes {
		got[a.Key] = a.Value.StringValue
	}
	if got["http.method"] != "GET" || got["http.status"] != "200" {
		t.Errorf("attributes = %+v, want http.method=GET / http.status=200", got)
	}
}

func TestHTTPSpanForwarder_customServiceNamePrefix(t *testing.T) {
	t.Parallel()
	recv := newFakeReceiver(http.StatusOK)
	srv := httptest.NewServer(recv)
	defer srv.Close()

	fwd, err := NewHTTPSpanForwarder(HTTPSpanForwarderConfig{
		BaseURL:           srv.URL,
		HTTPClient:        srv.Client(),
		Logger:            silentLogger(),
		ServiceNamePrefix: "agent-memory:",
	})
	if err != nil {
		t.Fatalf("NewHTTPSpanForwarder: %v", err)
	}
	if err := fwd.ForwardSpans(context.Background(), testSpansRepoID, sampleForwardedSpans()); err != nil {
		t.Fatalf("ForwardSpans: %v", err)
	}
	want := "agent-memory:" + testSpansRepoID
	found := false
	for _, a := range recv.LastRequest().ResourceSpans[0].Resource.Attributes {
		if a.Key == "service.name" && a.Value.StringValue == want {
			found = true
		}
	}
	if !found {
		t.Errorf("custom service.name prefix not applied")
	}
}

func TestHTTPSpanForwarder_emptySpans_noPOST(t *testing.T) {
	t.Parallel()
	recv := newFakeReceiver(http.StatusOK)
	srv := httptest.NewServer(recv)
	defer srv.Close()
	fwd := newTestForwarder(t, srv)

	if err := fwd.ForwardSpans(context.Background(), testSpansRepoID, nil); err != nil {
		t.Fatalf("err = %v, want nil for empty span slice", err)
	}
	if recv.lastMethod != "" {
		t.Errorf("receiver hit with empty slice; method = %q", recv.lastMethod)
	}
}

// -----------------------------------------------------------
// Response handling
// -----------------------------------------------------------

func TestHTTPSpanForwarder_503mapsToBackpressure(t *testing.T) {
	t.Parallel()
	recv := newFakeReceiver(http.StatusServiceUnavailable)
	recv.replyHeaders = http.Header{"Retry-After": []string{"5"}}
	srv := httptest.NewServer(recv)
	defer srv.Close()
	fwd := newTestForwarder(t, srv)

	err := fwd.ForwardSpans(context.Background(), testSpansRepoID, sampleForwardedSpans())
	if !errors.Is(err, ErrSpanIngestorBackpressure) {
		t.Fatalf("err = %v, want ErrSpanIngestorBackpressure", err)
	}
}

func TestHTTPSpanForwarder_4xx_mapsToError(t *testing.T) {
	t.Parallel()
	recv := newFakeReceiver(http.StatusBadRequest)
	recv.replyBody = `{"error":"bad shape"}`
	srv := httptest.NewServer(recv)
	defer srv.Close()
	fwd := newTestForwarder(t, srv)

	err := fwd.ForwardSpans(context.Background(), testSpansRepoID, sampleForwardedSpans())
	if err == nil {
		t.Fatalf("err = nil, want error on 400")
	}
	if errors.Is(err, ErrSpanIngestorBackpressure) {
		t.Errorf("err = %v, must NOT be ErrSpanIngestorBackpressure on 400", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("err = %v, want status code in message", err)
	}
}

func TestHTTPSpanForwarder_5xx_mapsToError(t *testing.T) {
	t.Parallel()
	recv := newFakeReceiver(http.StatusBadGateway)
	srv := httptest.NewServer(recv)
	defer srv.Close()
	fwd := newTestForwarder(t, srv)

	err := fwd.ForwardSpans(context.Background(), testSpansRepoID, sampleForwardedSpans())
	if err == nil {
		t.Fatalf("err = nil, want error on 502")
	}
	if errors.Is(err, ErrSpanIngestorBackpressure) {
		t.Errorf("err = %v, must NOT be backpressure on 502", err)
	}
}

func TestHTTPSpanForwarder_2xxAlternates_accepted(t *testing.T) {
	t.Parallel()
	for _, code := range []int{200, 202, 204} {
		code := code
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			recv := newFakeReceiver(code)
			srv := httptest.NewServer(recv)
			defer srv.Close()
			fwd := newTestForwarder(t, srv)
			if err := fwd.ForwardSpans(context.Background(), testSpansRepoID, sampleForwardedSpans()); err != nil {
				t.Errorf("status %d -> err %v, want nil", code, err)
			}
		})
	}
}

func TestHTTPSpanForwarder_contextCancelled_returnsError(t *testing.T) {
	t.Parallel()
	// Slow receiver so the client-side context deadline fires
	// before the handler returns. Using a bounded sleep (rather
	// than blocking on r.Context().Done() forever) keeps
	// srv.Close() from hanging when the test ends — some Go
	// httptest server builds don't reliably propagate client
	// cancellation to the request context until the underlying
	// TCP connection is closed.
	handlerDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(func() {
		// Close in a goroutine so a stuck handler never blocks
		// the test from exiting; the 2s upper bound above
		// guarantees the handler will return on its own.
		closed := make(chan struct{})
		go func() {
			srv.Close()
			close(closed)
		}()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Logf("warn: httptest.Server.Close did not return within 5s")
		}
	})
	fwd := newTestForwarder(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := fwd.ForwardSpans(ctx, testSpansRepoID, sampleForwardedSpans())
	if err == nil {
		t.Fatalf("err = nil, want context error")
	}
	// Best-effort: give the handler a moment to observe the
	// cancellation so srv.Close drains cleanly when the test
	// ends. Not strictly required for correctness.
	select {
	case <-handlerDone:
	case <-time.After(2500 * time.Millisecond):
	}
}

func TestHTTPSpanForwarder_transportError_returnsError(t *testing.T) {
	t.Parallel()
	// Pointer to an URL that will not resolve.
	fwd, err := NewHTTPSpanForwarder(HTTPSpanForwarderConfig{
		BaseURL:    "http://127.0.0.1:1", // closed port
		HTTPClient: &http.Client{Timeout: 200 * time.Millisecond},
		Logger:     silentLogger(),
	})
	if err != nil {
		t.Fatalf("NewHTTPSpanForwarder: %v", err)
	}
	if err := fwd.ForwardSpans(context.Background(), testSpansRepoID, sampleForwardedSpans()); err == nil {
		t.Fatalf("err = nil, want transport error")
	}
}
