package graphreader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ErrNotFound is returned by GetNode / GetEdge / NeighborhoodCard
// when the target row is missing — either because it was never
// inserted OR because it is retired and ReaderOptions.IncludeRetired
// is false. Both cases collapse onto a single sentinel because the
// reader contract treats "current view" as "not retired"; callers
// who need to disambiguate "never existed" from "retired" must
// re-query with `IncludeRetired = true`.
//
// Pattern-match with `errors.Is(err, graphreader.ErrNotFound)`.
var ErrNotFound = errors.New("graphreader: row not found")

// MaxListLimit caps every list-style read (ListNodes,
// ListEdgesFrom, ListEdgesTo) at ten thousand rows. Without this
// bound a repo with tens of thousands of Nodes — or a pathological
// file Node with thousands of `contains` edges, or a popular
// callee Node with thousands of inbound `static_calls` edges —
// would stream every row into the reader's address space on every
// `agent.recall` / `agent.expand` / `mgmt.read.*` request, which
// is the hot path the cap defends. The inbound (`ListEdgesTo`)
// path applies the SAME clamp via `appendLimit` so caller-fan-in
// scans (e.g. `agent.expand(direction='callers')`) cannot bypass
// the cap.
//
// The cap is BOTH the default (applied when a caller passes
// `Limit <= 0`) AND the maximum (caller-supplied values above
// this number are silently clamped down). Two roles, one number,
// because the policy here is a defence — not a UX-tunable page
// size. Callers who explicitly need < MaxListLimit rows can set
// `Limit` to a smaller value; nobody needs more in a single
// round-trip.
//
// Silent clamping is intentional but lossy: a caller asking for
// 50 000 rows and receiving 10 000 has no in-band signal that the
// result was truncated. Stage 2.2 ships this as the conservative
// safety net and explicitly defers cursor-based pagination — the
// stable `ORDER BY` on every list query (see query.go) makes
// `(last_key, limit)` seek-pagination straightforward to bolt on
// when a future surface demands exhaustive enumeration.
const MaxListLimit = 10_000

// normaliseLimit returns the effective `LIMIT` applied to a
// list-style query for the supplied caller-requested value. See
// MaxListLimit for the cap policy.
//
// Pulled out as a standalone helper so the same clamp rule is
// shared by `ListNodes` (which reads `ListNodesFilter.Limit`),
// `ListEdgesFrom`, and `ListEdgesTo` (both of which read
// `ReaderOptions.Limit`); a future cursor-pagination patch will
// reuse it for the per-page fetch size.
func normaliseLimit(requested int) int {
	if requested <= 0 || requested > MaxListLimit {
		return MaxListLimit
	}
	return requested
}

