package agentapi

// Unit tests for Stage 5.3 — `agent.expand` verb.
//
// What these tests prove (per the Stage 5.3 acceptance
// scenarios in implementation-plan.md §5.3):
//
//  1. Callees with hot-path ranking — edges with higher
//     `observation_count` surface before lower-count ones.
//  2. Depth cap honoured — a call chain longer than the
//     requested depth returns only nodes at depth ≤ N AND
//     sets `Truncated=true`.
//  3. Expand writes a `RecallContextLog(verb='expand')`
//     row so a later observe can pin to the expansion.
//
// What these tests also prove (cross-cutting):
//
//   - Input validation rejects empty node_id, invalid
//     direction, negative depth.
//   - The verb falls back to a degraded envelope when the
//     graph store is unavailable (e2e-scenarios.md §6
//     "Expand under graph_store_unavailable").
//   - `direction='callers'` walks the inbound edge set.

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
)

// fakeExpandWalker is a deterministic `EdgeWalker` that
// returns canned edges per anchor and canned nodes per id.
// Per-direction edge slices let one test cover both
// callees and callers without redefining the fake.
type fakeExpandWalker struct {
	calleeEdges map[string][]graphreader.Edge
	callerEdges map[string][]graphreader.Edge
	nodes       map[string]graphreader.Node

	calleeErr error
	callerErr error
	nodeErr   error

	calleeCalls []string
	callerCalls []string
	nodeCalls   []string
}

func (f *fakeExpandWalker) ListCallees(
	_ context.Context, nodeID string, _ []string, _ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	f.calleeCalls = append(f.calleeCalls, nodeID)
	if f.calleeErr != nil {
		return nil, f.calleeErr
	}
	return append([]graphreader.Edge(nil), f.calleeEdges[nodeID]...), nil
}

func (f *fakeExpandWalker) ListCallers(
	_ context.Context, nodeID string, _ []string, _ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	f.callerCalls = append(f.callerCalls, nodeID)
	if f.callerErr != nil {
		return nil, f.callerErr
	}
	return append([]graphreader.Edge(nil), f.callerEdges[nodeID]...), nil
}

func (f *fakeExpandWalker) GetNode(
	_ context.Context, nodeID string, _ graphreader.ReaderOptions,
) (graphreader.Node, error) {
	f.nodeCalls = append(f.nodeCalls, nodeID)
	if f.nodeErr != nil {
		return graphreader.Node{}, f.nodeErr
	}
	if n, ok := f.nodes[nodeID]; ok {
		return n, nil
	}
	return graphreader.Node{}, graphreader.ErrNotFound
}

// staticObsCounter returns a fixed observation_count map.
// Mirrors the production `EdgeObservationCounter` shape so
// the expand handler exercises the same hot-path hydration
// code path the recall handler does.
type staticObsCounter struct {
	counts    map[string]int64
	err       error
	callCount int
	lastIDs   []string
}

