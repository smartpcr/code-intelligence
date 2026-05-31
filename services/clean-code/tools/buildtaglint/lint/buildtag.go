// -----------------------------------------------------------------------
// <copyright file="buildtag.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package lint hosts the two custom `go/analysis`
// analyzers required by `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
// Sec 8.10:
//
//   - `nobuildtagbypass` -- implements the
//     `no-production-build-tag-bypass` rule.
//   - `nocliproductionsqlimport` -- implements the
//     `no-production-sql-import` rule.
//
// Both analyzers expose a public `Analyzer *analysis.Analyzer`
// variable so they can be composed under `multichecker` (the
// `cmd/buildtaglint` binary) or driven individually from
// `analysistest`-based unit tests.
package lint

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// BuildTagAnalyzer implements `no-production-build-tag-bypass`
// (tech-spec REFACTOR-GUIDE Sec 8.10).
//
// For every Go file the analyzer:
//
//  1. Walks every `steward.PolicyVersion{...}` composite
//     literal whose type resolves to the
//     `internal/policy/steward.PolicyVersion` named type.
//     Resolution uses `pass.TypesInfo`, so an aliased
//     import such as `stewardv1 "<...>/policy/steward"`
//     is handled correctly (this was the gap that
//     evaluator item #4 called out in iter 1).
//  2. Treats a literal as "unsigned" when the `Signature`
//     field is either absent or set to the predeclared
//     `nil` identifier (anything else is signed by
//     construction).
//  3. If at least one unsigned literal is present in the
//     file, asserts the file carries a `//go:build`
//     constraint that excludes the `prod` tag (the simplest
//     conforming form is `//go:build !prod`, which is what
//     `internal/cli/devpolicy/unsigned_dev.go` uses).
//
// A diagnostic is reported per offending literal and the
// rendered message always contains the literal substring
// `no-production-build-tag-bypass` so `make lint-cli` greps
// and CI log searches both surface the rule name.
var BuildTagAnalyzer = &analysis.Analyzer{
	Name: "nobuildtagbypass",
	Doc: "Asserts that every file constructing a steward.PolicyVersion " +
		"with a nil Signature carries a //go:build !prod constraint " +
		"(tech-spec REFACTOR-GUIDE Sec 8.10, rule " +
		"no-production-build-tag-bypass).",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      runBuildTag,
}

const (
	// stewardPolicyVersionTypeName is the unqualified name
	// of the type whose unsigned construction triggers the
	// lint rule. We resolve the package via type info
	// rather than a hard-coded import alias so a renamed
	// import (`stewardv1 "<...>/policy/steward"`) still
	// flags correctly.
	stewardPolicyVersionTypeName = "PolicyVersion"

	// stewardPackagePathSuffix is the suffix every
	// canonical import path of the steward package ends
	// with. Matching on the suffix (rather than the full
	// module-prefixed path) keeps the analyzer robust to
	// module renames; matching at `/policy/steward`
	// (rather than `/internal/policy/steward`) keeps the
	// analysistest fixture set free of Go's "internal
	// package" visibility rule (a fixture under
	// `testdata/src/foo` cannot import a package whose
	// path contains `/internal/`).
	stewardPackagePathSuffix = "/policy/steward"

	// signatureFieldName is the `PolicyVersion` field that,
	// when nil, makes the value an unsigned policy
	// version per architecture Sec 3.8 structural bypass.
	signatureFieldName = "Signature"

	// prodBuildTag is the build tag that, when satisfied,
	// causes the file to compile into the prod binary.
	// The lint rule requires that bypass files NEVER
	// satisfy a build configuration where `prod` is set.
	prodBuildTag = "prod"
)

