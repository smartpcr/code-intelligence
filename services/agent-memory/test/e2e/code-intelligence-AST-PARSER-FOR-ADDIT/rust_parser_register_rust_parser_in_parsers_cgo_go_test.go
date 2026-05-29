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
// Scenario state for stage 5.2: register-rust-parser-in-parsers-cgo-go
// ---------------------------------------------------------------------------

type rustRegState struct {
	// Scenario 1: CGO=on routing
	parser ast.Parser

	// Scenario 2: CGO=off EmitFile path
	dispatcher *ast.Dispatcher
	mockWriter *rustMockWriter
	mockLogger *rustMockLogger
	emitResult ast.EmitResult
	emitErr    error
}

// ---------------------------------------------------------------------------
// Mock writer — implements ast.Writer, records calls
// ---------------------------------------------------------------------------

type rustMockWriter struct {
	nodeCalls int
	edgeCalls int
}

func (w *rustMockWriter) InsertNode(_ ast.Node) error {
	w.nodeCalls++
	return nil
}

func (w *rustMockWriter) InsertEdge(_ ast.Edge) error {
	w.edgeCalls++
	return nil
}

// ---------------------------------------------------------------------------
// Mock logger — implements ast.Logger, captures events
// ---------------------------------------------------------------------------

type rustLogEntry struct {
	Event  string
	Reason string
}

type rustMockLogger struct {
	entries []rustLogEntry
}

func (l *rustMockLogger) Log(event string, fields map[string]string) {
	l.entries = append(l.entries, rustLogEntry{
		Event:  event,
		Reason: fields["reason"],
	})
}

// ---------------------------------------------------------------------------
// Scenario 1 steps: .rs routes to Rust (CGO=on only)
//
// parsers_cgo.go carries //go:build cgo. Under CGO=on its init()
// registers the Rust parser in the global registry. Under CGO=off
// the file is excluded, so SelectParser returns nil and the
// scenario is marked pending.
// ---------------------------------------------------------------------------

func (s *rustRegState) theDispatcherUnderCGOOn() error {
	// Probe the global registry — if Rust is NOT registered, this
	// is a CGO=off build (parsers_cgo.go was excluded).
	if ast.SelectParser("probe.rs", nil) == nil {
		return godog.ErrPending
	}
	return nil
}

func (s *rustRegState) selectParserRunsFor(filename string) error {
	s.parser = ast.SelectParser(filename, nil)
	if s.parser == nil {
		return fmt.Errorf("SelectParser(%q, nil) returned nil — .rs extension not registered", filename)
	}
	return nil
}

func (s *rustRegState) theSelectedParserLanguageIs(expected string) error {
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
// Scenario 2 steps: .rs skipped under CGO=off
//
// Under CGO=off, parsers_nocgo.go is compiled and its
// defaultParsers() returns nil. DefaultParsers() (exported in
// parser.go) delegates to that function. The dispatcher
// constructed from it has zero parsers registered, so EmitFile
// logs ast.dispatch.skip with reason "no_parser" and writes zero
// Nodes / Edges.
//
// Under CGO=on, DefaultParsers() returns the Rust parser; this
// scenario does not apply and is marked pending.
// ---------------------------------------------------------------------------

func (s *rustRegState) theDispatcherConstructedUnderCGOOff() error {
	// Call the REAL DefaultParsers(). Under CGO=off this calls
	// parsers_nocgo.go's defaultParsers() → nil. Under CGO=on
	// it calls parsers_cgo.go's defaultParsers() → [Rust].
	parsers := ast.DefaultParsers()
	if len(parsers) > 0 {
		// CGO=on build: this scenario tests CGO=off behaviour.
		return godog.ErrPending
	}

	s.mockWriter = &rustMockWriter{}
	s.mockLogger = &rustMockLogger{}
	s.dispatcher = ast.NewDefaultDispatcher(parsers, s.mockWriter, s.mockLogger)
	return nil
}

func (s *rustRegState) emitFileProcessesAFile(ext string) error {
	filename := "bar" + ext
	result, err := s.dispatcher.EmitFile(filename, []byte("fn main() {}"))
	s.emitResult = result
	s.emitErr = err
	return nil
}

func (s *rustRegState) firesWithReason(event, reason string) error {
	for _, entry := range s.mockLogger.entries {
		if entry.Event == event && entry.Reason == reason {
			return nil
		}
	}
	return fmt.Errorf("expected log entry event=%q reason=%q, got %+v", event, reason, s.mockLogger.entries)
}

func (s *rustRegState) noNodesOrEdgesAreWritten() error {
	if s.emitErr != nil {
		return fmt.Errorf("expected nil error from EmitFile, got: %v", s.emitErr)
	}
	if s.mockWriter.nodeCalls != 0 {
		return fmt.Errorf("expected zero InsertNode calls, got %d", s.mockWriter.nodeCalls)
	}
	if s.mockWriter.edgeCalls != 0 {
		return fmt.Errorf("expected zero InsertEdge calls, got %d", s.mockWriter.edgeCalls)
	}
	zero := ast.EmitResult{}
	if s.emitResult != zero {
		return fmt.Errorf("expected zero EmitResult, got %+v", s.emitResult)
	}
	return nil
}

// ---------------------------------------------------------------------------
// requireEnv is a package-local helper that skips the test when a
// required environment variable is unset.
// ---------------------------------------------------------------------------

func requireEnv(t *testing.T, name string) string {
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

func InitializeScenario_rust_parser_register_rust_parser_in_parsers_cgo_go(ctx *godog.ScenarioContext) {
	s := &rustRegState{}

	// Scenario 1: CGO=on routing
	ctx.Given(`^the dispatcher under CGO=on$`, s.theDispatcherUnderCGOOn)
	ctx.When(`^selectParser runs for "([^"]*)"$`, s.selectParserRunsFor)
	ctx.Then(`^the selected parser Language is "([^"]*)"$`, s.theSelectedParserLanguageIs)

	// Scenario 2: CGO=off EmitFile path
	ctx.Given(`^the dispatcher constructed via defaultParsers under CGO=off$`, s.theDispatcherConstructedUnderCGOOff)
	ctx.When(`^EmitFile processes a "([^"]*)" file$`, s.emitFileProcessesAFile)
	ctx.Then(`^"([^"]*)" fires with reason "([^"]*)"$`, s.firesWithReason)
	ctx.Then(`^no Nodes or Edges are written$`, s.noNodesOrEdgesAreWritten)
}

func TestE2E_rust_parser_register_rust_parser_in_parsers_cgo_go(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_rust_parser_register_rust_parser_in_parsers_cgo_go,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"rust_parser_register_rust_parser_in_parsers_cgo_go.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}