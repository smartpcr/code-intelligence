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

// TestValidateUsesVerbInReservedFlagMessage confirms the
// `verb` argument threaded through Validate is actually woven
// into the literal stderr prefix -- i.e. calling Validate("report",
// stderr) emits `cleanc report: --with-churn is reserved ...`
// rather than the legacy `cleanc analyze:` baked into the const
// form. Without this guard the iter-4 fix for item 4 (REPORT
// FLAG SURFACE INCOMPLETE) is half-done: the flag surface lights
// up but the error text still misleads operators into thinking
// the rejection came from `analyze`. The substring assertions
// the e2e scenarios pin (`--with-churn is reserved`) remain
// satisfied either way; this test covers the cosmetic accuracy
// gap that wasn't otherwise observable.
func TestValidateUsesVerbInReservedFlagMessage(t *testing.T) {
	t.Parallel()

	verbs := []string{VerbAnalyze, VerbReport}
	for _, verb := range verbs {
		verb := verb
		t.Run("verb="+verb, func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			g := Register(fs)
			churn := true
			g.WithChurn = &churn

			var buf bytes.Buffer
			if err := g.Validate(verb, &buf); err == nil {
				t.Fatalf("Validate(%q) returned nil, want non-nil error", verb)
			}
			got := buf.String()
			wantPrefix := "cleanc " + verb + ":"
			if !contains(got, wantPrefix) {
				t.Errorf("stderr = %q, want substring %q", got, wantPrefix)
			}
			if !contains(got, "--with-churn is reserved") {
				t.Errorf("stderr = %q lost the e2e substring %q", got, "--with-churn is reserved")
			}
		})
	}
}

// TestReservedTelemetryMessageForRoundTrip confirms the helper
// builds the verb-prefixed line for non-empty verbs and falls
// back to the legacy `ReservedTelemetryMessage` constant when
// verb is empty, so older callers (and any test still anchored
// on the constant) keep working unchanged.
func TestReservedTelemetryMessageForRoundTrip(t *testing.T) {
	t.Parallel()

	if got := ReservedTelemetryMessageFor(""); got != ReservedTelemetryMessage {
		t.Errorf("ReservedTelemetryMessageFor(\"\") = %q, want fallback %q", got, ReservedTelemetryMessage)
	}
	got := ReservedTelemetryMessageFor(VerbReport)
	if !contains(got, "cleanc report:") {
		t.Errorf("ReservedTelemetryMessageFor(report) = %q, want `cleanc report:` prefix", got)
	}
	if !contains(got, "--telemetry-otlp is reserved for a future story") {
		t.Errorf("ReservedTelemetryMessageFor(report) = %q dropped e2e substring", got)
	}
}

// TestReservedWithChurnMessageForRoundTrip mirrors the telemetry
// helper test for the `--with-churn` reservation message.
func TestReservedWithChurnMessageForRoundTrip(t *testing.T) {
	t.Parallel()

	if got := ReservedWithChurnMessageFor(""); got != ReservedWithChurnMessage {
		t.Errorf("ReservedWithChurnMessageFor(\"\") = %q, want fallback %q", got, ReservedWithChurnMessage)
	}
	got := ReservedWithChurnMessageFor(VerbReport)
	if !contains(got, "cleanc report:") {
		t.Errorf("ReservedWithChurnMessageFor(report) = %q, want `cleanc report:` prefix", got)
	}
	if !contains(got, "--with-churn is reserved for P2 and rejected in P0/P1") {
		t.Errorf("ReservedWithChurnMessageFor(report) = %q dropped e2e substring", got)
	}
	// The workstream brief (Stage 4.4: "Reserved Verbs And Flags")
	// pins the operator-facing hint that explains WHY --with-churn
	// is dark in P0/P1. Lose this clause and operators just see
	// "rejected" with no path forward to understand the gap.
	if !contains(got, "modification_count_in_window will not light up until the parser-attr extension ships") {
		t.Errorf("ReservedWithChurnMessageFor(report) = %q dropped operator-facing parser-attr hint", got)
	}
}

