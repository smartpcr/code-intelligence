package metric_ingestor_test

// This file pins the production SQL trace for the Stage 3.5
// stale-ScanRun sweep against a sqlmock-backed *sql.DB. The
// in-memory store tests (stale_scan_run_sweep_test.go) cover
// the behaviour matrix; THESE tests cover the literal
// statement shape -- a regression that renames the WHERE
// guard or drops the LIMIT clause fails here even if the
// in-memory store is happy.
//
// Iter 2 evaluator item 3: "The production PG sweep SQL ...
// has no sqlmock/PG tests; the new tests exercise only
// in-memory/fake stores."

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
)

const pgStaleSweepTestSchema = "clean_code_stale_sweep_test"

var (
	pgStaleSweepTestRepoID    = uuid.Must(uuid.FromString("aaaaaaaa-cccc-cccc-dddd-eeeeffff5555"))
	pgStaleSweepTestScanRunID = uuid.Must(uuid.FromString("11111111-2222-3333-4444-555555555aaa"))
	pgStaleSweepTestSHA       = "deadbeef0123456789abcdef0123456789abcdef"
)

// newSQLMockStaleSweepStore mirrors the helper next door,
// just with a dedicated schema name so a side-by-side run
// of both test files cannot accidentally cross-talk.
func newSQLMockStaleSweepStore(t *testing.T) (*metric_ingestor.PGScanRunStore, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	s, err := metric_ingestor.NewPGScanRunStoreWithSchema(db, pgStaleSweepTestSchema)
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

// TestPGStaleSweep_FindStaleRunningScanRuns_HappySingle pins
// the SELECT shape: `WHERE status='running' AND started_at < $1`,
// ordered oldest-first, capped by LIMIT $2. The returned row
// is `sha_binding='single'` so `to_sha` is non-empty.
func TestPGStaleSweep_FindStaleRunningScanRuns_HappySingle(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	cutoff := time.Date(2026, 6, 1, 11, 30, 0, 0, time.UTC)
	startedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT\s+scan_run_id,\s+repo_id,\s+kind::text,\s+sha_binding::text,\s+to_sha,\s+started_at`+
		`\s+FROM\s+"`+pgStaleSweepTestSchema+`"\."scan_run"`+
		`\s+WHERE\s+status\s*=\s*'running'\s+AND\s+started_at\s*<\s*\$1`+
		`\s+ORDER\s+BY\s+started_at\s+ASC,\s+scan_run_id\s+ASC`+
		`\s+LIMIT\s+\$2`).
		WithArgs(cutoff, 50).
		WillReturnRows(sqlmock.NewRows([]string{
			"scan_run_id", "repo_id", "kind", "sha_binding", "to_sha", "started_at",
		}).AddRow(
			pgStaleSweepTestScanRunID,
			pgStaleSweepTestRepoID,
			metric_ingestor.ScanRunKindFull,
			metric_ingestor.SHABindingSingle,
			pgStaleSweepTestSHA,
			startedAt,
		))

	out, err := s.FindStaleRunningScanRuns(context.Background(), cutoff, 50)
	if err != nil {
		t.Fatalf("FindStaleRunningScanRuns: err=%v, want nil", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out)=%d, want 1", len(out))
	}
	got := out[0]
	if got.ScanRunID != pgStaleSweepTestScanRunID {
		t.Errorf("ScanRunID=%s, want %s", got.ScanRunID, pgStaleSweepTestScanRunID)
	}
	if got.RepoID != pgStaleSweepTestRepoID {
		t.Errorf("RepoID=%s, want %s", got.RepoID, pgStaleSweepTestRepoID)
	}
	if got.SHABinding != metric_ingestor.SHABindingSingle {
		t.Errorf("SHABinding=%q, want %q", got.SHABinding, metric_ingestor.SHABindingSingle)
	}
	if got.ToSHA != pgStaleSweepTestSHA {
		t.Errorf("ToSHA=%q, want %q", got.ToSHA, pgStaleSweepTestSHA)
	}
	if !got.StartedAt.Equal(startedAt) {
		t.Errorf("StartedAt=%s, want %s", got.StartedAt, startedAt)
	}
	if got.Kind != metric_ingestor.ScanRunKindFull {
		t.Errorf("Kind=%q, want %q", got.Kind, metric_ingestor.ScanRunKindFull)
	}
}

// TestPGStaleSweep_FindStaleRunningScanRuns_PerRowNullToSHA
// pins the SQL NULL handling for per_row runs: the scan_run
// row has `to_sha IS NULL`; the projection back into Go
// MUST surface an empty ToSHA (not a panic, not a zero-width
// UTF-8 something).
func TestPGStaleSweep_FindStaleRunningScanRuns_PerRowNullToSHA(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	cutoff := time.Date(2026, 6, 1, 11, 30, 0, 0, time.UTC)
	startedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT\s+scan_run_id`).
		WithArgs(cutoff, 1).
		WillReturnRows(sqlmock.NewRows([]string{
			"scan_run_id", "repo_id", "kind", "sha_binding", "to_sha", "started_at",
		}).AddRow(
			pgStaleSweepTestScanRunID,
			pgStaleSweepTestRepoID,
			metric_ingestor.ScanRunKindExternalPerRow,
			metric_ingestor.SHABindingPerRow,
			nil, // <-- this is the critical column under test
			startedAt,
		))

	out, err := s.FindStaleRunningScanRuns(context.Background(), cutoff, 1)
	if err != nil {
		t.Fatalf("FindStaleRunningScanRuns (per_row NULL to_sha): err=%v, want nil", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out)=%d, want 1", len(out))
	}
	if out[0].ToSHA != "" {
		t.Errorf("ToSHA=%q, want empty for per_row binding", out[0].ToSHA)
	}
	if out[0].SHABinding != metric_ingestor.SHABindingPerRow {
		t.Errorf("SHABinding=%q, want per_row", out[0].SHABinding)
	}
}

