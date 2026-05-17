// Package graphwriter -- this file owns the §4 (Span Ingestor) and
// §C22 (degraded_reason surface) DML paths added in Stage 4.2.
//
// The three exported writers are:
//
//   - `AppendObservedCallTrace` -- single-tx UPSERT covering the
//     `observed_calls` Edge (via `insertEdgeImpl`), a fresh
//     `trace_observation_log` row, and the `trace_observation`
//     aggregate counter. A single tx is load-bearing: a crash
//     between the edge insert and the aggregate UPSERT would
//     leave an Edge that no log row references (the §3.3 G3
//     invariant of "Edge identity from observed traffic" would
//     hold but the operator dashboard would show a counter that
//     can never increment).
//
//   - `AppendSoloMethodObservation` -- single-tx UPSERT covering
//     the `method_solo_observation` aggregate for ROOT OTel
//     spans (no `parent_span_id`). §8.6 row 3 forbids creating
//     an Edge for these — we keep the latency on the destination
//     Method instead, in a parallel table that the same
//     in-process aggregator can feed.
//
//   - `UpsertRepoHealth` -- read-write for the cross-process
//     degraded-state flag the agent-api recall handler consults
//     when populating `RecallResponse.degraded`. Append-style
//     `INSERT ... ON CONFLICT DO UPDATE` so the Span Ingestor's
//     supervisor can flip the flag every sustain-window tick
//     without us forcing it to read first.
//
// Why these live here (not in a separate `traceobservation` or
// `health` package): GraphWriter is, by tech-spec §C12, the only
// library allowed to perform DML against the schema. Splitting
// the writer surface across packages would let a future bug
// route an UPDATE through a sibling helper that doesn't go
// through `runInTx` + `classifyErr` + `emitAudit` — losing the
// SQLSTATE 42501 classification and the structured audit record.
package graphwriter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
)

// ObservationInput is the per-span payload the Span Ingestor
// hands to `AppendObservedCallTrace` / `AppendSoloMethodObservation`.
//
// `P50LatencyMs` / `P95LatencyMs` are CALLER-COMPUTED. The
// writer cannot derive a quantile from a single point; the
// ingestor maintains a per-edge / per-method rolling-window
// aggregator (`internal/spaningestor.LatencyAggregator`) and
// hands the resulting numbers to this writer. The aggregator's
// state is in-process, so multi-instance deployments need to
// shard by repo (or edge) so two concurrent writers don't
// overwrite each other's `EXCLUDED.p50_latency_ms`; the v1
// composition is single-process per repo per the
// implementation-plan §4 expansion notes.
type ObservationInput struct {
	// TraceID is the OTel `trace_id` (typically a 16-byte hex
	// string). Forwarded verbatim into the log row's
	// `trace_id` column.
	TraceID string
	// SpanID is the OTel `span_id` (typically an 8-byte hex
	// string). Forwarded verbatim into the log row's
	// `span_id` column.
	SpanID string
	// StartedAt is the span's start time (UTC). MUST be a
	// real time (`!IsZero()`); the aggregate row's
	// `last_observed_at` is `GREATEST(existing, EXCLUDED)`
	// so an out-of-order span finishing AFTER a newer one
	// finished does NOT roll the aggregate timestamp
	// backward.
	StartedAt time.Time
	// DurationMs is the span's measured duration in
	// milliseconds. Logged verbatim and consumed by the
	// in-process latency aggregator on the caller side.
	DurationMs float64
	// P50LatencyMs is the caller-computed median over its
	// rolling window. Overwrites the aggregate row.
	P50LatencyMs float64
	// P95LatencyMs is the caller-computed 95th percentile.
	// Overwrites the aggregate row.
	P95LatencyMs float64
}

// validate checks the invariants the schema cannot express
// (zero time, empty trace/span identifiers). Returning before
// `runInTx` keeps a bad input from costing us a transaction.
func (in ObservationInput) validate(op string) error {
	if in.TraceID == "" {
		return fmt.Errorf("graphwriter: %s: empty trace_id", op)
	}
	if in.SpanID == "" {
		return fmt.Errorf("graphwriter: %s: empty span_id", op)
	}
	if in.StartedAt.IsZero() {
		return fmt.Errorf("graphwriter: %s: zero started_at", op)
	}
	// DurationMs can legitimately be 0 (a span shorter than
	// the clock granularity); negative durations are a bug in
	// the emitter so we reject them so the bad data never
	// reaches the histogram.
	if in.DurationMs < 0 {
		return fmt.Errorf("graphwriter: %s: negative duration_ms %f", op, in.DurationMs)
	}
	return nil
}

