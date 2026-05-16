-- 0019_repo_health.sql
--
-- Stage 4.2 step 5 (implementation-plan.md): persist the per-repo
-- degraded-state flag the Span Ingestor raises when its input
-- queue depth exceeds the §8.3 sustained envelope. Subsequent
-- `agent.recall` responses surface the flag via
-- `degraded=true, degraded_reason='span_ingestor_backpressure'`
-- (architecture.md §8.2; tech-spec §C22 closed degraded_reason
-- set).
--
-- Why a TABLE and not in-memory state
-- -----------------------------------
-- The Span Ingestor (cmd/span-ingestor) and the Agent API
-- (cmd/agent-api) run in SEPARATE processes. An in-memory
-- registry would be invisible across the process boundary; the
-- agent-api recall handler MUST consult the same state the
-- ingestor wrote, which means it has to live in a place both
-- binaries can reach. The cheapest such place is PostgreSQL,
-- which both binaries already have credentials for.
--
-- Cardinality
-- -----------
-- One row per `repo_id`. Both binaries upsert; the agent-api
-- reads. The table is intentionally narrow so the SELECT a
-- recall handler runs on every request is single-page.
--
-- Cleared-vs-stale distinction
-- ----------------------------
-- The ingestor sets `degraded=true` when backpressure is
-- triggered and writes `degraded=false` when the queue depth
-- drops below the clearance threshold. A row is never deleted;
-- the absence of a row for a given repo means "no signal yet"
-- which the agent-api treats as healthy.
--
-- Schema column rationale
-- -----------------------
--   * `repo_id` (PK; FK on `repo` ON DELETE CASCADE): rows are
--     keyed exclusively by repo. Cascade keeps the table
--     coherent if an operator ever deregisters a repo.
--   * `degraded` (boolean NOT NULL): the boolean the recall
--     response surfaces.
--   * `degraded_reason` (`degraded_reason` ENUM, nullable):
--     the closed-set reason; the CHECK below couples it with
--     `degraded` so the (degraded=true ↔ reason IS NOT NULL)
--     invariant is enforced in-database (matches the same
--     CHECK on `episode` from migration 0007).
--   * `source` (text NOT NULL): identifies the worker that
--     last wrote the row (e.g. `span-ingestor`,
--     `consolidator`) so operators can attribute the flag to
--     its origin. Free-form text — different worker classes
--     own their own values; we don't ENUM it because the set
--     of workers grows with the service.
--   * `updated_at` (timestamptz NOT NULL DEFAULT now()): the
--     UPSERT path sets this so a stale flag is detectable via
--     `WHERE updated_at < now() - interval 'N minutes'`.
--   * `since` (timestamptz NOT NULL DEFAULT now()): captures
--     when the CURRENT state (degraded or not) began. The
--     UPSERT preserves `since` when `degraded` is unchanged
--     and bumps it on transition. Operators read it to
--     answer "how long has this repo been degraded".
--
-- Role grants
-- -----------
-- `agent_memory_app` needs INSERT, SELECT, UPDATE (the
-- ingestor UPSERTs). `agent_memory_ro` needs SELECT (the
-- recall handler reads). The default-privileges rule from
-- migration 0017 already grants SELECT to `agent_memory_ro`
-- on every new table, so we only explicitly add the writer
-- grant here.
--
-- DELETE is never granted to either app role: state is
-- additive (rows accumulate as repos onboard; degraded-state
-- transitions update the row in place).

-- migrate:up
BEGIN;

CREATE TABLE repo_health (
    repo_id         uuid             PRIMARY KEY REFERENCES repo (repo_id) ON DELETE CASCADE,
    degraded        boolean          NOT NULL DEFAULT false,
    degraded_reason degraded_reason,
    source          text             NOT NULL,
    since           timestamptz      NOT NULL DEFAULT now(),
    updated_at      timestamptz      NOT NULL DEFAULT now(),
    CONSTRAINT repo_health_degraded_reason_chk CHECK (
           (degraded = true  AND degraded_reason IS NOT NULL)
        OR (degraded = false AND degraded_reason IS     NULL)
    )
);

-- One index for the "all currently degraded repos" operator
-- query; partial so only the (typically tiny) set of degraded
-- rows is indexed. Matches the dashboard query
-- `SELECT repo_id FROM repo_health WHERE degraded`.
CREATE INDEX repo_health_degraded_idx
    ON repo_health (repo_id) WHERE degraded;

DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format(
        'GRANT INSERT, SELECT, UPDATE ON %I.%I TO agent_memory_app',
        cs, 'repo_health'
    );
END$$;

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS repo_health;

COMMIT;
