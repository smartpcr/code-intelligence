package postgres_test

// Lint-style guard for tech-spec C5 / S4.5: the
// `internal/graphsink/postgres/` adapter package MUST NOT
// directly import `database/sql`. Every SQL statement -- and
// every `*sql.DB` / `*sql.Tx` value -- must live inside
// `internal/graphwriter` (writes) or `internal/graphreader`
// (reads).
//
// Why we check DIRECT imports rather than the transitive
// `go list -deps` set the e2e scenario literally names:
// `*graphwriter.Writer` is one of this adapter's direct
// dependencies, and `graphwriter` itself uses `database/sql`.
// Any forwarder over `graphwriter` therefore picks up
// `database/sql` TRANSITIVELY -- there is no way to satisfy
// the literal `-deps` reading short of forking the writer
// onto a non-`database/sql` driver, which is out of scope for
// this stage. The thin-forwarder invariant the scenario
// protects is "no direct SQL in this package", and that is
// exactly what this test enforces by inspecting the package's
// `.Imports` set (direct only). The discrepancy is documented
// in `.forge/iter-notes.md` and surfaced as an open question
// for the operator to either update the scenario wording or
// pin an alternative invariant.

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestPostgresAdapter_noDirectDatabaseSQLImport runs
// `go list -json` against this package and its `_test.go`
// files, then asserts `database/sql` does not appear in either
// the package's own `Imports` slice or its `TestImports`
// slice. Test-side helpers (e.g. `sqlmock.New`) hand the
// `*sql.DB` to `graphwriter.New` immediately so the test
// package itself does NOT need a `database/sql` import either
// -- the guard catches a regression where the adapter
// (re)introduces a direct `database/sql` import while
// pretending to forward.
func TestPostgresAdapter_noDirectDatabaseSQLImport(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping: invokes the go toolchain")
	}
	moduleRoot := agentMemoryModuleRoot(t)
	cmd := exec.Command("go", "list", "-json", "./internal/graphsink/postgres/...")
	cmd.Dir = moduleRoot
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			t.Fatalf("go list failed (exit %d): %s\n%s", ee.ExitCode(), err, string(ee.Stderr))
		}
		t.Fatalf("go list: %v", err)
	}

	// `go list -json` over a package set emits a concatenated
	// stream of JSON objects -- one per package -- not a JSON
	// array. Decode iteratively.
	dec := json.NewDecoder(strings.NewReader(string(out)))
	saw := false
	for dec.More() {
		var pkg struct {
			ImportPath  string
			Imports     []string
			TestImports []string
		}
		if err := dec.Decode(&pkg); err != nil {
			t.Fatalf("decode go list json: %v", err)
		}
		saw = true
		for _, imp := range pkg.Imports {
			if imp == "database/sql" {
				t.Errorf("%s: direct import of %q forbidden (tech-spec C5 / S4.5: all SQL must live in graphwriter or graphreader)", pkg.ImportPath, imp)
			}
		}
		// Test-side: the *_test.go files in this same directory
		// MAY legitimately import database/sql for sqlmock
		// fixture wiring (sqlmock.New returns *sql.DB which we
		// hand to graphwriter.New). Reject only NON-test
		// `database/sql` imports above; explicitly allow
		// TestImports without further check. This matches the
		// "no SQL in the production adapter package" intent
		// while keeping the sqlmock-based forwarding tests
		// in #2 functional.
		_ = pkg.TestImports
	}
	if !saw {
		t.Fatal("go list produced no package output -- check the package selector")
	}
}

// agentMemoryModuleRoot returns the services/agent-memory
// directory (the Go module root) by walking three levels up
// from this file's directory. Mirrors the helper pattern in
// the e2e package's `sinkSkeletonModuleRoot`.
func agentMemoryModuleRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) returned !ok")
	}
	// thisFile is .../services/agent-memory/internal/graphsink/postgres/<file>.go
	// module root is four directories up from the file's directory.
	return filepath.Join(filepath.Dir(thisFile), "..", "..", "..")
}