// ObservedCallTraceRecord is the post-write state of
// `AppendObservedCallTrace`. Mirrors `EdgeRecord` for the edge
// half and surfaces the freshly-incremented aggregate counter
// (so callers can confirm idempotency / monitor backpressure
// without re-querying).
type ObservedCallTraceRecord struct {
	Edge              EdgeRecord
	ObservationCount  int64  // post-write value
	SpanLogID         string // textual UUID
}

// AppendObservedCallTrace performs the full Span Ingestor write
// for ONE resolved caller→callee span:
//
//  1. UPSERT the `observed_calls` Edge (idempotent on
//     (repo_id, fingerprint)).
//  2. INSERT one `trace_observation_log` row (append-only).
//  3. UPSERT the `trace_observation` aggregate (counter +1,
//     latency / latest_span_ref updated).
//
// All three statements run inside a single transaction so a
// crash between any two of them rolls everything back — no
// orphan log row, no orphan aggregate, no double-counted span.
//
// `edgeIn` MUST have `Kind` blank; the helper pins it to
// `observed_calls` (mirroring `InsertObservedCallsEdge` so
// caller code reads declaratively).
//
// Routes through `emitAudit` so every call emits exactly one
// structured log record under op="append_observed_call_trace".
func (w *Writer) AppendObservedCallTrace(
	ctx context.Context,
	edgeIn EdgeInput,
	obs ObservationInput,
) (rec ObservedCallTraceRecord, err error) {
	edgeIn.Kind = "observed_calls"
	repoIDStr := edgeIn.RepoID.String()
	fields := auditFields{
		RepoID: repoIDStr,
		Kind:   edgeIn.Kind,
		SHA:    edgeIn.FromSHA,
	}
	defer w.auditDefer("append_observed_call_trace", &fields, &err)()

	if err := obs.validate("AppendObservedCallTrace"); err != nil {
		return ObservedCallTraceRecord{}, err
	}

	err = w.runInTx(ctx, "AppendObservedCallTrace", func(tx *sql.Tx) error {
		edgeRec, edgeErr := w.upsertObservedCallsEdgeTx(ctx, tx, edgeIn, &fields)
		if edgeErr != nil {
			return edgeErr
		}
		rec.Edge = edgeRec

		// 2. Append the log row. The PK is (span_log_id, started_at)
		// per the partitioned-parent table; `gen_random_uuid()` on
		// the schema default mints span_log_id, and we RETURNING it
		// so callers can correlate without a second round-trip.
		const insertLogQ = `
			INSERT INTO trace_observation_log
			    (edge_id, trace_id, span_id, started_at, duration_ms)
			VALUES ($1, $2, $3, $4, $5)
			RETURNING span_log_id::text
		`
		if err := tx.QueryRowContext(ctx, insertLogQ,
			edgeRec.EdgeID, obs.TraceID, obs.SpanID,
			obs.StartedAt.UTC(), obs.DurationMs,
		).Scan(&rec.SpanLogID); err != nil {
			return fmt.Errorf("graphwriter: AppendObservedCallTrace log: %w", err)
		}

		// 3. UPSERT the aggregate. `observation_count + 1` is
		// atomic under concurrent UPSERTs on the same edge (the
		// row lock the ON CONFLICT path takes serialises
		// counter mutations). last_observed_at uses GREATEST so
		// an out-of-order span finishing later does not roll
		// the aggregate timestamp backward (rubber-duck finding
		// #7); latest_span_ref is preserved when the incoming
		// span is older than what we already wrote.
		latestRef := obs.TraceID + ":" + obs.SpanID
		const upsertAggQ = `
			INSERT INTO trace_observation
			    (edge_id, observation_count, p50_latency_ms,
			     p95_latency_ms, latest_span_ref, last_observed_at)
			VALUES ($1, 1, $2, $3, $4, $5)
			ON CONFLICT (edge_id) DO UPDATE SET
			    observation_count = trace_observation.observation_count + 1,
			    p50_latency_ms    = EXCLUDED.p50_latency_ms,
			    p95_latency_ms    = EXCLUDED.p95_latency_ms,
			    latest_span_ref   = CASE
			        WHEN EXCLUDED.last_observed_at >= trace_observation.last_observed_at
			          OR trace_observation.last_observed_at IS NULL
			        THEN EXCLUDED.latest_span_ref
			        ELSE trace_observation.latest_span_ref
			    END,
			    last_observed_at  = GREATEST(
			        trace_observation.last_observed_at,
			        EXCLUDED.last_observed_at
			    )
			RETURNING observation_count
		`
		if err := tx.QueryRowContext(ctx, upsertAggQ,
			edgeRec.EdgeID, obs.P50LatencyMs, obs.P95LatencyMs,
			latestRef, obs.StartedAt.UTC(),
		).Scan(&rec.ObservationCount); err != nil {
			return fmt.Errorf("graphwriter: AppendObservedCallTrace aggregate: %w", err)
		}
		return nil
	})
	if err != nil {
		return ObservedCallTraceRecord{}, err
	}
	fields.Extras = append(fields.Extras,
		slog.String("edge_id", rec.Edge.EdgeID),
		slog.Bool("edge_inserted", rec.Edge.Inserted),
		slog.String("span_log_id", rec.SpanLogID),
		slog.Int64("observation_count", rec.ObservationCount),
	)
	return rec, nil
}

