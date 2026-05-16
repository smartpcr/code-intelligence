// Package graphreader is the read-only counterpart of
// `internal/graphwriter` (Stage 2.1). It implements the queries
// agent-memory's public surfaces use to resolve Node, Edge,
// neighborhood-card, and call-chain expansions against the
// structural graph tables.
//
// Stage 2.2 contract (implementation-plan.md §"Stage 2.2:
// GraphReader library"):
//
//   - `GetNode(ctx, node_id, opts)`,
//     `GetEdge(ctx, edge_id, opts)`,
//     `ListEdgesFrom(ctx, node_id, kinds...)`, and
//     `ListNodes(ctx, repo_id, kinds, filters...)` are the four
//     public read entry points. Every one of them wraps its
//     SELECT in a `WHERE NOT EXISTS (SELECT 1 FROM
//     node_retirement WHERE node_id = ...)` (and the analogous
//     edge form) anti-join so retired rows are invisible by
//     default. This is the G5 / G6 enforcement at the read
//     layer; the role-grants in 0017 prevent the reader from
//     mutating the tombstone tables themselves.
//
//   - `ReaderOptions.IncludeRetired bool` lets historical
//     Episode replay (Stage 2.4 `Resolve(context_id)` per
//     implementation-plan.md / risk §9.13) opt in to retired
//     rows. When `IncludeRetired = true`, the anti-join is
//     dropped AND each returned row carries the matching
//     retirement metadata (`retired_at_sha`, `retired_at`,
//     `superseded_by_node_id` where applicable) so the caller
//     can render "this Node was retired at SHA X" in
//     `mgmt.read.context`. Required by the brief's Test
//     Scenario "retired node visible with opt-in".
//
//   - `NeighborhoodCard(ctx, node_id, opts)` resolves a Node +
//     its outbound edges + the per-edge `TraceObservation`
//     aggregate (architecture.md §4.5). This is the building
//     block `agent.expand` and `mgmt.read.graph_node` call to
//     materialise a single hop with hot-path counters.
//
//   - `NewPool(ctx, dsn, opts)` constructs the `pgxpool.Pool`
//     the Reader runs on, authenticated as the `agent_memory_ro`
//     role created in migration 0017. The default `MaxConns`
//     is chosen to satisfy the tech-spec §8.3 RPS envelope
//     (50 RPS sustained / 200 RPS burst across `agent.recall`,
//     `agent.expand`, `agent.summarize`, and `mgmt.read.*`).
//
// Concurrency
// -----------
// The Reader is safe for concurrent use: it holds only a
// `*pgxpool.Pool` and the embedded `*slog.Logger`, both of
// which are concurrency-safe themselves. Every public method
// is stateless beyond the pool acquisition.
//
// Endpoint-retirement assumption
// ------------------------------
// The default-view (`IncludeRetired=false`) edge queries filter
// rows whose own `edge_id` has a tombstone row in
// `edge_retirement`. They do NOT separately verify that
// `src_node_id` / `dst_node_id` are still current. The reader
// trusts a cross-stage invariant maintained by the Tombstone
// Retirement Service (Stage 2.3): whenever a Node is retired,
// every incident Edge MUST be retired in the same write batch.
// As long as that invariant holds, an Edge that survived the
// default anti-join cannot point at a retired endpoint.
//
// If a future stage relaxes the writer's invariant — or if
// callers need to defend against a writer bug — the reader
// can grow an `endpoint-anti-join` mode by composing two more
// `NOT EXISTS (SELECT 1 FROM node_retirement WHERE node_id = e.{src,dst}_node_id)`
// clauses. We intentionally do not pay that cost today: it
// would double the planner work on the hot path for every
// read, even when the writer's invariant is healthy.
//
// Snapshot consistency
// --------------------
// Single-query methods (`GetNode`, `GetEdge`, `ListEdgesFrom`,
// `ListNodes`) implicitly run inside one MVCC snapshot — the
// snapshot pgx attaches to that statement. Methods that
// compose multiple queries (`NeighborhoodCard`) open an
// explicit `REPEATABLE READ`, read-only transaction so the
// composite result reflects a single coherent view of the
// graph; a concurrent retirement that lands between the two
// internal queries cannot produce a "current" seed Node
// alongside post-retirement edges.
//
// Error model
// -----------
// The library exposes a single sentinel `ErrNotFound`. Callers
// pattern-match on it with `errors.Is`:
//
//	node, err := reader.GetNode(ctx, id, graphreader.ReaderOptions{})
//	if errors.Is(err, graphreader.ErrNotFound) {
//	    // either the row never existed OR it's retired
//	    // and the caller did not opt in
//	}
//
// Any other error (driver-level network failure, malformed
// row, etc.) passes through unwrapped so callers see the
// underlying pgx context. Reads issue no DML, so there is no
// equivalent of `graphwriter.WriteContractViolation`.
package graphreader
