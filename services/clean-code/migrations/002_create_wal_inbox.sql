-- 002_create_wal_inbox.sql
-- Creates the wal_inbox table used by the reconciler to discover pending
-- WAL frames that need replaying into audit tables.

BEGIN;

CREATE TABLE IF NOT EXISTS wal_inbox (
    id           BIGSERIAL PRIMARY KEY,
    target_table TEXT NOT NULL,
    row_id       TEXT NOT NULL,
    payload      JSONB NOT NULL,
    replayed     BOOLEAN NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    replayed_at  TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_wal_inbox_pending
    ON wal_inbox (target_table) WHERE replayed = FALSE;

-- reconciler_replay(): processes all pending wal_inbox rows, inserting
-- each payload into its target audit table and marking the frame as
-- replayed.
CREATE OR REPLACE FUNCTION reconciler_replay() RETURNS VOID AS $$
DECLARE
    frame RECORD;
BEGIN
    FOR frame IN
        SELECT id, target_table, row_id, payload
        FROM wal_inbox
        WHERE replayed = FALSE
        ORDER BY id
        FOR UPDATE SKIP LOCKED
    LOOP
        -- Insert into evaluation_run
        IF frame.target_table = 'evaluation_run' THEN
            INSERT INTO evaluation_run (id, caller, status, detail, created_at)
            VALUES (
                frame.payload->>'id',
                COALESCE(frame.payload->>'caller', 'reconciler'),
                COALESCE(frame.payload->>'status', 'pending'),
                frame.payload->>'detail',
                NOW()
            )
            ON CONFLICT (id) DO NOTHING;

        -- Insert into evaluation_verdict
        ELSIF frame.target_table = 'evaluation_verdict' THEN
            INSERT INTO evaluation_verdict (id, run_id, verdict, detail, created_at)
            VALUES (
                frame.payload->>'id',
                COALESCE(frame.payload->>'run_id', ''),
                COALESCE(frame.payload->>'verdict', 'unknown'),
                frame.payload->>'detail',
                NOW()
            )
            ON CONFLICT (id) DO NOTHING;

        -- Insert into finding
        ELSIF frame.target_table = 'finding' THEN
            INSERT INTO finding (id, run_id, rule_id, severity, message, detail, created_at)
            VALUES (
                frame.payload->>'id',
                COALESCE(frame.payload->>'run_id', ''),
                COALESCE(frame.payload->>'rule_id', ''),
                COALESCE(frame.payload->>'severity', 'info'),
                frame.payload->>'message',
                frame.payload->>'detail',
                NOW()
            )
            ON CONFLICT (id) DO NOTHING;

        ELSE
            RAISE WARNING 'unknown target_table: %', frame.target_table;
            CONTINUE;  -- skip marking as replayed so the row stays pending
        END IF;

        -- Mark frame as replayed.
        UPDATE wal_inbox SET replayed = TRUE, replayed_at = NOW() WHERE id = frame.id;
    END LOOP;
END;
$$ LANGUAGE plpgsql;

COMMIT;
