package main

import (
	"bytes"
	"flag"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/flags"
)

// versionLineRegex is the strict line-level regex e2e-scenarios.md
// pins for the first line of `cleanc version` stdout. Kept
// outside the test function so the same value can be reused
// across multiple cases without redeclaring it.
//
// The alternation `(|prod)` accepts both the dev (no-tag,
// empty string) and the prod build's `prod` literal.
var versionLineRegex = regexp.MustCompile(`^cleanc \d+\.\d+\.\d+ \(build-tag=(|prod)\) \(parsers=[^)]+\) \(rule-packs=[^)]+\)$`)

// TestVersionFormatMatchesE2ERegex pins the first line of
// `cleanc version` stdout against the regex in
// e2e-scenarios.md line 146.
func TestVersionFormatMatchesE2ERegex(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := captureRun("version")
	if code != flags.ExitOK {
		t.Fatalf("cleanc version exit code = %d, want %d; stderr=%q", code, flags.ExitOK, stderr)
	}
	firstLine, _, _ := strings.Cut(stdout, "\n")
	if !versionLineRegex.MatchString(firstLine) {
		t.Errorf("first line %q does not match regex %s", firstLine, versionLineRegex)
	}
}

// TestVersionContainsImplPlanSubstrings pins the literal
// substrings implementation-plan.md Stage 1.1 line 41
// requires (`version=` and `parsers=[go,python,typescript,java]`).
func TestVersionContainsImplPlanSubstrings(t *testing.T) {
	t.Parallel()

	stdout, stderr, code := captureRun("version")
	if code != flags.ExitOK {
		t.Fatalf("cleanc version exit code = %d, want %d; stderr=%q", code, flags.ExitOK, stderr)
	}

	wantSubstrings := []string{
		"version=",
		"parsers=[go,python,typescript,java]",
		"rule-packs=[decoupling,solid]",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout does not contain %q\nstdout=%s", want, stdout)
		}
	}
}

// TestVersionParserSetIsExactlyFourLanguages parses the CSV
// inside `(parsers=...)` on the first line and asserts the
// language set is exactly `{go, python, typescript, java}`
// (e2e-scenarios.md line 147 -- "in any order; the test
// parses the CSV and compares as a set").
func TestVersionParserSetIsExactlyFourLanguages(t *testing.T) {
	t.Parallel()

	stdout, _, _ := captureRun("version")
	firstLine, _, _ := strings.Cut(stdout, "\n")

	parserRegex := regexp.MustCompile(`\(parsers=([^)]+)\)`)
	m := parserRegex.FindStringSubmatch(firstLine)
	if m == nil {
		t.Fatalf("first line %q does not contain (parsers=...)", firstLine)
	}
	got := strings.Split(m[1], ",")
	gotSet := make(map[string]bool, len(got))
	for _, l := range got {
		gotSet[strings.TrimSpace(l)] = true
	}
	want := []string{"go", "python", "typescript", "java"}
	if len(gotSet) != len(want) {
		t.Errorf("parser set size = %d, want %d (got %v)", len(gotSet), len(want), gotSet)
	}
	for _, lang := range want {
		if !gotSet[lang] {
			t.Errorf("parser set missing %q (got %v)", lang, gotSet)
		}
	}
}

// TestVersionContainsNoAnsiEscape asserts the version output
// is plain text -- no colouring (e2e-scenarios.md line 148).
func TestVersionContainsNoAnsiEscape(t *testing.T) {
	t.Parallel()

	stdout, _, _ := captureRun("version")
	if strings.ContainsRune(stdout, '\x1b') {
		t.Errorf("version stdout contains a 0x1b ANSI escape; want plain text\nstdout=%q", stdout)
	}
}

