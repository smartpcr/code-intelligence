package api

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// OTelTracer is the production [Tracer] implementation backed
// by the OpenTelemetry Go SDK. The composition root constructs
// one (passing in a real `trace.Tracer` obtained from the
// configured `trace.TracerProvider`) and the gateway emits
// real OTel spans for every authenticated request -- the
// spans land in whatever exporter the SDK was wired to
// (OTLP, Jaeger, Zipkin, stdout, ...).
//
// # Why a thin adapter
//
// The gateway uses the package-local [Tracer] / [Span]
// interfaces (rather than depending directly on
// `go.opentelemetry.io/otel/trace`) so:
//
//   - Tests can drive a [RecordingTracer] without spinning
//     up the OTel SDK.
//   - The composition root keeps a free hand to plug an
//     alternative tracing backend (Datadog,
//     custom-protocol) by writing one more adapter.
//   - The package's import surface stays narrow when the
//     gateway is consumed from a service that elected not
//     to use OTel.
//
// This adapter is the production-grade choice. The
// [SlogTracer] remains for in-process dev / unit-test paths
// where no collector runs.
type OTelTracer struct {
	tracer trace.Tracer
}

// NewOTelTracer wraps an `otel/trace.Tracer` in the
// gateway's [Tracer] interface. Panics on nil tracer (the
// composition root should pass a real one obtained via
// `otel.Tracer("gateway")` or
// `provider.Tracer("gateway")`).
func NewOTelTracer(tracer trace.Tracer) *OTelTracer {
	if tracer == nil {
		panic("api.NewOTelTracer: nil trace.Tracer")
	}
	return &OTelTracer{tracer: tracer}
}

// NewOTelTracerFromGlobal returns an [OTelTracer] backed by
// the OTel SDK's GLOBAL tracer provider
// (`otel.GetTracerProvider()`). Convenient for composition
// roots that have already configured the global provider
// from environment variables (OTLP exporter via
// `OTEL_EXPORTER_OTLP_ENDPOINT`, etc.).
//
// The instrumentation name `"clean-code/internal/api"` lets
// dashboards filter by gateway-emitted spans separately
// from downstream verb-emitted spans.
func NewOTelTracerFromGlobal() *OTelTracer {
	return NewOTelTracer(otel.Tracer("clean-code/internal/api"))
}

// StartSpan implements [Tracer]. The returned context carries
// the active OTel span so downstream verb handlers that
// further instrument with `otel.SpanFromContext` see the
// gateway's span as their parent.
func (t *OTelTracer) StartSpan(ctx context.Context, name string) (context.Context, Span) {
	ctx, sp := t.tracer.Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindServer),
		trace.WithTimestamp(time.Now()),
	)
	return ctx, &otelSpan{span: sp}
}

// otelSpan wraps an `otel/trace.Span` in the gateway's
// [Span] interface. The wrapper is a single-field struct so
// the indirection is essentially free on the hot path.
type otelSpan struct {
	span trace.Span
}

// SetAttribute implements [Span]. The implementation maps the
// gateway's `any`-typed attribute API to the closest OTel
// `attribute.KeyValue` type. Unsupported types fall back to
// `attribute.String(key, fmt.Sprint(value))` so an
// instrumented attribute is never silently dropped.
func (s *otelSpan) SetAttribute(key string, value any) {
	if s == nil || s.span == nil || key == "" {
		return
	}
	s.span.SetAttributes(attrKVOf(key, value))
}

// RecordError implements [Span]. Errors recorded via this
// path surface in OTel as an `exception` span event and
// stamp the span status to `Error` so dashboards highlight
// the span.
func (s *otelSpan) RecordError(err error) {
	if s == nil || s.span == nil || err == nil {
		return
	}
	s.span.RecordError(err)
	s.span.SetStatus(codes.Error, err.Error())
}

// End implements [Span]. The OTel SDK ships the span to the
// configured exporter on End().
func (s *otelSpan) End() {
	if s == nil || s.span == nil {
		return
	}
	s.span.End()
}

// attrKVOf converts an (key, any) pair to an
// `attribute.KeyValue`. The conversion covers the types the
// gateway actually stamps (string, int, bool, float, time);
// unknown types fall back to their `fmt.Sprint` form so an
// attribute is never silently dropped.
func attrKVOf(key string, value any) attribute.KeyValue {
	switch v := value.(type) {
	case string:
		return attribute.String(key, v)
	case bool:
		return attribute.Bool(key, v)
	case int:
		return attribute.Int(key, v)
	case int64:
		return attribute.Int64(key, v)
	case float64:
		return attribute.Float64(key, v)
	case []string:
		return attribute.StringSlice(key, v)
	case nil:
		return attribute.String(key, "")
	default:
		return attribute.String(key, fmt.Sprintf("%v", v))
	}
}
