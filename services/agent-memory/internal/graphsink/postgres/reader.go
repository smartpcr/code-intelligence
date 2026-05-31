package postgres

import (
	"context"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// Reader wraps `*graphreader.Reader` and re-exposes it as a
// `graphsink.Reader`. Every method is a pure 1:1 forward to
// the wrapped reader; the adapter package contains NO direct
// SQL (tech-spec C5 / S4.5).
//
// The wrapped reader is borrowed, not owned: the caller
// constructs it once at process startup against a
// `*pgxpool.Pool` and is responsible for closing the pool when
// the process shuts down. This adapter has no `Close` method
// because `graphsink.Reader` does not declare one.
//
// `Reader.backend` is typed as the unexported `readerBackend`
// interface (NOT the concrete `*graphreader.Reader`) so the
// forwarding tests in `reader_internal_test.go` can substitute
// a fake that records every call. Per the workstream's
// detailed-requirements line and prior evaluator feedback, the
// PUBLIC constructor `NewReader` keeps the concrete
// `*graphreader.Reader` parameter type so the call site
// documented in the workstream brief
// (`postgres.NewReader(graphreader.New(pool, log))`) is the
// public contract. Test wiring uses the unexported
// `newReaderWithBackend` constructor, declared in the same
// package and visible only to white-box tests.
type Reader struct {
	backend readerBackend
}

// readerBackend is the unexported testable surface of the
// reader wrapper -- it mirrors `graphsink.Reader` exactly,
// which `*graphreader.Reader` satisfies. Keeping this interface
// unexported preserves the public constructor's
// `*graphreader.Reader` parameter while still letting the
// forwarding tests substitute a fake (test file uses
// `package postgres`, white-box).
type readerBackend interface {
	ListRepos(ctx context.Context, opts graphreader.ReaderOptions) ([]graphreader.RepoSummary, error)
	ListNodes(
		ctx context.Context,
		repoID fingerprint.RepoID,
		kinds []string,
		f graphreader.ListNodesFilter,
		opts graphreader.ReaderOptions,
	) ([]graphreader.Node, error)
	ListEdgesFrom(ctx context.Context, srcNodeID string, kinds []string, opts graphreader.ReaderOptions) ([]graphreader.Edge, error)
	ListEdgesTo(ctx context.Context, dstNodeID string, kinds []string, opts graphreader.ReaderOptions) ([]graphreader.Edge, error)
	GetNode(ctx context.Context, nodeID string, opts graphreader.ReaderOptions) (graphreader.Node, error)
	LookupBySignature(
		ctx context.Context,
		repoID fingerprint.RepoID,
		kind string,
		canonicalSignature string,
		opts graphreader.ReaderOptions,
	) (graphreader.Node, error)
}

// NewReader wraps the supplied `*graphreader.Reader` as a
// `graphsink.Reader`. Panics on a nil reader for the same
// reason `NewSink` panics on a nil writer: a nil dependency
// here is a programmer bug that would otherwise surface as a
// nil-pointer dereference on the first list call.
//
// Production wiring:
//
//	pgPool, _ := graphreader.NewPool(ctx, dsn, opts)
//	r := postgres.NewReader(graphreader.New(pgPool, logger))
func NewReader(reader *graphreader.Reader) *Reader {
	if reader == nil {
		panic("graphsink/postgres: NewReader: nil *graphreader.Reader")
	}
	return &Reader{backend: reader}
}

// newReaderWithBackend is the package-internal constructor the
// forwarding tests use to substitute a fake `readerBackend`.
// Kept unexported so external callers cannot bypass the
// `*graphreader.Reader` contract declared by `NewReader`.
func newReaderWithBackend(b readerBackend) *Reader {
	if b == nil {
		panic("graphsink/postgres: newReaderWithBackend: nil backend")
	}
	return &Reader{backend: b}
}

// Compile-time assertion that *Reader satisfies graphsink.Reader.
var _ graphsink.Reader = (*Reader)(nil)

// ListRepos forwards 1:1 to the wrapped backend's `ListRepos`.
// The lifted SELECT lives in `internal/graphreader.Reader` so
// this method stays a pure forwarder with no direct SQL.
func (r *Reader) ListRepos(ctx context.Context, opts graphreader.ReaderOptions) ([]graphreader.RepoSummary, error) {
	return r.backend.ListRepos(ctx, opts)
}

// ListNodes forwards 1:1 to the wrapped backend's `ListNodes`.
func (r *Reader) ListNodes(
	ctx context.Context,
	repoID fingerprint.RepoID,
	kinds []string,
	f graphreader.ListNodesFilter,
	opts graphreader.ReaderOptions,
) ([]graphreader.Node, error) {
	return r.backend.ListNodes(ctx, repoID, kinds, f, opts)
}

// ListEdgesFrom forwards 1:1 to the wrapped backend's
// `ListEdgesFrom`.
func (r *Reader) ListEdgesFrom(
	ctx context.Context,
	srcNodeID string,
	kinds []string,
	opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return r.backend.ListEdgesFrom(ctx, srcNodeID, kinds, opts)
}

// ListEdgesTo forwards 1:1 to the wrapped backend's
// `ListEdgesTo`.
func (r *Reader) ListEdgesTo(
	ctx context.Context,
	dstNodeID string,
	kinds []string,
	opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return r.backend.ListEdgesTo(ctx, dstNodeID, kinds, opts)
}

// GetNode forwards 1:1 to the wrapped backend's `GetNode`.
func (r *Reader) GetNode(
	ctx context.Context,
	nodeID string,
	opts graphreader.ReaderOptions,
) (graphreader.Node, error) {
	return r.backend.GetNode(ctx, nodeID, opts)
}

// LookupBySignature forwards 1:1 to the wrapped backend's
// `LookupBySignature`. In production the backend's
// implementation is `*graphreader.Reader.LookupBySignature`,
// which itself dispatches through `ListNodes` with
// `ListNodesFilter.CanonicalSignature` set (impl-plan Stage
// 3.3 scenario `lookupbysignature-uses-filter`).
func (r *Reader) LookupBySignature(
	ctx context.Context,
	repoID fingerprint.RepoID,
	kind string,
	canonicalSignature string,
	opts graphreader.ReaderOptions,
) (graphreader.Node, error) {
	return r.backend.LookupBySignature(ctx, repoID, kind, canonicalSignature, opts)
}