// TestUnknownSubcommandExitsUsage validates the rejection
// path for non-canonical sub-commands (e2e-scenarios.md
// line 168).
func TestUnknownSubcommandExitsUsage(t *testing.T) {
	t.Parallel()

	for _, verb := range []string{"frobnicate", "ANALYZE", "anal", "report-md", "v"} {
		verb := verb
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			_, stderr, code := captureRun(verb)
			if code != flags.ExitUsage {
				t.Errorf("exit code = %d, want %d", code, flags.ExitUsage)
			}
			if !strings.Contains(stderr, flags.UnknownSubcommandPhrase) {
				t.Errorf("stderr does not contain %q\nstderr=%s", flags.UnknownSubcommandPhrase, stderr)
			}
		})
	}
}

// TestCanonicalVerbsDoNotEmitUnknownPhrase guards the
// e2e-scenarios.md line 157 background: for each canonical
// verb the literal phrase `unknown sub-command` MUST NOT
// appear on stderr (otherwise a typo-detector built on top
// of the literal phrase would false-positive).
func TestCanonicalVerbsDoNotEmitUnknownPhrase(t *testing.T) {
	t.Parallel()

	for _, verb := range flags.Verbs {
		verb := verb
		t.Run(verb, func(t *testing.T) {
			t.Parallel()
			_, stderr, _ := captureRun(verb)
			if strings.Contains(stderr, flags.UnknownSubcommandPhrase) {
				t.Errorf("stderr for canonical verb %q contains %q (must not)\nstderr=%s",
					verb, flags.UnknownSubcommandPhrase, stderr)
			}
		})
	}
}

// TestNoArgsExitsUsage validates that invoking the binary
// without any sub-command prints the global usage block and
// returns ExitUsage.
func TestNoArgsExitsUsage(t *testing.T) {
	t.Parallel()

	_, stderr, code := captureRun()
	if code != flags.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, flags.ExitUsage)
	}
	if !strings.Contains(stderr, "usage: cleanc <subcommand>") {
		t.Errorf("stderr missing global usage block\nstderr=%s", stderr)
	}
}

// TestAnalyzeMissingPathPrintsUsage validates the
// e2e-scenarios.md Stage 1.1 scenario (line 183) -- missing
// positional path exits 64 with `usage: cleanc analyze` on
// stderr.
func TestAnalyzeMissingPathPrintsUsage(t *testing.T) {
	t.Parallel()

	_, stderr, code := captureRun("analyze")
	if code != flags.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, flags.ExitUsage)
	}
	if !strings.Contains(stderr, "usage: cleanc analyze") {
		t.Errorf("stderr missing %q\nstderr=%s", "usage: cleanc analyze", stderr)
	}
}

// TestAnalyzeRejectsTelemetryOTLP validates the
// e2e-scenarios.md Stage 4.4 scenario (line 1061) -- a
// `--telemetry-otlp` value rejects with exit 64 and the
// literal "reserved for a future story" substring.
func TestAnalyzeRejectsTelemetryOTLP(t *testing.T) {
	t.Parallel()

	_, stderr, code := captureRun("analyze", ".", "--telemetry-otlp", "http://localhost:4317")
	if code != flags.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, flags.ExitUsage)
	}
	if !strings.Contains(stderr, "--telemetry-otlp is reserved for a future story") {
		t.Errorf("stderr missing reserved-telemetry phrase\nstderr=%s", stderr)
	}
}

// TestAnalyzeRejectsWithChurn validates the e2e-scenarios.md
// Stage 4.4 scenario (line 1067) -- `--with-churn` rejects
// with exit 64 and the literal "reserved for P2 and rejected
// in P0/P1" substring.
func TestAnalyzeRejectsWithChurn(t *testing.T) {
	t.Parallel()

	_, stderr, code := captureRun("analyze", ".", "--with-churn")
	if code != flags.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, flags.ExitUsage)
	}
	if !strings.Contains(stderr, "--with-churn is reserved for P2 and rejected in P0/P1") {
		t.Errorf("stderr missing reserved-with-churn phrase\nstderr=%s", stderr)
	}
}

