-- 0010_recall_context_log.sql
--
-- Stage 1.3 step 4 (implementation-plan.md): create the
-- `recall_context_log` table per architecture.md §5.4.1,
-- partitioned monthly on `created_at` per tech-spec §8.7.3.
--
-- `node_ids`, `edge_ids`, `concept_ids` are `uuid[]` per
-- architecture.md §5.4.1 (and tech-spec §8.7.1 type mapping
-- "uuid[] -> uuid[]"). Order is preserved because the
-- architecture says "Ordered list of ... ids returned".
--
-- The composite PK + no-DB-FK pattern mirrors 0007; see the
-- head of 0007_episode.sql for the rationale. Episode rows
-- carry a `context_id` that logically references this table;
-- the writer enforces that integrity at app level.

-- migrate:up
BEGIN;

CREATE TABLE recall_context_log (
    context_id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    repo_id                 uuid        NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    verb                    verb        NOT NULL,
    query_json              jsonb       NOT NULL,
    -- Ordered arrays per arch §5.4.1. PostgreSQL preserves
    -- insertion order in `uuid[]` columns; the writer must
    -- pass each list in rank order.
    node_ids                uuid[]      NOT NULL DEFAULT ARRAY[]::uuid[],
    edge_ids                uuid[]      NOT NULL DEFAULT ARRAY[]::uuid[],
    concept_ids             uuid[]      NOT NULL DEFAULT ARRAY[]::uuid[],
    reranker_model_version  text        NOT NULL,
    served_under_degraded   boolean     NOT NULL DEFAULT false,
    created_at              timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (context_id, created_at)
) PARTITION BY RANGE (created_at);

-- Hot-path: mgmt.read.recall_contexts walks by repo_id +
-- since-filter per architecture.md §6.2 / tech-spec §8.7.2.
CREATE INDEX recall_context_log_repo_created_idx
    ON recall_context_log (repo_id, created_at DESC);

CREATE TABLE recall_context_log_default
    PARTITION OF recall_context_log DEFAULT;

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS recall_context_log_default;
DROP TABLE IF EXISTS recall_context_log;

COMMIT;
