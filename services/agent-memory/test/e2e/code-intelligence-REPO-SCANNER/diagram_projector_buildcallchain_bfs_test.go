//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/diagram"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Constants
// ---------------------------------------------------------------------------

const bfsFixtureRepoURL = "https://github.com/example/bfs-target"

func bfsFixtureRepoID() (fingerprint.RepoID, error) {
	return fingerprint.RepoIDFromURL(bfsFixtureRepoURL)
}

const bfsFixtureSHA = "deadbeefdeadbeefdeadbeefdeadbeef12345678"

// ---------------------------------------------------------------------------
// Scenario state — backed by an ephemeral SQLite database
// ---------------------------------------------------------------------------

type buildCallChainBFSCtx struct {
	sink    *sqlite.Sink
	dbDir   string
	repoID  fingerprint.RepoID
	nodeIDs map[string]string // canonical signature -> nodeID
	diag    diagram.Diagram
	err     error
}

func (c *buildCallChainBFSCtx) cleanup() {
	if c.sink != nil {
		_ = c.sink.Close()
		c.sink = nil
	}
	if c.dbDir != "" {
		_ = os.RemoveAll(c.dbDir)
		c.dbDir = ""
	}
}

// openSink creates a fresh ephemeral SQLite DB and ensures the repo row.
func (c *buildCallChainBFSCtx) openSink() error {
	rid, err := bfsFixtureRepoID()
	if err != nil {
		return fmt.Errorf("bfsFixtureRepoID: %w", err)
	}
	c.repoID = rid
	c.nodeIDs = map[string]string{}

	dir, err := os.MkdirTemp("", "bfs-e2e-*")
	if err != nil {
		return fmt.Errorf("MkdirTemp: %w", err)
	}
	c.dbDir = dir

	sink, err := sqlite.Open(context.Background(), filepath.Join(dir, "graph.db"))
	if err != nil {
		return fmt.Errorf("sqlite.Open: %w", err)
	}
	c.sink = sink

	if _, err := sink.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL:            bfsFixtureRepoURL,
		DefaultBranch:  "main",
		CurrentHeadSHA: bfsFixtureSHA,
		RepoID:         rid,
	}); err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}
	return nil
}

// insertMethod inserts a method node with the given canonical signature
// and records its generated nodeID.
func (c *buildCallChainBFSCtx) insertMethod(sig string) error {
	// Use a parent file node shared across the fixture. Create it
	// idempotently (only the first call inserts).
	const fileSig = "file://bfs_fixture.go"
	if _, exists := c.nodeIDs[fileSig]; !exists {
		// Insert a repo node as the file's parent.
		const repoSig = "repo://bfs-target"
		if _, exists := c.nodeIDs[repoSig]; !exists {
			rec, err := c.sink.InsertNode(context.Background(), graphwriter.NodeInput{
				RepoID:             c.repoID,
				Kind:               "repo",
				CanonicalSignature: repoSig,
				FromSHA:            bfsFixtureSHA,
				AttrsJSON:          json.RawMessage(`{}`),
			})
			if err != nil {
				return fmt.Errorf("InsertNode repo: %w", err)
			}
			c.nodeIDs[repoSig] = rec.NodeID
		}
		rec, err := c.sink.InsertNode(context.Background(), graphwriter.NodeInput{
			RepoID:             c.repoID,
			Kind:               "file",
			CanonicalSignature: fileSig,
			ParentNodeID:       c.nodeIDs["repo://bfs-target"],
			FromSHA:            bfsFixtureSHA,
			AttrsJSON:          json.RawMessage(`{"language":"go"}`),
		})
		if err != nil {
			return fmt.Errorf("InsertNode file: %w", err)
		}
		c.nodeIDs[fileSig] = rec.NodeID
	}

	rec, err := c.sink.InsertNode(context.Background(), graphwriter.NodeInput{
		RepoID:             c.repoID,
		Kind:               "method",
		CanonicalSignature: sig,
		ParentNodeID:       c.nodeIDs[fileSig],
		FromSHA:            bfsFixtureSHA,
		AttrsJSON:          json.RawMessage(`{"language":"go"}`),
	})
	if err != nil {
		return fmt.Errorf("InsertNode method %q: %w", sig, err)
	}
	c.nodeIDs[sig] = rec.NodeID
	return nil
}

