package agentapi

// Unit tests for Stage 5.1 step 6 — RecallContextLog
// appender. The recall handler MUST append exactly one
// row per successful call (and per degraded fallback) so
// the agent caller can replay the context via
// `agent.observe(context_id=...)`.

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// TestRecall_appendsContextLogOnHappyPath proves the §9.5
// audit row is written on a normal recall (not just
// degraded fallback). The returned context_id surfaces on
// the response so the agent can persist it for
// `agent.observe`.
func TestRecall_appendsContextLogOnHappyPath(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	hits := []embedding.SearchHit{
		{PointID: "pub-1", Score: 0.9, Payload: map[string]any{
			"node_id": "node-1", "repo_id": "repo-a", "kind": "method",
		}},
		{PointID: "pub-2", Score: 0.8, Payload: map[string]any{
			"node_id": "node-2", "repo_id": "repo-a", "kind": "method",
		}},
	}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: hits,
	}}
	filter := &allowListFilter{allow: map[string]struct{}{
		"pub-1": {}, "pub-2": {},
	}}
	contextLog := &recordingContextLog{returnID: "ctx-fresh-001"}

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithReranker(NewV0ColdStartReranker(nil)),
		WithContextLog(contextLog),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "happy path recall",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      10,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}

	if contextLog.callCount != 1 {
		t.Fatalf("contextLog appended %d rows; want exactly 1", contextLog.callCount)
	}
	if contextLog.lastInput.ServedUnderDegraded {
		t.Fatalf("ServedUnderDegraded = true; want false on happy path")
	}
	if contextLog.lastInput.Verb != "recall" {
		t.Fatalf("Verb = %q; want %q", contextLog.lastInput.Verb, "recall")
	}
	if contextLog.lastInput.RepoID != "repo-a" {
		t.Fatalf("RepoID = %q; want %q", contextLog.lastInput.RepoID, "repo-a")
	}
	if contextLog.lastInput.RerankerModelVersion != V0ModelVersion {
		t.Fatalf("RerankerModelVersion = %q; want %q",
			contextLog.lastInput.RerankerModelVersion, V0ModelVersion)
	}

	// NodeIDs preserve rank order so a later observe walks
	// them back in the same order.
	wantNodeIDs := []string{"node-1", "node-2"}
	if len(contextLog.lastInput.NodeIDs) != len(wantNodeIDs) {
		t.Fatalf("NodeIDs len = %d; want %d", len(contextLog.lastInput.NodeIDs), len(wantNodeIDs))
	}
	for i, want := range wantNodeIDs {
		if contextLog.lastInput.NodeIDs[i] != want {
			t.Fatalf("NodeIDs[%d] = %q; want %q (rank order)", i, contextLog.lastInput.NodeIDs[i], want)
		}
	}

	// QueryJSON MUST round-trip through json.Valid so the
	// writer's `jsonb` column does not reject it.
	if !json.Valid(contextLog.lastInput.QueryJSON) {
		t.Fatalf("QueryJSON is not valid JSON: %s", string(contextLog.lastInput.QueryJSON))
	}

	// Response surfaces the new context id so the agent can
	// store it for later observe calls.
	if resp.ContextID != "ctx-fresh-001" {
		t.Fatalf("resp.ContextID = %q; want %q (from Append return)",
			resp.ContextID, "ctx-fresh-001")
	}
}

// TestRecall_contextLogAppendFailureIsSoft proves the §9.5
// soft-failure contract: an append error MUST NOT take down
// the read path — the agent still gets a response, just
// without a ContextID.
func TestRecall_contextLogAppendFailureIsSoft(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1}}
	hits := []embedding.SearchHit{
		{PointID: "pub-1", Score: 0.9, Payload: map[string]any{
			"node_id": "node-1", "repo_id": "repo-a", "kind": "method",
		}},
	}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: hits,
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pub-1": {}}}
	contextLog := &recordingContextLog{returnErr: errors.New("pg down")}

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithContextLog(contextLog),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "context log down",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      5,
	})
	if err != nil {
		t.Fatalf("Recall: %v (append failure MUST be soft)", err)
	}
	if resp.ContextID != "" {
		t.Fatalf("resp.ContextID = %q; want empty (append failed)", resp.ContextID)
	}
	if len(resp.Nodes) != 1 {
		t.Fatalf("len(resp.Nodes) = %d; want 1 (response still served)", len(resp.Nodes))
	}
}

// TestRecall_noContextLogWiredIsBenign proves the legacy
// path: a Service constructed without `WithContextLog` MUST
// still serve recall — the audit row just isn't written
// (resp.ContextID stays empty). This keeps existing fixtures
// working post-Stage-5.1.
func TestRecall_noContextLogWiredIsBenign(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1}}
	hits := []embedding.SearchHit{
		{PointID: "pub-1", Score: 0.9, Payload: map[string]any{"kind": "method"}},
	}
	search := &collectionSearcher{byCollection: map[string][]embedding.SearchHit{
		embedding.CollectionMethod: hits,
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pub-1": {}}}

	svc := NewService(emb, search, filter, WithLogger(quietLogger()))

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "no context log",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      5,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if resp.ContextID != "" {
		t.Fatalf("resp.ContextID = %q; want empty (no ContextLog wired)", resp.ContextID)
	}
}
