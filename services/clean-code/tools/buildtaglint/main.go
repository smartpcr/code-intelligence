// -----------------------------------------------------------------------
// <copyright file="main.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Command buildtaglint enforces the `no-production-build-tag-bypass`
// custom lint rule documented in `docs/stories/code-intelligence-REFACTOR-GUIDE/tech-spec.md`
// Sec 8.10.
//
// # What it checks
//
// For every `*.go` file under each directory passed on the
// command line (default: `./internal/cli/devpolicy`), this
// tool:
//
//  1. Parses the file with `go/parser` (in `ParseComments` mode).
//  2. Walks every composite literal of the form
//     `steward.PolicyVersion{ ... }`.
//  3. If at least one such literal is found AND any of those
//     literals omits the `Signature` field or sets it to a
//     literal `nil`, the file's `//go:build` constraints are
//     inspected.
//  4. The file MUST carry a `//go:build` constraint whose set
//     of allowed tag-combinations EXCLUDES `prod`. The simplest
//     conforming form is `//go:build !prod` (which is what the
//     repo standardises on for dev-only files such as
//     `unsigned_dev.go`). A `//go:build prod` constraint, or
//     the absence of any build constraint, is a violation.
//
// Violations are written to stderr in
// `file:line:col: message` form and the process exits with
// status 1. Clean runs exit 0 silently (or with a single-line
// "buildtaglint: clean" summary in verbose mode) so that `make
// lint` integration follows the standard "no output on success"
// convention.
//
// # Why this exists
//
// The `internal/cli/devpolicy` package synthesises an unsigned
// `steward.PolicyVersion` (no cryptographic signature) so a
// developer can exercise the CLI without standing up the
// Steward signing service. Per architecture Sec 1.6
// ("policy-signing-required") and tech-spec Sec 8.9 ("Build-tag
// matrix"), that bypass MUST be excluded at compile time from
// the production binary. The `//go:build !prod` constraint on
// every bypass file is the structural enforcement; this linter
// guards against the regression where someone adds a new
// bypass file and forgets the build tag.
//
// # Limitations
//
// The check is a deliberately conservative syntactic walk; it
// does not perform full type-checking. A `PolicyVersion{...}`
// composite literal qualified by a `steward` package selector
// is the construction shape used throughout the repo (see
// `internal/cli/devpolicy/unsigned_dev.go:226`); pure-variable
// constructions such as `var pv steward.PolicyVersion` are
// out of scope (they are vacuously unsigned and behave the
// same way under both build tags). The lint check trades
// completeness for zero false positives and zero third-party
// dependencies.
//
// # Usage
//
//	go run ./tools/buildtaglint ./internal/cli/devpolicy
//
// The Makefile target `lint-cli` (invoked by `make lint`)
// wires this tool into the standard developer workflow.
package main

