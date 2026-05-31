// Package graphsink declares the storage abstraction the
// REPO-SCANNER pipeline writes through (Sink) and reads back
// from (Reader). Three backends satisfy these interfaces in
// later stages of phase-graphsink-storage-abstraction:
//
//   - Postgres adapter (Stage 3.2) wrapping the existing
//     `*graphwriter.Writer` + `*graphreader.Reader` so the
//     production deployment keeps the same code path with no
//     behavioural change (append-only, tombstones, retirements,
//     trace_observation aggregates -- all inherited from the
//     wrapped types).
//
//   - SQLite backend (Stage 3.4) for the `codeintel scan`
//     single-file-per-repo CLI default. Mirrors the Postgres
//     `node` / `edge` schema columns and reuses `pkg/fingerprint`
//     verbatim so a repo scanned to SQLite and later re-scanned
//     to Postgres yields IDENTICAL node identities (S3.4
//     backend-parity ID).
//
//   - In-memory + JSON export backend (Stage 3.5) for the
//     `--store=memory --export graph.json` one-shot path the
//     React UI consumes directly when the management API is
//     not in front of the user.
//
// This file declares the writer half (Sink). The reader half
// (Reader) lives next door in `reader.go`. Both interfaces are
// the ONLY surface the dispatcher (`internal/repoindexer/ast`)
// and the diagram projector (`internal/diagram`, Stage 4)
// depend on, so the choice of backend is a wiring concern at
// the CLI / serve entry points and never leaks into the
// pipeline.
package graphsink

import (
	"context"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// Sink is the storage abstraction every `ast.Dispatcher` /
// `repoindexer.AncestryWriter` writes through. It is a strict
// superset of `repoindexer.RepoCommitNodeEdgeWriter` (declared
// in `internal/repoindexer/ancestry_writer_iface.go`): the four
// repo / commit / node / edge methods carry IDENTICAL
// signatures so an existing `*graphwriter.Writer` consumer
// upgraded to take a `graphsink.Sink` compiles unchanged, and
// any value satisfying `Sink` trivially satisfies the narrower
// `RepoCommitNodeEdgeWriter` shape used by `AncestryWriter`
// today.
//
// Sink ADDS exactly two methods on top of that base:
//
//   - Flush(ctx) lets buffered backends (the in-memory backend
//     batches inserts and emits the JSON export on Flush; the
//     SQLite backend commits the active transaction) finalise
//     pending work without closing the underlying store. The
//     CLI calls Flush at end-of-scan; the long-running worker
//     (`cmd/repoindexer`) calls it after each `ingest_jobs`
//     iteration so a process crash does not lose the row it
//     just wrote.
//
//   - Close() releases the backend's underlying resources (the
//     SQLite db handle, the in-memory backend's exported file
//     handle, the Postgres adapter's pool reference if it owns
//     it). It is idempotent: backends MUST tolerate a double
//     Close and return nil on the second call.
//
// The `*graphwriter.Writer` shipped today does NOT have Flush /
// Close methods -- it is a stateless wrapper around a
// caller-owned `*pgxpool.Pool` and commits each write inline.
// The Postgres adapter Stage 3.2 ships will wrap the Writer
// with no-op Flush / Close so the type-level superset relation
// holds at the adapter boundary, not on the concrete Writer
// itself.
//
// IMPORT-CYCLE NOTE: the four base method signatures are
// restated here rather than imported by embedding
// `repoindexer.RepoCommitNodeEdgeWriter` because Stage 3.3
// widens `repoindexer.NewAncestryWriter` to take a
// `graphsink.Sink` -- which would create a cycle
// (`graphsink` -> `repoindexer` -> `graphsink`) if Sink
// embedded the repoindexer-side interface. The shapes are kept
// byte-identical to the matching `*graphwriter.Writer` methods
// so the same `RepoInput` / `CommitInput` / `NodeInput` /
// `EdgeInput` payloads flow through every backend without
// translation, and so the fingerprint / role-grant invariants
// the Writer enforces are preserved when the worker / CLI talk
// to a non-Postgres backend.
type Sink interface {
	// EnsureRepo upserts the repo row keyed by URL and returns
	// the resulting RepoRecord. Identical semantics to
	// `*graphwriter.Writer.EnsureRepo`.
	EnsureRepo(ctx context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error)

	// EnsureCommit upserts the commit row keyed by
	// (RepoID, SHA) and returns the resulting CommitRecord.
	// Identical semantics to
	// `*graphwriter.Writer.EnsureCommit`.
	EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error)

	// InsertNode inserts a Node row keyed by the
	// fingerprint-derived NodeID and returns the resulting
	// NodeRecord. Identical semantics to
	// `*graphwriter.Writer.InsertNode` (append-only;
	// idempotent on duplicate fingerprint).
	InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error)

	// InsertEdge inserts an Edge row keyed by the
	// fingerprint-derived EdgeID and returns the resulting
	// EdgeRecord. Identical semantics to
	// `*graphwriter.Writer.InsertEdge` (append-only;
	// idempotent on duplicate fingerprint).
	InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error)

	// Flush finalises any buffered writes the backend may be
	// holding. Safe to call multiple times; backends with no
	// buffering (the Postgres adapter) return nil. Returns the
	// first error encountered persisting buffered work.
	Flush(ctx context.Context) error

	// Close releases backend-owned resources. Idempotent: the
	// second and subsequent calls MUST return nil. After Close
	// returns, calls to any other method on the Sink yield an
	// error -- backends choose the sentinel (typically a
	// wrapped `sql.ErrConnDone` for SQLite, `pgx`/`pgxpool`
	// closed-pool error for Postgres, package-level sentinel
	// for the in-memory backend).
	Close() error
}
