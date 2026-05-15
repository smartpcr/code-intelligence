package version

import "testing"

// TestDefaultsArePopulated guards against an empty defaults block
// silently shipping in a release binary -- if any of these ever
// becomes "" the build is unstamped and we want a loud test
// failure rather than a quiet "unknown" string showing up in
// /healthz output.
func TestDefaultsArePopulated(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"Version":   Version,
		"Commit":    Commit,
		"BuildDate": BuildDate,
	}
	for name, value := range cases {
		if value == "" {
			t.Errorf("%s default is empty; expected a non-empty fallback string", name)
		}
	}
}