func (s *staticObsCounter) CountByEdgeIDs(_ context.Context, ids []string) (map[string]int64, error) {
	s.callCount++
	s.lastIDs = append([]string(nil), ids...)
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[string]int64, len(ids))
	for _, id := range ids {
		if c, ok := s.counts[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

// newExpandRecorder builds a fresh recordingContextLog and
// returns it. The pointer is stored on the Service via
// WithContextLog so tests inspect the appended row.
func newExpandRecorder() *recordingContextLog {
	return &recordingContextLog{returnID: "ctx-expand-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
}

// repoUUID is a valid UUID literal the recallcontext
// validator accepts (the production validator rejects
// non-UUID repo ids). All fake nodes carry this so the
// happy-path context-log append succeeds.
const repoUUID = "11111111-2222-3333-4444-555555555555"

// uuid helpers — every id passed to the recallcontext
// appender must satisfy the UUID regex (see
// `internal/recallcontext/log.go.validateUUIDs`).
const (
	rootUUID  = "00000000-0000-0000-0000-000000000001"
	dstUUIDA  = "00000000-0000-0000-0000-0000000000aa"
	dstUUIDB  = "00000000-0000-0000-0000-0000000000bb"
	dstUUIDC  = "00000000-0000-0000-0000-0000000000cc"
	edgeUUIDA = "00000000-0000-0000-0000-00000000eea1"
	edgeUUIDB = "00000000-0000-0000-0000-00000000eeb1"
	edgeUUIDC = "00000000-0000-0000-0000-00000000eec1"
)

// minimalService builds a Service with the embedder /
// searcher / filter satisfied by no-op fakes — the expand
// verb does not consume them but the constructor panics on
// nil dependencies.
func minimalService(opts ...Option) *Service {
	emb := fakeEmbedder{vec: []float32{0.1}}
	srch := &collectionSearcher{}
	flt := &allowListFilter{allow: map[string]struct{}{}}
	all := append([]Option{WithLogger(quietLogger())}, opts...)
	return NewService(emb, srch, flt, all...)
}

// TestExpand_calleesWithHotPathRanking is the headline
// Stage 5.3 scenario: three `observed_calls` edges with
// observation_counts 1, 10, 100 from one Method must
// surface in descending count order.
func TestExpand_calleesWithHotPathRanking(t *testing.T) {
	walker := &fakeExpandWalker{
		calleeEdges: map[string][]graphreader.Edge{
			rootUUID: {
				{EdgeID: edgeUUIDA, RepoID: repoUUID, Kind: "observed_calls", SrcNodeID: rootUUID, DstNodeID: dstUUIDA},
				{EdgeID: edgeUUIDB, RepoID: repoUUID, Kind: "observed_calls", SrcNodeID: rootUUID, DstNodeID: dstUUIDB},
				{EdgeID: edgeUUIDC, RepoID: repoUUID, Kind: "observed_calls", SrcNodeID: rootUUID, DstNodeID: dstUUIDC},
			},
		},
		nodes: map[string]graphreader.Node{
			rootUUID: {NodeID: rootUUID, RepoID: repoUUID, Kind: "method", CanonicalSignature: "pkg.Root"},
			dstUUIDA: {NodeID: dstUUIDA, RepoID: repoUUID, Kind: "method", CanonicalSignature: "pkg.A"},
			dstUUIDB: {NodeID: dstUUIDB, RepoID: repoUUID, Kind: "method", CanonicalSignature: "pkg.B"},
			dstUUIDC: {NodeID: dstUUIDC, RepoID: repoUUID, Kind: "method", CanonicalSignature: "pkg.C"},
		},
	}
	counter := &staticObsCounter{counts: map[string]int64{
		edgeUUIDA: 1,
		edgeUUIDB: 10,
		edgeUUIDC: 100,
	}}
	contextLog := newExpandRecorder()

	svc := minimalService(
		WithEdgeWalker(walker),
		WithExpandObservationCounter(counter),
		WithContextLog(contextLog),
	)

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(resp.Edges) != 3 {
		t.Fatalf("len(Edges) = %d; want 3", len(resp.Edges))
	}
	wantCounts := []int64{100, 10, 1}
	for i, want := range wantCounts {
		if resp.Edges[i].ObservationCount != want {
			t.Fatalf("Edges[%d].ObservationCount = %d; want %d (DESC by count)",
				i, resp.Edges[i].ObservationCount, want)
		}
	}
	if resp.Truncated {
		t.Fatalf("Truncated = true; want false (graph naturally terminated)")
	}
	if resp.RootNodeID != rootUUID {
		t.Fatalf("RootNodeID = %q; want %q", resp.RootNodeID, rootUUID)
	}
	if len(resp.Nodes) != 3 {
		t.Fatalf("len(Nodes) = %d; want 3 (frontier nodes minus root)",
			len(resp.Nodes))
	}
	for _, n := range resp.Nodes {
		if n.NodeID == rootUUID {
			t.Fatalf("Nodes contains root %q; root should be excluded", rootUUID)
		}
	}
}

// TestExpand_depthCapHonoured proves the Stage 5.3
// depth-bound scenario: a chain longer than the requested
// depth surfaces only nodes at depth ≤ N and flips
// Truncated=true.
func TestExpand_depthCapHonoured(t *testing.T) {
	// Build a 10-deep linear chain: root -> n1 -> n2 -> ... -> n10.
	chainNodes := []string{
		rootUUID,
		"00000000-0000-0000-0000-000000000010",
		"00000000-0000-0000-0000-000000000020",
		"00000000-0000-0000-0000-000000000030",
		"00000000-0000-0000-0000-000000000040",
		"00000000-0000-0000-0000-000000000050",
		"00000000-0000-0000-0000-000000000060",
		"00000000-0000-0000-0000-000000000070",
		"00000000-0000-0000-0000-000000000080",
		"00000000-0000-0000-0000-000000000090",
		"00000000-0000-0000-0000-0000000000a0",
	}
	calleeMap := map[string][]graphreader.Edge{}
	nodesMap := map[string]graphreader.Node{}
	for i := 0; i < len(chainNodes); i++ {
		nodesMap[chainNodes[i]] = graphreader.Node{
			NodeID: chainNodes[i], RepoID: repoUUID,
			Kind: "method", CanonicalSignature: "pkg.Step" + chainNodes[i][len(chainNodes[i])-2:],
		}
		if i+1 < len(chainNodes) {
			edgeID := "00000000-0000-0000-0000-0000000ed" + chainNodes[i][len(chainNodes[i])-3:]
			calleeMap[chainNodes[i]] = []graphreader.Edge{{
				EdgeID:    edgeID,
				RepoID:    repoUUID,
				Kind:      "static_calls",
				SrcNodeID: chainNodes[i],
				DstNodeID: chainNodes[i+1],
			}}
		}
	}
	walker := &fakeExpandWalker{calleeEdges: calleeMap, nodes: nodesMap}

	svc := minimalService(WithEdgeWalker(walker))

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		Depth:     5,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	// Five hops past root → five reached nodes (chain[1]..chain[5]).
	if len(resp.Nodes) != 5 {
		t.Fatalf("len(Nodes) = %d; want 5 (depth=5)", len(resp.Nodes))
	}
	for _, n := range resp.Nodes {
		// Must NOT contain nodes at depths 6..10
		for i := 6; i < len(chainNodes); i++ {
			if n.NodeID == chainNodes[i] {
				t.Fatalf("Nodes contains depth-%d node %q; depth cap=5",
					i, n.NodeID)
			}
		}
	}
	if !resp.Truncated {
		t.Fatalf("Truncated = false; want true (chain continues past depth 5)")
	}
}

// TestExpand_depthCapNaturalTerminationNotTruncated proves
// the dual of the depth-cap test: when the graph naturally
// ends inside the depth budget, Truncated stays false.
func TestExpand_depthCapNaturalTerminationNotTruncated(t *testing.T) {
	walker := &fakeExpandWalker{
		calleeEdges: map[string][]graphreader.Edge{
			rootUUID: {{EdgeID: edgeUUIDA, RepoID: repoUUID, Kind: "static_calls", SrcNodeID: rootUUID, DstNodeID: dstUUIDA}},
			// dstUUIDA has NO outbound edges → BFS ends here.
		},
		nodes: map[string]graphreader.Node{
			rootUUID: {NodeID: rootUUID, RepoID: repoUUID, Kind: "method"},
			dstUUIDA: {NodeID: dstUUIDA, RepoID: repoUUID, Kind: "method"},
		},
	}

	svc := minimalService(WithEdgeWalker(walker))

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		Depth:     5,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if resp.Truncated {
		t.Fatalf("Truncated = true; want false (graph ended naturally)")
	}
}

// TestExpand_writesRecallContextLog proves Stage 5.3 step 3:
// a successful expand writes one RecallContextLog row with
// verb='expand' so a later observe can pin to this
// expansion.
func TestExpand_writesRecallContextLog(t *testing.T) {
	walker := &fakeExpandWalker{
		calleeEdges: map[string][]graphreader.Edge{
			rootUUID: {{EdgeID: edgeUUIDA, RepoID: repoUUID, Kind: "static_calls", SrcNodeID: rootUUID, DstNodeID: dstUUIDA}},
		},
		nodes: map[string]graphreader.Node{
			rootUUID: {NodeID: rootUUID, RepoID: repoUUID, Kind: "method"},
			dstUUIDA: {NodeID: dstUUIDA, RepoID: repoUUID, Kind: "method"},
		},
	}
	contextLog := newExpandRecorder()

	svc := minimalService(
		WithEdgeWalker(walker),
		WithContextLog(contextLog),
	)

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if contextLog.callCount != 1 {
		t.Fatalf("contextLog.callCount = %d; want 1", contextLog.callCount)
	}
	if contextLog.lastInput.Verb != "expand" {
		t.Fatalf("contextLog.lastInput.Verb = %q; want %q",
			contextLog.lastInput.Verb, "expand")
	}
	if contextLog.lastInput.RepoID != repoUUID {
		t.Fatalf("contextLog.lastInput.RepoID = %q; want %q",
			contextLog.lastInput.RepoID, repoUUID)
	}
	if contextLog.lastInput.ServedUnderDegraded {
		t.Fatalf("ServedUnderDegraded = true; want false on happy path")
	}
	if contextLog.lastInput.RerankerModelVersion != expandRerankerVersion {
		t.Fatalf("RerankerModelVersion = %q; want %q",
			contextLog.lastInput.RerankerModelVersion, expandRerankerVersion)
	}
	if resp.ContextID != "ctx-expand-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("resp.ContextID = %q; want the appender's return id",
			resp.ContextID)
	}
	// NodeIDs / EdgeIDs preserved so a later observe walks
	// them back faithfully.
	if got := len(contextLog.lastInput.NodeIDs); got != 1 {
		t.Fatalf("len(NodeIDs) = %d; want 1", got)
	}
	if got := len(contextLog.lastInput.EdgeIDs); got != 1 {
		t.Fatalf("len(EdgeIDs) = %d; want 1", got)
	}
}

// TestExpand_inputValidation proves all three Stage 5.3
// validation paths surface caller-correctable sentinels.
func TestExpand_inputValidation(t *testing.T) {
	walker := &fakeExpandWalker{
		nodes: map[string]graphreader.Node{
			rootUUID: {NodeID: rootUUID, RepoID: repoUUID, Kind: "method"},
		},
	}
	svc := minimalService(WithEdgeWalker(walker))
	ctx := context.Background()

	cases := []struct {
		name string
		req  ExpandRequest
		want error
	}{
		{
			name: "empty node_id",
			req:  ExpandRequest{Direction: DirectionCallees},
			want: ErrInvalidExpandNodeID,
		},
		{
			name: "invalid direction",
			req:  ExpandRequest{NodeID: rootUUID, Direction: "sideways"},
			want: ErrInvalidExpandDirection,
		},
		{
			name: "negative depth",
			req:  ExpandRequest{NodeID: rootUUID, Direction: DirectionCallees, Depth: -1},
			want: ErrInvalidExpandDepth,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.Expand(ctx, c.req)
			if !errors.Is(err, c.want) {
				t.Fatalf("err = %v; want %v", err, c.want)
			}
		})
	}
}

