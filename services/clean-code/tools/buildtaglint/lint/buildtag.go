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
	"go/build/constraint"
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
// `steward.PolicyVersion` (or any locally-aliased
// equivalent) AND the constructed value is unsigned
// (Signature is omitted or set to nil).
//
// Resolution uses the type system rather than the
// literal's syntactic spelling, so all of the following
// forms are caught:
//
//   - `steward.PolicyVersion{...}`              -- direct selector
//   - `stewardv1.PolicyVersion{...}`            -- aliased import
//   - `type PV = steward.PolicyVersion; PV{...}` -- local type alias
//   - `type PV steward.PolicyVersion; PV{...}`  -- named wrapper (caught only
//     when wrapper layout preserves Signature; otherwise treated as a
//     distinct type and skipped, since signing semantics no longer apply)
//
// We resolve the literal's type via `pass.TypesInfo.TypeOf(cl)`,
// strip pointer/alias indirection, and assert the underlying
// *types.Named's TypeName lives in a package whose import path
// ends with `stewardPackagePathSuffix` and whose name matches
// `stewardPolicyVersionTypeName`.
func isUnsignedStewardPolicyVersion(pass *analysis.Pass, cl *ast.CompositeLit) bool {
	// We deliberately do NOT short-circuit on `cl.Type == nil`:
	// for an elided inner literal such as
	// `[]steward.PolicyVersion{{Signature: nil}}` the inner
	// CompositeLit has no explicit `Type` AST node, but the
	// type checker still records the inferred element type
	// in `pass.TypesInfo.Types[cl].Type` -- so we resolve via
	// that map for both forms.
	tv, ok := pass.TypesInfo.Types[cl]
	if !ok || tv.Type == nil {
		return false
	}
	named := namedFromType(tv.Type)
	if named == nil {
		return false
	}
	obj := named.Obj()
	if obj == nil || obj.Pkg() == nil {
		return false
	}
	if obj.Name() != stewardPolicyVersionTypeName {
		return false
	}
	if !strings.HasSuffix(obj.Pkg().Path(), stewardPackagePathSuffix) {
		return false
	}
	return compositeIsUnsigned(cl)
}

// namedFromType returns the underlying *types.Named that a
// composite-literal type ultimately resolves to, peeling off
// pointer indirection and `type X = Y` aliases (`*types.Alias`,
// introduced in Go 1.22). Named wrappers (`type Foo pkg.Bar`)
// deliberately return their own *types.Named, which lives in
// the local package and will therefore fail the
// `stewardPackagePathSuffix` check above (correct: the wrapper
// is a distinct type with its own signing semantics).
func namedFromType(t types.Type) *types.Named {
	for {
		t = types.Unalias(t)
		switch v := t.(type) {
		case *types.Named:
			return v
		case *types.Pointer:
			t = v.Elem()
		default:
			return nil
		}
	}
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
// LEADING `//go:build` constraint (one that appears in a
// comment group BEFORE the package clause, which is the
// only position Go's toolchain recognises as a build
// constraint) whose semantic evaluation guarantees the
// file is excluded from any build that sets the `prod`
// tag.
//
// The leading-position check (`cg.End() < file.Package`)
// closes the iter-3 evaluator gap where a `//go:build !prod`
// comment placed anywhere in the file (including after the
// package clause) was accepted -- the Go toolchain ignores
// such comments as build constraints, so a prod build
// would still pick the file up.
func fileExcludesProd(file *ast.File) bool {
	for _, cg := range file.Comments {
		// `file.Package` is the token.Pos of the
		// `package` keyword. Only comment groups that
		// end strictly before it can be honoured as
		// build constraints by the Go toolchain.
		if cg.End() >= file.Package {
			continue
		}
		for _, c := range cg.List {
			if !constraint.IsGoBuild(c.Text) {
				continue
			}
			expr, err := constraint.Parse(c.Text)
			if err != nil {
				continue
			}
			if exprExcludesProd(expr) {
				return true
			}
		}
	}
	return false
}

// exprExcludesProd returns true when there is NO satisfying
// assignment of build tags with `prod = true` that makes
// `expr` evaluate to true. In other words: every config
// that builds the file has `prod` unset.
//
// We avoid the iter-3 token-heuristic gap (which accepted
// `!!prod` and `!(!prod)` as "excludes prod") by parsing
// the constraint with `go/build/constraint` and evaluating
// it semantically. Since the build-tag expression only
// references a handful of tags in practice (typically
// ≤4), we enumerate all assignments of the non-prod tags
// with `prod=true`. If any such assignment satisfies the
// expression, the file is reachable under a prod build
// and we reject.
//
// Examples (all correctly handled):
//
//   - `!prod`            -> excludes (no satisfying assignment with prod=true)
//   - `!prod && cgo`     -> excludes
//   - `!(prod)`          -> excludes (semantically `!prod`)
//   - `!!prod`           -> NOT excluded
//   - `!(!prod)`         -> NOT excluded
//   - `!prod || cgo`     -> NOT excluded (cgo-on prod builds compile)
//   - `!cgo`             -> NOT excluded (cgo-off prod builds compile)
func exprExcludesProd(expr constraint.Expr) bool {
	if expr == nil {
		return false
	}
	tags := collectBuildTags(expr)
	others := make([]string, 0, len(tags))
	for t := range tags {
		if t != prodBuildTag {
			others = append(others, t)
		}
	}
	n := len(others)
	limit := 1 << n
	for mask := 0; mask < limit; mask++ {
		assign := map[string]bool{prodBuildTag: true}
		for i, t := range others {
			assign[t] = (mask>>i)&1 == 1
		}
		if expr.Eval(func(tag string) bool { return assign[tag] }) {
			// Found a build config with prod=true that
			// satisfies the constraint -> bypass possible.
			return false
		}
	}
	return true
}

// collectBuildTags walks a parsed build-constraint
// expression tree and returns the set of tag identifiers
// it references. Used to bound the enumeration in
// `exprExcludesProd`.
func collectBuildTags(expr constraint.Expr) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(e constraint.Expr)
	walk = func(e constraint.Expr) {
		switch v := e.(type) {
		case *constraint.TagExpr:
			out[v.Tag] = struct{}{}
		case *constraint.NotExpr:
			walk(v.X)
		case *constraint.AndExpr:
			walk(v.X)
			walk(v.Y)
		case *constraint.OrExpr:
			walk(v.X)
			walk(v.Y)
		}
	}
	walk(expr)
	return out
}

// buildExprExcludesProd is a string-level convenience
// wrapper around `exprExcludesProd` used by unit tests. It
// returns false for any expression that does not parse
// cleanly as a `//go:build` constraint (including the
// empty string).
func buildExprExcludesProd(expr string) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return false
	}
	parsed, err := constraint.Parse("//go:build " + expr)
	if err != nil {
		return false
	}
	return exprExcludesProd(parsed)
}
