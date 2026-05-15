-- 0006a_ingest_jobs.sql
--
-- Stage 1.2 step 7 (implementation-plan.md): create the durable
-- job-queue table that backs the Repo Indexer (Stage 3.1) and the
-- Management onboarding verbs (Stage 7.1).
--
-- The implementation-plan calls out:
--   * columns: job_id uuid PK, repo_id uuid FK, mode ENUM
--     (full|delta|manual), from_sha text?, to_sha text,
--     status ENUM (pending|claimed|running|done|failed),
--     attempt_index int, claimed_by text?, created_at timestamptz,
--     updated_at timestamptz.
--   * UNIQUE on (repo_id, mode, COALESCE(from_sha,''), to_sha) for
--     idempotent enqueue.
--   * Partial B-tree on (status, created_at) WHERE status='pending'
--     so `SELECT ... FOR UPDATE SKIP LOCKED` is fast.
--
-- The two ENUMs (ingest_mode, ingest_status) are local to this
-- subsystem and are NOT listed in tech-spec §8.7.1 (which
-- enumerates the architecture-level ENUMs). They live in this
-- migration so the queue's closed set rotates with the queue, not
-- with the structural graph.
--
-- ingest_jobs is a working queue, NOT an audit log. tech-spec
-- §8.7.4 keeps it OUT of the append-only set; Stage 1.4 migration
-- 0016 grants INSERT+SELECT+UPDATE on it so the Repo Indexer can
-- flip status pending -> claimed -> running -> done/failed and
-- bump attempt_index.

-- migrate:up
BEGIN;

CREATE TYPE ingest_mode AS ENUM (
    'full',
    'delta',
    'manual'
);

CREATE TYPE ingest_status AS ENUM (
    'pending',
    'claimed',
    'running',
    'done',
    'failed'
);

CREATE TABLE ingest_jobs (
    job_id        uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id       uuid          NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    mode          ingest_mode   NOT NULL,
    from_sha      text,
    to_sha        text          NOT NULL,
    status        ingest_status NOT NULL DEFAULT 'pending',
    attempt_index int           NOT NULL DEFAULT 0,
    claimed_by    text,
    created_at    timestamptz   NOT NULL DEFAULT now(),
    updated_at    timestamptz   NOT NULL DEFAULT now()
);

-- Idempotent enqueue: the same (repo, mode, from_sha, to_sha)
-- tuple maps to a single ingest_jobs row even if the upstream
-- webhook receiver retries. COALESCE(from_sha,'') normalises the
-- nullable column so the uniqueness key is computable across
-- (full|manual) jobs (which have no from_sha) and delta jobs
-- (which do).
CREATE UNIQUE INDEX ingest_jobs_dedupe_uidx
    ON ingest_jobs (repo_id, mode, COALESCE(from_sha, ''), to_sha);

-- Hot path: `SELECT ... FROM ingest_jobs WHERE status='pending'
-- ORDER BY created_at FOR UPDATE SKIP LOCKED`. A partial index
-- keeps the working set small even after millions of completed
-- jobs accumulate (we never delete from this table; row count
-- equals lifetime enqueues).
CREATE INDEX ingest_jobs_pending_idx
    ON ingest_jobs (status, created_at)
    WHERE status = 'pending';

-- Operator visibility: "show me the most recent attempts per
-- repo, regardless of status".
CREATE INDEX ingest_jobs_repo_updated_idx
    ON ingest_jobs (repo_id, updated_at DESC);

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS ingest_jobs;
DROP TYPE  IF EXISTS ingest_status;
DROP TYPE  IF EXISTS ingest_mode;

COMMIT;
