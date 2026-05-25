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

// serviceRoot returns the absolute path to the services/clean-code
// directory by walking up from this source file's location.
func serviceRoot() string {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	root := filepath.Join(dir, "..", "..", "..")
	abs, _ := filepath.Abs(root)
	return abs
}

// readModulePath extracts the module path from go.mod in dir.
func readModulePath(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(line[len("module "):]), nil
		}
	}
	return "", fmt.Errorf("module directive not found in go.mod")
}

// recipesPackageExists checks whether internal/metrics/recipes has at
// least one .go file under svcRoot.
func recipesPackageExists(svcRoot string) bool {
	pkg := filepath.Join(svcRoot, "internal", "metrics", "recipes")
	info, err := os.Stat(pkg)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, _ := os.ReadDir(pkg)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}

// runProbe compiles and executes a small Go program within the service
// module, returning its combined stdout/stderr and exit code.
func runProbe(svcRoot, source string) (string, int, error) {
	tmpDir, err := os.MkdirTemp(svcRoot, "e2e-cycle-dup-probe-")
	if err != nil {
		return "", -1, fmt.Errorf("creating probe dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(source), 0644); err != nil {
		return "", -1, fmt.Errorf("writing probe: %w", err)
	}

	relDir, err := filepath.Rel(svcRoot, tmpDir)
	if err != nil {
		return "", -1, fmt.Errorf("relative path: %w", err)
	}

	cmd := exec.Command("go", "run", "./"+filepath.ToSlash(relDir))
	cmd.Dir = svcRoot
	cmd.Env = os.Environ()

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	exitCode := 0
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return buf.String(), exitCode, nil
}

// ---------- Scenario: cycle-member-flags-participants ----------

// cycleMemberResult holds the probe output for one file in the cycle analysis.
type cycleMemberResult struct {
	File      string `json:"file"`
	Value     int    `json:"value"`
	ScopeKind string `json:"scope_kind"`
}

// cycleMemberProbeOutput is the full output from the cycle_member probe.
type cycleMemberProbeOutput struct {
	Results       []cycleMemberResult `json:"results"`
	AllScopeKinds []string            `json:"all_scope_kinds"`
}

type cycleMemberState struct {
	svcRoot string
	output  cycleMemberProbeOutput
}

func (s *cycleMemberState) threeFilesFormingAnImportCycleAndAFileDOutsideTheCycle() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *cycleMemberState) theCycleMemberRecipeRuns() error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// The probe creates four Go source fixtures:
	//   A imports B, B imports C, C imports A  → cycle participants
	//   D imports nothing                      → not in cycle
	// It runs the cycle_member recipe and collects all results.
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/metrics/recipes"
)

type fileResult struct {
	File      string `+"`"+`json:"file"`+"`"+`
	Value     int    `+"`"+`json:"value"`+"`"+`
	ScopeKind string `+"`"+`json:"scope_kind"`+"`"+`
}

type output struct {
	Results       []fileResult `+"`"+`json:"results"`+"`"+`
	AllScopeKinds []string     `+"`"+`json:"all_scope_kinds"`+"`"+`
}

func main() {
	// Four fixture files: A->B->C->A (cycle), D (isolated)
	files := map[string]string{
		"a.go": "package proj\nimport _ \"proj/b\"\n",
		"b.go": "package proj\nimport _ \"proj/c\"\n",
		"c.go": "package proj\nimport _ \"proj/a\"\n",
		"d.go": "package proj\n",
	}

	reg := recipes.NewRegistry()
	reg.InitBasePack()

	fileBytes := make(map[string][]byte, len(files))
	for name, src := range files {
		fileBytes[name] = []byte(src)
	}

	drafts, err := reg.RunCycleMemberRecipe(fileBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cycle_member recipe error: %%v\n", err)
		os.Exit(1)
	}

	allScopes := make([]string, len(drafts))
	for i := range drafts {
		allScopes[i] = drafts[i].ScopeKind
	}

	var results []fileResult
	for _, d := range drafts {
		results = append(results, fileResult{
			File:      d.ScopeID,
			Value:     d.Value,
			ScopeKind: d.ScopeKind,
		})
	}

	if err := json.NewEncoder(os.Stdout).Encode(output{
		Results:       results,
		AllScopeKinds: allScopes,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	out, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running cycle_member probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("cycle_member probe exited %d:\n%s", exitCode, out)
	}

	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &s.output); err != nil {
		return fmt.Errorf("parsing probe output: %w\nraw: %s", err, out)
	}
	return nil
}

