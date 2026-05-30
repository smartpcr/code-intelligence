//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
)

// ---------------------------------------------------------------------------
// Self-contained helpers — prefixed with "pls" (per-language-smoke) so they
// coexist with identically-shaped helpers the sibling cgo-build-proof stage
// may define in the same e2e package. Each is fully standalone: this file
// compiles without any other _test.go in the package.
// ---------------------------------------------------------------------------

// plsModuleRoot returns the services/agent-memory directory (3 levels up).
func plsModuleRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	// test/e2e/code-intelligence-REPO-SCANNER -> services/agent-memory
	for i := 0; i < 3; i++ {
		dir = filepath.Dir(dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return "", fmt.Errorf("go.mod not found at %s: %w", dir, err)
	}
	return dir, nil
}

// plsHasCCompiler returns true if gcc or clang is on PATH.
func plsHasCCompiler() bool {
	for _, cc := range []string{"gcc", "clang"} {
		if _, err := exec.LookPath(cc); err == nil {
			return true
		}
	}
	return false
}

// plsSplitNonEmpty splits a whitespace-separated string, dropping blanks.
func plsSplitNonEmpty(s string) []string {
	var out []string
	for _, f := range strings.Fields(s) {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type plsSmokeState struct {
	modRoot string

	// Scenario 1: polyglot smoke test output (CGO=1, normal PATH)
	smokeOutput   string
	smokeExitCode int

	// Scenario 2: nocgo degradation test output + go list results
	nocgoDegradationOutput string
	nocgoDegradationExit   int
	goListGoFiles          []string

	// Scenario 3: polyglot smoke without pwsh on PATH
	noPwshOutput   string
	noPwshExitCode int
}

// plsNocgoTempTestSource is a Go test file that mirrors the polyglot smoke
// test structure but compiles under CGO_ENABLED=0. Under CGO=0 only
// PowerShell is registered (via parsers_nocgo.go); for the five tree-sitter
// languages SelectParser returns nil and the test calls t.Skip keyed on
// ErrParserUnavailable. For PowerShell, the test runs the FULL
// EmitFile path through the dispatcher and asserts the non-empty
// Node + Edge set (≥1 class, ≥1 method, ≥1 static_calls).
const plsNocgoTempTestSource = `package ast

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

func TestNocgoPolyglotDegradation(t *testing.T) {
	cases := []struct {
		language    string
		relPath     string
		requirePwsh bool
	}{
		{"c", "src/hello.c", false},
		{"cpp", "src/hello.cpp", false},
		{"csharp", "src/hello.cs", false},
		{"go_lang", "src/hello.go", false},
		{"rust", "src/hello.rs", false},
		{"powershell", "scripts/hello.ps1", true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.language, func(t *testing.T) {
			p := SelectParser(tc.relPath, nil)
			if p == nil {
				t.Skipf("ErrParserUnavailable: no parser registered for %s under current build tags", tc.relPath)
			}

			if tc.requirePwsh {
				if _, err := exec.LookPath("pwsh"); err != nil {
					t.Skipf("pwsh not on PATH: %v", err)
				}
			}

			ext := strings.ToLower(filepath.Ext(tc.relPath))
			fixturePath := filepath.Join("testdata", "polyglot", "hello"+ext)
			src, err := os.ReadFile(fixturePath)
			if err != nil {
				t.Fatalf("read fixture %s: %v", fixturePath, err)
			}

			fw := &ndFakeWriter{idBySig: map[string]string{}}
			d := NewDispatcher(fw, WithParsers(DefaultParsers()...))
			ev := repoindexer.EmitFileEvent{
				RepoID:     fingerprint.MustParseRepoID("11111111-2222-3333-4444-555555555555"),
				RepoURL:    "https://git.example/acme/nocgo",
				SHA:        "shaNOCGO",
				FileNodeID: "file-node-id",
				RelPath:    tc.relPath,
				Open: func() (repoindexer.ReadCloser, error) {
					return &ndStringRC{r: strings.NewReader(string(src))}, nil
				},
			}
			if _, emitErr := d.EmitFile(context.Background(), ev); emitErr != nil {
				t.Fatalf("EmitFile(%s): %v", tc.relPath, emitErr)
			}

			classes := fw.nodesOf("class")
			if len(classes) < 1 {
				t.Errorf("class/type Nodes = %d; want >=1", len(classes))
			}
			methods := fw.nodesOf("method")
			if len(methods) < 1 {
				t.Errorf("method Nodes = %d; want >=1", len(methods))
			}
			calls := fw.edgesOf("static_calls")
			if len(calls) < 1 {
				t.Errorf("static_calls Edges = %d; want >=1", len(calls))
			}
			if !t.Failed() {
				t.Logf("VERIFIED non-empty Node+Edge set: classes=%d methods=%d static_calls=%d", len(classes), len(methods), len(calls))
			}
		})
	}
}

type ndFakeWriter struct {
	mu      sync.Mutex
	nodes   []graphwriter.NodeInput
	edges   []graphwriter.EdgeInput
	idBySig map[string]string
}

func (f *ndFakeWriter) InsertNode(_ context.Context, in graphwriter.NodeInput) (graphwriter.NodeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id, dup := f.idBySig[in.CanonicalSignature]; dup {
		fp, _ := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
		return graphwriter.NodeRecord{NodeID: id, Fingerprint: fp, Inserted: false}, nil
	}
	id := "nd-" + strconv.Itoa(len(f.nodes))
	f.idBySig[in.CanonicalSignature] = id
	f.nodes = append(f.nodes, in)
	fp, _ := fingerprint.NodeFingerprint(in.RepoID, in.Kind, in.CanonicalSignature, in.FromSHA)
	return graphwriter.NodeRecord{NodeID: id, Fingerprint: fp, Inserted: true}, nil
}

func (f *ndFakeWriter) InsertEdge(_ context.Context, in graphwriter.EdgeInput) (graphwriter.EdgeRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edges = append(f.edges, in)
	return graphwriter.EdgeRecord{EdgeID: "nd-e-" + strconv.Itoa(len(f.edges) - 1), Inserted: true}, nil
}

func (f *ndFakeWriter) nodesOf(kind string) []graphwriter.NodeInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []graphwriter.NodeInput
	for _, n := range f.nodes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func (f *ndFakeWriter) edgesOf(kind string) []graphwriter.EdgeInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []graphwriter.EdgeInput
	for _, e := range f.edges {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

type ndStringRC struct {
	r *strings.Reader
}

func (s *ndStringRC) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *ndStringRC) Close() error               { return nil }
`

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *plsSmokeState) cgoWithCompiler(cgoVal string) error {
	if !plsHasCCompiler() {
		return fmt.Errorf("neither gcc nor clang found on PATH; CGO build requires a C compiler")
	}
	root, err := plsModuleRoot()
	if err != nil {
		return err
	}
	s.modRoot = root
	return nil
}

func (s *plsSmokeState) cgoOnly(cgoVal string) error {
	root, err := plsModuleRoot()
	if err != nil {
		return err
	}
	s.modRoot = root
	return nil
}

func (s *plsSmokeState) cgoWithCompilerPwshAbsent(cgoVal string) error {
	if !plsHasCCompiler() {
		return fmt.Errorf("neither gcc nor clang found on PATH; CGO build requires a C compiler")
	}
	root, err := plsModuleRoot()
	if err != nil {
		return err
	}
	s.modRoot = root
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *plsSmokeState) polyglotSmokeRuns(testName, cgoVal string) error {
	cmd := exec.Command("go", "test", "-v", "-run", testName, "-count=1",
		"./internal/repoindexer/ast/...")
	cmd.Dir = s.modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgoVal)
	out, err := cmd.CombinedOutput()
	s.smokeOutput = string(out)
	if err != nil {
		s.smokeExitCode = 1
	}
	return nil
}