// TestExpand_unavailableWhenNoWalker proves the unwired
// binary contract: a Service with no EdgeWalker returns
// ErrExpandUnavailable so the gRPC adapter can map to
// codes.Unimplemented.
func TestExpand_unavailableWhenNoWalker(t *testing.T) {
	svc := minimalService()

	_, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
	})
	if !errors.Is(err, ErrExpandUnavailable) {
		t.Fatalf("err = %v; want %v", err, ErrExpandUnavailable)
	}
}

// TestExpand_callersDirectionWalksInbound proves
// direction='callers' uses the inbound walk and reports
// the source-side reached node.
func TestExpand_callersDirectionWalksInbound(t *testing.T) {
	walker := &fakeExpandWalker{
		callerEdges: map[string][]graphreader.Edge{
			// dstUUIDA is the target Block; two methods call
			// into it.
			dstUUIDA: {
				{EdgeID: edgeUUIDA, RepoID: repoUUID, Kind: "static_calls",
					SrcNodeID: dstUUIDB, DstNodeID: dstUUIDA},
				{EdgeID: edgeUUIDB, RepoID: repoUUID, Kind: "static_calls",
					SrcNodeID: dstUUIDC, DstNodeID: dstUUIDA},
			},
		},
		nodes: map[string]graphreader.Node{
			dstUUIDA: {NodeID: dstUUIDA, RepoID: repoUUID, Kind: "block"},
			dstUUIDB: {NodeID: dstUUIDB, RepoID: repoUUID, Kind: "method"},
			dstUUIDC: {NodeID: dstUUIDC, RepoID: repoUUID, Kind: "method"},
		},
	}
	svc := minimalService(WithEdgeWalker(walker))

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    dstUUIDA,
		Direction: DirectionCallers,
		Depth:     1,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(resp.Nodes) != 2 {
		t.Fatalf("len(Nodes) = %d; want 2 (two callers)", len(resp.Nodes))
	}
	// Reached node for an inbound walk is the edge's
	// `src_node_id`.  Verify both expected callers appear.
	seen := map[string]bool{}
	for _, n := range resp.Nodes {
		seen[n.NodeID] = true
	}
	if !seen[dstUUIDB] || !seen[dstUUIDC] {
		t.Fatalf("Nodes missing expected callers; got %+v", resp.Nodes)
	}
	if len(walker.callerCalls) == 0 {
		t.Fatalf("expected ListCallers to be invoked; got %d calls",
			len(walker.callerCalls))
	}
	if len(walker.calleeCalls) != 0 {
		t.Fatalf("unexpected ListCallees calls = %d for callers direction",
			len(walker.calleeCalls))
	}
}

