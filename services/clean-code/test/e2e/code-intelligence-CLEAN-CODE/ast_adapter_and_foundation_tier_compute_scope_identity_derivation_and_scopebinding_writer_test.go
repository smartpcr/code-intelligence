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

// runProbe compiles and executes a small Go program within the service
// module, returning its stdout (for JSON parsing), stderr (for error
// diagnostics only), and exit code. Stdout and stderr are captured into
// separate buffers so that build-time noise from `go run` (module
// downloads, vet diagnostics, etc.) does not get prepended to the JSON
// payload on stdout and break json.Unmarshal in the callers.
func runProbe(svcRoot, source string) (string, string, int, error) {
	tmpDir, err := os.MkdirTemp(svcRoot, "e2e-scope-probe-")
	if err != nil {
		return "", "", -1, fmt.Errorf("creating probe dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if err := os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte(source), 0644); err != nil {
		return "", "", -1, fmt.Errorf("writing probe: %w", err)
	}

	relDir, err := filepath.Rel(svcRoot, tmpDir)
	if err != nil {
		return "", "", -1, fmt.Errorf("relative path: %w", err)
	}

	cmd := exec.Command("go", "run", "./"+filepath.ToSlash(relDir))
	cmd.Dir = svcRoot
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	exitCode := 0
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	return stdout.String(), stderr.String(), exitCode, nil
}

// ---------- Scenario: scope-id-determinism ----------

type scopeIDDeterminismState struct {
	svcRoot string
	id1     string
	id2     string
}

func (s *scopeIDDeterminismState) theSameInputsInvokedTwice() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *scopeIDDeterminismState) deriveScopeIDRunsForBothInvocations() error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/ast/scope"
)

type result struct {
	ID1 string `+"`"+`json:"id1"`+"`"+`
	ID2 string `+"`"+`json:"id2"`+"`"+`
}

func main() {
	repoID := "repo-001"
	scopeKind := "function"
	canonicalSig := "pkg.Foo#bar(int)"
	firstSeenSHA := "aabbccdd"

	id1 := scope.DeriveScopeID(repoID, scopeKind, canonicalSig, firstSeenSHA)
	id2 := scope.DeriveScopeID(repoID, scopeKind, canonicalSig, firstSeenSHA)

	if err := json.NewEncoder(os.Stdout).Encode(result{ID1: id1, ID2: id2}); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	stdoutOut, stderrOut, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running determinism probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("determinism probe exited %d:\nstderr: %s\nstdout: %s", exitCode, stderrOut, stdoutOut)
	}

	var res struct {
		ID1 string `json:"id1"`
		ID2 string `json:"id2"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdoutOut)), &res); err != nil {
		return fmt.Errorf("parsing probe output: %w\nstdout: %s\nstderr: %s", err, stdoutOut, stderrOut)
	}

	s.id1 = res.ID1
	s.id2 = res.ID2
	return nil
}

func (s *scopeIDDeterminismState) itReturnsTheSameUUIDBothTimes() error {
	if s.id1 == "" {
		return fmt.Errorf("first scope_id is empty")
	}
	if s.id1 != s.id2 {
		return fmt.Errorf("scope_id mismatch: %q != %q", s.id1, s.id2)
	}
	return nil
}

// ---------- Scenario: scope-id-stable-across-shas ----------

type scopeIDStableState struct {
	svcRoot string
	idShaA  string
	idShaB  string
}

func (s *scopeIDStableState) aSignatureFirstSeenAtSHAa() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *scopeIDStableState) theSameSignatureAppearsAtSHAb() error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/ast/scope"
)

type result struct {
	IDShaA string `+"`"+`json:"id_sha_a"`+"`"+`
	IDShaB string `+"`"+`json:"id_sha_b"`+"`"+`
}

func main() {
	repoID := "repo-001"
	scopeKind := "function"
	canonicalSig := "pkg.Foo#bar(int)"
	shaA := "aaaa1111"
	shaB := "bbbb2222"

	idA := scope.DeriveScopeID(repoID, scopeKind, canonicalSig, shaA)
	idB := scope.DeriveScopeID(repoID, scopeKind, canonicalSig, shaB)

	if err := json.NewEncoder(os.Stdout).Encode(result{IDShaA: idA, IDShaB: idB}); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	stdoutOut, stderrOut, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running SHA-stability probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("SHA-stability probe exited %d:\nstderr: %s\nstdout: %s", exitCode, stderrOut, stdoutOut)
	}

	var res struct {
		IDShaA string `json:"id_sha_a"`
		IDShaB string `json:"id_sha_b"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdoutOut)), &res); err != nil {
		return fmt.Errorf("parsing probe output: %w\nstdout: %s\nstderr: %s", err, stdoutOut, stderrOut)
	}

	s.idShaA = res.IDShaA
	s.idShaB = res.IDShaB
	return nil
}

