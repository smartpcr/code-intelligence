package partitionmaintainer

// Tests for the §8.2 Prometheus alert-rule artifact at
// `deploy/local/prometheus/rules/partition_rotation.rules.yml`.
//
// These tests are pure-Go (no Prometheus / yaml.v3 dep) and
// optionally shell out to `promtool` when it is on the PATH so
// the YAML is also semantically validated by the canonical
// tool. Setting `AGENT_MEMORY_PROMTOOL_REQUIRED=1` upgrades the
// promtool steps from "skip when missing" to "fail when
// missing" -- CI sets this once `promtool` is installed in the
// runner image.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// requirePromtool returns the absolute path to `promtool` when
// it is on PATH; otherwise skips the test (or fails if the
// env opt-in is set).
func requirePromtool(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("promtool")
	if err == nil {
		return p
	}
	if os.Getenv("AGENT_MEMORY_PROMTOOL_REQUIRED") != "" {
		t.Fatalf("promtool not on PATH but AGENT_MEMORY_PROMTOOL_REQUIRED is set: %v", err)
	}
	t.Skipf("promtool not on PATH; skipping (set AGENT_MEMORY_PROMTOOL_REQUIRED=1 to fail instead): %v", err)
	return ""
}

// repoRulesDir returns the absolute path of the rules
// directory, relative to this test file. The package is
// `internal/partitionmaintainer/`, so the rules dir is at
// `../../deploy/local/prometheus/rules/`.
func repoRulesDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	abs, err := filepath.Abs(filepath.Join(wd, "..", "..", "deploy", "local", "prometheus", "rules"))
	if err != nil {
		t.Fatalf("abs rules dir: %v", err)
	}
	st, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat rules dir %q: %v", abs, err)
	}
	if !st.IsDir() {
		t.Fatalf("%q is not a directory", abs)
	}
	return abs
}

// TestPartitionRotationRulesYAML_StructuralAssertions reads the
// alert-rule YAML file and asserts the Stage 8.2 alert is
// wired correctly. Doing this in code (rather than relying on
// promtool alone) catches breakage in environments without
// promtool AND closes the loop between the Go metric constant
// and the YAML's `expr`.
//
// Specifically we assert:
//
//   - The alert `PartitionProvisionLagHigh` exists.
//   - Its expression references the gauge metric name exposed
//     by the maintainer (the `MetricPartitionProvisionLagSeconds`
//     constant value -- currently "partition_provision_lag").
//   - The 1-day threshold is the literal 86400.
//   - The persistence window is `for: 10m`.
//   - The severity label is `critical`.
func TestPartitionRotationRulesYAML_StructuralAssertions(t *testing.T) {
	rulesPath := filepath.Join(repoRulesDir(t), "partition_rotation.rules.yml")
	raw, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatalf("read %q: %v", rulesPath, err)
	}
	body := string(raw)

	wantMetricName := MetricPartitionProvisionLagSeconds // "partition_provision_lag"
	checks := []struct {
		want string
		why  string
	}{
		{"alert: PartitionProvisionLagHigh", "Stage 8.2 alert must be named PartitionProvisionLagHigh"},
		{wantMetricName, "alert expr must reference the gauge metric (" + wantMetricName + ")"},
		{"> 86400", "alert threshold must be 86400 seconds (= 1 day)"},
		{"for: 10m", "alert must hold for 10m to ride through a single missed tick"},
		{"severity: critical", "alert must be tagged severity=critical"},
		// The annotations below are pinned by the promtool fixture
		// at partition_rotation.rules_test.yml. Adding or removing
		// an annotation in the rule without mirroring it in the
		// fixture would cause `promtool test rules` to fail; this
		// check catches the same drift even when promtool is
		// unavailable (e.g. on a developer laptop without a Prom
		// install). See the CONTRACT comment in the rule file.
		{"summary:", "alert must emit `summary` annotation (pinned by promtool fixture)"},
		{"description:", "alert must emit `description` annotation (pinned by promtool fixture)"},
		{"runbook_url:", "alert must emit `runbook_url` annotation (pinned by promtool fixture)"},
		{"gauge_value:", "alert must emit `gauge_value` annotation (pinned by promtool fixture)"},
	}
	for _, c := range checks {
		if !strings.Contains(body, c.want) {
			t.Errorf("partition_rotation.rules.yml missing %q -- %s", c.want, c.why)
		}
	}

	// The maintainer ALSO ships a companion stall alert. Its
	// presence is not a Stage 8.2 acceptance criterion but is
	// worth pinning so a future rename of either alert is
	// caught by this test.
	if !strings.Contains(body, "alert: PartitionMaintenanceStalled") {
		t.Errorf("partition_rotation.rules.yml missing companion alert PartitionMaintenanceStalled")
	}
}

