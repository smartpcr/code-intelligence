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
	"sort"
	"strings"
	"sync"
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

// recipesPackageExists checks whether internal/metrics/recipes has at
// least one .go file under svcRoot. Used by TestE2E_* to t.Skip the
// entire suite when the impl branch has not yet landed.
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

// probeMu serializes runProbe calls so that probe temp dirs never
// coexist inside the module tree across goroutines, even if a future
// caller adds t.Parallel() or godog Concurrency > 1.
//
// The temp dir MUST live inside svcRoot (see runProbe doc) so creating
// it "outside the module" is not a viable alternative — serialization
// is therefore the correct guard.
var probeMu sync.Mutex

// runProbe compiles and executes a small Go program within the service
// module, returning its combined stdout/stderr and exit code.
//
// The temp dir lives inside svcRoot on purpose: the probe source
// imports module-internal packages such as
// "<modpath>/internal/metrics/recipes", which `go run` can only
// resolve when the source file is inside the parent module. Placing
// the temp dir under os.TempDir() would put the file outside the
// module and break those imports.
//
// To prevent multiple in-tree probe directories from coexisting under
// concurrent callers, runProbe acquires probeMu for its entire
// lifetime: each invocation creates its tmpDir, runs the probe to
// completion, removes the tmpDir, then releases the lock.
func runProbe(svcRoot, source string) (string, int, error) {
	probeMu.Lock()
	defer probeMu.Unlock()

	tmpDir, err := os.MkdirTemp(svcRoot, "e2e-base-recipe-probe-")
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

// ---------- Scenario: base-recipes-only-canonical-kinds ----------

type registryCanonicalState struct {
	svcRoot     string
	metricKinds []string
}

func (s *registryCanonicalState) theRecipeRegistryAfterInit() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *registryCanonicalState) listingTheRegisteredMetricKindsForPack(pack string) error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/metrics/recipes"
)

type result struct {
	Kinds []string `+"`"+`json:"kinds"`+"`"+`
}

