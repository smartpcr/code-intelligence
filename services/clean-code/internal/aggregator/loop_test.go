package aggregator_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/aggregator"
)

// TestNewLoop_PanicsOnNilAggregator pins the wiring-bug-surface
// contract for the composition root.
func TestNewLoop_PanicsOnNilAggregator(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected NewLoop(nil) to panic")
		}
	}()
	_ = aggregator.NewLoop(nil)
}

// TestNewLoop_PanicsOnNonPositiveCadence covers the cadence
// guard. The loop's whole reason for being is the 15-min cadence;
// a zero or negative one is a misconfiguration that must surface
// at startup not at first tick.
func TestNewLoop_PanicsOnNonPositiveCadence(t *testing.T) {
	t.Parallel()
	agg, err := aggregator.NewAggregator(aggregator.NewInMemorySampleSource(nil), aggregator.NewInMemorySnapshotWriter())
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected NewLoop(cadence<=0) to panic")
		}
	}()
	_ = aggregator.NewLoop(agg, aggregator.WithLoopCadence(0))
}

// TestNewLoop_DefaultCadenceIs15Min pins the tech-spec Sec 8.2
// default to a regression-resistant assertion. Changing the
// constant in `internal/config` without updating this test would
// trip the test gate -- which is what we want when an operator
// is debating a different cadence.
func TestNewLoop_DefaultCadenceIs15Min(t *testing.T) {
	t.Parallel()
	agg, err := aggregator.NewAggregator(aggregator.NewInMemorySampleSource(nil), aggregator.NewInMemorySnapshotWriter())
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	l := aggregator.NewLoop(agg)
	if l.Cadence() != 15*time.Minute {
		t.Errorf("Loop.Cadence() = %s, want 15m0s", l.Cadence())
	}
}

// TestLoop_Run_RunsTickOnStartThenCadence is the canonical
// driver behaviour: with `runOnStart=true` (default) the loop
// fires one Tick immediately, then waits a cadence, then fires
// the next Tick. Ctx cancellation ends the loop on the second
// wait so the test does NOT need to block 15 minutes.
//
// We use the injected sleep channel to drive cadence transitions
// deterministically. The channel pattern mirrors
// `metric_ingestor/stale_scan_run_sweep_loop_test.go`.
func TestLoop_Run_RunsTickOnStartThenCadence(t *testing.T) {
	t.Parallel()
	rid := mustParseUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	src := aggregator.NewInMemorySampleSource([]aggregator.Observation{
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: 1},
	})
	writer := aggregator.NewInMemorySnapshotWriter()
	tickAt := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	agg, err := aggregator.NewAggregator(src, writer, aggregator.WithClock(fixedClock(tickAt)))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	// fakeSleep is a unit-test clock: every wait the loop
	// requests is routed through this channel so the test
	// dictates when Tick #2 (etc.) fires.
	sleepCh := make(chan time.Time)
	var sleepCallCount int32
	sleep := func(d time.Duration) <-chan time.Time {
		atomic.AddInt32(&sleepCallCount, 1)
		return sleepCh
	}

	loop := aggregator.NewLoop(agg,
		aggregator.WithLoopCadence(15*time.Minute),
		aggregator.WithLoopSleep(sleep),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() {
		runDone <- loop.Run(ctx)
	}()

	// Wait for the loop to have completed tick #1 and parked
	// in its cadence sleep.
	deadline := time.After(2 * time.Second)
	for {
		if len(writer.Writes()) >= 1 && atomic.LoadInt32(&sleepCallCount) >= 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("loop did not reach first sleep within 2s; writes=%d sleeps=%d", len(writer.Writes()), atomic.LoadInt32(&sleepCallCount))
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Release the first sleep -> the loop runs tick #2.
	sleepCh <- time.Now()

	// Wait until tick #2 is captured.
	deadline = time.After(2 * time.Second)
	for {
		if len(writer.Writes()) >= 2 && atomic.LoadInt32(&sleepCallCount) >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("loop did not record second tick within 2s; writes=%d sleeps=%d", len(writer.Writes()), atomic.LoadInt32(&sleepCallCount))
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-runDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop within 2s after cancel")
	}

	if got := len(writer.Writes()); got < 2 {
		t.Errorf("writer captured %d ticks, want >= 2", got)
	}
}

// TestLoop_Run_RunOnStartFalse_WaitsBeforeFirstTick covers the
// inverse: when run-on-start is disabled, the loop sleeps once
// before the first Tick. Used by tests / staged-rollout configs
// that want to delay the initial materialisation.
func TestLoop_Run_RunOnStartFalse_WaitsBeforeFirstTick(t *testing.T) {
	t.Parallel()
	rid := mustParseUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	src := aggregator.NewInMemorySampleSource([]aggregator.Observation{
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: 1},
	})
	writer := aggregator.NewInMemorySnapshotWriter()
	tickAt := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)
	agg, err := aggregator.NewAggregator(src, writer, aggregator.WithClock(fixedClock(tickAt)))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	sleepCh := make(chan time.Time)
	var sleepCallCount int32
	sleep := func(d time.Duration) <-chan time.Time {
		atomic.AddInt32(&sleepCallCount, 1)
		return sleepCh
	}

	loop := aggregator.NewLoop(agg,
		aggregator.WithLoopCadence(15*time.Minute),
		aggregator.WithLoopSleep(sleep),
		aggregator.WithLoopRunOnStart(false),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- loop.Run(ctx) }()

	// The loop should park in its FIRST sleep before any tick.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&sleepCallCount) < 1 {
		select {
		case <-deadline:
			t.Fatalf("loop did not request first sleep within 2s; sleeps=%d", atomic.LoadInt32(&sleepCallCount))
		case <-time.After(10 * time.Millisecond):
		}
	}
	if got := len(writer.Writes()); got != 0 {
		t.Fatalf("writer.Writes() len = %d, want 0 -- loop must not tick before first sleep elapses", got)
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop within 2s after cancel")
	}
}

