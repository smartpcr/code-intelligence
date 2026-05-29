package metric_ingestor_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
	"forge/services/clean-code/internal/metrics/recipes"
)

const pgMetricSampleTestSchema = "clean_code_ingestor_test"

// expectScanRunGuard registers the per-batch FOR SHARE
// guard SELECT (Stage 3.2 iter 17) inside the open
// transaction returning the requested observed `status`.
// Tests that drive a successful WriteBatch through the
// guard must call this between `ExpectBegin` and
// `ExpectPrepare(INSERT)`.
func expectScanRunGuard(t *testing.T, mock sqlmock.Sqlmock, producerRunID uuid.UUID, status string) {
	t.Helper()
	mock.ExpectQuery(`SELECT\s+status\s+FROM\s+"` + pgMetricSampleTestSchema + `"\."scan_run"\s+WHERE\s+scan_run_id\s+=\s+\$1\s+FOR\s+SHARE`).
		WithArgs(producerRunID).
		WillReturnRows(sqlmock.NewRows([]string{"status"}).AddRow(status))
}

// expectScanRunGuardError lets tests inject a guard-SELECT
// error (e.g. `sql.ErrNoRows`) without firing a rows-row.
func expectScanRunGuardError(t *testing.T, mock sqlmock.Sqlmock, producerRunID uuid.UUID, err error) {
	t.Helper()
	mock.ExpectQuery(`SELECT\s+status\s+FROM\s+"` + pgMetricSampleTestSchema + `"\."scan_run"\s+WHERE\s+scan_run_id\s+=\s+\$1\s+FOR\s+SHARE`).
		WithArgs(producerRunID).
		WillReturnError(err)
}

// expectMetricSampleInsertPrepare registers the INSERT INTO
// `metric_sample` prepared statement and returns its
// [sqlmock.ExpectedPrepare] so per-record EXECs can be
// chained. Pinned in one helper so the Stage 3.3 active-row
// UPSERT flow (`expectMetricSampleActiveUpsertPrepare`) is
// the symmetric next prepare in the same transaction.
func expectMetricSampleInsertPrepare(t *testing.T, mock sqlmock.Sqlmock) *sqlmock.ExpectedPrepare {
	t.Helper()
	return mock.ExpectPrepare(`INSERT\s+INTO\s+"` + pgMetricSampleTestSchema + `"\."metric_sample"`)
}

// expectMetricSampleActiveUpsertPrepare registers the
// Stage 3.3 INSERT ... ON CONFLICT DO UPDATE prepared
// statement against `metric_sample_active`. It is the
// canonical active-row UPSERT (tech-spec Sec 7.1.b lines
// 1070-1119 / architecture Sec 5.2.1 G2): no `swap_active`
// verb, trigger, or stored function exists in the canonical
// model.
//
// The regex pins:
//   - the qualified relation name (`<schema>.metric_sample_active`);
//   - the exact six-column INSERT list
//     `(repo_id, sha, scope_id, metric_kind, metric_version, sample_id)`;
//   - the exact six-placeholder VALUES list `($1, ..., $6)`;
//   - the exact ON CONFLICT quintuple target
//     `(repo_id, sha, scope_id, metric_kind, metric_version)`;
//   - the exact `DO UPDATE SET sample_id = EXCLUDED.sample_id`
//     clause.
//
// Any deviation -- an extra column, a wrong placeholder
// count, a different conflict target, a different SET clause
// (e.g. setting `created_at` or `degraded_reason`) -- fails
// the prepare-expectation match and surfaces as an unmet
// expectation in cleanup.
func expectMetricSampleActiveUpsertPrepare(t *testing.T, mock sqlmock.Sqlmock) *sqlmock.ExpectedPrepare {
	t.Helper()
	return mock.ExpectPrepare(
		`INSERT\s+INTO\s+"` + pgMetricSampleTestSchema + `"\."metric_sample_active"` +
			`\s+\(repo_id,\s+sha,\s+scope_id,\s+metric_kind,\s+metric_version,\s+sample_id\)` +
			`\s+VALUES\s+\(\$1,\s+\$2,\s+\$3,\s+\$4,\s+\$5,\s+\$6\)` +
			`\s+ON\s+CONFLICT\s+\(repo_id,\s+sha,\s+scope_id,\s+metric_kind,\s+metric_version\)` +
			`\s+DO\s+UPDATE\s+SET\s+sample_id\s+=\s+EXCLUDED\.sample_id\s*\z`,
	)
}

