package metric_ingestor_test

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
)

// pgExternalStoreTestNow is the fixed clock the PG external
// scan_run store sqlmock tests stamp on OpenedAt / endedAt.
// Pinned to a non-zero UTC instant so the store's UTC()
// conversion is observably a no-op.
func pgExternalStoreTestNow() time.Time {
	return time.Date(2026, 1, 14, 12, 0, 0, 0, time.UTC)
}

// pgExternalStoreTestPayloadHash returns a deterministic
// 32-byte fixture hash. The store's Validate guards
// `len(PayloadHash) == 32`, so the exact contents are
// irrelevant; the length is the only thing that matters.
func pgExternalStoreTestPayloadHash() []byte {
	h := make([]byte, 32)
	for i := range h {
		h[i] = byte(i + 1)
	}
	return h
}

// newPGExternalStoreFixture builds a sqlmock-backed
// [metric_ingestor.PGExternalScanRunStore] on the test
// schema `"clean_code_test"` so the qualified-table SQL
// matches a known regex.
func newPGExternalStoreFixture(t *testing.T) (*sql.DB, sqlmock.Sqlmock, *metric_ingestor.PGExternalScanRunStore, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	store, err := metric_ingestor.NewPGExternalScanRunStoreWithSchema(db, "clean_code_test")
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPGExternalScanRunStoreWithSchema: %v", err)
	}
	return db, mock, store, func() { _ = db.Close() }
}

