package agentapi

// Stage 8.1 / e2e §13 contract test for agent.recall:
//
//	"closed degraded_reason enforced — fault injector returns
//	 'oops' → response is either rewritten OR 500."
//
// Mirrors observe_faultinjection_test.go but for the recall
// verb. Recall funnels every successful exit through
// `applyRecallDegradedContract`, which:
//
//  1. Overlays a higher-priority injected reason on top of
//     the real degraded signal.
//  2. Runs `degraded.Enforce` on the final pair, wrapping a
//     non-closed reason as `ErrUnknownReason`.
//  3. Bumps the per-verb metric counter only when the
//     surviving reason is a closed-set value.

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/degraded"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

func TestRecall_faultInjection_closedSetReason_overlay(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	search := &recordingSearcher{hits: []embedding.SearchHit{
		{PointID: "pub-1", Score: 0.95, Payload: map[string]any{"kind": "method"}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pub-1": {}}}
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbRecall, degraded.ReasonRerankerModelStale)

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithDegradedMetric(metric),
		WithFaultInjector(fi),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "find a method",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      5,
	})
	if err != nil {
		t.Fatalf("Recall with closed-set injection must succeed, got err: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("resp.Degraded=false, want true under injection")
	}
	if resp.DegradedReason != degraded.ReasonRerankerModelStale {
		t.Fatalf("resp.DegradedReason=%q, want %q",
			resp.DegradedReason, degraded.ReasonRerankerModelStale)
	}
	if got := metric.Count(VerbRecall, degraded.ReasonRerankerModelStale); got != 1 {
		t.Fatalf("metric increment under injection = %d, want 1", got)
	}
}

func TestRecall_faultInjection_nonClosedReason_returnsInternalError(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	search := &recordingSearcher{hits: []embedding.SearchHit{
		{PointID: "pub-1", Score: 0.95, Payload: map[string]any{"kind": "method"}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pub-1": {}}}
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbRecall, "oops")

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithDegradedMetric(metric),
		WithFaultInjector(fi),
	)

	_, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "find a method",
		Kind:   embedding.NodeKindMethod,
		RepoID: "repo-a",
		K:      5,
	})
	if err == nil {
		t.Fatalf("Recall must fail when injector returns a non-closed reason")
	}
	if !errors.Is(err, degraded.ErrUnknownReason) {
		t.Fatalf("err = %v, want wraps ErrUnknownReason", err)
	}
	if got := metric.Count(VerbRecall, "oops"); got != 0 {
		t.Fatalf("metric MUST NOT count non-closed reasons, got %d", got)
	}
}
