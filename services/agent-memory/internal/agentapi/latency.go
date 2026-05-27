package agentapi

// Stage 8.3 step 1 — per-verb latency observers.
//
// The four agent verbs (`agent.recall`, `agent.observe`,
// `agent.expand`, `agent.summarize`) each carry an §8.3 SLO
// (see tech-spec.md §8.3). This file wires a side-effect-free
// latency observation hook onto every `Service` verb so the
// binary's `/metrics` handler can render the corresponding
// `*_duration_seconds` Prometheus histogram.
//
// Design choice: we accept a plain `LatencyObserver` callback
// rather than the concrete `*obs.Histogram`. Two reasons:
//
//   1. The `agentapi` package is consumed from the test
//      suite under a unit-test build that does NOT want to
//      pay the OTel SDK link cost; a callback keeps the
//      production wiring at `cmd/agent-api/main.go` (where
//      `obs.NewHistogram(...).Observe` is bound) and leaves
//      the test wiring free to bind a fake counter.
//   2. Future workstreams may want to fan-out the same
//      observation to multiple sinks (e.g. an internal
//      latency-budget enforcer alongside the Prometheus
//      histogram); a callback is trivially composable.
//
// The observer is invoked exactly once per verb call, in a
// deferred block that fires AFTER the named-return wrap. We
// observe BOTH successful and degraded responses — degraded
// responses still count against the SLO budget (the §8.3
// p95/p99 numbers do not exempt degraded calls).

import "time"

// LatencyObserver is the per-call callback the verb handlers
// invoke with the elapsed wall-clock seconds of the request.
// Implementations MUST be safe for concurrent invocation; the
// default Histogram in `internal/obs` satisfies that.
type LatencyObserver func(seconds float64)

// noopLatencyObserver is the zero-value the verb handlers see
// when no observer has been wired. We return it from the
// option-resolution paths so the call sites can call it
// unconditionally without a nil check (the dead-code
// elimination at the call site is the same either way).
func noopLatencyObserver(float64) {}

// observerOrNoop normalises a nil observer to the noop. This
// keeps the call site free of per-verb nil branches.
func observerOrNoop(o LatencyObserver) LatencyObserver {
	if o == nil {
		return noopLatencyObserver
	}
	return o
}

// recordLatency is the shared "defer" body the four verbs use
// to dispatch their elapsed-time sample. Centralising the
// `time.Since(start).Seconds()` math keeps the call sites
// trivially auditable — `defer recordLatency(s.recallLatency, time.Now())`
// is the canonical pattern.
func recordLatency(o LatencyObserver, start time.Time) {
	observerOrNoop(o)(time.Since(start).Seconds())
}

// WithRecallLatencyObserver wires the §8.3
// `agent_recall_duration_seconds` histogram observer.
func WithRecallLatencyObserver(o LatencyObserver) Option {
	return func(s *Service) {
		s.recallLatency = o
	}
}

// WithObserveLatencyObserver wires the §8.3
// `agent_observe_duration_seconds` histogram observer onto
// the `Service` struct. Stored on `Service.observeLatency`
// so the binary's wiring at `cmd/agent-api/main.go` can carry
// a single observer value across both the recall-side
// `Service` (for symmetry with the other verbs' option
// helpers) and the dedicated `ObserveService` (via
// `WithObserveLatency` in `observe.go`, where the verb
// implementation actually runs the deferred sample). A nil
// observer is a no-op.
func WithObserveLatencyObserver(o LatencyObserver) Option {
	return func(s *Service) {
		s.observeLatency = o
	}
}

// WithExpandLatencyObserver wires the §8.3
// `agent_expand_duration_seconds` histogram observer.
func WithExpandLatencyObserver(o LatencyObserver) Option {
	return func(s *Service) {
		s.expandLatency = o
	}
}

// WithSummarizeLatencyObserver wires the §8.3
// `agent_summarize_duration_seconds` histogram observer.
func WithSummarizeLatencyObserver(o LatencyObserver) Option {
	return func(s *Service) {
		s.summarizeLatency = o
	}
}
