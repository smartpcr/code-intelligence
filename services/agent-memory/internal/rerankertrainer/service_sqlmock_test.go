package rerankertrainer

// Behavioural unit tests driven through go-sqlmock for the
// Stage 6.4 publish+consume contract. These cover the
// "trained model published" e2e scenario at the unit-test
// layer (no PostgreSQL gate) by exercising the exact SQL the
// Service.Tick publish step issues AND the recall-side
// lookups the agent-api binary consults, so a SQL drift
// between the writer and reader paths surfaces here instead
// of in a live-DB run.
//
// Tests in this file:
//
//  1. TestPublish_insertsRowAndReturnsPublishedModel
//     -- happy path: publish() issues the INSERT exactly as
//        the schema expects, scans the returned model_id and
//        trained_at, and reports dup=false.
//
//  2. TestPublish_onConflictReportsDuplicate
//     -- ON CONFLICT (version) DO NOTHING fires when the
//        UNIQUE-version invariant is violated; sql.ErrNoRows
//        bubbles up the RETURNING and publish reports
//        dup=true so Tick stays idempotent.
//
//  3. TestPublish_arbitraryDBErrorPropagates
//     -- a non-23505 driver error surfaces unchanged so the
//        Tick's metrics counter trips.
//
//  4. TestLatestPublishedVersion_roundTripWithPublish
//     -- the recall consumer reads BACK the same version the
//        publish wrote. Mocks the INSERT then the SELECT in
//        a single sqlmock session; this is the closure of
//        the publish-and-consume loop F8 names.
//
//  5. TestLatestPublishedTrainedAt_missingRowReturnsOkFalse
//     -- a cold deploy (no published row yet) reports
//        ok=false so the recall path falls back to the V0
//        cold-start version identifier rather than emitting
//        an empty string.

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// newSqlmockService returns a Service wired to a sqlmock DB
// with permissive regex matching (matches the retirement
// package's pattern). The cleanup verifies all expectations
// were met.
func newSqlmockService(t *testing.T, trainer Trainer, cfg Config) (*Service, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherRegexp,
	))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	svc, err := New(db, cfg, trainer, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, mock, func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet sqlmock expectations: %v", err)
		}
		_ = db.Close()
	}
}

func TestPublish_insertsRowAndReturnsPublishedModel(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newSqlmockService(t, NoopTrainer{}, Config{})
	defer cleanup()

	now := time.Date(2025, 4, 1, 12, 0, 0, 0, time.UTC)
	out := TrainingOutput{
		Version:       "v-test-publish",
		ArtifactURI:   "data:application/json;base64,e30=",
		Metrics:       map[string]float64{"train_loss": 0.42},
		PublishStatus: StatusPublished,
	}

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO reranker_model")).
		WithArgs(out.Version, out.ArtifactURI, now, sqlmock.AnyArg(), StatusPublished).
		WillReturnRows(sqlmock.NewRows([]string{"model_id", "trained_at"}).
			AddRow("11111111-1111-1111-1111-111111111111", now))

	got, dup, err := svc.publish(context.Background(), out, StatusPublished, now)
	if err != nil {
		t.Fatalf("publish: unexpected error: %v", err)
	}
	if dup {
		t.Fatalf("publish: dup=true on a fresh row, want false")
	}
	if got.ModelID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("ModelID = %q, want the returned UUID", got.ModelID)
	}
	if got.Version != out.Version {
		t.Errorf("Version = %q, want %q", got.Version, out.Version)
	}
	if got.ArtifactURI != out.ArtifactURI {
		t.Errorf("ArtifactURI = %q, want %q", got.ArtifactURI, out.ArtifactURI)
	}
	if got.Status != StatusPublished {
		t.Errorf("Status = %q, want %q", got.Status, StatusPublished)
	}
	if !got.TrainedAt.Equal(now) {
		t.Errorf("TrainedAt = %v, want %v", got.TrainedAt, now)
	}
}

