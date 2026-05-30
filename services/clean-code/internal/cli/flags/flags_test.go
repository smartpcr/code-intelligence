package flags

import (
	"sort"
	"testing"
)

// TestExitCodeValuesPinned guards the exit codes against drift
// from tech-spec.md Sec 8.6. The CI gates assert these numbers
// byte-for-byte against the process exit code so a change here
// is a contract change.
func TestExitCodeValuesPinned(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		got  int
		want int
	}{
		{"ExitOK", ExitOK, 0},
		{"ExitFindingTriggered", ExitFindingTriggered, 1},
		{"ExitWalkerError", ExitWalkerError, 2},
		{"ExitUsage", ExitUsage, 64},
		{"ExitInternalError", ExitInternalError, 70},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (tech-spec Sec 8.6)", tc.name, tc.got, tc.want)
		}
	}
}

// TestVerbsContainsAllSubcommands checks the canonical verb
// list matches the closed sub-command set the dispatcher knows
// about. The order matters -- usage text renders verbs in
// the same order they appear in `Verbs`.
func TestVerbsContainsAllSubcommands(t *testing.T) {
	t.Parallel()

	want := []string{"analyze", "report", "version", "apply"}
	if len(Verbs) != len(want) {
		t.Fatalf("Verbs length = %d, want %d", len(Verbs), len(want))
	}
	for i, v := range want {
		if Verbs[i] != v {
			t.Errorf("Verbs[%d] = %q, want %q", i, Verbs[i], v)
		}
	}
}

// TestDefaultFindingsPinned guards the `--findings` default
// against drift from tech-spec.md Sec 8.1 row 2 -- the e2e
// scenarios reference the file name verbatim so a change
// here cascades through the test harness.
func TestDefaultFindingsPinned(t *testing.T) {
	t.Parallel()

	if DefaultFindings != "findings.json" {
		t.Errorf("DefaultFindings = %q, want %q", DefaultFindings, "findings.json")
	}
}

// TestDefaultExitOnPinned guards the `--exit-on` default
// against drift. The default of "block" means only severity
// block findings trip exit code 1.
func TestDefaultExitOnPinned(t *testing.T) {
	t.Parallel()

	if DefaultExitOn != "block" {
		t.Errorf("DefaultExitOn = %q, want %q", DefaultExitOn, "block")
	}
	if !IsValidExitOn(DefaultExitOn) {
		t.Errorf("DefaultExitOn %q is not in ExitOnLevels %v", DefaultExitOn, ExitOnLevels)
	}
}

// TestIsValidExitOnAcceptsClosedSet validates that the
// accepted set matches the spec exactly and rejects all other
// inputs.
func TestIsValidExitOnAcceptsClosedSet(t *testing.T) {
	t.Parallel()

	for _, lvl := range ExitOnLevels {
		if !IsValidExitOn(lvl) {
			t.Errorf("IsValidExitOn(%q) = false, want true", lvl)
		}
	}
	for _, bad := range []string{"", "INFO", "Block", "warning", "banana", "0", "1"} {
		if IsValidExitOn(bad) {
			t.Errorf("IsValidExitOn(%q) = true, want false", bad)
		}
	}
}

// TestExitOnLevelsSorted documents the expected severity
// ordering: info < warn < block.
func TestExitOnLevelsSorted(t *testing.T) {
	t.Parallel()

	want := []string{"info", "warn", "block"}
	if len(ExitOnLevels) != len(want) {
		t.Fatalf("ExitOnLevels length = %d, want %d", len(ExitOnLevels), len(want))
	}
	for i, lvl := range want {
		if ExitOnLevels[i] != lvl {
			t.Errorf("ExitOnLevels[%d] = %q, want %q", i, ExitOnLevels[i], lvl)
		}
	}
}

// TestNoDuplicateVerbs guards against a copy-paste error
// silently introducing a duplicate sub-command name.
func TestNoDuplicateVerbs(t *testing.T) {
	t.Parallel()

	seen := make(map[string]bool, len(Verbs))
	for _, v := range Verbs {
		if seen[v] {
			t.Errorf("duplicate verb %q in Verbs", v)
		}
		seen[v] = true
	}
	// Belt-and-braces: also confirm the list is in a stable
	// canonical order by re-sorting a copy and comparing
	// lengths. (Order is intentionally NOT alphabetical, so
	// we only check length parity here.)
	sorted := make([]string, len(Verbs))
	copy(sorted, Verbs)
	sort.Strings(sorted)
	if len(sorted) != len(Verbs) {
		t.Fatalf("sort changed slice length: %d -> %d", len(Verbs), len(sorted))
	}
}
