-- 0001_catalog_lifecycle.up.sql
--
-- Stage 1.2 (implementation-plan.md "Catalog and Lifecycle schema
-- migrations"): create the single `clean_code` PostgreSQL schema
-- (tech-spec C9 / Sec 8.1.3) and the Catalog + Lifecycle sub-store
-- tables -- `repo`, `commit`, `metric_kind`, `repo_event`,
-- `scan_run` -- whose entity catalogue lives at architecture
-- Sec 5.1 / Sec 5.7.
--
-- Naming and ownership (architecture Sec 1.5 G1 / Sec 1.5.1):
--   * Every Catalog/Lifecycle row is owned by a single writer per
--     the C5 ACL split. The COMMENTs on each table below name that
--     owner explicitly so a `psql \d+ clean_code.<table>` reveals
--     the writer without cross-referencing tech-spec Sec 7.2.
--   * The `Commit.scan_status` column has TWO ownership facets:
--       - The schema-level `DEFAULT 'pending'` supplies the initial
--         value at INSERT time (the Repo Indexer omits this column
--         from its INSERT column list per arch Sec 5.1.2 line 864).
--       - Every subsequent transition (`pending -> scanning ->
--         scanned | failed`) is written ONLY by the Metric Ingestor
--         (arch Sec 1.5.1 row 1). The column COMMENT pins this.
--
-- Enum policy (architecture Sec 5.1 / Sec 5.7, e2e-scenarios
-- "@invariant" features lines 169-183): all closed-set status /
-- kind values are PostgreSQL named ENUM types so the database
-- rejects out-of-set strings at INSERT time. Adding a value
-- requires its own migration -- deliberate friction to preserve
-- the C22 closed-set invariant.
--
-- This file is the `up` half of the Catalog/Lifecycle migration
-- pair. The matching `0001_catalog_lifecycle.down.sql` reverses
-- every object created here by dropping the `clean_code` schema
-- CASCADE; the per-stage tests under
-- `internal/storage/migrate_test.go` exercise the up/down
-- round-trip end-to-end.
--
-- Required PostgreSQL extension: `pgcrypto` (for `gen_random_uuid`).
-- The local-dev stack enables it in `deploy/local/postgres/init/
-- 00-extensions.sql`; production deployments must ensure the
-- extension is loaded before running this migration.

BEGIN;

-- ---------------------------------------------------------------------------
-- Schema
-- ---------------------------------------------------------------------------

-- One physical schema for the entire CLEAN-CODE service per
-- tech-spec C9 / Sec 8.1.3. Words like `catalog`, `lifecycle`,
-- `measurement`, `policy`, `audit`, `refactor` describe the
-- logical sub-stores in architecture Sec 1.5 but never appear as
-- schema names.
CREATE SCHEMA IF NOT EXISTS clean_code;

COMMENT ON SCHEMA clean_code IS
    'CLEAN-CODE service schema (tech-spec C9 / Sec 8.1.3). '
    'Hosts every Catalog, Lifecycle, Measurement, Policy, Audit, '
    'and Refactor table for the service. Per-sub-store writer '
    'authority is enforced via per-role grants pinned in '
    'tech-spec Sec 7.2 and applied by later migrations.';

-- ---------------------------------------------------------------------------
-- ENUM types (closed sets per architecture Sec 5.1 / Sec 5.7)
-- ---------------------------------------------------------------------------

-- architecture Sec 5.1.1 line 852: Repo.mode is `embedded`
-- (default; operator pin `ast-mode-default`, Sec 1.6) or `linked`.
CREATE TYPE clean_code.repo_mode AS ENUM (
    'embedded',
    'linked'
);

-- architecture Sec 5.1.2 line 864: Commit.scan_status is the
-- canonical four-value lifecycle enum. Allowed values are
-- exactly `pending`, `scanning`, `scanned`, `failed`. The
-- enum name `commit_scan_status` is intentionally distinct
-- from `scan_run_status` (tech-spec Sec 8.7 line 1286) because
-- `Commit.scan_status` tracks per-SHA pipeline phase while
-- `ScanRun.status` tracks per-scan-execution outcome.
CREATE TYPE clean_code.commit_scan_status AS ENUM (
    'pending',
    'scanning',
    'scanned',
    'failed'
);

-- architecture Sec 5.1.3 line 872: MetricKind.tier is
-- `foundation` or `system`.
CREATE TYPE clean_code.metric_kind_tier AS ENUM (
    'foundation',
    'system'
);

-- architecture Sec 5.1.4 line 883 + e2e-scenarios invariant
-- "RepoEvent.kind labels are exactly the four past-tense names":
-- canonical labels are `registered`, `retired`, `retract_intent`,
-- `mode_changed`. The past-tense forms (NOT `register`, `retire`,
-- `change_mode`, `intent_to_retract`) are pinned because the
-- evaluator agent reads them verbatim from the audit log.
CREATE TYPE clean_code.repo_event_kind AS ENUM (
    'registered',
    'retired',
    'retract_intent',
    'mode_changed'
);

-- architecture Sec 5.7 line 1273: ScanRun.kind enumerates the
-- five scan-execution shapes that the Metric Ingestor and the
-- External Metric Ingest Webhook submit.
CREATE TYPE clean_code.scan_run_kind AS ENUM (
    'full',
    'delta',
    'external_single',
    'external_per_row',
    'retract'
);

-- architecture Sec 5.7 line 1274 + Sec 1.5.1 row 3:
-- ScanRun.sha_binding is `single` (one SHA covers the whole run;
-- `to_sha` is non-null) or `per_row` (each emitted MetricSample
-- carries its own SHA from the payload; `to_sha` is NULL).
CREATE TYPE clean_code.scan_run_sha_binding AS ENUM (
    'single',
    'per_row'
);

-- architecture Sec 5.7 line 1280 + tech-spec Sec 8.7 line 1283:
-- ScanRun.status is `running`, `succeeded`, or `failed`. Distinct
-- from `commit_scan_status` (see note above).
CREATE TYPE clean_code.scan_run_status AS ENUM (
    'running',
    'succeeded',
    'failed'
);

-- ---------------------------------------------------------------------------
-- Repo (architecture Sec 5.1.1)
-- ---------------------------------------------------------------------------

CREATE TABLE clean_code.repo (
    -- architecture Sec 5.1.1: `repo_id uuid` primary key.
    repo_id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Free-form display label shown in operator surfaces.
    display_name        text                  NOT NULL,
    -- AST adapter mode for this repo. Operator pin
    -- `ast-mode-default` (Sec 1.6) supplies the default.
    mode                clean_code.repo_mode  NOT NULL DEFAULT 'embedded',
    -- Default branch name (e.g. `main`). Carries the branch the
    -- Repo Indexer follows for periodic full scans.
    default_branch      text                  NOT NULL,
    -- Head SHA of the default branch, cached by the Repo Indexer
    -- on each push/merge webhook so the Insights surface can do a
    -- single-row read for "what's current?" without scanning
    -- `commit`. Nullable until the first scan lands. Architecture
    -- Sec 5.1.1 lists this column (text?, nullable) and pins the
    -- Repo Indexer as the sole writer; the composite index
    -- `(repo_id, default_branch_head)` is required by the Stage 1.2
    -- implementation-plan item "Add indexes required by Repo
    -- Indexer hot reads".
    default_branch_head text,
    -- Append-only registration timestamp.
    created_at          timestamptz           NOT NULL DEFAULT now()
);

COMMENT ON TABLE clean_code.repo IS
    'Catalog/Lifecycle sub-store (architecture Sec 1.5 G1, Sec 5.1.1). '
    'Writer: the Management surface (`mgmt.register_repo` / '
    '`mgmt.set_mode`) inserts and updates rows; the Repo Indexer '
    'maintains `default_branch_head` after each push/merge.';

COMMENT ON COLUMN clean_code.repo.mode IS
    'AST adapter mode (architecture Sec 5.1.1 line 852). '
    'Allowed values: embedded | linked. Default ''embedded'' '
    'matches operator pin `ast-mode-default` (architecture Sec 1.6).';

COMMENT ON COLUMN clean_code.repo.default_branch_head IS
    'Head SHA of `default_branch`, maintained by the Repo Indexer '
    'on push/merge webhooks (architecture Sec 5.1.1). The matching '
    'index `repo_default_branch_head_idx` backs the Repo Indexer '
    'hot read path (implementation-plan Stage 1.2 indexes step). '
    'Nullable until the first scan lands.';

-- Repo Indexer hot read path: lookup "is this commit the current
-- head of the default branch?" by composite (repo_id,
-- default_branch_head). repo_id alone is already covered by the
-- primary key; this composite gives the index-only-scan shape the
-- planner picks for the head-equality predicate.
CREATE INDEX repo_default_branch_head_idx
    ON clean_code.repo (repo_id, default_branch_head);

