package obs

// Tracer-setup helper for the §8.3 step-2 OTel-trace export.
//
// Every long-running binary in `cmd/` calls [SetupTracer] once
// at startup and defers the returned shutdown function until
// graceful shutdown. The function reads the standard
// `OTEL_EXPORTER_OTLP_ENDPOINT` (or the trace-specific
// `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`) env var; when both
// are empty the binary runs with a noop tracer provider so
// that the same code path is exercised in local-dev (where
// the Collector may not be running) and in production (where
// it is).
//
// Why HTTP-only. The local-dev compose stack at
// `deploy/local/otel/config.yaml` ships both 4317 (gRPC) and
// 4318 (HTTP) on the Collector. We pick HTTP because:
//
//   - The gRPC exporter would force `google.golang.org/grpc`
//     into every binary's link graph; HTTP only depends on
//     `net/http` plus the OTel HTTP transport.
//   - The Collector's HTTP receiver accepts both pinned-path
//     (`/v1/traces`) and bare-path requests; the OTel HTTP
//     exporter uses pinned by default which lines up with the
//     Collector default config.
//   - Latency budget: a 4 ms inter-process HTTP hop within
//     the same compose network is well below the §8.3 SLO
//     budgets for every verb.
//
// Why trace-only. The §8.3 step-2 text says "OTel-trace
// export from every binary"; metrics export is explicitly NOT
// scoped (Prometheus scrape is the metric channel for v1).
// Wiring `sdkmetric` here would create a second metric path
// that the operator would have to reconcile with the
// hand-rolled `/metrics` body.

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// newResource builds the OTel `Resource` (`service.name` +
// `service.namespace`) for this binary. The namespace pins
// `agent-memory` so a downstream observability tool can group
// all binaries in this service mesh on a single row.
func newResource(svc string) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		semconv.ServiceName(svc),
		semconv.ServiceNamespaceKey.String("agent-memory"),
	}
	return resource.New(context.Background(),
		resource.WithSchemaURL(semconv.SchemaURL),
		resource.WithAttributes(attrs...),
	)
}

// ShutdownFunc is the trace-flush hook returned by
// [SetupTracer]. Callers MUST `defer shutdown(ctx)` so any
// in-flight span batch flushes before the process exits. A
// nil error from the shutdown function is the healthy
// outcome; a non-nil error is logged but does NOT change the
// process exit code (an unreachable Collector should not
// fail the agent-memory service that just successfully
// served all of its traffic).
type ShutdownFunc func(context.Context) error

// TracerSetupResult is what [SetupTracer] hands back. It
// embeds the configured shutdown hook plus the tracer
// instance the caller will use to start spans. The
// `EndpointResolved` and `Exporting` fields let the caller
// log what actually happened — operators want to see "OTLP
// export disabled (env unset)" rather than guess.
type TracerSetupResult struct {
	Tracer           trace.Tracer
	Shutdown         ShutdownFunc
	EndpointResolved string
	Exporting        bool
}

// ServiceName is the OTel `service.name` resource attribute
// that every span produced by `tracer.Start` will carry. The
// Collector keys downstream sinks on this so the operator
// can split agent-api spans from mgmt-api spans without
// guessing.
type ServiceName string

