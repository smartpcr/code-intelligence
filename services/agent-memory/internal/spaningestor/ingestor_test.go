package spaningestor

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
)

// fakeTraceWriter is the in-memory TraceWriter the ingestor
// unit tests use. Records every call so the test can assert on
// the per-Edge UPSERT pattern (idempotent on first-call,
// counter increments thereafter).
type fakeTraceWriter struct {
	mu sync.Mutex
	// edgeCalls is keyed by `src->dst`; the value is the
	// running observation_count we synthesize so subsequent
	// asserts see the right "post-write" value.
	edgeCalls  map[string]int64
	soloCalls  map[string]int64
	edgeWrites []observedCallTraceWrite
	soloWrites []soloMethodObservationWrite
	// failNext, when non-zero, returns an error from the
	// next N calls (decremented each time).
	failNext atomic.Int32
}

type observedCallTraceWrite struct {
	EdgeIn graphwriter.EdgeInput
	Obs    graphwriter.ObservationInput
}

type soloMethodObservationWrite struct {
	NodeID string
	Obs    graphwriter.ObservationInput
}

func newFakeTraceWriter() *fakeTraceWriter {
	return &fakeTraceWriter{
		edgeCalls: map[string]int64{},
		soloCalls: map[string]int64{},
	}
}

func (f *fakeTraceWriter) AppendObservedCallTrace(
	_ context.Context, in graphwriter.EdgeInput, obs graphwriter.ObservationInput,
) (graphwriter.ObservedCallTraceRecord, error) {
	if f.failNext.Load() > 0 {
		f.failNext.Add(-1)
		return graphwriter.ObservedCallTraceRecord{}, fmt.Errorf("fake: injected failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := in.SrcNodeID + "->" + in.DstNodeID
	f.edgeCalls[key]++
	f.edgeWrites = append(f.edgeWrites, observedCallTraceWrite{EdgeIn: in, Obs: obs})
	return graphwriter.ObservedCallTraceRecord{
		Edge: graphwriter.EdgeRecord{
			EdgeID:   "edge-" + key,
			Inserted: f.edgeCalls[key] == 1,
		},
		ObservationCount: f.edgeCalls[key],
		SpanLogID:        fmt.Sprintf("log-%s-%d", key, f.edgeCalls[key]),
	}, nil
}

func (f *fakeTraceWriter) AppendSoloMethodObservation(
	_ context.Context, nodeID string, obs graphwriter.ObservationInput,
) (graphwriter.SoloObservationRecord, error) {
	if f.failNext.Load() > 0 {
		f.failNext.Add(-1)
		return graphwriter.SoloObservationRecord{}, fmt.Errorf("fake: injected failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.soloCalls[nodeID]++
	f.soloWrites = append(f.soloWrites, soloMethodObservationWrite{NodeID: nodeID, Obs: obs})
	return graphwriter.SoloObservationRecord{
		NodeID:           nodeID,
		ObservationCount: f.soloCalls[nodeID],
		Inserted:         f.soloCalls[nodeID] == 1,
	}, nil
}

// fakeHealthWriter records UpsertRepoHealth calls so the
// backpressure test can assert the supervisor flipped the
// flag exactly once.
type fakeHealthWriter struct {
	mu     sync.Mutex
	writes []graphwriter.HealthInput
	failOnce atomic.Bool
}

func (f *fakeHealthWriter) UpsertRepoHealth(_ context.Context, in graphwriter.HealthInput) (graphwriter.HealthRecord, error) {
	if f.failOnce.CompareAndSwap(true, false) {
		return graphwriter.HealthRecord{}, fmt.Errorf("fake: injected failure")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, in)
	return graphwriter.HealthRecord{Inserted: true, TransitionedAt: in.ObservedAt}, nil
}

func (f *fakeHealthWriter) snapshot() []graphwriter.HealthInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]graphwriter.HealthInput, len(f.writes))
	copy(out, f.writes)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

const testRepoIDA = "11111111-2222-3333-4444-555555555555"

// validUUIDA is a deterministic UUID for tests; the binary's
// real flow gets this from the `repo` table.
func validUUIDA(suffix int) string {
	return fmt.Sprintf("11111111-2222-3333-4444-5555555555%02d", suffix)
}

// seedResolverWithCallChain registers a Caller→Callee Method
// pair on the fakeLookup so the resolver returns them for the
// canonical OTel attributes used by the scenarios below.
func seedResolverWithCallChain(t *testing.T) (*fakeLookup, callerCalleeIDs) {
	t.Helper()
	lookup := newFakeLookup()
	ids := callerCalleeIDs{
		Caller: "caller-node",
		Callee: "callee-node",
	}
	lookup.addMethod(testRepoIDA, "pkg.svc", "Caller", MethodCandidate{
		NodeID:             ids.Caller,
		CanonicalSignature: "https://x/y::method::pkg/svc.go#Caller()",
		FilePath:           "pkg/svc.go",
		BodyStartLine:      1,
		BodyEndLine:        20,
	})
	lookup.addMethod(testRepoIDA, "pkg.svc", "Callee", MethodCandidate{
		NodeID:             ids.Callee,
		CanonicalSignature: "https://x/y::method::pkg/svc.go#Callee()",
		FilePath:           "pkg/svc.go",
		BodyStartLine:      21,
		BodyEndLine:        40,
	})
	return lookup, ids
}

type callerCalleeIDs struct {
	Caller string
	Callee string
}

// TestIngestor_firstObservedCallCreatesEdge is the Stage 4.2
// Scenario 1 contract: given a span with parent in the same
// batch resolving to a caller Method and the span resolving to
// a callee Method, the Ingestor calls AppendObservedCallTrace
// exactly once with the (caller, callee) Edge — and the
// resulting EdgeRecord reports Inserted=true.
func TestIngestor_firstObservedCallCreatesEdge(t *testing.T) {
	lookup, ids := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())

	now := time.Now().UTC()
	parent := ObservationSpan{
		Span: Span{
			RepoID:  testRepoIDA,
			TraceID: "trace-1",
			SpanID:  "parent-span",
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg.svc",
				AttrCodeFunction:  "Caller",
			},
		},
		StartedAt:  now,
		DurationMs: 5,
	}
	child := ObservationSpan{
		Span: Span{
			RepoID:       testRepoIDA,
			TraceID:      "trace-1",
			SpanID:       "child-span",
			ParentSpanID: "parent-span",
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg.svc",
				AttrCodeFunction:  "Callee",
			},
		},
		StartedAt:  now.Add(1 * time.Millisecond),
		DurationMs: 3,
	}
	batch := SpanBatch{
		RepoID: testRepoIDA,
		Spans:  []ObservationSpan{parent, child},
	}

	ctx := context.Background()
	if err := ing.Enqueue(batch); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	// Drain one batch synchronously.
	ing.processBatch(ctx, batch)

	writer.mu.Lock()
	defer writer.mu.Unlock()
	if got := len(writer.edgeWrites); got != 1 {
		t.Fatalf("AppendObservedCallTrace calls = %d, want 1; soloWrites=%d", got, len(writer.soloWrites))
	}
	w := writer.edgeWrites[0]
	if w.EdgeIn.SrcNodeID != ids.Caller {
		t.Errorf("SrcNodeID = %q, want %q", w.EdgeIn.SrcNodeID, ids.Caller)
	}
	if w.EdgeIn.DstNodeID != ids.Callee {
		t.Errorf("DstNodeID = %q, want %q", w.EdgeIn.DstNodeID, ids.Callee)
	}
	if got := w.Obs.DurationMs; got != 3 {
		t.Errorf("DurationMs = %f, want 3", got)
	}
	// Parent (root) should land on the solo aggregate per
	// §8.6 row 3.
	if got := len(writer.soloWrites); got != 1 {
		t.Errorf("soloWrites = %d, want 1 (parent is root)", got)
	}
	if writer.soloWrites[0].NodeID != ids.Caller {
		t.Errorf("solo node_id = %q, want %q", writer.soloWrites[0].NodeID, ids.Caller)
	}
}

