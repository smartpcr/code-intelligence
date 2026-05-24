-- 0004_roles.up.sql
--
-- Stage 1.5 (implementation-plan.md "PostgreSQL role grants and
-- schema isolation"): create the nine writer roles for the
-- CLEAN-CODE service and grant the per-table privilege set pinned
-- by tech-spec Sec 7.2.
--
-- Roles created (tech-spec Sec 7.2 lines 1232-1261 -- the
-- canonical role names appearing on the actual GRANT/REVOKE
-- statements). NO `clean_code_aggregator`, `clean_code_rule_engine`,
-- or `clean_code_audit_replayer` aliases -- those names are NEVER
-- created in v1.
--
--   * clean_code_repo_indexer        -- Management's delegated
--                                       writer for Commit initial
--                                       rows and RepoEvent
--                                       (architecture Sec 3.3,
--                                       tech-spec Sec 7.2 line 691).
--   * clean_code_metric_ingestor     -- Foundation-tier MetricSample
--                                       writer + sole UPDATE of
--                                       `commit.scan_status` per
--                                       architecture Sec 1.5.1 row 1.
--   * clean_code_xrepo_aggregator    -- System-tier MetricSample
--                                       writer (`pack='system' AND
--                                       source='derived'`) +
--                                       RepoMetricSnapshot /
--                                       CrossRepoPercentile /
--                                       PortfolioSnapshot writer
--                                       (architecture Sec 3.10 step 4).
--   * clean_code_policy_steward      -- Sole writer of Rule,
--                                       RulePack, PolicyVersion,
--                                       PolicyActivation,
--                                       Threshold, Override
--                                       (architecture Sec 3.11);
--                                       also the writer of the
--                                       MetricKind catalogue rows.
--   * clean_code_solid_batch         -- SOLID Rule Engine batch
--                                       worker writes
--                                       EvaluationRun /
--                                       EvaluationVerdict /
--                                       Finding with
--                                       `caller='batch_refresh'`
--                                       (architecture Sec 3.6).
--   * clean_code_evaluator           -- Evaluator Surface writes
--                                       EvaluationRun /
--                                       EvaluationVerdict on the
--                                       degraded short-circuit
--                                       paths (architecture Sec 3.7)
--                                       and the synchronous gate
--                                       path.
--   * clean_code_wal_reconciler      -- WAL Reconciler replays
--                                       missing Audit rows after
--                                       a crash (architecture
--                                       Sec 7.10); shares the
--                                       three-way Audit grant per
--                                       tech-spec Sec 7.2 line 1374.
--   * clean_code_refactor_planner    -- Sole writer of HotSpot,
--                                       RefactorPlan, RefactorTask
--                                       (architecture Sec 3.9).
--   * clean_code_management          -- Writes Repo and RepoEvent;
--                                       Commit columns are written
--                                       through the Repo Indexer
--                                       delegate per architecture
--                                       Sec 1.5.1 row 1.
--
-- Two-writer carve-out (tech-spec Sec 7.2 lines 1212-1248 / lines
-- 1348-1364): both `clean_code_metric_ingestor` AND
-- `clean_code_xrepo_aggregator` get `INSERT, SELECT` on
-- `metric_sample` and `metric_retraction`, and
-- `INSERT, SELECT, UPDATE` on `metric_sample_active`. The two
-- roles never collide on a quintuple because `metric_kind`
-- partitions the pointer table by writer:
--   * Ingestor: pack IN ('base', 'solid', 'ingested')
--   * Aggregator: pack='system' AND source='derived'
-- Application-layer per-`metric_kind` filtering enforces the
-- carve-out; the DB grant is intentionally per-table (not
-- per-row), and DB-level `REVOKE UPDATE, DELETE` on
-- `metric_sample` plus `REVOKE DELETE` on `metric_retraction` /
-- `metric_sample_active` keep G3 row immutability for both
-- roles.
--
-- Append-only enforcement (G3 row immutability, tech-spec
-- C2 / C25): REVOKE statements explicitly issued against
--   * metric_sample (UPDATE, DELETE) -- C2
--   * metric_retraction (DELETE)
--   * metric_sample_active (DELETE)
--   * repo_metric_snapshot, cross_repo_percentile,
--     portfolio_snapshot (UPDATE, DELETE)
--   * evaluation_run, evaluation_verdict, finding
--     (UPDATE, DELETE) -- C25
-- from PUBLIC and from the two writer roles. UPDATE/DELETE on
-- every other append-only table (policy_version, threshold,
-- override, rule, rule_pack, policy_activation, hot_spot,
-- refactor_plan, refactor_task, repo_event, metric_kind,
-- scope_binding) is never GRANTed in the first place -- the
-- default "permission denied" state preserves immutability
-- there without an explicit REVOKE.
--
-- Read-only cross-sub-store access (tech-spec C6): every role
-- gets SELECT on every CLEAN-CODE table. The C6 constraint is
-- about WRITE isolation; reads are unrestricted across
-- sub-store boundaries so the Evaluator can read
-- `metric_sample`, the Refactor Planner can read `finding`,
-- the Cross-Repo Aggregator can read `metric_sample` to derive
-- system-tier rows, etc. Per-sub-store writer isolation is
-- enforced by the explicit INSERT/UPDATE grants below.
--
-- Concurrency posture: race-safe CREATE ROLE
-- ------------------------------------------
-- Roles are CLUSTER-WIDE in PostgreSQL. Integration tests share
-- a single PostgreSQL instance across many per-test schemas, so
-- two test schemas' 0004.up calls can race: both observe "role
-- does not exist" via SELECT, both attempt CREATE ROLE, the
-- loser hits SQLSTATE 42710 (`duplicate_object`) and aborts the
-- migration transaction. The DO blocks below catch
-- `duplicate_object` instead -- the only race-safe shape
-- PostgreSQL offers for CREATE ROLE, because there is no
-- `CREATE ROLE IF NOT EXISTS` syntax. Any other error
-- (insufficient_privilege, syntax error, etc.) propagates and
-- aborts the migration.
--
-- Down posture: REVOKE, never DROP ROLE
-- -------------------------------------
-- `0004_roles.down.sql` REVOKEs every grant this file issued
-- but DOES NOT DROP the roles. Reasons (mirroring agent-memory's
-- 0016_roles_grants.sql posture):
--   1. The role is cluster-wide; a sibling tenant or a
--      concurrent test schema may still hold grants that
--      reference it. PostgreSQL rejects DROP ROLE with
--      SQLSTATE 2BP01 when any privilege depends on the role.
--   2. Leaving the role idle is harmless -- it has NOLOGIN and
--      no remaining ACL entries after REVOKE.
--   3. The round-trip integration test's pre-condition
--      `DROP SCHEMA IF EXISTS clean_code CASCADE` and the
--      0001_catalog_lifecycle.down.sql `DROP SCHEMA ...
--      CASCADE` remove the ACL bindings to this schema's
--      tables automatically; the role itself persisting across
--      test runs is by design.
--
-- Why NOLOGIN
-- -----------
-- Every role is created NOLOGIN. Production deployments enable
-- LOGIN by issuing a separate operator step so the credential
-- never lives in the version-controlled migration:
--
--   ALTER ROLE clean_code_metric_ingestor WITH LOGIN PASSWORD '<from-secrets>';
--
-- The same pattern applies to every other role; an integration
-- test that needs to connect AS one of these roles ALTERs it
-- to LOGIN with a random per-test password, opens the
-- connection, exercises the scenario, then reverts the role
-- to NOLOGIN.

BEGIN;

-- ---------------------------------------------------------------------------
-- Race-safe role creation (NOLOGIN; production grants LOGIN out-of-band)
-- ---------------------------------------------------------------------------

DO $$ BEGIN
    CREATE ROLE clean_code_repo_indexer NOLOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE ROLE clean_code_metric_ingestor NOLOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE ROLE clean_code_xrepo_aggregator NOLOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE ROLE clean_code_policy_steward NOLOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE ROLE clean_code_solid_batch NOLOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE ROLE clean_code_evaluator NOLOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE ROLE clean_code_wal_reconciler NOLOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE ROLE clean_code_refactor_planner NOLOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

DO $$ BEGIN
    CREATE ROLE clean_code_management NOLOGIN;
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- ---------------------------------------------------------------------------
-- Schema USAGE (every role needs USAGE to resolve table names)
-- ---------------------------------------------------------------------------

GRANT USAGE ON SCHEMA clean_code TO clean_code_repo_indexer;
GRANT USAGE ON SCHEMA clean_code TO clean_code_metric_ingestor;
GRANT USAGE ON SCHEMA clean_code TO clean_code_xrepo_aggregator;
GRANT USAGE ON SCHEMA clean_code TO clean_code_policy_steward;
GRANT USAGE ON SCHEMA clean_code TO clean_code_solid_batch;
GRANT USAGE ON SCHEMA clean_code TO clean_code_evaluator;
GRANT USAGE ON SCHEMA clean_code TO clean_code_wal_reconciler;
GRANT USAGE ON SCHEMA clean_code TO clean_code_refactor_planner;
GRANT USAGE ON SCHEMA clean_code TO clean_code_management;

-- ---------------------------------------------------------------------------
-- Cross-sub-store read access (tech-spec C6)
-- ---------------------------------------------------------------------------
-- Every role gets SELECT on every Catalog/Lifecycle, Measurement,
-- Policy, Audit, and Refactor table. Writer isolation (C5) is
-- enforced by the per-table INSERT/UPDATE grants below; reads
-- are unrestricted across sub-store boundaries.

GRANT SELECT ON
    clean_code.repo,
    clean_code.commit,
    clean_code.metric_kind,
    clean_code.repo_event,
    clean_code.scan_run,
    clean_code.scope_binding,
    clean_code.metric_sample,
    clean_code.metric_retraction,
    clean_code.metric_sample_active,
    clean_code.repo_metric_snapshot,
    clean_code.cross_repo_percentile,
    clean_code.portfolio_snapshot,
    clean_code.rule_pack,
    clean_code.rule,
    clean_code.policy_version,
    clean_code.policy_activation,
    clean_code.threshold,
    clean_code.override,
    clean_code.evaluation_run,
    clean_code.evaluation_verdict,
    clean_code.finding,
    clean_code.hot_spot,
    clean_code.refactor_plan,
    clean_code.refactor_task
TO  clean_code_repo_indexer,
    clean_code_metric_ingestor,
    clean_code_xrepo_aggregator,
    clean_code_policy_steward,
    clean_code_solid_batch,
    clean_code_evaluator,
    clean_code_wal_reconciler,
    clean_code_refactor_planner,
    clean_code_management;

-- ---------------------------------------------------------------------------
-- Catalog / Lifecycle sub-store (tech-spec Sec 7.2 line 691)
-- ---------------------------------------------------------------------------

-- clean_code_management: writes Repo (column-level INSERT plus
-- column-level UPDATE on the mutable Management-owned columns)
-- and RepoEvent (append-only).
--
-- Both INSERT and UPDATE are column-level so the Repo Indexer's
-- canonical writer ownership of `default_branch_head`
-- (architecture Sec 5.1.1 line 854 + Sec 1.5.1 row 1) is
-- enforced at the DB layer rather than relying on application
-- discipline. Management's column lists are:
--   * INSERT: (repo_id, display_name, mode, default_branch)
--     -- the four columns Management names on
--     `mgmt.register_repo` (Sec 6.3); `default_branch_head`
--     is intentionally EXCLUDED so Management cannot supply
--     it at row-creation time, and `created_at` is excluded
--     so Management cannot backdate.
--   * UPDATE: (display_name, mode, default_branch)
--     -- the three lifecycle-mutable columns Management
--     names on `mgmt.set_mode` / `mgmt.rename_repo` /
--     `mgmt.set_default_branch`; `default_branch_head` is
--     intentionally EXCLUDED so only the Repo Indexer can
--     mutate it.
--
-- The remaining repo columns NOT in either grant are:
--   * `default_branch_head` -- the Repo Indexer's
--                              `GRANT INSERT/UPDATE
--                              (default_branch_head)` below is
--                              the only grant for it. (INSERT
--                              capability is unnecessary because
--                              the column is nullable -- the
--                              Indexer derives the head SHA only
--                              after the first scan and UPDATEs
--                              the column at that point.)
--   * `created_at`          -- append-only registration
--                              timestamp; DEFAULT `now()`
--                              supplies the value at row
--                              creation. No writer ever names
--                              this column at the DB layer.
--
-- Listing both INSERT and UPDATE column-by-column at the GRANT
-- layer makes the Repo Indexer's column-isolation contract
-- enforceable by the database: Management attempting
-- `INSERT INTO clean_code.repo (..., default_branch_head) VALUES (...)`
-- or `UPDATE clean_code.repo SET default_branch_head=...` now
-- fails with SQLSTATE 42501 (permission denied) at the DB,
-- per the C5 row 1 ACL.
GRANT INSERT (repo_id, display_name, mode, default_branch) ON clean_code.repo       TO clean_code_management;
GRANT UPDATE (display_name, mode, default_branch)          ON clean_code.repo       TO clean_code_management;
GRANT INSERT                                               ON clean_code.repo_event TO clean_code_management;

-- clean_code_repo_indexer: Management's delegated writer per
-- architecture Sec 3.3 and tech-spec Sec 7.2 line 691.
--   * INSERT on `commit` is COLUMN-LEVEL on (repo_id, sha,
--     parent_sha, committed_at) only. The Indexer omits
--     `scan_status` from its INSERT column list per architecture
--     Sec 5.1.2 line 864 + Sec 1.5.1 row 1; the schema-level
--     DEFAULT `'pending'` on `clean_code.commit.scan_status`
--     supplies the initial value. Column-level INSERT enforces
--     this at the DB layer -- an attempt to
--     `INSERT INTO clean_code.commit (..., scan_status) VALUES ...`
--     fails with SQLSTATE 42501. The Metric Ingestor remains the
--     sole UPDATE writer of `scan_status` (`GRANT UPDATE
--     (scan_status)` below).
--   * INSERT on `repo_event` -- push/merge events emitted by the
--     Repo Indexer ride the same RepoEvent surface as management
--     events.
--   * UPDATE on `repo.default_branch_head` only -- maintained by
--     the Repo Indexer on each push/merge webhook per the
--     `clean_code.repo.default_branch_head` column COMMENT in
--     0001_catalog_lifecycle.up.sql. Column-level UPDATE prevents
--     the Indexer from mutating Management-owned columns
--     (display_name, mode, default_branch, created_at).
GRANT INSERT (repo_id, sha, parent_sha, committed_at) ON clean_code.commit      TO clean_code_repo_indexer;
GRANT INSERT                                          ON clean_code.repo_event  TO clean_code_repo_indexer;
GRANT UPDATE (default_branch_head)                    ON clean_code.repo        TO clean_code_repo_indexer;

-- clean_code_metric_ingestor: sole writer of `commit.scan_status`
-- transitions (`pending` -> `scanning` -> `scanned | failed`)
-- per architecture Sec 1.5.1 row 1; sole writer of `scan_run`.
-- Column-level UPDATE on `commit` confines the Ingestor to the
-- scan_status column -- it cannot mutate any other Commit column
-- (parent_sha, committed_at, repo_id, sha) at the DB layer.
GRANT UPDATE (scan_status) ON clean_code.commit   TO clean_code_metric_ingestor;
GRANT INSERT, UPDATE       ON clean_code.scan_run TO clean_code_metric_ingestor;

-- clean_code_policy_steward: writer of the MetricKind catalogue
-- rows per the COMMENT on `clean_code.metric_kind` in
-- 0001_catalog_lifecycle.up.sql ("Writer: the Policy Steward (or
-- catalogue-seeding migration) is the only writer; rows are
-- append-only after a metric definition stabilises.").
GRANT INSERT ON clean_code.metric_kind TO clean_code_policy_steward;

-- ---------------------------------------------------------------------------
-- Measurement sub-store -- foundation tier (tech-spec Sec 7.2 line 692)
-- ---------------------------------------------------------------------------

-- clean_code_metric_ingestor: foundation-tier MetricSample writer
-- + ScopeBinding writer + MetricRetraction writer + active
-- pointer writer for foundation `metric_kind` values
-- (`pack IN ('base', 'solid', 'ingested')`). The pointer-table
-- UPDATE supports the retract-then-reinsert atomic pointer swap
-- via `ON CONFLICT ... DO UPDATE` per tech-spec Sec 7.1.b
-- lines 1241-1264. SELECT is also re-granted explicitly here
-- (already covered by the blanket cross-sub-store SELECT above)
-- to match the verbatim tech-spec Sec 7.2 line 1350-1352 phrasing
-- so a `grep -F "GRANT INSERT, SELECT ON clean_code.metric_sample"`
-- of the file finds the canonical statement.
GRANT INSERT, SELECT         ON clean_code.scope_binding        TO clean_code_metric_ingestor;
GRANT INSERT, SELECT         ON clean_code.metric_sample        TO clean_code_metric_ingestor;
GRANT INSERT, SELECT         ON clean_code.metric_retraction    TO clean_code_metric_ingestor;
GRANT INSERT, SELECT, UPDATE ON clean_code.metric_sample_active TO clean_code_metric_ingestor;

-- ---------------------------------------------------------------------------
-- Measurement sub-store -- system-tier carve-out
-- (tech-spec Sec 7.2 line 693 / lines 1212-1248 / lines 1353-1360)
-- ---------------------------------------------------------------------------

-- clean_code_xrepo_aggregator: system-tier MetricSample writer
-- (`pack='system' AND source='derived'`) + active pointer writer
-- for system `metric_kind` values + the three aggregator-owned
-- snapshot tables. The two-writer carve-out on metric_sample /
-- metric_retraction / metric_sample_active is grant-equal to the
-- Metric Ingestor at the DB layer; per-row writer isolation is
-- enforced by the application-layer per-`metric_kind` filter
-- (Aggregator only ever writes `pack='system'` rows). SELECT is
-- re-granted explicitly per the verbatim tech-spec Sec 7.2 lines
-- 1355-1360 phrasing.
GRANT INSERT, SELECT         ON clean_code.metric_sample          TO clean_code_xrepo_aggregator;
GRANT INSERT, SELECT         ON clean_code.metric_retraction      TO clean_code_xrepo_aggregator;
GRANT INSERT, SELECT, UPDATE ON clean_code.metric_sample_active   TO clean_code_xrepo_aggregator;
GRANT INSERT, SELECT         ON clean_code.repo_metric_snapshot   TO clean_code_xrepo_aggregator;
GRANT INSERT, SELECT         ON clean_code.cross_repo_percentile  TO clean_code_xrepo_aggregator;
GRANT INSERT, SELECT         ON clean_code.portfolio_snapshot     TO clean_code_xrepo_aggregator;

-- ---------------------------------------------------------------------------
-- Measurement sub-store -- G3 / C2 row immutability (tech-spec
-- Sec 7.2 lines 1362-1367)
-- ---------------------------------------------------------------------------
-- MetricSample remains strictly immutable for BOTH writer roles
-- (and for PUBLIC). The REVOKE statements below physically
-- prevent the writers from issuing UPDATE/DELETE even if the
-- application layer regresses on its append-only contract. The
-- side-relation MetricRetraction (tombstones) and
-- metric_sample_active (pointer rows) accept UPDATE on the
-- active pointer (for `ON CONFLICT ... DO UPDATE` pointer swaps)
-- but never DELETE -- retention is partition-drop only per
-- tech-spec Sec 8.1.4.

REVOKE UPDATE, DELETE ON clean_code.metric_sample          FROM PUBLIC, clean_code_metric_ingestor, clean_code_xrepo_aggregator;
REVOKE DELETE         ON clean_code.metric_retraction      FROM PUBLIC, clean_code_metric_ingestor, clean_code_xrepo_aggregator;
REVOKE DELETE         ON clean_code.metric_sample_active   FROM PUBLIC, clean_code_metric_ingestor, clean_code_xrepo_aggregator;
REVOKE UPDATE, DELETE ON clean_code.repo_metric_snapshot   FROM PUBLIC, clean_code_xrepo_aggregator;
REVOKE UPDATE, DELETE ON clean_code.cross_repo_percentile  FROM PUBLIC, clean_code_xrepo_aggregator;
REVOKE UPDATE, DELETE ON clean_code.portfolio_snapshot     FROM PUBLIC, clean_code_xrepo_aggregator;

-- ---------------------------------------------------------------------------
-- Policy / rules sub-store (tech-spec Sec 7.2 line 694)
-- ---------------------------------------------------------------------------

-- clean_code_policy_steward: sole writer of every Policy table
-- per the COMMENTs on rule_pack / rule / policy_version /
-- policy_activation / threshold / override in
-- 0003_policy_audit_refactor.up.sql ("Writer: the Policy Steward
-- (architecture Sec 3.11) is the only writer."). Rows are
-- append-only -- no UPDATE / DELETE grant is issued.

GRANT INSERT ON clean_code.rule              TO clean_code_policy_steward;
GRANT INSERT ON clean_code.rule_pack         TO clean_code_policy_steward;
GRANT INSERT ON clean_code.policy_version    TO clean_code_policy_steward;
GRANT INSERT ON clean_code.policy_activation TO clean_code_policy_steward;
GRANT INSERT ON clean_code.threshold         TO clean_code_policy_steward;
GRANT INSERT ON clean_code.override          TO clean_code_policy_steward;

-- ---------------------------------------------------------------------------
-- Audit / verdict sub-store (tech-spec Sec 7.2 lines 1372-1377)
-- ---------------------------------------------------------------------------

-- Three writers share one append-only write path -- the
-- Evaluator Surface (`caller='eval_gate'`), the SOLID Rule
-- Engine batch worker (`caller='batch_refresh'`, architecture
-- Sec 3.6), and the Audit WAL Reconciler (replay-only,
-- architecture Sec 7.10). The Reconciler MUST INSERT replayed
-- rows that are missing from the Audit tables, so it carries
-- the same three-way append-only grant as the other two
-- callers; there is NO separate SELECT-only grant.
--
-- `EvaluationRun.caller` tags each row's origin so post-hoc
-- queries can attribute writes to the correct caller without
-- relying on connection-time role attribution.

GRANT INSERT, SELECT ON clean_code.evaluation_run     TO clean_code_evaluator;
GRANT INSERT, SELECT ON clean_code.evaluation_run     TO clean_code_solid_batch;
GRANT INSERT, SELECT ON clean_code.evaluation_run     TO clean_code_wal_reconciler;

GRANT INSERT, SELECT ON clean_code.evaluation_verdict TO clean_code_evaluator;
GRANT INSERT, SELECT ON clean_code.evaluation_verdict TO clean_code_solid_batch;
GRANT INSERT, SELECT ON clean_code.evaluation_verdict TO clean_code_wal_reconciler;

GRANT INSERT, SELECT ON clean_code.finding            TO clean_code_evaluator;
GRANT INSERT, SELECT ON clean_code.finding            TO clean_code_solid_batch;
GRANT INSERT, SELECT ON clean_code.finding            TO clean_code_wal_reconciler;

-- C25 / G3: the three Audit tables are append-only across the
-- board. UPDATE and DELETE are revoked from every writer role
-- plus PUBLIC. Retention is partition-drop only per tech-spec
-- Sec 8.1.4 + Sec 7.10.

REVOKE UPDATE, DELETE ON clean_code.evaluation_run     FROM PUBLIC, clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
REVOKE UPDATE, DELETE ON clean_code.evaluation_verdict FROM PUBLIC, clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;
REVOKE UPDATE, DELETE ON clean_code.finding            FROM PUBLIC, clean_code_evaluator, clean_code_solid_batch, clean_code_wal_reconciler;

-- ---------------------------------------------------------------------------
-- Refactor sub-store (tech-spec Sec 7.2 line 696)
-- ---------------------------------------------------------------------------

-- clean_code_refactor_planner: sole writer of the Refactor
-- sub-store per the COMMENTs on hot_spot / refactor_plan /
-- refactor_task in 0003_policy_audit_refactor.up.sql ("Writer:
-- the Refactor Planner (architecture Sec 3.9)"). The Planner is
-- read-only against Measurement + Policy + Audit + Catalog per
-- C7 / C23 -- enforced by the absence of INSERT/UPDATE grants on
-- those sub-stores' tables (the planner role appears in the
-- blanket SELECT grant above and nowhere else).

GRANT INSERT ON clean_code.hot_spot       TO clean_code_refactor_planner;
GRANT INSERT ON clean_code.refactor_plan  TO clean_code_refactor_planner;
GRANT INSERT ON clean_code.refactor_task  TO clean_code_refactor_planner;

COMMIT;
