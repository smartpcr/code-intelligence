//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// requireEnv returns the value of the named environment variable,
// calling t.Skip when unset or empty.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// Shared state for rollout-playbook-and-operator-runbooks scenarios
// ---------------------------------------------------------------------------

type runbookState struct {
	repoRoot string

	// runbook-references-canonical-verbs
	runbookContent   string
	extractedVerbs   []string // ALL verb-like tokens found in the runbook
	canonicalFound   []string // subset of extractedVerbs that are canonical
	nonCanonicalHits []string // subset of extractedVerbs that are NOT canonical

	// changelog-lists-canonical-surface
	changelogPath    string
	changelogContent string
	v1Entry          string
}

// canonicalVerbs are the ONLY verb names that may appear in the runbook.
var canonicalVerbs = []string{
	"mgmt.register_repo",
	"mgmt.retract_sample",
	"mgmt.rescan",
	"mgmt.override",
	"policy.publish",
	"policy.activate",
	"eval.gate",
}

// canonicalVerbSet for O(1) lookup.
var canonicalVerbSet = func() map[string]bool {
	m := make(map[string]bool, len(canonicalVerbs))
	for _, v := range canonicalVerbs {
		m[v] = true
	}
	return m
}()

// verbPrefixes are the namespace prefixes whose dot-qualified tokens
// represent verbs in the clean-code service API surface.
var verbPrefixes = []string{"mgmt.", "policy.", "eval.", "ingest."}

// verbTokenRe matches any dot-qualified identifier that starts with a
// known verb prefix (e.g. mgmt.foo, policy.bar_baz, eval.gate).
// It intentionally captures multi-segment forms like mgmt.read.foo so
// they can be checked against the canonical set too.
var verbTokenRe = func() *regexp.Regexp {
	var escaped []string
	for _, p := range verbPrefixes {
		escaped = append(escaped, regexp.QuoteMeta(p))
	}
	// Match prefix followed by one or more word/dot segments.
	return regexp.MustCompile(`(?:` + strings.Join(escaped, "|") + `)[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*`)
}()

// pascalVerbRe catches PascalCase dot-separated names like
// Policy.Override.Add which are the known non-canonical forms.
var pascalVerbRe = regexp.MustCompile(`(?:Policy|Mgmt|Eval|Ingest)(?:\.[A-Z][a-zA-Z0-9]*)+`)

// canonicalFoundationMetrics is the canonical list of foundation metric_kind values.
var canonicalFoundationMetrics = []string{
	"cyclomatic_complexity",
	"cognitive_complexity",
	"duplication_ratio",
	"function_length",
	"parameter_count",
	"nesting_depth",
}

// canonicalSystemMetrics is the canonical list of system metric_kind values.
var canonicalSystemMetrics = []string{
	"coupling_between_objects",
	"afferent_coupling",
	"efferent_coupling",
	"instability_index",
	"lack_of_cohesion",
	"dependency_cycle_count",
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *runbookState) resolveRepoRoot() error {
	if s.repoRoot != "" {
		return nil
	}
	root := os.Getenv("CLEAN_CODE_REPO_ROOT")
	if root != "" {
		s.repoRoot = root
		return nil
	}
	// Walk up from test/e2e/code-intelligence-CLEAN-CODE -> services/clean-code
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}
	svcRoot := filepath.Join(wd, "..", "..", "..")
	svcRoot, _ = filepath.Abs(svcRoot)

	// Check if we're inside services/clean-code; go up 2 more for repo root.
	repoCandidate := filepath.Join(svcRoot, "..", "..")
	repoCandidate, _ = filepath.Abs(repoCandidate)
	if _, err := os.Stat(filepath.Join(repoCandidate, "services", "clean-code")); err == nil {
		s.repoRoot = repoCandidate
	} else {
		// We are probably running from within services/clean-code itself.
		s.repoRoot = svcRoot
	}
	return nil
}

// svcCleanCodeDir returns the absolute path to services/clean-code.
func (s *runbookState) svcCleanCodeDir() string {
	candidate := filepath.Join(s.repoRoot, "services", "clean-code")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	// repoRoot IS services/clean-code.
	return s.repoRoot
}

