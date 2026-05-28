-- 0011_seed_system_tier_metric_kinds.up.sql
--
-- Stage 7.2 -- system-tier metric_kind catalog seed.
--
-- Per architecture Sec 1.4.2 the Cross-Repo Aggregator
-- composer writes EXACTLY seven canonical system-tier
-- `metric_kind` rows -- `xrepo_dep_depth`,
-- `arch_debt_ratio`, `velocity_trend`, `arch_fitness`,
-- `blast_radius`, `xservice_test_reliability`,
-- `knowledge_index`. Every emitted `metric_sample` row
-- carries `(metric_kind, metric_version)` that MUST resolve
-- via the composite FK on
-- `clean_code.metric_sample.(metric_kind, metric_version) ->
-- clean_code.metric_kind.(metric_kind, metric_version)`
-- (per `migrations/0002_measurement.up.sql:348-350`).
--
-- The runtime aggregator role
-- (`clean_code_xrepo_aggregator`) has SELECT-only access to
-- `metric_kind` -- INSERT is granted ONLY to
-- `clean_code_policy_steward`
-- (`migrations/0004_roles.up.sql:350-355`). The composer
-- therefore cannot seed these rows at startup; this
-- schema-owner migration is the canonical insert path
-- (mirroring the iter-18 fix on
-- `0007_seed_foundation_metric_kinds.up.sql:1-58`).
--
-- INSERTs use `ON CONFLICT (metric_kind) DO NOTHING` so:
--   * the migration is idempotent across re-runs, AND
--   * a Steward-curated row (richer description, different
--     metric_version) takes precedence -- the canonical
--     writer of `metric_kind` remains the Policy Steward
--     per the COMMENT at
--     `0001_catalog_lifecycle.up.sql:283-286`.
--
-- All seven rows are seeded at `metric_version = 1`
-- matching the composer's `SystemMetricVersion` constant
-- (`internal/aggregator/system_tier.go` -- search the file
-- for `SystemMetricVersion`).
--
-- `tier='system'`, `pack='system'` per the architecture
-- contract that the Cross-Repo Aggregator is the SOLE
-- writer of `pack='system'` rows (Phase 1.5 grants;
-- `0004_roles.up.sql:392-394` grants INSERT on
-- `metric_sample` to `clean_code_xrepo_aggregator`).
--
-- Down: `0011_seed_system_tier_metric_kinds.down.sql`
-- DELETEs the seven `(metric_kind, metric_version)`
-- tuples at version=1. A Steward-curated row at a
-- DIFFERENT `metric_version` (>=2) is preserved by the
-- tuple predicate. The same Steward-customisation caveat
-- documented on the 0007 DOWN migration applies here.

BEGIN;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('xrepo_dep_depth', 1, 'system', 'system', 'count',
        'Longest cross-repo dependency chain length per repo '
        '(architecture Sec 1.4.2 row 1). Embedded mode emits '
        'degraded=true with degraded_reason=xrepo_edges_unavailable '
        'because the cross-repo edge corpus is not available.')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('arch_debt_ratio', 1, 'system', 'system', 'ratio',
        'Share of methods/files inside an architectural cycle, '
        'derived from foundation cycle_member rows '
        '(architecture Sec 1.4.2 row 2). Emits degraded=true with '
        'degraded_reason=samples_pending when cycle_member is absent.')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('velocity_trend', 1, 'system', 'system', 'ratio',
        'Rolling-window throughput change derived from '
        'modification_count_in_window foundation rows '
        '(architecture Sec 1.4.2 row 3). Emits degraded=true with '
        'degraded_reason=samples_pending when no velocity window '
        'samples are present.')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('arch_fitness', 1, 'system', 'system', 'score',
        'Composite architectural fitness score over coupling, '
        'cohesion, and cycle inputs (architecture Sec 1.4.2 row 4). '
        'Emits degraded=true with degraded_reason=samples_pending '
        'when foundation inputs are absent.')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('blast_radius', 1, 'system', 'system', 'count',
        'Downstream call-graph reach per method (architecture '
        'Sec 1.4.2 row 5). Embedded mode emits degraded=true '
        'with degraded_reason=xrepo_edges_unavailable because the '
        'cross-repo call-graph corpus is not available.')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('xservice_test_reliability', 1, 'system', 'system', 'ratio',
        'Cross-service test pass-first-try ratio derived from '
        'the foundation pass_first_try_ratio ingestion '
        '(architecture Sec 1.4.2 row 6). Emits degraded=true with '
        'degraded_reason=samples_pending when '
        'pass_first_try_ratio is absent; xrepo edge availability '
        'does NOT affect this kind.')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('knowledge_index', 1, 'system', 'system', 'score',
        'Author-knowledge concentration index per scope '
        '(architecture Sec 1.4.2 row 7). Emits degraded=true with '
        'degraded_reason=samples_pending when ingest.churn author '
        'data is absent for the scope.')
ON CONFLICT (metric_kind) DO NOTHING;

COMMIT;
