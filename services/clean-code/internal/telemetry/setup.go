package telemetry

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
)

// ShutdownFunc is the cleanup callback returned by [Setup].
// A composition root MUST defer this with a bounded
// context so the OTel SDK flushes its in-memory span batch
// before SIGTERM evicts the process. Returning nil from
// the func is allowed and indicates a clean shutdown.
type ShutdownFunc func(context.Context) error

// noopShutdown is the [ShutdownFunc] returned by [Setup]
// when telemetry is intentionally disabled (empty endpoint
// or explicit [Disabled] flag).
func noopShutdown(context.Context) error { return nil }

// SetupOptions configures [Setup]. Constructed by composition
// roots; tests use [SetupForTest].
type SetupOptions struct {
	// ServiceName is the `service.name` resource attribute
	// the OTLP exporter stamps on every span. Required.
	// The recommended convention is the binary name
	// (`clean-code-gateway`, `clean-code-aggregator`,
	// ...) so dashboards can filter spans by emitter
	// without joining onto a separate config table.
	ServiceName string

	// ServiceVersion is the `service.version` resource
	// attribute. Optional; defaults to "dev" when empty.
	// A composition root MAY pass the binary's build
	// version (e.g. injected via `-ldflags`).
	ServiceVersion string

	// DialTimeout caps the time spent establishing the
	// gRPC connection to the OTLP collector. The OTel
	// SDK's batch span processor enqueues spans regardless
	// of connectivity (it retries on next flush) so a
	// zero / aggressive timeout is fine; defaults to
	// 5 seconds when zero.
	DialTimeout time.Duration

	// Insecure forces plaintext gRPC. Default true since
	// the canonical local-dev OTel collector
	// (`deploy/local/otel/config.yaml`) accepts insecure
	// gRPC on `localhost:4317`. A production composition
	// root that targets a TLS-fronted collector should
	// set false and arrange certificate trust via the
	// system roots OR via env vars (`OTEL_EXPORTER_OTLP_*`)
	// honoured by the OTLP exporter directly.
	Insecure bool

	// Headers are optional HTTP/gRPC headers to attach to
	// every OTLP export (e.g. an authentication token).
	// nil OR empty disables header injection.
	Headers map[string]string

	// SamplerRatio is the head-based sampling ratio in
	// [0.0, 1.0]. 0 OR negative disables sampling (drops
	// every span); >=1.0 samples everything. Defaults to
	// 1.0 (sample everything) -- the canonical v0 default
	// since the clean-code service emits low-volume spans
	// (a handful per verb call).
	SamplerRatio float64
}

// ErrSetupServiceName is returned by [Setup] when
// [SetupOptions.ServiceName] is empty -- the OTLP exporter
// requires a non-empty `service.name` resource attribute
// per OTel semantic conventions and an empty value
// silently aliases ALL spans to the catch-all
// `unknown_service`.
var ErrSetupServiceName = errors.New("telemetry: Setup: ServiceName is required")

// Setup initialises the OTel SDK with the OTLP gRPC trace
// exporter pointed at `cfg.OTelEndpoint`. The function
// installs the constructed [sdktrace.TracerProvider] as the
// OTel global so any code path that calls
// `otel.Tracer(...)` (e.g. `api.NewOTelTracerFromGlobal()`)
// picks it up automatically.
//
// # Endpoint resolution
//
// When `cfg.OTelEndpoint` is empty the function INSTALLS
// a no-op tracer provider as the global (the SDK default
// when no provider has been installed yet) and returns a
// no-op [ShutdownFunc] with nil error. This is the
// intentional "telemetry disabled" path -- the binary stays
// functional, every span becomes a noop, and
// `AnnotateEvalGateSpan` / `AnnotateVerbDefaults` short-
// circuit on the `IsRecording()` check.
//
// When `cfg.OTelEndpoint` is set, the function builds the
// OTLP gRPC trace exporter, wraps it in a
// [sdktrace.BatchSpanProcessor], and returns the provider's
// `Shutdown` as the cleanup callback.
//
// # Lifecycle
//
// Call [Setup] ONCE per process, BEFORE any code path
// that constructs a tracer. Defer the returned
// [ShutdownFunc] with a bounded context (5 to 10 seconds is
// typical) so the SDK can drain its in-memory span batch on
// graceful shutdown. Calling [Setup] twice replaces the
// global provider; tests should call [SetupForTest] which
// takes care of resetting the global on test cleanup.
func Setup(ctx context.Context, cfg config.Config, opts SetupOptions) (ShutdownFunc, error) {
	if opts.ServiceName == "" {
		return nil, ErrSetupServiceName
	}
	if cfg.OTelEndpoint == "" {
		// Intentional "telemetry disabled" path. The SDK
		// global stays as its built-in noop provider so
		// `otel.Tracer(...)` calls keep working; spans
		// just go nowhere. Composition roots that
		// require telemetry MUST surface the empty
		// endpoint as a startup error themselves -- the
		// telemetry package does NOT decide policy.
		return noopShutdown, nil
	}
	tp, err := newTracerProvider(ctx, cfg.OTelEndpoint, opts)
	if err != nil {
		return nil, fmt.Errorf("telemetry: Setup: %w", err)
	}
	otel.SetTracerProvider(tp)
	// W3C trace-context is the universal cross-process
	// propagator; baggage is included so future cross-
	// service correlation hops are captured. The two-
	// element composite mirrors the OTel SDK
	// recommendation for v1+ pipelines.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown, nil
}

