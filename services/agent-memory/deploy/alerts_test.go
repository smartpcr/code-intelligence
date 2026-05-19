package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
)

// alertFile mirrors the subset of the Prometheus rule-file
// schema this test asserts against. We intentionally use a
// permissive shape — unknown fields are ignored — so future
// Prometheus rule-file additions (`keep_firing_for`, etc.) do
// not break the validator.
type alertFile struct {
	Groups []alertGroup `yaml:"groups"`
}

type alertGroup struct {
	Name     string      `yaml:"name"`
	Interval string      `yaml:"interval"`
	Rules    []alertRule `yaml:"rules"`
}

type alertRule struct {
	Alert       string            `yaml:"alert"`
	Record      string            `yaml:"record"`
	Expr        string            `yaml:"expr"`
	For         string            `yaml:"for"`
	Labels      map[string]string `yaml:"labels"`
	Annotations map[string]string `yaml:"annotations"`
}

func loadAlertFile(t *testing.T) alertFile {
	t.Helper()
	path := filepath.Join("alerts", "agent-memory.rules.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read alert rules: %v", err)
	}
	var f alertFile
	if err := yaml.Unmarshal(body, &f); err != nil {
		t.Fatalf("alert rules YAML malformed: %v", err)
	}
	if len(f.Groups) == 0 {
		t.Fatalf("alert rules file has no groups")
	}
	return f
}

// TestAlertRulesYAMLValid parses the rules file and asserts
// every rule's `expr` references only metric families this
// service emits (folding histogram suffixes the same way the
// dashboard validator does).
func TestAlertRulesYAMLValid(t *testing.T) {
	t.Parallel()

	f := loadAlertFile(t)
	known := knownMetricFamilies()
	var problems []string
	for _, g := range f.Groups {
		if g.Name == "" {
			problems = append(problems, "group with empty name")
		}
		for _, r := range g.Rules {
			if r.Alert == "" && r.Record == "" {
				problems = append(problems,
					"rule with no alert/record name in group "+g.Name)
				continue
			}
			if strings.TrimSpace(r.Expr) == "" {
				problems = append(problems,
					"rule "+r.Alert+" has empty expr")
				continue
			}
			for _, ref := range extractMetricRefs(r.Expr) {
				if _, ok := known[ref]; !ok {
					problems = append(problems,
						"rule "+r.Alert+" references unknown metric "+ref+
							" (expr: "+strings.ReplaceAll(r.Expr, "\n", " ")+")")
				}
			}
		}
	}
	if len(problems) > 0 {
		t.Fatalf("alert validation failed:\n  - %s",
			strings.Join(problems, "\n  - "))
	}
}

// TestRecallP95BreachRuleExists asserts the Stage 8.3 acceptance
// scenario "alert rule fires on synthetic SLO breach": a rule
// named recall_p95_breach must exist, target the §8.3 SLO
// threshold of 1.5s on the agent_recall_duration_seconds_bucket
// series, declare a non-zero `for:` window, AND carry a
// traffic-gate clause so a single slow probe cannot page.
func TestRecallP95BreachRuleExists(t *testing.T) {
	t.Parallel()

	f := loadAlertFile(t)
	var rule *alertRule
	for gi := range f.Groups {
		for ri := range f.Groups[gi].Rules {
			r := &f.Groups[gi].Rules[ri]
			if r.Alert == "recall_p95_breach" {
				rule = r
				break
			}
		}
		if rule != nil {
			break
		}
	}
	if rule == nil {
		t.Fatalf("alert rule recall_p95_breach not found (Stage 8.3 acceptance scenario requires it)")
	}

	// Normalize whitespace (collapse newlines and runs of
	// spaces to a single space) so the substring checks are
	// formatting-agnostic — the rule file uses a multi-line
	// `expr:` block scalar that interleaves `\n` with the
	// operator tokens.
	expr := strings.Join(strings.Fields(strings.ToLower(rule.Expr)), " ")
	if !strings.Contains(expr, "histogram_quantile(0.95") {
		t.Errorf("recall_p95_breach must use histogram_quantile(0.95, ...); got: %s", rule.Expr)
	}
	if !strings.Contains(expr, strings.ToLower(obs.MetricAgentRecallDurationSeconds)+"_bucket") {
		t.Errorf("recall_p95_breach must reference %s_bucket; got: %s",
			obs.MetricAgentRecallDurationSeconds, rule.Expr)
	}
	if !strings.Contains(expr, "> 1.5") {
		t.Errorf("recall_p95_breach must compare against the §8.3 SLO threshold 1.5 seconds; got: %s", rule.Expr)
	}
	if !strings.Contains(expr, " and ") {
		t.Errorf("recall_p95_breach must include a traffic-gate `and` clause so quiet windows do not page; got: %s", rule.Expr)
	}
	if !strings.Contains(expr, strings.ToLower(obs.MetricAgentRecallDurationSeconds)+"_count") {
		t.Errorf("recall_p95_breach traffic gate must reference %s_count; got: %s",
			obs.MetricAgentRecallDurationSeconds, rule.Expr)
	}
	if rule.For == "" || rule.For == "0s" || rule.For == "0" {
		t.Errorf("recall_p95_breach must declare a non-zero `for:` window so transient spikes do not page; got: %q", rule.For)
	}
	if rule.Labels["severity"] == "" {
		t.Errorf("recall_p95_breach must carry a severity label; got: %v", rule.Labels)
	}
	if rule.Annotations["summary"] == "" {
		t.Errorf("recall_p95_breach must carry a summary annotation; got: %v", rule.Annotations)
	}
}