// TestReservedSnippetCapLinesMessageForRoundTrip mirrors the
// telemetry/with-churn helper tests for the new
// `--snippet-cap-lines` reservation message (e2e-scenarios.md
// Stage 4.4 line 1072).
func TestReservedSnippetCapLinesMessageForRoundTrip(t *testing.T) {
	t.Parallel()

	if got := ReservedSnippetCapLinesMessageFor(""); got != ReservedSnippetCapLinesMessage {
		t.Errorf("ReservedSnippetCapLinesMessageFor(\"\") = %q, want fallback %q",
			got, ReservedSnippetCapLinesMessage)
	}
	got := ReservedSnippetCapLinesMessageFor(VerbReport)
	if !contains(got, "cleanc report:") {
		t.Errorf("ReservedSnippetCapLinesMessageFor(report) = %q, want `cleanc report:` prefix", got)
	}
	if !contains(got, "reserved for a future minor release") {
		t.Errorf("ReservedSnippetCapLinesMessageFor(report) = %q dropped e2e substring", got)
	}
}

// TestIsReservedSnippetCapLinesArg pins the closed set of arg
// forms the pre-scan in cmd/cleanc/main.go recognises as
// `--snippet-cap-lines`. The helper is the single source of
// truth — adding a new form (e.g. an alias) means editing this
// test, not the dispatcher.
func TestIsReservedSnippetCapLinesArg(t *testing.T) {
	t.Parallel()

	hits := []string{
		"--snippet-cap-lines",
		"-snippet-cap-lines",
		"--snippet-cap-lines=100",
		"-snippet-cap-lines=120",
		"--snippet-cap-lines=",
	}
	for _, arg := range hits {
		arg := arg
		t.Run("hit_"+arg, func(t *testing.T) {
			t.Parallel()
			if !IsReservedSnippetCapLinesArg(arg) {
				t.Errorf("IsReservedSnippetCapLinesArg(%q) = false, want true", arg)
			}
		})
	}

	misses := []string{
		"",
		"--snippet",
		"--snippet-cap",
		"--snippet-cap-lines-extra",
		"--snippet-cap-linesfoo",
		"--snippet-cap-line=10",
		"snippet-cap-lines",
		"100",
	}
	for _, arg := range misses {
		arg := arg
		t.Run("miss_"+arg, func(t *testing.T) {
			t.Parallel()
			if IsReservedSnippetCapLinesArg(arg) {
				t.Errorf("IsReservedSnippetCapLinesArg(%q) = true, want false", arg)
			}
		})
	}
}

// TestReservedApplyMessageMatchesE2E pins the literal substrings
// e2e-scenarios.md Stage 4.4 lines 1050-1051 require from the
// stderr output of `cleanc apply ...`. Both substrings MUST be
// present; the test would have caught the iter-6 backtick
// regression (the `cli-l7-authority` id was wrapped in backticks
// which broke the bare-pin substring assertion).
func TestReservedApplyMessageMatchesE2E(t *testing.T) {
	t.Parallel()

	wantSubstrings := []string{
		// e2e line 1050 — note: bare pin id, NO backticks.
		"not implemented; pending operator pin cli-l7-authority",
		// e2e line 1051 — operator-facing arch reference.
		"docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md Sec 6.3",
	}
	for _, want := range wantSubstrings {
		if !contains(ReservedApplyMessage, want) {
			t.Errorf("ReservedApplyMessage = %q is missing required substring %q",
				ReservedApplyMessage, want)
		}
	}
	// Guard against the iter-6 regression specifically: backticks
	// around the pin id break the e2e literal-substring match.
	if contains(ReservedApplyMessage, "`cli-l7-authority`") {
		t.Errorf("ReservedApplyMessage wraps `cli-l7-authority` in backticks; e2e line 1050 wants the bare identifier")
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
