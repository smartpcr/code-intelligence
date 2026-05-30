//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Spy writer — satisfies RepoCommitNodeEdgeWriter, records every call so the
// step assertions can interrogate counts, sequencing, and parent/child kinds.
// ---------------------------------------------------------------------------

type ancestrySpyWriter struct {
	mu  sync.Mutex
	seq int

	ensureRepoCalls   int
	ensureCommitCalls int
	insertNodeCalls   []ancestryNodeCall
	insertEdgeCalls   []ancestryEdgeCall

	// kindByNodeID lets the edge-by-kind-pair assertion resolve
	// the kind of each endpoint of an InsertEdge call. Populated
	// on every successful InsertNode.
	kindByNodeID map[string]string

	// repos / nodes / edges enforce idempotent dedupe so a
	// per-file replay behaves like the real graphwriter.
	repos map[string]graphwriter.RepoRecord
	nodes map[string]graphwriter.NodeRecord
	edges map[string]graphwriter.EdgeRecord

	nodeSeq int
	edgeSeq int
	repoSeq int
}

type ancestryNodeCall struct {
	seq    int
	kind   string
	nodeID string
}

type ancestryEdgeCall struct {
	seq           int
	kind          string
	srcNodeID     string
	dstNodeID     string
	srcKind       string
	dstKind       string
}

func newAncestrySpyWriter() *ancestrySpyWriter {
	return &ancestrySpyWriter{
		kindByNodeID: make(map[string]string),
		repos:        make(map[string]graphwriter.RepoRecord),
		nodes:        make(map[string]graphwriter.NodeRecord),
		edges:        make(map[string]graphwriter.EdgeRecord),
	}
}

func (s *ancestrySpyWriter) EnsureRepo(_ context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.ensureRepoCalls++
	if rec, ok := s.repos[in.URL]; ok {
		rec.Inserted = false
		return rec, nil
	}
	s.repoSeq++
	id := fingerprint.RepoID{}
	id[0] = byte(s.repoSeq)
	id[15] = 0x5A
	rec := graphwriter.RepoRecord{
		RepoID:   id.String(),
		ID:       id,
		Inserted: true,
	}
	s.repos[in.URL] = rec
	return rec, nil
}

func (s *ancestrySpyWriter) EnsureCommit(_ context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.ensureCommitCalls++
	return graphwriter.CommitRecord{
		RepoID:   in.RepoID.String(),
		SHA:      in.SHA,
		Inserted: true,
	}, nil
}

func (s *ancestrySpyWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	key := in.RepoID.String() + "|" + in.Kind + "|" + in.CanonicalSignature + "|" + in.FromSHA
	if rec, ok := s.nodes[key]; ok {
		// Idempotent replay must NOT be counted as a new call;
		// the scenarios assert on the count of distinct Node
		// emissions, not the number of times the writer was
		// hit. Replaying the same (kind, signature) tuple
		// would otherwise inflate "InsertNode with kind X is
		// called exactly N times".
		rec.Inserted = false
		return rec, nil
	}
	s.nodeSeq++
	nodeID := fmt.Sprintf("node-%04d", s.nodeSeq)
	rec := graphwriter.NodeRecord{
		NodeID:   nodeID,
		Inserted: true,
	}
	s.nodes[key] = rec
	s.kindByNodeID[nodeID] = in.Kind
	s.insertNodeCalls = append(s.insertNodeCalls, ancestryNodeCall{
		seq:    s.seq,
		kind:   in.Kind,
		nodeID: nodeID,
	})
	return rec, nil
}

func (s *ancestrySpyWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	key := in.RepoID.String() + "|" + in.Kind + "|" + in.SrcNodeID + "|" + in.DstNodeID + "|" + in.FromSHA
	if rec, ok := s.edges[key]; ok {
		rec.Inserted = false
		return rec, nil
	}
	s.edgeSeq++
	rec := graphwriter.EdgeRecord{
		EdgeID:   fmt.Sprintf("edge-%04d", s.edgeSeq),
		Inserted: true,
	}
	s.edges[key] = rec
	s.insertEdgeCalls = append(s.insertEdgeCalls, ancestryEdgeCall{
		seq:       s.seq,
		kind:      in.Kind,
		srcNodeID: in.SrcNodeID,
		dstNodeID: in.DstNodeID,
		srcKind:   s.kindByNodeID[in.SrcNodeID],
		dstKind:   s.kindByNodeID[in.DstNodeID],
	})
	return rec, nil
}

// currentSeq returns the monotonically-increasing call counter at
// the moment it is called. Used to take a snapshot of the writer
// state immediately after EnsureRepoAndCommit so subsequent
// assertions can verify "no per-file InsertNode happened before
// this point".
func (s *ancestrySpyWriter) currentSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

func (s *ancestrySpyWriter) nodeCountByKind(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.insertNodeCalls {
		if c.kind == kind {
			n++
		}
	}
	return n
}

