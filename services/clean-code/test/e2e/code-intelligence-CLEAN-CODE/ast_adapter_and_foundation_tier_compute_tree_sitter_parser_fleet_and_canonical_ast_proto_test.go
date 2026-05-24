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

// fixturesRoot returns the absolute path to the tests/fixtures/ast directory.
func fixturesRoot() string {
	return filepath.Join(serviceRoot(), "tests", "fixtures", "ast")
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
//
// Design note: the generated probe imports `${modPath}/internal/ast`,
// and Go's internal-package rule is filesystem-scoped — a probe living
// in `os.TempDir()` / `t.TempDir()` cannot import the module's
// internal packages, and neither `go.mod replace` nor `go run -modfile`
// lift that restriction (both affect module-path resolution, not the
// internal/ filesystem rule). The probe must therefore live inside
// svcRoot's tree.
//
// To avoid scattering transient probe directories at the module root,
// all scratch dirs are confined to a single dedicated subdirectory,
// svcRoot/.e2e-probes/, which:
//   - is skipped by `go list ./...` / `go build ./...` (dot-prefixed),
//   - is easy to gitignore and exclude from editor/linter scans, and
//   - keeps any directories leaked by abnormal exits in one predictable
//     place.
//
// The probe is invoked with `go run -mod=readonly <file>` so accidental
// dependency resolution cannot mutate go.mod or go.sum.
func runProbe(svcRoot, source string) (string, int, error) {
	probesRoot := filepath.Join(svcRoot, ".e2e-probes")
	if err := os.MkdirAll(probesRoot, 0o755); err != nil {
		return "", -1, fmt.Errorf("creating probes root: %w", err)
	}

	tmpDir, err := os.MkdirTemp(probesRoot, "probe-")
	if err != nil {
		return "", -1, fmt.Errorf("creating probe dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	mainPath := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(mainPath, []byte(source), 0o644); err != nil {
		return "", -1, fmt.Errorf("writing probe: %w", err)
	}

	cmd := exec.Command("go", "run", "-mod=readonly", mainPath)
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

// ---------- Scenario: parser-supports-v1-four-languages ----------

// parserFleetState carries state across steps for the parser-fleet scenarios.
type parserFleetState struct {
	svcRoot      string
	fixturesDir  string
	parseResults map[string]parseResultEntry // language -> result
	guardError   string
}

// parseResultEntry captures the outcome of parsing a single fixture file.
type parseResultEntry struct {
	Language  string `json:"language"`
	ScopeCount int  `json:"scope_count"`
	FileOK    bool   `json:"file_ok"`
}

func (p *parserFleetState) aFixtureFilePerV1PinnedLanguage() error {
	p.svcRoot = serviceRoot()
	p.fixturesDir = fixturesRoot()

	// Verify at least the fixture directories exist (or will be created by
	// make fixtures-ast). If they don't exist yet, run the bootstrap target.
	if _, err := os.Stat(p.fixturesDir); os.IsNotExist(err) {
		cmd := exec.Command("make", "fixtures-ast")
		cmd.Dir = p.svcRoot
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("make fixtures-ast failed (exit %v):\n%s", err, buf.String())
		}
	}

	for _, lang := range []string{"go", "python", "typescript", "java"} {
		langDir := filepath.Join(p.fixturesDir, lang)
		if _, err := os.Stat(langDir); err != nil {
			return fmt.Errorf("fixture directory for %s not found at %s: %w", lang, langDir, err)
		}
	}
	return nil
}

func (p *parserFleetState) theRegistryReturnsAParserAndParseRuns() error {
	modPath, err := readModulePath(p.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// The probe program uses the internal/ast package to:
	// 1. Get a parser for each v1 language from the registry
	// 2. Parse the first fixture file found in each language directory
	// 3. Output JSON with the results
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"%s/internal/ast"
)

type result struct {
	Language   string `+"`"+`json:"language"`+"`"+`
	ScopeCount int    `+"`"+`json:"scope_count"`+"`"+`
	FileOK     bool   `+"`"+`json:"file_ok"`+"`"+`
}

func main() {
	fixturesDir := os.Args[1]
	languages := []string{"go", "python", "typescript", "java"}
	results := make(map[string]result)

	registry := ast.NewRegistry()

	for _, lang := range languages {
		langDir := filepath.Join(fixturesDir, lang)
		entries, err := os.ReadDir(langDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading %%s: %%v\n", langDir, err)
			os.Exit(1)
		}
		if len(entries) == 0 {
			fmt.Fprintf(os.Stderr, "no fixture files in %%s\n", langDir)
			os.Exit(1)
		}

		// Find the first regular file in the fixture directory.
		var fixturePath string
		for _, e := range entries {
			if !e.IsDir() {
				fixturePath = filepath.Join(langDir, e.Name())
				break
			}
		}
		if fixturePath == "" {
			fmt.Fprintf(os.Stderr, "no regular files in %%s\n", langDir)
			os.Exit(1)
		}

		parser, err := registry.ParserFor(lang)
		if err != nil {
			fmt.Fprintf(os.Stderr, "registry.ParserFor(%%s): %%v\n", lang, err)
			os.Exit(1)
		}

		src, err := os.ReadFile(fixturePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading fixture %%s: %%v\n", fixturePath, err)
			os.Exit(1)
		}

		astFile, err := parser.Parse(fixturePath, src)
		if err != nil {
			fmt.Fprintf(os.Stderr, "parse(%%s): %%v\n", fixturePath, err)
			os.Exit(1)
		}

		results[lang] = result{
			Language:   astFile.Language,
			ScopeCount: len(astFile.Scopes),
			FileOK:     astFile.Language != "" && len(astFile.Scopes) > 0,
		}
	}

	if err := json.NewEncoder(os.Stdout).Encode(results); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	output, exitCode, err := runProbe(p.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running parse probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("parse probe exited %d:\n%s", exitCode, output)
	}

	p.parseResults = make(map[string]parseResultEntry)
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &p.parseResults); err != nil {
		return fmt.Errorf("parsing probe output: %w\nraw: %s", err, output)
	}
	return nil
}

func (p *parserFleetState) eachReturnsANonEmptyAstFileWithLanguageTagAndScope() error {
	for _, lang := range []string{"go", "python", "typescript", "java"} {
		r, ok := p.parseResults[lang]
		if !ok {
			return fmt.Errorf("no parse result for language %q", lang)
		}
		if r.Language == "" {
			return fmt.Errorf("AstFile.Language is empty for %s", lang)
		}
		if r.ScopeCount == 0 {
			return fmt.Errorf("AstFile.Scopes is empty for %s (expected at least one AstScope)", lang)
		}
		if !r.FileOK {
			return fmt.Errorf("parse result not OK for %s", lang)
		}
	}
	return nil
}

func (p *parserFleetState) attemptingToRegisterAFifthLanguageFailsTheRegistryGuard() error {
	modPath, err := readModulePath(p.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// The probe attempts to register C# (an unsupported language) and
	// expects the registry to return an error.
	probe := fmt.Sprintf(`package main

import (
	"fmt"
	"os"

	"%s/internal/ast"
)

func main() {
	registry := ast.NewRegistry()
	_, err := registry.ParserFor("csharp")
	if err != nil {
		fmt.Println("GUARD_OK:" + err.Error())
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "expected registry guard to reject csharp but it succeeded")
	os.Exit(1)
}
`, modPath)

	output, exitCode, err := runProbe(p.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running guard probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("guard probe exited %d (expected 0 with GUARD_OK):\n%s", exitCode, output)
	}

	trimmed := strings.TrimSpace(output)
	if !strings.HasPrefix(trimmed, "GUARD_OK:") {
		return fmt.Errorf("expected GUARD_OK prefix in output, got: %s", trimmed)
	}
	p.guardError = strings.TrimPrefix(trimmed, "GUARD_OK:")
	return nil
}

// ---------- Scenario: proto-round-trip ----------

// protoRoundTripState carries state for the proto-round-trip scenario.
type protoRoundTripState struct {
	svcRoot     string
	fixturesDir string
	roundTripOK bool
}

func (r *protoRoundTripState) aParsedAstFile() error {
	r.svcRoot = serviceRoot()
	r.fixturesDir = fixturesRoot()

	// Ensure fixtures exist.
	if _, err := os.Stat(r.fixturesDir); os.IsNotExist(err) {
		cmd := exec.Command("make", "fixtures-ast")
		cmd.Dir = r.svcRoot
		var buf bytes.Buffer
		cmd.Stdout = &buf
		cmd.Stderr = &buf
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("make fixtures-ast failed: %v\n%s", err, buf.String())
		}
	}

	// Verify at least one fixture language directory has files.
	goDir := filepath.Join(r.fixturesDir, "go")
	if _, err := os.Stat(goDir); err != nil {
		return fmt.Errorf("go fixture directory not found at %s: %w", goDir, err)
	}
	return nil
}

func (r *protoRoundTripState) itIsSerialisedToProtobufWireFormatAndDeserialised() error {
	modPath, err := readModulePath(r.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// The probe program:
	// 1. Parses a Go fixture file to get an AstFile
	// 2. Serialises the AstFile to protobuf wire format
	// 3. Deserialises the wire bytes back to a new AstFile
	// 4. Compares the two and outputs the result as JSON
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"%s/internal/ast"
	"google.golang.org/protobuf/proto"
)

type result struct {
	Equal         bool   `+"`"+`json:"equal"`+"`"+`
	OrigLanguage  string `+"`"+`json:"orig_language"`+"`"+`
	RtLanguage    string `+"`"+`json:"rt_language"`+"`"+`
	OrigScopes    int    `+"`"+`json:"orig_scopes"`+"`"+`
	RtScopes      int    `+"`"+`json:"rt_scopes"`+"`"+`
	WireBytes     int    `+"`"+`json:"wire_bytes"`+"`"+`
}

func main() {
	fixturesDir := os.Args[1]
	goDir := filepath.Join(fixturesDir, "go")
	entries, err := os.ReadDir(goDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading %%s: %%v\n", goDir, err)
		os.Exit(1)
	}

	var fixturePath string
	for _, e := range entries {
		if !e.IsDir() {
			fixturePath = filepath.Join(goDir, e.Name())
			break
		}
	}
	if fixturePath == "" {
		fmt.Fprintln(os.Stderr, "no fixture files found")
		os.Exit(1)
	}

	registry := ast.NewRegistry()
	parser, err := registry.ParserFor("go")
	if err != nil {
		fmt.Fprintf(os.Stderr, "registry.ParserFor(go): %%v\n", err)
		os.Exit(1)
	}

	src, err := os.ReadFile(fixturePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reading fixture: %%v\n", err)
		os.Exit(1)
	}

	original, err := parser.Parse(fixturePath, src)
	if err != nil {
		fmt.Fprintf(os.Stderr, "parse: %%v\n", err)
		os.Exit(1)
	}

	// Serialise to protobuf wire format.
	wire, err := proto.Marshal(original.ToProto())
	if err != nil {
		fmt.Fprintf(os.Stderr, "proto.Marshal: %%v\n", err)
		os.Exit(1)
	}

	// Deserialise back.
	var pbFile ast.AstFileProto
	if err := proto.Unmarshal(wire, &pbFile); err != nil {
		fmt.Fprintf(os.Stderr, "proto.Unmarshal: %%v\n", err)
		os.Exit(1)
	}

	roundTripped := ast.AstFileFromProto(&pbFile)

	r := result{
		Equal:        original.Equal(roundTripped),
		OrigLanguage: original.Language,
		RtLanguage:   roundTripped.Language,
		OrigScopes:   len(original.Scopes),
		RtScopes:     len(roundTripped.Scopes),
		WireBytes:    len(wire),
	}

	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	output, exitCode, err := runProbe(r.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running round-trip probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("round-trip probe exited %d:\n%s", exitCode, output)
	}

	var res struct {
		Equal        bool   `json:"equal"`
		OrigLanguage string `json:"orig_language"`
		RtLanguage   string `json:"rt_language"`
		OrigScopes   int    `json:"orig_scopes"`
		RtScopes     int    `json:"rt_scopes"`
		WireBytes    int    `json:"wire_bytes"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &res); err != nil {
		return fmt.Errorf("parsing round-trip output: %w\nraw: %s", err, output)
	}

	r.roundTripOK = res.Equal
	if res.WireBytes == 0 {
		return fmt.Errorf("protobuf wire bytes is 0; serialisation produced empty output")
	}
	return nil
}

func (r *protoRoundTripState) theResultingStructEqualsTheOriginalWithNoInformationLoss() error {
	if !r.roundTripOK {
		return fmt.Errorf("round-tripped AstFile does not equal the original (information loss detected)")
	}
	return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_ast_adapter_and_foundation_tier_compute_tree_sitter_parser_fleet_and_canonical_ast_proto(ctx *godog.ScenarioContext) {
	p := &parserFleetState{}
	r := &protoRoundTripState{}

	// parser-supports-v1-four-languages
	ctx.Step(`^a fixture file per v1-pinned language \(Go, Python, TypeScript, Java\)$`, p.aFixtureFilePerV1PinnedLanguage)
	ctx.Step(`^the registry returns a parser and Parse runs$`, p.theRegistryReturnsAParserAndParseRuns)
	ctx.Step(`^each returns a non-empty AstFile with the language tag set and at least one AstScope$`, p.eachReturnsANonEmptyAstFileWithLanguageTagAndScope)
	ctx.Step(`^attempting to register a fifth language fails the registry guard$`, p.attemptingToRegisterAFifthLanguageFailsTheRegistryGuard)

	// proto-round-trip
	ctx.Step(`^a parsed AstFile$`, r.aParsedAstFile)
	ctx.Step(`^it is serialised to protobuf wire format and deserialised$`, r.itIsSerialisedToProtobufWireFormatAndDeserialised)
	ctx.Step(`^the resulting struct equals the original with no information loss$`, r.theResultingStructEqualsTheOriginalWithNoInformationLoss)
}

func TestE2E_ast_adapter_and_foundation_tier_compute_tree_sitter_parser_fleet_and_canonical_ast_proto(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_ast_adapter_and_foundation_tier_compute_tree_sitter_parser_fleet_and_canonical_ast_proto,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"ast_adapter_and_foundation_tier_compute_tree_sitter_parser_fleet_and_canonical_ast_proto.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
