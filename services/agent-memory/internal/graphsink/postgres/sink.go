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
	"sync"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// Sink wraps `*graphwriter.Writer` and exposes it as a
// `graphsink.Sink`. The wrapped writer is owned by the caller
// (typically constructed once at process startup against a
// `*sql.DB` pool) so this adapter does not close the underlying
// pool on `Sink.Close`; it only flips its own `closed` gate so
// the post-Close lifecycle contract (every other method must
// return an error -- see `graphsink/sink.go:121`) holds.
//
// Construct one via `NewSink(writer)`. The constructor panics
// on a nil writer because a nil dependency is unambiguously a
// programmer bug that would otherwise surface as a nil-pointer
// dereference on the first scan write.
type Sink struct {
	writer *graphwriter.Writer

	// mu guards `closed`. Sink callers in the production wiring
	// are sequential per-scan but the contract permits
	// concurrent `Flush` / `Close`, so a mutex is the cheapest
	// safe gate.
	mu     sync.Mutex
	closed bool
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
	if err := s.checkOpen(); err != nil {
		return graphwriter.RepoRecord{}, err
	}
	if in.RepoID.IsZero() {
		return s.writer.EnsureRepo(ctx, in)
	}
	return s.writer.EnsureRepoWithID(ctx, in)
}

// EnsureCommit forwards 1:1 to
// `*graphwriter.Writer.EnsureCommit`.
func (s *Sink) EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	if err := s.checkOpen(); err != nil {
		return graphwriter.CommitRecord{}, err
	}
	return s.writer.EnsureCommit(ctx, in)
}

// InsertNode forwards 1:1 to
// `*graphwriter.Writer.InsertNode`. Errors from the wrapped
// writer -- including the typed
// `*graphwriter.WriteContractViolation` returned for SQLSTATE
// 42501 (insufficient privilege) -- propagate verbatim so
// callers can `errors.As(err, &wcv)` without any unwrapping by
// this adapter layer.
func (s *Sink) InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	if err := s.checkOpen(); err != nil {
		return graphwriter.NodeRecord{}, err
	}
	return s.writer.InsertNode(ctx, in)
}

// InsertEdge forwards 1:1 to
// `*graphwriter.Writer.InsertEdge`. Same verbatim-propagation
// contract for `*graphwriter.WriteContractViolation` as
// `InsertNode`.
func (s *Sink) InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	if err := s.checkOpen(); err != nil {
		return graphwriter.EdgeRecord{}, err
	}
	return s.writer.InsertEdge(ctx, in)
}

// Flush is a no-op for the Postgres adapter: the wrapped
// `*graphwriter.Writer` commits each write inline (one
// `runInTx`-bracketed transaction per row), so there are no
// buffered writes to drain. Returning nil keeps the Sink
// contract uniform across backends -- callers can `Flush` after
// every iteration of an ingest job without a backend-specific
// branch.
//
// Flush after Close returns `ErrSinkClosed` per the
// `graphsink.Sink` lifecycle contract (`internal/graphsink/sink.go`:
// "After Close returns, calls to any other method on the Sink
// yield an error").
func (s *Sink) Flush(_ context.Context) error {
	return s.checkOpen()
}

// ErrSinkClosed is returned by every non-Close method on a
// Sink whose `Close` has already returned. It is the
// adapter-side sentinel the
// `graphsink.Sink` interface documentation refers to
// ("backends choose the sentinel ..."). Pattern-match with
// `errors.Is(err, postgres.ErrSinkClosed)`.
var ErrSinkClosed = errors.New("graphsink/postgres: Sink closed")

// checkOpen returns ErrSinkClosed if `Close` has already been
// called. The mutex is held only for the read of `closed` so
// production write paths -- which never call Close concurrently
// with InsertNode/InsertEdge -- pay only one uncontended lock
// per call.
func (s *Sink) checkOpen() error {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return ErrSinkClosed
	}
	return nil
}

// Close marks the Sink closed. The wrapped writer is borrowed
// (the caller owns the underlying `*sql.DB` pool and is
// responsible for closing it) so this method does NOT close
// the pool. It flips the adapter's `closed` gate so subsequent
// calls to any other method return `ErrSinkClosed`, satisfying
// the lifecycle contract in `internal/graphsink/sink.go` which
// requires "calls to any other method on the Sink yield an
// error" after `Close` returns.
//
// Idempotent: the second and subsequent calls return nil per
// the Sink contract.
func (s *Sink) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