// upsertObservedCallsEdgeTx is the in-tx half of
// InsertObservedCallsEdge, factored out so AppendObservedCallTrace
// can run the edge UPSERT and the log+aggregate writes inside a
// SINGLE transaction (rubber-duck blocker #2 — calling
// InsertObservedCallsEdge first would commit its own tx, leaving
// a window where a crash strands an Edge with no log row).
//
// Mirrors `insertEdgeImpl` minus the surrounding `runInTx` and
// the standalone `emitAudit` wiring (the caller owns both). The
// `auditFields` pointer threads through so fingerprint_hex
// lands on the outer audit record.
func (w *Writer) upsertObservedCallsEdgeTx(
	ctx context.Context,
	tx *sql.Tx,
	in EdgeInput,
	fields *auditFields,
) (EdgeRecord, error) {
	const op = "AppendObservedCallTrace"
	if in.SrcNodeID == "" || in.DstNodeID == "" {
		return EdgeRecord{}, fmt.Errorf("graphwriter: %s: empty src/dst node_id", op)
	}
	if in.FromSHA == "" {
		return EdgeRecord{}, fmt.Errorf("graphwriter: %s: empty from_sha", op)
	}
	attrs, err := normaliseAttrs(in.AttrsJSON)
	if err != nil {
		return EdgeRecord{}, fmt.Errorf("graphwriter: %s attrs_json: %w", op, err)
	}
	repoIDStr := in.RepoID.String()

	srcRepo, srcFP, err := lookupNodeFingerprint(ctx, tx, in.SrcNodeID)
	if err != nil {
		return EdgeRecord{}, fmt.Errorf("graphwriter: %s src: %w", op, err)
	}
	dstRepo, dstFP, err := lookupNodeFingerprint(ctx, tx, in.DstNodeID)
	if err != nil {
		return EdgeRecord{}, fmt.Errorf("graphwriter: %s dst: %w", op, err)
	}
	if srcRepo != repoIDStr {
		return EdgeRecord{}, fmt.Errorf(
			"graphwriter: %s: src_node_id %s belongs to repo %s, not %s",
			op, in.SrcNodeID, srcRepo, repoIDStr,
		)
	}
	if dstRepo != repoIDStr {
		return EdgeRecord{}, fmt.Errorf(
			"graphwriter: %s: dst_node_id %s belongs to repo %s, not %s",
			op, in.DstNodeID, dstRepo, repoIDStr,
		)
	}

	fp, err := fingerprint.EdgeFingerprint(in.RepoID, in.Kind, srcFP, dstFP, in.FromSHA)
	if err != nil {
		return EdgeRecord{}, fmt.Errorf("graphwriter: %s fingerprint: %w", op, err)
	}
	rec := EdgeRecord{Fingerprint: fp, SrcFP: srcFP, DstFP: dstFP}
	fields.FingerprintHex = fp.Hex()

	const insertQ = `
		INSERT INTO edge
		    (fingerprint, repo_id, kind, src_node_id, dst_node_id, from_sha, attrs_json)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb)
		ON CONFLICT (repo_id, fingerprint) DO NOTHING
		RETURNING edge_id::text
	`
	scanErr := tx.QueryRowContext(ctx, insertQ,
		fp.Bytes(), repoIDStr, in.Kind,
		in.SrcNodeID, in.DstNodeID, in.FromSHA, string(attrs),
	).Scan(&rec.EdgeID)
	switch {
	case scanErr == nil:
		rec.Inserted = true
		return rec, nil
	case errors.Is(scanErr, sql.ErrNoRows):
		const selectQ = `
			SELECT edge_id::text FROM edge
			WHERE repo_id = $1 AND fingerprint = $2
		`
		if err := tx.QueryRowContext(ctx, selectQ, repoIDStr, fp.Bytes()).
			Scan(&rec.EdgeID); err != nil {
			return EdgeRecord{}, fmt.Errorf("graphwriter: %s fallback select: %w", op, err)
		}
		rec.Inserted = false
		return rec, nil
	default:
		return EdgeRecord{}, scanErr
	}
}

