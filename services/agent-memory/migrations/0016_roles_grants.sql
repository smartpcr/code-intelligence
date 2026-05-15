-- 0016_roles_grants.sql
--
-- Stage 1.4 step 2 (implementation-plan.md): create the
-- application + admin roles for the agent-memory service, and
-- grant exactly the per-table privilege set documented in
-- tech-spec §8.7.4.
--
-- Mutability classification mapped to grant shapes
-- ------------------------------------------------
-- INSERT, SELECT only -- no UPDATE, no DELETE -- on every table
-- the architecture pins as append-only:
--     node, edge, node_retirement, edge_retirement,
--     episode, episode_update, observation,
--     recall_context_log, trace_observation_log,
--     concept, concept_version, concept_support,
--     repo_commit (architectural "Commit" §5.1),
--     embedding_publish, embedding_publish_event (§9.6a).
--
-- INSERT, SELECT, UPDATE -- still no DELETE -- on the §8.7.4
-- UPDATE-grantable set:
--     trace_observation  (§5.2.3 mutable counter row),
--     repo               (§5.1 mutable settings),
--     consolidator_run   (§5.6 progress row),
--     promoter_run       (§5.6 progress row),
--     repo_event         (§5.6, not pinned as immutable upstream),
--     reranker_model     (§5.6 registry row),
--     ingest_jobs        (working queue from 0006a; status / attempt
--                         transitions are explicit UPDATEs).
--
-- DELETE is never granted to the application role. Partition
-- rotation and retention pruning are privileged operations
-- performed by `agent_memory_admin` (or operator-level roles
-- outside this migration); the §8.7.4 "DROP TABLE <partition>"
-- pattern intentionally bypasses row-level DELETE.
--
-- Concurrency posture: idempotent CREATE ROLE
-- -------------------------------------------
-- Roles are CLUSTER-WIDE in PostgreSQL. Integration tests in
-- test_migrate_test.go share a single Postgres instance across
-- many per-test schemas, so two test schemas' 0016.up calls can
-- race: both observe "role does not exist" via SELECT, both
-- attempt CREATE ROLE, the loser hits SQLSTATE 42710
-- (duplicate_object) and aborts the migration transaction. The
-- DO block below catches `duplicate_object` instead -- this is
-- the only race-safe shape PostgreSQL offers for CREATE ROLE,
-- because there is no `CREATE ROLE IF NOT EXISTS` syntax.
--
-- Down posture: REVOKE, never DROP ROLE
-- -------------------------------------
-- 0016.down REVOKEs every grant 0016.up issued but DOES NOT
-- DROP either role. Reasons:
--   1. The role is cluster-wide; a sibling tenant or a
--      concurrent test schema may still hold grants that
--      reference it. PostgreSQL rejects DROP ROLE with
--      SQLSTATE 2BP01 when any privilege depends on the role.
--   2. Leaving the role idle is harmless -- it has no LOGIN
--      and no remaining ACL entries after REVOKE.
--   3. The per-test cleanup in test_migrate_test.go drops the
--      whole schema with CASCADE, which removes the ACL
--      bindings to this schema's tables anyway; the role
--      itself persisting across test runs is by design.
--
-- Why NOLOGIN
-- -----------
-- The migration creates the application role as NOLOGIN by
-- design. Production deployments enable LOGIN by issuing a
-- separate operator step (so the credential never lives in
-- the version-controlled migration):
--
--   ALTER ROLE agent_memory_app WITH LOGIN PASSWORD '<from-secrets>';
--
-- The integration tests in test_stage14_role_grants_test.go
-- mirror that pattern -- they ALTER ROLE ... LOGIN with a
-- per-test random password before opening a fresh *sql.DB
-- connection as `agent_memory_app`, exercise the scenario,
-- then revert the role to NOLOGIN. That matches the
-- implementation-plan.md Stage 1.4 acceptance scenarios
-- which both call out "agent_memory_app role is logged in".
--
-- Schema-qualified GRANTs
-- -----------------------
-- All GRANT/REVOKE statements below are issued through a DO
-- loop that quotes the table name via `format('%I.%I', cs, t)`,
-- where `cs := current_schema()`. The earlier migrations use
-- unqualified DDL because their CREATE TABLE is unambiguous
-- under SET search_path. GRANT is more sensitive: an unqualified
-- `GRANT INSERT, SELECT ON node` would resolve through the
-- search_path, and a sibling tenant accidentally exposing a
-- `public.node` could siphon privileges. The DO-loop pattern
-- pins every grant to `current_schema().<table>` and removes
-- that surface.
--
-- Admin grant covers BOTH existing and future tables
-- --------------------------------------------------
-- `GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA` is a
-- point-in-time grant: it only covers tables that already exist
-- when 0016 runs. Any table created by a later migration
-- (Stage 2+ adds episodic/concept follow-ups, Stage 3+ adds new
-- log tables, etc.) would silently fall outside the admin role's
-- ACL, contradicting the "admin owns the full DML surface"
-- contract above and forcing every future migration author to
-- remember to re-issue the admin grant.
--
-- We close that gap with `ALTER DEFAULT PRIVILEGES IN SCHEMA`,
-- which writes a row into `pg_default_acl` so that any *future*
-- table created in this schema by the role that ran the ALTER
-- (i.e. the migration runner -- the same role that runs every
-- subsequent CREATE TABLE) automatically receives the admin
-- grant at creation time. There is no `FOR ROLE` clause: we
-- want the trigger keyed on whoever runs migrations, not on a
-- specific named role -- this keeps the per-tenant test pattern
-- (each test schema has its own current_user) working
-- identically.
--
-- The matching `ALTER DEFAULT PRIVILEGES ... REVOKE` in the
-- down block removes the pg_default_acl row so a Down/Up
-- round-trip leaves byte-identical catalog state (which the
-- round-trip schema fingerprint test in this package asserts).

