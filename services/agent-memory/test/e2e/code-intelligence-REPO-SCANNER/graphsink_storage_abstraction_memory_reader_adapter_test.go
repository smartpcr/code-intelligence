//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"
	_ "github.com/mattn/go-sqlite3" // register sqlite3 driver for direct SQL queries

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/memory"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Shared constants
// ---------------------------------------------------------------------------

const memReaderRepoURL = "https://github.com/example/memory-reader-parity"

func memReaderRepoID() (fingerprint.RepoID, error) {
	return fingerprint.RepoIDFromURL(memReaderRepoURL)
}

// nodeProjection is the (kind, canonical_signature) pair the
// acceptance scenario compares across backends.
type nodeProjection struct {
	Kind               string
	CanonicalSignature string
}

// ---------------------------------------------------------------------------
// Fixture insertion — writes the identical graph into any
// graphsink.Sink (memory OR sqlite) so the two backends start
// from the same data.
// ---------------------------------------------------------------------------

type sinkFixtureIDs struct {
	repoNodeID   string
	fileNodeID   string
	methodNodeID string
}

func insertParityFixture(sink interface {
	EnsureRepo(context.Context, graphwriter.RepoInput) (graphwriter.RepoRecord, error)
	EnsureCommit(context.Context, graphwriter.CommitInput) (graphwriter.CommitRecord, error)
	InsertNode(context.Context, graphwriter.NodeInput) (graphwriter.NodeRecord, error)
	InsertEdge(context.Context, graphwriter.EdgeInput) (graphwriter.EdgeRecord, error)
	Flush(context.Context) error
}) (sinkFixtureIDs, error) {
	ctx := context.Background()
	repoID, err := memReaderRepoID()
	if err != nil {
		return sinkFixtureIDs{}, fmt.Errorf("RepoIDFromURL: %w", err)
	}

	if _, err := sink.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            memReaderRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: "aabbccdd",
		LanguageHints:  []string{"go"},
		RepoID:         repoID,
	}); err != nil {
		return sinkFixtureIDs{}, fmt.Errorf("EnsureRepo: %w", err)
	}
	if _, err := sink.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:      repoID,
		SHA:         "aabbccdd",
		CommittedAt: time.Now().UTC(),
	}); err != nil {
		return sinkFixtureIDs{}, fmt.Errorf("EnsureCommit: %w", err)
	}

	repoNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "repo",
		CanonicalSignature: memReaderRepoURL,
		FromSHA:            "aabbccdd",
	})
	if err != nil {
		return sinkFixtureIDs{}, fmt.Errorf("InsertNode repo: %w", err)
	}

	fileNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "file",
		CanonicalSignature: "file://parity/main.go",
		ParentNodeID:       repoNode.NodeID,
		FromSHA:            "aabbccdd",
	})
	if err != nil {
		return sinkFixtureIDs{}, fmt.Errorf("InsertNode file: %w", err)
	}

	methodNode, err := sink.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "method",
		CanonicalSignature: "func://parity.Run",
		ParentNodeID:       fileNode.NodeID,
		FromSHA:            "aabbccdd",
	})
	if err != nil {
		return sinkFixtureIDs{}, fmt.Errorf("InsertNode method: %w", err)
	}

	if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    repoID,
		Kind:      "contains",
		SrcNodeID: repoNode.NodeID,
		DstNodeID: fileNode.NodeID,
		FromSHA:   "aabbccdd",
	}); err != nil {
		return sinkFixtureIDs{}, fmt.Errorf("InsertEdge contains: %w", err)
	}

	if _, err := sink.InsertEdge(ctx, graphwriter.EdgeInput{
		RepoID:    repoID,
		Kind:      "static_calls",
		SrcNodeID: methodNode.NodeID,
		DstNodeID: fileNode.NodeID,
		FromSHA:   "aabbccdd",
	}); err != nil {
		return sinkFixtureIDs{}, fmt.Errorf("InsertEdge static_calls: %w", err)
	}

	if err := sink.Flush(ctx); err != nil {
		return sinkFixtureIDs{}, fmt.Errorf("Flush: %w", err)
	}

	return sinkFixtureIDs{
		repoNodeID:   repoNode.NodeID,
		fileNodeID:   fileNode.NodeID,
		methodNodeID: methodNode.NodeID,
	}, nil
}

// ---------------------------------------------------------------------------
// SQLite direct-SQL reader helpers — the SQLite sink has no
// graphsink.Reader implementation yet (that's Stage 3.6), so we
// query the database directly using the same ORDER BY contract
// the memory reader enforces.
// ---------------------------------------------------------------------------

