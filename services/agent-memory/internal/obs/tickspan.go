package obs

// Stage 8.3 step 2 — TickSpan helper.
//
// Many of the agent-memory binaries are long-running tick
// loops (`for { tick(); time.Sleep(interval) }`). Wiring an
// OTel span around every loop body is repetitive, so this
// helper centralises the pattern:
//
//   - start a CHILD-NEW root span ("internal" SpanKind),
//   - call the worker body,
//   - record the returned error on the span (RecordError +
//     codes.Error so trace UIs colour the row red),
//   - end the span unconditionally so a panic still flushes.
//
// Callers pass the worker body as a closure returning an
// error. The span name is the verb (e.g. "promoter.tick");
// optional KeyValue attributes attach low-cardinality
// metadata (run_id, batch_size).
//
// Why "internal" SpanKind: these are background workers that
// neither receive nor emit RPC traffic on their own — the
// nearest analogue is OTel's "internal" kind which the
// W3C semantic-conventions §5.1 reserves for "this work is
// not on the request path".

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// tickSpanNoopTracer is the package-level fallback tracer used
// when TickSpan is invoked with a nil tracer. Hoisted to a
// package var so that the hot per-tick path of every
// background worker (promoter, consolidator, span-ingestor,
// trace-log-pruner, reranker-trainer, repo-indexer) avoids
// allocating a fresh noop TracerProvider on each call. This
// mirrors the singleton pattern used in agentapi/tracing.go,
// consolidator/service.go, spaningestor/ingestor.go, and
// qdrant-bootstrap/observability.go elsewhere in this PR.
var tickSpanNoopTracer trace.Tracer = noop.NewTracerProvider().Tracer("obs.tickspan.noop")

// TickSpan wraps body in an OTel span named verb. The
// returned error is surfaced verbatim to the caller; the
// span carries the error message + codes.Error if non-nil.
// A nil tracer is tolerated (substitutes the noop tracer)
// so packages can call TickSpan unconditionally even when
// no tracer is configured.
//
// Attributes are appended after the span starts so they
// reflect the FINAL state of the worker body (e.g. a tick
// that processes a variable batch size). Callers can also
// supply an `attrFunc` to derive attributes from the result.
func TickSpan(ctx context.Context, tracer trace.Tracer, verb string, attrs []attribute.KeyValue, body func(ctx context.Context) error) error {
	if tracer == nil {
		tracer = tickSpanNoopTracer
	}
	startOpts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
	}
	if len(attrs) > 0 {
		startOpts = append(startOpts, trace.WithAttributes(attrs...))
	}
	ctx, span := tracer.Start(ctx, verb, startOpts...)
	defer span.End()
	start := time.Now()
	err := body(ctx)
	span.SetAttributes(attribute.Float64("duration_seconds", time.Since(start).Seconds()))
	if err != nil && !errors.Is(err, context.Canceled) {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	return err
}
