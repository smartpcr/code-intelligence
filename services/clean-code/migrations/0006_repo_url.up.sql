-- 0006_repo_url.up.sql
--
-- iter-6 evaluator item 1 (Stage 3.2 Metric Ingestor):
-- Add a dedicated `repo_url` column to `clean_code.repo` so the
-- Metric Ingestor's canonical-signature resolver can stamp every
-- `metric_sample` (and the `scope_binding` it pivots on) with a
-- repository identifier that is BOTH:
--
--   * IMMUTABLE post-registration -- enforced at the DB layer by
--     the `tg_repo_url_write_once` trigger defined below: a
--     NULL -> non-NULL transition is allowed (so pre-0006 catalog
--     rows can be back-filled by a one-off operator UPDATE), but
--     any subsequent change is rejected with SQLSTATE 23514
--     (check_violation). The trigger guarantees G2 (scope_binding)
--     stability even if a future migration grants stronger UPDATE
--     privileges or a Management verb attempts a rename --
--     iter-7 evaluator item 4.
--   * STRUCTURED -- a URL-shaped string (e.g. `https://github.com/org/repo`)
--     that matches the agent-memory side's repo identifier, so a
--     canonical signature built on the clean-code side is byte-identical
--     to the one built on the agent-memory side for the same logical scope.
--
-- # Why a new column and not reuse `display_name`?
--
-- `clean_code.repo.display_name` is documented as "free-form" in
-- architecture.md Sec 5.1.1 line 876, with both INSERT and UPDATE
-- permissions on the management role per 0004_roles.up.sql lines
-- 311-312 (`mgmt.set_mode` / `mgmt.rename_repo` / etc.). Treating
-- it as the canonical URL would make canonical-signature parity and
-- G2 (scope_binding) stability dependent on operator policy --
-- a `display_name` UPDATE would silently break `scope_binding`
-- natural-key reuse for that repo. iter-4 + iter-5 evaluator
-- feedback flagged this as an architecture-parity gap; the fix is
-- a dedicated column that the application contract treats as
-- WRITE-ONCE.
--
-- # Nullability and back-compat
--
-- The new column is NULLABLE. Existing rows from earlier migrations
-- have no `repo_url`; the lookup
-- (`internal/metric_ingestor/repo_url_lookup.go`) raises
-- `ErrRepoURLLookupNotFound` for NULL values so the dispatcher
-- fails fast instead of silently minting signatures keyed off
-- an empty stamp.
--
-- # Scope of THIS migration vs Stage 1.2 follow-up
--
-- THIS migration (Stage 3.2 Metric Ingestor) adds ONLY:
--   (a) the `repo_url text NULL` column;
--   (b) column-level INSERT/UPDATE grants to Management; and
--   (c) the WRITE-ONCE trigger.
-- It does NOT alter the application-side `mgmt.register_repo`
-- signature. iter-7 evaluator item 3: the Stage 1.2 (Repo
-- Indexer) follow-up workstream
-- `ws-code-intelligence-clean-code-phase-repo-indexer-and-metric-ingestor-stage-mgmt-register-repo-repo-url`
-- (TODO -- not yet scaffolded) will:
--   1. Extend the Management surface so `mgmt.register_repo`
--      accepts `repo_url` as a required argument.
--   2. Backfill `repo_url` for existing rows via a one-off
--      DBA-run UPDATE (covered by the WRITE-ONCE trigger's
--      NULL -> non-NULL allowance).
--   3. Tighten the column to NOT NULL once every catalog row
--      has been backfilled.
-- Until that follow-up lands, the Metric Ingestor's PG path
-- requires test fixtures and operator-supplied repo rows to
-- include `repo_url`; the lookup fails fast (with
-- `ErrRepoURLLookupNotFound`) for NULL rows so the failure
-- mode is loud and obvious to triage.
--
-- # Privileges
--
-- Management gets column-level INSERT and UPDATE on `repo_url`
-- (parity with the existing `display_name` / `mode` / `default_branch`
-- grants). The UPDATE grant is intentionally kept so back-fill
-- of pre-0006 rows works; the `tg_repo_url_write_once` trigger
-- below is the actual immutability guard (it allows the
-- NULL -> non-NULL transition but rejects any subsequent change).
-- The Repo Indexer remains the sole writer of
-- `default_branch_head` (no change). Other roles inherit SELECT
-- via the cross-sub-store read grant in 0004_roles.up.sql
-- lines 227-260, which lists `clean_code.repo` -- so
-- `clean_code_metric_ingestor` can `SELECT repo_url FROM clean_code.repo`
-- without an explicit per-column SELECT grant (PG defaults to
-- table-level SELECT covering every column).

BEGIN;

-- ---------------------------------------------------------------------------
-- Column
-- ---------------------------------------------------------------------------
--
-- NULL allowed for back-compat with rows inserted before this
-- migration; the application's `PGRepoURLLookup` surfaces
-- `ErrRepoURLLookupNotFound` when the column is NULL so the
-- failure mode is loud rather than silent.

ALTER TABLE clean_code.repo
    ADD COLUMN IF NOT EXISTS repo_url text NULL;

