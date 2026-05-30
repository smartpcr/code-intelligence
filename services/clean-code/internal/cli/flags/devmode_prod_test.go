//go:build prod

package flags

import "testing"

func TestDefaultDevModeIsFalseOnProdBuild(t *testing.T) {
	if DefaultDevMode != false {
		t.Fatalf("flags.DefaultDevMode = %v under -tags prod; tech-spec Sec 8.9 pins FALSE for the release binary", DefaultDevMode)
	}
}
