package metric_ingestor_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
)

// TestSweeper_DrainsQueueThenIdles wires a state machine
// over a 3-commit in-memory store + counts the sweeper's
// ProcessOne invocations. The sweeper MUST drain the queue
// (3 successful processOne) AND THEN enter idle-sleep
// (1 cadence wait). When the test releases the sleep
// channel the loop polls once more (no work) and we
// cancel the context to terminate.
func TestSweeper_DrainsQueueThenIdles(t *testing.T) {
	repoID := uuid.Must(uuid.FromString("ddddffff-aaaa-aaaa-aaaa-111111111111"))
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(
			uuid.Must(uuid.FromString("eeeeffff-0001-0001-0001-000000000001")),
			uuid.Must(uuid.FromString("eeeeffff-0002-0002-0002-000000000002")),
			uuid.Must(uuid.FromString("eeeeffff-0003-0003-0003-000000000003")),
		)),
	)
	store.SeedPending(
		metric_ingestor.PendingCommit{RepoID: repoID, SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CommittedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)},
		metric_ingestor.PendingCommit{RepoID: repoID, SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", CommittedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)},
		metric_ingestor.PendingCommit{RepoID: repoID, SHA: "cccccccccccccccccccccccccccccccccccccccc", CommittedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)},
	)

	var scans int32
	scanner := scannerFunc(func(_ context.Context, _ metric_ingestor.ScanRunClaim) error {
		atomic.AddInt32(&scans, 1)
		return nil
	})

	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC))),
	)

	// idleCh is what the injected sweep clock returns; the
	// test fires it manually to release each idle wait
	// without burning real wall time.
	idleCh := make(chan time.Time, 1)
	var idleWaits int32
	sleepFn := func(_ time.Duration) <-chan time.Time {
		atomic.AddInt32(&idleWaits, 1)
		return idleCh
	}

	sweeper := metric_ingestor.NewSweeper(sm,
		metric_ingestor.WithSweeperCadence(50*time.Millisecond),
		metric_ingestor.WithSweeperClock(sleepFn),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- sweeper.Run(ctx) }()

	// Wait for the sweeper to drain the 3 commits and call
	// the sleep factory (idle wait). The store has 3 items,
	// the sweeper does 3 successful ProcessOne (drain
	// mode), THEN calls sleepFn once (idle wait).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&scans) == 3 && atomic.LoadInt32(&idleWaits) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&scans); got != 3 {
		t.Fatalf("scans = %d, want 3 (drain mode should not idle until queue is empty)", got)
	}
	if got := atomic.LoadInt32(&idleWaits); got != 1 {
		t.Fatalf("idleWaits = %d, want 1 (sweeper should idle exactly once after draining)", got)
	}

	cancel()
	// Releasing the idle channel lets the sweeper's wait
	// return -- it then re-checks ctx.Err() and exits.
	idleCh <- time.Now()

	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Sweeper.Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sweeper.Run did not return within 2s after cancel")
	}
}

// TestSweeper_ExitsImmediatelyOnCanceledContext pins the
// cooperative-shutdown contract: a sweeper invoked with an
// already-cancelled context exits BEFORE it claims any
// commit (no half-completed scan state is left).
func TestSweeper_ExitsImmediatelyOnCanceledContext(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "aaaa00000000000000000000000000000000000a",
		CommittedAt: time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC),
	})
	var scans int32
	scanner := scannerFunc(func(_ context.Context, _ metric_ingestor.ScanRunClaim) error {
		atomic.AddInt32(&scans, 1)
		return nil
	})
	sm := metric_ingestor.NewStateMachine(store, scanner)
	sweeper := metric_ingestor.NewSweeper(sm,
		metric_ingestor.WithSweeperCadence(1*time.Second),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sweeper.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Sweeper.Run with cancelled ctx: err=%v, want context.Canceled", err)
	}
	if got := atomic.LoadInt32(&scans); got != 0 {
		t.Errorf("scans = %d, want 0 (cancelled-on-entry sweeper must not claim commits)", got)
	}
}

// TestSweeper_RetriesAfterInfraError covers item 2's
// resilience: a transient infrastructure error (here:
// uuid factory exhaustion -> claim failure) does NOT
// terminate the loop. The sweeper logs + backs off, then
// retries on the next iteration.
func TestSweeper_RetriesAfterInfraError(t *testing.T) {
	// idFactory: first call succeeds, subsequent calls
	// fail -- the first ProcessOne CLAIMS a commit
	// successfully, but the second ProcessOne fails to
	// mint a new scan_run_id.
	idCallCount := int32(0)
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(func() (uuid.UUID, error) {
			n := atomic.AddInt32(&idCallCount, 1)
			if n == 1 {
				return stateRunID01, nil
			}
			return uuid.Nil, errors.New("simulated infrastructure failure")
		}),
	)
	store.SeedPending(
		metric_ingestor.PendingCommit{RepoID: stateRepoIDA, SHA: "1111111111111111111111111111111111111111", CommittedAt: time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)},
		metric_ingestor.PendingCommit{RepoID: stateRepoIDA, SHA: "2222222222222222222222222222222222222222", CommittedAt: time.Date(2026, 6, 3, 11, 0, 0, 0, time.UTC)},
	)
	scanner := scannerFunc(func(_ context.Context, _ metric_ingestor.ScanRunClaim) error { return nil })
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC))),
	)

	// errorBackoff and cadence both go through the same
	// sleep channel; the test counts waits to verify the
	// sweeper backed off on the infra error.
	backoffCh := make(chan time.Time, 8)
	var waits int32
	sleepFn := func(_ time.Duration) <-chan time.Time {
		atomic.AddInt32(&waits, 1)
		return backoffCh
	}

	sweeper := metric_ingestor.NewSweeper(sm,
		metric_ingestor.WithSweeperCadence(50*time.Millisecond),
		metric_ingestor.WithSweeperErrorBackoff(50*time.Millisecond),
		metric_ingestor.WithSweeperClock(sleepFn),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- sweeper.Run(ctx) }()

	// Wait for the sweeper to have processed at least one
	// commit AND then errored on the next ProcessOne
	// (waiting in the backoff). The first ProcessOne
	// drained 1 commit (DidWork=true, no wait). The
	// second ProcessOne calls store.ClaimNextPendingCommit
	// which fails to mint a scan_run_id -> sweeper backs
	// off via sleepFn.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&waits) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&waits); got < 1 {
		t.Fatalf("waits = %d, want >= 1 (sweeper must back off after infra failure rather than busy-looping)", got)
	}

	cancel()
	backoffCh <- time.Now()
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Sweeper.Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Sweeper.Run did not return within 2s after cancel")
	}
}

// TestNewSweeper_PanicsOnNilStateMachine pins the
// constructor's defensive contract.
func TestNewSweeper_PanicsOnNilStateMachine(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewSweeper(nil) did not panic")
		}
	}()
	_ = metric_ingestor.NewSweeper(nil)
}

// TestNewSweeper_PanicsOnNonPositiveCadence pins the
// constructor's cadence validation -- a zero cadence
// would yield a busy-loop and is always a wiring bug.
func TestNewSweeper_PanicsOnNonPositiveCadence(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewSweeper(..., WithSweeperCadence(0)) did not panic")
		}
	}()
	sm := metric_ingestor.NewStateMachine(
		metric_ingestor.NewInMemoryScanRunStore(),
		scannerFunc(func(_ context.Context, _ metric_ingestor.ScanRunClaim) error { return nil }),
	)
	_ = metric_ingestor.NewSweeper(sm, metric_ingestor.WithSweeperCadence(0))
}
