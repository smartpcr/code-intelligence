// Package orchestrator implements Stage 2.2 of the `cleanc`
// CLI pipeline: parse fan-out + recipe fan-out.
//
// The orchestrator is the L3 dispatch glue documented in
// REFACTOR-GUIDE `architecture.md` Sec 5.3 and Sec 10: it
// reads a [walk.WalkedFile] stream, parses each file on a
// `GOMAXPROCS`-sized worker pool, populates a
// [scopebinding.Table] with one row per emitted [parser.AstScope],
// then runs per-file [recipes.Recipe]s followed by project-
// level [recipes.ProjectRecipe]s on the collected
// `[]*parser.AstFile` corpus. The output is a deterministic
// `[]recipes.MetricSampleDraft` slice keyed by
// `(metric_kind, path, scope_kind, local_id)` -- Stage 2.3
// converts that slice into `rule_engine.Sample` rows by
// rewriting each `Scope.LocalID` to its durable
// `scopebinding.Table` UUID.
//
// # What this package is NOT
//
//   - NOT a rule-engine driver. The orchestrator stops at the
//     `[]recipes.MetricSampleDraft` boundary; Stage 2.3
//     (`rule_engine.InMemoryStore`) consumes drafts.
//   - NOT a planner. Stages 2.4 / 2.5 wire the refactor
//     planner over the engine's output, not over recipes.
//   - NOT a scope-id minter for durable UUIDs OUTSIDE the
//     table. Mint+insert happens here; downstream readers
//     look up by `(file_path, local_id)` via the returned
//     `Result.ScopeIDs` map.
//
// # Determinism contract (tech-spec C11)
//
// Two `cleanc analyze <path>` runs on the same checkout MUST
// produce byte-identical drafts. The orchestrator preserves
// determinism by:
//
//  1. Sorting parsed `*AstFile`s by `Path` before recipes.
//  2. Iterating recipes via [recipes.Registry.Recipes] (sorted
//     by metric_kind).
//  3. Iterating project recipes via
//     [recipes.ProjectRegistry.All] (sorted by metric_kind).
//  4. Sorting the final draft slice by
//     `(metric_kind, path, scope_kind, local_id, metric_version)`
//     using a stable sort so per-recipe emission order survives
//     intact for any ties the four-tuple doesn't break.
//
// # Per-job panic recovery (tech-spec Sec 8.6)
//
// A parser panic on a single file is captured in a
// PER-JOB `defer recover()`, surfaced as
// [walk.WalkSkip]`{Reason: "parser_panic"}`, and the worker
// continues to its next job. A panic OUTSIDE a per-file
// parse (in the orchestrator's own dispatch / recipe loops)
// is propagated -- the CLI binary exits 70 per tech-spec.
//
// Anchors:
//   - REFACTOR-GUIDE `architecture.md` Sec 3.3, 4.3, 5.3, 10.
//   - `tech-spec.md` Sec 8.6 (exit code 70), Sec 8.8
//     (`GOMAXPROCS` worker pool sizing), C11
//     (`(root_path, head_sha, policy_set)` byte-identical
//     determinism).
//   - `implementation-plan.md` Stage 2.2 lines 147-172.
package orchestrator