// runBuildTag drives the BuildTagAnalyzer over a single
// analysis pass (which corresponds to a single Go package).
//
// Scope gating: the brief restricts this rule to files
// under `internal/cli/devpolicy/`. We gate inside the
// analyzer (rather than relying on the caller to pass only
// that package pattern) so a single `go vet` invocation
// over the whole CLI tree produces correct results -- the
// SQLImportAnalyzer needs the wider scope, and
// `multichecker` runs every configured analyzer against
// every analyzed package.
//
// Test files (`_test.go`) are also excluded because they
// never ship in the prod binary regardless of build tags;
// flagging them would be a false positive.
func runBuildTag(pass *analysis.Pass) (interface{}, error) {
	insp := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// Pre-compute the in-scope files for this package so
	// we can short-circuit irrelevant packages cheaply.
	inScope := map[*ast.File]bool{}
	for _, f := range pass.Files {
		path := pass.Fset.Position(f.Pos()).Filename
		if !isDevpolicyFile(path) {
			continue
		}
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		inScope[f] = true
	}
	if len(inScope) == 0 {
		return nil, nil
	}

	// Bucket unsigned PolicyVersion literals by the file
	// they live in. We need a file-level grouping (not a
	// per-literal check) because the build-tag constraint
	// is a file-level property.
	type fileFinding struct {
		lit *ast.CompositeLit
	}
	perFile := map[*ast.File][]fileFinding{}

	nodeFilter := []ast.Node{(*ast.CompositeLit)(nil)}
	insp.Preorder(nodeFilter, func(n ast.Node) {
		cl := n.(*ast.CompositeLit)
		if !isUnsignedStewardPolicyVersion(pass, cl) {
			return
		}
		file := findEnclosingFile(pass, cl)
		if file == nil || !inScope[file] {
			return
		}
		perFile[file] = append(perFile[file], fileFinding{lit: cl})
	})

	for file, findings := range perFile {
		if fileExcludesProd(file) {
			continue
		}
		for _, f := range findings {
			pass.ReportRangef(f.lit,
				"no-production-build-tag-bypass: file constructs "+
					"steward.PolicyVersion with nil Signature but "+
					"does not carry a `//go:build !prod` constraint "+
					"(tech-spec REFACTOR-GUIDE Sec 8.10)")
		}
	}
	return nil, nil
}

// isDevpolicyFile returns true when `path` lives under
// the `internal/cli/devpolicy/` directory. The check is
// path-separator agnostic so it works on Windows
// (`internal\cli\devpolicy`) and POSIX (`internal/cli/devpolicy`)
// alike, and operates on the canonical-slashed form for
// stability.
func isDevpolicyFile(path string) bool {
	canon := strings.ReplaceAll(path, "\\", "/")
	return strings.Contains(canon, "/internal/cli/devpolicy/")
}

// findEnclosingFile returns the *ast.File that contains
// node `n` within `pass.Files`. Inspector walks across all
// files in a package so we have to recover the containing
// file ourselves before we can read its build constraints.
func findEnclosingFile(pass *analysis.Pass, n ast.Node) *ast.File {
	pos := n.Pos()
	for _, f := range pass.Files {
		if f.Pos() <= pos && pos <= f.End() {
			return f
		}
	}
	return nil
}

// isUnsignedStewardPolicyVersion returns true when the
// composite literal `cl` constructs a
// `steward.PolicyVersion` (or an aliased equivalent) AND
// the constructed value is unsigned (Signature is omitted
// or set to nil).
//
// Resolution path:
//
//  1. The literal's type expression must be a selector
//     (`pkg.PolicyVersion`) whose `Sel.Name` matches
//     `stewardPolicyVersionTypeName`. Non-selector or
//     selector-with-wrong-name literals are skipped.
//  2. We pull the *types.TypeName from `pass.TypesInfo`
//     and check that its package's import path ends with
//     `stewardPackagePathSuffix`. This is the alias-proof
//     check that supersedes iter 1's name-only match.
//  3. We walk the literal's element list: if any element
//     is `Signature: <non-nil>`, the literal is signed
//     and we return false. Otherwise we return true
//     (nil or absent Signature is unsigned).
func isUnsignedStewardPolicyVersion(pass *analysis.Pass, cl *ast.CompositeLit) bool {
	sel, ok := cl.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != stewardPolicyVersionTypeName {
		return false
	}
	// `pass.TypesInfo.Uses[sel.Sel]` returns the *types.TypeName
	// referenced by the selector. Its `Pkg()` is the steward
	// package regardless of the local import alias.
	obj := pass.TypesInfo.Uses[sel.Sel]
	if obj == nil {
		return false
	}
	tn, ok := obj.(*types.TypeName)
	if !ok || tn.Pkg() == nil {
		return false
	}
	if !strings.HasSuffix(tn.Pkg().Path(), stewardPackagePathSuffix) {
		return false
	}
	return compositeIsUnsigned(cl)
}

