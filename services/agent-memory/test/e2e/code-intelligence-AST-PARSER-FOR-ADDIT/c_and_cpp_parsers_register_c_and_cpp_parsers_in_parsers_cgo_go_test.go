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
// the unexported pickParser method.  This exercises the REAL
// defaultParsers() → extension-map → pickParser() registration path
// that parsers_cgo.go provides when CGO_ENABLED=1.
//
// The probe accepts E2E_PROBE_FILENAME (the file name to route) and an
// optional E2E_PROBE_HINTS (comma-separated language hints).
// ---------------------------------------------------------------------------

const cAndCppRegProbeSource = `package ast

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

type e2eCCppRegResult struct {
	Language string ` + "`" + `json:"language"` + "`" + `
	Found    bool   ` + "`" + `json:"found"` + "`" + `
}

type e2eCCppRegSpyWriter struct{}

func TestE2EProbe_CCppRegistration(t *testing.T) {
	filename := os.Getenv("E2E_PROBE_FILENAME")
	if filename == "" {
		t.Skip("E2E_PROBE_FILENAME not set")
	}

	// Parse optional hints from env var.
	var hints []string
	if h := os.Getenv("E2E_PROBE_HINTS"); h != "" {
		hints = strings.Split(h, ",")
	}

	d := NewDispatcher(&e2eCCppRegSpyWriter{})
	p := d.selectParser(filename, hints)

	out := e2eCCppRegResult{Found: p != nil}
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

type cCppRegProbeOutput struct {
	Language string `json:"language"`
	Found    bool   `json:"found"`
}

// ---------------------------------------------------------------------------
// runCCppRegProbe writes the probe _test.go into the ast package,
// runs it with CGO_ENABLED=1, and returns the parsed JSON result.
// ---------------------------------------------------------------------------

func runCCppRegProbe(filename string, hints []string) (*cCppRegProbeOutput, error) {
	modRoot, err := moduleRoot()
	if err != nil {
		return nil, err
	}

	astDir := filepath.Join(modRoot, "internal", "repoindexer", "ast")
	pid := os.Getpid()

	probeFile := filepath.Join(astDir, fmt.Sprintf("e2e_probe_ccpp_reg_%d_test.go", pid))
	if err := os.WriteFile(probeFile, []byte(cAndCppRegProbeSource), 0644); err != nil {
		return nil, fmt.Errorf("write probe: %w", err)
	}
	defer os.Remove(probeFile)

	env := append(os.Environ(),
		"CGO_ENABLED=1",
		"E2E_PROBE_FILENAME="+filename,
	)
	if len(hints) > 0 {
		env = append(env, "E2E_PROBE_HINTS="+strings.Join(hints, ","))
	}

	cmd := exec.Command("go", "test",
		"-run", "TestE2EProbe_CCppRegistration",
		"-v", "-count=1",
		"./internal/repoindexer/ast/",
	)
	cmd.Dir = modRoot
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("probe failed: %v\noutput:\n%s", err, string(output))
	}

	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "PROBE_OUTPUT:") {
			raw := strings.TrimPrefix(line, "PROBE_OUTPUT:")
			var out cCppRegProbeOutput
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return nil, fmt.Errorf("parse JSON: %w\nraw: %s", err, raw)
			}
			return &out, nil
		}
	}
	return nil, fmt.Errorf("PROBE_OUTPUT marker not found in output:\n%s", string(output))
}

// ---------------------------------------------------------------------------
// Scenario state for stage 3.3: register-c-and-cpp-parsers-in-parsers-cgo-go
// ---------------------------------------------------------------------------

type cCppRegState struct {
	probeResult *cCppRegProbeOutput
}

// ---------------------------------------------------------------------------
// Given: the dispatcher under CGO=on
// ---------------------------------------------------------------------------

func (s *cCppRegState) theDispatcherUnderCGOOn() error {
	return nil
}

// ---------------------------------------------------------------------------
// When: selectParser runs for "foo.c" / "foo.cpp"
// ---------------------------------------------------------------------------

func (s *cCppRegState) selectParserRunsFor(filename string) error {
	result, err := runCCppRegProbe(filename, nil)
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
// When: selectParser runs for "foo.h" with no hints
// ---------------------------------------------------------------------------

func (s *cCppRegState) selectParserRunsForWithNoHints(filename string) error {
	result, err := runCCppRegProbe(filename, nil)
	if err != nil {
		return fmt.Errorf("selectParser probe for %q (no hints) failed: %w", filename, err)
	}
	if !result.Found {
		return fmt.Errorf("selectParser(%q, nil) returned nil — .h not registered", filename)
	}
	s.probeResult = result
	return nil
}

// ---------------------------------------------------------------------------
// When: selectParser runs for "foo.h" with hints "cpp"
// ---------------------------------------------------------------------------

func (s *cCppRegState) selectParserRunsForWithHints(filename, hints string) error {
	hintSlice := strings.Split(hints, ",")
	result, err := runCCppRegProbe(filename, hintSlice)
	if err != nil {
		return fmt.Errorf("selectParser probe for %q (hints=%v) failed: %w", filename, hintSlice, err)
	}
	if !result.Found {
		return fmt.Errorf("selectParser(%q, %v) returned nil — .h not registered", filename, hintSlice)
	}
	s.probeResult = result
	return nil
}

// ---------------------------------------------------------------------------
// Then: the selected parser Language is "c" / "cpp"
// ---------------------------------------------------------------------------

func (s *cCppRegState) theSelectedParserLanguageIs(expected string) error {
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

func InitializeScenario_c_and_cpp_parsers_register_c_and_cpp_parsers_in_parsers_cgo_go(ctx *godog.ScenarioContext) {
	s := &cCppRegState{}

	ctx.Given(`^the dispatcher under CGO=on$`, s.theDispatcherUnderCGOOn)
	ctx.When(`^selectParser runs for "([^"]*)"$`, s.selectParserRunsFor)
	ctx.When(`^selectParser runs for "([^"]*)" with no hints$`, s.selectParserRunsForWithNoHints)
	ctx.When(`^selectParser runs for "([^"]*)" with hints "([^"]*)"$`, s.selectParserRunsForWithHints)
	ctx.Then(`^the selected parser Language is "([^"]*)"$`, s.theSelectedParserLanguageIs)
}

func TestE2E_c_and_cpp_parsers_register_c_and_cpp_parsers_in_parsers_cgo_go(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_c_and_cpp_parsers_register_c_and_cpp_parsers_in_parsers_cgo_go,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"c_and_cpp_parsers_register_c_and_cpp_parsers_in_parsers_cgo_go.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}