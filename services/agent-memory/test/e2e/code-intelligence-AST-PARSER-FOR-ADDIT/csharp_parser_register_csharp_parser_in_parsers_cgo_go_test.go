//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// ---------------------------------------------------------------------------
// Probe — a _test.go injected at runtime into internal/repoindexer/ast/
// that creates a Dispatcher with the default (CGO) parser set and calls
// the unexported selectParser method.  This exercises the REAL
// defaultParsers() → buildExtMap() → selectParser() registration path
// that parsers_cgo.go provides when CGO_ENABLED=1.
// ---------------------------------------------------------------------------

const csharpRegProbeSource = `package ast

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

type e2eRegSpyWriter struct{}

func (e2eRegSpyWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	return graphwriter.NodeRecord{NodeID: "spy-reg"}, nil
}

func (e2eRegSpyWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	return graphwriter.EdgeRecord{}, nil
}

type e2eRegResult struct {
	Language string ` + "`" + `json:"language"` + "`" + `
	Found    bool   ` + "`" + `json:"found"` + "`" + `
}

func TestE2EProbe_CSharpRegistration(t *testing.T) {
	filename := os.Getenv("E2E_PROBE_FILENAME")
	if filename == "" {
		t.Skip("E2E_PROBE_FILENAME not set")
	}

	// NewDispatcher with no WithParsers option uses defaultParsers(),
	// which under CGO_ENABLED=1 includes NewTreeSitterCSharpParser().
	d := NewDispatcher(&e2eRegSpyWriter{})
	p := d.selectParser(filename, nil)

	out := e2eRegResult{Found: p != nil}
	if p != nil {
		out.Language = p.Language()
	}

	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	fmt.Printf("PROBE_OUTPUT:%s\n", string(data))
}
`

// ---------------------------------------------------------------------------
// Local mirror type for JSON deserialization of probe output
// ---------------------------------------------------------------------------

type csharpRegProbeOutput struct {
	Language string `json:"language"`
	Found    bool   `json:"found"`
}

// ---------------------------------------------------------------------------
// runCSharpRegProbe writes the probe _test.go into the ast package,
// runs it with CGO_ENABLED=1, and returns the parsed JSON result.
// ---------------------------------------------------------------------------

func runCSharpRegProbe(filename string) (*csharpRegProbeOutput, error) {
	modRoot, err := moduleRoot()
	if err != nil {
		return nil, err
	}

	astDir := filepath.Join(modRoot, "internal", "repoindexer", "ast")
	pid := os.Getpid()

	probeFile := filepath.Join(astDir, fmt.Sprintf("e2e_probe_reg_%d_test.go", pid))
	if err := os.WriteFile(probeFile, []byte(csharpRegProbeSource), 0644); err != nil {
		return nil, fmt.Errorf("write probe: %w", err)
	}
	defer os.Remove(probeFile)

	cmd := exec.Command("go", "test",
		"-run", "TestE2EProbe_CSharpRegistration",
		"-v", "-count=1",
		"./internal/repoindexer/ast/",
	)
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=1",
		"E2E_PROBE_FILENAME="+filename,
	)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("probe failed: %v\noutput:\n%s", err, string(output))
	}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PROBE_OUTPUT:") {
			raw := strings.TrimPrefix(line, "PROBE_OUTPUT:")
			var out csharpRegProbeOutput
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return nil, fmt.Errorf("parse JSON: %w\nraw: %s", err, raw)
			}
			return &out, nil
		}
	}
	return nil, fmt.Errorf("PROBE_OUTPUT marker not found in output:\n%s", string(output))
}

// ---------------------------------------------------------------------------
// Scenario state for stage 4.2: register-csharp-parser-in-parsers-cgo-go
// ---------------------------------------------------------------------------

type csharpRegState struct {
	probeResult *csharpRegProbeOutput
}

// ---------------------------------------------------------------------------
// Given: the dispatcher under CGO=on
//
// This step is a precondition marker. The actual CGO enforcement happens
// when the probe subprocess runs with CGO_ENABLED=1, which selects the
// parsers_cgo.go defaultParsers() path containing the C# tree-sitter
// parser.
// ---------------------------------------------------------------------------

func (s *csharpRegState) theDispatcherUnderCGOOn() error {
	return nil
}

// ---------------------------------------------------------------------------
// When: selectParser runs for "foo.cs" / "foo.csx"
//
// Runs the probe inside the ast package with CGO_ENABLED=1 so that
// defaultParsers() includes NewTreeSitterCSharpParser().  The probe
// calls the real Dispatcher.selectParser() and reports the result.
// ---------------------------------------------------------------------------

func (s *csharpRegState) selectParserRunsFor(filename string) error {
	result, err := runCSharpRegProbe(filename)
	if err != nil {
		return fmt.Errorf("selectParser probe for %q failed: %w", filename, err)
	}
	if !result.Found {
		return fmt.Errorf("selectParser(%q, nil) returned nil — extension not registered in defaultParsers()", filename)
	}
	s.probeResult = result
	return nil
}

// ---------------------------------------------------------------------------
// Then: the selected parser Language is "csharp"
// ---------------------------------------------------------------------------

func (s *csharpRegState) theSelectedParserLanguageIs(expected string) error {
	if s.probeResult == nil {
		return fmt.Errorf("no probe result — the When step did not run")
	}
	if s.probeResult.Language != expected {
		return fmt.Errorf("Language() = %q, want %q", s.probeResult.Language, expected)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_csharp_parser_register_csharp_parser_in_parsers_cgo_go(ctx *godog.ScenarioContext) {
	s := &csharpRegState{}

	ctx.Given(`^the dispatcher under CGO=on$`, s.theDispatcherUnderCGOOn)
	ctx.When(`^selectParser runs for "([^"]*)"$`, s.selectParserRunsFor)
	ctx.Then(`^the selected parser Language is "([^"]*)"$`, s.theSelectedParserLanguageIs)
}

func TestE2E_csharp_parser_register_csharp_parser_in_parsers_cgo_go(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_csharp_parser_register_csharp_parser_in_parsers_cgo_go,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"csharp_parser_register_csharp_parser_in_parsers_cgo_go.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
