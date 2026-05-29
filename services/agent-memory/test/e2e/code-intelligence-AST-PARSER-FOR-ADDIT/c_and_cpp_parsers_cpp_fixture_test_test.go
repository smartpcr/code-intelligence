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

func cppFixtureModuleRoot() (string, error) {
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

// errCppAstDirMissing is a sentinel used to distinguish "ast package not
// present in this workspace" from real failures so godog steps can
// return godog.ErrPending instead of a hard failure.
var errCppAstDirMissing = fmt.Errorf("internal/repoindexer/ast directory not found — implementation not present in this workspace")

// ---------------------------------------------------------------------------
// Embedded C++ fixtures
// ---------------------------------------------------------------------------

// cppMainFixtureSource covers the baseline scenario:
//   - 2 classes (Base, Greeter : public Base) → 2 class nodes
//   - 3 methods (Base.greet, Greeter.hello, log_global) → 3 method nodes
//   - #include <iostream> → 1 package node + 1 imports edge
//   - #include "./local.h" → relative — must be DROPPED
//   - file→Base, file→Greeter, Base→greet, Greeter→hello, file→log_global → 5 contains
//   - Greeter extends Base → 1 extends edge
//   - hello() calls log_global() → 1 static_calls edge
//
// Expected totals: 2 class + 3 method + 1 package = 6 nodes
//
//	5 contains + 1 extends + 1 static_calls + 1 imports = 8 edges
const cppMainFixtureSource = `#include <iostream>
#include "./local.h"

class Base {
public:
    void greet() {}
};

class Greeter : public Base {
public:
    void hello() {
        log_global();
    }
};

void log_global() {}
`

// cppDedupeFixtureSource covers the dedupe scenario:
// A forward-declared method (Foo::bar) defined out-of-class must
// collapse to exactly one method node. The body contains a call
// to log_global, proving the definition's body was retained.
const cppDedupeFixtureSource = `class Foo {
public:
    void bar();
};

void log_global() {}

void Foo::bar() {
    log_global();
}
`

// cppInheritanceFixtureSource covers the base_access scenario:
// Greeter inherits publicly from Base. The class node's attrs_json
// must contain base_access["Base"] == "public".
const cppInheritanceFixtureSource = `class Base {};

class Greeter : public Base {};
`

// ---------------------------------------------------------------------------
// Probe: injected _test.go that runs inside the ast package and calls
// Dispatcher.EmitFile() with a spy writer, then prints JSON on stdout.
//
// The spy tracks a nodeID→NodeInput map so it can cross-reference
// EdgeInput.DstNodeID back to the target node. This lets the e2e
// step definitions verify which specific package nodes are targeted
// by imports edges, check attrs_json content, and verify method
// deduplication.
// ---------------------------------------------------------------------------

const cppFixtureProbeTestSource = `package ast

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

type e2eCppFixtureSpyWriter struct {
	nextID  int
	nodeMap map[string]graphwriter.NodeInput
	nodes   []graphwriter.NodeInput
	edges   []graphwriter.EdgeInput
}

func (w *e2eCppFixtureSpyWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	w.nextID++
	id := fmt.Sprintf("spy-%d", w.nextID)
	w.nodes = append(w.nodes, in)
	w.nodeMap[id] = in
	return graphwriter.NodeRecord{NodeID: id}, nil
}

func (w *e2eCppFixtureSpyWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	w.edges = append(w.edges, in)
	return graphwriter.EdgeRecord{}, nil
}

type e2eCppNodeSummary struct {
	Kind               string          ` + "`" + `json:"kind"` + "`" + `
	CanonicalSignature string          ` + "`" + `json:"canonical_signature"` + "`" + `
	AttrsJSON          json.RawMessage ` + "`" + `json:"attrs_json,omitempty"` + "`" + `
}

type e2eCppEdgeSummary struct {
	Kind         string ` + "`" + `json:"kind"` + "`" + `
	DstSignature string ` + "`" + `json:"dst_signature"` + "`" + `
}

type e2eCppImportsTarget struct {
	TargetSignature string ` + "`" + `json:"target_signature"` + "`" + `
}

type e2eCppMethodBody struct {
	QualifiedName string ` + "`" + `json:"qualified_name"` + "`" + `
	BodySource    string ` + "`" + `json:"body_source"` + "`" + `
}

type e2eCppFixtureOutput struct {
	Nodes          []e2eCppNodeSummary   ` + "`" + `json:"nodes"` + "`" + `
	Edges          []e2eCppEdgeSummary   ` + "`" + `json:"edges"` + "`" + `
	ImportsTargets []e2eCppImportsTarget ` + "`" + `json:"imports_targets"` + "`" + `
	MethodBodies   []e2eCppMethodBody    ` + "`" + `json:"method_bodies"` + "`" + `
}

func TestE2EProbe_CppFixtureEmitFile(t *testing.T) {
	srcFile := os.Getenv("E2E_PROBE_SOURCE_FILE")
	if srcFile == "" {
		t.Skip("E2E_PROBE_SOURCE_FILE not set")
	}
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	relPath := os.Getenv("E2E_PROBE_REL_PATH")
	if relPath == "" {
		relPath = "fixture.cpp"
	}

	// --- Phase 1: call Parse() directly to capture MethodDecl.BodySource ---
	parser := NewTreeSitterCppParser()
	parseResult, err := parser.Parse(relPath, src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var methodBodies []e2eCppMethodBody
	for _, m := range parseResult.Methods {
		methodBodies = append(methodBodies, e2eCppMethodBody{
			QualifiedName: m.QualifiedName,
			BodySource:    m.BodySource,
		})
	}

	// --- Phase 2: call EmitFile to capture nodes and edges ---
	spy := &e2eCppFixtureSpyWriter{nodeMap: make(map[string]graphwriter.NodeInput)}
	d := NewDispatcher(spy, WithParsers(NewTreeSitterCppParser()))

	_, err = d.EmitFile(context.Background(), repoindexer.EmitFileEvent{
		RepoID:     "e2e-fixture-repo",
		RepoURL:    "https://example.com/e2e-fixture",
		SHA:        "e2e-fixture-sha",
		RepoNodeID: "e2e-repo-node",
		FileNodeID: "e2e-file-node",
		RelPath:    relPath,
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(src)), nil
		},
	})
	if err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	result := e2eCppFixtureOutput{MethodBodies: methodBodies}
	for _, n := range spy.nodes {
		result.Nodes = append(result.Nodes, e2eCppNodeSummary{
			Kind:               n.Kind,
			CanonicalSignature: n.CanonicalSignature,
			AttrsJSON:          n.AttrsJSON,
		})
	}
	for _, e := range spy.edges {
		dstSig := ""
		if dn, ok := spy.nodeMap[e.DstNodeID]; ok {
			dstSig = dn.CanonicalSignature
		}
		result.Edges = append(result.Edges, e2eCppEdgeSummary{
			Kind:         e.Kind,
			DstSignature: dstSig,
		})
		if e.Kind == "imports" {
			targetNode, ok := spy.nodeMap[e.DstNodeID]
			sig := ""
			if ok {
				sig = targetNode.CanonicalSignature
			}
			result.ImportsTargets = append(result.ImportsTargets, e2eCppImportsTarget{
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

type cppNodeSummary struct {
	Kind               string          `json:"kind"`
	CanonicalSignature string          `json:"canonical_signature"`
	AttrsJSON          json.RawMessage `json:"attrs_json,omitempty"`
}

type cppEdgeSummary struct {
	Kind         string `json:"kind"`
	DstSignature string `json:"dst_signature"`
}

type cppImportsTarget struct {
	TargetSignature string `json:"target_signature"`
}

type cppMethodBody struct {
	QualifiedName string `json:"qualified_name"`
	BodySource    string `json:"body_source"`
}

type cppFixtureOutput struct {
	Nodes          []cppNodeSummary   `json:"nodes"`
	Edges          []cppEdgeSummary   `json:"edges"`
	ImportsTargets []cppImportsTarget `json:"imports_targets"`
	MethodBodies   []cppMethodBody    `json:"method_bodies"`
}

// ---------------------------------------------------------------------------
// Probe runner
// ---------------------------------------------------------------------------

func runCppFixtureProbe(cppSource string) (*cppFixtureOutput, error) {
	modRoot, err := cppFixtureModuleRoot()
	if err != nil {
		return nil, err
	}

	astDir := filepath.Join(modRoot, "internal", "repoindexer", "ast")

	// Guard: if the ast package directory does not exist in this
	// workspace (sparse checkout), return sentinel so the step can
	// mark the scenario as pending rather than failing.
	if _, err := os.Stat(astDir); os.IsNotExist(err) {
		return nil, errCppAstDirMissing
	}

	pid := os.Getpid()

	probeFile := filepath.Join(astDir, fmt.Sprintf("e2e_cpp_fixture_probe_%d_test.go", pid))
	if err := os.WriteFile(probeFile, []byte(cppFixtureProbeTestSource), 0644); err != nil {
		return nil, fmt.Errorf("write probe: %w", err)
	}
	defer os.Remove(probeFile)

	srcFile := filepath.Join(os.TempDir(), fmt.Sprintf("e2e_cpp_fixture_src_%d.cpp", pid))
	if err := os.WriteFile(srcFile, []byte(cppSource), 0644); err != nil {
		return nil, fmt.Errorf("write source: %w", err)
	}
	defer os.Remove(srcFile)

	cmd := exec.Command("go", "test",
		"-run", "TestE2EProbe_CppFixtureEmitFile",
		"-v", "-count=1",
		"./internal/repoindexer/ast/",
	)
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=1",
		"E2E_PROBE_SOURCE_FILE="+srcFile,
		"E2E_PROBE_REL_PATH=fixture.cpp",
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("probe failed: %v\noutput:\n%s", err, string(output))
	}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PROBE_OUTPUT:") {
			raw := strings.TrimPrefix(line, "PROBE_OUTPUT:")
			var out cppFixtureOutput
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

type cppFixtureState struct {
	fixtureSource string
	probeResult   *cppFixtureOutput
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *cppFixtureState) theEmbeddedCppFixture() error {
	s.fixtureSource = cppMainFixtureSource
	return nil
}

func (s *cppFixtureState) theEmbeddedDedupeFixture() error {
	s.fixtureSource = cppDedupeFixtureSource
	return nil
}

func (s *cppFixtureState) theInheritanceFixture() error {
	s.fixtureSource = cppInheritanceFixtureSource
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *cppFixtureState) emitFileRunsUnderCGOOn() error {
	result, err := runCppFixtureProbe(s.fixtureSource)
	if err == errCppAstDirMissing {
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

func (s *cppFixtureState) cppClassMethodAndPackageNodesAreEmitted(classCount, methodCount, packageCount int) error {
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
		return fmt.Errorf("node count mismatch: %s\nall nodes: %v", strings.Join(errs, "; "), s.cppNodeKinds())
	}
	return nil
}

func (s *cppFixtureState) cppContainsExtendsStaticCallsAndImportsEdgesAreEmitted(containsCount, extendsCount, staticCallsCount, importsCount int) error {
	if s.probeResult == nil {
		return godog.ErrPending
	}
	var actualContains, actualExtends, actualStaticCalls, actualImports int
	for _, e := range s.probeResult.Edges {
		switch e.Kind {
		case "contains":
			actualContains++
		case "extends":
			actualExtends++
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
	if actualExtends != extendsCount {
		errs = append(errs, fmt.Sprintf("extends edges: want %d, got %d", extendsCount, actualExtends))
	}
	if actualStaticCalls != staticCallsCount {
		errs = append(errs, fmt.Sprintf("static_calls edges: want %d, got %d", staticCallsCount, actualStaticCalls))
	}
	if actualImports != importsCount {
		errs = append(errs, fmt.Sprintf("imports edges: want %d, got %d", importsCount, actualImports))
	}
	if len(errs) > 0 {
		return fmt.Errorf("edge count mismatch: %s\nall edges: %v", strings.Join(errs, "; "), s.cppEdgeKinds())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — Scenario 1: relative include dropped + system include verified
// ---------------------------------------------------------------------------

func (s *cppFixtureState) zeroImportsEdgesTargetAPackageNodeWhoseModuleStartsWith(prefix string) error {
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

func (s *cppFixtureState) theSoleImportsEdgeTargetsAPackageNodeWhoseSignatureContains(substr string) error {
	if s.probeResult == nil {
		return godog.ErrPending
	}
	var importsEdges []cppEdgeSummary
	for _, e := range s.probeResult.Edges {
		if e.Kind == "imports" {
			importsEdges = append(importsEdges, e)
		}
	}
	if len(importsEdges) != 1 {
		return fmt.Errorf("want exactly 1 imports edge, got %d: %v", len(importsEdges), importsEdges)
	}
	if !strings.Contains(importsEdges[0].DstSignature, substr) {
		return fmt.Errorf("the sole imports edge targets %q, which does not contain %q",
			importsEdges[0].DstSignature, substr)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — Scenario 2: dedupe
// ---------------------------------------------------------------------------

func (s *cppFixtureState) exactlyNMethodNodesWithSignatureContaining(count int, substr string) error {
	if s.probeResult == nil {
		return godog.ErrPending
	}
	actual := 0
	for _, n := range s.probeResult.Nodes {
		if n.Kind == "method" && strings.Contains(n.CanonicalSignature, substr) {
			actual++
		}
	}
	if actual != count {
		return fmt.Errorf("want %d method nodes with signature containing %q, got %d\nall nodes: %v",
			count, substr, actual, s.cppNodeKinds())
	}
	return nil
}

func (s *cppFixtureState) theMethodNodeWithSignatureContainingHasNonEmptyBodySource(substr string) error {
	if s.probeResult == nil {
		return godog.ErrPending
	}
	// Check the raw ParseResult.MethodDecl.BodySource captured by the probe's
	// direct Parse() call — not the attrs_json proxy.
	for _, mb := range s.probeResult.MethodBodies {
		if strings.Contains(mb.QualifiedName, substr) {
			if mb.BodySource == "" {
				return fmt.Errorf("method %q has empty BodySource — "+
					"declaration/definition dedupe must retain the definition body",
					mb.QualifiedName)
			}
			return nil
		}
	}
	var names []string
	for _, mb := range s.probeResult.MethodBodies {
		names = append(names, mb.QualifiedName)
	}
	return fmt.Errorf("no method with QualifiedName containing %q in ParseResult\navailable: %v",
		substr, names)
}

func (s *cppFixtureState) exactlyNStaticCallsEdgesTargetingSignatureContaining(count int, substr string) error {
	if s.probeResult == nil {
		return godog.ErrPending
	}
	actual := 0
	for _, e := range s.probeResult.Edges {
		if e.Kind == "static_calls" && strings.Contains(e.DstSignature, substr) {
			actual++
		}
	}
	if actual != count {
		return fmt.Errorf("want %d static_calls edges targeting signature containing %q, got %d\nall edges: %v",
			count, substr, actual, s.cppEdgeDetails())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — Scenario 3: base_access attrs
// ---------------------------------------------------------------------------

func (s *cppFixtureState) classNodeWithSignatureContainingHasBaseAccess(classSub, baseName, expectedAccess string) error {
	if s.probeResult == nil {
		return godog.ErrPending
	}
	for _, n := range s.probeResult.Nodes {
		if n.Kind != "class" || !strings.Contains(n.CanonicalSignature, classSub) {
			continue
		}
		if len(n.AttrsJSON) == 0 {
			return fmt.Errorf("class node %q has empty attrs_json", n.CanonicalSignature)
		}
		var attrs map[string]json.RawMessage
		if err := json.Unmarshal(n.AttrsJSON, &attrs); err != nil {
			return fmt.Errorf("parse attrs_json for %q: %w\nraw: %s", n.CanonicalSignature, err, string(n.AttrsJSON))
		}
		baRaw, ok := attrs["base_access"]
		if !ok {
			return fmt.Errorf("class node %q attrs_json has no base_access key\nattrs_json: %s",
				n.CanonicalSignature, string(n.AttrsJSON))
		}
		var baseAccess map[string]string
		if err := json.Unmarshal(baRaw, &baseAccess); err != nil {
			return fmt.Errorf("parse base_access for %q: %w\nraw: %s", n.CanonicalSignature, err, string(baRaw))
		}
		gotAccess, ok := baseAccess[baseName]
		if !ok {
			return fmt.Errorf("base_access has no key %q; available keys: %v",
				baseName, mapKeys(baseAccess))
		}
		if gotAccess != expectedAccess {
			return fmt.Errorf("base_access[%q]: want %q, got %q", baseName, expectedAccess, gotAccess)
		}
		return nil
	}
	return fmt.Errorf("no class node with signature containing %q found\nall nodes: %v",
		classSub, s.cppNodeKinds())
}

// ---------------------------------------------------------------------------
// Diagnostic helpers
// ---------------------------------------------------------------------------

func (s *cppFixtureState) cppNodeKinds() []string {
	out := make([]string, len(s.probeResult.Nodes))
	for i, n := range s.probeResult.Nodes {
		out[i] = fmt.Sprintf("%s(%s)", n.Kind, n.CanonicalSignature)
	}
	return out
}

func (s *cppFixtureState) cppEdgeKinds() []string {
	out := make([]string, len(s.probeResult.Edges))
	for i, e := range s.probeResult.Edges {
		out[i] = e.Kind
	}
	return out
}

func (s *cppFixtureState) cppEdgeDetails() []string {
	out := make([]string, len(s.probeResult.Edges))
	for i, e := range s.probeResult.Edges {
		out[i] = fmt.Sprintf("%s→%s", e.Kind, e.DstSignature)
	}
	return out
}

func mapKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_c_and_cpp_parsers_cpp_fixture_test(ctx *godog.ScenarioContext) {
	s := &cppFixtureState{}

	ctx.Given(`^the embedded C\+\+ fixture$`, s.theEmbeddedCppFixture)
	ctx.Given(`^the embedded dedupe fixture$`, s.theEmbeddedDedupeFixture)
	ctx.Given(`^the inheritance fixture$`, s.theInheritanceFixture)
	ctx.When(`^EmitFile runs under CGO=on$`, s.emitFileRunsUnderCGOOn)
	ctx.Then(`^(\d+) class, (\d+) method, and (\d+) package nodes are emitted$`, s.cppClassMethodAndPackageNodesAreEmitted)
	ctx.Then(`^(\d+) contains, (\d+) extends, (\d+) static_calls, and (\d+) imports edges are emitted$`, s.cppContainsExtendsStaticCallsAndImportsEdgesAreEmitted)
	ctx.Then(`^zero imports edges target a package node whose module starts with "([^"]*)"$`, s.zeroImportsEdgesTargetAPackageNodeWhoseModuleStartsWith)
	ctx.Then(`^the sole imports edge targets a package node whose signature contains "([^"]*)"$`, s.theSoleImportsEdgeTargetsAPackageNodeWhoseSignatureContains)
	ctx.Then(`^exactly (\d+) method node with signature containing "([^"]*)" is emitted$`, s.exactlyNMethodNodesWithSignatureContaining)
	ctx.Then(`^the method node with signature containing "([^"]*)" has non-empty BodySource$`, s.theMethodNodeWithSignatureContainingHasNonEmptyBodySource)
	ctx.Then(`^exactly (\d+) static_calls edge targeting a signature containing "([^"]*)" is emitted$`, s.exactlyNStaticCallsEdgesTargetingSignatureContaining)
	ctx.Then(`^the class node with signature containing "([^"]*)" has base_access "([^"]*)" equal to "([^"]*)"$`, s.classNodeWithSignatureContainingHasBaseAccess)
}

func TestE2E_c_and_cpp_parsers_cpp_fixture_test(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_c_and_cpp_parsers_cpp_fixture_test,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"c_and_cpp_parsers_cpp_fixture_test.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}