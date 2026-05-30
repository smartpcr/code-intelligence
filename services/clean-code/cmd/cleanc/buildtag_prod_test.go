//go:build prod

package main

import (
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/flags"
)

// TestBuildTagIsProdOnProdBuild pins the cmd-local `buildTag`
// constant under `-tags prod`. The paired
// `buildtag_default_test.go` (`//go:build !prod`) handles the
// dev build. Both files together replace iter-1's
// unconditional `TestBuildTagMatchesDevDefault` (the source
// of iter-4 evaluator item 3 -- "PROD TEST FAILURE").
func TestBuildTagIsProdOnProdBuild(t *testing.T) {
	t.Parallel()

	if buildTag != "prod" {
		t.Errorf("buildTag = %q, want %q under -tags prod", buildTag, "prod")
	}
}

// TestFlagsDefaultDevModeIsFalseOnProdBuild belt-and-braces:
// confirms the centralised `flags.DefaultDevMode` under
// `-tags prod` is false. A regression that re-introduced
// `defaultDevMode` in cmd/cleanc/ (and missed flipping the
// prod twin) would trip here, not just in
// `internal/cli/flags/devmode_prod_test.go`.
func TestFlagsDefaultDevModeIsFalseOnProdBuild(t *testing.T) {
	t.Parallel()

	if flags.DefaultDevMode != false {
		t.Errorf("flags.DefaultDevMode = %v under -tags prod; want false", flags.DefaultDevMode)
	}
}