-- migrate:up
BEGIN;

-- Race-safe role creation. SELECT-then-CREATE has a TOCTOU gap
-- under concurrent test schemas; CREATE-then-EXCEPTION does not.
-- SQLSTATE 42710 is `duplicate_object` -- the only error we
-- want to swallow. Any other error (insufficient_privilege,
-- syntax error, etc.) propagates and aborts the migration.
DO $$
BEGIN
    CREATE ROLE agent_memory_app NOLOGIN;
EXCEPTION WHEN duplicate_object THEN
    NULL;
END$$;

DO $$
BEGIN
    CREATE ROLE agent_memory_admin NOLOGIN;
EXCEPTION WHEN duplicate_object THEN
    NULL;
END$$;

-- USAGE on the current schema lets the roles resolve unqualified
-- table names against the search_path that the application
-- (and the integration tests) SET on connect. format(%I, ...)
-- safely quotes the per-test random schema name.
DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO agent_memory_app',   cs);
    EXECUTE format('GRANT USAGE ON SCHEMA %I TO agent_memory_admin', cs);
END$$;

-- Append-only set (INSERT + SELECT). The architectural intent
-- (G3 / G4 / G5 / C2 / C8) is enforced at the database layer
-- here, not at the writer layer.
DO $$
DECLARE
    cs text := current_schema();
    append_only_tables constant text[] := ARRAY[
        'node', 'edge',
        'node_retirement', 'edge_retirement',
        'episode', 'episode_update', 'observation',
        'recall_context_log', 'trace_observation_log',
        'concept', 'concept_version', 'concept_support',
        'repo_commit',
        'embedding_publish', 'embedding_publish_event'
    ];
    t text;
BEGIN
    FOREACH t IN ARRAY append_only_tables LOOP
        EXECUTE format(
            'GRANT INSERT, SELECT ON %I.%I TO agent_memory_app',
            cs, t
        );
    END LOOP;
END$$;

