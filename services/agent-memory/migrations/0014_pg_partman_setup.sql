-- 0014_pg_partman_setup.sql
--
-- Stage 1.3 step 8 (implementation-plan.md): register every
-- partitioned table created in 0005..0010 with pg_partman so
-- the partman background worker (Stage 1.1 docker stack) keeps
-- a rolling window of forward partitions ahead of the writer.
--
-- Tables registered (tech-spec §8.7.3):
--   * trace_observation_log -- weekly RANGE on started_at  (0005)
--   * episode               -- monthly RANGE on created_at (0007)
--   * episode_update        -- monthly RANGE on created_at (0008)
--   * observation           -- monthly RANGE on created_at (0009)
--   * recall_context_log    -- monthly RANGE on created_at (0010)
--
-- Why p_default_table := false
-- ----------------------------
-- Each upstream migration creates its own `<table>_default`
-- partition so the parent is insertable BEFORE pg_partman runs
-- and so the round-trip migration test does not have to depend
-- on partman side-effects. pg_partman ≥ 5 will refuse to create
-- a NEW default if one already exists, so we pass
-- `p_default_table := false` to reuse the user-created one.
--
-- Why p_premake := 3
-- ------------------
-- The implementation-plan explicitly calls for the first three
-- forward partitions. pg_partman creates the current-period
-- partition plus `p_premake` future ones at registration time,
-- and the BGW maintains that buffer thereafter.
--
-- Schema qualification
-- --------------------
-- The integration tests (test_migrate_test.go) create a fresh
-- random schema per test and `SET search_path` so partman
-- functions resolve. We pass `current_schema()` into every
-- parent-table argument so the migration is schema-independent
-- and the same SQL runs identically in production
-- (search_path=public) and per-test (search_path=amtest_xxxx).
--
-- Timezone pin
-- ------------
-- pg_partman computes partition boundaries against the current
-- session timezone. We set it to UTC for the duration of the
-- transaction so the partition names are deterministic and the
-- round-trip schema fingerprint stays stable.
--
-- Down protocol
-- -------------
-- The migration runner reverts in reverse order, so 0014.down
-- fires before any of 0005..0010.down. We:
--   1. Drop any template tables pg_partman provisioned for the
--      parents we own (looked up via partman.part_config so we
--      never guess names);
--   2. DELETE the exact part_config rows for our parents (no
--      LIKE-pattern broad delete -- another tenant's partman
--      configs are untouched);
--   3. Leave the actual child partition tables to the cascading
--      DROP TABLE in 0005..0010.down. Children are partitions
--      of the parent and disappear when the parent does.

-- migrate:up
BEGIN;

SET LOCAL timezone = 'UTC';

DO $$
DECLARE
    parent_schema text := current_schema();
    -- (table, control_col, interval). Order matches the
    -- on-disk migration order so a single-step partman recovery
    -- procedure reads top-to-bottom.
    parents constant text[][] := ARRAY[
        ARRAY['trace_observation_log', 'started_at', '1 week'],
        ARRAY['episode',               'created_at', '1 month'],
        ARRAY['episode_update',        'created_at', '1 month'],
        ARRAY['observation',           'created_at', '1 month'],
        ARRAY['recall_context_log',    'created_at', '1 month']
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

DO $$
DECLARE
    parent_schema text := current_schema();
    -- Exact parent names per the up block; LIKE-pattern deletes
    -- would also unregister other tenants' tables that happened
    -- to share the schema name prefix.
    parent_names constant text[] := ARRAY[
        'trace_observation_log',
        'episode',
        'episode_update',
        'observation',
        'recall_context_log'
    ];
    qualified text[];
    n text;
    tpl text;
BEGIN
    qualified := ARRAY[]::text[];
    FOREACH n IN ARRAY parent_names LOOP
        qualified := array_append(qualified, parent_schema || '.' || n);
    END LOOP;

    -- 1. Drop any partman-created template tables. We look them up
    --    in part_config so the names are authoritative; pg_partman
    --    versions differ in their template-naming convention.
    FOR tpl IN
        SELECT template_table
        FROM partman.part_config
        WHERE parent_table = ANY(qualified)
          AND template_table IS NOT NULL
    LOOP
        EXECUTE 'DROP TABLE IF EXISTS ' || tpl || ' CASCADE';
    END LOOP;

    -- 2. Delete exactly our parent rows. Other parent_table rows
    --    in part_config (other schemas, other tenants) are left
    --    untouched.
    DELETE FROM partman.part_config
    WHERE parent_table = ANY(qualified);
END$$;

COMMIT;
