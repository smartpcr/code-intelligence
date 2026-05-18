package snapshot

import "sync/atomic"

// Metric name constants — the binary's `/metrics` endpoint and
// any downstream collector key off these literals. Centralised
// here so a typo in one call site does not silently un-pair a
// counter from its exposition. Mirrors the pattern used by the
// sibling promoter and consolidator packages.
//
// The two counters are the §6.2.1 spec's named pair for the
// `mgmt.snapshot` verb:
//
//   - `snapshot_pending_total` — cumulative across the binary's
//     lifetime. Incremented at enqueue time, once per target
//     row written. NOT a backlog gauge; the spec wording
//     ("snapshot_pending_total") is honoured as a counter so the
//     operator's `rate(snapshot_pending_total[5m])` query yields
//     enqueue throughput. A separate snapshot pipeline-backlog
//     gauge is intentionally out of scope for v1.
//
//   - `snapshot_published_total` — cumulative across the
//     binary's lifetime. Incremented by the publisher's
//     transition-to-published hook (see `internal/embedding/
//     publisher.go`) ONLY when the queued event for the
//     just-published row carried a `supersedes_publish_id`
//     marker. Pairing with `snapshot_pending_total` lets an
//     operator detect snapshot stalls without parsing logs:
//     a healthy snapshot ends with `pending == published`.
const (
	MetricSnapshotPendingTotal   = "snapshot_pending_total"
	MetricSnapshotPublishedTotal = "snapshot_published_total"
)

// Metrics is the package's atomic-counter surface. Construct
// via [NewMetrics]; read via Snapshot. All counters are
// goroutine-safe via `sync/atomic` so the verb handler and
// the publisher's transition-to-published hook can increment
// concurrently without locking.
type Metrics struct {
	pending   atomic.Uint64
	published atomic.Uint64
}

// NewMetrics returns a zero-initialised Metrics.
func NewMetrics() *Metrics { return &Metrics{} }

// IncPending adds n to `snapshot_pending_total`. Called by the
// snapshot Service once per target row written.
func (m *Metrics) IncPending(n uint64) {
	if m == nil || n == 0 {
		return
	}
	m.pending.Add(n)
}

// IncPublished adds n to `snapshot_published_total`. Called by
// the publisher's transition-to-published hook when the queued
// event for the just-published row carried a
// `supersedes_publish_id` marker.
func (m *Metrics) IncPublished(n uint64) {
	if m == nil || n == 0 {
		return
	}
	m.published.Add(n)
}

// PendingTotal returns the snapshot_pending_total value.
func (m *Metrics) PendingTotal() uint64 {
	if m == nil {
		return 0
	}
	return m.pending.Load()
}

// PublishedTotal returns the snapshot_published_total value.
func (m *Metrics) PublishedTotal() uint64 {
	if m == nil {
		return 0
	}
	return m.published.Load()
}

// Snapshot returns a freshly-allocated map of the package's
// counters keyed by the Metric* constants. Safe to iterate
// without holding a lock against concurrent IncX writers.
func (m *Metrics) Snapshot() map[string]uint64 {
	if m == nil {
		return map[string]uint64{
			MetricSnapshotPendingTotal:   0,
			MetricSnapshotPublishedTotal: 0,
		}
	}
	return map[string]uint64{
		MetricSnapshotPendingTotal:   m.pending.Load(),
		MetricSnapshotPublishedTotal: m.published.Load(),
	}
}
