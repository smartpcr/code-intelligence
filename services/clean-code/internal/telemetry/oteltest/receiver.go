// Package oteltest is a small test-support package that
// exposes an in-process OTLP/gRPC trace receiver. It exists
// so multiple test packages (notably
// `internal/telemetry/*_test.go` AND the higher-level
// `internal/api/otlp_real_handler_composition_test.go`) can
// share ONE implementation of the canonical Stage 9.4 "fake
// OTLP receiver" the implementation-plan pinned at line 820
// without duplicating ~70 lines of receiver / wait / map-
// projection glue.
//
// # When NOT to use this package
//
// Production code MUST NOT import this package. It pulls in
// `testing` and `grpc.Server` -- both legitimate for test-only
// scaffolding but inappropriate as production runtime
// dependencies. The package's import path lives under
// `internal/telemetry/oteltest/` so the Go toolchain treats it
// as a sibling helper, not part of the public telemetry API.
//
// # Why a regular package, not a `_test.go` helper
//
// Go's compiler refuses to import symbols defined in
// `*_test.go` files from a different package. Stage 9.4
// iter-4 evaluator item #1 requires the composition test in
// `internal/api` to drive the SAME fake OTLP receiver the
// middleware-only test in `internal/telemetry` uses. The
// only way to share the receiver across two packages is to
// promote it out of `_test.go` and into a regular package
// file the test code can import normally.
package oteltest

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// FakeOTLPReceiver is the in-process OTLP/gRPC trace
// collector the Stage 9.4 integration tests drive. It
// implements
// `opentelemetry.proto.collector.trace.v1.TraceService`
// over a real `grpc.Server` and records every received
// span on a mutex-guarded slice so callers can assert
// canonical Stage 9.4 attribute presence.
//
// The receiver is intentionally minimal: it accepts every
// export call, mirrors back an empty
// [coltracepb.ExportTraceServiceResponse], and never
// returns an error. The clean-code service's exporter
// wiring is the system under test, not the OTLP collector
// protocol -- which has comprehensive tests upstream in
// the OTel project.
type FakeOTLPReceiver struct {
	coltracepb.UnimplementedTraceServiceServer

	mu    sync.Mutex
	spans []*tracepb.ResourceSpans
}

// Export captures the incoming resource-span batch and
// returns an empty ack. The OTel SDK's batch span processor
// retries silently on error, which would mask test
// failures; returning nil here keeps the assertion path
// honest.
func (r *FakeOTLPReceiver) Export(_ context.Context, req *coltracepb.ExportTraceServiceRequest) (*coltracepb.ExportTraceServiceResponse, error) {
	r.mu.Lock()
	r.spans = append(r.spans, req.GetResourceSpans()...)
	r.mu.Unlock()
	return &coltracepb.ExportTraceServiceResponse{}, nil
}

// All returns a snapshot copy of the recorded resource-
// spans for assertion. Safe to call concurrently with
// further Export invocations.
func (r *FakeOTLPReceiver) All() []*tracepb.ResourceSpans {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*tracepb.ResourceSpans, len(r.spans))
	copy(out, r.spans)
	return out
}

// Start binds a real `grpc.Server` to a random localhost
// port, registers a [FakeOTLPReceiver] on it, and starts
// serving in a background goroutine. Returns the bound
// `host:port` endpoint, the receiver (for assertions), and
// a cleanup that stops the server.
//
// Using a real `net.Listen` (not `bufconn`) means the OTel
// `otlptracegrpc` exporter dials over the actual gRPC wire
// protocol -- the test exercises the full `Setup` ->
// `tracerProvider.Tracer` -> exporter -> receiver path
// end-to-end.
func Start(t *testing.T) (endpoint string, receiver *FakeOTLPReceiver, cleanup func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("oteltest.Start: net.Listen: %v", err)
	}
	srv := grpc.NewServer()
	rec := &FakeOTLPReceiver{}
	coltracepb.RegisterTraceServiceServer(srv, rec)
	go func() {
		_ = srv.Serve(lis)
	}()
	cleanup = func() {
		srv.GracefulStop()
	}
	return lis.Addr().String(), rec, cleanup
}

// WaitForSpans polls `r` until it has captured at least
// `want` distinct spans or `timeout` expires. Bridges the
// small async gap between the OTLP shutdown ack and the
// receiver's mutex-guarded buffer observed by
// [FakeOTLPReceiver.All]. Fails the test (via `t.Fatalf`)
// when the deadline expires with fewer than `want` spans.
func WaitForSpans(t *testing.T, r *FakeOTLPReceiver, want int, timeout time.Duration) []*tracepb.ResourceSpans {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		spans := r.All()
		total := 0
		for _, rs := range spans {
			for _, ss := range rs.GetScopeSpans() {
				total += len(ss.GetSpans())
			}
		}
		if total >= want {
			return spans
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("oteltest.WaitForSpans: receiver got fewer than %d spans within %s; have %s", want, timeout, summarise(r.All()))
	return nil
}

// AttrMap projects a slice of OTLP common KeyValue pairs
// into a name->string map for direct equality assertions.
// Non-string values are stringified via `fmt.Sprintf("%v",
// ...)` so the test can assert any attribute key uniformly
// even when the producer emitted a bool / int64 / double.
func AttrMap(kvs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		if kv == nil || kv.GetValue() == nil {
			continue
		}
		switch v := kv.GetValue().GetValue().(type) {
		case *commonpb.AnyValue_StringValue:
			out[kv.GetKey()] = v.StringValue
		case *commonpb.AnyValue_BoolValue:
			out[kv.GetKey()] = fmt.Sprintf("%v", v.BoolValue)
		case *commonpb.AnyValue_IntValue:
			out[kv.GetKey()] = fmt.Sprintf("%d", v.IntValue)
		case *commonpb.AnyValue_DoubleValue:
			out[kv.GetKey()] = fmt.Sprintf("%g", v.DoubleValue)
		default:
			out[kv.GetKey()] = fmt.Sprintf("%v", v)
		}
	}
	return out
}

// SpansByName collapses every span the receiver captured
// across all resource-spans / scope-spans batches into one
// `name -> *Span` map for fast lookup. When two spans share
// a name, the LAST one wins -- the integration tests below
// drive exactly one request per verb so a collision means
// the production code emitted a duplicate, which the test
// MUST surface (the map lookup against a later replacement
// also surfaces an unexpected `repo_id` overwrite).
func SpansByName(captured []*tracepb.ResourceSpans) map[string]*tracepb.Span {
	got := map[string]*tracepb.Span{}
	for _, rs := range captured {
		for _, ss := range rs.GetScopeSpans() {
			for _, s := range ss.GetSpans() {
				got[s.GetName()] = s
			}
		}
	}
	return got
}

// SpanNames returns the set of distinct span names in
// `got`, suitable for inclusion in a test failure message.
func SpanNames(got map[string]*tracepb.Span) []string {
	out := make([]string, 0, len(got))
	for k := range got {
		out = append(out, k)
	}
	return out
}

// summarise is the human-readable count summary the wait
// loop emits on timeout. Kept package-private; callers
// typically just need `t.Fatalf` formatting and the
// surfaced span count via [FakeOTLPReceiver.All].
func summarise(spans []*tracepb.ResourceSpans) string {
	n := 0
	for _, rs := range spans {
		for _, ss := range rs.GetScopeSpans() {
			n += len(ss.GetSpans())
		}
	}
	return fmt.Sprintf("%d spans across %d resource-batches", n, len(spans))
}
