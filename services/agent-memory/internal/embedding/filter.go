package embedding

// RecallFilter implements the §9.6a read-side primitive:
// given a candidate set of Qdrant point_ids returned by a
// vector search, drop the ones whose latest
// `embedding_publish_event` is NOT `published`.  The §9.6a
// read protocol pins this as the sole gating step a Qdrant
// hit must pass before being surfaced by `agent.recall`
// (tech-spec §9.6a "A vector hit from Qdrant is dereferenced
// through the most recent `EmbeddingPublishEvent` for that
// `publish_id`.  The hit is surfaced **iff** the latest event
// is 'published'.").
//
// The filter ALSO increments the `recall_filter_unpublished_total`
// counter (implementation-plan.md:505, :697-697, :728, :1380)
// per filtered hit so operators can detect a publish backlog
// without scraping the event log themselves.
//
// Why this lives in `internal/embedding`
// --------------------------------------
// The publisher OWNS the `embedding_publish` / `_event` tables;
// the §9.6a invariant "latest event must be `published` for
// the vector to be visible" is the write-side and read-side
// contract of the *same* table pair.  Putting the filter in
// any other package (e.g. internal/agentapi/recall.go where
// the recall pipeline lives) would risk the two halves
// diverging across a future schema change.  The recall path
// (Stage 4.1, not yet built) IMPORTS this filter; it does not
// re-implement the SQL.
//
// The interface is small on purpose: callers hand in
// `point_ids` (the natural surface of a Qdrant hit batch),
// not `publish_ids`, because the recall path never observes
// the publish_id surface directly.  The filter joins on
// `qdrant_point_id` to dereference.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/lib/pq"
)

// RecallMetrics is the operator-visible counter set the filter
// increments.  Exposed as a struct (not package-level globals)
// so the Stage 7 Prometheus binding can register one counter
// per `RecallFilter` instance and so unit tests can assert
// counter movement without sharing global state.
//
// All counters are monotonically non-decreasing and safe for
// concurrent use across goroutines (`atomic.Int64`).  A future
// reset path (`Reset()` on operator request) is intentionally
// absent — Prometheus counters do not reset, and the operator-
// triage workflow assumes a process-lifetime-monotonic count.
type RecallMetrics struct {
	// UnpublishedFiltered is the per-hit count of Qdrant
	// points whose latest `embedding_publish_event` was
	// NOT `published` at the moment the filter ran.  The
	// expected emission name is `recall_filter_unpublished_total`
	// (implementation-plan.md:505).  Callers translate this
	// `atomic.Int64` into the Prometheus counter at
	// scrape time.
	UnpublishedFiltered atomic.Int64
}

// Snapshot returns a stable read of every counter at a single
// instant.  Useful for tests that want to assert "the count
// went up by N during this call" without racing the metric.
type RecallMetricsSnapshot struct {
	UnpublishedFiltered int64
}

func (m *RecallMetrics) Snapshot() RecallMetricsSnapshot {
	return RecallMetricsSnapshot{
		UnpublishedFiltered: m.UnpublishedFiltered.Load(),
	}
}

// RecallFilter is the §9.6a read-side gate.  Construct via
// `NewRecallFilter`; one instance is safe for concurrent use
// across goroutines (the underlying `*sql.DB` and
// `*RecallMetrics` are concurrency-safe).
type RecallFilter struct {
	db      *sql.DB
	metrics *RecallMetrics
}

// NewRecallFilter constructs a `RecallFilter` over `db`.  The
// `db` should be the `agent_memory_reader` role connection
// (migration 0017 grants SELECT on the publish tables to that
// role); the filter never writes.  Panics on nil `db` — the
// filter cannot operate without one and a silent no-op would
// silently serve stale vectors to recall.
//
// Pass `nil` for `metrics` to opt out of counter tracking
// (e.g. for ad-hoc shell tools); production callers MUST
// supply a real `*RecallMetrics` so the §9.6a backlog signal
// reaches operators.
func NewRecallFilter(db *sql.DB, metrics *RecallMetrics) *RecallFilter {
	if db == nil {
		panic("embedding: NewRecallFilter: nil *sql.DB")
	}
	if metrics == nil {
		metrics = &RecallMetrics{}
	}
	return &RecallFilter{db: db, metrics: metrics}
}

