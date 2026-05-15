-- 0002_repo_commit.sql
--
-- Stage 1.2 step 2 (implementation-plan.md): create the `repo` and
-- `repo_commit` tables per architecture.md §5.6 with the timestamptz /
-- uuid / text typing of tech-spec §8.7.1.
--
-- `repo` is mutable (settings-only per architecture.md §5.1).
-- `repo_commit` is the immutable snapshot identity row per
-- architecture.md §5.1 "Commit | Immutable" and §8.7.4
-- (append-only, no UPDATE grant for the application role).
--
-- Naming note: the SQL table is `repo_commit`, not `commit`. The
-- architectural entity is still "Commit" (architecture.md §5.6),
-- but `COMMIT` is a PostgreSQL key word (non-reserved, recognized
-- by the parser in transaction-control contexts). Using it as an
-- unquoted identifier is legal but trips ORMs, query builders,
-- and hand-written SQL that don't quote it -- e.g. `DELETE FROM
-- commit WHERE ...` reads ambiguously next to `COMMIT;`. The
-- `repo_` prefix also matches the migration filename and the
-- `repo_event` naming used in 0006_repo_event.sql.
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

CREATE TABLE repo_commit (
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

CREATE INDEX repo_commit_committed_at_idx
    ON repo_commit (repo_id, committed_at DESC);

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS repo_commit;
DROP TABLE IF EXISTS repo;

COMMIT;