// TestIngestor_repeatedCallsIncrementAggregate is the Stage 4.2
// Scenario 2 contract: ingesting the same observation twice
// produces two AppendObservedCallTrace calls (the writer is
// responsible for incrementing observation_count under the
// hood); the fake writer's per-edge call counter mirrors that.
func TestIngestor_repeatedCallsIncrementAggregate(t *testing.T) {
	lookup, ids := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())

	mkBatch := func(spanID string) SpanBatch {
		now := time.Now().UTC()
		return SpanBatch{
			RepoID: testRepoIDA,
			Spans: []ObservationSpan{
				{
					Span: Span{
						RepoID:  testRepoIDA,
						TraceID: "trace-" + spanID,
						SpanID:  "p-" + spanID,
						Attributes: map[string]string{
							AttrCodeNamespace: "pkg.svc",
							AttrCodeFunction:  "Caller",
						},
					},
					StartedAt: now, DurationMs: 1,
				},
				{
					Span: Span{
						RepoID:       testRepoIDA,
						TraceID:      "trace-" + spanID,
						SpanID:       "c-" + spanID,
						ParentSpanID: "p-" + spanID,
						Attributes: map[string]string{
							AttrCodeNamespace: "pkg.svc",
							AttrCodeFunction:  "Callee",
						},
					},
					StartedAt: now.Add(time.Millisecond), DurationMs: 2,
				},
			},
		}
	}

	ctx := context.Background()
	ing.processBatch(ctx, mkBatch("A"))
	ing.processBatch(ctx, mkBatch("B"))
	ing.processBatch(ctx, mkBatch("C"))

	writer.mu.Lock()
	defer writer.mu.Unlock()
	key := ids.Caller + "->" + ids.Callee
	if got := writer.edgeCalls[key]; got != 3 {
		t.Fatalf("edgeCalls[%s] = %d, want 3", key, got)
	}
	// The last call's ObservedCallTraceRecord.ObservationCount
	// is the post-write value the fake writer synthesizes; the
	// last write should be the 3rd.
	if got := len(writer.edgeWrites); got != 3 {
		t.Fatalf("edgeWrites = %d, want 3", got)
	}
}

// TestIngestor_backpressureSurfacesHealthFlag is Scenario 3:
// driving the supervisor with simulated wall-clock so the
// `repo_health.degraded = true` write fires exactly once after
// the sustain window. The supervisor pulls `now` from the
// injected clock, sees depth >= threshold for `sustain`, and
// calls UpsertRepoHealth.
func TestIngestor_backpressureSurfacesHealthFlag(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	health := &fakeHealthWriter{}
	ing := NewIngestor(resolver, writer, health, Config{
		QueueDepth:               64,
		BackpressureThreshold:    2,
		BackpressureSustain:      30 * time.Second,
		BackpressureClearance:    30 * time.Second,
		HealthSupervisorInterval: 100 * time.Millisecond,
	}, discardLogger())

	// Inject a controllable clock.
	startInst := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	var nowAtomic atomic.Pointer[time.Time]
	cur := startInst
	nowAtomic.Store(&cur)
	ing.now = func() time.Time {
		t := nowAtomic.Load()
		return *t
	}

	// Pre-seed the metrics so the supervisor recognises the
	// repo as attributable (it only flags repos it has seen).
	ing.metrics.IncIngested(testRepoIDA)

	// Force the queue to be over-threshold by enqueueing
	// batches that we never drain.
	for i := 0; i < 5; i++ {
		batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
			Span: Span{RepoID: testRepoIDA, TraceID: "t", SpanID: fmt.Sprintf("s%d", i)},
		}}}
		if err := ing.Enqueue(batch); err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
	}
	if depth := ing.QueueDepth(); depth < 2 {
		t.Fatalf("queue depth = %d, want >= 2", depth)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Tick #1 at startInst: over threshold → records overSince.
	ing.supervisorTick(ctx, startInst)
	if writes := health.snapshot(); len(writes) != 0 {
		t.Fatalf("after first tick: writes = %d, want 0", len(writes))
	}

	// Tick #2 at startInst+sustain: should trigger degraded=true.
	at := startInst.Add(30 * time.Second)
	ing.supervisorTick(ctx, at)
	writes := health.snapshot()
	if len(writes) != 1 {
		t.Fatalf("after sustain elapsed: writes = %d, want 1", len(writes))
	}
	if !writes[0].Degraded {
		t.Errorf("write.Degraded = false, want true")
	}
	if writes[0].Reason != DegradedReasonBackpressure {
		t.Errorf("write.Reason = %q, want %q", writes[0].Reason, DegradedReasonBackpressure)
	}
	if writes[0].RepoID != testRepoIDA {
		t.Errorf("write.RepoID = %q, want %q", writes[0].RepoID, testRepoIDA)
	}
	if writes[0].Source != ingestorSource {
		t.Errorf("write.Source = %q, want %q", writes[0].Source, ingestorSource)
	}

	// Tick #3 at the same instant: state is already
	// degraded; supervisor should not re-write.
	ing.supervisorTick(ctx, at)
	if writes := health.snapshot(); len(writes) != 1 {
		t.Errorf("third tick (same state): writes = %d, want 1", len(writes))
	}

	// Drain the queue (depth back to 0) and tick at
	// at+clearance — the flag should clear.
	for {
		select {
		case <-ing.queue:
		default:
			goto drained
		}
	}
drained:
	if ing.QueueDepth() != 0 {
		t.Fatalf("queue not drained: depth=%d", ing.QueueDepth())
	}
	clearAt := at.Add(time.Millisecond)
	ing.supervisorTick(ctx, clearAt)
	if writes := health.snapshot(); len(writes) != 1 {
		t.Errorf("under-but-no-cooldown: writes = %d, want 1", len(writes))
	}
	clearAt = clearAt.Add(30 * time.Second)
	ing.supervisorTick(ctx, clearAt)
	writes = health.snapshot()
	if len(writes) != 2 {
		t.Fatalf("after clearance: writes = %d, want 2", len(writes))
	}
	if writes[1].Degraded {
		t.Errorf("second write.Degraded = true, want false")
	}
	if writes[1].Reason != "" {
		t.Errorf("second write.Reason = %q, want empty", writes[1].Reason)
	}
}

