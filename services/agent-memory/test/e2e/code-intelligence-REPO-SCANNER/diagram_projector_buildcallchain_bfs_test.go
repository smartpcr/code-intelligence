//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/diagram"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Fake graphsink.Reader for BuildCallChain BFS E2E scenarios
// ---------------------------------------------------------------------------

type bfsSigKey struct {
	repoID string
	kind   string
	sig    string
}

type bfsFakeReader struct {
	repos    []graphreader.RepoSummary
	nodes    map[string]graphreader.Node
	bySig    map[bfsSigKey]string
	outEdges map[string][]graphreader.Edge
	inEdges  map[string][]graphreader.Edge
}

func newBFSFakeReader() *bfsFakeReader {
	return &bfsFakeReader{
		nodes:    map[string]graphreader.Node{},
		bySig:    map[bfsSigKey]string{},
		outEdges: map[string][]graphreader.Edge{},
		inEdges:  map[string][]graphreader.Edge{},
	}
}

func (f *bfsFakeReader) addRepo(s graphreader.RepoSummary) {
	f.repos = append(f.repos, s)
}

func (f *bfsFakeReader) addNode(n graphreader.Node) {
	f.nodes[n.NodeID] = n
	f.bySig[bfsSigKey{repoID: n.RepoID, kind: n.Kind, sig: n.CanonicalSignature}] = n.NodeID
}

func (f *bfsFakeReader) addEdge(e graphreader.Edge) {
	f.outEdges[e.SrcNodeID] = append(f.outEdges[e.SrcNodeID], e)
	f.inEdges[e.DstNodeID] = append(f.inEdges[e.DstNodeID], e)
}

func (f *bfsFakeReader) ListRepos(
	_ context.Context, _ graphreader.ReaderOptions,
) ([]graphreader.RepoSummary, error) {
	out := make([]graphreader.RepoSummary, len(f.repos))
	copy(out, f.repos)
	return out, nil
}

func (f *bfsFakeReader) ListNodes(
	_ context.Context, _ fingerprint.RepoID, _ []string,
	_ graphreader.ListNodesFilter, _ graphreader.ReaderOptions,
) ([]graphreader.Node, error) {
	out := make([]graphreader.Node, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].NodeID < out[j].NodeID })
	return out, nil
}

func (f *bfsFakeReader) ListEdgesFrom(
	_ context.Context, srcNodeID string, kinds []string,
	_ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return bfsFilterEdges(f.outEdges[srcNodeID], kinds), nil
}

func (f *bfsFakeReader) ListEdgesTo(
	_ context.Context, dstNodeID string, kinds []string,
	_ graphreader.ReaderOptions,
) ([]graphreader.Edge, error) {
	return bfsFilterEdges(f.inEdges[dstNodeID], kinds), nil
}

