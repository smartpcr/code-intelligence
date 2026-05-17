package agentapi

// Unit tests for Stage 5.1 step 9 — degraded snapshot
// fallback (e2e-scenarios.md line 416 "Graph store outage
// returns a degraded recall from snapshot" and line 427
// "Embedding index outage degrades to structural-prior
// fallback").
//
// What these tests prove:
//
//  1. A Qdrant search error with a SnapshotSource wired
//     becomes a degraded RESPONSE (Degraded=true,
//     DegradedReason='embedding_index_unavailable') instead
//     of a hard error.
//  2. The response carries the snapshot's
//     `RerankerModelVersion` verbatim — the e2e contract
//     pins that the agent sees THE SNAPSHOT'S ranker
//     version, not the live cold-start fallback.
//  3. When a `ContextLogAppender` is wired, the fallback
//     path appends a fresh row with `ServedUnderDegraded=true`
//     so the downstream observe handler can auto-stamp the
//     `degraded_recall_context` Observation.
//  4. Without a SnapshotSource the legacy hard-fail
//     contract is preserved (Qdrant errors propagate).

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// erroringSearcher returns a fixed error on every Search.
type erroringSearcher struct{ err error }

func (e erroringSearcher) Search(_ context.Context, _ string, _ embedding.SearchRequest) ([]embedding.SearchHit, error) {
	return nil, e.err
}

// recordingContextLog captures the most recent append for
// inspection.  Mirrors the production `contextLog` adapter
// in cmd/agent-api/main.go (no business logic, just an
// observer).
type recordingContextLog struct {
	lastInput  ContextLogInput
	callCount  int
	returnID   string
	returnErr  error
}

func (r *recordingContextLog) Append(_ context.Context, in ContextLogInput) (ContextLogRecord, error) {
	r.callCount++
	r.lastInput = in
	if r.returnErr != nil {
		return ContextLogRecord{}, r.returnErr
	}
	id := r.returnID
	if id == "" {
		id = "ctx-fixed-uuid"
	}
	return ContextLogRecord{ContextID: id}, nil
}

// TestRecall_degradedSnapshotFallbackOnQdrantOutage proves
// the headline §9.5 contract: a Qdrant outage triggers a
// snapshot-backed degraded response with the §C22
// 'embedding_index_unavailable' reason.
func TestRecall_degradedSnapshotFallbackOnQdrantOutage(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	search := erroringSearcher{err: errors.New("connection refused")}
	filter := &allowListFilter{allow: map[string]struct{}{}}

	priorSnapshot := RecallSnapshot{
		ContextID:            "ctx-prev-1",
		RerankerModelVersion: "trained-v1.2",
		Nodes: []NodeHit{
			{NodeID: "node-prev-1", PointID: "pt-1", Score: 0.7, Kind: "method"},
			{NodeID: "node-prev-2", PointID: "pt-2", Score: 0.6, Kind: "method"},
		},
		Concepts: []ConceptHit{
			{ConceptID: "concept-prev-1", Name: "Lock", PointID: "pt-c1"},
		},
	}
	snapshotSrc := SnapshotSourceFunc(func(_ context.Context, repoID string) (RecallSnapshot, error) {
		if repoID != "repo-a" {
			return RecallSnapshot{}, ErrNoSnapshot
		}
		return priorSnapshot, nil
	})
	contextLog := &recordingContextLog{}

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithSnapshotFallback(snapshotSrc),
		WithContextLog(contextLog),
		WithReranker(NewV0ColdStartReranker(nil)),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "still want an answer please",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      5,
	})
	if err != nil {
		t.Fatalf("Recall: %v (degraded path MUST NOT propagate the cause)", err)
	}

	if !resp.Degraded {
		t.Fatalf("Degraded = false; want true (qdrant outage MUST surface degraded)")
	}
	if resp.DegradedReason != "embedding_index_unavailable" {
		t.Fatalf("DegradedReason = %q; want %q",
			resp.DegradedReason, "embedding_index_unavailable")
	}
	// The snapshot's ranker version, NOT the cold-start
	// fallback's — the agent sees what was live when the
	// snapshot was captured.
	if resp.RerankerModelVersion != "trained-v1.2" {
		t.Fatalf("RerankerModelVersion = %q; want snapshot's %q",
			resp.RerankerModelVersion, "trained-v1.2")
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("len(resp.Nodes) = %d; want 2 (snapshot nodes)", len(resp.Nodes))
	}
	if resp.Nodes[0].NodeID != "node-prev-1" {
		t.Fatalf("Nodes[0].NodeID = %q; want %q", resp.Nodes[0].NodeID, "node-prev-1")
	}
	if len(resp.Concepts) != 1 {
		t.Fatalf("len(resp.Concepts) = %d; want 1 (snapshot concept)", len(resp.Concepts))
	}

	// ContextLog row written for the degraded recall —
	// observe will read `served_under_degraded=true` here.
	if contextLog.callCount != 1 {
		t.Fatalf("contextLog append count = %d; want 1", contextLog.callCount)
	}
	if !contextLog.lastInput.ServedUnderDegraded {
		t.Fatalf("ContextLog row ServedUnderDegraded = false; want true")
	}
	if contextLog.lastInput.Verb != "recall" {
		t.Fatalf("ContextLog verb = %q; want %q", contextLog.lastInput.Verb, "recall")
	}
	if contextLog.lastInput.RepoID != "repo-a" {
		t.Fatalf("ContextLog repo_id = %q; want %q", contextLog.lastInput.RepoID, "repo-a")
	}
	if contextLog.lastInput.RerankerModelVersion != "trained-v1.2" {
		t.Fatalf("ContextLog reranker_model_version = %q; want snapshot's %q",
			contextLog.lastInput.RerankerModelVersion, "trained-v1.2")
	}
	if len(contextLog.lastInput.NodeIDs) != 2 {
		t.Fatalf("ContextLog node_ids len = %d; want 2", len(contextLog.lastInput.NodeIDs))
	}
}

