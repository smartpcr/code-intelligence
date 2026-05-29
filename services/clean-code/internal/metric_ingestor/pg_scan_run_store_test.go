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
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/repo_indexer"
)

const pgScanRunTestSchema = "clean_code_ingestor_test"

var (
	pgScanRunTestRepoID = uuid.Must(uuid.FromString("aaaaaaaa-bbbb-cccc-dddd-eeeeffff0001"))
	pgScanRunTestSHA    = "abc1234567890123456789012345678901234567"
)

// newSQLMockScanRunStore wires a PGScanRunStore against a
// regex-matching mock DB. Returns the store, the mock
// expectation surface, and a cleanup func that asserts
// all declared expectations were met.
func newSQLMockScanRunStore(t *testing.T) (*metric_ingestor.PGScanRunStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	s, err := metric_ingestor.NewPGScanRunStoreWithSchema(db, pgScanRunTestSchema)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPGScanRunStoreWithSchema: %v", err)
	}
	cleanup := func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock: unmet expectations: %v", err)
		}
		_ = db.Close()
	}
	return s, mock, cleanup
}

func TestNewPGScanRunStore_RejectsNilDB(t *testing.T) {
	t.Parallel()
	if _, err := metric_ingestor.NewPGScanRunStore(nil); !errors.Is(err, metric_ingestor.ErrPGScanRunStoreNilDB) {
		t.Errorf("NewPGScanRunStore(nil): err=%v, want errors.Is ErrPGScanRunStoreNilDB", err)
	}
}

func TestNewPGScanRunStoreWithSchema_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	if _, err := metric_ingestor.NewPGScanRunStoreWithSchema(db, "   "); !errors.Is(err, metric_ingestor.ErrPGScanRunStoreEmptySchema) {
		t.Errorf("NewPGScanRunStoreWithSchema(db, '   '): err=%v, want errors.Is ErrPGScanRunStoreEmptySchema", err)
	}
}

// TestPGScanRunStore_ClaimNextPendingCommit_Happy pins the
// canonical SQL trace for a successful claim: BEGIN,
// SELECT FOR UPDATE, INSERT scan_run, UPDATE commit,
// COMMIT.
func TestPGScanRunStore_ClaimNextPendingCommit_Happy(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	committedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	openedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT repo_id, sha, committed_at`) +
		`\s+FROM\s+"` + pgScanRunTestSchema + `"\."commit"` +
		`\s+WHERE\s+scan_status\s*=\s*'pending'` +
		`\s+ORDER\s+BY\s+committed_at\s+ASC,\s+sha\s+ASC` +
		`\s+LIMIT\s+1` +
		`\s+FOR\s+UPDATE\s+SKIP\s+LOCKED`).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha", "committed_at"}).
			AddRow(pgScanRunTestRepoID, pgScanRunTestSHA, committedAt))
	mock.ExpectExec(`INSERT\s+INTO\s+"` + pgScanRunTestSchema + `"\."scan_run"`).
		WithArgs(
			sqlmock.AnyArg(),       // scan_run_id (minted)
			pgScanRunTestRepoID,    // repo_id
			"full",                 // kind
			"single",               // sha_binding
			pgScanRunTestSHA,       // to_sha
			openedAt,               // started_at (UTC)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+"` + pgScanRunTestSchema + `"\."commit"\s+SET\s+scan_status\s*=\s*'scanning'`).
		WithArgs(pgScanRunTestRepoID, pgScanRunTestSHA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	claim, ok, err := s.ClaimNextPendingCommit(context.Background(), metric_ingestor.ClaimRequest{
		Kind:       "full",
		SHABinding: "single",
		OpenedAt:   openedAt,
	})
	if err != nil {
		t.Fatalf("ClaimNextPendingCommit: err=%v, want nil", err)
	}
	if !ok {
		t.Fatal("ClaimNextPendingCommit: ok=false, want true (a row was returned)")
	}
	if claim.RepoID != pgScanRunTestRepoID {
		t.Errorf("claim.RepoID=%s, want %s", claim.RepoID, pgScanRunTestRepoID)
	}
	if claim.SHA != pgScanRunTestSHA {
		t.Errorf("claim.SHA=%q, want %q", claim.SHA, pgScanRunTestSHA)
	}
	if claim.Kind != "full" {
		t.Errorf("claim.Kind=%q, want full", claim.Kind)
	}
	if claim.SHABinding != "single" {
		t.Errorf("claim.SHABinding=%q, want single", claim.SHABinding)
	}
	if claim.ScanRunID == uuid.Nil {
		t.Error("claim.ScanRunID is the zero UUID; want a freshly-minted UUID")
	}
}

