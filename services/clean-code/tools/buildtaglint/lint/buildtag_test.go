// -----------------------------------------------------------------------
// <copyright file="buildtag_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package lint

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

// TestBuildTagAnalyzer_Fixtures drives BuildTagAnalyzer
// against the per-scenario packages under
// `testdata/src/buildtag_*`. Each fixture file is annotated
// with `// want "..."` comments where a diagnostic is
// expected; analysistest verifies an exact match.
//
// Scenarios:
//
//   - `buildtag_bad_missing_tag` -- constructs
//     `steward.PolicyVersion{Signature: nil}` without a
//     `//go:build` constraint. Expect: diagnostic.
//   - `buildtag_bad_aliased`     -- same construction but
//     via an aliased steward import (`stewardv1 "..."`).
//     Expect: diagnostic. This is the regression guard
//     for evaluator iter 1, item #4.
//   - `buildtag_good_not_prod`   -- carries `//go:build !prod`.
//     Expect: no diagnostic.
//   - `buildtag_good_signed`     -- file carries no build tag
//     but Signature is non-nil. Expect: no diagnostic.
//
// Note: the "positive `//go:build prod` constraint" branch
// is covered by `TestBuildExprExcludesProd("prod")` below
// rather than a fixture, because analysistest builds with
// the default tag set and cannot load a `//go:build prod`
// file at all (the file is excluded from the build).
func TestBuildTagAnalyzer_Fixtures(t *testing.T) {
	testdata := analysistest.TestData()
	cases := []string{
		"example.com/internal/cli/devpolicy/buildtag_bad_missing_tag",
		"example.com/internal/cli/devpolicy/buildtag_bad_aliased",
		"example.com/internal/cli/devpolicy/buildtag_good_not_prod",
		"example.com/internal/cli/devpolicy/buildtag_good_signed",
	}
	for _, pkg := range cases {
		pkg := pkg
		t.Run(pkg, func(t *testing.T) {
			analysistest.Run(t, testdata, BuildTagAnalyzer, pkg)
		})
	}
}

// TestBuildExprExcludesProd is a unit test for the
// build-tag predicate. The fixture-based tests above
// already cover end-to-end behaviour; this one pins the
// exact accepted/rejected expression set so a future tweak
// cannot silently widen the accepted set.
func TestBuildExprExcludesProd(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		{"!prod", true},
		{"!prod && !release", true},
		{"!release && !prod", true},
		{"cgo && !prod", true},
		{"!(prod)", false},
		{"prod", false},
		{"!release", false},
		{"", false},
		{"!prod || other", false},
		{"prod && !release", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.expr, func(t *testing.T) {
			got := buildExprExcludesProd(c.expr)
			if got != c.want {
				t.Fatalf("buildExprExcludesProd(%q) = %v; want %v",
					c.expr, got, c.want)
			}
		})
	}
}
