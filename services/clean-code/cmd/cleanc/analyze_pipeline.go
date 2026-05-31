// -----------------------------------------------------------------------
// <copyright file="analyze_pipeline.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

// Package main / analyze pipeline helpers.
//
// This file factors the per-step helpers `runAnalyzePipeline`
// composes (RepoContext minting, run/verdict lookup, file
// summary projection, renderer dispatch, exit-code derivation)
// out of `main.go` so the composition root stays a linear
// recipe. Every helper is intentionally small and
// side-effect-free except for the writer-opening dispatchers
// (`dispatchMarkdown`, `dispatchJSONFile`,
// `dispatchDiagnostics`) which OWN the `os.Create` + `defer
// w.Close()` pattern pinned by the workstream brief.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/ast/parser"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/orchestrator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/report"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/walk"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
	rule_engine "github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// detectLanguagePreference is the ordered list of language
// tags the dispatcher walks when resolving
// [repocontext.RepoContext.ModulePath]. The first language
// whose per-language manifest yields a non-empty module path
// wins; the dispatcher does NOT mix module paths across
// languages because the parser fleet uses
// `AttrModulePath` as a single-valued attribute (a polyglot
// repo therefore reports whichever manifest the dispatcher
// finds first). The list order is intentionally identical to
// the parser fleet enumeration in `cleanc version` so the
// preference is loud rather than buried in helper code.
var detectLanguagePreference = []string{"go", "typescript", "python", "java"}

// buildRepoContext mints the immutable [repocontext.RepoContext]
// every downstream stage threads through the pipeline. The
// helper is the SINGLE place repo-root identity is derived;
// the rest of the analyze body treats the value as frozen.
//
// Module-path detection is a best-effort scan over
// [detectLanguagePreference]: the first language whose
// per-language manifest yields a non-empty path wins. An
// empty result is non-fatal -- the `cycle_member` recipe
// silently degrades when `AttrModulePath` is absent
// (architecture Sec 4.1 ModulePath row).
func buildRepoContext(absPath string) repocontext.RepoContext {
	repoID := repocontext.MintRepoID(absPath)
	headSHA, isGit := repocontext.DetectHeadSHA(absPath)
	var modulePath string
	for _, lang := range detectLanguagePreference {
		if m := repocontext.DetectModulePath(absPath, lang); m != "" {
			modulePath = m
			break
		}
	}
	return repocontext.RepoContext{
		RootPath:   repocontext.NormalisePath(absPath),
		RepoID:     repoID,
		HeadSHA:    headSHA,
		ModulePath: modulePath,
		IsGitRepo:  isGit,
	}
}

// lookupRunAndVerdict resolves the [rule_engine.EvaluationRun]
// and [rule_engine.EvaluationVerdict] rows for the freshly
// completed batch. The engine returns the row IDs on
// [rule_engine.RunResult]; this helper joins back against the
// store's snapshot helpers so the report renderers can read
// the full canonical row shapes (CreatedAt timestamps,
// Caller, Degraded flag, etc.) verbatim.
//
// A missing row signals an engine wiring bug; the helper
// returns the zero value rather than panicking so the
// renderers can still produce a partially-populated artifact
// the operator can inspect.
func lookupRunAndVerdict(store *rule_engine.InMemoryStore, res rule_engine.RunResult) (rule_engine.EvaluationRun, rule_engine.EvaluationVerdict) {
	var (
		run     rule_engine.EvaluationRun
		verdict rule_engine.EvaluationVerdict
	)
	for _, r := range store.Runs() {
		if r.EvaluationRunID == res.EvaluationRunID {
			run = r
			break
		}
	}
	for _, v := range store.Verdicts() {
		if v.VerdictID == res.EvaluationVerdictID {
			verdict = v
			break
		}
	}
	// Defensive: when the lookup missed a verdict row but the
	// engine did stamp a canonical Verdict on the RunResult,
	// surface it so `--exit-on` honours the engine's decision.
	if verdict.Verdict == "" && res.Verdict != "" {
		verdict.Verdict = res.Verdict
		verdict.VerdictID = res.EvaluationVerdictID
		verdict.EvaluationRunID = res.EvaluationRunID
	}
	return run, verdict
}