func (s *scopeIDStableState) deriveScopeIDAtSHAbReturnsTheSameScopeID() error {
	if s.idShaA == "" {
		return fmt.Errorf("scope_id at SHA A is empty")
	}
	if s.idShaA != s.idShaB {
		return fmt.Errorf("scope_id NOT stable across SHAs: SHA_A=%q SHA_B=%q (G2 violation: SHA must not be part of identity)", s.idShaA, s.idShaB)
	}
	return nil
}

// ---------- Scenario: scope-binding-idempotent-write ----------

type scopeBindingIdempotentState struct {
	svcRoot      string
	firstSeenSHA string
	rowCountPre  int
	rowCountPost int
	finalSHA     string
	writeErr     string
}

func (s *scopeBindingIdempotentState) aScopeBindingRowAlreadyPresent() error {
	s.svcRoot = serviceRoot()
	return nil
}

func (s *scopeBindingIdempotentState) theWriterReInsertsTheSameScopeIDAtADifferentSHA() error {
	modPath, err := readModulePath(s.svcRoot)
	if err != nil {
		return fmt.Errorf("reading module path: %w", err)
	}

	// The probe:
	// 1. Creates an in-memory ScopeBindingWriter
	// 2. Writes a scope_binding row (scope_id at SHA_A)
	// 3. Records row count and first_seen_sha
	// 4. Writes the same scope_id at SHA_B (re-insert)
	// 5. Records row count and first_seen_sha again
	// 6. Outputs JSON with both snapshots
	probe := fmt.Sprintf(`package main

import (
	"encoding/json"
	"fmt"
	"os"

	"%s/internal/ast/scope"
)

type result struct {
	WriteErr     string `+"`"+`json:"write_err"`+"`"+`
	RowCountPre  int    `+"`"+`json:"row_count_pre"`+"`"+`
	RowCountPost int    `+"`"+`json:"row_count_post"`+"`"+`
	FirstSeenSHA string `+"`"+`json:"first_seen_sha"`+"`"+`
	FinalSHA     string `+"`"+`json:"final_sha"`+"`"+`
}

func main() {
	repoID := "repo-001"
	scopeKind := "function"
	canonicalSig := "pkg.Foo#bar(int)"
	shaA := "aaaa1111"
	shaB := "bbbb2222"

	scopeID := scope.DeriveScopeID(repoID, scopeKind, canonicalSig, shaA)

	writer := scope.NewBindingWriter()

	// First write: insert at SHA A.
	if err := writer.Write(scopeID, repoID, scopeKind, canonicalSig, shaA); err != nil {
		fmt.Fprintf(os.Stderr, "first write failed: %%v\n", err)
		os.Exit(1)
	}
	rowCountPre := writer.RowCount()
	firstSHA := writer.FirstSeenSHA(scopeID)

	// Second write: re-insert same scope_id at SHA B.
	writeErr := ""
	if err := writer.Write(scopeID, repoID, scopeKind, canonicalSig, shaB); err != nil {
		writeErr = err.Error()
	}
	rowCountPost := writer.RowCount()
	finalSHA := writer.FirstSeenSHA(scopeID)

	r := result{
		WriteErr:     writeErr,
		RowCountPre:  rowCountPre,
		RowCountPost: rowCountPost,
		FirstSeenSHA: firstSHA,
		FinalSHA:     finalSHA,
	}

	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		fmt.Fprintf(os.Stderr, "json encode: %%v\n", err)
		os.Exit(1)
	}
}
`, modPath)

	stdoutOut, stderrOut, exitCode, err := runProbe(s.svcRoot, probe)
	if err != nil {
		return fmt.Errorf("running idempotent-write probe: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("idempotent-write probe exited %d:\nstderr: %s\nstdout: %s", exitCode, stderrOut, stdoutOut)
	}

	var res struct {
		WriteErr     string `json:"write_err"`
		RowCountPre  int    `json:"row_count_pre"`
		RowCountPost int    `json:"row_count_post"`
		FirstSeenSHA string `json:"first_seen_sha"`
		FinalSHA     string `json:"final_sha"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdoutOut)), &res); err != nil {
		return fmt.Errorf("parsing probe output: %w\nstdout: %s\nstderr: %s", err, stdoutOut, stderrOut)
	}

	s.writeErr = res.WriteErr
	s.rowCountPre = res.RowCountPre
	s.rowCountPost = res.RowCountPost
	s.firstSeenSHA = res.FirstSeenSHA
	s.finalSHA = res.FinalSHA
	return nil
}

func (s *scopeBindingIdempotentState) noErrorAndRowCountUnchangedAndFirstSeenSHAPreserved() error {
	if s.writeErr != "" {
		return fmt.Errorf("re-insert surfaced an error: %s", s.writeErr)
	}
	if s.rowCountPre != s.rowCountPost {
		return fmt.Errorf("row count changed after re-insert: pre=%d post=%d", s.rowCountPre, s.rowCountPost)
	}
	if s.firstSeenSHA == "" {
		return fmt.Errorf("first_seen_sha is empty")
	}
	if s.firstSeenSHA != s.finalSHA {
		return fmt.Errorf("first_seen_sha was mutated: original=%q final=%q", s.firstSeenSHA, s.finalSHA)
	}
	// Confirm it preserved the ORIGINAL SHA (A), not the re-insert SHA (B).
	if s.finalSHA == "bbbb2222" {
		return fmt.Errorf("first_seen_sha was overwritten to SHA B (%s); expected SHA A to be preserved", s.finalSHA)
	}
	return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_ast_adapter_and_foundation_tier_compute_scope_identity_derivation_and_scopebinding_writer(ctx *godog.ScenarioContext) {
	det := &scopeIDDeterminismState{}
	stab := &scopeIDStableState{}
	bind := &scopeBindingIdempotentState{}

	// scope-id-determinism
	ctx.Step(`^the same repo_id, scope_kind, canonical_signature, and first_seen_sha invoked twice$`, det.theSameInputsInvokedTwice)
	ctx.Step(`^DeriveScopeID runs for both invocations$`, det.deriveScopeIDRunsForBothInvocations)
	ctx.Step(`^it returns the same UUID both times$`, det.itReturnsTheSameUUIDBothTimes)

	// scope-id-stable-across-shas
	ctx.Step(`^a signature "pkg\.Foo#bar\(int\)" first seen at SHA A$`, stab.aSignatureFirstSeenAtSHAa)
	ctx.Step(`^the same signature appears at SHA B$`, stab.theSameSignatureAppearsAtSHAb)
	ctx.Step(`^DeriveScopeID at SHA B returns the same scope_id as at SHA A$`, stab.deriveScopeIDAtSHAbReturnsTheSameScopeID)

	// scope-binding-idempotent-write
	ctx.Step(`^a scope_binding row already present for a scope_id$`, bind.aScopeBindingRowAlreadyPresent)
	ctx.Step(`^the writer re-inserts the same scope_id at a different SHA$`, bind.theWriterReInsertsTheSameScopeIDAtADifferentSHA)
	ctx.Step(`^no error surfaces and the row count remains unchanged and first_seen_sha is preserved$`, bind.noErrorAndRowCountUnchangedAndFirstSeenSHAPreserved)
}

func TestE2E_ast_adapter_and_foundation_tier_compute_scope_identity_derivation_and_scopebinding_writer(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_ast_adapter_and_foundation_tier_compute_scope_identity_derivation_and_scopebinding_writer,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"ast_adapter_and_foundation_tier_compute_scope_identity_derivation_and_scopebinding_writer.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
