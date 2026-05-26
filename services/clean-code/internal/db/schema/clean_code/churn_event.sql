-- services/clean-code/internal/db/schema/clean_code/churn_event.sql
--
-- Canonical reference for the `clean_code.churn_event` staging
-- table created by Stage 4.4 (ingest churn verb feeds
-- materialiser; implementation-plan.md lines 410-425). The
-- AUTHORITATIVE SQL lives in
-- `services/clean-code/migrations/0010_churn_event.up.sql` --
-- that file is what `psql -f` actually applies. This file is
-- a docs-friendly mirror that the brief's "target files" list
-- explicitly names, kept so a reader who lands on the schema
-- folder can find the canonical shape without scrolling
-- through migration-specific clauses (BEGIN/COMMIT,
-- CONCURRENTLY guards, role grants).
--
-- Source of truth pins:
--   - Architecture Sec 4.4 lines 778-790  -- per-row-SHA contract
--   - Implementation-plan Stage 4.4 lines 410-425
--   - Tech-spec Sec 4.1.1 lines 287-291   -- materialiser is the
--                                            sole writer of
--                                            modification_count_in_window
--   - E2E scenarios.md lines 658-664      -- "verb writes ZERO
--                                            metric_sample rows"
--
-- Append-only contract: rows in `churn_event` are NEVER
-- updated or deleted by the application layer. The role
-- grants in `0010_churn_event.up.sql` REVOKE UPDATE/DELETE
-- from PUBLIC and from the `clean_code_metric_ingestor`
-- writer role so a regression in the writer cannot mutate
-- staged rows. Retention is partition-drop only (a future
-- stage).

CREATE TABLE clean_code.churn_event (
    churn_event_id      uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    scan_run_id         uuid        NOT NULL REFERENCES clean_code.scan_run (scan_run_id) ON DELETE RESTRICT,
    repo_id             uuid        NOT NULL REFERENCES clean_code.repo (repo_id)         ON DELETE RESTRICT,
    sha                 text        NOT NULL,
    file_path           text        NOT NULL,
    modified_at         timestamptz NOT NULL,
    author              text,
    payload_row_index   integer     NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT churn_event_sha_40_hex             CHECK (sha ~ '^[0-9a-fA-F]{40}$'),
    CONSTRAINT churn_event_file_path_nonempty     CHECK (length(file_path) > 0),
    CONSTRAINT churn_event_payload_row_index_positive CHECK (payload_row_index >= 0),
    CONSTRAINT churn_event_scan_run_row_uniq      UNIQUE (scan_run_id, payload_row_index)
);

CREATE INDEX churn_event_repo_modified_idx ON clean_code.churn_event (repo_id, modified_at DESC);
CREATE INDEX churn_event_scan_run_idx      ON clean_code.churn_event (scan_run_id);
CREATE INDEX churn_event_repo_file_idx     ON clean_code.churn_event (repo_id, file_path);

-- Writer role: `clean_code_metric_ingestor` (the webhook
-- process runs as this role); reader is the same role (the
-- materialiser scans `churn_event` at materialisation time).
-- No other role writes here.
--
-- GRANT INSERT, SELECT ON clean_code.churn_event TO clean_code_metric_ingestor;
-- REVOKE UPDATE, DELETE ON clean_code.churn_event FROM PUBLIC, clean_code_metric_ingestor;
