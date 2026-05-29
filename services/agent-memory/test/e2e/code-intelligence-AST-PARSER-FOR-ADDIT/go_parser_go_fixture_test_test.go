//go:build e2e

package e2e

import (
	"encoding/json"
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
// Shared helpers (unique per-stage to avoid collisions in e2e package)
// ---------------------------------------------------------------------------

func goFixtureModuleRoot() (string, error) {
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

// ---------------------------------------------------------------------------
// Canonical Go fixture — 1 struct, 1 pointer-receiver method calling a
// free function, 1 free function, 1 import, 1 static call, plus a
// receiver-field read (g.prefix).
// ---------------------------------------------------------------------------

const goFixtureSrc = `package hello

import "fmt"

type Greeter struct {
	prefix string
}

func (g *Greeter) Greet(name string) string {
	return formatGreeting(g.prefix, name)
}

func formatGreeting(prefix, name string) string {
	return fmt.Sprintf("%s %s", prefix, name)
}
`

// ---------------------------------------------------------------------------
// Writes fixture — pointer-receiver method assigning g.prefix = name.
// ---------------------------------------------------------------------------

const goWritesFixtureSrc = `package hello

type Greeter struct {
	prefix string
}

func (g *Greeter) SetPrefix(name string) {
	g.prefix = name
}
`

// ---------------------------------------------------------------------------
// Probe: injected _test.go that runs inside the ast package and calls
// Dispatcher.EmitFile() with a spy writer, then prints JSON on stdout.
//
// The spy tracks a nodeID→NodeInput map so it can cross-reference
// EdgeInput.DstNodeID back to the target node. This lets the E2E
// step definitions verify which specific nodes are targeted by edges.
// ---------------------------------------------------------------------------

const goFixtureProbeTestSource = `package ast

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

type e2eGoFixtureSpyWriter struct {
	nextID  int
	nodeMap map[string]graphwriter.NodeInput
	nodes   []graphwriter.NodeInput
	edges   []graphwriter.EdgeInput
}

func (w *e2eGoFixtureSpyWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	w.nextID++
	id := fmt.Sprintf("spy-%d", w.nextID)
	w.nodes = append(w.nodes, in)
	w.nodeMap[id] = in
	return graphwriter.NodeRecord{NodeID: id}, nil
}

func (w *e2eGoFixtureSpyWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	w.edges = append(w.edges, in)
	return graphwriter.EdgeRecord{}, nil
}

type e2eGoFixtureNodeSummary struct {
	Kind               string ` + "`" + `json:"kind"` + "`" + `
	CanonicalSignature string ` + "`" + `json:"canonical_signature"` + "`" + `
}

type e2eGoFixtureEdgeSummary struct {
	Kind string ` + "`" + `json:"kind"` + "`" + `
}

type e2eGoFixtureWritesTarget struct {
	TargetSignature string ` + "`" + `json:"target_signature"` + "`" + `
	TargetKind      string ` + "`" + `json:"target_kind"` + "`" + `
	UnresolvedDstID string ` + "`" + `json:"unresolved_dst_id,omitempty"` + "`" + `
}

type e2eGoFixtureOutput struct {
	Nodes         []e2eGoFixtureNodeSummary  ` + "`" + `json:"nodes"` + "`" + `
	Edges         []e2eGoFixtureEdgeSummary  ` + "`" + `json:"edges"` + "`" + `
	WritesTargets []e2eGoFixtureWritesTarget ` + "`" + `json:"writes_targets"` + "`" + `
}

func TestE2EProbe_GoFixtureEmitFile(t *testing.T) {
	srcFile := os.Getenv("E2E_PROBE_SOURCE_FILE")
	if srcFile == "" {
		t.Skip("E2E_PROBE_SOURCE_FILE not set")
	}
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	spy := &e2eGoFixtureSpyWriter{nodeMap: make(map[string]graphwriter.NodeInput)}
	d := NewDispatcher(spy, WithParsers(NewTreeSitterGoParser()))

	_, err = d.EmitFile(context.Background(), repoindexer.EmitFileEvent{
		RepoID:     "e2e-fixture-repo",
		RepoURL:    "https://example.com/e2e-fixture",
		SHA:        "e2e-fixture-sha",
		RepoNodeID: "e2e-repo-node",
		FileNodeID: "e2e-file-node",
		RelPath:    "src/hello.go",
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(src)), nil
		},
	})
	if err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	result := e2eGoFixtureOutput{}
	for _, n := range spy.nodes {
		result.Nodes = append(result.Nodes, e2eGoFixtureNodeSummary{
			Kind:               n.Kind,
			CanonicalSignature: n.CanonicalSignature,
		})
	}
	for _, e := range spy.edges {
		result.Edges = append(result.Edges, e2eGoFixtureEdgeSummary{
			Kind: e.Kind,
		})
		if e.Kind == "writes" {
			targetNode, ok := spy.nodeMap[e.DstNodeID]
			sig := ""
			kind := ""
			unresolvedID := ""
			if ok {
				sig = targetNode.CanonicalSignature
				kind = targetNode.Kind
			} else {
				unresolvedID = e.DstNodeID
			}
			result.WritesTargets = append(result.WritesTargets, e2eGoFixtureWritesTarget{
				TargetSignature: sig,
				TargetKind:      kind,
				UnresolvedDstID: unresolvedID,
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

type goFixtureNodeSummary struct {
	Kind               string `json:"kind"`
	CanonicalSignature string `json:"canonical_signature"`
}

type goFixtureEdgeSummary struct {
	Kind string `json:"kind"`
}

type goFixtureWritesTarget struct {
	TargetSignature string `json:"target_signature"`
	TargetKind      string `json:"target_kind"`
	UnresolvedDstID string `json:"unresolved_dst_id,omitempty"`
}

type goFixtureOutput struct {
	Nodes         []goFixtureNodeSummary  `json:"nodes"`
	Edges         []goFixtureEdgeSummary  `json:"edges"`
	WritesTargets []goFixtureWritesTarget `json:"writes_targets"`
}

// ---------------------------------------------------------------------------
// Probe runner — injects the probe test into the ast package directory,
// runs it via `go test`, and parses the JSON output. Returns an error
// (no fallback) when the implementation is absent — the test FAILS
// honestly rather than producing a false-positive E2E result.
// ---------------------------------------------------------------------------

func runGoFixtureProbe(goSource string) (*goFixtureOutput, error) {
	modRoot, err := goFixtureModuleRoot()
	if err != nil {
		return nil, err
	}

	astDir := filepath.Join(modRoot, "internal", "repoindexer", "ast")

	// Require the real dispatcher — no fallback, no skip, no substitute.
	// The test ONLY passes when the real EmitFile pipeline is available.
	dispatcherFile := filepath.Join(astDir, "dispatcher.go")
	if _, err := os.Stat(dispatcherFile); os.IsNotExist(err) {
		return nil, fmt.Errorf(
			"real Dispatcher.EmitFile pipeline required: %s absent — "+
				"this E2E test only passes when the implementation workstream "+
				"(phase-go-parser/stage-go-fixture-test) is merged",
			dispatcherFile)
	}

	pid := os.Getpid()

	probeFile := filepath.Join(astDir, fmt.Sprintf("e2e_go_fixture_probe_%d_test.go", pid))
	if err := os.WriteFile(probeFile, []byte(goFixtureProbeTestSource), 0644); err != nil {
		return nil, fmt.Errorf("write probe: %w", err)
	}
	defer os.Remove(probeFile)

	srcFile := filepath.Join(os.TempDir(), fmt.Sprintf("e2e_go_fixture_src_%d.go", pid))
	if err := os.WriteFile(srcFile, []byte(goSource), 0644); err != nil {
		return nil, fmt.Errorf("write source: %w", err)
	}
	defer os.Remove(srcFile)

	cmd := exec.Command("go", "test",
		"-run", "TestE2EProbe_GoFixtureEmitFile",
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
			var out goFixtureOutput
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

type goFixtureState struct {
	fixtureSrc  string
	probeResult *goFixtureOutput
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *goFixtureState) theEmbeddedGoFixture() error {
	s.fixtureSrc = goFixtureSrc
	return nil
}

func (s *goFixtureState) aGoFixtureWithMethodBody(body string) error {
	if body != "g.prefix = name" {
		return fmt.Errorf("unexpected method body %q; fixture is hard-coded for \"g.prefix = name\"", body)
	}
	s.fixtureSrc = goWritesFixtureSrc
	return nil
}

// ---------------------------------------------------------------------------
// When steps — runs the probe that calls the real Dispatcher.EmitFile()
// with a spy writer inside the ast package.
// ---------------------------------------------------------------------------

func (s *goFixtureState) emitFileRunsUnderCGOOn() error {
	result, err := runGoFixtureProbe(s.fixtureSrc)
	if err != nil {
		return fmt.Errorf("EmitFile probe failed: %w", err)
	}
	s.probeResult = result
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — Scenario 1: node counts from emitted graph nodes.
// ---------------------------------------------------------------------------

func (s *goFixtureState) nodeCountsEmitted(classN, methodN, packageN int) error {
	if s.probeResult == nil {
		return fmt.Errorf("probe result is nil — EmitFile step did not execute successfully")
	}
	counts := map[string]int{}
	for _, n := range s.probeResult.Nodes {
		counts[n.Kind]++
	}
	var errs []string
	if counts["class"] != classN {
		errs = append(errs, fmt.Sprintf("class nodes: want %d, got %d", classN, counts["class"]))
	}
	if counts["method"] != methodN {
		errs = append(errs, fmt.Sprintf("method nodes: want %d, got %d", methodN, counts["method"]))
	}
	if counts["package"] != packageN {
		errs = append(errs, fmt.Sprintf("package nodes: want %d, got %d", packageN, counts["package"]))
	}
	if len(errs) > 0 {
		return fmt.Errorf("node count mismatch: %s\nall nodes: %v",
			strings.Join(errs, "; "), goFixtureNodeKinds(s.probeResult))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — Scenario 1: edge counts from emitted graph edges.
// ---------------------------------------------------------------------------

func (s *goFixtureState) edgeCountsEmitted(containsN, staticCallsN, importsN int) error {
	if s.probeResult == nil {
		return fmt.Errorf("probe result is nil — EmitFile step did not execute successfully")
	}
	counts := map[string]int{}
	for _, e := range s.probeResult.Edges {
		counts[e.Kind]++
	}
	var errs []string
	if counts["contains"] != containsN {
		errs = append(errs, fmt.Sprintf("contains: want %d, got %d", containsN, counts["contains"]))
	}
	if counts["static_calls"] != staticCallsN {
		errs = append(errs, fmt.Sprintf("static_calls: want %d, got %d", staticCallsN, counts["static_calls"]))
	}
	if counts["imports"] != importsN {
		errs = append(errs, fmt.Sprintf("imports: want %d, got %d", importsN, counts["imports"]))
	}
	if len(errs) > 0 {
		return fmt.Errorf("edge count mismatch: %s\nall edges: %v",
			strings.Join(errs, "; "), goFixtureEdgeKinds(s.probeResult))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — Scenario 2: pointer receiver fingerprint from emitted nodes.
// ---------------------------------------------------------------------------

func (s *goFixtureState) capturedMethodSignatureContains(sub string) error {
	if s.probeResult == nil {
		return fmt.Errorf("probe result is nil — EmitFile step did not execute successfully")
	}
	for _, n := range s.probeResult.Nodes {
		if n.Kind == "method" && strings.Contains(n.CanonicalSignature, sub) {
			return nil
		}
	}
	var sigs []string
	for _, n := range s.probeResult.Nodes {
		if n.Kind == "method" {
			sigs = append(sigs, n.CanonicalSignature)
		}
	}
	return fmt.Errorf("no emitted method node signature contains %q; got %v", sub, sigs)
}

// ---------------------------------------------------------------------------
// Then steps — Scenario 3: writes edge from emitted graph edges.
// ---------------------------------------------------------------------------

func (s *goFixtureState) writesEdgeFromMethodToFieldMember(member string) error {
	if s.probeResult == nil {
		return fmt.Errorf("probe result is nil — EmitFile step did not execute successfully")
	}
	writeCount := 0
	for _, wt := range s.probeResult.WritesTargets {
		if wt.TargetKind == "field" && strings.Contains(wt.TargetSignature, member) {
			writeCount++
		}
	}
	if writeCount != 1 {
		return fmt.Errorf("writes edges to field member %q = %d; want exactly 1 with TargetKind=field\nwrites targets: %v",
			member, writeCount, s.probeResult.WritesTargets)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Diagnostic helpers
// ---------------------------------------------------------------------------

func goFixtureNodeKinds(out *goFixtureOutput) []string {
	result := make([]string, len(out.Nodes))
	for i, n := range out.Nodes {
		result[i] = fmt.Sprintf("%s(%s)", n.Kind, n.CanonicalSignature)
	}
	return result
}

func goFixtureEdgeKinds(out *goFixtureOutput) []string {
	result := make([]string, len(out.Edges))
	for i, e := range out.Edges {
		result[i] = e.Kind
	}
	return result
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_go_parser_go_fixture_test(ctx *godog.ScenarioContext) {
	s := &goFixtureState{}

	// Given
	ctx.Given(`^the embedded Go fixture$`, s.theEmbeddedGoFixture)
	ctx.Given(`^a Go fixture with method body "([^"]*)"$`, s.aGoFixtureWithMethodBody)

	// When
	ctx.When(`^EmitFile runs under CGO=on$`, s.emitFileRunsUnderCGOOn)

	// Then — Scenario 1
	ctx.Then(`^(\d+) class and (\d+) method and (\d+) package nodes are emitted$`, s.nodeCountsEmitted)
	ctx.Then(`^(\d+) contains and (\d+) static_calls and (\d+) imports edges are emitted$`, s.edgeCountsEmitted)

	// Then — Scenario 2
	ctx.Then(`^the captured method signature contains the substring "([^"]*)"$`, s.capturedMethodSignatureContains)

	// Then — Scenario 3
	ctx.Then(`^exactly one writes edge from the method to a field member named "([^"]*)" is emitted$`, s.writesEdgeFromMethodToFieldMember)
}

func TestE2E_go_parser_go_fixture_test(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_go_parser_go_fixture_test,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"go_parser_go_fixture_test.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
