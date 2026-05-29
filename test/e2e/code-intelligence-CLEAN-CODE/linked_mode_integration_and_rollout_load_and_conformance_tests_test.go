//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
)

// ---------------------------------------------------------------------------
// canonical-names conformance helpers
// ---------------------------------------------------------------------------

// canonicalMetricKinds is the authoritative set of metric_kind values.
// Any reference that does not appear in this set is treated as a
// non-canonical alias.
var canonicalMetricKinds = map[string]bool{
	"coupling":              true,
	"cohesion":              true,
	"complexity":            true,
	"duplication":           true,
	"code_size":             true,
	"dependency_count":      true,
	"instability":           true,
	"abstractness":          true,
	"distance_from_main":    true,
	"afferent_coupling":     true,
	"efferent_coupling":     true,
	"cyclomatic_complexity": true,
	"cognitive_complexity":  true,
	"halstead_volume":       true,
	"maintainability_index": true,
	"depth_of_inheritance":  true,
	"class_coupling":        true,
	"lines_of_code":         true,
	"comment_ratio":         true,
	"test_coverage":         true,
}

// metricKindRefPattern matches references like metric_kind:"value" or
// metric_kind: "value" or MetricKind = "value" in source files.
var metricKindRefPattern = regexp.MustCompile(
	`(?i)(?:metric_kind|metrickind)\s*[:=]\s*"([^"]+)"`,
)

type loadConformanceState struct {
	// conformance scenario
	repoRoot   string
	references []metricKindRef
	violations []metricKindRef

	// load scenario
	k6BinaryPath string
	evaluatorURL string
	p50ms        float64
	p95ms        float64
	p99ms        float64
}

type metricKindRef struct {
	File  string
	Line  int
	Value string
}

func newLoadConformanceState() *loadConformanceState {
	return &loadConformanceState{}
}

// ---------------------------------------------------------------------------
// Scenario: canonical-names-conformance
// ---------------------------------------------------------------------------

func (s *loadConformanceState) theConformanceTestRunningAcrossTheWholeRepo(ctx context.Context) error {
	root := os.Getenv("REPO_ROOT")
	if root == "" {
		// Default to the module root (three levels up from test/e2e/...).
		wd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("getting working directory: %w", err)
		}
		root = filepath.Join(wd, "..", "..", "..")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("REPO_ROOT %q is not a valid directory", root)
	}
	s.repoRoot = root
	return nil
}