func (s *plsSmokeState) nocgoDegradationTestRuns(cgoVal string) error {
	astDir := filepath.Join(s.modRoot, "internal", "repoindexer", "ast")
	tempFile := filepath.Join(astDir, "nocgo_polyglot_degradation_e2e_temp_test.go")

	// Write the temporary test file into the ast package.
	if err := os.WriteFile(tempFile, []byte(plsNocgoTempTestSource), 0644); err != nil {
		return fmt.Errorf("failed to write temp test file: %w", err)
	}
	defer os.Remove(tempFile)

	// Run the temp test under CGO=0.
	cmd := exec.Command("go", "test", "-v",
		"-run", "^TestNocgoPolyglotDegradation$",
		"-count=1", "./internal/repoindexer/ast/...")
	cmd.Dir = s.modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgoVal)
	out, err := cmd.CombinedOutput()
	s.nocgoDegradationOutput = string(out)
	if err != nil {
		s.nocgoDegradationExit = 1
	}

	// Also populate go list results for build-tag file-set queries.
	listCmd := exec.Command("go", "list", "-f",
		`{{range .GoFiles}}{{.}} {{end}}`,
		"./internal/repoindexer/ast/")
	listCmd.Dir = s.modRoot
	listCmd.Env = append(os.Environ(), "CGO_ENABLED="+cgoVal)
	listOut, listErr := listCmd.CombinedOutput()
	if listErr == nil {
		s.goListGoFiles = plsSplitNonEmpty(strings.TrimSpace(string(listOut)))
	}
	return nil
}

