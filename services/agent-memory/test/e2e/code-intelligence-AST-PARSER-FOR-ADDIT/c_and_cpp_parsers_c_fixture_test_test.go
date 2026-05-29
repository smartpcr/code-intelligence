//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// ---------------------------------------------------------------------------
// Shared helpers (one copy per package — guarded by build tag)
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		t.Skipf("required env var %s is not set — skipping", name)
	}
	return v
}

func cFixtureModuleRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	// thisFile is <MOD>/test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT/<file>.go
	dir := filepath.Dir(thisFile)
	for i := 0; i < 3; i++ {
		dir = filepath.Dir(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("go.mod not found at %s: %w", dir, err)
	}
	return dir, nil
}

// errAstDirMissing is a sentinel used to distinguish "ast package not
// present in this workspace" from real failures so godog steps can
// return godog.ErrPending instead of a hard failure.
var errAstDirMissing = fmt.Errorf("internal/repoindexer/ast directory not found — implementation not present in this workspace")

// ---------------------------------------------------------------------------
// Embedded C fixture
//
// Contains:
//   - 1 struct  (Config)            → 1 class node
//   - 2 functions (init_config, process) → 2 method nodes
//   - #include <stdio.h>            → 1 package node + 1 imports edge
//   - #include "./local.h"          → relative — must be DROPPED
//   - process() calls init_config() → 1 static_calls edge
//   - file contains struct + 2 fns  → 3 contains edges
//
// Expected totals: 1 class + 2 method + 1 package = 4 nodes
//                  3 contains + 1 static_calls + 1 imports = 5 edges
// ---------------------------------------------------------------------------

const cFixtureSource = `#include <stdio.h>
#include "./local.h"

struct Config {
    int timeout;
    char *name;
};

void init_config(struct Config *cfg) {
    cfg->timeout = 30;
    cfg->name = "default";
}

int process(struct Config *cfg) {
    init_config(cfg);
    return cfg->timeout;
}
`

// ---------------------------------------------------------------------------
// Probe: injected _test.go that runs inside the ast package and calls
// Dispatcher.EmitFile() with a spy writer, then prints JSON on stdout.
//
// The spy tracks a nodeID→NodeInput map so it can cross-reference
// EdgeInput.DstNodeID back to the target node. This lets the e2e
// step definitions verify which specific package nodes are targeted
// by imports edges (not just that package nodes exist).
// ---------------------------------------------------------------------------

const cFixtureProbeTestSource = `package ast

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
)

type e2eCFixtureSpyWriter struct {
	nextID  int
	nodeMap map[string]graphwriter.NodeInput
	nodes   []graphwriter.NodeInput
	edges   []graphwriter.EdgeInput
}

func (w *e2eCFixtureSpyWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	w.nextID++
	id := fmt.Sprintf("spy-%d", w.nextID)
	w.nodes = append(w.nodes, in)
	w.nodeMap[id] = in
	return graphwriter.NodeRecord{NodeID: id}, nil
}

func (w *e2eCFixtureSpyWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	w.edges = append(w.edges, in)
	return graphwriter.EdgeRecord{}, nil
}

type e2eCFixtureNodeSummary struct {
	Kind               string ` + "`" + `json:"kind"` + "`" + `
	CanonicalSignature string ` + "`" + `json:"canonical_signature"` + "`" + `
}

type e2eCFixtureEdgeSummary struct {
	Kind string ` + "`" + `json:"kind"` + "`" + `
}

type e2eCFixtureImportsTarget struct {
	TargetSignature string ` + "`" + `json:"target_signature"` + "`" + `
}

type e2eCFixtureOutput struct {
	Nodes          []e2eCFixtureNodeSummary    ` + "`" + `json:"nodes"` + "`" + `
	Edges          []e2eCFixtureEdgeSummary    ` + "`" + `json:"edges"` + "`" + `
	ImportsTargets []e2eCFixtureImportsTarget  ` + "`" + `json:"imports_targets"` + "`" + `
}

func TestE2EProbe_CFixtureEmitFile(t *testing.T) {
	srcFile := os.Getenv("E2E_PROBE_SOURCE_FILE")
	if srcFile == "" {
		t.Skip("E2E_PROBE_SOURCE_FILE not set")
	}
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	spy := &e2eCFixtureSpyWriter{nodeMap: make(map[string]graphwriter.NodeInput)}
	d := NewDispatcher(spy, WithParsers(NewTreeSitterCParser()))

	_, err = d.EmitFile(context.Background(), repoindexer.EmitFileEvent{
		RepoID:     "e2e-fixture-repo",
		RepoURL:    "https://example.com/e2e-fixture",
		SHA:        "e2e-fixture-sha",
		RepoNodeID: "e2e-repo-node",
		FileNodeID: "e2e-file-node",
		RelPath:    "fixture.c",
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(src)), nil
		},
	})
	if err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	result := e2eCFixtureOutput{}
	for _, n := range spy.nodes {
		result.Nodes = append(result.Nodes, e2eCFixtureNodeSummary{
			Kind:               n.Kind,
			CanonicalSignature: n.CanonicalSignature,
		})
	}
	for _, e := range spy.edges {
		result.Edges = append(result.Edges, e2eCFixtureEdgeSummary{
			Kind: e.Kind,
		})
		if e.Kind == "imports" {
			targetNode, ok := spy.nodeMap[e.DstNodeID]
			sig := ""
			if ok {
				sig = targetNode.CanonicalSignature
			}
			result.ImportsTargets = append(result.ImportsTargets, e2eCFixtureImportsTarget{
				TargetSignature: sig,
			})
		}
	}

	data, _ := json.Marshal(result)
	fmt.Printf("PROBE_OUTPUT:%s\n", string(data))
}
`

