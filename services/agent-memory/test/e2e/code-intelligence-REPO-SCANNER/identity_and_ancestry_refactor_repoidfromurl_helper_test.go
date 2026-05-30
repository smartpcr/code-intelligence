//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type repoIDFromURLState struct {
	// deterministic-for-same-url
	inputURL string
	firstID  fingerprint.RepoID
	secondID fingerprint.RepoID

	// different-urls-diverge
	urlA string
	urlB string
	idA  fingerprint.RepoID
	idB  fingerprint.RepoID

	// empty-url-rejected
	emptyErr error
	emptyID  fingerprint.RepoID
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *repoIDFromURLState) theSameInputURL(url string) error {
	s.inputURL = url
	return nil
}

func (s *repoIDFromURLState) twoDifferentURLs(urlA, urlB string) error {
	s.urlA = urlA
	s.urlB = urlB
	return nil
}

func (s *repoIDFromURLState) anEmptyStringAsTheURL() error {
	// Nothing to set up — the empty string is the input.
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *repoIDFromURLState) repoIDFromURLIsCalledTwiceWithThatURL() error {
	var err error
	s.firstID, err = fingerprint.RepoIDFromURL(s.inputURL)
	if err != nil {
		return err
	}
	s.secondID, err = fingerprint.RepoIDFromURL(s.inputURL)
	if err != nil {
		return err
	}
	return nil
}

func (s *repoIDFromURLState) bothAreHashedWithRepoIDFromURL() error {
	var err error
	s.idA, err = fingerprint.RepoIDFromURL(s.urlA)
	if err != nil {
		return err
	}
	s.idB, err = fingerprint.RepoIDFromURL(s.urlB)
	if err != nil {
		return err
	}
	return nil
}

func (s *repoIDFromURLState) repoIDFromURLIsCalledWithTheEmptyString() error {
	s.emptyID, s.emptyErr = fingerprint.RepoIDFromURL("")
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *repoIDFromURLState) theReturnedRepoIDIsByteIdenticalAcrossBothCalls() error {
	if s.firstID != s.secondID {
		return fmt.Errorf(
			"RepoIDFromURL not deterministic:\n first  = %s\n second = %s",
			s.firstID.String(), s.secondID.String(),
		)
	}
	return nil
}

func (s *repoIDFromURLState) theReturnedRepoIDValuesDiffer() error {
	if s.idA == s.idB {
		return fmt.Errorf(
			"URLs produced the same RepoID:\n urlA=%s → %s\n urlB=%s → %s",
			s.urlA, s.idA.String(), s.urlB, s.idB.String(),
		)
	}
	return nil
}

func (s *repoIDFromURLState) aNonNilErrorIsReturned() error {
	if s.emptyErr == nil {
		return fmt.Errorf("expected non-nil error for empty URL, got nil")
	}
	return nil
}

func (s *repoIDFromURLState) theRepoIDIsTheZeroValue() error {
	if !s.emptyID.IsZero() {
		return fmt.Errorf(
			"expected zero RepoID for empty URL, got %s",
			s.emptyID.String(),
		)
	}
	return nil
}

// aSeparateChildProcessReturnsTheSameValue spawns a child `go run` process
// that independently computes RepoIDFromURL for the same URL and prints
// the result. This verifies cross-process determinism (the acceptance
// scenario requires "byte-identical across both calls and across processes").
func (s *repoIDFromURLState) aSeparateChildProcessReturnsTheSameValue() error {
	// Resolve module root so we can `go run` from it.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("runtime.Caller failed")
	}
	modRoot := filepath.Dir(thisFile)
	for i := 0; i < 3; i++ {
		modRoot = filepath.Dir(modRoot)
	}

	// Build a tiny Go program inline via -run flag on `go test`.
	// Instead, use `go run` with a small main written to a temp file.
	tmpDir, err := os.MkdirTemp("", "repoid-cross-process-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	mainSrc := fmt.Sprintf(`package main

import (
	"fmt"
	"os"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

func main() {
	id, err := fingerprint.RepoIDFromURL(%q)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %%v\n", err)
		os.Exit(1)
	}
	fmt.Print(id.String())
}
`, s.inputURL)

	mainPath := filepath.Join(tmpDir, "main.go")
	if err := os.WriteFile(mainPath, []byte(mainSrc), 0o644); err != nil {
		return fmt.Errorf("writing temp main.go: %w", err)
	}

	// Create a go.mod that replaces the module with the local copy.
	goModSrc := fmt.Sprintf(`module cross-process-check

go 1.25.0

require github.com/smartpcr/code-intelligence/services/agent-memory v0.0.0

replace github.com/smartpcr/code-intelligence/services/agent-memory => %s
`, filepath.ToSlash(modRoot))

	goModPath := filepath.Join(tmpDir, "go.mod")
	if err := os.WriteFile(goModPath, []byte(goModSrc), 0o644); err != nil {
		return fmt.Errorf("writing temp go.mod: %w", err)
	}

	// Tidy to populate go.sum with transitive deps from the replaced module.
	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = tmpDir
	if out, err := tidy.CombinedOutput(); err != nil {
		return fmt.Errorf("go mod tidy failed: %w\noutput: %s", err, string(out))
	}

	cmd := exec.Command("go", "run", "main.go")
	cmd.Dir = tmpDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("child process failed: %w\noutput: %s", err, string(out))
	}

	childResult := strings.TrimSpace(string(out))
	parentResult := s.firstID.String()
	if childResult != parentResult {
		return fmt.Errorf(
			"cross-process determinism failed:\n parent = %s\n child  = %s",
			parentResult, childResult,
		)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_identity_and_ancestry_refactor_repoidfromurl_helper(ctx *godog.ScenarioContext) {
	s := &repoIDFromURLState{}

	// Given
	ctx.Given(`^the same input URL "([^"]*)"$`, s.theSameInputURL)
	ctx.Given(`^two different URLs "([^"]*)" and "([^"]*)"$`, s.twoDifferentURLs)
	ctx.Given(`^an empty string as the URL$`, s.anEmptyStringAsTheURL)

	// When
	ctx.When(`^RepoIDFromURL is called twice with that URL$`, s.repoIDFromURLIsCalledTwiceWithThatURL)
	ctx.When(`^both are hashed with RepoIDFromURL$`, s.bothAreHashedWithRepoIDFromURL)
	ctx.When(`^RepoIDFromURL is called with the empty string$`, s.repoIDFromURLIsCalledWithTheEmptyString)

	// Then
	ctx.Then(`^the returned RepoID is byte-identical across both calls$`, s.theReturnedRepoIDIsByteIdenticalAcrossBothCalls)
	ctx.Then(`^a separate child process computing RepoIDFromURL for the same URL returns the same value$`, s.aSeparateChildProcessReturnsTheSameValue)
	ctx.Then(`^the returned RepoID values differ$`, s.theReturnedRepoIDValuesDiffer)
	ctx.Then(`^a non-nil error is returned$`, s.aNonNilErrorIsReturned)
	ctx.Then(`^the RepoID is the zero value$`, s.theRepoIDIsTheZeroValue)
}

func TestE2E_identity_and_ancestry_refactor_repoidfromurl_helper(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"identity_and_ancestry_refactor_repoidfromurl_helper.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_identity_and_ancestry_refactor_repoidfromurl_helper,
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