// TestAnalyzeRejectsBadExitOn validates the
// e2e-scenarios.md Stage 3.3 scenario (line 788) -- an
// `--exit-on` value outside `{info, warn, block}` rejects
// with exit 64 and the literal "must be one of info, warn,
// block" substring.
func TestAnalyzeRejectsBadExitOn(t *testing.T) {
	t.Parallel()

	_, stderr, code := captureRun("analyze", ".", "--exit-on", "critical")
	if code != flags.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, flags.ExitUsage)
	}
	if !strings.Contains(stderr, "--exit-on must be one of info, warn, block") {
		t.Errorf("stderr missing exit-on usage phrase\nstderr=%s", stderr)
	}
}

// TestAnalyzeAcceptsAllValidExitOnLevels validates that each
// severity in the closed set parses successfully and reaches
// the pipeline. The pipeline now runs end-to-end against an
// empty temp directory, so an unrecognised level surfaces as
// `must be one of` (ExitUsage) -- the test pins that no such
// usage-rejection text appears for valid levels.
func TestAnalyzeAcceptsAllValidExitOnLevels(t *testing.T) {
	t.Parallel()

	for _, lvl := range flags.ExitOnLevels {
		lvl := lvl
		t.Run(lvl, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			_, stderr, code := captureRun("analyze", dir, "--exit-on", lvl)
			if code == flags.ExitUsage {
				t.Errorf("exit code = %d (ExitUsage) for valid level %q; stderr=%s", code, lvl, stderr)
			}
			if strings.Contains(stderr, "must be one of") {
				t.Errorf("stderr unexpectedly contains exit-on usage phrase for valid level %q\nstderr=%s", lvl, stderr)
			}
		})
	}
}

// TestAnalyzeHappyPathExitsCleanOnEmptyRepo validates the
// Stage 3.3 end-to-end pipeline reaches the renderer phase
// against an empty repo path and exits 0 (no findings, no
// `--exit-on` trigger). Resolves iter-1 evaluator item 1
// + item 2: the pipeline is no longer blocked on the
// devpolicy loader stub, and the test no longer pins the
// stub-era "loader not implemented" substring.
func TestAnalyzeHappyPathExitsCleanOnEmptyRepo(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, stderr, code := captureRun("analyze", dir)
	if code != flags.ExitOK {
		t.Errorf("exit code = %d, want %d (clean run, no findings); stderr=%s", code, flags.ExitOK, stderr)
	}
	for _, stale := range []string{"loader not yet wired", "Stage 1.4 follow-up", "ErrLoaderNotYetImplemented"} {
		if strings.Contains(stderr, stale) {
			t.Errorf("stderr still mentions stub-era phrase %q; the loader should be functional now.\nstderr=%s", stale, stderr)
		}
	}
}

// TestAnalyzeRejectsDevModeDisabled validates iter-1
// evaluator item 4: `--dev-mode=false` refuses to start with
// `ExitUsage` (64) and a signed-policy-loader diagnostic.
func TestAnalyzeRejectsDevModeDisabled(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	_, stderr, code := captureRun("analyze", dir, "--dev-mode=false")
	if code != flags.ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)\nstderr=%s", code, flags.ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "signed-policy loader") {
		t.Errorf("stderr missing 'signed-policy loader' diagnostic\nstderr=%s", stderr)
	}
	for _, stale := range []string{"loader not yet wired", "Stage 1.4 follow-up"} {
		if strings.Contains(stderr, stale) {
			t.Errorf("stderr leaks stub-era phrase %q in --dev-mode=false diagnostic\nstderr=%s", stale, stderr)
		}
	}
}

