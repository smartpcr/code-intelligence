-- 0012_seed_ingested_metric_kind_pass_first_try_ratio.down.sql
--
-- Reverse of
-- 0012_seed_ingested_metric_kind_pass_first_try_ratio.up.sql.
-- Deletes the single ingested-pack catalog row seeded by the
-- UP half, scoped to the EXACT
-- `(metric_kind, metric_version) = ('pass_first_try_ratio', 1)`
-- tuple. A Steward-curated row at a DIFFERENT `metric_version`
-- survives the DELETE.
--
-- Caveat: any `clean_code.metric_sample` rows that reference
-- `('pass_first_try_ratio', 1)` will block the DELETE via the
-- FK `metric_sample_metric_kind_fk` at
-- `migrations/0002_measurement.up.sql:348-350`
-- (`ON DELETE RESTRICT`). Operators rolling this DOWN on a
-- populated database MUST first remove the dependent
-- `metric_sample` rows. This is intentional: the catalog row
-- carries the unit + description for stored samples, so
-- silently dropping it would render the persisted data
-- uninterpretable.
--
-- Steward-customization safety:
--   * A row that the Policy Steward bumped to
--     `metric_version >= 2` is preserved -- the tuple
--     predicate below excludes it.
--   * A row that the Policy Steward seeded at
--     `metric_version = 1` BEFORE this UP ran cannot be
--     distinguished from a row this UP seeded (the UP
--     idempotency clause `ON CONFLICT (metric_kind) DO
--     NOTHING` makes the two cases indistinguishable
--     post-fact). Operators rolling back on a Steward-
--     customized DB MUST verify their v=1 row is
--     dispensable before applying this DOWN.

BEGIN;

DELETE FROM clean_code.metric_kind
 WHERE (metric_kind, metric_version) IN (
     ('pass_first_try_ratio', 1)
 );

COMMIT;
