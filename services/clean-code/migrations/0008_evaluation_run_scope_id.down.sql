-- 0008_evaluation_run_scope_id.down.sql
--
-- Reverse of 0008_evaluation_run_scope_id.up.sql. Drops the
-- compound dedup index before the column so the index does not
-- become orphaned in the middle of the rollback.
--
-- Live-rollback safety (iter-8 evaluator feedback #3): the
-- index is dropped CONCURRENTLY so DROP INDEX acquires
-- SHARE UPDATE EXCLUSIVE rather than ACCESS EXCLUSIVE on
-- `clean_code.evaluation_run`; the column drop that follows
-- is unavoidably ACCESS EXCLUSIVE but is a fast catalogue
-- update. CONCURRENTLY cannot run inside a transaction, so
-- this rollback file deliberately omits any BEGIN/COMMIT
-- envelope -- apply it via `psql -f` (autocommit) like the
-- up migration.

DROP INDEX CONCURRENTLY IF EXISTS clean_code.evaluation_run_dedup_idx;

ALTER TABLE clean_code.evaluation_run
    DROP COLUMN IF EXISTS scope_id;
