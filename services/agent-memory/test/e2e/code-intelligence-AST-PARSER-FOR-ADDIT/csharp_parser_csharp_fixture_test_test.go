//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// ---------------------------------------------------------------------------
// Shared helpers (one copy per package)
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		t.Skipf("required env var %s is not set — skipping", name)
	}
	return v
}

func moduleRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
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
// Probe 1 — Dispatcher probe (Scenario 1: fixture node+edge counts)
//
// Writes a _test.go into the ast package that creates a Dispatcher with
// a spy writer, calls EmitFile on the fixture, and counts nodes/edges
// by kind.
// ---------------------------------------------------------------------------

const csharpDispatcherProbeSource = `package ast

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

type e2eCsFixtureSpyWriter struct {
	nextID int
	nodes  []graphwriter.NodeInput
	edges  []graphwriter.EdgeInput
}

func (w *e2eCsFixtureSpyWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	w.nextID++
	w.nodes = append(w.nodes, in)
	return graphwriter.NodeRecord{NodeID: fmt.Sprintf("spy-%d", w.nextID)}, nil
}

func (w *e2eCsFixtureSpyWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	w.edges = append(w.edges, in)
	return graphwriter.EdgeRecord{}, nil
}

type e2eCsFixtureDispatcherOutput struct {
	ClassNodes      int            ` + "`" + `json:"class_nodes"` + "`" + `
	MethodNodes     int            ` + "`" + `json:"method_nodes"` + "`" + `
	PackageNodes    int            ` + "`" + `json:"package_nodes"` + "`" + `
	NodeKindCounts  map[string]int ` + "`" + `json:"node_kind_counts"` + "`" + `
	EdgeKindCounts  map[string]int ` + "`" + `json:"edge_kind_counts"` + "`" + `
}

func TestE2EProbe_CSharpFixtureDispatcher(t *testing.T) {
	srcFile := os.Getenv("E2E_PROBE_SOURCE_FILE")
	if srcFile == "" {
		t.Skip("E2E_PROBE_SOURCE_FILE not set")
	}
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	spy := &e2eCsFixtureSpyWriter{}
	d := NewDispatcher(spy, WithParsers(NewTreeSitterCSharpParser()))

	_, err = d.EmitFile(context.Background(), repoindexer.EmitFileEvent{
		RepoID:     "e2e-test-repo",
		RepoURL:    "https://example.com/e2e-test",
		SHA:        "e2e-probe-sha",
		RepoNodeID: "e2e-repo-node",
		FileNodeID: "e2e-file-node",
		RelPath:    "Fixture.cs",
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(src)), nil
		},
	})
	if err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	nodeCounts := make(map[string]int)
	edgeCounts := make(map[string]int)
	for _, n := range spy.nodes {
		nodeCounts[n.Kind]++
	}
	for _, e := range spy.edges {
		edgeCounts[e.Kind]++
	}

	result := e2eCsFixtureDispatcherOutput{
		ClassNodes:     nodeCounts["class"],
		MethodNodes:    nodeCounts["method"],
		PackageNodes:   nodeCounts["package"],
		NodeKindCounts: nodeCounts,
		EdgeKindCounts: edgeCounts,
	}
	data, _ := json.Marshal(result)
	fmt.Printf("PROBE_OUTPUT:%s\n", string(data))
}
`

// ---------------------------------------------------------------------------
// Probe 2 — Parse probe (Scenarios 2 & 3: partition matrix, partial flag)
//
// Calls NewTreeSitterCSharpParser().Parse() and emits the full
// ParseResult as JSON so the e2e step definitions can inspect
// Extends, Implements, and LangMeta without compile-time coupling.
// ---------------------------------------------------------------------------