// buildFileSummaries projects the orchestrator's per-file
// outputs onto the [report.WalkedFileSummary] slice the
// [report.RunArtifact] surfaces. Per-file disposition is:
//
//   - `parsed`        -- file appears in `result.Files`.
//   - `parser_error`  -- file appears in `result.Skips` with
//     reason [orchestrator.SkipReasonParserError].
//   - `parser_panic`  -- file appears in `result.Skips` with
//     reason [orchestrator.SkipReasonParserPanic].
//   - `skipped`       -- any other walker / orchestrator skip
//     reason.
//
// The returned slice is sorted by path ASC across BOTH
// partitions. The orchestrator sorts `result.Files` and
// `result.Skips` independently, but concatenating two
// independently-sorted subsequences does NOT yield a sorted
// whole; the final `sort.Slice` guarantees the merged
// invariant so downstream consumers can binary-search or
// diff against a prior sorted snapshot safely.
func buildFileSummaries(result *orchestrator.Result) []report.WalkedFileSummary {
	if result == nil {
		return nil
	}
	out := make([]report.WalkedFileSummary, 0, len(result.Files)+len(result.Skips))
	for _, f := range result.Files {
		out = append(out, report.WalkedFileSummary{
			Path:        f.GetPath(),
			Language:    f.GetLanguage(),
			SizeBytes:   int64(len(f.GetAttrs()[parser.AttrSourceBytes])),
			ParseStatus: "parsed",
		})
	}
	for _, s := range result.Skips {
		status := skipReasonToParseStatus(s.Reason)
		out = append(out, report.WalkedFileSummary{
			Path:        s.Path,
			ParseStatus: status,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// skipReasonToParseStatus maps the orchestrator + walker skip
// reason strings onto the closed [report.WalkedFileSummary.ParseStatus]
// vocabulary (`parsed`, `parser_error`, `parser_panic`,
// `skipped`). The renderer treats every status verbatim so
// the mapping is the single contract surface.
func skipReasonToParseStatus(reason string) string {
	switch reason {
	case orchestrator.SkipReasonParserError, orchestrator.SkipReasonScopeBindingError:
		return "parser_error"
	case orchestrator.SkipReasonParserPanic:
		return "parser_panic"
	case walk.SkipReasonDirectory,
		walk.SkipReasonGitignore,
		walk.SkipReasonSizeCap,
		walk.SkipReasonUnsupportedLanguage,
		walk.SkipReasonSymlinkLoop,
		walk.SkipReasonSymlink:
		return "skipped"
	default:
		return "skipped"
	}
}

// runTaskPlanner runs the Stage 8.2 [refactor.TaskPlanner]
// against the Stage 8.1 [refactor.PlanResult]. The composition
// step is split into its own helper so the linear recipe in
// `runAnalyzePipeline` stays readable.
//
// Returns `(plans, tasks, nil)` on success or `(nil, nil,
// err)` after writing a single operator-facing line to
// `stderr`. The caller maps a non-nil error to
// [flags.ExitInternalError]; helper-internal short-circuits
// (no policy version available, planner returned an empty
// snapshot) yield `(nil, nil, nil)` so the dispatcher
// continues with an empty refactor plan.
func runTaskPlanner(
	ctx context.Context,
	stderr io.Writer,
	bundle devpolicy.Bundle,
	planRes refactor.PlanResult,
	findings []rule_engine.Finding,
	repoCtx repocontext.RepoContext,
) ([]refactor.RefactorPlan, []refactor.RefactorTask, error) {
	// uuid.Nil sentinel: a zero PolicyVersionID means
	// Stage 8.1 produced no active snapshot (either
	// ErrNoActivePolicy or an empty input). Skip the
	// Stage 8.2 task planner; the report still surfaces a
	// zero-Plan and empty tasks slice.
	if planRes.Snapshot.PolicyVersionID.String() == "00000000-0000-0000-0000-000000000000" {
		return nil, nil, nil
	}
	tp, writer, err := orchestrator.NewTaskPlannerWiring(bundle, planRes.HotSpots, findings)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: task planner init: %v\n", err)
		return nil, nil, err
	}
	if _, err := tp.PlanFromSnapshot(ctx, repoCtx.RepoID, repoCtx.HeadSHA, planRes.Snapshot); err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: task planner run: %v\n", err)
		return nil, nil, err
	}
	return writer.Plans(), writer.Tasks(), nil
}

// wireBundle removed; the helper takes devpolicy.Bundle
// directly. Keeping the comment as a marker so a future
// refactor that wants to abstract the bundle type knows the
// alias was deliberately removed in favour of a direct
// import.

// dispatchMarkdown renders the markdown report to either the
// supplied path (when `outPath` is non-empty) or to `stdout`
// (when empty -- tech-spec Sec 8.1 row 1 pins the default).
// File writes use the `os.Create` + `defer w.Close()` pattern
// the workstream brief pins so a partial render leaves the
// destination file at the OS-truncation boundary.
//
// Returns the underlying error after writing an
// operator-facing line to `stderr`; the dispatcher maps the
// non-nil return to [flags.ExitInternalError].
func dispatchMarkdown(ctx context.Context, stdout, stderr io.Writer, outPath string, art report.RunArtifact) error {
	r := report.Markdown{}
	if outPath == "" {
		if err := r.Render(ctx, art, stdout); err != nil {
			fmt.Fprintf(stderr, "cleanc analyze: --out (stdout): %v\n", err)
			return err
		}
		return nil
	}
	f, err := os.Create(outPath)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: --out %s: %v\n", outPath, err)
		return err
	}
	defer func() { _ = f.Close() }()
	if err := r.Render(ctx, art, f); err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: --out %s: %v\n", outPath, err)
		return err
	}
	return nil
}