func newSQLMockMetricSampleWriter(t *testing.T) (*metric_ingestor.PGMetricSampleWriter, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(
		sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp),
	)
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	w, err := metric_ingestor.NewPGMetricSampleWriterWithSchema(db, pgMetricSampleTestSchema)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPGMetricSampleWriterWithSchema: %v", err)
	}
	cleanup := func() {
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("sqlmock: unmet expectations: %v", err)
		}
		_ = db.Close()
	}
	return w, mock, cleanup
}

func TestNewPGMetricSampleWriter_RejectsNilDB(t *testing.T) {
	t.Parallel()
	if _, err := metric_ingestor.NewPGMetricSampleWriter(nil); !errors.Is(err, metric_ingestor.ErrPGMetricSampleWriterNilDB) {
		t.Errorf("NewPGMetricSampleWriter(nil): err=%v, want errors.Is ErrPGMetricSampleWriterNilDB", err)
	}
}

func TestNewPGMetricSampleWriterWithSchema_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	db, _, _ := sqlmock.New()
	defer db.Close()
	if _, err := metric_ingestor.NewPGMetricSampleWriterWithSchema(db, ""); !errors.Is(err, metric_ingestor.ErrPGMetricSampleWriterEmptySchema) {
		t.Errorf("NewPGMetricSampleWriterWithSchema('') err=%v, want errors.Is ErrPGMetricSampleWriterEmptySchema", err)
	}
}

func TestPGMetricSampleWriter_WriteBatch_EmptyIsNoop(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()
	// No DB expectations: an empty batch must not open a transaction.
	if err := w.WriteBatch(context.Background(), nil); err != nil {
		t.Errorf("WriteBatch(nil) = %v, want nil", err)
	}
	if err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{}); err != nil {
		t.Errorf("WriteBatch(empty) = %v, want nil", err)
	}
	_ = mock
}

// TestPGMetricSampleWriter_WriteBatch_HappyPath pins the
// canonical SQL trace for a batch INSERT: BEGIN, PREPARE,
// EXEC per row, COMMIT.
func TestPGMetricSampleWriter_WriteBatch_HappyPath(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec := metric_ingestor.MetricSampleRecord{
		SampleID:      uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111111")),
		RepoID:        uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
		SHA:           "abc1234567890123456789012345678901234567",
		ScopeID:       uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333")),
		MetricKind:    "cyclo",
		MetricVersion: 1,
		Pack:          recipes.PackBase,
		Source:        recipes.SourceComputed,
		Value:         5,
		ProducerRunID: uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444")),
		Attrs:         map[string]string{"file": "pkg/foo.go"},
	}

	mock.ExpectBegin()
	expectScanRunGuard(t, mock, rec.ProducerRunID, "running")
	mock.ExpectPrepare(`INSERT\s+INTO\s+"` + pgMetricSampleTestSchema + `"\."metric_sample"`).
		ExpectExec().
		WithArgs(
			rec.SampleID, rec.RepoID, rec.SHA, rec.ScopeID,
			rec.MetricKind, rec.MetricVersion,
			rec.Value, "base", "computed",
			rec.ProducerRunID,
			`{"file":"pkg/foo.go"}`,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	expectMetricSampleActiveUpsertPrepare(t, mock).
		ExpectExec().
		WithArgs(
			rec.RepoID, rec.SHA, rec.ScopeID,
			rec.MetricKind, rec.MetricVersion,
			rec.SampleID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec}); err != nil {
		t.Fatalf("WriteBatch: err=%v, want nil", err)
	}
}