// findRunbookFiles locates markdown files that look like runbooks/playbooks/operator docs.
func (s *runbookState) findRunbookFiles() ([]string, error) {
	svcDir := s.svcCleanCodeDir()
	searchDirs := []string{
		filepath.Join(svcDir, "docs"),
		filepath.Join(svcDir, "runbooks"),
		filepath.Join(svcDir, "playbook"),
		svcDir,
	}
	patterns := []string{"*runbook*", "*playbook*", "*operator*", "*rollout*"}

	var files []string
	for _, dir := range searchDirs {
		for _, pat := range patterns {
			matches, _ := filepath.Glob(filepath.Join(dir, pat+".md"))
			files = append(files, matches...)
			matches, _ = filepath.Glob(filepath.Join(dir, "*", pat+".md"))
			files = append(files, matches...)
		}
	}

	seen := map[string]bool{}
	var unique []string
	for _, f := range files {
		abs, _ := filepath.Abs(f)
		if !seen[abs] {
			seen[abs] = true
			unique = append(unique, abs)
		}
	}
	return unique, nil
}

// ---------------------------------------------------------------------------
// Scenario: runbook-references-canonical-verbs
// ---------------------------------------------------------------------------

func (s *runbookState) theRunbookContent() error {
	if err := s.resolveRepoRoot(); err != nil {
		return err
	}
	files, err := s.findRunbookFiles()
	if err != nil {
		return fmt.Errorf("finding runbook files: %w", err)
	}
	if len(files) == 0 {
		return fmt.Errorf("no runbook/playbook markdown files found under %s", s.svcCleanCodeDir())
	}

	var sb strings.Builder
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return fmt.Errorf("reading %s: %w", f, err)
		}
		sb.WriteString(string(data))
		sb.WriteString("\n")
	}
	s.runbookContent = sb.String()
	return nil
}

func (s *runbookState) greppingForVerbNames() error {
	if s.runbookContent == "" {
		return fmt.Errorf("runbook content is empty")
	}

	seen := map[string]bool{}

	// Extract all lowercase dot-qualified verb tokens (mgmt.*, policy.*, eval.*, ingest.*).
	for _, m := range verbTokenRe.FindAllString(s.runbookContent, -1) {
		if !seen[m] {
			seen[m] = true
			s.extractedVerbs = append(s.extractedVerbs, m)
		}
	}

	// Extract PascalCase dot-separated verb tokens (Policy.Override.Add, etc.).
	for _, m := range pascalVerbRe.FindAllString(s.runbookContent, -1) {
		if !seen[m] {
			seen[m] = true
			s.extractedVerbs = append(s.extractedVerbs, m)
		}
	}

	// Classify each extracted verb as canonical or non-canonical.
	for _, v := range s.extractedVerbs {
		if canonicalVerbSet[v] {
			s.canonicalFound = append(s.canonicalFound, v)
		} else {
			s.nonCanonicalHits = append(s.nonCanonicalHits, v)
		}
	}

	return nil
}

