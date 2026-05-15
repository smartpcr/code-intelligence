-- 0015_embedding_publish.sql
--
-- Stage 1.4 step 1 (implementation-plan.md): create the
-- EmbeddingPublish + EmbeddingPublishEvent state-log pair per
-- tech-spec §9.6a. Both tables are append-only -- tech-spec
-- §8.7.4 routes them to the no-UPDATE grant list in 0016 -- and
-- monthly-partitioned on `created_at` per §8.7.3, with the
-- `(publish_id, created_at DESC)` "latest event" B-tree from
-- §8.7.2.
--
-- Architecture invariants preserved
-- ---------------------------------
-- The architecture's `Node.embedding_vec` (arch §5.2.1) and
-- `ConceptVersion.embedding_vec` (arch §5.5.2) *logical fields*
-- are materialised through the Qdrant join via
-- `EmbeddingPublish.qdrant_point_id`; there is intentionally no
-- physical `embedding_vec` column on `node` or `concept_version`
-- (tech-spec §8.7.1). G4 / G5 immutability is stronger because
-- of that absence: no vector column to mutate.
--
-- The mitigation for cross-store staleness between PostgreSQL
-- and Qdrant (§9.6a) is the append-only event log created here:
-- the writer NEVER updates a prior row; every status transition
-- ('queued' -> 'vector_written' -> 'published', or 'failed', or
-- 'superseded' on re-embedding) is a fresh EmbeddingPublishEvent
-- row. The latest event for a publish_id is read through the
-- (publish_id, created_at DESC) index, and recall hits are
-- filtered through "latest event is 'published'" -- see
-- tech-spec §9.6a read protocol.
--
-- Target discriminator (two-column + exactly-one CHECK)
-- -----------------------------------------------------
-- A publish row carries EITHER `node_id` (Method/Block embedding
-- emitted by the Repo Indexer) OR `concept_version_id` (Concept
-- vector emitted by the Concept Promoter). A table-level CHECK
-- enforces "exactly one of those two is non-null", mirroring the
-- Observation single-target pattern from 0009. The two-column
-- shape is chosen over a unified `(target_kind, target_id)` pair
-- so that real PostgreSQL FOREIGN KEYs to the two non-partitioned
-- parents (`node` from 0003 and `concept_version` from 0011) can
-- enforce referential integrity at the DB layer. The FK to the
-- partitioned `embedding_publish` parent on the EmbeddingPublishEvent
-- side is intentionally absent (same rationale as the 0007 header
-- "no DB-level FK against partitioned parents").
--
-- Because both FKs use ON DELETE RESTRICT, every DELETE on a
-- parent row in `node` or `concept_version` forces PostgreSQL to
-- prove that no referencing row exists in `embedding_publish`.
-- Without an index on the FK column on the child side, that proof
-- is a sequential scan over every partition -- which becomes a
-- multi-second lock-holding scan once monthly partitions
-- accumulate. Partial indexes on the two FK columns (defined
-- below, after the parent table) back the FKs cheaply: the
-- `WHERE ... IS NOT NULL` predicate exploits the exactly-one-target
-- CHECK so each index only stores the ~50% of rows that actually
-- reference its parent.
--
-- pg_partman registration owned here, not 0014
-- --------------------------------------------
-- 0014_pg_partman_setup.sql registered the five partitioned
-- parents created in 0005..0010. Migrations are append-only on
-- disk, so a deployment that has already applied 0014 must not
-- have its `partman.part_config` rows mutated by an in-place
-- 0014 edit. Each newly-introduced partitioned parent therefore
-- registers itself in its own migration. The DO block below
-- mirrors the 0014 shape (timezone pin, current_schema(),
-- p_premake := 3, p_default_table := false) so prod and
-- per-test schemas behave identically, and the down protocol
-- mirrors 0014.down so part_config + template tables are
-- cleaned up before the parents are dropped.

-- migrate:up
BEGIN;

-- Closed-set event-kind ENUM per tech-spec §9.6a write protocol.
-- Members map 1:1 onto the §9.6a state machine; new members MUST
-- be added in a dedicated migration so the closed-set invariant
-- stays auditable.
CREATE TYPE embedding_publish_event_kind AS ENUM (
    'queued',
    'vector_written',
    'published',
    'failed',
    'superseded'
);

