//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// ---------------------------------------------------------------------------
// Mock writer — implements ast.Writer, records calls
// ---------------------------------------------------------------------------

type psMockWriter struct {
	nodeCalls int
	edgeCalls int
}

func (w *psMockWriter) InsertNode(_ ast.Node) error {
	w.nodeCalls++
	return nil
}

func (w *psMockWriter) InsertEdge(_ ast.Edge) error {
	w.edgeCalls++
	return nil
}

// ---------------------------------------------------------------------------
// Mock logger — implements ast.Logger, captures events
// ---------------------------------------------------------------------------

type psLogEntry struct {
	Event  string
	Reason string
}

type psMockLogger struct {
	entries []psLogEntry
}

func (l *psMockLogger) Log(event string, fields map[string]string) {
	l.entries = append(l.entries, psLogEntry{
		Event:  event,
		Reason: fields["reason"],
	})
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type psRegState struct {
	// Scenarios 1 & 2: constructed via defaultParsers()
	parsers        []ast.Parser
	selectedParser ast.Parser

	// Scenario 3: pwsh-not-available dispatcher path
	dispatcher *ast.Dispatcher
	mockWriter *psMockWriter
	mockLogger *psMockLogger
	emitResult ast.EmitResult
	emitErr    error
}

// ---------------------------------------------------------------------------
// Scenarios 1 & 2: .ps1 routes to PowerShell
//
// Both parsers_cgo.go and parsers_nocgo.go register the PowerShell
// subprocess parser, so DefaultParsers() includes it regardless of
// CGO mode. However, a single test binary is compiled under exactly
// one build mode, so the scenario whose mode parameter does NOT match
// the compiled binary is skipped via godog.ErrPending.
//
// Full E2E coverage requires TWO go test invocations:
//   CGO_ENABLED=1 go test -tags e2e ./...
//   CGO_ENABLED=0 go test -tags e2e ./...
// Add a CI matrix entry to ensure both are exercised.
// ---------------------------------------------------------------------------

// detectedCGOMode infers the build mode from DefaultParsers().
// CGO=on includes tree-sitter parsers (Language != "powershell");
// CGO=off only has the PowerShell subprocess parser.
func detectedCGOMode() string {
	for _, p := range ast.DefaultParsers() {
		if p.Language() != "powershell" {
			return "on"
		}
	}
	return "off"
}

func (s *psRegState) dispatcherViaDefaultParsers(mode string) error {
	actual := detectedCGOMode()
	if mode != actual {
		return fmt.Errorf("%w: scenario requires CGO=%s but binary was built with CGO=%s",
			godog.ErrPending, mode, actual)
	}
	s.parsers = ast.DefaultParsers()
	if len(s.parsers) == 0 {
		return fmt.Errorf("DefaultParsers() returned empty list (expected PowerShell under CGO=%s)", mode)
	}
	return nil
}

func (s *psRegState) selectParserRunsFor(filename string) error {
	ext := filepath.Ext(filename)
	for _, p := range s.parsers {
		for _, e := range p.Extensions() {
			if e == ext {
				s.selectedParser = p
				return nil
			}
		}
	}
	return fmt.Errorf("no parser in DefaultParsers() handles extension %q", ext)
}

func (s *psRegState) selectedParserLanguageIs(expected string) error {
	if s.selectedParser == nil {
		return fmt.Errorf("no parser was selected in the When step")
	}
	got := s.selectedParser.Language()
	if got != expected {
		return fmt.Errorf("Language() = %q, want %q", got, expected)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 3: pwsh-not-available logs skip-not-error
//
// Constructs a PowerShell parser with pwsh absent from PATH, wraps it
// in a Dispatcher with a mock logger, and verifies that EmitFile logs
// ast.dispatch.skip (not ast.parse.error) and returns (EmitResult{}, nil).
// ---------------------------------------------------------------------------

func (s *psRegState) hostWithoutPwshOnPath() error {
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "")
	defer os.Setenv("PATH", origPath)
	p := ast.NewPowerShellParser()

	s.mockWriter = &psMockWriter{}
	s.mockLogger = &psMockLogger{}
	s.dispatcher = ast.NewDefaultDispatcher([]ast.Parser{p}, s.mockWriter, s.mockLogger)
	return nil
}

func (s *psRegState) emitFileProcesses(ext string) error {
	result, err := s.dispatcher.EmitFile("test"+ext, []byte("function Foo {}"))
	s.emitResult = result
	s.emitErr = err
	return nil
}

func (s *psRegState) dispatchSkipFiresNotError(skipEvent, reason, errorEvent string) error {
	// Verify that the error event was NOT logged.
	for _, entry := range s.mockLogger.entries {
		if entry.Event == errorEvent {
			return fmt.Errorf("unexpected %q logged: %+v", errorEvent, entry)
		}
	}
	// Verify that the expected skip event was logged with the right reason.
	for _, entry := range s.mockLogger.entries {
		if entry.Event == skipEvent && entry.Reason == reason {
			return nil
		}
	}
	return fmt.Errorf("expected log entry event=%q reason=%q, got %+v", skipEvent, reason, s.mockLogger.entries)
}

func (s *psRegState) emitFileReturnsZeroWithNilError() error {
	if s.emitErr != nil {
		return fmt.Errorf("expected nil error from EmitFile, got: %v", s.emitErr)
	}
	zero := ast.EmitResult{}
	if s.emitResult != zero {
		return fmt.Errorf("expected zero EmitResult, got %+v", s.emitResult)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_powershell_parser_register_powershell_parser_in_both_build_tag_files(ctx *godog.ScenarioContext) {
	s := &psRegState{}

	// Scenarios 1 & 2: construct via defaultParsers()
	ctx.Given(`^the dispatcher constructed via defaultParsers\(\) under CGO=(on|off)$`, s.dispatcherViaDefaultParsers)
	ctx.When(`^selectParser\("([^"]*)", nil\) runs$`, s.selectParserRunsFor)
	ctx.Then(`^Language\(\) == "([^"]*)"$`, s.selectedParserLanguageIs)

	// Scenario 3: pwsh-not-available
	ctx.Given(`^a host without pwsh on PATH$`, s.hostWithoutPwshOnPath)
	ctx.When(`^EmitFile processes a "([^"]*)" file$`, s.emitFileProcesses)
	ctx.Then(`^"([^"]*)" fires with reason "([^"]*)" not "([^"]*)"$`, s.dispatchSkipFiresNotError)
	ctx.Then(`^EmitFile returns zero EmitResult with nil error$`, s.emitFileReturnsZeroWithNilError)
}

func TestE2E_powershell_parser_register_powershell_parser_in_both_build_tag_files(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_powershell_parser_register_powershell_parser_in_both_build_tag_files,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"powershell_parser_register_powershell_parser_in_both_build_tag_files.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}