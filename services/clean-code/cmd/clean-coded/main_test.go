package main

import "testing"

// TestRun_VersionFlag exercises the `clean-coded -version` path
// without requiring a free TCP port. It also doubles as the
// minimal smoke test that the composition root links cleanly --
// the very fact that the test compiles is what asserts the
// composition root has no missing import / type mismatch.
func TestRun_VersionFlag(t *testing.T) {
	t.Parallel()
	if err := run([]string{"-version"}); err != nil {
		t.Fatalf("run(-version) returned %v; want nil", err)
	}
}

func TestRun_UnknownFlagFails(t *testing.T) {
	t.Parallel()
	if err := run([]string{"-not-a-flag"}); err == nil {
		t.Fatalf("run(-not-a-flag) returned nil; want a flag-parse error")
	}
}
