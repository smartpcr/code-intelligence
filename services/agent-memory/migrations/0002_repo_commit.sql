-- 0002_repo_commit.sql
--
-- Stage 1.2 step 2 (implementation-plan.md): create the `repo` and
-- `commit` tables per architecture.md §5.6 with the timestamptz /
-- uuid / text typing of tech-spec §8.7.1.
--
-- `repo` is mutable (settings-only per architecture.md §5.1).
-- `commit` is the immutable snapshot identity row per
-- architecture.md §5.1 "Commit | Immutable" and §8.7.4
-- (append-only, no UPDATE grant for the application role).
--
-- `language_hints` is stored as `text[]` so the Repo Indexer can
-- pass a closed list of language hints without a separate join
-- table. Order is not significant; nullability mirrors the
-- architecture spec (`language_hints[]` -- not marked optional,
-- but an empty array is allowed for repos without explicit hints).

-- migrate:up
BEGIN;

CREATE TABLE repo (
    repo_id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    url              text        NOT NULL,
    default_branch   text        NOT NULL,
    current_head_sha text        NOT NULL,
    language_hints   text[]      NOT NULL DEFAULT ARRAY[]::text[],
    created_at       timestamptz NOT NULL DEFAULT now()
);

-- A registered repository is uniquely identified by its remote URL.
-- This UNIQUE is what makes `mgmt.register` idempotent in Stage 7.1.
CREATE UNIQUE INDEX repo_url_uidx ON repo (url);

CREATE TABLE commit (
    -- (repo_id, sha) is the natural key for a Commit row per
    -- architecture.md §5.6 "Commit | repo_id, sha, ...". The
    -- composite PK avoids needing a synthetic surrogate column
    -- and lines up with how the Repo Indexer dedupes ingests.
    repo_id      uuid        NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    sha          text        NOT NULL,
    parent_sha   text,
    committed_at timestamptz NOT NULL,
    -- `index_status` is text per architecture.md §5.6; the closed
    -- set ('pending', 'indexing', 'indexed', 'failed') is enforced
    -- by the application layer in Stage 3.1. We keep it loose at
    -- the schema layer so a future status (e.g. 'reindexing') can
    -- land without a migration.
    index_status text        NOT NULL DEFAULT 'pending',
    PRIMARY KEY (repo_id, sha)
);

CREATE INDEX commit_repo_committed_at_idx
    ON commit (repo_id, committed_at DESC);

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS commit;
DROP TABLE IF EXISTS repo;

COMMIT;
