package agentapi

// Stage 8.3 step 2 — per-verb OpenTelemetry tracer wiring.
//
// The iter-1 implementation set up a global TracerProvider via
// `obs.SetupTracer` but never actually started spans, so the
// `agent.recall` / `agent.observe` / `agent.expand` /
// `agent.summarize` operations produced zero trace data. The
// iter-2 evaluator (finding #1) called this out: Stage 8.3
// requires real operational spans, not just an exporter.
//
// This file plumbs a `trace.Tracer` onto `Service` and
// `ObserveService` so each verb handler can wrap its body
// with a single span. The returned context is threaded into
// every downstream call (embedding, qdrant, DB, reranker)
// inside the verb so child spans naturally chain underneath
// the verb span when downstream packages start their own.
//
// Why `trace.Tracer` rather than another callback:
// `go.opentelemetry.io/otel/trace` is the abstractions-only
// package; it does NOT pull in the SDK. The verb code can
// therefore depend on the abstraction directly without
// inflating the unit-test build cost.
//
// Why a noop fallback: tests that don't wire an observer
// MUST still be able to call `s.tracer.Start(ctx, ...)`
// without a nil deref. We return a no-op tracer from the
// `tracerOrNoop` helper so the verb call site stays free of
// `if s.tracer != nil` branches.

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// noopTracer is the package-level singleton returned when no
// tracer has been wired. Allocating once at init avoids
// per-call cost.
var noopTracer trace.Tracer = noop.NewTracerProvider().Tracer("agent-memory.noop")

// tracerOrNoop normalises a nil tracer to the package noop so
// the verb call sites can call `tracer.Start(...)`
// unconditionally.
func tracerOrNoop(t trace.Tracer) trace.Tracer {
	if t == nil {
		return noopTracer
	}
	return t
}

// startVerbSpan is the shared entry point each verb uses to
// open its operational span. Centralising the call signature
// keeps the verb files brief and gives the span-naming
// convention a single point of audit.
//
// `name` MUST be one of the §8.3 canonical operation names
// (`agent.recall`, `agent.observe`, `agent.expand`,
// `agent.summarize`, `mgmt.ingest_spans`); the dashboard and
// alert rules look these up exactly. `repoID` is a
// LOW-cardinality attribute (the repo set is bounded by the
// operator's manifest); we deliberately avoid attaching
// `query` strings or per-call IDs that would blow up the
// trace store's cardinality.
func startVerbSpan(ctx context.Context, t trace.Tracer, name, repoID string) (context.Context, trace.Span) {
	return tracerOrNoop(t).Start(ctx, name, trace.WithAttributes(
		attribute.String("agent_memory.verb", name),
		attribute.String("agent_memory.repo_id", repoID),
	))
}

// endVerbSpan records the verb's terminal status on the span.
// Called from the verb's `defer` block AFTER the named-return
// wraps have run, so the span captures the final caller-
// visible outcome (status + degraded reason), not the
// intermediate happy-path return.
//
// `degradedReason` is the empty string when the response is
// not degraded; the production verbs pass
// `resp.DegradedReason` (a closed-set string from
// `internal/degraded`). We record it as an attribute (not as
// the span status) so a degraded-but-served response is
// still a `codes.Ok` from the tracing point of view; only an
// actual error sets `codes.Error`.
func endVerbSpan(span trace.Span, err error, degradedReason string) {
	if span == nil {
		return
	}
	if err != nil {
		span.RecordError(err)
		// Keep the status description short and stable so
		// trace UIs can group by it; the full error is on
		// the recorded exception event.
		span.SetStatus(codes.Error, classifyVerbError(err))
	} else if degradedReason != "" {
		span.SetAttributes(attribute.String("agent_memory.degraded_reason", degradedReason))
	}
	span.End()
}

// classifyVerbError collapses an error to a stable label for
// the span status description. We special-case the known
// agentapi sentinels so trace UIs can facet by reason
// without inheriting per-call detail strings.
func classifyVerbError(err error) string {
	switch {
	case errors.Is(err, ErrEmptyQuery),
		errors.Is(err, ErrInvalidKind),
		errors.Is(err, ErrInvalidK),
		errors.Is(err, ErrInvalidExpandNodeID),
		errors.Is(err, ErrInvalidExpandDirection),
		errors.Is(err, ErrInvalidExpandDepth),
		errors.Is(err, ErrSummarizeMissingTarget),
		errors.Is(err, ErrSummarizeAmbiguousTarget),
		errors.Is(err, ErrSummarizeMaxTokensRange),
		errors.Is(err, ErrSummarizeRepoIDRequired),
		errors.Is(err, ErrInvalidObservationRole),
		errors.Is(err, ErrInvalidObservationTarget),
		errors.Is(err, ErrInvalidOutcome),
		errors.Is(err, ErrMissingSessionID),
		errors.Is(err, ErrMissingContextID),
		errors.Is(err, ErrContextNotFound):
		return "invalid_argument"
	case errors.Is(err, ErrSummarizeUnconfigured),
		errors.Is(err, ErrExpandUnavailable):
		return "unimplemented"
	default:
		return "internal"
	}
}

// WithTracer wires an OpenTelemetry tracer onto the recall-
// side `Service`. The verb handlers (`Recall`, `Expand`,
// `Summarize`) will create a span per call under this
// tracer. A nil tracer is a no-op (the package-noop is
// substituted at call time).
//
// Wired from `cmd/agent-api/main.go` with the result of
// `obs.SetupTracer(...).Tracer` so the OTLP exporter
// configured by the binary picks the spans up.
func WithTracer(t trace.Tracer) Option {
	return func(s *Service) {
		s.tracer = t
	}
}

// WithObserveTracer wires the OpenTelemetry tracer onto the
// `ObserveService`. The `Observe` verb opens a span per
// call. A nil tracer is a no-op.
func WithObserveTracer(t trace.Tracer) ObserveOption {
	return func(s *ObserveService) {
		s.tracer = t
	}
}