func (f *bfsFakeReader) GetNode(
	_ context.Context, nodeID string, _ graphreader.ReaderOptions,
) (graphreader.Node, error) {
	n, ok := f.nodes[nodeID]
	if !ok {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	return n, nil
}

func (f *bfsFakeReader) LookupBySignature(
	_ context.Context, repoID fingerprint.RepoID, kind, sig string,
	_ graphreader.ReaderOptions,
) (graphreader.Node, error) {
	id, ok := f.bySig[bfsSigKey{repoID: repoID.String(), kind: kind, sig: sig}]
	if !ok {
		return graphreader.Node{}, graphreader.ErrNotFound
	}
	return f.nodes[id], nil
}

func bfsFilterEdges(in []graphreader.Edge, kinds []string) []graphreader.Edge {
	if len(kinds) == 0 {
		out := make([]graphreader.Edge, len(in))
		copy(out, in)
		return out
	}
	allowed := map[string]struct{}{}
	for _, k := range kinds {
		allowed[k] = struct{}{}
	}
	out := make([]graphreader.Edge, 0, len(in))
	for _, e := range in {
		if _, ok := allowed[e.Kind]; ok {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Fixture builders
// ---------------------------------------------------------------------------

const bfsFixtureRepoURL = "https://github.com/example/bfs-target"

func bfsFixtureRepoID() (fingerprint.RepoID, error) {
	return fingerprint.RepoIDFromURL(bfsFixtureRepoURL)
}

func bfsMethodNode(repoID fingerprint.RepoID, nodeID, sig string) graphreader.Node {
	return graphreader.Node{
		NodeID:             nodeID,
		RepoID:             repoID.String(),
		Kind:               "method",
		CanonicalSignature: sig,
		ParentNodeID:       "file-1",
		FromSHA:            "deadbeef",
		AttrsJSON:          json.RawMessage(`{"language":"go"}`),
	}
}

func bfsStaticCall(repoID fingerprint.RepoID, src, dst string) graphreader.Edge {
	return graphreader.Edge{
		EdgeID:    "e-" + src + "-" + dst,
		RepoID:    repoID.String(),
		Kind:      "static_calls",
		SrcNodeID: src,
		DstNodeID: dst,
		FromSHA:   "deadbeef",
	}
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type buildCallChainBFSCtx struct {
	reader *bfsFakeReader
	diag   diagram.Diagram
	err    error
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (c *buildCallChainBFSCtx) aMethodMWith2CalleesAnd3Callers() error {
	rid, err := bfsFixtureRepoID()
	if err != nil {
		return fmt.Errorf("fixture repo id: %w", err)
	}
	c.reader = newBFSFakeReader()
	c.reader.addRepo(graphreader.RepoSummary{
		RepoID: rid.String(),
		URL:    bfsFixtureRepoURL,
		SHA:    "deadbeef",
	})
	c.reader.addNode(bfsMethodNode(rid, "M", "M"))
	for _, callee := range []string{"C1", "C2"} {
		c.reader.addNode(bfsMethodNode(rid, callee, callee))
		c.reader.addEdge(bfsStaticCall(rid, "M", callee))
	}
	for _, caller := range []string{"P1", "P2", "P3"} {
		c.reader.addNode(bfsMethodNode(rid, caller, caller))
		c.reader.addEdge(bfsStaticCall(rid, caller, "M"))
	}
	return nil
}

func (c *buildCallChainBFSCtx) aChainABCD() error {
	rid, err := bfsFixtureRepoID()
	if err != nil {
		return fmt.Errorf("fixture repo id: %w", err)
	}
	c.reader = newBFSFakeReader()
	c.reader.addRepo(graphreader.RepoSummary{
		RepoID: rid.String(),
		URL:    bfsFixtureRepoURL,
		SHA:    "deadbeef",
	})
	for _, n := range []string{"A", "B", "C", "D"} {
		c.reader.addNode(bfsMethodNode(rid, n, n))
	}
	c.reader.addEdge(bfsStaticCall(rid, "A", "B"))
	c.reader.addEdge(bfsStaticCall(rid, "B", "C"))
	c.reader.addEdge(bfsStaticCall(rid, "C", "D"))
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (c *buildCallChainBFSCtx) buildCallChainRunsWithSeedDepthDirection(seed string, depth int, direction string) error {
	c.diag, c.err = diagram.BuildCallChain(
		context.Background(), c.reader, seed, depth, direction,
	)
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (c *buildCallChainBFSCtx) theDiagramContainsNNodesAndMEdges(wantNodes, wantEdges int) error {
	if c.err != nil {
		return fmt.Errorf("unexpected error: %w", c.err)
	}
	if got := len(c.diag.Nodes); got != wantNodes {
		return fmt.Errorf("node count = %d, want %d", got, wantNodes)
	}
	if got := len(c.diag.Edges); got != wantEdges {
		return fmt.Errorf("edge count = %d, want %d", got, wantEdges)
	}
	if c.diag.Stats.NodeCount != wantNodes {
		return fmt.Errorf("stats.nodeCount = %d, want %d", c.diag.Stats.NodeCount, wantNodes)
	}
	if c.diag.Stats.EdgeCount != wantEdges {
		return fmt.Errorf("stats.edgeCount = %d, want %d", c.diag.Stats.EdgeCount, wantEdges)
	}
	return nil
}

func (c *buildCallChainBFSCtx) theDiagramContainsNodesAndNot(present, absent string) error {
	if c.err != nil {
		return fmt.Errorf("unexpected error: %w", c.err)
	}
	nodeIDs := map[string]bool{}
	for _, n := range c.diag.Nodes {
		nodeIDs[n.ID] = true
	}
	for _, want := range strings.Split(present, ",") {
		if !nodeIDs[want] {
			return fmt.Errorf("expected node %q present, got ids=%v", want, nodeIDs)
		}
	}
	for _, notWant := range strings.Split(absent, ",") {
		if nodeIDs[notWant] {
			return fmt.Errorf("node %q must NOT appear, got ids=%v", notWant, nodeIDs)
		}
	}
	return nil
}

func (c *buildCallChainBFSCtx) itReturnsAnErrorWithCodeAndZeroValueDiagram(code string) error {
	if c.err == nil {
		return fmt.Errorf("expected error with code %q, got nil", code)
	}
	if !errors.Is(c.err, diagram.ErrSeedNotFound) {
		return fmt.Errorf("expected ErrSeedNotFound, got %v", c.err)
	}
	if !strings.Contains(c.err.Error(), code) {
		return fmt.Errorf("error %q must contain code %q", c.err.Error(), code)
	}
	// Zero-value envelope checks
	if c.diag.Diagram != "" || c.diag.LayoutHint != "" {
		return fmt.Errorf("expected zero envelope, got diagram=%q layout=%q",
			c.diag.Diagram, c.diag.LayoutHint)
	}
	if !c.diag.GeneratedAt.IsZero() {
		return fmt.Errorf("expected zero GeneratedAt, got %v", c.diag.GeneratedAt)
	}
	if (c.diag.Repo != diagram.Repo{}) {
		return fmt.Errorf("expected zero Repo, got %+v", c.diag.Repo)
	}
	if len(c.diag.Nodes) != 0 || len(c.diag.Edges) != 0 {
		return fmt.Errorf("expected empty nodes/edges, got %d/%d",
			len(c.diag.Nodes), len(c.diag.Edges))
	}
	if c.diag.Truncated {
		return fmt.Errorf("expected Truncated=false on zero envelope")
	}
	if c.diag.Stats.NodeCount != 0 || c.diag.Stats.EdgeCount != 0 ||
		c.diag.Stats.CappedAt != 0 || len(c.diag.Stats.Skipped) != 0 {
		return fmt.Errorf("expected zero Stats, got %+v", c.diag.Stats)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Initializer + test entrypoint
// ---------------------------------------------------------------------------

func InitializeScenario_diagram_projector_buildcallchain_bfs(ctx *godog.ScenarioContext) {
	c := &buildCallChainBFSCtx{}

	ctx.Step(`^a method M with 2 callees and 3 callers$`, c.aMethodMWith2CalleesAnd3Callers)
	ctx.Step(`^a chain A -> B -> C -> D$`, c.aChainABCD)
	ctx.Step(`^BuildCallChain runs with seed "([^"]*)", depth (\d+), direction "([^"]*)"$`,
		c.buildCallChainRunsWithSeedDepthDirection)
	ctx.Step(`^the diagram contains (\d+) nodes and (\d+) edges$`,
		c.theDiagramContainsNNodesAndMEdges)
	ctx.Step(`^the diagram contains nodes "([^"]*)" and not "([^"]*)"$`,
		c.theDiagramContainsNodesAndNot)
	ctx.Step(`^it returns an error with code "([^"]*)" and a zero-value diagram$`,
		c.itReturnsAnErrorWithCodeAndZeroValueDiagram)
}

func TestE2E_diagram_projector_buildcallchain_bfs(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_diagram_projector_buildcallchain_bfs,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"diagram_projector_buildcallchain_bfs.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog suite failed")
	}
}
