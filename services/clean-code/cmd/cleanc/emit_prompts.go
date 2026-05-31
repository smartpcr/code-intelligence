// -----------------------------------------------------------------------
// <copyright file="emit_prompts.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package main / emit-prompts flag wiring.
//
// Workstream brief: "Emit Prompts Flag Wiring" (Stage 4.3).
// This file owns the helpers the analyze dispatcher composes
// when `--emit-prompts <path|->` is non-empty:
//
//   - [detectBareEmitPrompts] -- pre-scan invoked before
//     `fs.Parse` runs. Catches `--emit-prompts` invocations
//     with no value (the bare last-positional case, the
//     "next token looks like another flag" case, AND the
//     `--emit-prompts=` attached-empty case) and returns
//     the literal stderr line the brief pins.
//
//   - [emitPromptsDirect]     -- runs the JSONL emitter
//     against the destination writer directly (file or
//     stdout). Returns the number of records emitted by
//     counting the `'\n'` bytes the emitter wrote (the
//     JSONL wire format pins one newline per record). The
//     caller compares the returned count to the pre-stamped
//     `art.Diagnostics.PromptCount` (which mirrors
//     `len(art.Tasks)`) and raises an internal-error if the
//     two disagree -- so the pre-render stamp on the
//     diagnostics block cannot silently desync from the
//     on-disk artifact.

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/suggest"
)

// emitPromptsBareMessage is the literal stderr line the
// dispatcher writes when the operator typed `--emit-prompts`
// with no value (or with a successor token that is itself a
// flag, or with an attached but empty value). Pinned by the
// workstream brief Stage 4.3 -- EXACT match, no `cleanc
// analyze:` prefix.
const emitPromptsBareMessage = "--emit-prompts requires a path or '-' for stdout"

// emitPromptsStdoutConflictMessage is the literal stderr line
// the dispatcher writes when `--emit-prompts -` is requested
// AND `--out` is unset (default = stdout). Pinned by the
// workstream brief Stage 4.3 -- EXACT match, no `cleanc
// analyze:` prefix.
const emitPromptsStdoutConflictMessage = "--emit-prompts - requires --out <path>; cannot route both markdown and JSONL to stdout"

// detectBareEmitPrompts pre-scans the raw CLI tokens for the
// `--emit-prompts` / `-emit-prompts` flag and reports
// `(message, true)` when the invocation carries no value.
//
// "Bare" matches THREE forms:
//
//   - `cleanc analyze . --emit-prompts`                       (last token)
//   - `cleanc analyze . --emit-prompts --findings foo.json`   (next token starts with `-`)
//   - `cleanc analyze . --emit-prompts=`                      (attached-empty value)
//
// The first two cases would silently consume a successor
// token (or trigger a stdlib "needs an argument" diagnostic)
// in the absence of this pre-scan. The third case is the
// iter-2 evaluator item 5 fix: the brief pins "--emit-prompts
// requires a path or '-'", so an explicitly empty attached
// value is a flag-presence-without-value bug and must be
// rejected with the same diagnostic as the bare forms.
//
// The `--emit-prompts=<non-empty>` form (including the
// `--emit-prompts=-` stdout sentinel) is NOT bare: a value
// is explicitly attached.
func detectBareEmitPrompts(args []string) (string, bool) {
	for i, a := range args {
		// Attached-empty: `--emit-prompts=` (no chars after
		// the `=`). Rejected per iter-2 item 5.
		if a == "--emit-prompts=" || a == "-emit-prompts=" {
			return emitPromptsBareMessage, true
		}
		if a != "--emit-prompts" && a != "-emit-prompts" {
			continue
		}
		if i == len(args)-1 {
			return emitPromptsBareMessage, true
		}
		next := args[i+1]
		if next == "--" {
			return emitPromptsBareMessage, true
		}
		if len(next) > 1 && strings.HasPrefix(next, "-") && next != "-" {
			return emitPromptsBareMessage, true
		}
	}
	return "", false
}

// newlineCountingWriter wraps an `io.Writer` and tracks the
// number of `'\n'` bytes flushed through it. The JSONL wire
// format pins exactly one newline per record (see
// `suggest.JSONL.Emit` doc), so the counter equals the
// number of records emitted regardless of buffering inside
// the emitter.
type newlineCountingWriter struct {
	w     io.Writer
	count int
}

// Write satisfies [io.Writer]: forwards bytes to the wrapped
// writer and increments the counter by the number of `'\n'`
// bytes in the slice.
func (n *newlineCountingWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		if b == '\n' {
			n.count++
		}
	}
	return n.w.Write(p)
}

// emitPromptsDirect runs the JSONL prompt emitter against
// the destination implied by `target`:
//
//   - `target == "-"`  -> the supplied `stdout` writer.
//   - otherwise         -> `target` is a filesystem path;
//     opened via `os.Create` with a `defer Close()` +
//     Close()-error promotion pattern matching the other
//     dispatch helpers (`dispatchMarkdown`,
//     `dispatchJSONFile`, `dispatchDiagnostics`).
//
// Returns `(rowCount, nil)` on success. The caller compares
// the count to the pre-stamped `art.Diagnostics.PromptCount`
// (`len(art.Tasks)`) and raises an internal-error on
// disagreement.
//
// Resolver wiring (per `suggest.JSONL` doc comment):
//
//   - `Bindings`   = `orch.ScopeBindings()`.
//   - `Rules`      = `suggest.NewSliceRuleResolver(bundle.Rules)`.
//   - `Thresholds` = `suggest.NewSliceThresholdResolver(bundle.Thresholds)`.
//
// On error, an operator-facing line is written to `stderr`
// naming the offending path (or `(stdout)`) and the caller
// maps the non-nil return to [flags.ExitInternalError].
func emitPromptsDirect(
	ctx context.Context,
	stdout, stderr io.Writer,
	target string,
	orch *orchestrator.Orchestrator,
	bundle devpolicy.Bundle,
	art report.RunArtifact,
) (int, error) {
	emitter := suggest.NewJSONL(orch.ScopeBindings())
	emitter.Rules = suggest.NewSliceRuleResolver(bundle.Rules)
	emitter.Thresholds = suggest.NewSliceThresholdResolver(bundle.Thresholds)

	if target == "-" {
		counter := &newlineCountingWriter{w: stdout}
		if err := emitter.Emit(ctx, art, counter); err != nil {
			fmt.Fprintf(stderr, "cleanc analyze: --emit-prompts (stdout): %v\n", err)
			return counter.count, err
		}
		return counter.count, nil
	}

	return emitPromptsToFile(ctx, stderr, target, emitter, art)
}

// emitPromptsToFile is the filesystem branch of
// [emitPromptsDirect]. Split out so the named-return
// (retErr) + deferred Close()-error promotion pattern stays
// in a small, single-purpose function (the stdout branch
// has no Close() to defer).
func emitPromptsToFile(
	ctx context.Context,
	stderr io.Writer,
	target string,
	emitter *suggest.JSONL,
	art report.RunArtifact,
) (rowCount int, retErr error) {
	f, err := os.Create(target)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: --emit-prompts %s: %v\n", target, err)
		return 0, err
	}
	counter := &newlineCountingWriter{w: f}
	defer func() {
		cerr := f.Close()
		if cerr != nil && retErr == nil {
			fmt.Fprintf(stderr, "cleanc analyze: --emit-prompts %s: %v\n", target, cerr)
			retErr = cerr
		}
	}()
	if err := emitter.Emit(ctx, art, counter); err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: --emit-prompts %s: %v\n", target, err)
		return counter.count, err
	}
	return counter.count, nil
}
