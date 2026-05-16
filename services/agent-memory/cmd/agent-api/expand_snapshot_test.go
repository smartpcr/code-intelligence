package main

// Unit tests for `newExpandSnapshotSourceFromDB` — the
// production-only SQL adapter behind
// `agentapi.ExpandSnapshotSource` for the Stage 5.3
// `agent.expand` verb (resolves iter-2 evaluator finding
// #3 "production snapshot reader is untested").
//
// What these tests exercise:
//
//   - Happy-path "hit": fetches the most recent
//     non-degraded expand row for the (repo, node,
//     direction) tuple AND hydrates the referenced Node /
//     Edge ids via the GraphReader interface.
//   - Cold-start "miss": `sql.ErrNoRows` is mapped to
//     `agentapi.ErrNoExpandSnapshot` so the expand handler
//     keeps serving an empty degraded envelope instead of
//     bubbling a generic SQL error.
//   - Malformed `repo_id`: short-circuited via
//     `fingerprint.ParseRepoID` and returns
//     `agentapi.ErrNoExpandSnapshot` BEFORE the DB
//     round-trip (matches `newSnapshotSourceFromDB`'s
//     contract).
//   - Empty `node_id`: cheap pre-flight rejection — the
//     `query_json->>'node_id'` filter can't match `''`.
//   - Connection-class hydration failure: a node-hydration
//     error that classifies as
//     `agentapi.ErrGraphStoreUnavailable` is propagated
//     UP (so the expand handler still emits the §C22
//     degraded envelope downstream).
//   - Per-id `graphreader.ErrNotFound` on hydration is
//     SOFT (logged + skipped) so a retired row does not
//     poison the whole snapshot.
//   - Filter shape: the SQL query MUST scope on
//     `verb='expand'`, `query_json->>'node_id'`,
//     `query_json->>'direction'`, `served_under_degraded=false`,
//     in that order — these are the keys the expand
//     handler stamps in `appendExpandContextLog`.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"net"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/agentapi"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
)

// Distinct UUIDs from the observation_counter tests so a
// regression here doesn't shadow that file's diagnostics.
const (
	snapRepoID  = "11111111-1111-1111-1111-111111111111"
	snapNodeID  = "22222222-2222-2222-2222-222222222222"
	snapNodeID2 = "44444444-4444-4444-4444-444444444444"
	snapEdgeID  = "33333333-3333-3333-3333-333333333333"
	snapEdgeID2 = "55555555-5555-5555-5555-555555555555"
	snapCtxID   = "66666666-6666-6666-6666-666666666666"
)

// fakeExpandGraphReader is the in-memory stand-in for
// `*graphreader.Reader`. The package-local interface in
// main.go (`expandSnapshotGraphReader`) made this trivial
// — pre-refactor we could not test hydration without a
// live pgxpool.
type fakeExpandGraphReader struct {
	nodes   map[string]graphreader.Node
	edges   map[string]graphreader.Edge
	nodeErr error
	edgeErr error
}

func (f *fakeExpandGraphReader) GetNode(_ context.Context, nodeID string, _ graphreader.ReaderOptions) (graphreader.Node, error) {
	if f.nodeErr != nil {
		return graphreader.Node{}, f.nodeErr
	}
	if n, ok := f.nodes[nodeID]; ok {
		return n, nil
	}
	return graphreader.Node{}, graphreader.ErrNotFound
}

func (f *fakeExpandGraphReader) GetEdge(_ context.Context, edgeID string, _ graphreader.ReaderOptions) (graphreader.Edge, error) {
	if f.edgeErr != nil {
		return graphreader.Edge{}, f.edgeErr
	}
	if e, ok := f.edges[edgeID]; ok {
		return e, nil
	}
	return graphreader.Edge{}, graphreader.ErrNotFound
}

// staticExpandObsCounter — minimal stand-in for the
// `agentapi.EdgeObservationCounter` interface, returning a
// fixed observation_count per edge id.
type staticExpandObsCounter struct {
	counts map[string]int64
}

func (s *staticExpandObsCounter) CountByEdgeIDs(_ context.Context, ids []string) (map[string]int64, error) {
	out := make(map[string]int64, len(ids))
	for _, id := range ids {
		if c, ok := s.counts[id]; ok {
			out[id] = c
		}
	}
	return out, nil
}

func quietExpandSnapshotLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// expectExpandSnapshotQuery centralises the canonical
// filter shape so every test pins the SAME query and a
// regression in the SELECT scaffold surfaces in one place.
func expectExpandSnapshotQuery(t *testing.T, mock sqlmock.Sqlmock, repoID, nodeID, direction string) *sqlmock.ExpectedQuery {
	t.Helper()
	const queryRE = `SELECT\s+context_id::text,\s*` +
		`\(SELECT COALESCE\(array_agg\(x::text\), ARRAY\[\]::text\[\]\) FROM unnest\(node_ids\) AS x\),\s*` +
		`\(SELECT COALESCE\(array_agg\(x::text\), ARRAY\[\]::text\[\]\) FROM unnest\(edge_ids\) AS x\)\s+` +
		`FROM\s+recall_context_log\s+` +
		`WHERE\s+repo_id\s+=\s+\$1\s+` +
		`AND\s+verb\s+=\s+'expand'\s+` +
		`AND\s+query_json->>'node_id'\s+=\s+\$2\s+` +
		`AND\s+query_json->>'direction'\s+=\s+\$3\s+` +
		`AND\s+served_under_degraded\s+=\s+false\s+` +
		`ORDER\s+BY\s+created_at\s+DESC\s+` +
		`LIMIT\s+1`
	return mock.ExpectQuery(queryRE).WithArgs(repoID, nodeID, direction)
}

