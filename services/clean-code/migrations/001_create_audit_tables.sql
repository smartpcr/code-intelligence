-- 001_create_audit_tables.sql
-- Creates the three Audit tables: evaluation_run, evaluation_verdict, finding.
-- Also creates the non-audit metric_sample table used in permission tests.

BEGIN;

CREATE TABLE IF NOT EXISTS evaluation_run (
    id          TEXT PRIMARY KEY,
    caller      TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'pending',
    detail      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS evaluation_verdict (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL,
    verdict     TEXT NOT NULL,
    detail      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS finding (
    id          TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL,
    rule_id     TEXT NOT NULL,
    severity    TEXT NOT NULL DEFAULT 'info',
    message     TEXT,
    detail      TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Non-audit table used to verify the reconciler role cannot write outside
-- the Audit tables.
CREATE TABLE IF NOT EXISTS metric_sample (
    id          TEXT PRIMARY KEY,
    metric_name TEXT,
    value       DOUBLE PRECISION,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMIT;