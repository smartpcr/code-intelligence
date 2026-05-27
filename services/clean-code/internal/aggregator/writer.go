package aggregator

import (
	"context"
	"sync"
)

// SnapshotWriter is the write-side dependency the aggregator
// invokes once per tick to persist the derived rows. The
// production implementation is [PGSnapshotWriter] which fires
// three INSERTs (one per snapshot table) inside a single
// transaction; tests inject [InMemorySnapshotWriter] to capture
// the call shape without the PG round-trip.
//
// The contract:
//
//   - WriteSnapshots is invoked at most once per tick.
//   - The three slices share the SAME `BuiltAt` timestamp (set
//     by [Aggregator.Tick] before calling); the writer MUST
//     persist that timestamp verbatim. No "now()" defaults at
//     write time.
//   - All three slices are insert-only -- the snapshot tables
//     have `INSERT, SELECT` grants and explicit
//     `REVOKE UPDATE, DELETE` for the
//     `clean_code_xrepo_aggregator` role (migration
//     0004_roles.up.sql lines 395-397 / 416-418). Implementations
//     MUST NOT issue UPDATE or DELETE -- snapshot rows are
//     append-only derivative views per G6.
//   - Either ALL three inserts succeed or NONE do. The
//     [PGSnapshotWriter] uses a single PG transaction; the
//     in-memory variant is atomic via mutex. A partial write
//     would let readers see an inconsistent "latest by built_at"
//     across the three tables for one tick.
type SnapshotWriter interface {
	WriteSnapshots(ctx context.Context, snap Snapshots) error
}

// Snapshots bundles the three slices the aggregator produces per
// tick. They are passed as one value so the [SnapshotWriter]
// implementation can serialise them inside a single transaction.
type Snapshots struct {
	RepoMetric       []RepoMetricSnapshotRow
	CrossRepoPercent []CrossRepoPercentileRow
	Portfolio        []PortfolioSnapshotRow
}

// InMemorySnapshotWriter is the test-side [SnapshotWriter]. Each
// successful `WriteSnapshots` appends one [Snapshots] entry to a
// goroutine-safe slice; tests assert on the captured shape.
type InMemorySnapshotWriter struct {
	mu     sync.Mutex
	writes []Snapshots
	// failErr, when non-nil, is returned by every
	// WriteSnapshots call without recording the snapshot. Tests
	// set it to simulate a PG outage and assert the loop keeps
	// ticking after backoff.
	failErr error
}

// NewInMemorySnapshotWriter constructs an empty in-memory writer.
func NewInMemorySnapshotWriter() *InMemorySnapshotWriter {
	return &InMemorySnapshotWriter{}
}

// SetFailError configures the writer to return `err` on every
// subsequent WriteSnapshots call. Pass nil to clear.
func (w *InMemorySnapshotWriter) SetFailError(err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failErr = err
}

// WriteSnapshots implements [SnapshotWriter]. Snapshots are
// recorded in invocation order so the test can inspect the full
// history of ticks the aggregator executed.
func (w *InMemorySnapshotWriter) WriteSnapshots(ctx context.Context, snap Snapshots) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failErr != nil {
		return w.failErr
	}
	// Deep-copy the slices so a caller mutating its locals after
	// the write does not perturb the captured history.
	cp := Snapshots{
		RepoMetric:       append([]RepoMetricSnapshotRow(nil), snap.RepoMetric...),
		CrossRepoPercent: append([]CrossRepoPercentileRow(nil), snap.CrossRepoPercent...),
		Portfolio:        append([]PortfolioSnapshotRow(nil), snap.Portfolio...),
	}
	w.writes = append(w.writes, cp)
	return nil
}

// Writes returns a copy of the captured tick history. Tests
// assert on the resulting slice.
func (w *InMemorySnapshotWriter) Writes() []Snapshots {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]Snapshots, len(w.writes))
	copy(out, w.writes)
	return out
}