// insertStaticCall inserts a static_calls edge between two already-
// inserted nodes identified by their canonical signatures.
func (c *buildCallChainBFSCtx) insertStaticCall(srcSig, dstSig string) error {
	srcID, ok := c.nodeIDs[srcSig]
	if !ok {
		return fmt.Errorf("source node %q not found", srcSig)
	}
	dstID, ok := c.nodeIDs[dstSig]
	if !ok {
		return fmt.Errorf("dest node %q not found", dstSig)
	}
	if _, err := c.sink.InsertEdge(context.Background(), graphwriter.EdgeInput{
		RepoID:    c.repoID,
		Kind:      "static_calls",
		SrcNodeID: srcID,
		DstNodeID: dstID,
		FromSHA:   bfsFixtureSHA,
		AttrsJSON: json.RawMessage(`{}`),
	}); err != nil {
		return fmt.Errorf("InsertEdge %s->%s: %w", srcSig, dstSig, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (c *buildCallChainBFSCtx) aMethodMWith2CalleesAnd3Callers() error {
	c.cleanup()
	if err := c.openSink(); err != nil {
		return err
	}
	// Insert seed + callees + callers
	for _, sig := range []string{"M", "C1", "C2", "P1", "P2", "P3"} {
		if err := c.insertMethod(sig); err != nil {
			return err
		}
	}
	// M -> C1, M -> C2
	for _, callee := range []string{"C1", "C2"} {
		if err := c.insertStaticCall("M", callee); err != nil {
			return err
		}
	}
	// P1 -> M, P2 -> M, P3 -> M
	for _, caller := range []string{"P1", "P2", "P3"} {
		if err := c.insertStaticCall(caller, "M"); err != nil {
			return err
		}
	}
	return nil
}

func (c *buildCallChainBFSCtx) aChainABCD() error {
	c.cleanup()
	if err := c.openSink(); err != nil {
		return err
	}
	for _, sig := range []string{"A", "B", "C", "D"} {
		if err := c.insertMethod(sig); err != nil {
			return err
		}
	}
	for _, pair := range [][2]string{{"A", "B"}, {"B", "C"}, {"C", "D"}} {
		if err := c.insertStaticCall(pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (c *buildCallChainBFSCtx) buildCallChainRunsWithSeedDepthDirection(seed string, depth int, direction string) error {
	// The sink (*sqlite.Sink) satisfies graphsink.Reader, so we pass
	// it directly to BuildCallChain — exercising the full SQLite
	// read path (LookupBySignature, ListEdgesFrom/To, GetNode).
	c.diag, c.err = diagram.BuildCallChain(
		context.Background(), c.sink, seed, depth, direction,
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
	// Build a map of canonical signatures present in the diagram.
	// The SQLite backend generates opaque node IDs (fingerprint
	// hashes), so we match on the label the projector derives from
	// the canonical signature (single-segment sigs like "A" map
	// 1:1 to labels).
	nodeSigs := map[string]bool{}
	for _, n := range c.diag.Nodes {
		nodeSigs[n.Label] = true
	}
	for _, want := range strings.Split(present, ",") {
		if !nodeSigs[want] {
			return fmt.Errorf("expected node with label %q present, got labels=%v", want, nodeSigs)
		}
	}
	for _, notWant := range strings.Split(absent, ",") {
		if nodeSigs[notWant] {
			return fmt.Errorf("node with label %q must NOT appear, got labels=%v", notWant, nodeSigs)
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

	// Clean up the ephemeral DB after each scenario.
	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		c.cleanup()
		return ctx, nil
	})

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
