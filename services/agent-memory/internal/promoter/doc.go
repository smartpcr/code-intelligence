// Package promoter is the Stage 6.2 Concept Promoter worker per
// implementation-plan.md §6.2 and architecture.md §7.8.
//
// Architectural intent
// --------------------
// The Concept Promoter runs after each ConsolidatorRun finishes
// (architecturally tied to the Consolidator wake cadence, but
// operationally driven by a periodic ticker on this binary).
// For every Concept whose latest ConceptVersion has crossed the
// §7.8 publishable threshold (`confidence >= 0.7` AND
// `support_count >= 5`), the Promoter:
//
//  1. Opens a `promoter_run` row (status='running') so the
//     subsequent `ConceptVersion(producer='promoter')` writes
//     have a real `producer_run_id` to FK-reference per
//     architecture.md §5.5.2 line 620.
//
//  2. Per the §8.7.1 lines 818-833 write protocol:
//
//     (a) Appends a NEW `concept_version` row with
//     `producer='promoter'`, `promoted=true`, the same
//     `confidence`/`support_count`/`negative_count` tuple as
//     the latest version, and `version_index = max+1`. This
//     row carries NO `embedding_vec` column (the vector
//     lives in Qdrant per tech-spec.md §8.7.1 line 569) and
//     NO `embedding_model_version` column (that field is on
//     `embedding_publish` per tech-spec.md §8.7 lines
//     807-809).
//
//     (b) Computes the Concept embedding (name +
//     description_md) and reserves a Qdrant `point_id`.
//
//     (c) Inserts an `embedding_publish` row whose
//     `concept_version_id` foreign-keys to the row from
//     step (a), with the reserved `point_id` and the
//     `embedding_model_version` reported by the configured
//     Embedder.
//
//     (d) Inserts the matching `embedding_publish_event` with
//     `event_kind='queued'`.
//
//     (e) Upserts the vector to the Qdrant
//     `agent_memory_concept` collection.
//
//     (f) On Qdrant success, appends `vector_written`; on
//     failure, appends `failed` and moves on (the next
//     promoter tick produces a fresh `'queued'` event,
//     never an UPDATE — append-only per §8.7.4).
//
//     (g) Issues a confirming Qdrant fetch (read-after-write);
//     on success, appends `published`. Only then is the
//     vector eligible for recall (Stage 5.1 filters on
//     latest event `'published'`).
//
//  3. Finalises the `promoter_run` row with `finished_at=now()`,
//     `concepts_promoted=<count of chains that reached
//     'published' this tick>`, `status='done'` (or `'failed'`
//     on a non-recordable error path).
//
// Sole-writer rule (architecture.md C12)
// --------------------------------------
// The Concept Promoter is the SOLE writer of Concept entries
// to the EmbeddingIndex. The Consolidator (Stage 6.1) appends
// `concept_version` rows with `producer='consolidator'` but
// NEVER writes `embedding_publish` rows whose
// `concept_version_id` is non-null. The
// `embedding_publish.exactly_one_target_chk` CHECK makes both
// shapes physically addressable in one table, and grant
// hygiene (migrations 0015/0016) plus this package's narrow
// SQL surface enforce the directional invariant.
//
// Ordering invariants (§8.7.1 write protocol)
// -------------------------------------------
// The following ordering MUST hold per promotion (the
// integration tests assert these explicitly):
//
//   - `promoter_run.started_at` < every
//     `concept_version.created_at` whose `producer='promoter'`
//     and `producer_run_id` is this run.
//
//   - For every such ConceptVersion, its `created_at` is
//     STRICTLY EARLIER than the `embedding_publish.created_at`
//     of its associated publish row. We achieve this by
//     splitting the ConceptVersion INSERT and the
//     EmbeddingPublish INSERT into TWO separate transactions:
//     PostgreSQL's `now()` returns the transaction start time,
//     so two distinct transactions guarantee two distinct
//     timestamps (microsecond-precision; a tx commit + new tx
//     begin takes well over 1 µs in practice).
//
//   - For every EmbeddingPublish, its event chain advances
//     through the §9.6a state machine:
//     `queued -> vector_written -> published` on the happy
//     path; `failed` on the unhappy path. The latest event
//     read (`ORDER BY created_at DESC, event_id DESC LIMIT 1`)
//     is what gates recall.
//
// Concurrency with the Consolidator
// ---------------------------------
// The Consolidator (Stage 6.1) also writes `concept_version`
// rows. Naively, two concurrent writers computing
// `version_index = max+1` would race the
// `concept_version_concept_version_uidx` unique constraint
// `(concept_id, version_index)`. The Promoter cooperates with
// the Consolidator's existing `SELECT ... FROM concept WHERE
// concept_id = $1 FOR UPDATE` lock by taking the SAME
// row-level lock inside its per-Concept transaction, then
// re-reading `MAX(version_index)` inside the locked tx. This
// guarantees the `version_index = max+1` calculation is
// performed against the post-Consolidator state.
//
// The Promoter ALSO holds its own session-level advisory
// lock (`PromoterAdvisoryLockKey`) so multiple Promoter
// replicas serialise their candidate-selection sweeps. The
// per-tick lock is the cross-replica gate; the per-Concept
// FOR UPDATE is the cross-writer gate.
//
// Retry path
// ----------
// A Concept whose chain stalled at `'queued'` / `'failed'` /
// `'vector_written'` (i.e. latest event is NOT 'published' or
// 'superseded') is automatically retried on the next tick.
// The retry phase runs BEFORE the forward phase so a stalled
// publish does not get starved by a steady stream of fresh
// candidates. Retries reuse the existing `embedding_publish`
// row (same `publish_id` and `qdrant_point_id`), bump
// `attempt_index = max(attempt_index)+1`, and append a fresh
// `queued` event before re-running steps 4-7. A model-version
// mismatch (operator bumped the embedder mid-flight) is
// surfaced as a warning and the row is left alone — the
// Stage 4.x flusher / supersede flow owns model bumps.
//
// Role / required DB grants
// -------------------------
// The `*sql.DB` MUST authenticate as a role with the
// following per-table grants (the full enumeration; the
// migration-level reference is 0016):
//
//	promoter_run                INSERT, SELECT, UPDATE  (lifecycle row)
//	concept                     SELECT                  (read candidate metadata; row-level FOR UPDATE only)
//	concept_version             INSERT, SELECT          (forward path append + version_index re-read)
//	embedding_publish           INSERT, SELECT          (publish row)
//	embedding_publish_event     INSERT, SELECT          (state-log events)
//
// All five grants are already issued by migration 0016 to
// `agent_memory_app`; this package adds no new tables and
// requires no additional migration.
package promoter