// TestPGMetricSampleWriter_WriteBatch_MultiRowIsOneTx
// proves N records share ONE transaction (atomic batch).
func TestPGMetricSampleWriter_WriteBatch_MultiRowIsOneTx(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	mk := func(seed byte) metric_ingestor.MetricSampleRecord {
		return metric_ingestor.MetricSampleRecord{
			SampleID:      uuid.Must(uuid.FromString("11111111-1111-1111-1111-1111111111" + twoHex(uint32(seed)))),
			RepoID:        uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
			SHA:           "abc1234567890123456789012345678901234567",
			ScopeID:       uuid.Must(uuid.FromString("33333333-3333-3333-3333-3333333333" + twoHex(uint32(seed)))),
			MetricKind:    "cyclo",
			MetricVersion: 1,
			Pack:          recipes.PackBase,
			Source:        recipes.SourceComputed,
			Value:         float64(seed),
			ProducerRunID: uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444")),
		}
	}
	recs := []metric_ingestor.MetricSampleRecord{mk(1), mk(2), mk(3)}

	mock.ExpectBegin()
	expectScanRunGuard(t, mock, recs[0].ProducerRunID, "running")
	prep := expectMetricSampleInsertPrepare(t, mock)
	for range recs {
		prep.ExpectExec().WillReturnResult(sqlmock.NewResult(0, 1))
	}
	prepActive := expectMetricSampleActiveUpsertPrepare(t, mock)
	for range recs {
		prepActive.ExpectExec().WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()

	if err := w.WriteBatch(context.Background(), recs); err != nil {
		t.Fatalf("WriteBatch: err=%v, want nil", err)
	}
}

// TestPGMetricSampleWriter_WriteBatch_NoOpOnExecError pins
// the atomic-batch contract: any per-row failure rolls back
// the whole batch (no partial-write surface).
func TestPGMetricSampleWriter_WriteBatch_RollsBackOnExecError(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec1 := metric_ingestor.MetricSampleRecord{
		SampleID:      uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111111")),
		RepoID:        uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
		SHA:           "abc1234567890123456789012345678901234567",
		ScopeID:       uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333")),
		MetricKind:    "cyclo", MetricVersion: 1,
		Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 1,
		ProducerRunID: uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444")),
	}
	rec2 := rec1
	rec2.SampleID = uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111112"))

	wantErr := errors.New("simulated INSERT failure")
	mock.ExpectBegin()
	expectScanRunGuard(t, mock, rec1.ProducerRunID, "running")
	prep := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO`))
	prep.ExpectExec().WillReturnResult(sqlmock.NewResult(0, 1))
	prep.ExpectExec().WillReturnError(wantErr)
	mock.ExpectRollback()

	err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec1, rec2})
	if err == nil || !errors.Is(err, wantErr) {
		t.Errorf("WriteBatch: err=%v, want wrapping of %v", err, wantErr)
	}
}

// TestPGMetricSampleWriter_WriteBatch_RejectsNaN pins the
// "no NaN values" guard -- a NaN would satisfy NOT NULL but
// would poison downstream aggregations.
func TestPGMetricSampleWriter_WriteBatch_RejectsNaN(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec := metric_ingestor.MetricSampleRecord{
		SampleID:      uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111111")),
		RepoID:        uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
		SHA:           "abc1234567890123456789012345678901234567",
		ScopeID:       uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333")),
		MetricKind:    "cyclo", MetricVersion: 1,
		Pack: recipes.PackBase, Source: recipes.SourceComputed,
		Value:         math.NaN(),
		ProducerRunID: uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444")),
	}

	mock.ExpectBegin()
	expectScanRunGuard(t, mock, rec.ProducerRunID, "running")
	mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO`))
	mock.ExpectRollback()

	err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec})
	if err == nil {
		t.Fatal("WriteBatch with NaN: err=nil, want validation error")
	}
}

