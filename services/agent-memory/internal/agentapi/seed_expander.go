// Package agentapi: graph seed expansion.
//
// Stage 5.1 step 4 of implementation-plan.md mandates
// "expand the seed set by 1-2 structural hops through
// GraphReader (Stage 2.2) and assemble the candidate set."
// This file owns the abstraction the recall handler calls;
// the production wiring (cmd/agent-api/main.go) supplies a
// concrete `GraphReaderExpander` backed by the real
// `*graphreader.Reader`.
//
// Why a narrow interface lives in agentapi
// ----------------------------------------
// The recall service consumes a tiny slice of the
// GraphReader surface: "given a seed Node id, return up to
// `depth` hops of outbound `static_calls` / `observed_calls`
// edges plus the destination Node ids." Pulling in the
// full `*graphreader.Reader` would force every test fake to
// also satisfy `ListNodes`, `NeighborhoodCard`, etc. The
// narrow interface here keeps tests trivially mockable AND
// makes the dependency arrow obvious (agentapi imports
// `internal/graphreader` only at the binary composition
// layer, not inside the service struct).
//
// Hop semantics
// -------------
//   - Hop 0 = the seed Node itself (not returned — the
//     caller already has it as the matched candidate).
//   - Hop 1 = direct outbound edges from the seed.
//   - Hop 2 = outbound edges from each Hop-1 destination.
//
// The default depth is 1 (cheap, almost-always-useful).
// Stage 5.1 bounds depth at 2 to keep the per-call fan-out
// linear in the seed count.
package agentapi

import (
	"context"
)

// SeedExpander returns the structural neighborhood for the
// supplied seed Node ids. A nil expander on the Service
// short-circuits Step 4 — the recall returns only the
// embedding-vector hits (still useful, just no structural
// enrichment).
type SeedExpander interface {
	// Expand walks up to `depth` hops outward from each
	// `seedNodeIDs` entry, returning the edges + per-edge
	// destination Node ids discovered.  Order is the
	// expander's choice; the reranker (Step 5) re-orders
	// before the response is built.
	Expand(ctx context.Context, seedNodeIDs []string, depth int) (ExpansionResult, error)
}

// ExpansionResult is the output of `SeedExpander.Expand`.
// The recall handler projects this onto the response
// envelope's `Edges` slice and tags each newly-discovered
// destination node with a non-zero `StructuralDistance` for
// the reranker.
type ExpansionResult struct {
	// Edges discovered during expansion (deduped).
	Edges []EdgeHit
	// Frontier is the hydrated set of destination Nodes the
	// expander walked into. Each entry carries its hop
	// distance (1 = direct neighbor, 2 = two-hop) and the
	// best seed score that reached it; the recall handler
	// promotes these entries onto the candidate set with
	// `StructuralDistance = hop` and an inherited score so
	// the reranker scores expansion candidates instead of
	// dropping them (evaluator iter-1 #4: pre-iter-2
	// frontiers were discarded, so expansion never affected
	// the served ordering).
	//
	// Stage 5.1 production expander (GraphReaderExpander)
	// populates this; the legacy `FrontierNodeIDs` slice is
	// preserved below for backward compat with existing
	// test fakes.
	Frontier []FrontierNode
	// FrontierNodeIDs is the legacy per-id slice. Deprecated
	// — populate `Frontier` instead. When `Frontier` is
	// empty but `FrontierNodeIDs` is non-empty, the recall
	// handler synthesises one `FrontierNode{NodeID, Hop: 1}`
	// per id so test fakes that only set this field still
	// see frontier promotion onto the candidate set.
	//
	// Deprecated: use Frontier.
	FrontierNodeIDs []string
}

// FrontierNode is one hydrated expansion-discovered Node the
// expander walked into. The shape is intentionally narrow:
// just enough for the recall handler to promote the entry
// onto a `Candidate` (the rest of the payload is opaque
// because the expander does not have a Qdrant point id for
// these nodes — they're graph-only).
type FrontierNode struct {
	// NodeID is the destination Node id.
	NodeID string
	// RepoID is the destination Node's repo (mirrors the
	// `NodeHit.RepoID` surface).
	RepoID string
	// Kind is the destination Node's kind (method / block /
	// class / file). The recall handler tags the promoted
	// candidate with this kind so the v0 reranker's kind
	// prior fires correctly.
	Kind string
	// CanonicalSignature is the dereferenced signature so
	// the NodeHit card the recall response surfaces carries
	// enough metadata for the agent to inspect without a
	// follow-up GetNode round-trip.
	CanonicalSignature string
	// Hop is the integer hop distance from the nearest seed
	// (1 for direct neighbour, 2 for two-hop). The recall
	// handler writes this onto `Candidate.StructuralDistance`
	// so the v0 cold-start reranker can apply the §9.5
	// distance penalty.
	Hop int
	// BestSeedScore is the highest Qdrant cosine score among
	// the seeds that reached this frontier node. The recall
	// handler discounts this by `0.5^hop` to compute the
	// inherited candidate score — a 1-hop neighbour of a
	// 0.9-score seed scores 0.45, a 2-hop neighbour 0.225.
	// Setting the inherited score (rather than zero) keeps
	// frontier candidates competitive after rerank instead
	// of always sinking to the bottom of the list (rubber-
	// duck #3 on the iter-2 plan).
	BestSeedScore float32
}

// SeedExpanderFunc adapts a plain function into a
// SeedExpander. Used by tests and by the binary composition
// root to bridge `*graphreader.Reader` without importing
// the reader package from agentapi.
type SeedExpanderFunc func(ctx context.Context, seedNodeIDs []string, depth int) (ExpansionResult, error)

// Expand implements SeedExpander.
func (f SeedExpanderFunc) Expand(ctx context.Context, seedNodeIDs []string, depth int) (ExpansionResult, error) {
	return f(ctx, seedNodeIDs, depth)
}

// defaultExpansionDepth is the Stage 5.1 default; the
// recall handler caps `depth` at `maxExpansionDepth` so a
// misconfigured option cannot fan out unboundedly.
const (
	defaultExpansionDepth = 1
	maxExpansionDepth     = 2
)