// TestRecall_degradedFallbackPreservesHardFailWithoutSnapshotSrc
// proves that a missing SnapshotSource leaves the legacy
// hard-fail contract intact — operators MUST NOT silently
// lose Qdrant outage signals if they forgot to wire the
// fallback.
func TestRecall_degradedFallbackPreservesHardFailWithoutSnapshotSrc(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	search := erroringSearcher{err: errors.New("connection refused")}
	filter := &allowListFilter{allow: map[string]struct{}{}}

	svc := NewService(emb, search, filter, WithLogger(quietLogger()))

	_, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "no snapshot wired",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      5,
	})
	if err == nil {
		t.Fatalf("Recall: got nil error; want a propagated Qdrant error (no SnapshotSource wired)")
	}
}

// TestRecall_degradedFallbackEmptySnapshotEnvelope proves the
// no-prior-snapshot path: when the source returns
// ErrNoSnapshot, the response is still a degraded envelope
// (with empty hits) so the agent sees the degraded signal
// instead of a hard error.
func TestRecall_degradedFallbackEmptySnapshotEnvelope(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	search := erroringSearcher{err: errors.New("connection refused")}
	filter := &allowListFilter{allow: map[string]struct{}{}}

	snapshotSrc := SnapshotSourceFunc(func(_ context.Context, _ string) (RecallSnapshot, error) {
		return RecallSnapshot{}, ErrNoSnapshot
	})

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithSnapshotFallback(snapshotSrc),
		WithReranker(NewV0ColdStartReranker(nil)),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "cold-start degraded fallback",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-no-snap",
		K:      5,
	})
	if err != nil {
		t.Fatalf("Recall: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("Degraded = false; want true (degraded contract even without snapshot)")
	}
	if resp.DegradedReason != "embedding_index_unavailable" {
		t.Fatalf("DegradedReason = %q; want %q",
			resp.DegradedReason, "embedding_index_unavailable")
	}
	if len(resp.Nodes) != 0 {
		t.Fatalf("len(resp.Nodes) = %d; want 0 (no prior snapshot)", len(resp.Nodes))
	}
	// On the no-snapshot path the response surfaces the
	// live reranker version (since there's no snapshot
	// version to project).
	if resp.RerankerModelVersion != V0ModelVersion {
		t.Fatalf("RerankerModelVersion = %q; want %q (live reranker)",
			resp.RerankerModelVersion, V0ModelVersion)
	}
}
