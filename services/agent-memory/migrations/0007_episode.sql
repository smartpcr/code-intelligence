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
-- The architecture's field-table prose (§5.3.1) carries several
-- bidirectional ("iff") invariants. We encode them at the schema
-- level so a malformed row is rejected at insert time and the
-- downstream readers (Consolidator, mgmt.read.episodes) can rely
-- on a known-good row shape.
--
--   * `synthesized_from_parent_episode_id IS NOT NULL`
--     IFF `kind='synthetic_positive'`
--     (arch §5.3.1: "Set on `synthetic_positive` rows only", G7).
--   * `synthesized_from_feedback_episode_id IS NOT NULL`
--     IFF `kind='synthetic_positive'`
--     (arch §5.3.1: "Set on `synthetic_positive` rows only", G7).
--   * `parent_episode_id IS NOT NULL`
--     IFF `kind='feedback'`
--     (arch §5.3.1: "Set on `feedback` rows ... Not set on
--      `synthetic_positive` rows"). `agent` rows do not carry a
--     parent_episode_id either; the architectural reference for
--     `agent` lineage is via `episode_group_id`.
--   * `context_id IS NULL` is LEGAL ONLY when `kind='feedback'`
--     (arch §5.3.1: "NULL is legal **only** for `feedback`
--     Episodes"). `agent` carries the recall context it consumed;
--     `synthetic_positive` copies the parent's `context_id` per G7.
--     The wording is one-directional: `feedback` rows MAY still
--     carry a non-null `context_id` (operator re-ran recall
--     before filing feedback). We encode the actively-checkable
--     contrapositive (`kind = 'feedback' OR context_id IS NOT NULL`)
--     rather than an iff.
--   * `degraded = true` IFF `degraded_reason IS NOT NULL`
--     (arch §5.3.1: "Set iff degraded=true").
--   * `outcome='human_corrected'` IFF `corrected_action IS NOT NULL`
--     (arch §5.3.1 / §6.2.2: "Required when outcome=human_corrected;
--     otherwise null").
--
-- context_id contract vs. arch §6.1.2 (`agent.observe`)
-- -----------------------------------------------------
-- Arch §6.1.2 declares `agent.observe(repo_id, session_id,
-- trace_id, action, outcome, signal?, context_id?,
-- observation_refs?)` with `context_id?` syntactically optional
-- on the wire. The schema does NOT honour that optionality for
-- non-`feedback` rows: the CHECK above rejects
-- `context_id IS NULL` whenever `kind <> 'feedback'`. This is
-- intentional and consistent with the field-table prose in
-- arch §5.3.1.
--
-- The contract is reconciled at the writer layer (Stage 5.1
-- GraphWriter), which owns the §6.1.2 → §5.3.1 translation:
--   * The canonical agent flow (arch §6.1.2 step 3 and the §7.3
--     sequence diagram) ALWAYS pairs `agent.observe` with a
--     preceding `agent.recall`. Recall always returns a
--     `context_id` -- including under degradation, where the id
--     references a `RecallContextLog` row with
--     `served_under_degraded=true` (arch §6.1.4). Stage 5.1
--     therefore never sees an `agent.observe` without a
--     `context_id` on the canonical path.
--   * `feedback` Episodes (arch §7.4) are written with
--     `context_id=NULL, parent_episode_id=<parent>`; the
--     `feedback` branch of the CHECK lets that row through.
--   * `synthetic_positive` Episodes (arch §7.7 step 4) COPY the
--     parent agent Episode's `context_id` per G7; the parent is
--     by construction non-null.
--
-- If a future flow legitimately produces a context-less non-
-- feedback Episode (e.g. a span-ingestor synthesised observation
-- with no recall), the writer MUST either (a) materialise a
-- degraded `RecallContextLog` row and supply its id, or (b)
-- relax this CHECK to a one-directional form in a follow-up
-- migration. The closed-set posture is by design: a context-less
-- agent Episode is at risk of dangling provenance for the
-- Consolidator and Reranker Trainer.
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
    -- Provenance invariants (see header). All four
    -- synthesized_from_* / parent_episode_id checks are
    -- bidirectional: the architectural wording is "set on X rows
    -- only", and the corresponding NULL/NOT-NULL relationship is
    -- enforced in both directions so a malformed (kind, field)
    -- combination is rejected regardless of which side of the
    -- equation is wrong.
    CONSTRAINT episode_synthesized_from_parent_provenance_chk CHECK (
        (kind  = 'synthetic_positive' AND synthesized_from_parent_episode_id IS NOT NULL)
        OR (kind <> 'synthetic_positive' AND synthesized_from_parent_episode_id IS NULL)
    ),
    CONSTRAINT episode_synthesized_from_feedback_provenance_chk CHECK (
        (kind  = 'synthetic_positive' AND synthesized_from_feedback_episode_id IS NOT NULL)
        OR (kind <> 'synthetic_positive' AND synthesized_from_feedback_episode_id IS NULL)
    ),
    CONSTRAINT episode_parent_episode_id_provenance_chk CHECK (
        (kind  = 'feedback' AND parent_episode_id IS NOT NULL)
        OR (kind <> 'feedback' AND parent_episode_id IS NULL)
    ),
    -- context_id IS NULL is LEGAL ONLY when kind='feedback'. The
    -- spec phrasing is one-directional ("NULL is legal only for
    -- feedback"); we encode the contrapositive as a CHECK because
    -- it is the actively-checkable form. feedback rows MAY still
    -- have a non-null context_id (e.g. operator re-ran recall
    -- before submitting feedback), so this is NOT iff.
    CONSTRAINT episode_context_id_required_unless_feedback_chk CHECK (
        kind = 'feedback' OR context_id IS NOT NULL
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
-- Partial: only synthetic_positive rows carry a non-null
-- synthesized_from_feedback_episode_id (enforced by
-- episode_synthesized_from_feedback_provenance_chk above).
--
-- This non-unique index complements the COMPOSITE PARTIAL UNIQUE
-- introduced by 0013 (`episode_synthetic_positive_feedback_uidx`
-- on `(synthesized_from_feedback_episode_id, created_at) WHERE
-- kind='synthetic_positive'`). Queries that filter only by
-- `synthesized_from_feedback_episode_id = $1` use this single-
-- column partial; queries that pin both the feedback id and a
-- created_at range use the 0013 composite. Keeping both is
-- intentional: dropping this index would force the planner to
-- treat the 0013 unique's `(col1, col2)` as a `col1`-prefix scan,
-- which is correct but defeats the partial-index pruning.
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