// Reader is the read-only access path for the structural graph
// tables. Construct one with `New`; the underlying pgxpool.Pool
// is owned by the caller (typically built via `NewPool`).
//
// Reader is safe for concurrent use: every public method takes a
// fresh acquire from the pool, runs a single SELECT, and returns.
// There is no per-Reader state mutated between calls.
type Reader struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// New constructs a Reader over the supplied pgxpool.Pool. The
// pool MUST be authenticated as a role with SELECT on every
// table listed in migration 0017 (typically `agent_memory_ro`);
// a writer-role pool also works but is not the recommended
// production wiring (G5 defence-in-depth — see doc.go).
//
// A nil logger is replaced with slog.Default(). A nil pool
// panics — passing a nil pool is unambiguously a programmer bug
// that would otherwise surface as a NPE on the first read.
func New(pool *pgxpool.Pool, logger *slog.Logger) *Reader {
	if pool == nil {
		panic("graphreader: nil *pgxpool.Pool")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reader{
		pool:   pool,
		logger: logger,
	}
}

// ReaderOptions controls per-call behaviour shared across the
// Reader entry points. The zero value is the production default
// (`current view, no retired rows, default result cap`) so
// omitting the parameter gives the safest behaviour.
type ReaderOptions struct {
	// IncludeRetired drops the tombstone anti-join, returning
	// retired Node / Edge rows alongside current ones. When set,
	// each returned struct's `Retirement` field is populated so
	// callers can render the retirement metadata
	// (`retired_at_sha`, `retired_at`, `superseded_by_node_id`).
	//
	// This is the opt-in path implementation-plan.md §"Stage
	// 2.4 RecallContextLog append helper" calls out for
	// `mgmt.read.context` so historical contexts remain
	// inspectable after their referenced rows are retired
	// (risk §9.13).
	IncludeRetired bool

	// Limit caps the number of rows returned by `ListEdgesFrom`
	// and `ListEdgesTo` (the outbound and inbound edge-listing
	// entry points). It is the "analogous option" — alongside the
	// `ListNodesFilter.Limit` field consumed by `ListNodes` —
	// that bounds the list-style entry points.
	//
	// Clamping policy (shared with `ListNodesFilter.Limit`):
	//
	//   * `Limit <= 0`             → `MaxListLimit` (default)
	//   * `0 < Limit <= MaxListLimit` → used as-is
	//   * `Limit > MaxListLimit`   → `MaxListLimit` (clamped)
	//
	// Honoured by `ListEdgesFrom` AND `ListEdgesTo` — both apply
	// the same `appendLimit` helper so caller-fan-in (inbound)
	// and callee-fan-out (outbound) scans share one cap. `GetNode`
	// and `GetEdge` are single-row lookups so `Limit` is
	// meaningless there; `NeighborhoodCard` is a 1-hop scan whose
	// fan-out is bounded by the seed Node's outbound degree, so it
	// ignores this field today (a defensive cap on the card's edge
	// scan is tracked as a follow-up — see doc.go). `ListNodes`
	// reads `ListNodesFilter.Limit` instead so per-call list shape
	// is configured next to the rest of the list filter.
	Limit int
}

// NodeRetirement is the tombstone metadata attached to a Node
// when `ReaderOptions.IncludeRetired = true` exposed a retired
// row. The shape mirrors the `node_retirement` schema from
// migration 0004.
type NodeRetirement struct {
	// RetiredAtSHA is the commit SHA at which this Node was
	// removed. The Repo Indexer delta pass sets this to
	// `parent(to_sha)` per architecture.md §4.6 step 2.
	RetiredAtSHA string
	// RetiredAt is the wall-clock time the tombstone landed.
	RetiredAt time.Time
	// SupersededByNodeID is the rename target, if any. The
	// renamed_to-edge path (Stage 2.3) sets this to point at
	// the new Node fingerprint. Empty when the Node was
	// removed without replacement (e.g. dead-code deletion).
	SupersededByNodeID string
}

// EdgeRetirement is the analogous tombstone metadata for Edge
// rows. Edges have no `superseded_by` because rename semantics
// are carried by the Node tombstone — an Edge that disappears
// at a new SHA is just gone.
type EdgeRetirement struct {
	RetiredAtSHA string
	RetiredAt    time.Time
}

// Node is the read-shape of one row from the `node` table. The
// fingerprint is exposed as a typed `fingerprint.Sum` so
// downstream code (rerank training, neighborhood resolution)
// can compare against writer output without re-decoding bytes.
type Node struct {
	NodeID             string
	RepoID             string
	Fingerprint        fingerprint.Sum
	Kind               string
	CanonicalSignature string
	ParentNodeID       string // empty when the Node has no parent (the repo Node)
	FromSHA            string
	AttrsJSON          json.RawMessage
	// Retirement is non-nil iff the row is retired AND the
	// caller passed `ReaderOptions.IncludeRetired = true`.
	Retirement *NodeRetirement
}

// Edge is the read-shape of one row from the `edge` table.
type Edge struct {
	EdgeID      string
	RepoID      string
	Fingerprint fingerprint.Sum
	Kind        string
	SrcNodeID   string
	DstNodeID   string
	FromSHA     string
	AttrsJSON   json.RawMessage
	// Retirement is non-nil iff the row is retired AND the
	// caller passed `ReaderOptions.IncludeRetired = true`.
	Retirement *EdgeRetirement
}

// GetNode fetches a single Node by id. Retired rows are hidden
// unless `opts.IncludeRetired = true`; in that case the returned
// `Node.Retirement` carries the tombstone metadata.
//
// Returns `ErrNotFound` when the row does not exist OR when the
// row is retired and IncludeRetired is false — see ErrNotFound
// docs. `opts.Limit` is ignored: GetNode is a single-row lookup.
func (r *Reader) GetNode(ctx context.Context, nodeID string, opts ReaderOptions) (Node, error) {
	if nodeID == "" {
		return Node{}, errors.New("graphreader: GetNode: empty node_id")
	}

	query := selectNodeQuery(opts.IncludeRetired)
	row := r.pool.QueryRow(ctx, query, nodeID)
	return scanNodeRow(row, opts.IncludeRetired)
}

// GetEdge fetches a single Edge by id. Same retirement semantics
// as GetNode: retired Edges are hidden by default and surfaced
// (with `EdgeRetirement` metadata) on opt-in. `opts.Limit` is
// ignored: GetEdge is a single-row lookup.
func (r *Reader) GetEdge(ctx context.Context, edgeID string, opts ReaderOptions) (Edge, error) {
	if edgeID == "" {
		return Edge{}, errors.New("graphreader: GetEdge: empty edge_id")
	}

	query := selectEdgeQuery(opts.IncludeRetired)
	row := r.pool.QueryRow(ctx, query, edgeID)
	return scanEdgeRow(row, opts.IncludeRetired)
}

// ListEdgesFrom returns every outbound Edge from the supplied
// `srcNodeID`. When `kinds` is non-empty the result is restricted
// to those edge kinds (e.g. `static_calls`, `observed_calls`);
// passing an empty slice means "all kinds".
//
// Retired Edges are filtered out unless
// `opts.IncludeRetired = true`. The result is ordered by
// `kind, edge_id` so successive calls with identical arguments
// return stable output (important for snapshot tests in
// downstream stages).
//
// `opts.Limit` bounds the row count — see ReaderOptions.Limit
// for the clamp policy. The query is always issued with a
// server-side `LIMIT $N` so even a caller that passes the
// zero-valued options struct cannot trip the OOM hazard a
// pathological repo (10k+ outbound edges from one Node) would
// otherwise pose on `agent.recall`.
func (r *Reader) ListEdgesFrom(
	ctx context.Context, srcNodeID string, kinds []string, opts ReaderOptions,
) ([]Edge, error) {
	if srcNodeID == "" {
		return nil, errors.New("graphreader: ListEdgesFrom: empty src_node_id")
	}
	if err := validateEdgeKinds(kinds); err != nil {
		return nil, err
	}

	query, args := selectEdgesFromQuery(srcNodeID, kinds, opts.IncludeRetired)
	query, args = appendLimit(query, args, opts.Limit)
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("graphreader: ListEdgesFrom query: %w", err)
	}
	defer rows.Close()
	return scanEdgeRows(rows, opts.IncludeRetired)
}

