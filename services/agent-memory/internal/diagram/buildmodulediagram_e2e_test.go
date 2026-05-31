// Bridge test: makes the BuildModuleDiagram godog suite discoverable
// from the declared local gate path `go test ./internal/diagram/...`.
//
// The canonical Gherkin feature and step definitions live in
// test/e2e/code-intelligence-REPO-SCANNER/. This bridge forwards
// `go test ./internal/diagram/...` to that canonical suite so the
// declared gate command exercises the acceptance scenarios without
// requiring an explicit -tags e2e flag.

package diagram_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestE2E_diagram_projector_buildmodulediagram(t *testing.T) {
	// Resolve module root from this file's location.
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// internal/diagram/ -> services/agent-memory/
	modRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	goMod := filepath.Join(modRoot, "go.mod")
	if _, err := os.Stat(goMod); err != nil {
		t.Fatalf("go.mod not found at %s: %v", modRoot, err)
	}

	args := []string{
		"test", "-tags", "e2e", "-v", "-count=1", "-timeout", "180s",
		"-run", `^TestE2E_diagram_projector_buildmodulediagram$`,
		"./test/e2e/code-intelligence-REPO-SCANNER/...",
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = modRoot
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	t.Logf("bridge: running godog suite from %s", modRoot)
	if err := cmd.Run(); err != nil {
		t.Fatalf("BuildModuleDiagram godog suite failed: %v", err)
	}
}
