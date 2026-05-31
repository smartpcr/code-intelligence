package postgres

import (
	"context"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// Reader wraps any value satisfying `graphsink.Reader` -- in
// production this is `*graphreader.Reader` -- and re-exposes
// it as a `graphsink.Reader`. Every method is a pure 1:1
// forward to the wrapped backend; the adapter package contains
// NO direct SQL (tech-spec C5 / S4.5).
//
// `NewReader` deliberately accepts the `graphsink.Reader`
// interface rather than the concrete `*graphreader.Reader`
// type. Two reasons:
//
//  1. Testability: forwarding tests
//     (`reader_test.go`) construct the adapter over a fake
//     backend that records every call, asserting 1:1 delegation
//     without standing up a Postgres pool (impl-plan Stage 3.3
//     scenario `listrepos-forwards-to-graphreader`).
//  2. Symmetry with the `Sink` half of the adapter, which
//     declares its dependency in terms of a wider type
//     (`*graphwriter.Writer`) precisely because tests need a
//     `sqlmock`-driven substitute.
//
// `*graphreader.Reader` satisfies `graphsink.Reader` after the
// Stage 3.3 lift of `ListRepos` and `LookupBySignature` so the
// production wiring -- `postgres.NewReader(graphreader.New(pool, log))`
// -- compiles unchanged.
type Reader struct {
	backend graphsink.Reader
}

// NewReader wraps `backend` as a `graphsink.Reader`. Panics on
// a nil backend for the same reason `NewSink` panics on a nil
// writer: a nil dependency here is a programmer bug that would
// otherwise surface as a nil-pointer dereference on the first
// list call.
//
// Production callers pass `graphreader.New(pool, logger)`; the
// resulting `*graphreader.Reader` satisfies `graphsink.Reader`.
// Tests pass a fake that records the call args.
func NewReader(backend graphsink.Reader) *Reader {
	if backend == nil {
		panic("graphsink/postgres: NewReader: nil graphsink.Reader backend")
	}
	return &Reader{backend: backend}
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
