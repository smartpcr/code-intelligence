// -----------------------------------------------------------------------
// <copyright file="buildtag_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package lint

import (
	"go/parser"
	"go/token"
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
		"example.com/internal/cli/devpolicy/buildtag_bad_local_alias",
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
//
// Because the predicate is now parser- and Eval-based
// (`go/build/constraint`) rather than a token heuristic,
// it correctly handles bypass forms that iter 3's
// evaluator called out as security gaps: `!!prod` and
// `!(!prod)` are rejected, while semantically-equivalent
// `!(prod)` is accepted.
func TestBuildExprExcludesProd(t *testing.T) {
	cases := []struct {
		expr string
		want bool
	}{
		// Excludes prod -- accepted.
		{"!prod", true},
		{"!prod && !release", true},
		{"!release && !prod", true},
		{"cgo && !prod", true},
		{"!(prod)", true}, // semantically !prod

		// Does NOT exclude prod -- rejected.
		{"prod", false},
		{"!release", false},
		{"", false},
		{"!prod || other", false}, // other-on prod builds compile
		{"prod && !release", false},
		{"!cgo", false}, // cgo-off prod builds compile

		// Iter-3 bypass-form regression guards (the
		// reason the token heuristic was replaced).
		{"!!prod", false},
		{"!(!prod)", false},
		{"!(!(!prod))", true}, // ===  !prod
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

// TestFileExcludesProd_LeadingConstraintRequired pins the
// leading-position requirement called out by iter-3
// evaluator item #1: a `//go:build !prod` comment placed
// AFTER the package clause is ignored by the Go toolchain
// and therefore must NOT count as excluding prod. We
// parse synthetic source strings directly because `gofmt`
// auto-rewrites misplaced build directives to the top of
// the file, which makes a static `testdata/src/...`
// fixture impossible to maintain.
func TestFileExcludesProd_LeadingConstraintRequired(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{
			name: "leading !prod -> excludes",
			src: "//go:build !prod\n\n" +
				"package x\n",
			want: true,
		},
		{
			name: "no constraint -> not excluded",
			src:  "package x\n",
			want: false,
		},
		{
			name: "directive after package clause -> not excluded",
			src: "package x\n\n" +
				"//go:build !prod\n",
			want: false,
		},
		{
			name: "directive inside function body -> not excluded",
			src: "package x\n\n" +
				"func F() {\n//go:build !prod\n}\n",
			want: false,
		},
		{
			name: "bypass form !!prod is leading but does not exclude",
			src: "//go:build !!prod\n\n" +
				"package x\n",
			want: false,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, "x.go", c.src, parser.ParseComments)
			if err != nil {
				t.Fatalf("parse error: %v", err)
			}
			got := fileExcludesProd(f)
			if got != c.want {
				t.Fatalf("fileExcludesProd(%q) = %v; want %v",
					c.src, got, c.want)
			}
		})
	}
}