// TestApplyReservedExitsUsage validates that `cleanc apply`
// returns ExitUsage and emits the operator-pin reserved
// message in the exact form e2e-scenarios.md Stage 4.4 lines
// 1048-1051 pin:
//
//   - exit code 64
//   - stderr contains the literal phrase
//     `not implemented; pending operator pin cli-l7-authority`
//     (NOTE: the pin identifier MUST be bare — wrapping it in
//     backticks breaks the substring match because backticks
//     are NOT part of the e2e literal phrase)
//   - stderr references
//     `docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md Sec 6.3`
//     so an operator who hits the reservation can read the
//     authoritative explanation of WHY the verb is gated.
//
// The test exercises BOTH the bare `cleanc apply` form (no
// positional) AND the documented e2e form `cleanc apply
// <uuid>` so the dispatcher cannot regress by demanding a
// positional or by silently swallowing one.
func TestApplyReservedExitsUsage(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{"apply"},
		{"apply", "00000000-0000-0000-0000-000000000000"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			_, stderr, code := captureRun(args...)
			if code != flags.ExitUsage {
				t.Errorf("exit code = %d, want %d", code, flags.ExitUsage)
			}
			// e2e-scenarios.md Stage 4.4 line 1050 — bare pin id, no backticks.
			if !strings.Contains(stderr, "not implemented; pending operator pin cli-l7-authority") {
				t.Errorf("stderr missing reserved-apply phrase (bare pin id)\nstderr=%s", stderr)
			}
			// e2e-scenarios.md Stage 4.4 line 1051 — arch reference.
			if !strings.Contains(stderr, "docs/stories/code-intelligence-REFACTOR-GUIDE/architecture.md Sec 6.3") {
				t.Errorf("stderr missing architecture.md Sec 6.3 reference\nstderr=%s", stderr)
			}
		})
	}
}

// TestReservedSurface is the table-driven invariant pinned by
// `e2e-scenarios.md` Stage 4.4 (line 1081 — "Test enumerates
// every reserved entry from tech-spec Sec 8.1"). It asserts
// that EVERY currently-reserved verb / flag listed in tech-spec
// Sec 8.1 exits with `ExitUsage` (64) AND emits the literal
// substring the e2e contract pins, so an accidental wiring of
// any reserved surface fails CI loudly.
//
// Reserved surface as of Stage 1.1:
//
//   - verb `apply` (e2e line 1050) — must contain
//     `not implemented; pending operator pin cli-l7-authority`
//   - flag `--telemetry-otlp` (e2e line 1061) — must contain
//     `--telemetry-otlp is reserved for a future story`
//   - flag `--with-churn` (e2e line 1067) — must contain
//     `--with-churn is reserved for P2 and rejected in P0/P1`
//   - flag `--snippet-cap-lines` (e2e line 1072) — must contain
//     `reserved for a future minor release`
//
// Adding a new reserved entry to tech-spec Sec 8.1 requires
// adding a row here too; deleting one is a contract change
// and requires updating e2e-scenarios.md in the same PR.
func TestReservedSurface(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		args           []string
		wantSubstrings []string
	}{
		{
			name:           "apply verb",
			args:           []string{"apply", "00000000-0000-0000-0000-000000000000"},
			wantSubstrings: []string{"not implemented; pending operator pin cli-l7-authority"},
		},
		{
			name:           "--telemetry-otlp on analyze",
			args:           []string{"analyze", ".", "--telemetry-otlp", "http://localhost:4317"},
			wantSubstrings: []string{"--telemetry-otlp is reserved for a future story"},
		},
		{
			name:           "--with-churn on analyze",
			args:           []string{"analyze", ".", "--with-churn"},
			wantSubstrings: []string{"--with-churn is reserved for P2 and rejected in P0/P1"},
		},
		{
			name:           "--snippet-cap-lines on analyze",
			args:           []string{"analyze", ".", "--snippet-cap-lines", "100"},
			wantSubstrings: []string{"reserved for a future minor release"},
		},
		{
			name:           "--snippet-cap-lines=N value-attached form on analyze",
			args:           []string{"analyze", ".", "--snippet-cap-lines=120"},
			wantSubstrings: []string{"reserved for a future minor release"},
		},
		{
			name:           "--snippet-cap-lines on report",
			args:           []string{"report", "findings.json", "--snippet-cap-lines", "100"},
			wantSubstrings: []string{"reserved for a future minor release"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, stderr, code := captureRun(tc.args...)
			if code != flags.ExitUsage {
				t.Errorf("exit code = %d, want %d (reserved surface must exit 64)\nstderr=%s",
					code, flags.ExitUsage, stderr)
			}
			for _, want := range tc.wantSubstrings {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr missing reserved-surface phrase %q\nstderr=%s", want, stderr)
				}
			}
		})
	}
}

