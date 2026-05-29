//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
)

// moduleRoot_pwsh returns the services/agent-memory directory.
func moduleRoot_pwsh() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	// thisFile is <mod>/test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT/<file>.go
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
// Scenario state
// ---------------------------------------------------------------------------

type powershellParserState struct {
	// Scenario 1: Build
	buildCGO1ExitCode int
	buildCGO1Output   string
	buildCGO0ExitCode int
	buildCGO0Output   string

	// Scenario 2: Sentinel
	parser      ast.LanguageParser
	parseResult ast.ParseResult
	parseErr    error

	// Scenario 3: Timeout
	slowParser      ast.LanguageParser
	slowParseResult ast.ParseResult
	slowParseErr    error
	fakePwshDir     string
}

// ---------------------------------------------------------------------------
// Scenario 1 — Build under both CGO=on and CGO=off
// ---------------------------------------------------------------------------

func (s *powershellParserState) theFileHasNoBuildTags() error {
	return nil
}

func (s *powershellParserState) goBuildAstRunsUnderCGO1() error {
	modRoot, err := moduleRoot_pwsh()
	if err != nil {
		return fmt.Errorf("cannot locate module root: %w", err)
	}
	cmd := exec.Command("go", "build", "./internal/repoindexer/ast/...")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	s.buildCGO1Output = string(out)
	if err != nil {
		s.buildCGO1ExitCode = 1
		return nil
	}
	s.buildCGO1ExitCode = 0
	return nil
}

func (s *powershellParserState) goBuildAstRunsUnderCGO0() error {
	modRoot, err := moduleRoot_pwsh()
	if err != nil {
		return fmt.Errorf("cannot locate module root: %w", err)
	}
	cmd := exec.Command("go", "build", "./internal/repoindexer/ast/...")
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	s.buildCGO0Output = string(out)
	if err != nil {
		s.buildCGO0ExitCode = 1
		return nil
	}
	s.buildCGO0ExitCode = 0
	return nil
}

