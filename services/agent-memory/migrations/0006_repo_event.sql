-- 0006_repo_event.sql
--
-- Stage 1.2 step 6 (implementation-plan.md): create the
-- `repo_event` table per architecture.md §5.6 line 644. The
-- `kind` column is the closed upstream set `push|merge|register|
-- manual`; this migration enforces that by typing the column with
-- the `repo_event_kind` ENUM created in 0001. No other `kind`
-- value can be inserted.
--
-- tech-spec §8.7.4 keeps `repo_event` UPDATE-grantable for now
-- because architecture.md does not pin it as immutable. If a
-- later architecture revision does pin it, the role grants in
-- Stage 1.4 migration 0016 move the table to the no-UPDATE side
-- without touching this DDL.

-- migrate:up
BEGIN;

CREATE TABLE repo_event (
    event_id    uuid            PRIMARY KEY DEFAULT gen_random_uuid(),
    repo_id     uuid            NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    kind        repo_event_kind NOT NULL,
    from_sha    text,
    to_sha      text            NOT NULL,
    received_at timestamptz     NOT NULL DEFAULT now()
);

CREATE INDEX repo_event_repo_received_at_idx
    ON repo_event (repo_id, received_at DESC);

CREATE INDEX repo_event_kind_received_at_idx
    ON repo_event (kind, received_at DESC);

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS repo_event;

COMMIT;