// SoloObservationRecord is the post-write state of
// `AppendSoloMethodObservation`.
type SoloObservationRecord struct {
	NodeID           string
	ObservationCount int64
	Inserted         bool // true on first insert
}

// AppendSoloMethodObservation records the destination-Method
// solo aggregate for a root OTel span (tech-spec §8.6 row 3:
// "drop the edge contribution but record the latency on the
// destination Method's solo aggregate"). Single tx (UPSERT only;
// no companion log table in v1 — see migration 0020 header).
//
// The function is purely the UPSERT — solo aggregates do NOT
// carry an Edge identity, so there is no fingerprint and no
// `observed_calls` row to write.
func (w *Writer) AppendSoloMethodObservation(
	ctx context.Context,
	methodNodeID string,
	obs ObservationInput,
) (rec SoloObservationRecord, err error) {
	fields := auditFields{
		Extras: []slog.Attr{slog.String("method_node_id", methodNodeID)},
	}
	defer w.auditDefer("append_solo_method_observation", &fields, &err)()

	if methodNodeID == "" {
		return SoloObservationRecord{}, errors.New(
			"graphwriter: AppendSoloMethodObservation: empty method_node_id")
	}
	if err := obs.validate("AppendSoloMethodObservation"); err != nil {
		return SoloObservationRecord{}, err
	}

	rec.NodeID = methodNodeID
	err = w.runInTx(ctx, "AppendSoloMethodObservation", func(tx *sql.Tx) error {
		// We need the repo_id for the audit record; look it up
		// inside the tx so the audit field reflects the row the
		// UPSERT actually touched (the ON CONFLICT path takes
		// the row-level lock on this exact node, so the lookup
		// races nothing).
		var repoIDStr string
		if err := tx.QueryRowContext(ctx,
			`SELECT repo_id::text FROM node WHERE node_id = $1`,
			methodNodeID,
		).Scan(&repoIDStr); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf(
					"graphwriter: AppendSoloMethodObservation: method_node_id %s not found",
					methodNodeID)
			}
			return fmt.Errorf("graphwriter: AppendSoloMethodObservation lookup: %w", err)
		}
		fields.RepoID = repoIDStr

		latestRef := obs.TraceID + ":" + obs.SpanID
		const upsertQ = `
			INSERT INTO method_solo_observation
			    (node_id, observation_count, p50_latency_ms,
			     p95_latency_ms, latest_span_ref, last_observed_at)
			VALUES ($1, 1, $2, $3, $4, $5)
			ON CONFLICT (node_id) DO UPDATE SET
			    observation_count = method_solo_observation.observation_count + 1,
			    p50_latency_ms    = EXCLUDED.p50_latency_ms,
			    p95_latency_ms    = EXCLUDED.p95_latency_ms,
			    latest_span_ref   = CASE
			        WHEN EXCLUDED.last_observed_at >= method_solo_observation.last_observed_at
			          OR method_solo_observation.last_observed_at IS NULL
			        THEN EXCLUDED.latest_span_ref
			        ELSE method_solo_observation.latest_span_ref
			    END,
			    last_observed_at  = GREATEST(
			        method_solo_observation.last_observed_at,
			        EXCLUDED.last_observed_at
			    )
			RETURNING observation_count, (xmax = 0) AS inserted
		`
		if err := tx.QueryRowContext(ctx, upsertQ,
			methodNodeID, obs.P50LatencyMs, obs.P95LatencyMs,
			latestRef, obs.StartedAt.UTC(),
		).Scan(&rec.ObservationCount, &rec.Inserted); err != nil {
			return fmt.Errorf("graphwriter: AppendSoloMethodObservation upsert: %w", err)
		}
		return nil
	})
	if err != nil {
		return SoloObservationRecord{}, err
	}
	fields.Extras = append(fields.Extras,
		slog.Int64("observation_count", rec.ObservationCount),
		slog.Bool("inserted", rec.Inserted),
	)
	return rec, nil
}

