-- 0002_measurement.down.sql
--
-- Reverse of 0002_measurement.up.sql. Drops every Measurement
-- table, every Measurement-specific ENUM type, and the
-- `metric_kind_natural_key_uniq` UNIQUE constraint that the up
-- half added to the 0001-owned `metric_kind` table.
--
-- Drop order is REVERSE dependency order: pointer + retraction
-- before `metric_sample` (which they FK), `metric_sample` before
-- `scope_binding` (which it FKs), then the aggregator-owned
-- snapshot tables (which only FK Catalog-tier tables and are
-- safe to drop in any order amongst themselves).
--
-- `DROP TABLE` (no CASCADE needed once the dependent tables are
-- already gone) preserves the "down only undoes its own up"
-- contract: catalog/lifecycle tables from 0001 are untouched. The
-- 0001-down (schema-CASCADE removal) still works as the catch-
-- all if 0002-down is skipped during a full reset.

BEGIN;

-- Aggregator-owned derivative views first (no FK chain amongst them).
DROP TABLE IF EXISTS clean_code.portfolio_snapshot;
DROP TABLE IF EXISTS clean_code.cross_repo_percentile;
DROP TABLE IF EXISTS clean_code.repo_metric_snapshot;

-- Active-row side relation before `metric_sample` (it FKs the
-- sample row's full quintuple).
DROP INDEX IF EXISTS clean_code.metric_sample_active_sample_id_uniq;
DROP TABLE IF EXISTS clean_code.metric_sample_active;

-- Retraction log before `metric_sample` (it FKs `sample_id`).
DROP TABLE IF EXISTS clean_code.metric_retraction;

-- Sample row before `scope_binding` (it FKs `scope_id`).
DROP INDEX IF EXISTS clean_code.metric_sample_repo_sha_idx;
DROP TABLE IF EXISTS clean_code.metric_sample;

DROP TABLE IF EXISTS clean_code.scope_binding;

-- Reverse of the ALTER TABLE in the up half. The constraint must
-- be dropped BEFORE the ENUMs in case of any incidental reference.
ALTER TABLE clean_code.metric_kind
    DROP CONSTRAINT IF EXISTS metric_kind_natural_key_uniq;

-- ENUM types last (anything that referenced them is gone by now).
DROP TYPE IF EXISTS clean_code.scope_kind;
DROP TYPE IF EXISTS clean_code.degraded_reason;
DROP TYPE IF EXISTS clean_code.metric_sample_source;
DROP TYPE IF EXISTS clean_code.metric_sample_pack;

COMMIT;