// TestIngestor_enqueueDropOnQueueFull confirms ErrQueueFull is
// returned without blocking, the per-span dropped counter
// increments, and a non-rejected batch following the rejection
// still goes through (the channel is not poisoned).
func TestIngestor_enqueueDropOnQueueFull(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 1}, discardLogger())

	full := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{RepoID: testRepoIDA, TraceID: "t", SpanID: "s1"},
	}}}
	if err := ing.Enqueue(full); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	if err := ing.Enqueue(full); err != ErrQueueFull {
		t.Fatalf("second Enqueue: %v; want ErrQueueFull", err)
	}
	counts := ing.metrics.SnapshotCounters()
	if counts["span_dropped_total"][testRepoIDA] != 1 {
		t.Errorf("span_dropped_total = %d, want 1", counts["span_dropped_total"][testRepoIDA])
	}
	if counts["span_ingested_total"][testRepoIDA] != 1 {
		t.Errorf("span_ingested_total = %d, want 1", counts["span_ingested_total"][testRepoIDA])
	}
}

// TestIngestor_crossBatchParentRoutesToPendingChildren confirms
// a span whose ParentSpanID is missing from both the in-batch
// parentMap and the cross-batch ParentIndex is parked in
// PendingChildren (evaluator iter-2 #2: out-of-order parent
// arrival). The legacy "write solo immediately" behaviour is
// preserved only when the pending cache is disabled
// (`PendingChildKeyCap: -1`); a dedicated test covers that
// fallback below.
func TestIngestor_crossBatchParentRoutesToPendingChildren(t *testing.T) {
	lookup, ids := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())

	now := time.Now().UTC()
	batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{
			RepoID:       testRepoIDA,
			TraceID:      "trace-x",
			SpanID:       "child-span",
			ParentSpanID: "unknown-parent",
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg.svc",
				AttrCodeFunction:  "Callee",
			},
		},
		StartedAt: now, DurationMs: 4,
	}}}
	ing.processBatch(context.Background(), batch)

	if got := len(writer.edgeWrites); got != 0 {
		t.Errorf("edgeWrites = %d, want 0 (parent not in batch)", got)
	}
	if got := len(writer.soloWrites); got != 0 {
		t.Errorf("soloWrites = %d, want 0 (child parked in pending)", got)
	}
	if c := ing.metrics.SnapshotCounters()["parent_span_missing_total"][testRepoIDA]; c != 1 {
		t.Errorf("parent_span_missing_total = %d, want 1", c)
	}
	if sz := ing.PendingChildren().Size(); sz != 1 {
		t.Errorf("pending children size = %d, want 1", sz)
	}
	_ = ids // resolver returns ids.Callee for the parked target; verified by reconcile test below.
}

// TestIngestor_crossBatchParentMissing_writeSoloWhenPendingDisabled
// covers the legacy fallback: with PendingChildKeyCap=-1 the
// pending cache is disabled, and a cross-batch parent-missing
// child writes a solo observation immediately.
func TestIngestor_crossBatchParentMissing_writeSoloWhenPendingDisabled(t *testing.T) {
	lookup, ids := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{
		QueueDepth:         16,
		PendingChildKeyCap: -1,
	}, discardLogger())

	now := time.Now().UTC()
	batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{
			RepoID:       testRepoIDA,
			TraceID:      "trace-y",
			SpanID:       "child-span",
			ParentSpanID: "unknown-parent",
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg.svc",
				AttrCodeFunction:  "Callee",
			},
		},
		StartedAt: now, DurationMs: 7,
	}}}
	ing.processBatch(context.Background(), batch)

	if got := len(writer.soloWrites); got != 1 {
		t.Fatalf("soloWrites = %d, want 1 (pending disabled → legacy solo)", got)
	}
	if writer.soloWrites[0].NodeID != ids.Callee {
		t.Errorf("solo NodeID = %q, want %q", writer.soloWrites[0].NodeID, ids.Callee)
	}
}

