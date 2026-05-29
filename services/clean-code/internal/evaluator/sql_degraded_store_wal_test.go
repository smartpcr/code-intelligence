package evaluator

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/audit/wal"
)

// TestSQLDegradedRunStore_AppendDegradedRun_EmitsWALFramesAroundEachInsert
// is the Stage 9.1 / iter-2 evaluator item #5 integration
// test for the gate's degraded path: every successful
// `evaluation_run` + `evaluation_verdict` degraded INSERT
// MUST be paired with a WAL frame, with the WAL fsync
// ordered BEFORE the SQL commit (architecture Sec 7.10 /
// tech-spec Sec 4.13).
//
// Strategy mirrors the rule_engine integration test:
// sqlmock backs the SQL side, a real wal.Writer rooted at
// `t.TempDir()` captures frames, the test asserts the WAL
// contains exactly two frames in canonical order (run →
// verdict) with the right table identities and PKs.
//
// This closes the gap iter-1 left open: prior to this
// test, no test in `internal/evaluator/*_test.go`
// exercised the WAL alongside the degraded SQL writes.
func TestSQLDegradedRunStore_AppendDegradedRun_EmitsWALFramesAroundEachInsert(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	walWriter := newTestWALWriter(t)
	store, err := NewSQLDegradedRunStore(SQLDegradedRunStoreConfig{DB: db, WalWriter: walWriter})
	if err != nil {
		t.Fatalf("NewSQLDegradedRunStore: %v", err)
	}

	runID := uuid.Must(uuid.NewV4())
	verdictID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	run := DegradedRun{
		EvaluationRunID: runID,
		RepoID:          repoID,
		SHA:             "abc1234567890abcdef1234567890abcdef12345",
		PolicyVersionID: pvID,
		CreatedAt:       1,
	}
	verdict := DegradedVerdict{
		VerdictID:       verdictID,
		EvaluationRunID: runID,
		Verdict:         VerdictWarn,
		Degraded:        true,
		DegradedReason:  "samples_pending",
		CreatedAt:       2,
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_verdict"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.AppendDegradedRun(context.Background(), run, verdict); err != nil {
		t.Fatalf("AppendDegradedRun: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}

	frames, err := wal.ReadAll(walWriter.Dir())
	if err != nil {
		t.Fatalf("wal.ReadAll: %v", err)
	}
	if len(frames) != 2 {
		t.Fatalf("frames: got %d, want 2 (run+verdict)", len(frames))
	}

	want := []struct {
		table wal.Table
		pk    uuid.UUID
	}{
		{wal.TableEvaluationRun, runID},
		{wal.TableEvaluationVerdict, verdictID},
	}
	for i, w := range want {
		f := frames[i]
		if f.Table != w.table {
			t.Errorf("frame[%d].Table: got %q, want %q", i, f.Table, w.table)
		}
		if f.RowPK != w.pk {
			t.Errorf("frame[%d].RowPK: got %s, want %s", i, f.RowPK, w.pk)
		}
		if f.Op != wal.OpInsert {
			t.Errorf("frame[%d].Op: got %q, want %q", i, f.Op, wal.OpInsert)
		}
		if len(f.RowJSON) == 0 {
			t.Errorf("frame[%d].RowJSON is empty", i)
		}
		if len(f.Signature) == 0 {
			t.Errorf("frame[%d].Signature is empty", i)
		}
		if !strings.Contains(string(f.RowJSON), w.pk.String()) {
			t.Errorf("frame[%d].RowJSON does not embed its PK %s: %s", i, w.pk, string(f.RowJSON))
		}
	}

	// Degraded run shape: the verdict frame's row_json
	// MUST surface the closed-set degraded_reason. The
	// reconciler keys on this on replay.
	if !strings.Contains(string(frames[1].RowJSON), `"samples_pending"`) {
		t.Errorf("verdict frame.RowJSON does not embed degraded_reason 'samples_pending': %s", string(frames[1].RowJSON))
	}
	if !strings.Contains(string(frames[1].RowJSON), `"degraded":true`) {
		t.Errorf("verdict frame.RowJSON does not embed degraded=true: %s", string(frames[1].RowJSON))
	}
	// Run frame: caller is always `eval_gate` on the
	// degraded path (the gate is the only writer here).
	if !strings.Contains(string(frames[0].RowJSON), `"eval_gate"`) {
		t.Errorf("run frame.RowJSON does not embed caller 'eval_gate': %s", string(frames[0].RowJSON))
	}
}

// TestSQLDegradedRunStore_AppendDegradedRun_WALFsyncFailureRollsBackSQL
// pins the brief's row+WAL atomicity contract on the
// degraded path: when the WAL flush FAILS the SQL
// transaction MUST roll back (no commit). The mock
// asserts `ExpectRollback` after the run INSERT, proving
// the store never reaches `tx.Commit()` when the signer
// errors.
func TestSQLDegradedRunStore_AppendDegradedRun_WALFsyncFailureRollsBackSQL(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	walWriter, err := wal.NewWriter(wal.WriterConfig{
		Dir:    t.TempDir(),
		Signer: failingSigner{},
	})
	if err != nil {
		t.Fatalf("wal.NewWriter: %v", err)
	}
	store, err := NewSQLDegradedRunStore(SQLDegradedRunStoreConfig{DB: db, WalWriter: walWriter})
	if err != nil {
		t.Fatalf("NewSQLDegradedRunStore: %v", err)
	}

	runID := uuid.Must(uuid.NewV4())
	verdictID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	run := DegradedRun{
		EvaluationRunID: runID,
		RepoID:          repoID,
		SHA:             "abc1234567890abcdef1234567890abcdef12345",
		PolicyVersionID: pvID,
		CreatedAt:       1,
	}
	verdict := DegradedVerdict{
		VerdictID:       verdictID,
		EvaluationRunID: runID,
		Verdict:         VerdictWarn,
		Degraded:        true,
		DegradedReason:  "samples_pending",
		CreatedAt:       2,
	}

	// failingSigner errors on the first StageNew call,
	// which is AFTER the evaluation_run INSERT runs (the
	// INSERT runs at sql_degraded_store.go:190 before the
	// staging at :201). The store surfaces the error and
	// the deferred Rollback fires.
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	err = store.AppendDegradedRun(context.Background(), run, verdict)
	if err == nil {
		t.Fatal("AppendDegradedRun: want error from WAL signer, got nil")
	}
	if !strings.Contains(err.Error(), "synthetic signer error") {
		t.Errorf("AppendDegradedRun: err = %v; want 'synthetic signer error' substring", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestSQLDegradedRunStore_AppendDegradedRun_WALFlushFailureRollsBackSQL
// is the Stage 9.1 / iter-3 evaluator item #3 test for the
// degraded path: a REAL write/fsync failure (vs. the
// signer-failure path covered by
// [TestSQLDegradedRunStore_AppendDegradedRun_WALFsyncFailureRollsBackSQL])
// MUST roll back the SQL transaction.
//
// Strategy mirrors the rule_engine equivalent: pin the
// writer's clock to a deterministic date, pre-create the
// per-day partition path AS A DIRECTORY so the writer's
// `os.OpenFile(..., O_CREATE|O_APPEND|O_WRONLY, ...)` fails
// at flush time, then assert the SQL mock saw both Audit
// INSERTs followed by Rollback (NO Commit).
//
// This pins the contract for the actual disk-failure mode
// the evaluator's iter-2 review flagged: signer failure is
// one case; write/sync failure is the case the brief's
// "WAL fsync before SQL commit" line is fundamentally
// about.
func TestSQLDegradedRunStore_AppendDegradedRun_WALFlushFailureRollsBackSQL(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	frozen := time.Date(2026, 4, 15, 12, 0, 0, 0, time.UTC)
	walDir := t.TempDir()
	walWriter, err := wal.NewWriter(wal.WriterConfig{
		Dir:    walDir,
		Signer: wal.NoopSigner{},
		Clock:  func() time.Time { return frozen },
	})
	if err != nil {
		t.Fatalf("wal.NewWriter: %v", err)
	}
	saboteur := filepath.Join(walDir, "2026-04-15.wal")
	if err := os.MkdirAll(saboteur, 0o755); err != nil {
		t.Fatalf("pre-create saboteur directory at %s: %v", saboteur, err)
	}

	store, err := NewSQLDegradedRunStore(SQLDegradedRunStoreConfig{DB: db, WalWriter: walWriter})
	if err != nil {
		t.Fatalf("NewSQLDegradedRunStore: %v", err)
	}

	runID := uuid.Must(uuid.NewV4())
	verdictID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	run := DegradedRun{
		EvaluationRunID: runID,
		RepoID:          repoID,
		SHA:             "abc1234567890abcdef1234567890abcdef12345",
		PolicyVersionID: pvID,
		CreatedAt:       1,
	}
	verdict := DegradedVerdict{
		VerdictID:       verdictID,
		EvaluationRunID: runID,
		Verdict:         VerdictWarn,
		Degraded:        true,
		DegradedReason:  "samples_pending",
		CreatedAt:       2,
	}

	// Both Audit INSERTs run BEFORE the WAL flush; the
	// degraded store calls `walBatch.Commit(ctx)` after the
	// verdict INSERT and before `tx.Commit`. The mock MUST
	// see Begin + run + verdict INSERTs + Rollback, NO Commit.
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_verdict"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	err = store.AppendDegradedRun(context.Background(), run, verdict)
	if err == nil {
		t.Fatal("AppendDegradedRun: want error from WAL flush, got nil")
	}
	if !strings.Contains(err.Error(), "WAL flush before SQL commit") {
		t.Errorf("AppendDegradedRun: err = %v; want 'WAL flush before SQL commit' substring", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// failingSigner is a test-only [wal.Signer] that always
// returns an error. Used to assert that a sign-time
// failure surfaces as a store error AND triggers SQL
// rollback before commit.
type failingSigner struct{}

func (failingSigner) SignFrame(_ context.Context, _ func(keyID uuid.UUID) ([]byte, error)) (uuid.UUID, []byte, error) {
	return uuid.Nil, nil, errSyntheticSigner
}

// errSyntheticSigner is the sentinel error used by
// [failingSigner].
var errSyntheticSigner = syntheticSignerError("synthetic signer error")

type syntheticSignerError string

func (e syntheticSignerError) Error() string { return string(e) }
