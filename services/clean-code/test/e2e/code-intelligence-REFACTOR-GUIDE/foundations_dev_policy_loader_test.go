//go:build e2e

// -----------------------------------------------------------------------
// <copyright file="foundations_dev_policy_loader_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Cucumber-godog binding for `foundations_dev_policy_loader.feature`.
//
// This file wires the .feature scenarios to Go step implementations so
// the Stage 1.1 build CI exercises the feature contract directly (no
// silently-orphaned BDD spec). The active dev loader still returns
// [devpolicy.ErrLoaderNotYetImplemented] (the Stage 1.4 YAML decoder
// + unsigned PolicyVersion synthesiser is owned by
// implementation-plan lines 90-100); scenarios that REQUIRE that
// Stage 1.4 body (rule counts, stable PolicyVersionID, filesystem
// override rule-id match) intentionally return [godog.ErrPending]
// from the Then step so the suite reports them as pending without
// failing the build. Scenarios that target Stage 1.1-shipped
// surfaces ([devpolicy.BannerText], [devpolicy.EmitBanner], and the
// [devpolicy.ErrLoaderNotYetImplemented] / [devpolicy.ErrMissingPolicyDir]
// sentinel layer) assert directly.
//
// Scenario 4 ("prod build refuses the dev-mode bypass at the loader
// layer") cannot run inside a non-prod test binary by definition --
// the build-tag matrix is enforced at COMPILE time. The prod-tag
// behaviour is exercised by `unsigned_prod_test.go` (`//go:build prod`)
// in the `internal/cli/devpolicy` package; the corresponding godog
// steps here return [godog.ErrPending] and cite that unit test as
// the canonical Stage 1.1 witness.

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cucumber/godog"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/policy/rulepacks"
)

// devPolicyLoaderState carries per-scenario state across the
// Given / When / Then steps. A fresh value is constructed in
// [InitializeScenario_foundations_dev_policy_loader] so
// scenarios stay independent.
type devPolicyLoaderState struct {
	src         devpolicy.LoaderSource
	bundle      devpolicy.Bundle
	loadErr     error
	tempDir     string
	customYAML  []byte
	bannerBuf   bytes.Buffer
	bannerWrote int
	bannerErr   error
}

func newDevPolicyLoaderState() *devPolicyLoaderState {
	return &devPolicyLoaderState{}
}

func (s *devPolicyLoaderState) cleanup() {
	if s.tempDir != "" {
		_ = os.RemoveAll(s.tempDir)
	}
}

// --- Given steps ---

// aBuildWithoutTheProdTag asserts the test binary is the
// !prod build. This file is compiled with `//go:build e2e`,
// which composes with the default tag set; the prod tag is
// mutually exclusive (see `unsigned_prod.go`). If a future
// composite build wired `-tags "e2e prod"` together this
// assertion would surface the mis-configuration here.
func (s *devPolicyLoaderState) aBuildWithoutTheProdTag() error {
	if isProdBuild() {
		return fmt.Errorf("scenario requires !prod build; current build is prod")
	}
	return nil
}

func (s *devPolicyLoaderState) aDevBuild() error {
	return s.aBuildWithoutTheProdTag()
}

func (s *devPolicyLoaderState) loaderSourceUseEmbeddedTrue() {
	s.src = devpolicy.LoaderSource{UseEmbedded: true}
}

func (s *devPolicyLoaderState) sameEmbeddedPackSetWalkedTwice() {
	s.src = devpolicy.LoaderSource{UseEmbedded: true}
}