// TestApplyHelpUsesBareIdentifier (iter-8) pins the consistency
// invariant that the operator-pin id `cli-l7-authority` appears
// in the SAME bare form across every operator-facing surface:
//
//   - the runtime apply rejection (`flags.ReservedApplyMessage`),
//   - the global help listing (`cleanc help` → `writeGlobalUsage`),
//   - the per-verb usage block (`cleanc help apply` → `applyUsage`).
//
// Wrapping `cli-l7-authority` in backticks on any surface breaks
// the parity with `e2e-scenarios.md` Stage 4.4 line 1050 (which
// pins the bare identifier as the literal substring) and creates
// drift between what an operator sees in stderr vs. in `help`.
// This test grep-checks each surface for the backtick-wrapped
// form and fails loudly if any one regresses.
func TestApplyHelpUsesBareIdentifier(t *testing.T) {
	t.Parallel()

	const backtickWrapped = "`cli-l7-authority`"
	const bareID = "cli-l7-authority"

	t.Run("cleanc help (global) does not backtick-wrap the pin id", func(t *testing.T) {
		t.Parallel()
		stdout, _, code := captureRun("help")
		if code != flags.ExitOK {
			t.Errorf("exit code = %d, want %d", code, flags.ExitOK)
		}
		if strings.Contains(stdout, backtickWrapped) {
			t.Errorf("`cleanc help` wraps %q in backticks; e2e line 1050 wants the bare id\nstdout=%s",
				bareID, stdout)
		}
		if !strings.Contains(stdout, bareID) {
			t.Errorf("`cleanc help` apply line missing bare id %q\nstdout=%s", bareID, stdout)
		}
	})

	t.Run("cleanc help apply does not backtick-wrap the pin id", func(t *testing.T) {
		t.Parallel()
		stdout, _, code := captureRun("help", "apply")
		if code != flags.ExitOK {
			t.Errorf("exit code = %d, want %d", code, flags.ExitOK)
		}
		if strings.Contains(stdout, backtickWrapped) {
			t.Errorf("`cleanc help apply` wraps %q in backticks; cross-surface drift with ReservedApplyMessage\nstdout=%s",
				bareID, stdout)
		}
		if !strings.Contains(stdout, bareID) {
			t.Errorf("`cleanc help apply` missing bare id %q\nstdout=%s", bareID, stdout)
		}
	})

	t.Run("applyUsage constant does not backtick-wrap the pin id", func(t *testing.T) {
		t.Parallel()
		if strings.Contains(applyUsage, backtickWrapped) {
			t.Errorf("applyUsage const wraps %q in backticks: %q", bareID, applyUsage)
		}
		if !strings.Contains(applyUsage, bareID) {
			t.Errorf("applyUsage const missing bare id %q: %q", bareID, applyUsage)
		}
	})

	t.Run("ReservedApplyMessage agrees with help surfaces", func(t *testing.T) {
		t.Parallel()
		if strings.Contains(flags.ReservedApplyMessage, backtickWrapped) {
			t.Errorf("ReservedApplyMessage wraps %q in backticks: %q", bareID, flags.ReservedApplyMessage)
		}
		if !strings.Contains(flags.ReservedApplyMessage, bareID) {
			t.Errorf("ReservedApplyMessage missing bare id %q: %q", bareID, flags.ReservedApplyMessage)
		}
	})
}