// TestPGExternalScanRunStore_OpenExternalScanRun_Insert_HappyPath
// pins the brief's primary positive: the INSERT ... ON
// CONFLICT DO NOTHING RETURNING returns a fresh
// scan_run_id, signalling AlreadyExisted=false.
func TestPGExternalScanRunStore_OpenExternalScanRun_Insert_HappyPath(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	repoID, _ := uuid.NewV4()
	expectedID, _ := uuid.NewV4()

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO "clean_code_test"."scan_run"`)).
		WithArgs(
			sqlmock.AnyArg(), // scan_run_id (minted inside the store)
			repoID,
			"external_per_row",
			"per_row",
			nil, // to_sha is NULL for per_row
			pgExternalStoreTestNow().UTC(),
			"churn",
			pgExternalStoreTestPayloadHash(),
		).
		WillReturnRows(sqlmock.NewRows([]string{"scan_run_id"}).AddRow(expectedID))

	res, err := store.OpenExternalScanRun(context.Background(), metric_ingestor.OpenExternalScanRunRequest{
		RepoID:      repoID,
		Verb:        "churn",
		Kind:        metric_ingestor.ScanRunKindExternalPerRow,
		SHABinding:  metric_ingestor.SHABindingPerRow,
		PayloadHash: pgExternalStoreTestPayloadHash(),
		OpenedAt:    pgExternalStoreTestNow(),
	})
	if err != nil {
		t.Fatalf("OpenExternalScanRun: %v", err)
	}
	if res.AlreadyExisted {
		t.Errorf("AlreadyExisted: want false, got true")
	}
	if res.ScanRunID != expectedID {
		t.Errorf("ScanRunID: want %s (from RETURNING), got %s", expectedID, res.ScanRunID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGExternalScanRunStore_OpenExternalScanRun_Conflict_ReturnsPriorID
// pins the durable-idempotency invariant (Stage 4.1 iter-3
// evaluator items): a second OpenExternalScanRun with the
// SAME (verb, payload_hash) returns the PRIOR scan_run_id
// with AlreadyExisted=true.
func TestPGExternalScanRunStore_OpenExternalScanRun_Conflict_ReturnsPriorID(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	repoID, _ := uuid.NewV4()
	priorID, _ := uuid.NewV4()

	// INSERT ... ON CONFLICT DO NOTHING returns 0 rows.
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO "clean_code_test"."scan_run"`)).
		WillReturnError(sql.ErrNoRows)

	// Follow-up SELECT returns the prior row.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT scan_run_id, status`)).
		WithArgs("churn", pgExternalStoreTestPayloadHash()).
		WillReturnRows(sqlmock.NewRows([]string{"scan_run_id", "status"}).
			AddRow(priorID, "succeeded"))

	res, err := store.OpenExternalScanRun(context.Background(), metric_ingestor.OpenExternalScanRunRequest{
		RepoID:      repoID,
		Verb:        "churn",
		Kind:        metric_ingestor.ScanRunKindExternalPerRow,
		SHABinding:  metric_ingestor.SHABindingPerRow,
		PayloadHash: pgExternalStoreTestPayloadHash(),
		OpenedAt:    pgExternalStoreTestNow(),
	})
	if err != nil {
		t.Fatalf("OpenExternalScanRun: %v", err)
	}
	if !res.AlreadyExisted {
		t.Errorf("AlreadyExisted: want true (conflict path), got false")
	}
	if res.ScanRunID != priorID {
		t.Errorf("ScanRunID: want %s (from SELECT), got %s", priorID, res.ScanRunID)
	}
	if string(res.ExistingStatus) != "succeeded" {
		t.Errorf("ExistingStatus: want %q, got %q", "succeeded", res.ExistingStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGExternalScanRunStore_OpenExternalScanRun_BadKind_NoDBRoundTrip pins
// the Validate guard: an unsupported kind never hits the DB.
func TestPGExternalScanRunStore_OpenExternalScanRun_BadKind_NoDBRoundTrip(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	repoID, _ := uuid.NewV4()
	_, err := store.OpenExternalScanRun(context.Background(), metric_ingestor.OpenExternalScanRunRequest{
		RepoID:      repoID,
		Verb:        "churn",
		Kind:        "full", // foundation-tier kind -- rejected by the external store
		SHABinding:  "single",
		ToSHA:       "abcd",
		PayloadHash: pgExternalStoreTestPayloadHash(),
		OpenedAt:    pgExternalStoreTestNow(),
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrExternalScanRunUnsupportedKind) {
		t.Errorf("error chain: want ErrExternalScanRunUnsupportedKind, got %v", err)
	}
	if got := mock.ExpectationsWereMet(); got != nil {
		// We never set up any expectations -- a met-but-
		// unexpected query is what we want to verify did
		// NOT happen.
		t.Errorf("unexpected: %v", got)
	}
}

// TestPGExternalScanRunStore_FinalizeExternalScanRun_HappyPath
// pins the UPDATE path: the store transitions the row from
// 'running' to 'succeeded' and the rows-affected check
// accepts exactly 1.
func TestPGExternalScanRunStore_FinalizeExternalScanRun_HappyPath(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	scanRunID, _ := uuid.NewV4()
	ended := pgExternalStoreTestNow().Add(time.Second)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE "clean_code_test"."scan_run"`)).
		WithArgs(scanRunID, "succeeded", ended.UTC()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.FinalizeExternalScanRun(context.Background(), scanRunID, metric_ingestor.ScanRunStatusSucceeded, ended); err != nil {
		t.Errorf("FinalizeExternalScanRun: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGExternalScanRunStore_FinalizeExternalScanRun_ZeroRowsAffected_ReturnsConcurrentFinalize
// pins the double-finalise guard: rowsAffected=0 (because
// the row is already terminal) surfaces as
// [metric_ingestor.ErrConcurrentFinalize].
func TestPGExternalScanRunStore_FinalizeExternalScanRun_ZeroRowsAffected_ReturnsConcurrentFinalize(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	scanRunID, _ := uuid.NewV4()
	ended := pgExternalStoreTestNow().Add(time.Second)

	mock.ExpectExec(regexp.QuoteMeta(`UPDATE "clean_code_test"."scan_run"`)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	err := store.FinalizeExternalScanRun(context.Background(), scanRunID, metric_ingestor.ScanRunStatusSucceeded, ended)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrConcurrentFinalize) {
		t.Errorf("error chain: want ErrConcurrentFinalize, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestPGExternalScanRunStore_LookupExternalScanRunByPayloadHash_NoMatch_ReturnsFalse
// pins the lookup-only path: when no row matches, the
// publisher-facing helper returns (uuid.Nil, "", false, nil)
// so the caller can branch on "not found" without parsing an
// error.
func TestPGExternalScanRunStore_LookupExternalScanRunByPayloadHash_NoMatch_ReturnsFalse(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT scan_run_id, status`)).
		WithArgs("churn", pgExternalStoreTestPayloadHash()).
		WillReturnError(sql.ErrNoRows)

	id, status, found, err := store.LookupExternalScanRunByPayloadHash(context.Background(),
		"churn",
		pgExternalStoreTestPayloadHash())
	if err != nil {
		t.Fatalf("LookupExternalScanRunByPayloadHash: %v", err)
	}
	if found {
		t.Errorf("found: want false, got true")
	}
	if id != uuid.Nil {
		t.Errorf("id: want zero UUID, got %s", id)
	}
	if string(status) != "" {
		t.Errorf("status: want empty, got %q", status)
	}
}

// TestNewPGExternalScanRunStore_NilDB_ReturnsSentinel pins the
// constructor-time guard: a nil *sql.DB fails fast with the
// canonical sentinel so a wiring bug doesn't manifest as a
// runtime nil-deref.
func TestNewPGExternalScanRunStore_NilDB_ReturnsSentinel(t *testing.T) {
	t.Parallel()
	_, err := metric_ingestor.NewPGExternalScanRunStore(nil)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrPGExternalScanRunStoreNilDB) {
		t.Errorf("error chain: want ErrPGExternalScanRunStoreNilDB, got %v", err)
	}
}

