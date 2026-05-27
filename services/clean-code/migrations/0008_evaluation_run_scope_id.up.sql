-- 0008_evaluation_run_scope_id.up.sql
--
-- Stage 5.7 (Rule Engine, synchronous mode) -- evaluator iter-6
-- feedback #2: extend cross-replica run dedup to caller='eval_gate'.
--
-- The canonical schema for `clean_code.evaluation_run` predates
-- the per-scope synchronous gate path: it does not record the
-- `scope` argument that `rule_engine.RunSync(repo_id, sha,
-- scope?, policy_version_id)` accepts. Stage 5.7 iter 6 wired
-- Store-level cross-replica dedup ONLY for `caller='batch_refresh'`
-- because a SCOPED `eval_gate` row could not be distinguished
-- from an UNSCOPED `eval_gate` row at the row level, and reusing
-- one for the other would yield an INCORRECT verdict (different
-- findings under a different scope).
--
-- This migration closes that gap by adding a nullable `scope_id`
-- column to `evaluation_run`. `scope_id IS NULL` represents the
-- whole-SHA evaluation (the canonical `eval.gate` happy path
-- when the caller did not supply a scope, and every
-- `batch_refresh` run by construction). `scope_id` is the per-
-- scope evaluation. The column is nullable so existing rows
-- (and the Audit-WAL reconciler's replay rows) retain meaningful
-- semantics under the old shape.
--
-- A composite index supports the new Store-level dedup query
-- shape: `WHERE repo_id = $1 AND sha = $2 AND
-- policy_version_id = $3 AND caller = $4 AND
-- scope_id IS NOT DISTINCT FROM $5 ORDER BY created_at DESC
-- LIMIT 1`. The leading columns mirror the lookup tuple; the
-- trailing `created_at DESC` lets PostgreSQL serve the
-- `ORDER BY created_at DESC LIMIT 1` directly from the index.

-- Idempotency contract (iter-9 evaluator feedback #1):
-- `IF NOT EXISTS` guards both the ALTER TABLE and the
-- CREATE INDEX so the whole migration is safe to re-run
-- against a partially-applied state. Concretely: a
-- previous run that successfully added the column but
-- whose `CREATE INDEX CONCURRENTLY` was interrupted
-- (leaving the index in PostgreSQL's INVALID state) can
-- be recovered by `DROP INDEX CONCURRENTLY IF EXISTS
-- clean_code.evaluation_run_dedup_idx` and re-running
-- `psql -f migrations/0008_evaluation_run_scope_id.up.sql`
-- WITHOUT the ALTER step failing on "column scope_id
-- already exists". This matches the operator-facing
-- retry path documented in `docs/rollout.md` Step 1.
ALTER TABLE clean_code.evaluation_run
    ADD COLUMN IF NOT EXISTS scope_id uuid NULL;

COMMENT ON COLUMN clean_code.evaluation_run.scope_id IS
    'Per-scope synchronous gate evaluation (Stage 5.7 / '
    'evaluator iter-6 feedback #2). NULL represents the '
    'whole-SHA evaluation: the canonical eval.gate happy path '
    'with no scope argument, and every batch_refresh run by '
    'construction. Non-null represents a per-scope eval.gate '
    'call. The column is nullable -- pre-migration rows and '
    'the Audit WAL reconciler''s replay rows retain meaningful '
    'semantics under the old shape.';

-- Compound dedup index. The lookup query produced by
-- `rule_engine.SQLStore.LookupRecentCanonicalRun` is
-- WHERE repo_id = $1 AND sha = $2 AND policy_version_id = $3
--   AND caller = $4 AND scope_id IS NOT DISTINCT FROM $5
-- ORDER BY created_at DESC LIMIT 1; the leading columns are the
-- equality predicates and the trailing `created_at DESC` lets
-- PostgreSQL serve the LIMIT 1 directly from the index without
-- a sort step.
--
-- Live-rollout safety (iter-8 evaluator feedback #3): the index
-- is built CONCURRENTLY so the build acquires
-- SHARE UPDATE EXCLUSIVE rather than ACCESS EXCLUSIVE on
-- `clean_code.evaluation_run` -- INSERTs from the Rule Engine
-- and the eval-gate degraded path continue to run while the
-- index is materialising. CONCURRENTLY cannot run inside a
-- transaction, so this migration file deliberately omits any
-- BEGIN/COMMIT envelope and operators MUST apply it with
-- `psql --single-transaction=off` (the default `psql -f` is
-- already off). The IF NOT EXISTS clause makes the build
-- idempotent: if a previous attempt was interrupted and left
-- the index in the INVALID state, the operator can DROP the
-- INVALID index (`DROP INDEX CONCURRENTLY` is also valid) and
-- retry without editing the migration.
CREATE INDEX CONCURRENTLY IF NOT EXISTS evaluation_run_dedup_idx
    ON clean_code.evaluation_run (
        repo_id,
        sha,
        policy_version_id,
        caller,
        scope_id,
        created_at DESC
    );
