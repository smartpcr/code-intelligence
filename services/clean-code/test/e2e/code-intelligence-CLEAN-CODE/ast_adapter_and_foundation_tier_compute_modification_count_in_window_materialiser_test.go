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
// directory by walking up from this source file's location. It returns
// an error if runtime.Caller cannot recover the source path (e.g. the
// binary was stripped of debug info) or if filepath.Abs fails, so that
// callers surface a clear root-cause error instead of operating on an
// empty/wrong path.
func serviceRoot() (string, error) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("serviceRoot: runtime.Caller(0) failed to recover source file path")
	}
	dir := filepath.Dir(thisFile)
	root := filepath.Join(dir, "..", "..", "..")
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("serviceRoot: resolving absolute path for %q: %w", root, err)
	}
	return abs, nil
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

// materProbeExists checks whether internal/metrics/materialisers has at
// least one .go file under svcRoot.
func materProbeExists(svcRoot string) bool {
	pkg := filepath.Join(svcRoot, "internal", "metrics", "materialisers")
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

// probeMu serializes runProbe calls so that probe temp dirs never
// coexist inside the module tree across goroutines.
var probeMu sync.Mutex

// runProbe compiles and executes a small Go program within the service
// module, returning its combined stdout/stderr and exit code.
func runProbe(svcRoot, source string) (string, int, error) {
	probeMu.Lock()
	defer probeMu.Unlock()

	tmpDir, err := os.MkdirTemp(svcRoot, "e2e-materialiser-probe-")
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

// ---------------------------------------------------------------------------
// probe result types
// ---------------------------------------------------------------------------

// materProbeResult is the JSON envelope the materialiser probe emits.
type materProbeResult struct {
	Samples []materSampleResult `json:"samples"`
}

type materSampleResult struct {
	MetricKind string `json:"metric_kind"`
	Pack       string `json:"pack"`
	Source     string `json:"source"`
	Scope      string `json:"scope"`
	Value      int    `json:"value"`
	Provenance string `json:"provenance"`
}

// ---------------------------------------------------------------------------
// scenario state
// ---------------------------------------------------------------------------

type modCountMaterialiserState struct {
	svcRoot    string
	scope      string
	withinDays bool // true = within window, false = outside window
	result     materProbeResult
	probeRan   bool
}

func (s *modCountMaterialiserState) churnRowsForScopeDatedWithinTheLast90Days(scope string) error {
	root, err := serviceRoot()
	if err != nil {
		return fmt.Errorf("resolving service root: %w", err)
	}
	s.svcRoot = root
	s.scope = scope
	s.withinDays = true
	return nil
}

func (s *modCountMaterialiserState) churnRowsForScopeDatedOlderThan90Days(scope string) error {
	root, err := serviceRoot()
	if err != nil {
		return fmt.Errorf("resolving service root: %w", err)
	}
	s.svcRoot = root
	s.scope = scope
	s.withinDays = false
	return nil
}

func (s *modCountMaterialiserState) theMaterialiserRuns() error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// The probe creates synthetic churn rows and feeds them through
	// the materialiser, then encodes the resulting MetricSample
	// slice as JSON on stdout.
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"%s/internal/metrics/materialisers"
)

type sampleResult struct {
	MetricKind string `+"`"+`json:"metric_kind"`+"`"+`
	Pack       string `+"`"+`json:"pack"`+"`"+`
	Source     string `+"`"+`json:"source"`+"`"+`
	Scope      string `+"`"+`json:"scope"`+"`"+`
	Value      int    `+"`"+`json:"value"`+"`"+`
	Provenance string `+"`"+`json:"provenance"`+"`"+`
}

type result struct {
	Samples []sampleResult `+"`"+`json:"samples"`+"`"+`
}

func main() {
	scope := %q
	withinWindow := %t

	var modifiedAt time.Time
	if withinWindow {
		// 10 days ago — well within the 90-day window.
		modifiedAt = time.Now().Add(-10 * 24 * time.Hour)
	} else {
		// 120 days ago — outside the 90-day window.
		modifiedAt = time.Now().Add(-120 * 24 * time.Hour)
	}

	rows := []materialisers.ChurnRow{
		{
			Scope:      scope,
			ModifiedAt: modifiedAt,
		},
	}

	m := materialisers.NewModificationCountInWindow()
	samples := m.Materialise(rows)

	out := result{}
	for _, s := range samples {
		provenance := ""
		if s.AttrsJSON != nil {
			if v, ok := s.AttrsJSON["provenance"]; ok {
				provenance = fmt.Sprintf("%%v", v)
			}
		}
		out.Samples = append(out.Samples, sampleResult{
			MetricKind: s.MetricKind,
			Pack:       s.Pack,
			Source:     s.Source,
			Scope:      s.Scope,
			Value:      s.Value,
			Provenance: provenance,
		})
	}

	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath, s.scope, s.withinDays)

	output, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running materialiser probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("materialiser probe exited %d:\n%s", exitCode, output)
	}

	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &s.result); err != nil {
		return fmt.Errorf("parsing probe output: %w\nraw: %s", err, output)
	}
	s.probeRan = true
	return nil
}