func main() {
	reg := recipes.NewRegistry()
	reg.InitBasePack()

	kinds := reg.MetricKinds(%q)

	if err := json.NewEncoder(os.Stdout).Encode(result{Kinds: kinds}); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath, pack)

	output, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running registry probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("registry probe exited %d:\n%s", exitCode, output)
	}

	var res struct {
		Kinds []string `json:"kinds"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &res); err != nil {
		return fmt.Errorf("parsing probe output: %w\nraw: %s", err, output)
	}

	s.metricKinds = res.Kinds
	return nil
}

func (s *registryCanonicalState) theResultIsExactly(expected string) error {
	want := strings.Split(expected, ",")
	sort.Strings(want)

	got := make([]string, len(s.metricKinds))
	copy(got, s.metricKinds)
	sort.Strings(got)

	if len(got) != len(want) {
		return fmt.Errorf("metric_kinds count mismatch: want %v, got %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			return fmt.Errorf("metric_kinds mismatch at index %d: want %q, got %q (full: want=%v got=%v)", i, want[i], got[i], want, got)
		}
	}

	// Guard: reject non-canonical aliases that must NOT appear.
	forbidden := []string{"cyclomatic_complexity", "lines_of_code", "function_length", "parameter_count", "nesting_depth"}
	gotSet := make(map[string]bool, len(got))
	for _, k := range got {
		gotSet[k] = true
	}
	for _, f := range forbidden {
		if gotSet[f] {
			return fmt.Errorf("non-canonical metric_kind %q is present but must NOT be registered", f)
		}
	}
	return nil
}

// ---------- Scenario: cyclo-known-value ----------

// cycloProbeResult holds the full probe output: the target draft plus
// all scope_kinds emitted, so we can reject forbidden non-canonical scopes.
type cycloProbeResult struct {
	MetricKind    string   `json:"metric_kind"`
	Value         int      `json:"value"`
	ScopeKind     string   `json:"scope_kind"`
	AllScopeKinds []string `json:"all_scope_kinds"`
}

type cycloKnownValueState struct {
	svcRoot string
	result  cycloProbeResult
}

func (s *cycloKnownValueState) aGoFixtureMethodWithTwoIfBranchesAndOneForLoop() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *cycloKnownValueState) theCycloRecipeRuns() error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// The probe creates a Go source fixture with a struct method that has
	// two if branches and one for loop (cyclomatic complexity = 1+2+1 = 4).
	// It collects ALL emitted drafts so the test can reject forbidden
	// non-canonical scope_kinds like "function".
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/metrics/recipes"
)

type result struct {
	MetricKind    string   `+"`"+`json:"metric_kind"`+"`"+`
	Value         int      `+"`"+`json:"value"`+"`"+`
	ScopeKind     string   `+"`"+`json:"scope_kind"`+"`"+`
	AllScopeKinds []string `+"`"+`json:"all_scope_kinds"`+"`"+`
}

// fixture is a Go method (receiver on a struct) with two if branches
// and one for loop — cyclomatic complexity 4.
const fixture = `+"`"+`package fixture

type Calculator struct{}

func (c *Calculator) Compute(a, b int) int {
	sum := 0
	if a > 0 {
		sum += a
	}
	if b > 0 {
		sum += b
	}
	for i := 0; i < sum; i++ {
		sum--
	}
	return sum
}
`+"`"+`

func main() {
	reg := recipes.NewRegistry()
	reg.InitBasePack()

	drafts, err := reg.RunRecipe("cyclo", "fixture.go", []byte(fixture))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cyclo recipe error: %%v\n", err)
		os.Exit(1)
	}

	if len(drafts) == 0 {
		fmt.Fprintf(os.Stderr, "cyclo recipe returned no drafts\n")
		os.Exit(1)
	}

	// Collect all scope_kinds for the forbidden-scope guard.
	allScopes := make([]string, len(drafts))
	for i := range drafts {
		allScopes[i] = drafts[i].ScopeKind
	}

	// Find the draft for the Compute method at scope_kind "method".
	var target *recipes.MetricSampleDraft
	for i := range drafts {
		if drafts[i].ScopeKind == "method" {
			target = &drafts[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "no draft with scope_kind=method found; got: ")
		for _, d := range drafts {
			fmt.Fprintf(os.Stderr, "{kind=%%s scope=%%s val=%%d} ", d.MetricKind, d.ScopeKind, d.Value)
		}
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	// Reject forbidden non-canonical scope_kind "function" inside the probe.
	for _, sk := range allScopes {
		if sk == "function" {
			fmt.Fprintf(os.Stderr, "FORBIDDEN: non-canonical scope_kind 'function' emitted (all scopes: %%v)\n", allScopes)
			os.Exit(2)
		}
	}

	if err := json.NewEncoder(os.Stdout).Encode(result{
		MetricKind:    target.MetricKind,
		Value:         target.Value,
		ScopeKind:     target.ScopeKind,
		AllScopeKinds: allScopes,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	output, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running cyclo probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("cyclo probe exited %d:\n%s", exitCode, output)
	}

	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &s.result); err != nil {
		return fmt.Errorf("parsing probe output: %w\nraw: %s", err, output)
	}
	return nil
}

func (s *cycloKnownValueState) itEmitsAMetricSampleDraftWithMetricKindAndValueAtScopeKind(wantKind string, wantValue int, wantScope string) error {
	if s.result.MetricKind != wantKind {
		return fmt.Errorf("metric_kind: want %q, got %q", wantKind, s.result.MetricKind)
	}
	if s.result.Value != wantValue {
		return fmt.Errorf("value: want %d, got %d", wantValue, s.result.Value)
	}
	if s.result.ScopeKind != wantScope {
		return fmt.Errorf("scope_kind: want %q, got %q (non-canonical 'function' is forbidden)", wantScope, s.result.ScopeKind)
	}

	// Reject forbidden non-canonical scope_kind "function" anywhere in drafts.
	for _, sk := range s.result.AllScopeKinds {
		if sk == "function" {
			return fmt.Errorf("forbidden non-canonical scope_kind %q was emitted alongside canonical drafts (all scopes: %v)", sk, s.result.AllScopeKinds)
		}
	}
	return nil
}

// ---------- Scenario: loc-counts-physical-lines ----------

// locProbeResult holds the loc draft plus all scope_kinds so we can
// reject the forbidden non-canonical "module" scope.
type locProbeResult struct {
	Value         int      `json:"value"`
	ScopeKind     string   `json:"scope_kind"`
	AllScopeKinds []string `json:"all_scope_kinds"`
}

type locPhysicalLinesState struct {
	svcRoot string
	result  locProbeResult
}

func (s *locPhysicalLinesState) aFortyTwoLinePythonSourceFileFixture() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *locPhysicalLinesState) theLocRecipeRunsAtScopeKind(scopeKind string) error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// Build a 42-line Python fixture. Each line is a comment.
	var lines []string
	for i := 1; i <= 42; i++ {
		lines = append(lines, fmt.Sprintf("# line %d", i))
	}
	fixture := strings.Join(lines, "\n") + "\n"

	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/metrics/recipes"
)

type result struct {
	Value         int      `+"`"+`json:"value"`+"`"+`
	ScopeKind     string   `+"`"+`json:"scope_kind"`+"`"+`
	AllScopeKinds []string `+"`"+`json:"all_scope_kinds"`+"`"+`
}

func main() {
	fixture := %q

	reg := recipes.NewRegistry()
	reg.InitBasePack()

	drafts, err := reg.RunRecipe("loc", "fixture.py", []byte(fixture))
	if err != nil {
		fmt.Fprintf(os.Stderr, "loc recipe error: %%v\n", err)
		os.Exit(1)
	}

	// Collect all scope_kinds for the forbidden-scope guard.
	allScopes := make([]string, len(drafts))
	for i := range drafts {
		allScopes[i] = drafts[i].ScopeKind
	}

	// Find the draft at the requested canonical scope_kind.
	var target *recipes.MetricSampleDraft
	for i := range drafts {
		if drafts[i].ScopeKind == %q {
			target = &drafts[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "no draft with scope_kind=%%s found; got: ", %q)
		for _, d := range drafts {
			fmt.Fprintf(os.Stderr, "{kind=%%s scope=%%s val=%%d} ", d.MetricKind, d.ScopeKind, d.Value)
		}
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	// Reject forbidden non-canonical scope_kind "module" inside the probe.
	for _, sk := range allScopes {
		if sk == "module" {
			fmt.Fprintf(os.Stderr, "FORBIDDEN: non-canonical scope_kind 'module' emitted (all scopes: %%v)\n", allScopes)
			os.Exit(2)
		}
	}

	if err := json.NewEncoder(os.Stdout).Encode(result{
		Value:         target.Value,
		ScopeKind:     target.ScopeKind,
		AllScopeKinds: allScopes,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath, fixture, scopeKind, scopeKind)

	output, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running loc probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("loc probe exited %d:\n%s", exitCode, output)
	}

	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &s.result); err != nil {
		return fmt.Errorf("parsing probe output: %w\nraw: %s", err, output)
	}
	return nil
}

func (s *locPhysicalLinesState) itEmitsValue(wantValue int) error {
	if s.result.Value != wantValue {
		return fmt.Errorf("loc value: want %d, got %d", wantValue, s.result.Value)
	}

	// Reject forbidden non-canonical scope_kind "module" anywhere in drafts.
	for _, sk := range s.result.AllScopeKinds {
		if sk == "module" {
			return fmt.Errorf("forbidden non-canonical scope_kind %q was emitted alongside canonical drafts (all scopes: %v)", sk, s.result.AllScopeKinds)
		}
	}
	return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_ast_adapter_and_foundation_tier_compute_base_pack_foundation_recipes(ctx *godog.ScenarioContext) {
	reg := &registryCanonicalState{}
	cyc := &cycloKnownValueState{}
	loc := &locPhysicalLinesState{}

	// base-recipes-only-canonical-kinds
	ctx.Step(`^the recipe registry after init$`, reg.theRecipeRegistryAfterInit)
	ctx.Step(`^listing the registered metric_kinds for pack "([^"]*)"$`, reg.listingTheRegisteredMetricKindsForPack)
	ctx.Step(`^the result is exactly "([^"]*)"$`, reg.theResultIsExactly)

	// cyclo-known-value
	ctx.Step(`^a Go fixture method with two if branches and one for loop$`, cyc.aGoFixtureMethodWithTwoIfBranchesAndOneForLoop)
	ctx.Step(`^the cyclo recipe runs$`, cyc.theCycloRecipeRuns)
	ctx.Step(`^it emits a MetricSampleDraft with metric_kind "([^"]*)" and value (\d+) at scope_kind "([^"]*)"$`, cyc.itEmitsAMetricSampleDraftWithMetricKindAndValueAtScopeKind)

	// loc-counts-physical-lines
	ctx.Step(`^a 42-line Python source file fixture$`, loc.aFortyTwoLinePythonSourceFileFixture)
	ctx.Step(`^the loc recipe runs at scope_kind "([^"]*)"$`, loc.theLocRecipeRunsAtScopeKind)
	ctx.Step(`^it emits value (\d+)$`, loc.itEmitsValue)
}

func TestE2E_ast_adapter_and_foundation_tier_compute_base_pack_foundation_recipes(t *testing.T) {
	svcRoot := serviceRoot()
	if !recipesPackageExists(svcRoot) {
		t.Skip("internal/metrics/recipes package not found; skipping until impl branch lands")
	}

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_ast_adapter_and_foundation_tier_compute_base_pack_foundation_recipes,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"ast_adapter_and_foundation_tier_compute_base_pack_foundation_recipes.feature"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
