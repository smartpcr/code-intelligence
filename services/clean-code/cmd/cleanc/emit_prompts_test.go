// -----------------------------------------------------------------------
// <copyright file="emit_prompts_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/flags"
)

// TestAnalyzeRejectsBareEmitPrompts validates the Stage 4.3
// contract: invoking `cleanc analyze` with `--emit-prompts`
// supplied but no value (either as the final token, OR with
// another flag as the successor) exits 64 with the literal
// stderr line pinned by the workstream brief.
func TestAnalyzeRejectsBareEmitPrompts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	cases := []struct {
		name string
		args []string
	}{
		{
			name: "bare-last-arg",
			args: []string{"analyze", dir, "--emit-prompts"},
		},
		{
			name: "single-dash-bare-last-arg",
			args: []string{"analyze", dir, "-emit-prompts"},
		},
		{
			name: "bare-followed-by-other-flag",
			args: []string{"analyze", dir, "--emit-prompts", "--findings", "f.json"},
		},
		{
			name: "bare-followed-by-end-of-flags-sentinel",
			args: []string{"analyze", dir, "--emit-prompts", "--"},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, stderr, code := captureRun(tc.args...)
			if code != flags.ExitUsage {
				t.Errorf("exit code = %d, want %d\nstderr=%s", code, flags.ExitUsage, stderr)
			}
			want := "--emit-prompts requires a path or '-' for stdout"
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr missing %q\nstderr=%s", want, stderr)
			}
		})
	}
}

// TestAnalyzeRejectsEmitPromptsDashWithStdoutOut validates the
// contract: `--emit-prompts -` (route JSONL to stdout) is
// mutually exclusive with `--out` defaulting to stdout. The
// dispatcher refuses the combination at flag-parse time with
// exit 64 and the literal stderr line the brief pins.
func TestAnalyzeRejectsEmitPromptsDashWithStdoutOut(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	_, stderr, code := captureRun("analyze", dir, "--emit-prompts", "-")
	if code != flags.ExitUsage {
		t.Errorf("exit code = %d, want %d (ExitUsage)\nstderr=%s", code, flags.ExitUsage, stderr)
	}
	want := "--emit-prompts - requires --out <path>; cannot route both markdown and JSONL to stdout"
	if !strings.Contains(stderr, want) {
		t.Errorf("stderr missing %q\nstderr=%s", want, stderr)
	}
}

// TestAnalyzeEmitPromptsDashWithOutFile validates the happy
// path: `--emit-prompts -` is accepted when `--out` is a
// file path. The run completes cleanly and the JSONL lands
// on stdout (empty for an empty repo with no tasks).
func TestAnalyzeEmitPromptsDashWithOutFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outDir := t.TempDir()
	stdout, stderr, code := captureRun("analyze", dir,
		"--out", filepath.Join(outDir, "report.md"),
		"--emit-prompts", "-")
	if code != flags.ExitOK {
		t.Errorf("exit code = %d, want %d\nstderr=%s", code, flags.ExitOK, stderr)
	}
	// Empty repo -> zero tasks -> zero JSONL lines on stdout.
	if stdout != "" {
		t.Errorf("expected empty stdout for zero-task run, got %q", stdout)
	}
}

// TestAnalyzeEmitPromptsToFile validates the happy path with
// a filesystem destination: the file is created (even when
// zero tasks are emitted) and the diagnostics block surfaces
// the `prompt_count` field.
func TestAnalyzeEmitPromptsToFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outDir := t.TempDir()
	promptsPath := filepath.Join(outDir, "prompts.jsonl")
	diagPath := filepath.Join(outDir, "diagnostics.json")
	_, stderr, code := captureRun("analyze", dir,
		"--out", filepath.Join(outDir, "report.md"),
		"--emit-prompts", promptsPath,
		"--diagnostics", diagPath)
	if code != flags.ExitOK {
		t.Errorf("exit code = %d, want %d\nstderr=%s", code, flags.ExitOK, stderr)
	}
	body, err := os.ReadFile(promptsPath)
	if err != nil {
		t.Fatalf("read prompts file: %v", err)
	}
	// Empty repo -> zero tasks -> empty JSONL artifact.
	if len(body) != 0 {
		t.Errorf("expected empty prompts file, got %d bytes:\n%s", len(body), string(body))
	}
	diag, err := os.ReadFile(diagPath)
	if err != nil {
		t.Fatalf("read diagnostics file: %v", err)
	}
	if !strings.Contains(string(diag), `"prompt_count"`) {
		t.Errorf("diagnostics sidecar missing prompt_count field:\n%s", string(diag))
	}
}

// TestDetectBareEmitPrompts_TableDrivesContract pins the
// detector's behaviour across every contract-relevant input
// shape so the dispatcher's pre-scan stays in step with the
// workstream brief.
func TestDetectBareEmitPrompts_TableDrivesContract(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		args    []string
		wantBad bool
	}{
		{"no-flag", []string{"analyze", "."}, false},
		{"with-value", []string{"--emit-prompts", "foo.jsonl"}, false},
		{"with-dash-value", []string{"--emit-prompts", "-"}, false},
		{"attached-value", []string{"--emit-prompts=foo.jsonl"}, false},
		{"attached-empty-value", []string{"--emit-prompts="}, false},
		{"bare-last", []string{"--emit-prompts"}, true},
		{"single-dash-bare-last", []string{"-emit-prompts"}, true},
		{"bare-followed-by-flag", []string{"--emit-prompts", "--out", "r.md"}, true},
		{"bare-followed-by-eof-sentinel", []string{"--emit-prompts", "--"}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			msg, bare := detectBareEmitPrompts(tc.args)
			if bare != tc.wantBad {
				t.Errorf("bare = %v, want %v (msg=%q)", bare, tc.wantBad, msg)
			}
			if bare && msg != emitPromptsBareMessage {
				t.Errorf("msg = %q, want %q", msg, emitPromptsBareMessage)
			}
		})
	}
}