// TestPGMetricSampleWriter_WriteBatch_RejectsZeroUUIDs proves
// the writer fails fast on zero UUID columns (the schema's FK
// would reject them anyway but the writer surfaces the bug
// at the application boundary).
func TestPGMetricSampleWriter_WriteBatch_RejectsZeroUUIDs(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec := metric_ingestor.MetricSampleRecord{
		SampleID:      uuid.Nil, // zero
		RepoID:        uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
		SHA:           "abc1234567890123456789012345678901234567",
		ScopeID:       uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333")),
		MetricKind:    "cyclo", MetricVersion: 1,
		Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 1,
		ProducerRunID: uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444")),
	}

	mock.ExpectBegin()
	expectScanRunGuard(t, mock, rec.ProducerRunID, "running")
	mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO`))
	mock.ExpectRollback()

	err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec})
	if err == nil {
		t.Fatal("WriteBatch with zero SampleID: err=nil, want validation error")
	}
}

// TestPGMetricSampleWriter_WriteBatch_RejectsPostFinalize
// proves the Stage 3.2 iter 17 post-finalize fence: a scanner
// goroutine that outlives the state machine's hard-timeout
// path (`state.go:1210-1224`) CANNOT commit metric_sample
// rows after `FinalizeScanRun` has transitioned the producer
// run to `'failed'`.
func TestPGMetricSampleWriter_WriteBatch_RejectsPostFinalize(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec := metric_ingestor.MetricSampleRecord{
		SampleID:      uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111111")),
		RepoID:        uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
		SHA:           "abc1234567890123456789012345678901234567",
		ScopeID:       uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333")),
		MetricKind:    "cyclo", MetricVersion: 1,
		Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 1,
		ProducerRunID: uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444")),
	}

	mock.ExpectBegin()
	// Guard SELECT returns the producer scan_run sitting at
	// `'failed'` (post-finalize). The writer MUST refuse the
	// batch with [ErrPGMetricSampleWriterPostFinalizeWrite]
	// and roll back BEFORE preparing the INSERT.
	expectScanRunGuard(t, mock, rec.ProducerRunID, "failed")
	mock.ExpectRollback()

	err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec})
	if err == nil || !errors.Is(err, metric_ingestor.ErrPGMetricSampleWriterPostFinalizeWrite) {
		t.Errorf("WriteBatch post-finalize: err=%v, want errors.Is ErrPGMetricSampleWriterPostFinalizeWrite", err)
	}
}

// TestPGMetricSampleWriter_WriteBatch_RejectsUnknownProducer
// proves the guard distinguishes "producer scan_run row does
// not exist" from "producer status is non-running" -- the
// former is a data-integrity bug, the latter is the expected
// post-finalize fence.
func TestPGMetricSampleWriter_WriteBatch_RejectsUnknownProducer(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec := metric_ingestor.MetricSampleRecord{
		SampleID:      uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111111")),
		RepoID:        uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
		SHA:           "abc1234567890123456789012345678901234567",
		ScopeID:       uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333")),
		MetricKind:    "cyclo", MetricVersion: 1,
		Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 1,
		ProducerRunID: uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444")),
	}

	mock.ExpectBegin()
	expectScanRunGuardError(t, mock, rec.ProducerRunID, sql.ErrNoRows)
	mock.ExpectRollback()

	err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec})
	if err == nil || !errors.Is(err, metric_ingestor.ErrPGMetricSampleWriterUnknownProducerRunID) {
		t.Errorf("WriteBatch unknown producer: err=%v, want errors.Is ErrPGMetricSampleWriterUnknownProducerRunID", err)
	}
}

// TestPGMetricSampleWriter_WriteBatch_RejectsMixedProducerRunIDs
// proves the writer refuses a batch whose records do NOT
// share a single producer_run_id, BEFORE opening any DB
// transaction. This is a programmer-bug fence: foundation
// dispatch stamps `ProducerRunID = scanRun.ID` uniformly
// across the batch.
func TestPGMetricSampleWriter_WriteBatch_RejectsMixedProducerRunIDs(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec1 := metric_ingestor.MetricSampleRecord{
		SampleID:      uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111111")),
		RepoID:        uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
		SHA:           "abc1234567890123456789012345678901234567",
		ScopeID:       uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333")),
		MetricKind:    "cyclo", MetricVersion: 1,
		Pack: recipes.PackBase, Source: recipes.SourceComputed, Value: 1,
		ProducerRunID: uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444")),
	}
	rec2 := rec1
	rec2.SampleID = uuid.Must(uuid.FromString("11111111-1111-1111-1111-111111111112"))
	// Different producer_run_id -- this MUST be refused
	// without opening a transaction.
	rec2.ProducerRunID = uuid.Must(uuid.FromString("55555555-5555-5555-5555-555555555555"))

	// No DB expectations: the validation fires BEFORE
	// BeginTx.

	err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec1, rec2})
	if err == nil || !errors.Is(err, metric_ingestor.ErrPGMetricSampleWriterBatchMixedProducerRunIDs) {
		t.Errorf("WriteBatch mixed producer ids: err=%v, want errors.Is ErrPGMetricSampleWriterBatchMixedProducerRunIDs", err)
	}
	_ = mock
}