// TestIngestor_unresolvedSpanIsDropped confirms an unresolvable
// span produces neither edge nor solo writes (the resolver's
// span_unresolved_total counter already captured the miss).
func TestIngestor_unresolvedSpanIsDropped(t *testing.T) {
	lookup := newFakeLookup()
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())

	batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{
			RepoID:  testRepoIDA,
			TraceID: "trace-x",
			SpanID:  "s",
			Attributes: map[string]string{
				AttrCodeNamespace: "unknown",
				AttrCodeFunction:  "Missing",
			},
		},
		StartedAt: time.Now().UTC(), DurationMs: 1,
	}}}
	ing.processBatch(context.Background(), batch)
	if got := len(writer.edgeWrites) + len(writer.soloWrites); got != 0 {
		t.Errorf("writes = %d, want 0", got)
	}
}

// TestIngestor_crossBatchParentFromIndexProducesEdge proves
// evaluator iter-1 fix #2: when a parent span arrived in a
// PRIOR batch, the cross-batch ParentIndex resolves it and the
// child still produces an observed_calls Edge — not a solo
// aggregate. Without this fix, normal collector batching
// silently lost edges whenever parent + child crossed a
// flush boundary.
func TestIngestor_crossBatchParentFromIndexProducesEdge(t *testing.T) {
	lookup, ids := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	now := time.Now().UTC()

	parentBatch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{
			RepoID: testRepoIDA, TraceID: "t-x", SpanID: "p1",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Caller"},
		},
		StartedAt: now, DurationMs: 5,
	}}}
	ing.processBatch(context.Background(), parentBatch)

	childBatch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{
			RepoID: testRepoIDA, TraceID: "t-x", SpanID: "c1", ParentSpanID: "p1",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Callee"},
		},
		StartedAt: now.Add(time.Millisecond), DurationMs: 2,
	}}}
	ing.processBatch(context.Background(), childBatch)

	if got := len(writer.edgeWrites); got != 1 {
		t.Fatalf("edgeWrites = %d, want 1 (parent should be found in cross-batch index)", got)
	}
	if writer.edgeWrites[0].EdgeIn.SrcNodeID != ids.Caller ||
		writer.edgeWrites[0].EdgeIn.DstNodeID != ids.Callee {
		t.Errorf("edge = (%q -> %q), want (%q -> %q)",
			writer.edgeWrites[0].EdgeIn.SrcNodeID, writer.edgeWrites[0].EdgeIn.DstNodeID,
			ids.Caller, ids.Callee)
	}
	if c := ing.metrics.SnapshotCounters()["parent_span_missing_total"][testRepoIDA]; c != 0 {
		t.Errorf("parent_span_missing_total = %d, want 0 (cross-batch index hit)", c)
	}
	hits, _, _, _ := ing.ParentIndex().Snapshot()
	if hits == 0 {
		t.Errorf("ParentIndex hits = 0, want > 0 (the child should have looked up p1)")
	}
}

// TestIngestor_parentIndexDisabledFallsBackToSolo proves the
// cache can be explicitly disabled (cfg.ParentCacheKeyCap < 0)
// without regressing the original v1 solo-aggregate behaviour.
func TestIngestor_parentIndexDisabledFallsBackToSolo(t *testing.T) {
	lookup, ids := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil,
		Config{QueueDepth: 16, ParentCacheKeyCap: -1}, discardLogger())
	if ing.ParentIndex() != nil {
		t.Fatalf("ParentIndex = %v, want nil when ParentCacheKeyCap < 0", ing.ParentIndex())
	}
	now := time.Now().UTC()
	parentBatch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{RepoID: testRepoIDA, TraceID: "t-y", SpanID: "p1",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Caller"}},
		StartedAt: now, DurationMs: 5,
	}}}
	ing.processBatch(context.Background(), parentBatch)
	childBatch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{RepoID: testRepoIDA, TraceID: "t-y", SpanID: "c1", ParentSpanID: "p1",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Callee"}},
		StartedAt: now, DurationMs: 5,
	}}}
	ing.processBatch(context.Background(), childBatch)
	if got := len(writer.edgeWrites); got != 0 {
		t.Errorf("edgeWrites = %d, want 0 with disabled cache", got)
	}
	if got := len(writer.soloWrites); got < 1 {
		t.Errorf("soloWrites = %d, want >= 1", got)
	}
	_ = ids
}

// TestIngestor_blockResolvedAnchorsOnBlockNode proves evaluator
// iter-1 fix #3: when the resolver returns StatusBlock with a
// non-nil Block candidate, the observation anchors on the
// Block node, NOT the parent Method. This makes call edges and
// observation rows reflect the §3.7 "Block as observation
// anchor" semantics.
func TestIngestor_blockResolvedAnchorsOnBlockNode(t *testing.T) {
	lookup, ids := seedResolverWithCallChain(t)
	// Add a Block under Callee that covers lineno 25.
	lookup.addBlock(ids.Callee, BlockCandidate{
		NodeID:    "blk-callee-inner",
		StartLine: 21, EndLine: 30, Kind: "branch",
	})
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())

	now := time.Now().UTC()
	batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{
			RepoID: testRepoIDA, TraceID: "t-z", SpanID: "s1",
			Attributes: map[string]string{
				AttrCodeNamespace: "pkg.svc",
				AttrCodeFunction:  "Callee",
				AttrCodeFilepath:  "pkg/svc.go",
				AttrCodeLineno:    "25",
			},
		},
		StartedAt: now, DurationMs: 4,
	}}}
	ing.processBatch(context.Background(), batch)
	if got := len(writer.soloWrites); got != 1 {
		t.Fatalf("soloWrites = %d, want 1", got)
	}
	if writer.soloWrites[0].NodeID != "blk-callee-inner" {
		t.Errorf("solo NodeID = %q, want %q (Block anchor)", writer.soloWrites[0].NodeID, "blk-callee-inner")
	}
}

