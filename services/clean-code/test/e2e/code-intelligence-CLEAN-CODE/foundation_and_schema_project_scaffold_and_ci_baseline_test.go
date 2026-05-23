//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"gopkg.in/yaml.v3"
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

// repoRoot returns the repository root (two levels above the service).
func repoRoot() string {
root := filepath.Join(serviceRoot(), "..", "..")
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

// scaffoldState carries state across Given/When/Then steps for
// the scaffold-builds-clean and config-honours-pins scenarios.
type scaffoldState struct {
svcRoot     string
repoDir     string
cmdOutput   string
cmdExitCode int
configFile   string
configValues map[string]string
tmpCleanup   func()
}

// ---------- Scenario: scaffold-builds-clean ----------

func (s *scaffoldState) aFreshCheckoutOfTheRepository() error {
s.svcRoot = serviceRoot()
s.repoDir = repoRoot()
if _, err := os.Stat(filepath.Join(s.svcRoot, "Makefile")); err != nil {
return fmt.Errorf("Makefile not found in %s: %w", s.svcRoot, err)
}
return nil
}

func (s *scaffoldState) makeBuildLintTestRunsInTheServiceDirectory() error {
cmd := exec.Command("make", "build", "lint", "test")
cmd.Dir = s.svcRoot
var buf bytes.Buffer
cmd.Stdout = &buf
cmd.Stderr = &buf
err := cmd.Run()
s.cmdOutput = buf.String()
if err != nil {
if exitErr, ok := err.(*exec.ExitError); ok {
s.cmdExitCode = exitErr.ExitCode()
} else {
s.cmdExitCode = -1
}
} else {
s.cmdExitCode = 0
}
return nil
}

func (s *scaffoldState) theCommandExitsWithNoMissingTargetErrors() error {
if s.cmdExitCode != 0 {
return fmt.Errorf("make exited %d; output:\n%s", s.cmdExitCode, s.cmdOutput)
}
for _, p := range []string{"No rule to make target", "missing target", "*** No targets"} {
if strings.Contains(s.cmdOutput, p) {
return fmt.Errorf("missing-target error %q in output:\n%s", p, s.cmdOutput)
}
}
return nil
}

func (s *scaffoldState) theBinaryIsProduced(name string) error {
binPath := filepath.Join(s.svcRoot, "bin", name)
if _, err := os.Stat(binPath); err != nil {
if runtime.GOOS == "windows" {
if _, err2 := os.Stat(binPath + ".exe"); err2 == nil {
return nil
}
}
return fmt.Errorf("binary %q not found at %s: %w", name, binPath, err)
}
return nil
}

// ---------- Scenario: ci-workflow-triggers ----------

// ghWorkflow mirrors the subset of a GitHub Actions workflow YAML we inspect.
type ghWorkflow struct {
	On   ghWorkflowOn          `yaml:"on"`
	Jobs map[string]ghWorkflowJob `yaml:"jobs"`
}

// ghWorkflowOnTrigger represents a single trigger block (push / pull_request).
type ghWorkflowOnTrigger struct {
	Paths []string `yaml:"paths"`
}

// ghWorkflowOn handles "on" as either a map or a list of event names. We
// only need the map form; if the YAML uses the shorthand list we leave
// PullRequest nil and the path-trigger check will fail with a clear message.
type ghWorkflowOn struct {
	PullRequest *ghWorkflowOnTrigger `yaml:"pull_request"`
}

type ghWorkflowJob struct {
	Steps []ghWorkflowStep `yaml:"steps"`
}

type ghWorkflowStep struct {
	Name string `yaml:"name"`
	Run  string `yaml:"run"`
	Uses string `yaml:"uses"`
}

type ciState struct {
	svcRoot     string
	repoDir     string
	pathPattern string
	workflow    *ghWorkflow
	rawYAML     []byte
}

func (c *ciState) aPRTouching(pathPattern string) error {
	c.svcRoot = serviceRoot()
	c.repoDir = repoRoot()
	c.pathPattern = pathPattern
	if _, err := os.Stat(c.svcRoot); err != nil {
		return fmt.Errorf("service directory not found at %s: %w", c.svcRoot, err)
	}
	return nil
}

func (c *ciState) gitHubActionsEvaluatesTheWorkflowFile() error {
	// Locate and parse the workflow YAML so later steps can inspect it.
	wfPath := filepath.Join(c.repoDir, ".github", "workflows", "clean-code-ci.yml")
	data, err := os.ReadFile(wfPath)
	if err != nil {
		return fmt.Errorf("reading workflow file %s: %w", wfPath, err)
	}
	c.rawYAML = data

	var wf ghWorkflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return fmt.Errorf("parsing workflow YAML: %w", err)
	}
	c.workflow = &wf
	return nil
}