// TestPGScanRunStore_ClaimNextPendingCommit_NoPendingRow pins
// the "no work" surface: the SELECT returns sql.ErrNoRows,
// the store rolls back and returns (zero, false, nil) --
// the sweep loop interprets this as the idle signal.
func TestPGScanRunStore_ClaimNextPendingCommit_NoPendingRow(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT\s+repo_id,\s+sha,\s+committed_at`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	claim, ok, err := s.ClaimNextPendingCommit(context.Background(), metric_ingestor.ClaimRequest{
		Kind:       "full",
		SHABinding: "single",
		OpenedAt:   time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("ClaimNextPendingCommit (no rows): err=%v, want nil", err)
	}
	if ok {
		t.Error("ClaimNextPendingCommit (no rows): ok=true, want false")
	}
	if claim.ScanRunID != uuid.Nil {
		t.Errorf("ClaimNextPendingCommit (no rows): claim=%+v, want zero", claim)
	}
}

// TestPGScanRunStore_ClaimNextPendingCommit_ConcurrentRace
// pins the rowsAffected=0 surface: a second worker raced the
// UPDATE between the SELECT-FOR-UPDATE and the UPDATE, so
// the store returns ErrConcurrentClaim and rolls back.
func TestPGScanRunStore_ClaimNextPendingCommit_ConcurrentRace(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	committedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	openedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT\s+repo_id,\s+sha,\s+committed_at`).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha", "committed_at"}).
			AddRow(pgScanRunTestRepoID, pgScanRunTestSHA, committedAt))
	mock.ExpectExec(`INSERT\s+INTO\s+"` + pgScanRunTestSchema + `"\."scan_run"`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+"` + pgScanRunTestSchema + `"\."commit"\s+SET\s+scan_status\s*=\s*'scanning'`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // raced
	mock.ExpectRollback()

	_, _, err := s.ClaimNextPendingCommit(context.Background(), metric_ingestor.ClaimRequest{
		Kind: "full", SHABinding: "single", OpenedAt: openedAt,
	})
	if !errors.Is(err, metric_ingestor.ErrConcurrentClaim) {
		t.Errorf("ClaimNextPendingCommit (raced): err=%v, want errors.Is ErrConcurrentClaim", err)
	}
}

// TestPGScanRunStore_ClaimNextPendingCommit_RejectsBadKind
// pins the validation-before-DB contract: an out-of-set
// kind never touches the DB.
func TestPGScanRunStore_ClaimNextPendingCommit_RejectsBadKind(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()
	// No DB expectations -- validation fails first.

	_, _, err := s.ClaimNextPendingCommit(context.Background(), metric_ingestor.ClaimRequest{
		Kind: "external_double", SHABinding: "single", OpenedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("ClaimNextPendingCommit bad kind: err=nil, want validation error")
	}
	_ = mock // suppress unused
}

// TestPGScanRunStore_FinalizeScanRun_HappySucceeded pins the
// canonical success-finalize SQL trace.
func TestPGScanRunStore_FinalizeScanRun_HappySucceeded(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	scanRunID := uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555555"))
	endedAt := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE\s+"` + pgScanRunTestSchema + `"\."scan_run"\s+SET\s+status\s*=\s*\$1`).
		WithArgs("succeeded", endedAt, scanRunID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+"` + pgScanRunTestSchema + `"\."commit"\s+SET\s+scan_status\s*=\s*\$1`).
		WithArgs("scanned", pgScanRunTestRepoID, pgScanRunTestSHA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := s.FinalizeScanRun(context.Background(),
		metric_ingestor.ScanRunClaim{
			ScanRunID: scanRunID, RepoID: pgScanRunTestRepoID, SHA: pgScanRunTestSHA,
			Kind: "full", SHABinding: "single",
		},
		metric_ingestor.ScanRunStatusSucceeded,
		repo_indexer.ScanStatusScanned,
		endedAt,
	)
	if err != nil {
		t.Fatalf("FinalizeScanRun (succeeded): err=%v, want nil", err)
	}
}

// TestPGScanRunStore_FinalizeScanRun_HappyFailed pins the
// canonical failure-finalize SQL trace.
func TestPGScanRunStore_FinalizeScanRun_HappyFailed(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	scanRunID := uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555556"))
	endedAt := time.Date(2026, 6, 1, 13, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE\s+"` + pgScanRunTestSchema + `"\."scan_run"\s+SET\s+status\s*=\s*\$1`).
		WithArgs("failed", endedAt, scanRunID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+"` + pgScanRunTestSchema + `"\."commit"\s+SET\s+scan_status\s*=\s*\$1`).
		WithArgs("failed", pgScanRunTestRepoID, pgScanRunTestSHA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err := s.FinalizeScanRun(context.Background(),
		metric_ingestor.ScanRunClaim{
			ScanRunID: scanRunID, RepoID: pgScanRunTestRepoID, SHA: pgScanRunTestSHA,
			Kind: "full", SHABinding: "single",
		},
		metric_ingestor.ScanRunStatusFailed,
		repo_indexer.ScanStatusFailed,
		endedAt,
	)
	if err != nil {
		t.Fatalf("FinalizeScanRun (failed): err=%v, want nil", err)
	}
}

// TestPGScanRunStore_FinalizeScanRun_RejectsMismatchedPair
// pins the (succeeded,scanned) / (failed,failed) invariant
// at the PG layer (parity with InMemoryScanRunStore).
func TestPGScanRunStore_FinalizeScanRun_RejectsMismatchedPair(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()
	// No DB expectations -- validation fails first.
	_ = mock

	err := s.FinalizeScanRun(context.Background(),
		metric_ingestor.ScanRunClaim{
			ScanRunID: uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555557")),
			RepoID:    pgScanRunTestRepoID, SHA: pgScanRunTestSHA,
			Kind: "full", SHABinding: "single",
		},
		metric_ingestor.ScanRunStatusSucceeded,
		repo_indexer.ScanStatusFailed, // mismatch
		time.Now(),
	)
	if err == nil {
		t.Fatal("FinalizeScanRun mismatched pair: err=nil, want validation error")
	}
}

// TestPGScanRunStore_FinalizeScanRun_RejectsNonTerminal pins
// the running-status guard at the PG layer.
func TestPGScanRunStore_FinalizeScanRun_RejectsNonTerminal(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()
	_ = mock

	err := s.FinalizeScanRun(context.Background(),
		metric_ingestor.ScanRunClaim{
			ScanRunID: uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555558")),
			RepoID:    pgScanRunTestRepoID, SHA: pgScanRunTestSHA,
			Kind: "full", SHABinding: "single",
		},
		metric_ingestor.ScanRunStatusRunning, // non-terminal
		repo_indexer.ScanStatusScanned,
		time.Now(),
	)
	if err == nil {
		t.Fatal("FinalizeScanRun non-terminal: err=nil, want validation error")
	}
}

// TestPGScanRunStore_FinalizeScanRun_RejectsDoubleFinalize
// pins the rowsAffected=0 surface: a second finalize call
// finds the scan_run already at status=succeeded so the
// UPDATE affects 0 rows; the store rolls back and returns
// ErrConcurrentFinalize.
func TestPGScanRunStore_FinalizeScanRun_RejectsDoubleFinalize(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	scanRunID := uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555559"))
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE\s+"` + pgScanRunTestSchema + `"\."scan_run"`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // already finalized
	mock.ExpectRollback()

	err := s.FinalizeScanRun(context.Background(),
		metric_ingestor.ScanRunClaim{
			ScanRunID: scanRunID, RepoID: pgScanRunTestRepoID, SHA: pgScanRunTestSHA,
			Kind: "full", SHABinding: "single",
		},
		metric_ingestor.ScanRunStatusSucceeded,
		repo_indexer.ScanStatusScanned,
		time.Now(),
	)
	if !errors.Is(err, metric_ingestor.ErrConcurrentFinalize) {
		t.Errorf("FinalizeScanRun double: err=%v, want errors.Is ErrConcurrentFinalize", err)
	}
}