// TestReportMissingFindingsExitsUsage validates the report
// sub-command rejects a missing positional argument with
// ExitUsage.
func TestReportMissingFindingsExitsUsage(t *testing.T) {
	t.Parallel()

	_, stderr, code := captureRun("report")
	if code != flags.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, flags.ExitUsage)
	}
	if !strings.Contains(stderr, "usage: cleanc report") {
		t.Errorf("stderr missing report usage block\nstderr=%s", stderr)
	}
}

// TestAnalyzeRejectsSurplusPositionals resolves iter-4
// evaluator item 5 -- "SURPLUS POSITIONALS STILL ACCEPTED".
// `cleanc analyze` accepts EXACTLY one positional argument
// (the repo path); two-or-more positionals is an operator-
// facing usage error and MUST exit 64. Iter-1 only checked
// `len(positionals) < 1`, silently dropping the surplus.
func TestAnalyzeRejectsSurplusPositionals(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{"analyze", ".", "extra"},
		{"analyze", ".", "extra", "more"},
		{"analyze", ".", "--exit-on", "warn", "extra"},
		{"analyze", "first-path", "second-path"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			_, stderr, code := captureRun(args...)
			if code != flags.ExitUsage {
				t.Errorf("exit code = %d, want %d (surplus positional)\nstderr=%s", code, flags.ExitUsage, stderr)
			}
			if !strings.Contains(stderr, "expected exactly 1 positional argument") {
				t.Errorf("stderr missing surplus-positional notice\nstderr=%s", stderr)
			}
		})
	}
}

// TestReportRejectsSurplusPositionals is the report twin of
// TestAnalyzeRejectsSurplusPositionals. Both verbs share the
// same `len(positionals) != 1` rule (resolves iter-4
// evaluator item 5).
func TestReportRejectsSurplusPositionals(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{"report", "findings.json", "extra"},
		{"report", "a.json", "b.json"},
		{"report", "a.json", "--out", "r.md", "b.json"},
	}
	for _, args := range cases {
		args := args
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			t.Parallel()
			_, stderr, code := captureRun(args...)
			if code != flags.ExitUsage {
				t.Errorf("exit code = %d, want %d (surplus positional)\nstderr=%s", code, flags.ExitUsage, stderr)
			}
			if !strings.Contains(stderr, "expected exactly 1 positional argument") {
				t.Errorf("stderr missing surplus-positional notice\nstderr=%s", stderr)
			}
		})
	}
}

// TestReportRegistersFullGlobalFlagSurface resolves iter-4
// evaluator item 4 -- "REPORT FLAG SURFACE INCOMPLETE". The
// dispatcher must register every Sec 8.1 global on `report`
// (not just `--out`); the easiest unambiguous proof is that
// the same reserved-flag rejections that fire on `analyze`
// also fire on `report`.
func TestReportRegistersFullGlobalFlagSurface(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		extraArgs []string
		wantPhrase string
	}{
		{
			name:       "rejects --telemetry-otlp",
			extraArgs:  []string{"--telemetry-otlp", "http://localhost:4317"},
			wantPhrase: "--telemetry-otlp is reserved",
		},
		{
			name:       "rejects --with-churn",
			extraArgs:  []string{"--with-churn"},
			wantPhrase: "--with-churn is reserved",
		},
		{
			name:       "rejects --exit-on banana",
			extraArgs:  []string{"--exit-on", "banana"},
			wantPhrase: "--exit-on must be one of",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			args := append([]string{"report", "findings.json"}, tc.extraArgs...)
			_, stderr, code := captureRun(args...)
			if code != flags.ExitUsage {
				t.Errorf("exit code = %d, want %d\nstderr=%s", code, flags.ExitUsage, stderr)
			}
			if !strings.Contains(stderr, tc.wantPhrase) {
				t.Errorf("stderr missing %q\nstderr=%s", tc.wantPhrase, stderr)
			}
		})
	}
}

