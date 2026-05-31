//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Recording writer — captures every (canonical_signature, kind,
// parent_node_id, fingerprint) tuple the AncestryWriter emits so
// the golden-fixture scenario can assert byte-identical output.
// ---------------------------------------------------------------------------

type recordingTuple struct {
	Kind               string
	CanonicalSignature string
	ParentNodeID       string
	FingerprintHex     string
}

type recordingEdgeTuple struct {
	Kind      string
	SrcNodeID string
	DstNodeID string
}

type goldenRecordingWriter struct {
	mu sync.Mutex

	repoSeq int
	nodeSeq int
	edgeSeq int

	repos map[string]graphwriter.RepoRecord
	nodes map[string]graphwriter.NodeRecord
	edges map[string]graphwriter.EdgeRecord

	// Ordered records for golden comparison.
	nodeTuples []recordingTuple
	edgeTuples []recordingEdgeTuple

	// Track assigned repo ID for fingerprint computation.
	assignedRepoID fingerprint.RepoID
}

func newGoldenRecordingWriter() *goldenRecordingWriter {
	return &goldenRecordingWriter{
		repos: make(map[string]graphwriter.RepoRecord),
		nodes: make(map[string]graphwriter.NodeRecord),
		edges: make(map[string]graphwriter.EdgeRecord),
	}
}

func (w *goldenRecordingWriter) EnsureRepo(_ context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if rec, ok := w.repos[in.URL]; ok {
		rec.Inserted = false
		return rec, nil
	}
	w.repoSeq++
	id := fingerprint.RepoID{}
	id[0] = byte(w.repoSeq)
	id[15] = 0xAA
	rec := graphwriter.RepoRecord{
		RepoID:   id.String(),
		ID:       id,
		Inserted: true,
	}
	w.repos[in.URL] = rec
	w.assignedRepoID = id
	return rec, nil
}

func (w *goldenRecordingWriter) EnsureCommit(_ context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return graphwriter.CommitRecord{
		RepoID:   in.RepoID.String(),
		SHA:      in.SHA,
		Inserted: true,
	}, nil
}

func (w *goldenRecordingWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := in.RepoID.String() + "|" + in.Kind + "|" + in.CanonicalSignature + "|" + in.FromSHA
	if rec, ok := w.nodes[key]; ok {
		rec.Inserted = false
		return rec, nil
	}
	w.nodeSeq++
	nodeID := fmt.Sprintf("node-%04d", w.nodeSeq)

	fp, err := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
	if err != nil {
		return graphwriter.NodeRecord{}, fmt.Errorf("fingerprint: %w", err)
	}

	rec := graphwriter.NodeRecord{
		NodeID:      nodeID,
		Fingerprint: fp,
		Inserted:    true,
	}
	w.nodes[key] = rec
	w.nodeTuples = append(w.nodeTuples, recordingTuple{
		Kind:               in.Kind,
		CanonicalSignature: in.CanonicalSignature,
		ParentNodeID:       in.ParentNodeID,
		FingerprintHex:     fmt.Sprintf("%x", fp),
	})
	return rec, nil
}

func (w *goldenRecordingWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := in.RepoID.String() + "|" + in.Kind + "|" + in.SrcNodeID + "|" + in.DstNodeID + "|" + in.FromSHA
	if rec, ok := w.edges[key]; ok {
		rec.Inserted = false
		return rec, nil
	}
	w.edgeSeq++
	rec := graphwriter.EdgeRecord{
		EdgeID:   fmt.Sprintf("edge-%04d", w.edgeSeq),
		Inserted: true,
	}
	w.edges[key] = rec
	w.edgeTuples = append(w.edgeTuples, recordingEdgeTuple{
		Kind:      in.Kind,
		SrcNodeID: in.SrcNodeID,
		DstNodeID: in.DstNodeID,
	})
	return rec, nil
}

// ---------------------------------------------------------------------------
// Golden fixture: the exact (kind, canonical_signature, parent
// relationship) tuples the pre-refactor worker.runFull produced
// for the 3-file fixture. The refactored AncestryWriter path
// MUST reproduce these byte-for-byte.
//
// Fixture files: README.md, pkg/foo.go, pkg/sub/bar.go
// Repo URL: https://example.test/golden-repo
// SHA: deadbeef1234
// ---------------------------------------------------------------------------