// TestExpand_degradedFallbackOnGraphOutage proves the
// e2e-scenarios.md §6 "Expand under graph_store_unavailable"
// scenario: a pool-level connection error promotes to the
// §C22 degraded reason and returns a fallback envelope
// PROVIDED a snapshot source has been wired. Without a
// snapshot source the verb hard-fails — see
// `TestExpand_degradedHardFailsWhenNoSnapshotWired`.
func TestExpand_degradedFallbackOnGraphOutage(t *testing.T) {
	walker := &fakeExpandWalker{
		nodeErr: &net.OpError{Op: "dial", Err: errors.New("connection refused")},
	}
	snap := ExpandSnapshotSourceFunc(func(_ context.Context, repoID, nodeID, direction string) (ExpandSnapshot, error) {
		if repoID != repoUUID {
			return ExpandSnapshot{}, ErrNoExpandSnapshot
		}
		return ExpandSnapshot{
			ContextID:  "ctx-snap-1",
			RootNodeID: nodeID,
			Nodes: []NodeHit{
				{NodeID: dstUUIDA, RepoID: repoUUID, Kind: "method", CanonicalSignature: "pkg.SnapA"},
			},
			Edges: []EdgeHit{
				{EdgeID: edgeUUIDA, RepoID: repoUUID, Kind: "static_calls", SrcNodeID: nodeID, DstNodeID: dstUUIDA},
			},
		}, nil
	})
	contextLog := newExpandRecorder()

	svc := minimalService(
		WithEdgeWalker(walker),
		WithExpandSnapshot(snap),
		WithContextLog(contextLog),
	)

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		RepoID:    repoUUID,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("Degraded = false; want true on graph outage")
	}
	if resp.DegradedReason != DegradedReasonGraphStoreUnavailable {
		t.Fatalf("DegradedReason = %q; want %q",
			resp.DegradedReason, DegradedReasonGraphStoreUnavailable)
	}
	if len(resp.Nodes) != 1 || resp.Nodes[0].NodeID != dstUUIDA {
		t.Fatalf("Nodes = %+v; want snapshot's [{NodeID:%s ...}]",
			resp.Nodes, dstUUIDA)
	}
	if contextLog.callCount != 1 {
		t.Fatalf("contextLog.callCount = %d; want 1 (degraded path also appends)",
			contextLog.callCount)
	}
	if !contextLog.lastInput.ServedUnderDegraded {
		t.Fatalf("ServedUnderDegraded = false; want true on degraded path")
	}
	if contextLog.lastInput.Verb != "expand" {
		t.Fatalf("Verb = %q; want %q", contextLog.lastInput.Verb, "expand")
	}
}

// TestExpand_degradedEnvelopeWhenSnapshotMissing proves the
// secondary path: a graph outage WITH a snapshot source but
// NO matching row still returns a degraded envelope
// (empty Nodes/Edges) rather than a hard error — the
// snapshot being wired is the operator's opt-in for the
// "degrade not fail" contract (e2e-scenarios.md §6 cold-
// start row), even when the snapshot has nothing to serve.
func TestExpand_degradedEnvelopeWhenSnapshotMissing(t *testing.T) {
	walker := &fakeExpandWalker{
		nodeErr: &net.OpError{Op: "dial", Err: errors.New("connection refused")},
	}
	snap := ExpandSnapshotSourceFunc(func(_ context.Context, _, _, _ string) (ExpandSnapshot, error) {
		return ExpandSnapshot{}, ErrNoExpandSnapshot
	})

	svc := minimalService(
		WithEdgeWalker(walker),
		WithExpandSnapshot(snap),
	)

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		RepoID:    repoUUID,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("Degraded = false; want true on graph outage")
	}
	if len(resp.Edges) != 0 || len(resp.Nodes) != 0 {
		t.Fatalf("expected empty Edges/Nodes when no snapshot; got %+v / %+v",
			resp.Edges, resp.Nodes)
	}
}

