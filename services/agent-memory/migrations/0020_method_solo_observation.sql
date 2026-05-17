-- 0020_method_solo_observation.sql
--
-- Stage 4.2 step 4 (implementation-plan.md): persist the
-- destination-Method solo aggregate for ROOT OTel spans (spans
-- with no `parent_span_id`).
--
-- Why a dedicated table
-- ---------------------
-- Tech-spec §8.6 row 3 pins root-span behaviour:
--   "If the parent span is missing (root span), drop the edge
--    contribution but record the latency on the destination
--    Method's solo aggregate."
--
-- `TraceObservation` is keyed by `edge_id` and is therefore the
-- wrong home for a Method-anchored aggregate (there is no Edge
-- for a solo observation; synthesizing a self-loop Edge would
-- pollute `static_calls` / `observed_calls` walks the
-- GraphReader does for §4.5 neighborhood expansion).
--
-- We add a parallel table keyed by Method `node_id`. The
-- columns mirror `TraceObservation` so the same in-process
-- aggregator code computes p50 / p95 / count / latest, and the
-- writer path (`graphwriter.AppendSoloMethodObservation`) is a
-- straight UPSERT.
--
-- A companion `method_solo_observation_log` would parallel
-- `trace_observation_log` for rebuildability, but Stage 4.2's
-- acceptance scenarios do not require it and the dataset is
-- much smaller (root spans are typically entry points, not
-- every internal call). The aggregate-only shape is
-- recoverable from the OTel collector retransmit story; a
-- follow-up workstream can promote it to a log+aggregate pair
-- if operators want partition-level retention parity with
-- TraceObservationLog.
--
-- Mutability classification
-- -------------------------
-- Per tech-spec §8.7.4, `TraceObservation` is on the
-- UPDATE-grantable set (counter row; provenance lives in the
-- append-only log). `method_solo_observation` matches that
-- shape: the ingestor UPSERTs counters; provenance is not
-- captured at the row level (see "companion log" rationale
-- above). The role grant below mirrors `trace_observation`.
--
-- Cascade semantics
-- -----------------
-- `ON DELETE RESTRICT` on `node_id` matches the Node→counter
-- relationship from `trace_observation.edge_id`: a Method is
-- never hard-deleted in v1 (G5 + tombstones), so the cascade
-- never fires.

-- migrate:up
BEGIN;

CREATE TABLE method_solo_observation (
    -- One row per Method that has been observed as a root span.
    node_id           uuid              PRIMARY KEY REFERENCES node (node_id) ON DELETE RESTRICT,
    observation_count bigint            NOT NULL DEFAULT 0,
    p50_latency_ms    double precision  NOT NULL DEFAULT 0,
    p95_latency_ms    double precision  NOT NULL DEFAULT 0,
    latest_span_ref   text,
    last_observed_at  timestamptz
);

-- Dashboard query "most-recently observed root methods" needs
-- a descending-time index; mirrors trace_observation's
-- last_observed_at index.
CREATE INDEX method_solo_observation_last_observed_at_idx
    ON method_solo_observation (last_observed_at DESC NULLS LAST);

DO $$
DECLARE
    cs text := current_schema();
BEGIN
    EXECUTE format(
        'GRANT INSERT, SELECT, UPDATE ON %I.%I TO agent_memory_app',
        cs, 'method_solo_observation'
    );
END$$;

COMMIT;

-- migrate:down
BEGIN;

DROP TABLE IF EXISTS method_solo_observation;

COMMIT;
