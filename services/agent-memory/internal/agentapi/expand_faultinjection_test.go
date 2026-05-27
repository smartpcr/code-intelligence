package agentapi

// Stage 8.1 / e2e §13 contract test for agent.expand.
// Mirrors recall_faultinjection_test.go but for the expand
// verb. Expand funnels every successful exit through
// `applyExpandDegradedContract`; the same overlay-then-enforce
// pipeline runs.

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/degraded"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
)

func newExpandFITestWalker() *fakeExpandWalker {
	return &fakeExpandWalker{
		calleeEdges: map[string][]graphreader.Edge{
			rootUUID: {
				{EdgeID: edgeUUIDA, RepoID: repoUUID, Kind: "observed_calls", SrcNodeID: rootUUID, DstNodeID: dstUUIDA},
			},
		},
		nodes: map[string]graphreader.Node{
			rootUUID: {NodeID: rootUUID, RepoID: repoUUID, Kind: "method", CanonicalSignature: "pkg.Root"},
			dstUUIDA: {NodeID: dstUUIDA, RepoID: repoUUID, Kind: "method", CanonicalSignature: "pkg.A"},
		},
	}
}

func TestExpand_faultInjection_closedSetReason_overlay(t *testing.T) {
	walker := newExpandFITestWalker()
	counter := &staticObsCounter{counts: map[string]int64{edgeUUIDA: 1}}
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbExpand, degraded.ReasonGraphStoreUnavailable)

	svc := minimalService(
		WithEdgeWalker(walker),
		WithExpandObservationCounter(counter),
		WithDegradedMetric(metric),
		WithFaultInjector(fi),
	)

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
	})
	if err != nil {
		t.Fatalf("Expand with closed-set injection must succeed, got err: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("resp.Degraded=false, want true under injection")
	}
	if resp.DegradedReason != degraded.ReasonGraphStoreUnavailable {
		t.Fatalf("resp.DegradedReason=%q, want %q",
			resp.DegradedReason, degraded.ReasonGraphStoreUnavailable)
	}
	if got := metric.Count(VerbExpand, degraded.ReasonGraphStoreUnavailable); got != 1 {
		t.Fatalf("metric increment under injection = %d, want 1", got)
	}
}

func TestExpand_faultInjection_nonClosedReason_returnsInternalError(t *testing.T) {
	walker := newExpandFITestWalker()
	counter := &staticObsCounter{counts: map[string]int64{edgeUUIDA: 1}}
	metric := degraded.NewCounter()
	fi := degraded.NewMapFaultInjector()
	fi.SetForVerb(VerbExpand, "oops")

	svc := minimalService(
		WithEdgeWalker(walker),
		WithExpandObservationCounter(counter),
		WithDegradedMetric(metric),
		WithFaultInjector(fi),
	)

	_, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
	})
	if err == nil {
		t.Fatalf("Expand must fail when injector returns a non-closed reason")
	}
	if !errors.Is(err, degraded.ErrUnknownReason) {
		t.Fatalf("err = %v, want wraps ErrUnknownReason", err)
	}
	if got := metric.Count(VerbExpand, "oops"); got != 0 {
		t.Fatalf("metric MUST NOT count non-closed reasons, got %d", got)
	}
}