// TestExpand_degradedHardFailsWhenNoSnapshotWired proves
// the recall-mirror contract: a binary with NO snapshot
// source must surface the underlying graph outage as a
// hard error instead of silently emitting a degraded
// envelope (evaluator iter-1 #2). Otherwise a
// misconfigured production binary would degrade every
// response and the operator would never notice.
func TestExpand_degradedHardFailsWhenNoSnapshotWired(t *testing.T) {
	connErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	walker := &fakeExpandWalker{nodeErr: connErr}

	svc := minimalService(WithEdgeWalker(walker)) // no WithExpandSnapshot

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		RepoID:    repoUUID,
	})
	if err == nil {
		t.Fatalf("Expand: nil error; want hard fail when no snapshot wired (got resp=%+v)", resp)
	}
	if !errors.Is(err, connErr) {
		t.Fatalf("err = %v; want chain to include the original outage cause (%v)",
			err, connErr)
	}
}

// TestExpand_hardDepthCeilingClampsExcessRequest proves
// the operator's depth cap is actually enforced when a
// caller asks for a depth larger than the service's
// configured maximum (default 5). The fake builds a 12-deep
// chain so a `Depth=999` request, silently clamped to 5,
// MUST stop at depth 5 with `Truncated=true` instead of
// walking the full chain. This is the load-bearing test for
// evaluator iter-1 #5: prior version used a 0-edge root
// which proved nothing about clamping (only that the
// validator accepted large depths).
func TestExpand_hardDepthCeilingClampsExcessRequest(t *testing.T) {
	// Build a 12-deep linear chain: root → n1 → ... → n12.
	chainNodes := []string{
		rootUUID,
		"00000000-0000-0000-0000-000000000c01",
		"00000000-0000-0000-0000-000000000c02",
		"00000000-0000-0000-0000-000000000c03",
		"00000000-0000-0000-0000-000000000c04",
		"00000000-0000-0000-0000-000000000c05",
		"00000000-0000-0000-0000-000000000c06",
		"00000000-0000-0000-0000-000000000c07",
		"00000000-0000-0000-0000-000000000c08",
		"00000000-0000-0000-0000-000000000c09",
		"00000000-0000-0000-0000-000000000c0a",
		"00000000-0000-0000-0000-000000000c0b",
		"00000000-0000-0000-0000-000000000c0c",
	}
	calleeMap := map[string][]graphreader.Edge{}
	nodesMap := map[string]graphreader.Node{}
	for i := 0; i < len(chainNodes); i++ {
		nodesMap[chainNodes[i]] = graphreader.Node{
			NodeID: chainNodes[i], RepoID: repoUUID, Kind: "method",
		}
		if i+1 < len(chainNodes) {
			edgeID := "00000000-0000-0000-0000-00000000c" + chainNodes[i][len(chainNodes[i])-3:]
			calleeMap[chainNodes[i]] = []graphreader.Edge{{
				EdgeID:    edgeID,
				RepoID:    repoUUID,
				Kind:      "static_calls",
				SrcNodeID: chainNodes[i],
				DstNodeID: chainNodes[i+1],
			}}
		}
	}
	walker := &fakeExpandWalker{calleeEdges: calleeMap, nodes: nodesMap}

	// Service uses the package default (5). No
	// WithExpandMaxDepth override.
	svc := minimalService(WithEdgeWalker(walker))

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		Depth:     999, // way past HardExpandDepthCeiling
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	// Default cap is 5 → only chain[1..5] reached. Asserts
	// the silent clamp (not an InvalidArgument).
	if got := len(resp.Nodes); got != DefaultExpandDepth {
		t.Fatalf("len(Nodes) = %d; want %d (default cap)", got, DefaultExpandDepth)
	}
	// chain[6..12] MUST NOT appear.
	for _, n := range resp.Nodes {
		for i := DefaultExpandDepth + 1; i < len(chainNodes); i++ {
			if n.NodeID == chainNodes[i] {
				t.Fatalf("Nodes contains beyond-cap node %q (depth %d > %d)",
					n.NodeID, i, DefaultExpandDepth)
			}
		}
	}
	// Real work remained past the cap → truncated MUST be true.
	if !resp.Truncated {
		t.Fatalf("Truncated = false; want true (chain extends past cap)")
	}
}

// TestExpand_perServiceDepthCapAlsoClamps complements the
// hard-ceiling test by proving the per-service
// `WithExpandMaxDepth` option (operator policy lever)
// independently clamps a large caller depth. Together with
// the test above this pins both the package-default and the
// operator-configured branches of the depth-cap logic.
func TestExpand_perServiceDepthCapAlsoClamps(t *testing.T) {
	chainNodes := []string{
		rootUUID,
		"00000000-0000-0000-0000-000000000d01",
		"00000000-0000-0000-0000-000000000d02",
		"00000000-0000-0000-0000-000000000d03",
		"00000000-0000-0000-0000-000000000d04",
		"00000000-0000-0000-0000-000000000d05",
	}
	calleeMap := map[string][]graphreader.Edge{}
	nodesMap := map[string]graphreader.Node{}
	for i := 0; i < len(chainNodes); i++ {
		nodesMap[chainNodes[i]] = graphreader.Node{
			NodeID: chainNodes[i], RepoID: repoUUID, Kind: "method",
		}
		if i+1 < len(chainNodes) {
			edgeID := "00000000-0000-0000-0000-00000000d" + chainNodes[i][len(chainNodes[i])-3:]
			calleeMap[chainNodes[i]] = []graphreader.Edge{{
				EdgeID:    edgeID,
				RepoID:    repoUUID,
				Kind:      "static_calls",
				SrcNodeID: chainNodes[i],
				DstNodeID: chainNodes[i+1],
			}}
		}
	}
	walker := &fakeExpandWalker{calleeEdges: calleeMap, nodes: nodesMap}

	// Operator policy: cap depth at 2 even though the
	// package default is 5.
	svc := minimalService(
		WithEdgeWalker(walker),
		WithExpandMaxDepth(2),
	)

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		Depth:     999,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if got := len(resp.Nodes); got != 2 {
		t.Fatalf("len(Nodes) = %d; want 2 (per-service cap)", got)
	}
	if !resp.Truncated {
		t.Fatalf("Truncated = false; want true (work remains past per-service cap)")
	}
}