const csharpParseProbeSource = `package ast

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

type e2eCsParseProbeClass struct {
	QualifiedName string         ` + "`" + `json:"qualified_name"` + "`" + `
	Kind          string         ` + "`" + `json:"kind"` + "`" + `
	Extends       []string       ` + "`" + `json:"extends"` + "`" + `
	Implements    []string       ` + "`" + `json:"implements"` + "`" + `
	LangMeta      map[string]any ` + "`" + `json:"lang_meta"` + "`" + `
}

type e2eCsParseProbeOutput struct {
	Classes []e2eCsParseProbeClass ` + "`" + `json:"classes"` + "`" + `
}

func TestE2EProbe_CSharpParse(t *testing.T) {
	srcFile := os.Getenv("E2E_PROBE_SOURCE_FILE")
	if srcFile == "" {
		t.Skip("E2E_PROBE_SOURCE_FILE not set")
	}
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	parser := NewTreeSitterCSharpParser()
	result, err := parser.Parse("test.cs", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	out := e2eCsParseProbeOutput{}
	for _, c := range result.Classes {
		cls := e2eCsParseProbeClass{
			QualifiedName: c.QualifiedName,
			Kind:          c.Kind,
			Extends:       c.Extends,
			Implements:    c.Implements,
			LangMeta:      c.LangMeta,
		}
		if cls.Extends == nil {
			cls.Extends = []string{}
		}
		if cls.Implements == nil {
			cls.Implements = []string{}
		}
		if cls.LangMeta == nil {
			cls.LangMeta = map[string]any{}
		}
		out.Classes = append(out.Classes, cls)
	}

	data, _ := json.Marshal(out)
	fmt.Printf("PROBE_OUTPUT:%s\n", string(data))
}
`

// ---------------------------------------------------------------------------
// Probe output mirror types
// ---------------------------------------------------------------------------

type csDispatcherOutput struct {
	ClassNodes     int            `json:"class_nodes"`
	MethodNodes    int            `json:"method_nodes"`
	PackageNodes   int            `json:"package_nodes"`
	NodeKindCounts map[string]int `json:"node_kind_counts"`
	EdgeKindCounts map[string]int `json:"edge_kind_counts"`
}

type csParseClass struct {
	QualifiedName string         `json:"qualified_name"`
	Kind          string         `json:"kind"`
	Extends       []string       `json:"extends"`
	Implements    []string       `json:"implements"`
	LangMeta      map[string]any `json:"lang_meta"`
}

type csParseOutput struct {
	Classes []csParseClass `json:"classes"`
}

// ---------------------------------------------------------------------------
// Probe runners
// ---------------------------------------------------------------------------

func runCSharpProbe(probeSource, testFunc, csharpSource string) (string, error) {
	modRoot, err := moduleRoot()
	if err != nil {
		return "", err
	}

	astDir := filepath.Join(modRoot, "internal", "repoindexer", "ast")
	pid := os.Getpid()

	probeFile := filepath.Join(astDir, fmt.Sprintf("e2e_cs_fixture_probe_%d_test.go", pid))
	if err := os.WriteFile(probeFile, []byte(probeSource), 0644); err != nil {
		return "", fmt.Errorf("write probe: %w", err)
	}
	defer os.Remove(probeFile)

	srcFile := filepath.Join(os.TempDir(), fmt.Sprintf("e2e_cs_fixture_src_%d.cs", pid))
	if err := os.WriteFile(srcFile, []byte(csharpSource), 0644); err != nil {
		return "", fmt.Errorf("write source: %w", err)
	}
	defer os.Remove(srcFile)

	cmd := exec.Command("go", "test",
		"-run", testFunc,
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
		return "", fmt.Errorf("probe %s failed: %v\noutput:\n%s", testFunc, err, string(output))
	}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PROBE_OUTPUT:") {
			return strings.TrimPrefix(line, "PROBE_OUTPUT:"), nil
		}
	}
	return "", fmt.Errorf("PROBE_OUTPUT marker not found in output:\n%s", string(output))
}

func runCSharpDispatcherProbe(source string) (*csDispatcherOutput, error) {
	raw, err := runCSharpProbe(csharpDispatcherProbeSource, "TestE2EProbe_CSharpFixtureDispatcher", source)
	if err != nil {
		return nil, err
	}
	var out csDispatcherOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse JSON: %w\nraw: %s", err, raw)
	}
	return &out, nil
}

