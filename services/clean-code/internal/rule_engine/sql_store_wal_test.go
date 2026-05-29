package rule_engine

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

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// TestSQLStore_AppendEvaluation_EmitsWALFramesAroundEachInsert is
// the Stage 9.1 / iter-2 evaluator item #5 integration test:
// every successful audit INSERT MUST be paired with a WAL
// frame, with the WAL fsync ordered BEFORE the SQL commit
// (architecture Sec 7.10 / tech-spec Sec 4.13).
//
// Strategy:
//
//   - sqlmock backs the SQL side so we can pin the
//     INSERT-then-Commit ordering without a live Postgres.
//   - A real `wal.Writer` is rooted at `t.TempDir()` so we
//     can read the partition file back via `wal.ReadAll`
//     and assert the frames the Store actually staged.
//   - We compose `AppendEvaluation` with ONE run + ONE
//     verdict + TWO findings. The test asserts the WAL
//     contains exactly four frames in canonical order:
//     run → verdict → finding[0] → finding[1]; each
//     frame's `Table`, `RowPK`, and `Op == OpInsert` are
//     pinned.
//
// This closes the gap iter-1 left open: prior to this
// test, no test in `internal/rule_engine/*_test.go`
// exercised the WAL alongside the SQL writes.
func TestSQLStore_AppendEvaluation_EmitsWALFramesAroundEachInsert(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stewardStore, err := steward.NewSQLStoreWithSchema(db, "clean_code")
	if err != nil {
		t.Fatalf("steward.NewSQLStoreWithSchema: %v", err)
	}
	walWriter := newTestWALWriter(t)
	store, err := NewSQLStore(SQLStoreConfig{DB: db, Steward: stewardStore, WalWriter: walWriter})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}

	runID := uuid.Must(uuid.NewV4())
	verdictID := uuid.Must(uuid.NewV4())
	finding1ID := uuid.Must(uuid.NewV4())
	finding2ID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	now := time.Now().UTC()

	run := EvaluationRun{
		EvaluationRunID: runID,
		RepoID:          repoID,
		SHA:             "abc1234567890abcdef1234567890abcdef12345",
		PolicyVersionID: pvID,
		Caller:          CallerBatchRefresh,
		CreatedAt:       now,
	}
	verdict := EvaluationVerdict{
		VerdictID:       verdictID,
		EvaluationRunID: runID,
		Verdict:         VerdictBlock,
		Degraded:        false,
		CreatedAt:       now,
	}
	findings := []Finding{
		{
			FindingID:       finding1ID,
			EvaluationRunID: runID,
			RepoID:          repoID,
			SHA:             run.SHA,
			ScopeID:         scopeID,
			RuleID:          "solid.srp.lcom4_high",
			RuleVersion:     1,
			PolicyVersionID: pvID,
			MetricSampleIDs: []uuid.UUID{uuid.Must(uuid.NewV4())},
			Severity:        steward.SeverityBlock,
			Delta:           DeltaNew,
			ExplanationMD:   "LCOM4 too high (1)",
			CreatedAt:       now,
		},
		{
			FindingID:       finding2ID,
			EvaluationRunID: runID,
			RepoID:          repoID,
			SHA:             run.SHA,
			ScopeID:         scopeID,
			RuleID:          "solid.srp.lcom4_high",
			RuleVersion:     1,
			PolicyVersionID: pvID,
			MetricSampleIDs: []uuid.UUID{uuid.Must(uuid.NewV4())},
			Severity:        steward.SeverityWarn,
			Delta:           DeltaUnchanged,
			ExplanationMD:   "LCOM4 too high (2)",
			CreatedAt:       now,
		},
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_verdict"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."finding"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."finding"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := store.AppendEvaluation(context.Background(), run, verdict, findings); err != nil {
		t.Fatalf("AppendEvaluation: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}

	frames, err := wal.ReadAll(walWriter.Dir())
	if err != nil {
		t.Fatalf("wal.ReadAll: %v", err)
	}
	if len(frames) != 4 {
		t.Fatalf("frames: got %d, want 4 (run+verdict+2 findings)", len(frames))
	}

	want := []struct {
		table wal.Table
		pk    uuid.UUID
	}{
		{wal.TableEvaluationRun, runID},
		{wal.TableEvaluationVerdict, verdictID},
		{wal.TableFinding, finding1ID},
		{wal.TableFinding, finding2ID},
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
}

// TestSQLStore_AppendEvaluation_WALFsyncFailureRollsBackSQL pins
// the brief's row+WAL atomicity contract: when the WAL flush
// FAILS the SQL transaction MUST roll back (no commit). The
// mock asserts `ExpectRollback` after the two staged INSERTs,
// proving the Store never reaches the `tx.Commit()` call when
// `batch.Commit` errors.
//
// Implementation strategy: we replace the writer's signer
// after construction with a deterministic-error signer so the
// batch's pre-flush signing step fails. The store sees the
// error, runs its rollback `defer`, and the SQL mock
// confirms no Commit was attempted.
//
// NOTE -- this is a behavioural test, not a perf test. The
// `errSigner` returns a synthetic error; the writer's
// `Commit(ctx)` propagates it to the store and the store
// errors back to the caller before SQL commit.
func TestSQLStore_AppendEvaluation_WALFsyncFailureRollsBackSQL(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stewardStore, err := steward.NewSQLStoreWithSchema(db, "clean_code")
	if err != nil {
		t.Fatalf("steward.NewSQLStoreWithSchema: %v", err)
	}
	walWriter, err := wal.NewWriter(wal.WriterConfig{
		Dir:    t.TempDir(),
		Signer: failingSigner{},
	})
	if err != nil {
		t.Fatalf("wal.NewWriter: %v", err)
	}
	store, err := NewSQLStore(SQLStoreConfig{DB: db, Steward: stewardStore, WalWriter: walWriter})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}

	runID := uuid.Must(uuid.NewV4())
	verdictID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	now := time.Now().UTC()

	run := EvaluationRun{
		EvaluationRunID: runID,
		RepoID:          repoID,
		SHA:             "abc1234567890abcdef1234567890abcdef12345",
		PolicyVersionID: pvID,
		Caller:          CallerBatchRefresh,
		CreatedAt:       now,
	}
	verdict := EvaluationVerdict{
		VerdictID:       verdictID,
		EvaluationRunID: runID,
		Verdict:         VerdictPass,
		CreatedAt:       now,
	}

	// failingSigner errors on the first SignFrame call, so
	// the first StageNew call inside appendEvaluationInTx
	// fails AFTER the evaluation_run INSERT has executed
	// (the INSERT runs at sql_store.go:705 before the
	// staging at :721). The store surfaces the staging
	// error, the deferred Rollback runs, and the SQL
	// mock confirms no Commit was attempted.
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	err = store.AppendEvaluation(context.Background(), run, verdict, nil)
	if err == nil {
		t.Fatal("AppendEvaluation: want error from WAL signer, got nil")
	}
	if !strings.Contains(err.Error(), "synthetic signer error") {
		t.Errorf("AppendEvaluation: err = %v; want 'synthetic signer error' substring", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestSQLStore_AppendEvaluation_WALFlushFailureRollsBackSQL is
// the Stage 9.1 / iter-3 evaluator item #3 test: it pins the
// brief's atomicity contract for the REAL fsync/write-failure
// path (vs. the signer-failure path covered by
// [TestSQLStore_AppendEvaluation_WALFsyncFailureRollsBackSQL]).
//
// Strategy: use [wal.NoopSigner] so every `StageNew` succeeds
// in memory, then force the writer's append+fsync call to
// fail by pre-creating the per-day partition file's target
// path AS A DIRECTORY. The writer's `os.OpenFile(path,
// O_CREATE|O_APPEND|O_WRONLY, ...)` fails because the path
// is a directory, NOT a writable file -- a true write/sync
// failure mode, not a signer failure.
//
// The SQL mock expects every audit-row INSERT to run first
// (Begin → run + verdict + 2 finding INSERTs) and then
// `Rollback` after the WAL flush errors. NO `Commit` is
// expected.
//
// We pin the writer's clock so the partition file name is
// deterministic: the on-disk pre-created directory must
// share the writer's UTC date.
func TestSQLStore_AppendEvaluation_WALFlushFailureRollsBackSQL(t *testing.T) {
	t.Parallel()

	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	stewardStore, err := steward.NewSQLStoreWithSchema(db, "clean_code")
	if err != nil {
		t.Fatalf("steward.NewSQLStoreWithSchema: %v", err)
	}

	// Pin a deterministic UTC clock so the writer's
	// partition file name resolves to the SAME date as the
	// directory we're about to pre-create.
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

	// Sabotage: pre-create `2026-04-15.wal` AS A DIRECTORY.
	// When the writer tries to `os.OpenFile(...,
	// O_CREATE|O_APPEND|O_WRONLY, ...)` against this path,
	// the syscall fails because the entry is a directory,
	// not a file. The writer's `appendAndSync` surfaces the
	// error to `TxBatch.Commit`, which surfaces to the
	// Store, which rolls back the SQL transaction.
	saboteur := filepath.Join(walDir, "2026-04-15.wal")
	if err := os.MkdirAll(saboteur, 0o755); err != nil {
		t.Fatalf("pre-create saboteur directory at %s: %v", saboteur, err)
	}

	store, err := NewSQLStore(SQLStoreConfig{DB: db, Steward: stewardStore, WalWriter: walWriter})
	if err != nil {
		t.Fatalf("NewSQLStore: %v", err)
	}

	runID := uuid.Must(uuid.NewV4())
	verdictID := uuid.Must(uuid.NewV4())
	finding1ID := uuid.Must(uuid.NewV4())
	finding2ID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	now := time.Now().UTC()

	run := EvaluationRun{
		EvaluationRunID: runID,
		RepoID:          repoID,
		SHA:             "abc1234567890abcdef1234567890abcdef12345",
		PolicyVersionID: pvID,
		Caller:          CallerBatchRefresh,
		CreatedAt:       now,
	}
	verdict := EvaluationVerdict{
		VerdictID:       verdictID,
		EvaluationRunID: runID,
		Verdict:         VerdictPass,
		CreatedAt:       now,
	}
	findings := []Finding{
		{
			FindingID:       finding1ID,
			EvaluationRunID: runID,
			RepoID:          repoID,
			SHA:             run.SHA,
			ScopeID:         scopeID,
			RuleID:          "solid.srp.lcom4_high",
			RuleVersion:     1,
			PolicyVersionID: pvID,
			MetricSampleIDs: []uuid.UUID{uuid.Must(uuid.NewV4())},
			Severity:        steward.SeverityBlock,
			Delta:           DeltaNew,
			ExplanationMD:   "LCOM4 too high (1)",
			CreatedAt:       now,
		},
		{
			FindingID:       finding2ID,
			EvaluationRunID: runID,
			RepoID:          repoID,
			SHA:             run.SHA,
			ScopeID:         scopeID,
			RuleID:          "solid.srp.lcom4_high",
			RuleVersion:     1,
			PolicyVersionID: pvID,
			MetricSampleIDs: []uuid.UUID{uuid.Must(uuid.NewV4())},
			Severity:        steward.SeverityWarn,
			Delta:           DeltaUnchanged,
			ExplanationMD:   "LCOM4 too high (2)",
			CreatedAt:       now,
		},
	}

	// Every audit-row INSERT runs BEFORE the WAL
	// flush (the Store stages frames in-memory during
	// the tx, then calls `walBatch.Commit(ctx)` AFTER
	// the last INSERT but BEFORE `tx.Commit`). The mock
	// MUST therefore see Begin + run + verdict + 2
	// finding INSERTs + Rollback, and NO Commit.
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_verdict"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."finding"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."finding"`)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	err = store.AppendEvaluation(context.Background(), run, verdict, findings)
	if err == nil {
		t.Fatal("AppendEvaluation: want error from WAL flush, got nil")
	}
	if !strings.Contains(err.Error(), "WAL flush before SQL commit") {
		t.Errorf("AppendEvaluation: err = %v; want 'WAL flush before SQL commit' substring (so operators recognise the contract)", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// failingSigner is a test-only [wal.Signer] that always
// returns an error. Used to assert that a sign-time failure
// surfaces as a store error AND triggers SQL rollback
// before commit.
type failingSigner struct{}

func (failingSigner) SignFrame(_ context.Context, _ func(keyID uuid.UUID) ([]byte, error)) (uuid.UUID, []byte, error) {
	return uuid.Nil, nil, errSyntheticSigner
}

// errSyntheticSigner is the sentinel error used by
// [failingSigner].
var errSyntheticSigner = syntheticSignerError("synthetic signer error")

type syntheticSignerError string

func (e syntheticSignerError) Error() string { return string(e) }
