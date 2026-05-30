//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// moduleRoot returns the services/agent-memory directory (the Go module root).
// ---------------------------------------------------------------------------

func moduleRootWorkerAdopts() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}

// ---------------------------------------------------------------------------
// workerAdoptsSpyWriter — records InsertNode/InsertEdge calls so the step
// assertions can compare pre-refactor inline output vs post-refactor
// AncestryWriter output.
// ---------------------------------------------------------------------------

type workerAdoptsSpyWriter struct {
	mu sync.Mutex

	ensureRepoCalls   int
	ensureCommitCalls int
	nodes             []workerAdoptsNodeRecord
	edges             []workerAdoptsEdgeRecord

	nodesByKey map[string]graphwriter.NodeRecord
	edgesByKey map[string]graphwriter.EdgeRecord
	nodeSeq    int
	edgeSeq    int
	repoSeq    int
	repos      map[string]graphwriter.RepoRecord
}

type workerAdoptsNodeRecord struct {
	kind               string
	canonicalSignature string
	parentNodeID       string
	nodeID             string
	fingerprint        string
}

type workerAdoptsEdgeRecord struct {
	kind      string
	srcNodeID string
	dstNodeID string
	edgeID    string
}

func newWorkerAdoptsSpyWriter() *workerAdoptsSpyWriter {
	return &workerAdoptsSpyWriter{
		nodesByKey: make(map[string]graphwriter.NodeRecord),
		edgesByKey: make(map[string]graphwriter.EdgeRecord),
		repos:      make(map[string]graphwriter.RepoRecord),
	}
}