const (
	// ServiceNameAgentAPI is for `cmd/agent-api`.
	ServiceNameAgentAPI ServiceName = "agent-api"
	// ServiceNameMgmtAPI is for `cmd/mgmt-api`.
	ServiceNameMgmtAPI ServiceName = "mgmt-api"
	// ServiceNameSpanIngestor is for `cmd/span-ingestor`.
	ServiceNameSpanIngestor ServiceName = "span-ingestor"
	// ServiceNameConsolidator is for `cmd/consolidator`.
	ServiceNameConsolidator ServiceName = "consolidator"
	// ServiceNameConceptPromoter is for `cmd/concept-promoter`.
	ServiceNameConceptPromoter ServiceName = "concept-promoter"
	// ServiceNameRepoIndexer is for `cmd/repoindexer`.
	ServiceNameRepoIndexer ServiceName = "repoindexer"
	// ServiceNameWebhookReceiver is for `cmd/webhook-receiver`.
	ServiceNameWebhookReceiver ServiceName = "webhook-receiver"
	// ServiceNameRerankerTrainer is for `cmd/reranker-trainer`.
	ServiceNameRerankerTrainer ServiceName = "reranker-trainer"
	// ServiceNameTraceLogPruner is for `cmd/trace-log-pruner`.
	ServiceNameTraceLogPruner ServiceName = "trace-log-pruner"
	// ServiceNameQdrantBootstrap is for `cmd/qdrant-bootstrap`,
	// the §6.4 vector-store provisioner / snapshot scheduler.
	// Even in single-shot bootstrap mode (`--snapshot-interval=0`)
	// the binary publishes /metrics so Stage 8.3's "every binary
	// emits Prometheus + OTel" requirement holds before the
	// scheduler-mode daemon starts.
	ServiceNameQdrantBootstrap ServiceName = "qdrant-bootstrap"
	// ServiceNameRerankerSidecar names the Python sidecar at
	// `cmd/reranker-sidecar/main.py`. Declared here so the
	// inventory test (`cmd_inventory_test.go`) and any future
	// Go-side wrapper that proxies sidecar requests can use
	// the same canonical service name.
	ServiceNameRerankerSidecar ServiceName = "reranker-sidecar"
)

// SetupTracer wires the package-global `otel.Tracer(...)` to
// an OTLP/HTTP-backed batch span processor when an endpoint
// is configured, otherwise to a noop provider so the binary
// behaves the same in either mode.
//
// Env vars consulted (in order):
//
//	OTEL_EXPORTER_OTLP_TRACES_ENDPOINT  preferred, traces-only
//	OTEL_EXPORTER_OTLP_ENDPOINT         standard fallback
//
// Both forms accept a `host:port` (e.g. `localhost:4318`),
// a `scheme://host:port` (e.g. `http://otel-collector:4318`),
// or a full URL with path. The function normalises to the
// `otlptracehttp.WithEndpoint(host:port)` + optional
// `WithInsecure` / `WithURLPath` shape the SDK requires.
//
// The W3C trace-context + baggage text-map propagators are
// installed globally so an incoming HTTP/gRPC request that
// already carries a `traceparent` header continues that trace
// rather than starting a fresh one. This is the contract the
// downstream Span Ingestor's OTLP receiver assumes.
//
// Concurrency / test-safety contract. SetupTracer mutates two
// process-global singletons via `otel.SetTracerProvider` and
// `otel.SetTextMapPropagator`. This is intentional — every
// caller (HTTP handler, background worker, sidecar) reaches
// for the configured tracer through the same `otel.Tracer(...)`
// global, so installing it once at process startup is the
// whole point. It does mean that:
//
//   - Production binaries MUST call SetupTracer exactly once
//     from `main` before serving traffic.
//   - Unit tests that exercise SetupTracer MUST NOT call
//     `t.Parallel()`. The existing tests use `t.Setenv`,
//     which Go's `testing` package already refuses to combine
//     with `t.Parallel`, so the current suite is safe by
//     construction; the prohibition is restated here so
//     future contributors who replace `t.Setenv` with manual
//     `os.Setenv` do not accidentally introduce a data race
//     on the global TracerProvider.
func SetupTracer(ctx context.Context, svc ServiceName, logger *slog.Logger) (TracerSetupResult, error) {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	// Stage 8.3 iter-3 evaluator fix #3 — the function MUST
	// always return a usable TracerSetupResult, including a
	// non-nil Shutdown hook, even on a configuration error.
	// Callers defer `tracerSetup.Shutdown(ctx)` immediately
	// after assignment; a nil function there panics with
	// `invalid memory address` and tears the binary down
	// before it ever serves a request. We build a noop
	// fallback up-front and only swap in the live exporter
	// on the happy path.
	noopFallback := func() TracerSetupResult {
		tp := noop.NewTracerProvider()
		return TracerSetupResult{
			Tracer:           tp.Tracer(string(svc)),
			Shutdown:         func(context.Context) error { return nil },
			EndpointResolved: "",
			Exporting:        false,
		}
	}
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"))
	if endpoint == "" {
		endpoint = strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	}
	if endpoint == "" {
		// Noop tracer. Install propagators anyway so an
		// inbound trace-context header survives the hop
		// even when this binary does not export its own
		// spans.
		//
		// Not safe for parallel tests: the next two calls
		// mutate process-global singletons. See the
		// SetupTracer doc comment ("Concurrency /
		// test-safety contract") for the full rationale.
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		tp := noop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		logger.Info("obs.tracer.disabled",
			slog.String("service_name", string(svc)),
			slog.String("hint", "set OTEL_EXPORTER_OTLP_ENDPOINT to enable OTLP/HTTP trace export"),
		)
		return TracerSetupResult{
			Tracer:           tp.Tracer(string(svc)),
			Shutdown:         func(context.Context) error { return nil },
			EndpointResolved: "",
			Exporting:        false,
		}, nil
	}

	host, urlPath, insecure, err := parseOTLPEndpoint(endpoint)
	if err != nil {
		return noopFallback(), fmt.Errorf("obs.SetupTracer: parse OTLP endpoint %q: %w", endpoint, err)
	}

	exporterOpts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(host),
	}
	if insecure {
		exporterOpts = append(exporterOpts, otlptracehttp.WithInsecure())
	}
	if urlPath != "" {
		exporterOpts = append(exporterOpts, otlptracehttp.WithURLPath(urlPath))
	}

	exporter, err := otlptrace.New(ctx, otlptracehttp.NewClient(exporterOpts...))
	if err != nil {
		return noopFallback(), fmt.Errorf("obs.SetupTracer: otlptrace.New: %w", err)
	}

	res, err := newResource(string(svc))
	if err != nil {
		_ = exporter.Shutdown(ctx)
		return noopFallback(), fmt.Errorf("obs.SetupTracer: resource: %w", err)
	}

	bsp := sdktrace.NewBatchSpanProcessor(exporter,
		sdktrace.WithMaxExportBatchSize(512),
		sdktrace.WithBatchTimeout(5*time.Second),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(bsp),
		sdktrace.WithResource(res),
	)
	// Not safe for parallel tests: the next two calls mutate
	// process-global singletons. See the SetupTracer doc
	// comment ("Concurrency / test-safety contract") for the
	// full rationale.
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	logger.Info("obs.tracer.exporting",
		slog.String("service_name", string(svc)),
		slog.String("endpoint", endpoint),
		slog.String("host", host),
		slog.String("path", urlPath),
		slog.Bool("insecure", insecure),
	)

	shutdown := ShutdownFunc(func(ctx context.Context) error {
		// Flush the in-memory batch before tearing down
		// the exporter; both calls share an outer timeout
		// because a slow Collector should not block the
		// process exit indefinitely.
		shutCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var firstErr error
		if err := tp.ForceFlush(shutCtx); err != nil {
			firstErr = err
		}
		if err := tp.Shutdown(shutCtx); err != nil && firstErr == nil {
			firstErr = err
		}
		return firstErr
	})

	return TracerSetupResult{
		Tracer:           tp.Tracer(string(svc)),
		Shutdown:         shutdown,
		EndpointResolved: endpoint,
		Exporting:        true,
	}, nil
}