-- UPDATE-grantable set (INSERT + SELECT + UPDATE). DELETE is
-- intentionally withheld -- partition-drop and retention
-- pruning are admin-role operations, not application DML.
DO $$
DECLARE
    cs text := current_schema();
    update_grantable_tables constant text[] := ARRAY[
        'trace_observation',
        'repo',
        'consolidator_run', 'promoter_run',
        'repo_event',
        'reranker_model',
        'ingest_jobs'
    ];
    t text;
BEGIN
    FOREACH t IN ARRAY update_grantable_tables LOOP
        EXECUTE format(
            'GRANT INSERT, SELECT, UPDATE ON %I.%I TO agent_memory_app',
            cs, t
        );
    END LOOP;
END$$;

-- Admin role owns the full DML surface for operator-side fix-ups
-- (manual partition swaps, role rotations, retention pruning).
-- Sequences are unused in this schema (every PK defaults to
-- gen_random_uuid()), so ALL TABLES is sufficient -- no sequence
-- grants needed.
--
-- Two-step grant: the first EXECUTE covers tables that already
-- exist (every CREATE TABLE in 0001..0015), and the second
-- EXECUTE installs a default-privileges rule so any table
-- created in this schema by the migration runner from this
-- point onward (Stage 2+ migrations, etc.) is automatically
-- granted to the admin role at creation time. See the file
-- header "Admin grant covers BOTH existing and future tables"
-- block for the full rationale.
DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format(
        'GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA %I TO agent_memory_admin',
        cs
    );
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES IN SCHEMA %I '
        || 'GRANT ALL PRIVILEGES ON TABLES TO agent_memory_admin',
        cs
    );
END$$;

COMMIT;

-- migrate:down
BEGIN;

-- REVOKE the per-schema grants we issued. Roles are NOT dropped
-- (see file header: cluster-wide, possibly held by sibling
-- tenants; DROP ROLE would fail with SQLSTATE 2BP01 in that
-- case anyway).

DO $$
DECLARE
    cs text := current_schema();
    append_only_tables constant text[] := ARRAY[
        'node', 'edge',
        'node_retirement', 'edge_retirement',
        'episode', 'episode_update', 'observation',
        'recall_context_log', 'trace_observation_log',
        'concept', 'concept_version', 'concept_support',
        'repo_commit',
        'embedding_publish', 'embedding_publish_event'
    ];
    t text;
BEGIN
    FOREACH t IN ARRAY append_only_tables LOOP
        EXECUTE format(
            'REVOKE INSERT, SELECT ON %I.%I FROM agent_memory_app',
            cs, t
        );
    END LOOP;
END$$;

DO $$
DECLARE
    cs text := current_schema();
    update_grantable_tables constant text[] := ARRAY[
        'trace_observation',
        'repo',
        'consolidator_run', 'promoter_run',
        'repo_event',
        'reranker_model',
        'ingest_jobs'
    ];
    t text;
BEGIN
    FOREACH t IN ARRAY update_grantable_tables LOOP
        EXECUTE format(
            'REVOKE INSERT, SELECT, UPDATE ON %I.%I FROM agent_memory_app',
            cs, t
        );
    END LOOP;
END$$;

-- Symmetric admin revoke: drop the default-privileges row first
-- (so a future Up re-installs it cleanly and the round-trip
-- fingerprint test sees byte-identical pg_default_acl state),
-- then revoke from existing tables, then drop USAGE on the
-- schema for both roles.
DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES IN SCHEMA %I '
        || 'REVOKE ALL PRIVILEGES ON TABLES FROM agent_memory_admin',
        cs
    );
    EXECUTE format(
        'REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA %I FROM agent_memory_admin',
        cs
    );
    EXECUTE format('REVOKE USAGE ON SCHEMA %I FROM agent_memory_admin', cs);
    EXECUTE format('REVOKE USAGE ON SCHEMA %I FROM agent_memory_app',   cs);
END$$;

COMMIT;