// TestPGExternalScanRunStore_OpenExternalScanRun_BadVerb_NoDBRoundTrip
// (iter-3 evaluator item #2) pins the new closed-set verb
// guard: an unsupported verb never hits the DB, and the
// verb-keyed idempotency invariant cannot be bypassed by
// passing a synthetic verb.
func TestPGExternalScanRunStore_OpenExternalScanRun_BadVerb_NoDBRoundTrip(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	repoID, _ := uuid.NewV4()
	_, err := store.OpenExternalScanRun(context.Background(), metric_ingestor.OpenExternalScanRunRequest{
		RepoID:      repoID,
		Verb:        "evil_verb",
		Kind:        metric_ingestor.ScanRunKindExternalPerRow,
		SHABinding:  metric_ingestor.SHABindingPerRow,
		PayloadHash: pgExternalStoreTestPayloadHash(),
		OpenedAt:    pgExternalStoreTestNow(),
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, metric_ingestor.ErrExternalScanRunUnsupportedVerb) {
		t.Errorf("error chain: want ErrExternalScanRunUnsupportedVerb, got %v", err)
	}
	if got := mock.ExpectationsWereMet(); got != nil {
		t.Errorf("unexpected: %v", got)
	}
}

// TestPGExternalScanRunStore_OpenExternalScanRun_VerbKindMismatch_NoDBRoundTrip
// (iter-3 evaluator item #2) pins the canonical verb->kind
// matrix: a caller that supplies `verb=churn` with
// `kind=external_single` is a wiring bug -- the Validate
// guard short-circuits BEFORE the DB. This also prevents
// the durable index from being keyed on (verb=churn,
// payload_hash=X) when another caller writes the same
// (verb=churn, payload_hash=X) under a different kind, which
// would degrade replay semantics.
func TestPGExternalScanRunStore_OpenExternalScanRun_VerbKindMismatch_NoDBRoundTrip(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	repoID, _ := uuid.NewV4()
	_, err := store.OpenExternalScanRun(context.Background(), metric_ingestor.OpenExternalScanRunRequest{
		RepoID:      repoID,
		Verb:        "churn", // canonical -> external_per_row
		Kind:        metric_ingestor.ScanRunKindExternalSingle,
		SHABinding:  metric_ingestor.SHABindingSingle,
		ToSHA:       "deadbeef",
		PayloadHash: pgExternalStoreTestPayloadHash(),
		OpenedAt:    pgExternalStoreTestNow(),
	})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if got := mock.ExpectationsWereMet(); got != nil {
		t.Errorf("unexpected: %v", got)
	}
}

// TestPGExternalScanRunStore_LookupExternalScanRunStatusByID_HappyPath
// pins the new helper the PGScanRunRepository.Finalize path
// consults on the ErrConcurrentFinalize branch (iter-3 item
// #4): a row exists and its status is returned along with
// found=true.
func TestPGExternalScanRunStore_LookupExternalScanRunStatusByID_HappyPath(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	scanRunID, _ := uuid.NewV4()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT status`)).
		WithArgs(scanRunID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow("succeeded"))

	status, found, err := store.LookupExternalScanRunStatusByID(context.Background(), scanRunID)
	if err != nil {
		t.Fatalf("LookupExternalScanRunStatusByID: %v", err)
	}
	if !found {
		t.Errorf("found: want true, got false")
	}
	if string(status) != "succeeded" {
		t.Errorf("status: want %q, got %q", "succeeded", status)
	}
}

// TestPGExternalScanRunStore_LookupExternalScanRunStatusByID_NotFound
// pins the helper's no-row branch: (uuid.Nil, "", false, nil)
// for an unknown scan_run_id.
func TestPGExternalScanRunStore_LookupExternalScanRunStatusByID_NotFound(t *testing.T) {
	t.Parallel()
	_, mock, store, cleanup := newPGExternalStoreFixture(t)
	defer cleanup()

	scanRunID, _ := uuid.NewV4()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT status`)).
		WithArgs(scanRunID).
		WillReturnError(sql.ErrNoRows)

	status, found, err := store.LookupExternalScanRunStatusByID(context.Background(), scanRunID)
	if err != nil {
		t.Fatalf("LookupExternalScanRunStatusByID: %v", err)
	}
	if found {
		t.Errorf("found: want false, got true")
	}
	if string(status) != "" {
		t.Errorf("status: want empty, got %q", status)
	}
}