// parseOTLPEndpoint normalises the four common ways an
// operator can spell an OTLP endpoint:
//
//  1. `localhost:4318`             — bare host:port (insecure inferred)
//  2. `http://collector:4318`      — explicit insecure URL
//  3. `https://collector:4318/v1/traces` — explicit secure URL with path
//  4. `https://collector`          — host only (insecure inferred from scheme)
//
// The returned `host` is the raw `host:port` the
// `otlptracehttp.WithEndpoint` option expects; `urlPath` is
// the explicit path (empty if the operator did not override
// the default `/v1/traces`); `insecure` is true unless the
// URL parses as `https://`.
func parseOTLPEndpoint(endpoint string) (host, urlPath string, insecure bool, err error) {
	if endpoint == "" {
		return "", "", false, fmt.Errorf("empty endpoint")
	}
	// `localhost:4318` is not a valid URL (no scheme), so
	// guess HTTP first. If it parses as a host:port with
	// nothing else, accept it.
	if !strings.Contains(endpoint, "://") {
		// Bare `host:port` — treat as insecure HTTP.
		return endpoint, "", true, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", "", false, err
	}
	if u.Host == "" {
		return "", "", false, fmt.Errorf("missing host in %q", endpoint)
	}
	insecure = u.Scheme != "https"
	urlPath = u.Path
	if urlPath == "/" {
		urlPath = ""
	}
	return u.Host, urlPath, insecure, nil
}
