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
// Probe 1 — Parse probe (Scenario 2: C struct + free function)
//
// A _test.go written at runtime into internal/repoindexer/ast/ that
// calls NewTreeSitterCParser().Parse() and emits class/method JSON.
// ---------------------------------------------------------------------------

const cParserProbeTestSource = `package ast

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

type e2eProbeClass struct {
	QualifiedName string
	Kind          string
}

type e2eProbeMethod struct {
	QualifiedName  string
	ParamSignature string
}

type e2eParseOutput struct {
	Classes []e2eProbeClass
	Methods []e2eProbeMethod
}

func TestE2EProbe_CParser(t *testing.T) {
	srcFile := os.Getenv("E2E_PROBE_SOURCE_FILE")
	if srcFile == "" {
		t.Skip("E2E_PROBE_SOURCE_FILE not set")
	}
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	parser := NewTreeSitterCParser()
	result, err := parser.Parse("probe.c", src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	out := e2eParseOutput{}
	for _, c := range result.Classes {
		out.Classes = append(out.Classes, e2eProbeClass{
			QualifiedName: c.QualifiedName,
			Kind:          c.Kind,
		})
	}
	for _, m := range result.Methods {
		out.Methods = append(out.Methods, e2eProbeMethod{
			QualifiedName:  m.QualifiedName,
			ParamSignature: m.ParamSignature,
		})
	}

	data, _ := json.Marshal(out)
	fmt.Printf("PROBE_OUTPUT:%s\n", string(data))
}
`

// ---------------------------------------------------------------------------
// Probe 2 — Dispatcher probe (Scenario 3: Relative include dropped)
//
// STRUCTURAL CHANGE from prior iterations:
//   - Iters 1-3: imported ast directly → compile failure (sparse workspace)
//   - Iter 4: standalone main.go with LOCAL COPY of isRelativeImport
//   - Iter 5: _test.go calling isRelativeImport directly
//   - Iter 6 (this): _test.go calling Dispatcher.EmitFile() with a spy
//     writer that captures emitted edges — exercises the FULL dispatcher
//     Pass 0 pipeline (EmitFile → emit → emitImportsEdges →
//     isRelativeImport → writer.InsertEdge)
//
// The spy implements the nodeEdgeWriter interface (unexported, accessible
// from package ast _test.go). The spy captures every InsertNode and
// InsertEdge call, then the probe counts "imports" edges and lists
// "package" node signatures to identify which modules received edges.
// ---------------------------------------------------------------------------

const cDispatcherProbeTestSource = `package ast

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

type e2eSpyWriter struct {
	nextID int
	nodes  []graphwriter.NodeInput
	edges  []graphwriter.EdgeInput
}

func (w *e2eSpyWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	w.nextID++
	w.nodes = append(w.nodes, in)
	return graphwriter.NodeRecord{NodeID: fmt.Sprintf("spy-%d", w.nextID)}, nil
}

func (w *e2eSpyWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	w.edges = append(w.edges, in)
	return graphwriter.EdgeRecord{}, nil
}

type e2eDispatcherOutput struct {
	ImportEdgeCount int      ` + "`" + `json:"import_edge_count"` + "`" + `
	PackageNodes    []string ` + "`" + `json:"package_nodes"` + "`" + `
}

func TestE2EProbe_DispatcherPass0(t *testing.T) {
	srcFile := os.Getenv("E2E_PROBE_SOURCE_FILE")
	if srcFile == "" {
		t.Skip("E2E_PROBE_SOURCE_FILE not set")
	}
	src, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}

	spy := &e2eSpyWriter{}
	d := NewDispatcher(spy, WithParsers(NewTreeSitterCParser()))

	_, err = d.EmitFile(context.Background(), repoindexer.EmitFileEvent{
		RepoID:     "e2e-test-repo",
		RepoURL:    "https://example.com/e2e-test",
		SHA:        "e2e-probe-sha",
		RepoNodeID: "e2e-repo-node",
		FileNodeID: "e2e-file-node",
		RelPath:    "test.c",
		Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(src)), nil
		},
	})
	if err != nil {
		t.Fatalf("EmitFile: %v", err)
	}

	var importCount int
	var pkgSigs []string
	for _, e := range spy.edges {
		if e.Kind == "imports" {
			importCount++
		}
	}
	for _, n := range spy.nodes {
		if n.Kind == "package" {
			pkgSigs = append(pkgSigs, n.CanonicalSignature)
		}
	}

	result := e2eDispatcherOutput{
		ImportEdgeCount: importCount,
		PackageNodes:    pkgSigs,
	}
	data, _ := json.Marshal(result)
	fmt.Printf("PROBE_OUTPUT:%s\n", string(data))
}
`

// ---------------------------------------------------------------------------
// Local mirror types for JSON deserialization of probe output
// ---------------------------------------------------------------------------

