-- 0007_episode.sql
--
-- Stage 1.3 step 1 (implementation-plan.md): create the `episode`
-- table per architecture.md §5.3.1, partitioned monthly on
-- `created_at` per tech-spec §8.7.3.
--
-- Partitioned-table primary-key rule
-- ----------------------------------
-- PostgreSQL requires every column of a UNIQUE / PRIMARY KEY on a
-- partitioned table to include the partition key columns. The
-- architecture-level PK is logically `episode_id` alone (arch
-- §5.3.1), but PostgreSQL will reject `PRIMARY KEY (episode_id)`
-- on a table partitioned by `created_at`. We surface the physical
-- PK as the composite `(episode_id, created_at)` so:
--   * The row is uniquely addressable within its partition.
--   * `episode_id` remains the architectural identity (UUIDs make
--     a collision astronomically unlikely, and the writer never
--     reuses an episode_id across rows).
-- Child tables (EpisodeUpdate, Observation, ConceptSupport) that
-- carry an `episode_id` reference DO NOT declare a foreign-key
-- constraint to this table: a real FK to a partitioned parent
-- would require the child to carry the parent's partition key
-- as a second column. App-level referential integrity is owned
-- by the GraphWriter (Stage 5.1); each child carries an index
-- on `episode_id` so the join stays cheap.
--
-- Provenance CHECK constraints
-- ----------------------------
-- The architecture's field-table prose carries several "X iff Y"
-- invariants that the schema can enforce cheaply:
--   * `kind='synthetic_positive'` ⇒ both `synthesized_from_*`
--     fields are non-null (arch §5.3.1, G7).
--   * `kind='feedback'` ⇒ `parent_episode_id` is non-null
--     (arch §5.3.1: "Set on feedback rows").
--   * `degraded = true` ⇔ `degraded_reason IS NOT NULL`
--     (arch §5.3.1: "Set iff degraded=true").
--   * `outcome='human_corrected'` ⇒ `corrected_action` is
--     non-null (arch §5.3.1 / §6.2.2).
-- These CHECKs raise a hard error at insert time and let the
-- writer rely on a known-good row shape.
--
-- Default partition
-- -----------------
-- A bootstrap default partition lets this migration apply and
-- the round-trip test pass before 0014 wires pg_partman. The
-- default is registered with pg_partman in 0014 via
-- `p_default_table := false` so the user-created default is
-- retained and acts as the overflow bucket pg_partman expects.
-- Application writers MUST NOT rely on the default partition;
-- once 0014 has provisioned forward partitions every legitimate
-- insert lands in a dated partition.

-- migrate:up
BEGIN;

CREATE TABLE episode (
    episode_id       uuid         NOT NULL DEFAULT gen_random_uuid(),
    episode_group_id uuid         NOT NULL,
    repo_id          uuid         NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    session_id       text         NOT NULL,
    trace_id         text         NOT NULL,
    kind             episode_kind NOT NULL,
    -- parent_episode_id, synthesized_from_*, and context_id all
    -- logically reference partitioned tables (Episode itself,
    -- RecallContextLog). PostgreSQL FKs to partitioned parents
    -- require composite columns; we keep these as plain uuid?
    -- with no DB-level FK and rely on app-level integrity in
    -- the GraphWriter (Stage 5.1).
    parent_episode_id                    uuid,
    synthesized_from_parent_episode_id   uuid,
    synthesized_from_feedback_episode_id uuid,
    context_id                           uuid,
    action            jsonb       NOT NULL,
    outcome           outcome     NOT NULL,
    corrected_action  jsonb,
    signal_json       jsonb,
    degraded          boolean     NOT NULL DEFAULT false,
    degraded_reason   degraded_reason,
    created_at        timestamptz NOT NULL DEFAULT now(),
    -- Composite PK (see header). The leading column is the
    -- architectural identity; the trailing column is the
    -- partition key required by PostgreSQL.
    PRIMARY KEY (episode_id, created_at),
    -- Provenance invariants (see header).
    CONSTRAINT episode_synthetic_positive_provenance_chk CHECK (
        kind <> 'synthetic_positive'
        OR (
            synthesized_from_parent_episode_id   IS NOT NULL
            AND synthesized_from_feedback_episode_id IS NOT NULL
        )
    ),
    CONSTRAINT episode_feedback_provenance_chk CHECK (
        kind <> 'feedback'
        OR parent_episode_id IS NOT NULL
    ),
    CONSTRAINT episode_degraded_reason_chk CHECK (
        (degraded =  true  AND degraded_reason IS NOT NULL)
        OR (degraded = false AND degraded_reason IS     NULL)
    ),
    CONSTRAINT episode_corrected_action_chk CHECK (
        (outcome  = 'human_corrected' AND corrected_action IS NOT NULL)
        OR (outcome <> 'human_corrected' AND corrected_action IS     NULL)
    )
) PARTITION BY RANGE (created_at);

-- Hot-path read per tech-spec §8.7.2: mgmt.read.episodes carries
-- a `since` filter so the partition pruner engages on
-- (repo_id, created_at DESC).
CREATE INDEX episode_repo_created_idx
    ON episode (repo_id, created_at DESC);

-- Synthetic-positive provenance lookups (used by the Reranker
-- Trainer to walk back to the originating feedback Episode).
-- The 0013 sentinel table enforces uniqueness; this index just
-- makes the join cheap. Partial: only synthetic_positive rows
-- carry a non-null synthesized_from_feedback_episode_id.
CREATE INDEX episode_synthesized_from_feedback_idx
    ON episode (synthesized_from_feedback_episode_id)
    WHERE synthesized_from_feedback_episode_id IS NOT NULL;

-- Bootstrap default partition; pg_partman (0014) keeps the
-- rolling window of dated partitions ahead of the writer. See
-- the file header for the contract.
CREATE TABLE episode_default
    PARTITION OF episode DEFAULT;

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS episode_default;
DROP TABLE IF EXISTS episode;

COMMIT;
