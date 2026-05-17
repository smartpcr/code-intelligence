-- 0021_concept_candidate.sql
--
-- Stage 6.1 follow-on (iter-4): durable sub-threshold candidate
-- support for the Consolidator worker. Addresses the iter-3
-- evaluator's #1 finding -- the "walk-until-first-pending"
-- cursor-mgmt strategy in service.go could pin the high-water
-- mark forever when a permanently sub-threshold (or negative-
-- only) signature appeared early in scan order, forcing
-- unbounded re-scans and wake-after-N storms.
--
-- Design summary
-- --------------
-- One table keyed on the 32-byte SHA-256 `signature` (same value
-- the Concept table uses as its `fingerprint`). Each row records
-- one support contribution -- (signature, repo_id, node_id,
-- episode_id, polarity) -- toward a NOT-YET-PROMOTED concept
-- candidate. The Consolidator INSERTs per-(Episode, Node) rows
-- on every Tick that scans an Episode whose signature has no
-- crystallised Concept yet. When the cumulative
-- COUNT(DISTINCT episode_id) FILTER (WHERE polarity='positive')
-- crosses the threshold, the Consolidator PROMOTEs the
-- candidate atomically inside the per-group transaction:
--   1. INSERT into concept (with fingerprint = signature).
--   2. INSERT into concept_version (v=0, support_count =
--      distinct positive episodes, negative_count = distinct
--      negative episodes).
--   3. COPY each pending candidate_support row into
--      concept_support (preserving repo_id, node_id, episode_id,
--      polarity).
--   4. UPDATE concept_candidate_support
--          SET promoted_to_concept_id = <concept_id>
--        WHERE candidate_support_id = ANY(<locked_ids>);
--      i.e. mark the locked-FOR-UPDATE row set as promoted.
--
-- The DELETE pattern is intentionally AVOIDED:
--   * §8.7.4 reserves DELETE for `agent_memory_admin` only;
--     adding DELETE to `agent_memory_app`'s grant set would
--     break the "no DELETE for the app role" architectural
--     posture established in 0016.
--   * The promoted-flag pattern preserves the audit trail of
--     which Episodes contributed to a Concept (operator-visible
--     forensics).
--
-- Cursor semantics consequence
-- ----------------------------
-- With this table in place, the Consolidator's high-water mark
-- ALWAYS advances to the latest scanned Episode. Sub-threshold
-- support is durable across ticks via concept_candidate_support;
-- the cursor never has to be "held back". This makes the lag
-- gauge monotonic, eliminates the wake-after-N storm risk
-- when long-tail signatures sit below threshold forever, and
-- removes the iter-3 walk-until-first-pending logic in
-- service.go's processOnce.
--
-- Cross-repo support (G6) is preserved
-- ------------------------------------
-- The candidate_support row carries an explicit repo_id (the
-- Episode's repo). On promotion, each row is faithfully copied
-- into concept_support so a Concept whose Episodes span two
-- repos lands one concept_support row per (repo, episode, node)
-- tuple -- identical to the today-path that already covers
-- TestTick_supportSpansRepos.
--
-- Grants
-- ------
-- This migration runs AFTER 0016 (the role-grants migration).
-- 0016's `GRANT ... ON ALL TABLES IN SCHEMA` is point-in-time
-- and does NOT cover later-created tables, but the
-- `ALTER DEFAULT PRIVILEGES IN SCHEMA ... GRANT ALL PRIVILEGES
-- ON TABLES TO agent_memory_admin` rule that 0016 installed
-- DOES cover this table automatically (admin gets full DML).
-- We add `agent_memory_app`'s `INSERT, SELECT, UPDATE` grant
-- explicitly here -- mirroring 0012's UPDATE-grantable set
-- (consolidator_run is the closest analogue: app reads, writes,
-- updates its own progress state but never deletes).

-- migrate:up
BEGIN;

CREATE TABLE concept_candidate_support (
    candidate_support_id   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    signature              bytea       NOT NULL,
    repo_id                uuid        NOT NULL REFERENCES repo (repo_id) ON DELETE RESTRICT,
    -- node_id is optional for the same reason concept_support.node_id is
    -- optional (arch §5.5.3): an Episode may have no node_hit observations
    -- (edge-only / concept-only Episode).
    node_id                uuid        REFERENCES node (node_id) ON DELETE RESTRICT,
    episode_id             uuid        NOT NULL,
    polarity               polarity    NOT NULL,
    created_at             timestamptz NOT NULL DEFAULT now(),
    -- NULL while the candidate is still accumulating sub-threshold
    -- support; set to the concept_id at PROMOTE time. The cumulative
    -- count query filters `promoted_to_concept_id IS NULL` so a
    -- promoted candidate's rows never inflate the next candidate
    -- for the same signature (defence in depth: the post-promotion
    -- code path goes straight to concept_support, but this filter
    -- means a buggy or replayed promoter cannot create a phantom
    -- second candidate).
    promoted_to_concept_id uuid        REFERENCES concept (concept_id) ON DELETE RESTRICT,
    CONSTRAINT concept_candidate_support_signature_octet_length_chk
        CHECK (octet_length(signature) = 32)
);

-- Cumulative-count read path: WHERE signature = $1 AND promoted_to_concept_id IS NULL.
-- Partial index keeps the pending working-set tight.
CREATE INDEX concept_candidate_support_sig_pending_idx
    ON concept_candidate_support (signature, episode_id)
    WHERE promoted_to_concept_id IS NULL;

-- Promotion-time scan: "all pending support rows for signature $sig".
-- (Slightly different from the partial index: this one is needed for
-- the SELECT ... FOR UPDATE that locks the promotion set.)
CREATE INDEX concept_candidate_support_sig_idx
    ON concept_candidate_support (signature);

-- Per-app grant (post-0016: the GRANT ... ALL TABLES IN SCHEMA there
-- is point-in-time, so a table created here needs an explicit grant).
-- Admin already has DEFAULT PRIVILEGES coverage via 0016.
DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format(
        'GRANT INSERT, SELECT, UPDATE ON %I.%I TO agent_memory_app',
        cs, 'concept_candidate_support'
    );
END$$;

COMMIT;

-- migrate:down
BEGIN;

DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format(
        'REVOKE INSERT, SELECT, UPDATE ON %I.%I FROM agent_memory_app',
        cs, 'concept_candidate_support'
    );
END$$;

DROP TABLE IF EXISTS concept_candidate_support;

COMMIT;
