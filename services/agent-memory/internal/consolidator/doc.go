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
//	4. Emit synthetic_positive Episodes for parent agent Episodes
//	    that received an operator-correction EpisodeUpdate
//	    (Stage 6.3 operator-correction auto-promotion, owned by
//	    this package per architecture §7.7 step 4 — see the
//	    "Operator-correction auto-promotion" subsection below).
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
// Operator-correction auto-promotion (Stage 6.3, arch §7.7 step 4)
// -----------------------------------------------------------------
// At the end of each Tick (after `processOnce` succeeds, before
// the advisory lock is released), `emitSyntheticPositives`
// drives a CTE-bounded scan over the `episode_update` table (the
// `eu_changes` CTE inside `scanSyntheticCandidates`) for rows
// filed SINCE THE LAST 'done' run, then for each affected parent
// agent Episode checks that the parent's GLOBAL LATEST
// `EpisodeUpdate` (ORDER BY created_at DESC, update_id DESC
// LIMIT 1) carries `new_outcome='human_corrected'` AND that the
// parent has a matching feedback Episode carrying
// `corrected_action`. For each such parent that does not yet
// have a `synthetic_positive` child, the worker emits exactly
// one `kind='synthetic_positive'` Episode that:
//
//   - Copies the parent's `context_id` (C16, required by
//     `episode_context_id_required_unless_feedback_chk`).
//   - Sets `action = corrected_action`, `outcome = 'success'`
//     (positive polarity per the polarity table above).
//   - Sets `synthesized_from_parent_episode_id` to the parent and
//     `synthesized_from_feedback_episode_id` to the feedback Episode
//     (both required by the provenance CHECK constraints in
//     migration 0007).
//   - Mirrors the parent's Observation rows onto the synth via a
//     single `INSERT … SELECT FROM observation WHERE episode_id =
//     <parent>` (C17).
//   - Floors `created_at` to `GREATEST(clock_timestamp(),
//     max(newHighWaterMark, priorHighWaterMark) + 1µs)` so the
//     next Tick's `(created_at, episode_id) > cursor` delta scan
//     always picks the synth up — without the floor, a synth
//     inserted in the same microsecond as the high-water mark
//     could tie and be skipped.
//
// Why drive from EpisodeUpdate (not from Episode)
// -----------------------------------------------
// implementation-plan.md §6.3 line 1060 says "scan EpisodeUpdate
// rows since the last run for new_outcome='human_corrected'".
// The CTE-as-driver shape (`WITH eu_changes AS (SELECT DISTINCT
// episode_id FROM episode_update WHERE created_at > $cursor)`)
// honours that literally: the outermost relation IS the
// EpisodeUpdate stream and the parent Episode is joined onto
// each candidate EU's episode_id. The shape also gives the
// planner a chance to prune `episode_update` partitions by
// the `created_at > $cursor` predicate at the leaves.
//
// Why LATEST EU state globally (not within the cursor window)
// -----------------------------------------------------------
// The architecture treats `episode_update` as an append-only
// log; the "current status" of a parent Episode is the
// new_outcome of the latest row OVERALL, not the latest within
// any window. A parent that received a `human_corrected` EU at
// T1 (inside the cursor window) and a superseding `success` EU
// at T2 (also inside the cursor window) MUST NOT be promoted —
// the LATERAL LIMIT 1 reads the global latest and the
// `latest_eu.new_outcome = 'human_corrected'` predicate filters
// such retracted corrections out.
//
// Why aborting on synth-phase failure is safe
// -------------------------------------------
// `runEmissionPhase` propagates errors from
// `emitSyntheticPositives` up to `Tick`, so a synth-phase
// failure causes the surrounding tick to finalise
// status='failed'. `priorHighWater()` filters status='done',
// so the EU cursor effectively does NOT advance until a tick
// successfully emits every eligible synth. This is what makes
// the shared Episode high-water mark a SAFE proxy for the EU
// window: any EU left unprocessed by a failed synth phase is
// re-scanned by the next tick's CTE because the cursor stays
// put. Without this property, a synth-phase failure that
// happened to land BEFORE the cursor advancement would lose
// the correction forever (the next tick's
// `eu.created_at > $cursor` predicate would skip the
// unprocessed EU).
//
// processOnce's concept-promotion writes ARE committed by
// `promoteWithDedup` before the synth phase runs, and they are
// idempotent on re-scan (the dedup ledger inside
// `promoteWithDedup` skips already-promoted candidate rows),
// so the redundant work the next tick performs is bounded and
// benign.
//
// Idempotency
// -----------
// Three layers (defence in depth):
//   - SINCE-LAST-RUN cursor in the `eu_changes` CTE prunes EUs
//     already processed by prior successful ticks.
//   - `NOT EXISTS (SELECT 1 FROM episode synth WHERE
//     synth.synthesized_from_parent_episode_id = parent.episode_id)`
//     in the candidate scan AND in the per-candidate INSERT
//     catches the rare race where a sibling replica beat us to
//     the advisory lock between cursor read and INSERT.
//   - Partial UNIQUE index from migration 0013 on
//     (synthesized_from_feedback_episode_id, created_at) WHERE
//     kind='synthetic_positive' catches any same-tuple race
//     that bypasses both. SQLSTATE 23505 (unique_violation) is
//     logged and skipped.
//
// What this package does NOT do
// -----------------------------
//
//   - It does NOT write the FEEDBACK Episode itself (the
//     `kind='feedback'` row produced by `mgmt.feedback`). That row
//     is written by the management API / GraphWriter (Stage 5.1 /
//     5.2); this package only READS feedback Episodes via the
//     candidate scan above.
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
//	episode                     INSERT, SELECT          (delta scan + Stage 6.3 synth INSERT)
//	episode_update              SELECT                  (Stage 6.3 candidate scan)
//	observation                 INSERT, SELECT          (signature inputs + Stage 6.3 mirror)
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
