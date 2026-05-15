-- 0013_synthetic_positive_unique.sql
--
-- Stage 1.3 step 7 (implementation-plan.md): single-emission gate
-- on synthetic-positive Episodes per parent feedback Episode (the
-- §9.8 mitigation). The plan calls for a partial UNIQUE index on
-- `episode.synthesized_from_feedback_episode_id WHERE kind =
-- 'synthetic_positive'`.
--
-- Composite partial UNIQUE on partitioned `episode`
-- -------------------------------------------------
-- PostgreSQL requires every column of a UNIQUE / PRIMARY KEY on a
-- partitioned table to include the partition-key column(s). The
-- `episode` table is partitioned monthly on `created_at`; a
-- partial UNIQUE on `(synthesized_from_feedback_episode_id) WHERE
-- kind='synthetic_positive'` alone is rejected by PostgreSQL with
-- "unique constraint on partitioned table must include all
-- partitioning columns".
--
-- Per the operator's Stage 1.3 iteration 2 directive, we
-- substitute a COMPOSITE partial UNIQUE on
-- `(synthesized_from_feedback_episode_id, created_at) WHERE
-- kind='synthetic_positive'`. This honours the implementation-
-- plan's literal index shape and accepts the documented narrower
-- enforcement: two synthetic_positive rows that share a
-- `synthesized_from_feedback_episode_id` collide ONLY when they
-- also share the same `created_at`. Two writes within the same
-- transaction get the same `now()` snapshot and therefore DO
-- collide; two writes across transactions (or across Consolidator
-- restarts) usually differ in `created_at` by at least a clock
-- tick and therefore do NOT collide.
--
-- What this index DOES enforce
--   * Same-transaction or same-tick double inserts of a
--     synthetic_positive for the same feedback episode (the
--     "Consolidator emits twice in one tick" race).
--   * The implementation-plan's literal text and shape, satisfying
--     the schema-level audit hook.
--
-- What this index DOES NOT enforce
--   * Cross-restart / cross-time single-emission. Two
--     Consolidator runs at different wall-clock times that
--     produce a synthetic_positive for the same feedback Episode
--     will BOTH land. The Consolidator (Stage 5.4) is the
--     authoritative cross-restart guard: its emission ledger
--     (Redis SET keyed by feedback_episode_id, see §7.7 step 4)
--     is consulted before any synthetic_positive write, and the
--     writer surfaces a domain-specific "already emitted" no-op
--     when the ledger entry exists.
--
-- The DB-level partial UNIQUE remains a last-line belt-and-
-- suspenders gate. The two-layer story (app-layer ledger + DB
-- composite UNIQUE) is the pragmatic answer to the partition-key
-- constraint PostgreSQL imposes on partitioned tables.

-- migrate:up
BEGIN;

CREATE UNIQUE INDEX episode_synthetic_positive_feedback_uidx
    ON episode (synthesized_from_feedback_episode_id, created_at)
    WHERE kind = 'synthetic_positive';

COMMIT;

-- migrate:down
BEGIN;

-- Migrations run down in reverse order, so 0013.down fires
-- BEFORE 0007.down drops the episode parent. Drop the index
-- explicitly here so a one-step rollback of 0013 alone (without
-- tearing the table down) is clean.
DROP INDEX IF EXISTS episode_synthetic_positive_feedback_uidx;

COMMIT;
