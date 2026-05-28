// Package conformance hosts cross-package invariants that
// would be impossible to enforce inside a single Go package's
// unit tests. The WAL scope test below walks the full module
// import graph and asserts the Stage 9.1 brief's allow-list
// is honoured: only the Audit-write call sites + the WAL
// package itself + the composition root + the two binaries
// may import `internal/audit/wal`. A new importer is a
// design-review trigger, not a silent code change.
package conformance

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// walImportPath is the fully-qualified import path of the
// Audit WAL writer package. Any module-internal package whose
// `Deps` list contains this path is treated as an importer
// and matched against the allow-list.
const walImportPath = "github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"

// allowedWalImporters is the closed set of packages the
// Stage 9.1 brief permits to import the Audit WAL writer.
// Adding to this list is a brief-level design change, not a
// PR-level decision -- the conformance test holds the line
// so a casual import doesn't smuggle a non-audit write into
// the WAL.
//
// CALLOUTS:
//   - `internal/audit/wal` is its own (trivial) entry: a
//     package always depends on itself via `go list`.
//   - `internal/evaluator` is the degraded-path writer.
//   - `internal/rule_engine` is the happy-path writer
//     (engine + SQLStore + txStore + sql_store_test).
//   - `internal/composition` is the production wiring root
//     (constructs the writer + the signer shim).
//   - `cmd/clean-code-eval-gate` and `cmd/clean-code-gateway`
//     are the binaries that resolve the writer via
//     composition. Their command packages legitimately
//     reference the wal type through the constructor return.
//   - `test/conformance` is THIS test package -- it needs the
//     import to mention the wal package by name.
var allowedWalImporters = map[string]struct{}{
	walImportPath: {},
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/evaluator":       {},
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine":     {},
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/composition":     {},
	"github.com/smartpcr/code-intelligence/services/clean-code/cmd/clean-code-eval-gate": {},
	"github.com/smartpcr/code-intelligence/services/clean-code/cmd/clean-code-gateway":   {},
	"github.com/smartpcr/code-intelligence/services/clean-code/test/conformance":         {},
}

// goListPackage mirrors the subset of `go list -deps -json`'s
// output the conformance test consumes. Field names match the
// Go tool's documented schema.
type goListPackage struct {
	ImportPath string   `json:"ImportPath"`
	Module     *struct{ Path string } `json:"Module,omitempty"`
	Standard   bool     `json:"Standard"`
	Imports    []string `json:"Imports"`
}

// TestWALScope walks the full import graph rooted at the
// module's top-level package set and asserts that every
// importer of [walImportPath] appears in
// [allowedWalImporters]. A failure prints the unauthorised
// importer plus the WAL-package's allow-list so the
// reviewer can decide whether to extend the allow-list
// (Sec 7.10 design change) or remove the offending import.
//
// The test invokes the `go` toolchain via `os/exec` rather
// than walking the source tree by hand: `go list` is the
// authoritative resolver for build-tag-conditional imports
// and module-level rewrites.
func TestWALScope(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-json", "../..."+"/...")
	cmd.Dir = "."
	// Prefer module-relative roots so `go list` walks every
	// production package under services/clean-code/...
	// regardless of the cwd at test invocation. The
	// `../...` glob covers every sibling test target plus
	// the production tree (cmd/, internal/, composition/).
	cmd = exec.Command("go", "list", "-deps", "-json", "../../...")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list -deps -json ../../...: %v -- this conformance test requires the go toolchain on PATH and the worktree's go.mod to resolve. Stderr: %s",
			err, strings.TrimSpace(stderrFromExitErr(err)))
	}

	dec := json.NewDecoder(strings.NewReader(string(out)))
	violations := make([]string, 0)
	seen := make(map[string]struct{})

	for dec.More() {
		var pkg goListPackage
		if err := dec.Decode(&pkg); err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		if pkg.Standard {
			// Stdlib packages cannot import our internal
			// types; skip them to keep the loop cheap.
			continue
		}
		// Skip third-party module deps -- they cannot
		// import an `internal/` package across module
		// boundaries (compile error), so a false positive
		// is impossible.
		if pkg.Module == nil ||
			pkg.Module.Path != "github.com/smartpcr/code-intelligence/services/clean-code" {
			continue
		}
		// Dedupe (go list -deps emits the same package
		// once per root, but multiple roots multiply
		// emissions).
		if _, ok := seen[pkg.ImportPath]; ok {
			continue
		}
		seen[pkg.ImportPath] = struct{}{}

		for _, imp := range pkg.Imports {
			if imp != walImportPath {
				continue
			}
			if _, ok := allowedWalImporters[pkg.ImportPath]; ok {
				continue
			}
			violations = append(violations, pkg.ImportPath)
		}
	}

	if len(violations) > 0 {
		allowed := make([]string, 0, len(allowedWalImporters))
		for k := range allowedWalImporters {
			allowed = append(allowed, "  - "+k)
		}
		t.Fatalf(
			"WAL scope violation: the following packages import %s but are NOT in the allow-list:\n  - %s\n\n"+
				"Allow-list (Stage 9.1 brief / architecture Sec 7.10):\n%s\n\n"+
				"To fix EITHER remove the import (the WAL is scoped EXCLUSIVELY to the three Audit tables) OR extend %s with the new entry after a design-review approval.",
			walImportPath,
			strings.Join(violations, "\n  - "),
			strings.Join(allowed, "\n"),
			"allowedWalImporters in test/conformance/wal_scope_test.go",
		)
	}
}

// stderrFromExitErr extracts the stderr capture from an
// `*exec.ExitError` (if any). Used to surface a useful
// failure message when `go list` aborts.
func stderrFromExitErr(err error) string {
	type stderrCarrier interface {
		Stderr() []byte
	}
	if e, ok := err.(stderrCarrier); ok {
		return string(e.Stderr())
	}
	if e, ok := err.(*exec.ExitError); ok {
		return string(e.Stderr)
	}
	return err.Error()
}