// HealthInput is the cross-process degraded-state flag the Span
// Ingestor writes and the agent-api recall handler reads.
//
// `Degraded == false` clears any prior `degraded_reason`. The
// caller passes an empty `Reason` for the cleared case; the
// CHECK constraint on the table enforces the (degraded ↔ reason
// IS NOT NULL) invariant — passing a non-empty reason with
// Degraded=false trips a 23514 check_violation, surfacing the
// caller's bug at the database layer rather than letting
// inconsistent state land.
type HealthInput struct {
	RepoID         string // textual UUID
	Degraded       bool
	Reason         string // degraded_reason ENUM literal, empty when !Degraded
	Source         string // attribution: e.g. "span-ingestor"
	ObservedAt     time.Time
}

// HealthRecord is the post-write state of `UpsertRepoHealth`.
type HealthRecord struct {
	Inserted        bool   // true on first insert
	TransitionedAt  time.Time // timestamp the CURRENT state began
}

// UpsertRepoHealth writes the per-repo degraded-state row. Used
// by the Span Ingestor (cmd/span-ingestor) to raise the
// `span_ingestor_backpressure` flag; agent-api reads via the
// `agent_memory_ro` role.
//
// `since` semantics: preserved across same-state UPSERTs
// (degraded→degraded), bumped on state transitions
// (degraded→healthy or healthy→degraded). This lets operators
// answer "how long has this repo been degraded" with a single
// SELECT.
//
// Caller MUST pass `Reason == ""` when `Degraded == false` and
// vice versa; the CHECK constraint will reject mismatches.
func (w *Writer) UpsertRepoHealth(
	ctx context.Context, in HealthInput,
) (rec HealthRecord, err error) {
	fields := auditFields{
		RepoID: in.RepoID,
		Extras: []slog.Attr{
			slog.Bool("degraded", in.Degraded),
			slog.String("reason", in.Reason),
			slog.String("source", in.Source),
		},
	}
	defer w.auditDefer("upsert_repo_health", &fields, &err)()

	if in.RepoID == "" {
		return HealthRecord{}, errors.New("graphwriter: UpsertRepoHealth: empty repo_id")
	}
	if in.Source == "" {
		return HealthRecord{}, errors.New("graphwriter: UpsertRepoHealth: empty source")
	}
	if in.Degraded && in.Reason == "" {
		return HealthRecord{}, errors.New(
			"graphwriter: UpsertRepoHealth: Degraded=true requires non-empty Reason")
	}
	if !in.Degraded && in.Reason != "" {
		return HealthRecord{}, errors.New(
			"graphwriter: UpsertRepoHealth: Degraded=false requires empty Reason")
	}
	now := in.ObservedAt
	if now.IsZero() {
		now = w.now().UTC()
	} else {
		now = now.UTC()
	}

	// Reason is the closed-set degraded_reason ENUM. `pq` won't
	// auto-cast a NULL string to the enum type, so we pass either
	// a typed text + cast or NULL via sql.NullString.
	var reason sql.NullString
	if in.Reason != "" {
		reason = sql.NullString{String: in.Reason, Valid: true}
	}

	// since semantics: on same-state conflict keep the existing
	// `since`; on state transition (XOR check) bump to `now`.
	const upsertQ = `
		INSERT INTO repo_health
		    (repo_id, degraded, degraded_reason, source, since, updated_at)
		VALUES ($1, $2, $3::degraded_reason, $4, $5, $5)
		ON CONFLICT (repo_id) DO UPDATE SET
		    degraded        = EXCLUDED.degraded,
		    degraded_reason = EXCLUDED.degraded_reason,
		    source          = EXCLUDED.source,
		    since           = CASE
		        WHEN repo_health.degraded IS DISTINCT FROM EXCLUDED.degraded
		         OR repo_health.degraded_reason IS DISTINCT FROM EXCLUDED.degraded_reason
		        THEN EXCLUDED.since
		        ELSE repo_health.since
		    END,
		    updated_at      = EXCLUDED.updated_at
		RETURNING (xmax = 0) AS inserted, since
	`
	err = w.runInTx(ctx, "UpsertRepoHealth", func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, upsertQ,
			in.RepoID, in.Degraded, reason, in.Source, now,
		).Scan(&rec.Inserted, &rec.TransitionedAt)
	})
	if err != nil {
		return HealthRecord{}, err
	}
	fields.Extras = append(fields.Extras,
		slog.Bool("inserted", rec.Inserted),
		slog.Time("since", rec.TransitionedAt),
	)
	return rec, nil
}