import (
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// stewardPackageSelector is the import-alias under which the
// `steward.PolicyVersion` type is referenced throughout
// `internal/cli/devpolicy`. The repo never re-aliases the
// `steward` import (verified by grep), so a literal selector
// match against this string is sufficient.
const stewardPackageSelector = "steward"

// policyVersionTypeName is the type-name half of the
// `steward.PolicyVersion` selector. Together they form the
// composite-literal shape this linter scans for.
const policyVersionTypeName = "PolicyVersion"

// signatureFieldName is the `PolicyVersion` field that, when
// nil, makes the constructed value an "unsigned" policy
// version per architecture Sec 3.8 structural bypass.
const signatureFieldName = "Signature"

// prodBuildTag is the build tag whose presence in a positive
// constraint (or absence in a negative constraint) signals
// that the file would compile into the prod binary. The lint
// rule rejects either case for files that construct unsigned
// PolicyVersions.
const prodBuildTag = "prod"

// finding describes one violation. The lint tool batches all
// findings for a single run so the operator sees the full set
// in one invocation rather than fixing-then-re-running.
type finding struct {
	path    string
	pos     token.Position
	message string
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d:%d: %s", f.path, f.pos.Line, f.pos.Column, f.message)
}

func main() {
	verbose := flag.Bool("v", false, "print a per-file trace and a clean-run summary")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr,
			"buildtaglint: enforce the no-production-build-tag-bypass lint rule\n"+
				"\n"+
				"Usage:\n"+
				"  buildtaglint [-v] [path ...]\n"+
				"\n"+
				"Each path is walked recursively for *.go files. With no\n"+
				"paths, ./internal/cli/devpolicy is used.\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	paths := flag.Args()
	if len(paths) == 0 {
		paths = []string{filepath.Join("internal", "cli", "devpolicy")}
	}

	var findings []finding
	var scanned int
	for _, root := range paths {
		res, err := scan(root, *verbose)
		if err != nil {
			fmt.Fprintf(os.Stderr, "buildtaglint: %v\n", err)
			os.Exit(2)
		}
		scanned += res.scanned
		findings = append(findings, res.findings...)
	}

	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintln(os.Stderr, f.String())
		}
		fmt.Fprintf(os.Stderr,
			"buildtaglint: %d violation(s) of no-production-build-tag-bypass (tech-spec Sec 8.10)\n",
			len(findings))
		os.Exit(1)
	}
	if *verbose {
		fmt.Fprintf(os.Stderr, "buildtaglint: clean (%d file(s) scanned)\n", scanned)
	}
}

// scanResult collects per-root totals so the summary line in
// `-v` mode can report file counts accurately even when the
// caller passes multiple roots.
type scanResult struct {
	scanned  int
	findings []finding
}

// scan walks root and lints every Go file (excluding `_test.go`
// files, which never ship in the prod binary anyway).
func scan(root string, verbose bool) (scanResult, error) {
	var out scanResult
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		out.scanned++
		findings, err := lintFile(path, verbose)
		if err != nil {
			return fmt.Errorf("lintFile %s: %w", path, err)
		}
		out.findings = append(out.findings, findings...)
		return nil
	})
	if walkErr != nil {
		return out, walkErr
	}
	return out, nil
}

// lintFile parses one Go file and returns any findings.
func lintFile(path string, verbose bool) ([]finding, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	unsignedLits := findUnsignedPolicyVersionLiterals(file)
	if verbose {
		fmt.Fprintf(os.Stderr, "buildtaglint: %s: %d unsigned PolicyVersion literal(s)\n",
			path, len(unsignedLits))
	}
	if len(unsignedLits) == 0 {
		return nil, nil
	}

	if hasNotProdBuildTag(file) {
		return nil, nil
	}

	out := make([]finding, 0, len(unsignedLits))
	for _, lit := range unsignedLits {
		out = append(out, finding{
			path: path,
			pos:  fset.Position(lit.Pos()),
			message: "no-production-build-tag-bypass: file constructs " +
				"steward.PolicyVersion with nil Signature but does not " +
				"carry a `//go:build !prod` constraint (tech-spec Sec 8.10)",
		})
	}
	return out, nil
}

// findUnsignedPolicyVersionLiterals walks the file AST and
// returns every `steward.PolicyVersion{...}` composite literal
// whose `Signature` field is either absent or explicitly set
// to `nil`. Literals that set `Signature` to a non-nil value
// (e.g. a function call returning a `[]byte`) are excluded:
// they are signed by construction and not bypass candidates.
func findUnsignedPolicyVersionLiterals(file *ast.File) []*ast.CompositeLit {
	var out []*ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		cl, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := cl.Type.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if ident.Name != stewardPackageSelector || sel.Sel.Name != policyVersionTypeName {
			return true
		}
		if isUnsignedComposite(cl) {
			out = append(out, cl)
		}
		return true
	})
	return out
}

