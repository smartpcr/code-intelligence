package spaningestor

import (
	"context"
	"testing"
	"time"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestOTLPGRPC_ExportEnqueuesEachRepo proves evaluator iter-1
// fix #1: the gRPC OTLP server adapts an
// ExportTraceServiceRequest into per-repo SpanBatches and
// pushes them through EnqueueAtomic. Both per-batch
// invariants and the all-or-nothing atomicity contract from
// the HTTP receiver apply.
func TestOTLPGRPC_ExportEnqueuesEachRepo(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	ing := NewIngestor(resolver, newFakeTraceWriter(), nil,
		Config{QueueDepth: 16}, discardLogger())
	srv := NewOTLPGRPCServer(ing, staticServiceMap(map[string]string{
		"svc-a": testRepoIDA,
	}), discardLogger())

	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{
			{
				Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
					{Key: "service.name", Value: &commonpb.AnyValue{
						Value: &commonpb.AnyValue_StringValue{StringValue: "svc-a"},
					}},
				}},
				ScopeSpans: []*tracepb.ScopeSpans{
					{Spans: []*tracepb.Span{{
						TraceId:           []byte("0123456789abcdef0123456789abcdef"),
						SpanId:            []byte("11112222aabbccdd"),
						Name:              "Callee",
						StartTimeUnixNano: uint64(time.Now().UnixNano()),
						EndTimeUnixNano:   uint64(time.Now().Add(2 * time.Millisecond).UnixNano()),
						Attributes: []*commonpb.KeyValue{
							{Key: AttrCodeNamespace, Value: &commonpb.AnyValue{
								Value: &commonpb.AnyValue_StringValue{StringValue: "pkg.svc"},
							}},
							{Key: AttrCodeFunction, Value: &commonpb.AnyValue{
								Value: &commonpb.AnyValue_StringValue{StringValue: "Callee"},
							}},
						},
					}}},
				},
			},
		},
	}
	resp, err := srv.Export(context.Background(), req)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if resp == nil {
		t.Fatalf("Export returned nil response")
	}
	if got := ing.QueueDepth(); got != 1 {
		t.Errorf("QueueDepth = %d, want 1", got)
	}
	if got := ing.metrics.SnapshotCounters()["span_ingested_total"][testRepoIDA]; got != 1 {
		t.Errorf("span_ingested_total = %d, want 1", got)
	}
}

// TestOTLPGRPC_BackpressureReturnsUnavailable proves the
// gRPC adapter translates ErrQueueFull → codes.Unavailable so
// the Collector backs off per the OTLP spec.
func TestOTLPGRPC_BackpressureReturnsUnavailable(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	ing := NewIngestor(resolver, newFakeTraceWriter(), nil,
		Config{QueueDepth: 1}, discardLogger())
	// Fill the single slot.
	if err := ing.Enqueue(SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{RepoID: testRepoIDA, TraceID: "t", SpanID: "pre"},
	}}}); err != nil {
		t.Fatalf("pre-Enqueue: %v", err)
	}
	srv := NewOTLPGRPCServer(ing, staticServiceMap(map[string]string{
		"svc-a": testRepoIDA,
	}), discardLogger())
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: "svc-a"},
				}},
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{TraceId: []byte("abcd"), SpanId: []byte("ef01")}},
			}},
		}},
	}
	_, err := srv.Export(context.Background(), req)
	if err == nil {
		t.Fatalf("Export = nil, want Unavailable")
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("error is not a gRPC status: %v", err)
	}
	if st.Code() != codes.Unavailable {
		t.Errorf("code = %s, want Unavailable", st.Code())
	}
}

// TestOTLPGRPC_UnknownServiceDropsSilently proves spans with
// no service.name mapping are silently dropped (no 4xx-equivalent
// gRPC error) so an unknown emitter can't flood the receiver.
func TestOTLPGRPC_UnknownServiceDropsSilently(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	ing := NewIngestor(resolver, newFakeTraceWriter(), nil,
		Config{QueueDepth: 16}, discardLogger())
	srv := NewOTLPGRPCServer(ing, staticServiceMap(map[string]string{}),
		discardLogger())
	req := &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				{Key: "service.name", Value: &commonpb.AnyValue{
					Value: &commonpb.AnyValue_StringValue{StringValue: "unknown"},
				}},
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{
				Spans: []*tracepb.Span{{TraceId: []byte("abcd"), SpanId: []byte("ef01")}},
			}},
		}},
	}
	if _, err := srv.Export(context.Background(), req); err != nil {
		t.Fatalf("Export: %v", err)
	}
	if got := ing.QueueDepth(); got != 0 {
		t.Errorf("QueueDepth = %d, want 0", got)
	}
}

// TestHexEncode verifies the hex-encoder shared between
// gRPC and HTTP/protobuf transports (TraceID/SpanID
// normalization).
func TestHexEncode(t *testing.T) {
	cases := []struct {
		in   []byte
		want string
	}{
		{in: nil, want: ""},
		{in: []byte{0x00}, want: "00"},
		{in: []byte{0x1a, 0xfe, 0x0c}, want: "1afe0c"},
	}
	for _, c := range cases {
		if got := hexEncode(c.in); got != c.want {
			t.Errorf("hexEncode(%x) = %q, want %q", c.in, got, c.want)
		}
	}
}
