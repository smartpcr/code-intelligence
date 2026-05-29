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
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// ---------------------------------------------------------------------------
// Probe — a _test.go injected at runtime into internal/repoindexer/ast/
// that constructs a Dispatcher from the unexported defaultParsers() and
// checks whether the extension map routes the given filename. This
// exercises the REAL defaultParsers() → NewDispatcher → extMap path
// that parsers_cgo.go provides when CGO_ENABLED=1.
//
// The probe accepts E2E_PROBE_FILENAME via env var.
// ---------------------------------------------------------------------------

const goRegProbeSource = `package ast

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

type e2eGoRegResult struct {
	Language string ` + "`" + `json:"language"` + "`" + `
	Found    bool   ` + "`" + `json:"found"` + "`" + `
}

func TestE2EProbe_GoRegistration(t *testing.T) {
	filename := os.Getenv("E2E_PROBE_FILENAME")
	if filename == "" {
		t.Skip("E2E_PROBE_FILENAME not set")
	}

	// Exercise the real defaultParsers() and NewDispatcher path.
	parsers := defaultParsers()
	d := NewDispatcher(parsers, nil, nil)
	ext := filepath.Ext(filename)
	p, found := d.extMap[ext]

	out := e2eGoRegResult{Found: found}
	if found {
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

type goRegProbeOutput struct {
	Language string `json:"language"`
	Found    bool   `json:"found"`
}

// ---------------------------------------------------------------------------
// moduleRoot_goReg locates the Go module root (directory containing
// go.mod) by walking up from this source file's location.
// ---------------------------------------------------------------------------

func moduleRoot_goReg() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	// test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT -> 3 levels up to module root
	for i := 0; i < 3; i++ {
		dir = filepath.Dir(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("go.mod not found at %s: %w", dir, err)
	}
	return dir, nil
}

// ---------------------------------------------------------------------------
// runGoRegProbe writes the probe _test.go into the ast package,
// runs it with CGO_ENABLED=1, and returns the parsed JSON result.
// ---------------------------------------------------------------------------

func runGoRegProbe(filename string) (*goRegProbeOutput, error) {
	modRoot, err := moduleRoot_goReg()
	if err != nil {
		return nil, err
	}

	astDir := filepath.Join(modRoot, "internal", "repoindexer", "ast")
	pid := os.Getpid()

	probeFile := filepath.Join(astDir, fmt.Sprintf("e2e_probe_go_reg_%d_test.go", pid))
	if err := os.WriteFile(probeFile, []byte(goRegProbeSource), 0644); err != nil {
		return nil, fmt.Errorf("write probe: %w", err)
	}
	defer os.Remove(probeFile)

	env := append(os.Environ(),
		"CGO_ENABLED=1",
		"E2E_PROBE_FILENAME="+filename,
	)

	cmd := exec.Command("go", "test",
		"-run", "TestE2EProbe_GoRegistration",
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
			var out goRegProbeOutput
			if err := json.Unmarshal([]byte(raw), &out); err != nil {
				return nil, fmt.Errorf("parse JSON: %w\nraw: %s", err, raw)
			}
			return &out, nil
		}
	}
	return nil, fmt.Errorf("PROBE_OUTPUT marker not found in output:\n%s", string(output))
}

// ---------------------------------------------------------------------------
// Fakes for the CGO=off EmitFile scenario
// ---------------------------------------------------------------------------

// fakeWriter records whether InsertNode or InsertEdge was called.
type fakeWriter struct {
	nodeCount int
	edgeCount int
}

func (w *fakeWriter) InsertNode(n ast.Node) error {
	w.nodeCount++
	return nil
}

func (w *fakeWriter) InsertEdge(e ast.Edge) error {
	w.edgeCount++
	return nil
}

// captureLogger records structured log events.
type captureLogger struct {
	entries []ast.LogEntry
}

func (l *captureLogger) Log(msg string, attrs map[string]string) {
	l.entries = append(l.entries, ast.LogEntry{Message: msg, Attrs: attrs})
}

// ---------------------------------------------------------------------------
// Scenario state for stage 2.2: register-go-parser-in-parsers-cgo-go
// ---------------------------------------------------------------------------

type goRegState struct {
	// Scenario 1: CGO=on routing (probe result)
	probeResult *goRegProbeOutput

	// Scenario 2: CGO=off EmitFile
	dispatcher *ast.Dispatcher
	writer     *fakeWriter
	logger     *captureLogger
	emitResult ast.EmitResult
	emitErr    error
}

// ---------------------------------------------------------------------------
// Given: the dispatcher constructed with defaultParsers under CGO=on
//
// This is a precondition marker. The actual defaultParsers() exercise
// happens in the When step via the probe subprocess (which constructs
// a Dispatcher from defaultParsers() inside the ast package where the
// unexported function is accessible).
// ---------------------------------------------------------------------------

func (s *goRegState) theDispatcherConstructedWithDefaultParsersUnderCGOOn() error {
	return nil
}

// ---------------------------------------------------------------------------
// Given: the dispatcher constructed with defaultParsers under CGO=off
//
// We simulate the CGO=off path by constructing a Dispatcher with an
// empty parser set (what defaultParsers() returns when //go:build !cgo
// is active). This ensures EmitFile actually exercises the skip path.
// ---------------------------------------------------------------------------

func (s *goRegState) theDispatcherConstructedWithDefaultParsersUnderCGOOff() error {
	s.writer = &fakeWriter{}
	s.logger = &captureLogger{}
	// Simulate CGO=off: construct dispatcher with NO parsers.
	// Under a real CGO=off build, defaultParsers() returns nil.
	// We replicate that here so the test is non-vacuous even
	// when the binary was compiled with CGO=on.
	s.dispatcher = ast.NewDispatcher(nil, s.writer, s.logger)
	return nil
}

// ---------------------------------------------------------------------------
// When: selectParser runs for "<filename>"
//
// Uses the probe subprocess to exercise defaultParsers() →
// NewDispatcher → extMap lookup inside the ast package with
// CGO_ENABLED=1.
// ---------------------------------------------------------------------------

func (s *goRegState) selectParserRunsFor(filename string) error {
	result, err := runGoRegProbe(filename)
	if err != nil {
		return fmt.Errorf("selectParser probe for %q failed: %w", filename, err)
	}
	if !result.Found {
		return fmt.Errorf("selectParser(%q) returned nil — extension not registered in defaultParsers()", filename)
	}
	s.probeResult = result
	return nil
}

// ---------------------------------------------------------------------------
// When: EmitFile processes a "<ext>" file
// ---------------------------------------------------------------------------

func (s *goRegState) emitFileProcessesAFile(ext string) error {
	filename := "bar" + ext
	s.emitResult, s.emitErr = s.dispatcher.EmitFile(filename, []byte("package main\n"))
	return nil
}

// ---------------------------------------------------------------------------
// Then: the returned parser Language is "<expected>"
// ---------------------------------------------------------------------------

func (s *goRegState) theReturnedParserLanguageIs(expected string) error {
	if s.probeResult == nil {
		return fmt.Errorf("no probe result — the When step did not run")
	}
	if s.probeResult.Language != expected {
		return fmt.Errorf("Language() = %q, want %q", s.probeResult.Language, expected)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then: the structured log emits ast.dispatch.skip with reason "<reason>"
// ---------------------------------------------------------------------------

func (s *goRegState) theStructuredLogEmitsSkipWithReason(reason string) error {
	if s.emitErr != nil {
		return fmt.Errorf("EmitFile returned unexpected error: %v", s.emitErr)
	}
	if len(s.logger.entries) == 0 {
		return fmt.Errorf("no log entries captured; expected ast.dispatch.skip")
	}
	for _, entry := range s.logger.entries {
		if entry.Message == "ast.dispatch.skip" {
			got := entry.Attrs["reason"]
			if got != reason {
				return fmt.Errorf("ast.dispatch.skip reason = %q, want %q", got, reason)
			}
			return nil
		}
	}
	msgs := make([]string, len(s.logger.entries))
	for i, e := range s.logger.entries {
		msgs[i] = e.Message
	}
	return fmt.Errorf("no ast.dispatch.skip entry found; logged messages: %v", msgs)
}

// ---------------------------------------------------------------------------
// Then: no Node or Edge is inserted
// ---------------------------------------------------------------------------

func (s *goRegState) noNodeOrEdgeIsInserted() error {
	if s.writer.nodeCount != 0 {
		return fmt.Errorf("expected 0 node inserts, got %d", s.writer.nodeCount)
	}
	if s.writer.edgeCount != 0 {
		return fmt.Errorf("expected 0 edge inserts, got %d", s.writer.edgeCount)
	}
	if s.emitResult.NodeCount != 0 || s.emitResult.EdgeCount != 0 {
		return fmt.Errorf("EmitResult = {Nodes:%d, Edges:%d}, want zero",
			s.emitResult.NodeCount, s.emitResult.EdgeCount)
	}
	return nil
}

// ---------------------------------------------------------------------------
// requireEnv is a package-local helper that skips the test when a
// required environment variable is unset.
// ---------------------------------------------------------------------------

func requireEnv_go_parser_register(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("required env var %s is not set", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_go_parser_register_go_parser_in_parsers_cgo_go(ctx *godog.ScenarioContext) {
	s := &goRegState{}

	// Scenario 1: CGO=on routing
	ctx.Given(`^the dispatcher constructed with defaultParsers under CGO=on$`, s.theDispatcherConstructedWithDefaultParsersUnderCGOOn)
	ctx.When(`^selectParser runs for "([^"]*)"$`, s.selectParserRunsFor)
	ctx.Then(`^the returned parser Language is "([^"]*)"$`, s.theReturnedParserLanguageIs)

	// Scenario 2: CGO=off EmitFile
	ctx.Given(`^the dispatcher constructed with defaultParsers under CGO=off$`, s.theDispatcherConstructedWithDefaultParsersUnderCGOOff)
	ctx.When(`^EmitFile processes a "([^"]*)" file$`, s.emitFileProcessesAFile)
	ctx.Then(`^the structured log emits ast\.dispatch\.skip with reason "([^"]*)"$`, s.theStructuredLogEmitsSkipWithReason)
	ctx.Then(`^no Node or Edge is inserted$`, s.noNodeOrEdgeIsInserted)
}

func TestE2E_go_parser_register_go_parser_in_parsers_cgo_go(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_go_parser_register_go_parser_in_parsers_cgo_go,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"go_parser_register_go_parser_in_parsers_cgo_go.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}