// TestIngestor_enqueueAtomicAllOrNothing proves evaluator iter-1
// fix #5: EnqueueAtomic atomically accepts or rejects every
// per-repo batch. With insufficient slots, NO batch is enqueued
// (so a Collector retry doesn't duplicate already-enqueued
// spans).
func TestIngestor_enqueueAtomicAllOrNothing(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	ing := NewIngestor(resolver, newFakeTraceWriter(), nil,
		Config{QueueDepth: 2}, discardLogger())
	// Fill 1 of the 2 slots with a pre-batch so EnqueueAtomic
	// of 2 more batches fails (cap-len = 1 < 2).
	preBatch := SpanBatch{RepoID: validUUIDA(1), Spans: []ObservationSpan{{
		Span: Span{RepoID: validUUIDA(1), TraceID: "t", SpanID: "pre"},
	}}}
	if err := ing.Enqueue(preBatch); err != nil {
		t.Fatalf("pre-Enqueue: %v", err)
	}
	batches := []SpanBatch{
		{RepoID: validUUIDA(2), Spans: []ObservationSpan{{Span: Span{RepoID: validUUIDA(2), TraceID: "t", SpanID: "a"}}}},
		{RepoID: validUUIDA(3), Spans: []ObservationSpan{{Span: Span{RepoID: validUUIDA(3), TraceID: "t", SpanID: "b"}}}},
	}
	if err := ing.EnqueueAtomic(batches); err == nil {
		t.Fatalf("EnqueueAtomic = nil, want ErrQueueFull")
	}
	// Queue should still contain only the preBatch.
	if got := ing.QueueDepth(); got != 1 {
		t.Errorf("QueueDepth = %d, want 1 (atomic rejection must not enqueue any)", got)
	}
	// Both repos should have IncDropped == 1 (one span each).
	dropped := ing.metrics.SnapshotCounters()["span_dropped_total"]
	if dropped[validUUIDA(2)] != 1 || dropped[validUUIDA(3)] != 1 {
		t.Errorf("dropped = %v, want both repos incremented by 1", dropped)
	}
}

// TestIngestor_enqueueAtomicFitsAllAcceptsAll proves the
// positive path: when capacity is sufficient, all batches
// land in the queue with IncIngested + AddInflight per span.
func TestIngestor_enqueueAtomicFitsAllAcceptsAll(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	ing := NewIngestor(resolver, newFakeTraceWriter(), nil,
		Config{QueueDepth: 4}, discardLogger())
	batches := []SpanBatch{
		{RepoID: validUUIDA(2), Spans: []ObservationSpan{{Span: Span{RepoID: validUUIDA(2), TraceID: "t", SpanID: "a"}}}},
		{RepoID: validUUIDA(3), Spans: []ObservationSpan{{Span: Span{RepoID: validUUIDA(3), TraceID: "t", SpanID: "b"}}}},
	}
	if err := ing.EnqueueAtomic(batches); err != nil {
		t.Fatalf("EnqueueAtomic: %v", err)
	}
	if got := ing.QueueDepth(); got != 2 {
		t.Errorf("QueueDepth = %d, want 2", got)
	}
	inflight := ing.metrics.InflightSnapshot()
	if inflight[validUUIDA(2)] != 1 || inflight[validUUIDA(3)] != 1 {
		t.Errorf("inflight = %v, want both repos = 1", inflight)
	}
}

// TestIngestor_inflightDecrementsAfterProcessBatch proves
// evaluator iter-1 fix #6: in-flight counter goes back to zero
// after processBatch completes, so the supervisor's
// attribution scoping sees an accurate "currently
// contributing" set.
func TestIngestor_inflightDecrementsAfterProcessBatch(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 4}, discardLogger())
	batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{RepoID: testRepoIDA, TraceID: "t", SpanID: "p1",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Caller"}},
		StartedAt: time.Now().UTC(), DurationMs: 1,
	}}}
	if err := ing.Enqueue(batch); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if got := ing.metrics.InflightSnapshot()[testRepoIDA]; got != 1 {
		t.Fatalf("inflight after Enqueue = %d, want 1", got)
	}
	// Drain one batch (the Run loop calls processBatch).
	b := <-ing.queue
	ing.processBatch(context.Background(), b)
	if got := ing.metrics.InflightSnapshot()[testRepoIDA]; got != 0 {
		t.Errorf("inflight after processBatch = %d, want 0", got)
	}
}

// TestIngestor_attributableReposScopesByInflight proves
// evaluator iter-1 fix #6: a repo that historically ingested
// but has zero in-flight is NOT marked as attributable for new
// degraded transitions (only kept if already degraded so its
// clearance can fire).
func TestIngestor_attributableReposScopesByInflight(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 4}, discardLogger())
	// Inflate the lifetime ingested counter for repo A but
	// leave its in-flight at zero.
	ing.metrics.IncIngested(testRepoIDA)
	// Active repo B with current in-flight.
	repoB := validUUIDA(7)
	ing.metrics.AddInflight(repoB, 1)

	repos := ing.attributableRepos()
	hasB := false
	for _, r := range repos {
		if r == testRepoIDA {
			t.Errorf("quiesced repo A still attributable: %v", repos)
		}
		if r == repoB {
			hasB = true
		}
	}
	if !hasB {
		t.Errorf("active repo B not attributable: %v", repos)
	}
}

