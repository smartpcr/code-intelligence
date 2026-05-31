//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cucumber/godog"
)

// ---------------------------------------------------------------------------
// Binary build (once per test process)
// ---------------------------------------------------------------------------

var (
	codeintelBinary     string
	codeintelBuildOnce  sync.Once
	codeintelBuildError error
)

// buildCodeintelBinary compiles cmd/codeintel into a temp
// directory and returns the path to the resulting executable.
// The build is performed exactly once per process.
func buildCodeintelBinary() (string, error) {
	codeintelBuildOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			codeintelBuildError = fmt.Errorf("cannot locate module root: %w", err)
			return
		}
		tmpDir, err := os.MkdirTemp("", "codeintel-e2e-*")
		if err != nil {
			codeintelBuildError = fmt.Errorf("cannot create temp dir: %w", err)
			return
		}
		binName := "codeintel"
		if runtime.GOOS == "windows" {
			binName += ".exe"
		}
		binPath := filepath.Join(tmpDir, binName)
		cmd := exec.Command("go", "build", "-o", binPath, "./cmd/codeintel")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			codeintelBuildError = fmt.Errorf("go build failed: %w\n%s", err, string(out))
			return
		}
		codeintelBinary = binPath
	})
	return codeintelBinary, codeintelBuildError
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type cobraScaffoldingState struct {
	binaryPath string
	stdout     string
	stderr     string
	exitCode   int
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *cobraScaffoldingState) aBuiltCodeintelBinary() error {
	binPath, err := buildCodeintelBinary()
	if err != nil {
		return fmt.Errorf("failed to build codeintel binary: %w", err)
	}
	s.binaryPath = binPath
	s.stdout = ""
	s.stderr = ""
	s.exitCode = 0
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *cobraScaffoldingState) codeintelRunsWith(rawArgs string) error {
	args := strings.Fields(rawArgs)
	cmd := exec.Command(s.binaryPath, args...)
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	s.stdout = outBuf.String()
	s.stderr = errBuf.String()
	s.exitCode = 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			s.exitCode = exitErr.ExitCode()
		} else {
			return fmt.Errorf("exec failed: %w", err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *cobraScaffoldingState) stdoutNamesTheSubcommand(name string) error {
	if !strings.Contains(s.stdout, name) {
		return fmt.Errorf("stdout does not contain subcommand %q:\n%s", name, s.stdout)
	}
	return nil
}

func (s *cobraScaffoldingState) theExitCodeIsNonZero() error {
	if s.exitCode == 0 {
		return fmt.Errorf("expected non-zero exit code, but got 0\nstdout: %s\nstderr: %s", s.stdout, s.stderr)
	}
	return nil
}

func (s *cobraScaffoldingState) stderrNamesTheOffendingSubcommand(name string) error {
	if !strings.Contains(s.stderr, name) {
		return fmt.Errorf("stderr does not contain %q:\n%s", name, s.stderr)
	}
	return nil
}

func (s *cobraScaffoldingState) stderrContainsValidJSON() error {
	raw := strings.TrimSpace(s.stderr)
	if raw == "" {
		return fmt.Errorf("stderr is empty; expected at least one JSON log line")
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err == nil {
			return nil
		}
	}
	return fmt.Errorf("no line in stderr is valid JSON:\n%s", raw)
}

// ---------------------------------------------------------------------------
// Initializer & entrypoint
// ---------------------------------------------------------------------------

func InitializeScenario_codeintel_cli_binary_cobra_scaffolding(ctx *godog.ScenarioContext) {
	s := &cobraScaffoldingState{}

	ctx.Given(`^a built codeintel binary$`, s.aBuiltCodeintelBinary)
	ctx.When(`^codeintel runs with "([^"]*)"$`, s.codeintelRunsWith)
	ctx.Then(`^stdout names the subcommand "([^"]*)"$`, s.stdoutNamesTheSubcommand)
	ctx.Then(`^the exit code is non-zero$`, s.theExitCodeIsNonZero)
	ctx.Then(`^stderr names the offending subcommand "([^"]*)"$`, s.stderrNamesTheOffendingSubcommand)
	ctx.Then(`^stderr contains a line that is valid JSON parseable by encoding/json$`, s.stderrContainsValidJSON)
}

func TestE2E_codeintel_cli_binary_cobra_scaffolding(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_codeintel_cli_binary_cobra_scaffolding,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"codeintel_cli_binary_cobra_scaffolding.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}

	// Clean up the built binary.
	if codeintelBinary != "" {
		os.RemoveAll(filepath.Dir(codeintelBinary))
	}
}
