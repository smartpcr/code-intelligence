// Package rerankertrainer implements the Stage 6.4 Reranker
// Trainer worker per implementation-plan.md §6.4 and
// architecture.md §3.6 / tech-spec §8.4.
//
// What the trainer is responsible for
// -----------------------------------
//
// The trainer runs on a configurable cadence (default: nightly
// per tech-spec §8.4) plus an optional on-demand wake when the
// labelled-Episode count has grown by ≥ 5 % since the last run.
// Each Tick:
//
//  1. Acquires a `pg_try_advisory_lock` so multiple replicas
//     do not double-train on the same window.
//  2. Pulls labelled training pairs from the trailing
//     `Config.TrainingWindow` (default 90 days) of Episodes.
//     EVERY pair selector -- positives (success / agent),
//     synthetic positives, negatives (failure / degraded /
//     human_corrected), AND parent-of-synthetic-positive --
//     applies the `created_at >= now() - $window` predicate
//     so the trainer never re-trains against stale-historical
//     corrections. Iter-3 review item 5 closed the prior
//     drift where synthetic positives were "all-time" while
//     the rest of the pipeline used the trailing window.
//  3. Applies the per-actor sliding-window correction cap from
//     risk §9.4 as a COMBINED budget across positives and
//     negatives -- positives + negatives are merged into one
//     slice, `applyActorCap` runs ONCE on the combined slice,
//     and the kept rows are partitioned back. A noisy
//     operator's promotion + demotion thus consume the same
//     single budget cell, not separate cells. Iter-3 review
//     item 4 closed the prior per-bucket shape.
//  4. Calls a pluggable `Trainer` selected at startup via
//     AGENT_MEMORY_RERANKER_TRAINER_KIND /
//     AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT:
//       - "sidecar" (default when ENDPOINT is set): POSTs
//         the labelled pairs to the Python BERT cross-
//         encoder sidecar at `<endpoint>/train` and persists
//         the returned TrainingOutput. This is the
//         production deployment shape and the ≤ 200M-param
//         BERT trainer of record per §6.4 step-3. Reference
//         implementation at
//         services/agent-memory/cmd/reranker-sidecar/.
//       - "linear": runs the in-process logistic-regression
//         baseline trainer. Marked DEV/CI ONLY in the
//         loadConfig doc-comment. Useful for hermetic
//         tests + early-onboarding deployments where the
//         labelled corpus is too thin to support a real
//         cross-encoder fine-tune.
//       - "noop": writes a deterministic fingerprint with
//         no training. Hermetic CI opt-in only.
//     The previous "noop default" shape was rejected (iter-3
//     review item 3); the loadConfig now REFUSES to start
//     when neither ENDPOINT nor KIND is set.
//  5. INSERTs a `reranker_model` row carrying `version`,
//     `artifact_uri`, `trained_at`, and `metrics_json` per
//     tech-spec §8.4. The row's `status` is derived from the
//     trainer: linear and sidecar publish `'published'`;
//     noop publishes `'shadow'` unless the operator opts in
//     with `AGENT_MEMORY_RERANKER_ALLOW_NOOP_PUBLISH=true`
//     (the staleness gate at §9.10 then keeps firing on the
//     last *real* published model).
//
// Version derivation: deterministic, idempotent
// ---------------------------------------------
//
// The trainer derives a `version` string from the SHA-256
// fingerprint of the training input (sorted episode ids +
// observations + window bounds + training config) plus a
// short trainer-class tag. Two ticks that consume the same
// Episode set produce the same version, and the
// `reranker_model.version` UNIQUE index then converts a
// duplicate publish into a no-op. This is the idempotency
// contract a redo-from-restart relies on -- without the
// deterministic fingerprint, an in-flight Tick that crashed
// AFTER training but BEFORE the INSERT would re-train on
// retry and publish a second row.
//
// Mark-stale on recall: where the §9.10 flag actually fires
// ---------------------------------------------------------
//
// The trainer publishes; the recall verb (`internal/agentapi/
// recall.go`) reads. The §9.10 contract -- "if
// `last_trained_at` exceeds 7 days, recall responses carry
// `degraded=true, degraded_reason='reranker_model_stale'`" --
// is enforced in `internal/agentapi/recall.go`'s
// `applyRerankerStaleness` step (added in this stage). The
// trainer itself only owns the data: ensuring the freshest
// `reranker_model` row reflects the latest successful training
// run. The closed-set ENUM literal `reranker_model_stale`
// is reused from `internal/agentapi/summarize.go` (Stage 5.4
// landed the constant).
//
// Recall-time consumption of the published artifact
// -------------------------------------------------
//
// The trainer publishes the artifact_uri; the recall path
// (`internal/agentapi/{reranker.go, linear_weights_decoder.go,
// bert_sidecar_decoder.go}`) consumes it. The
// PublishedReranker wrapper at agent-api startup is wired
// with a MultiArtifactDecoder chain that recognises:
//   - `data:application/json;base64,...` URIs (LinearTrainer
//     output) -- decoded in-process,
//   - `file://...` URIs (sidecar output) -- forwarded to
//     the sidecar's `/rank` endpoint over HTTP.
// Both paths are atomic via AtomicReranker.RankWithVersion
// so a publish landing mid-recall cannot cause "ranked with
// model A, advertised version B" drift.
//
// Why no trainer_run table
// ------------------------
//
// Sister workers (Consolidator §6.1, Promoter §6.2) maintain
// `consolidator_run` / `promoter_run` lifecycle tables because
// their downstream rows (`ConceptVersion`) carry a
// `producer_run_id` FK that must point at a committed parent.
// The Trainer's only artefact is the `reranker_model` row
// itself; no downstream entity carries a `trainer_run_id`.
// Lifecycle observability is sufficient through the atomic
// in-process counters in metrics.go plus the durable
// `reranker_model.trained_at` trail in PostgreSQL.
//
// Layout
// ------
//
//	doc.go                -- this overview
//	trainer.go            -- Trainer interface,
//	                         TrainingInput / Output,
//	                         NoopTrainer, version helpers
//	linear_trainer.go     -- in-process baseline trainer
//	sidecar_trainer.go    -- HTTP adapter for the Python
//	                         BERT cross-encoder sidecar
//	pairs.go              -- LabelledPair type, PullPairs
//	                         (DB scan + combined actor cap)
//	metrics.go            -- atomic counter surface,
//	                         Metric* constants
//	service.go            -- Service{db,cfg,trainer,metrics,
//	                         logger}, Tick lifecycle, Run
//	                         loop, LatestPublishedArtifact
package rerankertrainer
