-- 0013_synthetic_positive_unique.sql
--
-- Stage 1.3 step 7 (implementation-plan.md): enforce that the
-- Consolidator (§7.7 step 4) emits AT MOST ONE synthetic-positive
-- Episode per parent feedback Episode, even across restarts (the
-- §9.8 mitigation). The plan literally calls for a partial UNIQUE
-- index on `episode.synthesized_from_feedback_episode_id WHERE
-- kind='synthetic_positive'`.
--
-- Why this is NOT a partial UNIQUE on `episode`
-- ---------------------------------------------
-- PostgreSQL requires every column of a UNIQUE / PRIMARY KEY on a
-- partitioned table to include the partition-key column(s). The
-- `episode` table is partitioned monthly on `created_at`; a
-- partial UNIQUE on `(synthesized_from_feedback_episode_id) WHERE
-- kind='synthetic_positive'` is rejected by PostgreSQL with
-- "unique constraint on partitioned table must include all
-- partitioning columns". Including `created_at` in the index
-- gives only per-partition uniqueness -- which does NOT prevent
-- the §9.8 risk (a Consolidator restart in the following month
-- could legitimately re-emit because the rows land in different
-- partitions).
--
-- Sentinel table + AFTER INSERT trigger
-- -------------------------------------
-- We honour the §9.8 mitigation's intent ("single-emission across
-- restarts") with a NON-partitioned sentinel table whose PRIMARY
-- KEY is exactly `synthesized_from_feedback_episode_id`. The
-- table is fed by an `AFTER INSERT` row-level trigger on
-- `episode` that fires only for `kind='synthetic_positive'`
-- rows. Because the trigger inserts in the same transaction as
-- the Episode insert, a duplicate sentinel PK aborts the entire
-- Episode insert -- which is exactly the behaviour the
-- implementation-plan test scenario expects ("When the second
-- is inserted, Then the partial UNIQUE index rejects it"). The
-- error surface mentions the sentinel table's PK rather than a
-- partial-unique index name; the writer (Stage 5.2) translates
-- the SQLSTATE into a domain-specific dedupe-noop.
--
-- AFTER INSERT row triggers on partitioned tables propagate to
-- every partition in PostgreSQL 13+; we run on 16, so the
-- trigger applies uniformly to user-created and pg_partman-
-- managed child partitions alike.

-- migrate:up
BEGIN;

CREATE TABLE synthetic_positive_emission (
    -- PK == the architectural uniqueness key. The
    -- implementation-plan's "partial UNIQUE on Episode
    -- WHERE kind='synthetic_positive'" is satisfied here at
    -- the cross-partition layer.
    synthesized_from_feedback_episode_id uuid        PRIMARY KEY,
    -- Pinned for forensic walk-back from this sentinel row to
    -- the Episode it gated. The composite Episode PK requires
    -- both columns; we carry `episode_created_at` so a join
    -- with partition pruning is cheap.
    episode_id                           uuid        NOT NULL,
    episode_created_at                   timestamptz NOT NULL,
    emitted_at                           timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX synthetic_positive_emission_episode_idx
    ON synthetic_positive_emission (episode_id, episode_created_at);

-- The trigger function inserts a sentinel row for every
-- synthetic_positive Episode insert. The `OR EXCEPTION` path is
-- intentional: a duplicate `synthesized_from_feedback_episode_id`
-- raises a PK-violation error which rolls back the Episode
-- insert. Writers detect this via the SQLSTATE on their side.
--
-- Function naming: namespaced to its trigger so 0013.down can
-- DROP FUNCTION cleanly without checking for other bindings.
CREATE FUNCTION episode_synthetic_positive_sentinel()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF NEW.kind = 'synthetic_positive' THEN
        -- The CHECK in 0007
        -- (episode_synthesized_from_feedback_provenance_chk) already
        -- guarantees synthesized_from_feedback_episode_id is
        -- non-null for synthetic_positive rows, so this insert
        -- is always well-formed.
        INSERT INTO synthetic_positive_emission (
            synthesized_from_feedback_episode_id,
            episode_id,
            episode_created_at
        ) VALUES (
            NEW.synthesized_from_feedback_episode_id,
            NEW.episode_id,
            NEW.created_at
        );
    END IF;
    RETURN NULL;  -- AFTER trigger; return value ignored
END;
$$;

CREATE TRIGGER episode_synthetic_positive_sentinel
    AFTER INSERT ON episode
    FOR EACH ROW
    EXECUTE FUNCTION episode_synthetic_positive_sentinel();

COMMIT;

-- migrate:down
BEGIN;

-- DROP TRIGGER first so DROP FUNCTION isn't blocked by a
-- depending trigger; DROP TABLE last because the trigger
-- function's body references it.
DROP TRIGGER  IF EXISTS episode_synthetic_positive_sentinel ON episode;
DROP FUNCTION IF EXISTS episode_synthetic_positive_sentinel();
DROP TABLE    IF EXISTS synthetic_positive_emission;

COMMIT;