-- ---------------------------------------------------------------------------
-- Commit (architecture Sec 5.1.2)
-- ---------------------------------------------------------------------------
--
-- Identifier note: `commit` is a non-reserved PostgreSQL keyword,
-- so the schema-qualified form `clean_code.commit` is accepted
-- unquoted by the parser. We use the bare name to match the
-- canonical entity name in architecture Sec 5.1.2 and the table
-- name listed in the Stage 1.2 implementation step verbatim
-- (`tables `repo`, `commit`, `metric_kind`, `repo_event`,
-- `scan_run``). Callers must always schema-qualify references
-- (`clean_code.commit`) to avoid ambiguity with the transaction-
-- control `COMMIT` keyword.

CREATE TABLE clean_code.commit (
    -- architecture Sec 5.1.2: composite primary key (repo_id, sha)
    -- with repo_id as FK -> Repo.
    repo_id      uuid                          NOT NULL
                  REFERENCES clean_code.repo (repo_id)
                  ON DELETE RESTRICT,
    sha          text                          NOT NULL,
    -- Nullable for the very first commit of a repo (architecture
    -- Sec 5.1.2 line 862).
    parent_sha   text,
    -- Author/committer timestamp from git (arch Sec 5.1.2 line 863).
    committed_at timestamptz                   NOT NULL,
    -- See COMMENT below. The DEFAULT supplies the initial 'pending'
    -- on INSERT so the Repo Indexer omits this column from its
    -- INSERT column list (arch Sec 5.1.2 line 864). The Metric
    -- Ingestor is the only writer that subsequently transitions
    -- the value (arch Sec 1.5.1 row 1).
    scan_status  clean_code.commit_scan_status NOT NULL DEFAULT 'pending',
    PRIMARY KEY (repo_id, sha)
);

