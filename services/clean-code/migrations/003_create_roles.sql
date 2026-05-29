-- 003_create_roles.sql
-- Creates the database roles used by the Audit WAL subsystem.
-- The reconciler role can only write to the three Audit tables
-- (evaluation_run, evaluation_verdict, finding) and read/write wal_inbox.

BEGIN;

-- Create roles if they don't already exist.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'clean_code_wal_reconciler') THEN
        CREATE ROLE clean_code_wal_reconciler LOGIN PASSWORD 'e2e_test_password';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'clean_code_evaluator') THEN
        CREATE ROLE clean_code_evaluator LOGIN PASSWORD 'e2e_test_password';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'clean_code_solid_batch') THEN
        CREATE ROLE clean_code_solid_batch LOGIN PASSWORD 'e2e_test_password';
    END IF;
END
$$;

-- Reconciler role: Audit tables + wal_inbox only.
GRANT SELECT, INSERT, UPDATE ON evaluation_run TO clean_code_wal_reconciler;
GRANT SELECT, INSERT, UPDATE ON evaluation_verdict TO clean_code_wal_reconciler;
GRANT SELECT, INSERT, UPDATE ON finding TO clean_code_wal_reconciler;
GRANT SELECT, INSERT, UPDATE ON wal_inbox TO clean_code_wal_reconciler;
GRANT USAGE, SELECT ON SEQUENCE wal_inbox_id_seq TO clean_code_wal_reconciler;

-- Explicitly REVOKE access to non-audit tables so the permission test works.
REVOKE ALL ON metric_sample FROM clean_code_wal_reconciler;

-- Evaluator role: broader access (used by evaluator service).
GRANT SELECT, INSERT, UPDATE ON evaluation_run TO clean_code_evaluator;
GRANT SELECT, INSERT, UPDATE ON evaluation_verdict TO clean_code_evaluator;
GRANT SELECT, INSERT, UPDATE ON finding TO clean_code_evaluator;
GRANT SELECT, INSERT, UPDATE ON wal_inbox TO clean_code_evaluator;
GRANT USAGE, SELECT ON SEQUENCE wal_inbox_id_seq TO clean_code_evaluator;
GRANT SELECT, INSERT, UPDATE ON metric_sample TO clean_code_evaluator;

-- Solid batch role.
GRANT SELECT, INSERT, UPDATE ON evaluation_run TO clean_code_solid_batch;
GRANT SELECT, INSERT, UPDATE ON evaluation_verdict TO clean_code_solid_batch;
GRANT SELECT, INSERT, UPDATE ON finding TO clean_code_solid_batch;
GRANT SELECT, INSERT ON metric_sample TO clean_code_solid_batch;

-- Allow reconciler to execute the replay function.
GRANT EXECUTE ON FUNCTION reconciler_replay() TO clean_code_wal_reconciler;

COMMIT;