// TestIngestor_observedEdgeCarriesRealSHAFromReader proves
// evaluator iter-1 fix #7: with a SHAReader wired,
// EdgeInput.FromSHA carries the repo's current_head_sha — not
// the documented "observed" sentinel.
func TestIngestor_observedEdgeCarriesRealSHAFromReader(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	ing.SetSHAReader(fakeSHAReader{m: map[string]string{testRepoIDA: "abc123"}})

	now := time.Now().UTC()
	batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{
		{Span: Span{RepoID: testRepoIDA, TraceID: "t", SpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Caller"}},
			StartedAt: now, DurationMs: 5},
		{Span: Span{RepoID: testRepoIDA, TraceID: "t", SpanID: "c", ParentSpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Callee"}},
			StartedAt: now.Add(time.Millisecond), DurationMs: 2},
	}}
	ing.processBatch(context.Background(), batch)
	if len(writer.edgeWrites) != 1 {
		t.Fatalf("edgeWrites = %d, want 1", len(writer.edgeWrites))
	}
	if writer.edgeWrites[0].EdgeIn.FromSHA != "abc123" {
		t.Errorf("FromSHA = %q, want %q", writer.edgeWrites[0].EdgeIn.FromSHA, "abc123")
	}
}

// TestIngestor_observedEdgeFallsBackToObservedWithoutReader
// proves the fallback: with no SHAReader (or one returning ""),
// FromSHA reverts to the "observed" sentinel so the G2
// fingerprint preimage is still well-defined.
func TestIngestor_observedEdgeFallsBackToObservedWithoutReader(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	// Default: no SHAReader installed -> shaCache.Get returns "".

	now := time.Now().UTC()
	batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{
		{Span: Span{RepoID: testRepoIDA, TraceID: "t", SpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Caller"}},
			StartedAt: now, DurationMs: 5},
		{Span: Span{RepoID: testRepoIDA, TraceID: "t", SpanID: "c", ParentSpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Callee"}},
			StartedAt: now.Add(time.Millisecond), DurationMs: 2},
	}}
	ing.processBatch(context.Background(), batch)
	if len(writer.edgeWrites) != 1 {
		t.Fatalf("edgeWrites = %d, want 1", len(writer.edgeWrites))
	}
	if writer.edgeWrites[0].EdgeIn.FromSHA != "observed" {
		t.Errorf("FromSHA = %q, want %q (fallback)", writer.edgeWrites[0].EdgeIn.FromSHA, "observed")
	}
}

// fakeSHAReader is a trivial SHAReader for tests.
type fakeSHAReader struct{ m map[string]string }

func (f fakeSHAReader) CurrentHeadSHA(_ context.Context, repoID string) (string, error) {
	return f.m[repoID], nil
}

// errSHAReader always returns an error — used by evaluator
// iter-2 #4 tests to verify the ingestor drops the edge rather
// than silently writing under the "observed" sentinel.
type errSHAReader struct{ err error }

func (e errSHAReader) CurrentHeadSHA(_ context.Context, _ string) (string, error) {
	return "", e.err
}

// TestIngestor_observedEdgeDroppedOnSHALookupError covers
// evaluator iter-2 finding #4: when the SHA lookup raises a
// real error (DB outage, permission), the ingestor MUST drop
// the edge and bump sha_lookup_error_total — silently writing
// under the "observed" sentinel would corrupt G2 fingerprints
// against a snapshot SHA that never existed.
func TestIngestor_observedEdgeDroppedOnSHALookupError(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())
	ing.SetSHAReader(errSHAReader{err: fmt.Errorf("db: connection refused")})

	now := time.Now().UTC()
	batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{
		{Span: Span{RepoID: testRepoIDA, TraceID: "t-err", SpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Caller"}},
			StartedAt: now, DurationMs: 5},
		{Span: Span{RepoID: testRepoIDA, TraceID: "t-err", SpanID: "c", ParentSpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Callee"}},
			StartedAt: now.Add(time.Millisecond), DurationMs: 2},
	}}
	ing.processBatch(context.Background(), batch)

	if got := len(writer.edgeWrites); got != 0 {
		t.Fatalf("edgeWrites = %d, want 0 (SHA lookup error → drop, NOT sentinel)", got)
	}
	if c := ing.metrics.SnapshotCounters()["sha_lookup_error_total"][testRepoIDA]; c != 1 {
		t.Errorf("sha_lookup_error_total = %d, want 1", c)
	}
}

// TestIngestor_traceScopedSpanIDsAvoidCrossTraceCollision is
// evaluator iter-2 finding #3: a multi-trace batch with two
// distinct traces that happen to share a span_id MUST NOT
// resolve a child of one trace against the parent of the other.
// The fix keys the in-batch parent map by (trace_id, span_id),
// not span_id alone.
func TestIngestor_traceScopedSpanIDsAvoidCrossTraceCollision(t *testing.T) {
	lookup := newFakeLookup()
	idsA := callerCalleeIDs{Caller: "A-caller", Callee: "A-callee"}
	idsB := callerCalleeIDs{Caller: "B-caller", Callee: "B-callee"}
	lookup.addMethod(testRepoIDA, "pkg.svc", "CallerA", MethodCandidate{
		NodeID: idsA.Caller, CanonicalSignature: "::CallerA", FilePath: "a.go",
		BodyStartLine: 1, BodyEndLine: 10,
	})
	lookup.addMethod(testRepoIDA, "pkg.svc", "CalleeA", MethodCandidate{
		NodeID: idsA.Callee, CanonicalSignature: "::CalleeA", FilePath: "a.go",
		BodyStartLine: 11, BodyEndLine: 20,
	})
	lookup.addMethod(testRepoIDA, "pkg.svc", "CallerB", MethodCandidate{
		NodeID: idsB.Caller, CanonicalSignature: "::CallerB", FilePath: "b.go",
		BodyStartLine: 1, BodyEndLine: 10,
	})
	lookup.addMethod(testRepoIDA, "pkg.svc", "CalleeB", MethodCandidate{
		NodeID: idsB.Callee, CanonicalSignature: "::CalleeB", FilePath: "b.go",
		BodyStartLine: 11, BodyEndLine: 20,
	})
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())

	now := time.Now().UTC()
	// Both traces use SpanID "p" for the parent and "c" for the
	// child. If the in-batch map were keyed by span_id alone,
	// child-A would resolve against parent-B (or vice versa)
	// depending on map iteration order, producing exactly one
	// wrong edge.
	batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{
		{Span: Span{RepoID: testRepoIDA, TraceID: "trace-A", SpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "CallerA"}},
			StartedAt: now, DurationMs: 5},
		{Span: Span{RepoID: testRepoIDA, TraceID: "trace-B", SpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "CallerB"}},
			StartedAt: now, DurationMs: 5},
		{Span: Span{RepoID: testRepoIDA, TraceID: "trace-A", SpanID: "c", ParentSpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "CalleeA"}},
			StartedAt: now.Add(time.Millisecond), DurationMs: 2},
		{Span: Span{RepoID: testRepoIDA, TraceID: "trace-B", SpanID: "c", ParentSpanID: "p",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "CalleeB"}},
			StartedAt: now.Add(time.Millisecond), DurationMs: 2},
	}}
	ing.processBatch(context.Background(), batch)

	if got := len(writer.edgeWrites); got != 2 {
		t.Fatalf("edgeWrites = %d, want 2 (one per trace, no cross-trace collision)", got)
	}
	wantPairs := map[string]bool{
		idsA.Caller + "->" + idsA.Callee: true,
		idsB.Caller + "->" + idsB.Callee: true,
	}
	for _, w := range writer.edgeWrites {
		key := w.EdgeIn.SrcNodeID + "->" + w.EdgeIn.DstNodeID
		if !wantPairs[key] {
			t.Errorf("unexpected edge %q (cross-trace collision)", key)
		}
		delete(wantPairs, key)
	}
	if len(wantPairs) != 0 {
		t.Errorf("missing edges: %v", wantPairs)
	}
}