func (s *cycleMemberState) filesABAndCEachEmitValueAtScopeKind(wantValue int, wantScope string) error {
	cycleFiles := map[string]bool{"a.go": false, "b.go": false, "c.go": false}
	for _, r := range s.output.Results {
		if _, ok := cycleFiles[r.File]; ok {
			if r.Value != wantValue {
				return fmt.Errorf("file %s: want value %d, got %d", r.File, wantValue, r.Value)
			}
			if r.ScopeKind != wantScope {
				return fmt.Errorf("file %s: want scope_kind %q, got %q", r.File, wantScope, r.ScopeKind)
			}
			cycleFiles[r.File] = true
		}
	}
	for f, found := range cycleFiles {
		if !found {
			return fmt.Errorf("cycle participant %q not found in results", f)
		}
	}
	return nil
}

func (s *cycleMemberState) fileDEmitsValueAtScopeKind(wantValue int, wantScope string) error {
	for _, r := range s.output.Results {
		if r.File == "d.go" {
			if r.Value != wantValue {
				return fmt.Errorf("file d.go: want value %d, got %d", wantValue, r.Value)
			}
			if r.ScopeKind != wantScope {
				return fmt.Errorf("file d.go: want scope_kind %q, got %q", wantScope, r.ScopeKind)
			}
			return nil
		}
	}
	return fmt.Errorf("file d.go not found in results")
}

func (s *cycleMemberState) theCycleMemberRecipeNEVEREmitsAtScopeKind(forbidden string) error {
	for _, sk := range s.output.AllScopeKinds {
		if sk == forbidden {
			return fmt.Errorf("forbidden non-canonical scope_kind %q was emitted (all scopes: %v)", forbidden, s.output.AllScopeKinds)
		}
	}
	return nil
}

// ---------- Scenario: duplication-ratio-bounded-zero-to-one ----------

type dupRatioResult struct {
	Value     float64 `json:"value"`
	ScopeKind string  `json:"scope_kind"`
}

type dupRatioProbeOutput struct {
	Results       []dupRatioResult `json:"results"`
	AllScopeKinds []string         `json:"all_scope_kinds"`
}

type dupRatioState struct {
	svcRoot string
	output  dupRatioProbeOutput
}

func (s *dupRatioState) aSourceCorpusWithKnownDuplicatedBlocks() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *dupRatioState) theDuplicationRatioRecipeRunsAtScopeKind(scopeKind string) error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// The probe creates a file with deliberately duplicated blocks,
	// then runs the duplication_ratio recipe. The exact ratio depends
	// on the impl, but it must be in [0,1] and only at canonical scopes.
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/metrics/recipes"
)

type result struct {
	Value     float64 `+"`"+`json:"value"`+"`"+`
	ScopeKind string  `+"`"+`json:"scope_kind"`+"`"+`
}

type output struct {
	Results       []result `+"`"+`json:"results"`+"`"+`
	AllScopeKinds []string `+"`"+`json:"all_scope_kinds"`+"`"+`
}

func main() {
	// Create a fixture with duplicated blocks: the same 5-line block
	// repeated twice to guarantee a non-zero duplication ratio.
	duplicatedBlock := "func helper(x int) int {\n\ty := x * 2\n\tz := y + 1\n\treturn z\n}\n"
	fixture := "package dup\n\n" + duplicatedBlock + "\n" +
		"// duplicate below\n" +
		"func helper2(x int) int {\n\ty := x * 2\n\tz := y + 1\n\treturn z\n}\n"

	reg := recipes.NewRegistry()
	reg.InitBasePack()

	drafts, err := reg.RunRecipe("duplication_ratio", "fixture.go", []byte(fixture))
	if err != nil {
		fmt.Fprintf(os.Stderr, "duplication_ratio recipe error: %%v\n", err)
		os.Exit(1)
	}

	allScopes := make([]string, len(drafts))
	for i := range drafts {
		allScopes[i] = drafts[i].ScopeKind
	}

	var results []result
	for _, d := range drafts {
		results = append(results, result{
			Value:     d.FloatValue,
			ScopeKind: d.ScopeKind,
		})
	}

	if err := json.NewEncoder(os.Stdout).Encode(output{
		Results:       results,
		AllScopeKinds: allScopes,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	out, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running duplication_ratio probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("duplication_ratio probe exited %d:\n%s", exitCode, out)
	}

	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &s.output); err != nil {
		return fmt.Errorf("parsing probe output: %w\nraw: %s", err, out)
	}
	return nil
}

