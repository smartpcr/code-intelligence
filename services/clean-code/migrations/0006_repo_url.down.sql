-- 0006_repo_url.down.sql
--
-- Reverse of 0006_repo_url.up.sql. Drops the `repo_url` column
-- on `clean_code.repo`; the column-level grants disappear with
-- the column per PostgreSQL semantics.
--
-- Caveat: any `clean_code.scope_binding` rows whose
-- `canonical_signature` embeds the real URL stamp (vs the
-- synthetic `clean-code-repo:<repoID>` fallback) will continue
-- to exist after this DOWN runs -- the column drop does NOT
-- rewrite history. Running this DOWN after metric_sample rows
-- have landed with URL-stamped signatures effectively decouples
-- the read side (lookup returns "not found") from the persisted
-- canonical signatures (URL-stamped). The next scan would mint
-- DIFFERENT scope_id rows for the same logical scopes, breaking
-- G2 stability for the affected repos. Operators should treat
-- this DOWN as a development-only escape hatch, not a recovery
-- step for a populated database.

BEGIN;

-- Drop the WRITE-ONCE trigger first (PostgreSQL would
-- cascade the trigger when the column is dropped, but
-- naming both keeps the rollback idempotent and makes the
-- intent explicit).
DROP TRIGGER IF EXISTS tg_repo_url_write_once ON clean_code.repo;
DROP FUNCTION IF EXISTS clean_code.tg_repo_url_write_once();

ALTER TABLE clean_code.repo
    DROP COLUMN IF EXISTS repo_url;

COMMIT;