COMMENT ON TABLE clean_code.commit IS
    'Catalog/Lifecycle sub-store (architecture Sec 1.5 G1, Sec 5.1.2). '
    'Writers: the Repo Indexer is the only writer that INSERTs new '
    'rows; the Metric Ingestor is the only writer that UPDATEs '
    '`scan_status`. No other column is updated after row creation.';

COMMENT ON COLUMN clean_code.commit.scan_status IS
    'Per-SHA scan-pipeline phase (architecture Sec 5.1.2 line 864). '
    'Allowed values: pending | scanning | scanned | failed. '
    'The schema DEFAULT supplies the initial ''pending'' on INSERT '
    '(the Repo Indexer omits this column from its INSERT column '
    'list). The Metric Ingestor is the SOLE writer of subsequent '
    'transitions (pending -> scanning -> scanned | failed) per '
    'architecture Sec 1.5.1 row 1.';

-- architecture Sec 5.1.2: (repo_id, sha) is the composite PK,
-- which PostgreSQL realises as a unique B-tree index named
-- `commit_pkey`. The implementation-plan Stage 1.2 item
-- "UNIQUE(repo_id, sha) on `commit`" is satisfied by that PK
-- index; no redundant separate UNIQUE index is created.

-- ---------------------------------------------------------------------------
-- MetricKind (architecture Sec 5.1.3)
-- ---------------------------------------------------------------------------

