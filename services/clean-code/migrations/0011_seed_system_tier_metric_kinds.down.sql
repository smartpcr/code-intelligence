-- 0011_seed_system_tier_metric_kinds.down.sql
--
-- Reverses the seven canonical system-tier metric_kind
-- seed rows inserted by the matching `.up.sql`. The
-- DELETE is scoped to the EXACT (metric_kind,
-- metric_version) tuples this UP inserted (all seven
-- system-tier kinds at metric_version = 1). A Steward-
-- curated row at a DIFFERENT metric_version (>=2) is
-- preserved by the tuple predicate.
--
-- # Steward-customization safety
--
-- A Steward-curated row that pre-existed at
-- metric_version = 1 BEFORE this UP ran cannot be
-- distinguished from a row this UP seeded post-fact
-- (the ON CONFLICT (metric_kind) DO NOTHING in the UP
-- makes the two cases indistinguishable). Running this
-- DOWN deletes BOTH cases. Operators with a Steward-
-- managed v1 row MUST instead bump the seed-managed
-- copy to metric_version=2 BEFORE running this DOWN
-- (matches the operator-facing caveat documented on
-- `0007_seed_foundation_metric_kinds.down.sql`).
--
-- # Order
--
-- Per `metric_sample.metric_kind` composite FK to
-- `metric_kind.(metric_kind, metric_version)` (migration
-- `0002_measurement.up.sql:348-350`) this DELETE will
-- FAIL when an active `metric_sample` row references one
-- of the seven kinds at metric_version=1 -- the operator
-- MUST retract those rows (via the canonical retraction
-- workflow) before running this DOWN.

BEGIN;

DELETE FROM clean_code.metric_kind
WHERE (metric_kind, metric_version) IN (
        ('xrepo_dep_depth', 1),
        ('arch_debt_ratio', 1),
        ('velocity_trend', 1),
        ('arch_fitness', 1),
        ('blast_radius', 1),
        ('xservice_test_reliability', 1),
        ('knowledge_index', 1)
);

COMMIT;