// pathPatternMatches checks whether any of the configured pull_request path
// patterns cover the expected path glob (e.g. "services/clean-code/**").
func pathPatternMatches(configured []string, expected string) bool {
	for _, p := range configured {
		if p == expected {
			return true
		}
		// Also accept a trailing wildcard that would cover the expected path.
		if strings.HasSuffix(p, "/**") && strings.HasPrefix(expected, strings.TrimSuffix(p, "/**")) {
			return true
		}
	}
	return false
}

func (c *ciState) workflowRunsMakeLintTestAndContainerBuildAndBothSucceed(workflowPath string) error {
	if c.workflow == nil {
		return fmt.Errorf("workflow was not parsed (gitHubActionsEvaluatesTheWorkflowFile must run first)")
	}

	// 1. Verify the workflow file path matches what was declared in the scenario.
	expectedFull := filepath.Join(c.repoDir, filepath.FromSlash(workflowPath))
	if _, err := os.Stat(expectedFull); err != nil {
		return fmt.Errorf("workflow file %s not found: %w", expectedFull, err)
	}

	// 2. Verify pull_request trigger includes the expected path pattern.
	if c.workflow.On.PullRequest == nil {
		return fmt.Errorf("workflow does not define a pull_request trigger")
	}
	if !pathPatternMatches(c.workflow.On.PullRequest.Paths, c.pathPattern) {
		return fmt.Errorf(
			"pull_request.paths %v does not match expected pattern %q",
			c.workflow.On.PullRequest.Paths, c.pathPattern,
		)
	}

	// 3. Verify at least one job step runs "make lint test" (or separate make lint + make test).
	foundLint := false
	foundTest := false
	foundContainerBuild := false

	for _, job := range c.workflow.Jobs {
		for _, step := range job.Steps {
			run := step.Run
			if strings.Contains(run, "make lint") {
				foundLint = true
			}
			if strings.Contains(run, "make test") {
				foundTest = true
			}
			// "make lint test" satisfies both at once.
			if strings.Contains(run, "make lint test") || strings.Contains(run, "make build lint test") {
				foundLint = true
				foundTest = true
			}
			// Container build: docker build step or a known container-build action.
			if strings.Contains(run, "docker build") ||
				strings.Contains(step.Uses, "docker/build-push-action") {
				foundContainerBuild = true
			}
			// Step name heuristic for container build jobs.
			nameLower := strings.ToLower(step.Name)
			if strings.Contains(nameLower, "container build") || strings.Contains(nameLower, "docker build") || strings.Contains(nameLower, "build image") || strings.Contains(nameLower, "build container") {
				foundContainerBuild = true
			}
		}
	}

	if !foundLint {
		return fmt.Errorf("no workflow step runs make lint; raw YAML:\n%s", string(c.rawYAML))
	}
	if !foundTest {
		return fmt.Errorf("no workflow step runs make test; raw YAML:\n%s", string(c.rawYAML))
	}
	if !foundContainerBuild {
		return fmt.Errorf("no workflow step performs a container build (docker build or docker/build-push-action); raw YAML:\n%s", string(c.rawYAML))
	}

	// 4. Both jobs succeed on the empty scaffold — run make lint, make test,
	//    and docker build locally to verify they exit 0.
	if code, out := c.runCmd("make", "lint"); code != 0 {
		return fmt.Errorf("make lint failed (exit %d):\n%s", code, out)
	}
	if code, out := c.runCmd("make", "test"); code != 0 {
		return fmt.Errorf("make test failed (exit %d):\n%s", code, out)
	}

	dockerfile := filepath.Join(c.svcRoot, "Dockerfile")
	if _, err := os.Stat(dockerfile); err != nil {
		return fmt.Errorf("Dockerfile not found in %s: %w", c.svcRoot, err)
	}
	if code, out := c.runCmd("docker", "build", "--no-cache", "-t", "clean-code-e2e-verify:latest", "."); code != 0 {
		return fmt.Errorf("docker build failed (exit %d):\n%s", code, out)
	}

	return nil
}

func (c *ciState) runCmd(name string, args ...string) (int, string) {
	cmd := exec.Command(name, args...)
	cmd.Dir = c.svcRoot
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
	return exitCode, buf.String()
}

// ---------- Scenario: config-honours-pins ----------

func (s *scaffoldState) aConfigFileThatOmitsTheFiveOperatorPins() error {
s.svcRoot = serviceRoot()
tmpFile, err := os.CreateTemp("", "e2e-config-*.cfg")
if err != nil {
return fmt.Errorf("creating temp config: %w", err)
}
if _, err := tmpFile.WriteString("# E2E test config - all operator pins omitted\n"); err != nil {
tmpFile.Close()
os.Remove(tmpFile.Name())
return err
}
tmpFile.Close()
s.configFile = tmpFile.Name()
s.tmpCleanup = func() { os.Remove(tmpFile.Name()) }
return nil
}

