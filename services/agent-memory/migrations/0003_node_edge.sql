-- 0003_node_edge.sql
--
-- Stage 1.2 step 3 (implementation-plan.md): create the `node` and
-- `edge` tables.
--
-- Per tech-spec §8.7.1:
--   * NO `embedding_vec` column on `node` -- the architecture.md
--     §5.2.1 `embedding_vec` logical field is satisfied by joining
--     to the `EmbeddingPublish` log pair (§9.6a) and dereferencing
--     into Qdrant. There is no PostgreSQL vector column.
--   * `fingerprint` is `bytea` with a CHECK that the length is
--     exactly 32 bytes (G2 invariant: 32-byte sha256 digest).
--
-- Per tech-spec §8.7.2:
--   * UNIQUE `(repo_id, fingerprint)` on `node` and `edge`. This
--     is the G2 dedupe path that lets the Repo Indexer's
--     idempotent re-ingest of the same SHA land identical rows.
--
-- Per architecture.md §5.2.1 / §5.2.2 (G5 row immutability):
--   * No `to_sha` column on either table -- retirement is recorded
--     by an append-only tombstone in migration 0004.

-- migrate:up
BEGIN;

CREATE TABLE node (
    node_id             uuid       PRIMARY KEY DEFAULT gen_random_uuid(),
    fingerprint         bytea      NOT NULL,
    repo_id             uuid       NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    kind                node_kind  NOT NULL,
    canonical_signature text       NOT NULL,
    parent_node_id      uuid       REFERENCES node (node_id) ON DELETE RESTRICT,
    from_sha            text       NOT NULL,
    -- Language-specific attributes (visibility, return type,
    -- block_kind discriminator for Block nodes, etc.). Set at
    -- insert time only (G5).
    attrs_json          jsonb      NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT node_fingerprint_octet_length_chk
        CHECK (octet_length(fingerprint) = 32)
);

-- G2 dedupe per tech-spec §8.7.2.
CREATE UNIQUE INDEX node_repo_fingerprint_uidx
    ON node (repo_id, fingerprint);

-- Parent-chain walks for the Repo→Package→File→Class→Method→Block
-- hierarchy. Used by mgmt.read.graph_node and by the Repo Indexer
-- when stitching block parents.
CREATE INDEX node_parent_idx
    ON node (parent_node_id)
    WHERE parent_node_id IS NOT NULL;

CREATE INDEX node_repo_kind_idx
    ON node (repo_id, kind);

CREATE TABLE edge (
    edge_id     uuid      PRIMARY KEY DEFAULT gen_random_uuid(),
    fingerprint bytea     NOT NULL,
    repo_id     uuid      NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    kind        edge_kind NOT NULL,
    src_node_id uuid      NOT NULL REFERENCES node (node_id) ON DELETE RESTRICT,
    dst_node_id uuid      NOT NULL REFERENCES node (node_id) ON DELETE RESTRICT,
    from_sha    text      NOT NULL,
    attrs_json  jsonb     NOT NULL DEFAULT '{}'::jsonb,
    CONSTRAINT edge_fingerprint_octet_length_chk
        CHECK (octet_length(fingerprint) = 32)
);

CREATE UNIQUE INDEX edge_repo_fingerprint_uidx
    ON edge (repo_id, fingerprint);

-- Forward / reverse adjacency lookups for static call-chain
-- expansion (architecture.md §4.5). These are the most common
-- GraphReader hot paths.
CREATE INDEX edge_src_kind_idx ON edge (src_node_id, kind);
CREATE INDEX edge_dst_kind_idx ON edge (dst_node_id, kind);

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS edge;
DROP TABLE IF EXISTS node;

COMMIT;
