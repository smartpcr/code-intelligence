// Package telemetry is the clean-code service's central
// OpenTelemetry + Prometheus seam, delivered by
// implementation-plan Stage 9.4
// (phase-audit-wal-and-reliability-hardening /
// stage-otel-telemetry-across-all-surfaces) per architecture
// Sec 8.
//
// # Responsibilities
//
//  1. [Setup] initialises the OTel SDK with the OTLP gRPC
//     exporter pointed at [config.Config.OTelEndpoint]
//     (operator pin `CLEAN_CODE_OTEL_ENDPOINT`). A composition
//     root calls [Setup] ONCE at process start, defers
//     the returned `shutdown` until SIGTERM, and the rest of
//     the binary uses `otel.GetTracerProvider()` /
//     `otel.GetTracerProvider().Tracer(...)` to emit spans.
//
//  2. [AnnotateEvalGateSpan] stamps the four eval-gate-
//     specific span attributes (`policy_version_id`,
//     `degraded`, `degraded_reason`, `verdict`) on the
//     currently-active OTel span. The function is a safe
//     no-op when no OTel span is in `ctx` (e.g. SDK not
//     initialised, or running under a non-OTel tracer in
//     tests).
//
//  3. [PrometheusHandler] returns an [http.Handler] that
//     concatenates the Prometheus text-exposition output of
//     every [Collector] registered with it. The
//     collectors satisfied by Stage 9.4 are:
//
//      - the existing
//        `internal/metric_ingestor.StaleScanRunSweepMetrics`
//        (Stage 3.5 sweep counters), which already implements
//        `WriteText` and so satisfies [Collector] verbatim.
//      - the new [AggregatorTickMetrics] (Stage 7.1 cross-
//        repo aggregator tick duration histogram).
//      - the new [WALReplayMetrics] (Stage 9.2 audit WAL
//        replay duration histogram).
//      - the new [RuleEngineMetrics] (Stage 5.7 rule engine
//        evaluations counter + per-verdict counter).
//
// # Canonical span-attribute keys
//
// The constants in [attrs.go] mirror the architecture Sec 8
// taxonomy and are exported so dashboards / alerts can
// reference them verbatim. The gateway in `internal/api`
// stamps the foundation attrs (`verb`, `repo_id`,
// `caller_subject`, `http.*`) on every span via its own
// `api.Tracer` seam; the four eval-gate-specific attrs land
// inside the verb handler via [AnnotateEvalGateSpan].
//
// # Test-only seams
//
// [NewFakeOTLPReceiver] starts an in-process gRPC OTLP
// trace-service implementation that records every
// `ExportRequest` it receives. The integration test in
// `integration_test.go` wires the real OTel SDK to this
// fake to assert end-to-end span attributes ride the wire
// (gate-span-carries-verdict-tag scenario from impl-plan
// Stage 9.4).
package telemetry