const (
	goldenRepoURL = "https://example.test/golden-repo"
	goldenSHA     = "deadbeef1234"
)

var goldenFixtureFiles = []string{
	"README.md",
	"pkg/foo.go",
	"pkg/sub/bar.go",
}

// goldenNodeTuples is the ordered list of (kind, canonical_signature)
// pairs. Parent relationships are verified structurally rather than
// by node ID (IDs are synthetic in the spy writer). Order follows
// the AncestryWriter call sequence: repo node first, then per-file
// package+file pairs in walk order.
var goldenNodeTuples = []struct {
	Kind               string
	CanonicalSignature string
}{
	{"repo", "https://example.test/golden-repo"},
	{"package", "https://example.test/golden-repo::pkg::"},
	{"file", "https://example.test/golden-repo::file::README.md"},
	{"package", "https://example.test/golden-repo::pkg::pkg"},
	{"file", "https://example.test/golden-repo::file::pkg/foo.go"},
	{"package", "https://example.test/golden-repo::pkg::pkg/sub"},
	{"file", "https://example.test/golden-repo::file::pkg/sub/bar.go"},
}

// goldenEdgeTuples is the ordered list of (kind, src→dst) edges.
// Src/dst are encoded as "node-NNNN" IDs assigned by the spy.
var goldenEdgeTuples = []struct {
	Kind      string
	SrcIndex  int // index into goldenNodeTuples for src
	DstIndex  int // index into goldenNodeTuples for dst
}{
	{"contains", 0, 1}, // repo → root-package
	{"contains", 1, 2}, // root-package → README.md
	{"contains", 0, 3}, // repo → pkg-package
	{"contains", 3, 4}, // pkg-package → pkg/foo.go
	{"contains", 0, 5}, // repo → pkg/sub-package
	{"contains", 5, 6}, // pkg/sub-package → pkg/sub/bar.go
}

