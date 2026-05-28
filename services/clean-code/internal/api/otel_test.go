package api

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestOTelTracer_StartSpan_StampsAttributesAndEnd(t *testing.T) {
	t.Parallel()
	rec := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	tracer := NewOTelTracer(provider.Tracer("test"))
	ctx, sp := tracer.StartSpan(context.Background(), "http.gateway")
	sp.SetAttribute("verb", "mgmt.register_repo")
	sp.SetAttribute("repo_id", "r-7")
	sp.SetAttribute("http.status_code", 200)
	sp.End()
	if !trace.SpanFromContext(ctx).SpanContext().IsValid() {
		t.Errorf("context has no valid span")
	}
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	s := spans[0]
	if s.Name() != "http.gateway" {
		t.Errorf("name=%q, want http.gateway", s.Name())
	}
	want := map[attribute.Key]attribute.Value{
		"verb":             attribute.StringValue("mgmt.register_repo"),
		"repo_id":          attribute.StringValue("r-7"),
		"http.status_code": attribute.IntValue(200),
	}
	for _, kv := range s.Attributes() {
		if w, ok := want[kv.Key]; ok {
			if kv.Value != w {
				t.Errorf("attr %s = %v, want %v", kv.Key, kv.Value.Emit(), w.Emit())
			}
			delete(want, kv.Key)
		}
	}
	for k := range want {
		t.Errorf("missing attribute %s", k)
	}
}

func TestOTelTracer_RecordErrorMarksErrorStatus(t *testing.T) {
	t.Parallel()
	rec := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	defer func() { _ = provider.Shutdown(context.Background()) }()
	tracer := NewOTelTracer(provider.Tracer("test"))
	_, sp := tracer.StartSpan(context.Background(), "http.gateway")
	sp.RecordError(errors.New("downstream blew up"))
	sp.End()
	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(spans))
	}
	events := spans[0].Events()
	if len(events) == 0 {
		t.Errorf("no error event recorded")
	}
	if spans[0].Status().Code.String() != "Error" {
		t.Errorf("status=%v, want Error", spans[0].Status().Code)
	}
}

func TestNewOTelTracer_PanicsOnNil(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Errorf("NewOTelTracer(nil) did not panic")
		}
	}()
	NewOTelTracer(nil)
}

func TestNewOTelTracerFromGlobal_ReturnsAdapter(t *testing.T) {
	t.Parallel()
	adapter := NewOTelTracerFromGlobal()
	if adapter == nil || adapter.tracer == nil {
		t.Fatalf("NewOTelTracerFromGlobal returned an unusable adapter")
	}
}

func TestOTelSpan_NilSafe(t *testing.T) {
	t.Parallel()
	// Ensure nil-guards do not panic.
	var sp *otelSpan
	sp.SetAttribute("k", "v")
	sp.RecordError(errors.New("x"))
	sp.End()
}

func TestAttrKVOf_TypeCoverage(t *testing.T) {
	t.Parallel()
	cases := []struct {
		v    any
		want attribute.Value
	}{
		{"s", attribute.StringValue("s")},
		{true, attribute.BoolValue(true)},
		{42, attribute.IntValue(42)},
		{int64(43), attribute.Int64Value(43)},
		{3.14, attribute.Float64Value(3.14)},
		{[]string{"a", "b"}, attribute.StringSliceValue([]string{"a", "b"})},
		{nil, attribute.StringValue("")},
	}
	for _, c := range cases {
		got := attrKVOf("k", c.v)
		if got.Value != c.want {
			t.Errorf("attrKVOf(%v)=%v, want %v", c.v, got.Value.Emit(), c.want.Emit())
		}
	}
	// Fallback for unknown type goes through fmt.Sprint.
	got := attrKVOf("k", struct{ N int }{N: 7})
	if got.Value.Type() != attribute.STRING {
		t.Errorf("fallback type=%v, want STRING", got.Value.Type())
	}
}
