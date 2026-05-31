// Package postgres is the Postgres-backed implementation of the
// `graphsink.Sink` and `graphsink.Reader` interfaces. It is a
// pure adapter: every method forwards 1:1 to the existing
// `*graphwriter.Writer` (Sink side) or `*graphreader.Reader`
// (Reader side) without issuing any direct SQL of its own.
//
// This adapter is the production wiring for the REPO-SCANNER
// pipeline. It exists so the same dispatcher / diagram
// projector code path the SQLite and in-memory backends use
// (Stages 3.4 and 3.5) targets Postgres in production without
// any behavioural drift -- append-only writes, tombstone
// retirement, trace_observation aggregates, role-grant
// boundaries, ListNodes/Edges retirement filtering,
// MaxListLimit clamping -- all inherited unchanged from the
// wrapped types.
//
// TECH-SPEC C5 / S4.5 INVARIANT: this package MUST NOT contain
// direct SQL. Every read path must route through a typed method
// on `*graphreader.Reader`; every write path must route through
// a typed method on `*graphwriter.Writer`. The
// `internal/graphreader.Reader.ListRepos` lift performed in
// Stage 3.3 exists precisely so the `ListRepos` method on this
// adapter can satisfy the invariant -- without it the adapter
// would either have to embed `*pgxpool.Pool` (creating a
// second, parallel SQL path) or import `internal/mgmtapi`
// (a layering inversion).
package postgres

import (
	"context"
	"errors"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// Sink wraps `*graphwriter.Writer` and exposes it as a
// `graphsink.Sink`. The wrapped writer is owned by the caller
// (typically constructed once at process startup against a
// `*sql.DB` pool) so this adapter does not close it on
// `Sink.Close`.
//
// Construct one via `NewSink(writer)`. The constructor panics
// on a nil writer because a nil dependency is unambiguously a
// programmer bug that would otherwise surface as a nil-pointer
// dereference on the first scan write.
type Sink struct {
	writer *graphwriter.Writer
}

// NewSink wraps the supplied `*graphwriter.Writer` as a
// `graphsink.Sink`. The writer is borrowed, not owned: this
// adapter's `Close` does NOT close the wrapped writer (the
// writer is stateless atop a caller-owned `*sql.DB` pool).
func NewSink(writer *graphwriter.Writer) *Sink {
	if writer == nil {
		panic("graphsink/postgres: NewSink: nil *graphwriter.Writer")
	}
	return &Sink{writer: writer}
}

// Compile-time assertion that *Sink satisfies graphsink.Sink.
var _ graphsink.Sink = (*Sink)(nil)

// EnsureRepo forwards to the wrapped writer. When `in.RepoID`
// is non-zero the call routes through
// `*graphwriter.Writer.EnsureRepoWithID` -- the precomputed-PK
// variant -- so the scanner CLI's `fingerprint.RepoIDFromURL`
// derivation lands on the row's `repo_id` PK and the in-memory
// / SQLite backends agree on node identity for the same repo
// (architecture S3.4 backend-parity ID). When `in.RepoID` is
// zero the call routes through `*graphwriter.Writer.EnsureRepo`
// and the schema's `gen_random_uuid()` default picks the PK --
// the original Stage 2.1 behaviour, preserved for existing
// (non-scanner) call sites that have not adopted the
// precomputed PK.
func (s *Sink) EnsureRepo(ctx context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	if in.RepoID.IsZero() {
		return s.writer.EnsureRepo(ctx, in)
	}
	return s.writer.EnsureRepoWithID(ctx, in)
}

// EnsureCommit forwards 1:1 to
// `*graphwriter.Writer.EnsureCommit`.
func (s *Sink) EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	return s.writer.EnsureCommit(ctx, in)
}

// InsertNode forwards 1:1 to
// `*graphwriter.Writer.InsertNode`.
func (s *Sink) InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	return s.writer.InsertNode(ctx, in)
}

// InsertEdge forwards 1:1 to
// `*graphwriter.Writer.InsertEdge`.
func (s *Sink) InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	return s.writer.InsertEdge(ctx, in)
}

// Flush is a no-op for the Postgres adapter: the wrapped
// `*graphwriter.Writer` commits each write inline (one
// `runInTx`-bracketed transaction per row), so there are no
// buffered writes to drain. Returning nil keeps the Sink
// contract uniform across backends -- callers can `Flush` after
// every iteration of an ingest job without a backend-specific
// branch.
func (s *Sink) Flush(_ context.Context) error {
	return nil
}

// errClosed is the sentinel reserved for post-Close calls on a
// future evolution of this adapter that owns its underlying
// resources. Today the Postgres adapter borrows the writer from
// its caller so Close is a no-op; the sentinel is exported in
// shape via `errors.Is` for callers that already pattern-match
// for it on other backends.
var errClosed = errors.New("graphsink/postgres: Sink closed")

// Close is a no-op for the Postgres adapter: the wrapped
// writer is borrowed (the caller owns the underlying
// `*sql.DB` pool and is responsible for closing it). Close is
// idempotent -- the second and subsequent calls return nil per
// the Sink contract.
//
// We do not flip an internal `closed` flag because the wrapped
// writer is stateless and there is no resource for this layer
// to release. A future evolution that takes ownership of the
// pool (e.g. an embedded SQLite implementation that internally
// instantiates a writer) would gate subsequent method calls on
// the flag and return `errClosed`.
func (s *Sink) Close() error {
	_ = errClosed // reserved for the future owned-pool evolution
	return nil
}
