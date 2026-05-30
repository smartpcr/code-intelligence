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
// severity in the closed set parses successfully (the
// pipeline still stubs out at the end with exit 70, so the
// test only asserts the flag parser accepts the value).
func TestAnalyzeAcceptsAllValidExitOnLevels(t *testing.T) {
	t.Parallel()

	for _, lvl := range flags.ExitOnLevels {
		lvl := lvl
		t.Run(lvl, func(t *testing.T) {
			t.Parallel()
			_, stderr, code := captureRun("analyze", ".", "--exit-on", lvl)
			if code != flags.ExitInternalError {
				t.Errorf("exit code = %d, want %d (pipeline stub); stderr=%s",
					code, flags.ExitInternalError, stderr)
			}
			if strings.Contains(stderr, "must be one of") {
				t.Errorf("stderr unexpectedly contains exit-on usage phrase for valid level %q\nstderr=%s", lvl, stderr)
			}
		})
	}
}

// TestAnalyzeStubExitsInternalError validates that an
// otherwise-valid `cleanc analyze <path>` invocation in the
// Stage 1.1 skeleton exits with EX_SOFTWARE (70) and an
// explicit "not yet wired" stderr line -- the skeleton must
// not claim success for unimplemented behaviour.
func TestAnalyzeStubExitsInternalError(t *testing.T) {
	t.Parallel()

	_, stderr, code := captureRun("analyze", ".")
	if code != flags.ExitInternalError {
		t.Errorf("exit code = %d, want %d", code, flags.ExitInternalError)
	}
	if !strings.Contains(stderr, "not yet wired") {
		t.Errorf("stderr missing 'not yet wired' notice\nstderr=%s", stderr)
	}
}

// TestApplyReservedExitsUsage validates that `cleanc apply`
// returns ExitUsage with the operator-pin reserved message.
func TestApplyReservedExitsUsage(t *testing.T) {
	t.Parallel()

	_, stderr, code := captureRun("apply")
	if code != flags.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, flags.ExitUsage)
	}
	if !strings.Contains(stderr, "pending operator pin `cli-l7-authority`") {
		t.Errorf("stderr missing reserved-apply message\nstderr=%s", stderr)
	}
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