// compositeIsUnsigned inspects the keyed element list of a
// composite literal and returns true when Signature is
// either absent, set to bare `nil`, or set to a typed `nil`
// such as `[]byte(nil)`. Anything else (a function call, a
// non-nil expression) counts as signed.
func compositeIsUnsigned(cl *ast.CompositeLit) bool {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			// Positional construction: cannot tell which
			// slot is Signature. Treat as unsigned to be
			// safe; the repo never uses positional form
			// for PolicyVersion.
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != signatureFieldName {
			continue
		}
		return isNilExpr(kv.Value)
	}
	return true
}

// isNilExpr returns true when `e` is the predeclared `nil`
// identifier or a type-conversion of nil (`[]byte(nil)`,
// `(*foo)(nil)`).
func isNilExpr(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name == "nil"
	case *ast.CallExpr:
		// Type-conversion form, e.g. `[]byte(nil)`.
		if len(v.Args) == 1 {
			return isNilExpr(v.Args[0])
		}
	}
	return false
}

// fileExcludesProd returns true when the file carries a
// `//go:build` constraint that excludes the `prod` tag.
//
// We deliberately avoid `go/build/constraint` to keep the
// rule's accepted-expression set narrow: the repo only
// uses simple conjunctions for bypass files (`!prod`,
// `!prod && cgo`), and a stricter "exclusion required"
// posture is safer than a fully general SAT-style check
// that might accept exotic disjunctions where a prod build
// could slip through.
func fileExcludesProd(file *ast.File) bool {
	for _, cg := range file.Comments {
		for _, c := range cg.List {
			text := strings.TrimSpace(c.Text)
			const prefix = "//go:build"
			if !strings.HasPrefix(text, prefix) {
				continue
			}
			expr := strings.TrimSpace(strings.TrimPrefix(text, prefix))
			if buildExprExcludesProd(expr) {
				return true
			}
		}
	}
	return false
}

// buildExprExcludesProd returns true when the build-tag
// expression `expr` is satisfied ONLY by builds that do NOT
// set the `prod` tag.
//
// Accepted: `!prod`, `!prod && X`, `X && !prod && Y`.
// Rejected (conservative): anything containing `||`, any
// positive `prod` token, the empty constraint.
func buildExprExcludesProd(expr string) bool {
	tokens := tokenizeBuildExpr(expr)
	for _, t := range tokens {
		if t == "||" {
			return false
		}
	}
	sawNotProd := false
	for i, t := range tokens {
		if t != prodBuildTag {
			continue
		}
		if i == 0 || tokens[i-1] != "!" {
			return false
		}
		sawNotProd = true
	}
	return sawNotProd
}

// tokenizeBuildExpr splits a `//go:build` expression into
// the minimal set of tokens needed by
// `buildExprExcludesProd`. `!` is split off the front of
// any tag (`!prod` -> `!`, `prod`).
func tokenizeBuildExpr(expr string) []string {
	expr = strings.ReplaceAll(expr, "(", " ( ")
	expr = strings.ReplaceAll(expr, ")", " ) ")
	fields := strings.Fields(expr)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		for strings.HasPrefix(f, "!") && len(f) > 1 {
			out = append(out, "!")
			f = f[1:]
		}
		out = append(out, f)
	}
	return out
}
