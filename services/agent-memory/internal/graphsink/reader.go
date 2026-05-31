package graphsink

import (
	"context"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// Reader is the read-only access path the diagram projector
// (`internal/diagram`, Stage 4) and the `codeintel serve`
// HTTP handlers (Stage 6) consume. It is the read-side mirror
// of `Sink`: three backends will satisfy it -- a Postgres
// adapter wrapping `*graphreader.Reader` (Stage 3.2), a SQLite
// reader (Stage 3.4), and an in-memory / JSON-snapshot reader
// (Stage 3.5) -- and the choice of backend is a wiring
// concern at the entry point.
//
// All shapes reuse types declared in `internal/graphreader`
// (`Node`, `Edge`, `ListNodesFilter`, `ReaderOptions`,
// `RepoSummary`) so a Postgres-backed reader can forward
// straight through with no translation and so the JSON
// envelope the React UI consumes (Stage 7.2) is identical
// regardless of which backend served the scan. The single
// source of truth for `RepoSummary` is
// `internal/graphreader/types.go`.
//
// METHOD SCOPE -- the six methods here are the union of the
// surfaces the diagram projector and the multi-repo overview
// page need:
//
//   - ListRepos backs the repo picker on the React UI and the
//     `GET /v1/repos` HTTP endpoint. Stage 3.3 lifts the SELECT
//     currently inlined at `internal/mgmtapi/read.go:803`
//     (`handleListRepos`) into `internal/graphreader.Reader`
//     itself so the Postgres adapter can forward to that
//     primitive WITHOUT issuing direct SQL (tech-spec C5 / S4.5
//     forbid backends bypassing the typed reader API).
//
//   - ListNodes / ListEdgesFrom / ListEdgesTo / GetNode are the
//     containment-tree walk + outbound/inbound edge fan
//     primitives the module-diagram (top-down) and call-chain
//     (left-right) builders use.
//
//   - LookupBySignature resolves a `--seed <canonical_signature>`
//     CLI argument to a concrete Node before the call-chain BFS
//     starts. It is also the lookup the dispatcher uses when
//     the user passes a human-readable seed in the URL of the
//     `GET /api/diagram/calls?seed=...` endpoint.
type Reader interface {
	// ListRepos returns the per-repo overview rows the repo
	// picker / `GET /v1/repos` response materialises. Returns
	// `[]graphreader.RepoSummary` directly -- the single source
	// of truth for the wire shape -- so all three backends
	// agree on the envelope Stage 7.2 marshals. Honours
	// `opts.Limit` and `opts.IncludeRetired` per the standard
	// `ReaderOptions` contract.
	ListRepos(ctx context.Context, opts graphreader.ReaderOptions) ([]graphreader.RepoSummary, error)

	// ListNodes returns every Node in `repoID` matching `kinds`
	// (empty = all kinds) and the per-field filters in `f`.
	// Identical semantics to `*graphreader.Reader.ListNodes`
	// (stable `kind, canonical_signature, node_id` ordering;
	// retirement filter honoured via `opts.IncludeRetired`;
	// row count clamped by `f.Limit` per `MaxListLimit`).
	ListNodes(
		ctx context.Context,
		repoID fingerprint.RepoID,
		kinds []string,
		f graphreader.ListNodesFilter,
		opts graphreader.ReaderOptions,
	) ([]graphreader.Node, error)

	// ListEdgesFrom returns every outbound Edge from
	// `srcNodeID` matching `kinds` (empty = all kinds).
	// Identical semantics to
	// `*graphreader.Reader.ListEdgesFrom`. Used by the
	// call-chain BFS in the callees direction and by the
	// module-diagram import roll-up.
	ListEdgesFrom(
		ctx context.Context,
		srcNodeID string,
		kinds []string,
		opts graphreader.ReaderOptions,
	) ([]graphreader.Edge, error)

	// ListEdgesTo returns every inbound Edge to `dstNodeID`
	// matching `kinds`. Identical semantics to
	// `*graphreader.Reader.ListEdgesTo`. Used by the call-chain
	// BFS in the callers direction.
	ListEdgesTo(
		ctx context.Context,
		dstNodeID string,
		kinds []string,
		opts graphreader.ReaderOptions,
	) ([]graphreader.Edge, error)

	// GetNode fetches a single Node by id. Identical semantics
	// to `*graphreader.Reader.GetNode`: returns
	// `graphreader.ErrNotFound` for missing or hidden-by-default
	// retired rows. `opts.Limit` is ignored (single-row lookup).
	GetNode(
		ctx context.Context,
		nodeID string,
		opts graphreader.ReaderOptions,
	) (graphreader.Node, error)

	// LookupBySignature resolves a (repoID, kind,
	// canonicalSignature) triple to its current Node, the
	// human-friendly counterpart to `GetNode`. Returns
	// `graphreader.ErrNotFound` when no matching Node exists
	// (or the matching Node is retired and
	// `opts.IncludeRetired = false`). Used by `codeintel
	// diagram calls --seed <signature>` and by the
	// `GET /api/diagram/calls?seed=<signature>` HTTP endpoint
	// to translate a user-supplied seed into the concrete
	// `nodeID` the BFS expects.
	LookupBySignature(
		ctx context.Context,
		repoID fingerprint.RepoID,
		kind string,
		canonicalSignature string,
		opts graphreader.ReaderOptions,
	) (graphreader.Node, error)
}