func runCSharpParseProbe(source string) (*csParseOutput, error) {
	raw, err := runCSharpProbe(csharpParseProbeSource, "TestE2EProbe_CSharpParse", source)
	if err != nil {
		return nil, err
	}
	var out csParseOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse JSON: %w\nraw: %s", err, raw)
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type csharpFixtureState struct {
	// Scenario 1: fixture node+edge count (dispatcher)
	fixtureSrc       string
	dispatcherResult *csDispatcherOutput

	// Scenario 2: partition matrix (parser)
	partitionSrc    string
	partitionResult *csParseOutput

	// Scenario 3: partial class flag (parser)
	partialSrc    string
	partialResult *csParseOutput
}

// ---------------------------------------------------------------------------
// Scenario 1 — C# fixture node and edge count
// ---------------------------------------------------------------------------

func (s *csharpFixtureState) theCSharpFixtureSource(src *godog.DocString) error {
	s.fixtureSrc = strings.TrimSpace(src.Content)
	return nil
}

func (s *csharpFixtureState) emitFileRunsUnderCGOOn() error {
	result, err := runCSharpDispatcherProbe(s.fixtureSrc)
	if err != nil {
		return fmt.Errorf("dispatcher probe failed: %w", err)
	}
	s.dispatcherResult = result
	return nil
}

func (s *csharpFixtureState) nClassNodesAreEmitted(expected int) error {
	if s.dispatcherResult.ClassNodes != expected {
		return fmt.Errorf("expected %d class nodes, got %d (all node counts: %v)",
			expected, s.dispatcherResult.ClassNodes, s.dispatcherResult.NodeKindCounts)
	}
	return nil
}

func (s *csharpFixtureState) nMethodNodesAreEmitted(expected int) error {
	if s.dispatcherResult.MethodNodes != expected {
		return fmt.Errorf("expected %d method nodes, got %d (all node counts: %v)",
			expected, s.dispatcherResult.MethodNodes, s.dispatcherResult.NodeKindCounts)
	}
	return nil
}

func (s *csharpFixtureState) nPackageNodesAreEmitted(expected int) error {
	if s.dispatcherResult.PackageNodes != expected {
		return fmt.Errorf("expected %d package nodes, got %d (all node counts: %v)",
			expected, s.dispatcherResult.PackageNodes, s.dispatcherResult.NodeKindCounts)
	}
	return nil
}

func (s *csharpFixtureState) nEdgesOfKindAreEmitted(expected int, kind string) error {
	actual := s.dispatcherResult.EdgeKindCounts[kind]
	if actual != expected {
		return fmt.Errorf("expected %d %s edges, got %d (all edge counts: %v)",
			expected, kind, actual, s.dispatcherResult.EdgeKindCounts)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — Base-list partition decision matrix
// ---------------------------------------------------------------------------

func (s *csharpFixtureState) csharpPartitionSource(source string) error {
	s.partitionSrc = source
	return nil
}

func (s *csharpFixtureState) thePartitionSourceIsParsedWithTheCSharpParser() error {
	result, err := runCSharpParseProbe(s.partitionSrc)
	if err != nil {
		return fmt.Errorf("parse probe failed: %w", err)
	}
	s.partitionResult = result
	return nil
}

func (s *csharpFixtureState) classExtendsAndImplements(className, extendsCSV, implementsCSV string) error {
	if s.partitionResult == nil {
		return fmt.Errorf("no parse result available")
	}

	var found *csParseClass
	for i := range s.partitionResult.Classes {
		if s.partitionResult.Classes[i].QualifiedName == className {
			found = &s.partitionResult.Classes[i]
			break
		}
	}
	if found == nil {
		names := make([]string, len(s.partitionResult.Classes))
		for i, c := range s.partitionResult.Classes {
			names[i] = c.QualifiedName
		}
		return fmt.Errorf("class %q not found; have %v", className, names)
	}

	wantExtends := splitCSV(extendsCSV)
	wantImplements := splitCSV(implementsCSV)

	sort.Strings(wantExtends)
	sort.Strings(wantImplements)
	gotExtends := sortedCopy(found.Extends)
	gotImplements := sortedCopy(found.Implements)

	if !slicesEqual(gotExtends, wantExtends) {
		return fmt.Errorf("class %q Extends mismatch: want %v, got %v",
			className, wantExtends, gotExtends)
	}
	if !slicesEqual(gotImplements, wantImplements) {
		return fmt.Errorf("class %q Implements mismatch: want %v, got %v",
			className, wantImplements, gotImplements)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 3 — Partial class flag
// ---------------------------------------------------------------------------

func (s *csharpFixtureState) csharpPartialClassSource(src *godog.DocString) error {
	s.partialSrc = strings.TrimSpace(src.Content)
	return nil
}

func (s *csharpFixtureState) thePartialSourceIsParsedWithTheCSharpParser() error {
	result, err := runCSharpParseProbe(s.partialSrc)
	if err != nil {
		return fmt.Errorf("parse probe failed: %w", err)
	}
	s.partialResult = result
	return nil
}

func (s *csharpFixtureState) theClassHasLangMetaPartialEqualToTrue(className string) error {
	if s.partialResult == nil {
		return fmt.Errorf("no parse result available")
	}

	var found *csParseClass
	for i := range s.partialResult.Classes {
		if s.partialResult.Classes[i].QualifiedName == className {
			found = &s.partialResult.Classes[i]
			break
		}
	}
	if found == nil {
		names := make([]string, len(s.partialResult.Classes))
		for i, c := range s.partialResult.Classes {
			names[i] = c.QualifiedName
		}
		return fmt.Errorf("class %q not found; have %v", className, names)
	}

	if found.LangMeta == nil {
		return fmt.Errorf("class %q LangMeta is nil", className)
	}

	partialRaw, ok := found.LangMeta["partial"]
	if !ok {
		return fmt.Errorf("class %q LangMeta has no 'partial' key; keys: %v",
			className, langMetaKeysCsFixture(found.LangMeta))
	}

	switch v := partialRaw.(type) {
	case bool:
		if !v {
			return fmt.Errorf("class %q LangMeta['partial'] = false, want true", className)
		}
	case float64:
		// JSON numbers may decode as float64
		if v != 1 {
			return fmt.Errorf("class %q LangMeta['partial'] = %v, want true", className, v)
		}
	default:
		return fmt.Errorf("class %q LangMeta['partial'] has unexpected type %T: %v",
			className, partialRaw, partialRaw)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Utility helpers
// ---------------------------------------------------------------------------

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func sortedCopy(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func langMetaKeysCsFixture(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_csharp_parser_csharp_fixture_test(ctx *godog.ScenarioContext) {
	s := &csharpFixtureState{}

	// Scenario 1: C# fixture node and edge count
	ctx.Given(`^the C# fixture source:$`, s.theCSharpFixtureSource)
	ctx.When(`^EmitFile runs under CGO on$`, s.emitFileRunsUnderCGOOn)
	ctx.Then(`^(\d+) class nodes are emitted$`, s.nClassNodesAreEmitted)
	ctx.Then(`^(\d+) method nodes are emitted$`, s.nMethodNodesAreEmitted)
	ctx.Then(`^(\d+) package node(?:s)? (?:is|are) emitted$`, s.nPackageNodesAreEmitted)
	ctx.Then(`^(\d+) extends edge(?:s)? (?:is|are) emitted$`, func(n int) error {
		return s.nEdgesOfKindAreEmitted(n, "extends")
	})
	ctx.Then(`^(\d+) implements edge(?:s)? (?:is|are) emitted$`, func(n int) error {
		return s.nEdgesOfKindAreEmitted(n, "implements")
	})
	ctx.Then(`^(\d+) static_calls edge(?:s)? (?:is|are) emitted$`, func(n int) error {
		return s.nEdgesOfKindAreEmitted(n, "static_calls")
	})
	ctx.Then(`^(\d+) imports edge(?:s)? (?:is|are) emitted$`, func(n int) error {
		return s.nEdgesOfKindAreEmitted(n, "imports")
	})

	// Scenario 2: Base-list partition decision matrix
	ctx.Given(`^C# partition source "([^"]*)"$`, s.csharpPartitionSource)
	ctx.When(`^the partition source is parsed with the C# parser$`, s.thePartitionSourceIsParsedWithTheCSharpParser)
	ctx.Then(`^class "([^"]*)" Extends list is "([^"]*)" and Implements list is "([^"]*)"$`, s.classExtendsAndImplements)

	// Scenario 3: Partial class flag
	ctx.Given(`^C# partial class source:$`, s.csharpPartialClassSource)
	ctx.When(`^the partial source is parsed with the C# parser$`, s.thePartialSourceIsParsedWithTheCSharpParser)
	ctx.Then(`^the class "([^"]*)" has LangMeta partial equal to true$`, s.theClassHasLangMetaPartialEqualToTrue)
}

func TestE2E_csharp_parser_csharp_fixture_test(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_csharp_parser_csharp_fixture_test,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"csharp_parser_csharp_fixture_test.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}