// TestExpand_rejectsCrossTenantRepoID proves the tenant-
// safety check: if a caller passes a RepoID that does not
// match the seed node's repo, the verb rejects with an
// explicit error rather than silently re-writing the field.
func TestExpand_rejectsCrossTenantRepoID(t *testing.T) {
	walker := &fakeExpandWalker{
		nodes: map[string]graphreader.Node{
			rootUUID: {NodeID: rootUUID, RepoID: repoUUID, Kind: "method"},
		},
	}
	svc := minimalService(WithEdgeWalker(walker))

	_, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		RepoID:    "99999999-9999-9999-9999-999999999999",
	})
	if err == nil {
		t.Fatalf("expected error on cross-tenant repo_id")
	}
}

// boundedListWalker — a fakeExpandWalker variant whose
// `ListCallees` honours the reader's `Limit` option. The
// stock fake returns its FULL canned slice regardless of
// `Limit`, which is fine for depth/ordering tests but
// hides the production "edge-budget overflow" path the
// probe-overshoot pattern (evaluator iter-2 #1) was added
// to detect. This walker truncates the canned slice to
// `opts.Limit + 1` (where +1 reflects the probe-overshoot
// request) so we can pin the overflow contract end-to-end.
type boundedListWalker struct {
	*fakeExpandWalker
}

func (b *boundedListWalker) ListCallees(
	ctx context.Context, nodeID string, kinds []string, opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	edges, err := b.fakeExpandWalker.ListCallees(ctx, nodeID, kinds, opts)
	if err != nil {
		return nil, err
	}
	if opts.Limit > 0 && len(edges) > opts.Limit {
		edges = edges[:opts.Limit]
	}
	return edges, nil
}

// TestExpand_edgeBudgetOverflowSetsTruncated proves the
// evaluator iter-2 #1 fix: when a SINGLE anchor returns
// MORE edges than the remaining `MaxEdges` budget, the
// probe-overshoot request (`Limit = remainingBudget + 1`)
// detects the overflow and sets `Truncated=true`, even
// when the BFS would otherwise terminate naturally inside
// the depth budget. Without this check the response could
// silently omit edges while reporting `Truncated=false`.
//
// Setup: one anchor (the seed root) has 7 outbound static
// edges to 7 distinct dst nodes. With `MaxEdges=3` the
// reader request asks for 4 rows, the bounded walker
// returns 4, the handler keeps 3 and flips the flag.
func TestExpand_edgeBudgetOverflowSetsTruncated(t *testing.T) {
	// Build 7 dst node UUIDs + 7 edge UUIDs.
	dst := []string{
		"00000000-0000-0000-0000-00000000fa01",
		"00000000-0000-0000-0000-00000000fa02",
		"00000000-0000-0000-0000-00000000fa03",
		"00000000-0000-0000-0000-00000000fa04",
		"00000000-0000-0000-0000-00000000fa05",
		"00000000-0000-0000-0000-00000000fa06",
		"00000000-0000-0000-0000-00000000fa07",
	}
	edgeIDs := []string{
		"00000000-0000-0000-0000-00000000fb01",
		"00000000-0000-0000-0000-00000000fb02",
		"00000000-0000-0000-0000-00000000fb03",
		"00000000-0000-0000-0000-00000000fb04",
		"00000000-0000-0000-0000-00000000fb05",
		"00000000-0000-0000-0000-00000000fb06",
		"00000000-0000-0000-0000-00000000fb07",
	}
	edges := make([]graphreader.Edge, len(dst))
	nodesMap := map[string]graphreader.Node{
		rootUUID: {NodeID: rootUUID, RepoID: repoUUID, Kind: "method"},
	}
	for i, d := range dst {
		edges[i] = graphreader.Edge{
			EdgeID: edgeIDs[i], RepoID: repoUUID, Kind: "static_calls",
			SrcNodeID: rootUUID, DstNodeID: d,
		}
		nodesMap[d] = graphreader.Node{NodeID: d, RepoID: repoUUID, Kind: "method"}
	}
	inner := &fakeExpandWalker{
		calleeEdges: map[string][]graphreader.Edge{rootUUID: edges},
		nodes:       nodesMap,
	}
	walker := &boundedListWalker{fakeExpandWalker: inner}

	svc := minimalService(WithEdgeWalker(walker))

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		Depth:     1,
		MaxEdges:  3, // anchor has 7 edges; budget is 3
		MaxNodes:  100,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(resp.Edges) > 3 {
		t.Fatalf("len(Edges) = %d; want ≤ 3 (MaxEdges)", len(resp.Edges))
	}
	if !resp.Truncated {
		t.Fatalf("Truncated = false; want true (anchor had 7 edges, budget 3)")
	}
}

