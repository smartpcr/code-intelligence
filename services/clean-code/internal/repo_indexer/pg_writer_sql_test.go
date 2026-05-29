package repo_indexer_test

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/repo_indexer"
)

// pgWriterSQLTestRepoID is the canonical repo UUID reused
// across every SQL-shape test in this file. Pinned so each
// test makes the same `pg_advisory_xact_lock(...)`
// argument shape and prepared-statement parameter trace,
// keeping the mocked-expectation lists short.
var pgWriterSQLTestRepoID = uuid.Must(uuid.FromString(
	"11111111-2222-3333-4444-555555555555",
))

// pgWriterSQLTestCommittedAt is the canonical fixed commit
// timestamp used by every SQL-shape test. UTC so the
// Postgres `timestamptz` column receives a stable
// representation regardless of the test runner's TZ.
var pgWriterSQLTestCommittedAt = time.Date(
	2026, 5, 24, 12, 0, 0, 0, time.UTC,
)

// pgWriterSQLTestSchema is the test-only schema name
// threaded through [repo_indexer.NewPGCatalogWriterWithSchema]
// so the SQL-shape assertions can include the schema literal
// in their match patterns -- the production default
// `clean_code` would also work, but pinning a test-specific
// value keeps the assertions visibly diff-able from
// production behaviour.
const pgWriterSQLTestSchema = "clean_code_indexer_test"

// newSQLMockWriter wires a [PGCatalogWriter] against a
// regex-matching mock DB. Returns the writer, the mock
// expectation surface, and a cleanup func that asserts all
// declared expectations were satisfied (a leaking
// expectation = the writer skipped a statement and is
// suppressed silently).
func newSQLMockWriter(t *testing.T) (*repo_indexer.PGCatalogWriter, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	w, err := repo_indexer.NewPGCatalogWriterWithSchema(db, pgWriterSQLTestSchema)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPGCatalogWriterWithSchema: %v", err)
	}
	cleanup := func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock: unmet expectations: %v", err)
		}
		_ = db.Close()
	}
	return w, mock, cleanup
}