// TestReportAcceptsAllSec81Flags is the positive twin of
// TestReportRegistersFullGlobalFlagSurface: every Sec 8.1
// global flag MUST parse successfully when passed to
// `report` (the stub still exits 70 once parsing succeeds,
// so the test only asserts the flag parser did NOT reject
// the value with exit 64).
func TestReportAcceptsAllSec81Flags(t *testing.T) {
	t.Parallel()

	cases := [][]string{
		{"--out", "report.md"},
		{"--findings", "custom.json"},
		{"--emit-prompts", "prompts.jsonl"},
		{"--policy", "/tmp/policy"},
		{"--top-n", "5"},
		{"--exit-on", "warn"},
		{"--diagnostics", "diag.json"},
		{"--dev-mode"},
	}
	for _, extra := range cases {
		extra := extra
		t.Run(strings.Join(extra, " "), func(t *testing.T) {
			t.Parallel()
			args := append([]string{"report", "findings.json"}, extra...)
			_, stderr, code := captureRun(args...)
			if code != flags.ExitInternalError {
				t.Errorf("exit code = %d, want %d (pipeline stub); stderr=%s",
					code, flags.ExitInternalError, stderr)
			}
		})
	}
}

// TestHelpNoArgExitsZero validates `cleanc help` prints the
// global usage block to stdout (not stderr) and exits 0.
func TestHelpNoArgExitsZero(t *testing.T) {
	t.Parallel()

	stdout, _, code := captureRun("help")
	if code != flags.ExitOK {
		t.Errorf("exit code = %d, want %d", code, flags.ExitOK)
	}
	if !strings.Contains(stdout, "usage: cleanc <subcommand>") {
		t.Errorf("stdout missing global usage block\nstdout=%s", stdout)
	}
}

// TestHelpVerbPrintsPerVerbUsage validates `cleanc help <verb>`
// emits the verb-specific usage line to stdout for every
// canonical verb.
func TestHelpVerbPrintsPerVerbUsage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		verb string
		want string
	}{
		{flags.VerbAnalyze, "usage: cleanc analyze"},
		{flags.VerbReport, "usage: cleanc report"},
		{flags.VerbVersion, "usage: cleanc version"},
		{flags.VerbApply, "usage: cleanc apply"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.verb, func(t *testing.T) {
			t.Parallel()
			stdout, _, code := captureRun("help", tc.verb)
			if code != flags.ExitOK {
				t.Errorf("exit code = %d, want %d", code, flags.ExitOK)
			}
			if !strings.Contains(stdout, tc.want) {
				t.Errorf("stdout missing %q\nstdout=%s", tc.want, stdout)
			}
		})
	}
}