func (s *scaffoldState) theLoaderInitialises() error {
modPath, err := readModulePath(s.svcRoot)
if err != nil {
return fmt.Errorf("reading module path: %w", err)
}

tmpDir, err := os.MkdirTemp(s.svcRoot, "e2e-config-probe-")
if err != nil {
return fmt.Errorf("creating probe dir: %w", err)
}
defer os.RemoveAll(tmpDir)

probe := fmt.Sprintf(`package main

import (
"encoding/json"
"fmt"
"os"

"%s/internal/config"
)

func main() {
cfg, err := config.Load()
if err != nil {
fmt.Fprintf(os.Stderr, "config.Load: %%v\n", err)
os.Exit(1)
}
m := map[string]string{
"ASTModeDefault":               cfg.ASTModeDefault,
"ExternalMetricCoverageFormat": cfg.ExternalMetricCoverageFormat,
"GateDegradedPolicy":           cfg.GateDegradedPolicy,
"PolicySigningRequired":        cfg.PolicySigningRequired,
"RefactorEffortSource":         cfg.RefactorEffortSource,
}
if err := json.NewEncoder(os.Stdout).Encode(m); err != nil {
fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
os.Exit(1)
}
}
`, modPath)

if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(probe), 0644); err != nil {
return fmt.Errorf("writing probe: %w", err)
}

relDir, err := filepath.Rel(s.svcRoot, tmpDir)
if err != nil {
return fmt.Errorf("relative path: %w", err)
}

cmd := exec.Command("go", "run", "./"+filepath.ToSlash(relDir))
cmd.Dir = s.svcRoot

var env []string
for _, e := range os.Environ() {
if !strings.HasPrefix(e, "CLEAN_CODE_") {
env = append(env, e)
}
}
if s.configFile != "" {
env = append(env, "CLEAN_CODE_CONFIG_FILE="+s.configFile)
}
cmd.Env = env

var stdout, stderr bytes.Buffer
cmd.Stdout = &stdout
cmd.Stderr = &stderr

if err := cmd.Run(); err != nil {
return fmt.Errorf("config probe failed: %v\nstderr: %s", err, stderr.String())
}

s.configValues = make(map[string]string)
if err := json.Unmarshal(stdout.Bytes(), &s.configValues); err != nil {
return fmt.Errorf("parsing probe output: %w\nraw: %s", err, stdout.String())
}
return nil
}

func (s *scaffoldState) itReturnsValueForField(expected, field string) error {
fieldMap := map[string]string{
"AST mode default":                "ASTModeDefault",
"external metric coverage format": "ExternalMetricCoverageFormat",
"gate degraded policy":            "GateDegradedPolicy",
"policy signing required":         "PolicySigningRequired",
"refactor effort source":          "RefactorEffortSource",
}
key, ok := fieldMap[strings.TrimSpace(field)]
if !ok {
return fmt.Errorf("unknown config field %q", field)
}
actual, ok := s.configValues[key]
if !ok {
return fmt.Errorf("field %q not found in loader output", key)
}
if actual != expected {
return fmt.Errorf("expected %s = %q, got %q", key, expected, actual)
}
return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_foundation_and_schema_project_scaffold_and_ci_baseline(ctx *godog.ScenarioContext) {
s := &scaffoldState{}

ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
if s.tmpCleanup != nil {
s.tmpCleanup()
}
return ctx, nil
})

// scaffold-builds-clean
ctx.Step(`^a fresh checkout of the repository$`, s.aFreshCheckoutOfTheRepository)
ctx.Step(`^"make build lint test" runs in the service directory$`, s.makeBuildLintTestRunsInTheServiceDirectory)
ctx.Step(`^the command exits 0 with no missing-target errors$`, s.theCommandExitsWithNoMissingTargetErrors)
ctx.Step(`^the "([^"]*)" binary is produced$`, s.theBinaryIsProduced)

// ci-workflow-triggers
ci := &ciState{}
ctx.Step(`^a PR touching "([^"]*)"$`, ci.aPRTouching)
ctx.Step(`^GitHub Actions evaluates the workflow file$`, ci.gitHubActionsEvaluatesTheWorkflowFile)
ctx.Step(`^"([^"]*)" runs make lint test and the container build job and both succeed on the empty scaffold$`, ci.workflowRunsMakeLintTestAndContainerBuildAndBothSucceed)

// config-honours-pins
ctx.Step(`^a config file that omits the five operator pins$`, s.aConfigFileThatOmitsTheFiveOperatorPins)
ctx.Step(`^the loader initialises$`, s.theLoaderInitialises)
ctx.Step(`^it returns "([^"]*)" as the (.+)$`, s.itReturnsValueForField)
}

func TestE2E_foundation_and_schema_project_scaffold_and_ci_baseline(t *testing.T) {
suite := godog.TestSuite{
ScenarioInitializer: InitializeScenario_foundation_and_schema_project_scaffold_and_ci_baseline,
Options: &godog.Options{
Format:   "pretty",
Paths:    []string{"foundation_and_schema_project_scaffold_and_ci_baseline.feature"},
TestingT: t,
},
}
if suite.Run() != 0 {
t.Fatal("non-zero status returned, failed to run feature tests")
}
}