func (s *devPolicyLoaderState) tempDirWithCustomYAML() error {
	dir, err := os.MkdirTemp("", "devpolicy-loader-feature-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	s.tempDir = dir
	s.customYAML = []byte(`# devpolicy loader feature fixture
pack_id: test.custom
pack_version: "0.0.1"
rules:
  - rule_id: test.custom.rule1
    title: "fixture rule one"
`)
	return os.WriteFile(filepath.Join(dir, "custom.yaml"), s.customYAML, 0o644)
}

func (s *devPolicyLoaderState) loaderSourceUseEmbeddedFalseDirPathSet() {
	s.src = devpolicy.LoaderSource{UseEmbedded: false, DirPath: s.tempDir}
}

// aBinaryBuiltWithProdTag is the Given for scenario 4. The
// scenario cannot execute inside the dev-tag test binary by
// definition (build-tag matrix is enforced at COMPILE time);
// the prod-tag behaviour is pinned by the prod-tag unit tests
// in `internal/cli/devpolicy/unsigned_prod_test.go`. Returning
// [godog.ErrPending] here marks the scenario as pending in the
// godog report without failing the suite.
func (s *devPolicyLoaderState) aBinaryBuiltWithProdTag() error {
	return godog.ErrPending
}

// --- When steps ---

func (s *devPolicyLoaderState) loaderLoadRuns() error {
	s.bundle, s.loadErr = devpolicy.NewLoader().Load(context.Background(), s.src)
	return nil
}

func (s *devPolicyLoaderState) loaderLoadRunsInDevBuild() error {
	return s.loaderLoadRuns()
}

func (s *devPolicyLoaderState) loaderLoadRunsAnySource() error {
	return s.loaderLoadRuns()
}

func (s *devPolicyLoaderState) synthesisePolicyVersionInvokedTwiceWithSameEffort() error {
	// Stage 1.1 dev loader does not synthesise a PolicyVersion;
	// it returns [devpolicy.ErrLoaderNotYetImplemented]. Stage 1.4
	// will fill in the synthesiser body behind the same interface.
	// For this stage we exercise the Load call twice so any
	// future implementation that introduced per-call state would
	// fail loudly here.
	_, errA := devpolicy.NewLoader().Load(context.Background(), s.src)
	_, errB := devpolicy.NewLoader().Load(context.Background(), s.src)
	if !errors.Is(errA, devpolicy.ErrLoaderNotYetImplemented) ||
		!errors.Is(errB, devpolicy.ErrLoaderNotYetImplemented) {
		return fmt.Errorf("expected ErrLoaderNotYetImplemented on both invocations; got (%v, %v)", errA, errB)
	}
	return nil
}

func (s *devPolicyLoaderState) emitBannerWritesToBuffer() {
	s.bannerBuf.Reset()
	s.bannerWrote, s.bannerErr = devpolicy.EmitBanner(&s.bannerBuf)
}

// --- Then steps ---

// bundleContainsAtLeastOneRulePerYAML covers scenario 1's
// final Then. Stage 1.1's dev loader returns
// [devpolicy.ErrLoaderNotYetImplemented]; the per-rule
// synthesis body lands in Stage 1.4 (implementation-plan
// items 97-102). The step asserts the closest Stage-1.1
// pinnable contract -- the Load call returned the
// not-yet-implemented sentinel AND the underlying embedded
// rulepacks contain at least one YAML in each family -- then
// returns [godog.ErrPending] to mark the scenario's full
// assertion as pending.
func (s *devPolicyLoaderState) bundleContainsAtLeastOneRulePerYAML() error {
	if !errors.Is(s.loadErr, devpolicy.ErrLoaderNotYetImplemented) {
		return fmt.Errorf("Stage 1.1 contract: expected ErrLoaderNotYetImplemented; got err=%v", s.loadErr)
	}
	if len(s.bundle.Rules) != 0 {
		return fmt.Errorf("Stage 1.1 contract: expected zero-value Bundle.Rules alongside the sentinel; got %d entries", len(s.bundle.Rules))
	}
	if err := embeddedRulepacksHaveBothFamilies(); err != nil {
		return err
	}
	return godog.ErrPending
}

func (s *devPolicyLoaderState) bundlePolicyVersionSignatureIsNil() error {
	if s.bundle.PolicyVersion.Signature != nil {
		return fmt.Errorf("Stage 1.1 contract: expected zero-value PolicyVersion.Signature alongside the sentinel; got non-nil")
	}
	return godog.ErrPending
}

func (s *devPolicyLoaderState) policyVersionIDIsByteForByteIdentical() error {
	// Stage 1.4 body lands the stable UUID-v5 synthesis. Mark
	// pending; the Stage 1.1 stub returns zero-value Bundle.
	return godog.ErrPending
}

func (s *devPolicyLoaderState) bundleRulesIsPermutationStableCopy() error {
	// Stage 1.4 body lands the deterministic walk order. Mark
	// pending.
	return godog.ErrPending
}

func (s *devPolicyLoaderState) bundleRulePacksLengthIs(expected int) error {
	if !errors.Is(s.loadErr, devpolicy.ErrLoaderNotYetImplemented) {
		return fmt.Errorf("Stage 1.1 contract: expected ErrLoaderNotYetImplemented for filesystem source; got err=%v", s.loadErr)
	}
	_ = expected // expected==1 in the feature; asserted in Stage 1.4
	return godog.ErrPending
}

func (s *devPolicyLoaderState) ruleIdsInBundleMatchCustomYAML() error {
	return godog.ErrPending
}

func (s *devPolicyLoaderState) returnedErrorIsErrDevModeUnavailable() error {
	// Scenario 4 cannot run in the !prod test binary. The
	// canonical Stage 1.1 witness is
	// `internal/cli/devpolicy/unsigned_prod_test.go`'s
	// `TestProdLoad_ReturnsErrDevModeUnavailable` (build tag
	// `prod`).
	return godog.ErrPending
}

func (s *devPolicyLoaderState) errorStringContainsImplPlanPhrase() error {
	// Asserts the literal pinned in implementation-plan Stage
	// 1.4 line 100. The constant string lives on the
	// [devpolicy.ErrDevModeUnavailable] sentinel and is
	// readable from this build, so we exercise it directly --
	// the prod-build invocation requirement is what flips this
	// step to pending.
	const want = "dev-mode policy bypass not available in prod build"
	if !strings.Contains(devpolicy.ErrDevModeUnavailable.Error(), want) {
		return fmt.Errorf("ErrDevModeUnavailable.Error() = %q; want it to contain %q",
			devpolicy.ErrDevModeUnavailable.Error(), want)
	}
	return godog.ErrPending
}

func (s *devPolicyLoaderState) returnedBundleHasEmptyRulesAndRulePacks() error {
	return godog.ErrPending
}

func (s *devPolicyLoaderState) bufferContentsExactlyEqualC10BannerStringPlusNewline() error {
	if s.bannerErr != nil {
		return fmt.Errorf("EmitBanner returned error: %w", s.bannerErr)
	}
	want := devpolicy.BannerText + "\n"
	if got := s.bannerBuf.String(); got != want {
		return fmt.Errorf("buffer contents = %q; want %q", got, want)
	}
	if s.bannerWrote != len(want) {
		return fmt.Errorf("EmitBanner reported %d bytes written; want %d", s.bannerWrote, len(want))
	}
	return nil
}

func (s *devPolicyLoaderState) bannerTextConstantEquals(expected string) error {
	if devpolicy.BannerText != expected {
		return fmt.Errorf("devpolicy.BannerText = %q; want %q (constraint C10)", devpolicy.BannerText, expected)
	}
	return nil
}

// --- helpers ---

// embeddedRulepacksHaveBothFamilies walks the binary-baked
// rulepack FS and returns an error if either of the two
// canonical family subdirectories is empty. Used by scenario
// 1's Stage-1.1 floor assertion.
func embeddedRulepacksHaveBothFamilies() error {
	var solidYAMLs, decouplingYAMLs int
	err := fs.WalkDir(rulepacks.EmbeddedFS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		switch {
		case strings.HasPrefix(path, "solid/"):
			solidYAMLs++
		case strings.HasPrefix(path, "decoupling/"):
			decouplingYAMLs++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("WalkDir(rulepacks.EmbeddedFS): %w", err)
	}
	if solidYAMLs == 0 {
		return fmt.Errorf("embedded rulepacks contain zero solid/*.yaml files (//go:embed pattern likely broken)")
	}
	if decouplingYAMLs == 0 {
		return fmt.Errorf("embedded rulepacks contain zero decoupling/*.yaml files (//go:embed pattern likely broken)")
	}
	return nil
}

// isProdBuild reports whether the current binary was compiled
// with the `prod` build tag. The dev loader's Load returns
// [devpolicy.ErrLoaderNotYetImplemented] in the !prod build
// and [devpolicy.ErrDevModeUnavailable] in the prod build;
// probing the sentinel is a tag-free way to detect the
// active build mode at runtime.
func isProdBuild() bool {
	_, err := devpolicy.NewLoader().Load(context.Background(), devpolicy.LoaderSource{UseEmbedded: true})
	return errors.Is(err, devpolicy.ErrDevModeUnavailable)
}

// InitializeScenario_foundations_dev_policy_loader registers
// every step the `.feature` scenarios reference. The
// numeric-capture step for "Bundle.RulePacks length is N"
// uses an `int` capture group to match godog's automatic
// parameter conversion.
func InitializeScenario_foundations_dev_policy_loader(ctx *godog.ScenarioContext) {
	s := newDevPolicyLoaderState()

	ctx.After(func(ctx2 context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		s.cleanup()
		return ctx2, nil
	})

	// Given
	ctx.Step(`^a build WITHOUT the prod tag$`, s.aBuildWithoutTheProdTag)
	ctx.Step(`^a dev build$`, s.aDevBuild)
	ctx.Step(`^the LoaderSource has UseEmbedded set to true$`, s.loaderSourceUseEmbeddedTrue)
	ctx.Step(`^the same embedded pack set walked twice in succession$`, s.sameEmbeddedPackSetWalkedTwice)
	ctx.Step(`^a temporary directory containing a custom\.yaml file that matches the embedded rulepack schema$`, s.tempDirWithCustomYAML)
	ctx.Step(`^the LoaderSource has UseEmbedded false and DirPath set to that temp directory$`, s.loaderSourceUseEmbeddedFalseDirPathSet)
	ctx.Step(`^a binary built with go build -tags prod of the devpolicy package$`, s.aBinaryBuiltWithProdTag)

	// When
	ctx.Step(`^devpolicy\.NewLoader\(\)\.Load runs against the embedded fs\.FS$`, s.loaderLoadRuns)
	ctx.Step(`^synthesisePolicyVersion is invoked both times with the same effort_model_version$`, s.synthesisePolicyVersionInvokedTwiceWithSameEffort)
	ctx.Step(`^devpolicy\.NewLoader\(\)\.Load runs in a dev build$`, s.loaderLoadRunsInDevBuild)
	ctx.Step(`^the test invokes devpolicy\.NewLoader\(\)\.Load with any LoaderSource$`, s.loaderLoadRunsAnySource)
	ctx.Step(`^devpolicy\.EmitBanner writes to a bytes\.Buffer$`, s.emitBannerWritesToBuffer)

	// Then
	ctx.Step(`^the returned Bundle\.Rules contains at least one rule for every YAML under services/clean-code/policy/rulepacks/\{solid,decoupling\}/$`, s.bundleContainsAtLeastOneRulePerYAML)
	ctx.Step(`^the returned Bundle\.PolicyVersion\.Signature is nil$`, s.bundlePolicyVersionSignatureIsNil)
	ctx.Step(`^the returned PolicyVersionID is byte-for-byte identical between the two runs$`, s.policyVersionIDIsByteForByteIdentical)
	ctx.Step(`^the second Bundle\.Rules slice is a permutation-stable copy of the first$`, s.bundleRulesIsPermutationStableCopy)
	ctx.Step(`^the returned Bundle\.RulePacks length is (\d+)$`, s.bundleRulePacksLengthIs)
	ctx.Step(`^the rule ids in Bundle\.Rules match the ids declared in custom\.yaml$`, s.ruleIdsInBundleMatchCustomYAML)
	ctx.Step(`^the returned error satisfies errors\.Is\(err, devpolicy\.ErrDevModeUnavailable\)$`, s.returnedErrorIsErrDevModeUnavailable)
	ctx.Step(`^the error string contains the literal phrase "([^"]*)"$`, func(_ string) error {
		return s.errorStringContainsImplPlanPhrase()
	})
	ctx.Step(`^the returned Bundle has empty Rules and RulePacks slices$`, s.returnedBundleHasEmptyRulesAndRulePacks)
	ctx.Step(`^the buffer contents exactly equal the C10 banner string followed by a single newline$`, s.bufferContentsExactlyEqualC10BannerStringPlusNewline)
	ctx.Step(`^the constant devpolicy\.BannerText equals "([^"]*)"$`, s.bannerTextConstantEquals)
}

// TestE2E_foundations_dev_policy_loader runs the cucumber
// scenarios declared in `foundations_dev_policy_loader.feature`
// against the Stage 1.1 dev-build loader. Scenarios that
// require Stage 1.4 (the YAML decoder body, stable
// PolicyVersionID synthesis, filesystem override rule
// matching) or the prod-tag build (refusal-at-loader-layer)
// resolve their Then steps to [godog.ErrPending] so godog
// reports them as pending. The Stage 1.1 banner and sentinel
// surfaces assert directly. The godog suite is configured
// with the default `Strict = false`, so pending scenarios do
// not fail the build.
func TestE2E_foundations_dev_policy_loader(t *testing.T) {
	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_foundations_dev_policy_loader,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"foundations_dev_policy_loader.feature"},
			TestingT: t,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run dev-policy-loader feature scenarios")
	}
}
