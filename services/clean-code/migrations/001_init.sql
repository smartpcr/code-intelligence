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
