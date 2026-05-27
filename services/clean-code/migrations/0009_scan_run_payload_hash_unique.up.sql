-- 0009_scan_run_payload_hash_unique.up.sql
--
-- Stage 4.1 (External metric ingest webhook) -- evaluator
-- iter-3 feedback #2: the durable idempotency anchor MUST
-- be keyed on (verb, payload_hash), NOT on (kind, payload_hash).
-- Verbs map MANY-to-one to scan_run.kind:
--
--   - coverage     -> kind=external_single
--   - test_balance -> kind=external_single
--   - churn        -> kind=external_per_row
--   - defects      -> kind=external_per_row
--
-- A (kind, payload_hash) unique key would collapse
-- coverage+test_balance and churn+defects -- two distinct
-- verbs uploading the exact same canonical body would
-- silently replay each other's scan_run_id. The brief
-- (Stage 4.1 implementation-plan / tech-spec Sec 7):
--
--   "Add idempotency layer: compute payload_hash = sha256(
--    canonicalised body); if a scan_run(payload_hash=...)
--    already exists for THIS VERB, return the stored
--    scan_run_id without re-executing."
--
-- This migration introduces a `verb` column on `scan_run`
-- alongside the `kind` enum and switches the partial unique
-- index to `(verb, payload_hash)`. Foundation-tier rows
-- continue to leave both `verb` and `payload_hash` NULL; a
-- new CHECK constraint pins the
-- (verb IS NULL <-> payload_hash IS NULL) invariant.
--
-- # Operator notes
--
-- - The migration is composed of several statements (ALTER
--   TABLE, ADD CONSTRAINT, CREATE INDEX CONCURRENTLY).
--   `CREATE/DROP INDEX CONCURRENTLY` CANNOT run inside a
--   transaction, so this file deliberately omits any
--   BEGIN/COMMIT envelope -- operators MUST apply it with
--   `psql -f` (autocommit). Each statement is its own
--   implicit transaction.
-- - The migration is idempotent: `ADD COLUMN IF NOT EXISTS`,
--   `DROP INDEX/CONSTRAINT IF EXISTS`, and `CREATE INDEX
--   CONCURRENTLY IF NOT EXISTS` allow a partial first-run
--   to be replayed safely.
-- - If a prior iter-2 attempt installed the old
--   `scan_run_payload_hash_kind_uniq` index, this file
--   drops it BEFORE the new index is built so the new
--   constraint becomes the sole anchor.
-- - The defensive UPDATE below backfills any external
--   scan_run rows that landed in a dev DB while the old
--   (kind, payload_hash) shape was deployed. The Stage 4.1
--   webhook was NOT mounted in production at the time the
--   prior shape existed, so this UPDATE is a no-op on
--   production but keeps the new CHECK from failing on dev.

DROP INDEX CONCURRENTLY IF EXISTS clean_code.scan_run_payload_hash_kind_uniq;

ALTER TABLE clean_code.scan_run
    ADD COLUMN IF NOT EXISTS verb text;

COMMENT ON COLUMN clean_code.scan_run.verb IS
    'External-ingest verb that opened this scan_run row '
    '(architecture Sec 5.7 / Stage 4.1 implementation-plan / '
    'evaluator iter-3 feedback #2). One of '
    '''coverage'' | ''test_balance'' | ''churn'' | ''defects'' '
    'for external rows; NULL for foundation-tier rows. The '
    'scan_run_verb_payload_hash_consistent CHECK enforces the '
    '(verb IS NULL <-> payload_hash IS NULL) invariant; the '
    'scan_run_payload_hash_verb_uniq partial unique index '
    'enforces idempotency per-verb.';

-- Defensive backfill for any dev-DB rows that landed under
-- the prior (kind, payload_hash) shape. Production has none.
UPDATE clean_code.scan_run
   SET verb = '__legacy_' || kind::text
 WHERE payload_hash IS NOT NULL
   AND verb IS NULL;

ALTER TABLE clean_code.scan_run
    DROP CONSTRAINT IF EXISTS scan_run_verb_payload_hash_consistent;

ALTER TABLE clean_code.scan_run
    ADD CONSTRAINT scan_run_verb_payload_hash_consistent
        CHECK (
            (verb IS NULL AND payload_hash IS NULL)
            OR
            (verb IS NOT NULL AND payload_hash IS NOT NULL)
        );

CREATE UNIQUE INDEX CONCURRENTLY IF NOT EXISTS scan_run_payload_hash_verb_uniq
    ON clean_code.scan_run (verb, payload_hash)
    WHERE payload_hash IS NOT NULL;

COMMENT ON INDEX clean_code.scan_run_payload_hash_verb_uniq IS
    'Stage 4.1 external ingest webhook idempotency anchor '
    '(tech-spec Sec 7 / Stage 4.1 implementation-plan / '
    'evaluator iter-3 feedback #2). Partial unique on '
    '(verb, payload_hash) WHERE payload_hash IS NOT NULL so '
    'foundation-tier scan_run rows (verb IS NULL) do NOT '
    'collide on the unique constraint, and so the two '
    'external-ingest verbs that share kind=external_per_row '
    '(churn + defects) and the two that share '
    'kind=external_single (coverage + test_balance) get '
    'INDEPENDENT idempotency tracks. The webhook''s '
    'PGScanRunRepository.OpenExternal emits '
    'INSERT ... ON CONFLICT (verb, payload_hash) DO NOTHING '
    'RETURNING scan_run_id to atomically claim or surface '
    'a prior scan_run for the same per-verb payload.';