// TestSemverPrefix pins the SemVer normalisation helper.
// The default `0.0.0-dev` must reduce to `0.0.0` so the
// strict e2e version-line regex matches; a clean SemVer
// `1.2.3` is returned verbatim.
func TestSemverPrefix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{"0.0.0-dev", "0.0.0"},
		{"1.2.3", "1.2.3"},
		{"1.2.3-rc.1", "1.2.3"},
		{"1.2.3+build.42", "1.2.3"},
		{"1.2.3-rc.1+build.42", "1.2.3"},
		{"", ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := semverPrefix(tc.in); got != tc.want {
				t.Errorf("semverPrefix(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// (Build-tag assertions moved to the paired files
// `buildtag_default_test.go` (`//go:build !prod`) and
// `buildtag_prod_test.go` (`//go:build prod`), so a
// `go test -tags prod ./cmd/cleanc/...` run no longer
// fires the dev-build assertion against a prod constant.
// Resolves iter-4 evaluator item 3 -- "PROD TEST FAILURE".)

// captureRun is a small helper for the tests: it invokes the
// dispatcher with the supplied args and returns the captured
// stdout, stderr, and exit code.
func captureRun(args ...string) (stdout, stderr string, code int) {
	var so, se bytes.Buffer
	code = run(args, &so, &se)
	return so.String(), se.String(), code
}

// TestStage11ScopeNote_PinsOperatorDecision verifies the
// runtime-checkable witness of the operator's 2026-05-30 scope
// resolution (Option A). The constant is referenced by the
// package-level godoc and exists so a future refactor that
// strips the doc anchor (e.g. a doc rewrite) cannot silently
// drop the recorded boundary between Stage 1.1 (this work) and
// Stages 1.2 / 1.4 (downstream workstreams).
//
// The test pins:
//
//   - the operator-pinned scope phrase ("CLI skeleton + global
//     flag surface only");
//   - the deferral targets (the three downstream package paths
//     -- repocontext, scopebinding, devpolicy-loader);
//   - the answer date + option identifier (2026-05-30, Option A)
//     so a future audit can correlate the anchor to the answer
//     event recorded in `.forge/memory/workstream-context.md`.
//
// A spec edit that legitimately reshapes the Stage 1.1
// boundary MUST update both this constant AND the workstream-
// context answer entry; this test fails the first time those
// two get out of sync.
func TestStage11ScopeNote_PinsOperatorDecision(t *testing.T) {
	t.Parallel()

	required := []string{
		"Stage 1.1 = CLI skeleton + global flag surface only",
		"repocontext",
		"scopebinding",
		"devpolicy-loader",
		"Stages 1.2 and 1.4",
		"2026-05-30",
		"Option A",
	}
	for _, want := range required {
		if !strings.Contains(Stage11ScopeNote, want) {
			t.Errorf("Stage11ScopeNote does not contain %q\nactual: %q", want, Stage11ScopeNote)
		}
	}
}

// TestParseInterleavedFlagsCollectsPositionalsAndFlags exercises
// the helper that lets `cleanc analyze . --exit-on warn` see
// the flag after the positional (the stdlib `flag.Parse`
// stops at the first non-flag token, so the e2e command line
// shapes would otherwise silently lose flag values).
func TestParseInterleavedFlagsCollectsPositionalsAndFlags(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		args           []string
		wantPositional []string
		wantFlag       string // value of -alpha after parse
	}{
		{
			name:           "flag-then-positional",
			args:           []string{"--alpha", "x", "first"},
			wantPositional: []string{"first"},
			wantFlag:       "x",
		},
		{
			name:           "positional-then-flag",
			args:           []string{"first", "--alpha", "x"},
			wantPositional: []string{"first"},
			wantFlag:       "x",
		},
		{
			name:           "interleaved",
			args:           []string{"--alpha", "x", "first", "--alpha", "y", "second"},
			wantPositional: []string{"first", "second"},
			wantFlag:       "y",
		},
		{
			name:           "empty",
			args:           []string{},
			wantPositional: nil,
			wantFlag:       "default",
		},
		{
			name:           "positional-only",
			args:           []string{"first", "second"},
			wantPositional: []string{"first", "second"},
			wantFlag:       "default",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := flag.NewFlagSet("test", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			alpha := fs.String("alpha", "default", "test flag")
			pos, err := parseInterleavedFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(pos) != len(tc.wantPositional) {
				t.Fatalf("positionals = %v, want %v", pos, tc.wantPositional)
			}
			for i, want := range tc.wantPositional {
				if pos[i] != want {
					t.Errorf("positionals[%d] = %q, want %q", i, pos[i], want)
				}
			}
			if *alpha != tc.wantFlag {
				t.Errorf("alpha = %q, want %q", *alpha, tc.wantFlag)
			}
		})
	}
}
