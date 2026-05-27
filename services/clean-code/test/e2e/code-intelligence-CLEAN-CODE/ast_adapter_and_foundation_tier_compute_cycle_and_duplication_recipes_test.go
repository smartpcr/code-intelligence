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

	// The probe hand-builds four *parser.AstFile fixtures that
	// mirror what the Stage 2.1 parser fleet emits (one package
	// scope per file, one file scope parented to it, an
	// "imports" edge per dependency with `To.Id =
	// "qualified:<target>"`). It then drives
	// recipes.NewCycleMemberRecipe().ComputeProject(asts) -- the
	// real recipes-package API surface -- and emits a JSON blob
	// that the step assertions read.
	//
	//   a.go (package pkg_a) imports pkg_b
	//   b.go (package pkg_b) imports pkg_c
	//   c.go (package pkg_c) imports pkg_a   -> SCC participants
	//   d.go (package pkg_d) no imports      -> outside cycle
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"

	"%[1]s/internal/ast/parser"
	"%[1]s/internal/ast/scope"
	"%[1]s/internal/metrics/recipes"
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

// makeFile assembles a canonical *parser.AstFile with one
// SCOPE_KIND_PACKAGE scope, one SCOPE_KIND_FILE scope parented
// to it, and one "imports"-kind AstEdge per element in
// 'imports'. The "qualified:<target>" prefix on the edge's
// To.Id is the parser contract that the cycle_member recipe's
// importTarget helper strips before resolving the target
// against the package-name index (see
// internal/metrics/recipes/cycle_member.go importTarget).
func makeFile(filePath, pkgName string, imports []string) *parser.AstFile {
	pkgID := "local:pkg:" + filePath
	fileID := "local:file:" + filePath
	af := &parser.AstFile{
		Language: "go",
		Path:     filePath,
		Attrs:    map[string]string{},
		Scopes: []*parser.AstScope{
			{
				ScopeId:       pkgID,
				ScopeKind:     parser.ScopeKindPackage,
				Name:          pkgName,
				QualifiedName: pkgName,
				Range:         &parser.AstRange{StartByte: 0, EndByte: 1, StartLine: 1, EndLine: 1, StartCol: 1, EndCol: 1},
			},
			{
				ScopeId:       fileID,
				ScopeKind:     parser.ScopeKindFile,
				Name:          filePath,
				QualifiedName: filePath,
				ParentScopeId: pkgID,
				Range:         &parser.AstRange{StartByte: 0, EndByte: 1, StartLine: 1, EndLine: 1, StartCol: 1, EndCol: 1},
			},
		},
	}
	for _, target := range imports {
		af.Edges = append(af.Edges, &parser.AstEdge{
			Kind: "imports",
			From: &parser.AstRef{Kind: parser.RefKindScope, Id: fileID},
			To:   &parser.AstRef{Kind: parser.RefKindScope, Id: "qualified:" + target},
		})
	}
	return af
}

func main() {
	asts := []*parser.AstFile{
		makeFile("a.go", "pkg_a", []string{"pkg_b"}),
		makeFile("b.go", "pkg_b", []string{"pkg_c"}),
		makeFile("c.go", "pkg_c", []string{"pkg_a"}),
		makeFile("d.go", "pkg_d", nil),
	}

	drafts := recipes.NewCycleMemberRecipe().ComputeProject(asts)

	// AllScopeKinds captures EVERY emitted scope_kind so the
	// "NEVER emits at scope_kind=module" guard can spot a
	// non-canonical drift even on package-scope drafts.
	allScopes := make([]string, len(drafts))
	for i := range drafts {
		allScopes[i] = string(drafts[i].Scope.Kind)
	}

	// Results carries ONLY the file-scope drafts -- the step
	// assertions look up by basename ("a.go", "b.go", "c.go",
	// "d.go"); a package-scope draft would share the same
	// basename via path.Base and corrupt the scope_kind check.
	var results []fileResult
	for _, d := range drafts {
		if d.Scope.Kind != scope.KindFile {
			continue
		}
		results = append(results, fileResult{
			File:      path.Base(d.Scope.Path),
			Value:     int(d.Value),
			ScopeKind: string(d.Scope.Kind),
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

	// The probe hand-builds one *parser.AstFile fixture
	// carrying duplicated content under
	// Attrs[recipes.AttrSourceBytes] (the parser's source-bytes
	// attr the duplication_ratio recipe inspects FIRST for
	// lexical tokenisation -- see
	// internal/metrics/recipes/duplication_ratio.go fileTokens
	// tier 1). It then drives
	// recipes.NewDuplicationRatioRecipe().Compute(ast) -- the
	// real recipes-package API surface -- and emits a JSON blob
	// for the step assertions. The exact ratio depends on the
	// 50-token window detector; the contract under test is
	// (a) value in [0,1] and (b) scope_kind in {file,package}.
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%[1]s/internal/ast/parser"
	"%[1]s/internal/metrics/recipes"
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
	// A duplicated block repeated twice (with light variation
	// in the wrapper function name) so the lexical detector
	// has at least one non-trivial 50-token clone to flag.
	duplicatedBlock := "func helper(x int) int {\n\ty := x * 2\n\tz := y + 1\n\treturn z\n}\n"
	fixture := "package dup\n\n" + duplicatedBlock + "\n" +
		"// duplicate below\n" +
		"func helper2(x int) int {\n\ty := x * 2\n\tz := y + 1\n\treturn z\n}\n"

	ast := &parser.AstFile{
		Language: "go",
		Path:     "fixture.go",
		Attrs: map[string]string{
			recipes.AttrSourceBytes: fixture,
		},
		Scopes: []*parser.AstScope{
			{
				ScopeId:       "local:file:fixture.go",
				ScopeKind:     parser.ScopeKindFile,
				Name:          "fixture.go",
				QualifiedName: "fixture.go",
				Range:         &parser.AstRange{StartByte: 0, EndByte: 1, StartLine: 1, EndLine: 1, StartCol: 1, EndCol: 1},
			},
		},
	}

	drafts := recipes.NewDuplicationRatioRecipe().Compute(ast)

	allScopes := make([]string, len(drafts))
	for i := range drafts {
		allScopes[i] = string(drafts[i].Scope.Kind)
	}

	var results []result
	for _, d := range drafts {
		results = append(results, result{
			Value:     d.Value,
			ScopeKind: string(d.Scope.Kind),
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
	// Accept integer or decimal bounds (e.g. "0 and 1" or "0.0 and 1.0"); godog coerces both to float64.
	ctx.Step(`^the emitted value is between (\d+(?:\.\d+)?) and (\d+(?:\.\d+)?) inclusive$`, dup.theEmittedValueIsBetweenAndInclusive)
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