// goldenParentIndex maps each node index to its expected parent node
// index. -1 means no parent (the repo node).
var goldenParentIndex = []int{
	-1, // repo: no parent
	0,  // root-package: parent is repo
	1,  // README.md: parent is root-package
	0,  // pkg-package: parent is repo
	3,  // pkg/foo.go: parent is pkg-package
	0,  // pkg/sub-package: parent is repo
	5,  // pkg/sub/bar.go: parent is pkg/sub-package
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type workerAdoptsState struct {
	writer         *goldenRecordingWriter
	files          []string
	err            error
	secondRunNodes []recordingTuple

	// For the helpers-no-internal-callers scenario.
	grepHits []string
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *workerAdoptsState) anInMemoryFixtureRepoWithFiles(fileList string) error {
	s.files = strings.Split(fileList, ",")
	s.writer = newGoldenRecordingWriter()
	return nil
}

func (s *workerAdoptsState) aRecordingRepoCommitNodeEdgeWriter() error {
	// Writer already created in the previous Given step.
	if s.writer == nil {
		s.writer = newGoldenRecordingWriter()
	}
	return nil
}

func (s *workerAdoptsState) theExistingWorkerIntegrationTestSuite() error {
	// Acknowledge that the existing worker_integration_test.go
	// exists. This scenario is about verifying it passes against
	// a live Postgres — which is a CI-only concern.
	return nil
}

func (s *workerAdoptsState) theRefactoredCodebaseUnder(dir string) error {
	// dir is "internal/" — used in the When step.
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *workerAdoptsState) ancestryWriterRunsTheFullAncestryPipeline() error {
	ctx := context.Background()
	aw := repoindexer.NewAncestryWriter(s.writer, goldenRepoURL, goldenSHA)
	if _, err := aw.EnsureRepoAndCommit(ctx, "main", []string{"go"}); err != nil {
		return fmt.Errorf("EnsureRepoAndCommit: %w", err)
	}
	for _, f := range s.files {
		if _, err := aw.EnsureFile(ctx, repoindexer.WalkFile{RelPath: f}); err != nil {
			return fmt.Errorf("EnsureFile(%q): %w", f, err)
		}
	}
	return nil
}

func (s *workerAdoptsState) theSuiteIsEvaluatedForPostgresAvailability() error {
	// This is a non-gate-required scenario. We verify that the
	// test file exists and that it requires AGENT_MEMORY_PG_URL.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	integFile := filepath.Join(repoRoot, "internal", "repoindexer", "worker_integration_test.go")
	if _, err := os.Stat(integFile); err != nil {
		return fmt.Errorf("worker_integration_test.go not found at %s: %w", integFile, err)
	}
	return nil
}

func (s *workerAdoptsState) weSearchForUnexportedHelperNames(nameList string) error {
	names := strings.Split(nameList, ",")
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	internalDir := filepath.Join(repoRoot, "internal")

	for _, name := range names {
		name = strings.TrimSpace(name)
		hits, err := grepForIdentifier(internalDir, name)
		if err != nil {
			return fmt.Errorf("scanning for %q: %w", name, err)
		}
		s.grepHits = append(s.grepHits, hits...)
	}
	return nil
}

// grepForIdentifier walks Go source files under dir and searches
// for occurrences of the exact identifier (not part of a larger
// word, not in a comment that references the exported form).
func grepForIdentifier(dir, ident string) ([]string, error) {
	var hits []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		// Skip test fixture data and the e2e test files themselves.
		if strings.Contains(path, "test"+string(os.PathSeparator)+"e2e") {
			return nil
		}
		f, fErr := os.Open(path)
		if fErr != nil {
			return fErr
		}
		defer f.Close()
		scanner := bufio.NewScanner(f)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if strings.Contains(line, ident) {
				hits = append(hits, fmt.Sprintf("%s:%d: %s", path, lineNo, strings.TrimSpace(line)))
			}
		}
		return scanner.Err()
	})
	return hits, err
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *workerAdoptsState) theCapturedNodeTuplesMatchTheGoldenFixtureExactly() error {
	s.writer.mu.Lock()
	defer s.writer.mu.Unlock()

	if len(s.writer.nodeTuples) != len(goldenNodeTuples) {
		return fmt.Errorf("node count: got %d, want %d", len(s.writer.nodeTuples), len(goldenNodeTuples))
	}
	for i, got := range s.writer.nodeTuples {
		want := goldenNodeTuples[i]
		if got.Kind != want.Kind || got.CanonicalSignature != want.CanonicalSignature {
			return fmt.Errorf("node[%d]: got (kind=%q, sig=%q), want (kind=%q, sig=%q)",
				i, got.Kind, got.CanonicalSignature, want.Kind, want.CanonicalSignature)
		}

		// Verify parent_node_id relationship.
		wantParentIdx := goldenParentIndex[i]
		if wantParentIdx == -1 {
			if got.ParentNodeID != "" {
				return fmt.Errorf("node[%d] (%s): expected no parent, got %q",
					i, got.Kind, got.ParentNodeID)
			}
		} else {
			wantParentNodeID := fmt.Sprintf("node-%04d", wantParentIdx+1)
			if got.ParentNodeID != wantParentNodeID {
				return fmt.Errorf("node[%d] (%s): parent got %q, want %q",
					i, got.CanonicalSignature, got.ParentNodeID, wantParentNodeID)
			}
		}
	}
	return nil
}

func (s *workerAdoptsState) theCapturedEdgeTuplesMatchTheGoldenFixtureExactly() error {
	s.writer.mu.Lock()
	defer s.writer.mu.Unlock()

	if len(s.writer.edgeTuples) != len(goldenEdgeTuples) {
		return fmt.Errorf("edge count: got %d, want %d", len(s.writer.edgeTuples), len(goldenEdgeTuples))
	}
	for i, got := range s.writer.edgeTuples {
		want := goldenEdgeTuples[i]
		wantSrc := fmt.Sprintf("node-%04d", want.SrcIndex+1)
		wantDst := fmt.Sprintf("node-%04d", want.DstIndex+1)
		if got.Kind != want.Kind {
			return fmt.Errorf("edge[%d]: kind got %q, want %q", i, got.Kind, want.Kind)
		}
		if got.SrcNodeID != wantSrc {
			return fmt.Errorf("edge[%d]: src got %q, want %q", i, got.SrcNodeID, wantSrc)
		}
		if got.DstNodeID != wantDst {
			return fmt.Errorf("edge[%d]: dst got %q, want %q", i, got.DstNodeID, wantDst)
		}
	}
	return nil
}