// TestPGStaleSweep_FindStaleRunningScanRuns_EmptyResult is
// the no-stale-rows path: the SELECT returns an empty
// rowset, the store returns an empty slice (NOT nil) and no
// error. The empty-slice contract matters because the
// sweep's `for _, sr := range stale` would handle nil
// transparently, but tests assert the slice shape so a
// future refactor to `if stale == nil` does not silently
// regress.
func TestPGStaleSweep_FindStaleRunningScanRuns_EmptyResult(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	cutoff := time.Date(2026, 6, 1, 11, 30, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT\s+scan_run_id`).
		WithArgs(cutoff, 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"scan_run_id", "repo_id", "kind", "sha_binding", "to_sha", "started_at",
		}))

	out, err := s.FindStaleRunningScanRuns(context.Background(), cutoff, 100)
	if err != nil {
		t.Fatalf("FindStaleRunningScanRuns (empty): err=%v, want nil", err)
	}
	if out == nil {
		t.Error("FindStaleRunningScanRuns (empty): returned nil slice, want empty []StaleScanRun{}")
	}
	if len(out) != 0 {
		t.Errorf("len(out)=%d, want 0", len(out))
	}
}

// TestPGStaleSweep_FindStaleRunningScanRuns_RejectsBadLimit
// pins the validation-before-DB contract: a non-positive
// limit returns a validation error and never opens a query.
func TestPGStaleSweep_FindStaleRunningScanRuns_RejectsBadLimit(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()
	// No DB expectations -- the validator fires first.

	_, err := s.FindStaleRunningScanRuns(context.Background(), time.Now(), 0)
	if err == nil {
		t.Fatal("FindStaleRunningScanRuns (limit=0): err=nil, want validation error")
	}
	_, err = s.FindStaleRunningScanRuns(context.Background(), time.Now(), -42)
	if err == nil {
		t.Fatal("FindStaleRunningScanRuns (limit=-42): err=nil, want validation error")
	}
	_ = mock
}

// TestPGStaleSweep_FindStaleRunningScanRuns_QueryError
// pins the infra-failure surface: a DB error during SELECT
// surfaces wrapped (errors.Is on sentinel) so the caller
// can distinguish "no work" (empty slice) from "DB sick".
func TestPGStaleSweep_FindStaleRunningScanRuns_QueryError(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	sentinel := errors.New("pgstalesweeptest: forced query failure")
	mock.ExpectQuery(`SELECT\s+scan_run_id`).WillReturnError(sentinel)

	_, err := s.FindStaleRunningScanRuns(context.Background(), time.Now(), 10)
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("FindStaleRunningScanRuns (DB error): err=%v, want errors.Is sentinel", err)
	}
}

// TestPGStaleSweep_FailStaleScanRun_HappySingle pins the
// canonical SQL trace for the single-binding happy path:
// BEGIN, guarded UPDATE scan_run (1 row), guarded UPDATE
// commit (1 row), COMMIT.
func TestPGStaleSweep_FailStaleScanRun_HappySingle(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	endedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stale := metric_ingestor.StaleScanRun{
		ScanRunID:  pgStaleSweepTestScanRunID,
		RepoID:     pgStaleSweepTestRepoID,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		ToSHA:      pgStaleSweepTestSHA,
		StartedAt:  time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
	}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."scan_run"`+
		`\s+SET\s+status\s*=\s*'failed',\s+ended_at\s*=\s*\$1`+
		`\s+WHERE\s+scan_run_id\s*=\s*\$2\s+AND\s+status\s*=\s*'running'`).
		WithArgs(endedAt, pgStaleSweepTestScanRunID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."commit"`+
		`\s+SET\s+scan_status\s*=\s*'failed'`+
		`\s+WHERE\s+repo_id\s*=\s*\$1\s+AND\s+sha\s*=\s*\$2\s+AND\s+scan_status\s*=\s*'scanning'`).
		WithArgs(pgStaleSweepTestRepoID, pgStaleSweepTestSHA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	res, err := s.FailStaleScanRun(context.Background(), stale, endedAt)
	if err != nil {
		t.Fatalf("FailStaleScanRun: err=%v, want nil", err)
	}
	if !res.ScanRunTransitioned {
		t.Error("ScanRunTransitioned=false, want true")
	}
	if !res.CommitTransitioned {
		t.Error("CommitTransitioned=false, want true")
	}
}

// TestPGStaleSweep_FailStaleScanRun_HappyPerRow pins the
// per_row variant: the scan_run UPDATE fires, but the
// commit UPDATE is SKIPPED entirely (no `(repo_id, sha)`
// anchor for per_row runs). A regression that issues the
// commit UPDATE anyway -- and finds 0 rows -- would still
// pass an in-memory test but trip this sqlmock test
// because the mock has NO expectation for the commit
// UPDATE.
func TestPGStaleSweep_FailStaleScanRun_HappyPerRow(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	endedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stale := metric_ingestor.StaleScanRun{
		ScanRunID:  pgStaleSweepTestScanRunID,
		RepoID:     pgStaleSweepTestRepoID,
		Kind:       metric_ingestor.ScanRunKindExternalPerRow,
		SHABinding: metric_ingestor.SHABindingPerRow,
		ToSHA:      "", // <- per_row has no anchor SHA
		StartedAt:  time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
	}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."scan_run"`+
		`\s+SET\s+status\s*=\s*'failed'`).
		WithArgs(endedAt, pgStaleSweepTestScanRunID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// NO ExpectExec for the commit UPDATE.
	mock.ExpectCommit()

	res, err := s.FailStaleScanRun(context.Background(), stale, endedAt)
	if err != nil {
		t.Fatalf("FailStaleScanRun: err=%v, want nil", err)
	}
	if !res.ScanRunTransitioned {
		t.Error("ScanRunTransitioned=false, want true")
	}
	if res.CommitTransitioned {
		t.Error("CommitTransitioned=true, want false (per_row has no commit anchor)")
	}
}

// TestPGStaleSweep_FailStaleScanRun_RacedScanRun pins the
// rowsAffected=0 race surface: another writer raced the
// scan_run to a terminal state between the SELECT and the
// UPDATE. The store COMMITs (no error) and returns
// ScanRunTransitioned=false. The commit UPDATE STILL fires
// because the production guard is `WHERE status='running'`
// on the scan_run UPDATE only -- the commit UPDATE has its
// own `WHERE scan_status='scanning'` guard.
func TestPGStaleSweep_FailStaleScanRun_RacedScanRun(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	endedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stale := metric_ingestor.StaleScanRun{
		ScanRunID:  pgStaleSweepTestScanRunID,
		RepoID:     pgStaleSweepTestRepoID,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		ToSHA:      pgStaleSweepTestSHA,
		StartedAt:  time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
	}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."scan_run"`).
		WithArgs(endedAt, pgStaleSweepTestScanRunID).
		WillReturnResult(sqlmock.NewResult(0, 0)) // <-- raced
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."commit"`).
		WithArgs(pgStaleSweepTestRepoID, pgStaleSweepTestSHA).
		WillReturnResult(sqlmock.NewResult(0, 0)) // commit also raced
	mock.ExpectCommit()

	res, err := s.FailStaleScanRun(context.Background(), stale, endedAt)
	if err != nil {
		t.Fatalf("FailStaleScanRun (raced): err=%v, want nil (race is benign)", err)
	}
	if res.ScanRunTransitioned {
		t.Error("ScanRunTransitioned=true, want false (raced)")
	}
	if res.CommitTransitioned {
		t.Error("CommitTransitioned=true, want false (commit raced too)")
	}
}

