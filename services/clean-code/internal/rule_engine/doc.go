// Package rule_engine is the SOLID Rule Engine. It evaluates
// the active [steward.PolicyVersion]'s rules against the
// MetricSample rows produced by Phase 3 ingestion and writes
// the canonical Audit triple ([EvaluationRun],
// [EvaluationVerdict], [Finding]) per architecture Sec 3.6
// (lines 536-556) and Sec 4.2 (lines 759-775) of the
// CLEAN-CODE story.
//
// # Two callable modes
//
// The engine exposes exactly two entry points, both writing
// the SAME row set (ONE [EvaluationRun] + ONE
// [EvaluationVerdict] + N [Finding] rows) in the SAME
// transaction:
//
//   - [Engine.RunSync] -- synchronous mode invoked by
//     `eval.gate` (Stage 5.8). Caller-stamp is
//     `[CallerEvalGate]` (`eval_gate`). Returns the run /
//     verdict / finding IDs so the gate can shape its
//     response without a second read.
//
//   - [Engine.RunBatch] -- batch refresh mode invoked by the
//     post-scan dispatcher after a Metric Ingestor completes
//     a SHA (architecture Sec 4.1 line 752, Sec 4.7). Caller-
//     stamp is `[CallerBatchRefresh]` (`batch_refresh`).
//
// Both modes share the same evaluation core ([Engine.run]) so
// adding behaviour to one mode automatically applies to the
// other; the only operational difference is the
// `[EvaluationRun.Caller]` discriminator.
//
// # Writer-ownership
//
// Per architecture Sec 1.5 G1 and tech-spec Sec 7.2 lines
// 1256-1261, the three Audit tables (`evaluation_run`,
// `evaluation_verdict`, `finding`) are granted INSERT in
// parallel to three roles:
//
//   - `clean_code_solid_batch` -- this engine on the
//     `[CallerBatchRefresh]` path AND on the
//     `[CallerEvalGate]` path (the in-process synchronous
//     invocation from `eval.gate` shares the same writer
//     identity; the caller discriminator distinguishes them
//     in audit).
//   - `clean_code_evaluator` -- the `eval.gate` short-circuit
//     paths (signature-invalid, samples_pending) where the
//     engine is NOT invoked and the gate writes a
//     run+verdict pair with zero findings directly.
//   - `clean_code_wal_reconciler` -- replay-only.
//
// The engine is the canonical writer of `evaluation_verdict`
// whenever the synchronous rule pass is invoked; the gate
// writes its own run+verdict pair only on the degraded
// short-circuits.
//
// # Advisory lock
//
// Concurrent [Engine.RunSync] calls for the same
// `(repo_id, sha)` are serialised by an in-process advisory
// lock so two parallel `eval.gate` invocations do not
// interleave their Audit writes. The production-PG path
// upgrades the in-process [sync.Mutex] to
// `pg_advisory_xact_lock(hashtext(repo_id::text || sha))`
// inside the [Store.AppendEvaluation] transaction; the
// in-process lock guards the read-modify-write window so the
// engine sees a consistent prior-finding snapshot for the
// delta computation.
//
// # Determinism and purity
//
// [Engine.run] is deterministic over its `(repo_id, sha,
// policy_version_id)` inputs and the durable state in
// [Store]: re-running it with the same inputs against the
// same store snapshot produces the same row set (modulo the
// `[Engine.newID]` UUID generator and the `[Engine.clock]`
// timestamp, which tests pin via [Config]). The DSL
// evaluator ([Predicate.Eval]) is pure per Stage 5.4 design.
package rule_engine
