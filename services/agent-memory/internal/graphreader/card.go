package graphreader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// TraceObservation is the per-edge aggregate counters loaded
// alongside an Edge when it is exposed through a
// NeighborhoodCard. The shape mirrors the `trace_observation`
// schema from migration 0005.
//
// When an Edge has no `trace_observation` row (e.g. a
// `static_calls` edge that was never observed), the Card's
// `TraceObservation` field is nil. Distinguishing nil from a
// zero-value struct lets the caller render "never observed" vs.
// "observed zero times since the partition rolled".
type TraceObservation struct {
	// ObservationCount is the lifetime span count attributed to
	// this Edge.
	ObservationCount int64
	// P50LatencyMs / P95LatencyMs are the latency aggregates
	// the Span Ingestor maintains (architecture.md §5.2.3).
	P50LatencyMs float64
	P95LatencyMs float64
	// LatestSpanRef is the most recent `trace_id` (or
	// composite ref) the ingestor attributed to this Edge.
	// Empty when no span has landed.
	LatestSpanRef string
	// LastObservedAt is the wall-clock time of the most recent
	// span attribution. Zero when no span has landed.
	LastObservedAt time.Time
}

// CardEdge is the per-edge entry inside a NeighborhoodCard. It
// embeds the structural Edge plus the optional aggregate so
// `agent.expand` / `mgmt.read.graph_node` callers can rank by
// observation count without a follow-up join.
type CardEdge struct {
	Edge
	// TraceObservation is non-nil when the underlying Edge has
	// a matching `trace_observation` row.
	TraceObservation *TraceObservation
}

// NeighborhoodCard is the structured response of
// `Reader.NeighborhoodCard`: the seed Node plus every
// (outbound) Edge from it and the per-Edge TraceObservation
// aggregate. This is the building block `agent.expand`
// (architecture.md §4.5) and `mgmt.read.graph_node` use to
// materialise a single 1-hop view with hot-path counters.
type NeighborhoodCard struct {
	// Node is the seed row; the Reader caller specified its
	// node_id. Retirement metadata is attached when
	// `ReaderOptions.IncludeRetired = true` exposed a retired
	// row.
	Node Node
	// Edges are the outbound edges from `Node`. They are
	// returned in `kind, edge_id` order so the response is
	// snapshot-test stable.
	Edges []CardEdge
}

// NeighborhoodCard returns the seed Node plus every outbound
// Edge with the per-Edge `TraceObservation` aggregate joined in.
// The required test scenario is "neighborhood card resolves
// observed_calls": given a Method Node with one observed_calls
// edge whose TraceObservation.observation_count = 42, the card
// must list the edge with observation_count = 42.
//
// Retirement semantics mirror the rest of the reader:
//
//   - When `opts.IncludeRetired = false`, retired Nodes return
//     ErrNotFound (matching GetNode) AND retired Edges are
//     filtered out of the result.
//
//   - When `opts.IncludeRetired = true`, retired Nodes are
//     returned with `Node.Retirement` set, AND retired Edges
//     remain in `Edges` with `CardEdge.Retirement` set.
//
// Snapshot consistency
// --------------------
// The seed-Node lookup and the outbound-edge scan run inside
// the same `REPEATABLE READ`, read-only transaction. That
// guarantees the returned card represents one coherent slice
// of the graph: an inflight `node_retirement` insert that
// lands between the two queries cannot cause the card to
// surface a "current" Node alongside edges that the
// post-retirement view would filter out. Without this
// transaction the writer's tombstone race window would let
// `agent.expand` return a card whose Node and Edges disagreed
// on liveness.
func (r *Reader) NeighborhoodCard(
	ctx context.Context, nodeID string, opts ReaderOptions,
) (NeighborhoodCard, error) {
	if nodeID == "" {
		return NeighborhoodCard{}, errors.New("graphreader: NeighborhoodCard: empty node_id")
	}

	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return NeighborhoodCard{}, fmt.Errorf("graphreader: begin tx: %w", err)
	}
	// Read-only transactions never produce conflicts to commit;
	// `Rollback` is always safe and matches the "release the
	// snapshot, return the conn to the pool" semantics we
	// actually want. pgx ignores Rollback after Commit, so
	// `defer tx.Rollback` is idempotent.
	defer func() { _ = tx.Rollback(ctx) }()

	node, err := r.getNodeTx(ctx, tx, nodeID, opts.IncludeRetired)
	if err != nil {
		// ErrNotFound passes through unwrapped so callers can
		// errors.Is() match the same sentinel they get from
		// GetNode.
		return NeighborhoodCard{}, err
	}

	edges, err := r.listCardEdgesTx(ctx, tx, nodeID, opts.IncludeRetired)
	if err != nil {
		return NeighborhoodCard{}, err
	}
	return NeighborhoodCard{Node: node, Edges: edges}, nil
}

// getNodeTx is the in-transaction variant of GetNode used by
// NeighborhoodCard's snapshot-consistent path. The SQL is
// identical to selectNodeQuery — we just route the QueryRow
// through the open transaction so it sees the same
// REPEATABLE READ snapshot as the edge scan.
func (r *Reader) getNodeTx(
	ctx context.Context, tx pgx.Tx, nodeID string, includeRetired bool,
) (Node, error) {
	query := selectNodeQuery(includeRetired)
	row := tx.QueryRow(ctx, query, nodeID)
	return scanNodeRow(row, includeRetired)
}

