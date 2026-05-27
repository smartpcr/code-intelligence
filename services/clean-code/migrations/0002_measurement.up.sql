-- 0002_measurement.up.sql
--
-- Stage 1.3 (implementation-plan.md "Measurement schema and active
-- row index"): create the Measurement sub-store tables in the
-- shared `clean_code` PostgreSQL schema (tech-spec C9 / Sec 8.1.3).
-- Tables created here: `scope_binding`, `metric_sample`,
-- `metric_retraction`, `metric_sample_active`, `repo_metric_snapshot`,
-- `cross_repo_percentile`, and `portfolio_snapshot`. Their entity
-- catalogue lives at architecture Sec 5.2.1 -- 5.2.6; tech-spec
-- Sec 7.1.b carries the canonical DDL.
--
-- Identity contract (architecture G2 + Sec 5.2.1 lines 991-1003):
-- at any instant at most one ACTIVE row exists per
-- `(repo_id, sha, scope_id, metric_kind, metric_version)` quintuple,
-- where a row is "active" iff no `MetricRetraction` row references
-- its `sample_id`. PostgreSQL prohibits a partial unique index whose
-- predicate is `WHERE sample_id NOT IN (SELECT sample_id FROM
-- metric_retraction)` (subqueries are forbidden in index
-- predicates). Tech-spec Sec 7.1.b lines 1041-1068 / Sec 10A lines
-- 1659-1675 pin the implementation as a SIDE RELATION
-- `metric_sample_active(repo_id, sha, scope_id, metric_kind,
-- metric_version, sample_id)` whose own PRIMARY KEY is the
-- quintuple and whose `sample_id` carries a separate UNIQUE INDEX.
-- The semantic guarantee (at most one active row per quintuple) is
-- identical to architecture's mandated partial unique index;
-- the index physically sits on the side relation rather than on
-- `metric_sample` because `metric_sample` rows are immutable per
-- G3 / C2 (no mutable `is_retracted` flag column is permitted
-- as an alternative predicate carrier).
--
-- Row immutability (G3 / C2): `metric_sample` is append-only. The
-- role grants added by Stage 1.5 (`0004_roles.up.sql`) REVOKE
-- UPDATE and DELETE from every writer role so the database
-- enforces immutability at the ACL layer in addition to the
-- "only INSERTs are issued" application-layer rule. Corrections
-- to a sample are issued by APPENDING a `metric_retraction` row
-- referencing the prior `sample_id` (tombstone) and APPENDING a
-- fresh `metric_sample` row with a new `sample_id`; the active
-- pointer is then atomically re-pointed at the new sample via
-- `INSERT ... ON CONFLICT ... DO UPDATE` on `metric_sample_active`.
--
-- Partitioning (tech-spec Sec 7.1.b partitioning-deferral pin,
-- Sec 8.1.4): the long-term shape pins `PARTITION BY LIST
-- (metric_kind)` with monthly sub-partitions on
-- `sample_date_bucket`. Three independent PostgreSQL constraints
-- prevent that shape from coexisting with the other Sec 7.1.b
-- pins in a single CREATE TABLE:
--   (1) `PRIMARY KEY (sample_id)` alone is not legal on a table
--       partitioned by `metric_kind` -- a unique constraint on
--       a partitioned table MUST include every partition-key
--       column, so the PK becomes `(sample_id, metric_kind)`.
--   (2) `metric_retraction.sample_id REFERENCES metric_sample
--       (sample_id)` (tech-spec line 1096) and
--       `metric_sample_active.sample_id REFERENCES metric_sample
--       (sample_id)` (tech-spec line 1114) both require UNIQUE
--       on `sample_id` alone -- contradicts (1).
--   (3) PG 16 does not allow a STORED GENERATED column to be
--       a partition key by itself; the long-term sub-partition
--       scheme on `sample_date_bucket` therefore needs an
--       ordinary writer-supplied column or a different
--       sub-partition strategy.
-- Stage 1.3 accordingly DEFERS `PARTITION BY LIST` to a future
-- operational migration. The deferral is recorded as a first-
-- class pin in the tech-spec (Sec 7.1.b partitioning-deferral
-- pin, Sec 8.1.4 deferral footnote, decision row 3) so the
-- migration matches the accepted plan. The deferral does not
-- affect identity / immutability / active-row semantics, which
-- are enforced at the row + side-relation layer here. The
-- future partition migration is a rewrite-cost transition,
-- NOT a metadata-only one; documented to set operator
-- expectations.
--
-- `sample_date_bucket` (tech-spec line 1087) IS included in
-- this migration -- it is the monthly bucket the future
-- partition migration will key sub-partitions on, and Insights
-- can use it today for time-windowed reads without paying for
-- a re-derivation. The expression uses `(created_at AT TIME
-- ZONE 'UTC')` to coerce the timestamptz to a timezone-
-- independent timestamp; `date_trunc('month', timestamp)` is
-- IMMUTABLE (whereas `date_trunc('month', timestamptz)` is
-- STABLE, which PostgreSQL rejects inside GENERATED ALWAYS AS).
-- Bucket semantics are deliberately UTC -- the bucket boundary
-- is the first of the month at 00:00 UTC regardless of the
-- writer's local timezone. This matches the cross-repo
-- aggregate read pattern (architecture Sec 3.10) which is
-- inherently global and tz-agnostic.
--
-- This file is the `up` half of the Measurement migration pair.
-- The matching `0002_measurement.down.sql` reverses every object
-- created here in reverse dependency order.

BEGIN;

-- ---------------------------------------------------------------------------
-- ENUM types (closed sets per architecture Sec 5.2 / Sec 1.4.1)
-- ---------------------------------------------------------------------------

-- architecture Sec 5.2.1 line 901 + Sec 1.4.1: MetricSample.pack is
-- `base | solid | system | ingested`. The Compute Engine writes
-- `base`/`solid` rows, the External Metric Ingest Webhook writes
-- `ingested` rows, and the Cross-Repo Aggregator is the ONLY writer
-- that produces `system` rows (architecture Sec 3.10 step 4).
CREATE TYPE clean_code.metric_sample_pack AS ENUM (
    'base',
    'solid',
    'system',
    'ingested'
);

-- architecture Sec 5.2.1 line 902: MetricSample.source is `computed`
-- (Compute Engine, Sec 3.4), `ingested` (External Metric Ingest
-- Webhook, Sec 3.12), or `derived` (Cross-Repo Aggregator, Sec 3.10
-- step 4). The derived path is the ONLY path that produces
-- `pack='system'` rows.
CREATE TYPE clean_code.metric_sample_source AS ENUM (
    'computed',
    'derived',
    'ingested'
);

-- architecture Sec 8.2 / tech-spec C21 line 813-818: the closed set
-- of `degraded_reason` values shared by both `MetricSample.degraded_reason`
-- (Sec 5.2.1 line 904) and (in a later migration) `EvaluationVerdict
-- .degraded_reason` (Sec 5.4.3). Adding a value requires its own
-- migration -- deliberate friction per C22. The enum is named
-- generically (`degraded_reason`) rather than table-scoped so
-- `0003_policy_audit_refactor.up.sql` can reuse it on
-- `evaluation_verdict` without duplicating the closed set.
CREATE TYPE clean_code.degraded_reason AS ENUM (
    'xrepo_edges_unavailable',
    'samples_pending',
    'policy_signature_invalid',
    'percentile_stale'
);

-- architecture Sec 5.2.3 line 1046: ScopeBinding.scope_kind is the
-- canonical scope-discriminator enum used across ScopeBinding,
-- RepoMetricSnapshot (Sec 5.2.4), CrossRepoPercentile (Sec 5.2.5),
-- PortfolioSnapshot (Sec 5.2.6), and (in a later migration) Override
-- .scope_filter / HotSpot (Sec 5.3.6 / Sec 5.5.1). Defining it once
-- here keeps the closed set in a single place.
CREATE TYPE clean_code.scope_kind AS ENUM (
    'repo',
    'package',
    'file',
    'class',
    'interface',
    'method',
    'block'
);

-- ---------------------------------------------------------------------------
-- Extend the 0001 `metric_kind` catalogue with the composite UNIQUE
-- constraint that `metric_sample` will FK against.
-- ---------------------------------------------------------------------------
--
-- 0001 line 263-265 explicitly anticipates this Stage 1.3 follow-up
-- ("Stage 1.3 will FK against (metric_kind, metric_version)"). The
-- PK on `metric_kind` already makes the column unique, so adding
-- `UNIQUE (metric_kind, metric_version)` is logically redundant on
-- the data side -- BUT PostgreSQL requires an explicit unique
-- constraint or unique index on the exact column set the FK
-- references. The constraint name `metric_kind_natural_key_uniq`
-- makes the intent legible in `\d clean_code.metric_kind`.
ALTER TABLE clean_code.metric_kind
    ADD CONSTRAINT metric_kind_natural_key_uniq
    UNIQUE (metric_kind, metric_version);

-- ---------------------------------------------------------------------------
-- ScopeBinding (architecture Sec 5.2.3)
-- ---------------------------------------------------------------------------
--
-- Created BEFORE `metric_sample` so the latter's `scope_id` FK
-- resolves.
--
-- `scope_id` is a **deterministic UUIDv5** of `(repo_id, scope_kind,
-- canonical_signature, first_seen_sha)` -- the writer derives it
-- client-side so the same scope across two SHAs (with the same
-- canonical signature) produces the same `scope_id`. This is the
-- G2 "stable across SHAs" invariant (architecture Sec 5.2.3 line
-- 1044). The PRIMARY KEY on `scope_id` catches duplicate-derived
-- UUIDs; the additional `scope_binding_natural_key_uniq`
-- constraint below also catches a writer bug that produces two
-- different UUIDs for the same natural-key tuple.

CREATE TABLE clean_code.scope_binding (
    -- architecture Sec 5.2.3 line 1044: deterministic UUID from
    -- (repo_id, scope_kind, canonical_signature, first_seen_sha).
    scope_id              uuid                       PRIMARY KEY,
    repo_id               uuid                       NOT NULL
                          REFERENCES clean_code.repo (repo_id)
                          ON DELETE RESTRICT,
    scope_kind            clean_code.scope_kind      NOT NULL,
    -- Language-stable identifier (architecture Sec 5.2.3 line
    -- 1047). Same recipe as agent-memory `Node.canonical_signature`.
    canonical_signature   text                       NOT NULL,
    -- SHA at which this signature first appeared (architecture Sec
    -- 5.2.3 line 1048).
    first_seen_sha        text                       NOT NULL,
    -- Set IFF the service is running in `linked` mode
    -- (architecture Sec 5.2.3 line 1049). When set, this is the
    -- agent-memory `Node.node_id`. Nullable in `embedded` mode.
    agent_memory_node_id  uuid,
    -- Language-specific attributes (architecture Sec 5.2.3 line
    -- 1050). Insert-time only per G3.
    attrs_json            jsonb                      NOT NULL
                          DEFAULT '{}'::jsonb,
    -- Append-only.
    created_at            timestamptz                NOT NULL
                          DEFAULT now(),
    -- Defense-in-depth against a writer bug that derives two
    -- different `scope_id` UUIDs for the same natural-key tuple
    -- (architecture G2 "stable across SHAs" invariant). The
    -- writer's deterministic UUIDv5 should make this redundant on
    -- the happy path; the explicit UNIQUE catches drift in test
    -- and production both.
    CONSTRAINT scope_binding_natural_key_uniq
        UNIQUE (repo_id, scope_kind, canonical_signature, first_seen_sha)
);

COMMENT ON TABLE clean_code.scope_binding IS
    'Measurement sub-store (architecture Sec 1.5 G1, Sec 5.2.3). '
    'Writer: the Metric Ingestor is the SOLE writer; rows are '
    'append-only after creation per G3 row immutability. '
    'scope_id is a deterministic UUIDv5 of (repo_id, scope_kind, '
    'canonical_signature, first_seen_sha), STABLE ACROSS SHAs per '
    'architecture G2 (line 154); the writer derives it client-side '
    'so the same scope across two SHAs produces the same scope_id.';

COMMENT ON COLUMN clean_code.scope_binding.scope_id IS
    'Deterministic UUIDv5 of (repo_id, scope_kind, canonical_signature, '
    'first_seen_sha) per architecture Sec 5.2.3 line 1044. Stable '
    'across SHAs (G2). The writer computes the UUID client-side; '
    'the PRIMARY KEY here enforces uniqueness, the '
    'scope_binding_natural_key_uniq constraint enforces the '
    'natural-key uniqueness as additional defense-in-depth.';

-- ---------------------------------------------------------------------------
-- MetricSample (architecture Sec 5.2.1 -- the canonical row of the service)
-- ---------------------------------------------------------------------------
--
-- The canonical natural key per G2 is the QUINTUPLE
-- `(repo_id, sha, scope_id, metric_kind, metric_version)`. Active-row
-- uniqueness over that quintuple is enforced by the side-relation
-- `metric_sample_active` below; this table is the raw, append-only
-- store of every sample ever computed, including retracted samples
-- (which remain as the audit trail per G3).
--
-- Columns deliberately ABSENT (per the Stage 1.3 brief and
-- architecture Sec 5.2.1):
--   * `unit`           -- lives on `metric_kind` (architecture Sec 5.1.3 line 874).
--   * `policy_version_id` -- lives on `finding` (architecture Sec 5.4.1)
--                            and `hot_spot` (architecture Sec 5.5.1).
--   * `scan_run_id`    -- the run attribution column is `producer_run_id`.
--   * `computed_at`    -- the timestamp column is `created_at`.

CREATE TABLE clean_code.metric_sample (
    -- architecture Sec 5.2.1 line 894: server-generated UUID PK.
    sample_id        uuid                                 PRIMARY KEY
                     DEFAULT gen_random_uuid(),
    -- Natural-key columns (architecture Sec 5.2.1 lines 895-899).
    repo_id          uuid                                 NOT NULL
                     REFERENCES clean_code.repo (repo_id)
                     ON DELETE RESTRICT,
    sha              text                                 NOT NULL,
    scope_id         uuid                                 NOT NULL
                     REFERENCES clean_code.scope_binding (scope_id)
                     ON DELETE RESTRICT,
    metric_kind      text                                 NOT NULL,
    metric_version   integer                              NOT NULL,
    -- Numeric value (architecture Sec 5.2.1 line 900). NULL is
    -- ALLOWED iff `degraded = true` per implementation-plan.md
    -- lines 23, 674, 683 (system-tier fail-safe contract):
    -- when the Cross-Repo Aggregator composes a system-tier row
    -- under degraded inputs it WRITES the row anyway with
    -- `degraded=true, degraded_reason='samples_pending' |
    -- 'xrepo_edges_unavailable'` and either a best-effort
    -- partial value OR NULL. Architecture Sec 5.2.1 line 900
    -- types this as `double` (no NOT NULL assertion); the
    -- `metric_sample_value_present_unless_degraded` CHECK below
    -- enforces "value IS NOT NULL OR degraded=true" so the
    -- nullability is gated on the degraded flag at the row
    -- level rather than coupled to the column type. Foundation
    -- (`source='computed'`) and ingested (`source='ingested'`)
    -- rows are always written with `degraded=false` per
    -- architecture Sec 5.2.1 line 903 and therefore always
    -- supply a non-null value.
    value            double precision,
    -- Pack + source closed-set columns (architecture Sec 5.2.1
    -- lines 901-902). The Cross-Repo Aggregator is the ONLY writer
    -- that produces `pack='system' AND source='derived'` rows; the
    -- ACL split is enforced at the role-grant layer in Stage 1.5
    -- (`0004_roles.up.sql`) plus the application-layer per-metric_kind
    -- partitioning of the writer fleet.
    pack             clean_code.metric_sample_pack        NOT NULL,
    source           clean_code.metric_sample_source      NOT NULL,
    -- degraded + degraded_reason (architecture Sec 5.2.1 lines
    -- 903-904). `degraded=true` is settable at INSERT only (G3
    -- row immutability); a sample that lands degraded stays
    -- degraded at that SHA forever as the audit trail. The
    -- CHECK below enforces "degraded_reason set iff degraded=true".
    degraded         boolean                              NOT NULL
                     DEFAULT false,
    degraded_reason  clean_code.degraded_reason,
    -- Architecture Sec 5.2.1 line 905: producer_run_id FK -> ScanRun.
    -- The brief explicitly pins the name as `producer_run_id`, NOT
    -- `scan_run_id`.
    producer_run_id  uuid                                 NOT NULL
                     REFERENCES clean_code.scan_run (scan_run_id)
                     ON DELETE RESTRICT,
    -- Per-metric attributes (architecture Sec 5.2.1 line 906).
    -- Examples: cycle_id for `cycle_member` metric (Sec 1.4.1 row
    -- 10), language tag, sub-scope id. Insert-time only per G3.
    attrs_json       jsonb,
    -- Append-only timestamp (architecture Sec 5.2.1 line 907 +
    -- tech-spec Sec 8.7 naming convention "the canonical
    -- append-only timestamp column name across all sub-stores is
    -- `created_at`").
    created_at       timestamptz                          NOT NULL
                     DEFAULT now(),
    -- Monthly UTC bucket of `created_at` (tech-spec Sec 7.1.b
    -- line 1087, Sec 7.1.b partitioning-deferral pin). The
    -- expression is structured to be IMMUTABLE so PostgreSQL
    -- accepts it inside GENERATED ALWAYS AS:
    --   * `(created_at AT TIME ZONE 'UTC')` casts the
    --     timestamptz value to a timezone-independent
    --     `timestamp without time zone` rooted in UTC. With a
    --     literal-constant timezone this conversion is
    --     IMMUTABLE.
    --   * `date_trunc('month', timestamp)` is IMMUTABLE (the
    --     `timestamptz` overload is STABLE and would be
    --     rejected here).
    --   * `::date` is IMMUTABLE.
    -- Semantics: the bucket is the first day of the calendar
    -- month at 00:00 UTC. A sample written at 2025-11-30
    -- 23:30:00+0900 (= 2025-11-30 14:30:00 UTC) lands in the
    -- November bucket; the same writer's 2025-12-01 09:00:00+0900
    -- (= 2025-12-01 00:00:00 UTC) sample lands in December.
    -- This matches the cross-repo aggregate read pattern (Sec
    -- 3.10) which is inherently global and tz-agnostic.
    sample_date_bucket date                               GENERATED ALWAYS AS
                     (date_trunc('month', (created_at AT TIME ZONE 'UTC'))::date)
                     STORED,
    -- Composite FK against the 0001 `metric_kind` catalog row's
    -- natural key (anticipated by 0001 line 263-265 comment).
    -- Backed by the `metric_kind_natural_key_uniq` constraint
    -- added above.
    CONSTRAINT metric_sample_metric_kind_fk
        FOREIGN KEY (metric_kind, metric_version)
        REFERENCES clean_code.metric_kind (metric_kind, metric_version)
        ON DELETE RESTRICT,
    -- "Set iff degraded=true" (architecture Sec 5.2.1 line 904).
    CONSTRAINT metric_sample_degraded_reason_consistent CHECK (
        (degraded = false AND degraded_reason IS NULL)
        OR
        (degraded = true  AND degraded_reason IS NOT NULL)
    ),
    -- "value present unless degraded" (implementation-plan.md
    -- lines 23, 674, 683). The schema accepts NULL `value`
    -- only when `degraded=true`; happy-path rows always carry
    -- a real numeric value. This is the row-level enforcement
    -- of the system-tier fail-safe contract from architecture
    -- Sec 3.10 step 4 / tech-spec Sec 4.2: the Cross-Repo
    -- Aggregator never silently drops a system-tier row when
    -- inputs are missing; it writes the row with `degraded=true`
    -- and either a best-effort partial value or NULL.
    CONSTRAINT metric_sample_value_present_unless_degraded CHECK (
        value IS NOT NULL OR degraded = true
    ),
    -- Defense-in-depth for the active-pointer composite FK below:
    -- `metric_sample_active` FKs the full quintuple back to
    -- `metric_sample` so a pointer row's denormalized quintuple
    -- cannot disagree with the referenced sample. PostgreSQL
    -- requires an explicit UNIQUE constraint on the exact target
    -- column tuple for the FK to resolve. `sample_id` alone is
    -- already PK, so the tuple is trivially unique -- this
    -- constraint just exposes that uniqueness as an FK target.
    CONSTRAINT metric_sample_active_target_uniq
        UNIQUE (sample_id, repo_id, sha, scope_id, metric_kind, metric_version)
);

COMMENT ON TABLE clean_code.metric_sample IS
    'Measurement sub-store (architecture Sec 1.5 G1, Sec 5.2.1) -- '
    'the canonical row of the service. APPEND-ONLY per G3 / C2: no '
    'column is ever UPDATEd after insert; corrections are issued '
    'as (metric_retraction tombstone + new metric_sample row) pairs '
    'with the active pointer atomically re-pointed at the new row. '
    'Writers: Metric Ingestor (foundation tier, pack IN (base, solid, '
    'ingested)) and Cross-Repo Aggregator (system tier, pack=system '
    'AND source=derived). Per-metric_kind ACL carve-out enforced by '
    'the role grants added in `0004_roles.up.sql` plus application-'
    'layer per-kind partitioning.';

COMMENT ON COLUMN clean_code.metric_sample.producer_run_id IS
    'Run attribution FK -> ScanRun (architecture Sec 5.2.1 line 905). '
    'Canonical column name is `producer_run_id`, NOT `scan_run_id`.';

COMMENT ON COLUMN clean_code.metric_sample.created_at IS
    'Append-only insert timestamp (architecture Sec 5.2.1 line 907). '
    'Canonical column name is `created_at`, NOT `computed_at`.';

COMMENT ON COLUMN clean_code.metric_sample.degraded_reason IS
    'Closed-set reason drawn from architecture Sec 8.2 / tech-spec '
    'C21: xrepo_edges_unavailable | samples_pending | '
    'policy_signature_invalid | percentile_stale. Set IFF degraded=true; '
    'enforced by `metric_sample_degraded_reason_consistent` CHECK.';

COMMENT ON COLUMN clean_code.metric_sample.value IS
    'Numeric value (architecture Sec 5.2.1 line 900). NULL is '
    'ALLOWED iff degraded=true per implementation-plan.md lines '
    '23, 674, 683 (system-tier fail-safe contract from '
    'architecture Sec 3.10 step 4 / tech-spec Sec 4.2): the '
    'Cross-Repo Aggregator never silently drops a system-tier '
    'row when inputs are missing; it writes the row anyway with '
    'degraded=true and either a best-effort partial value or '
    'NULL. Enforced by `metric_sample_value_present_unless_degraded` '
    'CHECK.';

COMMENT ON COLUMN clean_code.metric_sample.sample_date_bucket IS
    'Monthly UTC bucket of `created_at` (tech-spec Sec 7.1.b line 1087, '
    'Sec 7.1.b partitioning-deferral pin). STORED GENERATED ALWAYS '
    'column; the expression uses `(created_at AT TIME ZONE ''UTC'')` '
    'to coerce timestamptz to a timezone-independent timestamp so '
    'date_trunc is IMMUTABLE. Bucket boundary is the first of the '
    'month at 00:00 UTC. This is the monthly sub-partition key the '
    'deferred PARTITION BY operational migration will use (Stage 1.3 '
    'deferral pin in tech-spec Sec 7.1.b).';

-- Hot-read covering index for the SHA-pinned read pattern
-- (architecture Sec 5.2.1 lines 933-951, tech-spec Sec 7.1.b lines
-- 1150-1160): readers always join through `metric_sample_active`
-- on the quintuple, but the underlying lookup of the active sample
-- row still scans `metric_sample` by `(repo_id, sha)`. This
-- composite index keeps that read on the index-only path.
CREATE INDEX metric_sample_repo_sha_idx
    ON clean_code.metric_sample (repo_id, sha);

-- ---------------------------------------------------------------------------
-- MetricRetraction (architecture Sec 5.2.2 / tech-spec Sec 7.1.b)
-- ---------------------------------------------------------------------------
--
-- Append-only tombstone log. One row per retraction event. The
-- `UNIQUE` on `sample_id` prevents double-retraction: a sample can
-- be retracted at most once, after which the active pointer's
-- pointer row must be re-pointed at a fresh sample row before any
-- further retraction can occur.

CREATE TABLE clean_code.metric_retraction (
    -- architecture Sec 5.2.2 line 1030: PK.
    retraction_id  uuid                              PRIMARY KEY
                   DEFAULT gen_random_uuid(),
    -- FK to the row being retracted (architecture Sec 5.2.2 line
    -- 1031). UNIQUE prevents double-retraction (tech-spec line
    -- 1095 + 1101).
    sample_id      uuid                              NOT NULL UNIQUE
                   REFERENCES clean_code.metric_sample (sample_id)
                   ON DELETE RESTRICT,
    -- Free-form reason (architecture Sec 5.2.2 line 1032), e.g.
    -- "file is vendored" or "superseded".
    reason         text                              NOT NULL,
    -- `ingestor` or `operator:<actor_id>` per architecture Sec
    -- 5.2.2 line 1033.
    appended_by    text                              NOT NULL,
    -- Append-only.
    created_at     timestamptz                       NOT NULL
                   DEFAULT now()
);

COMMENT ON TABLE clean_code.metric_retraction IS
    'Measurement sub-store (architecture Sec 1.5 G1, Sec 5.2.2). '
    'Writer: the Metric Ingestor (Sec 1.5.1 row 1) appends rows on '
    '`mgmt.retract_sample` (Sec 6.3) and on the supersede-during-'
    'reinsert transactional pattern (tech-spec Sec 7.1.b lines '
    '1125-1148). APPEND-ONLY -- no UPDATE / no DELETE. The UNIQUE '
    'on `sample_id` prevents double-retraction (tech-spec line 1101).';

-- ---------------------------------------------------------------------------
-- MetricSampleActive (the side relation -- tech-spec Sec 7.1.b /
-- locked-decision pin Sec 10A lines 1659-1675)
-- ---------------------------------------------------------------------------
--
-- The ACTIVE-row pointer table that materialises architecture's
-- mandated partial unique index over the quintuple. Architecture
-- (Sec 5.2.1 lines 991-1003) owns the contract; tech-spec Sec
-- 7.1.b owns this implementation. The PRIMARY KEY over the
-- quintuple is the unique B-tree that enforces "at most one active
-- row per quintuple" -- the partial-index predicate is satisfied
-- by construction because only active pointers ever land here.
--
-- Mutable BY DESIGN per tech-spec line 1104-1106: the Metric
-- Ingestor (or the Cross-Repo Aggregator, for `pack='system'`
-- rows) UPDATEs `sample_id` via `INSERT ... ON CONFLICT
-- (repo_id, sha, scope_id, metric_kind, metric_version) DO UPDATE
-- SET sample_id = EXCLUDED.sample_id`. The `metric_sample` row
-- it formerly pointed at remains in the table forever per G3.
--
-- The COMPOSITE FK back to `metric_sample` ensures the pointer
-- row's denormalized quintuple matches the referenced sample's
-- own columns -- a critical defense against a writer bug that
-- mis-points the active pointer to a sample with a different
-- repo / sha / scope / kind / version. Without this composite FK
-- the active-row reads (architecture Sec 5.2.1 Read-time semantics)
-- could return logically wrong rows while all simple-FK constraints
-- pass.

CREATE TABLE clean_code.metric_sample_active (
    -- Quintuple = the active-row identity per architecture G2.
    repo_id         uuid                              NOT NULL,
    sha             text                              NOT NULL,
    scope_id        uuid                              NOT NULL,
    metric_kind     text                              NOT NULL,
    metric_version  integer                           NOT NULL,
    -- Pointer to the active sample. The UNIQUE INDEX below
    -- (`metric_sample_active_sample_id_uniq`, tech-spec line 1118)
    -- guarantees one pointer never references two samples.
    sample_id       uuid                              NOT NULL,
    -- Architecture Sec 5.2.1 line 996: PRIMARY KEY on the quintuple
    -- carries the active-row uniqueness guarantee.
    PRIMARY KEY (repo_id, sha, scope_id, metric_kind, metric_version),
    -- Composite FK back to metric_sample's full quintuple so the
    -- denormalized columns here cannot disagree with the
    -- referenced sample. The reference target is the
    -- `metric_sample_active_target_uniq` constraint on
    -- `metric_sample` above.
    CONSTRAINT metric_sample_active_sample_consistent_fk
        FOREIGN KEY (sample_id, repo_id, sha, scope_id, metric_kind, metric_version)
        REFERENCES clean_code.metric_sample
            (sample_id, repo_id, sha, scope_id, metric_kind, metric_version)
        ON DELETE RESTRICT
);

-- Tech-spec line 1118-1119: "additionally ensures a single pointer
-- never references two samples". The composite FK above already
-- pins quintuple consistency; this index enforces the 1:1 mapping
-- between pointers and samples.
CREATE UNIQUE INDEX metric_sample_active_sample_id_uniq
    ON clean_code.metric_sample_active (sample_id);

COMMENT ON TABLE clean_code.metric_sample_active IS
    'Active-row pointer side relation (tech-spec Sec 7.1.b lines '
    '1103-1119, locked-decision pin Sec 10A lines 1659-1675). '
    'Implements architecture''s mandated partial unique index over '
    'the (repo_id, sha, scope_id, metric_kind, metric_version) '
    'quintuple (architecture G2 / Sec 5.2.1 lines 991-1003). One '
    'row per ACTIVE metric sample; PRIMARY KEY enforces at-most-one-'
    'active-row-per-quintuple. Writers: Metric Ingestor (foundation '
    'tier metric_kinds) and Cross-Repo Aggregator (system tier '
    'metric_kinds) maintain pointers via INSERT ... ON CONFLICT DO '
    'UPDATE. Mutable BY DESIGN -- a UNIQUE pointer table that the '
    'writers re-point during retract-then-reinsert is the only PG-'
    'valid encoding of architecture''s partial-unique-index contract '
    '(see DDL header comment for the PostgreSQL-DDL constraint).';

-- ---------------------------------------------------------------------------
-- RepoMetricSnapshot (architecture Sec 5.2.4)
-- ---------------------------------------------------------------------------

CREATE TABLE clean_code.repo_metric_snapshot (
    -- architecture Sec 5.2.4 line 1057: PK.
    snapshot_id   uuid                       PRIMARY KEY
                  DEFAULT gen_random_uuid(),
    repo_id       uuid                       NOT NULL
                  REFERENCES clean_code.repo (repo_id)
                  ON DELETE RESTRICT,
    -- Part of natural key (architecture Sec 5.2.4 line 1059).
    metric_kind   text                       NOT NULL
                  REFERENCES clean_code.metric_kind (metric_kind)
                  ON DELETE RESTRICT,
    -- Part of natural key (architecture Sec 5.2.4 line 1060).
    scope_kind    clean_code.scope_kind      NOT NULL,
    -- Canonical column name is `count`, NOT `sample_count`
    -- (per the Stage 1.3 brief and architecture Sec 5.2.4 line 1061).
    count         bigint                     NOT NULL,
    mean          double precision           NOT NULL,
    p50           double precision           NOT NULL,
    p90           double precision           NOT NULL,
    p99           double precision           NOT NULL,
    -- Aggregator-built timestamp (architecture Sec 5.2.4 line 1066).
    built_at      timestamptz                NOT NULL DEFAULT now()
);

COMMENT ON TABLE clean_code.repo_metric_snapshot IS
    'Measurement sub-store (architecture Sec 1.5 G1, Sec 5.2.4). '
    'Writer: the Cross-Repo Aggregator (Sec 3.10 step 4) is the SOLE '
    'writer; rows are append-only derivative views per G6 (the rows '
    'are recomputable from `metric_sample` at any time). No `sha` '
    'column -- repo-wide latest aggregates only.';

-- ---------------------------------------------------------------------------
-- CrossRepoPercentile (architecture Sec 5.2.5)
-- ---------------------------------------------------------------------------

CREATE TABLE clean_code.cross_repo_percentile (
    -- architecture Sec 5.2.5 line 1072: PK.
    percentile_id    uuid                       PRIMARY KEY
                     DEFAULT gen_random_uuid(),
    -- Part of natural key (architecture Sec 5.2.5 line 1073).
    metric_kind      text                       NOT NULL
                     REFERENCES clean_code.metric_kind (metric_kind)
                     ON DELETE RESTRICT,
    -- Part of natural key (architecture Sec 5.2.5 line 1074).
    scope_kind       clean_code.scope_kind      NOT NULL,
    -- Per-repo histogram for portfolio UI rendering (architecture
    -- Sec 5.2.5 line 1075).
    histogram_json   jsonb                      NOT NULL,
    p50              double precision           NOT NULL,
    p90              double precision           NOT NULL,
    p99              double precision           NOT NULL,
    -- Freshness clock (architecture Sec 5.2.5 line 1079, Sec 8.4).
    built_at         timestamptz                NOT NULL DEFAULT now()
);

COMMENT ON TABLE clean_code.cross_repo_percentile IS
    'Measurement sub-store (architecture Sec 1.5 G1, Sec 5.2.5). '
    'Writer: the Cross-Repo Aggregator (Sec 3.10 step 4) is the SOLE '
    'writer; rows are append-only derivative views per G6. `built_at` '
    'is the freshness clock the Insights surface compares against '
    '`freshness_window_seconds` (Sec 8.2) to emit `percentile_stale` '
    'degraded responses.';

-- ---------------------------------------------------------------------------
-- PortfolioSnapshot (architecture Sec 5.2.6)
-- ---------------------------------------------------------------------------

CREATE TABLE clean_code.portfolio_snapshot (
    -- architecture Sec 5.2.6 line 1085: PK.
    portfolio_snapshot_id  uuid                       PRIMARY KEY
                           DEFAULT gen_random_uuid(),
    -- architecture Sec 5.2.6 line 1086.
    metric_kind            text                       NOT NULL
                           REFERENCES clean_code.metric_kind (metric_kind)
                           ON DELETE RESTRICT,
    scope_kind             clean_code.scope_kind      NOT NULL,
    -- Number of repos contributing (architecture Sec 5.2.6 line
    -- 1088).
    repo_count             integer                    NOT NULL,
    -- Operator-pinned aggregate shape (architecture Sec 5.2.6 line
    -- 1089). The Stage 1.3 brief pins: no `sum_value` /
    -- `weighted_mean` columns -- those aggregates are encoded as
    -- keys inside `aggregate_json`.
    aggregate_json         jsonb                      NOT NULL,
    built_at               timestamptz                NOT NULL
                           DEFAULT now()
);

COMMENT ON TABLE clean_code.portfolio_snapshot IS
    'Measurement sub-store (architecture Sec 1.5 G1, Sec 5.2.6). '
    'Writer: the Cross-Repo Aggregator (Sec 3.10 step 4) is the SOLE '
    'writer; rows are append-only derivative views per G6. Aggregate '
    'shape is operator-pinned and serialised into `aggregate_json` '
    'rather than spread across typed columns; this lets v1 add new '
    'aggregate keys without a schema migration.';

COMMIT;
