// -----------------------------------------------------------------------
// <copyright file="load_unsigned_bundle_prod_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

//go:build prod

// This test is the prod-gated proof the acceptance scenario
// (implementation-plan.md Stage 3.4) requires: it calls
// LoadUnsignedBundle(...) and asserts the returned error
// equals devpolicy.ErrDevModeUnavailable with the exact
// message "dev-mode policy bypass not available in prod build".

package devpolicy

import (
	"context"
	"testing"
)

// TestLoadUnsignedBundle_ProdReturnsErrDevModeUnavailable calls
// LoadUnsignedBundle and asserts the returned error is IDENTICAL
// (not merely errors.Is-compatible) to ErrDevModeUnavailable,
// and that the error message exactly matches the impl-plan-pinned
// text.
func TestLoadUnsignedBundle_ProdReturnsErrDevModeUnavailable(t *testing.T) {
	t.Parallel()

	bundle, err := LoadUnsignedBundle(context.Background(), LoaderSource{UseEmbedded: true})
	if err == nil {
		t.Fatal("LoadUnsignedBundle returned nil error; want ErrDevModeUnavailable")
	}

	// Identity check: the prod build must return the sentinel
	// value VERBATIM, not a wrapped copy.
	if err != ErrDevModeUnavailable { //nolint:errorlint
		t.Fatalf("LoadUnsignedBundle error = %v (identity %p); want IDENTITY ErrDevModeUnavailable (%p)",
			err, err, ErrDevModeUnavailable)
	}

	// Exact message check (not substring): the error text
	// must equal the full sentinel string.
	const wantMsg = "devpolicy: dev-mode policy bypass not available in prod build"
	if got := err.Error(); got != wantMsg {
		t.Errorf("LoadUnsignedBundle error message = %q; want exact %q", got, wantMsg)
	}

	// Zero-value bundle check: no partial state leak.
	if len(bundle.Rules) != 0 || len(bundle.RulePacks) != 0 {
		t.Errorf("LoadUnsignedBundle returned non-zero Bundle %+v; want zero value", bundle)
	}
}

// TestProdBuildExcludesDevBypass is the build-tag-gated guarantee
// pinned by `implementation-plan.md` Stage 5.1 (Hardening &
// Release / Build Tag Matrix) and `e2e-scenarios.md` "Prod build
// excludes bypass via unit test". It is invoked by the
// `build-prod` job in `.github/workflows/clean-code-ci.yml` via
// `go test -tags prod -run TestProdBuildExcludesDevBypass
// ./internal/cli/devpolicy/...` and asserts:
//
//  1. The prod build's `LoadUnsignedBundle` returns the
//     `ErrDevModeUnavailable` sentinel for every canonical
//     [LoaderSource] shape (embedded, filesystem, zero-value).
//  2. The error text contains the impl-plan-pinned phrase
//     `dev-mode policy bypass not available in prod build`
//     (Stage 1.4 line 100; Stage 5.1 acceptance scenario).
//  3. The returned [Bundle] is zero-valued -- no partial dev-
//     mode bypass state leaks into the caller.
//  4. The sentinel returns BEFORE any I/O so a misconfigured
//     `--policy <dir>` cannot trick the prod loader into
//     stat-ing the filesystem (architecture Sec 7.2 "earliest
//     reachable layer" refusal).
//
// The guarantee lives in a build-tag-gated UNIT TEST, NOT in a
// hidden CLI subcommand, because `architecture.md` Sec 3.6 and
// `tech-spec.md` Sec 4.1 pin the CLI surface to four
// subcommands only (`analyze`, `report`, `version`, `policy`).
// Smuggling a `cleanc verify-prod-build`-style helper would
// breach that contract; the build-tag gate `//go:build prod`
// keeps the assertion compile-time-fused to the prod fleet.
func TestProdBuildExcludesDevBypass(t *testing.T) {
	t.Parallel()

	const wantSubstr = "dev-mode policy bypass not available in prod build"

	cases := []struct {
		name string
		src  LoaderSource
	}{
		{name: "embedded-source", src: LoaderSource{UseEmbedded: true}},
		{name: "filesystem-source", src: LoaderSource{DirPath: t.TempDir()}},
		{name: "zero-value-source", src: LoaderSource{}},
		{
			// A NUL-byte-containing path would panic `os.Stat`
			// on POSIX-shaped roots; the prod loader MUST
			// short-circuit BEFORE reaching for any I/O, so
			// this case proves the no-I/O contract.
			name: "invalid-dirpath-no-io",
			src:  LoaderSource{DirPath: "/this/path/\x00/MUST/never/be/statted/by/prod/Load"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bundle, err := LoadUnsignedBundle(context.Background(), tc.src)
			if err == nil {
				t.Fatalf("LoadUnsignedBundle(%s) returned nil error; want ErrDevModeUnavailable", tc.name)
			}
			// Identity check: the sentinel must come back VERBATIM
			// (callers should be free to write `err ==
			// ErrDevModeUnavailable` in addition to `errors.Is`).
			if err != ErrDevModeUnavailable { //nolint:errorlint
				t.Fatalf("LoadUnsignedBundle(%s) err = %v (identity %p); want IDENTITY ErrDevModeUnavailable (%p)",
					tc.name, err, err, ErrDevModeUnavailable)
			}
			if got := err.Error(); !contains(got, wantSubstr) {
				t.Errorf("LoadUnsignedBundle(%s) error text = %q; want it to contain %q",
					tc.name, got, wantSubstr)
			}
			if len(bundle.Rules) != 0 || len(bundle.RulePacks) != 0 || len(bundle.Thresholds) != 0 {
				t.Errorf("LoadUnsignedBundle(%s) returned non-zero Bundle %+v; want zero value (no partial bypass leak)",
					tc.name, bundle)
			}
		})
	}
}

// NOTE: a `contains` helper already lives in unsigned_prod_test.go
// (same package, same `//go:build prod` tag). Reuse it here instead
// of declaring a duplicate -- Go would reject a second declaration
// at compile time and a renamed copy would just be dead weight.
