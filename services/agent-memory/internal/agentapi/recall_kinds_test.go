package agentapi

// Unit tests for the Stage 5.1 iter-2 contracts the
// evaluator iter-1 review called out:
//
//   - #2 / #3: `RecallRequest.Kinds` is the canonical
//     mixed-kind selector and the recall service routes
//     each entry to the matching Qdrant collection.
//   - #4: the expander's `Frontier` is promoted onto the
//     candidate set with a hop-scaled inherited score and a
//     non-zero StructuralDistance so the reranker sees the
//     structural signal.
//   - #6: a `graph_store_unavailable` expander error
//     triggers the snapshot-backed degraded fallback (the
//     pre-iter-2 path silently swallowed graph outages).
//
// These tests do NOT replace the existing iter-1 contract
// tests — they assert NEW behaviour. Every iter-1 test that
// exercises the deprecated `Kind` field continues to pass
// because `normalizeKinds` treats `Kind` as a legacy alias.

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// TestRecall_kindsExplicitConceptOnly proves the new
// public contract: a caller can ask for the concept-only
// recall path by passing `Kinds: ["concept"]`. The recall
// handler:
//   - Searches the Concept collection (and ONLY the Concept
//     collection).
//   - Filters concept point ids through the publish filter.
//   - Surfaces the surviving hits on `resp.Concepts`, NOT
//     on `resp.Nodes`.
func TestRecall_kindsExplicitConceptOnly(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	conceptHits := []embedding.SearchHit{
		{PointID: "concept-pub-1", Score: 0.95, Payload: map[string]any{
			"concept_id": "concept-A", "name": "ChannelPattern",
		}},
		{PointID: "concept-pub-2", Score: 0.90, Payload: map[string]any{
			"concept_id": "concept-B", "name": "MutexPattern",
		}},
	}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionConcept: conceptHits,
		// Method/Block collections intentionally populated
		// — if the handler mistakenly fans out to them the
		// assertion below catches the extra Search call.
		embedding.CollectionMethod: []embedding.SearchHit{{
			PointID: "method-pub-x", Score: 0.99,
			Payload: map[string]any{"node_id": "node-X", "repo_id": "repo-a", "kind": "method"},
		}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{
		"concept-pub-1": {},
		"concept-pub-2": {},
		"method-pub-x":  {},
	}}
	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		// Concept-only callers do NOT need conceptsEnabled —
		// the explicit `Kinds` value carries the intent.
		WithReranker(NewV0ColdStartReranker(nil)),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "channel-pattern concept",
		Kinds:  []string{"concept"},
		RepoID: "repo-a",
		K:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if search.callCount[embedding.CollectionConcept] != 1 {
		t.Fatalf("Concept search calls = %d; want 1", search.callCount[embedding.CollectionConcept])
	}
	if got := search.callCount[embedding.CollectionMethod]; got != 0 {
		t.Fatalf("Method search calls = %d; want 0 (Kinds=[concept] must not fan out to method)", got)
	}
	if len(resp.Concepts) != 2 {
		t.Fatalf("len(resp.Concepts) = %d; want 2", len(resp.Concepts))
	}
	if len(resp.Nodes) != 0 {
		t.Fatalf("len(resp.Nodes) = %d; want 0 (concept-only recall MUST NOT project as Nodes)", len(resp.Nodes))
	}
}

// TestRecall_kindsMethodAndBlock proves the multi-kind
// mixed-collection fan-out the proto contract pins:
// `Kinds=["method","block"]` issues TWO Searches and merges
// the results into one ranked list.
func TestRecall_kindsMethodAndBlock(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: {{
			PointID: "method-pub", Score: 0.95,
			Payload: map[string]any{"node_id": "node-m", "repo_id": "r1", "kind": "method"},
		}},
		embedding.CollectionBlock: {{
			PointID: "block-pub", Score: 0.85,
			Payload: map[string]any{"node_id": "node-b", "repo_id": "r1", "kind": "block"},
		}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{
		"method-pub": {},
		"block-pub":  {},
	}}
	svc := NewService(emb, search, filter, WithLogger(quietLogger()))

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "mixed method+block",
		Kinds:  []string{"method", "block"},
		RepoID: "r1",
		K:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if search.callCount[embedding.CollectionMethod] != 1 {
		t.Fatalf("Method calls = %d; want 1", search.callCount[embedding.CollectionMethod])
	}
	if search.callCount[embedding.CollectionBlock] != 1 {
		t.Fatalf("Block calls = %d; want 1", search.callCount[embedding.CollectionBlock])
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("len(resp.Nodes) = %d; want 2 (one per kind)", len(resp.Nodes))
	}
}

