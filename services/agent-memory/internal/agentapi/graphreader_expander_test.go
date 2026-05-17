package agentapi

// Unit tests for the GraphReader-backed seed expander
// (`graphreader_expander.go`). These tests use a fake
// `EdgeKindReader` so they run without a live pgxpool —
// the production wiring is exercised by the integration
// test workstream that owns the `recall_context_log`
// migration.
//
// What these tests prove (resolves evaluator iter-1 #5
// "The production expander is not the required 1-2 hop
// GraphReader path"):
//
//   1. The expander walks `depth` hops through
//      `ListEdgesFrom`, NOT a single fixed SQL query.
//   2. Frontier nodes are hydrated with kind +
//      canonical_signature so the recall handler can
//      project a usable NodeHit (the previous version
//      surfaced bare ids).
//   3. Frontier deduplication pins each node to its FIRST
//      hop — a destination reached at hop 1 by seed-A is
//      NOT re-listed at hop 2 even when reachable via
//      seed-B.
//   4. Pool-connection errors are mapped onto
//      `ErrGraphStoreUnavailable` so the recall handler
//      can trigger the §C22 `graph_store_unavailable`
//      degraded fallback.
//   5. The default edge-kind filter restricts to
//      `static_calls` + `observed_calls` — the §9.5
//      ranker's scoring inputs.

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
)

// fakeEdgeReader is a deterministic `EdgeKindReader`. The
// `edges` map is keyed by source node id and returns the
// edge slice on every ListEdgesFrom call for that source.
// `nodes` provides the GetNode lookup table for frontier
// hydration. `listErr` overrides edges and surfaces on
// every ListEdgesFrom — useful for the pool-error tests.
type fakeEdgeReader struct {
	edges   map[string][]graphreader.Edge
	nodes   map[string]graphreader.Node
	listErr error
	nodeErr error

	listKinds  [][]string
	listCalls  []string
	nodeCalls  []string
	listLimits []int
}

func (f *fakeEdgeReader) ListEdgesFrom(
	_ context.Context, srcNodeID string, kinds []string, opts graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	f.listCalls = append(f.listCalls, srcNodeID)
	f.listKinds = append(f.listKinds, append([]string(nil), kinds...))
	f.listLimits = append(f.listLimits, opts.Limit)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]graphreader.Edge(nil), f.edges[srcNodeID]...), nil
}

