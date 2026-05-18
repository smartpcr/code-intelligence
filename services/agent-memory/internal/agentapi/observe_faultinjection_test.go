package agentapi

// Stage 8.1 / e2e §13 contract test:
//
//	"closed degraded_reason enforced — fault injector returns
//	 'oops' → response is either rewritten OR 500."
//
// The verb funnels every successful exit through
// `applyDegradedContract`, which runs `degraded.Enforce` on
// the final pair. A non-closed reason is wrapped in
// `degraded.ErrUnknownReason` and surfaced as an error so the
// gRPC adapter's default branch returns `codes.Internal`.
//
// This test pins both halves of the contract:
//
//  1. Injecting a CLOSED-SET reason (`consolidator_backpressure`)
//     on a healthy call rewrites the response to that reason
//     and bumps the per-verb metric counter.
//  2. Injecting a NON-CLOSED reason (`"oops"`) on a healthy
//     call returns the typed `degraded.ErrUnknownReason`
//     wrapped, leaving the Episode row durably appended (the
//     persistence boundary already crossed) but failing the
//     wire so the gRPC adapter maps to `codes.Internal`.
//  3. Injection MUST NOT mask a real higher-priority outage
//     (priority overlay invariant).

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/degraded"
)

func TestObserve_faultInjection_closedSetReason_overlay(t *testing.T) {
	writer := &fakeEpisodeWriter{}
	resolver := &fakeContextResolver{}
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbObserve, degraded.ReasonConsolidatorBackpressure)

	svc := newTestService(t, writer, resolver,
		WithObserveFaultInjector(fi),
		WithObserveDegradedMetric(metric),
	)

	resp, err := svc.Observe(context.Background(), validReq())
	if err != nil {
		t.Fatalf("Observe with closed-set injection must succeed, got err: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("resp.Degraded=false, want true under injection")
	}
	if resp.DegradedReason != degraded.ReasonConsolidatorBackpressure {
		t.Fatalf("resp.DegradedReason=%q, want %q",
			resp.DegradedReason, degraded.ReasonConsolidatorBackpressure)
	}
	if got := metric.Count(VerbObserve, degraded.ReasonConsolidatorBackpressure); got != 1 {
		t.Fatalf("metric increment under injection = %d, want 1", got)
	}
}

func TestObserve_faultInjection_nonClosedReason_returnsInternalError(t *testing.T) {
	writer := &fakeEpisodeWriter{}
	resolver := &fakeContextResolver{}
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbObserve, "oops") // open-set reason — must trip Enforce

	svc := newTestService(t, writer, resolver,
		WithObserveFaultInjector(fi),
		WithObserveDegradedMetric(metric),
	)

	_, err := svc.Observe(context.Background(), validReq())
	if err == nil {
		t.Fatalf("Observe must fail when injector returns a non-closed reason")
	}
	if !errors.Is(err, degraded.ErrUnknownReason) {
		t.Fatalf("err = %v, want wraps ErrUnknownReason", err)
	}

	// The persistence boundary was already crossed before the
	// post-Append injection ran — the row IS durable.  The
	// failure surfaces at the wire only.  This is the §13
	// contract: a 500 to the agent caller does NOT mean the
	// Episode was lost.
	if rows := writer.snapshot(); len(rows) != 1 {
		t.Fatalf("Append calls = %d, want 1 (row durable, wire fails post-Append)", len(rows))
	}

	// Metric does NOT increment on a non-closed reason — the
	// counter is a closed-set instrument.
	if got := metric.Count(VerbObserve, "oops"); got != 0 {
		t.Fatalf("metric MUST NOT count non-closed reasons, got %d", got)
	}
}

// Real outages dominate over injection: with an episodic-log
// outage forcing WAL fallback AND a fault-injection rule set
// to a lower-priority closed reason, the response carries the
// real reason (episodic_log_unavailable), not the injected one.
func TestObserve_faultInjection_priorityOverlay_realOutageWins(t *testing.T) {
	writer := &fakeEpisodeWriter{
		errs: []error{ErrEpisodicLogUnavailable},
	}
	resolver := &fakeContextResolver{}
	wal := &fakeWAL{}
	fi := degraded.NewMapFaultInjector()
	// Inject a LOWER-priority reason.  consolidator_backpressure
	// has priority 20; episodic_log_unavailable has priority 100.
	fi.SetForVerb(VerbObserve, degraded.ReasonConsolidatorBackpressure)

	svc := newTestService(t, writer, resolver,
		WithObserveWAL(wal),
		WithObserveFaultInjector(fi),
	)

	resp, err := svc.Observe(context.Background(), validReq())
	if err != nil {
		t.Fatalf("Observe with WAL fallback must succeed, got %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("resp.Degraded=false, want true")
	}
	if resp.DegradedReason != degraded.ReasonEpisodicLogUnavailable {
		t.Fatalf("resp.DegradedReason = %q, want %q (real outage MUST dominate over lower-priority injection)",
			resp.DegradedReason, degraded.ReasonEpisodicLogUnavailable)
	}
}
