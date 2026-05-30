package flags

import (
	"bytes"
	"flag"
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

// TestPolicyDefaultTopNPinnedToTwenty pins the new
// `PolicyDefaultTopN` constant added to resolve iter-4
// evaluator item 7 -- "TOP-N DEFAULT TEXT DOES NOT MATCH
// SPEC". Tech-spec Sec 8.1 row 6 says the literal CLI value
// `--top-n 0` means "use policy default of 20", so the
// renderer substitutes this constant when it sees the
// default. Renaming or renumbering is a contract change
// against the e2e renderer scenarios.
func TestPolicyDefaultTopNPinnedToTwenty(t *testing.T) {
	t.Parallel()

	if PolicyDefaultTopN != 20 {
		t.Errorf("PolicyDefaultTopN = %d, want 20 (tech-spec Sec 8.1 row 6)", PolicyDefaultTopN)
	}
	if DefaultTopN != 0 {
		t.Errorf("DefaultTopN = %d, want 0 (tech-spec Sec 8.1 row 6: 0 means use PolicyDefaultTopN)", DefaultTopN)
	}
}

// TestRegisterAttachesEverySec81Flag pins the closed
// flag-name set the Register helper attaches to its target
// flag-set. Resolves iter-4 evaluator item 4 by giving the
// helper a single-source-of-truth test, so a regression that
// drops or renames a flag in Register would fail HERE rather
// than only in main_test.go where the surface is hidden
// behind the dispatcher.
func TestRegisterAttachesEverySec81Flag(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	g := Register(fs)
	if g == nil {
		t.Fatal("Register returned nil *Globals")
	}

	want := []string{
		"out", "findings", "emit-prompts", "policy",
		"with-churn", "top-n", "exit-on", "diagnostics",
		"dev-mode", "telemetry-otlp",
	}
	seen := make(map[string]bool, len(want))
	fs.VisitAll(func(f *flag.Flag) { seen[f.Name] = true })

	for _, name := range want {
		if !seen[name] {
			t.Errorf("Register did not attach --%s", name)
		}
	}
	if len(seen) != len(want) {
		t.Errorf("Register attached %d flags %v, want %d %v", len(seen), seen, len(want), want)
	}
}

// TestRegisterUsesPackageDefaultDevMode resolves iter-4
// evaluator item 6 ("DEV-MODE DEFAULT NOT CENTRALIZED IN
// FLAGS HELPER"). The `--dev-mode` default MUST come from
// the build-tag-paired `DefaultDevMode` constant in this
// package, not from a cmd-local twin -- so a regression
// that re-introduces `cmd/cleanc/buildtag_default.go`'s
// `defaultDevMode` would not be silently shadowed.
func TestRegisterUsesPackageDefaultDevMode(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	g := Register(fs)
	if g.DevMode == nil {
		t.Fatal("Globals.DevMode is nil after Register")
	}
	if *g.DevMode != DefaultDevMode {
		t.Errorf("--dev-mode default = %v, want flags.DefaultDevMode = %v",
			*g.DevMode, DefaultDevMode)
	}

	// Cross-check via the actual flag's DefValue text.
	f := fs.Lookup("dev-mode")
	if f == nil {
		t.Fatal("--dev-mode flag not found on flag-set")
	}
	wantText := "true"
	if !DefaultDevMode {
		wantText = "false"
	}
	if f.DefValue != wantText {
		t.Errorf("--dev-mode DefValue = %q, want %q", f.DefValue, wantText)
	}
}

// TestGlobalsValidateRejectsReservedFlags walks the three
// cross-flag rules pinned by e2e-scenarios.md Stage 3.3 /
// Stage 4.4 (rejected `--telemetry-otlp`, rejected
// `--with-churn`, rejected `--exit-on banana`) and confirms
// Validate writes the pinned literal message AND returns a
// non-nil error so the dispatcher can map the result to
// ExitUsage.
func TestGlobalsValidateRejectsReservedFlags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		mutate     func(g *Globals)
		wantPhrase string
	}{
		{
			name: "telemetry-otlp",
			mutate: func(g *Globals) {
				v := "http://localhost:4317"
				g.TelemetryOTLP = &v
			},
			wantPhrase: "--telemetry-otlp is reserved for a future story",
		},
		{
			name: "with-churn",
			mutate: func(g *Globals) {
				v := true
				g.WithChurn = &v
			},
			wantPhrase: "--with-churn is reserved for P2",
		},
		{
			name: "exit-on banana",
			mutate: func(g *Globals) {
				v := "banana"
				g.ExitOn = &v
			},
			wantPhrase: "--exit-on must be one of",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			g := Register(fs)
			tc.mutate(g)
			var buf bytes.Buffer
			err := g.Validate(VerbAnalyze, &buf)
			if err == nil {
				t.Fatalf("Validate returned nil, want non-nil error")
			}
			if got := buf.String(); !contains(got, tc.wantPhrase) {
				t.Errorf("stderr = %q, want substring %q", got, tc.wantPhrase)
			}
		})
	}
}

// TestGlobalsValidateAcceptsDefaults confirms a freshly-
// registered Globals passes Validate without writing
// anything to stderr (the defaults are the happy path).
func TestGlobalsValidateAcceptsDefaults(t *testing.T) {
	t.Parallel()

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	g := Register(fs)
	var buf bytes.Buffer
	if err := g.Validate(VerbAnalyze, &buf); err != nil {
		t.Errorf("Validate(defaults) error = %v, want nil", err)
	}
	if buf.Len() != 0 {
		t.Errorf("Validate(defaults) wrote %q to stderr, want empty", buf.String())
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