CREATE TABLE clean_code.metric_kind (
    -- architecture Sec 5.1.3: `metric_kind` is the primary key
    -- (matches the catalogue at Sec 1.4).
    metric_kind     text                          PRIMARY KEY,
    -- Monotonic; bumped on definitional change (G2). Part of the
    -- natural key in `MetricSample` (Stage 1.3 will FK against
    -- (metric_kind, metric_version)).
    metric_version  integer                       NOT NULL,
    -- foundation or system (closed set per Sec 5.1.3 line 872).
    tier            clean_code.metric_kind_tier   NOT NULL,
    -- `base`, `solid`, `ingested`, or `system` per Sec 5.1.3 line
    -- 873. Kept as `text` (not an ENUM) at the catalog row because
    -- the enforcing enum lives on `metric_sample.pack` in Stage
    -- 1.3 -- a separate type here would duplicate that closed set.
    pack            text                          NOT NULL,
    -- E.g. `count`, `ratio`, `seconds` (Sec 5.1.3 line 874). The
    -- unit lives on the catalog row, NOT on `MetricSample`.
    unit            text                          NOT NULL,
    -- Human-readable description (Sec 5.1.3 line 875). The
    -- architecture field table does not mark this nullable
    -- (no `?` suffix), so NOT NULL is the canon-faithful shape.
    description_md  text                          NOT NULL
);

COMMENT ON TABLE clean_code.metric_kind IS
    'Catalog/Lifecycle sub-store (architecture Sec 1.5 G1, Sec 5.1.3). '
    'Writer: the Policy Steward (or catalogue-seeding migration) '
    'is the only writer; rows are append-only after a metric '
    'definition stabilises.';

COMMENT ON COLUMN clean_code.metric_kind.pack IS
    'Pack label `base | solid | ingested | system` (architecture '
    'Sec 5.1.3 line 873). The enforcing ENUM ships on '
    '`metric_sample.pack` in Stage 1.3; the column is text here '
    'to avoid duplicating the closed set.';

-- ---------------------------------------------------------------------------
-- RepoEvent (architecture Sec 5.1.4)
-- ---------------------------------------------------------------------------

CREATE TABLE clean_code.repo_event (
    -- architecture Sec 5.1.4: `event_id uuid` primary key.
    event_id     uuid                         PRIMARY KEY
                  DEFAULT gen_random_uuid(),
    repo_id      uuid                         NOT NULL
                  REFERENCES clean_code.repo (repo_id)
                  ON DELETE RESTRICT,
    -- Canonical closed-set per Sec 5.1.4 line 883. Past-tense
    -- forms (`registered`/`retired`/`mode_changed`) are pinned by
    -- the e2e-scenarios "RepoEvent.kind labels are exactly the
    -- four past-tense names" invariant; the present-tense
    -- alternatives (`register`/`retire`/`change_mode`/
    -- `intent_to_retract`) are explicitly rejected.
    kind         clean_code.repo_event_kind   NOT NULL,
    -- Per-kind payload (architecture Sec 5.1.4 line 884). Defaults
    -- to an empty object so a writer that has no kind-specific
    -- payload can INSERT without supplying this column.
    payload_json jsonb                        NOT NULL
                  DEFAULT '{}'::jsonb,
    -- Append-only.
    created_at   timestamptz                  NOT NULL DEFAULT now()
);

