package graphwriter

// Integration tests for the Stage 4.2 trace-observation writer
// (AppendObservedCallTrace, AppendSoloMethodObservation,
// UpsertRepoHealth). Skips cleanly when AGENT_MEMORY_PG_URL is
// unset, mirroring writer_integration_test.go.
//
// Scenarios:
//
//   * AppendObservedCallTrace
//       - first call: edge Inserted=true, log row written,
//         aggregate row created with observation_count=1
//       - second call: edge Inserted=false (same fingerprint),
//         second log row appended, observation_count=2
//       - last_observed_at uses GREATEST (rubber-duck finding #7)
//   * AppendSoloMethodObservation
//       - first / repeated count semantics
//       - method_node_id must exist
//   * UpsertRepoHealth
//       - first INSERT: Inserted=true, since=ObservedAt
//       - second UPSERT same-state: Inserted=false, since unchanged
//       - state transition: since bumps to new ObservedAt
//       - CHECK constraint rejects Degraded=true with NULL reason
//         (via the writer's pre-validation: the value never reaches PG)

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

const observedCallsKind = "observed_calls"

// seedTwoMethods creates two Method-kind nodes in the same repo
// so the Edge endpoint same-repo guard is satisfied.
func seedTwoMethods(t *testing.T, ctx context.Context, w *Writer) (RepoRecord, NodeRecord, NodeRecord) {
	t.Helper()
	repo := seedRepo(t, ctx, w)
	src, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.Trace.Src#run()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed src method: %v", err)
	}
	dst, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.Trace.Dst#run()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed dst method: %v", err)
	}
	return repo, src, dst
}

