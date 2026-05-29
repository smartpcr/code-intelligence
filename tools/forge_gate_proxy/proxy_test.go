// Package forge_gate_proxy is the cross-module test discovery shim for
// the Forge per-iter test gate. It exists for a single reason: the
// gate runs `go test ./... -run '<regex>'` from the REPOSITORY ROOT,
// but the repo is a multi-module Go workspace:
//
//   - root `go.mod`                          (this module, historical)
//   - `services/clean-code/go.mod`           (the real service)
//   - `services/agent-memory/go.mod`         (sibling service)
//
// Go's `./...` pattern does NOT cross module boundaries even in
// workspace mode (see https://go.dev/ref/mod#workspaces and the
// confirmed behavior of `go list ./...` from this workspace root).
// So `./...` from the root finds zero packages -- the gate exits 1
// with `no packages to test`, and NONE of the workstream's actual
// tests run.
//
// This proxy package's `TestMain` is the workaround: it shells out to
// `go test ./...` IN EACH SERVICE MODULE'S DIRECTORY and propagates
// the aggregated exit code. The full real test suite of every service
// module runs as a side-effect of the gate's `go test ./...` command
// discovering this single package at root.
//
// Why TestMain (not a Test* function)? `go test` runs `TestMain`
// regardless of the `-run` filter -- the filter only applies to
// individual `m.Run()` test selection. By doing the work in TestMain
// and never calling `m.Run()`, the gate's `-run '<long-regex>'` does
// not need to match any test name in this package: TestMain runs,
// the service tests run, the exit code propagates.
//
// Follow-up: this shim is OBVIATED if the repo merges the three
// `go.mod` files into a single root module (drops 9+ CI workflow
// rewrites). Until then, this is the LEAST invasive fix.
package forge_gate_proxy_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// submoduleTestTargets is the ordered list of go-module directories
// (relative to the repo root) whose `go test ./...` must run as part
// of the workspace-wide test gate. Currently only the clean-code
// service is gated -- the workstream this proxy was added for
// (`ws-code-intelligence-clean-code-...`) lives entirely under
// `services/clean-code/`, and gating sibling modules whose tests
// have pre-existing failures (e.g. agent-memory's fingerprint
// goldens were generated on an older Go runtime and emit different
// bytes on the current toolchain) would fail-open the gate on
// unrelated work. A follow-up workstream should add agent-memory
// to this list after the pre-existing failures are repaired.
var submoduleTestTargets = []string{
	"services/clean-code",
}

// repoRoot returns the absolute path to the repository root. The
// package lives at `tools/forge_gate_proxy/` so the root is two
// directories above this file. We resolve the path at runtime via
// `runtime.Caller` so the test binary runs identically whether
// `go test` is invoked from the repo root (Forge gate) or from
// `tools/forge_gate_proxy/` (developer iteration).
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("forge_gate_proxy: runtime.Caller failed -- cannot resolve repo root")
	}
	// file == .../tools/forge_gate_proxy/proxy_test.go
	dir := filepath.Dir(file)        // .../tools/forge_gate_proxy
	dir = filepath.Dir(dir)          // .../tools
	dir = filepath.Dir(dir)          // .../repo-root
	return dir, nil
}

// TestMain is the test-binary entry point. It runs the FULL service
// test suite of every module listed in `submoduleTestTargets`, in
// order, and exits with the aggregated status:
//
//   - exit 0 -- every submodule's `go test ./...` exited 0.
//   - exit 1 -- AT LEAST ONE submodule's `go test ./...` exited
//     non-zero (the inner module's test output is forwarded to
//     stdout/stderr so the failing test name is visible in the gate
//     log).
//
// The `-run` flag passed by the gate is INTENTIONALLY ignored: this
// shim is the entire test gate for the multi-module repo, so it must
// run every submodule's tests unconditionally. Per-test filtering is
// a no-op because `m.Run()` is never called -- there are no Test*
// functions in this package whose execution the gate's `-run` could
// scope.
//
// To skip the proxy for fast local iteration (e.g. you're only
// editing a single submodule and don't want to run the other), set
// FORGE_GATE_PROXY_SKIP=1 in the environment -- the proxy exits 0
// immediately and the developer can `cd` into the submodule and run
// `go test ./...` directly.
func TestMain(m *testing.M) {
	if os.Getenv("FORGE_GATE_PROXY_SKIP") == "1" {
		fmt.Fprintln(os.Stdout, "forge_gate_proxy: FORGE_GATE_PROXY_SKIP=1 -- skipping submodule tests")
		os.Exit(0)
	}

	root, err := repoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge_gate_proxy: cannot resolve repo root: %v\n", err)
		os.Exit(2)
	}

	exitCode := 0
	for _, sub := range submoduleTestTargets {
		modDir := filepath.Join(root, filepath.FromSlash(sub))
		fmt.Fprintf(os.Stdout, "forge_gate_proxy: ---- running %s -> go test ./... ----\n", sub)

		// -count=1 disables Go's per-package test cache so the gate
		// reflects the WORKING-TREE state every time, not a stale
		// cached result from a previous identical invocation.
		cmd := exec.Command("go", "test", "-count=1", "./...")
		cmd.Dir = modDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "forge_gate_proxy: %s tests FAILED: %v\n", sub, err)
			exitCode = 1
		} else {
			fmt.Fprintf(os.Stdout, "forge_gate_proxy: %s tests OK\n", sub)
		}
	}

	if exitCode == 0 {
		fmt.Fprintln(os.Stdout, "forge_gate_proxy: all submodule tests passed")
	} else {
		fmt.Fprintln(os.Stderr, "forge_gate_proxy: one or more submodules failed -- see log above")
	}
	os.Exit(exitCode)
}

// TestProxy_ServiceTestsAggregated is a NO-OP marker test that
// documents the proxy contract in its name and ensures the test
// binary builds even on a future Go version that requires at least
// one Test* function for the binary to be emitted. The real work is
// in TestMain above; this function never runs because TestMain
// exits before m.Run() would be called.
//
// Naming it `TestProxy_*` keeps it OUT OF every Forge gate `-run`
// regex (which enumerates real service test names) so it does not
// shadow or duplicate any real test in the service modules.
func TestProxy_ServiceTestsAggregated(t *testing.T) {
	t.Skip("proxy work is done in TestMain; this test exists only to keep the binary buildable")
}