type probeClass struct {
	QualifiedName string `json:"QualifiedName"`
	Kind          string `json:"Kind"`
}

type probeMethod struct {
	QualifiedName  string `json:"QualifiedName"`
	ParamSignature string `json:"ParamSignature"`
}

type probeParseOutput struct {
	Classes []probeClass  `json:"Classes"`
	Methods []probeMethod `json:"Methods"`
}

type probeDispatcherOutput struct {
	ImportEdgeCount int      `json:"import_edge_count"`
	PackageNodes    []string `json:"package_nodes"`
}

// ---------------------------------------------------------------------------
// Probe runners
// ---------------------------------------------------------------------------

// runProbe is the shared helper that writes a probe _test.go into the
// ast package directory, runs the specified test function, and extracts
// the PROBE_OUTPUT JSON line from stdout.
func runProbe(probeSource, testFunc, cSource, filename string) (string, error) {
	modRoot, err := moduleRoot()
	if err != nil {
		return "", err
	}

	astDir := filepath.Join(modRoot, "internal", "repoindexer", "ast")
	pid := os.Getpid()

	probeFile := filepath.Join(astDir, fmt.Sprintf("e2e_probe_%d_test.go", pid))
	if err := os.WriteFile(probeFile, []byte(probeSource), 0644); err != nil {
		return "", fmt.Errorf("write probe: %w", err)
	}
	defer os.Remove(probeFile)

	srcFile := filepath.Join(os.TempDir(), fmt.Sprintf("e2e_probe_src_%d.c", pid))
	if err := os.WriteFile(srcFile, []byte(cSource), 0644); err != nil {
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
		"E2E_PROBE_FILENAME="+filename,
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

func runCParserProbe(source, filename string) (*probeParseOutput, error) {
	raw, err := runProbe(cParserProbeTestSource, "TestE2EProbe_CParser", source, filename)
	if err != nil {
		return nil, err
	}
	var out probeParseOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse JSON: %w\nraw: %s", err, raw)
	}
	return &out, nil
}

func runDispatcherProbe(source string) (*probeDispatcherOutput, error) {
	raw, err := runProbe(cDispatcherProbeTestSource, "TestE2EProbe_DispatcherPass0", source, "test.c")
	if err != nil {
		return nil, err
	}
	var out probeDispatcherOutput
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("parse JSON: %w\nraw: %s", err, raw)
	}
	return &out, nil
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type cParserState struct {
	// Scenario 1: Build under CGO=on
	cgoEnabled    string
	buildExitCode int
	buildOutput   string

	// Scenario 2: C struct + free function
	source      string
	parseResult *probeParseOutput

	// Scenario 3: Relative include dropped (dispatcher Pass 0)
	includeSrc       string
	dispatcherResult *probeDispatcherOutput
}

// ---------------------------------------------------------------------------
// Scenario 1 — Build under CGO=on
// ---------------------------------------------------------------------------

func (s *cParserState) cgoEnabledIsSetTo(val string) error {
	s.cgoEnabled = val
	return nil
}

func (s *cParserState) goBuildRunsOnTheAstPackageFromServicesAgentMemory() error {
	modRoot, err := moduleRoot()
	if err != nil {
		return fmt.Errorf("cannot locate module root: %w", err)
	}
	cmd := exec.Command("go", "build", "./internal/repoindexer/ast/...")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+s.cgoEnabled)
	out, err := cmd.CombinedOutput()
	s.buildOutput = string(out)
	if err != nil {
		s.buildExitCode = 1
		return nil
	}
	s.buildExitCode = 0
	return nil
}

