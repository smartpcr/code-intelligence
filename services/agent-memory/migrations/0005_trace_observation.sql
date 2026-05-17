-- 0005_trace_observation.sql
--
-- Stage 1.2 step 5 (implementation-plan.md): create the mutable
-- aggregate `trace_observation` table plus the partitioned
-- append-only parent `trace_observation_log`.
--
-- Per architecture.md §5.2.3 the parent Edge row stays immutable
-- (G5); the mutable counters live on `trace_observation` (one row
-- per observed_calls edge), and the source-of-truth provenance is
-- the append-only `trace_observation_log` (one row per ingested
-- span). The log is partitioned weekly on `started_at` per
-- tech-spec §8.7.3 so the §8.1 30-day retention pruner can drop
-- whole partitions instead of running a row-by-row DELETE.
--
-- pg_partman registration and forward-partition provisioning are
-- deferred to Stage 1.3 migration 0014. This migration creates
-- the partitioned parent table only.

-- migrate:up
BEGIN;

CREATE TABLE trace_observation (
    -- One row per observed_calls Edge. The Edge row's append-only
    -- invariant (G5) is preserved because all mutation happens
    -- here, not on `edge`.
    edge_id           uuid        PRIMARY KEY REFERENCES edge (edge_id) ON DELETE RESTRICT,
    observation_count bigint      NOT NULL DEFAULT 0,
    p50_latency_ms    double precision NOT NULL DEFAULT 0,
    p95_latency_ms    double precision NOT NULL DEFAULT 0,
    latest_span_ref   text,
    last_observed_at  timestamptz
);

CREATE INDEX trace_observation_last_observed_at_idx
    ON trace_observation (last_observed_at DESC NULLS LAST);

-- Partitioned parent for the append-only span log. Each partition
-- covers one week of `started_at`; pg_partman (Stage 1.4 / 0014)
-- maintains the rolling window. NOTE: partitioned tables in
-- PostgreSQL cannot declare a PRIMARY KEY unless every column of
-- the PK is also part of the partition key. We surface the
-- (span_log_id, started_at) composite as the PK so a single span
-- log row is uniquely addressable within its partition.
CREATE TABLE trace_observation_log (
    span_log_id  uuid        NOT NULL DEFAULT gen_random_uuid(),
    edge_id      uuid        NOT NULL REFERENCES edge (edge_id) ON DELETE RESTRICT,
    trace_id     text        NOT NULL,
    span_id      text        NOT NULL,
    started_at   timestamptz NOT NULL,
    duration_ms  double precision NOT NULL,
    PRIMARY KEY (span_log_id, started_at)
) PARTITION BY RANGE (started_at);

-- tech-spec §8.7.2: TraceObservationLog scan -- B-tree on
-- (edge_id, started_at DESC). On a partitioned table this index
-- is a propagation template: PostgreSQL creates a matching child
-- index on every existing partition, and pg_partman creates one
-- on every future partition as it provisions them.
CREATE INDEX trace_observation_log_edge_started_idx
    ON trace_observation_log (edge_id, started_at DESC);

-- A bootstrap default partition lets the migration apply (and
-- the round-trip test pass) before pg_partman has been wired in
-- 0014. Any row whose `started_at` falls outside the explicit
-- weekly partitions lands here; in steady state the pg_partman
-- maintenance worker keeps this empty by provisioning forward
-- partitions ahead of the writer.
CREATE TABLE trace_observation_log_default
    PARTITION OF trace_observation_log DEFAULT;

COMMIT;

-- migrate:down
BEGIN;

-- Children are dropped automatically when the partitioned parent
-- is dropped, but DROP TABLE IF EXISTS on the default partition
-- first keeps the down-migration explicit and idempotent.
DROP TABLE IF EXISTS trace_observation_log_default;
DROP TABLE IF EXISTS trace_observation_log;
DROP TABLE IF EXISTS trace_observation;

COMMIT;
