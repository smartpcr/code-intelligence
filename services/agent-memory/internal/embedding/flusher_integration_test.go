package embedding

// Integration test for `Flusher` (the §9.6a background retry
// scanner).  Verifies the "failed publish retries" scenario
// from implementation-plan.md:495-501 end-to-end through the
// flusher path: a publish that fails on first attempt must
// be picked up by the flusher (without the caller holding
// onto the source content) and re-driven to `published`.
//
// The test pins three flusher behaviours:
//
//   1. `failed`-latest row is retried — the flusher invokes
//      `Retry` with the resolver-supplied PublishRequest and
//      the row reaches `published`.
//   2. Stale `queued`-latest row is retried — proves the
//      "publisher crashed mid-attempt" recovery path.
//   3. Fresh `queued`-latest row is NOT touched — proves the
//      threshold gating so an in-flight healthy publish is
//      not stomped by a concurrent retry.
//
// All three scenarios share one flusher instance + one
// resolver, exercised across a single Flush call so the
// per-Flush `Stats` shape is locked too.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

// mapContentResolver is the test-side `ContentResolver` that
// returns canned `PublishRequest`s keyed by publish_id.  A
// resolver entry can be flagged `superseded: true` to drive
// the `ErrSupersededByModel` path.
type mapContentResolver struct {
	entries map[string]resolverEntry
	calls   atomic.Int32
}

type resolverEntry struct {
	req        PublishRequest
	err        error
	superseded bool
}

func (m *mapContentResolver) Resolve(_ context.Context, lookup ContentLookup) (PublishRequest, error) {
	m.calls.Add(1)
	e, ok := m.entries[lookup.PublishID]
	if !ok {
		return PublishRequest{}, fmt.Errorf("no resolver entry for publish_id=%s", lookup.PublishID)
	}
	if e.superseded {
		return PublishRequest{}, ErrSupersededByModel
	}
	if e.err != nil {
		return PublishRequest{}, e.err
	}
	return e.req, nil
}