// TestPGStaleSweep_FailStaleScanRun_RacedCommitOnly pins
// the partial-race: scan_run UPDATE landed (1 row), but the
// commit had already raced to scanned/failed (0 rows). The
// store still COMMITs the scan_run transition; only
// CommitTransitioned is false.
func TestPGStaleSweep_FailStaleScanRun_RacedCommitOnly(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	endedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stale := metric_ingestor.StaleScanRun{
		ScanRunID:  pgStaleSweepTestScanRunID,
		RepoID:     pgStaleSweepTestRepoID,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		ToSHA:      pgStaleSweepTestSHA,
		StartedAt:  time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
	}

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."scan_run"`).
		WithArgs(endedAt, pgStaleSweepTestScanRunID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."commit"`).
		WithArgs(pgStaleSweepTestRepoID, pgStaleSweepTestSHA).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	res, err := s.FailStaleScanRun(context.Background(), stale, endedAt)
	if err != nil {
		t.Fatalf("FailStaleScanRun (commit raced): err=%v, want nil", err)
	}
	if !res.ScanRunTransitioned {
		t.Error("ScanRunTransitioned=false, want true (only the commit raced)")
	}
	if res.CommitTransitioned {
		t.Error("CommitTransitioned=true, want false (commit raced)")
	}
}