func (s *ancestrySpyWriter) edgeCountByKindPair(edgeKind, srcKind, dstKind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.insertEdgeCalls {
		if e.kind == edgeKind && e.srcKind == srcKind && e.dstKind == dstKind {
			n++
		}
	}
	return n
}

func (s *ancestrySpyWriter) totalNodeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.insertNodeCalls)
}

// allNodesUpToSeqHaveKinds checks that every InsertNode call with
// seq <= cutoff has a kind in the allowed set. Used to enforce
// "EnsureRepoAndCommit completes before any EnsureFile call".
func (s *ancestrySpyWriter) allNodesUpToSeqHaveKinds(cutoff int, allowed map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.insertNodeCalls {
		if c.seq <= cutoff && !allowed[c.kind] {
			return fmt.Errorf(
				"InsertNode(kind=%q) at seq %d should be repo-level (cutoff %d)",
				c.kind, c.seq, cutoff,
			)
		}
	}
	return nil
}

func (s *ancestrySpyWriter) allNodesBeyondSeqHaveKinds(cutoff int, allowed map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.insertNodeCalls {
		if c.seq > cutoff && !allowed[c.kind] {
			return fmt.Errorf(
				"InsertNode(kind=%q) at seq %d occurred after repo-setup cutoff %d",
				c.kind, c.seq, cutoff,
			)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type ancestryWriterState struct {
	spy    *ancestrySpyWriter
	writer *repoindexer.AncestryWriter
	files  []string
	err    error

	seqAfterRepoSetup int
}

const (
	ancestryTestRepoURL = "https://example.test/ancestry-writer-e2e"
	ancestryTestSHA     = "abc123def456"
)

func (st *ancestryWriterState) fresh() {
	st.spy = newAncestrySpyWriter()
	st.writer = repoindexer.NewAncestryWriter(st.spy, ancestryTestRepoURL, ancestryTestSHA)
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (st *ancestryWriterState) aScanThatWalksFiles(n int) error {
	st.fresh()
	st.files = make([]string, n)
	for i := 0; i < n; i++ {
		st.files[i] = fmt.Sprintf("pkg/mod%d/file%d.go", i%10, i)
	}
	return nil
}

func (st *ancestryWriterState) filesAllUnder(n int, dir string) error {
	st.fresh()
	st.files = make([]string, n)
	for i := 0; i < n; i++ {
		st.files[i] = fmt.Sprintf("%sfile%d.go", dir, i)
	}
	return nil
}

func (st *ancestryWriterState) aWorkspaceOfFiles(n int) error {
	st.fresh()
	dirs := []string{"internal/foo/", "internal/bar/", "pkg/baz/"}
	st.files = make([]string, n)
	for i := 0; i < n; i++ {
		st.files[i] = fmt.Sprintf("%sfile%d.go", dirs[i%len(dirs)], i)
	}
	return nil
}

func (st *ancestryWriterState) aFreshAncestryWriter() error {
	st.fresh()
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

// theAncestryWriterRuns drives the scan workflow by calling the
// combined EnsureRepoAndCommit method, then EnsureFile per file.
// EnsureRepoAndCommit invokes EnsureRepo and EnsureCommit on the
// spy writer, which records the calls — genuine observation, not
// manual counter increments.
func (st *ancestryWriterState) theAncestryWriterRuns() error {
	ctx := context.Background()

	if _, err := st.writer.EnsureRepoAndCommit(ctx, "main", []string{"go"}); err != nil {
		return fmt.Errorf("EnsureRepoAndCommit: %w", err)
	}

	// Record seq point: everything up to here is repo-level
	// setup; everything after is EnsureFile-driven.
	st.seqAfterRepoSetup = st.spy.currentSeq()

	for _, f := range st.files {
		if _, err := st.writer.EnsureFile(ctx, repoindexer.WalkFile{RelPath: f}); err != nil {
			return fmt.Errorf("EnsureFile(%q): %w", f, err)
		}
	}
	return nil
}

func (st *ancestryWriterState) ensureFileRunsPerFile() error {
	return st.theAncestryWriterRuns()
}

func (st *ancestryWriterState) ensureFileRunsOncePerFile() error {
	return st.theAncestryWriterRuns()
}

func (st *ancestryWriterState) ensureFileCalledBeforeEnsureRepoAndCommit() error {
	ctx := context.Background()
	_, st.err = st.writer.EnsureFile(ctx, repoindexer.WalkFile{RelPath: "some/file.go"})
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (st *ancestryWriterState) ensureRepoCalledExactlyNTimes(n int) error {
	st.spy.mu.Lock()
	got := st.spy.ensureRepoCalls
	st.spy.mu.Unlock()
	if got != n {
		return fmt.Errorf("w.EnsureRepo invoked %d times, want %d", got, n)
	}
	return nil
}

func (st *ancestryWriterState) ensureCommitCalledExactlyNTimes(n int) error {
	st.spy.mu.Lock()
	got := st.spy.ensureCommitCalls
	st.spy.mu.Unlock()
	if got != n {
		return fmt.Errorf("w.EnsureCommit invoked %d times, want %d", got, n)
	}
	return nil
}

func (st *ancestryWriterState) insertNodeWithKindCalledExactlyNTimes(kind string, n int) error {
	got := st.spy.nodeCountByKind(kind)
	if got != n {
		return fmt.Errorf("InsertNode(kind=%q) called %d times, want %d", kind, got, n)
	}
	return nil
}

func (st *ancestryWriterState) ensureRepoAndCommitCompletesBeforeAnyEnsureFile() error {
	if st.seqAfterRepoSetup == 0 {
		return fmt.Errorf("seqAfterRepoSetup not recorded; was theAncestryWriterRuns called?")
	}

	repoOnly := map[string]bool{"repo": true}
	if err := st.spy.allNodesUpToSeqHaveKinds(st.seqAfterRepoSetup, repoOnly); err != nil {
		return fmt.Errorf("pre-EnsureFile check: %w", err)
	}

	postRepoKinds := map[string]bool{"package": true, "file": true}
	if err := st.spy.allNodesBeyondSeqHaveKinds(st.seqAfterRepoSetup, postRepoKinds); err != nil {
		return fmt.Errorf("post-EnsureFile check: %w", err)
	}

	return nil
}

func (st *ancestryWriterState) containsEdgeBetweenKindsInsertedNTimes(srcKind, dstKind string, n int) error {
	got := st.spy.edgeCountByKindPair("contains", srcKind, dstKind)
	if got != n {
		return fmt.Errorf(
			"%q->%q contains edges: got %d, want %d",
			srcKind, dstKind, got, n,
		)
	}
	return nil
}

func (st *ancestryWriterState) nFileNodesAreInserted(n int) error {
	return st.insertNodeWithKindCalledExactlyNTimes("file", n)
}

func (st *ancestryWriterState) nContainsEdgesAreInserted(n int, srcKind, dstKind string) error {
	return st.containsEdgeBetweenKindsInsertedNTimes(srcKind, dstKind, n)
}

func (st *ancestryWriterState) aNonNilErrorIsReturnedAncestry() error {
	if st.err == nil {
		return fmt.Errorf("expected non-nil error, got nil")
	}
	if !errors.Is(st.err, repoindexer.ErrAncestryNotReady) {
		return fmt.Errorf("expected ErrAncestryNotReady, got %v", st.err)
	}
	return nil
}

func (st *ancestryWriterState) noNodesAreInserted() error {
	got := st.spy.totalNodeCount()
	if got != 0 {
		return fmt.Errorf("expected 0 nodes inserted, got %d", got)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_identity_and_ancestry_refactor_ancestrywriter_factored_from_worker(ctx *godog.ScenarioContext) {
	st := &ancestryWriterState{}

	// Given
	ctx.Given(`^a scan that walks (\d+) files$`, st.aScanThatWalksFiles)
	ctx.Given(`^(\d+) files all under "([^"]*)"$`, st.filesAllUnder)
	ctx.Given(`^a workspace of (\d+) files$`, st.aWorkspaceOfFiles)
	ctx.Given(`^a fresh AncestryWriter$`, st.aFreshAncestryWriter)

	// When
	ctx.When(`^the ancestry writer runs$`, st.theAncestryWriterRuns)
	ctx.When(`^EnsureFile runs per file$`, st.ensureFileRunsPerFile)
	ctx.When(`^EnsureFile runs once per file$`, st.ensureFileRunsOncePerFile)
	ctx.When(`^EnsureFile is called before EnsureRepoAndCommit$`, st.ensureFileCalledBeforeEnsureRepoAndCommit)

	// Then
	ctx.Then(`^w\.EnsureRepo is called exactly (\d+) times?$`, st.ensureRepoCalledExactlyNTimes)
	ctx.Then(`^w\.EnsureCommit is called exactly (\d+) times?$`, st.ensureCommitCalledExactlyNTimes)
	ctx.Then(`^InsertNode with kind "([^"]*)" is called exactly (\d+) times?$`, st.insertNodeWithKindCalledExactlyNTimes)
	ctx.Then(`^EnsureRepoAndCommit completes before any EnsureFile call$`, st.ensureRepoAndCommitCompletesBeforeAnyEnsureFile)
	ctx.Then(`^the "([^"]*)" to "([^"]*)" contains edge is inserted exactly (\d+) times?$`, st.containsEdgeBetweenKindsInsertedNTimes)
	ctx.Then(`^(\d+) file nodes are inserted$`, st.nFileNodesAreInserted)
	ctx.Then(`^(\d+) "([^"]*)" to "([^"]*)" contains edges are inserted$`, st.nContainsEdgesAreInserted)
	ctx.Then(`^a non-nil error is returned$`, st.aNonNilErrorIsReturnedAncestry)
	ctx.Then(`^no nodes are inserted$`, st.noNodesAreInserted)
}

func TestE2E_identity_and_ancestry_refactor_ancestrywriter_factored_from_worker(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"identity_and_ancestry_refactor_ancestrywriter_factored_from_worker.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_identity_and_ancestry_refactor_ancestrywriter_factored_from_worker,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{featurePath},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
