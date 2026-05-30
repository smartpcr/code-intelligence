//go:build e2e

package e2e

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type degradationMatrixState struct {
	repoRoot    string
	coverageMD  string // raw content of COVERAGE.md
	testsMD     string // raw content of .claude/context/tests.md
	langRows    map[string]string // language -> source parser file found
}

// repoRoot returns the repository root (5 levels up from this test file
// in test/e2e/code-intelligence-REPO-SCANNER/ -> services/agent-memory/ -> repo root).
func repoRootFromTestFile() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	// test/e2e/code-intelligence-REPO-SCANNER -> test/e2e -> test -> services/agent-memory -> repo root
	for i := 0; i < 5; i++ {
		dir = filepath.Dir(dir)
	}
	// Sanity check: .claude/context should exist at the repo root.
	if _, err := os.Stat(filepath.Join(dir, ".claude", "context")); err != nil {
		return "", fmt.Errorf(".claude/context not found at repo root %s: %w", dir, err)
	}
	return dir, nil
}

// expectedLanguages lists the eight languages the coverage doc must contain,
// mapped to a substring that must appear in the source-parser-file column
// of the same row.
var expectedLanguages = map[string]string{
	"TypeScript / JavaScript": "parser_treesitter.go",
	"Python":                  "parser_treesitter.go",
	"C":                       "parser_treesitter_c.go",
	"C++":                     "parser_treesitter_cpp.go",
	"C#":                      "parser_treesitter_csharp.go",
	"Go":                      "parser_treesitter_go.go",
	"Rust":                    "parser_treesitter_rust.go",
	"PowerShell":              "parser_powershell.go",
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *degradationMatrixState) aFreshClone() error {
	root, err := repoRootFromTestFile()
	if err != nil {
		return err
	}
	s.repoRoot = root
	s.langRows = make(map[string]string)
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *degradationMatrixState) catCoverageRuns(relPath string) error {
	// Normalize separators for the host OS.
	absPath := filepath.Join(s.repoRoot, filepath.FromSlash(relPath))
	content, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", absPath, err)
	}
	s.coverageMD = string(content)
	return nil
}

func (s *degradationMatrixState) markdownLinkCheckerRunsAgainst(relPath string) error {
	absPath := filepath.Join(s.repoRoot, filepath.FromSlash(relPath))
	content, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("cannot read %s: %w", absPath, err)
	}
	s.testsMD = string(content)
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *degradationMatrixState) allEightLanguageRowsArePresent() error {
	// Parse the per-language coverage matrix table from COVERAGE.md.
	// Each data row is split on "|" and the first column is trimmed and
	// compared exactly, avoiding fragile substring matches (e.g. "C" vs "C++").
	scanner := bufio.NewScanner(strings.NewReader(s.coverageMD))
	tableRowRe := regexp.MustCompile(`^\|[^|]+\|`)
	for scanner.Scan() {
		line := scanner.Text()
		if !tableRowRe.MatchString(line) {
			continue
		}
		// Skip header separator rows (| --- | --- |).
		if strings.Contains(line, "---") && !strings.ContainsAny(line, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 2 {
			continue
		}
		// cols[0] is empty (before the leading "|"), cols[1] is the language column.
		firstCol := strings.TrimSpace(cols[1])
		for lang, parserFile := range expectedLanguages {
			if firstCol == lang && strings.Contains(line, parserFile) {
				s.langRows[lang] = parserFile
			}
		}
	}

	var missing []string
	for lang := range expectedLanguages {
		if _, found := s.langRows[lang]; !found {
			missing = append(missing, lang)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("COVERAGE.md is missing rows for: %s", strings.Join(missing, ", "))
	}
	return nil
}

func (s *degradationMatrixState) eachRowNamesItsSourceParserFile(pattern string) error {
	// Already verified during allEightLanguageRowsArePresent — each matched
	// row contained both the language name and a parser_*.go filename.
	// Double-check the glob pattern matches.
	for lang, parserFile := range s.langRows {
		matched, err := filepath.Match(pattern, parserFile)
		if err != nil {
			return fmt.Errorf("bad glob pattern %q: %w", pattern, err)
		}
		if !matched {
			return fmt.Errorf("language %q references %q which does not match %q", lang, parserFile, pattern)
		}
	}
	return nil
}

func (s *degradationMatrixState) linkToCoverageResolvesToExistingFile(target string) error {
	// Extract relative Markdown links from tests.md that reference COVERAGE.md.
	linkRe := regexp.MustCompile(`\]\(([^)]+` + regexp.QuoteMeta(target) + `[^)]*)\)`)
	matches := linkRe.FindAllStringSubmatch(s.testsMD, -1)
	if len(matches) == 0 {
		return fmt.Errorf("no Markdown link referencing %q found in tests.md", target)
	}

	testsDir := filepath.Join(s.repoRoot, ".claude", "context")
	for _, m := range matches {
		relLink := m[1]
		// Strip any anchor fragment (e.g., #section).
		if idx := strings.Index(relLink, "#"); idx >= 0 {
			relLink = relLink[:idx]
		}
		absTarget := filepath.Join(testsDir, filepath.FromSlash(relLink))
		if _, err := os.Stat(absTarget); err != nil {
			return fmt.Errorf("link %q in tests.md resolves to %s which does not exist: %w", m[1], absTarget, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_parser_coverage_verification_degradation_matrix_in_docs(ctx *godog.ScenarioContext) {
	s := &degradationMatrixState{}

	// Given
	ctx.Given(`^a fresh clone$`, s.aFreshClone)

	// When
	ctx.When(`^"cat ([^"]*)" runs$`, s.catCoverageRuns)
	ctx.When(`^a Markdown link checker runs against "([^"]*)"$`, s.markdownLinkCheckerRunsAgainst)

	// Then
	ctx.Then(`^all eight language rows are present$`, s.allEightLanguageRowsArePresent)
	ctx.Then(`^each row names its source "([^"]*)" file$`, s.eachRowNamesItsSourceParserFile)
	ctx.Then(`^the link to "([^"]*)" resolves to an existing file$`, s.linkToCoverageResolvesToExistingFile)
}

func TestE2E_parser_coverage_verification_degradation_matrix_in_docs(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile), "parser_coverage_verification_degradation_matrix_in_docs.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_parser_coverage_verification_degradation_matrix_in_docs,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{featurePath},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}