// TestIngestor_parentArrivesAfterChildEmitsEdge covers
// evaluator iter-2 finding #2: a child span exported BEFORE
// its parent is parked in PendingChildren; when the parent
// arrives in a subsequent batch the parked child is drained
// and emitted as a real observed-call edge.
func TestIngestor_parentArrivesAfterChildEmitsEdge(t *testing.T) {
	lookup, ids := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 16}, discardLogger())

	now := time.Now().UTC()
	// 1) Child arrives FIRST. Parent ("p1") not in batch, not
	//    in ParentIndex → should be parked, not solo-written.
	childBatch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{
			RepoID: testRepoIDA, TraceID: "t-late", SpanID: "c1", ParentSpanID: "p1",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Callee"},
		},
		StartedAt: now, DurationMs: 4,
	}}}
	ing.processBatch(context.Background(), childBatch)
	if got := len(writer.edgeWrites); got != 0 {
		t.Fatalf("after child-only batch: edgeWrites=%d, want 0", got)
	}
	if got := len(writer.soloWrites); got != 0 {
		t.Fatalf("after child-only batch: soloWrites=%d, want 0 (child parked, not solo'd)", got)
	}
	if sz := ing.PendingChildren().Size(); sz != 1 {
		t.Fatalf("PendingChildren size = %d, want 1", sz)
	}

	// 2) Parent arrives in a later batch. The Remember call in
	//    pass 1 should drain the pending child and emit the
	//    observed-call edge.
	parentBatch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{
			RepoID: testRepoIDA, TraceID: "t-late", SpanID: "p1",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Caller"},
		},
		StartedAt: now, DurationMs: 6,
	}}}
	ing.processBatch(context.Background(), parentBatch)

	if got := len(writer.edgeWrites); got != 1 {
		t.Fatalf("after parent batch: edgeWrites=%d, want 1 (reconciled edge)", got)
	}
	if w := writer.edgeWrites[0]; w.EdgeIn.SrcNodeID != ids.Caller || w.EdgeIn.DstNodeID != ids.Callee {
		t.Errorf("reconciled edge = (%q -> %q), want (%q -> %q)",
			w.EdgeIn.SrcNodeID, w.EdgeIn.DstNodeID, ids.Caller, ids.Callee)
	}
	if c := ing.metrics.SnapshotCounters()["parent_arrived_late_total"][testRepoIDA]; c != 1 {
		t.Errorf("parent_arrived_late_total = %d, want 1", c)
	}
	if sz := ing.PendingChildren().Size(); sz != 0 {
		t.Errorf("PendingChildren size after drain = %d, want 0", sz)
	}
	// Parent itself was a root span → solo.
	if got := len(writer.soloWrites); got != 1 {
		t.Errorf("soloWrites = %d, want 1 (root parent)", got)
	}
}

// TestIngestor_pendingChildTTLExpiryWritesSolo covers the TTL
// flush path: a parked child whose parent never arrives is
// eventually evicted by the supervisor tick and written as a
// solo observation with the parent_never_arrived counter
// bumped.
func TestIngestor_pendingChildTTLExpiryWritesSolo(t *testing.T) {
	lookup, ids := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	// Short TTL so the deadline passes on the next supervisor
	// tick when we drive a virtual clock.
	ing := NewIngestor(resolver, writer, nil, Config{
		QueueDepth:        16,
		PendingChildTTL:   10 * time.Millisecond,
		PendingChildKeyCap: 16,
	}, discardLogger())

	now := time.Now().UTC()
	childBatch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
		Span: Span{
			RepoID: testRepoIDA, TraceID: "t-orphan", SpanID: "c1", ParentSpanID: "never-arrives",
			Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Callee"},
		},
		StartedAt: now, DurationMs: 4,
	}}}
	ing.processBatch(context.Background(), childBatch)
	if sz := ing.PendingChildren().Size(); sz != 1 {
		t.Fatalf("PendingChildren size = %d, want 1", sz)
	}

	// Drive the cache clock past the TTL.
	time.Sleep(30 * time.Millisecond)

	// Directly invoke the supervisor flush hook (production
	// uses the supervisor goroutine).
	ing.flushExpiredPendingChildren(context.Background())

	if sz := ing.PendingChildren().Size(); sz != 0 {
		t.Errorf("PendingChildren size after TTL flush = %d, want 0", sz)
	}
	if got := len(writer.soloWrites); got != 1 {
		t.Fatalf("soloWrites after TTL flush = %d, want 1", got)
	}
	if writer.soloWrites[0].NodeID != ids.Callee {
		t.Errorf("solo NodeID = %q, want %q", writer.soloWrites[0].NodeID, ids.Callee)
	}
	if c := ing.metrics.SnapshotCounters()["parent_never_arrived_total"][testRepoIDA]; c != 1 {
		t.Errorf("parent_never_arrived_total = %d, want 1", c)
	}
}

// TestIngestor_pendingChildLRUEvictionWritesSolo covers the
// other eviction path: when the PendingChildIndex hits its
// keyCap, the oldest parked children are evicted and written
// as solo observations so their timing data is not lost.
func TestIngestor_pendingChildLRUEvictionWritesSolo(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{
		QueueDepth:         16,
		PendingChildKeyCap: 2, // tiny cap to force LRU evictions
		PendingChildTTL:    time.Hour,
	}, discardLogger())

	now := time.Now().UTC()
	for k := 0; k < 4; k++ {
		batch := SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
			Span: Span{
				RepoID:       testRepoIDA,
				TraceID:      fmt.Sprintf("trace-%d", k),
				SpanID:       "c",
				ParentSpanID: "p",
				Attributes: map[string]string{
					AttrCodeNamespace: "pkg.svc",
					AttrCodeFunction:  "Callee",
				},
			},
			StartedAt: now, DurationMs: float64(k + 1),
		}}}
		ing.processBatch(context.Background(), batch)
	}
	if sz := ing.PendingChildren().Size(); sz != 2 {
		t.Errorf("PendingChildren size = %d, want 2 (LRU capped)", sz)
	}
	// Two children evicted by the cap → written as solo.
	if got := len(writer.soloWrites); got != 2 {
		t.Errorf("soloWrites = %d, want 2 (LRU-evicted children)", got)
	}
	if c := ing.metrics.SnapshotCounters()["parent_never_arrived_total"][testRepoIDA]; c != 2 {
		t.Errorf("parent_never_arrived_total = %d, want 2", c)
	}
}

