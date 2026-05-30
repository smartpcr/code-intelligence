// -----------------------------------------------------------------------
// <copyright file="foundations_dev_policy_loader_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package devpolicy_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"gopkg.in/yaml.v3"
)

// moduleRoot returns the module root (services/clean-code) relative to
// this test file at internal/cli/devpolicy/ — three directories up.
func moduleRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// rulePackOnDisk mirrors the YAML on-disk shape so the test independently
// parses each embedded YAML and cross-checks every rule_id.
type rulePackOnDisk struct {
	PackID string `yaml:"pack_id"`
	Rules  []struct {
		RuleID string `yaml:"rule_id"`
	} `yaml:"rules"`
}

type devPolicyState struct {
	loader    devpolicy.Loader
	bundle    devpolicy.Bundle
	policyIDA string
	policyIDB string
	tmpDir    string
	yamlIDs   []string
	banner    bytes.Buffer
	prodOut   string
	prodErr   error
	modRoot   string
}

func newDevPolicyState() *devPolicyState {
	return &devPolicyState{modRoot: moduleRoot()}
}

// --- Scenario: embedded packs loaded ---

func (s *devPolicyState) aNoTagBuild() {
	s.loader = devpolicy.NewLoader()
}

func (s *devPolicyState) loaderLoadCalledWithEmbeddedTrue() error {
	b, err := s.loader.Load(context.Background(), devpolicy.LoaderSource{UseEmbedded: true})
	if err != nil {
		return fmt.Errorf("Loader.Load(UseEmbedded:true): %w", err)
	}
	s.bundle = b
	return nil
}

func (s *devPolicyState) everyYAMLProducedAtLeastOneRule() error {
	// Independently walk the on-disk rulepacks source tree (NOT via
	// LoaderSource.FS()) so the verification is truly independent of
	// the embed wiring the Loader uses.
	rulepacksDir := filepath.Join(s.modRoot, "policy", "rulepacks")
	embFS := os.DirFS(rulepacksDir)

	loadedIDs := make(map[string]bool)
	for _, r := range s.bundle.Rules {
		loadedIDs[r.RuleID] = true
	}

	var yamlCount int
	walkErr := fs.WalkDir(embFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return nil
		}
		yamlCount++

		data, readErr := fs.ReadFile(embFS, path)
		if readErr != nil {
			return fmt.Errorf("reading %s: %w", path, readErr)
		}

		var pack rulePackOnDisk
		if yamlErr := yaml.Unmarshal(data, &pack); yamlErr != nil {
			return fmt.Errorf("parsing %s: %w", path, yamlErr)
		}

		if len(pack.Rules) == 0 {
			return fmt.Errorf("YAML %s declares zero rules", path)
		}

		for _, r := range pack.Rules {
			if !loadedIDs[r.RuleID] {
				return fmt.Errorf("rule %q from %s not found in Bundle.Rules", r.RuleID, path)
			}
		}

		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	if yamlCount == 0 {
		return fmt.Errorf("no YAML files found in embedded FS")
	}
	if len(s.bundle.RulePacks) != yamlCount {
		return fmt.Errorf("expected %d RulePacks (one per YAML), got %d", yamlCount, len(s.bundle.RulePacks))
	}
	return nil
}

// --- Scenario: stable policy id ---

func (s *devPolicyState) embeddedPackSetLoadedOnce() error {
	s.loader = devpolicy.NewLoader()
	b, err := s.loader.Load(context.Background(), devpolicy.LoaderSource{UseEmbedded: true})
	if err != nil {
		return fmt.Errorf("Load: %w", err)
	}
	s.bundle = b
	return nil
}

func (s *devPolicyState) synthesisePolicyVersionCalledTwice() {
	// Call SynthesisePolicyVersion directly on the same rule list twice.
	pvA := devpolicy.SynthesisePolicyVersion(s.bundle.Rules)
	pvB := devpolicy.SynthesisePolicyVersion(s.bundle.Rules)
	s.policyIDA = pvA.PolicyVersionID
	s.policyIDB = pvB.PolicyVersionID
}

func (s *devPolicyState) policyVersionIDsAreIdentical() error {
	if s.policyIDA == "" {
		return fmt.Errorf("PolicyVersionID A is empty")
	}
	if s.policyIDA != s.policyIDB {
		return fmt.Errorf("PolicyVersionID mismatch: %q vs %q", s.policyIDA, s.policyIDB)
	}
	return nil
}