func (s *workerAdoptsSpyWriter) EnsureRepo(_ context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
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

func (s *workerAdoptsSpyWriter) EnsureCommit(_ context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ensureCommitCalls++
	return graphwriter.CommitRecord{
		RepoID:   in.RepoID.String(),
		SHA:      in.SHA,
		Inserted: true,
	}, nil
}

func (s *workerAdoptsSpyWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := in.RepoID.String() + "|" + in.Kind + "|" + in.CanonicalSignature + "|" + in.FromSHA
	if rec, ok := s.nodesByKey[key]; ok {
		rec.Inserted = false
		return rec, nil
	}
	s.nodeSeq++
	nodeID := fmt.Sprintf("node-%04d", s.nodeSeq)
	rec := graphwriter.NodeRecord{
		NodeID:   nodeID,
		Inserted: true,
	}
	s.nodesByKey[key] = rec
	s.nodes = append(s.nodes, workerAdoptsNodeRecord{
		kind:               in.Kind,
		canonicalSignature: in.CanonicalSignature,
		parentNodeID:       in.ParentNodeID,
		nodeID:             nodeID,
		fingerprint:        key,
	})
	return rec, nil
}

func (s *workerAdoptsSpyWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := in.RepoID.String() + "|" + in.Kind + "|" + in.SrcNodeID + "|" + in.DstNodeID + "|" + in.FromSHA
	if rec, ok := s.edgesByKey[key]; ok {
		rec.Inserted = false
		return rec, nil
	}
	s.edgeSeq++
	rec := graphwriter.EdgeRecord{
		EdgeID:   fmt.Sprintf("edge-%04d", s.edgeSeq),
		Inserted: true,
	}
	s.edgesByKey[key] = rec
	s.edges = append(s.edges, workerAdoptsEdgeRecord{
		kind:      in.Kind,
		srcNodeID: in.SrcNodeID,
		dstNodeID: in.DstNodeID,
		edgeID:    rec.EdgeID,
	})
	return rec, nil
}

// ---------------------------------------------------------------------------
// fullModeAttrs mirrors repoindexer.fullModeAttrs (unexported) so the
// pre-refactor inline path produces byte-identical attrs_json.
// ---------------------------------------------------------------------------

type workerAdoptsFullModeAttrs struct {
	RelPath  string `json:"rel_path,omitempty"`
	Producer string `json:"producer,omitempty"`
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type workerAdoptsState struct {
	files []string

	preNodes  []workerAdoptsNodeRecord
	preEdges  []workerAdoptsEdgeRecord
	postNodes []workerAdoptsNodeRecord
	postEdges []workerAdoptsEdgeRecord

	modRoot string

	// For the byte-identity integration test.
	byteIdentityTestOutput string
	byteIdentityTestErr    error

	// For the integration suite scenario.
	testSuiteOutput string
	testSuiteErr    error

	// For the grep scenario.
	grepOutput map[string]string
}

const (
	workerAdoptsTestRepoURL = "https://example.test/worker-adopts-e2e"
	workerAdoptsTestSHA     = "deadbeef1234"
)

// ---------------------------------------------------------------------------
// runPreRefactorInlineFlow replicates the exact sequence of InsertNode /
// InsertEdge calls that worker.runFull made BEFORE the AncestryWriter
// refactor: EnsureRepo, EnsureCommit, InsertNode(repo), then per-file
// inline package-dedupe + InsertNode(package) + InsertEdge(repo→pkg) +
// InsertNode(file) + InsertEdge(pkg→file). This is the "before" baseline.
// ---------------------------------------------------------------------------

func runPreRefactorInlineFlow(spy *workerAdoptsSpyWriter, files []string) error {
	ctx := context.Background()

	repoRec, err := spy.EnsureRepo(ctx, graphwriter.RepoInput{
		URL:            workerAdoptsTestRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: workerAdoptsTestSHA,
		LanguageHints:  []string{"go"},
	})
	if err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}
	assignedRepoID := repoRec.ID

	if _, err := spy.EnsureCommit(ctx, graphwriter.CommitInput{
		RepoID:    assignedRepoID,
		SHA:       workerAdoptsTestSHA,
		ParentSHA: "",
	}); err != nil {
		return fmt.Errorf("EnsureCommit: %w", err)
	}

	repoAttrs, _ := json.Marshal(workerAdoptsFullModeAttrs{Producer: "repoindexer.full"})
	repoNode, err := spy.InsertNode(ctx, graphwriter.NodeInput{
		RepoID:             assignedRepoID,
		Kind:               "repo",
		CanonicalSignature: repoindexer.CanonicalRepoSig(workerAdoptsTestRepoURL),
		FromSHA:            workerAdoptsTestSHA,
		AttrsJSON:          repoAttrs,
	})
	if err != nil {
		return fmt.Errorf("InsertNode(repo): %w", err)
	}

	type pkgEntry struct{ nodeID string }
	packages := make(map[string]pkgEntry)

	for _, f := range files {
		dir := repoindexer.CanonicalPackageDir(f)
		pkg, ok := packages[dir]
		if !ok {
			pkgAttrs, _ := json.Marshal(workerAdoptsFullModeAttrs{
				RelPath: dir, Producer: "repoindexer.full",
			})
			pkgRec, pErr := spy.InsertNode(ctx, graphwriter.NodeInput{
				RepoID:             assignedRepoID,
				Kind:               "package",
				CanonicalSignature: repoindexer.CanonicalPackageSig(workerAdoptsTestRepoURL, dir),
				ParentNodeID:       repoNode.NodeID,
				FromSHA:            workerAdoptsTestSHA,
				AttrsJSON:          pkgAttrs,
			})
			if pErr != nil {
				return fmt.Errorf("InsertNode(package %q): %w", dir, pErr)
			}
			pkg = pkgEntry{nodeID: pkgRec.NodeID}
			packages[dir] = pkg

			if _, eErr := spy.InsertEdge(ctx, graphwriter.EdgeInput{
				RepoID:    assignedRepoID,
				Kind:      "contains",
				SrcNodeID: repoNode.NodeID,
				DstNodeID: pkg.nodeID,
				FromSHA:   workerAdoptsTestSHA,
			}); eErr != nil {
				return fmt.Errorf("InsertEdge(repo→pkg): %w", eErr)
			}
		}

		fileAttrs, _ := json.Marshal(workerAdoptsFullModeAttrs{
			RelPath: f, Producer: "repoindexer.full",
		})
		fileRec, fErr := spy.InsertNode(ctx, graphwriter.NodeInput{
			RepoID:             assignedRepoID,
			Kind:               "file",
			CanonicalSignature: repoindexer.CanonicalFileSig(workerAdoptsTestRepoURL, f),
			ParentNodeID:       pkg.nodeID,
			FromSHA:            workerAdoptsTestSHA,
			AttrsJSON:          fileAttrs,
		})
		if fErr != nil {
			return fmt.Errorf("InsertNode(file %q): %w", f, fErr)
		}

		if _, eErr := spy.InsertEdge(ctx, graphwriter.EdgeInput{
			RepoID:    assignedRepoID,
			Kind:      "contains",
			SrcNodeID: pkg.nodeID,
			DstNodeID: fileRec.NodeID,
			FromSHA:   workerAdoptsTestSHA,
		}); eErr != nil {
			return fmt.Errorf("InsertEdge(pkg→file): %w", eErr)
		}
	}

	return nil
}

// ---------------------------------------------------------------------------
// runPostRefactorAncestryWriterFlow drives the AncestryWriter (the refactored
// path). This is what worker.runFull now delegates to.
// ---------------------------------------------------------------------------

func runPostRefactorAncestryWriterFlow(spy *workerAdoptsSpyWriter, files []string) error {
	ctx := context.Background()
	aw := repoindexer.NewAncestryWriter(spy, workerAdoptsTestRepoURL, workerAdoptsTestSHA)
	if _, err := aw.EnsureRepoAndCommit(ctx, "main", []string{"go"}); err != nil {
		return fmt.Errorf("EnsureRepoAndCommit: %w", err)
	}
	for _, f := range files {
		if _, err := aw.EnsureFile(ctx, repoindexer.WalkFile{RelPath: f}); err != nil {
			return fmt.Errorf("EnsureFile(%q): %w", f, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (st *workerAdoptsState) theFixtureRepoWithFilesAcrossPackages(nFiles, nPkgs int) error {
	st.modRoot = moduleRootWorkerAdopts()
	dirs := make([]string, nPkgs)
	for i := 0; i < nPkgs; i++ {
		dirs[i] = fmt.Sprintf("pkg/mod%d/", i)
	}
	st.files = make([]string, nFiles)
	for i := 0; i < nFiles; i++ {
		st.files[i] = fmt.Sprintf("%sfile%d.go", dirs[i%nPkgs], i)
	}
	return nil
}

func (st *workerAdoptsState) workerRunFullDelegatesToNewAncestryWriter() error {
	workerPath := filepath.Join(st.modRoot, "internal", "repoindexer", "worker.go")
	content, err := os.ReadFile(workerPath)
	if err != nil {
		return fmt.Errorf("reading worker.go: %w", err)
	}
	src := string(content)
	if !strings.Contains(src, "NewAncestryWriter(") {
		return fmt.Errorf("worker.go does not contain NewAncestryWriter call — refactor not applied")
	}
	if !strings.Contains(src, "EnsureRepoAndCommit(") {
		return fmt.Errorf("worker.go does not call EnsureRepoAndCommit — refactor incomplete")
	}
	if !strings.Contains(src, ".EnsureFile(") {
		return fmt.Errorf("worker.go does not call EnsureFile — refactor incomplete")
	}
	return nil
}

func (st *workerAdoptsState) theRepoindexerTestSuite() error {
	st.modRoot = moduleRootWorkerAdopts()
	return nil
}

func (st *workerAdoptsState) theRefactoredCodebaseUnder() error {
	st.modRoot = moduleRootWorkerAdopts()
	st.grepOutput = make(map[string]string)
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (st *workerAdoptsState) preRefactorInlineFlowRunsAgainstSpy() error {
	preSpy := newWorkerAdoptsSpyWriter()
	if err := runPreRefactorInlineFlow(preSpy, st.files); err != nil {
		return fmt.Errorf("pre-refactor inline flow: %w", err)
	}
	st.preNodes = preSpy.nodes
	st.preEdges = preSpy.edges
	return nil
}

func (st *workerAdoptsState) postRefactorAncestryWriterFlowRunsAgainstFreshSpy() error {
	postSpy := newWorkerAdoptsSpyWriter()
	if err := runPostRefactorAncestryWriterFlow(postSpy, st.files); err != nil {
		return fmt.Errorf("post-refactor AncestryWriter flow: %w", err)
	}
	st.postNodes = postSpy.nodes
	st.postEdges = postSpy.edges
	return nil
}

func (st *workerAdoptsState) goTestRunsByteIdentityTest() error {
	cmd := exec.Command("go", "test", "-v", "-count=1",
		"-run", "TestWorker_fullIngest_graphIsByteIdenticalToCanonicalIdentity",
		"./internal/repoindexer/...")
	cmd.Dir = st.modRoot
	out, err := cmd.CombinedOutput()
	st.byteIdentityTestOutput = string(out)
	st.byteIdentityTestErr = err
	return nil
}

func (st *workerAdoptsState) theRepoindexerTestSuiteRunsViaGoTest() error {
	cmd := exec.Command("go", "test", "-v", "-count=1", "./internal/repoindexer/...")
	cmd.Dir = st.modRoot
	out, err := cmd.CombinedOutput()
	st.testSuiteOutput = string(out)
	st.testSuiteErr = err
	return nil
}

func (st *workerAdoptsState) grepScansForOldUnexportedHelperNames() error {
	internalDir := filepath.Join(st.modRoot, "internal")
	targets := []string{
		"canonicalRepoSig",
		"canonicalPackageDir",
		"canonicalPackageSig",
		"canonicalFileSig",
	}
	for _, target := range targets {
		cmd := exec.Command("grep", "-rIn", target, internalDir)
		out, _ := cmd.CombinedOutput()
		st.grepOutput[target] = string(out)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (st *workerAdoptsState) everyNodeHasIdenticalSigKindParent() error {
	if len(st.preNodes) != len(st.postNodes) {
		return fmt.Errorf("node count mismatch: pre=%d post=%d", len(st.preNodes), len(st.postNodes))
	}
	for i := range st.preNodes {
		pre := st.preNodes[i]
		post := st.postNodes[i]
		if pre.kind != post.kind {
			return fmt.Errorf("node[%d] kind mismatch: pre=%q post=%q", i, pre.kind, post.kind)
		}
		if pre.canonicalSignature != post.canonicalSignature {
			return fmt.Errorf("node[%d] canonical_signature mismatch: pre=%q post=%q",
				i, pre.canonicalSignature, post.canonicalSignature)
		}
		if pre.parentNodeID != post.parentNodeID {
			return fmt.Errorf("node[%d] parent_node_id mismatch: pre=%q post=%q",
				i, pre.parentNodeID, post.parentNodeID)
		}
	}
	return nil
}

func (st *workerAdoptsState) everyEdgeHasIdenticalKindAndSrcDst() error {
	if len(st.preEdges) != len(st.postEdges) {
		return fmt.Errorf("edge count mismatch: pre=%d post=%d", len(st.preEdges), len(st.postEdges))
	}
	for i := range st.preEdges {
		pre := st.preEdges[i]
		post := st.postEdges[i]
		if pre.kind != post.kind {
			return fmt.Errorf("edge[%d] kind mismatch: pre=%q post=%q", i, pre.kind, post.kind)
		}
		if pre.srcNodeID != post.srcNodeID || pre.dstNodeID != post.dstNodeID {
			return fmt.Errorf("edge[%d] src-dst mismatch: pre=(%s→%s) post=(%s→%s)",
				i, pre.srcNodeID, pre.dstNodeID, post.srcNodeID, post.dstNodeID)
		}
	}
	return nil
}

func (st *workerAdoptsState) everyNodeFingerprintByteIdentical() error {
	if len(st.preNodes) != len(st.postNodes) {
		return fmt.Errorf("node count mismatch: pre=%d post=%d", len(st.preNodes), len(st.postNodes))
	}
	for i := range st.preNodes {
		if st.preNodes[i].fingerprint != st.postNodes[i].fingerprint {
			return fmt.Errorf("node[%d] fingerprint mismatch: pre=%q post=%q",
				i, st.preNodes[i].fingerprint, st.postNodes[i].fingerprint)
		}
	}
	return nil
}

func (st *workerAdoptsState) byteIdentityIntegrationTestDidNotFail() error {
	// The test requires AGENT_MEMORY_PG_URL. Without PG it skips
	// (exit 0). With PG it runs worker.runFull and verifies byte-
	// identity. Either way, a "--- FAIL:" line means the refactor
	// broke something.
	if strings.Contains(st.byteIdentityTestOutput, "--- FAIL:") {
		return fmt.Errorf("byte-identity integration test FAILED:\n%s", st.byteIdentityTestOutput)
	}
	return nil
}

func (st *workerAdoptsState) testSuiteExitsWithCode0() error {
	if st.testSuiteErr != nil {
		return fmt.Errorf("go test exited with error: %v\noutput:\n%s",
			st.testSuiteErr, st.testSuiteOutput)
	}
	return nil
}

func (st *workerAdoptsState) noTestFunctionsReportFAIL() error {
	if strings.Contains(st.testSuiteOutput, "--- FAIL:") {
		return fmt.Errorf("test suite contains FAIL:\n%s", st.testSuiteOutput)
	}
	return nil
}

func (st *workerAdoptsState) noHitsForPatternRemain(pattern string) error {
	output := st.grepOutput[pattern]
	if strings.TrimSpace(output) != "" {
		return fmt.Errorf("grep -rIn %q found hits:\n%s", pattern, output)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_identity_and_ancestry_refactor_worker_adopts_ancestrywriter(ctx *godog.ScenarioContext) {
	st := &workerAdoptsState{}

	// Given
	ctx.Given(`^the same fixture repo with (\d+) files across (\d+) packages$`, st.theFixtureRepoWithFilesAcrossPackages)
	ctx.Given(`^worker\.runFull delegates to NewAncestryWriter in the source$`, st.workerRunFullDelegatesToNewAncestryWriter)
	ctx.Given(`^the repoindexer test suite under internal/repoindexer$`, st.theRepoindexerTestSuite)
	ctx.Given(`^the refactored codebase under services/agent-memory/internal/$`, st.theRefactoredCodebaseUnder)

	// When
	ctx.When(`^the pre-refactor inline worker flow runs against the spy$`, st.preRefactorInlineFlowRunsAgainstSpy)
	ctx.When(`^the post-refactor AncestryWriter flow runs against a fresh spy$`, st.postRefactorAncestryWriterFlowRunsAgainstFreshSpy)
	ctx.When(`^go test runs TestWorker_fullIngest_graphIsByteIdenticalToCanonicalIdentity$`, st.goTestRunsByteIdentityTest)
	ctx.When(`^the repoindexer test suite runs via go test$`, st.theRepoindexerTestSuiteRunsViaGoTest)
	ctx.When(`^grep -rIn scans for old unexported helper names$`, st.grepScansForOldUnexportedHelperNames)

	// Then
	ctx.Then(`^every node has identical canonical_signature, kind, and parent_node_id$`, st.everyNodeHasIdenticalSigKindParent)
	ctx.Then(`^every edge has identical kind and src-dst node pairs$`, st.everyEdgeHasIdenticalKindAndSrcDst)
	ctx.Then(`^every node fingerprint is byte-identical$`, st.everyNodeFingerprintByteIdentical)
	ctx.Then(`^the byte-identity integration test did not fail$`, st.byteIdentityIntegrationTestDidNotFail)
	ctx.Then(`^the test suite exits with code 0$`, st.testSuiteExitsWithCode0)
	ctx.Then(`^no test functions report FAIL in the output$`, st.noTestFunctionsReportFAIL)
	ctx.Then(`^no hits for "([^"]*)" remain in the output$`, st.noHitsForPatternRemain)
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
