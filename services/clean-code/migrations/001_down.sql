-- 001_down.sql — Tear down clean_code schema
-- Applied by `make migrate-down`.

BEGIN;

DROP TABLE IF EXISTS clean_code.policy_activation CASCADE;
DROP TABLE IF EXISTS clean_code.refactor_task CASCADE;
DROP TABLE IF EXISTS clean_code.finding CASCADE;
DROP TABLE IF EXISTS clean_code.override CASCADE;
DROP TABLE IF EXISTS clean_code.evaluation_verdict CASCADE;
DROP TABLE IF EXISTS clean_code.repo CASCADE;
DROP TYPE IF EXISTS clean_code.finding_delta_enum CASCADE;
DROP TYPE IF EXISTS clean_code.verdict_enum CASCADE;
DROP SCHEMA IF EXISTS clean_code CASCADE;

COMMIT;