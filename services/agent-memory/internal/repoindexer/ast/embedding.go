package ast

import "context"

// NodeEmbeddingPublisher is the small consumer-side interface
// the Stage 3.2 dispatcher uses to fan Method/Block node
// emissions out to the Stage 3.3 EmbeddingIndex writer
// (`internal/embedding.Publisher`) without importing the
// embedding package directly.  Keeping the interface here
// (where it's consumed) lets ast unit tests inject a recording
// fake while production wiring satisfies it through a small
// adapter in `internal/embedding/astadapter.go`.
//
// Implementations MUST honour the supplied `ctx` for both
// cancellation and timeout.  Returning a non-nil error is
// classified by the dispatcher into two buckets:
//
//   - `errors.Is(err, ErrPublishRecordedFailed)` — the publish
//     attempt's failure was DURABLY recorded by the
//     EmbeddingIndex writer (i.e. an `embedding_publish_event`
//     of kind `failed` exists).  The dispatcher logs and
//     continues; a background flusher will retry.
//
//   - any other error — the writer could NOT record durable
//     state.  The dispatcher returns the error so the ingest
//     job fails (a silent swallow here would leave the
//     EmbeddingIndex permanently divergent from the structural
//     graph for the affected nodes).
//
// The two-bucket discipline matches tech-spec §9.6a "Qdrant
// outages surface through C22 as `embedding_index_unavailable`;
// the writer queues the publish by leaving the latest event at
// `queued` or `failed` and a background flusher retries".
type NodeEmbeddingPublisher interface {
	PublishNodeEmbedding(ctx context.Context, req NodeEmbedRequest) (NodeEmbedResult, error)
}

// NodeEmbedRequest is the primitive-typed request shape the
// dispatcher hands to a `NodeEmbeddingPublisher`.  Using
// primitives (no ast-internal types) keeps the interface
// implementable from outside the ast package without a
// circular dependency.
type NodeEmbedRequest struct {
	// NodeID is the textual UUID of the Method / Block Node
	// the dispatcher just inserted (and whose parent→child
	// `contains` edge it has also just committed).  The
	// EmbeddingIndex writer uses it as the
	// `embedding_publish.node_id` foreign key target.
	NodeID string
	// RepoID is the textual UUID of the owning Repo, copied
	// through onto the Qdrant payload so recall filter
	// pushdown by repo_id works as expected.
	RepoID string
	// Kind discriminates the publish surface.  Closed set
	// {"method", "block"}.  The publisher rejects anything
	// else; the ast package's two emit passes only ever
	// supply these two values, so a violation here is a
	// hard wiring bug.
	Kind string
	// CanonicalSignature is the architecture-owned
	// `Node.canonical_signature` value, persisted as a
	// payload field by the EmbeddingIndex writer for
	// diagnostic reverse-lookup.
	CanonicalSignature string
	// Content is the source text to embed.  For a Method
	// this is the parser's `BodySource`; for a Block this is
	// the file source sliced at `[Block.StartByte:EndByte]`.
	// For a bodyless Method declaration (TS interface
	// member, abstract method) the dispatcher falls back to
	// the canonical signature so the publish hook still
	// fires for every emitted Method node — see
	// `SignatureOnly` below.
	Content string
	// SignatureOnly is true when `Content` is the canonical
	// signature instead of the body text (because the
	// emitted Method node has no body — TS interface members,
	// abstract methods).  Carried through onto the Qdrant
	// payload so a future recall reader can distinguish
	// "embedded body" from "embedded signature" without
	// re-querying PostgreSQL.  False for every Block publish
	// and for every Method publish whose body was non-empty.
	SignatureOnly bool
}

// NodeEmbedResult exposes the durable identifiers the
// EmbeddingIndex writer minted for this attempt.  The
// dispatcher currently uses only `LastEventKind` for its
// continue-vs-abort decision, but logging both publish_id and
// point_id keeps the operator audit trail aligned with the
// `embedding_publish` table.
type NodeEmbedResult struct {
	PublishID     string
	QdrantPointID string
	LastEventKind string
}

// ErrPublishRecordedFailed is the sentinel a
// `NodeEmbeddingPublisher` returns (wrapped) when the publish
// attempt failed AND the failure was durably recorded as a
// `failed` event row.  The dispatcher tests `errors.Is(err,
// ast.ErrPublishRecordedFailed)` to decide whether to swallow
// the failure (recorded → safe to continue) or propagate it
// (not recorded → abort).
//
// The corresponding error in `internal/embedding` is
// `embedding.ErrAttemptFailed`; the adapter in
// `internal/embedding/astadapter.go` rewrites one onto the
// other so the ast package never imports embedding.
var ErrPublishRecordedFailed = errPublishRecordedFailed{}

type errPublishRecordedFailed struct{}

func (errPublishRecordedFailed) Error() string {
	return "ast: embedding publish attempt failed (recorded as 'failed')"
}

// noopNodeEmbeddingPublisher is the dispatcher's default
// publisher.  Returns zero-value results and no error so the
// existing Stage 3.2 tests (which do not exercise publish
// state) keep passing unchanged after the publisher hook
// lands.
type noopNodeEmbeddingPublisher struct{}

func (noopNodeEmbeddingPublisher) PublishNodeEmbedding(_ context.Context, _ NodeEmbedRequest) (NodeEmbedResult, error) {
	return NodeEmbedResult{}, nil
}