-- EmbeddingPublish -- one row per intended vector publish.
-- Append-only (immutable after insert): the 0016 role grants
-- deny UPDATE on this table to the application role.
CREATE TABLE embedding_publish (
    publish_id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    -- node_id and concept_version_id are the two architectural
    -- targets a vector publish may reference. Exactly one of
    -- them is non-null per the CHECK below. Both reference
    -- non-partitioned tables, so a real DB-level FK with
    -- ON DELETE RESTRICT is honoured here (compare with
    -- 0007/0008/0010 which point at partitioned parents and
    -- carry no DB-level FK -- see the 0007 header). The FK
    -- columns are backed by the partial indexes defined below
    -- so the RESTRICT lookup on parent DELETE is an index
    -- probe instead of a per-partition sequential scan.
    node_id                 uuid        REFERENCES node            (node_id)            ON DELETE RESTRICT,
    concept_version_id      uuid        REFERENCES concept_version (concept_version_id) ON DELETE RESTRICT,
    embedding_model_version text        NOT NULL,
    -- Qdrant addresses points by uuid in production deployments
    -- (the embedding writer chooses the uuid at publish time
    -- and carries it through the §9.6a state transitions).
    -- Storing it as uuid keeps the type discipline; deployments
    -- that map to a non-uuid Qdrant addressing model can widen
    -- this in a follow-up migration without losing data.
    qdrant_point_id         uuid        NOT NULL,
    created_at              timestamptz NOT NULL DEFAULT now(),
    -- Composite PK is forced by PostgreSQL's partitioned-table
    -- rule (see 0007 header for the full rationale).
    -- Architecturally `publish_id` alone is the identity-bearing
    -- column; the partition key tags along.
    PRIMARY KEY (publish_id, created_at),
    -- Exactly-one-target invariant. The architectural intent in
    -- §9.6a is that a publish row references either a Node or
    -- a ConceptVersion; never both, never neither. Mirrors the
    -- `observation_exactly_one_target_chk` pattern from 0009.
    CONSTRAINT embedding_publish_exactly_one_target_chk CHECK (
        ( (node_id            IS NOT NULL)::int
        + (concept_version_id IS NOT NULL)::int
        ) = 1
    )
) PARTITION BY RANGE (created_at);

-- FK-backing indexes on the two target discriminator columns.
-- Both FKs are ON DELETE RESTRICT, so a DELETE on `node` or
-- `concept_version` triggers a referential-integrity probe
-- against `embedding_publish`. Without these indexes the probe
-- degrades into a sequential scan across every monthly partition,
-- holding row-level locks on the parent table for the duration.
--
-- The `WHERE ... IS NOT NULL` predicate is safe and tight: the
-- `embedding_publish_exactly_one_target_chk` CHECK guarantees
-- exactly one of the two columns is non-null per row, so each
-- partial index covers ~50% of rows -- exactly the rows the
-- FK probe needs to find. Defined on the partitioned parent so
-- PostgreSQL propagates a matching index onto every existing
-- partition and pg_partman onto every future partition (same
-- propagation pattern as the §8.7.2 latest-event index below).
CREATE INDEX embedding_publish_node_id_idx
    ON embedding_publish (node_id)
    WHERE node_id IS NOT NULL;

CREATE INDEX embedding_publish_concept_version_id_idx
    ON embedding_publish (concept_version_id)
    WHERE concept_version_id IS NOT NULL;

-- Bootstrap default partition. pg_partman (registered below)
-- keeps the rolling window of dated partitions ahead of the
-- writer; the user-created default is retained because the
-- partman registration passes `p_default_table := false`.
CREATE TABLE embedding_publish_default
    PARTITION OF embedding_publish DEFAULT;

-- EmbeddingPublishEvent -- one row per status transition. The
-- writer NEVER updates a prior row; every status change is a new
-- row with a fresh `created_at` (§9.6a). `publish_id` has no
-- DB-level FK because the parent is partitioned (same rationale
-- as the 0007 header). App-level integrity is owned by the
-- EmbeddingIndex writer in the Repo Indexer (Stage 3.1) and
-- Concept Promoter (Stage 6.2).
CREATE TABLE embedding_publish_event (
    event_id      uuid                         NOT NULL DEFAULT gen_random_uuid(),
    publish_id    uuid                         NOT NULL,
    event_kind    embedding_publish_event_kind NOT NULL,
    -- `attempt_index` is the retry counter for a given publish
    -- transition; it is monotonically non-decreasing and must
    -- never be negative. Turning a decrement bug into a CHECK
    -- violation at the DB layer is consistent with how 0011
    -- guards `support_count` / `negative_count` on
    -- concept_version.
    attempt_index int                          NOT NULL DEFAULT 0,
    details_json  jsonb,
    created_at    timestamptz                  NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, created_at),
    CONSTRAINT embedding_publish_event_attempt_index_nonneg_chk
        CHECK (attempt_index >= 0)
) PARTITION BY RANGE (created_at);

-- tech-spec §8.7.2: "EmbeddingPublish event lookup -- B-tree on
-- (publish_id, created_at DESC) for the 'latest event' read in
-- §9.6a". On a partitioned table this index is a propagation
-- template; PostgreSQL creates a matching child index on every
-- partition and pg_partman creates one on every future partition
-- as it provisions them.
CREATE INDEX embedding_publish_event_publish_created_idx
    ON embedding_publish_event (publish_id, created_at DESC);

CREATE TABLE embedding_publish_event_default
    PARTITION OF embedding_publish_event DEFAULT;

-- pg_partman registration for the two new parents. Same shape
-- as 0014 (timezone pin, current_schema(), p_premake=3,
-- p_default_table=false) so test/prod behaviour matches the
-- existing partitioned parents. The SET LOCAL timezone is the
-- single most important defensive pin -- pg_partman computes
-- dated partition NAMES against the current session timezone,
-- and a drifted name breaks the round-trip canonical schema
-- fingerprint test.
SET LOCAL timezone = 'UTC';

DO $$
DECLARE
    parent_schema text := current_schema();
    parents constant text[][] := ARRAY[
        ARRAY['embedding_publish',       'created_at', '1 month'],
        ARRAY['embedding_publish_event', 'created_at', '1 month']
    ];
    p text[];
BEGIN
    FOREACH p SLICE 1 IN ARRAY parents LOOP
        PERFORM partman.create_parent(
            p_parent_table   := parent_schema || '.' || p[1],
            p_control        := p[2],
            p_interval       := p[3],
            p_type           := 'range',
            p_premake        := 3,
            p_default_table  := false
        );
    END LOOP;
END$$;

COMMIT;

-- migrate:down
BEGIN;

-- Mirror 0014.down: drop any partman-created template tables
-- for the parents we own, DELETE the exact part_config rows,
-- then drop the relations themselves. Children (dated
-- partitions) cascade with the partitioned parent on DROP TABLE.
-- The FK-backing indexes added on the partitioned parent are
-- propagated to every partition; they are dropped implicitly
-- by the DROP TABLE on the parent below, so no explicit DROP
-- INDEX is required (and would in fact fail on the partition
-- copies if attempted directly).
--
-- The two-step partman cleanup matters because:
--   1. If `part_config` still references this schema's parents
--      after the schema is dropped, the partman BGW will fail
--      to maintain a dangling parent_table reference. This is
--      what the per-test cleanup in test_migrate_test.go
--      independently guards against, but Down also has to clean
--      up cleanly so a manual `migrate down` outside the test
--      harness leaves no orphan rows behind.
--   2. The round-trip schema fingerprint test asserts byte-
--      identical state across Down/Up; a leftover partman row
--      would let the second `partman.create_parent` fail or
--      double-register, breaking the test.
DO $$
DECLARE
    parent_schema text := current_schema();
    parent_names constant text[] := ARRAY[
        'embedding_publish',
        'embedding_publish_event'
    ];
    qualified text[];
    n text;
    tpl text;
BEGIN
    qualified := ARRAY[]::text[];
    FOREACH n IN ARRAY parent_names LOOP
        qualified := array_append(qualified, parent_schema || '.' || n);
    END LOOP;

    -- pg_partman stores fully-qualified, already-quoted
    -- identifiers in template_table; format(%s,...) is the
    -- correct splice (%I would double-quote the whole string).
    FOR tpl IN
        SELECT template_table
        FROM partman.part_config
        WHERE parent_table = ANY(qualified)
          AND template_table IS NOT NULL
    LOOP
        EXECUTE format('DROP TABLE IF EXISTS %s CASCADE', tpl);
    END LOOP;

    DELETE FROM partman.part_config
    WHERE parent_table = ANY(qualified);
END$$;

DROP TABLE IF EXISTS embedding_publish_event_default;
DROP TABLE IF EXISTS embedding_publish_event;
DROP TABLE IF EXISTS embedding_publish_default;
DROP TABLE IF EXISTS embedding_publish;
DROP TYPE  IF EXISTS embedding_publish_event_kind;

COMMIT;