// ListEdgesTo is the inbound mirror of ListEdgesFrom: it
// returns every Edge whose `dst_node_id` matches `dstNodeID`.
// Used by `agent.expand(direction='callers')` to walk the
// call chain backwards from a target Node (architecture.md
// §6.1.3).
//
// Retirement, kind-filter, ordering, and limit semantics are
// IDENTICAL to ListEdgesFrom — see that function's doc for
// the full contract. The two methods share `selectEdgesToQuery`
// / `selectEdgesFromQuery` so any divergence shows up as a
// query.go change rather than a silent contract drift.
func (r *Reader) ListEdgesTo(
	ctx context.Context, dstNodeID string, kinds []string, opts ReaderOptions,
) ([]Edge, error) {
	if dstNodeID == "" {
		return nil, errors.New("graphreader: ListEdgesTo: empty dst_node_id")
	}
	if err := validateEdgeKinds(kinds); err != nil {
		return nil, err
	}

	query, args := selectEdgesToQuery(dstNodeID, kinds, opts.IncludeRetired)
	query, args = appendLimit(query, args, opts.Limit)
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("graphreader: ListEdgesTo query: %w", err)
	}
	defer rows.Close()
	return scanEdgeRows(rows, opts.IncludeRetired)
}

// ListNodesFilter is the structured filter shape ListNodes
// accepts. The zero value selects every Node in the repo (up to
// `MaxListLimit`). Each non-zero field adds an AND clause.
type ListNodesFilter struct {
	// ParentNodeID restricts the result to Nodes whose
	// `parent_node_id` is this id. Use the
	// repo→package→file→class→method→block hierarchy walk
	// (architecture.md §4.5) by chaining ListNodes calls with
	// this filter pointed at the prior level's NodeID.
	ParentNodeID string
	// FromSHA restricts the result to Nodes inserted at the
	// supplied SHA. Useful for snapshot-at-commit views in
	// `mgmt.read.graph_at`.
	FromSHA string
	// CanonicalSignature pins an exact-match lookup on
	// `canonical_signature`. Together with `Kind` this is the
	// natural-key resolution path callers use when they have a
	// Java method handle but no node_id.
	CanonicalSignature string
	// Limit caps the number of Nodes returned. The clamp policy
	// matches `ReaderOptions.Limit`:
	//
	//   * `Limit <= 0`             → `MaxListLimit` (default)
	//   * `0 < Limit <= MaxListLimit` → used as-is
	//   * `Limit > MaxListLimit`   → `MaxListLimit` (clamped)
	//
	// Per the reviewer note that drove this field, the cap
	// stops a repo with tens of thousands of Nodes from
	// loading every row into memory on every `agent.recall` /
	// `mgmt.read.*` call. Combine `Limit` with the stable
	// `ORDER BY (kind, canonical_signature, node_id)` (see
	// query.go) for deterministic batches; full cursor-based
	// pagination is deferred (see doc.go).
	Limit int
}

