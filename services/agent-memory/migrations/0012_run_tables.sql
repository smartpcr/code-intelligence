-- 0012_run_tables.sql
--
-- Stage 1.3 step 6 (implementation-plan.md): create the
-- `consolidator_run`, `promoter_run`, and `reranker_model` tables
-- per architecture.md §5.6 / tech-spec §8.4.
--
-- Mutability classification (tech-spec §8.7.4)
-- --------------------------------------------
-- All three tables are listed in §8.7.4 as **UPDATE-grantable**:
-- the Consolidator and Concept Promoter progress
-- `started_at -> finished_at -> status`, and the Reranker Trainer
-- mutates `reranker_model` as the model lifecycle advances. The
-- role grants in Stage 1.4 (0016) honour that by giving
-- `agent_memory_app` INSERT+SELECT+UPDATE on these tables.
--
-- Run status closed sets
-- ----------------------
-- The architecture-level enums in 0001 do not include a `run_status`
-- type. Architecture.md §5.6 leaves the status discriminator as
-- text. We keep these columns `text NOT NULL` here so a future
-- value (e.g. `'cancelled'`) does not require a schema migration;
-- the application layer enforces the closed set at insert time.
-- This matches the pattern used by `repo_commit.index_status`
-- in 0002.

-- migrate:up
BEGIN;

CREATE TABLE consolidator_run (
    run_id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    started_at               timestamptz NOT NULL DEFAULT now(),
    finished_at              timestamptz,
    -- episode_high_water_mark is the architectural cursor the
    -- next run begins from. It points at a partitioned Episode
    -- row; we drop the FK per the 0007 header convention.
    episode_high_water_mark  uuid,
    status                   text        NOT NULL DEFAULT 'pending'
);

-- Operator-visibility: "most recent runs first".
CREATE INDEX consolidator_run_started_idx
    ON consolidator_run (started_at DESC);

-- The Concept Promoter reads "latest finished ConsolidatorRun"
-- to bound its window per arch §7.7.
CREATE INDEX consolidator_run_finished_idx
    ON consolidator_run (finished_at DESC NULLS LAST);

CREATE TABLE promoter_run (
    run_id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    started_at          timestamptz NOT NULL DEFAULT now(),
    finished_at         timestamptz,
    concepts_promoted   int         NOT NULL DEFAULT 0,
    status              text        NOT NULL DEFAULT 'pending'
);

CREATE INDEX promoter_run_started_idx
    ON promoter_run (started_at DESC);

CREATE INDEX promoter_run_finished_idx
    ON promoter_run (finished_at DESC NULLS LAST);

-- Reranker registry (tech-spec §8.4: "Same PostgreSQL instance,
-- `reranker_model` table with `version`, `artifact_uri`,
-- `trained_at`, `metrics_json`").
CREATE TABLE reranker_model (
    model_id      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- A given model version string is published exactly once
    -- (every GraphReader fetches by version label, never by
    -- surrogate id). UNIQUE guards against double-publish.
    version       text        NOT NULL,
    artifact_uri  text        NOT NULL,
    trained_at    timestamptz NOT NULL DEFAULT now(),
    metrics_json  jsonb       NOT NULL DEFAULT '{}'::jsonb,
    -- A coarse 'published'/'shadow'/'retired' lifecycle flag the
    -- Reranker Trainer flips as new versions land. Stays text to
    -- avoid pinning the closed set in DDL (see header).
    status        text        NOT NULL DEFAULT 'shadow'
);

CREATE UNIQUE INDEX reranker_model_version_uidx
    ON reranker_model (version);

-- GraphReader: "latest published version" lookup.
CREATE INDEX reranker_model_status_trained_idx
    ON reranker_model (status, trained_at DESC);

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS reranker_model;
DROP TABLE IF EXISTS promoter_run;
DROP TABLE IF EXISTS consolidator_run;

COMMIT;