func (s *plsSmokeState) polyglotSmokeRunsWithoutPwsh(testName, cgoVal string) error {
	cmd := exec.Command("go", "test", "-v", "-run", testName, "-count=1",
		"./internal/repoindexer/ast/...")
	cmd.Dir = s.modRoot
	cmd.Env = plsBuildEnvWithoutPwsh(cgoVal)
	out, err := cmd.CombinedOutput()
	s.noPwshOutput = string(out)
	if err != nil {
		s.noPwshExitCode = 1
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — scenario 1 (polyglot-smoke-cgo)
// ---------------------------------------------------------------------------

func (s *plsSmokeState) everySubtestPasses() error {
	if s.smokeExitCode != 0 {
		return fmt.Errorf("TestPolyglotParseSmoke failed (exit %d):\n%s",
			s.smokeExitCode, s.smokeOutput)
	}
	if strings.Contains(s.smokeOutput, "--- FAIL:") {
		return fmt.Errorf("output contains FAIL lines:\n%s", s.smokeOutput)
	}
	if strings.Contains(s.smokeOutput, "--- SKIP:") {
		return fmt.Errorf("output contains SKIP lines (expected all subtests to pass):\n%s", s.smokeOutput)
	}
	return nil
}

func (s *plsSmokeState) smokeShowsPassForLang(lang string) error {
	marker := "--- PASS: TestPolyglotParseSmoke/" + lang
	if !strings.Contains(s.smokeOutput, marker) {
		skipMarker := "--- SKIP: TestPolyglotParseSmoke/" + lang
		if strings.Contains(s.smokeOutput, skipMarker) {
			return fmt.Errorf("language %q was SKIPPED (expected PASS)", lang)
		}
		return fmt.Errorf("no PASS for %q;\noutput:\n%s", lang, s.smokeOutput)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — scenario 2 (nocgo-degraded)
// ---------------------------------------------------------------------------

func (s *plsSmokeState) onlyFixturePassesInNocgo(lang string) error {
	marker := "--- PASS: TestNocgoPolyglotDegradation/" + lang
	if !strings.Contains(s.nocgoDegradationOutput, marker) {
		return fmt.Errorf("%s subtest did not PASS in nocgo degradation test;\noutput:\n%s",
			lang, s.nocgoDegradationOutput)
	}

	// Verify no OTHER language also passed — the step says "only" this one passes.
	const passPrefix = "--- PASS: TestNocgoPolyglotDegradation/"
	var unexpected []string
	for _, line := range strings.Split(s.nocgoDegradationOutput, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, passPrefix) {
			continue
		}
		// Extract the subtest name after the prefix.
		idx := strings.Index(trimmed, passPrefix)
		rest := trimmed[idx+len(passPrefix):]
		// rest is e.g. "powershell (0.12s)" — grab the first token.
		subtestName := strings.Fields(rest)[0]
		if subtestName != lang {
			unexpected = append(unexpected, subtestName)
		}
	}
	if len(unexpected) > 0 {
		return fmt.Errorf("expected only %s to PASS under CGO=0, but these also passed: %v;\noutput:\n%s",
			lang, unexpected, s.nocgoDegradationOutput)
	}
	return nil
}

func (s *plsSmokeState) fixtureSkippedViaErrParserUnavailable(lang string) error {
	skipMarker := "--- SKIP: TestNocgoPolyglotDegradation/" + lang
	if !strings.Contains(s.nocgoDegradationOutput, skipMarker) {
		passMarker := "--- PASS: TestNocgoPolyglotDegradation/" + lang
		if strings.Contains(s.nocgoDegradationOutput, passMarker) {
			return fmt.Errorf("%s subtest PASSED under CGO=0 (expected SKIP via ErrParserUnavailable);\noutput:\n%s",
				lang, s.nocgoDegradationOutput)
		}
		return fmt.Errorf("no SKIP for %s subtest in nocgo degradation output;\noutput:\n%s",
			lang, s.nocgoDegradationOutput)
	}
	// Verify the skip message references ErrParserUnavailable
	if !strings.Contains(s.nocgoDegradationOutput, "ErrParserUnavailable") {
		return fmt.Errorf("%s subtest was skipped but skip message does not reference ErrParserUnavailable;\noutput:\n%s",
			lang, s.nocgoDegradationOutput)
	}
	return nil
}

func (s *plsSmokeState) nocgoConfirmsNonEmptyNodeEdgeSet() error {
	// The temp test logs "VERIFIED non-empty Node+Edge set: classes=N methods=N static_calls=N"
	// for each subtest that passes. Check the output for this marker.
	if !strings.Contains(s.nocgoDegradationOutput, "VERIFIED non-empty Node+Edge set:") {
		return fmt.Errorf(
			"nocgo degradation output does not contain VERIFIED marker proving "+
				"PowerShell fixture emits non-empty Node+Edge set;\noutput:\n%s",
			s.nocgoDegradationOutput)
	}
	// Also verify the logged counts are non-zero
	if strings.Contains(s.nocgoDegradationOutput, "classes=0") ||
		strings.Contains(s.nocgoDegradationOutput, "methods=0") ||
		strings.Contains(s.nocgoDegradationOutput, "static_calls=0") {
		return fmt.Errorf(
			"nocgo degradation output shows zero-count for a Node/Edge kind "+
				"(PowerShell fixture should emit >=1 of each);\noutput:\n%s",
			s.nocgoDegradationOutput)
	}
	return nil
}

func (s *plsSmokeState) fileCompiledUnder(fileName, cgoVal string) error {
	for _, f := range s.goListGoFiles {
		if f == fileName {
			return nil
		}
	}
	return fmt.Errorf("%s NOT compiled under CGO_ENABLED=%s; go files: %v",
		fileName, cgoVal, s.goListGoFiles)
}

func (s *plsSmokeState) fileNotCompiledUnder(fileName, cgoVal string) error {
	for _, f := range s.goListGoFiles {
		if f == fileName {
			return fmt.Errorf(
				"%s IS compiled under CGO_ENABLED=%s (should be excluded); go files: %v",
				fileName, cgoVal, s.goListGoFiles)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Then steps — scenario 3 (pwsh-missing-skip)
// ---------------------------------------------------------------------------

func (s *plsSmokeState) powershellSubtestIsSkipped() error {
	skipMarker := "--- SKIP: TestPolyglotParseSmoke/powershell"
	if strings.Contains(s.noPwshOutput, skipMarker) {
		return nil
	}
	passMarker := "--- PASS: TestPolyglotParseSmoke/powershell"
	if strings.Contains(s.noPwshOutput, passMarker) {
		return fmt.Errorf(
			"powershell subtest PASSED (expected SKIP; PATH surgery may not have removed pwsh);\n"+
				"output:\n%s", s.noPwshOutput)
	}
	return fmt.Errorf("no SKIP for powershell subtest;\noutput:\n%s", s.noPwshOutput)
}

func (s *plsSmokeState) allNonPowershellSubtestsPass() error {
	langs := []string{"typescript", "python", "c", "cpp", "csharp", "go", "rust"}
	for _, lang := range langs {
		marker := "--- PASS: TestPolyglotParseSmoke/" + lang
		if !strings.Contains(s.noPwshOutput, marker) {
			return fmt.Errorf("language %q did not PASS in pwsh-absent run;\noutput:\n%s",
				lang, s.noPwshOutput)
		}
	}
	return nil
}

func (s *plsSmokeState) errParserUnavailableSentinelConfirms() error {
	// Run TestPowerShellParser_NoPwsh_ReturnsSentinel (no build tag needed)
	// to verify the parser returns ErrParserUnavailable with reason=pwsh_not_available.
	cmd := exec.Command("go", "test", "-v",
		"-run", "TestPowerShellParser_NoPwsh_ReturnsSentinel",
		"-count=1", "./internal/repoindexer/ast/...")
	cmd.Dir = s.modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		return fmt.Errorf("sentinel test failed:\n%s", output)
	}
	marker := "--- PASS: TestPowerShellParser_NoPwsh_ReturnsSentinel"
	if !strings.Contains(output, marker) {
		return fmt.Errorf("no PASS for TestPowerShellParser_NoPwsh_ReturnsSentinel;\noutput:\n%s",
			output)
	}
	return nil
}

func (s *plsSmokeState) dispatcherSkipTestPassesUnder(cgoVal string) error {
	// TestDispatcher_ErrParserUnavailable_LogsSkip lives behind
	// //go:build canonical_dispatcher, so we pass that tag explicitly.
	// This test verifies the dispatcher emits ast.dispatch.skip with
	// reason=pwsh_not_available when the parser returns ErrParserUnavailable.
	cmd := exec.Command("go", "test", "-v",
		"-tags", "canonical_dispatcher",
		"-run", "TestDispatcher_ErrParserUnavailable_LogsSkip",
		"-count=1", "./internal/repoindexer/ast/...")
	cmd.Dir = s.modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED="+cgoVal)
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		return fmt.Errorf("dispatcher skip test failed (CGO_ENABLED=%s):\n%s",
			cgoVal, output)
	}
	marker := "--- PASS: TestDispatcher_ErrParserUnavailable_LogsSkip"
	if !strings.Contains(output, marker) {
		return fmt.Errorf("no PASS for TestDispatcher_ErrParserUnavailable_LogsSkip;\noutput:\n%s",
			output)
	}
	return nil
}

// ---------------------------------------------------------------------------
// PATH surgery helper — removes directories containing pwsh/pwsh.exe from
// PATH and pins CC/CXX to absolute paths so CGO compilation survives.
// ---------------------------------------------------------------------------

func plsBuildEnvWithoutPwsh(cgoVal string) []string {
	// Pre-resolve C compiler paths before filtering PATH so CGO can
	// still find gcc/clang even if they share a directory with pwsh.
	gccPath, _ := exec.LookPath("gcc")
	clangPath, _ := exec.LookPath("clang")
	gxxPath, _ := exec.LookPath("g++")
	clangxxPath, _ := exec.LookPath("clang++")

	env := os.Environ()
	var result []string
	ccSet, cxxSet := false, false

	for _, e := range env {
		key := strings.SplitN(e, "=", 2)[0]
		upper := strings.ToUpper(key)
		switch {
		case upper == "PATH":
			parts := strings.SplitN(e, "=", 2)
			dirs := filepath.SplitList(parts[1])
			var filtered []string
			for _, d := range dirs {
				pwshName := "pwsh"
				if runtime.GOOS == "windows" {
					pwshName = "pwsh.exe"
				}
				if _, statErr := os.Stat(filepath.Join(d, pwshName)); statErr == nil {
					continue // skip directory containing pwsh
				}
				filtered = append(filtered, d)
			}
			result = append(result, key+"="+strings.Join(filtered, string(os.PathListSeparator)))
		case upper == "CC":
			result = append(result, e)
			ccSet = true
		case upper == "CXX":
			result = append(result, e)
			cxxSet = true
		default:
			result = append(result, e)
		}
	}

	// Pin CC/CXX so CGO finds the compiler even though PATH dirs were removed.
	if !ccSet {
		if gccPath != "" {
			result = append(result, "CC="+gccPath)
		} else if clangPath != "" {
			result = append(result, "CC="+clangPath)
		}
	}
	if !cxxSet {
		if gxxPath != "" {
			result = append(result, "CXX="+gxxPath)
		} else if clangxxPath != "" {
			result = append(result, "CXX="+clangxxPath)
		}
	}

	result = append(result, "CGO_ENABLED="+cgoVal)
	return result
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_parser_coverage_verification_per_language_parse_smoke_fixtures(ctx *godog.ScenarioContext) {
	s := &plsSmokeState{}

	// Given
	ctx.Given(`^CGO_ENABLED is "([^"]*)" and a C compiler is on PATH$`, s.cgoWithCompiler)
	ctx.Given(`^CGO_ENABLED is "([^"]*)"$`, s.cgoOnly)
	ctx.Given(`^CGO_ENABLED is "([^"]*)" and a C compiler is on PATH and pwsh is absent from PATH$`, s.cgoWithCompilerPwshAbsent)

	// When
	ctx.When(`^the polyglot smoke test "([^"]*)" runs under CGO_ENABLED "([^"]*)"$`, s.polyglotSmokeRuns)
	ctx.When(`^the nocgo polyglot degradation test exercises the dispatcher under CGO_ENABLED "([^"]*)"$`, s.nocgoDegradationTestRuns)
	ctx.When(`^the polyglot smoke test "([^"]*)" runs without pwsh under CGO_ENABLED "([^"]*)"$`, s.polyglotSmokeRunsWithoutPwsh)

	// Then — scenario 1
	ctx.Then(`^every subtest passes without skips$`, s.everySubtestPasses)
	ctx.Then(`^the test output shows a PASS for language "([^"]*)"$`, s.smokeShowsPassForLang)

	// Then — scenario 2
	ctx.Then(`^only the "([^"]*)" fixture passes in the nocgo degradation test$`, s.onlyFixturePassesInNocgo)
	ctx.Then(`^the nocgo degradation output confirms the powershell fixture emits non-empty Node and Edge sets$`, s.nocgoConfirmsNonEmptyNodeEdgeSet)
	ctx.Then(`^the "([^"]*)" fixture is skipped via t\.Skip keyed on ErrParserUnavailable$`, s.fixtureSkippedViaErrParserUnavailable)
	ctx.Then(`^"([^"]*)" is compiled under CGO_ENABLED "([^"]*)"$`, s.fileCompiledUnder)
	ctx.Then(`^"([^"]*)" is not compiled under CGO_ENABLED "([^"]*)"$`, s.fileNotCompiledUnder)

	// Then — scenario 3
	ctx.Then(`^the powershell subtest is skipped$`, s.powershellSubtestIsSkipped)
	ctx.Then(`^all non-powershell subtests pass$`, s.allNonPowershellSubtestsPass)
	ctx.Then(`^the ErrParserUnavailable sentinel test confirms the pwsh_not_available reason$`, s.errParserUnavailableSentinelConfirms)
	ctx.Then(`^the dispatcher ErrParserUnavailable skip test passes under CGO_ENABLED "([^"]*)"$`, s.dispatcherSkipTestPassesUnder)
}

func TestE2E_parser_coverage_verification_per_language_parse_smoke_fixtures(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_parser_coverage_verification_per_language_parse_smoke_fixtures,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"parser_coverage_verification_per_language_parse_smoke_fixtures.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}