// TestRecall_kindsMismatchRejected proves the §5.1
// validator rejects a `Kind` that is NOT in `Kinds`. A
// silent acceptance would let a caller think their narrowed
// `Kind` was honoured when in fact the wider `Kinds`
// triggered fan-out.
func TestRecall_kindsMismatchRejected(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1}}
	search := &collectionSearcher{}
	filter := &allowListFilter{}
	svc := NewService(emb, search, filter, WithLogger(quietLogger()))

	_, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "mismatch",
		Kinds:  []string{"block", "concept"},
		Kind:   embedding.NodeKindMethod, // not in Kinds
		RepoID: "r1",
		K:      5,
	})
	if !errors.Is(err, ErrInvalidKind) {
		t.Fatalf("Recall err = %v; want ErrInvalidKind wrap", err)
	}
}

// TestRecall_frontierPromotedAsCandidate proves evaluator
// iter-1 #4: the expander's frontier MUST become a
// rerankable Candidate with a non-zero StructuralDistance.
// We assert the response by checking:
//   - the surviving Nodes slice carries BOTH the seed and
//     the frontier node ids (proving the frontier was not
//     silently discarded);
//   - the v0 reranker placed the seed (cosine 0.9, hop 0)
//     ahead of the frontier (inherited 0.45, hop 1) — proving
//     the structural distance signal flowed through.
func TestRecall_frontierPromotedAsCandidate(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1}}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: {{
			PointID: "seed-pub", Score: 0.9,
			Payload: map[string]any{
				"node_id":             "seed-N",
				"repo_id":             "repo-a",
				"kind":                "method",
				"canonical_signature": "pkg.Seed",
			},
		}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"seed-pub": {}}}
	expander := &recordingExpander{
		returnRes: ExpansionResult{
			Edges: []EdgeHit{
				{EdgeID: "e1", SrcNodeID: "seed-N", DstNodeID: "frontier-N", Kind: "static_calls"},
			},
			Frontier: []FrontierNode{{
				NodeID:             "frontier-N",
				RepoID:             "repo-a",
				Kind:               embedding.NodeKindMethod,
				CanonicalSignature: "pkg.Frontier",
				Hop:                1,
				BestSeedScore:      0.9, // inherited from seed
			}},
		},
	}
	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithSeedExpander(expander),
		WithExpansionDepth(1),
		WithReranker(NewV0ColdStartReranker(nil)),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "promote frontier",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	// Both the seed AND the frontier node should appear on
	// the served Nodes slice.
	gotIDs := make([]string, len(resp.Nodes))
	for i, n := range resp.Nodes {
		gotIDs[i] = n.NodeID
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("len(resp.Nodes) = %d; want 2 (seed + frontier); got=%v", len(resp.Nodes), gotIDs)
	}
	// v0 reranker MUST place seed (cosine 0.9, hop 0) ahead
	// of frontier (inherited 0.45, hop 1): structural
	// distance is the deciding factor.
	if resp.Nodes[0].NodeID != "seed-N" {
		t.Fatalf("resp.Nodes[0] = %q; want seed-N (structural distance MUST favour direct hit); got=%v",
			resp.Nodes[0].NodeID, gotIDs)
	}
	if resp.Nodes[1].NodeID != "frontier-N" {
		t.Fatalf("resp.Nodes[1] = %q; want frontier-N (hop-1 promotion); got=%v",
			resp.Nodes[1].NodeID, gotIDs)
	}
}

