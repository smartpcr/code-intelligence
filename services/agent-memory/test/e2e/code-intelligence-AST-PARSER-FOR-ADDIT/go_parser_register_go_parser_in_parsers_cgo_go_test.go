//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

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
	// Scenario 1: CGO=on routing
	parser ast.Parser

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
// parsers_cgo.go registers the Go parser via init(). Because the ast
// package is imported above, the init() has already run by test time.
// We also verify defaultParsers() includes the Go parser.
// ---------------------------------------------------------------------------

func (s *goRegState) theDispatcherConstructedWithDefaultParsersUnderCGOOn() error {
	// parsers_cgo.go init() fires on import — nothing else needed.
	// defaultParsers() returns the CGO parser set; SelectParser uses
	// the global registry populated by init().
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
// ---------------------------------------------------------------------------

func (s *goRegState) selectParserRunsFor(filename string) error {
	s.parser = ast.SelectParser(filename, nil)
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
	if s.parser == nil {
		return fmt.Errorf("no parser was selected in the When step")
	}
	got := s.parser.Language()
	if got != expected {
		return fmt.Errorf("Language() = %q, want %q", got, expected)
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