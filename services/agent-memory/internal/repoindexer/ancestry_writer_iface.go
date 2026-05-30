package repoindexer

import (
	"context"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// RepoCommitNodeEdgeWriter is the narrow writer interface
// AncestryWriter (see ancestry.go) depends on. It is the strict
// minimum surface the repo -> commit -> repo-node -> package ->
// file -> contains-edge ancestry pipeline needs, named and
// scoped so the Phase 2 work of REPO-SCANNER (impl-plan
// "AncestryWriter factored from worker") can land WITHOUT
// depending on the Phase 3 `graphsink.Sink` abstraction yet:
//
//   - Today the only concrete implementation is
//     *graphwriter.Writer, which already satisfies this
//     interface natively (no new method needed; see the
//     `_ RepoCommitNodeEdgeWriter = (*graphwriter.Writer)(nil)`
//     assertion in ancestry.go).
//
//   - When Phase 3 ships graphsink.Sink (a strict superset
//     adding `Flush(ctx)` + `Close()`), the AncestryWriter
//     constructor's parameter type widens to graphsink.Sink
//     without breaking existing call sites: any value
//     satisfying graphsink.Sink trivially satisfies
//     RepoCommitNodeEdgeWriter, so passing a Postgres /
//     SQLite / in-memory sink in continues to compile.
//
// Method shapes are kept byte-identical to the matching
// *graphwriter.Writer methods so the same RepoInput / NodeInput
// / EdgeInput / CommitInput payloads flow through every
// downstream sink without translation, and so any role-grant
// or fingerprint invariant the writer enforces is preserved
// when the worker / CLI talk to this surface.
type RepoCommitNodeEdgeWriter interface {
	EnsureRepo(ctx context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error)
	EnsureCommit(ctx context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error)
	InsertNode(ctx context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error)
	InsertEdge(ctx context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error)
}

// Compile-time assertion that *graphwriter.Writer satisfies the
// interface natively. If a future graphwriter refactor changes
// any of the four method signatures this assertion fails at
// build time, surfacing the contract drift before the worker
// or the CLI hits it at runtime.
var _ RepoCommitNodeEdgeWriter = (*graphwriter.Writer)(nil)