// jsonRenderer is the local shape every JSON sidecar
// dispatcher accepts. The [report.JSON] type satisfies it
// via a method value (`report.JSON{}.Render`).
type jsonRenderer func(ctx context.Context, art report.RunArtifact, w io.Writer) error

// dispatchJSONFile writes a JSON artifact to `path` via the
// supplied `render` callback when `path` is non-empty; an
// empty path is a no-op (the operator did not request the
// sidecar). The `flagName` argument is the literal `--<flag>`
// label woven into the stderr diagnostic so the operator
// sees the offending flag on a failed write.
func dispatchJSONFile(ctx context.Context, stderr io.Writer, path, flagName string, render jsonRenderer, art report.RunArtifact) error {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: %s %s: %v\n", flagName, path, err)
		return err
	}
	defer func() { _ = f.Close() }()
	if err := render(ctx, art, f); err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: %s %s: %v\n", flagName, path, err)
		return err
	}
	return nil
}

// dispatchDiagnostics writes the orchestrator's
// [orchestrator.Diagnostics] container to `path` as
// indented JSON when `path` is non-empty. The Diagnostics
// JSON is intentionally narrower than the findings JSON: it
// carries only the dark-metric inventory + effort-source
// stamp, so it is a small operator-facing diagnostic
// sidecar suitable for "why did `cyclo` go dark?" queries.
func dispatchDiagnostics(stderr io.Writer, path string, diag orchestrator.Diagnostics) error {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: --diagnostics %s: %v\n", path, err)
		return err
	}
	defer func() { _ = f.Close() }()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(diag); err != nil {
		fmt.Fprintf(stderr, "cleanc analyze: --diagnostics %s: %v\n", path, err)
		return err
	}
	return nil
}