func (s *dupRatioState) theEmittedValueIsBetweenAndInclusive(lo, hi float64) error {
	if len(s.output.Results) == 0 {
		return fmt.Errorf("no results emitted by duplication_ratio recipe")
	}
	for _, r := range s.output.Results {
		if r.Value < lo || r.Value > hi {
			return fmt.Errorf("duplication_ratio value %.4f is outside [%.1f, %.1f]", r.Value, lo, hi)
		}
	}
	return nil
}

func (s *dupRatioState) theRowScopeKindIsIn(allowed string) error {
	allowedSet := make(map[string]bool)
	for _, sk := range strings.Split(allowed, ",") {
		allowedSet[strings.TrimSpace(sk)] = true
	}
	for _, r := range s.output.Results {
		if !allowedSet[r.ScopeKind] {
			return fmt.Errorf("scope_kind %q is not in allowed set %v", r.ScopeKind, allowedSet)
		}
	}
	return nil
}

func (s *dupRatioState) theDuplicationRatioRecipeNEVEREmitsAtScopeKindOr(forbidden1, forbidden2 string) error {
	for _, sk := range s.output.AllScopeKinds {
		if sk == forbidden1 || sk == forbidden2 {
			return fmt.Errorf("forbidden non-canonical scope_kind %q was emitted (all scopes: %v)", sk, s.output.AllScopeKinds)
		}
	}
	return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_ast_adapter_and_foundation_tier_compute_cycle_and_duplication_recipes(ctx *godog.ScenarioContext) {
	cyc := &cycleMemberState{}
	dup := &dupRatioState{}

	// cycle-member-flags-participants
	ctx.Step(`^three files A->B->C->A forming an import cycle and a file D outside the cycle$`, cyc.threeFilesFormingAnImportCycleAndAFileDOutsideTheCycle)
	ctx.Step(`^the cycle_member recipe runs$`, cyc.theCycleMemberRecipeRuns)
	ctx.Step(`^files A, B, and C each emit value (\d+) at scope_kind "([^"]*)"$`, cyc.filesABAndCEachEmitValueAtScopeKind)
	ctx.Step(`^file D emits value (\d+) at scope_kind "([^"]*)"$`, cyc.fileDEmitsValueAtScopeKind)
	ctx.Step(`^the cycle_member recipe NEVER emits at scope_kind "([^"]*)"$`, cyc.theCycleMemberRecipeNEVEREmitsAtScopeKind)

	// duplication-ratio-bounded-zero-to-one
	ctx.Step(`^a source corpus with known duplicated blocks$`, dup.aSourceCorpusWithKnownDuplicatedBlocks)
	ctx.Step(`^the duplication_ratio recipe runs at scope_kind "([^"]*)"$`, dup.theDuplicationRatioRecipeRunsAtScopeKind)
	ctx.Step(`^the emitted value is between (\d+) and (\d+) inclusive$`, dup.theEmittedValueIsBetweenAndInclusive)
	ctx.Step(`^the row scope_kind is in "([^"]*)"$`, dup.theRowScopeKindIsIn)
	ctx.Step(`^the duplication_ratio recipe NEVER emits at scope_kind "([^"]*)" or "([^"]*)"$`, dup.theDuplicationRatioRecipeNEVEREmitsAtScopeKindOr)
}

func TestE2E_ast_adapter_and_foundation_tier_compute_cycle_and_duplication_recipes(t *testing.T) {
	svcRoot := serviceRoot()
	if !recipesPackageExists(svcRoot) {
		t.Skip("internal/metrics/recipes package not found; skipping until impl branch lands")
	}

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_ast_adapter_and_foundation_tier_compute_cycle_and_duplication_recipes,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"ast_adapter_and_foundation_tier_compute_cycle_and_duplication_recipes.feature"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}