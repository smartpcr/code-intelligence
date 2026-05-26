-- services/clean-code/internal/db/schema/clean_code/scan_run.sql
--
-- Canonical reference for the `clean_code.scan_run` table.
-- The AUTHORITATIVE SQL lives in two migrations:
--   1. `services/clean-code/migrations/0001_catalog_lifecycle.up.sql`
--      lines 337-419 -- the initial table, kinds, sha-binding
--      CHECK, and the started_at index.
--   2. `services/clean-code/migrations/0009_scan_run_payload_hash_unique.up.sql`
--      -- adds the `verb` column and the partial unique index
--      on (verb, payload_hash) that anchors webhook idempotency.
--
-- This file is a docs-friendly mirror of the post-migration-0009
-- shape, kept so a reader who lands on the schema folder can
-- find the canonical column set without re-reading two
-- migrations. Stage 4.4 references this file in its
-- implementation-plan target list because the `ingest.churn`
-- verb opens a scan_run row with the specific shape pinned
-- here (kind='external_per_row', sha_binding='per_row',
-- to_sha IS NULL, verb='churn', payload_hash=sha256(body))
-- before staging events into `churn_event`.
--
-- Source of truth pins:
--   - Architecture Sec 5.7 lines 1265-1280       -- canonical columns
--   - Architecture Sec 4.4 lines 778-790         -- per-row-SHA contract
--   - Tech-spec Sec 7 / Stage 4.1 implementation-plan
--                                                -- payload_hash idempotency
--   - E2E scenarios.md lines 658-664             -- churn verb's scan_run shape

-- Enums (created in migration 0001).
--
-- CREATE TYPE clean_code.scan_run_kind         AS ENUM ('full', 'delta', 'external_single', 'external_per_row', 'retract');
-- CREATE TYPE clean_code.scan_run_sha_binding  AS ENUM ('single', 'per_row');
-- CREATE TYPE clean_code.scan_run_status       AS ENUM ('running', 'succeeded', 'failed');

CREATE TABLE clean_code.scan_run (
    scan_run_id   uuid                              PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id       uuid                              NOT NULL REFERENCES clean_code.repo (repo_id) ON DELETE RESTRICT,
    -- One of full | delta | external_single | external_per_row | retract (Sec 5.7 line 1273).
    kind          clean_code.scan_run_kind          NOT NULL,
    -- single = one SHA per run; per_row = each emitted sample carries its own SHA.
    sha_binding   clean_code.scan_run_sha_binding   NOT NULL DEFAULT 'single',
    from_sha      text,
    -- NULL when sha_binding='per_row' (e.g. churn); non-null when 'single'.
    to_sha        text,
    -- sha256 of the webhook body bytes; the (verb, payload_hash)
    -- partial unique index from migration 0009 is the
    -- idempotency anchor for external_* kinds.
    payload_hash  bytea,
    -- External-ingest verb that opened this scan_run row.
    -- One of 'coverage' | 'test_balance' | 'churn' | 'defects'
    -- for external rows; NULL for foundation-tier rows.
    -- Added in migration 0009.
    verb          text,
    started_at    timestamptz                       NOT NULL DEFAULT now(),
    ended_at      timestamptz,
    status        clean_code.scan_run_status        NOT NULL DEFAULT 'running',

    -- sha_binding <-> to_sha invariant (migration 0001 line 385-389).
    CONSTRAINT scan_run_sha_binding_consistent CHECK (
        (sha_binding = 'single'  AND to_sha IS NOT NULL)
        OR
        (sha_binding = 'per_row' AND to_sha IS NULL)
    ),
    -- verb <-> payload_hash null-correlation (migration 0009 lines 78-87).
    CONSTRAINT scan_run_verb_payload_hash_consistent CHECK (
        (verb IS NULL     AND payload_hash IS NULL)
        OR
        (verb IS NOT NULL AND payload_hash IS NOT NULL)
    )
);

-- Per-repo hot-read index (migration 0001 line 418-419).
CREATE INDEX scan_run_repo_started_idx
    ON clean_code.scan_run (repo_id, started_at DESC);

-- Per-verb idempotency anchor (migration 0009 line 89-91).
-- Two concurrent webhook deliveries with the SAME
-- (verb, payload_hash) observe exactly one scan_run row.
CREATE UNIQUE INDEX scan_run_payload_hash_verb_uniq
    ON clean_code.scan_run (verb, payload_hash)
    WHERE payload_hash IS NOT NULL;

-- Writer role (granted in migration 0004):
--   GRANT INSERT, UPDATE ON clean_code.scan_run TO clean_code_metric_ingestor;
--
-- The `ingest.churn` verb opens its scan_run row with the
-- canonical shape:
--   INSERT INTO clean_code.scan_run
--     (repo_id, kind, sha_binding, to_sha, verb, payload_hash, status)
--   VALUES
--     ($1,        'external_per_row', 'per_row', NULL, 'churn', $2, 'running')
--   ON CONFLICT (verb, payload_hash) WHERE payload_hash IS NOT NULL DO NOTHING
--   RETURNING scan_run_id;
--
-- IMPORTANT: the `ON CONFLICT (verb, payload_hash)` target
-- MUST include the `WHERE payload_hash IS NOT NULL`
-- predicate to match the actual partial unique index created
-- by migration 0009 (`scan_run_payload_hash_verb_uniq`).
-- Omitting the predicate would error with "no unique or
-- exclusion constraint matching the ON CONFLICT
-- specification" (PG-42P10) because PG requires the target
-- predicate to exactly match the partial index predicate.
--
-- A conflict (i.e. a prior call already opened this scan_run
-- for the same body) surfaces an empty RETURNING set; the
-- Router then SELECTs the prior scan_run_id and short-circuits
-- the verb handler with a replay envelope per Stage 4.1.
