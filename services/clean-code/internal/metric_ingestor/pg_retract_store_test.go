package metric_ingestor_test

// Stage 3.4 -- sqlmock-driven tests for the production
// PostgreSQL implementations of [RetractScanRunStore],
// [RetractionStore], [SampleResolver], and
// [RescanScanRunStore]. These tests pin the exact SQL
// shape each store emits against the canonical schema:
// regex-matching only the stable fragments (table name,
// columns, ON CONFLICT clause) so the test stays robust
// against whitespace edits but breaks loudly if the
// statement's IDENTITY changes (e.g. dropping the
// `ON CONFLICT (sample_id) DO NOTHING` makes the
// retract path no longer idempotent).

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	metric_ingestor "github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
)

const pgRetractTestSchema = "clean_code_retract_test"

// sqlmockSetup is a tiny helper that yields (*sql.DB,
// sqlmock.Sqlmock, error) using the regex matcher. All
// test cases in this file share this single setup so the
// regex-vs-default matcher convention stays uniform.
func sqlmockSetup(t *testing.T) (*sql.DB, sqlmock.Sqlmock, error) {
	t.Helper()
	return sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
	)
}

func TestPGRetractScanRunStore_OpenRetractScanRun_InsertsRunningRow(t *testing.T) {
	db, mock, err := sqlmockSetup(t)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store, err := metric_ingestor.NewPGRetractScanRunStoreWithSchema(db, pgRetractTestSchema)
	if err != nil {
		t.Fatalf("NewPGRetractScanRunStoreWithSchema: %v", err)
	}

	repoID := uuid.Must(uuid.NewV4())
	sha := "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
	openedAt := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

	mock.ExpectExec(`INSERT INTO "clean_code_retract_test"."scan_run"\s+\(scan_run_id, repo_id, kind, sha_binding, to_sha, started_at, status\)\s+VALUES \(\$1, \$2, \$3, \$4, \$5, \$6, 'running'\)`).
		WithArgs(sqlmock.AnyArg(), repoID, "retract", "single", sha, openedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	scanRunID, err := store.OpenRetractScanRun(context.Background(), repoID, sha, openedAt)
	if err != nil {
		t.Fatalf("OpenRetractScanRun: %v", err)
	}
	if scanRunID == uuid.Nil {
		t.Errorf("scan_run_id: got zero UUID; want minted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

func TestPGRetractScanRunStore_FinalizeRetractScanRun_UpdatesRow(t *testing.T) {
	db, mock, err := sqlmockSetup(t)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store, _ := metric_ingestor.NewPGRetractScanRunStoreWithSchema(db, pgRetractTestSchema)

	scanRunID := uuid.Must(uuid.NewV4())
	endedAt := time.Date(2024, 1, 2, 3, 4, 7, 0, time.UTC)

	mock.ExpectExec(`UPDATE "clean_code_retract_test"."scan_run"\s+SET status = \$2, ended_at = \$3\s+WHERE scan_run_id = \$1 AND status = 'running'`).
		WithArgs(scanRunID, "succeeded", endedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.FinalizeRetractScanRun(context.Background(), scanRunID, "succeeded", endedAt); err != nil {
		t.Fatalf("FinalizeRetractScanRun: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

func TestPGRetractScanRunStore_FinalizeRetractScanRun_NoRowsErrors(t *testing.T) {
	db, mock, err := sqlmockSetup(t)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store, _ := metric_ingestor.NewPGRetractScanRunStoreWithSchema(db, pgRetractTestSchema)

	scanRunID := uuid.Must(uuid.NewV4())
	mock.ExpectExec(`UPDATE "clean_code_retract_test"."scan_run"`).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err = store.FinalizeRetractScanRun(context.Background(), scanRunID, "succeeded", time.Now().UTC())
	if err == nil {
		t.Fatal("Finalize: expected error on 0 rowsAffected")
	}
	if !errors.Is(err, metric_ingestor.ErrConcurrentFinalize) {
		t.Errorf("Finalize: err=%v; want ErrConcurrentFinalize", err)
	}
}

func TestPGRetractionStore_SampleExists_TrueAndFalse(t *testing.T) {
	db, mock, err := sqlmockSetup(t)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store, _ := metric_ingestor.NewPGRetractionStoreWithSchema(db, pgRetractTestSchema)

	sampleID := uuid.Must(uuid.NewV4())

	mock.ExpectQuery(`SELECT 1 FROM "clean_code_retract_test"."metric_sample" WHERE sample_id = \$1`).
		WithArgs(sampleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	exists, err := store.SampleExists(context.Background(), sampleID)
	if err != nil {
		t.Fatalf("SampleExists: %v", err)
	}
	if !exists {
		t.Errorf("SampleExists: got false; want true")
	}

	mock.ExpectQuery(`SELECT 1 FROM "clean_code_retract_test"."metric_sample" WHERE sample_id = \$1`).
		WithArgs(sampleID).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	exists, err = store.SampleExists(context.Background(), sampleID)
	if err != nil {
		t.Fatalf("SampleExists (no-row): %v", err)
	}
	if exists {
		t.Errorf("SampleExists no-row: got true; want false")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("ExpectationsWereMet: %v", err)
	}
}

func TestPGRetractionStore_ResolveSample_HappyPath(t *testing.T) {
	db, mock, err := sqlmockSetup(t)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store, _ := metric_ingestor.NewPGRetractionStoreWithSchema(db, pgRetractTestSchema)

	sampleID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	sha := "1111111111111111111111111111111111111111"

	mock.ExpectQuery(`SELECT repo_id, sha FROM "clean_code_retract_test"."metric_sample" WHERE sample_id = \$1`).
		WithArgs(sampleID).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha"}).AddRow(repoID, sha))

	gotRepo, gotSHA, found, err := store.ResolveSample(context.Background(), sampleID)
	if err != nil {
		t.Fatalf("ResolveSample: %v", err)
	}
	if !found {
		t.Fatalf("ResolveSample: found=false; want true")
	}
	if gotRepo != repoID {
		t.Errorf("repo_id: got %s; want %s", gotRepo, repoID)
	}
	if gotSHA != sha {
		t.Errorf("sha: got %s; want %s", gotSHA, sha)
	}
}

func TestPGRetractionStore_Lookup_FoundAndNotFound(t *testing.T) {
	db, mock, err := sqlmockSetup(t)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store, _ := metric_ingestor.NewPGRetractionStoreWithSchema(db, pgRetractTestSchema)

	sampleID := uuid.Must(uuid.NewV4())
	retractionID := uuid.Must(uuid.NewV4())
	createdAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT retraction_id, sample_id, reason, appended_by, created_at\s+FROM "clean_code_retract_test"."metric_retraction"\s+WHERE sample_id = \$1`).
		WithArgs(sampleID).
		WillReturnRows(sqlmock.NewRows([]string{"retraction_id", "sample_id", "reason", "appended_by", "created_at"}).
			AddRow(retractionID, sampleID, "vendored", "operator:alice", createdAt))

	row, found, err := store.Lookup(context.Background(), sampleID)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("Lookup: found=false; want true")
	}
	if row.RetractionID != retractionID || row.SampleID != sampleID || row.Reason != "vendored" || row.AppendedBy != "operator:alice" {
		t.Errorf("Lookup row mismatch: %+v", row)
	}

	mock.ExpectQuery(`SELECT retraction_id, sample_id, reason, appended_by, created_at\s+FROM "clean_code_retract_test"."metric_retraction"`).
		WithArgs(sampleID).
		WillReturnRows(sqlmock.NewRows([]string{"retraction_id", "sample_id", "reason", "appended_by", "created_at"}))
	_, found, err = store.Lookup(context.Background(), sampleID)
	if err != nil {
		t.Fatalf("Lookup (no-row): %v", err)
	}
	if found {
		t.Error("Lookup no-row: found=true; want false")
	}
}

func TestPGRetractionStore_Append_FreshInsertReturnsInsertedTrue(t *testing.T) {
	db, mock, err := sqlmockSetup(t)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store, _ := metric_ingestor.NewPGRetractionStoreWithSchema(db, pgRetractTestSchema)

	row := metric_ingestor.RetractionRow{
		RetractionID: uuid.Must(uuid.NewV4()),
		SampleID:     uuid.Must(uuid.NewV4()),
		Reason:       "vendored file",
		AppendedBy:   "operator:alice",
		CreatedAt:    time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC),
	}

	mock.ExpectQuery(`INSERT INTO "clean_code_retract_test"."metric_retraction"\s+\(retraction_id, sample_id, reason, appended_by, created_at\)\s+VALUES \(\$1, \$2, \$3, \$4, \$5\)\s+ON CONFLICT \(sample_id\) DO NOTHING\s+RETURNING retraction_id, sample_id, reason, appended_by, created_at`).
		WithArgs(row.RetractionID, row.SampleID, "vendored file", "operator:alice", row.CreatedAt).
		WillReturnRows(sqlmock.NewRows([]string{"retraction_id", "sample_id", "reason", "appended_by", "created_at"}).
			AddRow(row.RetractionID, row.SampleID, row.Reason, row.AppendedBy, row.CreatedAt))

	stored, inserted, err := store.Append(context.Background(), row)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !inserted {
		t.Errorf("inserted: got false; want true")
	}
	if stored.RetractionID != row.RetractionID {
		t.Errorf("retraction_id mismatch: got %s; want %s", stored.RetractionID, row.RetractionID)
	}
}

func TestPGRetractionStore_Append_ConflictReturnsExistingInsertedFalse(t *testing.T) {
	db, mock, err := sqlmockSetup(t)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store, _ := metric_ingestor.NewPGRetractionStoreWithSchema(db, pgRetractTestSchema)

	sampleID := uuid.Must(uuid.NewV4())
	existing := metric_ingestor.RetractionRow{
		RetractionID: uuid.Must(uuid.NewV4()),
		SampleID:     sampleID,
		Reason:       "vendored file",
		AppendedBy:   "operator:alice",
		CreatedAt:    time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC),
	}
	loser := metric_ingestor.RetractionRow{
		RetractionID: uuid.Must(uuid.NewV4()),
		SampleID:     sampleID,
		Reason:       "duplicate retract from racer",
		AppendedBy:   "operator:bob",
		CreatedAt:    time.Date(2024, 6, 1, 10, 0, 1, 0, time.UTC),
	}

	// INSERT ON CONFLICT DO NOTHING -> empty RETURNING.
	mock.ExpectQuery(`INSERT INTO "clean_code_retract_test"."metric_retraction"`).
		WithArgs(loser.RetractionID, loser.SampleID, loser.Reason, loser.AppendedBy, loser.CreatedAt).
		WillReturnRows(sqlmock.NewRows([]string{"retraction_id", "sample_id", "reason", "appended_by", "created_at"}))
	// Post-conflict Lookup yields the existing row.
	mock.ExpectQuery(`SELECT retraction_id, sample_id, reason, appended_by, created_at\s+FROM "clean_code_retract_test"."metric_retraction"\s+WHERE sample_id = \$1`).
		WithArgs(sampleID).
		WillReturnRows(sqlmock.NewRows([]string{"retraction_id", "sample_id", "reason", "appended_by", "created_at"}).
			AddRow(existing.RetractionID, existing.SampleID, existing.Reason, existing.AppendedBy, existing.CreatedAt))

	stored, inserted, err := store.Append(context.Background(), loser)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if inserted {
		t.Errorf("inserted: got true; want false (race-loser path)")
	}
	if stored.RetractionID != existing.RetractionID {
		t.Errorf("stored.RetractionID: got %s; want %s (must surface PRE-EXISTING row, not the loser)",
			stored.RetractionID, existing.RetractionID)
	}
	if stored.Reason != existing.Reason {
		t.Errorf("stored.Reason: got %q; want %q", stored.Reason, existing.Reason)
	}
}

func TestPGRescanScanRunStore_OpenRescanRun_InsertsFullKindRunningRow(t *testing.T) {
	db, mock, err := sqlmockSetup(t)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	store, err := metric_ingestor.NewPGRescanScanRunStoreWithSchema(db, pgRetractTestSchema)
	if err != nil {
		t.Fatalf("NewPGRescanScanRunStoreWithSchema: %v", err)
	}

	repoID := uuid.Must(uuid.NewV4())
	sha := "abc1230000000000000000000000000000000000"
	openedAt := time.Date(2024, 6, 1, 10, 0, 0, 0, time.UTC)

	mock.ExpectExec(`INSERT INTO "clean_code_retract_test"."scan_run"\s+\(scan_run_id, repo_id, kind, sha_binding, to_sha, started_at, status\)\s+VALUES \(\$1, \$2, \$3, \$4, \$5, \$6, 'running'\)`).
		WithArgs(sqlmock.AnyArg(), repoID, "full", "single", sha, openedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	scanRunID, err := store.OpenRescanRun(context.Background(), repoID, sha, openedAt)
	if err != nil {
		t.Fatalf("OpenRescanRun: %v", err)
	}
	if scanRunID == uuid.Nil {
		t.Errorf("scan_run_id: got zero UUID; want minted")
	}
}
