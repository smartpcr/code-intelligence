// Package agentapi: degraded-snapshot fallback for the
// recall verb.
//
// Stage 5.1 step 9 of implementation-plan.md mandates a
// "degraded_reason='graph_store_unavailable' /
// 'embedding_index_unavailable' fallback that serves from
// the most recent valid `RecallContextLog` snapshot." The
// e2e-scenarios.md §4 "Graph store outage returns a
// degraded recall from snapshot" (line 416) and "Embedding
// index outage degrades to structural-prior fallback"
// (line 427) Gherkin scenarios pin the contract:
//
//   - The response carries `degraded=true` with one of the
//     two closed-set reasons.
//   - `nodes[]` / `edges[]` / `concepts[]` come from the most
//     recent snapshot the cache knows about for this repo.
//   - `reranker_model_version` reflects THAT snapshot's
//     version (NOT the cold-start fallback's, because the
//     snapshot was captured under whatever ranker was live
//     at the time).
//   - A fresh `RecallContextLog` row is written with
//     `served_under_degraded=true` so downstream `observe`
//     calls can detect the degraded provenance via
//     architecture.md §6.1.2.
package agentapi

import (
	"context"
	"errors"
)

// SnapshotSource is the read-side abstraction the recall
// handler consults to load the most recent valid recall
// snapshot for a repo. Stage 5.1 ships
// `agentapi.RecallContextSnapshotReader` (in
// `cmd/agent-api/main.go`) as the production
// implementation; this interface keeps the recall service
// free of any direct dependency on the
// `recallcontext` package's read shape.
//
// The recall service treats LatestForRepo as a soft
// dependency: a `nil` SnapshotSource disables the snapshot-
// fallback path (the recall returns an empty response with
// `degraded=true` and the reason but no hits — better than
// returning a stale response from a different repo). An
// error from LatestForRepo is logged at warn and the
// fallback degrades to the empty-hits response.
type SnapshotSource interface {
	// LatestForRepo returns the most recent successful
	// recall snapshot for `repoID`. Returns
	// `ErrNoSnapshot` when no row exists (cold-start repo).
	// Any other error is treated by the recall handler as a
	// soft failure — the degraded-reason envelope still
	// surfaces but `Hits` is empty.
	LatestForRepo(ctx context.Context, repoID string) (RecallSnapshot, error)
}

// SnapshotSourceFunc adapts a plain function into a
// SnapshotSource. Used by the binary composition root.
type SnapshotSourceFunc func(ctx context.Context, repoID string) (RecallSnapshot, error)

// LatestForRepo implements SnapshotSource.
func (f SnapshotSourceFunc) LatestForRepo(ctx context.Context, repoID string) (RecallSnapshot, error) {
	return f(ctx, repoID)
}

// RecallSnapshot is the materialised view of the most
// recent valid `recall_context_log(verb='recall')` row for a
// repo, including the dereferenced Node / Edge / Concept
// cards in storage order.
//
// The fields mirror `RecallResponse` minus the operational
// counters; the fallback path projects this onto a
// `RecallResponse` and stamps `Degraded=true`,
// `DegradedReason=<reason>` before returning.
type RecallSnapshot struct {
	// ContextID is the source RecallContextLog row id (NOT
	// the new degraded-fallback row id — that is minted
	// fresh by the fallback writer).
	ContextID string
	// RerankerModelVersion is the version pinned on the
	// source row; the fallback response surfaces this
	// verbatim per the e2e scenario "reranker_model_version
	// reflects that snapshot's version".
	RerankerModelVersion string
	// Nodes / Edges / Concepts are the dereferenced cards
	// in the source row's storage order.
	Nodes    []NodeHit
	Edges    []EdgeHit
	Concepts []ConceptHit
}

// NodeHit / EdgeHit / ConceptHit are the typed cards the
// recall response surfaces. They are intentionally
// separate from the `Hit` shape (which still represents
// raw Qdrant hits) so the response envelope can be projected
// onto the gRPC `NodeCard` / `EdgeCard` / `ConceptCard`
// messages without ambiguity.
type NodeHit struct {
	NodeID             string
	RepoID             string
	Kind               string
	CanonicalSignature string
	Score              float32
	PointID            string
}

// EdgeHit is the structural edge surfaced via expansion.
type EdgeHit struct {
	EdgeID           string
	RepoID           string
	Kind             string
	SrcNodeID        string
	DstNodeID        string
	ObservationCount int64
}

// ConceptHit is the Concept-layer hit (architecture.md §5.5.1).
type ConceptHit struct {
	ConceptID string
	Name      string
	Score     float32
	PointID   string
}