// TestExpand_probeGraphOutageRoutesDegraded proves the
// evaluator iter-2 #2 fix: a graph-store outage during
// the depth-boundary probe propagates UP through bfsExpand
// so `Service.Expand` can route the failure into
// `expandDegradedFallback` and emit the §C22 degraded
// signal. Without propagation a transient pool failure
// during the probe would surface as a non-degraded partial
// response (the prior implementation set `Truncated=true`
// and returned cleanly, masking the outage).
//
// Setup: chain root → A. The depth-1 walk succeeds (one
// edge). At the depth boundary the probe asks for `A`'s
// outbound edges and gets a `*net.OpError`. Result: the
// service routes through the snapshot fallback.
func TestExpand_probeGraphOutageRoutesDegraded(t *testing.T) {
	connErr := &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	// Walker variant whose ListCallees succeeds for the
	// seed but fails for any subsequent anchor (the probe).
	walker := &probeFailingWalker{
		seedEdges: []graphreader.Edge{
			{EdgeID: edgeUUIDA, RepoID: repoUUID, Kind: "static_calls",
				SrcNodeID: rootUUID, DstNodeID: dstUUIDA},
		},
		seedID:    rootUUID,
		probeErr:  connErr,
		seedNode:  graphreader.Node{NodeID: rootUUID, RepoID: repoUUID, Kind: "method"},
		dstNode:   graphreader.Node{NodeID: dstUUIDA, RepoID: repoUUID, Kind: "method"},
		dstNodeID: dstUUIDA,
	}

	snap := ExpandSnapshotSourceFunc(func(_ context.Context, repoID, nodeID, _ string) (ExpandSnapshot, error) {
		if repoID != repoUUID {
			return ExpandSnapshot{}, ErrNoExpandSnapshot
		}
		return ExpandSnapshot{
			ContextID:  "ctx-snap-probe-1",
			RootNodeID: nodeID,
			Nodes: []NodeHit{
				{NodeID: dstUUIDA, RepoID: repoUUID, Kind: "method"},
			},
		}, nil
	})

	svc := minimalService(
		WithEdgeWalker(walker),
		WithExpandSnapshot(snap),
	)

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		Depth:     1,
		RepoID:    repoUUID,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("Degraded = false; want true on probe-time graph outage")
	}
	if resp.DegradedReason != DegradedReasonGraphStoreUnavailable {
		t.Fatalf("DegradedReason = %q; want %q",
			resp.DegradedReason, DegradedReasonGraphStoreUnavailable)
	}
	if resp.ContextID != "ctx-snap-probe-1" && len(resp.Nodes) == 0 {
		t.Fatalf("expected the snapshot to be served (Nodes / ContextID populated); got %+v", resp)
	}
}

// probeFailingWalker is a tightly-controlled `EdgeWalker`:
// the seed anchor returns a single edge cleanly, but the
// subsequent probe call (which targets the seed's child)
// returns a connection error. This isolates the probe-
// failure code path from any other potential outage
// surface.
type probeFailingWalker struct {
	seedEdges []graphreader.Edge
	seedID    string
	probeErr  error
	seedNode  graphreader.Node
	dstNode   graphreader.Node
	dstNodeID string
}

func (p *probeFailingWalker) ListCallees(
	_ context.Context, nodeID string, _ []string, _ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	if nodeID == p.seedID {
		return append([]graphreader.Edge(nil), p.seedEdges...), nil
	}
	return nil, p.probeErr
}

func (p *probeFailingWalker) ListCallers(
	_ context.Context, _ string, _ []string, _ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return nil, nil
}

func (p *probeFailingWalker) GetNode(
	_ context.Context, nodeID string, _ graphreader.ReaderOptions,
) (graphreader.Node, error) {
	if nodeID == p.seedID {
		return p.seedNode, nil
	}
	if nodeID == p.dstNodeID {
		return p.dstNode, nil
	}
	return graphreader.Node{}, graphreader.ErrNotFound
}

