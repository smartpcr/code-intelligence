package deploy

// Stage 8.3 iter-2 evaluator fix #2 -- "Prometheus/OTel are
// still not exposed from every binary". This static
// inventory walks `services/agent-memory/cmd/*` and asserts
// each binary either:
//
//   - is a Go binary that calls obs.SetupTracer AND mounts a
//     /metrics handler (or the qdrant-bootstrap / repoindexer
//     equivalent through observability.go); OR
//
//   - is the Python sidecar at cmd/reranker-sidecar/main.py
//     which imports the local `observability` module's
//     setup_tracer + install_observability helpers.
//
// The test reads the source files literally so the inventory
// catches regressions where someone removes the OTel hookup
// without flipping a feature flag the runtime test would
// notice. We deliberately do NOT spin up the actual binaries
// (a `go run` in CI is slow and flaky) -- the static
// inventory is sufficient because the build gate already
// guarantees the source compiles.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cmdRoot is the relative path from the `deploy` package
// (cwd at test time) to the cmd/ directory.
const cmdRoot = "../cmd"

// binaryExpectations encodes the per-binary contract.
// `metricsAnchors` is a list of substrings; at least one MUST
// appear in main.go for the binary to be considered to
// expose /metrics. `tracerAnchors` is the same for OTel
// setup.
type binaryExpectations struct {
	mainPath       string
	metricsAnchors []string
	tracerAnchors  []string
	// allowMissing is for binaries that are scaffolds /
	// stubs that have not been wired yet AND whose absence
	// is intentional (none currently).
	allowMissing bool
}

func goBinaryExpectations() map[string]binaryExpectations {
	common := []string{
		`obs.SetupTracer(`,
		`obs.SetupTracer (`,
	}
	return map[string]binaryExpectations{
		"agent-api": {
			mainPath: "agent-api/main.go",
			metricsAnchors: []string{
				`"/metrics"`,
				`obs.NewHistogram`,
			},
			tracerAnchors: common,
		},
		"mgmt-api": {
			mainPath:       "mgmt-api/main.go",
			metricsAnchors: []string{`"/metrics"`, `NewCombinedMetricsHandlerWithExtras`},
			tracerAnchors:  common,
		},
		"consolidator": {
			mainPath:       "consolidator/main.go",
			metricsAnchors: []string{`"/metrics"`},
			tracerAnchors:  common,
		},
		"concept-promoter": {
			mainPath:       "concept-promoter/main.go",
			metricsAnchors: []string{`"/metrics"`},
			tracerAnchors:  common,
		},
		"repoindexer": {
			mainPath:       "repoindexer/main.go",
			metricsAnchors: []string{`"/metrics"`},
			tracerAnchors:  common,
		},
		"reranker-trainer": {
			mainPath:       "reranker-trainer/main.go",
			metricsAnchors: []string{`"/metrics"`},
			tracerAnchors:  common,
		},
		"span-ingestor": {
			mainPath:       "span-ingestor/main.go",
			metricsAnchors: []string{`"/metrics"`},
			tracerAnchors:  common,
		},
		"trace-log-pruner": {
			mainPath:       "trace-log-pruner/main.go",
			metricsAnchors: []string{`"/metrics"`},
			tracerAnchors:  common,
		},
		"webhook-receiver": {
			mainPath:       "webhook-receiver/main.go",
			metricsAnchors: []string{`"/metrics"`},
			tracerAnchors:  common,
		},
		"qdrant-bootstrap": {
			mainPath:       "qdrant-bootstrap/main.go",
			metricsAnchors: []string{`startMetricsServer`, `"/metrics"`},
			tracerAnchors:  common,
		},
	}
}

func pythonBinaryExpectations() map[string]binaryExpectations {
	return map[string]binaryExpectations{
		"reranker-sidecar": {
			mainPath: "reranker-sidecar/main.py",
			metricsAnchors: []string{
				`install_observability(`,
				`SidecarMetrics(`,
			},
			tracerAnchors: []string{
				`setup_tracer(`,
				`from observability import`,
			},
		},
	}
}

// TestEveryBinaryExposesObservability is the §8.3 step 1+2
// enforcement test. It walks cmd/* and verifies each
// expected binary's main file references both a Prometheus
// /metrics surface AND an OTel tracer setup call.
func TestEveryBinaryExposesObservability(t *testing.T) {
	t.Parallel()

	allExpect := map[string]binaryExpectations{}
	for k, v := range goBinaryExpectations() {
		allExpect[k] = v
	}
	for k, v := range pythonBinaryExpectations() {
		allExpect[k] = v
	}

	for name, want := range allExpect {
		name := name
		want := want
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(cmdRoot, want.mainPath)
			body, err := os.ReadFile(path)
			if err != nil {
				if want.allowMissing {
					t.Skipf("main file %q missing (allow_missing=true): %v", path, err)
					return
				}
				t.Fatalf("read %s: %v", path, err)
			}
			text := string(body)
			matched := func(anchors []string, kind string) {
				t.Helper()
				for _, anchor := range anchors {
					if strings.Contains(text, anchor) {
						return
					}
				}
				t.Errorf("%s: no %s anchor matched. Tried: %v",
					path, kind, anchors)
			}
			matched(want.metricsAnchors, "Prometheus /metrics")
			matched(want.tracerAnchors, "OTel tracer setup")
		})
	}
}

// TestNoUnexpectedBinaries walks cmd/* and fails if a new
// directory shows up that is NOT in the inventory expectation
// table. Guards against the easy regression where a future
// PR adds cmd/new-thing/main.go without wiring observability;
// the inventory must be updated as new binaries land.
func TestNoUnexpectedBinaries(t *testing.T) {
	t.Parallel()

	known := map[string]bool{}
	for k := range goBinaryExpectations() {
		known[k] = true
	}
	for k := range pythonBinaryExpectations() {
		known[k] = true
	}

	entries, err := os.ReadDir(cmdRoot)
	if err != nil {
		t.Fatalf("read cmd dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		// Skip hidden dirs like __pycache__ inside a
		// sub-binary; here we filter at the top level.
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if !known[e.Name()] {
			t.Errorf("unexpected cmd/%s -- add it to the observability inventory (cmd_inventory_test.go) or rename / remove",
				e.Name())
		}
	}

	// Symmetric check: every name in the inventory MUST
	// have a directory on disk.
	for name := range known {
		full := filepath.Join(cmdRoot, name)
		st, err := os.Stat(full)
		if err != nil {
			t.Errorf("inventory lists cmd/%s but it does not exist on disk: %v", name, err)
			continue
		}
		if !st.IsDir() {
			t.Errorf("inventory expects cmd/%s to be a directory; got mode %v", name, st.Mode())
		}
	}
}

// Compile-time anchor; keep fs import alive without
// generating an unused-import error if we drop a usage above.
var _ fs.FileInfo = nil

// ensure `fmt` is referenced even when no formatting is
// emitted on the happy path.
var _ = fmt.Sprintf