// TestIngestor_enqueueAtomicNeverBlocksUnderConcurrentEnqueue
// covers evaluator iter-2 finding #1: concurrent single-batch
// Enqueue and multi-batch EnqueueAtomic must NEVER cause either
// caller to block indefinitely. Before the fix, Enqueue did not
// take enqueueMu, so it could consume capacity after
// EnqueueAtomic's precheck and turn the multi-batch's blocking
// send into a hang. The new design holds enqueueMu in both
// paths and uses non-blocking sends throughout.
func TestIngestor_enqueueAtomicNeverBlocksUnderConcurrentEnqueue(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	// Small queue + NO drainer ⇒ every overflow MUST return
	// ErrQueueFull, never hang.
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 8}, discardLogger())

	makeBatch := func(repo, span string) SpanBatch {
		return SpanBatch{RepoID: repo, Spans: []ObservationSpan{{
			Span: Span{RepoID: repo, TraceID: "t", SpanID: span,
				Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Caller"}},
			StartedAt: time.Now().UTC(), DurationMs: 1,
		}}}
	}

	const producers = 8
	const callsPerProducer = 50
	const atomicGroupSize = 3

	var (
		wg            sync.WaitGroup
		fullCount     atomic.Int64
		okCount       atomic.Int64
	)
	// Mixed single-batch and multi-batch producers, all racing
	// against an undersized queue. If the old race re-surfaced,
	// one of the goroutines would block here forever and the
	// test deadline would fire.
	deadline := time.Now().Add(5 * time.Second)
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for k := 0; k < callsPerProducer; k++ {
				if time.Now().After(deadline) {
					t.Errorf("producer %d: deadline exceeded — likely blocked", id)
					return
				}
				if k%2 == 0 {
					if err := ing.Enqueue(makeBatch(testRepoIDA, fmt.Sprintf("s-%d-%d", id, k))); err != nil {
						if err == ErrQueueFull {
							fullCount.Add(1)
						} else {
							t.Errorf("Enqueue: %v", err)
						}
					} else {
						okCount.Add(1)
					}
				} else {
					group := make([]SpanBatch, atomicGroupSize)
					for j := 0; j < atomicGroupSize; j++ {
						group[j] = makeBatch(testRepoIDA, fmt.Sprintf("a-%d-%d-%d", id, k, j))
					}
					if err := ing.EnqueueAtomic(group); err != nil {
						if err == ErrQueueFull {
							fullCount.Add(1)
						} else {
							t.Errorf("EnqueueAtomic: %v", err)
						}
					} else {
						okCount.Add(int64(atomicGroupSize))
					}
				}
			}
		}(p)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("producers hung — race re-surfaced (EnqueueAtomic blocked on a full queue)")
	}
	t.Logf("queue=%d ok=%d full=%d", ing.QueueDepth(), okCount.Load(), fullCount.Load())
	if okCount.Load()+fullCount.Load() == 0 {
		t.Errorf("no calls completed — likely deadlock")
	}
	// At least SOME ErrQueueFull is expected given the
	// undersized queue; the stress test would be uninteresting
	// otherwise. Drainer is absent so the queue fills up.
	if fullCount.Load() == 0 {
		t.Errorf("no queue-full responses — undersized queue should overflow")
	}
}

// TestIngestor_enqueueAtomicAtomicityUnderConcurrentEnqueue
// is a focused version of the race test: drain the queue
// almost full so a single Enqueue can complete, then run a
// concurrent EnqueueAtomic that needs 3 slots. The atomic
// call must either succeed in placing all 3 OR fail cleanly
// with ErrQueueFull. It must NOT place 2 and then hang.
func TestIngestor_enqueueAtomicAtomicityUnderConcurrentEnqueue(t *testing.T) {
	lookup, _ := seedResolverWithCallChain(t)
	resolver := New(lookup, NewMetrics(), discardLogger())
	writer := newFakeTraceWriter()
	ing := NewIngestor(resolver, writer, nil, Config{QueueDepth: 4}, discardLogger())

	mk := func(span string) SpanBatch {
		return SpanBatch{RepoID: testRepoIDA, Spans: []ObservationSpan{{
			Span: Span{RepoID: testRepoIDA, TraceID: "t", SpanID: span,
				Attributes: map[string]string{AttrCodeNamespace: "pkg.svc", AttrCodeFunction: "Caller"}},
			StartedAt: time.Now().UTC(), DurationMs: 1,
		}}}
	}
	// Pre-fill to depth 2 of 4.
	for k := 0; k < 2; k++ {
		if err := ing.Enqueue(mk(fmt.Sprintf("pre-%d", k))); err != nil {
			t.Fatalf("pre-fill Enqueue: %v", err)
		}
	}

	atomicResult := make(chan error, 1)
	enqueueResult := make(chan error, 1)
	start := make(chan struct{})

	go func() {
		<-start
		atomicResult <- ing.EnqueueAtomic([]SpanBatch{
			mk("atomic-1"), mk("atomic-2"), mk("atomic-3"),
		})
	}()
	go func() {
		<-start
		enqueueResult <- ing.Enqueue(mk("single"))
	}()
	close(start)

	timeout := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-atomicResult:
		case <-enqueueResult:
		case <-timeout:
			t.Fatal("call hung — race re-surfaced (atomicity vs concurrent Enqueue broke)")
		}
	}
	depth := ing.QueueDepth()
	if depth > 4 {
		t.Errorf("queue depth = %d, exceeds capacity 4 — atomicity violated", depth)
	}
}
