-- 0010_churn_event.down.sql
--
-- Reverse of 0010_churn_event.up.sql. Drops the
-- `clean_code.churn_event` staging table and its indexes.
-- The webhook MUST be taken off-line before this rollback or
-- the `ingest.churn` verb will start failing with an SQLSTATE
-- 42P01 (undefined_table).
--
-- The table is owned by Stage 4.4 only; no downstream
-- writer depends on it. The materialiser
-- (`internal/metrics/materialisers/modification_count.go`)
-- gracefully reports zero rows when the table is missing
-- (it surfaces the underlying SQL error wrapped in
-- ErrChurnEventRead), so a rolled-back staging table does
-- not corrupt the metric_sample state -- it simply stalls
-- the materialiser until the table is restored.

BEGIN;

DROP INDEX IF EXISTS clean_code.churn_event_repo_file_idx;
DROP INDEX IF EXISTS clean_code.churn_event_scan_run_idx;
DROP INDEX IF EXISTS clean_code.churn_event_repo_modified_idx;

DROP TABLE IF EXISTS clean_code.churn_event;

COMMIT;