// TestPartitionRotationRulesFixture_AnnotationsAreMirrored asserts
// that every annotation key the rule emits is mirrored under
// `exp_annotations:` in the promtool fixture. `promtool test
// rules` does a full-set comparison, so a rule-only annotation
// addition silently breaks the canonical alert test the next
// time promtool runs. This pure-Go check catches the drift in
// every environment (including ones without promtool).
func TestPartitionRotationRulesFixture_AnnotationsAreMirrored(t *testing.T) {
	dir := repoRulesDir(t)
	ruleBody := mustReadFile(t, filepath.Join(dir, "partition_rotation.rules.yml"))
	fixtureBody := mustReadFile(t, filepath.Join(dir, "partition_rotation.rules_test.yml"))

	for _, key := range []string{"summary", "description", "runbook_url", "gauge_value"} {
		ruleKey := key + ":"
		if !strings.Contains(ruleBody, ruleKey) {
			t.Errorf("rule file does not emit annotation %q (test assumption broken)", key)
			continue
		}
		if !strings.Contains(fixtureBody, ruleKey) {
			t.Errorf("promtool fixture is missing annotation %q under exp_annotations -- promtool test rules will fail", key)
		}
	}
}

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %q: %v", path, err)
	}
	return string(b)
}

// TestPartitionRotationRules_PromtoolCheckRules invokes
// `promtool check rules` on the file -- the canonical syntactic
// validator. Skipped when promtool is unavailable unless
// AGENT_MEMORY_PROMTOOL_REQUIRED is set.
func TestPartitionRotationRules_PromtoolCheckRules(t *testing.T) {
	promtool := requirePromtool(t)
	rulesPath := filepath.Join(repoRulesDir(t), "partition_rotation.rules.yml")
	cmd := exec.Command(promtool, "check", "rules", rulesPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("promtool check rules failed: %v\n%s", err, out)
	}
	// promtool prints "SUCCESS: N rules found" on success; we
	// don't pin the format because promtool minor versions
	// have re-worded it. Exit code 0 is the contract.
}

// TestPartitionRotationRules_PromtoolTestRules evaluates the
// alert against the synthetic-series fixture in
// `partition_rotation.rules_test.yml`. This is the §8.2 "lag
// alert fires" acceptance scenario expressed in the canonical
// promtool format: a 25-hour gauge above 86400 must fire the
// alert; a sub-86400 gauge must not.
func TestPartitionRotationRules_PromtoolTestRules(t *testing.T) {
	promtool := requirePromtool(t)
	testPath := filepath.Join(repoRulesDir(t), "partition_rotation.rules_test.yml")
	if _, err := os.Stat(testPath); err != nil {
		t.Fatalf("stat %q: %v", testPath, err)
	}
	cmd := exec.Command(promtool, "test", "rules", testPath)
	// promtool resolves rule_files relative to the test file's
	// directory, so cd there before invoking.
	cmd.Dir = filepath.Dir(testPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("promtool test rules failed: %v\n%s", err, out)
	}
}