// TestPGCatalogWriter_FirstSHA_InsertsCommitAndRegisteredEvent
// pins the happy path for a brand-new repo: the writer
// (a) acquires the per-repo advisory lock, (b) INSERTs the
// commit row WITHOUT naming scan_status, (c) probes for a
// pre-existing registered event and finds NONE, (d) INSERTs
// the canonical past-tense `registered` event, (e) COMMITs.
// Result: both `CommitInserted` and `EventInserted` true.
func TestPGCatalogWriter_FirstSHA_InsertsCommitAndRegisteredEvent(t *testing.T) {
	t.Parallel()

	w, mock, cleanup := newSQLMockWriter(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(\$1, \$2\)`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Commit INSERT: RETURNING 1 -> commit_inserted=true.
	mock.ExpectQuery(`INSERT INTO "clean_code_indexer_test"\."commit"[\s\S]*RETURNING 1`).
		WithArgs(pgWriterSQLTestRepoID, "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4", "", pgWriterSQLTestCommittedAt).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	// Event probe: no rows -> ErrNoRows -> event INSERT path.
	mock.ExpectQuery(`SELECT 1 FROM "clean_code_indexer_test"\."repo_event"`).
		WithArgs(pgWriterSQLTestRepoID, "registered").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	mock.ExpectExec(`INSERT INTO "clean_code_indexer_test"\."repo_event" \(repo_id, kind\)`).
		WithArgs(pgWriterSQLTestRepoID, "registered").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	res, err := w.EnsureCommitAndRegisteredEvent(context.Background(), repo_indexer.CommitEnsureRequest{
		RepoID:      pgWriterSQLTestRepoID,
		SHA:         "a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4a1b2c3d4",
		ParentSHA:   "",
		CommittedAt: pgWriterSQLTestCommittedAt,
	})
	if err != nil {
		t.Fatalf("EnsureCommitAndRegisteredEvent: %v", err)
	}
	if !res.CommitInserted {
		t.Errorf("CommitInserted = false; want true (first-SHA path)")
	}
	if !res.EventInserted {
		t.Errorf("EventInserted = false; want true (no prior registered event)")
	}
}

// TestPGCatalogWriter_DuplicateCommit_DoesNotReinsertEvent
// pins the idempotent redelivery path: the commit INSERT
// hits ON CONFLICT DO NOTHING (returning zero rows scanned
// = `sql.ErrNoRows`), the writer treats this as
// `commit_inserted=false`, and STILL probes for the
// registered event. If the event already exists, NO second
// INSERT fires. This is the canonical webhook-retry shape:
// a duplicate delivery for a repo that's been registered
// before must be a no-op at the SQL layer.
func TestPGCatalogWriter_DuplicateCommit_DoesNotReinsertEvent(t *testing.T) {
	t.Parallel()

	w, mock, cleanup := newSQLMockWriter(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(\$1, \$2\)`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Commit INSERT: 0 rows returned (ON CONFLICT path).
	mock.ExpectQuery(`INSERT INTO "clean_code_indexer_test"\."commit"`).
		WithArgs(pgWriterSQLTestRepoID, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "cafef00dcafef00dcafef00dcafef00dcafef00d", pgWriterSQLTestCommittedAt).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	// Event probe: registered event already present.
	mock.ExpectQuery(`SELECT 1 FROM "clean_code_indexer_test"\."repo_event"`).
		WithArgs(pgWriterSQLTestRepoID, "registered").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	// NO second event INSERT expected -- if the writer
	// fires one anyway, ExpectationsWereMet() trips on a
	// surplus exec.
	mock.ExpectCommit()

	res, err := w.EnsureCommitAndRegisteredEvent(context.Background(), repo_indexer.CommitEnsureRequest{
		RepoID:      pgWriterSQLTestRepoID,
		SHA:         "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		ParentSHA:   "cafef00dcafef00dcafef00dcafef00dcafef00d",
		CommittedAt: pgWriterSQLTestCommittedAt,
	})
	if err != nil {
		t.Fatalf("EnsureCommitAndRegisteredEvent: %v", err)
	}
	if res.CommitInserted {
		t.Errorf("CommitInserted = true; want false (ON CONFLICT DO NOTHING path)")
	}
	if res.EventInserted {
		t.Errorf("EventInserted = true; want false (registered already present)")
	}
}

// TestPGCatalogWriter_FreshCommit_PreexistingEvent_OnlyInsertsCommit
// pins the third lifecycle shape: a new SHA on a repo that
// has ALREADY been registered (e.g. a follow-up commit after
// the first ever delivery). RETURNING 1 -> commit_inserted,
// but the event probe sees an existing registered event ->
// no second event INSERT.
func TestPGCatalogWriter_FreshCommit_PreexistingEvent_OnlyInsertsCommit(t *testing.T) {
	t.Parallel()

	w, mock, cleanup := newSQLMockWriter(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock\(\$1, \$2\)`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO "clean_code_indexer_test"\."commit"`).
		WithArgs(pgWriterSQLTestRepoID, "0123456789abcdef0123456789abcdef01234567", "fedcba9876543210fedcba9876543210fedcba98", pgWriterSQLTestCommittedAt).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectQuery(`SELECT 1 FROM "clean_code_indexer_test"\."repo_event"`).
		WithArgs(pgWriterSQLTestRepoID, "registered").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectCommit()

	res, err := w.EnsureCommitAndRegisteredEvent(context.Background(), repo_indexer.CommitEnsureRequest{
		RepoID:      pgWriterSQLTestRepoID,
		SHA:         "0123456789abcdef0123456789abcdef01234567",
		ParentSHA:   "fedcba9876543210fedcba9876543210fedcba98",
		CommittedAt: pgWriterSQLTestCommittedAt,
	})
	if err != nil {
		t.Fatalf("EnsureCommitAndRegisteredEvent: %v", err)
	}
	if !res.CommitInserted {
		t.Errorf("CommitInserted = false; want true (RETURNING 1)")
	}
	if res.EventInserted {
		t.Errorf("EventInserted = true; want false (registered already present)")
	}
}

// TestPGCatalogWriter_CommitINSERTOmitsScanStatus is the
// architectural pin: the prepared commit INSERT must NOT
// name `scan_status`. iter-1 evaluator item 2 -- "Repo
// Indexer never writes scan_status; only the Metric
// Ingestor does". A regression that added `scan_status` to
// the column list (well-meaning -- "make initial state
// explicit") would let the indexer overwrite a downstream
// transition the moment redelivery raced a Metric Ingestor
// tx; the DB DEFAULT 'pending' is the sole source of the
// initial value.
//
// We assert at SQL-shape level: the INSERT SQL string must
// fail a `scan_status` regex match. Because go-sqlmock
// builds expectations against the actual SQL strings the
// writer issues, we capture the recorded SQL via a custom
// matcher.
func TestPGCatalogWriter_CommitINSERTOmitsScanStatus(t *testing.T) {
	t.Parallel()

	w, mock, cleanup := newSQLMockWriter(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Match the INSERT prepared statement; we make the regex
	// also explicitly assert the column list is EXACTLY the
	// canonical four columns (no scan_status appended).
	insertPattern := `INSERT INTO "clean_code_indexer_test"\."commit" \(repo_id, sha, parent_sha, committed_at\)`
	mock.ExpectQuery(insertPattern).
		WithArgs(pgWriterSQLTestRepoID, "abababababababababababababababababababab", "", pgWriterSQLTestCommittedAt).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectQuery(`SELECT 1 FROM "clean_code_indexer_test"\."repo_event"`).
		WithArgs(pgWriterSQLTestRepoID, "registered").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectCommit()

	if _, err := w.EnsureCommitAndRegisteredEvent(context.Background(), repo_indexer.CommitEnsureRequest{
		RepoID:      pgWriterSQLTestRepoID,
		SHA:         "abababababababababababababababababababab",
		ParentSHA:   "",
		CommittedAt: pgWriterSQLTestCommittedAt,
	}); err != nil {
		t.Fatalf("EnsureCommitAndRegisteredEvent: %v", err)
	}

	// Defence-in-depth: belt-and-braces grep across the
	// REGEX patterns we ever fed to the mock. If a future
	// edit appends `, scan_status` to the column list, the
	// `(repo_id, sha, parent_sha, committed_at)` pattern
	// above stops matching and the test trips at
	// ExpectationsWereMet via "missing matching query"; THIS
	// extra assertion catches the inverse mistake of someone
	// LOOSENING the pattern to also match scan_status.
	if regexp.MustCompile(`scan_status`).MatchString(insertPattern) {
		t.Fatalf("test pattern itself names scan_status -- the architectural pin is broken: %q", insertPattern)
	}
}

// TestPGCatalogWriter_UsesNULLIFForParentSHA pins the
// empty-string-to-NULL conversion: the commit INSERT must
// pass `NULLIF($3, '')` so an application-layer empty
// parent_sha lands as DB NULL on the first-commit-of-repo
// (migration 0001 line 221 -- `parent_sha text` nullable).
// A naked `$3` would store the literal empty string and
// break downstream "parent_sha IS NULL" predicates.
func TestPGCatalogWriter_UsesNULLIFForParentSHA(t *testing.T) {
	t.Parallel()

	w, mock, cleanup := newSQLMockWriter(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	// Match the prepared statement that contains the
	// `NULLIF($3, '')` literal. If a future refactor drops
	// the NULLIF, the regex no longer matches and the test
	// fails before any rows are scanned.
	mock.ExpectQuery(`NULLIF\(\$3, ''\)`).
		WithArgs(pgWriterSQLTestRepoID, "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd", "", pgWriterSQLTestCommittedAt).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectQuery(`SELECT 1 FROM "clean_code_indexer_test"\."repo_event"`).
		WithArgs(pgWriterSQLTestRepoID, "registered").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectCommit()

	if _, err := w.EnsureCommitAndRegisteredEvent(context.Background(), repo_indexer.CommitEnsureRequest{
		RepoID:      pgWriterSQLTestRepoID,
		SHA:         "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd",
		ParentSHA:   "",
		CommittedAt: pgWriterSQLTestCommittedAt,
	}); err != nil {
		t.Fatalf("EnsureCommitAndRegisteredEvent: %v", err)
	}
}

// TestPGCatalogWriter_UsesONCONFLICTDoNothingReturning pins
// the redelivery-idempotent INSERT shape:
// `ON CONFLICT (repo_id, sha) DO NOTHING RETURNING 1`.
// A regression that swapped DO NOTHING for DO UPDATE SET
// scan_status=... would simultaneously violate the
// "indexer never writes scan_status" invariant AND turn the
// redelivery semantics from no-op to overwrite.
func TestPGCatalogWriter_UsesONCONFLICTDoNothingReturning(t *testing.T) {
	t.Parallel()

	w, mock, cleanup := newSQLMockWriter(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`ON CONFLICT \(repo_id, sha\) DO NOTHING\s+RETURNING 1`).
		WithArgs(pgWriterSQLTestRepoID, "efefefefefefefefefefefefefefefefefefefef", "", pgWriterSQLTestCommittedAt).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectQuery(`SELECT 1 FROM "clean_code_indexer_test"\."repo_event"`).
		WithArgs(pgWriterSQLTestRepoID, "registered").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectCommit()

	if _, err := w.EnsureCommitAndRegisteredEvent(context.Background(), repo_indexer.CommitEnsureRequest{
		RepoID:      pgWriterSQLTestRepoID,
		SHA:         "efefefefefefefefefefefefefefefefefefefef",
		ParentSHA:   "",
		CommittedAt: pgWriterSQLTestCommittedAt,
	}); err != nil {
		t.Fatalf("EnsureCommitAndRegisteredEvent: %v", err)
	}
}

// TestPGCatalogWriter_EventINSERTNamesCanonicalRegisteredLiteral
// pins the canonical past-tense `registered` literal at the
// event INSERT site (architecture Sec 5.1.4 lines 877-884).
// Iter-1 evaluator item 2 elevated the past-tense form to
// the canonical one -- the imperative `register` is a
// forbidden alias. Asserting via the bind arg (vs the SQL
// string) catches a refactor that switches to `'register'`
// even if the column list and ON CONFLICT shape stay the
// same.
func TestPGCatalogWriter_EventINSERTNamesCanonicalRegisteredLiteral(t *testing.T) {
	t.Parallel()

	w, mock, cleanup := newSQLMockWriter(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO "clean_code_indexer_test"\."commit"`).
		WithArgs(pgWriterSQLTestRepoID, "1212121212121212121212121212121212121212", "", pgWriterSQLTestCommittedAt).
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}).AddRow(1))
	mock.ExpectQuery(`SELECT 1 FROM "clean_code_indexer_test"\."repo_event"`).
		WithArgs(pgWriterSQLTestRepoID, "registered").
		WillReturnRows(sqlmock.NewRows([]string{"?column?"}))
	// CRITICAL: the second bind arg here MUST be the literal
	// "registered" (past tense). If the writer ever passes
	// "register" the WithArgs match fails and the test trips.
	mock.ExpectExec(`INSERT INTO "clean_code_indexer_test"\."repo_event"`).
		WithArgs(pgWriterSQLTestRepoID, "registered").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if _, err := w.EnsureCommitAndRegisteredEvent(context.Background(), repo_indexer.CommitEnsureRequest{
		RepoID:      pgWriterSQLTestRepoID,
		SHA:         "1212121212121212121212121212121212121212",
		ParentSHA:   "",
		CommittedAt: pgWriterSQLTestCommittedAt,
	}); err != nil {
		t.Fatalf("EnsureCommitAndRegisteredEvent: %v", err)
	}
}

// TestPGCatalogWriter_CommitInsertError_RollsBack pins the
// failure-rollback shape: a non-NoRows error from the
// commit INSERT must (a) propagate as a wrapped error to
// the caller, (b) NOT advance to the event probe, (c) NOT
// commit the transaction. The mock's deferred
// `ExpectCommit()` is INTENTIONALLY ABSENT so the writer's
// implicit `tx.Rollback()` path is exercised; if a future
// refactor forgets to short-circuit on the commit-INSERT
// error and still attempts the event probe, the mock will
// trip with "missing expectation".
func TestPGCatalogWriter_CommitInsertError_RollsBack(t *testing.T) {
	t.Parallel()

	w, mock, cleanup := newSQLMockWriter(t)
	defer cleanup()

	boom := errors.New("simulated commit-INSERT failure")
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(`INSERT INTO "clean_code_indexer_test"\."commit"`).
		WithArgs(pgWriterSQLTestRepoID, "9999999999999999999999999999999999999999", "", pgWriterSQLTestCommittedAt).
		WillReturnError(boom)
	mock.ExpectRollback()

	_, err := w.EnsureCommitAndRegisteredEvent(context.Background(), repo_indexer.CommitEnsureRequest{
		RepoID:      pgWriterSQLTestRepoID,
		SHA:         "9999999999999999999999999999999999999999",
		ParentSHA:   "",
		CommittedAt: pgWriterSQLTestCommittedAt,
	})
	if err == nil {
		t.Fatal("EnsureCommitAndRegisteredEvent: err = nil; want wrapped INSERT failure")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %q does not wrap simulated INSERT failure", err)
	}
}

// TestPGCatalogWriter_AdvisoryLockError_RollsBackEarly
// pins the lock-acquisition failure shape: a non-nil error
// from `pg_advisory_xact_lock` must (a) surface to the
// caller as a wrapped error, (b) NOT proceed to the commit
// INSERT, (c) NOT commit the tx. This is the canonical
// "another instance is mid-transaction holding the lock and
// our connection died waiting" diagnostic shape.
func TestPGCatalogWriter_AdvisoryLockError_RollsBackEarly(t *testing.T) {
	t.Parallel()

	w, mock, cleanup := newSQLMockWriter(t)
	defer cleanup()

	boom := errors.New("simulated advisory-lock failure")
	mock.ExpectBegin()
	mock.ExpectExec(`SELECT pg_advisory_xact_lock`).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(boom)
	mock.ExpectRollback()

	_, err := w.EnsureCommitAndRegisteredEvent(context.Background(), repo_indexer.CommitEnsureRequest{
		RepoID:      pgWriterSQLTestRepoID,
		SHA:         "3333333333333333333333333333333333333333",
		ParentSHA:   "",
		CommittedAt: pgWriterSQLTestCommittedAt,
	})
	if err == nil {
		t.Fatal("EnsureCommitAndRegisteredEvent: err = nil; want wrapped lock failure")
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %q does not wrap simulated lock failure", err)
	}
}
