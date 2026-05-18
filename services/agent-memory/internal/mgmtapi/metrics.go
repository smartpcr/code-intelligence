package mgmtapi

import "sync/atomic"

// Metrics is the sink the snapshot verb uses to surface
// progress counters. The implementation-plan §7.4 calls out
// two named series:
//
//   - snapshot_pending_total   -- monotonic counter incremented
//     by N every time `mgmt.snapshot` enqueues N new
//     `EmbeddingPublish` rows. Pending here means "enqueued
//     and not yet known to be published" from this process's
//     point of view.
//   - snapshot_published_total -- monotonic counter incremented
//     by the EmbeddingIndex writer (Repo Indexer / Concept
//     Promoter) each time a snapshot-driven publish reaches
//     `event_kind='published'`. The mgmt-api handler does NOT
//     own this transition; the interface is exposed here so
//     the same metrics implementation can be wired from the
//     downstream worker.
//
// Both counters are intentionally process-local. A clustered
// deployment scrapes each replica and sums them via the
// Prometheus / collector layer. Implementations MUST be safe
// for concurrent use.
//
// The default implementation [NoOpMetrics] is the zero value
// of the package-internal `noopMetrics` type. Callers that
// want observable counters wire [NewInMemoryMetrics].
type Metrics interface {
	// IncSnapshotPending records that n new snapshot-driven
	// EmbeddingPublish rows were enqueued. n is non-negative;
	// callers passing 0 (e.g. snapshot on an empty repo) MUST
	// still call this with n=0 so log-shape parity is
	// preserved.
	IncSnapshotPending(n int)

	// IncSnapshotPublished records that n snapshot-driven
	// publishes reached `event_kind='published'`. The mgmt-api
	// handler does NOT call this; the EmbeddingIndex writer
	// owns the transition (tech-spec §9.6a).
	IncSnapshotPublished(n int)
}

// NoOpMetrics is a Metrics that drops every increment. Used
// as the default when [Options.Metrics] is nil so the handler
// can call the interface unconditionally without a nil-check
// per call site.
type NoOpMetrics struct{}

// IncSnapshotPending implements [Metrics].
func (NoOpMetrics) IncSnapshotPending(int) {}

// IncSnapshotPublished implements [Metrics].
func (NoOpMetrics) IncSnapshotPublished(int) {}

// InMemoryMetrics is the default observable Metrics
// implementation. It exposes the two counters as atomic
// int64s so /metrics scrapes do not race with handler writes.
// Callers wire one instance per process and read the
// counters via [InMemoryMetrics.Snapshot] when serving
// /metrics.
//
// Multi-replica deployments still aggregate at the Prometheus
// layer; this struct is per-process.
type InMemoryMetrics struct {
	pending   atomic.Int64
	published atomic.Int64
}

// NewInMemoryMetrics returns a zero-valued [InMemoryMetrics]
// ready for use. Provided as a function (vs returning the
// struct literal) so we can extend the struct with non-
// trivial init later without breaking callers.
func NewInMemoryMetrics() *InMemoryMetrics { return &InMemoryMetrics{} }

// IncSnapshotPending implements [Metrics]. Negative n is
// treated as zero to preserve the counter monotonicity
// invariant Prometheus requires.
func (m *InMemoryMetrics) IncSnapshotPending(n int) {
	if n <= 0 {
		return
	}
	m.pending.Add(int64(n))
}

// IncSnapshotPublished implements [Metrics]. See
// [InMemoryMetrics.IncSnapshotPending] for the monotonicity
// rationale.
func (m *InMemoryMetrics) IncSnapshotPublished(n int) {
	if n <= 0 {
		return
	}
	m.published.Add(int64(n))
}

// Snapshot returns the current counter values. Safe for
// concurrent use with [InMemoryMetrics.IncSnapshotPending] /
// [InMemoryMetrics.IncSnapshotPublished]; the two atomic
// loads are not transactionally consistent with each other,
// but each value is independently up-to-date.
func (m *InMemoryMetrics) Snapshot() (pending, published int64) {
	return m.pending.Load(), m.published.Load()
}
