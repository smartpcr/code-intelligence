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

// solidRecipesPackageExists checks whether internal/metrics/recipes has at
// least one .go file under svcRoot. Used by TestE2E_* to t.Skip the
// entire suite when the impl branch has not yet landed.
func solidRecipesPackageExists(svcRoot string) bool {
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

// runProbe compiles and executes a small Go program within the service
// module, returning its combined stdout/stderr and exit code.
func runProbe(svcRoot, source string) (string, int, error) {
	tmpDir, err := os.MkdirTemp(svcRoot, "e2e-solid-recipe-probe-")
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

// ---------- Scenario: solid-recipes-only-canonical-kinds ----------

type solidRegistryCanonicalState struct {
	svcRoot     string
	metricKinds []string
}

func (s *solidRegistryCanonicalState) theRegistryAfterInit() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *solidRegistryCanonicalState) listingTheRegisteredMetricKindsForPack(pack string) error {
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
	reg.InitSolidPack()

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

func (s *solidRegistryCanonicalState) theMetricKindsAreExactly(expected string) error {
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
	forbidden := []string{"cohesion", "afferent_coupling", "efferent_coupling", "dit", "noc"}
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

// ---------- Scenario: lcom4-class-known-value ----------

type lcom4KnownValueState struct {
	svcRoot string
	value   int
}

func (s *lcom4KnownValueState) aJavaClassFixtureWithTwoDisjointMethodClustersShareingNoFields() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *lcom4KnownValueState) theLcom4RecipeRuns() error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// Java class with two disjoint method clusters:
	// Cluster 1: methodA() uses fieldA
	// Cluster 2: methodB() uses fieldB
	// No field is shared → LCOM4 = 2
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/metrics/recipes"
)

type result struct {
	Value int `+"`"+`json:"value"`+"`"+`
}

const fixture = `+"`"+`public class DisjointService {
    private int fieldA;
    private int fieldB;

    public int methodA() {
        return fieldA + 1;
    }

    public int methodB() {
        return fieldB + 1;
    }
}
`+"`"+`

func main() {
	reg := recipes.NewRegistry()
	reg.InitSolidPack()

	drafts, err := reg.RunRecipe("lcom4", "DisjointService.java", []byte(fixture))
	if err != nil {
		fmt.Fprintf(os.Stderr, "lcom4 recipe error: %%v\n", err)
		os.Exit(1)
	}

	if len(drafts) == 0 {
		fmt.Fprintf(os.Stderr, "lcom4 recipe returned no drafts\n")
		os.Exit(1)
	}

	// Find the draft at scope_kind "class".
	var target *recipes.MetricSampleDraft
	for i := range drafts {
		if drafts[i].ScopeKind == "class" {
			target = &drafts[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "no draft with scope_kind=class found; got: ")
		for _, d := range drafts {
			fmt.Fprintf(os.Stderr, "{kind=%%s scope=%%s val=%%d} ", d.MetricKind, d.ScopeKind, d.Value)
		}
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	if err := json.NewEncoder(os.Stdout).Encode(result{Value: target.Value}); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	output, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running lcom4 probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("lcom4 probe exited %d:\n%s", exitCode, output)
	}

	var res struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &res); err != nil {
		return fmt.Errorf("parsing probe output: %w\nraw: %s", err, output)
	}

	s.value = res.Value
	return nil
}

// ---------- Scenario: cbo-counts-distinct-targets ----------

type cboDistinctTargetsState struct {
	svcRoot string
	value   int
}

func (s *cboDistinctTargetsState) aClassReferencingFourDistinctExternalClasses() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *cboDistinctTargetsState) theCouplingBetweenObjectsRecipeRuns() error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// Java class that references exactly four distinct external classes:
	// Logger, DatabaseConnection, HttpClient, JsonParser
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/metrics/recipes"
)

type result struct {
	Value int `+"`"+`json:"value"`+"`"+`
}

const fixture = `+"`"+`import java.util.logging.Logger;

public class OrderService {
    private Logger logger;
    private DatabaseConnection db;
    private HttpClient client;
    private JsonParser parser;

    public OrderService(Logger logger, DatabaseConnection db, HttpClient client, JsonParser parser) {
        this.logger = logger;
        this.db = db;
        this.client = client;
        this.parser = parser;
    }