// TestLoop_Run_TickErrorContinuesAfterBackoff is the resilience
// guarantee: a writer outage MUST NOT terminate the loop. The
// loop logs the error and waits `errorBackoff` before retrying.
// We simulate a transient outage by setting fail-error on the
// writer for the first tick and clearing it for the second.
func TestLoop_Run_TickErrorContinuesAfterBackoff(t *testing.T) {
	t.Parallel()
	rid := mustParseUUID(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	src := aggregator.NewInMemorySampleSource([]aggregator.Observation{
		{RepoID: rid, MetricKind: "cyclo", ScopeKind: "method", Value: 1},
	})
	writer := aggregator.NewInMemorySnapshotWriter()
	writer.SetFailError(errors.New("simulated PG outage"))

	agg, err := aggregator.NewAggregator(src, writer)
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}

	sleepCh := make(chan time.Time)
	var sleepCallCount int32
	var waitedDurations []time.Duration
	var mu sync.Mutex
	sleep := func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waitedDurations = append(waitedDurations, d)
		mu.Unlock()
		atomic.AddInt32(&sleepCallCount, 1)
		return sleepCh
	}

	loop := aggregator.NewLoop(agg,
		aggregator.WithLoopCadence(15*time.Minute),
		aggregator.WithLoopErrorBackoff(30*time.Second),
		aggregator.WithLoopSleep(sleep),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runDone := make(chan error, 1)
	go func() { runDone <- loop.Run(ctx) }()

	// First tick fires immediately, fails, loop parks in error-
	// backoff sleep.
	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&sleepCallCount) < 1 {
		select {
		case <-deadline:
			t.Fatalf("loop did not request first sleep within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Clear the failure so the next tick succeeds; release sleep.
	writer.SetFailError(nil)
	sleepCh <- time.Now()

	// Wait for the second tick to succeed (writes captured).
	deadline = time.After(2 * time.Second)
	for len(writer.Writes()) < 1 {
		select {
		case <-deadline:
			t.Fatalf("loop did not produce a successful tick after backoff; writes=%d", len(writer.Writes()))
		case <-time.After(10 * time.Millisecond):
		}
	}

	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop after cancel")
	}

	// The first wait should be the ERROR backoff (30s), not the
	// healthy cadence (15m). This pins the runOnce branch that
	// returns errorBackoff on tick failure.
	mu.Lock()
	defer mu.Unlock()
	if len(waitedDurations) < 1 {
		t.Fatalf("no waited durations captured")
	}
	if waitedDurations[0] != 30*time.Second {
		t.Errorf("first wait = %s, want 30s (error backoff)", waitedDurations[0])
	}
}

// TestLoop_Aggregator_RoundTrips covers the composition-root
// helper -- the composition root needs to share dependencies
// between the loop and the Prometheus exporter, so Loop must
// expose its wrapped aggregator.
func TestLoop_Aggregator_RoundTrips(t *testing.T) {
	t.Parallel()
	agg, err := aggregator.NewAggregator(aggregator.NewInMemorySampleSource(nil), aggregator.NewInMemorySnapshotWriter())
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	loop := aggregator.NewLoop(agg)
	if loop.Aggregator() != agg {
		t.Errorf("Loop.Aggregator() returned a different pointer than the one passed to NewLoop")
	}
}

// silence unused uuid import when the test compiles without
// using mustParseUUID in any test below.
var _ = uuid.Nil
