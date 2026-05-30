//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// requireEnv skips the test when a required env var is unset.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v, ok := os.LookupEnv(name)
	if !ok || v == "" {
		t.Skipf("required env var %s is not set — skipping", name)
	}
	return v
}

// repoRoot returns the repository root by walking up from this source file
// until we leave the services/agent-memory subtree (go.mod lives there).
func repoRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	// thisFile is <repo>/services/agent-memory/test/e2e/code-intelligence-AST-PARSER-FOR-ADDIT/<file>.go
	// Walk up 5 levels to reach repo root.
	dir := filepath.Dir(thisFile)
	for i := 0; i < 5; i++ {
		dir = filepath.Dir(dir)
	}
	// Sanity-check: services/agent-memory/go.mod should exist under dir.
	gomod := filepath.Join(dir, "services", "agent-memory", "go.mod")
	if _, err := os.Stat(gomod); err != nil {
		return "", fmt.Errorf("repo root sanity check failed: %s not found from %s: %w", gomod, dir, err)
	}
	return dir, nil
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type docMatrixState struct {
	fileContent string
	grepPattern string
	matchLines  []string
}

// ---------------------------------------------------------------------------
// Step implementations
// ---------------------------------------------------------------------------

func (s *docMatrixState) theEditedFile(relPath string) error {
	root, err := repoRoot()
	if err != nil {
		return fmt.Errorf("cannot locate repo root: %w", err)
	}
	absPath := filepath.Join(root, filepath.FromSlash(relPath))
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", absPath, err)
	}
	s.fileContent = string(data)
	return nil
}

func (s *docMatrixState) grepForPatternRunsAgainstFile(pattern string) error {
	s.grepPattern = pattern
	s.matchLines = nil
	for _, line := range strings.Split(s.fileContent, "\n") {
		if strings.Contains(line, pattern) {
			s.matchLines = append(s.matchLines, line)
		}
	}
	return nil
}

func (s *docMatrixState) itMatchesNonEmptyLine() error {
	if len(s.matchLines) == 0 {
		return fmt.Errorf("grep for %q matched zero lines in tests.md", s.grepPattern)
	}
	for _, line := range s.matchLines {
		if strings.TrimSpace(line) != "" {
			return nil
		}
	}
	return fmt.Errorf("grep for %q matched only blank lines", s.grepPattern)
}

func (s *docMatrixState) itMatchesAtLeastOneLine() error {
	if len(s.matchLines) == 0 {
		return fmt.Errorf("grep for %q matched zero lines in tests.md", s.grepPattern)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_cross_cutting_tests_documentation_validation_documentation_support_matrix_update(ctx *godog.ScenarioContext) {
	s := &docMatrixState{}

	ctx.Given(`^the edited "([^"]*)"$`, s.theEditedFile)
	ctx.When(`^a grep for "([^"]*)" is run against the file$`, s.grepForPatternRunsAgainstFile)
	ctx.When(`^grep for "([^"]*)" runs$`, s.grepForPatternRunsAgainstFile)
	ctx.Then(`^it matches a non-empty line in the new matrix$`, s.itMatchesNonEmptyLine)
	ctx.Then(`^it matches at least one line$`, s.itMatchesAtLeastOneLine)
}

func TestE2E_cross_cutting_tests_documentation_validation_documentation_support_matrix_update(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_cross_cutting_tests_documentation_validation_documentation_support_matrix_update,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"cross_cutting_tests_documentation_validation_documentation_support_matrix_update.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}