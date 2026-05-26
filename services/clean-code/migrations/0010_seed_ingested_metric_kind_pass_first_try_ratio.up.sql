-- 0010_seed_ingested_metric_kind_pass_first_try_ratio.up.sql
--
-- Stage 4.3 -- seed the `pass_first_try_ratio` row in
-- `clean_code.metric_kind` so the new `ingest.test_balance`
-- verb's writes can satisfy the composite FK
-- `clean_code.metric_sample.(metric_kind, metric_version)`
-- (`migrations/0002_measurement.up.sql:348-350`).
--
-- The runtime `clean_code_metric_ingestor` role has NO
-- INSERT on `clean_code.metric_kind`
-- (`migrations/0004_roles.up.sql:350-355`), so this row
-- MUST be present before the test_balance writer attempts
-- to persist any `metric_sample` row. The migration runner
-- connects as the schema owner / superuser per the standard
-- deploy flow (mirrors `0007_seed_foundation_metric_kinds`).
--
-- Tier / pack: `pass_first_try_ratio` is the FIRST member
-- of architecture Sec 1.4.2's ingested-system-metric set
-- to land in code. By [packToTier]
-- (`internal/metric_ingestor/metric_kind_catalog.go:151-160`)
-- pack `ingested` maps to tier `foundation` (foundation-tier
-- producers persist directly; the system tier is
-- materialiser-emitted from foundation rows). The row
-- therefore lives at `(tier='foundation', pack='ingested')`.
--
-- Unit / description metadata mirror the entry under the
-- same key in `foundationCatalogMetadata`
-- (`internal/metric_ingestor/metric_kind_catalog.go:107-143`)
-- AND the version mirrors `ingestedMetricKinds`
-- (`internal/metric_ingestor/metric_kind_catalog.go:
-- ingestedMetricKinds` entry for `pass_first_try_ratio`,
-- version 1) so the composition-root
-- `metric_ingestor.VerifyMetricKindCatalog` probe wired in
-- `cmd/clean-code-metric-ingestor/main.go:
-- verifyMetricKindCatalog` finds an exact match at startup
-- -- a fresh process refuses to come up against a catalog
-- missing this row.
--
-- INSERT uses `ON CONFLICT (metric_kind) DO NOTHING` so:
--   * the migration is idempotent across re-runs, AND
--   * a Steward-curated row (richer description, different
--     version) takes precedence -- the COMMENT on
--     `clean_code.metric_kind` at
--     `0001_catalog_lifecycle.up.sql:283-286` pins the Policy
--     Steward as the canonical writer.
--
-- Down: `0010_seed_ingested_metric_kind_pass_first_try_ratio.down.sql`
-- scopes its DELETE to the EXACT `(metric_kind,
-- metric_version) = ('pass_first_try_ratio', 1)` tuple this
-- UP inserts.

BEGIN;

INSERT INTO clean_code.metric_kind
       (metric_kind, metric_version, tier, pack, unit, description_md)
VALUES ('pass_first_try_ratio', 1, 'foundation', 'ingested', 'ratio',
        'Fraction of test attempts that passed on the first try per scope; '
        'ingested via `ingest.test_balance` '
        '(architecture Sec 1.4.2; tech-spec Sec 8.5).')
ON CONFLICT (metric_kind) DO NOTHING;

COMMIT;