// ────────────────────────────────────────────────────────
// iter-6 evaluator item 2: sqlmock coverage for the new
// PeekNextPendingCommits + ClaimSpecificPendingCommit
// surfaces introduced by iter-5 evaluator item 4
// (head-of-line probe fanout). The pre-existing test
// suite ended at FinalizeScanRun_RejectsDoubleFinalize and
// did NOT pin the SQL shape, no-row, or
// concurrent-race behaviour of these two methods. These
// tests close that gap.
// ────────────────────────────────────────────────────────

// TestPGScanRunStore_PeekNextPendingCommits_Happy pins the
// SQL shape of the multi-row peek that drives the
// state machine's head-of-line probe loop. The query must:
//
//   - SELECT (repo_id, sha, committed_at) FROM
//     <schema>.commit
//   - WHERE scan_status = 'pending'
//   - ORDER BY committed_at ASC, sha ASC (stable order so
//     ties resolve deterministically across replicas)
//   - LIMIT $1 (bound by the caller's fanout)
//   - NO `FOR UPDATE` -- peeks must not block claims.
func TestPGScanRunStore_PeekNextPendingCommits_Happy(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	committedAt1 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	committedAt2 := time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC)
	repoID2 := uuid.Must(uuid.FromString("aaaaaaaa-bbbb-cccc-dddd-eeeeffff0002"))
	sha2 := "def4567890123456789012345678901234567890"

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT repo_id, sha, committed_at`) +
		`\s+FROM\s+"` + pgScanRunTestSchema + `"\."commit"` +
		`\s+WHERE\s+scan_status\s*=\s*'pending'` +
		`\s+ORDER\s+BY\s+committed_at\s+ASC,\s+sha\s+ASC` +
		`\s+LIMIT\s+\$1`).
		WithArgs(16).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha", "committed_at"}).
			AddRow(pgScanRunTestRepoID, pgScanRunTestSHA, committedAt1).
			AddRow(repoID2, sha2, committedAt2))

	got, err := s.PeekNextPendingCommits(context.Background(), 16)
	if err != nil {
		t.Fatalf("PeekNextPendingCommits: err=%v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("PeekNextPendingCommits: len=%d, want 2", len(got))
	}
	if got[0].RepoID != pgScanRunTestRepoID || got[0].SHA != pgScanRunTestSHA {
		t.Errorf("got[0]=(%s,%s), want (%s,%s)", got[0].RepoID, got[0].SHA, pgScanRunTestRepoID, pgScanRunTestSHA)
	}
	if got[1].RepoID != repoID2 || got[1].SHA != sha2 {
		t.Errorf("got[1]=(%s,%s), want (%s,%s)", got[1].RepoID, got[1].SHA, repoID2, sha2)
	}
	// The state machine relies on committed_at ASC ordering
	// so the head-of-line skip is deterministic per tick;
	// pin it here.
	if !got[0].CommittedAt.Equal(committedAt1) || !got[1].CommittedAt.Equal(committedAt2) {
		t.Errorf("committed_at order: got=(%s,%s), want (%s,%s)",
			got[0].CommittedAt, got[1].CommittedAt, committedAt1, committedAt2)
	}
}

// TestPGScanRunStore_PeekNextPendingCommits_Empty pins the
// "no pending work" surface: rows.Next() returns false on
// first call -> the method returns (nil, nil), not an
// error. The state machine interprets a nil slice as the
// idle signal and emits SkipReasonNoCandidate without a
// claim attempt.
func TestPGScanRunStore_PeekNextPendingCommits_Empty(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	mock.ExpectQuery(`SELECT\s+repo_id,\s+sha,\s+committed_at`).
		WithArgs(4).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha", "committed_at"}))

	got, err := s.PeekNextPendingCommits(context.Background(), 4)
	if err != nil {
		t.Fatalf("PeekNextPendingCommits empty: err=%v, want nil", err)
	}
	if got != nil {
		t.Errorf("PeekNextPendingCommits empty: got=%v, want nil", got)
	}
}

// TestPGScanRunStore_PeekNextPendingCommits_RejectsZeroLimit
// pins the input-validation surface: zero/negative limits
// are a wiring bug. The method must reject them with a
// descriptive error rather than silently producing a
// no-LIMIT query that would scan the whole pending queue.
func TestPGScanRunStore_PeekNextPendingCommits_RejectsZeroLimit(t *testing.T) {
	t.Parallel()
	s, _, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	for _, bad := range []int{0, -1, -10} {
		if _, err := s.PeekNextPendingCommits(context.Background(), bad); err == nil {
			t.Errorf("PeekNextPendingCommits(%d): err=nil, want non-nil", bad)
		}
	}
}

// TestPGScanRunStore_PeekNextPendingCommits_QueryError pins
// the infrastructure-failure surface: a SELECT error
// (e.g. dropped connection) propagates wrapped with the
// method-name prefix so callers can grep it in logs.
func TestPGScanRunStore_PeekNextPendingCommits_QueryError(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	wantErr := errors.New("simulated PG connection reset")
	mock.ExpectQuery(`SELECT\s+repo_id,\s+sha,\s+committed_at`).
		WithArgs(2).
		WillReturnError(wantErr)

	_, err := s.PeekNextPendingCommits(context.Background(), 2)
	if err == nil {
		t.Fatal("PeekNextPendingCommits: err=nil, want non-nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("PeekNextPendingCommits: err=%v, want wrap of %v", err, wantErr)
	}
}

// TestPGScanRunStore_ClaimSpecificPendingCommit_Happy pins
// the full state-transitioning transaction shape for the
// targeted claim path. The transaction must:
//
//  1. BEGIN
//  2. SELECT FOR UPDATE SKIP LOCKED the specific
//     (repo_id, sha) row, filtered on scan_status='pending'.
//  3. INSERT scan_run.
//  4. UPDATE commit.scan_status pending -> scanning.
//  5. COMMIT.
//
// Each step's SQL shape is pinned so a future refactor can't
// silently change the contract.
func TestPGScanRunStore_ClaimSpecificPendingCommit_Happy(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	committedAt := time.Date(2026, 6, 2, 10, 0, 0, 0, time.UTC)
	openedAt := time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT repo_id, sha, committed_at`) +
		`\s+FROM\s+"` + pgScanRunTestSchema + `"\."commit"` +
		`\s+WHERE\s+repo_id\s*=\s*\$1\s+AND\s+sha\s*=\s*\$2\s+AND\s+scan_status\s*=\s*'pending'` +
		`\s+FOR\s+UPDATE\s+SKIP\s+LOCKED`).
		WithArgs(pgScanRunTestRepoID, pgScanRunTestSHA).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha", "committed_at"}).
			AddRow(pgScanRunTestRepoID, pgScanRunTestSHA, committedAt))
	mock.ExpectExec(`INSERT\s+INTO\s+"` + pgScanRunTestSchema + `"\."scan_run"`).
		WithArgs(
			sqlmock.AnyArg(),    // scan_run_id (minted)
			pgScanRunTestRepoID, // repo_id
			"full",              // kind
			"single",            // sha_binding
			pgScanRunTestSHA,    // to_sha
			openedAt,            // started_at (UTC)
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+"` + pgScanRunTestSchema + `"\."commit"\s+SET\s+scan_status\s*=\s*'scanning'` +
		`\s+WHERE\s+repo_id\s*=\s*\$1\s+AND\s+sha\s*=\s*\$2\s+AND\s+scan_status\s*=\s*'pending'`).
		WithArgs(pgScanRunTestRepoID, pgScanRunTestSHA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	claim, ok, err := s.ClaimSpecificPendingCommit(context.Background(),
		pgScanRunTestRepoID, pgScanRunTestSHA,
		metric_ingestor.ClaimRequest{
			Kind:       "full",
			SHABinding: "single",
			OpenedAt:   openedAt,
		})
	if err != nil {
		t.Fatalf("ClaimSpecificPendingCommit: err=%v, want nil", err)
	}
	if !ok {
		t.Fatal("ClaimSpecificPendingCommit: ok=false, want true")
	}
	if claim.RepoID != pgScanRunTestRepoID || claim.SHA != pgScanRunTestSHA {
		t.Errorf("claim=(%s,%s), want (%s,%s)", claim.RepoID, claim.SHA, pgScanRunTestRepoID, pgScanRunTestSHA)
	}
	if claim.ScanRunID == uuid.Nil {
		t.Error("claim.ScanRunID is the zero UUID; want a freshly-minted UUID")
	}
	if claim.Kind != "full" || claim.SHABinding != "single" {
		t.Errorf("claim kind/binding=(%q,%q), want (full,single)", claim.Kind, claim.SHABinding)
	}
	if !claim.OpenedAt.Equal(openedAt) {
		t.Errorf("claim.OpenedAt=%s, want %s", claim.OpenedAt, openedAt)
	}
}

// TestPGScanRunStore_ClaimSpecificPendingCommit_NotFound
// pins the raced-away surface: the SELECT FOR UPDATE
// returns sql.ErrNoRows (the row already transitioned out
// of 'pending' between peek and claim). The method must
// return (zero, false, nil) -- NOT an error -- so the
// state-machine probe loop can move on to the next
// peeked candidate without panicking.
func TestPGScanRunStore_ClaimSpecificPendingCommit_NotFound(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT\s+repo_id,\s+sha,\s+committed_at`).
		WithArgs(pgScanRunTestRepoID, pgScanRunTestSHA).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	claim, ok, err := s.ClaimSpecificPendingCommit(context.Background(),
		pgScanRunTestRepoID, pgScanRunTestSHA,
		metric_ingestor.ClaimRequest{
			Kind: "full", SHABinding: "single", OpenedAt: time.Now(),
		})
	if err != nil {
		t.Errorf("ClaimSpecificPendingCommit raced-away: err=%v, want nil", err)
	}
	if ok {
		t.Error("ClaimSpecificPendingCommit raced-away: ok=true, want false")
	}
	if claim.ScanRunID != uuid.Nil || claim.RepoID != uuid.Nil {
		t.Errorf("ClaimSpecificPendingCommit raced-away: claim=%+v, want zero", claim)
	}
}

