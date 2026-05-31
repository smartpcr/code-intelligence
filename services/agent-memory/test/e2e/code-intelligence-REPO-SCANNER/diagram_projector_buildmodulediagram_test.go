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
	"testing"
	"time"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/diagram"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphsink/sqlite"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// CLI bootstrap — build the codeintel binary, verify subcommands.
// ---------------------------------------------------------------------------

// bmdBuildBinary compiles cmd/codeintel into a temp dir and
// returns its path. Proves the codeintel CLI binary compiles
// with CGO_ENABLED=1 (required for SQLite + tree-sitter).
func bmdBuildBinary(t *testing.T) string {
	t.Helper()
	root, err := moduleRoot() // defined in parser_coverage_verification_cgo_build_proof_test.go
	if err != nil {
		t.Fatalf("moduleRoot: %v", err)
	}
	tmpDir := t.TempDir()
	binName := "codeintel"
	if runtime.GOOS == "windows" {
		binName += ".exe"
	}
	binPath := filepath.Join(tmpDir, binName)
	cmd := exec.Command("go", "build", "-o", binPath, "./cmd/codeintel")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build cmd/codeintel failed: %v\n%s", err, string(out))
	}
	if _, err := os.Stat(binPath); err != nil {
		t.Fatalf("binary not found after build: %v", err)
	}
	return binPath
}

// ---------------------------------------------------------------------------
// buildModuleDiagramCtx — shared scenario state.
// ---------------------------------------------------------------------------

type buildModuleDiagramCtx struct {
	t         *testing.T
	tempDir   string
	binaryOK bool

	sink      *sqlite.Sink
	repoID    fingerprint.RepoID
	repoIDStr string
	sha       string

	// nodeIDs tracks sink-assigned node IDs by local label.
	nodeIDs map[string]string

	result    diagram.Diagram
	resultErr error
}

func newBuildModuleDiagramCtx(t *testing.T) *buildModuleDiagramCtx {
	return &buildModuleDiagramCtx{
		t:       t,
		tempDir: t.TempDir(),
		nodeIDs: make(map[string]string),
		sha:     "9d1a2c47b3e95f80a162b08c5d3f4e71",
	}
}

func (c *buildModuleDiagramCtx) openSQLiteSink() error {
	if c.sink != nil {
		return nil
	}
	// Name the DB polyglot.db to match the declared bootstrap artifact.
	dbPath := filepath.Join(c.tempDir, "polyglot.db")
	sink, err := sqlite.Open(context.Background(), dbPath)
	if err != nil {
		return fmt.Errorf("sqlite.Open: %w", err)
	}
	c.sink = sink
	return nil
}