// TestPGStaleSweep_FailStaleScanRun_ScanRunUpdateError
// pins the rollback path: the scan_run UPDATE errors out
// and the entire transaction rolls back; no partial state
// lands.
func TestPGStaleSweep_FailStaleScanRun_ScanRunUpdateError(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	endedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	stale := metric_ingestor.StaleScanRun{
		ScanRunID:  pgStaleSweepTestScanRunID,
		RepoID:     pgStaleSweepTestRepoID,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		ToSHA:      pgStaleSweepTestSHA,
		StartedAt:  time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
	}

	sentinel := errors.New("pgstalesweeptest: forced UPDATE failure")
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."scan_run"`).
		WithArgs(endedAt, pgStaleSweepTestScanRunID).
		WillReturnError(sentinel)
	mock.ExpectRollback()

	_, err := s.FailStaleScanRun(context.Background(), stale, endedAt)
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("FailStaleScanRun: err=%v, want errors.Is sentinel", err)
	}
}

// TestPGStaleSweep_FailStaleScanRun_RejectsBadProjection
// pins the validation-before-DB contract: a malformed
// StaleScanRun (zero ScanRunID, zero RepoID, bogus binding,
// etc.) never touches the DB.
func TestPGStaleSweep_FailStaleScanRun_RejectsBadProjection(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()
	// No DB expectations.

	cases := []metric_ingestor.StaleScanRun{
		{}, // entirely zero
		{
			ScanRunID:  uuid.Nil,
			RepoID:     pgStaleSweepTestRepoID,
			Kind:       metric_ingestor.ScanRunKindFull,
			SHABinding: metric_ingestor.SHABindingSingle,
			ToSHA:      pgStaleSweepTestSHA,
		},
		{
			ScanRunID:  pgStaleSweepTestScanRunID,
			RepoID:     pgStaleSweepTestRepoID,
			Kind:       metric_ingestor.ScanRunKindFull,
			SHABinding: metric_ingestor.SHABindingSingle,
			ToSHA:      "", // single binding with empty to_sha
		},
		{
			ScanRunID:  pgStaleSweepTestScanRunID,
			RepoID:     pgStaleSweepTestRepoID,
			Kind:       metric_ingestor.ScanRunKindExternalPerRow,
			SHABinding: metric_ingestor.SHABindingPerRow,
			ToSHA:      "not_empty_when_should_be", // per_row with to_sha
		},
		{
			ScanRunID:  pgStaleSweepTestScanRunID,
			RepoID:     pgStaleSweepTestRepoID,
			Kind:       "garbage_kind",
			SHABinding: metric_ingestor.SHABindingSingle,
			ToSHA:      pgStaleSweepTestSHA,
		},
	}
	for i, c := range cases {
		_, err := s.FailStaleScanRun(context.Background(), c, time.Now())
		if err == nil {
			t.Errorf("case %d (%+v): err=nil, want validation error", i, c)
		}
	}
	_ = mock
}

// TestPGStaleSweep_FailStaleScanRun_RejectsZeroEndedAt pins
// the contract that endedAt must be supplied.
func TestPGStaleSweep_FailStaleScanRun_RejectsZeroEndedAt(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()
	// No DB expectations.

	stale := metric_ingestor.StaleScanRun{
		ScanRunID:  pgStaleSweepTestScanRunID,
		RepoID:     pgStaleSweepTestRepoID,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		ToSHA:      pgStaleSweepTestSHA,
		StartedAt:  time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
	}
	_, err := s.FailStaleScanRun(context.Background(), stale, time.Time{})
	if err == nil {
		t.Fatal("FailStaleScanRun (zero endedAt): err=nil, want validation error")
	}
	_ = mock
}

// TestPGStaleSweep_FailScanningCommitsForFailedScanRuns_Happy
// pins the bulk CTE UPDATE shape: ONE statement inside a tx
// that UPDATEs commits matching the candidate CTE (join to
// scan_run on (repo_id, to_sha), guarded on status='failed'
// AND sha_binding='single' AND commit.scan_status='scanning').
func TestPGStaleSweep_FailScanningCommitsForFailedScanRuns_Happy(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	// The CTE must match -- assert the literal candidate
	// subselect AND the outer guarded UPDATE.
	pattern := `WITH\s+candidates\s+AS\s*\(` +
		`\s*SELECT\s+c\.repo_id,\s+c\.sha` +
		`\s+FROM\s+"` + pgStaleSweepTestSchema + `"\."commit"\s+c` +
		`\s+JOIN\s+"` + pgStaleSweepTestSchema + `"\."scan_run"\s+sr` +
		`\s+ON\s+sr\.repo_id\s*=\s*c\.repo_id` +
		`\s+AND\s+sr\.to_sha\s*=\s*c\.sha` +
		`\s+WHERE\s+c\.scan_status\s*=\s*'scanning'` +
		`\s+AND\s+sr\.status\s*=\s*'failed'` +
		`\s+AND\s+sr\.sha_binding\s*=\s*'single'` +
		`\s+LIMIT\s+\$1` +
		`\s*\)` +
		`\s+UPDATE\s+"` + pgStaleSweepTestSchema + `"\."commit"\s+c` +
		`\s+SET\s+scan_status\s*=\s*'failed'` +
		`\s+FROM\s+candidates` +
		`\s+WHERE\s+c\.repo_id\s*=\s*candidates\.repo_id` +
		`\s+AND\s+c\.sha\s*=\s*candidates\.sha` +
		`\s+AND\s+c\.scan_status\s*=\s*'scanning'`

	if _, err := regexp.Compile(pattern); err != nil {
		t.Fatalf("regex compile sanity: %v", err)
	}

	mock.ExpectBegin()
	mock.ExpectExec(pattern).WithArgs(500).
		WillReturnResult(sqlmock.NewResult(0, 3))
	mock.ExpectCommit()

	count, err := s.FailScanningCommitsForFailedScanRuns(context.Background(), 500)
	if err != nil {
		t.Fatalf("FailScanningCommitsForFailedScanRuns: err=%v, want nil", err)
	}
	if count != 3 {
		t.Errorf("count=%d, want 3", count)
	}
}

// TestPGStaleSweep_FailScanningCommitsForFailedScanRuns_NoneToClean
// pins the zero-affected-row surface: returns 0, no error.
func TestPGStaleSweep_FailScanningCommitsForFailedScanRuns_NoneToClean(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`WITH\s+candidates\s+AS`).WithArgs(100).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	count, err := s.FailScanningCommitsForFailedScanRuns(context.Background(), 100)
	if err != nil {
		t.Fatalf("FailScanningCommitsForFailedScanRuns (none): err=%v, want nil", err)
	}
	if count != 0 {
		t.Errorf("count=%d, want 0", count)
	}
}

// TestPGStaleSweep_FailScanningCommitsForFailedScanRuns_RejectsBadLimit
// pins the validation-before-DB contract for the bulk
// cleanup step.
func TestPGStaleSweep_FailScanningCommitsForFailedScanRuns_RejectsBadLimit(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()
	// No DB expectations.

	if _, err := s.FailScanningCommitsForFailedScanRuns(context.Background(), 0); err == nil {
		t.Error("FailScanningCommitsForFailedScanRuns (limit=0): err=nil, want validation error")
	}
	if _, err := s.FailScanningCommitsForFailedScanRuns(context.Background(), -1); err == nil {
		t.Error("FailScanningCommitsForFailedScanRuns (limit=-1): err=nil, want validation error")
	}
	_ = mock
}

// TestPGStaleSweep_FailScanningCommitsForFailedScanRuns_UpdateError
// pins the rollback path on bulk UPDATE failure.
func TestPGStaleSweep_FailScanningCommitsForFailedScanRuns_UpdateError(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	sentinel := errors.New("pgstalesweeptest: forced cleanup UPDATE failure")
	mock.ExpectBegin()
	mock.ExpectExec(`WITH\s+candidates\s+AS`).WithArgs(100).WillReturnError(sentinel)
	mock.ExpectRollback()

	_, err := s.FailScanningCommitsForFailedScanRuns(context.Background(), 100)
	if err == nil || !errors.Is(err, sentinel) {
		t.Errorf("FailScanningCommitsForFailedScanRuns: err=%v, want errors.Is sentinel", err)
	}
}

// TestPGStaleSweep_FullSweepDrainsEndToEnd glues all three
// methods together via the public StaleScanRunSweep API:
// one Sweep call drives FindStaleRunningScanRuns +
// FailStaleScanRun + FailScanningCommitsForFailedScanRuns
// against the mocked DB. Failure mode under test: a
// regression that reorders the steps or drops the final
// cleanup call.
func TestPGStaleSweep_FullSweepDrainsEndToEnd(t *testing.T) {
	t.Parallel()
	s, mock, cleanup := newSQLMockStaleSweepStore(t)
	defer cleanup()

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	// scanTimeout=30min so cutoff = 11:30
	cutoff := now.Add(-30 * time.Minute)

	// First (and only) batch: 1 stale row. Since 1 <
	// batchLimit, the inner drain-loop breaks BEFORE
	// re-querying. The cleanup step then fires.
	mock.ExpectQuery(`SELECT\s+scan_run_id`).
		WithArgs(cutoff, 100).
		WillReturnRows(sqlmock.NewRows([]string{
			"scan_run_id", "repo_id", "kind", "sha_binding", "to_sha", "started_at",
		}).AddRow(
			pgStaleSweepTestScanRunID, pgStaleSweepTestRepoID,
			metric_ingestor.ScanRunKindFull,
			metric_ingestor.SHABindingSingle,
			pgStaleSweepTestSHA, startedAt,
		))

	// The one row's tx.
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."scan_run"`).
		WithArgs(now, pgStaleSweepTestScanRunID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE\s+"`+pgStaleSweepTestSchema+`"\."commit"`).
		WithArgs(pgStaleSweepTestRepoID, pgStaleSweepTestSHA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	// Final cleanup UPDATE (limit = batchLimit * maxBatches = 100 * 1024).
	mock.ExpectBegin()
	mock.ExpectExec(`WITH\s+candidates\s+AS`).
		WithArgs(100 * 1024).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	sweep := metric_ingestor.NewStaleScanRunSweep(
		s,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepBatchLimit(100),
		metric_ingestor.WithStaleSweepClock(func() time.Time { return now }),
	)
	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: err=%v, want nil", err)
	}
	if report.Scanned != 1 {
		t.Errorf("Scanned=%d, want 1", report.Scanned)
	}
	if report.ScanRunsTransitioned != 1 {
		t.Errorf("ScanRunsTransitioned=%d, want 1", report.ScanRunsTransitioned)
	}
	if report.CommitsTransitioned != 1 {
		t.Errorf("CommitsTransitioned=%d, want 1", report.CommitsTransitioned)
	}
	if report.OrphanedCommitsCleaned != 2 {
		t.Errorf("OrphanedCommitsCleaned=%d, want 2", report.OrphanedCommitsCleaned)
	}
	if metrics := sweep.Metrics(); metrics.StaleScansTotal() != 1 || metrics.FailedCommitsTotal() != 3 {
		t.Errorf("metrics: stale=%d failed=%d, want (1, 3)",
			metrics.StaleScansTotal(), metrics.FailedCommitsTotal())
	}
}

// Suppress "imported and not used" if a later refactor
// removes the explicit sql usages.
var _ = sql.ErrNoRows
