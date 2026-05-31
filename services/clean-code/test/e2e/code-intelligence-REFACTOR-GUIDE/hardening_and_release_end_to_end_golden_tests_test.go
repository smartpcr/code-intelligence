//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="hardening_and_release_end_to_end_golden_tests_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// ---------------------------------------------------------------------------
// State shared across steps for one scenario.
// ---------------------------------------------------------------------------

type e2eGoldenState struct {
	scenarioDir  string
	scenarioName string
	binaryPath   string

	exitCode int
	stdout   string
	stderr   string

	findingsJSON []byte
	promptsJSONL []byte
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (s *e2eGoldenState) resolveModuleRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	// thisFile = <mod>/test/e2e/code-intelligence-REFACTOR-GUIDE/<file>.go
	return filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", "..", ".."))
}

func (s *e2eGoldenState) ensureBinary() error {
	if s.binaryPath != "" {
		return nil
	}
	root := s.resolveModuleRoot()
	name := "cleanc"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(root, "bin", name)
	if _, err := os.Stat(p); err != nil {
		return fmt.Errorf("cleanc binary not found at %s — run 'make build' first: %w", p, err)
	}
	s.binaryPath = p
	return nil
}

func (s *e2eGoldenState) readScenarioMeta(name string) (string, error) {
	p := filepath.Join(s.scenarioDir, name)
	data, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", p, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// e2eGoldenFindBash locates a working bash binary.
// On Windows, prefers Git Bash over WSL bash to avoid filesystem issues.
func e2eGoldenFindBash() (string, error) {
	if runtime.GOOS == "windows" {
		candidates := []string{
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
	}
	p, err := exec.LookPath("bash")
	if err != nil {
		return "", fmt.Errorf("bash not found: %w", err)
	}
	return p, nil
}

// e2eGoldenNormalize replaces volatile fields (UUIDs, timestamps,
// absolute paths) and sorts finding lines in the "## Findings"
// section so that byte-comparison is deterministic across
// runs and machines.
func e2eGoldenNormalize(data []byte, moduleRoot string) []byte {
	s := string(data)

	// Normalise the module root in both slash orientations.
	fwdRoot := strings.ReplaceAll(moduleRoot, `\`, "/")
	if fwdRoot != moduleRoot {
		s = strings.ReplaceAll(s, moduleRoot, "/NORMALIZED_ROOT")
	}
	s = strings.ReplaceAll(s, fwdRoot, "/NORMALIZED_ROOT")

	// Replace UUID v4/v5 hex patterns.
	uuidRe := regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	s = uuidRe.ReplaceAllString(s, "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX")

	// Replace ISO-8601 / RFC-3339 timestamps (with optional fractional seconds).
	tsRe := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})`)
	s = tsRe.ReplaceAllString(s, "XXXX-XX-XXTXX:XX:XXZ")

	// Sort finding lines in the "## Findings" section of report.md.
	// Findings are rendered as "- <rule> [<severity>]" lines.
	// The engine sorts findings by random UUID, so the order varies.
	s = e2eGoldenSortFindingSection(s)

	return []byte(s)
}

// e2eGoldenSortFindingSection sorts the bullet lines inside the
// "## Findings" section of a rendered report.md. Lines are
// identified as "- " prefixed lines between the "## Findings"
// header and the next "##" header (or EOF).
func e2eGoldenSortFindingSection(s string) string {
	const header = "## Findings"
	idx := strings.Index(s, header)
	if idx < 0 {
		return s
	}
	// Find the start of finding lines (after the header line).
	afterHeader := idx + len(header)
	nlAfterHeader := strings.Index(s[afterHeader:], "\n")
	if nlAfterHeader < 0 {
		return s
	}
	findingsStart := afterHeader + nlAfterHeader + 1

	// Find the end of the findings section (next "##" or EOF).
	rest := s[findingsStart:]
	findingsEnd := len(s)
	nextSection := strings.Index(rest, "\n##")
	if nextSection >= 0 {
		findingsEnd = findingsStart + nextSection
	}

	// Extract the section and sort only the "- " lines.
	section := s[findingsStart:findingsEnd]
	lines := strings.Split(section, "\n")
	var findingLines []string
	var preamble []string
	seenFinding := false
	for _, l := range lines {
		if strings.HasPrefix(l, "- ") {
			findingLines = append(findingLines, l)
			seenFinding = true
		} else if !seenFinding {
			preamble = append(preamble, l)
		}
	}
	sort.Strings(findingLines)

	var sorted []string
	sorted = append(sorted, preamble...)
	sorted = append(sorted, findingLines...)
	return s[:findingsStart] + strings.Join(sorted, "\n") + s[findingsEnd:]
}

// e2eGoldenDiff produces a readable diff summary for golden mismatches.
func e2eGoldenDiff(want, got string, maxLines int) string {
	wantLines := strings.Split(want, "\n")
	gotLines := strings.Split(got, "\n")
	var sb strings.Builder
	shown := 0
	for i := 0; i < len(wantLines) || i < len(gotLines); i++ {
		if shown >= maxLines {
			fmt.Fprintf(&sb, "... (%d more differing lines)\n", max(len(wantLines), len(gotLines))-i)
			break
		}
		w, g := "", ""
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if w != g {
			fmt.Fprintf(&sb, "line %d:\n  want: %s\n  got:  %s\n", i+1, w, g)
			shown++
		}
	}
	return sb.String()
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *e2eGoldenState) theScenarioDirectory(dir string) error {
	root := s.resolveModuleRoot()
	s.scenarioDir = filepath.Join(root, filepath.FromSlash(dir))
	s.scenarioName = filepath.Base(strings.TrimSuffix(dir, "/"))
	if _, err := os.Stat(s.scenarioDir); err != nil {
		return fmt.Errorf("scenario dir %s: %w", s.scenarioDir, err)
	}
	return nil
}

func (s *e2eGoldenState) theCleancDevBinaryIsBuilt() error {
	return s.ensureBinary()
}

// ---------------------------------------------------------------------------
// When step — ALL scenarios execute via the scenario's run.sh
// ---------------------------------------------------------------------------

func (s *e2eGoldenState) runShExecutesForScenario() error {
	if err := s.ensureBinary(); err != nil {
		return err
	}

	// Delete stale artifacts so we don't read leftovers from prior runs.
	for _, f := range []string{"findings.json", "report.md", "prompts.jsonl"} {
		os.Remove(filepath.Join(s.scenarioDir, f))
	}

	bashPath, err := e2eGoldenFindBash()
	if err != nil {
		return err
	}

	runSh := filepath.Join(s.scenarioDir, "run.sh")
	if _, err := os.Stat(runSh); err != nil {
		return fmt.Errorf("run.sh not found in %s: %w", s.scenarioDir, err)
	}

	// Fix CRLF/BOM — git on Windows may check out with CRLF which breaks bash.
	scriptBytes, err := os.ReadFile(runSh)
	if err != nil {
		return fmt.Errorf("read run.sh: %w", err)
	}
	modified := false
	if len(scriptBytes) >= 3 && scriptBytes[0] == 0xEF && scriptBytes[1] == 0xBB && scriptBytes[2] == 0xBF {
		scriptBytes = scriptBytes[3:]
		modified = true
	}
	if bytes.Contains(scriptBytes, []byte("\r")) {
		scriptBytes = bytes.ReplaceAll(scriptBytes, []byte("\r"), nil)
		modified = true
	}
	if modified {
		if err := os.WriteFile(runSh, scriptBytes, 0o755); err != nil {
			return fmt.Errorf("fix run.sh line endings: %w", err)
		}
	}

	binaryPathForBash := strings.ReplaceAll(s.binaryPath, `\`, "/")
	runShForBash := strings.ReplaceAll(runSh, `\`, "/")

	cmd := exec.Command(bashPath, runShForBash)
	cmd.Dir = s.scenarioDir
	cmd.Env = append(os.Environ(), "CLEANC_BINARY_PATH="+binaryPathForBash)
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	runErr := cmd.Run()
	s.stdout = stdoutBuf.String()
	s.stderr = stderrBuf.String()
	if runErr != nil {
		if exitErr, ok := runErr.(*exec.ExitError); ok {
			s.exitCode = exitErr.ExitCode()
		} else {
			return fmt.Errorf("run.sh execution failed: %w\nstdout:\n%s\nstderr:\n%s",
				runErr, s.stdout, s.stderr)
		}
	} else {
		s.exitCode = 0
	}

	// Read artifacts that run.sh wrote to the scenario directory.
	s.findingsJSON, _ = os.ReadFile(filepath.Join(s.scenarioDir, "findings.json"))
	s.promptsJSONL, _ = os.ReadFile(filepath.Join(s.scenarioDir, "prompts.jsonl"))

	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *e2eGoldenState) exitCodeMatchesScenarioExpected() error {
	metaStr, err := s.readScenarioMeta("expected_exit_code")
	if err != nil {
		return err
	}
	expected, err := strconv.Atoi(metaStr)
	if err != nil {
		return fmt.Errorf("parse expected_exit_code %q: %w", metaStr, err)
	}
	if s.exitCode != expected {
		return fmt.Errorf("expected exit code %d, got %d\nstdout:\n%s\nstderr:\n%s",
			expected, s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

func (s *e2eGoldenState) theObservedExitCodeEquals(expected int) error {
	if s.exitCode != expected {
		return fmt.Errorf("expected exit code %d, got %d\nstdout:\n%s\nstderr:\n%s",
			expected, s.exitCode, s.stdout, s.stderr)
	}
	return nil
}

// artifactByteMatchesGolden normalises both the actual artifact
// (produced by run.sh) and the committed golden file, then performs
// a byte-level comparison. This satisfies the acceptance criterion
// "report.md byte-matches golden" while accounting for the non-
// deterministic fields the binary produces (random UUIDs, wall-clock
// timestamps, machine-specific absolute paths).
//
// Set UPDATE_GOLDEN=1 to regenerate the golden file from the current
// binary output (normalised).
func (s *e2eGoldenState) artifactByteMatchesGolden(artifact, scenario string) error {
	moduleRoot := s.resolveModuleRoot()

	// Read the actual artifact produced by run.sh.
	actualPath := filepath.Join(s.scenarioDir, artifact)
	actual, err := os.ReadFile(actualPath)
	if err != nil {
		return fmt.Errorf("read actual %s: %w (run.sh may not have produced it)\nstdout:\n%s\nstderr:\n%s",
			artifact, err, s.stdout, s.stderr)
	}
	if len(actual) == 0 {
		return fmt.Errorf("%s was produced but is empty", artifact)
	}

	normActual := e2eGoldenNormalize(actual, moduleRoot)

	// Update mode: regenerate golden from current binary output.
	goldenPath := filepath.Join(moduleRoot, "tests", "e2e", "cleanc", "scenarios", scenario, "golden", artifact)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			return fmt.Errorf("mkdir golden: %w", err)
		}
		if err := os.WriteFile(goldenPath, normActual, 0o644); err != nil {
			return fmt.Errorf("write golden: %w", err)
		}
		return nil
	}

	// Read the committed golden file.
	golden, err := os.ReadFile(goldenPath)
	if err != nil {
		return fmt.Errorf("read golden %s: %w (run UPDATE_GOLDEN=1 to generate)", goldenPath, err)
	}

	// Normalise the golden too (the committed file already is,
	// but this ensures consistency if the golden was hand-edited).
	normGolden := e2eGoldenNormalize(golden, moduleRoot)

	if !bytes.Equal(normActual, normGolden) {
		return fmt.Errorf("%s normalised golden mismatch for %s:\n%s",
			artifact, scenario,
			e2eGoldenDiff(string(normGolden), string(normActual), 30))
	}
	return nil
}

func (s *e2eGoldenState) findingsListsExactlyFourFilesWithDistinctLanguages() error {
	if len(s.findingsJSON) == 0 {
		return fmt.Errorf("findings.json is empty or was not produced\nstdout:\n%s\nstderr:\n%s",
			s.stdout, s.stderr)
	}
	var doc struct {
		Files []struct {
			Path     string `json:"path"`
			Language string `json:"language"`
		} `json:"Files"`
	}
	if err := json.Unmarshal(s.findingsJSON, &doc); err != nil {
		return fmt.Errorf("unmarshal findings.json: %w", err)
	}
	if len(doc.Files) != 4 {
		return fmt.Errorf("expected exactly 4 Files entries, got %d: %v",
			len(doc.Files), doc.Files)
	}
	langs := make(map[string]bool)
	for _, f := range doc.Files {
		if f.Language == "" {
			return fmt.Errorf("file %q has empty language", f.Path)
		}
		langs[f.Language] = true
	}
	if len(langs) != 4 {
		return fmt.Errorf("expected 4 distinct languages, got %d: %v", len(langs), langs)
	}
	return nil
}

func (s *e2eGoldenState) promptsJSONLLineCountEqualsExpected() error {
	metaStr, err := s.readScenarioMeta("expected_task_count")
	if err != nil {
		return err
	}
	expected, err := strconv.Atoi(metaStr)
	if err != nil {
		return fmt.Errorf("parse expected_task_count %q: %w", metaStr, err)
	}
	lines := e2eGoldenNonEmptyLines(s.promptsJSONL)
	if len(lines) != expected {
		return fmt.Errorf("expected %d prompt lines, got %d", expected, len(lines))
	}
	return nil
}

func (s *e2eGoldenState) everyPromptsJSONLLineIsValidJSONWithFormatVersion(version string) error {
	lines := e2eGoldenNonEmptyLines(s.promptsJSONL)
	if len(lines) == 0 {
		return fmt.Errorf("prompts.jsonl has no non-empty lines")
	}
	for i, line := range lines {
		var rec map[string]interface{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return fmt.Errorf("line %d is not valid JSON: %w\nline: %s", i+1, err, line)
		}
		v, ok := rec["prompt_format_version"]
		if !ok {
			return fmt.Errorf("line %d missing prompt_format_version", i+1)
		}
		if fmt.Sprint(v) != version {
			return fmt.Errorf("line %d prompt_format_version = %q, want %q", i+1, v, version)
		}
	}
	return nil
}

func e2eGoldenNonEmptyLines(data []byte) []string {
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Scenario initializer and test entry point
// ---------------------------------------------------------------------------

func InitializeScenario_hardening_and_release_end_to_end_golden_tests(ctx *godog.ScenarioContext) {
	s := &e2eGoldenState{}

	ctx.Step(`^the scenario directory "([^"]*)"$`, s.theScenarioDirectory)
	ctx.Step(`^the cleanc dev binary is built$`, s.theCleancDevBinaryIsBuilt)
	ctx.Step(`^run\.sh executes for the scenario$`, s.runShExecutesForScenario)
	ctx.Step(`^exit code matches the scenario expected_exit_code$`, s.exitCodeMatchesScenarioExpected)
	ctx.Step(`^the observed exit code equals (\d+)$`, s.theObservedExitCodeEquals)
	ctx.Step(`^the artifact "([^"]*)" byte-matches the golden file for "([^"]*)"$`, s.artifactByteMatchesGolden)
	ctx.Step(`^findings\.json lists exactly four Files entries with distinct language values$`, s.findingsListsExactlyFourFilesWithDistinctLanguages)
	ctx.Step(`^prompts\.jsonl line count equals the scenario expected_task_count$`, s.promptsJSONLLineCountEqualsExpected)
	ctx.Step(`^every prompts\.jsonl line is valid JSON with prompt_format_version "([^"]*)"$`, s.everyPromptsJSONLLineIsValidJSONWithFormatVersion)
}

func TestE2E_hardening_and_release_end_to_end_golden_tests(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_hardening_and_release_end_to_end_golden_tests,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"hardening_and_release_end_to_end_golden_tests.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("godog suite failed")
	}
}
