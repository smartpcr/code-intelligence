-- 0010_churn_event.up.sql
--
-- Stage 4.4 (External metric ingest webhook -- ingest churn
-- verb feeds materialiser, implementation-plan.md lines
-- 410-425): create the `clean_code.churn_event` staging
-- table that the `ingest.churn` verb writes per-file
-- modification records into. The materialiser
-- (`internal/metrics/materialisers/modification_count.go`)
-- reads from this table on its next pass and is the SOLE
-- writer of `metric_kind='modification_count_in_window'`
-- (Stage 2.6); the verb itself writes ZERO `metric_sample`
-- rows directly (iter 1 evaluator item 4).
--
-- # Why a separate staging table
--
-- The `ingest.churn` payload arrives via the External Metric
-- Ingest Webhook with per-row SHAs (architecture Sec 4.4
-- line 781). The materialiser groups rows per scope and
-- emits one `metric_sample(metric_kind='modification_count_in_window',
-- pack='base', source='computed')` row with
-- `attrs_json.provenance='ingested'` per tech-spec Sec 4.1.1
-- lines 287-291 and Sec 4.11 lines 444-454. The materialiser
-- therefore needs to read MANY churn records (each row is a
-- per-file-per-commit touch) to emit ONE per-scope sample,
-- and to do so on a different cadence from the webhook
-- delivery. A durable staging table is the natural seam:
--
--   - Verb writes go into `churn_event` (this table) inside
--     the verb's request scope (fast, bounded, no materialiser
--     coupling).
--   - Materialiser reads later from `churn_event` (its own
--     ScanRun, its own transaction).
--
-- Without the seam, the verb would have to either run the
-- materialiser inline (forcing a metric_sample write inside
-- the verb's call stack -- violating "verb writes ZERO
-- metric_sample rows" per the brief) or buffer in memory
-- (losing data across restarts).
--
-- # Append-only contract
--
-- Rows in `churn_event` are append-only. The materialiser
-- never UPDATEs or DELETEs them; partition-based retention
-- (a future stage) is the only legitimate removal path. The
-- role grants below REVOKE UPDATE+DELETE from PUBLIC and
-- from the writer role, mirroring the `metric_sample`
-- immutability contract from migration 0004.
--
-- # Operator notes
--
-- - This migration runs inside a single transaction (no
--   CONCURRENTLY clauses) so a partial failure rolls back
--   cleanly. The table is new, so there is no pre-existing
--   data to worry about.
-- - The `scan_run_id` FK references `clean_code.scan_run`;
--   a churn payload that arrives without an open scan_run
--   row would be a Router-layer bug surfaced at INSERT time
--   with an SQLSTATE 23503 (foreign_key_violation).
-- - The `repo_id` FK references `clean_code.repo` with
--   ON DELETE RESTRICT so a repo cannot be retired while its
--   churn events still anchor an unmaterialised window.

BEGIN;

CREATE TABLE clean_code.churn_event (
    -- Per-row primary key minted by the verb at staging time.
    -- UUIDv7 in the application layer; `gen_random_uuid()` is
    -- the DB default for callers that do not supply one.
    churn_event_id      uuid        PRIMARY KEY
                                    DEFAULT gen_random_uuid(),
    -- Parent ScanRun the verb opened (verb='churn',
    -- kind='external_per_row', sha_binding='per_row',
    -- to_sha IS NULL) per architecture Sec 4.4 and
    -- e2e-scenarios.md lines 658-664. The FK pins the
    -- writer-ownership chain: every staged row is bound to
    -- the durable scan_run that the Router claimed before
    -- dispatching the verb.
    scan_run_id         uuid        NOT NULL
                                    REFERENCES clean_code.scan_run (scan_run_id)
                                    ON DELETE RESTRICT,
    -- Parent repo. Duplicated from `scan_run.repo_id` so the
    -- materialiser's per-repo read path is index-friendly
    -- without a join.
    repo_id             uuid        NOT NULL
                                    REFERENCES clean_code.repo (repo_id)
                                    ON DELETE RESTRICT,
    -- Per-row commit SHA (per-row binding, architecture Sec
    -- 4.4 line 781). 40-char hex; the CHECK rejects malformed
    -- inputs that slipped past the application-layer
    -- validator (`churn.shaRegex`).
    sha                 text        NOT NULL,
    -- Repo-relative path of the touched file. The
    -- materialiser resolves `(repo_id, file_path)` to a
    -- durable `scope_id` via the `scope_binding` table at
    -- read time; the verb does NOT do scope resolution at
    -- stage time (so a payload referencing a path the AST
    -- adapter has not yet bound is still stageable).
    file_path           text        NOT NULL,
    -- Commit modification timestamp in UTC. The
    -- materialiser's window math (`now - window_days * 24h`)
    -- is the only consumer; values before 1970 or after now+1d
    -- are accepted at the DB layer (the materialiser applies
    -- its own clock-skew defence per
    -- `modification_count.go`'s window guard).
    modified_at         timestamptz NOT NULL,
    -- Commit author identity (e.g. `alice@example.com`).
    -- Reserved for the `knowledge_index` (system-tier row 7)
    -- which is not built at Stage 4.4; nullable so a publisher
    -- without author data does not have to supply an empty
    -- string. The `modification_count_in_window` materialiser
    -- ignores this column.
    author              text,
    -- Zero-based row index inside the source payload. Lets the
    -- materialiser (and ops debug tools) reconstruct the
    -- original payload order without an extra timestamp
    -- resolution. UNIQUE per scan_run_id so duplicate inserts
    -- for the same payload row surface as 23505 instead of
    -- silently double-counting.
    payload_row_index   integer     NOT NULL,
    -- Stage-time clock. Pinned at INSERT time so operators
    -- can correlate a staged row with the webhook delivery
    -- audit trail.
    created_at          timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT churn_event_sha_40_hex CHECK (
        sha ~ '^[0-9a-fA-F]{40}$'
    ),
    CONSTRAINT churn_event_file_path_nonempty CHECK (
        length(file_path) > 0
    ),
    CONSTRAINT churn_event_payload_row_index_positive CHECK (
        payload_row_index >= 0
    ),
    -- Per-scan_run row identity: a publisher retrying the
    -- SAME payload row inside the SAME scan_run claim hits
    -- 23505 instead of duplicating the row. Combined with
    -- the (verb, payload_hash) idempotency anchor on
    -- scan_run (migration 0009), this enforces the
    -- "exactly N events per N-row payload" invariant.
    CONSTRAINT churn_event_scan_run_row_uniq UNIQUE (scan_run_id, payload_row_index)
);

COMMENT ON TABLE clean_code.churn_event IS
    'Staging table for the `ingest.churn` external webhook '
    '(architecture Sec 4.4, implementation-plan Stage 4.4 '
    'lines 410-425). Append-only per-file modification '
    'records. The materialiser at '
    '`internal/metrics/materialisers/modification_count.go` '
    'is the sole reader -- it groups rows by (repo, '
    'scope) within the configured window_days (tech-spec '
    'Sec 8.2 default 90) and emits one '
    'metric_kind=''modification_count_in_window'' sample with '
    'pack=''base'', source=''computed'', '
    'attrs_json.provenance=''ingested'' per tech-spec Sec '
    '4.1.1 lines 287-291. The `ingest.churn` verb writes '
    'ZERO `metric_sample` rows directly.';

COMMENT ON COLUMN clean_code.churn_event.scan_run_id IS
    'Parent ScanRun opened by the External Metric Ingest '
    'Webhook Router with (verb=''churn'', '
    'kind=''external_per_row'', sha_binding=''per_row'', '
    'to_sha IS NULL, payload_hash=sha256(body)) per '
    'architecture Sec 4.4 and e2e-scenarios.md lines '
    '658-664. The (verb, payload_hash) partial unique index '
    'on scan_run (migration 0009) is the AUTHORITATIVE '
    'idempotency anchor: the Router short-circuits durable '
    'replays at scan_run-open time, BEFORE dispatching to '
    'the verb, so a replayed payload NEVER reaches the '
    'churn_event INSERT path. The `churn_event_scan_run_row_uniq` '
    'defence-in-depth constraint below catches a '
    'DIFFERENT failure mode -- an in-process re-fire of the '
    'verb against an already-claimed (still-in-flight) '
    'scan_run -- and is intentionally noisier than the '
    'silent-success the Router gives a durable replay.';

COMMENT ON COLUMN clean_code.churn_event.sha IS
    'Per-row commit SHA (architecture Sec 4.4 line 781 -- '
    '"each row has its own SHA"). 40-char hex; the '
    '`churn_event_sha_40_hex` CHECK rejects malformed '
    'inputs that slipped past the application-layer '
    'validator (`internal/ingest/churn.shaRegex`).';

COMMENT ON COLUMN clean_code.churn_event.payload_row_index IS
    'Zero-based row index inside the source payload, pinned '
    'at stage time. Lets the materialiser (and ops debug '
    'tools) reconstruct the original payload order without '
    'a separate timestamp resolution. The '
    '`churn_event_scan_run_row_uniq` unique constraint '
    'rejects a retry of the same payload row inside the '
    'same scan_run claim with 23505.';

-- ---------------------------------------------------------------------------
-- Indexes
-- ---------------------------------------------------------------------------

-- Materialiser hot read path: "give me every churn record
-- for repo X within the last window_days". The
-- `modification_count_in_window` materialiser uses this
-- index to bound its window scan and to sort rows in
-- descending modified_at order so the LATEST in-window SHA
-- per scope falls out without an extra sort.
CREATE INDEX churn_event_repo_modified_idx
    ON clean_code.churn_event (repo_id, modified_at DESC);

-- Per-ScanRun audit lookup: "show me the staged rows for
-- this scan_run_id" -- used by ops dashboards and by the
-- handler_test.go scenario asserting "exactly N events for
-- the verb's call stack".
CREATE INDEX churn_event_scan_run_idx
    ON clean_code.churn_event (scan_run_id);

-- Materialiser per-scope lookup: speeds the
-- `(repo_id, file_path)` -> `scope_id` resolution loop that
-- runs once per emitted draft. NOT unique -- the same file
-- may legitimately appear in many rows (one per touching
-- commit).
CREATE INDEX churn_event_repo_file_idx
    ON clean_code.churn_event (repo_id, file_path);

-- ---------------------------------------------------------------------------
-- Role grants (matching the migration 0004 pattern)
-- ---------------------------------------------------------------------------

-- The webhook process runs as `clean_code_metric_ingestor`
-- (the same role that writes scan_run and metric_sample);
-- it is the writer of `churn_event`. Reads are granted to
-- the same role so the materialiser (which also runs under
-- this role) can scan the staging table at materialisation
-- time. No other role writes here.
GRANT INSERT, SELECT ON clean_code.churn_event TO clean_code_metric_ingestor;

-- Append-only contract (mirrors the metric_sample
-- immutability pattern from migration 0004 lines 413-415).
-- Partition-drop is the only legitimate removal path; it is
-- a DDL operation not gated by these grants.
REVOKE UPDATE, DELETE ON clean_code.churn_event FROM PUBLIC, clean_code_metric_ingestor;

COMMIT;
