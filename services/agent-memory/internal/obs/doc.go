// Package obs holds the cross-cutting observability surface
// for every binary in `services/agent-memory`. It is the
// single place where:
//
//   - Prometheus histogram exposition is rendered (no
//     dependency on github.com/prometheus/client_golang — the
//     repo deliberately hand-rolls the text-exposition format
//     so the binaries stay lean; see [Histogram] for the
//     implementation contract).
//   - OpenTelemetry tracing is set up (see [SetupTracer]).
//     Trace export is OTLP/HTTP only — see the local-dev
//     compose stack at `deploy/local/otel/config.yaml` which
//     exposes both 4317 (gRPC) and 4318 (HTTP).
//   - The §8.3 metric NAMES are pinned as constants so a
//     refactor cannot silently rename the metric the operator
//     scrapes (see metrics.go).
//
// This package owns implementation-plan.md Stage 8.3 step 1
// (metric shape) and step 2 (OTel-trace export). The Grafana
// dashboard JSON and Prometheus alert rules under
// `deploy/dashboards/` and `deploy/alerts/` are the operator-
// facing artefacts that close out steps 3 and 4; they
// reference the metric NAMES exported from this package so a
// rename forces a coordinated update.
//
// Why hand-roll the histogram. The agent-memory binaries
// already hand-roll counter + gauge exposition (see
// internal/degraded/prometheus.go, internal/mgmtapi/
// metrics_handler.go, the per-binary writeMetrics
// functions). Adding github.com/prometheus/client_golang now
// would split the exposition surface across two libraries
// (the prom client emits its own `# HELP` / `# TYPE` lines,
// metric ordering, escape rules) and produce a
// not-quite-consistent /metrics body that operators would
// have to deduplicate by hand. Keeping all exposition in this
// package keeps the contract single-sourced.
//
// Concurrency. Both [Histogram] and the OTel SDK call paths
// are safe for use across goroutines. The histogram uses a
// per-bucket atomic counter so a 50-RPS scrape against a 50-
// RPS agent.recall handler does not lock either side.
package obs
