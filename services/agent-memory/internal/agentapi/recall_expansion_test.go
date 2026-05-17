package agentapi

// Unit tests for Stage 5.1 step 4 — graph seed expansion.
// When a SeedExpander is wired, the recall handler MUST:
//   1. Pass the Method/Block seed node ids (NOT Concept ids)
//      to the expander.
//   2. Surface the discovered edges on the response envelope's
//      `Edges` slice.
//   3. Continue serving even when the expander errors —
//      structural enrichment is a soft enhancement.

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// recordingExpander captures the seed ids the recall passed
// and returns canned edges.
type recordingExpander struct {
	lastSeeds []string
	lastDepth int
	callCount int
	returnErr error
	returnRes ExpansionResult
}

func (r *recordingExpander) Expand(_ context.Context, seedIDs []string, depth int) (ExpansionResult, error) {
	r.callCount++
	r.lastSeeds = append([]string(nil), seedIDs...)
	r.lastDepth = depth
	if r.returnErr != nil {
		return ExpansionResult{}, r.returnErr
	}
	return r.returnRes, nil
}

// TestRecall_expandsFromMethodSeeds proves the headline
// Stage 5.1 step 4 contract: the expander receives the
// Method seed node ids and the discovered edges surface on
// resp.Edges.
func TestRecall_expandsFromMethodSeeds(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1}}
	hits := []embedding.SearchHit{
		{PointID: "pub-1", Score: 0.9, Payload: map[string]any{
			"node_id": "seed-1", "repo_id": "repo-a", "kind": "method",
		}},
		{PointID: "pub-2", Score: 0.8, Payload: map[string]any{
			"node_id": "seed-2", "repo_id": "repo-a", "kind": "method",
		}},
	}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: hits,
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pub-1": {}, "pub-2": {}}}
	expander := &recordingExpander{
		returnRes: ExpansionResult{
			Edges: []EdgeHit{
				{EdgeID: "e1", SrcNodeID: "seed-1", DstNodeID: "neighbor-1", Kind: "static_calls"},
				{EdgeID: "e2", SrcNodeID: "seed-2", DstNodeID: "neighbor-2", Kind: "observed_calls"},
			},
			FrontierNodeIDs: []string{"neighbor-1", "neighbor-2"},
		},
	}

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithSeedExpander(expander),
		WithExpansionDepth(2),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "expand from seeds",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if expander.callCount != 1 {
		t.Fatalf("expander calls = %d; want 1", expander.callCount)
	}
	wantSeeds := map[string]bool{"seed-1": true, "seed-2": true}
	if len(expander.lastSeeds) != len(wantSeeds) {
		t.Fatalf("seed count = %d; want %d", len(expander.lastSeeds), len(wantSeeds))
	}
	for _, s := range expander.lastSeeds {
		if !wantSeeds[s] {
			t.Fatalf("unexpected seed %q passed to expander", s)
		}
	}
	if expander.lastDepth != 2 {
		t.Fatalf("expander depth = %d; want 2 (WithExpansionDepth(2))", expander.lastDepth)
	}
	if len(resp.Edges) != 2 {
		t.Fatalf("len(resp.Edges) = %d; want 2", len(resp.Edges))
	}
}

// TestRecall_expanderErrorIsSoft proves the soft-enhancement
// contract: an expander error MUST NOT take down the recall
// response. The Edges slice surfaces empty and the rest of
// the response is intact.
func TestRecall_expanderErrorIsSoft(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1}}
	hits := []embedding.SearchHit{
		{PointID: "pub-1", Score: 0.9, Payload: map[string]any{
			"node_id": "seed-1", "repo_id": "repo-a", "kind": "method",
		}},
	}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: hits,
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pub-1": {}}}
	expander := &recordingExpander{returnErr: errors.New("graph reader transient")}

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithSeedExpander(expander),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "soft expander failure",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      5,
	})
	if err != nil {
		t.Fatalf("Recall: %v (expander error MUST be soft)", err)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("len(resp.Nodes) = %d; want 1 (recall continues without expansion)", len(resp.Nodes))
	}
	if len(resp.Edges) != 0 {
		t.Fatalf("len(resp.Edges) = %d; want 0 (expansion failed)", len(resp.Edges))
	}
}
