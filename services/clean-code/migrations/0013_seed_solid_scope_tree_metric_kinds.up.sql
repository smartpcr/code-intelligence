-- 0013_seed_solid_scope_tree_metric_kinds.up.sql
--
-- Stage 2.2 -- SOLID-pack scope-tree foundation seeds.
--
-- Adds the three foundation-tier `metric_kind` catalog rows
-- that the Stage 2.2 parse-and-recipe-fanout pipeline now
-- emits:
--
--   * `interface_width`           (architecture Sec 1.4.1 row 8 -- ISP input)
--   * `depth_of_inheritance`      (architecture Sec 1.4.1 row 9 -- LSP input)
--   * `coupling_between_objects`  (architecture Sec 1.4.1 row 10 -- DIP input)
--
-- All three are produced by per-file recipes registered in
-- `internal/metrics/recipes/registry.go::DefaultRegistry`
-- (Stage 2.2 added the three recipe files
-- `interface_width.go`, `depth_of_inheritance.go`, and
-- `coupling_between_objects.go`). Without these rows the
-- runtime `clean-code-metric-ingestor` startup-gate
-- `verifyMetricKindCatalog`
-- (`cmd/clean-code-metric-ingestor/main.go:369-373`) -- which
-- derives the expected catalog from `recipes.DefaultRegistry()`
-- via `metric_ingestor.MetricKindCatalogRowsForRegistry` --
-- refuses to come up against a fresh schema, blocking the
-- composition root before the first sample is persisted.
--
-- These rows are seeded HERE (rather than appended to
-- `0007_seed_foundation_metric_kinds.up.sql`) because Forge's
-- migration runner keys by version number and rejects
-- mutations to already-applied numeric prefixes; an additive
-- 0013 entry is the canonical extension shape, mirroring the
-- earlier additive `0012_seed_ingested_metric_kind_pass_first_try_ratio`
-- pair.
--
-- The runtime `clean_code_metric_ingestor` role has NO
-- INSERT on `clean_code.metric_kind`
-- (`0004_roles.up.sql:350-355`); INSERT is granted ONLY to
-- `clean_code_policy_steward`. This migration is executed by
-- the schema owner / superuser (the standard production
-- deploy flow), mirroring `0007_seed_foundation_metric_kinds`.
--
-- Tier / pack: per `packToTier`
-- (`internal/metric_ingestor/metric_kind_catalog.go:151-160`)
-- pack `solid` maps to tier `foundation`. All three rows
-- therefore live at `(tier='foundation', pack='solid')`.
--
-- Unit / description metadata mirrors the entries under the
-- same keys in `foundationCatalogMetadata`
-- (`internal/metric_ingestor/metric_kind_catalog.go:136-147`)
-- AND `metric_version=1` mirrors the recipe `Version()`
-- constants
-- (`internal/metrics/recipes/interface_width.go::interfaceWidthVersion`,
--  `internal/metrics/recipes/depth_of_inheritance.go::depthOfInheritanceVersion`,
--  `internal/metrics/recipes/coupling_between_objects.go::couplingBetweenObjectsVersion`)
-- so the composition-root
-- `metric_ingestor.VerifyMetricKindCatalog` probe finds an
-- exact match at startup.
--
-- INSERT uses `ON CONFLICT (metric_kind) DO NOTHING` so:
--   * the migration is idempotent across re-runs, AND
--   * a Steward-curated row (richer description, different
--     metric_version) takes precedence -- the COMMENT on
--     `clean_code.metric_kind` at
--     `0001_catalog_lifecycle.up.sql:283-286` pins the Policy
--     Steward as the canonical writer.
--
-- Down: `0013_seed_solid_scope_tree_metric_kinds.down.sql`
-- scopes its DELETE to the EXACT `(metric_kind,
-- metric_version)` tuples this UP inserts (three kinds at
-- `metric_version = 1`).

BEGIN;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('interface_width', 1, 'foundation', 'solid', 'count',
        'Method count of a class/interface''s exposed surface; '
        'drives ISP rule (architecture Sec 1.4.1 row 8).')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('depth_of_inheritance', 1, 'foundation', 'solid', 'count',
        'Length of the per-file inheritance chain (extends/embeds) '
        'from a class to its deepest ancestor; drives LSP / '
        'composition-over-inheritance rules '
        '(architecture Sec 1.4.1 row 9).')
ON CONFLICT (metric_kind) DO NOTHING;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('coupling_between_objects', 1, 'foundation', 'solid', 'count',
        'Count of distinct external classes a class depends on via '
        'extends/implements/embeds/imports/calls/field-access edges; '
        'drives DIP / decoupling rules '
        '(architecture Sec 1.4.1 row 10).')
ON CONFLICT (metric_kind) DO NOTHING;

COMMIT;