// TestPGScanRunStore_ClaimSpecificPendingCommit_ConcurrentRace
// pins the "won the SELECT, lost the UPDATE" surface: the
// SELECT returned a row but the UPDATE affected 0 rows
// (some other writer transitioned the row between
// statements). The method must surface
// ErrConcurrentClaim so the dispatcher can re-peek.
func TestPGScanRunStore_ClaimSpecificPendingCommit_ConcurrentRace(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	committedAt := time.Date(2026, 6, 3, 10, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT\s+repo_id,\s+sha,\s+committed_at`).
		WithArgs(pgScanRunTestRepoID, pgScanRunTestSHA).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "sha", "committed_at"}).
			AddRow(pgScanRunTestRepoID, pgScanRunTestSHA, committedAt))
	mock.ExpectExec(`INSERT\s+INTO\s+"` + pgScanRunTestSchema + `"\."scan_run"`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+"` + pgScanRunTestSchema + `"\."commit"\s+SET\s+scan_status\s*=\s*'scanning'`).
		WillReturnResult(sqlmock.NewResult(0, 0)) // race lost
	mock.ExpectRollback()

	_, _, err := s.ClaimSpecificPendingCommit(context.Background(),
		pgScanRunTestRepoID, pgScanRunTestSHA,
		metric_ingestor.ClaimRequest{
			Kind: "full", SHABinding: "single", OpenedAt: time.Now(),
		})
	if !errors.Is(err, metric_ingestor.ErrConcurrentClaim) {
		t.Errorf("ClaimSpecificPendingCommit race: err=%v, want errors.Is ErrConcurrentClaim", err)
	}
}