func (s *modCountMaterialiserState) itEmitsAMetricSampleWithMetricKindAndPackAndSource(
	wantKind, wantPack, wantSource string,
) error {
	for _, sample := range s.result.Samples {
		if sample.MetricKind == wantKind && sample.Pack == wantPack && sample.Source == wantSource {
			return nil
		}
	}
	return fmt.Errorf(
		"no MetricSample with metric_kind=%q pack=%q source=%q found; got %d samples: %+v",
		wantKind, wantPack, wantSource, len(s.result.Samples), s.result.Samples,
	)
}

func (s *modCountMaterialiserState) theSampleRecordsAttrsJSONProvenance(wantProv string) error {
	for _, sample := range s.result.Samples {
		if sample.MetricKind == "modification_count_in_window" && sample.Provenance == wantProv {
			return nil
		}
	}
	return fmt.Errorf(
		"no MetricSample with metric_kind=modification_count_in_window has attrs_json.provenance=%q; got: %+v",
		wantProv, s.result.Samples,
	)
}

func (s *modCountMaterialiserState) noMetricSampleRowIsWrittenForScope(scope string) error {
	for _, sample := range s.result.Samples {
		if sample.Scope == scope {
			return fmt.Errorf(
				"expected no metric_sample for scope %q but found: %+v",
				scope, sample,
			)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

func InitializeScenario_ast_adapter_and_foundation_tier_compute_modification_count_in_window_materialiser(ctx *godog.ScenarioContext) {
	state := &modCountMaterialiserState{}

	ctx.Step(
		`^churn rows for scope "([^"]*)" dated within the last 90 days$`,
		state.churnRowsForScopeDatedWithinTheLast90Days,
	)
	ctx.Step(
		`^churn rows for scope "([^"]*)" dated older than 90 days$`,
		state.churnRowsForScopeDatedOlderThan90Days,
	)
	ctx.Step(
		`^the materialiser runs$`,
		state.theMaterialiserRuns,
	)
	ctx.Step(
		`^it emits a MetricSample with metric_kind "([^"]*)" and pack "([^"]*)" and source "([^"]*)"$`,
		state.itEmitsAMetricSampleWithMetricKindAndPackAndSource,
	)
	ctx.Step(
		`^the sample records attrs_json provenance "([^"]*)"$`,
		state.theSampleRecordsAttrsJSONProvenance,
	)
	ctx.Step(
		`^no metric_sample row is written for scope "([^"]*)"$`,
		state.noMetricSampleRowIsWrittenForScope,
	)
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_ast_adapter_and_foundation_tier_compute_modification_count_in_window_materialiser(t *testing.T) {
	svcRoot, err := serviceRoot()
	if err != nil {
		t.Fatalf("resolving service root: %v", err)
	}
	if !materProbeExists(svcRoot) {
		t.Skip("internal/metrics/materialisers package not found; skipping until impl branch lands")
	}

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_ast_adapter_and_foundation_tier_compute_modification_count_in_window_materialiser,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"ast_adapter_and_foundation_tier_compute_modification_count_in_window_materialiser.feature"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