// Metrics returns the metric set the filter writes to.  Use
// `Metrics().Snapshot()` from tests; production callers wire
// the same pointer into the Prometheus registry.
func (f *RecallFilter) Metrics() *RecallMetrics {
	return f.metrics
}

// FilterPublishedPoints returns the subset of `pointIDs`
// whose backing publish row's latest
// `embedding_publish_event` is `published`.  Points missing
// from the publish table entirely (i.e. an orphan Qdrant
// point with no PostgreSQL row) are filtered out — the §9.6a
// invariant requires both halves; we cannot serve a point
// without provenance.
//
// Behaviour pinned by the §9.6a read protocol (tech-spec
// §9.6a):
//
//   - Empty input returns `(nil, nil)` — no SQL fired, no
//     counter movement.
//   - Order of the returned slice is the input order with
//     filtered entries removed (callers often want to walk
//     the filtered list parallel to a parallel slice such as
//     Qdrant scores).
//   - Duplicate `pointID`s in the input are preserved in the
//     output if their state allows (this lets a caller probe
//     "any of these N candidates is published" cheaply).
//   - The counter is incremented by `len(input) - len(output)`,
//     i.e. once per filtered hit.  A points-not-in-publish-
//     table entry counts as filtered.
//
// Errors:
//
//   - Network / SQL failures bubble up unwrapped; the caller
//     should treat them as a recall degradation (per §9.5
//     fallback to structural prior) rather than as missing
//     data.
//   - Invalid (non-UUID) point ids surface as a `pq` syntax
//     error from the `uuid[]` cast — bare-string filtering
//     here would silently hide a wiring bug elsewhere.
func (f *RecallFilter) FilterPublishedPoints(
	ctx context.Context,
	pointIDs []string,
) ([]string, error) {
	if len(pointIDs) == 0 {
		return nil, nil
	}

	// Build the lookup set: ONE SQL round-trip via pq's
	// `Array` binding.  The query joins the publish row to
	// its latest event with `DISTINCT ON` and returns only
	// the rows whose latest event_kind is 'published'.
	//
	// We DO NOT use `ORDER BY ... DESC LIMIT 1` in a
	// correlated subquery because that pattern scans the
	// partitioned event table once per input point on
	// PostgreSQL 16 (the partition-pruning planner cannot
	// see the correlation).  The `DISTINCT ON` form below
	// pushes a single partition-pruned sort that hits the
	// `(publish_id, created_at DESC)` index from migration
	// 0015 once.
	const q = `
		WITH latest AS (
		    SELECT DISTINCT ON (e.publish_id)
		        e.publish_id,
		        e.event_kind
		    FROM embedding_publish_event e
		    JOIN embedding_publish p
		      ON p.publish_id = e.publish_id
		    WHERE p.qdrant_point_id = ANY($1::uuid[])
		    ORDER BY e.publish_id, e.created_at DESC, e.event_id DESC
		)
		SELECT p.qdrant_point_id::text
		FROM embedding_publish p
		JOIN latest l ON l.publish_id = p.publish_id
		WHERE p.qdrant_point_id = ANY($1::uuid[])
		  AND l.event_kind = 'published'
	`

	rows, err := f.db.QueryContext(ctx, q, pq.Array(pointIDs))
	if err != nil {
		return nil, fmt.Errorf("embedding: FilterPublishedPoints query: %w", err)
	}
	defer rows.Close()

	publishedSet := make(map[string]struct{}, len(pointIDs))
	for rows.Next() {
		var pid string
		if err := rows.Scan(&pid); err != nil {
			return nil, fmt.Errorf("embedding: FilterPublishedPoints scan: %w", err)
		}
		publishedSet[strings.ToLower(pid)] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("embedding: FilterPublishedPoints rows: %w", err)
	}

	out := make([]string, 0, len(pointIDs))
	filtered := int64(0)
	for _, pid := range pointIDs {
		if _, ok := publishedSet[strings.ToLower(pid)]; ok {
			out = append(out, pid)
			continue
		}
		filtered++
	}
	if filtered > 0 {
		f.metrics.UnpublishedFiltered.Add(filtered)
	}
	return out, nil
}
