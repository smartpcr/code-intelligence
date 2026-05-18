// Package degraded centralises the §6.3 / §8.2 / C22
// degraded-mode contract surface shared by every Agent and
// Management verb in the agent-memory service.
//
// Stage 8.1 of `implementation-plan.md` wires the contract:
// the closed set of `degraded_reason` ENUM values
// (architecture.md §8.2), a per-verb degraded counter for
// dashboards, and an optional test-only fault-injection seam
// so the closed-set guard can be exercised end-to-end.
//
// The closed set is the single source of truth. Any verb
// that wants to surface a degraded response MUST pick one of:
//
//   - `episodic_log_unavailable`
//   - `graph_store_unavailable`
//   - `embedding_index_unavailable`
//   - `reranker_model_stale`
//   - `span_ingestor_backpressure`
//   - `consolidator_backpressure`
//
// The adapter / handler wraps the enforcer to map
// [ErrUnknownReason] onto a hard-failure status code (gRPC
// `codes.Internal`, HTTP 500). This matches the §8.1 e2e
// scenario "closed degraded_reason enforced" — the wire is
// either a valid closed reason OR a 500.
//
// Package layout
// --------------
//   - reason.go        — closed-set constants, IsClosed,
//                        Enforce, Priority.
//   - metric.go        — Metric interface + in-process
//                        Counter implementation (no Prometheus
//                        dependency at this layer; the
//                        composition root may bridge).
//   - faultinjector.go — FaultInjector interface +
//                        MapFaultInjector test seam (concurrent-safe).
//
// No package imports here so the surface stays standalone
// and importable by every other internal package (avoiding
// import cycles with `agentapi` / `mgmtapi` / `consolidator`).
package degraded