func (s *powershellParserState) bothBuildsSucceed() error {
	if s.buildCGO1ExitCode != 0 {
		return fmt.Errorf("go build with CGO_ENABLED=1 failed (exit %d):\n%s",
			s.buildCGO1ExitCode, s.buildCGO1Output)
	}
	if s.buildCGO0ExitCode != 0 {
		return fmt.Errorf("go build with CGO_ENABLED=0 failed (exit %d):\n%s",
			s.buildCGO0ExitCode, s.buildCGO0Output)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2 — pwsh missing returns sentinel
// ---------------------------------------------------------------------------

func (s *powershellParserState) parserWithPwshAbsent() error {
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "")
	s.parser = ast.NewPowerShellParser()
	os.Setenv("PATH", origPath)
	return nil
}

func (s *powershellParserState) parseRunsOnAbsentParser(filename, source string) error {
	res, err := s.parser.Parse(filename, []byte(source))
	s.parseResult = res
	s.parseErr = err
	return nil
}

func (s *powershellParserState) returnsErrParserUnavailable() error {
	if s.parseErr == nil {
		return fmt.Errorf("Parse returned nil error; want ErrParserUnavailable")
	}
	if !errors.Is(s.parseErr, ast.ErrParserUnavailable) {
		return fmt.Errorf("errors.Is(err, ErrParserUnavailable) = false; got err=%v", s.parseErr)
	}
	return nil
}

func (s *powershellParserState) parseResultIsEmpty() error {
	if len(s.parseResult.Classes) != 0 || len(s.parseResult.Methods) != 0 || len(s.parseResult.Imports) != 0 {
		return fmt.Errorf("ParseResult is not empty: classes=%d methods=%d imports=%d",
			len(s.parseResult.Classes), len(s.parseResult.Methods), len(s.parseResult.Imports))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 3 — pwsh timeout returns error not sentinel
// ---------------------------------------------------------------------------

func (s *powershellParserState) parserWithFakePwshThatSleeps() error {
	tmpDir, err := os.MkdirTemp("", "fake-pwsh-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	s.fakePwshDir = tmpDir

	var fakeBin string
	if runtime.GOOS == "windows" {
		// On Windows, exec.LookPath("pwsh") checks PATHEXT which includes .bat
		fakeBin = filepath.Join(tmpDir, "pwsh.bat")
		// ping -n 31 localhost blocks for ~30 seconds on Windows
		err = os.WriteFile(fakeBin, []byte("@echo off\r\nping -n 31 127.0.0.1 >nul\r\n"), 0o755)
	} else {
		fakeBin = filepath.Join(tmpDir, "pwsh")
		err = os.WriteFile(fakeBin, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	}
	if err != nil {
		return fmt.Errorf("failed to write fake pwsh: %w", err)
	}

	origPath := os.Getenv("PATH")
	os.Setenv("PATH", tmpDir+string(os.PathListSeparator)+origPath)
	s.slowParser = ast.NewPowerShellParser()
	os.Setenv("PATH", origPath)
	return nil
}

func (s *powershellParserState) parseRunsOnSlowParser(filename, source string) error {
	res, err := s.slowParser.Parse(filename, []byte(source))
	s.slowParseResult = res
	s.slowParseErr = err
	return nil
}

func (s *powershellParserState) returnsNonNilError() error {
	if s.slowParseErr == nil {
		return fmt.Errorf("Parse returned nil error; want non-nil timeout error")
	}
	return nil
}

func (s *powershellParserState) errorDoesNotWrapSentinel() error {
	if errors.Is(s.slowParseErr, ast.ErrParserUnavailable) {
		return fmt.Errorf("errors.Is(err, ErrParserUnavailable) = true on timeout; want false so safeParse routes to ast.parse.error")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_powershell_parser_powershellparser_subprocess_implementation(ctx *godog.ScenarioContext) {
	s := &powershellParserState{}

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if s.fakePwshDir != "" {
			os.RemoveAll(s.fakePwshDir)
		}
		return ctx, nil
	})

	// Scenario 1: Build
	ctx.Given(`^the file has no build tags$`, s.theFileHasNoBuildTags)
	ctx.When(`^go build \./internal/repoindexer/ast/\.\.\. runs from services/agent-memory under CGO_ENABLED=1$`, s.goBuildAstRunsUnderCGO1)
	ctx.When(`^go build \./internal/repoindexer/ast/\.\.\. runs from services/agent-memory under CGO_ENABLED=0$`, s.goBuildAstRunsUnderCGO0)
	ctx.Then(`^both builds succeed$`, s.bothBuildsSucceed)

	// Scenario 2: Sentinel
	ctx.Given(`^a PowerShell parser constructed with pwsh absent from PATH$`, s.parserWithPwshAbsent)
	ctx.When(`^Parse "([^"]*)" with source "([^"]*)" runs$`, s.parseRunsOnAbsentParser)
	ctx.Then(`^it returns an error wrapping ErrParserUnavailable$`, s.returnsErrParserUnavailable)
	ctx.Then(`^the ParseResult is empty$`, s.parseResultIsEmpty)

	// Scenario 3: Timeout
	ctx.Given(`^a PowerShell parser constructed with a fake pwsh that sleeps$`, s.parserWithFakePwshThatSleeps)
	ctx.When(`^Parse "([^"]*)" with source "([^"]*)" runs on the slow parser$`, s.parseRunsOnSlowParser)
	ctx.Then(`^it returns a non-nil error$`, s.returnsNonNilError)
	ctx.Then(`^the error does not wrap ErrParserUnavailable$`, s.errorDoesNotWrapSentinel)
}

func TestE2E_powershell_parser_powershellparser_subprocess_implementation(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_powershell_parser_powershellparser_subprocess_implementation,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"powershell_parser_powershellparser_subprocess_implementation.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}