// ListNodes returns every Node in `repoID` matching `kinds`
// (empty slice = all kinds) and the per-field filters in `f`.
// Retired rows are hidden unless `opts.IncludeRetired = true`.
//
// Stable order: `kind, canonical_signature, node_id` so
// snapshot tests in downstream stages can assert on a
// deterministic sequence.
//
// `f.Limit` bounds the row count — see ListNodesFilter.Limit
// for the clamp policy. The query is always issued with a
// server-side `LIMIT $N` so even a caller that passes the
// zero-valued filter cannot trip the OOM hazard a large repo
// would otherwise pose on `agent.recall` / `mgmt.read.*`.
// `opts.Limit` is ignored on this method; configure list shape
// alongside the other list filters via `f.Limit`.
func (r *Reader) ListNodes(
	ctx context.Context,
	repoID fingerprint.RepoID,
	kinds []string,
	f ListNodesFilter,
	opts ReaderOptions,
) ([]Node, error) {
	if repoID.IsZero() {
		return nil, errors.New("graphreader: ListNodes: zero repo_id")
	}
	if err := validateNodeKinds(kinds); err != nil {
		return nil, err
	}

	query, args := selectNodesQuery(repoID, kinds, f, opts.IncludeRetired)
	query, args = appendLimit(query, args, f.Limit)
	rows, err := r.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("graphreader: ListNodes query: %w", err)
	}
	defer rows.Close()
	return scanNodeRows(rows, opts.IncludeRetired)
}