// findingsTriggerExit collapses the engine's canonical
// [rule_engine.Verdict], the per-finding severities the
// engine produced, and the operator's `--exit-on <sev>`
// threshold into a single boolean: true when EITHER the
// verdict rank or ANY finding's severity rank meets or
// exceeds the threshold rank.
//
// Why both inputs: the engine collapses info-only findings
// to [rule_engine.VerdictPass] (the canonical "no
// gate-tripping finding"). An `--exit-on=info` operator
// who relied on the verdict alone would silently exit 0
// even when info findings exist (regression item 3 in the
// iter-1 evaluator review). Considering the per-finding
// severity slice lets `--exit-on=info` honour the literal
// "any finding meets the info threshold" semantics
// tech-spec Sec 8.6 C9 pins.
//
// Rank scale (info=1, warn=2, block=3) — DISTINCT for every
// severity so an info finding does NOT satisfy
// `--exit-on=warn` (iter-2 evaluator item 3). The verdict
// scale uses the same numeric values for warn/block; the
// engine never emits a "VerdictInfo" (info findings collapse
// to Pass), so a verdict-only check at `--exit-on=info` is
// always false -- the per-finding loop carries the info
// signal.
//
// Mapping:
//
//   - `--exit-on=info`  -> trigger on any info, warn, or
//     block finding (verdict never reaches the info rank).
//   - `--exit-on=warn`  -> trigger on warn-or-block finding
//     OR warn/block verdict; info findings do NOT trigger.
//   - `--exit-on=block` -> trigger on block finding OR
//     block verdict.
func findingsTriggerExit(verdict rule_engine.Verdict, findings []rule_engine.Finding, threshold string) bool {
	tRank := thresholdRank(threshold)
	if tRank == 0 {
		return false
	}
	if vRank := verdictRank(verdict); vRank >= tRank {
		return true
	}
	for _, f := range findings {
		if findingSeverityRank(f.Severity) >= tRank {
			return true
		}
	}
	return false
}

// findingSeverityRank assigns the canonical numeric rank for
// a per-finding [steward.Severity] value. Each of info /
// warn / block carries a distinct rank so the
// `--exit-on <sev>` threshold-comparison check in
// [findingsTriggerExit] does NOT conflate info with warn
// (iter-2 evaluator item 3).
//
// Scale: info=1, warn=2, block=3. Any non-canonical value
// is treated as the lowest non-trigger rank (0) so a future
// enum widening is fail-safe rather than silently lighting
// up an `--exit-on=block` gate.
func findingSeverityRank(s steward.Severity) int {
	switch s {
	case steward.SeverityBlock:
		return 3
	case steward.SeverityWarn:
		return 2
	case steward.SeverityInfo:
		return 1
	default:
		return 0
	}
}

// verdictTriggersExit collapses the engine's canonical
// [rule_engine.Verdict] and the `--exit-on <sev>` operator
// threshold into a single boolean: true when the verdict
// rank meets or exceeds the threshold rank, false otherwise.
//
// Deprecated: use [findingsTriggerExit] -- this helper does
// not consult per-finding severities so it misses
// `--exit-on=info` cases when the engine collapses info-only
// findings to `VerdictPass`. Kept for unit-test coverage of
// the verdict-only path.
func verdictTriggersExit(verdict rule_engine.Verdict, threshold string) bool {
	tRank := thresholdRank(threshold)
	if tRank == 0 {
		return false
	}
	return verdictRank(verdict) >= tRank
}

// verdictRank assigns the canonical numeric rank for a
// [rule_engine.Verdict]. The engine never emits an
// info-level verdict (info findings collapse to
// [rule_engine.VerdictPass]); pass therefore ranks 0, warn
// ranks 2, block ranks 3 -- mirroring
// [findingSeverityRank] so a single threshold comparison
// is meaningful across both axes.
func verdictRank(v rule_engine.Verdict) int {
	switch v {
	case rule_engine.VerdictBlock:
		return 3
	case rule_engine.VerdictWarn:
		return 2
	case rule_engine.VerdictPass:
		return 0
	default:
		return 0
	}
}

// thresholdRank assigns the canonical numeric rank for the
// `--exit-on` threshold. Distinct values for info / warn /
// block mirror [findingSeverityRank] so an info finding
// does NOT satisfy a `--exit-on=warn` threshold
// (iter-2 evaluator item 3).
//
// Scale: info=1, warn=2, block=3. An unrecognised
// threshold (the flag-set validator already rejects these)
// returns 0 so [findingsTriggerExit] short-circuits to
// `false` -- a future enum widening is fail-safe.
func thresholdRank(threshold string) int {
	switch threshold {
	case "info":
		return 1
	case "warn":
		return 2
	case "block":
		return 3
	default:
		return 0
	}
}