// TestPartitionProvisionLagAlerts checks that the alert
// surface for `partition_provision_lag` follows the iter-2
// dual-rule model (real threshold + missing-metric guard).
// Iter-1 relied on `absent()` alone because no production
// code emitted the metric; iter-2 added a real emitter on
// mgmt-api so the SLO is now a `> 86400` rule with the
// `absent()` rule retained as a separate scrape-health
// signal.
func TestPartitionProvisionLagAlerts(t *testing.T) {
	t.Parallel()

	f := loadAlertFile(t)
	var hasThreshold, hasAbsent bool
	for gi := range f.Groups {
		for ri := range f.Groups[gi].Rules {
			r := &f.Groups[gi].Rules[ri]
			if !strings.Contains(r.Expr, obs.MetricPartitionProvisionLag) {
				continue
			}
			lower := strings.ToLower(r.Expr)
			if strings.Contains(lower, "absent(") {
				hasAbsent = true
				continue
			}
			// Anything that compares the gauge against a
			// numeric threshold (>, <, >=, <=) counts as a
			// real SLO-driven alert.
			if strings.ContainsAny(r.Expr, "><") {
				hasThreshold = true
			}
		}
	}
	if !hasThreshold {
		t.Errorf("iter-2 introduces a real %s emitter on mgmt-api; "+
			"a `> threshold` SLO alert MUST exist now that the metric is no longer stubbed",
			obs.MetricPartitionProvisionLag)
	}
	if !hasAbsent {
		t.Errorf("an `absent(%s)` guard MUST remain to catch the case where mgmt-api is unreachable; "+
			"removing it silently masks a scrape outage",
			obs.MetricPartitionProvisionLag)
	}
}

// TestRerankerSidecarAlertFilterMatchesEmitter (iter-4
// evaluator finding #1) makes the alert/emitter contract a
// compile-time-style assertion. The alert
// `reranker_sidecar_error_burst` filters
// `reranker_sidecar_inference_total{status="error"}`; the
// Python sidecar's middleware in
// `cmd/reranker-sidecar/observability.py` MUST emit that
// exact label value or the alert is wired but cannot fire.
//
// Iter-3 had the alert filter `status="error"` while the
// Python side emitted `status="500"` / `status="200"` --
// the alert was load-bearing for the SLO but had a
// silently-broken filter. This test catches the next
// drift of either side from the contract.
func TestRerankerSidecarAlertFilterMatchesEmitter(t *testing.T) {
	t.Parallel()

	f := loadAlertFile(t)
	var rule *alertRule
	for gi := range f.Groups {
		for ri := range f.Groups[gi].Rules {
			r := &f.Groups[gi].Rules[ri]
			if r.Alert == "reranker_sidecar_error_burst" {
				rule = r
				break
			}
		}
	}
	if rule == nil {
		t.Fatalf("reranker_sidecar_error_burst rule missing; iter-3 added it -- did this iter remove it?")
	}
	// Capture the LITERAL string the alert filters on.
	if !strings.Contains(rule.Expr, `status="error"`) {
		t.Errorf("alert filters on something other than status=\"error\" -- update this test AND the Python emitter together: %s", rule.Expr)
	}

	// Now read the Python observability module and assert it
	// emits the same literal. We grep the source file rather
	// than spinning up a Python interpreter from the Go
	// test process; the contract is the LABEL VALUE, which
	// is a string in both source files.
	pyPath := filepath.Join("..", "cmd", "reranker-sidecar", "observability.py")
	pyBody, err := os.ReadFile(pyPath)
	if err != nil {
		t.Fatalf("read %s: %v", pyPath, err)
	}
	body := string(pyBody)
	// The emitter source MUST reference the bounded enum
	// (either via the constant or the literal). We accept
	// either form so a future rename of `_STATUS_ERROR` to
	// `STATUS_ERROR` does not break this check.
	if !strings.Contains(body, `"error"`) {
		t.Errorf("observability.py does not contain the literal \"error\" status value the alert filters on")
	}
	// AND the emitter MUST NOT emit raw HTTP codes as
	// label values -- the iter-3 regression. Raw `"500"`
	// or `str(response.status_code)` patterns are the
	// red flag.
	if strings.Contains(body, `status="500"`) || strings.Contains(body, `status=str(response.status_code)`) {
		t.Errorf("observability.py still emits raw HTTP status codes as label values -- alert filter status=\"error\" will not match. Use _classify_status() instead.")
	}
}

// TestRerankerSidecarLabelCardinalityBound (iter-4 evaluator
// finding #2) asserts the bounded-route allow-list is in
// place. Cardinality of `route` is the most common metrics
// foot-gun under attacker-controlled paths (404 storms).
func TestRerankerSidecarLabelCardinalityBound(t *testing.T) {
	t.Parallel()

	pyPath := filepath.Join("..", "cmd", "reranker-sidecar", "observability.py")
	body, err := os.ReadFile(pyPath)
	if err != nil {
		t.Fatalf("read %s: %v", pyPath, err)
	}
	src := string(body)
	// The bounded helper must exist and be wired into the
	// middleware. We assert both anchors.
	if !strings.Contains(src, "_KNOWN_ROUTES") {
		t.Errorf("observability.py must define a bounded _KNOWN_ROUTES allow-list to cap route label cardinality")
	}
	if !strings.Contains(src, "_classify_route(request)") {
		t.Errorf("observability.py middleware must call _classify_route(request) instead of using request.url.path directly")
	}
	// The dangerous pattern -- using request.url.path
	// straight as a label -- must be gone.
	if strings.Contains(src, "route = request.url.path") {
		t.Errorf("observability.py still uses raw request.url.path as the route label; 404 storms will explode label cardinality")
	}
}
