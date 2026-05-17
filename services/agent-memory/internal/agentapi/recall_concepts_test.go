package agentapi

// Unit tests for Stage 5.1 step 2 — mixed-collection seed
// fan-out. When the recall service is constructed with
// `WithConceptsEnabled(true)`, the handler MUST issue a
// second Search against `embedding.CollectionConcept` and
// merge those hits into the response envelope's `Concepts`
// slice.
//
// The §9.6a publish filter MUST apply to BOTH collections
// (Method + Concept) in a single round-trip — i.e. the
// filter sees the union of point ids, not two independent
// invocations — so the `recall_filter_unpublished_total`
// counter increments once per unpublished hit regardless of
// which collection it came from.

import (
	"context"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// collectionSearcher returns per-collection canned hits.
// Unlike `recordingSearcher` (which stores one collection
// at a time), this fake supports the mixed-seed flow that
// hits both Method and Concept collections in one Recall
// call.
type collectionSearcher struct {
	byCollection map[string][]embedding.SearchHit
	callCount    map[string]int
}

func (c *collectionSearcher) Search(_ context.Context, collection string, _ embedding.SearchRequest) ([]embedding.SearchHit, error) {
	if c.callCount == nil {
		c.callCount = make(map[string]int)
	}
	c.callCount[collection]++
	hits := c.byCollection[collection]
	out := make([]embedding.SearchHit, len(hits))
	copy(out, hits)
	return out, nil
}

// TestRecall_mixedSeedIncludesConcepts is the headline test
// for the Stage 5.1 mixed-seed scenario (e2e-scenarios.md
// line 365 "mixed seed includes Concepts"): when concepts
// are enabled, the response carries Concept hits alongside
// the Method/Block hits.
func TestRecall_mixedSeedIncludesConcepts(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	methodHits := []embedding.SearchHit{
		{PointID: "method-pub-1", Score: 0.95, Payload: map[string]any{
			"node_id": "node-1", "repo_id": "repo-a", "kind": "method",
		}},
		{PointID: "method-pub-2", Score: 0.90, Payload: map[string]any{
			"node_id": "node-2", "repo_id": "repo-a", "kind": "method",
		}},
	}
	conceptHits := []embedding.SearchHit{
		{PointID: "concept-pub-1", Score: 0.85, Payload: map[string]any{
			"concept_id": "concept-1", "name": "MutexPattern",
		}},
	}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod:  methodHits,
		embedding.CollectionConcept: conceptHits,
	}}
	filter := &allowListFilter{allow: map[string]struct{}{
		"method-pub-1":  {},
		"method-pub-2":  {},
		"concept-pub-1": {},
	}}
	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithConceptsEnabled(true),
		WithReranker(NewV0ColdStartReranker(nil)),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "find code that locks a shared resource",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	// Both collections must have been searched, exactly once each.
	if got := search.callCount[embedding.CollectionMethod]; got != 1 {
		t.Fatalf("Method searches = %d; want 1", got)
	}
	if got := search.callCount[embedding.CollectionConcept]; got != 1 {
		t.Fatalf("Concept searches = %d; want 1 (concepts enabled)", got)
	}

	// Filter saw the UNION of point ids in one invocation —
	// proves the §9.6a single-round-trip contract.
	if filter.callCount != 1 {
		t.Fatalf("filter call count = %d; want 1 (mixed-seed must merge before filtering)",
			filter.callCount)
	}
	wantInput := []string{"method-pub-1", "method-pub-2", "concept-pub-1"}
	if len(filter.lastInput) != len(wantInput) {
		t.Fatalf("filter input len = %d; want %d", len(filter.lastInput), len(wantInput))
	}
	for i, want := range wantInput {
		if filter.lastInput[i] != want {
			t.Fatalf("filter input[%d] = %q; want %q", i, filter.lastInput[i], want)
		}
	}

	// Response carries Concepts in addition to Nodes.
	if len(resp.Nodes) != 2 {
		t.Fatalf("len(resp.Nodes) = %d; want 2 (both methods passed filter)", len(resp.Nodes))
	}
	if len(resp.Concepts) != 1 {
		t.Fatalf("len(resp.Concepts) = %d; want 1 (concept passed filter)", len(resp.Concepts))
	}
	if resp.Concepts[0].ConceptID != "concept-1" {
		t.Fatalf("Concept[0].ConceptID = %q; want %q", resp.Concepts[0].ConceptID, "concept-1")
	}
	if resp.Concepts[0].Name != "MutexPattern" {
		t.Fatalf("Concept[0].Name = %q; want %q", resp.Concepts[0].Name, "MutexPattern")
	}

	// Reranker model version is pinned on the response so
	// the e2e Gherkin assertion on `reranker_model_version`
	// holds.
	if resp.RerankerModelVersion != V0ModelVersion {
		t.Fatalf("RerankerModelVersion = %q; want %q",
			resp.RerankerModelVersion, V0ModelVersion)
	}
}

// TestRecall_conceptsDisabledByDefault proves the
// backward-compatibility contract: a Service constructed
// without `WithConceptsEnabled` MUST NOT fan out to the
// Concept collection. This is the load-bearing assertion
// that existing recall fixtures (which use single-collection
// fakes) keep passing post-Stage-5.1.
func TestRecall_conceptsDisabledByDefault(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	hits := []embedding.SearchHit{
		{PointID: "pub-1", Score: 0.9, Payload: map[string]any{"kind": "method"}},
	}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: hits,
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pub-1": {}}}
	svc := NewService(emb, search, filter, WithLogger(quietLogger()))

	_, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "no concept fan-out",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      5,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if got := search.callCount[embedding.CollectionConcept]; got != 0 {
		t.Fatalf("Concept searches = %d; want 0 (concepts off by default)", got)
	}
}
