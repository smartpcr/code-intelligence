-- 0006b_ingest_jobs_affected_node_count.sql
--
-- Stage 3.4 (Delta re-index handler) — evaluator finding #5.
--
-- The Repo Indexer's delta-mode publish step writes the
-- `repo.delta_ingested` event payload's `affected_node_count`
-- field from the in-memory `DeltaSummary` produced by runDelta.
-- That accumulator counts mutations made during the CURRENT run
-- (nodes the AST emitter touched + nodes the run tombstoned).
--
-- A publish-step failure (NOTIFY broker outage, network flap,
-- ambiguous commit ack) requeues the row to status='pending';
-- the next claim re-enters runDelta which re-processes the same
-- FileChange list. The second-attempt mutations are mostly
-- no-ops (rows already retired or already inserted), so the
-- second-attempt `DeltaSummary.AffectedNodeCount()` is much
-- smaller than the first attempt's. Publishing the smaller
-- second-attempt count would silently lie to downstream
-- consumers about how much work the delta did.
--
-- This migration adds an `affected_node_count` column to
-- `ingest_jobs` so the count can be persisted monotonically
-- across retries. The worker writes
-- `affected_node_count = GREATEST(stored, current_run)` via an
-- autocommit UPDATE BEFORE the publish-tx so the first attempt's
-- higher value sticks through a subsequent rollback, and the
-- published event reads the persisted value from the row.
--
-- NOT NULL DEFAULT 0 is safe on populated tables: PostgreSQL
-- 11+ fast-path adds a NOT NULL default column without rewriting
-- the heap. Existing rows materialise as 0 (the natural "no
-- delta yet" sentinel for full / manual / not-yet-published
-- delta rows).

-- migrate:up
BEGIN;

ALTER TABLE ingest_jobs
    ADD COLUMN affected_node_count int NOT NULL DEFAULT 0;

-- The column is operator-readable; the role grant policy from
-- migration 0016 already gives `agent_memory_app` UPDATE on the
-- full `ingest_jobs` row set, so no extra grant is needed for
-- the new column.

COMMIT;

-- migrate:down
BEGIN;

ALTER TABLE ingest_jobs
    DROP COLUMN IF EXISTS affected_node_count;

COMMIT;