// newTracerProvider builds the configured TracerProvider.
// Split out from [Setup] so test helpers can swap in an
// in-process span processor without going through the OTLP
// exporter dial.
func newTracerProvider(ctx context.Context, endpoint string, opts SetupOptions) (*sdktrace.TracerProvider, error) {
	dialTimeout := opts.DialTimeout
	if dialTimeout <= 0 {
		dialTimeout = 5 * time.Second
	}
	exporterOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
	}
	if opts.Insecure || isLocalEndpoint(endpoint) {
		// `WithInsecure()` is the OTel-canonical way to
		// disable client transport security for the
		// exporter's gRPC connection -- it bypasses the
		// exporter's default-TLS path entirely. Using
		// `WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials()))`
		// here doesn't work because the OTLP exporter
		// installs its own credentials on top, leading
		// to a "first record does not look like a TLS
		// handshake" failure against a plaintext
		// receiver.
		exporterOpts = append(exporterOpts, otlptracegrpc.WithInsecure())
	}
	if len(opts.Headers) > 0 {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithHeaders(opts.Headers))
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	exporter, err := otlptracegrpc.New(dialCtx, exporterOpts...)
	if err != nil {
		return nil, fmt.Errorf("otlptracegrpc.New(%s): %w", endpoint, err)
	}
	return newTracerProviderWithExporter(exporter, opts), nil
}

// newTracerProviderWithExporter is the test seam used by
// `integration_test.go` to swap in a fake OTLP exporter
// without going through the gRPC dial. Production code
// reaches this through [newTracerProvider].
func newTracerProviderWithExporter(exporter sdktrace.SpanExporter, opts SetupOptions) *sdktrace.TracerProvider {
	res := buildResource(opts)
	sampler := buildSampler(opts.SamplerRatio)
	return sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
}

func buildResource(opts SetupOptions) *sdkresource.Resource {
	version := opts.ServiceVersion
	if version == "" {
		version = "dev"
	}
	return sdkresource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(opts.ServiceName),
		semconv.ServiceVersion(version),
	)
}

func buildSampler(ratio float64) sdktrace.Sampler {
	switch {
	case ratio <= 0:
		// Negative / zero ratio = never sample. Useful
		// for emergency "disable telemetry but keep
		// resource attributes" deployments without
		// touching the endpoint pin.
		return sdktrace.NeverSample()
	case ratio >= 1.0:
		return sdktrace.AlwaysSample()
	default:
		return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
	}
}

// isLocalEndpoint returns true when the OTLP endpoint
// hostname is `localhost` / loopback. Used to default to
// plaintext gRPC in dev without forcing every developer to
// flip the `Insecure` flag.
func isLocalEndpoint(endpoint string) bool {
	if endpoint == "" {
		return false
	}
	host := endpoint
	if i := strings.LastIndex(endpoint, ":"); i > 0 {
		host = endpoint[:i]
	}
	switch host {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// SpanFromContext is a thin re-export of
// `go.opentelemetry.io/otel/trace.SpanFromContext` provided
// so callers that already import `internal/telemetry` for
// the attribute keys don't need to also import the OTel
// `trace` package separately. Returns the noop span (always
// safe to call SetAttributes / End on) when no span is in
// ctx.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}