func (s *runbookState) onlyCanonicalNamesAppear() error {
	if len(s.canonicalFound) == 0 {
		return fmt.Errorf("no canonical verb names found in runbook content; expected all of: %v\nextracted verbs: %v", canonicalVerbs, s.extractedVerbs)
	}

	// Reject any non-canonical verb tokens.
	if len(s.nonCanonicalHits) > 0 {
		sort.Strings(s.nonCanonicalHits)
		return fmt.Errorf("found %d non-canonical verb(s) in runbook content: %v\nonly these are allowed: %v",
			len(s.nonCanonicalHits), s.nonCanonicalHits, canonicalVerbs)
	}

	// Enforce that EVERY canonical verb appears in the runbook (completeness).
	foundSet := make(map[string]bool, len(s.canonicalFound))
	for _, v := range s.canonicalFound {
		foundSet[v] = true
	}
	var missing []string
	for _, v := range canonicalVerbs {
		if !foundSet[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("runbook is missing %d canonical verb(s): %v\nfound: %v",
			len(missing), missing, s.canonicalFound)
	}

	return nil
}

func (s *runbookState) nonCanonicalNamesAreAbsent(name1, name2 string) error {
	banned := []string{name1, name2}
	var found []string
	for _, b := range banned {
		if strings.Contains(s.runbookContent, b) {
			found = append(found, b)
		}
	}
	if len(found) > 0 {
		return fmt.Errorf("non-canonical verb(s) found in runbook content: %v; they must not appear", found)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: changelog-lists-canonical-surface
// ---------------------------------------------------------------------------

func (s *runbookState) theChangelogAt(relPath string) error {
	if err := s.resolveRepoRoot(); err != nil {
		return err
	}
	absPath := filepath.Join(s.repoRoot, filepath.FromSlash(relPath))
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("reading changelog at %s: %w", absPath, err)
	}
	s.changelogPath = absPath
	s.changelogContent = string(data)
	return nil
}

func (s *runbookState) parsingTheV1Entry() error {
	if s.changelogContent == "" {
		return fmt.Errorf("changelog content is empty")
	}

	// Extract the v1 section: heading "## v1" / "## [v1.0]" / "## 1.0" etc.
	re := regexp.MustCompile(`(?mi)^##\s+\[?v?1[\.\]0-9].*$`)
	loc := re.FindStringIndex(s.changelogContent)
	if loc == nil {
		// Fallback: use everything from first "v1" mention.
		idx := strings.Index(strings.ToLower(s.changelogContent), "v1")
		if idx < 0 {
			idx = strings.Index(s.changelogContent, "1.0")
		}
		if idx < 0 {
			return fmt.Errorf("no v1 entry found in changelog")
		}
		start := idx
		if start > 20 {
			start -= 20
		}
		s.v1Entry = s.changelogContent[start:]
	} else {
		rest := s.changelogContent[loc[0]:]
		nextH2 := regexp.MustCompile(`(?m)^##\s+`)
		locs := nextH2.FindAllStringIndex(rest, 2)
		if len(locs) > 1 {
			s.v1Entry = rest[:locs[1][0]]
		} else {
			s.v1Entry = rest
		}
	}
	return nil
}

func (s *runbookState) theSchemaIs(expected string) error {
	if !strings.Contains(s.v1Entry, expected) {
		return fmt.Errorf("v1 entry does not mention schema %q\nentry:\n%s", expected, truncate(s.v1Entry, 500))
	}
	return nil
}

func (s *runbookState) theVerdictValuesAre(expected string) error {
	vals := strings.Split(expected, "|")
	var missing []string
	for _, v := range vals {
		if !strings.Contains(s.v1Entry, v) {
			missing = append(missing, v)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("v1 entry is missing verdict values: %v\nentry:\n%s", missing, truncate(s.v1Entry, 500))
	}
	return nil
}

func (s *runbookState) overrideHasNoField(fieldName string) error {
	if strings.Contains(s.v1Entry, fieldName) {
		return fmt.Errorf("v1 entry references %q but it should not\nentry:\n%s", fieldName, truncate(s.v1Entry, 500))
	}
	return nil
}

func (s *runbookState) theFoundationAndSystemMetricKindCountsMatchTheCanonicalLists() error {
	entry := s.v1Entry

	// First try to parse a JSON block inside the v1 entry for structured counts.
	foundationCount, systemCount, err := s.extractMetricKindCountsFromJSON(entry)
	if err == nil {
		// Validate against canonical list sizes.
		wantFoundation := len(canonicalFoundationMetrics)
		wantSystem := len(canonicalSystemMetrics)
		if foundationCount != wantFoundation {
			return fmt.Errorf("v1 entry lists foundation metric_kind count=%d, want %d (canonical: %v)",
				foundationCount, wantFoundation, canonicalFoundationMetrics)
		}
		if systemCount != wantSystem {
			return fmt.Errorf("v1 entry lists system metric_kind count=%d, want %d (canonical: %v)",
				systemCount, wantSystem, canonicalSystemMetrics)
		}
		return nil
	}

	// Fallback: count occurrences of each canonical metric name in the text.
	var missingFoundation, missingSystem []string
	for _, m := range canonicalFoundationMetrics {
		if !strings.Contains(entry, m) {
			missingFoundation = append(missingFoundation, m)
		}
	}
	for _, m := range canonicalSystemMetrics {
		if !strings.Contains(entry, m) {
			missingSystem = append(missingSystem, m)
		}
	}

	// Also verify the entry states the correct counts.
	wantFoundation := len(canonicalFoundationMetrics)
	wantSystem := len(canonicalSystemMetrics)
	foundationCountStr := fmt.Sprintf("foundation: %d", wantFoundation)
	systemCountStr := fmt.Sprintf("system: %d", wantSystem)
	foundationCountAlt := fmt.Sprintf("foundation (%d)", wantFoundation)
	systemCountAlt := fmt.Sprintf("system (%d)", wantSystem)

	entryLower := strings.ToLower(entry)
	hasFoundationCount := strings.Contains(entryLower, foundationCountStr) || strings.Contains(entryLower, foundationCountAlt)
	hasSystemCount := strings.Contains(entryLower, systemCountStr) || strings.Contains(entryLower, systemCountAlt)

	if !hasFoundationCount && len(missingFoundation) > 0 {
		return fmt.Errorf("v1 entry does not list all foundation metric_kinds (missing: %v) and does not state the count (%d)\nentry:\n%s",
			missingFoundation, wantFoundation, truncate(entry, 500))
	}
	if !hasSystemCount && len(missingSystem) > 0 {
		return fmt.Errorf("v1 entry does not list all system metric_kinds (missing: %v) and does not state the count (%d)\nentry:\n%s",
			missingSystem, wantSystem, truncate(entry, 500))
	}

	return nil
}

// extractMetricKindCountsFromJSON tries to parse a JSON object from the
// v1 entry that contains metric_kind counts (e.g.
// {"foundation": 6, "system": 6}).
func (s *runbookState) extractMetricKindCountsFromJSON(entry string) (foundation, system int, err error) {
	// Look for ```json ... ``` blocks.
	jsonBlockRe := regexp.MustCompile("(?s)```json\\s*\n(.*?)```")
	matches := jsonBlockRe.FindAllStringSubmatch(entry, -1)
	for _, m := range matches {
		var obj map[string]interface{}
		if jsonErr := json.Unmarshal([]byte(m[1]), &obj); jsonErr != nil {
			continue
		}
		// Look for metric_kind or metric_kinds key.
		for _, key := range []string{"metric_kind", "metric_kinds", "metric_kind_counts"} {
			if val, ok := obj[key]; ok {
				if counts, ok := val.(map[string]interface{}); ok {
					f, fOk := counts["foundation"].(float64)
					sy, sOk := counts["system"].(float64)
					if fOk && sOk {
						return int(f), int(sy), nil
					}
				}
			}
		}
	}
	return 0, 0, fmt.Errorf("no JSON metric_kind counts found")
}

// truncate returns at most n characters of s, with "..." appended if truncated.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ======================================================================
// Godog wiring
// ======================================================================

func InitializeScenario_linked_mode_integration_and_rollout_rollout_playbook_and_operator_runbooks(ctx *godog.ScenarioContext) {
	s := &runbookState{}

	// runbook-references-canonical-verbs
	ctx.Step(`^the runbook content$`, s.theRunbookContent)
	ctx.Step(`^grepping for verb names$`, s.greppingForVerbNames)
	ctx.Step(`^only canonical names appear$`, s.onlyCanonicalNamesAppear)
	ctx.Step(`^non-canonical names "([^"]*)" and "([^"]*)" are absent$`, s.nonCanonicalNamesAreAbsent)

	// changelog-lists-canonical-surface
	ctx.Step(`^the changelog at "([^"]*)"$`, s.theChangelogAt)
	ctx.Step(`^parsing the v1 entry$`, s.parsingTheV1Entry)
	ctx.Step(`^the schema is "([^"]*)"$`, s.theSchemaIs)
	ctx.Step(`^the verdict values are "([^"]*)"$`, s.theVerdictValuesAre)
	ctx.Step(`^override has no "([^"]*)" field$`, s.overrideHasNoField)
	ctx.Step(`^the foundation and system metric_kind counts match the canonical lists$`, s.theFoundationAndSystemMetricKindCountsMatchTheCanonicalLists)
}

func TestE2E_linked_mode_integration_and_rollout_rollout_playbook_and_operator_runbooks(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_linked_mode_integration_and_rollout_rollout_playbook_and_operator_runbooks,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"linked_mode_integration_and_rollout_rollout_playbook_and_operator_runbooks.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}