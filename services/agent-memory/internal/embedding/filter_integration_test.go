package embedding

// Integration test for `RecallFilter` (the §9.6a read-side
// gate).  Verifies the "vector excluded until published"
// scenario from implementation-plan.md:502-505 end-to-end:
// a publish row whose latest event is `queued` MUST be
// filtered out by `FilterPublishedPoints`, and the
// `recall_filter_unpublished_total` counter (exposed as
// `RecallMetrics.UnpublishedFiltered`) MUST increment per
// filtered hit.
//
// The test seeds three publish rows with three distinct
// states so the filter is exercised across the full §9.6a
// state-machine surface a recall caller can encounter:
//
//   1. `published`     — fully landed; MUST be returned.
//   2. `queued`        — publisher started but never reached
//                        step 6; MUST be filtered.
//   3. `failed`        — transient outage recorded; MUST be
//                        filtered (recall path would otherwise
//                        return a vector whose Qdrant point
//                        may or may not exist depending on
//                        whether the failure was upsert vs
//                        confirm).
//
// Plus one orphan point_id (no PostgreSQL row at all) so the
// "missing provenance = filter out" corner is locked.
//
// Skips cleanly when AGENT_MEMORY_PG_URL is unset, mirroring
// the convention in publisher_integration_test.go.

import (
	"context"
	"sync/atomic"
	"testing"
)