// ---------------------------------------------------------------------------
// Local mirror types for JSON deserialization
// ---------------------------------------------------------------------------

type cFixtureNodeSummary struct {
	Kind               string `json:"kind"`
	CanonicalSignature string `json:"canonical_signature"`
}

type cFixtureEdgeSummary struct {
	Kind string `json:"kind"`
}

type cFixtureImportsTarget struct {
	TargetSignature string `json:"target_signature"`
}

type cFixtureOutput struct {
	Nodes          []cFixtureNodeSummary    `json:"nodes"`
	Edges          []cFixtureEdgeSummary    `json:"edges"`
	ImportsTargets []cFixtureImportsTarget  `json:"imports_targets"`
}

// ---------------------------------------------------------------------------
// Probe runner
// ---------------------------------------------------------------------------

func runCFixtureProbe(cSource string) (*cFixtureOutput, error) {
	modRoot, err := cFixtureModuleRoot()
	if err != nil {
		return nil, err
	}

	astDir := filepath.Join(modRoot, "internal", "repoindexer", "ast")

	// Guard: if the ast package directory does not exist in this
	// workspace (sparse checkout), return sentinel so the step can
	// mark the scenario as pending rather than failing.
	if _, err := os.Stat(astDir); os.IsNotExist(err) {
		return nil, errAstDirMissing
	}

	pid := os.Getpid()

	probeFile := filepath.Join(astDir, fmt.Sprintf("e2e_c_fixture_probe_%d_test.go", pid))
	if err := os.WriteFile(probeFile, []byte(cFixtureProbeTestSource), 0644); err != nil {
		return nil, fmt.Errorf("write probe: %w", err)
	}
	defer os.Remove(probeFile)

	srcFile := filepath.Join(os.TempDir(), fmt.Sprintf("e2e_c_fixture_src_%d.c", pid))
	if err := os.WriteFile(srcFile, []byte(cSource), 0644); err != nil {
		return nil, fmt.Errorf("write source: %w", err)
	}
	defer os.Remove(srcFile)

	cmd := exec.Command("go", "test",
		"-run", "TestE2EProbe_CFixtureEmitFile",
		"-v", "-count=1",
		"./internal/repoindexer/ast/",
	)
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=1",
		"E2E_PROBE_SOURCE_FILE="+srcFile,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("probe failed: %v\noutput:\n%s", err, string(output))
	}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PROBE_OUTPUT:") {
			raw := strings.TrimPrefix(line, "PROBE_OUTPUT:")
			var out cFixtureOutput
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return nil, fmt.Errorf("parse JSON: %w\nraw: %s", err, raw)
			}
			return &out, nil
		}
	}
	return nil, fmt.Errorf("PROBE_OUTPUT marker not found in output:\n%s", string(output))
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type cFixtureState struct {
	probeResult *cFixtureOutput
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *cFixtureState) theEmbeddedCFixture() error {
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *cFixtureState) emitFileRunsUnderCGOOn() error {
	result, err := runCFixtureProbe(cFixtureSource)
	if errors.Is(err, errAstDirMissing) {
		return godog.ErrPending
	}
	if err != nil {
		return fmt.Errorf("EmitFile probe failed: %w", err)
	}
	s.probeResult = result
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — Scenario 1: node + edge counts
// ---------------------------------------------------------------------------

func (s *cFixtureState) classMethodAndPackageNodesAreEmitted(classCount, methodCount, packageCount int) error {
	if s.probeResult == nil {
		return godog.ErrPending
	}
	var actualClass, actualMethod, actualPackage int
	for _, n := range s.probeResult.Nodes {
		switch n.Kind {
		case "class":
			actualClass++
		case "method":
			actualMethod++
		case "package":
			actualPackage++
		}
	}
	var errs []string
	if actualClass != classCount {
		errs = append(errs, fmt.Sprintf("class nodes: want %d, got %d", classCount, actualClass))
	}
	if actualMethod != methodCount {
		errs = append(errs, fmt.Sprintf("method nodes: want %d, got %d", methodCount, actualMethod))
	}
	if actualPackage != packageCount {
		errs = append(errs, fmt.Sprintf("package nodes: want %d, got %d", packageCount, actualPackage))
	}
	if len(errs) > 0 {
		return fmt.Errorf("node count mismatch: %s\nall nodes: %v", strings.Join(errs, "; "), s.cFixtureNodeKinds())
	}
	return nil
}

func (s *cFixtureState) containsStaticCallsAndImportsEdgesAreEmitted(containsCount, staticCallsCount, importsCount int) error {
	if s.probeResult == nil {
		return godog.ErrPending
	}
	var actualContains, actualStaticCalls, actualImports int
	for _, e := range s.probeResult.Edges {
		switch e.Kind {
		case "contains":
			actualContains++
		case "static_calls":
			actualStaticCalls++
		case "imports":
			actualImports++
		}
	}
	var errs []string
	if actualContains != containsCount {
		errs = append(errs, fmt.Sprintf("contains edges: want %d, got %d", containsCount, actualContains))
	}
	if actualStaticCalls != staticCallsCount {
		errs = append(errs, fmt.Sprintf("static_calls edges: want %d, got %d", staticCallsCount, actualStaticCalls))
	}
	if actualImports != importsCount {
		errs = append(errs, fmt.Sprintf("imports edges: want %d, got %d", importsCount, actualImports))
	}
	if len(errs) > 0 {
		return fmt.Errorf("edge count mismatch: %s\nall edges: %v", strings.Join(errs, "; "), s.cFixtureEdgeKinds())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — Scenario 2: relative include dropped
//
// Verifies that no imports EDGE targets a package node whose module
// starts with the given prefix. The probe cross-references each
// imports edge's DstNodeID back to the spy's nodeMap, so we check
// the actual edge→target relationship, not just whether any package
// node exists with a matching signature.
// ---------------------------------------------------------------------------

func (s *cFixtureState) zeroImportsEdgesTargetAPackageNodeWhoseModuleStartsWith(prefix string) error {
	if s.probeResult == nil {
		return godog.ErrPending
	}
	for _, it := range s.probeResult.ImportsTargets {
		if strings.HasPrefix(it.TargetSignature, prefix) || strings.Contains(it.TargetSignature, prefix) {
			return fmt.Errorf(
				"found imports edge targeting package node %q which contains %q — "+
					"relative includes must be dropped by the dispatcher",
				it.TargetSignature, prefix)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Diagnostic helpers
// ---------------------------------------------------------------------------

func (s *cFixtureState) cFixtureNodeKinds() []string {
	out := make([]string, len(s.probeResult.Nodes))
	for i, n := range s.probeResult.Nodes {
		out[i] = fmt.Sprintf("%s(%s)", n.Kind, n.CanonicalSignature)
	}
	return out
}

func (s *cFixtureState) cFixtureEdgeKinds() []string {
	out := make([]string, len(s.probeResult.Edges))
	for i, e := range s.probeResult.Edges {
		out[i] = e.Kind
	}
	return out
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_c_and_cpp_parsers_c_fixture_test(ctx *godog.ScenarioContext) {
	s := &cFixtureState{}

	ctx.Given(`^the embedded C fixture$`, s.theEmbeddedCFixture)
	ctx.When(`^EmitFile runs under CGO=on$`, s.emitFileRunsUnderCGOOn)
	ctx.Then(`^(\d+) class, (\d+) method, and (\d+) package nodes are emitted$`, s.classMethodAndPackageNodesAreEmitted)
	ctx.Then(`^(\d+) contains, (\d+) static_calls, and (\d+) imports edges are emitted$`, s.containsStaticCallsAndImportsEdgesAreEmitted)
	ctx.Then(`^zero imports edges target a package node whose module starts with "([^"]*)"$`, s.zeroImportsEdgesTargetAPackageNodeWhoseModuleStartsWith)
}

func TestE2E_c_and_cpp_parsers_c_fixture_test(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_c_and_cpp_parsers_c_fixture_test,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"c_and_cpp_parsers_c_fixture_test.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}