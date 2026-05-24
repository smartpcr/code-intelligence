-- 0005_policy_signing_keys.down.sql
--
-- Reverse of 0005_policy_signing_keys.up.sql. Drops the
-- `clean_code.policy_signing_keys` table and its index. The
-- privilege grants disappear with the table per PostgreSQL
-- semantics; the writer roles themselves persist (created by
-- 0004_roles.up.sql) and are NOT dropped here.

BEGIN;

DROP INDEX IF EXISTS clean_code.policy_signing_keys_valid_from_idx;
DROP TABLE IF EXISTS clean_code.policy_signing_keys;

COMMIT;