func TestRecallFilter_filtersUnpublishedAndIncrementsCounter(t *testing.T) {
	fix := openPGFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID, nodePublished := seedMethodNode(t, ctx, fix.app)
	_, nodeQueued := seedMethodNode(t, ctx, fix.app)
	_, nodeFailed := seedMethodNode(t, ctx, fix.app)

	// State 1: drive a real publish to completion via the
	// publisher.  Reaches `published` through the §9.6a
	// 7-step happy path.
	q := &mockQdrant{}
	p := NewPublisher(fix.app, fixedEmbedder{Version: "filter-stub@v1"}, q)
	resPub, err := p.Publish(ctx, PublishRequest{
		NodeID: nodePublished, RepoID: repoID, Kind: NodeKindMethod,
		CanonicalSignature: "test::sig::FilterPublished", Content: "func F() {}",
	})
	if err != nil {
		t.Fatalf("Publish published row: %v", err)
	}

	// State 2: insert a publish row + queued event ONLY,
	// the same shape `TestPublisher_recall_excludesUntilPublished`
	// uses for its raw-SQL probe — the filter sees this
	// as "unpublished, filter out".
	pidQueued, err := NewUUIDv4()
	if err != nil {
		t.Fatalf("NewUUIDv4: %v", err)
	}
	var publishIDQueued string
	if err := fix.app.QueryRowContext(ctx, `
		INSERT INTO embedding_publish
		    (node_id, embedding_model_version, qdrant_point_id)
		VALUES ($1, $2, $3)
		RETURNING publish_id::text
	`, nodeQueued, "filter-stub@v1", pidQueued).Scan(&publishIDQueued); err != nil {
		t.Fatalf("insert queued publish row: %v", err)
	}
	if _, err := fix.app.ExecContext(ctx, `
		INSERT INTO embedding_publish_event (publish_id, event_kind, attempt_index)
		VALUES ($1, 'queued'::embedding_publish_event_kind, 0)
	`, publishIDQueued); err != nil {
		t.Fatalf("insert queued event: %v", err)
	}

	// State 3: drive a publish that fails (Qdrant rejects
	// the upsert).  Reaches `failed` event_kind through the
	// publisher's own §9.6a error path.
	var failCalls atomic.Int32
	qFail := &mockQdrant{
		UpsertFn: func(ctx context.Context, _, _ string, _ []float32, _ map[string]any) error {
			failCalls.Add(1)
			return errFailUpsert
		},
	}
	pFail := NewPublisher(fix.app, fixedEmbedder{Version: "filter-stub@v1"}, qFail)
	resFail, err := pFail.Publish(ctx, PublishRequest{
		NodeID: nodeFailed, RepoID: repoID, Kind: NodeKindMethod,
		CanonicalSignature: "test::sig::FilterFailed", Content: "func F() {}",
	})
	if err == nil {
		t.Fatalf("Publish failed-row: expected ErrAttemptFailed, got nil")
	}
	if resFail.LastEventKind != EventKindFailed {
		t.Fatalf("Publish failed-row LastEventKind = %q; want failed", resFail.LastEventKind)
	}

	// State 4: an orphan point_id that has NO publish row
	// at all.  Could arise if Qdrant was repopulated from
	// a stale backup or if the operator manually upserted
	// a vector.  Filter must drop it.
	orphan, err := NewUUIDv4()
	if err != nil {
		t.Fatalf("NewUUIDv4 orphan: %v", err)
	}

	// Run the filter against all four point ids.
	metrics := &RecallMetrics{}
	filter := NewRecallFilter(fix.app, metrics)

	before := metrics.Snapshot()
	input := []string{
		resPub.QdrantPointID, // published — keep
		pidQueued,            // queued    — drop
		resFail.QdrantPointID,// failed    — drop
		orphan,               // orphan    — drop
	}
	got, err := filter.FilterPublishedPoints(ctx, input)
	if err != nil {
		t.Fatalf("FilterPublishedPoints: %v", err)
	}
	if len(got) != 1 || got[0] != resPub.QdrantPointID {
		t.Fatalf("FilterPublishedPoints = %v; want exactly [%s] (the published point)",
			got, resPub.QdrantPointID)
	}

	// Counter must have moved by exactly 3 — one per
	// filtered hit.  Anything else means the filter
	// double-counted (over-incremented) or silently
	// failed to count one of the three failure modes
	// (queued, failed, orphan).
	after := metrics.Snapshot()
	delta := after.UnpublishedFiltered - before.UnpublishedFiltered
	if delta != 3 {
		t.Fatalf("UnpublishedFiltered delta = %d; want 3 "+
			"(queued + failed + orphan are all unpublished)", delta)
	}

	// Re-run with only the published id.  Counter must NOT
	// move; output must equal the single input.
	beforeSecond := metrics.Snapshot()
	got2, err := filter.FilterPublishedPoints(ctx, []string{resPub.QdrantPointID})
	if err != nil {
		t.Fatalf("FilterPublishedPoints (published only): %v", err)
	}
	if len(got2) != 1 || got2[0] != resPub.QdrantPointID {
		t.Fatalf("second FilterPublishedPoints = %v; want [%s]", got2, resPub.QdrantPointID)
	}
	afterSecond := metrics.Snapshot()
	if afterSecond.UnpublishedFiltered != beforeSecond.UnpublishedFiltered {
		t.Fatalf("counter moved on published-only call: before=%d after=%d",
			beforeSecond.UnpublishedFiltered, afterSecond.UnpublishedFiltered)
	}

	// Empty input must short-circuit with (nil, nil) and
	// no counter movement — the recall path calls the
	// filter even on empty Qdrant hit batches, and we do
	// not want the metric polluted by zero-input calls.
	beforeEmpty := metrics.Snapshot()
	got3, err := filter.FilterPublishedPoints(ctx, nil)
	if err != nil {
		t.Fatalf("FilterPublishedPoints (empty): %v", err)
	}
	if got3 != nil {
		t.Fatalf("empty input must return nil slice; got %v", got3)
	}
	if metrics.Snapshot().UnpublishedFiltered != beforeEmpty.UnpublishedFiltered {
		t.Fatalf("counter moved on empty input")
	}
}

// errFailUpsert is the sentinel used to drive the publisher's
// `failed` event recording in the filter test.  Defined at
// package scope so the test assertion can compare against it
// if needed.
var errFailUpsert = upsertFailErr{}

type upsertFailErr struct{}

func (upsertFailErr) Error() string { return "filter-test: upsert refused" }