// appendLimit clamps the caller-requested limit to
// `MaxListLimit`, appends it as the next positional parameter,
// and tacks ` LIMIT $N` onto the SQL string. The space prefix
// is significant: the SQL strings produced by `query.go` end on
// the `ORDER BY` line so a bare `LIMIT` token would collide
// with the trailing whitespace/newline of the multi-line
// string literal. Returning the new query+args pair keeps the
// caller free of any "remember to also extend args" hazard.
func appendLimit(query string, args []any, requested int) (string, []any) {
	args = append(args, normaliseLimit(requested))
	query += fmt.Sprintf(" LIMIT $%d", len(args))
	return query, args
}

// rowScanner is the minimal interface common to pgx.Row and
// pgx.Rows so `scanNodeRow` / `scanEdgeRow` can serve both
// single-row and multi-row paths without duplication.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanNodeRow decodes a single Node-shaped row produced by the
// `selectNodeQuery` / `selectNodesQuery` family. When
// `includeRetired` is true the tail of the projection carries
// the (nullable) tombstone columns; we map them onto
// `Node.Retirement` only when `retired_at_sha` is non-null.
func scanNodeRow(row rowScanner, includeRetired bool) (Node, error) {
	var n Node
	var (
		fp           []byte
		parent       *string
		attrs        []byte
		retSHA       *string
		retAt        *time.Time
		supersededBy *string
	)
	dest := []any{
		&n.NodeID, &n.RepoID, &fp, &n.Kind,
		&n.CanonicalSignature, &parent, &n.FromSHA, &attrs,
	}
	if includeRetired {
		dest = append(dest, &retSHA, &retAt, &supersededBy)
	}
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Node{}, ErrNotFound
		}
		return Node{}, fmt.Errorf("graphreader: scan node: %w", err)
	}
	if len(fp) > 0 {
		sum, err := fingerprint.SumFromBytes(fp)
		if err != nil {
			return Node{}, fmt.Errorf("graphreader: decode node fingerprint: %w", err)
		}
		n.Fingerprint = sum
	}
	if parent != nil {
		n.ParentNodeID = *parent
	}
	n.AttrsJSON = append(json.RawMessage(nil), attrs...)
	if includeRetired && retSHA != nil {
		ret := &NodeRetirement{RetiredAtSHA: *retSHA}
		if retAt != nil {
			ret.RetiredAt = *retAt
		}
		if supersededBy != nil {
			ret.SupersededByNodeID = *supersededBy
		}
		n.Retirement = ret
	}
	return n, nil
}

// scanNodeRows iterates pgx.Rows applying scanNodeRow to each.
// Errors mid-iteration short-circuit; the rows handle is owned
// by the caller.
func scanNodeRows(rows pgx.Rows, includeRetired bool) ([]Node, error) {
	var out []Node
	for rows.Next() {
		n, err := scanNodeRow(rows, includeRetired)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphreader: nodes rows: %w", err)
	}
	return out, nil
}

// scanEdgeRow is the Edge analogue of scanNodeRow.
func scanEdgeRow(row rowScanner, includeRetired bool) (Edge, error) {
	var e Edge
	var (
		fp     []byte
		attrs  []byte
		retSHA *string
		retAt  *time.Time
	)
	dest := []any{
		&e.EdgeID, &e.RepoID, &fp, &e.Kind,
		&e.SrcNodeID, &e.DstNodeID, &e.FromSHA, &attrs,
	}
	if includeRetired {
		dest = append(dest, &retSHA, &retAt)
	}
	if err := row.Scan(dest...); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Edge{}, ErrNotFound
		}
		return Edge{}, fmt.Errorf("graphreader: scan edge: %w", err)
	}
	if len(fp) > 0 {
		sum, err := fingerprint.SumFromBytes(fp)
		if err != nil {
			return Edge{}, fmt.Errorf("graphreader: decode edge fingerprint: %w", err)
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
	return e, nil
}

func scanEdgeRows(rows pgx.Rows, includeRetired bool) ([]Edge, error) {
	var out []Edge
	for rows.Next() {
		e, err := scanEdgeRow(rows, includeRetired)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("graphreader: edges rows: %w", err)
	}
	return out, nil
}
