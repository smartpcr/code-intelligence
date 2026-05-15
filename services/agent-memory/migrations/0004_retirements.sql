-- 0004_retirements.sql
--
-- Stage 1.2 step 4 (implementation-plan.md): create the two
-- tombstone tables per architecture.md §5.2.4 and tech-spec
-- §8.7.2 (tombstone single-row enforcement, G5).
--
-- Both tables are append-only at the role layer (Stage 1.4 grants
-- INSERT/SELECT only); the per-row UNIQUE index on (node_id) /
-- (edge_id) gives us "exactly one tombstone per retired entity"
-- as a hard constraint at the schema layer.

-- migrate:up
BEGIN;

CREATE TABLE node_retirement (
    retirement_id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    node_id                uuid        NOT NULL REFERENCES node (node_id) ON DELETE RESTRICT,
    retired_at_sha         text        NOT NULL,
    retired_at             timestamptz NOT NULL DEFAULT now(),
    superseded_by_node_id  uuid        REFERENCES node (node_id) ON DELETE RESTRICT
);

-- G5: exactly one tombstone per Node id. The UNIQUE here is the
-- enforcement; the architecture's "current iff no tombstone"
-- anti-join in mgmt.read.graph_node depends on this.
CREATE UNIQUE INDEX node_retirement_node_id_uidx
    ON node_retirement (node_id);

CREATE INDEX node_retirement_retired_at_idx
    ON node_retirement (retired_at DESC);

CREATE TABLE edge_retirement (
    retirement_id  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    edge_id        uuid        NOT NULL REFERENCES edge (edge_id) ON DELETE RESTRICT,
    retired_at_sha text        NOT NULL,
    retired_at     timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX edge_retirement_edge_id_uidx
    ON edge_retirement (edge_id);

CREATE INDEX edge_retirement_retired_at_idx
    ON edge_retirement (retired_at DESC);

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS edge_retirement;
DROP TABLE IF EXISTS node_retirement;

COMMIT;