func (c *buildModuleDiagramCtx) ensureRepo(url string) error {
	if err := c.openSQLiteSink(); err != nil {
		return err
	}
	repoID, err := fingerprint.RepoIDFromURL(url)
	if err != nil {
		return fmt.Errorf("RepoIDFromURL: %w", err)
	}
	c.repoID = repoID
	c.repoIDStr = repoID.String()

	_, err = c.sink.EnsureRepo(context.Background(), graphwriter.RepoInput{
		URL:            url,
		DefaultBranch:  "main",
		CurrentHeadSHA: c.sha,
		LanguageHints:  []string{"go"},
		RepoID:         repoID,
	})
	if err != nil {
		return fmt.Errorf("EnsureRepo: %w", err)
	}
	_, err = c.sink.EnsureCommit(context.Background(), graphwriter.CommitInput{
		RepoID:      repoID,
		SHA:         c.sha,
		CommittedAt: time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		return fmt.Errorf("EnsureCommit: %w", err)
	}
	return nil
}

func (c *buildModuleDiagramCtx) insertNode(label, kind, sig, parentLabel string) error {
	parentNodeID := ""
	if parentLabel != "" {
		var ok bool
		parentNodeID, ok = c.nodeIDs[parentLabel]
		if !ok {
			return fmt.Errorf("parent %q not found in nodeIDs", parentLabel)
		}
	}
	var attrs json.RawMessage
	if kind != "repo" {
		attrs = json.RawMessage(`{"language":"go"}`)
	}
	rec, err := c.sink.InsertNode(context.Background(), graphwriter.NodeInput{
		RepoID:             c.repoID,
		Kind:               kind,
		CanonicalSignature: sig,
		ParentNodeID:       parentNodeID,
		FromSHA:            c.sha,
		AttrsJSON:          attrs,
	})
	if err != nil {
		return fmt.Errorf("InsertNode(%s): %w", label, err)
	}
	c.nodeIDs[label] = rec.NodeID
	return nil
}

func (c *buildModuleDiagramCtx) insertEdge(srcLabel, dstLabel, kind string) error {
	srcID, ok := c.nodeIDs[srcLabel]
	if !ok {
		return fmt.Errorf("src %q not found in nodeIDs", srcLabel)
	}
	dstID, ok := c.nodeIDs[dstLabel]
	if !ok {
		return fmt.Errorf("dst %q not found in nodeIDs", dstLabel)
	}
	_, err := c.sink.InsertEdge(context.Background(), graphwriter.EdgeInput{
		RepoID:    c.repoID,
		Kind:      kind,
		SrcNodeID: srcID,
		DstNodeID: dstID,
		FromSHA:   c.sha,
	})
	if err != nil {
		return fmt.Errorf("InsertEdge(%s->%s): %w", srcLabel, dstLabel, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Steps — CLI bootstrap (shared Given for all scenarios)
// ---------------------------------------------------------------------------

func (c *buildModuleDiagramCtx) theCodeintelCLIBinaryCompilesAndItsSubcommandsAreRegistered() error {
	binPath := bmdBuildBinary(c.t)
	c.binaryOK = true

	root, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("moduleRoot: %w", err)
	}

	// 1. Exercise codeintel scan against the polyglot fixture directory.
	//    This writes a scan-smoke.db via the real CLI scan path,
	//    matching the declared bootstrap: codeintel scan <path> --store sqlite --out <db>.
	fixtureDir := filepath.Join(root, "internal", "repoindexer", "ast", "testdata", "polyglot")
	scanDB := filepath.Join(c.tempDir, "scan-smoke.db")
	scanCmd := exec.Command(binPath, "scan", fixtureDir, "--store", "sqlite", "--out", scanDB)
	scanOut, scanErr := scanCmd.CombinedOutput()
	if scanErr != nil {
		return fmt.Errorf("codeintel scan failed: %v\n%s", scanErr, string(scanOut))
	}
	c.t.Logf("codeintel scan succeeded: %s", strings.TrimSpace(string(scanOut)))

	// Verify the scan produced a non-empty DB.
	fi, err := os.Stat(scanDB)
	if err != nil || fi.Size() == 0 {
		return fmt.Errorf("codeintel scan produced empty or missing DB: %s", scanDB)
	}

	// 2. Exercise codeintel diagram module against the scanned DB.
	//    Verifies the full CLI pipeline: scan → polyglot.db → diagram JSON.
	diagramCmd := exec.Command(binPath, "diagram", "module",
		"--store", "sqlite", "--db", scanDB)
	diagramOut, diagramErr := diagramCmd.CombinedOutput()
	if diagramErr != nil {
		return fmt.Errorf("codeintel diagram module failed: %v\n%s", diagramErr, string(diagramOut))
	}

	// Verify the diagram output is valid JSON with expected top-level keys.
	var diagramJSON map[string]interface{}
	if err := json.Unmarshal(diagramOut, &diagramJSON); err != nil {
		return fmt.Errorf("diagram module output is not valid JSON: %v", err)
	}
	if _, ok := diagramJSON["nodes"]; !ok {
		return fmt.Errorf("diagram module output missing 'nodes' key")
	}
	if _, ok := diagramJSON["edges"]; !ok {
		return fmt.Errorf("diagram module output missing 'edges' key")
	}
	c.t.Logf("codeintel diagram module produced valid JSON with nodes + edges")

	return nil
}

// ---------------------------------------------------------------------------
// Steps — module-tree-shape
// ---------------------------------------------------------------------------

func (c *buildModuleDiagramCtx) aSQLiteGraphStoreWith1Repo2PackagesAnd5Files() error {
	if err := c.ensureRepo("https://example.com/example/app"); err != nil {
		return err
	}
	if err := c.insertNode("repo", "repo",
		"https://example.com/example/app::repo", ""); err != nil {
		return err
	}
	if err := c.insertNode("pkg-a", "package",
		"example/app/pkg/a", "repo"); err != nil {
		return err
	}
	if err := c.insertNode("pkg-b", "package",
		"example/app/pkg/b", "repo"); err != nil {
		return err
	}
	for i, name := range []string{"one", "two", "three"} {
		label := fmt.Sprintf("file-a%d", i+1)
		sig := fmt.Sprintf("example/app/pkg/a/%s.go", name)
		if err := c.insertNode(label, "file", sig, "pkg-a"); err != nil {
			return err
		}
	}
	for i, name := range []string{"one", "two"} {
		label := fmt.Sprintf("file-b%d", i+1)
		sig := fmt.Sprintf("example/app/pkg/b/%s.go", name)
		if err := c.insertNode(label, "file", sig, "pkg-b"); err != nil {
			return err
		}
	}
	return nil
}

func (c *buildModuleDiagramCtx) buildModuleDiagramRunsAtGranularity(granularity string) error {
	c.result, c.resultErr = diagram.BuildModuleDiagram(
		context.Background(), c.sink, c.repoID, granularity,
	)
	return c.resultErr
}

func (c *buildModuleDiagramCtx) theDiagramHas3NodesAnd2ContainsEdges() error {
	if got := len(c.result.Nodes); got != 3 {
		return fmt.Errorf("expected 3 nodes (repo + 2 packages), got %d: %+v",
			got, bmdNodeKinds(c.result.Nodes))
	}
	if c.result.Nodes[0].Kind != "repo" {
		return fmt.Errorf("first node kind = %q, want repo", c.result.Nodes[0].Kind)
	}
	containsCount := 0
	for _, e := range c.result.Edges {
		if e.Kind == "contains" {
			containsCount++
		}
	}
	if containsCount != 2 {
		return fmt.Errorf("expected 2 contains edges, got %d", containsCount)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Steps — imports-rollup-weight
// ---------------------------------------------------------------------------

func (c *buildModuleDiagramCtx) aSQLiteGraphStoreWhere3FilesInPkgAEachImport2FilesInPkgB() error {
	if err := c.ensureRepo("https://example.com/example/rollup6"); err != nil {
		return err
	}
	if err := c.insertNode("repo", "repo",
		"https://example.com/example/rollup6::repo", ""); err != nil {
		return err
	}
	if err := c.insertNode("pkg-a", "package",
		"https://example.com/example/rollup6::package::pkg/a", "repo"); err != nil {
		return err
	}
	if err := c.insertNode("pkg-b", "package",
		"https://example.com/example/rollup6::package::pkg/b", "repo"); err != nil {
		return err
	}
	for i := 1; i <= 3; i++ {
		label := fmt.Sprintf("file-a%d", i)
		sig := fmt.Sprintf("pkg/a/f%d.go", i)
		if err := c.insertNode(label, "file", sig, "pkg-a"); err != nil {
			return err
		}
	}
	for j := 1; j <= 2; j++ {
		label := fmt.Sprintf("file-b%d", j)
		sig := fmt.Sprintf("pkg/b/g%d.go", j)
		if err := c.insertNode(label, "file", sig, "pkg-b"); err != nil {
			return err
		}
	}
	// 3 source files × 2 destination files = 6 distinct import edges.
	for i := 1; i <= 3; i++ {
		for j := 1; j <= 2; j++ {
			srcLabel := fmt.Sprintf("file-a%d", i)
			dstLabel := fmt.Sprintf("file-b%d", j)
			if err := c.insertEdge(srcLabel, dstLabel, "imports"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *buildModuleDiagramCtx) theProjectorRunsAtGranularity(granularity string) error {
	c.result, c.resultErr = diagram.BuildModuleDiagram(
		context.Background(), c.sink, c.repoID, granularity,
	)
	return c.resultErr
}

func (c *buildModuleDiagramCtx) exactlyOneImportsEdgePkgAToPkgBWithWeight6IsEmitted() error {
	var importsEdges []diagram.Edge
	for _, e := range c.result.Edges {
		if e.Kind == "imports" {
			importsEdges = append(importsEdges, e)
		}
	}
	if len(importsEdges) != 1 {
		return fmt.Errorf("expected exactly 1 rolled-up imports edge, got %d: %+v",
			len(importsEdges), importsEdges)
	}
	if importsEdges[0].Weight != 6 {
		return fmt.Errorf("imports edge weight = %d, want 6", importsEdges[0].Weight)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Steps — granularity-file
// ---------------------------------------------------------------------------

func (c *buildModuleDiagramCtx) aSQLiteGraphStoreWithImportsAtGranularityFile() error {
	if err := c.ensureRepo("https://example.com/example/rollup"); err != nil {
		return err
	}
	if err := c.insertNode("repo", "repo",
		"https://example.com/example/rollup::repo", ""); err != nil {
		return err
	}
	if err := c.insertNode("pkg-a", "package",
		"https://example.com/example/rollup::package::pkg/a", "repo"); err != nil {
		return err
	}
	if err := c.insertNode("pkg-fmt", "package",
		"https://example.com/example/rollup::package::fmt", "repo"); err != nil {
		return err
	}
	if err := c.insertNode("pkg-log", "package",
		"https://example.com/example/rollup::package::log", "repo"); err != nil {
		return err
	}
	for i := 1; i <= 3; i++ {
		label := fmt.Sprintf("file-a%d", i)
		sig := fmt.Sprintf("pkg/a/f%d.go", i)
		if err := c.insertNode(label, "file", sig, "pkg-a"); err != nil {
			return err
		}
	}
	// Each of 3 files imports fmt and log (file -> package edges).
	for i := 1; i <= 3; i++ {
		srcLabel := fmt.Sprintf("file-a%d", i)
		for _, dstLabel := range []string{"pkg-fmt", "pkg-log"} {
			if err := c.insertEdge(srcLabel, dstLabel, "imports"); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *buildModuleDiagramCtx) fileNodesAppearAndPerFileImportsEdgesAreNOTRolledUp() error {
	fileNodeCount := 0
	for _, n := range c.result.Nodes {
		if n.Kind == "file" {
			fileNodeCount++
		}
	}
	if fileNodeCount != 3 {
		return fmt.Errorf("expected 3 file nodes, got %d", fileNodeCount)
	}
	importsCount := 0
	for _, e := range c.result.Edges {
		if e.Kind != "imports" {
			continue
		}
		importsCount++
		if e.Weight != 1 {
			return fmt.Errorf("per-file imports edge weight = %d, want 1: %+v",
				e.Weight, e)
		}
	}
	if importsCount != 6 {
		return fmt.Errorf("expected 6 per-file imports edges (3 files × 2 imports), got %d",
			importsCount)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func bmdNodeKinds(nodes []diagram.Node) map[string]int {
	m := make(map[string]int)
	for _, n := range nodes {
		m[n.Kind]++
	}
	return m
}

// ---------------------------------------------------------------------------
// Initializer + test entrypoint
// ---------------------------------------------------------------------------

var buildModuleDiagramTestingT *testing.T

func InitializeScenario_diagram_projector_buildmodulediagram(ctx *godog.ScenarioContext) {
	var c *buildModuleDiagramCtx

	ctx.Before(func(gCtx context.Context, sc *godog.Scenario) (context.Context, error) {
		c = newBuildModuleDiagramCtx(buildModuleDiagramTestingT)
		return gCtx, nil
	})
	ctx.After(func(gCtx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if c != nil && c.sink != nil {
			_ = c.sink.Close()
		}
		return gCtx, nil
	})

	// CLI bootstrap — build binary, verify scan + diagram subcommands.
	ctx.Step(`^the codeintel CLI binary compiles and its scan and diagram subcommands are registered$`,
		func() error { return c.theCodeintelCLIBinaryCompilesAndItsSubcommandsAreRegistered() })

	// module-tree-shape
	ctx.Step(`^a SQLite polyglot\.db with 1 repo, 2 packages, and 5 files$`,
		func() error { return c.aSQLiteGraphStoreWith1Repo2PackagesAnd5Files() })
	ctx.Step(`^BuildModuleDiagram runs at granularity "([^"]*)"$`,
		func(g string) error { return c.buildModuleDiagramRunsAtGranularity(g) })
	ctx.Step(`^the diagram has 3 nodes and 2 contains edges$`,
		func() error { return c.theDiagramHas3NodesAnd2ContainsEdges() })

	// imports-rollup-weight
	ctx.Step(`^a SQLite polyglot\.db where 3 files in pkg/a each import 2 files in pkg/b$`,
		func() error { return c.aSQLiteGraphStoreWhere3FilesInPkgAEachImport2FilesInPkgB() })
	ctx.Step(`^the projector runs at granularity "([^"]*)"$`,
		func(g string) error { return c.theProjectorRunsAtGranularity(g) })
	ctx.Step(`^exactly one imports edge "pkg:a -> pkg:b" with weight 6 is emitted$`,
		func() error { return c.exactlyOneImportsEdgePkgAToPkgBWithWeight6IsEmitted() })

	// granularity-file
	ctx.Step(`^a SQLite polyglot\.db with imports at granularity "([^"]*)"$`,
		func(_ string) error { return c.aSQLiteGraphStoreWithImportsAtGranularityFile() })
	ctx.Step(`^file Nodes appear and per-file imports edges are NOT rolled up$`,
		func() error { return c.fileNodesAppearAndPerFileImportsEdgesAreNOTRolledUp() })
}

func TestE2E_diagram_projector_buildmodulediagram(t *testing.T) {
	buildModuleDiagramTestingT = t

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_diagram_projector_buildmodulediagram,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"diagram_projector_buildmodulediagram.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog suite failed")
	}
}
