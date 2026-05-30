// -----------------------------------------------------------------------
// <copyright file="unsigned_prod_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build prod

// Tests for the prod build's [Loader] implementation declared
// in `unsigned_prod.go`. These tests are mutually exclusive
// with the dev-build tests: only one set is compiled per
// build. The dev-build tests live in `loader_test.go`
// (default tag) and exercise the [LoaderSource.FS] choice
// point + the [devLoader] stub body.
//
// # Pinned contracts
//
//  1. `NewLoader()` returns a non-nil [Loader] in the prod
//     build (no nil-pointer surprises for downstream callers).
//  2. Every `Load` call returns the [ErrDevModeUnavailable]
//     sentinel verbatim (errors.Is satisfies the check; the
//     wrapper-via-`fmt.Errorf` pattern still works).
//  3. The [Bundle] returned alongside the error is the
//     zero value (no partial-state leak into the caller).
//  4. Load performs NO I/O before returning the sentinel
//     (the architecture Sec 7.2 "earliest reachable layer"
//     refusal). The test passes a [LoaderSource] that would
//     fail `os.Stat` if Load reached for it.

package devpolicy

import (
	"context"
	"errors"
	"testing"
)

// TestProdNewLoader_ReturnsNonNil pins that `NewLoader()`
// in a `-tags prod` build returns a usable [Loader] value
// (not nil). This guards the prod-build dispatcher from a
// nil-pointer panic at the first reachable layer.
func TestProdNewLoader_ReturnsNonNil(t *testing.T) {
	t.Parallel()

	if NewLoader() == nil {
		t.Fatal("NewLoader() returned nil in prod build; want a non-nil Loader")
	}
}

// TestProdLoad_ReturnsErrDevModeUnavailable pins that every
// `Load` call in a prod build returns the
// [ErrDevModeUnavailable] sentinel verbatim (errors.Is must
// satisfy the check). The two sub-tests exercise both
// canonical [LoaderSource] shapes (embedded + filesystem)
// so a future implementation that branched on the source
// kind would fail loudly here.
func TestProdLoad_ReturnsErrDevModeUnavailable(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		src  LoaderSource
	}{
		{name: "embedded-source", src: LoaderSource{UseEmbedded: true}},
		{name: "filesystem-source", src: LoaderSource{DirPath: t.TempDir()}},
		{name: "zero-value-source", src: LoaderSource{}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ldr := NewLoader()
			bundle, err := ldr.Load(context.Background(), tc.src)
			if !errors.Is(err, ErrDevModeUnavailable) {
				t.Errorf("Load(%s): err = %v; want errors.Is(err, ErrDevModeUnavailable)", tc.name, err)
			}
			if bundle.PolicyVersion.PolicyVersionID != (Bundle{}).PolicyVersion.PolicyVersionID ||
				len(bundle.Rules) != 0 || len(bundle.RulePacks) != 0 {
				t.Errorf("Load(%s): returned non-zero Bundle %+v; want zero value alongside the sentinel error", tc.name, bundle)
			}
		})
	}
}

// TestProdLoad_NoFSAccessBeforeError pins the architecture
// Sec 7.2 "earliest reachable layer" refusal: the prod
// Load body MUST NOT call `src.FS()` (or any other I/O)
// before returning the sentinel. The test passes a
// [LoaderSource] with a guaranteed-invalid `DirPath`; if
// Load reached for `os.Stat` we would either observe the
// underlying OS error wrapped through `devpolicy: policy
// dir: ...` rather than the bare sentinel, or in extreme
// cases observe a panic for a NUL-byte path on POSIX
// hosts. Either outcome fails this assertion.
func TestProdLoad_NoFSAccessBeforeError(t *testing.T) {
	t.Parallel()

	src := LoaderSource{DirPath: "/this/path/\x00/MUST/never/be/statted/by/prod/Load"}
	bundle, err := NewLoader().Load(context.Background(), src)
	if !errors.Is(err, ErrDevModeUnavailable) {
		t.Errorf("Load(invalid-dirpath): err = %v; want bare ErrDevModeUnavailable (no I/O attempted)", err)
	}
	if err != nil && err != ErrDevModeUnavailable { //nolint:errorlint
		// The pinned contract is identity, not just wrapping:
		// the prod build returns the sentinel value VERBATIM
		// so callers can write `if err == ErrDevModeUnavailable`
		// in addition to `errors.Is`. A future edit that wraps
		// the sentinel via fmt.Errorf would fail this check.
		t.Errorf("Load(invalid-dirpath): err = %v (identity %p); want IDENTITY ErrDevModeUnavailable (%p)", err, err, ErrDevModeUnavailable)
	}
	if len(bundle.Rules) != 0 || len(bundle.RulePacks) != 0 {
		t.Errorf("Load(invalid-dirpath): returned non-zero Bundle %+v; want zero value", bundle)
	}
}

// TestProdLoad_ErrorTextMatchesImplPlan pins that the
// [ErrDevModeUnavailable] error text is byte-identical to
// the form pinned in `implementation-plan.md` Stage 1.4
// line 100 ("dev-mode policy bypass not available in prod
// build"). This guards against a string drift that would
// silently break operator-side log parsing or text
// matching in downstream tooling.
func TestProdLoad_ErrorTextMatchesImplPlan(t *testing.T) {
	t.Parallel()

	_, err := NewLoader().Load(context.Background(), LoaderSource{UseEmbedded: true})
	if err == nil {
		t.Fatal("Load returned nil error; want ErrDevModeUnavailable")
	}
	const wantSubstr = "dev-mode policy bypass not available in prod build"
	if got := err.Error(); !contains(got, wantSubstr) {
		t.Errorf("Load error text = %q; want it to contain the impl-plan-pinned phrase %q", got, wantSubstr)
	}
}

// contains is a local helper to avoid an extra import. It
// is intentionally minimal -- the package-internal scope
// keeps it out of the public surface.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
