//go:build e2e

package e2e

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cucumber/godog"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
)

// ---------------------------------------------------------------------------
// Scenario state
// ---------------------------------------------------------------------------

type canonicalSigState struct {
	repoURL string
	relPath string

	// Computed results cached after "When" step.
	repoSig    string
	pkgDir     string
	pkgSig     string
	fileSig    string
	pkgDirRan  bool
}

// ---------------------------------------------------------------------------
// Given steps
// ---------------------------------------------------------------------------

func (s *canonicalSigState) aFixedRepoURLAndRelPath(repoURL, relPath string) error {
	s.repoURL = repoURL
	s.relPath = relPath
	return nil
}

// ---------------------------------------------------------------------------
// When steps
// ---------------------------------------------------------------------------

func (s *canonicalSigState) theExportedHelpersRun() error {
	s.repoSig = repoindexer.CanonicalRepoSig(s.repoURL)
	s.pkgDir = repoindexer.CanonicalPackageDir(s.relPath)
	s.pkgSig = repoindexer.CanonicalPackageSig(s.repoURL, s.pkgDir)
	s.fileSig = repoindexer.CanonicalFileSig(s.repoURL, s.relPath)
	s.pkgDirRan = true
	return nil
}

func (s *canonicalSigState) canonicalPackageDirRuns() error {
	s.pkgDir = repoindexer.CanonicalPackageDir(s.relPath)
	s.pkgDirRan = true
	return nil
}

// ---------------------------------------------------------------------------
// Then steps
// ---------------------------------------------------------------------------

func (s *canonicalSigState) canonicalRepoSigReturns(want string) error {
	if s.repoSig != want {
		return fmt.Errorf("CanonicalRepoSig(%q) = %q, want %q", s.repoURL, s.repoSig, want)
	}
	return nil
}

func (s *canonicalSigState) canonicalPackageDirReturns(want string) error {
	if !s.pkgDirRan {
		return fmt.Errorf("CanonicalPackageDir was not invoked")
	}
	if s.pkgDir != want {
		return fmt.Errorf("CanonicalPackageDir(%q) = %q, want %q", s.relPath, s.pkgDir, want)
	}
	return nil
}

func (s *canonicalSigState) canonicalPackageSigReturns(want string) error {
	if s.pkgSig != want {
		return fmt.Errorf("CanonicalPackageSig(%q, %q) = %q, want %q", s.repoURL, s.pkgDir, s.pkgSig, want)
	}
	return nil
}

func (s *canonicalSigState) canonicalFileSigReturns(want string) error {
	if s.fileSig != want {
		return fmt.Errorf("CanonicalFileSig(%q, %q) = %q, want %q", s.repoURL, s.relPath, s.fileSig, want)
	}
	return nil
}

func (s *canonicalSigState) itReturnsEmptyStringRepoRoot() error {
	if s.pkgDir != "" {
		return fmt.Errorf("CanonicalPackageDir(%q) = %q, want \"\" (repo root)", s.relPath, s.pkgDir)
	}
	return nil
}

func (s *canonicalSigState) itReturnsWithForwardSlashSeparators(want string) error {
	if s.pkgDir != want {
		return fmt.Errorf("CanonicalPackageDir(%q) = %q, want %q", s.relPath, s.pkgDir, want)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Godog wiring
// ---------------------------------------------------------------------------

func InitializeScenario_identity_and_ancestry_refactor_promote_canonical_signature_helpers(ctx *godog.ScenarioContext) {
	s := &canonicalSigState{}

	// Given
	ctx.Given(`^a fixed repoURL "([^"]*)" and relPath "([^"]*)"$`, s.aFixedRepoURLAndRelPath)

	// When
	ctx.When(`^the exported helpers run$`, s.theExportedHelpersRun)
	ctx.When(`^CanonicalPackageDir runs$`, s.canonicalPackageDirRuns)

	// Then
	ctx.Then(`^CanonicalRepoSig returns "([^"]*)"$`, s.canonicalRepoSigReturns)
	ctx.Then(`^CanonicalPackageDir returns "([^"]*)"$`, s.canonicalPackageDirReturns)
	ctx.Then(`^CanonicalPackageSig returns "([^"]*)"$`, s.canonicalPackageSigReturns)
	ctx.Then(`^CanonicalFileSig returns "([^"]*)"$`, s.canonicalFileSigReturns)
	ctx.Then(`^it returns "" \(empty string == repo root\)$`, s.itReturnsEmptyStringRepoRoot)
	ctx.Then(`^it returns "([^"]*)" with forward-slash separators$`, s.itReturnsWithForwardSlashSeparators)
}

func TestE2E_identity_and_ancestry_refactor_promote_canonical_signature_helpers(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	featurePath := filepath.Join(filepath.Dir(thisFile),
		"identity_and_ancestry_refactor_promote_canonical_signature_helpers.feature")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_identity_and_ancestry_refactor_promote_canonical_signature_helpers,
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