// ErrNoSnapshot is the sentinel SnapshotSource implementations
// return when there is no prior snapshot for the given repo.
// The recall handler treats it as a non-error empty result.
var ErrNoSnapshot = errors.New("agentapi: no snapshot available for repo")

// ErrGraphStoreUnavailable is the sentinel SeedExpander and
// SnapshotSource implementations return when the underlying
// graph store (pgxpool) is unreachable. The recall handler
// pattern-matches with `errors.Is` and routes the call into
// `degradedFallback("graph", err)` so the §C22 closed-set
// `graph_store_unavailable` reason surfaces on the response
// envelope instead of either swallowing the outage (the
// pre-iter-2 behaviour) or surfacing it as a hard 5xx.
//
// Production implementations
// --------------------------
//   - `GraphReaderExpander.Expand` wraps pgxpool connection
//     errors (timeout, refused, closed pool) into this
//     sentinel so the recall handler can fall back to the
//     snapshot.
//   - `cmd/agent-api/main.go.newSnapshotSourceFromDB`
//     wraps GraphReader hydration pool errors into this
//     sentinel too — the snapshot fallback path must still
//     produce a degraded envelope (with empty hits) rather
//     than swallow a dependency outage.
//
// Callers that want to surface a different reason (e.g. the
// embedding-index outage) MUST return their own sentinel and
// rely on the `layer` argument the recall handler passes to
// `classifyDegradedReason`.
var ErrGraphStoreUnavailable = errors.New("agentapi: graph store unavailable")

// DegradedReasonGraphStoreUnavailable is the §C22 reason
// emitted when the GraphReader read pool is unreachable.
// Exported so external callers (including the gRPC adapter
// and downstream tests) can pattern-match against the wire
// value rather than re-declaring the literal.
const DegradedReasonGraphStoreUnavailable = "graph_store_unavailable"

// DegradedReasonEmbeddingIndexUnavailable is the §C22 reason
// emitted when the Qdrant vector store is unreachable.
const DegradedReasonEmbeddingIndexUnavailable = "embedding_index_unavailable"

// degradedReasonGraphUnavailable / degradedReasonEmbeddingUnavailable
// are the legacy unexported aliases kept as package-internal
// convenience constants. New code SHOULD use the exported
// names.
const degradedReasonGraphUnavailable = DegradedReasonGraphStoreUnavailable

const degradedReasonEmbeddingUnavailable = DegradedReasonEmbeddingIndexUnavailable

// snapshotResponseForDegraded projects the snapshot onto a
// RecallResponse stamped with the supplied degraded
// reason. The caller is responsible for invoking the
// ContextLog appender afterwards with
// `ServedUnderDegraded=true`.
func snapshotResponseForDegraded(
	snap RecallSnapshot, reason string, k int,
) RecallResponse {
	if k <= 0 {
		k = len(snap.Nodes) + len(snap.Concepts)
	}
	resp := RecallResponse{
		Degraded:             true,
		DegradedReason:       reason,
		RerankerModelVersion: snap.RerankerModelVersion,
		Edges:                append([]EdgeHit(nil), snap.Edges...),
	}
	// Cap nodes + concepts at k preserving snapshot order
	// (the snapshot stored them in score-rank order).
	remaining := k
	nodeCap := remaining
	if nodeCap > len(snap.Nodes) {
		nodeCap = len(snap.Nodes)
	}
	resp.Nodes = append([]NodeHit(nil), snap.Nodes[:nodeCap]...)
	remaining -= nodeCap
	conceptCap := remaining
	if conceptCap < 0 {
		conceptCap = 0
	}
	if conceptCap > len(snap.Concepts) {
		conceptCap = len(snap.Concepts)
	}
	resp.Concepts = append([]ConceptHit(nil), snap.Concepts[:conceptCap]...)
	return resp
}

// classifyDegradedReason maps a low-level dependency error
// (e.g. a Qdrant HTTP failure, a GraphReader pool error) to
// the closed-set §C22 reason the recall response surfaces.
// Returns the empty string when the error does not map to
// any degraded reason (callers MUST propagate such errors
// as hard failures rather than degrade silently).
//
// The classifier is layer-tagged rather than substring-
// matching the error chain: the downstream HTTPQdrant /
// pgxpool layers do not export typed "unreachable" errors,
// and a structural recognizer would mis-classify a malformed
// query as a degraded outage. Today the recall handler only
// invokes the fallback when an entire dependency (Qdrant or
// the graph reader) failed — at that point the layer tag is
// the ground truth for the §C22 closed-set reason.
func classifyDegradedReason(layer string, err error) string {
	if err == nil {
		return ""
	}
	switch layer {
	case "qdrant":
		return degradedReasonEmbeddingUnavailable
	case "graph":
		return degradedReasonGraphUnavailable
	default:
		return ""
	}
}
