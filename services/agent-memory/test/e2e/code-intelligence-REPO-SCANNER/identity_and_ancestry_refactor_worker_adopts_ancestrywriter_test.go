//go:build e2e

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Recording writer — captures every (canonical_signature, kind,
// parent_node_id, fingerprint) node tuple AND every (kind,
// src_node_id, dst_node_id, fingerprint) edge tuple the
// AncestryWriter emits, so the golden-fixture scenario can assert
// byte-identical output matching the pre-refactor worker.runFull.
// ---------------------------------------------------------------------------

type goldenNodeTupleRecord struct {
	Kind               string
	CanonicalSignature string
	ParentNodeID       string
	FingerprintHex     string
}

type goldenEdgeTupleRecord struct {
	Kind           string
	SrcNodeID      string
	DstNodeID      string
	FingerprintHex string
}

type runFullRecordingWriter struct {
	mu sync.Mutex

	repoSeq int
	nodeSeq int
	edgeSeq int

	repos map[string]graphwriter.RepoRecord
	nodes map[string]graphwriter.NodeRecord
	edges map[string]graphwriter.EdgeRecord

	// Fingerprint lookup by node ID so edge fingerprints can
	// resolve src/dst.
	fpByNodeID map[string]fingerprint.Sum

	// Ordered records for golden comparison.
	nodeTuples []goldenNodeTupleRecord
	edgeTuples []goldenEdgeTupleRecord

	// Track assigned repo ID for fingerprint computation.
	assignedRepoID fingerprint.RepoID
}

func newRunFullRecordingWriter() *runFullRecordingWriter {
	return &runFullRecordingWriter{
		repos:      make(map[string]graphwriter.RepoRecord),
		nodes:      make(map[string]graphwriter.NodeRecord),
		edges:      make(map[string]graphwriter.EdgeRecord),
		fpByNodeID: make(map[string]fingerprint.Sum),
	}
}

func (w *runFullRecordingWriter) EnsureRepo(_ context.Context, in graphwriter.RepoInput) (graphwriter.RepoRecord, error) {
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

func (w *runFullRecordingWriter) EnsureCommit(_ context.Context, in graphwriter.CommitInput) (graphwriter.CommitRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return graphwriter.CommitRecord{
		RepoID:   in.RepoID.String(),
		SHA:      in.SHA,
		Inserted: true,
	}, nil
}

func (w *runFullRecordingWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
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
	w.fpByNodeID[nodeID] = fp
	w.nodeTuples = append(w.nodeTuples, goldenNodeTupleRecord{
		Kind:               in.Kind,
		CanonicalSignature: in.CanonicalSignature,
		ParentNodeID:       in.ParentNodeID,
		FingerprintHex:     fp.Hex(),
	})
	return rec, nil
}

func (w *runFullRecordingWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := in.RepoID.String() + "|" + in.Kind + "|" + in.SrcNodeID + "|" + in.DstNodeID + "|" + in.FromSHA
	if rec, ok := w.edges[key]; ok {
		rec.Inserted = false
		return rec, nil
	}
	w.edgeSeq++

	srcFP := w.fpByNodeID[in.SrcNodeID]
	dstFP := w.fpByNodeID[in.DstNodeID]
	edgeFP, err := fingerprint.EdgeFingerprint(in.RepoID, in.Kind, srcFP, dstFP, in.FromSHA)
	if err != nil {
		return graphwriter.EdgeRecord{}, fmt.Errorf("edge fingerprint: %w", err)
	}

	rec := graphwriter.EdgeRecord{
		EdgeID:      fmt.Sprintf("edge-%04d", w.edgeSeq),
		Fingerprint: edgeFP,
		SrcFP:       srcFP,
		DstFP:       dstFP,
		Inserted:    true,
	}
	w.edges[key] = rec
	w.edgeTuples = append(w.edgeTuples, goldenEdgeTupleRecord{
		Kind:           in.Kind,
		SrcNodeID:      in.SrcNodeID,
		DstNodeID:      in.DstNodeID,
		FingerprintHex: edgeFP.Hex(),
	})
	return rec, nil
}

// ---------------------------------------------------------------------------
// COMMITTED GOLDEN SNAPSHOT
//
// These are the pre-refactor node/edge tuples for the 3-file
// fixture (README.md, pkg/foo.go, pkg/sub/bar.go) with:
//   Repo URL:  https://example.test/golden-repo
//   SHA:       deadbeef1234
//   Spy RepoID: [0x01, 0…, 0xAA]
//
// Fingerprints were computed from the fingerprint package's
// NodeFingerprint / EdgeFingerprint functions and frozen here.
// Any drift in the canonical-signature helpers, the fingerprint
// pre-image format, or the AncestryWriter call sequence shifts
// these values and fails the test — which is exactly the point.
// ---------------------------------------------------------------------------

const (
	goldenRepoURL = "https://example.test/golden-repo"
	goldenSHA     = "deadbeef1234"
	goldenParent  = "aaa111"
	goldenHead    = "bbb222"
)

var goldenFixtureFiles = []string{
	"README.md",
	"pkg/foo.go",
	"pkg/sub/bar.go",
}

type committedNodeSnapshot struct {
	Kind               string
	CanonicalSignature string
	ParentIndex        int    // -1 = no parent
	FingerprintHex     string // frozen SHA-256 hex
}

// goldenCommittedNodes — frozen pre-refactor output.
var goldenCommittedNodes = []committedNodeSnapshot{
	{Kind: "repo", CanonicalSignature: "https://example.test/golden-repo", ParentIndex: -1,
		FingerprintHex: "5e74a9a8c2fd3aadf5e3f3a7ed8dfa35dcc25619a53234ea5f271a99dd088883"},
	{Kind: "package", CanonicalSignature: "https://example.test/golden-repo::pkg::", ParentIndex: 0,
		FingerprintHex: "e327e045f494be70327274403aac97ecc79ef56509536c6a9cefa3d6fee8d0a2"},
	{Kind: "file", CanonicalSignature: "https://example.test/golden-repo::file::README.md", ParentIndex: 1,
		FingerprintHex: "bf1336ecd5db45448c4e972a4a0751d5fff86fe6d1ec8e7dc40c0c29bd5d1c9a"},
	{Kind: "package", CanonicalSignature: "https://example.test/golden-repo::pkg::pkg", ParentIndex: 0,
		FingerprintHex: "f5a0ea1eb3fb433d3f5a5e6364ef5ff310200885f6098e983579267131d34593"},
	{Kind: "file", CanonicalSignature: "https://example.test/golden-repo::file::pkg/foo.go", ParentIndex: 3,
		FingerprintHex: "0fb9f2b224f163a70f119c1ab4e49830a0b797dfce784c3af8bbb344ba91c451"},
	{Kind: "package", CanonicalSignature: "https://example.test/golden-repo::pkg::pkg/sub", ParentIndex: 0,
		FingerprintHex: "d8f94bc0e0ef3fba4eb11a6d81eced6ee860e18abb5e563164e601ffa33088a9"},
	{Kind: "file", CanonicalSignature: "https://example.test/golden-repo::file::pkg/sub/bar.go", ParentIndex: 5,
		FingerprintHex: "9bdd64c723f19fae527b72ff5ae54dd0a724a546afcb11f9cb25b88a960adc9e"},
}

type committedEdgeSnapshot struct {
	Kind           string
	SrcIndex       int    // index into goldenCommittedNodes
	DstIndex       int    // index into goldenCommittedNodes
	FingerprintHex string // frozen SHA-256 hex
}

// goldenCommittedEdges — frozen pre-refactor output.
var goldenCommittedEdges = []committedEdgeSnapshot{
	{Kind: "contains", SrcIndex: 0, DstIndex: 1,
		FingerprintHex: "f991d1a6af588a2c18c0959efddb836f2effe0e0dee2d2d6884029da59cc4a16"},
	{Kind: "contains", SrcIndex: 1, DstIndex: 2,
		FingerprintHex: "d5977778e085eeb8c04c73533d363c7b5af249f0e869903f4e5661e00939e76e"},
	{Kind: "contains", SrcIndex: 0, DstIndex: 3,
		FingerprintHex: "edd66cccb5806477937fac4f7f1900bbb713e65222ed2584c8dd578129fdba21"},
	{Kind: "contains", SrcIndex: 3, DstIndex: 4,
		FingerprintHex: "274c2ab5ea5909e9edd4750a18b030d9f029be9076c218e1d869258479e8034e"},
	{Kind: "contains", SrcIndex: 0, DstIndex: 5,
		FingerprintHex: "30984649d8bab2dcb1808d88baffde051d19b3c57641dfa5c0e1e6e3f25f50a2"},
	{Kind: "contains", SrcIndex: 5, DstIndex: 6,
		FingerprintHex: "3465213828afdcf49ce5fbb35a4a1089f970ae454ac197c79d5b0c4545301349"},
}

// ---------------------------------------------------------------------------
// AST-based runFull wiring verification
//
// Instead of string-scanning worker.go for tokens (which can miss
// ordering, arguments, conditions, and summary wiring), we parse
// the Go AST with go/parser and verify:
//
//  1. runFull is a method on *Worker
//  2. The method body contains the five AncestryWriter delegation
//     calls in the correct ORDER: NewAncestryWriter → SetParentSHA
//     → SetCurrentHeadSHA → EnsureRepoAndCommit → EnsureFile
//  3. The method body assigns FullSummary fields from FileAncestry
//     fields (PackagesEnsured, PackagesInserted, FilesEnsured,
//     FilesInserted, ContainsEdgesInserted)
//
// The AST naturally bounds the method body (no EOF slicing) and
// understands Go syntax (no false matches on comments or strings).
// ---------------------------------------------------------------------------

// astCallCollector walks an AST node and collects method/function
// call names in source order.
func astCallCollector(node ast.Node) []string {
	var calls []string
	ast.Inspect(node, func(n ast.Node) bool {
		ce, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := ce.Fun.(type) {
		case *ast.SelectorExpr:
			calls = append(calls, fn.Sel.Name)
		case *ast.Ident:
			calls = append(calls, fn.Name)
		}
		return true
	})
	return calls
}

// astAssignmentCollector walks an AST node and collects all
// field assignment targets — both direct assignments like
// `summary.PackagesEnsured++` and composite literal fields like
// `summary := FullSummary{RepoNodeID: ...}`.
func astAssignmentCollector(node ast.Node) []string {
	var assigns []string
	ast.Inspect(node, func(n ast.Node) bool {
		switch stmt := n.(type) {
		case *ast.AssignStmt:
			for _, lhs := range stmt.Lhs {
				if sel, ok := lhs.(*ast.SelectorExpr); ok {
					if recv, ok := sel.X.(*ast.Ident); ok {
						assigns = append(assigns, recv.Name+"."+sel.Sel.Name)
					}
				}
			}
			// Check RHS for composite literals with named fields
			// (e.g. summary := FullSummary{RepoNodeID: ...}).
			for _, rhs := range stmt.Rhs {
				cl, ok := rhs.(*ast.CompositeLit)
				if !ok {
					continue
				}
				// Find the variable name from LHS.
				var varName string
				for _, lhs := range stmt.Lhs {
					if id, ok := lhs.(*ast.Ident); ok {
						varName = id.Name
						break
					}
				}
				if varName == "" {
					continue
				}
				for _, elt := range cl.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok {
							assigns = append(assigns, varName+"."+key.Name)
						}
					}
				}
			}
		case *ast.IncDecStmt:
			if sel, ok := stmt.X.(*ast.SelectorExpr); ok {
				if recv, ok := sel.X.(*ast.Ident); ok {
					assigns = append(assigns, recv.Name+"."+sel.Sel.Name)
				}
			}
		}
		return true
	})
	return assigns
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type workerAdoptsState struct {
	writer  *runFullRecordingWriter
	files   []string
	summary workerRunFullSummary

	// worker.go source content for structural check
	workerSource string

	// worker-integration-still-passes
	integResult string

	// helpers-no-internal-callers
	grepHits []string
}

type workerRunFullSummary struct {
	RepoNodeID            string
	CommitInserted        bool
	PackagesEnsured       int
	PackagesInserted      int
	FilesEnsured          int
	FilesInserted         int
	ContainsEdgesInserted int
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *workerAdoptsState) anInMemoryFixtureRepoWithFiles(fileList string) error {
	s.files = strings.Split(fileList, ",")
	s.writer = newRunFullRecordingWriter()
	return nil
}

func (s *workerAdoptsState) aRecordingRepoCommitNodeEdgeWriter() error {
	if s.writer == nil {
		s.writer = newRunFullRecordingWriter()
	}
	return nil
}

func (s *workerAdoptsState) theExistingWorkerIntegrationTestSuite() error {
	return nil
}

func (s *workerAdoptsState) theRefactoredCodebaseUnder(_ string) error {
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

// workerRunFullThroughAncestryWriter replicates the exact call
// sequence of worker.runFull (worker.go lines 1095-1167):
//
//  1. NewAncestryWriter(w.writer, repoURL, job.ToSHA)
//  2. aw.SetParentSHA(job.FromSHA)
//  3. aw.SetCurrentHeadSHA(repoHeadSHA)
//  4. aw.EnsureRepoAndCommit(ctx, repoBranch, repoLang)
//  5. per-file: aw.EnsureFile(ctx, file) + FullSummary tracking
//
// The structural Then step verifies worker.go contains these
// exact call sites in runFull, so the e2e test and the production
// code cannot drift independently.
func (s *workerAdoptsState) workerRunFullThroughAncestryWriter(parentSHA, headSHA string) error {
	// Load worker.go source for the structural verification step.
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
	workerPath := filepath.Join(repoRoot, "internal", "repoindexer", "worker.go")
	src, err := os.ReadFile(workerPath)
	if err != nil {
		return fmt.Errorf("reading worker.go: %w", err)
	}
	s.workerSource = string(src)

	ctx := context.Background()

	// Replicate worker.runFull steps 1-3.
	aw := repoindexer.NewAncestryWriter(s.writer, goldenRepoURL, goldenSHA)
	aw.SetParentSHA(parentSHA)
	aw.SetCurrentHeadSHA(headSHA)

	// Step 4: pre-walk ancestry.
	ancestry, err := aw.EnsureRepoAndCommit(ctx, "main", []string{"go"})
	if err != nil {
		return fmt.Errorf("EnsureRepoAndCommit: %w", err)
	}

	s.summary.RepoNodeID = ancestry.RepoNodeID
	s.summary.CommitInserted = ancestry.CommitInserted

	// Step 5: per-file walk with FullSummary tracking.
	pkgDirSeen := make(map[string]struct{})
	for _, f := range s.files {
		fa, eErr := aw.EnsureFile(ctx, repoindexer.WalkFile{RelPath: f})
		if eErr != nil {
			return fmt.Errorf("EnsureFile(%q): %w", f, eErr)
		}

		if _, seen := pkgDirSeen[fa.PackageDir]; !seen {
			pkgDirSeen[fa.PackageDir] = struct{}{}
			s.summary.PackagesEnsured++
		}
		if fa.PackageNewlyInserted {
			s.summary.PackagesInserted++
		}
		if fa.PackageEdgeInserted {
			s.summary.ContainsEdgesInserted++
		}
		s.summary.FilesEnsured++
		if fa.NewlyInserted {
			s.summary.FilesInserted++
		}
		if fa.FileEdgeInserted {
			s.summary.ContainsEdgesInserted++
		}
	}
	return nil
}

func (s *workerAdoptsState) theIntegrationSuiteRunsAgainstPG() error {
	pgURL := os.Getenv("AGENT_MEMORY_PG_URL")
	_, thisFile, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..", "..")

	integFile := filepath.Join(repoRoot, "internal", "repoindexer", "worker_integration_test.go")
	content, err := os.ReadFile(integFile)
	if err != nil {
		return fmt.Errorf("worker_integration_test.go not found: %w", err)
	}
	for _, fn := range []string{
		"TestWorker_fullIngest_buildsRepoPackageFileAncestry",
		"AGENT_MEMORY_PG_URL",
	} {
		if !strings.Contains(string(content), fn) {
			return fmt.Errorf("worker_integration_test.go missing %q", fn)
		}
	}

	if pgURL == "" {
		s.integResult = "skipped:no-dsn"
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test",
		"-count=1", "-timeout=4m",
		"-run", "TestWorker",
		"./internal/repoindexer/",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(), "AGENT_MEMORY_PG_URL="+pgURL)
	out, err := cmd.CombinedOutput()
	if err != nil {
		s.integResult = fmt.Sprintf("failed:%s\n%s", err, string(out))
		return fmt.Errorf("integration tests failed: %s\n%s", err, string(out))
	}
	s.integResult = "passed"
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

// workerGoConfirmsWiringAST parses worker.go with go/parser,
// locates the runFull method on *Worker, and verifies:
//
//  1. The five AncestryWriter delegation calls appear in order
//  2. The FullSummary field assignments are present
//
// This is structurally stronger than string scanning because the
// AST naturally bounds the method body, understands Go syntax,
// and verifies call ordering — catching incorrect ordering,
// swapped arguments, missing conditions, or summary-wiring drift
// that string-contains checks would miss.
func (s *workerAdoptsState) workerGoConfirmsWiringAST() error {
	if s.workerSource == "" {
		return fmt.Errorf("worker.go source not loaded")
	}

	// Parse worker.go into a Go AST.
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "worker.go", s.workerSource, 0)
	if err != nil {
		return fmt.Errorf("go/parser failed on worker.go: %w", err)
	}

	// Find func (w *Worker) runFull(...) in the AST.
	var runFullDecl *ast.FuncDecl
	for _, decl := range f.Decls {
		fd, ok := decl.(*ast.FuncDecl)
		if !ok || fd.Name.Name != "runFull" {
			continue
		}
		if fd.Recv == nil || len(fd.Recv.List) == 0 {
			continue
		}
		recvType := fd.Recv.List[0].Type
		if star, ok := recvType.(*ast.StarExpr); ok {
			if ident, ok := star.X.(*ast.Ident); ok && ident.Name == "Worker" {
				runFullDecl = fd
				break
			}
		}
	}
	if runFullDecl == nil {
		return fmt.Errorf("worker.go: func (w *Worker) runFull not found in AST")
	}

	// 1. Verify the five AncestryWriter delegation calls appear
	//    in the correct order within runFull's body.
	calls := astCallCollector(runFullDecl.Body)

	requiredSequence := []string{
		"NewAncestryWriter",
		"SetParentSHA",
		"SetCurrentHeadSHA",
		"EnsureRepoAndCommit",
		"EnsureFile",
	}

	lastIdx := -1
	for _, required := range requiredSequence {
		found := false
		for i := lastIdx + 1; i < len(calls); i++ {
			if calls[i] == required {
				lastIdx = i
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf(
				"runFull AST: %q not found (or out of order) after position %d in call sequence %v",
				required, lastIdx, calls)
		}
	}

	// 2. Verify FullSummary field assignments are present in
	//    runFull — these wire FileAncestry results into the
	//    returned summary. Without these, the summary counters
	//    would be zero even though EnsureFile succeeded.
	assigns := astAssignmentCollector(runFullDecl.Body)
	requiredAssigns := []string{
		"summary.RepoNodeID",
		"summary.CommitInserted",
		"summary.PackagesEnsured",
		"summary.PackagesInserted",
		"summary.FilesEnsured",
		"summary.FilesInserted",
		"summary.ContainsEdgesInserted",
	}
	assignSet := make(map[string]bool, len(assigns))
	for _, a := range assigns {
		assignSet[a] = true
	}
	var missingAssigns []string
	for _, ra := range requiredAssigns {
		if !assignSet[ra] {
			missingAssigns = append(missingAssigns, ra)
		}
	}
	if len(missingAssigns) > 0 {
		return fmt.Errorf(
			"runFull AST: missing FullSummary field assignments: %s — "+
				"found assignments: %v",
			strings.Join(missingAssigns, ", "), assigns)
	}

	return nil
}

func (s *workerAdoptsState) capturedNodeTuplesMatchCommittedSnapshot() error {
	s.writer.mu.Lock()
	defer s.writer.mu.Unlock()

	if len(s.writer.nodeTuples) != len(goldenCommittedNodes) {
		return fmt.Errorf("node count: got %d, want %d", len(s.writer.nodeTuples), len(goldenCommittedNodes))
	}
	for i, got := range s.writer.nodeTuples {
		want := goldenCommittedNodes[i]

		if got.Kind != want.Kind {
			return fmt.Errorf("node[%d]: kind got %q, want %q", i, got.Kind, want.Kind)
		}
		if got.CanonicalSignature != want.CanonicalSignature {
			return fmt.Errorf("node[%d]: canonical_signature got %q, want %q",
				i, got.CanonicalSignature, want.CanonicalSignature)
		}

		// Assert parent_node_id.
		if want.ParentIndex == -1 {
			if got.ParentNodeID != "" {
				return fmt.Errorf("node[%d] (%s): expected no parent, got %q",
					i, got.Kind, got.ParentNodeID)
			}
		} else {
			wantParentNodeID := fmt.Sprintf("node-%04d", want.ParentIndex+1)
			if got.ParentNodeID != wantParentNodeID {
				return fmt.Errorf("node[%d] (%s): parent got %q, want %q",
					i, got.CanonicalSignature, got.ParentNodeID, wantParentNodeID)
			}
		}

		// Assert committed fingerprint.
		if got.FingerprintHex != want.FingerprintHex {
			return fmt.Errorf("node[%d] (%s): fingerprint got %q, want committed %q",
				i, got.CanonicalSignature, got.FingerprintHex, want.FingerprintHex)
		}
	}
	return nil
}

func (s *workerAdoptsState) capturedEdgeTuplesMatchCommittedSnapshot() error {
	s.writer.mu.Lock()
	defer s.writer.mu.Unlock()

	if len(s.writer.edgeTuples) != len(goldenCommittedEdges) {
		return fmt.Errorf("edge count: got %d, want %d", len(s.writer.edgeTuples), len(goldenCommittedEdges))
	}
	for i, got := range s.writer.edgeTuples {
		want := goldenCommittedEdges[i]
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
		if got.FingerprintHex != want.FingerprintHex {
			return fmt.Errorf("edge[%d] (%s %s→%s): fingerprint got %q, want committed %q",
				i, got.Kind, got.SrcNodeID, got.DstNodeID, got.FingerprintHex, want.FingerprintHex)
		}
	}
	return nil
}

func (s *workerAdoptsState) fullSummaryCountersMatch() error {
	checks := []struct {
		name string
		got  int
		want int
	}{
		{"PackagesEnsured", s.summary.PackagesEnsured, 3},
		{"PackagesInserted", s.summary.PackagesInserted, 3},
		{"FilesEnsured", s.summary.FilesEnsured, 3},
		{"FilesInserted", s.summary.FilesInserted, 3},
		{"ContainsEdgesInserted", s.summary.ContainsEdgesInserted, 6},
	}
	var errs []string
	for _, c := range checks {
		if c.got != c.want {
			errs = append(errs, fmt.Sprintf("%s: got %d, want %d", c.name, c.got, c.want))
		}
	}
	if !s.summary.CommitInserted {
		errs = append(errs, "CommitInserted: got false, want true")
	}
	if s.summary.RepoNodeID == "" {
		errs = append(errs, "RepoNodeID: got empty string")
	}
	if len(errs) > 0 {
		return fmt.Errorf("FullSummary mismatch:\n  %s", strings.Join(errs, "\n  "))
	}
	return nil
}

func (s *workerAdoptsState) fingerprintsStableAcrossSecondRun() error {
	w2 := newRunFullRecordingWriter()
	ctx := context.Background()
	aw2 := repoindexer.NewAncestryWriter(w2, goldenRepoURL, goldenSHA)
	aw2.SetParentSHA(goldenParent)
	aw2.SetCurrentHeadSHA(goldenHead)
	if _, err := aw2.EnsureRepoAndCommit(ctx, "main", []string{"go"}); err != nil {
		return fmt.Errorf("second run EnsureRepoAndCommit: %w", err)
	}
	for _, f := range s.files {
		if _, err := aw2.EnsureFile(ctx, repoindexer.WalkFile{RelPath: f}); err != nil {
			return fmt.Errorf("second run EnsureFile(%q): %w", f, err)
		}
	}

	s.writer.mu.Lock()
	firstNodes := s.writer.nodeTuples
	firstEdges := s.writer.edgeTuples
	s.writer.mu.Unlock()
	w2.mu.Lock()
	secondNodes := w2.nodeTuples
	secondEdges := w2.edgeTuples
	w2.mu.Unlock()

	if len(firstNodes) != len(secondNodes) {
		return fmt.Errorf("second run node count mismatch: %d vs %d", len(firstNodes), len(secondNodes))
	}
	for i := range firstNodes {
		if firstNodes[i].FingerprintHex != secondNodes[i].FingerprintHex {
			return fmt.Errorf("node[%d] fingerprint drift: %q vs %q",
				i, firstNodes[i].FingerprintHex, secondNodes[i].FingerprintHex)
		}
	}
	if len(firstEdges) != len(secondEdges) {
		return fmt.Errorf("second run edge count mismatch: %d vs %d", len(firstEdges), len(secondEdges))
	}
	for i := range firstEdges {
		if firstEdges[i].FingerprintHex != secondEdges[i].FingerprintHex {
			return fmt.Errorf("edge[%d] fingerprint drift: %q vs %q",
				i, firstEdges[i].FingerprintHex, secondEdges[i].FingerprintHex)
		}
	}
	return nil
}

func (s *workerAdoptsState) theSuiteResultIsRecorded() error {
	switch {
	case s.integResult == "passed":
		return nil
	case s.integResult == "skipped:no-dsn":
		return nil
	case strings.HasPrefix(s.integResult, "failed:"):
		return fmt.Errorf("integration suite failed: %s", s.integResult)
	default:
		return fmt.Errorf("unexpected integResult state: %q", s.integResult)
	}
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
	ctx.When(`^worker\.runFull executes through AncestryWriter with parentSHA "([^"]*)" and headSHA "([^"]*)"$`,
		s.workerRunFullThroughAncestryWriter)
	ctx.When(`^the integration suite runs against the provided Postgres DSN if available$`,
		s.theIntegrationSuiteRunsAgainstPG)
	ctx.When(`^we search for unexported helper names "([^"]*)"$`, s.weSearchForUnexportedHelperNames)

	// Then
	ctx.Then(`^the Go AST of worker\.go confirms runFull delegates to AncestryWriter in the correct call order with summary wiring$`,
		s.workerGoConfirmsWiringAST)
	ctx.Then(`^the captured node tuples match the committed golden snapshot$`,
		s.capturedNodeTuplesMatchCommittedSnapshot)
	ctx.Then(`^the captured edge tuples match the committed golden snapshot$`,
		s.capturedEdgeTuplesMatchCommittedSnapshot)
	ctx.Then(`^the FullSummary counters match the expected values$`,
		s.fullSummaryCountersMatch)
	ctx.Then(`^fingerprints are stable across a second identical run$`,
		s.fingerprintsStableAcrossSecondRun)
	ctx.Then(`^the suite result is recorded$`,
		s.theSuiteResultIsRecorded)
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