    public void process() {
        logger.info("processing");
        db.query("SELECT 1");
        String raw = client.get("/api/orders");
        parser.parse(raw);
    }
}
`+"`"+`

func main() {
	reg := recipes.NewRegistry()
	reg.InitSolidPack()

	drafts, err := reg.RunRecipe("coupling_between_objects", "OrderService.java", []byte(fixture))
	if err != nil {
		fmt.Fprintf(os.Stderr, "cbo recipe error: %%v\n", err)
		os.Exit(1)
	}

	if len(drafts) == 0 {
		fmt.Fprintf(os.Stderr, "cbo recipe returned no drafts\n")
		os.Exit(1)
	}

	// Find the draft at scope_kind "class".
	var target *recipes.MetricSampleDraft
	for i := range drafts {
		if drafts[i].ScopeKind == "class" {
			target = &drafts[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "no draft with scope_kind=class found; got: ")
		for _, d := range drafts {
			fmt.Fprintf(os.Stderr, "{kind=%%s scope=%%s val=%%d} ", d.MetricKind, d.ScopeKind, d.Value)
		}
		fmt.Fprintln(os.Stderr)
		os.Exit(1)
	}

	if err := json.NewEncoder(os.Stdout).Encode(result{Value: target.Value}); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	output, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running cbo probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("cbo probe exited %d:\n%s", exitCode, output)
	}

	var res struct {
		Value int `json:"value"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &res); err != nil {
		return fmt.Errorf("parsing probe output: %w\nraw: %s", err, output)
	}

	s.value = res.Value
	return nil
}

// ---------- Shared step: it emits value N ----------

type solidValueAssertionState struct {
	lcom4State *lcom4KnownValueState
	cboState   *cboDistinctTargetsState
	lastValue  int
}

func (s *solidValueAssertionState) setFromLCOM4() {
	s.lastValue = s.lcom4State.value
}

func (s *solidValueAssertionState) setFromCBO() {
	s.lastValue = s.cboState.value
}

func (s *solidValueAssertionState) itEmitsValue(wantValue int) error {
	if s.lastValue != wantValue {
		return fmt.Errorf("value: want %d, got %d", wantValue, s.lastValue)
	}
	return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_ast_adapter_and_foundation_tier_compute_solid_pack_foundation_recipes(ctx *godog.ScenarioContext) {
	reg := &solidRegistryCanonicalState{}
	lcom4 := &lcom4KnownValueState{}
	cbo := &cboDistinctTargetsState{}
	valAssert := &solidValueAssertionState{lcom4State: lcom4, cboState: cbo}

	// solid-recipes-only-canonical-kinds
	ctx.Step(`^the registry after init$`, reg.theRegistryAfterInit)
	ctx.Step(`^listing the registered metric_kinds for pack "([^"]*)"$`, reg.listingTheRegisteredMetricKindsForPack)
	ctx.Step(`^the metric_kinds are exactly "([^"]*)"$`, reg.theMetricKindsAreExactly)

	// lcom4-class-known-value
	ctx.Step(`^a Java class fixture with two disjoint method clusters sharing no fields$`, lcom4.aJavaClassFixtureWithTwoDisjointMethodClustersShareingNoFields)
	ctx.Step(`^the lcom4 recipe runs$`, func() error {
		err := lcom4.theLcom4RecipeRuns()
		if err == nil {
			valAssert.setFromLCOM4()
		}
		return err
	})

	// cbo-counts-distinct-targets
	ctx.Step(`^a class referencing four distinct external classes$`, cbo.aClassReferencingFourDistinctExternalClasses)
	ctx.Step(`^the coupling_between_objects recipe runs$`, func() error {
		err := cbo.theCouplingBetweenObjectsRecipeRuns()
		if err == nil {
			valAssert.setFromCBO()
		}
		return err
	})

	// Shared assertion
	ctx.Step(`^it emits value (\d+)$`, valAssert.itEmitsValue)
}

func TestE2E_ast_adapter_and_foundation_tier_compute_solid_pack_foundation_recipes(t *testing.T) {
	svcRoot := serviceRoot()
	if !solidRecipesPackageExists(svcRoot) {
		t.Skip("internal/metrics/recipes package not found; skipping until impl branch lands")
	}

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_ast_adapter_and_foundation_tier_compute_solid_pack_foundation_recipes,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"ast_adapter_and_foundation_tier_compute_solid_pack_foundation_recipes.feature"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}