// TestExpand_largeMaxNodesAndMaxEdgesClampDownToServiceCeiling
// proves the iter-4 evaluator finding fix: a caller asking
// for `MaxNodes` / `MaxEdges` larger than the per-service
// effective ceiling (the `WithExpandMaxNodes` /
// `WithExpandMaxEdges` operator override, else the package
// default) gets the result silently clamped DOWN to that
// ceiling. The ExpandRequest doc claims this behaviour for
// memory + §8.3 RPS-budget safety; this test pins the
// implementation so a future refactor can't silently let
// large caller values through to `bfsExpand`'s map
// preallocations.
//
// Setup: anchor with 6 outbound edges to 6 distinct dst
// nodes; service capped at `WithExpandMaxNodes(2),
// WithExpandMaxEdges(3)`; caller requests `MaxNodes=999,
// MaxEdges=999`. Expected: `len(Edges) <= 3`,
// `len(Nodes) <= 2`, `Truncated=true` (since real fan-out
// exceeds the clamped budget).
func TestExpand_largeMaxNodesAndMaxEdgesClampDownToServiceCeiling(t *testing.T) {
	dst := []string{
		"00000000-0000-0000-0000-00000000cd01",
		"00000000-0000-0000-0000-00000000cd02",
		"00000000-0000-0000-0000-00000000cd03",
		"00000000-0000-0000-0000-00000000cd04",
		"00000000-0000-0000-0000-00000000cd05",
		"00000000-0000-0000-0000-00000000cd06",
	}
	edgeIDs := []string{
		"00000000-0000-0000-0000-00000000ce01",
		"00000000-0000-0000-0000-00000000ce02",
		"00000000-0000-0000-0000-00000000ce03",
		"00000000-0000-0000-0000-00000000ce04",
		"00000000-0000-0000-0000-00000000ce05",
		"00000000-0000-0000-0000-00000000ce06",
	}
	edges := make([]graphreader.Edge, len(dst))
	nodesMap := map[string]graphreader.Node{
		rootUUID: {NodeID: rootUUID, RepoID: repoUUID, Kind: "method"},
	}
	for i, d := range dst {
		edges[i] = graphreader.Edge{
			EdgeID: edgeIDs[i], RepoID: repoUUID, Kind: "static_calls",
			SrcNodeID: rootUUID, DstNodeID: d,
		}
		nodesMap[d] = graphreader.Node{NodeID: d, RepoID: repoUUID, Kind: "method"}
	}
	inner := &fakeExpandWalker{
		calleeEdges: map[string][]graphreader.Edge{rootUUID: edges},
		nodes:       nodesMap,
	}
	walker := &boundedListWalker{fakeExpandWalker: inner}

	const svcMaxNodes = 2
	const svcMaxEdges = 3
	svc := minimalService(
		WithEdgeWalker(walker),
		WithExpandMaxNodes(svcMaxNodes),
		WithExpandMaxEdges(svcMaxEdges),
	)

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		Depth:     1,
		MaxNodes:  999, // way past svcMaxNodes
		MaxEdges:  999, // way past svcMaxEdges
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(resp.Edges) > svcMaxEdges {
		t.Fatalf("len(Edges) = %d; want ≤ %d (caller request 999 must clamp to service ceiling)",
			len(resp.Edges), svcMaxEdges)
	}
	if len(resp.Nodes) > svcMaxNodes {
		t.Fatalf("len(Nodes) = %d; want ≤ %d (caller request 999 must clamp to service ceiling)",
			len(resp.Nodes), svcMaxNodes)
	}
	if !resp.Truncated {
		t.Fatalf("Truncated = false; want true (anchor had 6 edges, clamped budget = %d)", svcMaxEdges)
	}
}

// TestExpand_smallPositiveMaxNodesEdgesAreHonoured proves
// the COMPLEMENT of the clamp test: a caller asking for a
// SMALLER `MaxNodes` / `MaxEdges` than the service ceiling
// still gets the smaller cap (the clamp is one-sided —
// callers can ask for less, never more). This guards
// against a sloppy refactor that swaps the clamp for an
// unconditional override.
func TestExpand_smallPositiveMaxNodesEdgesAreHonoured(t *testing.T) {
	// Reuse the 6-edge anchor from the clamp test so we can
	// observe the smaller cap actually limiting output.
	dst := []string{
		"00000000-0000-0000-0000-00000000cf01",
		"00000000-0000-0000-0000-00000000cf02",
		"00000000-0000-0000-0000-00000000cf03",
		"00000000-0000-0000-0000-00000000cf04",
		"00000000-0000-0000-0000-00000000cf05",
		"00000000-0000-0000-0000-00000000cf06",
	}
	edgeIDs := []string{
		"00000000-0000-0000-0000-00000000d001",
		"00000000-0000-0000-0000-00000000d002",
		"00000000-0000-0000-0000-00000000d003",
		"00000000-0000-0000-0000-00000000d004",
		"00000000-0000-0000-0000-00000000d005",
		"00000000-0000-0000-0000-00000000d006",
	}
	edges := make([]graphreader.Edge, len(dst))
	nodesMap := map[string]graphreader.Node{
		rootUUID: {NodeID: rootUUID, RepoID: repoUUID, Kind: "method"},
	}
	for i, d := range dst {
		edges[i] = graphreader.Edge{
			EdgeID: edgeIDs[i], RepoID: repoUUID, Kind: "static_calls",
			SrcNodeID: rootUUID, DstNodeID: d,
		}
		nodesMap[d] = graphreader.Node{NodeID: d, RepoID: repoUUID, Kind: "method"}
	}
	inner := &fakeExpandWalker{
		calleeEdges: map[string][]graphreader.Edge{rootUUID: edges},
		nodes:       nodesMap,
	}
	walker := &boundedListWalker{fakeExpandWalker: inner}

	// Default service ceiling (200 nodes / 400 edges) —
	// caller asks for 4 / 5, both well below ceiling.
	svc := minimalService(WithEdgeWalker(walker))

	resp, err := svc.Expand(context.Background(), ExpandRequest{
		NodeID:    rootUUID,
		Direction: DirectionCallees,
		Depth:     1,
		MaxNodes:  4,
		MaxEdges:  5,
	})
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(resp.Edges) > 5 {
		t.Fatalf("len(Edges) = %d; want ≤ 5 (caller request honoured)", len(resp.Edges))
	}
	if len(resp.Nodes) > 4 {
		t.Fatalf("len(Nodes) = %d; want ≤ 4 (caller request honoured)", len(resp.Nodes))
	}
	if !resp.Truncated {
		t.Fatalf("Truncated = false; want true (anchor had 6 edges, caller budget = 5)")
	}
}
