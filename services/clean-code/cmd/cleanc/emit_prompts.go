// -----------------------------------------------------------------------
// <copyright file="emit_prompts.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package main / emit-prompts flag wiring.
//
// Workstream brief: "Emit Prompts Flag Wiring" (Stage 4.3).
// This file owns the three helpers the analyze dispatcher
// composes when `--emit-prompts <path|->` is non-empty:
//
//   - [detectBareEmitPrompts] -- pre-scan invoked before
//     `fs.Parse` runs. Catches `--emit-prompts` invocations
//     with no value (the bare last-positional case AND the
//     "next token looks like another flag" case) and returns
//     the literal stderr line the brief pins.
//
//   - [emitPromptsBuffer]     -- runs the JSONL emitter into
//     an in-memory `bytes.Buffer` so the row count can be
//     stamped onto `art.Diagnostics.PromptCount` BEFORE the
//     markdown / JSON renderers run. The buffer is returned
//     to the caller which flushes it to the destination
//     writer at the very end of `runAnalyzePipeline`.
//
//   - [writePromptBuffer]     -- flushes the buffered JSONL
//     bytes to either `stdout` (when the flag value is the
//     literal `-`) or the file path supplied. File writes
//     use the `os.Create` + `defer w.Close()` pattern the
//     workstream brief pins for every CLI-side artifact
//     writer.

package main

import (
	"bytes"
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
// flag). Pinned by the workstream brief Stage 4.3.
const emitPromptsBareMessage = "--emit-prompts requires a path or '-' for stdout"

// emitPromptsStdoutConflictMessage is the literal stderr line
// the dispatcher writes when `--emit-prompts -` is requested
// AND `--out` is unset (default = stdout). Routing both the
// markdown report and the JSONL prompt stream to stdout would
// silently interleave the two artifacts and corrupt both;
// the contract refuses the combination outright with exit 64
// (BSD `EX_USAGE`). Pinned by the workstream brief Stage 4.3.
const emitPromptsStdoutConflictMessage = "--emit-prompts - requires --out <path>; cannot route both markdown and JSONL to stdout"

// detectBareEmitPrompts pre-scans the raw CLI tokens for the
// `--emit-prompts` / `-emit-prompts` flag and reports
// `(message, true)` when the invocation carries no value.
//
// "Bare" matches BOTH common forms:
//
//   - `cleanc analyze . --emit-prompts`                   (last token)
//   - `cleanc analyze . --emit-prompts --findings foo.json` (next token starts with `-`)
//
// The second case is detected because the stdlib `flag`
// parser would otherwise silently consume `--findings` as
// the value for `--emit-prompts` (it does not peek at the
// next-token shape). The workstream brief pins a "bare"
// rejection for both surfaces so the operator sees the same
// actionable error regardless of which footgun they tripped.
//
// The `--emit-prompts=<value>` form (including the empty
// `--emit-prompts=`) is NOT considered bare: a value is
// explicitly attached. An empty attached value is treated as
// "disabled" by the caller.
func detectBareEmitPrompts(args []string) (string, bool) {
	for i, a := range args {
		if a != "--emit-prompts" && a != "-emit-prompts" {
			continue
		}
		// Last token -> no value can follow.
		if i == len(args)-1 {
			return emitPromptsBareMessage, true
		}
		next := args[i+1]
		// Next token is the POSIX end-of-flags sentinel:
		// the operator intended the stdlib to stop flag
		// parsing here, so `--emit-prompts` carries no
		// value.
		if next == "--" {
			return emitPromptsBareMessage, true
		}
		// Next token starts with `-` and is not the
		// literal `-` (stdout sentinel) and is not a
		// negative-number-looking value: treat as bare so
		// `--emit-prompts --findings foo.json` does not
		// silently mis-bind.
		if len(next) > 1 && strings.HasPrefix(next, "-") && next != "-" {
			return emitPromptsBareMessage, true
		}
	}
	return "", false
}

// emitPromptsBuffer runs the JSONL prompt emitter into an
// in-memory `bytes.Buffer` and returns `(bytes, rowCount, nil)`
// on success. Buffering lets the composition root stamp
// `art.Diagnostics.PromptCount` BEFORE the markdown / JSON
// renderers run -- the renderers surface the count in their
// diagnostics block, so the stamp must precede dispatch.
//
// Row count is the number of `'\n'` bytes the emitter wrote
// (the JSONL wire format pins one newline per record); the
// emitter does not return a count and intercepting the writer
// here is the lightest-weight contract.
//
// Resolver wiring (per `suggest.JSONL` doc comment):
//
//   - `Bindings`   = `orch.ScopeBindings()` (the orchestrator
//     populated table; the emitter rejects a nil binding
//     table with `suggest.ErrNilBindingTable`).
//   - `Rules`      = `suggest.NewSliceRuleResolver(bundle.Rules)`
//     so `prose_suggestion` prefers the rule pack author's
//     `DescriptionMD` over the task planner's deterministic
//     fallback (architecture Sec 3.7.3).
//   - `Thresholds` = `suggest.NewSliceThresholdResolver(bundle.Thresholds)`
//     so `metric_evidence[].threshold` / `.op` reflect the
//     bundle's canonical thresholds rather than the
//     fail-closed empty array.
//
// On error, an operator-facing line is written to `stderr`
// and the caller maps the non-nil return to
// [flags.ExitInternalError].
func emitPromptsBuffer(
	ctx context.Context,
	stderr io.Writer,
	orch *orchestrator.Orchestrator,
	bundle devpolicy.Bundle,
	art report.RunArtifact,
) ([]byte, int, error) {
	emitter := suggest.NewJSONL(orch.ScopeBindings())
	emitter.Rules = suggest.NewSliceRuleResolver(bundle.Rules)
	emitter.Thresholds = suggest.NewSliceThresholdResolver(bundle.Thresholds)

	var buf bytes.Buffer
	if err := emitter.Emit(ctx, art, &buf); err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: --emit-prompts: %v\n", err)
		return nil, 0, err
	}
	count := bytes.Count(buf.Bytes(), []byte{'\n'})
	return buf.Bytes(), count, nil
}