// TestNewExpandSnapshotSourceFromDB_happyPath_hydratesNodesAndEdges
// proves the load-bearing hit case: one prior non-degraded
// expand row references one node + one edge; hydration
// returns real cards; observation_count is populated.
func TestNewExpandSnapshotSourceFromDB_happyPath_hydratesNodesAndEdges(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectExpandSnapshotQuery(t, mock, snapRepoID, snapNodeID, "callees").
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "node_ids", "edge_ids",
		}).AddRow(
			snapCtxID,
			"{"+snapNodeID2+"}",
			"{"+snapEdgeID+"}",
		))

	gr := &fakeExpandGraphReader{
		nodes: map[string]graphreader.Node{
			snapNodeID2: {
				NodeID:             snapNodeID2,
				RepoID:             snapRepoID,
				Kind:               "method",
				CanonicalSignature: "pkg.Foo#bar()",
			},
		},
		edges: map[string]graphreader.Edge{
			snapEdgeID: {
				EdgeID:    snapEdgeID,
				RepoID:    snapRepoID,
				Kind:      "static_calls",
				SrcNodeID: snapNodeID,
				DstNodeID: snapNodeID2,
			},
		},
	}
	counter := &staticExpandObsCounter{counts: map[string]int64{snapEdgeID: 17}}

	src := newExpandSnapshotSourceFromDB(db, gr, counter, quietExpandSnapshotLogger())
	snap, err := src.LatestForExpand(context.Background(), snapRepoID, snapNodeID, "callees")
	if err != nil {
		t.Fatalf("LatestForExpand: %v", err)
	}
	if snap.ContextID != snapCtxID {
		t.Fatalf("ContextID = %q; want %q", snap.ContextID, snapCtxID)
	}
	if snap.RootNodeID != snapNodeID {
		t.Fatalf("RootNodeID = %q; want %q (must echo the request node)",
			snap.RootNodeID, snapNodeID)
	}
	if len(snap.Nodes) != 1 || snap.Nodes[0].NodeID != snapNodeID2 {
		t.Fatalf("Nodes = %+v; want exactly one node with id %q",
			snap.Nodes, snapNodeID2)
	}
	if snap.Nodes[0].CanonicalSignature != "pkg.Foo#bar()" {
		t.Fatalf("Nodes[0].CanonicalSignature = %q; want hydrated value",
			snap.Nodes[0].CanonicalSignature)
	}
	if len(snap.Edges) != 1 || snap.Edges[0].EdgeID != snapEdgeID {
		t.Fatalf("Edges = %+v; want one edge with id %q",
			snap.Edges, snapEdgeID)
	}
	if snap.Edges[0].ObservationCount != 17 {
		t.Fatalf("Edges[0].ObservationCount = %d; want 17 (observation hydration)",
			snap.Edges[0].ObservationCount)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestNewExpandSnapshotSourceFromDB_coldStartMiss proves
// sql.ErrNoRows → agentapi.ErrNoExpandSnapshot mapping so
// the expand handler keeps the degraded path soft instead
// of bubbling a generic SQL error.
func TestNewExpandSnapshotSourceFromDB_coldStartMiss(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectExpandSnapshotQuery(t, mock, snapRepoID, snapNodeID, "callees").
		WillReturnError(sql.ErrNoRows)

	src := newExpandSnapshotSourceFromDB(db, &fakeExpandGraphReader{}, nil, quietExpandSnapshotLogger())
	_, err = src.LatestForExpand(context.Background(), snapRepoID, snapNodeID, "callees")
	if !errors.Is(err, agentapi.ErrNoExpandSnapshot) {
		t.Fatalf("err = %v; want ErrNoExpandSnapshot", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestNewExpandSnapshotSourceFromDB_malformedRepoIDIsSoft
// proves the same malformed-id soft-fail the recall
// snapshot source has: a non-UUID repo id returns
// ErrNoExpandSnapshot BEFORE the DB query. Without this
// short-circuit Postgres would surface SQLSTATE 22P02
// which is NOT classified as a graph outage and would
// leak to the agent.
func TestNewExpandSnapshotSourceFromDB_malformedRepoIDIsSoft(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	// Intentionally NO ExpectQuery — any DB hit fails.

	src := newExpandSnapshotSourceFromDB(db, &fakeExpandGraphReader{}, nil, quietExpandSnapshotLogger())
	_, err = src.LatestForExpand(context.Background(), "not-a-uuid", snapNodeID, "callees")
	if !errors.Is(err, agentapi.ErrNoExpandSnapshot) {
		t.Fatalf("err = %v; want ErrNoExpandSnapshot for malformed repo_id", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestNewExpandSnapshotSourceFromDB_emptyNodeIDIsSoft —
// `query_json->>'node_id'` cannot match `”` (it's keyed
// on the canonical UUID), and an empty id is almost
// certainly a caller bug rather than a real query. Soft-
// fail same as malformed repo.
func TestNewExpandSnapshotSourceFromDB_emptyNodeIDIsSoft(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()
	// Intentionally NO ExpectQuery — any DB hit fails.

	src := newExpandSnapshotSourceFromDB(db, &fakeExpandGraphReader{}, nil, quietExpandSnapshotLogger())
	_, err = src.LatestForExpand(context.Background(), snapRepoID, "", "callees")
	if !errors.Is(err, agentapi.ErrNoExpandSnapshot) {
		t.Fatalf("err = %v; want ErrNoExpandSnapshot for empty node_id", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestNewExpandSnapshotSourceFromDB_nodeHydrationConnectionError
// proves a connection-class node-hydration failure
// propagates as `agentapi.ErrGraphStoreUnavailable` so the
// expand handler can emit the §C22 closed-set degraded
// signal. Without this wrap a transient DB blip during
// snapshot rehydrate would surface as a generic Internal
// at the caller.
func TestNewExpandSnapshotSourceFromDB_nodeHydrationConnectionError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectExpandSnapshotQuery(t, mock, snapRepoID, snapNodeID, "callees").
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "node_ids", "edge_ids",
		}).AddRow(
			snapCtxID,
			"{"+snapNodeID2+"}",
			"{}",
		))

	gr := &fakeExpandGraphReader{
		nodeErr: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
	}
	src := newExpandSnapshotSourceFromDB(db, gr, nil, quietExpandSnapshotLogger())
	_, err = src.LatestForExpand(context.Background(), snapRepoID, snapNodeID, "callees")
	if err == nil {
		t.Fatalf("err = nil; want graph-store-unavailable wrap")
	}
	if !errors.Is(err, agentapi.ErrGraphStoreUnavailable) {
		t.Fatalf("err = %v; want ErrGraphStoreUnavailable wrap", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestNewExpandSnapshotSourceFromDB_edgeHydrationConnectionError
// is the edge-side mirror — same §C22 contract. Pinned
// separately because the implementation has TWO loops
// (nodes then edges) and a regression that only touches
// the edge classifier would slip past the node-only test.
func TestNewExpandSnapshotSourceFromDB_edgeHydrationConnectionError(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectExpandSnapshotQuery(t, mock, snapRepoID, snapNodeID, "callees").
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "node_ids", "edge_ids",
		}).AddRow(
			snapCtxID,
			"{}",
			"{"+snapEdgeID+"}",
		))

	gr := &fakeExpandGraphReader{
		edgeErr: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")},
	}
	src := newExpandSnapshotSourceFromDB(db, gr, nil, quietExpandSnapshotLogger())
	_, err = src.LatestForExpand(context.Background(), snapRepoID, snapNodeID, "callees")
	if err == nil {
		t.Fatalf("err = nil; want graph-store-unavailable wrap on edge hydration")
	}
	if !errors.Is(err, agentapi.ErrGraphStoreUnavailable) {
		t.Fatalf("err = %v; want ErrGraphStoreUnavailable wrap", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestNewExpandSnapshotSourceFromDB_perIDNotFoundIsSoft
// proves the snapshot stays best-effort: when one of the
// referenced ids is retired (`graphreader.ErrNotFound`),
// we LOG + skip rather than fail the entire snapshot.
// Otherwise a single pruned row would make every degraded
// response empty.
func TestNewExpandSnapshotSourceFromDB_perIDNotFoundIsSoft(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectExpandSnapshotQuery(t, mock, snapRepoID, snapNodeID, "callees").
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "node_ids", "edge_ids",
		}).AddRow(
			snapCtxID,
			"{"+snapNodeID2+"}",
			"{"+snapEdgeID+","+snapEdgeID2+"}",
		))

	// Only edge #1 + node #1 exist in the fake; edge #2
	// returns ErrNotFound. The snapshot should contain
	// edge #1 but skip edge #2.
	gr := &fakeExpandGraphReader{
		nodes: map[string]graphreader.Node{
			snapNodeID2: {
				NodeID: snapNodeID2,
				RepoID: snapRepoID,
				Kind:   "method",
			},
		},
		edges: map[string]graphreader.Edge{
			snapEdgeID: {
				EdgeID:    snapEdgeID,
				RepoID:    snapRepoID,
				Kind:      "static_calls",
				SrcNodeID: snapNodeID,
				DstNodeID: snapNodeID2,
			},
			// snapEdgeID2 intentionally absent
		},
	}
	src := newExpandSnapshotSourceFromDB(db, gr, nil, quietExpandSnapshotLogger())
	snap, err := src.LatestForExpand(context.Background(), snapRepoID, snapNodeID, "callees")
	if err != nil {
		t.Fatalf("LatestForExpand: %v (per-id NotFound must be soft)", err)
	}
	if len(snap.Edges) != 1 || snap.Edges[0].EdgeID != snapEdgeID {
		t.Fatalf("Edges = %+v; want exactly the surviving edge", snap.Edges)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}

// TestNewExpandSnapshotSourceFromDB_emptyArraysHydrateCleanly
// proves the empty-array degenerate case: a snapshot row
// can exist with `{}` for both node_ids and edge_ids (e.g.
// a node with no outbound edges was previously expanded).
// The reader must surface this as a non-error snapshot
// with empty Nodes/Edges — NOT ErrNoExpandSnapshot — so
// the expand handler can still serve the ContextID echo.
func TestNewExpandSnapshotSourceFromDB_emptyArraysHydrateCleanly(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() { _ = db.Close() }()

	expectExpandSnapshotQuery(t, mock, snapRepoID, snapNodeID, "callees").
		WillReturnRows(sqlmock.NewRows([]string{
			"context_id", "node_ids", "edge_ids",
		}).AddRow(snapCtxID, "{}", "{}"))

	src := newExpandSnapshotSourceFromDB(db, &fakeExpandGraphReader{}, nil, quietExpandSnapshotLogger())
	snap, err := src.LatestForExpand(context.Background(), snapRepoID, snapNodeID, "callees")
	if err != nil {
		t.Fatalf("LatestForExpand: %v", err)
	}
	if snap.ContextID != snapCtxID {
		t.Fatalf("ContextID = %q; want %q", snap.ContextID, snapCtxID)
	}
	if len(snap.Nodes) != 0 {
		t.Fatalf("Nodes = %+v; want empty", snap.Nodes)
	}
	if len(snap.Edges) != 0 {
		t.Fatalf("Edges = %+v; want empty", snap.Edges)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unfulfilled expectations: %v", err)
	}
}