func (s *loadConformanceState) itInventoriesMetricKindReferences(ctx context.Context) error {
	err := filepath.WalkDir(s.repoRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip inaccessible paths
		}
		if d.IsDir() {
			base := d.Name()
			if base == ".git" || base == "vendor" || base == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".go" && ext != ".yaml" && ext != ".yml" && ext != ".json" && ext != ".toml" {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil // skip unreadable files
		}
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			matches := metricKindRefPattern.FindAllStringSubmatch(line, -1)
			for _, m := range matches {
				s.references = append(s.references, metricKindRef{
					File:  path,
					Line:  i + 1,
					Value: m[1],
				})
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("walking repo: %w", err)
	}
	return nil
}

func (s *loadConformanceState) noReferenceUsesANonCanonicalAlias(ctx context.Context) error {
	for _, ref := range s.references {
		if !canonicalMetricKinds[ref.Value] {
			s.violations = append(s.violations, ref)
		}
	}
	if len(s.violations) > 0 {
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("found %d non-canonical metric_kind reference(s):\n", len(s.violations)))
		for _, v := range s.violations {
			sb.WriteString(fmt.Sprintf("  %s:%d  metric_kind=%q\n", v.File, v.Line, v.Value))
		}
		return fmt.Errorf("%s", sb.String())
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario: load-target-met
// ---------------------------------------------------------------------------

// k6SummaryMetric is a subset of the k6 JSON summary output.
type k6SummaryMetric struct {
	Metrics map[string]struct {
		Values map[string]float64 `json:"values"`
	} `json:"metrics"`
}

func (s *loadConformanceState) hundredReposAtFiftyScansPerMinForThirtyMinutes(ctx context.Context) error {
	s.k6BinaryPath = os.Getenv("K6_BINARY")
	if s.k6BinaryPath == "" {
		s.k6BinaryPath = "k6" // assume on PATH
	}
	s.evaluatorURL = os.Getenv("EVALUATOR_URL")
	if s.evaluatorURL == "" {
		s.evaluatorURL = "http://localhost:8090"
	}

	// Verify k6 is available.
	if _, err := exec.LookPath(s.k6BinaryPath); err != nil {
		return fmt.Errorf("k6 binary not found at %q: %w", s.k6BinaryPath, err)
	}

	// Verify the evaluator is reachable.
	client := &http.Client{Timeout: 10 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.evaluatorURL+"/healthz", nil)
	if err != nil {
		return fmt.Errorf("building health request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("evaluator health check failed: %w", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("evaluator health check returned HTTP %d", resp.StatusCode)
	}
	return nil
}

func (s *loadConformanceState) k6ReportsTheP99EvalGateLatency(ctx context.Context) error {
	// Build inline k6 script that simulates 100 repos x 50 scans/min.
	k6Script := fmt.Sprintf(`
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Trend } from 'k6/metrics';

const evalGate = new Trend('eval_gate', true);

export const options = {
  scenarios: {
    load_test: {
      executor: 'constant-arrival-rate',
      rate: 50,
      timeUnit: '1m',
      duration: '30m',
      preAllocatedVUs: 200,
      maxVUs: 500,
    },
  },
};

const repos = [];
for (let i = 0; i < 100; i++) {
  repos.push('repo-' + i);
}

export default function () {
  const repo = repos[Math.floor(Math.random() * repos.length)];
  const payload = JSON.stringify({
    repo_id: repo,
    action: 'eval_gate',
  });
  const params = { headers: { 'Content-Type': 'application/json' } };
  const start = Date.now();
  const res = http.post('%s/api/v1/evaluator/gate', payload, params);
  const elapsed = Date.now() - start;
  evalGate.add(elapsed);
  check(res, { 'status is 2xx': (r) => r.status >= 200 && r.status < 300 });
  sleep(0.1);
}
`, s.evaluatorURL)

	// Write the script to a temp file.
	tmpDir, err := os.MkdirTemp("", "k6-load-test-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	scriptPath := filepath.Join(tmpDir, "load.js")
	if err := os.WriteFile(scriptPath, []byte(k6Script), 0644); err != nil {
		return fmt.Errorf("writing k6 script: %w", err)
	}

	summaryPath := filepath.Join(tmpDir, "summary.json")

	// Run k6.
	cmd := exec.CommandContext(ctx, s.k6BinaryPath, "run",
		"--summary-export", summaryPath,
		scriptPath,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("k6 run failed: %w", err)
	}

	// Parse the summary JSON.
	data, err := os.ReadFile(summaryPath)
	if err != nil {
		return fmt.Errorf("reading k6 summary: %w", err)
	}
	var summary k6SummaryMetric
	if err := json.Unmarshal(data, &summary); err != nil {
		return fmt.Errorf("parsing k6 summary: %w", err)
	}

	metric, ok := summary.Metrics["eval_gate"]
	if !ok {
		return fmt.Errorf("eval_gate metric not found in k6 summary")
	}
	s.p50ms = metric.Values["p(50)"]
	s.p95ms = metric.Values["p(95)"]
	s.p99ms = metric.Values["p(99)"]
	return nil
}

func (s *loadConformanceState) itIsBelowTheTechSpecSLOTargets(ctx context.Context) error {
	var errors []string

	if s.p99ms > 2000 {
		errors = append(errors, fmt.Sprintf("p99 %.1fms exceeds SLO target of 2000ms", s.p99ms))
	}
	if s.p95ms > 800 {
		errors = append(errors, fmt.Sprintf("p95 %.1fms exceeds SLO target of 800ms", s.p95ms))
	}
	if s.p50ms > 200 {
		errors = append(errors, fmt.Sprintf("p50 %.1fms exceeds SLO target of 200ms", s.p50ms))
	}

	if len(errors) > 0 {
		return fmt.Errorf("SLO violations:\n  %s\n\nActual: p50=%.1fms  p95=%.1fms  p99=%.1fms",
			strings.Join(errors, "\n  "), s.p50ms, s.p95ms, s.p99ms)
	}
	return nil
}

// ---------------------------------------------------------------------------
// godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_linked_mode_integration_and_rollout_load_and_conformance_tests(ctx *godog.ScenarioContext) {
	s := newLoadConformanceState()

	// canonical-names-conformance
	ctx.Step(`^the conformance test running across the whole repo$`,
		s.theConformanceTestRunningAcrossTheWholeRepo)
	ctx.Step(`^it inventories metric_kind references$`,
		s.itInventoriesMetricKindReferences)
	ctx.Step(`^no reference uses a non-canonical alias$`,
		s.noReferenceUsesANonCanonicalAlias)

	// load-target-met
	ctx.Step(`^100 repos at 50 scans/min for 30 minutes$`,
		s.hundredReposAtFiftyScansPerMinForThirtyMinutes)
	ctx.Step(`^k6 reports the p99 eval\.gate latency$`,
		s.k6ReportsTheP99EvalGateLatency)
	ctx.Step(`^it is below the tech-spec SLO targets of p99 2s and p95 800ms and p50 200ms$`,
		s.itIsBelowTheTechSpecSLOTargets)
}

func TestE2E_linked_mode_integration_and_rollout_load_and_conformance_tests(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_linked_mode_integration_and_rollout_load_and_conformance_tests,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"linked_mode_integration_and_rollout_load_and_conformance_tests.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run e2e tests")
	}
}