COMMENT ON COLUMN clean_code.repo.repo_url IS
    'Operator-provided repository URL (e.g. https://github.com/org/repo). '
    'WRITE-ONCE: enforced at the DB layer by the BEFORE UPDATE trigger '
    '`tg_repo_url_write_once` defined alongside this column in '
    '0006_repo_url.up.sql. A NULL -> non-NULL transition is allowed (so '
    'pre-0006 catalog rows can be back-filled by a DBA), but any '
    'subsequent change is rejected with SQLSTATE 23514 (check_violation). '
    'This guarantees that `scope_binding.canonical_signature` rows '
    'keyed on this URL remain stable across the lifetime of the repo '
    'row, satisfying the G2 stability guarantee. The Metric Ingestor '
    'reads this via `PGRepoURLLookup.LookupRepoURL` to stamp every '
    '`metric_sample` and `scope_binding` row''s canonical signature '
    'with a structured, agent-memory-parity URL; if the column is NULL '
    'the lookup returns `ErrRepoURLLookupNotFound` and the dispatcher '
    'aborts the scan rather than silently minting signatures with an '
    'empty URL stamp (iter-6 evaluator item 1; iter-7 evaluator item 4).';

-- ---------------------------------------------------------------------------
-- Privileges (parity with 0004_roles.up.sql column-level grants)
-- ---------------------------------------------------------------------------
--
-- Management owns this column at registration time. The grant
-- additions mirror the existing per-column INSERT/UPDATE shape
-- so the C5 row 1 ACL contract continues to be enforceable at
-- the DB layer (an `UPDATE ... SET default_branch_head=...`
-- attempt from Management still fails with SQLSTATE 42501).
--
-- The Repo Indexer is NOT granted INSERT/UPDATE on `repo_url`
-- -- it is Management-owned. The Metric Ingestor (and every
-- other read consumer) inherits SELECT via the cross-sub-store
-- read grant on `clean_code.repo` in 0004_roles.up.sql line 228,
-- which covers all columns.
--
-- iter-7 evaluator item 4: the UPDATE grant is kept (so a DBA
-- back-fill UPDATE can run as Management instead of superuser),
-- but the `tg_repo_url_write_once` trigger below blocks any
-- non-back-fill update -- a defence-in-depth pattern that
-- survives even if a future migration accidentally widens the
-- UPDATE grant.

GRANT INSERT (repo_url) ON clean_code.repo TO clean_code_management;
GRANT UPDATE (repo_url) ON clean_code.repo TO clean_code_management;

-- ---------------------------------------------------------------------------
-- WRITE-ONCE enforcement (iter-7 evaluator item 4)
-- ---------------------------------------------------------------------------
--
-- A BEFORE UPDATE OF repo_url trigger that:
--   * ALLOWS the NULL -> non-NULL transition (back-fill of
--     pre-0006 rows; one-off operator correction before first
--     scan when the column was initially inserted as NULL).
--   * REJECTS any change once `repo_url` is already non-NULL.
--     The error message names both the OLD and NEW values so
--     an operator can immediately see what was attempted.
--
-- The function is declared SECURITY INVOKER (the default) so
-- it runs with the privileges of the caller -- this is
-- intentional: only roles that already have UPDATE (repo_url)
-- can reach the trigger, and the trigger then narrows their
-- effective rights to "initial set only".
--
-- The function is owned by the schema owner (the migration
-- runner), so a Management-role UPDATE attempt that violates
-- the trigger surfaces the trigger's RAISE EXCEPTION to the
-- caller (not a permission-denied error).

CREATE OR REPLACE FUNCTION clean_code.tg_repo_url_write_once()
RETURNS trigger AS $$
BEGIN
    -- The trigger is declared `BEFORE UPDATE OF repo_url` so
    -- it only fires when repo_url is in the SET list; we
    -- still guard on the actual value-changing condition so
    -- a redundant `UPDATE ... SET repo_url = repo_url` is a
    -- no-op rather than an error (some ORMs emit these).
    IF OLD.repo_url IS NOT NULL
       AND NEW.repo_url IS DISTINCT FROM OLD.repo_url THEN
        RAISE EXCEPTION USING
            ERRCODE = '23514',
            MESSAGE = format(
                'clean_code.repo.repo_url is WRITE-ONCE: cannot change from %L to %L for repo_id %L',
                OLD.repo_url, NEW.repo_url, OLD.repo_id
            ),
            HINT    = 'This column backs the canonical-signature stamp for the repo''s '
                      'scope_binding rows; changing it would invalidate G2 stability. '
                      'If the URL is wrong, the operator must retire the repo row '
                      '(retract its metric_sample rows + delete the scope_binding rows) '
                      'and re-register with the correct URL.';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

COMMENT ON FUNCTION clean_code.tg_repo_url_write_once() IS
    'iter-7 evaluator item 4: WRITE-ONCE enforcement for '
    'clean_code.repo.repo_url. Allows NULL -> non-NULL '
    '(back-fill); rejects any subsequent change with SQLSTATE '
    '23514. Defence-in-depth that survives accidental widening '
    'of the UPDATE (repo_url) grant in future migrations.';

DROP TRIGGER IF EXISTS tg_repo_url_write_once ON clean_code.repo;

CREATE TRIGGER tg_repo_url_write_once
    BEFORE UPDATE OF repo_url ON clean_code.repo
    FOR EACH ROW
    EXECUTE FUNCTION clean_code.tg_repo_url_write_once();

COMMIT;
