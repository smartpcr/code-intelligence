package embedding

import (
	"context"
	"errors"
	"fmt"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// AsASTPublisher returns a `*Publisher` wrapped in a thin
// adapter that satisfies `ast.NodeEmbeddingPublisher`.  This
// file is the ONE place inside `internal/embedding` that
// imports `internal/repoindexer/ast`; the rest of the package
// is kept free of the ast dependency so future packages (the
// Stage 4.1 delta handler, the Stage 6.2 Concept Promoter)
// can consume the publisher without dragging ast types
// through their own surface.
//
// The adapter performs three responsibilities:
//
//   - Type translation between `ast.NodeEmbedRequest` and
//     `embedding.PublishRequest` (same fields, separate types
//     because the ast package cannot import embedding).
//   - Error translation: an `embedding.ErrAttemptFailed`-
//     wrapped failure is rewritten to wrap
//     `ast.ErrPublishRecordedFailed` so the dispatcher can
//     `errors.Is(err, ast.ErrPublishRecordedFailed)` without
//     knowing about embedding's sentinels.
//   - Result translation, copying the durable
//     publish_id / point_id / last_event_kind back to the ast
//     caller.
func AsASTPublisher(p *Publisher) ast.NodeEmbeddingPublisher {
	if p == nil {
		panic("embedding: AsASTPublisher: nil *Publisher")
	}
	return astPublisherAdapter{p: p}
}

type astPublisherAdapter struct{ p *Publisher }

// PublishNodeEmbedding satisfies `ast.NodeEmbeddingPublisher`
// by delegating to `Publisher.Publish` with one-to-one field
// mapping.  See file header for the error-translation rule.
func (a astPublisherAdapter) PublishNodeEmbedding(
	ctx context.Context,
	req ast.NodeEmbedRequest,
) (ast.NodeEmbedResult, error) {
	res, err := a.p.Publish(ctx, PublishRequest{
		NodeID:             req.NodeID,
		RepoID:             req.RepoID,
		Kind:               req.Kind,
		CanonicalSignature: req.CanonicalSignature,
		Content:            req.Content,
		SignatureOnly:      req.SignatureOnly,
	})

	astRes := ast.NodeEmbedResult{
		PublishID:     res.PublishID,
		QdrantPointID: res.QdrantPointID,
		LastEventKind: res.LastEventKind,
	}

	if err == nil {
		return astRes, nil
	}
	if errors.Is(err, ErrAttemptFailed) {
		// Preserve the original message but reroot the chain
		// onto the ast-level sentinel so the dispatcher
		// doesn't need to know about embedding.ErrAttemptFailed.
		return astRes, fmt.Errorf("%w: %v", ast.ErrPublishRecordedFailed, err)
	}
	return astRes, err
}
