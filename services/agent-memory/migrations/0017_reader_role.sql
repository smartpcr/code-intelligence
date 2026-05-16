-- 0017_reader_role.sql
--
-- Stage 2.2 step 4 (implementation-plan.md): create the
-- `agent_memory_ro` role used by the GraphReader library's
-- `pgxpool` connection pool. The brief calls for "the read-only
-- role"; Stage 1.4 (migration 0016) created `agent_memory_app`
-- (INSERT/SELECT/UPDATE on the surfaces it owns) and
-- `agent_memory_admin` (full DML for operator fix-ups). Neither
-- of those satisfies "read-only" — `agent_memory_app` can still
-- INSERT into every append-only log, and `admin` can do anything.
--
-- Why a separate role
-- -------------------
-- The GraphReader library is the only API used by `agent.recall`,
-- `agent.expand`, `agent.summarize`, and every `mgmt.read.*` verb
-- (architecture.md §4.1 / §4.5 / §6.2). A misconfigured caller
-- that opened a writer pool against the reader entry points
-- would silently re-introduce the G5 violation that 0016 closed
-- at the database layer — the only defence is a role whose ACL
-- is SELECT-only. Pool-level wiring in `internal/graphreader/pool.go`
-- authenticates as this role so the database refuses any DML
-- the reader tries to issue.
--
-- Grant shape
-- -----------
-- `agent_memory_ro` gets `SELECT` (no INSERT, no UPDATE, no
-- DELETE) on every table the reader library needs:
--   * Structural: `repo`, `repo_commit`, `node`, `edge`.
--   * Tombstones: `node_retirement`, `edge_retirement` — the
--     anti-join in GraphReader filters retired rows by default
--     (G5), so reader requires SELECT on the tombstone tables
--     too.
--   * Aggregates: `trace_observation` — neighborhood cards
--     (architecture.md §4.5) include the per-edge observation
--     counters.
--   * Episodic context (Stage 2.4+ consumers): `episode`,
--     `episode_update`, `observation`, `recall_context_log`,
--     `trace_observation_log` — `mgmt.read.context` resolves
--     a `context_id` to its dereferenced cards through this role.
--   * Concept registry: `concept`, `concept_version`,
--     `concept_support` — `agent.recall` and
--     `mgmt.read.concepts` need it.
--   * Run / registry: `consolidator_run`, `promoter_run`,
--     `repo_event`, `reranker_model` — `mgmt.read.runs` and the
--     reranker-model lookup hit these.
--   * Embedding state log: `embedding_publish`,
--     `embedding_publish_event` — needed to dereference Qdrant
--     point IDs from a Node row (tech-spec §8.7.1 / §9.6a).
--   * Job queue: `ingest_jobs` — `mgmt.read.ingest_status`
--     inspects pending / in-flight job rows.
--
-- DELETE is never granted. UPDATE is never granted. The
-- reader's contract is "no DML"; the role grants enforce it.
--
-- Default privileges (forward-compatibility)
-- ------------------------------------------
-- Same shape as 0016 (`ALTER DEFAULT PRIVILEGES ... GRANT SELECT
-- ON TABLES TO agent_memory_ro`): a future Stage N migration
-- adding a new read-visible table inherits the SELECT grant
-- automatically. Without this, every new table would silently
-- fall outside the reader ACL and downstream `mgmt.read.*`
-- queries would fail at runtime.
--
-- Concurrency / NOLOGIN posture
-- -----------------------------
-- Mirrors 0016 byte-for-byte: NOLOGIN at creation time;
-- production sets `LOGIN PASSWORD '<secret>'` via a separate
-- operator step; integration tests flip LOGIN with a per-test
-- random password gated by a cluster-wide `pg_advisory_lock`
-- (`testpglock.AcquireRoRoleLogin`) so concurrent test binaries
-- can't clobber each other's password.

-- migrate:up
BEGIN;

DO $$
BEGIN
    CREATE ROLE agent_memory_ro NOLOGIN;
EXCEPTION WHEN duplicate_object THEN
    NULL;
END$$;

DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO agent_memory_ro', cs);
END$$;

-- SELECT-only on every table the GraphReader resolves against.
-- Listed verbatim so the role ACL is auditable from this single
-- migration; future tables get SELECT via the
-- ALTER DEFAULT PRIVILEGES block below.
DO $$
DECLARE
    cs text := current_schema();
    readable_tables constant text[] := ARRAY[
        -- Structural
        'repo', 'repo_commit', 'node', 'edge',
        -- Tombstones (G5 anti-join inputs)
        'node_retirement', 'edge_retirement',
        -- Aggregates and span log
        'trace_observation', 'trace_observation_log',
        -- Episodic surface
        'episode', 'episode_update', 'observation', 'recall_context_log',
        -- Concept registry
        'concept', 'concept_version', 'concept_support',
        -- Operational / registry
        'consolidator_run', 'promoter_run', 'repo_event',
        'reranker_model', 'ingest_jobs',
        -- Embedding state log
        'embedding_publish', 'embedding_publish_event'
    ];
    t text;
BEGIN
    FOREACH t IN ARRAY readable_tables LOOP
        EXECUTE format(
            'GRANT SELECT ON %I.%I TO agent_memory_ro',
            cs, t
        );
    END LOOP;
END$$;

-- Future-table coverage: any table created in this schema by
-- the migration runner from this point onward picks up the
-- SELECT grant automatically. Same shape as the admin
-- ALTER DEFAULT PRIVILEGES block in 0016; the down half drops
-- the pg_default_acl row so a round-trip is byte-identical.
DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES IN SCHEMA %I '
        || 'GRANT SELECT ON TABLES TO agent_memory_ro',
        cs
    );
END$$;

COMMIT;

-- migrate:down
BEGIN;

DO $$
DECLARE
    cs text := current_schema();
    readable_tables constant text[] := ARRAY[
        'repo', 'repo_commit', 'node', 'edge',
        'node_retirement', 'edge_retirement',
        'trace_observation', 'trace_observation_log',
        'episode', 'episode_update', 'observation', 'recall_context_log',
        'concept', 'concept_version', 'concept_support',
        'consolidator_run', 'promoter_run', 'repo_event',
        'reranker_model', 'ingest_jobs',
        'embedding_publish', 'embedding_publish_event'
    ];
    t text;
BEGIN
    FOREACH t IN ARRAY readable_tables LOOP
        EXECUTE format(
            'REVOKE SELECT ON %I.%I FROM agent_memory_ro',
            cs, t
        );
    END LOOP;
END$$;

DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES IN SCHEMA %I '
        || 'REVOKE SELECT ON TABLES FROM agent_memory_ro',
        cs
    );
    EXECUTE format('REVOKE USAGE ON SCHEMA %I FROM agent_memory_ro', cs);
END$$;

-- agent_memory_ro is left in place (cluster-wide; sibling
-- tenants may still hold grants). Same rationale as 0016.

COMMIT;