func TestAppendObservedCallTrace_singleTxInsertEdgeLogAndAggregate(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo, src, dst := seedTwoMethods(t, ctx, w)

	now := time.Now().UTC().Truncate(time.Microsecond)
	in := EdgeInput{
		RepoID: repo.ID,
		// Kind blank by design; the writer pins it.
		SrcNodeID: src.NodeID, DstNodeID: dst.NodeID,
		FromSHA: "observed",
	}
	obs := ObservationInput{
		TraceID: "trace-abc", SpanID: "span-1",
		StartedAt: now, DurationMs: 12.5,
		P50LatencyMs: 12.5, P95LatencyMs: 12.5,
	}
	rec, err := w.AppendObservedCallTrace(ctx, in, obs)
	if err != nil {
		t.Fatalf("AppendObservedCallTrace first: %v", err)
	}
	if !rec.Edge.Inserted {
		t.Error("first call: Edge.Inserted = false, want true")
	}
	if rec.ObservationCount != 1 {
		t.Errorf("first call: ObservationCount = %d, want 1", rec.ObservationCount)
	}
	if rec.SpanLogID == "" {
		t.Error("SpanLogID empty after first call")
	}

	// Confirm: one edge, one log row, one aggregate row.
	var edgeCount, logCount, aggCount int
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM edge WHERE edge_id = $1`, rec.Edge.EdgeID,
	).Scan(&edgeCount); err != nil {
		t.Fatalf("edge count: %v", err)
	}
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM trace_observation_log WHERE edge_id = $1`, rec.Edge.EdgeID,
	).Scan(&logCount); err != nil {
		t.Fatalf("log count: %v", err)
	}
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM trace_observation WHERE edge_id = $1`, rec.Edge.EdgeID,
	).Scan(&aggCount); err != nil {
		t.Fatalf("agg count: %v", err)
	}
	if edgeCount != 1 || logCount != 1 || aggCount != 1 {
		t.Errorf("post-write row counts: edge=%d log=%d agg=%d; want 1/1/1",
			edgeCount, logCount, aggCount)
	}

	// Second call increments counter, leaves Inserted=false on edge.
	obs2 := obs
	obs2.SpanID = "span-2"
	obs2.StartedAt = now.Add(50 * time.Millisecond)
	obs2.P50LatencyMs = 11.0
	obs2.P95LatencyMs = 14.0
	rec2, err := w.AppendObservedCallTrace(ctx, in, obs2)
	if err != nil {
		t.Fatalf("AppendObservedCallTrace second: %v", err)
	}
	if rec2.Edge.Inserted {
		t.Error("second call: Edge.Inserted = true, want false (idempotent)")
	}
	if rec2.ObservationCount != 2 {
		t.Errorf("second call: ObservationCount = %d, want 2", rec2.ObservationCount)
	}
	if rec.Edge.EdgeID != rec2.Edge.EdgeID {
		t.Errorf("EdgeID changed across calls: %s -> %s", rec.Edge.EdgeID, rec2.Edge.EdgeID)
	}
	// Log table accumulates.
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT count(*) FROM trace_observation_log WHERE edge_id = $1`, rec.Edge.EdgeID,
	).Scan(&logCount); err != nil {
		t.Fatalf("log count post-2nd: %v", err)
	}
	if logCount != 2 {
		t.Errorf("log count = %d after 2 writes, want 2", logCount)
	}

	// Verify aggregate has the latest values.
	var p50, p95 float64
	var latestRef string
	var lastObservedAt time.Time
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT p50_latency_ms, p95_latency_ms, latest_span_ref, last_observed_at
		   FROM trace_observation WHERE edge_id = $1`, rec.Edge.EdgeID,
	).Scan(&p50, &p95, &latestRef, &lastObservedAt); err != nil {
		t.Fatalf("aggregate read: %v", err)
	}
	if p50 != 11.0 || p95 != 14.0 {
		t.Errorf("aggregate latencies: p50=%v p95=%v; want 11/14 (latest wins)", p50, p95)
	}
	if latestRef != "trace-abc:span-2" {
		t.Errorf("latest_span_ref = %q; want trace-abc:span-2", latestRef)
	}
	if !lastObservedAt.Equal(obs2.StartedAt) {
		t.Errorf("last_observed_at = %v; want %v", lastObservedAt, obs2.StartedAt)
	}
}

// TestAppendObservedCallTrace_lastObservedAtUsesGreatest is the
// out-of-order-arrival guard: a second observation whose
// StartedAt is BEFORE the first must NOT roll the aggregate
// timestamp backward.
func TestAppendObservedCallTrace_lastObservedAtUsesGreatest(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo, src, dst := seedTwoMethods(t, ctx, w)
	in := EdgeInput{
		RepoID: repo.ID, SrcNodeID: src.NodeID, DstNodeID: dst.NodeID, FromSHA: "observed",
	}

	t1 := time.Now().UTC().Truncate(time.Microsecond)
	t0 := t1.Add(-5 * time.Minute)

	if _, err := w.AppendObservedCallTrace(ctx, in, ObservationInput{
		TraceID: "trace", SpanID: "newer", StartedAt: t1, DurationMs: 1,
	}); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if _, err := w.AppendObservedCallTrace(ctx, in, ObservationInput{
		TraceID: "trace", SpanID: "older", StartedAt: t0, DurationMs: 2,
	}); err != nil {
		t.Fatalf("second append (out-of-order): %v", err)
	}

	var lastObservedAt time.Time
	var latestRef string
	if err := fix.owner.QueryRowContext(ctx,
		`SELECT t.last_observed_at, t.latest_span_ref
		   FROM trace_observation t
		   JOIN edge e ON e.edge_id = t.edge_id
		  WHERE e.src_node_id = $1 AND e.dst_node_id = $2`,
		src.NodeID, dst.NodeID,
	).Scan(&lastObservedAt, &latestRef); err != nil {
		t.Fatalf("aggregate read: %v", err)
	}
	if !lastObservedAt.Equal(t1) {
		t.Errorf("last_observed_at = %v; want %v (GREATEST guard)", lastObservedAt, t1)
	}
	if latestRef != "trace:newer" {
		t.Errorf("latest_span_ref = %q; want trace:newer (newer SHOULD win)", latestRef)
	}
}

// TestAppendSoloMethodObservation_idempotentIncrementsCounter
// covers the root-span path (no parent in batch).
func TestAppendSoloMethodObservation_idempotentIncrementsCounter(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	method, err := w.InsertNode(ctx, NodeInput{
		RepoID: repo.ID, Kind: "method",
		CanonicalSignature: "pkg.Solo#run()", FromSHA: "deadbeef",
	})
	if err != nil {
		t.Fatalf("seed method: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	obs := ObservationInput{
		TraceID: "trace", SpanID: "span-1",
		StartedAt: now, DurationMs: 7.0,
		P50LatencyMs: 7.0, P95LatencyMs: 7.0,
	}

	rec, err := w.AppendSoloMethodObservation(ctx, method.NodeID, obs)
	if err != nil {
		t.Fatalf("AppendSoloMethodObservation first: %v", err)
	}
	if !rec.Inserted || rec.ObservationCount != 1 {
		t.Errorf("first call: Inserted=%v count=%d; want (true,1)", rec.Inserted, rec.ObservationCount)
	}
	obs.SpanID = "span-2"
	obs.StartedAt = now.Add(time.Millisecond)
	rec2, err := w.AppendSoloMethodObservation(ctx, method.NodeID, obs)
	if err != nil {
		t.Fatalf("AppendSoloMethodObservation second: %v", err)
	}
	if rec2.Inserted || rec2.ObservationCount != 2 {
		t.Errorf("second call: Inserted=%v count=%d; want (false,2)", rec2.Inserted, rec2.ObservationCount)
	}
}

// TestAppendSoloMethodObservation_unknownNodeRejected confirms a
// nonexistent method_node_id surfaces as an error before the
// UPSERT.
func TestAppendSoloMethodObservation_unknownNodeRejected(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	_, err := w.AppendSoloMethodObservation(ctx, "00000000-0000-0000-0000-000000000000", ObservationInput{
		TraceID: "t", SpanID: "s", StartedAt: time.Now().UTC(), DurationMs: 1,
	})
	if err == nil {
		t.Fatal("want error for unknown node_id, got nil")
	}
}

// TestUpsertRepoHealth_transitionsBumpSince walks the state
// machine: insert → same-state UPSERT → transition → another
// transition; verifies `since` semantics each step.
func TestUpsertRepoHealth_transitionsBumpSince(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repo := seedRepo(t, ctx, w)
	repoIDStr := repo.RepoID

	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	a, err := w.UpsertRepoHealth(ctx, HealthInput{
		RepoID: repoIDStr, Degraded: true,
		Reason: "span_ingestor_backpressure", Source: "span-ingestor",
		ObservedAt: t0,
	})
	if err != nil {
		t.Fatalf("first UpsertRepoHealth: %v", err)
	}
	if !a.Inserted || !a.TransitionedAt.Equal(t0) {
		t.Errorf("first: Inserted=%v since=%v; want (true,%v)", a.Inserted, a.TransitionedAt, t0)
	}

	// Same-state UPSERT (degraded→degraded with same reason) -
	// since must NOT bump.
	t1 := t0.Add(10 * time.Second)
	b, err := w.UpsertRepoHealth(ctx, HealthInput{
		RepoID: repoIDStr, Degraded: true,
		Reason: "span_ingestor_backpressure", Source: "span-ingestor",
		ObservedAt: t1,
	})
	if err != nil {
		t.Fatalf("second UpsertRepoHealth: %v", err)
	}
	if b.Inserted {
		t.Error("same-state UPSERT: Inserted=true, want false")
	}
	if !b.TransitionedAt.Equal(t0) {
		t.Errorf("same-state UPSERT: since=%v; want preserved %v", b.TransitionedAt, t0)
	}

	// Transition degraded→healthy: since must bump to t2.
	t2 := t1.Add(20 * time.Second)
	c, err := w.UpsertRepoHealth(ctx, HealthInput{
		RepoID: repoIDStr, Degraded: false,
		Reason: "", Source: "span-ingestor",
		ObservedAt: t2,
	})
	if err != nil {
		t.Fatalf("transition UpsertRepoHealth: %v", err)
	}
	if !c.TransitionedAt.Equal(t2) {
		t.Errorf("transition: since=%v; want %v (bumped)", c.TransitionedAt, t2)
	}
}

// TestUpsertRepoHealth_invariantsRejectedByWriter confirms the
// caller-side guards: Degraded=true with empty Reason, or
// Degraded=false with non-empty Reason, both error WITHOUT
// touching the database (preventing the CHECK constraint
// violation from surfacing as a confusing 23514 to the
// operator).
func TestUpsertRepoHealth_invariantsRejectedByWriter(t *testing.T) {
	fix := openFixture(t)
	defer fix.cleanup()
	w := New(fix.app, slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	_, err := w.UpsertRepoHealth(ctx, HealthInput{
		RepoID: "11111111-2222-3333-4444-555555555555",
		Degraded: true, Reason: "",
		Source: "span-ingestor", ObservedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Error("Degraded=true Reason='': want error")
	}
	_, err = w.UpsertRepoHealth(ctx, HealthInput{
		RepoID: "11111111-2222-3333-4444-555555555555",
		Degraded: false, Reason: "span_ingestor_backpressure",
		Source: "span-ingestor", ObservedAt: time.Now().UTC(),
	})
	if err == nil {
		t.Error("Degraded=false Reason=non-empty: want error")
	}
}

// Silence the unused-const linter when only one test in this
// file references it.
var _ = observedCallsKind