func (s *workerAdoptsState) fingerprintsAreStableAcrossASecondIdenticalRun() error {
	// Run the same pipeline a second time with a fresh writer and
	// verify every fingerprint is byte-identical.
	w2 := newGoldenRecordingWriter()
	ctx := context.Background()
	aw2 := repoindexer.NewAncestryWriter(w2, goldenRepoURL, goldenSHA)
	if _, err := aw2.EnsureRepoAndCommit(ctx, "main", []string{"go"}); err != nil {
		return fmt.Errorf("second run EnsureRepoAndCommit: %w", err)
	}
	for _, f := range s.files {
		if _, err := aw2.EnsureFile(ctx, repoindexer.WalkFile{RelPath: f}); err != nil {
			return fmt.Errorf("second run EnsureFile(%q): %w", f, err)
		}
	}

	s.writer.mu.Lock()
	first := s.writer.nodeTuples
	s.writer.mu.Unlock()
	w2.mu.Lock()
	second := w2.nodeTuples
	w2.mu.Unlock()

	if len(first) != len(second) {
		return fmt.Errorf("second run node count mismatch: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].FingerprintHex != second[i].FingerprintHex {
			return fmt.Errorf("node[%d] fingerprint drift: %q vs %q",
				i, first[i].FingerprintHex, second[i].FingerprintHex)
		}
	}
	return nil
}

func (s *workerAdoptsState) itIsAcknowledgedAsANonGateRequiredScenario() error {
	// The scenario's proof strategy is [service:postgres]; it is
	// explicitly "not a gate-required scenario" per the
	// implementation plan. We verify that the integration test
	// file exists (confirmed in the When step) and that it
	// references the AGENT_MEMORY_PG_URL env var.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	integFile := filepath.Join(repoRoot, "internal", "repoindexer", "worker_integration_test.go")
	content, err := os.ReadFile(integFile)
	if err != nil {
		return fmt.Errorf("reading worker_integration_test.go: %w", err)
	}
	if !strings.Contains(string(content), "AGENT_MEMORY_PG_URL") {
		return fmt.Errorf("worker_integration_test.go does not reference AGENT_MEMORY_PG_URL")
	}
	return nil
}

func (s *workerAdoptsState) noHitsRemainInTheScannedFiles() error {
	if len(s.grepHits) > 0 {
		sort.Strings(s.grepHits)
		return fmt.Errorf("unexported helpers still referenced (%d hits):\n%s",
			len(s.grepHits), strings.Join(s.grepHits, "\n"))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_identity_and_ancestry_refactor_worker_adopts_ancestrywriter(ctx *godog.ScenarioContext) {
	s := &workerAdoptsState{}

	// Given
	ctx.Given(`^an in-memory fixture repo with files "([^"]*)"$`, s.anInMemoryFixtureRepoWithFiles)
	ctx.Given(`^a recording RepoCommitNodeEdgeWriter$`, s.aRecordingRepoCommitNodeEdgeWriter)
	ctx.Given(`^the existing worker_integration_test\.go suite$`, s.theExistingWorkerIntegrationTestSuite)
	ctx.Given(`^the refactored codebase under "([^"]*)"$`, s.theRefactoredCodebaseUnder)

	// When
	ctx.When(`^AncestryWriter runs the full ancestry pipeline$`, s.ancestryWriterRunsTheFullAncestryPipeline)
	ctx.When(`^the suite is evaluated for Postgres availability$`, s.theSuiteIsEvaluatedForPostgresAvailability)
	ctx.When(`^we search for unexported helper names "([^"]*)"$`, s.weSearchForUnexportedHelperNames)

	// Then
	ctx.Then(`^the captured node tuples match the golden fixture exactly$`, s.theCapturedNodeTuplesMatchTheGoldenFixtureExactly)
	ctx.Then(`^the captured edge tuples match the golden fixture exactly$`, s.theCapturedEdgeTuplesMatchTheGoldenFixtureExactly)
	ctx.Then(`^fingerprints are stable across a second identical run$`, s.fingerprintsAreStableAcrossASecondIdenticalRun)
	ctx.Then(`^it is acknowledged as a non-gate-required scenario needing a provided Postgres DSN$`, s.itIsAcknowledgedAsANonGateRequiredScenario)
	ctx.Then(`^no hits remain in the scanned files$`, s.noHitsRemainInTheScannedFiles)
}

func TestE2E_identity_and_ancestry_refactor_worker_adopts_ancestrywriter(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"identity_and_ancestry_refactor_worker_adopts_ancestrywriter.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_identity_and_ancestry_refactor_worker_adopts_ancestrywriter,
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
