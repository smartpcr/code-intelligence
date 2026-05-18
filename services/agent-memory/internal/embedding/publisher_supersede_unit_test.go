package embedding

// Iter-4 fix #1: focused unit coverage for the Method/Block
// publisher's snapshot-supersede plumbing. The Method/Block
// path lives at `Publisher.commitPublishedWithSupersede` +
// `Publisher.runAttempt` step 6+7 (see publisher.go:652-684,
// 835-933) and mirrors the concept-side
// `promoter.commitConceptPublishedWithSupersede` +
// `runAttempt` hook flow that iter-3 already pinned. The
// evaluator's iter-3 finding #1 noted that the concept-side
// coverage exists but the Method/Block side does NOT — a
// regression that broke the Method/Block supersede contract
// would currently slip through. This file closes that gap
// with three helper-level tests (probe-with-supersede,
// probe-without-supersede, probe-no-queued-row) and two
// runAttempt-driven end-to-end tests (hook fires with
// SupersededPublishID on the snapshot path, hook fires with
// empty SupersededPublishID on the non-snapshot path).
//
// These tests are sqlmock-only (no docker-compose dependency)
// so they run as part of `go test ./...` in CI.

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// silentLoggerSupersede discards every Publisher-emitted log
// record so the test runner output stays clean. Suffixed
// with `Supersede` to avoid colliding with a future
// publisher_unit_test.go helper of the same name.
func silentLoggerSupersede() *slog.Logger {
	return slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newSqlmockPublisher constructs a Publisher backed by a
// sqlmock-driven *sql.DB and the supplied stub Embedder /
// Qdrant. The returned cleanup closure asserts all expected
// sqlmock interactions occurred. We bypass NewPublisher's
// nil-arg panic by constructing the struct directly because
// some tests use a stubQdrant whose default
// PointExists returns true (good) but tests that exercise
// the upsert path still want a fresh stub each time.
func newSqlmockPublisher(t *testing.T, embedder Embedder, qdrant Qdrant, opts ...Option) (*Publisher, *sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
		sqlmock.MonitorPingsOption(false),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	p := &Publisher{
		db:       db,
		embedder: embedder,
		qdrant:   qdrant,
		logger:   silentLoggerSupersede(),
		newUUID:  NewUUIDv4,
	}
	for _, opt := range opts {
		opt(p)
	}
	cleanup := func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
	return p, db, mock, cleanup
}

// -----------------------------------------------------------
// Helper-level coverage for commitPublishedWithSupersede.
//
// These three tests pin the §9.6a atomic-supersede contract
// at the helper boundary so a regression in the SQL sequence
// (e.g. dropping the superseded insert when the probe
// returns non-empty, or committing without inserting
// published) breaks a focused, fast unit test rather than
// only surfacing in the heavier integration suite.
// -----------------------------------------------------------

// TestCommitPublishedWithSupersede_emitsSupersedeForPrior
// pins the snapshot-mint path: when the queued event at
// attempt_index=0 carries `supersedes_publish_id`, the
// helper MUST emit a `superseded` event for that prior in
// the SAME tx as the `published` event for the current
// publish, and return the prior publish_id so the caller
// can wire the post-publish hook.
func TestCommitPublishedWithSupersede_emitsSupersedeForPrior(t *testing.T) {
	const (
		newPublishID   = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaa1"
		priorPublishID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbb2"
	)

	p, _, mock, cleanup := newSqlmockPublisher(t, stubEmbedder{}, stubQdrant{})
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs(newPublishID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(priorPublishID))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'published'::embedding_publish_event_kind`).
		WithArgs(newPublishID, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'superseded'::embedding_publish_event_kind`).
		WithArgs(priorPublishID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, err := p.commitPublishedWithSupersede(context.Background(), newPublishID, 0)
	if err != nil {
		t.Fatalf("commitPublishedWithSupersede: %v", err)
	}
	if got != priorPublishID {
		t.Errorf("returned supersededPublishID = %q, want %q (the probed prior publish_id MUST round-trip to the caller so the post-publish hook can populate PublishedEvent.SupersededPublishID)", got, priorPublishID)
	}
}

// TestCommitPublishedWithSupersede_noSupersedeWhenAbsent
// pins the non-snapshot path: a publish whose queued event
// has no `supersedes_publish_id` MUST insert `published`
// only (no superseded insert), commit, and return empty.
func TestCommitPublishedWithSupersede_noSupersedeWhenAbsent(t *testing.T) {
	const newPublishID = "cccccccc-cccc-cccc-cccc-ccccccccccc3"

	p, _, mock, cleanup := newSqlmockPublisher(t, stubEmbedder{}, stubQdrant{})
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs(newPublishID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(""))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'published'::embedding_publish_event_kind`).
		WithArgs(newPublishID, 2).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// NO superseded insert expected.
	mock.ExpectCommit()

	got, err := p.commitPublishedWithSupersede(context.Background(), newPublishID, 2)
	if err != nil {
		t.Fatalf("commitPublishedWithSupersede: %v", err)
	}
	if got != "" {
		t.Errorf("returned supersededPublishID = %q, want empty (non-snapshot publishes MUST NOT carry a supersede id)", got)
	}
}

// TestCommitPublishedWithSupersede_noQueuedRow_treatAsEmpty
// pins the defensive sql.ErrNoRows path: per the helper's
// docstring ("Retry-only callers can theoretically race
// this read in a malformed schema. Treat as no-supersede
// and continue"), an empty result from the probe MUST NOT
// abort the publish; the published event still needs to
// land.
func TestCommitPublishedWithSupersede_noQueuedRow_treatAsEmpty(t *testing.T) {
	const newPublishID = "dddddddd-dddd-dddd-dddd-ddddddddddd4"

	p, _, mock, cleanup := newSqlmockPublisher(t, stubEmbedder{}, stubQdrant{})
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs(newPublishID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'published'::embedding_publish_event_kind`).
		WithArgs(newPublishID, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// NO superseded insert expected.
	mock.ExpectCommit()

	got, err := p.commitPublishedWithSupersede(context.Background(), newPublishID, 0)
	if err != nil {
		t.Fatalf("commitPublishedWithSupersede: %v (sql.ErrNoRows on probe MUST be tolerated)", err)
	}
	if got != "" {
		t.Errorf("returned supersededPublishID = %q, want empty (sql.ErrNoRows on probe → empty supersede)", got)
	}
}

// -----------------------------------------------------------
// runAttempt-driven end-to-end coverage for the post-publish
// hook + SupersededPublishID round-trip.
//
// These two tests drive `Publisher.runAttempt` (the shared
// step-4-through-7 chain used by both Publish and Retry) with
// stubEmbedder + stubQdrant + sqlmock, install
// `WithPostPublishHook(captureFn)`, and assert the captured
// PublishedEvent carries the right SupersededPublishID. This
// mirrors the iter-3 concept-side test
// `promoter.TestRunAttempt_postPublishHookFires_withSupersedeID`
// so Method/Block and Concept publishes share identical
// hook-firing semantics.
//
// We pre-seed the stubQdrant with a successful upsert + the
// PointExists confirm returns true by default — runAttempt's
// step-4b upsert + step-5 confirm both succeed. The only PG
// roundtrips runAttempt makes (beyond commitPublishedWithSupersede)
// are the step-4c vector_written insertEvent — sqlmocked
// here.
// -----------------------------------------------------------

// TestRunAttempt_postPublishHookFires_withSupersedeID is the
// iter-4 fix #1 Method/Block-side hook coverage. Drives the
// full runAttempt success path with a snapshot-mint queued
// event (probe returns prior_publish_id), asserts the
// installed hook fires once with the right PublishID +
// SupersededPublishID + Kind + NodeID + RepoID +
// ModelVersion. Without this assertion, a regression that
// dropped the SupersededPublishID field from the
// PublishedEvent payload (or never fired the hook on the
// supersede path) would silently break
// `snapshot_published_total` for Method/Block publishes.
func TestRunAttempt_postPublishHookFires_withSupersedeID(t *testing.T) {
	const (
		publishID      = "11111111-1111-1111-1111-111111111111"
		priorPublishID = "22222222-2222-2222-2222-222222222222"
		pointID        = "33333333-3333-3333-3333-333333333333"
		nodeID         = "44444444-4444-4444-4444-444444444444"
		repoID         = "55555555-5555-5555-5555-555555555555"
	)

	var (
		captured     PublishedEvent
		captureCount int
		captureMu    sync.Mutex
	)
	hook := func(ev PublishedEvent) {
		captureMu.Lock()
		defer captureMu.Unlock()
		captureCount++
		captured = ev
	}

	p, _, mock, cleanup := newSqlmockPublisher(t, stubEmbedder{}, stubQdrant{}, WithPostPublishHook(hook))
	defer cleanup()

	// runAttempt step 4c: vector_written event.
	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs(publishID, EventKindVectorWritten, 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// runAttempt step 6: commitPublishedWithSupersede.
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs(publishID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(priorPublishID))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'published'::embedding_publish_event_kind`).
		WithArgs(publishID, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'superseded'::embedding_publish_event_kind`).
		WithArgs(priorPublishID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	req := PublishRequest{
		NodeID:             nodeID,
		RepoID:             repoID,
		Kind:               NodeKindMethod,
		CanonicalSignature: "pkg::TheMethod()",
		Content:            "func TheMethod() {}",
	}
	result := PublishResult{
		PublishID:     publishID,
		QdrantPointID: pointID,
		AttemptIndex:  0,
		LastEventKind: EventKindQueued,
	}

	got, err := p.runAttempt(context.Background(), req, result, "stub-embedder@v1")
	if err != nil {
		t.Fatalf("runAttempt: %v", err)
	}
	if got.LastEventKind != EventKindPublished {
		t.Fatalf("LastEventKind = %q, want %q", got.LastEventKind, EventKindPublished)
	}

	captureMu.Lock()
	defer captureMu.Unlock()
	if captureCount != 1 {
		t.Fatalf("post-publish hook fire count = %d, want 1 (the hook MUST fire exactly once on the success path)", captureCount)
	}
	if captured.PublishID != publishID {
		t.Errorf("hook PublishID = %q, want %q", captured.PublishID, publishID)
	}
	if captured.SupersededPublishID != priorPublishID {
		t.Errorf("hook SupersededPublishID = %q, want %q (the probed supersedes_publish_id MUST round-trip into the Method/Block PublishedEvent — without this, snapshot_published_total for Method/Block stays at 0 even when a snapshot supersede actually happened)", captured.SupersededPublishID, priorPublishID)
	}
	if captured.NodeID != nodeID {
		t.Errorf("hook NodeID = %q, want %q", captured.NodeID, nodeID)
	}
	if captured.RepoID != repoID {
		t.Errorf("hook RepoID = %q, want %q", captured.RepoID, repoID)
	}
	if captured.Kind != NodeKindMethod {
		t.Errorf("hook Kind = %q, want %q", captured.Kind, NodeKindMethod)
	}
	if captured.ModelVersion != "stub-embedder@v1" {
		t.Errorf("hook ModelVersion = %q, want %q (hook MUST report the embedder's current ModelVersion, not the publishRow's recorded one)", captured.ModelVersion, "stub-embedder@v1")
	}
}

// TestRunAttempt_postPublishHookFires_emptySupersedeOnNonSnapshot
// pins the negative case for Method/Block: an ordinary
// (non-snapshot-enqueued) publish MUST still fire the hook,
// but with SupersededPublishID="". The cross-binary metrics
// consumer in cmd/repoindexer/main.go uses the empty value
// to skip the `snapshot_published_total++` — without this
// assertion, a regression that always returned a non-empty
// value (or never fired the hook at all) would silently
// break the counter accounting.
func TestRunAttempt_postPublishHookFires_emptySupersedeOnNonSnapshot(t *testing.T) {
	const (
		publishID = "66666666-6666-6666-6666-666666666666"
		pointID   = "77777777-7777-7777-7777-777777777777"
		nodeID    = "88888888-8888-8888-8888-888888888888"
		repoID    = "99999999-9999-9999-9999-999999999999"
	)

	var (
		captured     PublishedEvent
		captureCount int
		captureMu    sync.Mutex
	)
	hook := func(ev PublishedEvent) {
		captureMu.Lock()
		defer captureMu.Unlock()
		captureCount++
		captured = ev
	}

	p, _, mock, cleanup := newSqlmockPublisher(t, stubEmbedder{}, stubQdrant{}, WithPostPublishHook(hook))
	defer cleanup()

	mock.ExpectExec(`INSERT INTO embedding_publish_event`).
		WithArgs(publishID, EventKindVectorWritten, 0, nil).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT coalesce\(details_json ->> 'supersedes_publish_id', ''\)`).
		WithArgs(publishID).
		WillReturnRows(sqlmock.NewRows([]string{"coalesce"}).AddRow(""))
	mock.ExpectExec(`INSERT INTO embedding_publish_event[\s\S]+'published'::embedding_publish_event_kind`).
		WithArgs(publishID, 0).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// NO superseded insert expected — non-snapshot path.
	mock.ExpectCommit()

	req := PublishRequest{
		NodeID:             nodeID,
		RepoID:             repoID,
		Kind:               NodeKindBlock,
		CanonicalSignature: "pkg::block@42-58",
		Content:            "block body",
	}
	result := PublishResult{
		PublishID:     publishID,
		QdrantPointID: pointID,
		AttemptIndex:  0,
		LastEventKind: EventKindQueued,
	}

	got, err := p.runAttempt(context.Background(), req, result, "stub-embedder@v1")
	if err != nil {
		t.Fatalf("runAttempt: %v", err)
	}
	if got.LastEventKind != EventKindPublished {
		t.Fatalf("LastEventKind = %q, want %q", got.LastEventKind, EventKindPublished)
	}

	captureMu.Lock()
	defer captureMu.Unlock()
	if captureCount != 1 {
		t.Fatalf("post-publish hook fire count = %d, want 1 (hook fires on EVERY published transition, not just supersede)", captureCount)
	}
	if captured.SupersededPublishID != "" {
		t.Errorf("hook SupersededPublishID = %q, want empty (non-snapshot Method/Block publish MUST NOT carry a supersede id — the metrics consumer relies on '' to mean 'do not bump snapshot_published_total')", captured.SupersededPublishID)
	}
	if captured.PublishID != publishID {
		t.Errorf("hook PublishID = %q, want %q", captured.PublishID, publishID)
	}
	if captured.Kind != NodeKindBlock {
		t.Errorf("hook Kind = %q, want %q", captured.Kind, NodeKindBlock)
	}
}

// TestWithPostPublishHook_optionInstallsHook is a focused
// constructor-level check: the WithPostPublishHook Option
// MUST install the supplied callback on the Publisher and
// MUST tolerate a nil supplied callback (preserving the
// publisher's default "no observer" state). Pinning this at
// the option boundary catches the kind of regression where
// a refactor moved the field but forgot to update the
// Option's assignment.
func TestWithPostPublishHook_optionInstallsHook(t *testing.T) {
	t.Run("non_nil_installs", func(t *testing.T) {
		called := false
		hook := func(PublishedEvent) { called = true }
		p, _, _, cleanup := newSqlmockPublisher(t, stubEmbedder{}, stubQdrant{}, WithPostPublishHook(hook))
		defer cleanup()
		if p.postPublishHook == nil {
			t.Fatalf("WithPostPublishHook(non-nil) failed to install: postPublishHook is nil")
		}
		p.postPublishHook(PublishedEvent{PublishID: "x"})
		if !called {
			t.Errorf("installed hook did not fire when invoked directly")
		}
	})

	t.Run("nil_preserves_default", func(t *testing.T) {
		p, _, _, cleanup := newSqlmockPublisher(t, stubEmbedder{}, stubQdrant{}, WithPostPublishHook(nil))
		defer cleanup()
		if p.postPublishHook != nil {
			t.Errorf("WithPostPublishHook(nil) wrongly overwrote default postPublishHook=nil")
		}
	})
}