// TestPGScanRunStore_ClaimSpecificPendingCommit_ValidatesArgs
// pins the input-validation surface: zero RepoID, empty
// SHA, and bad ClaimRequest all reject BEFORE opening the
// transaction. Any one of these reaching the DB layer
// would be a wiring bug.
func TestPGScanRunStore_ClaimSpecificPendingCommit_ValidatesArgs(t *testing.T) {
	t.Parallel()
	s, _, cleanup := newSQLMockScanRunStore(t)
	defer cleanup()

	good := metric_ingestor.ClaimRequest{Kind: "full", SHABinding: "single", OpenedAt: time.Now()}
	bad := metric_ingestor.ClaimRequest{Kind: "incremental" /* not canonical */, SHABinding: "single", OpenedAt: time.Now()}

	for _, tc := range []struct {
		name   string
		repoID uuid.UUID
		sha    string
		req    metric_ingestor.ClaimRequest
	}{
		{"zero-RepoID", uuid.Nil, pgScanRunTestSHA, good},
		{"empty-SHA", pgScanRunTestRepoID, "", good},
		{"bad-Kind", pgScanRunTestRepoID, pgScanRunTestSHA, bad},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			_, ok, err := s.ClaimSpecificPendingCommit(context.Background(), tc.repoID, tc.sha, tc.req)
			if err == nil {
				t.Errorf("[%s] err=nil, want non-nil", tc.name)
			}
			if ok {
				t.Errorf("[%s] ok=true, want false", tc.name)
			}
		})
	}
}
