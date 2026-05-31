package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/flags"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// TestFindingsTriggerExitSeverityMatrix pins the
// `--exit-on <sev>` -> finding-severity trigger matrix
// tech-spec Sec 8.6 C9 requires: distinct ranks for info /
// warn / block so an info-only finding does NOT satisfy
// `--exit-on=warn` and a warn-only finding does NOT satisfy
// `--exit-on=block` (iter-2 evaluator item 3).
//
// The verdict argument is fixed to [rule_engine.VerdictPass]
// for every row because the engine collapses info-only
// findings to `Pass`; the test isolates the per-finding
// severity loop in [findingsTriggerExit].
func TestFindingsTriggerExitSeverityMatrix(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		severity  steward.Severity
		threshold string
		want      bool
	}{
		// info finding
		{"info-finding/exit=info", steward.SeverityInfo, "info", true},
		{"info-finding/exit=warn", steward.SeverityInfo, "warn", false},
		{"info-finding/exit=block", steward.SeverityInfo, "block", false},
		// warn finding
		{"warn-finding/exit=info", steward.SeverityWarn, "info", true},
		{"warn-finding/exit=warn", steward.SeverityWarn, "warn", true},
		{"warn-finding/exit=block", steward.SeverityWarn, "block", false},
		// block finding
		{"block-finding/exit=info", steward.SeverityBlock, "info", true},
		{"block-finding/exit=warn", steward.SeverityBlock, "warn", true},
		{"block-finding/exit=block", steward.SeverityBlock, "block", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			findings := []rule_engine.Finding{{Severity: tc.severity}}
			got := findingsTriggerExit(rule_engine.VerdictPass, findings, tc.threshold)
			if got != tc.want {
				t.Fatalf("findingsTriggerExit(Pass, [%s], %q) = %v, want %v",
					tc.severity, tc.threshold, got, tc.want)
			}
		})
	}
}

// TestFindingsTriggerExitEmpty pins the zero-findings +
// pass-verdict case: regardless of threshold (other than
// the always-false unrecognised default), no trigger fires.
func TestFindingsTriggerExitEmpty(t *testing.T) {
	t.Parallel()
	for _, th := range []string{"info", "warn", "block"} {
		if findingsTriggerExit(rule_engine.VerdictPass, nil, th) {
			t.Errorf("empty findings + pass verdict triggered exit on threshold=%q", th)
		}
	}
}

// TestFindingsTriggerExitVerdictAxis pins the verdict
// axis: a Warn/Block verdict with NO findings still
// triggers `--exit-on=warn`/`block` so a CI gate that only
// observes the aggregate verdict (e.g. a thresholds
// policy-version row) still fires.
func TestFindingsTriggerExitVerdictAxis(t *testing.T) {
	t.Parallel()
	cases := []struct {
		verdict   rule_engine.Verdict
		threshold string
		want      bool
	}{
		{rule_engine.VerdictWarn, "info", true},
		{rule_engine.VerdictWarn, "warn", true},
		{rule_engine.VerdictWarn, "block", false},
		{rule_engine.VerdictBlock, "info", true},
		{rule_engine.VerdictBlock, "warn", true},
		{rule_engine.VerdictBlock, "block", true},
	}
	for _, tc := range cases {
		if got := findingsTriggerExit(tc.verdict, nil, tc.threshold); got != tc.want {
			t.Errorf("findingsTriggerExit(%s, nil, %q) = %v, want %v",
				tc.verdict, tc.threshold, got, tc.want)
		}
	}
}

// TestRecoverDispatchConvertsPanic pins the
// implementation-plan Stage 3.3 panic contract: a panic
// raised inside the dispatcher surfaces as
// [flags.ExitInternalError] with the panic value AND a Go
// stack trace written to stderr (iter-2 evaluator item 4).
// Exercises [recoverDispatch] directly with a panicking
// closure so the recovery frame is testable without making
// a real sub-command panic.
func TestRecoverDispatchConvertsPanic(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := recoverDispatch(func() int {
		panic("synthetic boom")
	}, &stderr)
	if code != flags.ExitInternalError {
		t.Fatalf("exit code = %d, want %d (ExitInternalError)", code, flags.ExitInternalError)
	}
	out := stderr.String()
	if !strings.Contains(out, "cleanc: panic: synthetic boom") {
		t.Errorf("stderr missing panic banner; got=%q", out)
	}
	// runtime.Stack output contains "goroutine" + the
	// recovering function's frame; either is a sufficient
	// stack-trace presence assertion.
	if !strings.Contains(out, "goroutine") {
		t.Errorf("stderr missing goroutine stack frame; got=%q", out)
	}
}

// TestRecoverDispatchPassesThroughExitCode pins the
// happy-path contract: when the closure returns normally,
// [recoverDispatch] returns the closure's exit code
// verbatim and writes nothing to stderr.
func TestRecoverDispatchPassesThroughExitCode(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	code := recoverDispatch(func() int { return 42 }, &stderr)
	if code != 42 {
		t.Errorf("exit code = %d, want 42", code)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want empty", stderr.String())
	}
}