// listCardEdgesTx is the in-transaction variant of the edge
// scan path used by NeighborhoodCard. Same SQL as listCardEdges
// but routed through the open transaction so both queries
// observe the same MVCC snapshot.
func (r *Reader) listCardEdgesTx(
	ctx context.Context, tx pgx.Tx, srcNodeID string, includeRetired bool,
) ([]CardEdge, error) {
	query := neighborhoodEdgesQuery(includeRetired)
	rows, err := tx.Query(ctx, query, srcNodeID)
	if err != nil {
		return nil, fmt.Errorf("graphreader: NeighborhoodCard edges: %w", err)
	}
	defer rows.Close()
	return scanCardEdgeRows(rows, includeRetired)
}

// scanCardEdgeRows decodes the edge + observation projection
// row-by-row. It is intentionally separate from scanEdgeRows
// (reader.go) because the SELECT carries five extra trailing
// columns (the trace_observation tuple).
func scanCardEdgeRows(rows pgx.Rows, includeRetired bool) ([]CardEdge, error) {
	var out []CardEdge
	for rows.Next() {
		ce, err := scanCardEdgeRow(rows, includeRetired)
		if err != nil {
			return nil, err
		}
		out = append(out, ce)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphreader: NeighborhoodCard rows: %w", err)
	}
	return out, nil
}

// scanCardEdgeRow decodes a single Edge + retirement +
// TraceObservation row in one pass. Layout matches
// neighborhoodEdgesQuery: the edge projection columns first, the
// optional retirement pair (when `includeRetired = true`), then
// the five trace_observation columns last.
//
// The trace_observation columns are nullable (LEFT JOIN). The
// observation_count pointer is the discriminator: nil means the
// edge has no aggregate row, which we surface as
// `CardEdge.TraceObservation = nil`.
func scanCardEdgeRow(row rowScanner, includeRetired bool) (CardEdge, error) {
	var (
		e      Edge
		fp     []byte
		attrs  []byte
		retSHA *string
		retAt  *time.Time

		obsCount       *int64
		p50            *float64
		p95            *float64
		latestSpanRef  *string
		lastObservedAt *time.Time
	)
	dest := []any{
		&e.EdgeID, &e.RepoID, &fp, &e.Kind,
		&e.SrcNodeID, &e.DstNodeID, &e.FromSHA, &attrs,
	}
	if includeRetired {
		dest = append(dest, &retSHA, &retAt)
	}
	dest = append(dest,
		&obsCount, &p50, &p95, &latestSpanRef, &lastObservedAt,
	)
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return CardEdge{}, ErrNotFound
		}
		return CardEdge{}, fmt.Errorf("graphreader: scan card edge: %w", err)
	}
	if len(fp) > 0 {
		sum, err := fingerprint.SumFromBytes(fp)
		if err != nil {
			return CardEdge{}, fmt.Errorf("graphreader: decode edge fingerprint: %w", err)
		}
		e.Fingerprint = sum
	}
	e.AttrsJSON = append(json.RawMessage(nil), attrs...)
	if includeRetired && retSHA != nil {
		ret := &EdgeRetirement{RetiredAtSHA: *retSHA}
		if retAt != nil {
			ret.RetiredAt = *retAt
		}
		e.Retirement = ret
	}
	ce := CardEdge{Edge: e}
	if obsCount != nil {
		obs := &TraceObservation{ObservationCount: *obsCount}
		if p50 != nil {
			obs.P50LatencyMs = *p50
		}
		if p95 != nil {
			obs.P95LatencyMs = *p95
		}
		if latestSpanRef != nil {
			obs.LatestSpanRef = *latestSpanRef
		}
		if lastObservedAt != nil {
			obs.LastObservedAt = *lastObservedAt
		}
		ce.TraceObservation = obs
	}
	return ce, nil
}

// neighborhoodEdgesQuery is the SQL behind
// listCardEdges. It returns the same projection as
// `selectEdgesFromQuery` plus five trailing columns from the
// LEFT-joined `trace_observation` row, in the order matched by
// scanCardEdgeRow.
//
// Stable sort `e.kind, e.edge_id` so successive cards over the
// same Node return rows in the same sequence — the Stage 2.2
// brief's `NeighborhoodCard` test scenario asserts on a single
// edge with observation_count = 42, but downstream stages will
// snapshot multi-edge cards and rely on deterministic order.
//
// Parameter shape: $1 = src_node_id (text).
func neighborhoodEdgesQuery(includeRetired bool) string {
	if includeRetired {
		return `
			SELECT ` + edgeProjectionWithRetirement + `,
				obs.observation_count,
				obs.p50_latency_ms,
				obs.p95_latency_ms,
				obs.latest_span_ref,
				obs.last_observed_at
			FROM edge e
			LEFT JOIN edge_retirement er ON er.edge_id = e.edge_id
			LEFT JOIN trace_observation obs ON obs.edge_id = e.edge_id
			WHERE e.src_node_id = $1
			ORDER BY e.kind, e.edge_id
		`
	}
	return `
		SELECT ` + edgeProjectionCurrent + `,
			obs.observation_count,
			obs.p50_latency_ms,
			obs.p95_latency_ms,
			obs.latest_span_ref,
			obs.last_observed_at
		FROM edge e
		LEFT JOIN trace_observation obs ON obs.edge_id = e.edge_id
		WHERE e.src_node_id = $1
		AND NOT EXISTS (
			SELECT 1 FROM edge_retirement er WHERE er.edge_id = e.edge_id
		)
		ORDER BY e.kind, e.edge_id
	`
}