func (f *fakeEdgeReader) GetNode(
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

// TestGraphReaderExpander_walksDepth1 proves the BFS
// happy-path: one hop from the seed surfaces the
// outbound edges + hydrated frontier nodes.
func TestGraphReaderExpander_walksDepth1(t *testing.T) {
	reader := &fakeEdgeReader{
		edges: map[string][]graphreader.Edge{
			"seed-1": {
				{EdgeID: "e1", RepoID: "r", Kind: "static_calls", SrcNodeID: "seed-1", DstNodeID: "n2"},
				{EdgeID: "e2", RepoID: "r", Kind: "observed_calls", SrcNodeID: "seed-1", DstNodeID: "n3"},
			},
		},
		nodes: map[string]graphreader.Node{
			"n2": {NodeID: "n2", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N2"},
			"n3": {NodeID: "n3", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N3"},
		},
	}
	exp := NewGraphReaderExpander(reader, nil, 0)
	out, err := exp.Expand(context.Background(), []string{"seed-1"}, 1)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out.Edges) != 2 {
		t.Fatalf("len(out.Edges) = %d; want 2", len(out.Edges))
	}
	if len(out.Frontier) != 2 {
		t.Fatalf("len(out.Frontier) = %d; want 2 (n2 + n3)", len(out.Frontier))
	}
	// Frontier MUST be hydrated with kind + signature so
	// the recall handler can project a usable NodeHit
	// downstream.
	gotSigs := map[string]string{}
	for _, fn := range out.Frontier {
		gotSigs[fn.NodeID] = fn.CanonicalSignature
		if fn.Hop != 1 {
			t.Fatalf("frontier %q Hop = %d; want 1", fn.NodeID, fn.Hop)
		}
	}
	if gotSigs["n2"] != "pkg.N2" || gotSigs["n3"] != "pkg.N3" {
		t.Fatalf("frontier hydration missing signatures; got=%v", gotSigs)
	}
}

// TestGraphReaderExpander_defaultEdgeKinds proves the
// default kind filter is the §9.5 call-graph set, NOT a
// wide-open "all kinds" walk. A contains-edge expander
// would dwarf the call-graph signal the ranker cares about.
func TestGraphReaderExpander_defaultEdgeKinds(t *testing.T) {
	reader := &fakeEdgeReader{}
	exp := NewGraphReaderExpander(reader, nil, 0)
	_, _ = exp.Expand(context.Background(), []string{"seed-1"}, 1)
	if len(reader.listKinds) != 1 {
		t.Fatalf("ListEdgesFrom calls = %d; want 1", len(reader.listKinds))
	}
	got := reader.listKinds[0]
	wantSet := map[string]bool{"static_calls": true, "observed_calls": true}
	if len(got) != len(wantSet) {
		t.Fatalf("default kinds = %v; want %v", got, wantSet)
	}
	for _, k := range got {
		if !wantSet[k] {
			t.Fatalf("unexpected default kind %q in %v", k, got)
		}
	}
}

// TestGraphReaderExpander_dedupesAcrossSeeds proves that a
// node reachable from two seeds at the same hop only
// surfaces ONCE in the frontier (no double-count, no
// shadow EdgeHit for the same edge_id).
func TestGraphReaderExpander_dedupesAcrossSeeds(t *testing.T) {
	reader := &fakeEdgeReader{
		edges: map[string][]graphreader.Edge{
			"seed-1": {
				{EdgeID: "e1", RepoID: "r", Kind: "static_calls", SrcNodeID: "seed-1", DstNodeID: "shared"},
			},
			"seed-2": {
				{EdgeID: "e2", RepoID: "r", Kind: "static_calls", SrcNodeID: "seed-2", DstNodeID: "shared"},
			},
		},
		nodes: map[string]graphreader.Node{
			"shared": {NodeID: "shared", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.Shared"},
		},
	}
	exp := NewGraphReaderExpander(reader, nil, 0)
	out, err := exp.Expand(context.Background(), []string{"seed-1", "seed-2"}, 1)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out.Edges) != 2 {
		t.Fatalf("len(out.Edges) = %d; want 2 (one per seed)", len(out.Edges))
	}
	if len(out.Frontier) != 1 {
		t.Fatalf("len(out.Frontier) = %d; want 1 (shared MUST dedupe)", len(out.Frontier))
	}
	if out.Frontier[0].NodeID != "shared" {
		t.Fatalf("frontier[0] = %q; want shared", out.Frontier[0].NodeID)
	}
}

// TestGraphReaderExpander_walksDepth2 proves the 2-hop
// walk: seed → n2 → n3 surfaces THREE edges (one from
// seed, two from n2) and the frontier carries n2 at hop 1
// + n3 at hop 2.
func TestGraphReaderExpander_walksDepth2(t *testing.T) {
	reader := &fakeEdgeReader{
		edges: map[string][]graphreader.Edge{
			"seed-1": {
				{EdgeID: "e1", RepoID: "r", Kind: "static_calls", SrcNodeID: "seed-1", DstNodeID: "n2"},
			},
			"n2": {
				{EdgeID: "e2", RepoID: "r", Kind: "static_calls", SrcNodeID: "n2", DstNodeID: "n3"},
				{EdgeID: "e3", RepoID: "r", Kind: "observed_calls", SrcNodeID: "n2", DstNodeID: "n4"},
			},
		},
		nodes: map[string]graphreader.Node{
			"n2": {NodeID: "n2", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N2"},
			"n3": {NodeID: "n3", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N3"},
			"n4": {NodeID: "n4", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N4"},
		},
	}
	exp := NewGraphReaderExpander(reader, nil, 0)
	out, err := exp.Expand(context.Background(), []string{"seed-1"}, 2)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out.Edges) != 3 {
		t.Fatalf("len(out.Edges) = %d; want 3 (seed→n2, n2→n3, n2→n4)", len(out.Edges))
	}
	hops := map[string]int{}
	for _, fn := range out.Frontier {
		hops[fn.NodeID] = fn.Hop
	}
	if hops["n2"] != 1 {
		t.Fatalf("n2 hop = %d; want 1; got frontier=%v", hops["n2"], hops)
	}
	if hops["n3"] != 2 {
		t.Fatalf("n3 hop = %d; want 2; got frontier=%v", hops["n3"], hops)
	}
	if hops["n4"] != 2 {
		t.Fatalf("n4 hop = %d; want 2; got frontier=%v", hops["n4"], hops)
	}
}

// TestGraphReaderExpander_poolErrorMapsToSentinel proves
// the connection-class error → `ErrGraphStoreUnavailable`
// mapping. The recall handler pattern-matches this sentinel
// to trigger the §C22 degraded fallback.
func TestGraphReaderExpander_poolErrorMapsToSentinel(t *testing.T) {
	t.Run("net.OpError", func(t *testing.T) {
		reader := &fakeEdgeReader{
			listErr: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
		}
		exp := NewGraphReaderExpander(reader, nil, 0)
		_, err := exp.Expand(context.Background(), []string{"seed-1"}, 1)
		if !errors.Is(err, ErrGraphStoreUnavailable) {
			t.Fatalf("err = %v; want ErrGraphStoreUnavailable wrap", err)
		}
	})

	t.Run("pgconn class 08", func(t *testing.T) {
		reader := &fakeEdgeReader{
			listErr: &pgconn.PgError{Code: "08006", Message: "connection failure"},
		}
		exp := NewGraphReaderExpander(reader, nil, 0)
		_, err := exp.Expand(context.Background(), []string{"seed-1"}, 1)
		if !errors.Is(err, ErrGraphStoreUnavailable) {
			t.Fatalf("err = %v; want ErrGraphStoreUnavailable wrap", err)
		}
	})

	t.Run("non-connection pg error is NOT promoted", func(t *testing.T) {
		// SQLSTATE 23505 = unique_violation; this is a
		// domain error, NOT a pool outage. MUST NOT
		// surface as `graph_store_unavailable` because the
		// agent would receive a degraded envelope when the
		// system is actually healthy.
		reader := &fakeEdgeReader{
			listErr: &pgconn.PgError{Code: "23505", Message: "duplicate key"},
		}
		exp := NewGraphReaderExpander(reader, nil, 0)
		_, err := exp.Expand(context.Background(), []string{"seed-1"}, 1)
		if err == nil {
			t.Fatalf("expected non-nil error")
		}
		if errors.Is(err, ErrGraphStoreUnavailable) {
			t.Fatalf("non-connection pg error MUST NOT map to ErrGraphStoreUnavailable; got %v", err)
		}
	})
}

// TestGraphReaderExpander_depthZeroIsNoOp proves the
// trivial no-op path: depth <= 0 returns an empty result
// without ever touching the reader (cheap defence
// against an oncall accidentally setting expansion to 0).
func TestGraphReaderExpander_depthZeroIsNoOp(t *testing.T) {
	reader := &fakeEdgeReader{}
	exp := NewGraphReaderExpander(reader, nil, 0)
	out, err := exp.Expand(context.Background(), []string{"seed-1"}, 0)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(out.Edges) != 0 || len(out.Frontier) != 0 {
		t.Fatalf("depth=0 expansion MUST be empty; got edges=%d frontier=%d",
			len(out.Edges), len(out.Frontier))
	}
	if len(reader.listCalls) != 0 {
		t.Fatalf("depth=0 MUST NOT call ListEdgesFrom; got %d calls", len(reader.listCalls))
	}
}

// TestNewGraphReaderExpander_nilReaderPanics proves the
// fail-fast contract: a nil reader is unambiguously a
// programmer bug and must crash at wiring time, not on
// the first request.
func TestNewGraphReaderExpander_nilReaderPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("NewGraphReaderExpander(nil) did not panic")
		}
	}()
	_ = NewGraphReaderExpander(nil, nil, 0)
}

// fakeObservationCounter is a deterministic
// EdgeObservationCounter used by the iter-3 ObservationCount
// hydration tests. `counts` maps edge_id -> aggregate; an
// edge not present in the map ends up with count = 0 (the
// proto contract's documented fallback). `err` shortcuts to
// the error path.
type fakeObservationCounter struct {
	counts    map[string]int64
	err       error
	lastBatch []string
	calls     int
}

func (f *fakeObservationCounter) CountByEdgeIDs(
	_ context.Context, edgeIDs []string,
) (map[string]int64, error) {
	f.calls++
	f.lastBatch = append([]string(nil), edgeIDs...)
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]int64, len(edgeIDs))
	for _, id := range edgeIDs {
		if c, ok := f.counts[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

// TestGraphReaderExpander_populatesObservationCount proves
// evaluator iter-2 finding #4: the proto
// `EdgeCard.observation_count` field must reflect the
// real `trace_observation` aggregate. Without a counter
// wired the field stays zero (the in-tree default); with
// one wired EVERY EdgeHit gets its count stamped in a
// SINGLE batched call.
func TestGraphReaderExpander_populatesObservationCount(t *testing.T) {
	reader := &fakeEdgeReader{
		edges: map[string][]graphreader.Edge{
			"seed-1": {
				{EdgeID: "e1", RepoID: "r", Kind: "static_calls", SrcNodeID: "seed-1", DstNodeID: "n2"},
				{EdgeID: "e2", RepoID: "r", Kind: "observed_calls", SrcNodeID: "seed-1", DstNodeID: "n3"},
				{EdgeID: "e3", RepoID: "r", Kind: "static_calls", SrcNodeID: "seed-1", DstNodeID: "n4"},
			},
		},
		nodes: map[string]graphreader.Node{
			"n2": {NodeID: "n2", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N2"},
			"n3": {NodeID: "n3", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N3"},
			"n4": {NodeID: "n4", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N4"},
		},
	}
	counter := &fakeObservationCounter{
		counts: map[string]int64{
			"e1": 42,
			// e2 intentionally absent — observed_calls with
			// no rolling aggregate stays at 0.
			"e3": 7,
		},
	}
	exp := NewGraphReaderExpander(reader, nil, DefaultExpanderFanOut).
		WithObservationCounter(counter)
	res, err := exp.Expand(context.Background(), []string{"seed-1"}, 1)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(res.Edges) != 3 {
		t.Fatalf("len(res.Edges) = %d; want 3", len(res.Edges))
	}
	if counter.calls != 1 {
		t.Fatalf("counter.calls = %d; want 1 (batched once after BFS)", counter.calls)
	}
	if len(counter.lastBatch) != 3 {
		t.Fatalf("counter.lastBatch = %v; want 3 edge ids", counter.lastBatch)
	}
	byID := map[string]int64{}
	for _, e := range res.Edges {
		byID[e.EdgeID] = e.ObservationCount
	}
	if byID["e1"] != 42 {
		t.Fatalf("e1.ObservationCount = %d; want 42", byID["e1"])
	}
	if byID["e2"] != 0 {
		t.Fatalf("e2.ObservationCount = %d; want 0 (missing from counter map)", byID["e2"])
	}
	if byID["e3"] != 7 {
		t.Fatalf("e3.ObservationCount = %d; want 7", byID["e3"])
	}
}

// TestGraphReaderExpander_observationCountNilCounterStaysZero
// proves the fallback contract: the counter is OPTIONAL,
// and tests / environments that don't wire one keep
// ObservationCount = 0 silently. Defends against an
// accidental nil-pointer regression in
// hydrateObservationCounts.
func TestGraphReaderExpander_observationCountNilCounterStaysZero(t *testing.T) {
	reader := &fakeEdgeReader{
		edges: map[string][]graphreader.Edge{
			"seed-1": {
				{EdgeID: "e1", RepoID: "r", Kind: "static_calls", SrcNodeID: "seed-1", DstNodeID: "n2"},
			},
		},
		nodes: map[string]graphreader.Node{
			"n2": {NodeID: "n2", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N2"},
		},
	}
	exp := NewGraphReaderExpander(reader, nil, DefaultExpanderFanOut)
	res, err := exp.Expand(context.Background(), []string{"seed-1"}, 1)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(res.Edges) != 1 || res.Edges[0].ObservationCount != 0 {
		t.Fatalf("ObservationCount = %v; want 0 (no counter wired)", res.Edges)
	}
}

// TestGraphReaderExpander_observationCountErrorIsSoft proves
// the production resilience contract: a transient
// trace_observation outage MUST NOT take down the recall
// path. The expander returns the BFS edges with zero counts
// when the counter fails on a non-connection-class error
// (the recall handler will then log + serve the edges).
func TestGraphReaderExpander_observationCountErrorIsSoft(t *testing.T) {
	reader := &fakeEdgeReader{
		edges: map[string][]graphreader.Edge{
			"seed-1": {
				{EdgeID: "e1", RepoID: "r", Kind: "static_calls", SrcNodeID: "seed-1", DstNodeID: "n2"},
			},
		},
		nodes: map[string]graphreader.Node{
			"n2": {NodeID: "n2", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N2"},
		},
	}
	counter := &fakeObservationCounter{err: errors.New("trace_observation: 42P01 relation does not exist")}
	exp := NewGraphReaderExpander(reader, nil, DefaultExpanderFanOut).
		WithObservationCounter(counter)
	res, err := exp.Expand(context.Background(), []string{"seed-1"}, 1)
	if err != nil {
		t.Fatalf("Expand: %v (soft failures should not propagate)", err)
	}
	if len(res.Edges) != 1 || res.Edges[0].ObservationCount != 0 {
		t.Fatalf("ObservationCount = %v; want zero (counter error fell back)", res.Edges)
	}
}

// TestGraphReaderExpander_observationCountConnectionErrorDegrades
// proves the §C22 degraded-fallback contract: a connection-
// class failure on the trace_observation query MUST promote
// to ErrGraphStoreUnavailable so the recall handler routes
// the call into degradedFallback(`graph”, ...).
func TestGraphReaderExpander_observationCountConnectionErrorDegrades(t *testing.T) {
	reader := &fakeEdgeReader{
		edges: map[string][]graphreader.Edge{
			"seed-1": {
				{EdgeID: "e1", RepoID: "r", Kind: "static_calls", SrcNodeID: "seed-1", DstNodeID: "n2"},
			},
		},
		nodes: map[string]graphreader.Node{
			"n2": {NodeID: "n2", RepoID: "r", Kind: "method", CanonicalSignature: "pkg.N2"},
		},
	}
	counter := &fakeObservationCounter{
		err: &net.OpError{Op: "dial", Err: errors.New("connection refused")},
	}
	exp := NewGraphReaderExpander(reader, nil, DefaultExpanderFanOut).
		WithObservationCounter(counter)
	_, err := exp.Expand(context.Background(), []string{"seed-1"}, 1)
	if err == nil {
		t.Fatalf("Expand: want ErrGraphStoreUnavailable; got nil")
	}
	if !errors.Is(err, ErrGraphStoreUnavailable) {
		t.Fatalf("Expand: err = %v; want ErrGraphStoreUnavailable", err)
	}
}