// writePromptBuffer flushes the buffered JSONL bytes to the
// destination implied by `target`:
//
//   - `target == "-"`  -> the supplied `stdout` writer
//     (the operator opted in to stdout via the dash
//     sentinel; the analyze dispatcher already verified
//     `--out` is a file path so the two artifacts cannot
//     collide on stdout).
//   - otherwise         -> `target` is a filesystem path;
//     the writer is opened via `os.Create` and closed
//     with the same `defer Close()` + Close()-error
//     promotion pattern the other dispatch helpers
//     (`dispatchMarkdown`, `dispatchJSONFile`,
//     `dispatchDiagnostics`) use.
//
// On error, an operator-facing line is written to `stderr`
// naming the offending path (or `(stdout)`) and the caller
// maps the non-nil return to [flags.ExitInternalError].
func writePromptBuffer(stdout, stderr io.Writer, target string, body []byte) (retErr error) {
	if target == "-" {
		if _, err := stdout.Write(body); err != nil {
			fmt.Fprintf(stderr, "cleanc analyze: --emit-prompts (stdout): %v\n", err)
			return err
		}
		return nil
	}
	f, err := os.Create(target)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: --emit-prompts %s: %v\n", target, err)
		return err
	}
	defer func() {
		cerr := f.Close()
		if cerr != nil && retErr == nil {
			fmt.Fprintf(stderr, "cleanc analyze: --emit-prompts %s: %v\n", target, cerr)
			retErr = cerr
		}
	}()
	if _, err := f.Write(body); err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: --emit-prompts %s: %v\n", target, err)
		return err
	}
	return nil
}
