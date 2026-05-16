package repoindexer

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testBarrier is a one-shot barrier used by the pool fan-out
// integration test to guarantee N workers ALL hold their
// respective claims concurrently before any of them is allowed
// to complete. Without it, the test was scheduler-dependent: a
// fast in-memory job could be claimed and finished by one
// goroutine before its siblings ever entered ProcessOnce, and
// the post-run "distinct claimed_by >= 2" assertion would flap.
//
// Usage pattern (in the test):
//
//	bar := newTestBarrier(2)
//	mat := &barrierMaterializer{Inner: &InMemoryMaterializer{...}, Barrier: bar}
//	// every PerWorker.Materializer points at `mat` so each Materialize
//	// call participates in the same barrier.
//
// The first N-1 arrivers block in Wait; the Nth arriver triggers
// the release (idempotent via sync.Once so it is safe even if
// >N workers somehow arrive). Wait honors ctx cancellation so a
// stuck barrier (e.g. only one worker entered Materialize before
// the test deadline) surfaces as a context error rather than
// hanging the test process.
type testBarrier struct {
	n        int32
	count    atomic.Int32
	released chan struct{}
	once     sync.Once
}

func newTestBarrier(n int) *testBarrier {
	if n < 1 {
		panic("repoindexer: newTestBarrier: n must be >= 1")
	}
	return &testBarrier{n: int32(n), released: make(chan struct{})}
}

// Wait blocks until N callers have arrived OR ctx is cancelled.
// Returns ctx.Err() if cancellation wins the race; nil
// otherwise. The release is idempotent: callers arriving after
// the Nth (e.g. when the pool spawns more workers than there
// are jobs to claim) pass through immediately.
func (b *testBarrier) Wait(ctx context.Context) error {
	if b.count.Add(1) >= b.n {
		b.once.Do(func() { close(b.released) })
	}
	select {
	case <-b.released:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// barrierMaterializer wraps an inner Materializer and forces
// every Materialize call to pass through a one-shot testBarrier
// before delegating. A worker that successfully claimed a row
// CANNOT advance past Materialize until N-1 siblings have
// claimed their own rows -- which makes the post-run
// "distinct claimed_by >= 2" assertion deterministic regardless
// of goroutine scheduling.
type barrierMaterializer struct {
	Inner   Materializer
	Barrier *testBarrier
}

func (b *barrierMaterializer) Materialize(ctx context.Context, repoURL, sha string) (Workspace, error) {
	if b.Barrier == nil {
		return nil, errors.New("repoindexer: barrierMaterializer: nil Barrier")
	}
	if err := b.Barrier.Wait(ctx); err != nil {
		return nil, err
	}
	return b.Inner.Materialize(ctx, repoURL, sha)
}

// TestTestBarrier_releasesOnNthArrival is the happy-path
// proof-of-correctness for the barrier helper used by
// TestPool_runsConfiguredWorkerCount. N goroutines all call
// Wait; the Nth arriver releases everyone. The test fails by
// timeout if any arriver remains blocked.
func TestTestBarrier_releasesOnNthArrival(t *testing.T) {
	t.Parallel()
	const n = 4
	bar := newTestBarrier(n)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := bar.Wait(ctx); err != nil {
				t.Errorf("Wait returned error: %v", err)
			}
		}()
	}
	wg.Wait()
}

// TestTestBarrier_excessArriversPassThrough proves the
// idempotent-release contract: once N arrivers have triggered
// the close, additional arrivers (e.g. when Pool spawns more
// workers than there are jobs and a "no work today" worker
// somehow enters Materialize) MUST pass through immediately
// rather than panicking on a double close.
func TestTestBarrier_excessArriversPassThrough(t *testing.T) {
	t.Parallel()
	bar := newTestBarrier(2)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// First two arrivers release the barrier.
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = bar.Wait(ctx) }()
	}
	wg.Wait()

	// Late arrivers must pass through cleanly.
	for i := 0; i < 3; i++ {
		if err := bar.Wait(ctx); err != nil {
			t.Errorf("late Wait #%d returned %v; want nil pass-through", i, err)
		}
	}
}

// TestTestBarrier_honorsContextCancellation pins the
// rubber-duck-driven contract that a stuck barrier (only one
// arriver, never enough to release) surfaces as ctx.Err()
// rather than hanging the test process. Without this guard a
// flaky pool test could deadlock the whole `go test` run.
func TestTestBarrier_honorsContextCancellation(t *testing.T) {
	t.Parallel()
	bar := newTestBarrier(2) // need 2, only 1 arrives
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := bar.Wait(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait err = %v; want context.DeadlineExceeded", err)
	}
}

// _ keeps the unused-import sentinel honest in case a future
// edit drops the atomic reference -- atomic.Int32 is used by
// the testBarrier above.
var _ = atomic.Int32{}
