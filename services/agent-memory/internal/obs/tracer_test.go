package obs_test

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
)

func TestSetupTracer_NoopWhenEnvUnset(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	res, err := obs.SetupTracer(context.Background(), obs.ServiceNameAgentAPI, logger)
	if err != nil {
		t.Fatalf("SetupTracer: %v", err)
	}
	if res.Exporting {
		t.Errorf("expected Exporting=false when no endpoint set")
	}
	if res.EndpointResolved != "" {
		t.Errorf("expected empty EndpointResolved, got %q", res.EndpointResolved)
	}
	// Shutdown is always callable and returns nil for noop.
	if err := res.Shutdown(context.Background()); err != nil {
		t.Errorf("noop Shutdown: %v", err)
	}
	// The global tracer provider MUST be wired up — recall handlers
	// later call `otel.Tracer(...)` and expect to get back a non-nil
	// tracer (even if its spans are dropped).
	tr := otel.Tracer("test")
	if tr == nil {
		t.Fatal("global tracer is nil")
	}
	_, span := tr.Start(context.Background(), "noop")
	span.End()
}

func TestSetupTracer_ExportingWhenEnvSet(t *testing.T) {
	// Setting a syntactically valid endpoint is enough to
	// validate the setup path — the BatchSpanProcessor
	// retries silently on connection failure, so no actual
	// listener is needed.
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:14318")
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	res, err := obs.SetupTracer(context.Background(), obs.ServiceNameSpanIngestor, logger)
	if err != nil {
		t.Fatalf("SetupTracer: %v", err)
	}
	if !res.Exporting {
		t.Errorf("expected Exporting=true when endpoint set")
	}
	if !strings.Contains(res.EndpointResolved, "14318") {
		t.Errorf("expected endpoint echo to contain port, got %q", res.EndpointResolved)
	}
	// Shutdown should be quick because we never sent a span.
	if err := res.Shutdown(context.Background()); err != nil {
		// Errors are tolerable here (Collector not reachable)
		// but the call itself MUST return without panic.
		t.Logf("Shutdown returned error (expected when Collector absent): %v", err)
	}
}

func TestSetupTracer_TracesEndpointPreferredOverGeneric(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://generic:9999")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://traces:4318")
	res, err := obs.SetupTracer(context.Background(), obs.ServiceNameConsolidator, nil)
	if err != nil {
		t.Fatalf("SetupTracer: %v", err)
	}
	defer func() { _ = res.Shutdown(context.Background()) }()
	if !strings.Contains(res.EndpointResolved, "traces:4318") {
		t.Errorf("expected traces-specific endpoint to win, got %q", res.EndpointResolved)
	}
}

func TestSetupTracer_BareHostPort(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "localhost:4318")
	res, err := obs.SetupTracer(context.Background(), obs.ServiceNameMgmtAPI, nil)
	if err != nil {
		t.Fatalf("SetupTracer: %v", err)
	}
	defer func() { _ = res.Shutdown(context.Background()) }()
	if !res.Exporting {
		t.Errorf("expected Exporting=true for bare host:port")
	}
}

func TestSetupTracer_BadEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://%41:bad")
	res, err := obs.SetupTracer(context.Background(), obs.ServiceNameConceptPromoter, nil)
	if err == nil {
		t.Fatal("expected error for malformed endpoint")
	}
	// Stage 8.3 iter-3 evaluator fix #3 — on configuration
	// error, callers must still be able to `defer res.Shutdown(ctx)`
	// without a nil-deref panic. Verify the contract.
	if res.Shutdown == nil {
		t.Fatal("error path returned nil Shutdown; deferring it would panic")
	}
	if err := res.Shutdown(context.Background()); err != nil {
		t.Errorf("noop Shutdown returned err=%v; want nil", err)
	}
	if res.Tracer == nil {
		t.Fatal("error path returned nil Tracer; tracer.Start would panic")
	}
	// And the noop tracer must produce a real span (no nil
	// span returned from Start).
	_, span := res.Tracer.Start(context.Background(), "noop-probe")
	if span == nil {
		t.Fatal("noop tracer returned nil span from Start")
	}
	span.End()
	if res.Exporting {
		t.Errorf("error path must report Exporting=false; got true")
	}
}