func TestFlusher_retriesStuckRowsAndRespectsThresholds(t *testing.T) {
	fix := openPGFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID, nodeFailed := seedMethodNode(t, ctx, fix.app)
	_, nodeStaleQueued := seedMethodNode(t, ctx, fix.app)
	_, nodeFreshQueued := seedMethodNode(t, ctx, fix.app)
	_, nodeSuperseded := seedMethodNode(t, ctx, fix.app)

	// Setup #1: drive a real Publish to a `failed` event
	// via a Qdrant mock that errors on the FIRST upsert.
	// The second upsert (from the flusher's Retry) succeeds.
	var upserts atomic.Int32
	q := &mockQdrant{
		UpsertFn: func(ctx context.Context, _, _ string, _ []float32, _ map[string]any) error {
			if upserts.Add(1) == 1 {
				return errors.New("flusher-test: first attempt fails")
			}
			return nil
		},
	}
	p := NewPublisher(fix.app, fixedEmbedder{Version: "flusher-stub@v1"}, q)

	reqFailed := PublishRequest{
		NodeID: nodeFailed, RepoID: repoID, Kind: NodeKindMethod,
		CanonicalSignature: "test::sig::Failed", Content: "func F() {}",
	}
	resFailed, err := p.Publish(ctx, reqFailed)
	if err == nil {
		t.Fatalf("Publish failed-row: expected ErrAttemptFailed; got nil")
	}
	if resFailed.LastEventKind != EventKindFailed {
		t.Fatalf("Publish failed-row LastEventKind = %q; want failed", resFailed.LastEventKind)
	}

	// Setup #2: stale `queued`-latest row.  Insert directly
	// with `created_at = now() - 1h` so it sits above the
	// flusher's queuedAgeThreshold (we pin to 30s below).
	pidStale, err := NewUUIDv4()
	if err != nil {
		t.Fatalf("NewUUIDv4 stale: %v", err)
	}
	var publishIDStale string
	if err := fix.app.QueryRowContext(ctx, `
		INSERT INTO embedding_publish
		    (node_id, embedding_model_version, qdrant_point_id, created_at)
		VALUES ($1, $2, $3, now() - interval '1 hour')
		RETURNING publish_id::text
	`, nodeStaleQueued, "flusher-stub@v1", pidStale).Scan(&publishIDStale); err != nil {
		t.Fatalf("insert stale publish row: %v", err)
	}
	if _, err := fix.app.ExecContext(ctx, `
		INSERT INTO embedding_publish_event (publish_id, event_kind, attempt_index, created_at)
		VALUES ($1, 'queued'::embedding_publish_event_kind, 0, now() - interval '1 hour')
	`, publishIDStale); err != nil {
		t.Fatalf("insert stale queued event: %v", err)
	}
	reqStale := PublishRequest{
		NodeID: nodeStaleQueued, RepoID: repoID, Kind: NodeKindMethod,
		CanonicalSignature: "test::sig::Stale", Content: "func G() {}",
	}

	// Setup #3: fresh `queued`-latest row (created NOW).
	// MUST be skipped by the flusher because the in-flight
	// publish is younger than the threshold.
	pidFresh, err := NewUUIDv4()
	if err != nil {
		t.Fatalf("NewUUIDv4 fresh: %v", err)
	}
	var publishIDFresh string
	if err := fix.app.QueryRowContext(ctx, `
		INSERT INTO embedding_publish
		    (node_id, embedding_model_version, qdrant_point_id)
		VALUES ($1, $2, $3)
		RETURNING publish_id::text
	`, nodeFreshQueued, "flusher-stub@v1", pidFresh).Scan(&publishIDFresh); err != nil {
		t.Fatalf("insert fresh publish row: %v", err)
	}
	if _, err := fix.app.ExecContext(ctx, `
		INSERT INTO embedding_publish_event (publish_id, event_kind, attempt_index)
		VALUES ($1, 'queued'::embedding_publish_event_kind, 0)
	`, publishIDFresh); err != nil {
		t.Fatalf("insert fresh queued event: %v", err)
	}

	// Setup #4: a `failed`-latest row whose resolver
	// declares the model has been bumped — the flusher
	// must record `superseded` and skip Retry.  Insert
	// the row at "1h ago" so it crosses the failed
	// threshold too.
	pidSup, err := NewUUIDv4()
	if err != nil {
		t.Fatalf("NewUUIDv4 superseded: %v", err)
	}
	var publishIDSup string
	if err := fix.app.QueryRowContext(ctx, `
		INSERT INTO embedding_publish
		    (node_id, embedding_model_version, qdrant_point_id, created_at)
		VALUES ($1, $2, $3, now() - interval '1 hour')
		RETURNING publish_id::text
	`, nodeSuperseded, "old-model@v0", pidSup).Scan(&publishIDSup); err != nil {
		t.Fatalf("insert superseded publish row: %v", err)
	}
	if _, err := fix.app.ExecContext(ctx, `
		INSERT INTO embedding_publish_event (publish_id, event_kind, attempt_index, created_at)
		VALUES ($1, 'failed'::embedding_publish_event_kind, 0, now() - interval '1 hour')
	`, publishIDSup); err != nil {
		t.Fatalf("insert superseded failed event: %v", err)
	}

	resolver := &mapContentResolver{
		entries: map[string]resolverEntry{
			resFailed.PublishID: {req: reqFailed},
			publishIDStale:      {req: reqStale},
			publishIDSup:        {superseded: true},
		},
	}

	flusher := NewFlusher(fix.app, p, resolver,
		WithFailedAgeThreshold(0*time.Second), // every failed row qualifies
		WithQueuedAgeThreshold(30*time.Second),
		WithScanLimit(50),
	)

	stats, err := flusher.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Scanned MUST equal 3 (failed + stale-queued +
	// superseded).  Fresh-queued is below the threshold
	// and must NOT appear.
	if stats.Scanned != 3 {
		t.Fatalf("Scanned = %d; want 3 (failed + stale-queued + superseded). Stats=%+v",
			stats.Scanned, stats)
	}
	if stats.Retried != 2 {
		t.Fatalf("Retried = %d; want 2 (failed + stale-queued). Stats=%+v",
			stats.Retried, stats)
	}
	if stats.Superseded != 1 {
		t.Fatalf("Superseded = %d; want 1. Stats=%+v", stats.Superseded, stats)
	}
	if stats.ResolveErrors != 0 || stats.RetryErrors != 0 || stats.RetriedFailed != 0 {
		t.Fatalf("Unexpected non-zero error counts: %+v", stats)
	}

	// Verify the failed row's event log now ends in
	// `published`.
	if latest := readLatestEvent(t, ctx, fix.app, resFailed.PublishID); latest != EventKindPublished {
		t.Fatalf("failed row latest = %q after flush; want published", latest)
	}

	// Verify the stale-queued row now ends in `published`.
	if latest := readLatestEvent(t, ctx, fix.app, publishIDStale); latest != EventKindPublished {
		t.Fatalf("stale-queued row latest = %q after flush; want published", latest)
	}

	// Verify the fresh-queued row was NOT touched (still
	// `queued`, no `failed` / `published` event added).
	if latest := readLatestEvent(t, ctx, fix.app, publishIDFresh); latest != EventKindQueued {
		t.Fatalf("fresh-queued row latest = %q; flusher must NOT retry rows under threshold", latest)
	}
	freshEvents := readEventLog(t, ctx, fix.app, publishIDFresh)
	if len(freshEvents) != 1 {
		t.Fatalf("fresh-queued event log = %+v; want exactly 1 row (the original queued event)",
			freshEvents)
	}

	// Verify the superseded row's log ends in `superseded`
	// and that the flusher did NOT invoke Retry on it
	// (the resolver was the only path that fired).
	if latest := readLatestEvent(t, ctx, fix.app, publishIDSup); latest != EventKindSuperseded {
		t.Fatalf("superseded row latest = %q; want superseded", latest)
	}

	// Cumulative metrics on the flusher: one flush, two
	// retried, one superseded.
	if got := flusher.Metrics().FlushesTotal.Load(); got != 1 {
		t.Fatalf("FlushesTotal = %d; want 1", got)
	}
	if got := flusher.Metrics().RetriedTotal.Load(); got != 2 {
		t.Fatalf("RetriedTotal = %d; want 2", got)
	}
	if got := flusher.Metrics().SupersededTotal.Load(); got != 1 {
		t.Fatalf("SupersededTotal = %d; want 1", got)
	}

	// Resolver was called for every scanned row that
	// passed the Flusher-side supersede pre-check.  The
	// pre-check short-circuits `publishIDSup` (whose row
	// records `old-model@v0` while the publisher is now
	// on `flusher-stub@v1`) BEFORE the resolver — so only
	// the failed + stale-queued rows reach `Resolve`.
	// This is the iter-4 behaviour change that closes the
	// "model bump churn forever" risk: a stale-model row
	// no longer materializes its body just to be retired.
	if got := resolver.calls.Load(); got != 2 {
		t.Fatalf("resolver.calls = %d; want 2 "+
			"(failed + stale-queued; superseded row is "+
			"short-circuited by the Flusher pre-check before "+
			"reaching the resolver)", got)
	}
}

// TestFlusher_resolverErrorDoesNotMutateRow proves that a
// resolver failure (anything other than `ErrSupersededByModel`)
// leaves the publish row in its existing state — the flusher
// does NOT record a fresh `failed` event for the resolve
// failure.  This matches the design intent: the resolver is
// a wiring concern, not a §9.6a state transition.
func TestFlusher_resolverErrorDoesNotMutateRow(t *testing.T) {
	fix := openPGFixture(t)
	defer fix.cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), testDBTimeout)
	defer cancel()

	repoID, nodeID := seedMethodNode(t, ctx, fix.app)

	q := &mockQdrant{
		UpsertFn: func(ctx context.Context, _, _ string, _ []float32, _ map[string]any) error {
			return errors.New("forced failure")
		},
	}
	p := NewPublisher(fix.app, fixedEmbedder{Version: "flusher-resolver@v1"}, q)

	req := PublishRequest{
		NodeID: nodeID, RepoID: repoID, Kind: NodeKindMethod,
		CanonicalSignature: "test::sig::ResolverErr", Content: "func F() {}",
	}
	res, err := p.Publish(ctx, req)
	if err == nil {
		t.Fatalf("Publish: expected ErrAttemptFailed; got nil")
	}
	if res.LastEventKind != EventKindFailed {
		t.Fatalf("Publish LastEventKind = %q; want failed", res.LastEventKind)
	}

	before := readEventLog(t, ctx, fix.app, res.PublishID)

	resolver := &mapContentResolver{
		entries: map[string]resolverEntry{
			res.PublishID: {err: errors.New("resolver unreachable")},
		},
	}
	flusher := NewFlusher(fix.app, p, resolver, WithFailedAgeThreshold(0))

	stats, err := flusher.Flush(ctx)
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if stats.ResolveErrors != 1 {
		t.Fatalf("ResolveErrors = %d; want 1. Stats=%+v", stats.ResolveErrors, stats)
	}
	if stats.Retried != 0 || stats.RetriedFailed != 0 || stats.Superseded != 0 {
		t.Fatalf("unexpected non-zero successful counts: %+v", stats)
	}

	after := readEventLog(t, ctx, fix.app, res.PublishID)
	if len(after) != len(before) {
		t.Fatalf("resolver error mutated event log: before=%+v after=%+v", before, after)
	}
}
