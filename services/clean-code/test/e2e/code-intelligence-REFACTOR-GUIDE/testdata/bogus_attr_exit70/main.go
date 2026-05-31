// Package main is a test helper that exercises the production
// [orchestrator.ValidateBogusAttrRow] function — the same
// closed-set validation the orchestrator runs at init time
// (dark_metrics.go init → validateMetricAttrRequirements).
// ValidateBogusAttrRow constructs a single-row table with
// bogus_attr and calls the production validator. When the
// production validator detects the unknown attr it returns an
// error; this binary prints it to stderr and exits with code
// 70 — mirroring the CLI binary's init-time panic-to-exit
// mapping (tech-spec Sec 8.6: panics outside per-file parse
// exit 70).
//
// The E2E step runs this helper via exec.Command and asserts:
//   - exit code == 70
//   - stderr contains "NOT in the closed dark-metric attr set (tech-spec Sec 8.7"
package main

import (
	"fmt"
	"os"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
)

func main() {
	if err := orchestrator.ValidateBogusAttrRow(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(70)
	}
}