// --- Scenario: filesystem override ---

func (s *devPolicyState) tempDirWithCustomYAML() error {
	dir, err := os.MkdirTemp("", "devpolicy-e2e-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	s.tmpDir = dir
	s.yamlIDs = []string{"e2e-custom-rule-1", "e2e-custom-rule-2"}

	yamlContent := `pack_id: "e2e-custom-pack"
version: "1.0"
rules:
  - rule_id: "e2e-custom-rule-1"
    metric_kind: "loc"
    scope_kind: "function"
    predicate_dsl: "metric_kind == 'loc' AND value >= 100"
    description: "Functions with more than 100 lines"
    suggested_refactor: "Extract smaller functions"
  - rule_id: "e2e-custom-rule-2"
    metric_kind: "duplication_ratio"
    scope_kind: "file"
    predicate_dsl: "metric_kind == 'duplication_ratio' AND value >= 0.3"
    description: "Files with high duplication"
    suggested_refactor: "Extract shared utilities"
`
	if err := os.WriteFile(filepath.Join(dir, "custom.yaml"), []byte(yamlContent), 0644); err != nil {
		return fmt.Errorf("writing custom.yaml: %w", err)
	}
	s.loader = devpolicy.NewLoader()
	return nil
}

func (s *devPolicyState) loaderLoadWithDirPath() error {
	b, err := s.loader.Load(context.Background(), devpolicy.LoaderSource{
		UseEmbedded: false,
		DirPath:     s.tmpDir,
	})
	if err != nil {
		return fmt.Errorf("Loader.Load(DirPath): %w", err)
	}
	s.bundle = b
	return nil
}

func (s *devPolicyState) rulePacksLengthIs1AndRuleIDsMatch() error {
	if s.tmpDir != "" {
		defer os.RemoveAll(s.tmpDir)
	}
	if len(s.bundle.RulePacks) != 1 {
		return fmt.Errorf("expected 1 RulePack, got %d", len(s.bundle.RulePacks))
	}
	actualIDs := make(map[string]bool)
	for _, r := range s.bundle.Rules {
		actualIDs[r.RuleID] = true
	}
	for _, expectedID := range s.yamlIDs {
		if !actualIDs[expectedID] {
			return fmt.Errorf("expected rule ID %q not found in Bundle.Rules (got %v)", expectedID, actualIDs)
		}
	}
	return nil
}

// --- Scenario: prod build refuses bypass ---

func (s *devPolicyState) goBuildWithTagsProd() {
	// Actual go build + test happens in the When step.
}

func (s *devPolicyState) testCallsLoaderLoadInProd() error {
	testCode := `package devpolicy

import (
	"context"
	"testing"
)

func TestProdRefusal_e2e_check(t *testing.T) {
	l := NewLoader()
	_, err := l.Load(context.Background(), LoaderSource{UseEmbedded: true})
	if err == nil {
		t.Fatal("expected error from prod build, got nil")
	}
	expected := "dev-mode policy bypass not available in prod build"
	if err.Error() != expected {
		t.Fatalf("expected error %q, got %q", expected, err.Error())
	}
}
`
	// Write the temp test file into an OS temp directory instead of the
	// source tree, then use -overlay to map it into the package for the
	// go test invocation.  This avoids leaving a _test.go behind if the
	// process is killed before cleanup runs.
	tmpDir, err := os.MkdirTemp("", "prod-e2e-check-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	tmpTestFile := filepath.Join(tmpDir, "prod_e2e_check_test.go")
	if err := os.WriteFile(tmpTestFile, []byte(testCode), 0644); err != nil {
		return fmt.Errorf("writing temp prod test: %w", err)
	}

	// Build the overlay JSON that maps the virtual source-tree path to
	// the actual temp file so `go test` sees it inside the package.
	virtualPath := filepath.Join(s.modRoot, "internal", "cli", "devpolicy", "prod_e2e_check_test.go")
	overlay := struct {
		Replace map[string]string `json:"Replace"`
	}{
		Replace: map[string]string{virtualPath: tmpTestFile},
	}
	overlayJSON, err := json.Marshal(overlay)
	if err != nil {
		return fmt.Errorf("marshalling overlay JSON: %w", err)
	}
	overlayFile := filepath.Join(tmpDir, "overlay.json")
	if err := os.WriteFile(overlayFile, overlayJSON, 0644); err != nil {
		return fmt.Errorf("writing overlay.json: %w", err)
	}

	cmd := exec.Command("go", "test", "-overlay", overlayFile, "-tags", "prod", "-run", "TestProdRefusal_e2e_check", "-count=1", "-v", "./internal/cli/devpolicy/")
	cmd.Dir = s.modRoot
	out, err := cmd.CombinedOutput()
	s.prodOut = string(out)
	s.prodErr = err
	return nil
}

func (s *devPolicyState) itReturnsTheError(expected string) error {
	if s.prodErr != nil {
		return fmt.Errorf("prod build test failed (exit error): %v\nOutput:\n%s", s.prodErr, s.prodOut)
	}
	if !strings.Contains(s.prodOut, "PASS") {
		return fmt.Errorf("prod build test did not PASS:\n%s", s.prodOut)
	}
	return nil
}

// --- Scenario: banner text exact ---

func (s *devPolicyState) aDevBuild() {
	// Running in a dev build (no prod tag); nothing to set up.
}

func (s *devPolicyState) emitBannerWritesToBuffer() {
	s.banner.Reset()
	devpolicy.EmitBanner(&s.banner)
}

func (s *devPolicyState) bufferEqualsC10BannerLiteral() error {
	actual := s.banner.String()
	// Hardcoded C10 banner literal — NOT referencing devpolicy.DevModeBanner
	// so this test is a true contract check, not a tautology.
	const c10Banner = "\u26a0  DEV MODE \u2014 unsigned policy bypass active. Not for production use.\n"
	if actual != c10Banner {
		return fmt.Errorf("banner mismatch:\n  expected: %q\n  actual:   %q", c10Banner, actual)
	}
	return nil
}

// --- Scenario initializer ---

func InitializeScenario_foundations_dev_policy_loader(ctx *godog.ScenarioContext) {
	s := newDevPolicyState()

	// Scenario: embedded packs loaded
	ctx.Step(`^a no-tag build$`, s.aNoTagBuild)
	ctx.Step(`^Loader\.Load is called with LoaderSource UseEmbedded true$`, s.loaderLoadCalledWithEmbeddedTrue)
	ctx.Step(`^every YAML file under the embedded rulepacks FS produced at least one rule in Bundle\.Rules$`, s.everyYAMLProducedAtLeastOneRule)

	// Scenario: stable policy id
	ctx.Step(`^the embedded pack set is loaded once to obtain the rule list$`, s.embeddedPackSetLoadedOnce)
	ctx.Step(`^SynthesisePolicyVersion is called on that rule list twice$`, s.synthesisePolicyVersionCalledTwice)
	ctx.Step(`^the two returned PolicyVersionIDs are byte-for-byte identical$`, s.policyVersionIDsAreIdentical)

	// Scenario: filesystem override
	ctx.Step(`^a temp directory with custom\.yaml matching the embedded shape$`, s.tempDirWithCustomYAML)
	ctx.Step(`^Loader\.Load is called with LoaderSource UseEmbedded false and DirPath set to the temp directory$`, s.loaderLoadWithDirPath)
	ctx.Step(`^the returned Bundle\.RulePacks length is 1 and the rule ids match the YAML$`, s.rulePacksLengthIs1AndRuleIDsMatch)

	// Scenario: prod build refuses bypass
	ctx.Step(`^a go build with tags prod of internal/cli/devpolicy$`, s.goBuildWithTagsProd)
	ctx.Step(`^the test calls Loader\.Load$`, s.testCallsLoaderLoadInProd)
	ctx.Step(`^it returns the error "([^"]*)"$`, s.itReturnsTheError)

	// Scenario: banner text exact
	ctx.Step(`^a dev build$`, s.aDevBuild)
	ctx.Step(`^EmitBanner writes to a bytes\.Buffer$`, s.emitBannerWritesToBuffer)
	ctx.Step(`^the buffer string equals "([^"]*)"$`, s.bufferEqualsC10BannerLiteral)
}

func TestE2E_foundations_dev_policy_loader(t *testing.T) {
	featurePath := filepath.Join("..", "..", "..", "test", "e2e",
		"code-intelligence-REFACTOR-GUIDE",
		"foundations_dev_policy_loader.feature")

	if _, err := os.Stat(featurePath); err != nil {
		t.Fatalf("feature file not found at %s: %v", featurePath, err)
	}

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_foundations_dev_policy_loader,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{featurePath},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}