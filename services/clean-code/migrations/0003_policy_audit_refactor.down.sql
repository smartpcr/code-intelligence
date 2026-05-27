-- 0003_policy_audit_refactor.down.sql
--
-- Reverse of 0003_policy_audit_refactor.up.sql. Drops the
-- twelve Policy / Audit / Refactor tables this migration
-- created plus the six ENUM types it defines, in reverse
-- dependency order so FK constraints unwind cleanly. The
-- per-object DROP shape (rather than a `DROP SCHEMA ...
-- CASCADE` shortcut) keeps each migration's down half
-- symmetric to its up half -- a Stage 1.4 rollback that
-- needs to leave Stage 1.2 tables intact CAN do so because
-- this script only touches Stage 1.4 objects.
--
-- DROP order (reverse of CREATE):
--   1. Refactor tables    (refactor_task -> refactor_plan -> hot_spot)
--   2. Audit tables       (finding -> evaluation_verdict -> evaluation_run)
--   3. Policy tables      (override -> threshold -> policy_activation
--                          -> policy_version -> rule -> rule_pack)
--   4. ENUM types         (threshold_op, refactor_task_kind,
--                          evaluation_verdict_value,
--                          evaluation_run_caller, finding_delta,
--                          rule_severity)
--
-- `IF EXISTS` on every statement so a partially-applied up
-- migration (e.g. one that failed mid-pass) can still be
-- rolled back without manual cleanup. `CASCADE` is
-- intentionally NOT used here -- the explicit drop order
-- means we want PostgreSQL to fail loudly if some unexpected
-- dependent object exists (e.g. a view created out-of-band
-- against `finding`), so an operator can investigate before
-- silently losing data.

BEGIN;

-- Refactor sub-store -----------------------------------------------
DROP TABLE IF EXISTS clean_code.refactor_task;
DROP TABLE IF EXISTS clean_code.refactor_plan;
DROP TABLE IF EXISTS clean_code.hot_spot;

-- Audit sub-store --------------------------------------------------
DROP TABLE IF EXISTS clean_code.finding;
DROP TABLE IF EXISTS clean_code.evaluation_verdict;
DROP TABLE IF EXISTS clean_code.evaluation_run;

-- Policy sub-store -------------------------------------------------
DROP TABLE IF EXISTS clean_code.override;
DROP TABLE IF EXISTS clean_code.threshold;
DROP TABLE IF EXISTS clean_code.policy_activation;
DROP TABLE IF EXISTS clean_code.policy_version;
DROP TABLE IF EXISTS clean_code.rule;
DROP TABLE IF EXISTS clean_code.rule_pack;

-- ENUM types -------------------------------------------------------
DROP TYPE IF EXISTS clean_code.threshold_op;
DROP TYPE IF EXISTS clean_code.refactor_task_kind;
DROP TYPE IF EXISTS clean_code.evaluation_verdict_value;
DROP TYPE IF EXISTS clean_code.evaluation_run_caller;
DROP TYPE IF EXISTS clean_code.finding_delta;
DROP TYPE IF EXISTS clean_code.rule_severity;

COMMIT;
