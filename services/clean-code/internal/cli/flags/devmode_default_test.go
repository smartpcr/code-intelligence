//go:build !prod

package flags

import "testing"

func TestDefaultDevModeIsTrueOnDevBuild(t *testing.T) {
	if DefaultDevMode != true {
		t.Fatalf("flags.DefaultDevMode = %v under no build tag; tech-spec Sec 8.9 pins TRUE for the dev build", DefaultDevMode)
	}
}