// isUnsignedComposite returns true when the composite literal
// either omits `Signature` entirely or sets it to the
// predeclared `nil` identifier. A keyed composite that sets
// `Signature` to anything else is considered signed.
func isUnsignedComposite(cl *ast.CompositeLit) bool {
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			// Positional construction (no field names): we
			// cannot tell which slot is Signature without
			// type info. Treat positional construction as
			// unsigned to be conservative; the repo never
			// uses positional construction for
			// PolicyVersion (verified by grep), so this
			// branch is defensive only.
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != signatureFieldName {
			continue
		}
		// Found an explicit Signature: ... assignment.
		if ident, ok := kv.Value.(*ast.Ident); ok && ident.Name == "nil" {
			return true
		}
		return false
	}
	// No Signature field set: zero-value = nil slice =>
	// unsigned.
	return true
}

// hasNotProdBuildTag returns true when the file carries a
// `//go:build` constraint that excludes the `prod` tag. The
// simplest conforming form is `//go:build !prod`, which is
// the convention used throughout `internal/cli/devpolicy`
// (e.g. `unsigned_dev.go:7`). More elaborate constraints
// such as `//go:build !prod && !release` are also accepted
// because they likewise exclude `prod`.
//
// The check is syntactic: it locates the `//go:build` line,
// trims the directive prefix, and searches the remaining
// expression for the substring `!prod` while making sure no
// bare `prod` token appears in a positive position. This is
// good enough for the constraint forms the repo uses and
// avoids pulling in `go/build/constraint` (which would also
// work but adds parsing surface that is not needed for the
// narrow set of expressions in scope here).
func hasNotProdBuildTag(file *ast.File) bool {
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
// The check tokenises on whitespace and the boolean
// operators `&&`, `||`, `!`, and parentheses, then verifies
// that:
//
//   - `!prod` appears as a negated tag in the expression, AND
//   - `prod` does NOT appear as a positive (un-negated) tag.
//
// For the expressions actually used in this repo
// (`!prod`, `!prod && something`, `something && !prod`),
// this is equivalent to a full satisfiability check. For
// disjunctions such as `!prod || other` the function
// conservatively returns false, because `other` alone would
// allow a prod build to slip through; the repo never uses
// such a disjunction for the bypass files, so this
// conservative posture costs nothing.
func buildExprExcludesProd(expr string) bool {
	tokens := tokenizeBuildExpr(expr)

	// Reject anything with a disjunction: `||` could let a
	// prod build satisfy the constraint via the other
	// branch. Conservative; matches repo conventions.
	for _, t := range tokens {
		if t == "||" {
			return false
		}
	}

	sawNotProd := false
	for i, t := range tokens {
		if t == prodBuildTag {
			// Positive `prod` appears: this file IS gated
			// to prod (not what we want).
			if i == 0 || tokens[i-1] != "!" {
				return false
			}
			// `!prod` -- counts as the exclusion we need.
			sawNotProd = true
		}
	}
	return sawNotProd
}

// tokenizeBuildExpr splits a `//go:build` expression into
// the minimal set of tokens needed by `buildExprExcludesProd`:
// identifiers, `&&`, `||`, `!`, `(`, `)`. Whitespace is the
// primary delimiter; `!` is split off the front of any tag
// token so it surfaces as its own token (`!prod` -> `!`,
// `prod`). This is deliberately not a full Go build-tag
// parser; it is the smallest tokenizer that handles the
// expressions actually written in this repo.
func tokenizeBuildExpr(expr string) []string {
	// Normalise parentheses so they tokenise as standalone
	// units.
	expr = strings.ReplaceAll(expr, "(", " ( ")
	expr = strings.ReplaceAll(expr, ")", " ) ")
	fields := strings.Fields(expr)

	out := make([]string, 0, len(fields))
	for _, f := range fields {
		// Split a leading `!` off a tag (`!prod` -> `!`, `prod`).
		for strings.HasPrefix(f, "!") && len(f) > 1 {
			out = append(out, "!")
			f = f[1:]
		}
		out = append(out, f)
	}
	return out
}
