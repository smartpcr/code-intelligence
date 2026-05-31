//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="hardening_and_release_documentation_and_release_notes_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// docReleaseNotesState holds per-scenario state for the
// documentation-and-release-notes acceptance scenarios.
type docReleaseNotesState struct {
	repoRoot string
	filePath string
	matches  []string
}

func newDocReleaseNotesState() *docReleaseNotesState {
	return &docReleaseNotesState{}
}

// resolveRepoRootDRN returns the repository root by walking up from the
// current test file (services/clean-code/test/e2e/code-intelligence-REFACTOR-GUIDE).
func resolveRepoRootDRN() string {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	// Walk up: code-intelligence-REFACTOR-GUIDE -> e2e -> test -> clean-code -> services -> repo-root
	return filepath.Join(dir, "..", "..", "..", "..", "..")
}

// --- Given steps ---

func (s *docReleaseNotesState) theUpdatedFile(relPath string) error {
	s.repoRoot = resolveRepoRootDRN()
	s.filePath = filepath.Join(s.repoRoot, filepath.FromSlash(relPath))
	info, err := os.Stat(s.filePath)
	if err != nil {
		return fmt.Errorf("file %s not found: %w", s.filePath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("expected a file at %s, got a directory", s.filePath)
	}
	return nil
}

func (s *docReleaseNotesState) theFile(relPath string) error {
	return s.theUpdatedFile(relPath)
}

// --- When steps ---

func (s *docReleaseNotesState) grepFixedStringRunsAgainstIt(pattern string) error {
	f, err := os.Open(s.filePath)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", s.filePath, err)
	}
	defer f.Close()

	s.matches = nil
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.Contains(line, pattern) {
			s.matches = append(s.matches, line)
		}
	}
	return scanner.Err()
}

// --- Then steps ---

func (s *docReleaseNotesState) itReturnsExactlyOneMatch() error {
	if len(s.matches) != 1 {
		return fmt.Errorf("expected exactly 1 match in %s, got %d", s.filePath, len(s.matches))
	}
	return nil
}

func (s *docReleaseNotesState) itReturnsAtLeastOneMatch() error {
	if len(s.matches) == 0 {
		return fmt.Errorf("expected at least one match in %s, got none", s.filePath)
	}
	return nil
}

func (s *docReleaseNotesState) itReturnsAtLeastOneMatchUnderHeading(heading string) error {
	if len(s.matches) == 0 {
		return fmt.Errorf("expected at least one match in %s, got none", s.filePath)
	}

	// Verify the heading exists and at least one match line appears after it
	// (before the next same-level heading).
	f, err := os.Open(s.filePath)
	if err != nil {
		return fmt.Errorf("cannot open %s: %w", s.filePath, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	inSection := false
	foundUnderHeading := false

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "## ") {
			if trimmed == heading {
				inSection = true
				continue
			} else if inSection {
				break
			}
		}

		if inSection {
			for _, m := range s.matches {
				if line == m {
					foundUnderHeading = true
					break
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	if !inSection {
		return fmt.Errorf("heading %q not found in %s", heading, s.filePath)
	}
	if !foundUnderHeading {
		return fmt.Errorf("matched lines exist in %s but none appear under the %s section", s.filePath, heading)
	}
	return nil
}

// --- Scenario initializer ---

func InitializeScenario_hardening_and_release_documentation_and_release_notes(ctx *godog.ScenarioContext) {
	s := newDocReleaseNotesState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		return ctx2, nil
	})

	// Given
	ctx.Step(`^the updated "([^"]*)"$`, s.theUpdatedFile)
	ctx.Step(`^the file "([^"]*)"$`, s.theFile)

	// When
	ctx.Step(`^grep -F "([^"]*)" runs against it$`, s.grepFixedStringRunsAgainstIt)

	// Then
	ctx.Step(`^it returns exactly one match$`, s.itReturnsExactlyOneMatch)
	ctx.Step(`^it returns at least one match$`, s.itReturnsAtLeastOneMatch)
	ctx.Step(`^it returns at least one match under an "([^"]*)" heading$`, s.itReturnsAtLeastOneMatchUnderHeading)
}

func TestE2E_hardening_and_release_documentation_and_release_notes(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_hardening_and_release_documentation_and_release_notes,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"hardening_and_release_documentation_and_release_notes.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