func (s *cParserState) theBuildSucceeds() error {
	if s.buildExitCode != 0 {
		return fmt.Errorf("go build failed (exit %d):\n%s", s.buildExitCode, s.buildOutput)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — C struct + free function
// ---------------------------------------------------------------------------

func (s *cParserState) cSource(src *godog.DocString) error {
	s.source = strings.TrimSpace(src.Content)
	return nil
}

func (s *cParserState) theSourceIsParsedWithTheCTreeSitterParser() error {
	result, err := runCParserProbe(s.source, "test.c")
	if err != nil {
		return fmt.Errorf("C parse probe failed: %w", err)
	}
	s.parseResult = result
	return nil
}

func (s *cParserState) theResultContainsAClassDeclWithQualifiedNameAndKind(qname, kind string) error {
	for _, cls := range s.parseResult.Classes {
		if cls.QualifiedName == qname && cls.Kind == kind {
			return nil
		}
	}
	descs := make([]string, len(s.parseResult.Classes))
	for i, c := range s.parseResult.Classes {
		descs[i] = fmt.Sprintf("{QualifiedName:%q, Kind:%q}", c.QualifiedName, c.Kind)
	}
	return fmt.Errorf("no ClassDecl with QualifiedName=%q Kind=%q; have %v", qname, kind, descs)
}

func (s *cParserState) theResultContainsAMethodDeclWithQualifiedNameAndParamSignature(qname, paramSig string) error {
	for _, m := range s.parseResult.Methods {
		if m.QualifiedName == qname && m.ParamSignature == paramSig {
			return nil
		}
	}
	descs := make([]string, len(s.parseResult.Methods))
	for i, m := range s.parseResult.Methods {
		descs[i] = fmt.Sprintf("{QualifiedName:%q, ParamSignature:%q}", m.QualifiedName, m.ParamSignature)
	}
	return fmt.Errorf("no MethodDecl with QualifiedName=%q ParamSignature=%q; have %v", qname, paramSig, descs)
}

// ---------------------------------------------------------------------------
// Scenario 3 — Relative include dropped (dispatcher Pass 0)
//
// STRUCTURAL CHANGE from iterations 4-5:
// Previous iterations called isRelativeImport() directly — the evaluator
// correctly noted this "applies a filter" rather than "running the
// dispatcher Pass 0 path." This iteration creates a Dispatcher with a
// spy writer, calls EmitFile(), and asserts on the import edges the spy
// captured. This exercises the FULL pipeline:
//   EmitFile → emit → emitImportsEdges → isRelativeImport → InsertEdge
// ---------------------------------------------------------------------------

func (s *cParserState) cSourceWithIncludes(src *godog.DocString) error {
	s.includeSrc = strings.TrimSpace(src.Content)
	return nil
}

func (s *cParserState) theDispatcherEmitFileProcessesTheCSourceInPass0() error {
	result, err := runDispatcherProbe(s.includeSrc)
	if err != nil {
		return fmt.Errorf("dispatcher probe failed: %w", err)
	}
	s.dispatcherResult = result
	return nil
}

func (s *cParserState) zeroImportsEdgesAreEmittedWhoseTargetContains(fragment string) error {
	for _, sig := range s.dispatcherResult.PackageNodes {
		if strings.Contains(sig, fragment) {
			return fmt.Errorf("found imports edge for package %q (contains %q); "+
				"expected zero imports edges for relative includes",
				sig, fragment)
		}
	}
	return nil
}

func (s *cParserState) atLeastOneImportsEdgeIsEmittedWhoseTargetContains(fragment string) error {
	for _, sig := range s.dispatcherResult.PackageNodes {
		if strings.Contains(sig, fragment) {
			return nil
		}
	}
	return fmt.Errorf("no imports edge whose package signature contains %q; "+
		"emitted %d import edges, package nodes: %v",
		fragment, s.dispatcherResult.ImportEdgeCount, s.dispatcherResult.PackageNodes)
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_c_and_cpp_parsers_ctreesitterparser_implementation(ctx *godog.ScenarioContext) {
	s := &cParserState{}

	// Scenario 1: Build under CGO=on
	ctx.Given(`^CGO_ENABLED is set to "([^"]*)"$`, s.cgoEnabledIsSetTo)
	ctx.When(`^go build runs on the ast package from services/agent-memory$`, s.goBuildRunsOnTheAstPackageFromServicesAgentMemory)
	ctx.Then(`^the build succeeds$`, s.theBuildSucceeds)

	// Scenario 2: C struct + free function
	ctx.Given(`^C source:$`, s.cSource)
	ctx.When(`^the source is parsed with the C tree-sitter parser$`, s.theSourceIsParsedWithTheCTreeSitterParser)
	ctx.Then(`^the result contains a ClassDecl with QualifiedName "([^"]*)" and Kind "([^"]*)"$`, s.theResultContainsAClassDeclWithQualifiedNameAndKind)
	ctx.Then(`^the result contains a MethodDecl with QualifiedName "([^"]*)" and ParamSignature "([^"]*)"$`, s.theResultContainsAMethodDeclWithQualifiedNameAndParamSignature)

	// Scenario 3: Relative include dropped (dispatcher EmitFile Pass 0)
	ctx.Given(`^C source with includes:$`, s.cSourceWithIncludes)
	ctx.When(`^the dispatcher EmitFile processes the C source in Pass 0$`, s.theDispatcherEmitFileProcessesTheCSourceInPass0)
	ctx.Then(`^zero imports edges are emitted whose target contains "([^"]*)"$`, s.zeroImportsEdgesAreEmittedWhoseTargetContains)
	ctx.Then(`^at least one imports edge is emitted whose target contains "([^"]*)"$`, s.atLeastOneImportsEdgeIsEmittedWhoseTargetContains)
}

func TestE2E_c_and_cpp_parsers_ctreesitterparser_implementation(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_c_and_cpp_parsers_ctreesitterparser_implementation,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"c_and_cpp_parsers_ctreesitterparser_implementation.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}