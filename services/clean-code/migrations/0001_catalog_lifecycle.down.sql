-- 0001_catalog_lifecycle.down.sql
--
-- Reverse of 0001_catalog_lifecycle.up.sql. Drops the entire
-- `clean_code` schema with CASCADE so every table, ENUM type,
-- index, and constraint declared in the up half is removed in a
-- single operation. The CASCADE keeps the local-dev reset
-- deterministic: a `make migrate-down` after `make migrate-up`
-- leaves the database in the same shape it was in before the
-- first up migration ran -- nothing left over to wedge the next
-- up pass.
--
-- The matching Stage 1.2 test scenario (implementation-plan.md
-- line 79, "catalog-up-down") asserts that after this script
-- runs `\dt clean_code.*` returns zero rows. DROP SCHEMA ...
-- CASCADE is the minimal way to satisfy that contract while also
-- removing the ENUM types (which are not visible to `\dt` but
-- would otherwise persist and break a re-apply of 0001 with a
-- `type "..." already exists` error).
--
-- The `pgcrypto` extension is intentionally left in place: it is
-- enabled by `deploy/local/postgres/init/00-extensions.sql` as a
-- database-level extension (not a schema-level one), and other
-- schemas may depend on it.

BEGIN;

DROP SCHEMA IF EXISTS clean_code CASCADE;

COMMIT;
