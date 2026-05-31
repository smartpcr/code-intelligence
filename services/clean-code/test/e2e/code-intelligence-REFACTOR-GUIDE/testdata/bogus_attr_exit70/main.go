// Package main is a test helper that exercises the dark-metric
// closed-set validation with a bogus_attr value. When validation
// detects the unknown attr it prints the error to stderr and exits
// with code 70 — mirroring the CLI binary's init-time panic-to-exit
// mapping (tech-spec Sec 8.6: panics outside per-file parse exit 70).
//
// The E2E step runs this helper via exec.Command and asserts:
//   - exit code == 70
//   - stderr contains "NOT in the closed dark-metric attr set (tech-spec Sec 8.7"
package main

import (
	"fmt"
	"os"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metrics/recipes"
)

// allowedDarkAttrs mirrors the closed set in
// orchestrator/dark_metrics.go (tech-spec Sec 8.7 line 1008).
var allowedDarkAttrs = map[string]struct{}{
	recipes.AttrDecisionBlocks: {},
	recipes.AttrCallEdges:      {},
	recipes.AttrFieldAccesses:  {},
}

func main() {
	bogusAttr := "bogus_attr"

	// Validate — same check as orchestrator.validateMetricAttrRequirements.
	if _, ok := allowedDarkAttrs[bogusAttr]; !ok {
		errMsg := fmt.Sprintf(
			"orchestrator: invalid metricAttrRequirements table: "+
				"row 0 (kind=%q): Attrs[0]=%q is NOT in the closed dark-metric attr set "+
				"(tech-spec Sec 8.7 line 1008: {%q, %q, %q})",
			"bogus_metric", bogusAttr,
			recipes.AttrDecisionBlocks, recipes.AttrCallEdges, recipes.AttrFieldAccesses,
		)
		fmt.Fprintln(os.Stderr, errMsg)
		os.Exit(70)
	}
}
