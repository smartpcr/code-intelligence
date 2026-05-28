package telemetry

import (
	"context"
	"testing"

	"github.com/gofrs/uuid"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/evaluator"
)

// newRecordingTracer wires an in-process OTel SDK with an
// [tracetest.InMemoryExporter] so the test can introspect
// every span attribute set by [AnnotateEvalGateSpan] /
// [AnnotateVerbDefaults] without going through the OTLP
// gRPC dial. Returns the tracer + exporter + cleanup
// closure.
func newRecordingTracer(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(exporter),
	)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	return tp, exporter
}

func TestAnnotateEvalGateSpan_StampsAllFourCanonicalAttrs(t *testing.T) {
	tp, exporter := newRecordingTracer(t)
	tracer := tp.Tracer("telemetry-test")

	ctx, span := tracer.Start(context.Background(), "eval.gate")
	pvid := uuid.Must(uuid.NewV4())
	result := evaluator.EvaluateResult{
		PolicyVersionID: pvid,
		Degraded:        true,
		DegradedReason:  evaluator.DegradedReasonSamplesPending,
		Verdict:         evaluator.VerdictWarn,
	}
	AnnotateEvalGateSpan(ctx, result)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans; want 1", len(spans))
	}
	attrs := attrMap(spans[0].Attributes)
	if got := attrs[AttrPolicyVersionID]; got != pvid.String() {
		t.Errorf("attr %s = %q; want %q", AttrPolicyVersionID, got, pvid.String())
	}
	if got := attrs[AttrDegraded]; got != "true" {
		t.Errorf("attr %s = %q; want \"true\"", AttrDegraded, got)
	}
	if got := attrs[AttrDegradedReason]; got != "samples_pending" {
		t.Errorf("attr %s = %q; want %q", AttrDegradedReason, got, "samples_pending")
	}
	if got := attrs[AttrVerdict]; got != "warn" {
		t.Errorf("attr %s = %q; want %q", AttrVerdict, got, "warn")
	}
}

func TestAnnotateEvalGateSpan_ZeroUUIDProjectsEmptyString(t *testing.T) {
	tp, exporter := newRecordingTracer(t)
	tracer := tp.Tracer("telemetry-test")

	ctx, span := tracer.Start(context.Background(), "eval.gate")
	AnnotateEvalGateSpan(ctx, evaluator.EvaluateResult{
		Verdict: evaluator.VerdictPass,
	})
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans; want 1", len(spans))
	}
	attrs := attrMap(spans[0].Attributes)
	if got, ok := attrs[AttrPolicyVersionID]; !ok || got != "" {
		t.Errorf("attr %s = %q (present=%v); want empty string", AttrPolicyVersionID, got, ok)
	}
	if got := attrs[AttrVerdict]; got != "pass" {
		t.Errorf("attr %s = %q; want \"pass\"", AttrVerdict, got)
	}
}

func TestAnnotateEvalGateSpan_NilCtxIsNoop(t *testing.T) {
	// Must not panic; must not allocate. The function is
	// called from handler paths that may receive a
	// background context where no span is bound.
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AnnotateEvalGateSpan(nil ctx) panicked: %v", r)
		}
	}()
	AnnotateEvalGateSpan(context.TODO(), evaluator.EvaluateResult{})
	//nolint:staticcheck // intentionally testing the nil-ctx defensive guard
	AnnotateEvalGateSpan(nil, evaluator.EvaluateResult{})
}

func TestAnnotateVerbDefaults_StampsEmptyAttrs(t *testing.T) {
	tp, exporter := newRecordingTracer(t)
	tracer := tp.Tracer("telemetry-test")

	ctx, span := tracer.Start(context.Background(), "mgmt.register_repo")
	AnnotateVerbDefaults(ctx)
	span.End()

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans; want 1", len(spans))
	}
	attrs := attrMap(spans[0].Attributes)
	for _, k := range []string{AttrPolicyVersionID, AttrDegradedReason, AttrVerdict} {
		got, ok := attrs[k]
		if !ok {
			t.Errorf("attr %s missing; want empty-string default", k)
		}
		if got != "" {
			t.Errorf("attr %s = %q; want empty string default", k, got)
		}
	}
	if got := attrs[AttrDegraded]; got != "false" {
		t.Errorf("attr %s = %q; want \"false\" default", AttrDegraded, got)
	}
}

// attrMap stringifies a span attribute set for assertions.
// Bools render as "true"/"false" so the test matrix is
// uniform across stringly + boolean keys.
func attrMap(attrs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}
