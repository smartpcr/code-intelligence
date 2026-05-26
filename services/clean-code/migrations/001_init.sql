-- 001_init.sql — Bootstrap schema for the clean-code service.
-- Idempotent: safe to run multiple times.

CREATE SCHEMA IF NOT EXISTS clean_code;

-- Enum: scan_status lifecycle for a commit.
DO $$ BEGIN
    CREATE TYPE clean_code.scan_status AS ENUM ('pending', 'scanning', 'scanned', 'failed');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Enum: scan_run status.
DO $$ BEGIN
    CREATE TYPE clean_code.scan_run_status AS ENUM ('running', 'succeeded', 'failed');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Enum: scan_run kind (allowed values).
DO $$ BEGIN
    CREATE TYPE clean_code.scan_run_kind AS ENUM ('ast_metrics', 'lint', 'complexity', 'dependency');
EXCEPTION WHEN duplicate_object THEN NULL;
END $$;

-- Repo table.
CREATE TABLE IF NOT EXISTS clean_code.repo (
    repo_id      UUID PRIMARY KEY,
    display_name TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT 'main',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Commit table with scan_status state machine.
CREATE TABLE IF NOT EXISTS clean_code.commit (
    sha          TEXT PRIMARY KEY,
    repo_id      UUID NOT NULL REFERENCES clean_code.repo(repo_id),
    scan_status  clean_code.scan_status NOT NULL DEFAULT 'pending',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ScanRun table: one row per scan attempt against a commit.
CREATE TABLE IF NOT EXISTS clean_code.scan_run (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    commit_sha   TEXT NOT NULL REFERENCES clean_code.commit(sha),
    kind         clean_code.scan_run_kind NOT NULL,
    status       clean_code.scan_run_status NOT NULL DEFAULT 'running',
    started_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at  TIMESTAMPTZ
);

-- metric_sample: one row per computed metric snapshot for a commit.
CREATE TABLE IF NOT EXISTS clean_code.metric_sample (
    sample_id    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    commit_sha   TEXT NOT NULL REFERENCES clean_code.commit(sha),
    payload      JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- metric_sample_active: pointer table — one row per commit, pointing to
-- the currently-active metric_sample.  UPSERT keeps exactly one row.
CREATE TABLE IF NOT EXISTS clean_code.metric_sample_active (
    commit_sha   TEXT PRIMARY KEY REFERENCES clean_code.commit(sha),
    sample_id    UUID NOT NULL REFERENCES clean_code.metric_sample(sample_id),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- metric_retraction: tombstone table for retracted samples (tech-spec §7.2).
-- The row in metric_sample_active is NOT deleted (REVOKE DELETE); readers
-- LEFT JOIN metric_retraction and filter WHERE mr.sample_id IS NULL.
CREATE TABLE IF NOT EXISTS clean_code.metric_retraction (
    sample_id    UUID PRIMARY KEY REFERENCES clean_code.metric_sample(sample_id),
    reason       TEXT NOT NULL DEFAULT '',
    retracted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);