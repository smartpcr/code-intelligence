-- 0011_concept.sql
--
-- Stage 1.3 step 5 (implementation-plan.md): create the
-- `concept`, `concept_version`, and `concept_support` tables per
-- architecture.md §5.5. These are the bottom of the Concept layer
-- (G4 -- append-only) and are NOT partitioned per tech-spec
-- §8.7.3 (bounded cardinality: O(promoted concepts) and
-- O(supporting episodes) which both grow slowly).
--
-- Cross-repo Concept fingerprint
-- ------------------------------
-- Per G6 the Concept fingerprint is cross-repo: `UNIQUE
-- (fingerprint)` with NO `repo_id` column. Two repositories that
-- canonicalise the same concept hash collide into the same row.
-- This is the architectural intent (§3.4 step 5) -- shared
-- patterns surface across repos.
--
-- Polymorphic producer_run_id
-- ---------------------------
-- ConceptVersion.producer_run_id points at `consolidator_run`
-- (when `producer='consolidator'`) or `promoter_run` (when
-- `producer='promoter'`) per arch §5.5.2. There is no single
-- table to FK against; the dispatch is by the `producer` enum.
-- We keep the column as plain `uuid` with no DB-level FK and
-- rely on app-level integrity in the Consolidator (Stage 5.2)
-- and Concept Promoter (Stage 7.8). Same pattern as the
-- partitioned-parent FK drop documented at the head of 0007.
--
-- Episode reference from ConceptSupport
-- -------------------------------------
-- ConceptSupport.episode_id (optional) references the partitioned
-- Episode table; the no-DB-FK pattern from 0007 applies.

-- migrate:up
BEGIN;

CREATE TABLE concept (
    concept_id      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    fingerprint     bytea       NOT NULL,
    name            text        NOT NULL,
    description_md  text        NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT concept_fingerprint_octet_length_chk
        CHECK (octet_length(fingerprint) = 32)
);

-- G6: cross-repo concept uniqueness per tech-spec §8.7.2.
CREATE UNIQUE INDEX concept_fingerprint_uidx
    ON concept (fingerprint);

CREATE TABLE concept_version (
    concept_version_id  uuid             PRIMARY KEY DEFAULT gen_random_uuid(),
    concept_id          uuid             NOT NULL REFERENCES concept (concept_id) ON DELETE RESTRICT,
    version_index       int              NOT NULL,
    confidence          double precision NOT NULL,
    confidence_band     concept_band     NOT NULL,
    support_count       int              NOT NULL DEFAULT 0,
    negative_count      int              NOT NULL DEFAULT 0,
    producer            producer         NOT NULL,
    -- See header: polymorphic FK across consolidator_run / promoter_run;
    -- enforced at app level by the producer-specific writer.
    producer_run_id     uuid             NOT NULL,
    -- `embedding_vec` is intentionally absent (tech-spec §8.7.1):
    -- vectors live in Qdrant, dereferenced via the
    -- EmbeddingPublish log pair (§9.6a, Stage 1.4).
    promoted            boolean          NOT NULL DEFAULT false,
    created_at          timestamptz      NOT NULL DEFAULT now(),
    CONSTRAINT concept_version_confidence_range_chk
        CHECK (confidence >= 0.0 AND confidence <= 1.0),
    CONSTRAINT concept_version_version_index_positive_chk
        CHECK (version_index >= 0),
    -- support_count / negative_count are append-only counters
    -- incremented by the Consolidator (Stage 5.2) and Concept
    -- Promoter (Stage 7.8). They must never go negative; the
    -- CHECK turns a decrement bug into a constraint violation
    -- at the DB instead of corrupt counter state.
    CONSTRAINT concept_version_support_count_nonneg_chk
        CHECK (support_count >= 0),
    CONSTRAINT concept_version_negative_count_nonneg_chk
        CHECK (negative_count >= 0)
);

-- arch §5.5.2 + tech-spec §8.7.2: "most recent ConceptVersion"
-- read; ordered DESC on version_index so a `LIMIT 1` returns
-- the latest version in one index seek.
CREATE INDEX concept_version_concept_version_idx
    ON concept_version (concept_id, version_index DESC);

-- Uniqueness on (concept_id, version_index) guards against the
-- "two concurrent producers wrote v=N" race. The architecture
-- says version_index is monotonic per concept_id; this UNIQUE
-- index turns the invariant into a hard constraint.
CREATE UNIQUE INDEX concept_version_concept_version_uidx
    ON concept_version (concept_id, version_index);

CREATE TABLE concept_support (
    support_id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    concept_id          uuid        NOT NULL REFERENCES concept (concept_id) ON DELETE RESTRICT,
    concept_version_id  uuid        NOT NULL REFERENCES concept_version (concept_version_id) ON DELETE RESTRICT,
    repo_id             uuid        NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    -- node_id is optional per arch §5.5.3; Node is non-partitioned
    -- so the FK works straightforwardly.
    node_id             uuid        REFERENCES node (node_id) ON DELETE RESTRICT,
    -- episode_id is optional per arch §5.5.3; Episode is
    -- partitioned so no DB-level FK (see 0007 header).
    episode_id          uuid,
    polarity            polarity    NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now()
);

-- Reranker-trainer scan: "all support for concept_version_id"
-- in append order.
CREATE INDEX concept_support_concept_version_idx
    ON concept_support (concept_version_id, created_at DESC);

-- Per-concept rollup queries.
CREATE INDEX concept_support_concept_idx
    ON concept_support (concept_id, created_at DESC);

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS concept_support;
DROP TABLE IF EXISTS concept_version;
DROP TABLE IF EXISTS concept;

COMMIT;