// TestRecall_graphStoreUnavailableDegrades proves evaluator
// iter-1 #6: when the expander returns
// `ErrGraphStoreUnavailable`, the recall handler MUST
// project onto the snapshot fallback with
// `degraded_reason='graph_store_unavailable'`. Pre-iter-2
// the handler silently logged + continued, so the agent
// never saw the §C22 closed-set signal.
func TestRecall_graphStoreUnavailableDegrades(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1}}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: {{
			PointID: "seed-pub", Score: 0.9,
			Payload: map[string]any{
				"node_id": "seed-N", "repo_id": "repo-a", "kind": "method",
			},
		}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"seed-pub": {}}}
	expander := &recordingExpander{returnErr: ErrGraphStoreUnavailable}
	snap := &recordingSnapshot{
		snap: RecallSnapshot{
			ContextID:            "ctx-prior",
			RerankerModelVersion: V0ModelVersion,
			Nodes:                []NodeHit{{NodeID: "snap-N", RepoID: "repo-a", Kind: "method", CanonicalSignature: "pkg.Old"}},
		},
	}

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithSeedExpander(expander),
		WithSnapshotFallback(snap),
		WithReranker(NewV0ColdStartReranker(nil)),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "graph store down",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      5,
	})
	if err != nil {
		t.Fatalf("Recall: %v (graph_store_unavailable MUST be handled by snapshot fallback)", err)
	}
	if !resp.Degraded {
		t.Fatalf("resp.Degraded = false; want true")
	}
	if resp.DegradedReason != DegradedReasonGraphStoreUnavailable {
		t.Fatalf("resp.DegradedReason = %q; want %q",
			resp.DegradedReason, DegradedReasonGraphStoreUnavailable)
	}
	if snap.callCount != 1 {
		t.Fatalf("snapshot calls = %d; want 1", snap.callCount)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != "snap-N" {
		t.Fatalf("resp.Nodes = %+v; want one snap-N entry from snapshot", resp.Nodes)
	}
}

// recordingSnapshot is a deterministic SnapshotSource for
// the graph-degraded test. The legacy snapshot tests use a
// similar fake; defined here so this file is self-contained.
type recordingSnapshot struct {
	snap      RecallSnapshot
	err       error
	callCount int
	lastRepo  string
}

func (r *recordingSnapshot) LatestForRepo(_ context.Context, repoID string) (RecallSnapshot, error) {
	r.callCount++
	r.lastRepo = repoID
	if r.err != nil {
		return RecallSnapshot{}, r.err
	}
	return r.snap, nil
}

// TestRecall_frontierInheritsScoreWhenBestSeedScoreZero proves
// evaluator iter-2 finding #2: production GraphReaderExpander
// hydrates Frontier entries with BestSeedScore=0 (the graph
// layer doesn't see cosine scores). The recall handler MUST
// rescue these entries by inheriting the max seed-candidate
// score, otherwise frontier nodes rank as `structural-only
// negatives” and never make the served Nodes slice.
//
// Without the iter-3 fix the frontier node here would carry
// score = 0.0 * 0.5 = 0.0 and be discarded by the reranker.
// With the fix it inherits 0.85 * 0.5 = 0.425 (seed-N's
// score, hop-1 discount) and surfaces in second place behind
// the seed.
func TestRecall_frontierInheritsScoreWhenBestSeedScoreZero(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1}}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: {{
			PointID: "seed-pub", Score: 0.85,
			Payload: map[string]any{
				"node_id":             "seed-N",
				"repo_id":             "repo-a",
				"kind":                "method",
				"canonical_signature": "pkg.Seed",
			},
		}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"seed-pub": {}}}
	// Production GraphReaderExpander shape: Frontier entries
	// have BestSeedScore = 0 because the graph layer has no
	// embedding scores. The recall handler must fall back to
	// the aggregate max-seed-score.
	expander := &recordingExpander{
		returnRes: ExpansionResult{
			Edges: []EdgeHit{
				{EdgeID: "e1", SrcNodeID: "seed-N", DstNodeID: "frontier-N", Kind: "static_calls"},
			},
			Frontier: []FrontierNode{{
				NodeID:             "frontier-N",
				RepoID:             "repo-a",
				Kind:               embedding.NodeKindMethod,
				CanonicalSignature: "pkg.Frontier",
				Hop:                1,
				BestSeedScore:      0, // <-- production zero
			}},
		},
	}
	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithSeedExpander(expander),
		WithExpansionDepth(1),
		WithReranker(NewV0ColdStartReranker(nil)),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "frontier inherits seed score",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if len(resp.Nodes) != 2 {
		ids := make([]string, len(resp.Nodes))
		for i, n := range resp.Nodes {
			ids[i] = n.NodeID
		}
		t.Fatalf("len(resp.Nodes) = %d; want 2 (seed + rescued frontier); got=%v",
			len(resp.Nodes), ids)
	}
	if resp.Nodes[0].NodeID != "seed-N" {
		t.Fatalf("resp.Nodes[0] = %q; want seed-N (cosine 0.85, hop 0 wins)",
			resp.Nodes[0].NodeID)
	}
	if resp.Nodes[1].NodeID != "frontier-N" {
		t.Fatalf("resp.Nodes[1] = %q; want frontier-N (inherited 0.425, hop 1)",
			resp.Nodes[1].NodeID)
	}
}
