// Package consolidator is the Stage 6.1 Learning-Loop Consolidator
// worker per implementation-plan.md §6.1 and architecture.md §7.7.
//
// Architectural intent
// --------------------
// The Consolidator turns repeated observation patterns into durable
// Concepts. Architecturally (arch §7.7):
//
//	(Consolidator wakes every N Episodes or every K minutes.)
//	1. Read Episodes since last high-water mark.
//	2. Group by (repo_id, observation_signature_hash).
//	3. For each group whose support crossed threshold:
//	     - If fingerprint not seen: append Concept row.
//	     - Always:                 append ConceptVersion.
//	     -                         append ConceptSupport rows.
//	4. (synthetic_positive mirror flow — owned by Stage 5.2 / 7.3,
//	    NOT this package).
//	5. Persist a ConsolidatorRun row with the new high-water mark.
//
// What this package does
// ----------------------
//
//   - `Service.Tick(ctx)` — runs ONE consolidation pass. It opens
//     the `ConsolidatorRun` row FIRST (status='running') so any
//     `ConceptVersion.producer_run_id` written within the tick can
//     reference an existing row per arch §5.5.2 FK rule, scans the
//     Episode + Observation tables since the most recent finished
//     run's `episode_high_water_mark`, groups by observation
//     signature, crystallises Concepts that cross the support
//     threshold, and finalises the same `ConsolidatorRun` row
//     (status='done' on success, 'failed' on error) so the row
//     transitions only the four mutable fields tech-spec §8.7.4
//     permits.
//
//   - `Service.Run(ctx)` — long-running poll loop. Runs Tick once
//     immediately (so a fresh deploy doesn't have to wait a full
//     interval before the first sweep) and then on a configurable
//     ticker.
//
//   - `Service.Metrics()` — snapshot of the package counters,
//     including the `consolidator_episode_lag` gauge (the
//     implementation-plan §6.1 metric requirement
//     `max(Episode.created_at) − high-water-mark`).
//
// Cross-repo Concept (G6)
// -----------------------
// The Concept table has NO `repo_id` column (migration 0011). When
// Episodes from multiple repos share the same observation
// signature they collide on the same Concept row, and ConceptSupport
// rows discriminate the contributing repo via `concept_support.repo_id`.
// This package honours that invariant by computing the signature
// from the Observation set ONLY -- and crucially over the
// CANONICAL FINGERPRINTS of the targeted Nodes/Edges/Concepts
// (G2: every Node/Edge/Concept carries a deterministic 32-byte
// fingerprint per migrations 0003 / 0011), NOT over the per-repo
// node_id uuids. Two repos that index the same canonical element
// produce identical fingerprints, so their Observations produce
// identical signatures and their Episodes' support flows into
// the same Concept row. ConceptSupport rows are emitted per
// (Concept, Episode, repo_id, Node) tuple -- one row per
// contributing Node in each Episode, per implementation-plan.md
// §6.1 line 895.
//
// Threshold and polarity (G4)
// ---------------------------
// `Config.Threshold` (default 10 — see DefaultThreshold) is the
// minimum CUMULATIVE positive support_count required to crystallise
// a Concept for the first time. Once a Concept exists, every
// subsequent tick that observes new contributions emits a fresh
// ConceptVersion row carrying the updated (support_count,
// negative_count, confidence) tuple — the threshold only gates the
// initial emission.
//
// Episode → polarity mapping (matches arch §3.6 / §4.3):
//
//	positive  := outcome='success' OR kind='synthetic_positive'
//	negative  := outcome IN ('failure', 'refused',
//	                         'degraded', 'human_corrected')
//
// Confidence is computed as
// `support_count / (support_count + negative_count)` (defaulting to
// 0.5 when both counts are zero; that branch is unreachable in
// practice because the threshold guard requires support_count > 0).
// `confidence_band` is derived: <0.3 = low, [0.3, 0.7) = medium,
// >=0.7 = high.
//
// What this package does NOT do
// -----------------------------
//
//   - It does NOT run the §7.3 synthetic_positive mirror flow.
//     That flow is owned by the EpisodeUpdate / agent.observe
//     path (Stage 5.2). The Consolidator only consumes its output.
//
//   - It does NOT promote Concepts to publishable status. That is
//     the Concept Promoter (Stage 6.2 / §7.8).
//
//   - It does NOT touch the `embedding_publish*` tables. The
//     Concept embedding is written by the Concept Promoter.
//
// Role / required DB grants
// -------------------------
// The `*sql.DB` MUST authenticate as a role with the following
// per-table grants (the full enumeration; every table the worker
// touches is listed here so a `grep -F "concept_candidate_support"`
// or `grep -F "consolidator_run"` against the package finds the
// dependency in one place):
//
//	concept                     INSERT, SELECT
//	concept_version             INSERT, SELECT
//	concept_support             INSERT, SELECT
//	concept_candidate_support   INSERT, SELECT, UPDATE  (iter-4 staging table)
//	consolidator_run            INSERT, SELECT, UPDATE  (lifecycle row)
//	episode                     SELECT                  (delta scan)
//	observation                 SELECT                  (signature inputs)
//
// `concept_candidate_support` is the iter-4 durable staging
// table added in migration 0021. Stage 6.1's
// `emitGroupCandidatePath` reads pending rows (SELECT), inserts
// per-tick contributions (INSERT), and updates
// `promoted_to_concept_id` at promotion time (UPDATE), so all
// three privileges are required on this single table.
// `consolidator_run` similarly needs INSERT (openRun), SELECT
// (priorHighWater), and UPDATE (finalizeRun).
//
// Migration 0016 covers the original grant set for the
// `agent_memory_app` role; migration 0021 ships its own
// explicit `GRANT INSERT, SELECT, UPDATE ON concept_candidate_support`
// block because 0016's `GRANT ... ON ALL TABLES IN SCHEMA` is
// point-in-time and does NOT cover tables created in later
// migrations.
package consolidator
