-- 0004_roles.down.sql
--
-- Reverse of 0004_roles.up.sql. Symmetrically REVOKEs every
-- grant the up half issued, but does NOT DROP the nine writer
-- roles created by the up half.
--
-- Why REVOKE-only (no DROP ROLE)
-- ------------------------------
-- Roles are CLUSTER-WIDE in PostgreSQL. A sibling tenant or a
-- concurrent test schema may still hold grants that reference
-- one of the nine `clean_code_*` roles; PostgreSQL rejects
-- DROP ROLE with SQLSTATE 2BP01 (`dependent_privilege_descriptors_still_exist`)
-- in that case. Leaving the role idle after REVOKE is harmless:
-- it has NOLOGIN and no remaining ACL entries against this
-- schema after the down completes. The matching
-- `0001_catalog_lifecycle.down.sql` runs `DROP SCHEMA
-- clean_code CASCADE`, which removes the schema and any
-- residual ACL bindings to its tables automatically; the
-- roles themselves persisting across test runs is by design
-- (mirrors agent-memory's 0016_roles_grants.sql posture).
--
-- DROP order (reverse of the up half)
-- -----------------------------------
--   1. Refactor sub-store     (refactor_planner)
--   2. Audit sub-store        (evaluator, solid_batch, wal_reconciler)
--   3. Policy sub-store       (policy_steward)
--   4. Measurement sub-store  (metric_ingestor, xrepo_aggregator)
--   5. Catalog / Lifecycle    (management, repo_indexer,
--                              metric_ingestor scan_status,
--                              policy_steward metric_kind)
--   6. Cross-sub-store SELECT (all nine roles)
--   7. Schema USAGE           (all nine roles)
--
-- Every REVOKE is followed by its symmetric companion in the
-- up half; a Down/Up round-trip leaves the schema-level ACL
-- state byte-identical to a fresh Up apply.
--
-- Note: the `REVOKE UPDATE, DELETE ON ... FROM PUBLIC, <roles>`
-- statements in the up half are themselves restrictive (they
-- subtract privilege rather than adding it). There is no
-- symmetric Down-side GRANT for them -- granting UPDATE/DELETE
-- back to PUBLIC or to the writer roles would re-open the
-- append-only carve-out the up half intentionally locks shut.
-- Re-running 0004.up after this down is fine: the GRANTs the
-- up half issues are idempotent (re-granting an already-held
-- privilege is a no-op in PostgreSQL), and the REVOKEs the up
-- half issues against UPDATE/DELETE are also idempotent (the
-- privilege was never granted).

BEGIN;

-- ---------------------------------------------------------------------------
-- Refactor sub-store
-- ---------------------------------------------------------------------------

REVOKE INSERT ON clean_code.hot_spot       FROM clean_code_refactor_planner;
REVOKE INSERT ON clean_code.refactor_plan  FROM clean_code_refactor_planner;
REVOKE INSERT ON clean_code.refactor_task  FROM clean_code_refactor_planner;

-- ---------------------------------------------------------------------------
-- Audit / verdict sub-store
-- ---------------------------------------------------------------------------

REVOKE INSERT ON clean_code.evaluation_run     FROM clean_code_evaluator;
REVOKE INSERT ON clean_code.evaluation_run     FROM clean_code_solid_batch;
REVOKE INSERT ON clean_code.evaluation_run     FROM clean_code_wal_reconciler;

REVOKE INSERT ON clean_code.evaluation_verdict FROM clean_code_evaluator;
REVOKE INSERT ON clean_code.evaluation_verdict FROM clean_code_solid_batch;
REVOKE INSERT ON clean_code.evaluation_verdict FROM clean_code_wal_reconciler;

REVOKE INSERT ON clean_code.finding            FROM clean_code_evaluator;
REVOKE INSERT ON clean_code.finding            FROM clean_code_solid_batch;
REVOKE INSERT ON clean_code.finding            FROM clean_code_wal_reconciler;

-- ---------------------------------------------------------------------------
-- Policy / rules sub-store
-- ---------------------------------------------------------------------------

REVOKE INSERT ON clean_code.rule              FROM clean_code_policy_steward;
REVOKE INSERT ON clean_code.rule_pack         FROM clean_code_policy_steward;
REVOKE INSERT ON clean_code.policy_version    FROM clean_code_policy_steward;
REVOKE INSERT ON clean_code.policy_activation FROM clean_code_policy_steward;
REVOKE INSERT ON clean_code.threshold         FROM clean_code_policy_steward;
REVOKE INSERT ON clean_code.override          FROM clean_code_policy_steward;

-- ---------------------------------------------------------------------------
-- Measurement sub-store -- system-tier carve-out
-- ---------------------------------------------------------------------------

REVOKE INSERT         ON clean_code.metric_sample          FROM clean_code_xrepo_aggregator;
REVOKE INSERT         ON clean_code.metric_retraction      FROM clean_code_xrepo_aggregator;
REVOKE INSERT, UPDATE ON clean_code.metric_sample_active   FROM clean_code_xrepo_aggregator;
REVOKE INSERT         ON clean_code.repo_metric_snapshot   FROM clean_code_xrepo_aggregator;
REVOKE INSERT         ON clean_code.cross_repo_percentile  FROM clean_code_xrepo_aggregator;
REVOKE INSERT         ON clean_code.portfolio_snapshot     FROM clean_code_xrepo_aggregator;

-- ---------------------------------------------------------------------------
-- Measurement sub-store -- foundation tier
-- ---------------------------------------------------------------------------

REVOKE INSERT         ON clean_code.scope_binding        FROM clean_code_metric_ingestor;
REVOKE INSERT         ON clean_code.metric_sample        FROM clean_code_metric_ingestor;
REVOKE INSERT         ON clean_code.metric_retraction    FROM clean_code_metric_ingestor;
REVOKE INSERT, UPDATE ON clean_code.metric_sample_active FROM clean_code_metric_ingestor;

-- ---------------------------------------------------------------------------
-- Catalog / Lifecycle sub-store
-- ---------------------------------------------------------------------------

REVOKE INSERT ON clean_code.metric_kind FROM clean_code_policy_steward;

REVOKE UPDATE (scan_status) ON clean_code.commit   FROM clean_code_metric_ingestor;
REVOKE INSERT, UPDATE       ON clean_code.scan_run FROM clean_code_metric_ingestor;

REVOKE INSERT (repo_id, sha, parent_sha, committed_at) ON clean_code.commit      FROM clean_code_repo_indexer;
REVOKE INSERT                                          ON clean_code.repo_event  FROM clean_code_repo_indexer;
REVOKE UPDATE (default_branch_head)                    ON clean_code.repo        FROM clean_code_repo_indexer;

REVOKE INSERT (repo_id, display_name, mode, default_branch) ON clean_code.repo       FROM clean_code_management;
REVOKE UPDATE (display_name, mode, default_branch)          ON clean_code.repo       FROM clean_code_management;
REVOKE INSERT                                               ON clean_code.repo_event FROM clean_code_management;

-- ---------------------------------------------------------------------------
-- Cross-sub-store SELECT
-- ---------------------------------------------------------------------------

REVOKE SELECT ON
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
FROM clean_code_repo_indexer,
    clean_code_metric_ingestor,
    clean_code_xrepo_aggregator,
    clean_code_policy_steward,
    clean_code_solid_batch,
    clean_code_evaluator,
    clean_code_wal_reconciler,
    clean_code_refactor_planner,
    clean_code_management;

-- ---------------------------------------------------------------------------
-- Schema USAGE
-- ---------------------------------------------------------------------------

REVOKE USAGE ON SCHEMA clean_code FROM clean_code_repo_indexer;
REVOKE USAGE ON SCHEMA clean_code FROM clean_code_metric_ingestor;
REVOKE USAGE ON SCHEMA clean_code FROM clean_code_xrepo_aggregator;
REVOKE USAGE ON SCHEMA clean_code FROM clean_code_policy_steward;
REVOKE USAGE ON SCHEMA clean_code FROM clean_code_solid_batch;
REVOKE USAGE ON SCHEMA clean_code FROM clean_code_evaluator;
REVOKE USAGE ON SCHEMA clean_code FROM clean_code_wal_reconciler;
REVOKE USAGE ON SCHEMA clean_code FROM clean_code_refactor_planner;
REVOKE USAGE ON SCHEMA clean_code FROM clean_code_management;

COMMIT;
