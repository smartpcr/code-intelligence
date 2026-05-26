-- 0009_scan_run_payload_hash_unique.down.sql
--
-- Reverse of 0009_scan_run_payload_hash_unique.up.sql. Drops
-- the partial unique index on (verb, payload_hash) and the
-- supporting CHECK constraint, then drops the `verb` column.
-- The webhook MUST be taken off-line BEFORE this rollback or
-- its idempotency layer degrades to the in-memory store;
-- running concurrent webhook traffic against the rollback
-- can produce duplicate scan_run rows for the same
-- (verb, payload_hash).
--
-- Live-rollback safety:
--   - DROP INDEX CONCURRENTLY acquires SHARE UPDATE EXCLUSIVE
--     rather than ACCESS EXCLUSIVE on clean_code.scan_run.
--   - ALTER TABLE DROP CONSTRAINT and DROP COLUMN take a
--     brief ACCESS EXCLUSIVE; pause writers if needed.
--   - CONCURRENTLY cannot run inside a transaction so this
--     rollback file omits BEGIN/COMMIT -- apply via
--     `psql -f` (autocommit).
--
-- After this rollback, dev DBs that still have a stale
-- iter-2 `scan_run_payload_hash_kind_uniq` index are NOT
-- restored. Reapply the prior iter-2 migration manually if
-- that legacy index is required for any downstream tooling.

DROP INDEX CONCURRENTLY IF EXISTS clean_code.scan_run_payload_hash_verb_uniq;

ALTER TABLE clean_code.scan_run
    DROP CONSTRAINT IF EXISTS scan_run_verb_payload_hash_consistent;

ALTER TABLE clean_code.scan_run
    DROP COLUMN IF EXISTS verb;