COMMENT ON TABLE clean_code.repo_event IS
    'Catalog/Lifecycle sub-store (architecture Sec 1.5 G1, Sec 5.1.4). '
    'Writer: the Management surface (`mgmt.register_repo`, '
    '`mgmt.retract_sample`, `mgmt.set_mode`) appends rows on every '
    'lifecycle transition. Append-only (no UPDATE / no DELETE).';

-- Repo Indexer / Insights hot read path: "show me the most recent
-- lifecycle events for repo X". The DESC ordering matches the
-- query shape.
CREATE INDEX repo_event_repo_created_idx
    ON clean_code.repo_event (repo_id, created_at DESC);

-- ---------------------------------------------------------------------------
-- ScanRun (architecture Sec 5.7)
-- ---------------------------------------------------------------------------

CREATE TABLE clean_code.scan_run (
    -- architecture Sec 5.7 line 1271: `scan_run_id uuid` PK.
    scan_run_id   uuid                              PRIMARY KEY
                   DEFAULT gen_random_uuid(),
    repo_id       uuid                              NOT NULL
                   REFERENCES clean_code.repo (repo_id)
                   ON DELETE RESTRICT,
    -- One of full | delta | external_single | external_per_row |
    -- retract (Sec 5.7 line 1273).
    kind          clean_code.scan_run_kind          NOT NULL,
    -- `single` (one SHA per run; `to_sha` non-null) or `per_row`
    -- (each emitted sample carries its own SHA; `to_sha` NULL)
    -- per Sec 5.7 line 1274. Defaults to `single` because the
    -- common foundation-tier scan path emits one SHA per run.
    sha_binding   clean_code.scan_run_sha_binding   NOT NULL
                   DEFAULT 'single',
    -- Set for `kind='delta'` (Sec 5.7 line 1275).
    from_sha      text,
    -- Set when `sha_binding='single'`; NULL when
    -- `sha_binding='per_row'` (Sec 5.7 line 1276).
    to_sha        text,
    -- sha256 of the input payload (external modes only); used for
    -- idempotency (Sec 5.7 line 1277).
    payload_hash  bytea,
    started_at    timestamptz                       NOT NULL
                   DEFAULT now(),
    ended_at      timestamptz,
    -- `running | succeeded | failed` per Sec 5.7 line 1280.
    -- Defaults to `running` because the Metric Ingestor INSERTs
    -- the row at scan start and transitions it on completion.
    status        clean_code.scan_run_status        NOT NULL
                   DEFAULT 'running'
);

COMMENT ON TABLE clean_code.scan_run IS
    'Catalog/Lifecycle sub-store (architecture Sec 1.5 G1, Sec 5.7 '
    'lines 1265-1267). Writer: the Metric Ingestor is the SOLE '
    'writer; it INSERTs the row at scan start (status=''running'') '
    'and UPDATEs `ended_at` + `status` on completion. The External '
    'Metric Ingest Webhook delegates its scan creation to the '
    'Metric Ingestor rather than writing directly.';

COMMENT ON COLUMN clean_code.scan_run.sha_binding IS
    'SHA-attribution mode (architecture Sec 5.7 line 1274). '
    'single = one SHA for the whole run (to_sha non-null). '
    'per_row = each emitted MetricSample carries its own SHA '
    'from the payload (to_sha NULL; e.g. churn / defect ingest).';

COMMENT ON COLUMN clean_code.scan_run.status IS
    'Per-scan-execution outcome (architecture Sec 5.7 line 1280). '
    'Distinct from `clean_code.commit_scan_status` -- this enum '
    'tracks the run, not the per-SHA pipeline phase.';

-- Metric Ingestor sweep + Insights hot read path: "what scan runs
-- has repo X had recently?" Implementation-plan Stage 1.2 indexes
-- step pins this composite.
CREATE INDEX scan_run_repo_started_idx
    ON clean_code.scan_run (repo_id, started_at DESC);

COMMIT;
