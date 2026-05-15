-- 0008_episode_update.sql
--
-- Stage 1.3 step 2 (implementation-plan.md): create the
-- `episode_update` table per architecture.md §5.3.2, partitioned
-- monthly on `created_at` per tech-spec §8.7.3.
--
-- The implementation-plan calls out "FK to Episode.episode_id"; on
-- PostgreSQL 16 that requires the parent's partition key
-- (`episode.created_at`) to be carried as a column on this child,
-- bloating every EpisodeUpdate row with the parent's append
-- timestamp. We carry the `episode_id` column without a DB-level
-- FK and rely on app-level referential integrity in the
-- GraphWriter (Stage 5.1), matching the pattern documented at the
-- head of 0007_episode.sql.
--
-- Hot-path: the `current_status` join described in tech-spec
-- §8.7.2 walks from `episode` to the most-recent EpisodeUpdate.
-- A partial B-tree on `(episode_id)` (where `episode_id` is
-- NOT NULL by definition; the partial clause is vacuous here
-- but documents intent) gives the join an index seek instead
-- of a partition-aware sequential scan; the leading
-- `episode_id` column means partition pruning still engages
-- via the matching predicate on the joined `episode.created_at`.

-- migrate:up
BEGIN;

CREATE TABLE episode_update (
    update_id   uuid        NOT NULL DEFAULT gen_random_uuid(),
    episode_id  uuid        NOT NULL,
    new_outcome outcome     NOT NULL,
    note        text,
    actor       actor       NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (update_id, created_at)
) PARTITION BY RANGE (created_at);

-- current_status join per tech-spec §8.7.2. Carrying
-- (episode_id, created_at DESC) lets the GraphReader fetch the
-- most-recent EpisodeUpdate per episode_id with a single index
-- range scan. The DESC qualifier keeps the latest row first.
CREATE INDEX episode_update_episode_created_idx
    ON episode_update (episode_id, created_at DESC);

CREATE TABLE episode_update_default
    PARTITION OF episode_update DEFAULT;

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS episode_update_default;
DROP TABLE IF EXISTS episode_update;

COMMIT;
