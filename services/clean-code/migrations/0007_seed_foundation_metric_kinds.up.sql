-- 0007_seed_foundation_metric_kinds.up.sql
--
-- Stage 3.2 iter 18 -- evaluator role-grant fix.
--
-- Migration 0004_roles.up.sql:350-355 grants INSERT on
-- `clean_code.metric_kind` ONLY to `clean_code_policy_steward`.
-- The documented production connection
-- (`docs/runbook.md:113`, `docs/rollout.md:68`) opens as
-- `clean_code_metric_ingestor`, which has NO INSERT permission
-- on this table -- so the iter-17 startup-time seed path was
-- structurally unworkable under the documented role contract.
--
-- This migration moves the FK-precondition catalog rows into a
-- schema-owner migration (the migration runner connects as the
-- schema owner / superuser per the standard production deploy
-- flow) so the rows are present BEFORE the ingestor opens its
-- connection. The runtime application now does VERIFY only
-- (SELECT, which the ingestor role can do).
--
-- Each row is the (metric_kind, metric_version, tier, pack,
-- unit, description_md) tuple every foundation-tier producer
-- needs for the composite FK on
-- `clean_code.metric_sample.(metric_kind, metric_version)`
-- (per `migrations/0002_measurement.up.sql:348-350`).
--
-- INSERTs use `ON CONFLICT (metric_kind) DO NOTHING` so:
--   * the migration is idempotent across re-runs, AND
--   * a Steward-curated row (richer description, different
--     version) takes precedence -- the COMMENT on
--     `clean_code.metric_kind` at `0001_catalog_lifecycle.up.sql:283-286`
--     pins the Policy Steward as the canonical writer.
--
-- The application's
-- `internal/metric_ingestor.VerifyMetricKindCatalog` is the
-- runtime SELECT-only fence that surfaces version drift
-- between this seed and the in-process recipe versions
-- (cmd/clean-code-metric-ingestor/main.go: verifyMetricKindCatalog).
--
-- Source-of-truth metadata table:
--   `internal/metric_ingestor/metric_kind_catalog.go`
--     -> `foundationCatalogMetadata` (unit + description_md)
-- Source of (metric_kind, metric_version) for each row:
--   `internal/metrics/recipes/registry.go:124-135`
--   `internal/metrics/materialisers/modification_count.go:80,89`
--
-- Down: `0007_seed_foundation_metric_kinds.down.sql` scopes
-- its DELETE to the EXACT `(metric_kind, metric_version)`
-- tuples this UP inserts (all nine kinds -- seven foundation
-- + two external-ingest coverage kinds -- at
-- `metric_version = 1`). A Steward-curated row at a
-- DIFFERENT `metric_version` (>=2) is preserved by the
-- tuple predicate. A Steward-curated row that pre-existed
-- at `metric_version = 1` BEFORE this UP ran cannot be
-- distinguished from a row this UP seeded post-fact (the
-- `ON CONFLICT (metric_kind) DO NOTHING` clause above
-- makes the two cases indistinguishable). See the
-- "Steward-customization safety" block in the DOWN file
-- for the operator-facing caveat.

BEGIN;

-- Foundation tier / base pack (architecture Sec 1.4.1 rows 1-3).
INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('cyclo', 1, 'foundation', 'base', 'count',
        'McCabe cyclomatic complexity per method and per file '
        '(architecture Sec 1.4.1 row 1).')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('cognitive_complexity', 1, 'foundation', 'base', 'count',
        'SonarSource-style cognitive complexity per method and per file '
        '(architecture Sec 1.4.1 row 2).')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('loc', 1, 'foundation', 'base', 'count',
        'Source lines of code per method and per file '
        '(architecture Sec 1.4.1 row 3).')
ON CONFLICT (metric_kind) DO NOTHING;

-- Foundation tier / solid pack (architecture Sec 1.4.1 rows 4-6).
INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('lcom4', 1, 'foundation', 'solid', 'count',
        'Lack of cohesion (Hitz & Montazeri LCOM4) per class '
        '(architecture Sec 1.4.1 row 4).')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('fan_in', 1, 'foundation', 'solid', 'count',
        'Inbound call-graph edge count per method '
        '(architecture Sec 1.4.1 row 5).')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('fan_out', 1, 'foundation', 'solid', 'count',
        'Outbound call-graph edge count per method '
        '(architecture Sec 1.4.1 row 6).')
ON CONFLICT (metric_kind) DO NOTHING;

-- Foundation tier / base pack churn materialiser
-- (architecture Sec 1.4.1 row 12; tech-spec Sec 8.2).
INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('modification_count_in_window', 1, 'foundation', 'base', 'count',
        'Number of file modifications observed within the configured '
        'window_days for a given scope '
        '(architecture Sec 1.4.1 row 12; tech-spec Sec 8.2).')
ON CONFLICT (metric_kind) DO NOTHING;

-- Foundation tier / ingested pack external coverage rows
-- (architecture Sec 1.4.1 row 16; tech-spec Sec 4.1.1).
-- These are the ONLY canonical coverage metric_kinds; the
-- iter-1 evaluator removed `coverage_line` and
-- `coverage_branch` aliases (item 4) so a `coverage_line`
-- row MUST NOT be inserted by any downstream migration.
-- Source-of-truth constants:
--   `internal/ingest/coverage/cobertura.go` ->
--     `MetricKindCoverageLineRatio`, `MetricKindCoverageBranchRatio`,
--     `MetricVersion`.
INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('coverage_line_ratio', 1, 'foundation', 'ingested', 'ratio',
        'Per-file line-coverage ratio (lines_covered / lines_valid) '
        'ingested from an external coverage publisher (Cobertura format) '
        '(architecture Sec 1.4.1 row 16; tech-spec Sec 4.1.1).')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('coverage_branch_ratio', 1, 'foundation', 'ingested', 'ratio',
        'Per-file branch-coverage ratio (branches_covered / branches_valid) '
        'ingested from an external coverage publisher (Cobertura format) '
        '(architecture Sec 1.4.1 row 16; tech-spec Sec 4.1.1).')
ON CONFLICT (metric_kind) DO NOTHING;

COMMIT;