func TestPublish_onConflictReportsDuplicate(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newSqlmockService(t, NoopTrainer{}, Config{})
	defer cleanup()

	now := time.Date(2025, 4, 1, 12, 0, 0, 0, time.UTC)
	out := TrainingOutput{
		Version:       "v-already-published",
		ArtifactURI:   "data:application/json;base64,e30=",
		Metrics:       map[string]float64{},
		PublishStatus: StatusShadow,
	}

	// ON CONFLICT DO NOTHING + RETURNING produces an empty
	// row set on a duplicate. The standard library surfaces
	// that as sql.ErrNoRows on the Scan.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO reranker_model")).
		WithArgs(out.Version, out.ArtifactURI, now, sqlmock.AnyArg(), StatusShadow).
		WillReturnError(sql.ErrNoRows)

	got, dup, err := svc.publish(context.Background(), out, StatusShadow, now)
	if err != nil {
		t.Fatalf("publish: unexpected error on duplicate: %v", err)
	}
	if !dup {
		t.Fatalf("publish: dup=false on duplicate, want true")
	}
	if got.Version != out.Version {
		t.Errorf("Version = %q, want %q", got.Version, out.Version)
	}
	if got.ModelID != "" {
		t.Errorf("ModelID = %q, want empty on duplicate", got.ModelID)
	}
}

func TestPublish_arbitraryDBErrorPropagates(t *testing.T) {
	t.Parallel()
	svc, mock, cleanup := newSqlmockService(t, NoopTrainer{}, Config{})
	defer cleanup()

	now := time.Date(2025, 4, 1, 12, 0, 0, 0, time.UTC)
	out := TrainingOutput{
		Version:       "v-error",
		ArtifactURI:   "data:application/json;base64,e30=",
		Metrics:       map[string]float64{},
		PublishStatus: StatusPublished,
	}

	driverErr := errors.New("simulated driver-level failure")
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO reranker_model")).
		WithArgs(out.Version, out.ArtifactURI, now, sqlmock.AnyArg(), StatusPublished).
		WillReturnError(driverErr)

	_, dup, err := svc.publish(context.Background(), out, StatusPublished, now)
	if err == nil {
		t.Fatalf("publish: expected error to propagate, got nil")
	}
	if dup {
		t.Errorf("publish: dup=true on driver error, want false")
	}
	if !errors.Is(err, driverErr) {
		t.Errorf("publish: error chain does not include driver error: %v", err)
	}
}

func TestLatestPublishedVersion_roundTripWithPublish(t *testing.T) {
	t.Parallel()
	// Closure of the publish-and-consume loop: the writer
	// path inserts a row; the reader path (which the
	// agent-api binary calls through PublishedRerankerSource)
	// MUST observe the exact version the writer wrote.
	svc, mock, cleanup := newSqlmockService(t, NoopTrainer{}, Config{})
	defer cleanup()

	now := time.Date(2025, 4, 1, 12, 0, 0, 0, time.UTC)
	const wantVersion = "v-round-trip"
	const wantArtifact = "data:application/json;base64,e30="

	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO reranker_model")).
		WithArgs(wantVersion, wantArtifact, now, sqlmock.AnyArg(), StatusPublished).
		WillReturnRows(sqlmock.NewRows([]string{"model_id", "trained_at"}).
			AddRow("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", now))

	// LatestPublishedVersion SQL: status='published' filter,
	// ORDER BY trained_at DESC LIMIT 1.
	mock.ExpectQuery(`(?s)SELECT version.+FROM reranker_model.+WHERE status = 'published'`).
		WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(wantVersion))

	// LatestPublishedTrainedAt SQL: same shape returning trained_at.
	mock.ExpectQuery(`(?s)SELECT trained_at.+FROM reranker_model.+WHERE status = 'published'`).
		WillReturnRows(sqlmock.NewRows([]string{"trained_at"}).AddRow(now))

	// Write half.
	if _, _, err := svc.publish(context.Background(), TrainingOutput{
		Version:       wantVersion,
		ArtifactURI:   wantArtifact,
		Metrics:       map[string]float64{},
		PublishStatus: StatusPublished,
	}, StatusPublished, now); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Read half (mirrors what newPublishedRerankerSourceFromDB
	// in cmd/agent-api/main.go runs on every recall request).
	gotVersion, ok, err := LatestPublishedVersion(context.Background(), svc.db)
	if err != nil {
		t.Fatalf("LatestPublishedVersion: %v", err)
	}
	if !ok {
		t.Fatalf("LatestPublishedVersion: ok=false after a successful publish")
	}
	if gotVersion != wantVersion {
		t.Errorf("LatestPublishedVersion = %q, want %q", gotVersion, wantVersion)
	}

	gotTrainedAt, ok, err := LatestPublishedTrainedAt(context.Background(), svc.db)
	if err != nil {
		t.Fatalf("LatestPublishedTrainedAt: %v", err)
	}
	if !ok {
		t.Fatalf("LatestPublishedTrainedAt: ok=false after a successful publish")
	}
	if !gotTrainedAt.Equal(now) {
		t.Errorf("LatestPublishedTrainedAt = %v, want %v", gotTrainedAt, now)
	}
}