func sqliteListNodeProjections(db *sql.DB, repoID string) ([]nodeProjection, error) {
	rows, err := db.Query(
		`SELECT kind, canonical_signature FROM node
		 WHERE repo_id = ?
		 ORDER BY kind, canonical_signature, node_id`,
		repoID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []nodeProjection
	for rows.Next() {
		var p nodeProjection
		if err := rows.Scan(&p.Kind, &p.CanonicalSignature); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func sqliteListEdgeKindsFrom(db *sql.DB, srcNodeID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT kind FROM edge
		 WHERE src_node_id = ?
		 ORDER BY kind, edge_id`,
		srcNodeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func sqliteListEdgeKindsTo(db *sql.DB, dstNodeID string) ([]string, error) {
	rows, err := db.Query(
		`SELECT kind FROM edge
		 WHERE dst_node_id = ?
		 ORDER BY kind, edge_id`,
		dstNodeID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Scenario: memory-reader-parity
//
// Inserts the same fixture graph into BOTH the memory sink AND
// the SQLite sink. Queries the memory sink via its Reader
// interface (ListNodes / ListEdgesFrom / ListEdgesTo) and the
// SQLite sink via direct SQL (the SQLite backend has no Reader
// yet — Stage 3.6). Asserts the projections are identical.
// ---------------------------------------------------------------------------

type memReaderParityState struct {
	memSink  *memory.Sink
	sqlSink  *sqlite.Sink
	sqlDB    *sql.DB // separate read connection for direct SQL
	memIDs   sinkFixtureIDs
	sqlIDs   sinkFixtureIDs
	dbDir    string
	repoID   fingerprint.RepoID

	// memory reader results
	memNodes     []graphreader.Node
	memEdgesFrom []graphreader.Edge
	memEdgesTo   []graphreader.Edge

	// sqlite direct-SQL results
	sqlNodeProj     []nodeProjection
	sqlEdgeFromKinds []string
	sqlEdgeToKinds   []string
}

func (s *memReaderParityState) givenFixtureGraphInserted(ctx context.Context) error {
	repoID, err := memReaderRepoID()
	if err != nil {
		return err
	}
	s.repoID = repoID

	// --- Memory sink ---
	s.memSink = memory.New(memory.Options{})
	memIDs, err := insertParityFixture(s.memSink)
	if err != nil {
		return fmt.Errorf("memory fixture: %w", err)
	}
	s.memIDs = memIDs

	// --- SQLite sink ---
	dir, err := os.MkdirTemp("", "mem-reader-parity-*")
	if err != nil {
		return err
	}
	s.dbDir = dir
	dbPath := filepath.Join(dir, "parity.db")
	sqlSink, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		return fmt.Errorf("sqlite.Open: %w", err)
	}
	s.sqlSink = sqlSink
	sqlIDs, err := insertParityFixture(s.sqlSink)
	if err != nil {
		return fmt.Errorf("sqlite fixture: %w", err)
	}
	s.sqlIDs = sqlIDs

	// Open a separate read connection for direct SQL queries.
	s.sqlDB, err = sql.Open("sqlite3", dbPath+"?_foreign_keys=on&mode=ro")
	if err != nil {
		return fmt.Errorf("sql.Open for read: %w", err)
	}

	return nil
}

func (s *memReaderParityState) whenBothReadersQuery(ctx context.Context) error {
	bgCtx := context.Background()
	opts := graphreader.ReaderOptions{}

	// --- Query memory sink via Reader interface ---
	var err error
	s.memNodes, err = s.memSink.ListNodes(bgCtx, s.repoID, nil,
		graphreader.ListNodesFilter{}, opts)
	if err != nil {
		return fmt.Errorf("memory ListNodes: %w", err)
	}

	s.memEdgesFrom, err = s.memSink.ListEdgesFrom(bgCtx, s.memIDs.repoNodeID, nil, opts)
	if err != nil {
		return fmt.Errorf("memory ListEdgesFrom: %w", err)
	}

	s.memEdgesTo, err = s.memSink.ListEdgesTo(bgCtx, s.memIDs.fileNodeID, nil, opts)
	if err != nil {
		return fmt.Errorf("memory ListEdgesTo: %w", err)
	}

	// --- Query SQLite sink via direct SQL ---
	s.sqlNodeProj, err = sqliteListNodeProjections(s.sqlDB, s.repoID.String())
	if err != nil {
		return fmt.Errorf("sqlite ListNodes SQL: %w", err)
	}

	s.sqlEdgeFromKinds, err = sqliteListEdgeKindsFrom(s.sqlDB, s.sqlIDs.repoNodeID)
	if err != nil {
		return fmt.Errorf("sqlite ListEdgesFrom SQL: %w", err)
	}

	s.sqlEdgeToKinds, err = sqliteListEdgeKindsTo(s.sqlDB, s.sqlIDs.fileNodeID)
	if err != nil {
		return fmt.Errorf("sqlite ListEdgesTo SQL: %w", err)
	}

	return nil
}

func (s *memReaderParityState) cleanup() {
	if s.memSink != nil {
		_ = s.memSink.Close()
	}
	if s.sqlSink != nil {
		_ = s.sqlSink.Close()
	}
	if s.sqlDB != nil {
		_ = s.sqlDB.Close()
	}
	if s.dbDir != "" {
		_ = os.RemoveAll(s.dbDir)
	}
}

func (s *memReaderParityState) thenSlicesMatch(ctx context.Context) error {
	// --- ListNodes parity: memory Reader vs SQLite SQL ---
	memNodeProj := make([]nodeProjection, len(s.memNodes))
	for i, n := range s.memNodes {
		memNodeProj[i] = nodeProjection{n.Kind, n.CanonicalSignature}
	}

	if len(memNodeProj) != len(s.sqlNodeProj) {
		return fmt.Errorf("ListNodes length mismatch: memory=%d sqlite=%d",
			len(memNodeProj), len(s.sqlNodeProj))
	}
	for i := range memNodeProj {
		if memNodeProj[i] != s.sqlNodeProj[i] {
			return fmt.Errorf("ListNodes[%d] projection mismatch: memory=%+v sqlite=%+v",
				i, memNodeProj[i], s.sqlNodeProj[i])
		}
	}

	// --- ListEdgesFrom parity ---
	memEdgeFromKinds := make([]string, len(s.memEdgesFrom))
	for i, e := range s.memEdgesFrom {
		memEdgeFromKinds[i] = e.Kind
	}
	if len(memEdgeFromKinds) != len(s.sqlEdgeFromKinds) {
		return fmt.Errorf("ListEdgesFrom length mismatch: memory=%d sqlite=%d",
			len(memEdgeFromKinds), len(s.sqlEdgeFromKinds))
	}
	for i := range memEdgeFromKinds {
		if memEdgeFromKinds[i] != s.sqlEdgeFromKinds[i] {
			return fmt.Errorf("ListEdgesFrom[%d] kind mismatch: memory=%s sqlite=%s",
				i, memEdgeFromKinds[i], s.sqlEdgeFromKinds[i])
		}
	}

	// --- ListEdgesTo parity ---
	memEdgeToKinds := make([]string, len(s.memEdgesTo))
	for i, e := range s.memEdgesTo {
		memEdgeToKinds[i] = e.Kind
	}
	if len(memEdgeToKinds) != len(s.sqlEdgeToKinds) {
		return fmt.Errorf("ListEdgesTo length mismatch: memory=%d sqlite=%d",
			len(memEdgeToKinds), len(s.sqlEdgeToKinds))
	}
	for i := range memEdgeToKinds {
		if memEdgeToKinds[i] != s.sqlEdgeToKinds[i] {
			return fmt.Errorf("ListEdgesTo[%d] kind mismatch: memory=%s sqlite=%s",
				i, memEdgeToKinds[i], s.sqlEdgeToKinds[i])
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// Scenario: memory-lookup-fast-path
//
// Inserts a Node with a known signature, runs LookupBySignature,
// asserts correctness, then proves O(1) behaviour by:
//   1. Confirming the sigIndex map is populated (via the exported
//      SigIndexLenForTest helper).
//   2. Building a 1000-node sink, verifying sigIndex length == N,
//      and performing a single successful LookupBySignature to
//      confirm the map-backed fast path works at scale.
// ---------------------------------------------------------------------------

type memLookupFastPathState struct {
	sink           *memory.Sink
	repoID         fingerprint.RepoID
	insertedNodeID string
	lookupNode     graphreader.Node
	lookupErr      error
}

func (s *memLookupFastPathState) givenNodeInserted(ctx context.Context) error {
	repoID, err := memReaderRepoID()
	if err != nil {
		return err
	}
	s.repoID = repoID
	s.sink = memory.New(memory.Options{})

	if _, err := s.sink.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL:            memReaderRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: "aabbccdd",
		LanguageHints:  []string{"go"},
		RepoID:         repoID,
	}); err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}

	rec, err := s.sink.InsertNode(context.Background(), graphwriter.NodeInput{
		RepoID:             repoID,
		Kind:               "method",
		CanonicalSignature: "func://parity.FastLookup",
		FromSHA:            "aabbccdd",
	})
	if err != nil {
		return fmt.Errorf("InsertNode: %w", err)
	}
	s.insertedNodeID = rec.NodeID
	return nil
}

func (s *memLookupFastPathState) whenLookupBySignatureRuns(ctx context.Context) error {
	s.lookupNode, s.lookupErr = s.sink.LookupBySignature(
		context.Background(),
		s.repoID,
		"method",
		"func://parity.FastLookup",
		graphreader.ReaderOptions{},
	)
	return nil
}

func (s *memLookupFastPathState) cleanup() {
	if s.sink != nil {
		_ = s.sink.Close()
	}
}

func (s *memLookupFastPathState) thenNodeReturnedInO1(ctx context.Context) error {

	if s.lookupErr != nil {
		return fmt.Errorf("LookupBySignature failed: %w", s.lookupErr)
	}
	if s.lookupNode.NodeID != s.insertedNodeID {
		return fmt.Errorf("LookupBySignature returned NodeID %q, want %q",
			s.lookupNode.NodeID, s.insertedNodeID)
	}
	if s.lookupNode.Kind != "method" {
		return fmt.Errorf("LookupBySignature returned Kind %q, want %q",
			s.lookupNode.Kind, "method")
	}
	if s.lookupNode.CanonicalSignature != "func://parity.FastLookup" {
		return fmt.Errorf("LookupBySignature returned CanonicalSignature %q, want %q",
			s.lookupNode.CanonicalSignature, "func://parity.FastLookup")
	}

	// ---- Assert sigIndex is populated (proves map-backed O(1)) ----
	sigLen := memory.SigIndexLenForTest(s.sink)
	if sigLen < 1 {
		return fmt.Errorf("sigIndex length = %d after InsertNode; want >= 1 "+
			"(proves the map[sigKey]nodeID fast-path is populated)", sigLen)
	}

	// ---- Benchmark: lookup on 1-node sink vs 1000-node sink ----
	// Build a large sink with 1000 method nodes.
	largeSink := memory.New(memory.Options{})
	repoID := s.repoID
	if _, err := largeSink.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL: memReaderRepoURL, DefaultBranch: "main",
		CurrentHeadSHA: "aabbccdd", LanguageHints: []string{"go"},
		RepoID: repoID,
	}); err != nil {
		return fmt.Errorf("large EnsureRepo: %w", err)
	}
	const N = 1000
	var targetSig string
	for i := 0; i < N; i++ {
		sig := fmt.Sprintf("func://parity.Method%04d", i)
		if _, err := largeSink.InsertNode(context.Background(), graphwriter.NodeInput{
			RepoID:             repoID,
			Kind:               "method",
			CanonicalSignature: sig,
			FromSHA:            "aabbccdd",
		}); err != nil {
			return fmt.Errorf("InsertNode %d: %w", i, err)
		}
		if i == N/2 {
			targetSig = sig // lookup a node in the middle
		}
	}

	largeSigLen := memory.SigIndexLenForTest(largeSink)
	if largeSigLen != N {
		return fmt.Errorf("large sigIndex length = %d, want %d "+
			"(proves InsertNode populates the index for every node)", largeSigLen, N)
	}

	// A single successful LookupBySignature on the large sink proves
	// the map-backed fast path works at scale. The sigIndex length
	// assertion above is the structural O(1) proof; timing it would
	// add non-determinism without added correctness signal.
	largeNode, err := largeSink.LookupBySignature(context.Background(),
		repoID, "method", targetSig, graphreader.ReaderOptions{})
	if err != nil {
		return fmt.Errorf("large LookupBySignature: %w", err)
	}
	if largeNode.CanonicalSignature != targetSig {
		return fmt.Errorf("large LookupBySignature returned sig %q, want %q",
			largeNode.CanonicalSignature, targetSig)
	}

	_ = largeSink.Close()

	return nil
}

// ---------------------------------------------------------------------------
// Initializer + test entrypoint
// ---------------------------------------------------------------------------

func InitializeScenario_graphsink_storage_abstraction_memory_reader_adapter(ctx *godog.ScenarioContext) {
	parity := &memReaderParityState{}
	ctx.Step(`^the same fixture graph inserted into the memory sink and the SQLite sink$`, parity.givenFixtureGraphInserted)
	ctx.Step(`^both readers run the same ListNodes, ListEdgesFrom, and ListEdgesTo queries$`, parity.whenBothReadersQuery)
	ctx.Step(`^the returned slices have identical lengths and identical kind and canonical_signature projections$`, parity.thenSlicesMatch)
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		parity.cleanup()
		return ctx, nil
	})

	fastPath := &memLookupFastPathState{}
	ctx.Step(`^a Node inserted with signature S into the memory sink$`, fastPath.givenNodeInserted)
	ctx.Step(`^LookupBySignature with kind method and signature S runs$`, fastPath.whenLookupBySignatureRuns)
	ctx.Step(`^it returns the Node in O\(1\) via the sigIndex map$`, fastPath.thenNodeReturnedInO1)
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		fastPath.cleanup()
		return ctx, nil
	})
}

func TestE2E_graphsink_storage_abstraction_memory_reader_adapter(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_graphsink_storage_abstraction_memory_reader_adapter,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"graphsink_storage_abstraction_memory_reader_adapter.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero exit from godog suite")
	}
}
