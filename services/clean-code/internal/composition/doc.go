// Package composition holds composition-root helpers that
// build production-grade subsystems (mgmt writer, ingest
// webhook router, evaluator gate) without binding the
// callers to a specific HTTP mux topology.
//
// Each helper takes the durable inputs the subsystem needs
// (typically one or two `*sql.DB` handles + a few config
// values) and returns the fully-wired runtime object (e.g.
// `*management.MgmtWriter`, `*webhook.Router`,
// `*evaluator.Gate`). The CALLER decides where to mount the
// resulting handler -- whether that is the metric-ingestor's
// own mux, the eval-gate's mux, or the OIDC gateway's
// canonical verb registry via the api.ProductionWiringDeps
// adapters.
//
// # Motivation
//
// Stage 6.4 (HTTP/JSON gateway and OIDC auth) introduces a
// third composition root (cmd/clean-code-gateway) which
// must mount the same MgmtWriter / webhook.Router /
// evaluator.Gate that the existing
// cmd/clean-code-metric-ingestor and cmd/clean-code-eval-gate
// binaries already construct. The naive copy-paste between
// the three binaries drifts; this package centralises the
// wiring so all three composition roots construct the SAME
// object graph with the SAME role boundaries.
//
// # Role boundaries
//
// The helpers preserve the documented PG role boundaries
// (migrations/0004_roles.up.sql):
//
//   - BuildMgmtWriter takes ingestorDB (clean_code_metric_ingestor
//     grants for scan_run + metric_retraction) AND mgmtDB
//     (clean_code_management grants for repo + repo_event).
//   - BuildIngestRouter takes ingestorDB ONLY -- every store
//     it wires runs under clean_code_metric_ingestor grants.
//   - BuildEvalGate takes evaluatorDB (clean_code_evaluator
//     grants for the two degraded short-circuit paths) AND
//     solidBatchDB (clean_code_solid_batch grants for the
//     canonical rule-pass Audit triple).
//
// A composition root that wants to collapse role boundaries
// (dev / E2E only) passes the SAME `*sql.DB` for both
// arguments; the helpers do NOT enforce role separation
// themselves -- the DB credentials carried by the handle do.
package composition
