//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
)

// ---------------------------------------------------------------------------
// Spy sink — records InsertNode / InsertEdge calls for assertion
// ---------------------------------------------------------------------------

type ancestrySpySink struct {
	mu       sync.Mutex
	seq      int
	nodes    []ancestryNodeCall
	edges    []ancestryEdgeCall
	kindByFP map[string]string
}

type ancestryNodeCall struct {
	seq   int
	kind  string
	fpHex string
}

type ancestryEdgeCall struct {
	seq   int
	kind  string
	srcFP string
	dstFP string
}

func newAncestrySpySink() *ancestrySpySink {
	return &ancestrySpySink{kindByFP: make(map[string]string)}
}

func (s *ancestrySpySink) InsertNode(ctx context.Context, n *repoindexer.Node) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	fpHex := n.Fingerprint.Hex()
	s.nodes = append(s.nodes, ancestryNodeCall{
		seq:   s.seq,
		kind:  n.Kind,
		fpHex: fpHex,
	})
	s.kindByFP[fpHex] = n.Kind
	return nil
}

func (s *ancestrySpySink) InsertEdge(ctx context.Context, e *repoindexer.Edge) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	s.edges = append(s.edges, ancestryEdgeCall{
		seq:   s.seq,
		kind:  e.Kind,
		srcFP: e.SrcFingerprint.Hex(),
		dstFP: e.DstFingerprint.Hex(),
	})
	return nil
}

func (s *ancestrySpySink) currentSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

func (s *ancestrySpySink) nodeCountByKind(kind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.nodes {
		if c.kind == kind {
			n++
		}
	}
	return n
}

func (s *ancestrySpySink) edgeCountByKindPair(edgeKind, srcKind, dstKind string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, e := range s.edges {
		if e.kind != edgeKind {
			continue
		}
		if s.kindByFP[e.srcFP] == srcKind && s.kindByFP[e.dstFP] == dstKind {
			n++
		}
	}
	return n
}

func (s *ancestrySpySink) totalNodeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.nodes)
}

// allNodesUpToSeqHaveKinds checks that every InsertNode call with
// seq <= cutoff has a kind in the allowed set.
func (s *ancestrySpySink) allNodesUpToSeqHaveKinds(cutoff int, allowed map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.nodes {
		if c.seq <= cutoff && !allowed[c.kind] {
			return fmt.Errorf(
				"InsertNode(kind=%q) at seq %d should be repo-level (cutoff %d)",
				c.kind, c.seq, cutoff,
			)
		}
	}
	return nil
}

// allNodesBeyondSeqHaveKinds checks that every InsertNode call with
// seq > cutoff has a kind in the allowed set.
func (s *ancestrySpySink) allNodesBeyondSeqHaveKinds(cutoff int, allowed map[string]bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.nodes {
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
	spy    *ancestrySpySink
	writer *repoindexer.AncestryWriter
	files  []string
	err    error

	seqAfterRepoSetup int
}

const (
	ancestryTestRepoURL = "https://example.test/ancestry-writer-e2e"
	ancestryTestSHA     = "abc123def456"
)

// countMethodCalls returns how many times the named method was invoked
// by the AncestryWriter implementation. The MethodCalls slice is
// populated by the real EnsureRepo and EnsureCommit methods, not by
// the test code — this is genuine observation, not manual counting.
func (st *ancestryWriterState) countMethodCalls(method string) int {
	n := 0
	for _, m := range st.writer.MethodCalls {
		if m == method {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (st *ancestryWriterState) aScanThatWalksFiles(n int) error {
	st.spy = newAncestrySpySink()
	var err error
	st.writer, err = repoindexer.NewAncestryWriter(st.spy, ancestryTestRepoURL, ancestryTestSHA)
	if err != nil {
		return fmt.Errorf("NewAncestryWriter: %w", err)
	}
	st.files = make([]string, n)
	for i := 0; i < n; i++ {
		st.files[i] = fmt.Sprintf("pkg/mod%d/file%d.go", i%10, i)
	}
	return nil
}

func (st *ancestryWriterState) filesAllUnder(n int, dir string) error {
	st.spy = newAncestrySpySink()
	var err error
	st.writer, err = repoindexer.NewAncestryWriter(st.spy, ancestryTestRepoURL, ancestryTestSHA)
	if err != nil {
		return fmt.Errorf("NewAncestryWriter: %w", err)
	}
	st.files = make([]string, n)
	for i := 0; i < n; i++ {
		st.files[i] = fmt.Sprintf("%sfile%d.go", dir, i)
	}
	return nil
}

func (st *ancestryWriterState) aWorkspaceOfFiles(n int) error {
	st.spy = newAncestrySpySink()
	var err error
	st.writer, err = repoindexer.NewAncestryWriter(st.spy, ancestryTestRepoURL, ancestryTestSHA)
	if err != nil {
		return fmt.Errorf("NewAncestryWriter: %w", err)
	}
	dirs := []string{"internal/foo/", "internal/bar/", "pkg/baz/"}
	st.files = make([]string, n)
	for i := 0; i < n; i++ {
		st.files[i] = fmt.Sprintf("%sfile%d.go", dirs[i%len(dirs)], i)
	}
	return nil
}

func (st *ancestryWriterState) aFreshAncestryWriter() error {
	st.spy = newAncestrySpySink()
	var err error
	st.writer, err = repoindexer.NewAncestryWriter(st.spy, ancestryTestRepoURL, ancestryTestSHA)
	if err != nil {
		return fmt.Errorf("NewAncestryWriter: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

// theAncestryWriterRuns drives the scan workflow by calling the
// combined EnsureRepoAndCommit method, then EnsureFile per file.
// EnsureRepoAndCommit internally calls EnsureRepo and EnsureCommit,
// which record themselves in writer.MethodCalls — a genuine
// observation mechanism, not manual counter increments.
func (st *ancestryWriterState) theAncestryWriterRuns() error {
	ctx := context.Background()

	// Call the combined method — exercises EnsureRepoAndCommit.
	if err := st.writer.EnsureRepoAndCommit(ctx); err != nil {
		return fmt.Errorf("EnsureRepoAndCommit: %w", err)
	}

	// Record seq point: everything through here is repo-level setup;
	// everything after is EnsureFile-driven.
	st.seqAfterRepoSetup = st.spy.currentSeq()

	// EnsureFile for each file in the workspace.
	for _, f := range st.files {
		if err := st.writer.EnsureFile(ctx, f); err != nil {
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
	st.err = st.writer.EnsureFile(ctx, "some/file.go")
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (st *ancestryWriterState) ensureRepoCalledExactlyNTimes(n int) error {
	got := st.countMethodCalls("EnsureRepo")
	if got != n {
		return fmt.Errorf("w.EnsureRepo invoked %d times, want %d", got, n)
	}
	return nil
}

func (st *ancestryWriterState) ensureCommitCalledExactlyNTimes(n int) error {
	got := st.countMethodCalls("EnsureCommit")
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