-- 0009_observation.sql
--
-- Stage 1.3 step 3 (implementation-plan.md): create the
-- `observation` table per architecture.md §5.3.3, partitioned
-- monthly on `created_at` per tech-spec §8.7.3.
--
-- Single-target invariant
-- -----------------------
-- Architecture §5.3.3 says "Exactly one of node_id, edge_id,
-- concept_id, degraded_recall_context_id is non-null" AND each
-- target column is "Set iff role=<corresponding role>". Both
-- invariants are enforced as table-level CHECK constraints. The
-- combined CHECK (`observation_role_target_chk`) is strictly
-- stronger than the "exactly one non-null" form alone -- it
-- rejects role/target mismatches like `role=node_hit` with only
-- `concept_id` set. tech-spec §8.7.4 calls out the exactly-one
-- form as a minimum bar; we exceed it because the role/target
-- pairing is part of the architecture-level type signature.
--
-- FK choices
-- ----------
-- `node_id` and `edge_id` reference NON-partitioned parents
-- (Node / Edge from 0003) so we declare real FKs with
-- ON DELETE RESTRICT. `concept_id` references the (yet-to-be-
-- created) Concept table from migration 0011; we keep the
-- column without a DB-level FK to avoid a back-and-forth ALTER
-- across migration boundaries (the role/target CHECK above is
-- the substantive integrity gate; Concept is append-only per
-- G4 so the row never disappears anyway). `degraded_recall_
-- context_id` and `episode_id` reference partitioned parents
-- (RecallContextLog, Episode), so they follow the no-DB-FK
-- pattern documented at the head of 0007_episode.sql.
--
-- Weight column (no range CHECK -- intentional)
-- ---------------------------------------------
-- arch §5.3.3 defines `weight` as the caller-supplied "how much
-- did this element contribute to the action" measure and does
-- not pin a range. tech-spec §8.7.4 enumerates the schema-level
-- CHECK constraints required on this table (single-target plus
-- role/target pairing); a weight range CHECK is intentionally
-- not in that list. The reranker's positive-vs-negative training
-- signal lives on `Episode.outcome` (arch §3.6, §4.3) -- failure
-- / degraded / pre-correction `human_corrected` Episodes feed
-- the negative side, success / synthetic_positive Episodes feed
-- the positive side. Observation `weight` only expresses
-- contribution magnitude *within* a given Episode, so the schema
-- does not constrain its sign or scale. Contrast with
-- `concept_version_confidence_range_chk` in 0011 where the
-- [0.0, 1.0] bound is part of the confidence-band definition in
-- arch §5.5.2 and is therefore a schema-level invariant.

-- migrate:up
BEGIN;

CREATE TABLE observation (
    observation_id              uuid             NOT NULL DEFAULT gen_random_uuid(),
    episode_id                  uuid             NOT NULL,
    role                        observation_role NOT NULL,
    node_id                     uuid             REFERENCES node (node_id) ON DELETE RESTRICT,
    edge_id                     uuid             REFERENCES edge (edge_id) ON DELETE RESTRICT,
    concept_id                  uuid,
    degraded_recall_context_id  uuid,
    weight                      double precision NOT NULL DEFAULT 0,
    created_at                  timestamptz      NOT NULL DEFAULT now(),
    PRIMARY KEY (observation_id, created_at),

    -- tech-spec §8.7.4: "exactly one of ... is non-null".
    CONSTRAINT observation_exactly_one_target_chk CHECK (
        ( (node_id IS NOT NULL)::int
        + (edge_id IS NOT NULL)::int
        + (concept_id IS NOT NULL)::int
        + (degraded_recall_context_id IS NOT NULL)::int
        ) = 1
    ),

    -- arch §5.3.3 role/target pairing. Strictly stronger than the
    -- exactly-one CHECK alone -- catches `role=node_hit, concept_id=...`.
    CONSTRAINT observation_role_target_chk CHECK (
        (role = 'node_hit'                AND node_id                    IS NOT NULL)
        OR (role IN ('edge_hit', 'call_edge_hit') AND edge_id            IS NOT NULL)
        OR (role = 'concept_hit'          AND concept_id                 IS NOT NULL)
        OR (role = 'degraded_recall_context' AND degraded_recall_context_id IS NOT NULL)
    )
) PARTITION BY RANGE (created_at);

-- Per tech-spec §8.7.2 Observation needs a hot-path index for
-- partition-aware reads. The architecture's Observation table
-- has no `repo_id` column (unlike Episode/RecallContextLog), so
-- the §8.7.2 "(repo_id, created_at DESC)" wording is satisfied
-- by the parent Episode's repo_id reached through the join. The
-- physical index here keys on (episode_id, created_at DESC),
-- which is the column pair every read uses to gather an
-- Episode's observations in append order.
CREATE INDEX observation_episode_created_idx
    ON observation (episode_id, created_at DESC);

CREATE TABLE observation_default
    PARTITION OF observation DEFAULT;

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS observation_default;
DROP TABLE IF EXISTS observation;

COMMIT;