func TestLatestPublishedVersion_missingRowReturnsOkFalse(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherRegexp,
	))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
		_ = db.Close()
	}()

	mock.ExpectQuery(`(?s)SELECT version.+FROM reranker_model.+WHERE status = 'published'`).
		WillReturnError(sql.ErrNoRows)

	gotVersion, ok, err := LatestPublishedVersion(context.Background(), db)
	if err != nil {
		t.Fatalf("LatestPublishedVersion: %v", err)
	}
	if ok {
		t.Errorf("ok=true on a cold deploy, want false")
	}
	if gotVersion != "" {
		t.Errorf("version = %q, want empty", gotVersion)
	}
}

func TestLatestPublishedTrainedAt_missingRowReturnsOkFalse(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(
		sqlmock.QueryMatcherRegexp,
	))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("unmet expectations: %v", err)
		}
		_ = db.Close()
	}()

	mock.ExpectQuery(`(?s)SELECT trained_at.+FROM reranker_model.+WHERE status = 'published'`).
		WillReturnError(sql.ErrNoRows)

	gotTime, ok, err := LatestPublishedTrainedAt(context.Background(), db)
	if err != nil {
		t.Fatalf("LatestPublishedTrainedAt: %v", err)
	}
	if ok {
		t.Errorf("ok=true on a cold deploy, want false")
	}
	if !gotTime.IsZero() {
		t.Errorf("time = %v, want zero on cold deploy", gotTime)
	}
}

func TestTick_lockSkippedNoPublish(t *testing.T) {
	t.Parallel()
	// pg_try_advisory_lock returning false (another replica
	// holds the lock) MUST exit Tick as a no-op: no PullPairs
	// queries fire, no publish, no metrics bump beyond
	// IncRuns + IncLockSkipped. This guards the cross-replica
	// serialisation contract that lets multiple agent-memory
	// replicas safely run the trainer side-by-side.
	cfg := Config{
		AdvisoryLockKey: 42,
		MinEpisodes:     1,
		AllowNoopPublish: false,
	}
	svc, mock, cleanup := newSqlmockService(t, NoopTrainer{}, cfg)
	defer cleanup()

	// Conn().QueryRowContext -> pg_try_advisory_lock returns false.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT pg_try_advisory_lock($1)")).
		WithArgs(int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
	// No further queries: no PullPairs, no publish, no unlock
	// (the deferred unlock only fires when we actually held
	// the lock).

	res, err := svc.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: unexpected error on lock-skip: %v", err)
	}
	if !res.LockSkipped {
		t.Errorf("LockSkipped=false, want true")
	}
	if res.Published != nil {
		t.Errorf("Published non-nil on lock-skip: %+v", res.Published)
	}
	if metrics := svc.Metrics().Snapshot(); metrics["reranker_lock_skipped_total"] != 1 {
		t.Errorf("metrics[reranker_lock_skipped_total] = %v, want 1", metrics["reranker_lock_skipped_total"])
	}
}
