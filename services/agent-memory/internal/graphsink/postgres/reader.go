package postgres

import (
	"context"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// Reader wraps `*graphreader.Reader` and exposes it as a
// `graphsink.Reader`. Every method is a pure 1:1 forward to the
// typed reader API on the underlying `*graphreader.Reader`; the
// adapter package contains NO direct SQL (tech-spec C5 / S4.5).
//
// The wrapped reader is borrowed, not owned: the caller
// constructs it once at process startup against a
// `*pgxpool.Pool` and is responsible for closing the pool when
// the process shuts down. This adapter has no `Close` method
// because `graphsink.Reader` does not declare one -- reads do
// not have a flush/close lifecycle and the underlying pool's
// shutdown is the caller's concern.
type Reader struct {
	reader *graphreader.Reader
}

// NewReader wraps the supplied `*graphreader.Reader` as a
// `graphsink.Reader`. Panics on a nil reader for the same
// reason `NewSink` panics on a nil writer: a nil dependency
// here is a programmer bug that would otherwise surface as a
// nil-pointer dereference on the first list call.
func NewReader(reader *graphreader.Reader) *Reader {
	if reader == nil {
		panic("graphsink/postgres: NewReader: nil *graphreader.Reader")
	}
	return &Reader{reader: reader}
}

// Compile-time assertion that *Reader satisfies graphsink.Reader.
var _ graphsink.Reader = (*Reader)(nil)

// ListRepos forwards 1:1 to `*graphreader.Reader.ListRepos`.
// The lifted SELECT lives in `internal/graphreader.Reader` so
// this method stays a pure forwarder with no direct SQL.
func (r *Reader) ListRepos(ctx context.Context, opts graphreader.ReaderOptions) ([]graphreader.RepoSummary, error) {
	return r.reader.ListRepos(ctx, opts)
}

// ListNodes forwards 1:1 to `*graphreader.Reader.ListNodes`.
func (r *Reader) ListNodes(
	ctx context.Context,
	repoID fingerprint.RepoID,
	kinds []string,
	f graphreader.ListNodesFilter,
	opts graphreader.ReaderOptions,
) ([]graphreader.Node, error) {
	return r.reader.ListNodes(ctx, repoID, kinds, f, opts)
}

// ListEdgesFrom forwards 1:1 to
// `*graphreader.Reader.ListEdgesFrom`.
func (r *Reader) ListEdgesFrom(
	ctx context.Context,
	srcNodeID string,
	kinds []string,
	opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return r.reader.ListEdgesFrom(ctx, srcNodeID, kinds, opts)
}

// ListEdgesTo forwards 1:1 to
// `*graphreader.Reader.ListEdgesTo`.
func (r *Reader) ListEdgesTo(
	ctx context.Context,
	dstNodeID string,
	kinds []string,
	opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return r.reader.ListEdgesTo(ctx, dstNodeID, kinds, opts)
}

// GetNode forwards 1:1 to `*graphreader.Reader.GetNode`.
func (r *Reader) GetNode(
	ctx context.Context,
	nodeID string,
	opts graphreader.ReaderOptions,
) (graphreader.Node, error) {
	return r.reader.GetNode(ctx, nodeID, opts)
}

// LookupBySignature forwards 1:1 to
// `*graphreader.Reader.LookupBySignature`.
func (r *Reader) LookupBySignature(
	ctx context.Context,
	repoID fingerprint.RepoID,
	kind string,
	canonicalSignature string,
	opts graphreader.ReaderOptions,
) (graphreader.Node, error) {
	return r.reader.LookupBySignature(ctx, repoID, kind, canonicalSignature, opts)
}
