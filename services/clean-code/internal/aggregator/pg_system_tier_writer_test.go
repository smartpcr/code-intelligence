package aggregator_test

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
)

const pgSystemTierWriterTestSchema = "clean_code_aggregator_test"

func newSQLMockSystemTierWriter(t *testing.T) (*aggregator.PGSystemTierWriter, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	w, err := aggregator.NewPGSystemTierWriterWithSchema(db, pgSystemTierWriterTestSchema)
	if err != nil {
		_ = db.Close()
		t.Fatalf("NewPGSystemTierWriterWithSchema: %v", err)
	}
	return w, mock, func() { _ = db.Close() }
}

// TestNewPGSystemTierWriter_RejectsNilDB pins the wiring guard.
func TestNewPGSystemTierWriter_RejectsNilDB(t *testing.T) {
	t.Parallel()
	if _, err := aggregator.NewPGSystemTierWriter(nil); !errors.Is(err, aggregator.ErrPGSystemTierWriterNilDB) {
		t.Errorf("NewPGSystemTierWriter(nil) err = %v, want ErrPGSystemTierWriterNilDB", err)
	}
}

// TestNewPGSystemTierWriterWithSchema_RejectsEmptySchema pins
// the guard against blank schema names. The PG writer is the
// SOLE persistence path for `pack='system'` rows (Phase 1.5
// grants) so a misconfigured schema is a deploy-time bug.
func TestNewPGSystemTierWriterWithSchema_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	db, _, _ := sqlmock.New()
	defer db.Close()
	if _, err := aggregator.NewPGSystemTierWriterWithSchema(db, ""); !errors.Is(err, aggregator.ErrPGSystemTierWriterEmptySchema) {
		t.Errorf("NewPGSystemTierWriterWithSchema(db, \"\") err = %v, want ErrPGSystemTierWriterEmptySchema", err)
	}
	if _, err := aggregator.NewPGSystemTierWriterWithSchema(db, "   "); !errors.Is(err, aggregator.ErrPGSystemTierWriterEmptySchema) {
		t.Errorf("NewPGSystemTierWriterWithSchema(db, \"   \") err = %v, want ErrPGSystemTierWriterEmptySchema", err)
	}
}

// happySystemTierSample returns a non-degraded sample shaped
// the way the composer would emit it -- caller plugs in the
// metric_kind so the test can iterate over the seven canonical
// kinds.
func happySystemTierSample(t *testing.T, kind string) aggregator.SystemTierSample {
	t.Helper()
	v := 1.0
	return aggregator.SystemTierSample{
		SampleID:       uuid.Must(uuid.NewV4()),
		RepoID:         uuid.Must(uuid.NewV4()),
		SHA:            "abc1230000000000000000000000000000000000",
		ScopeID:        uuid.Must(uuid.NewV4()),
		ScopeKind:      "repo",
		MetricKind:     kind,
		MetricVersion:  1,
		Value:          &v,
		Pack:           "system",
		Source:         "derived",
		Degraded:       false,
		DegradedReason: "",
		ProducerRunID:  uuid.Must(uuid.NewV4()),
		Attrs:          map[string]string{"window": "30d"},
	}
}

// degradedSystemTierSample returns a degraded sample shape the
// composer emits for embedded-mode `xrepo_dep_depth` /
// `blast_radius`.
func degradedSystemTierSample(t *testing.T) aggregator.SystemTierSample {
	t.Helper()
	return aggregator.SystemTierSample{
		SampleID:       uuid.Must(uuid.NewV4()),
		RepoID:         uuid.Must(uuid.NewV4()),
		SHA:            "abc1230000000000000000000000000000000000",
		ScopeID:        uuid.Must(uuid.NewV4()),
		ScopeKind:      "repo",
		MetricKind:     "xrepo_dep_depth",
		MetricVersion:  1,
		Value:          nil,
		Pack:           "system",
		Source:         "derived",
		Degraded:       true,
		DegradedReason: "xrepo_edges_unavailable",
		ProducerRunID:  uuid.Must(uuid.NewV4()),
	}
}

// TestPGSystemTierWriter_WriteSamples_SingleTransaction is the
// canonical write-trace pin. One composer batch produces one
// PG transaction carrying:
//
//  1. one PREPARE for the `metric_sample` INSERT
//  2. one PREPARE for the `metric_sample_active` upsert
//  3. one Exec per sample against each prepared statement
//  4. one Commit at the end
//
// The atomicity property is the architecture G6 guarantee
// that no reader observes a `metric_sample_active` pointer
// referring to a `metric_sample` row that does not exist (or
// vice versa). A partial commit would violate the composite
// FK from `metric_sample_active` back to `metric_sample`.
func TestPGSystemTierWriter_WriteSamples_SingleTransaction(t *testing.T) {
	t.Parallel()
	w, mock, closeFn := newSQLMockSystemTierWriter(t)
	defer closeFn()

	samples := []aggregator.SystemTierSample{
		happySystemTierSample(t, "arch_debt_ratio"),
		degradedSystemTierSample(t),
	}

	mock.ExpectBegin()
	insertPattern := regexp.MustCompile(`INSERT INTO "clean_code_aggregator_test"."metric_sample"`)
	upsertPattern := regexp.MustCompile(`INSERT INTO "clean_code_aggregator_test"."metric_sample_active".*ON CONFLICT.*DO UPDATE SET sample_id = EXCLUDED.sample_id`)
	insertPrep := mock.ExpectPrepare(insertPattern.String())
	upsertPrep := mock.ExpectPrepare(upsertPattern.String())
	for range samples {
		insertPrep.ExpectExec().WillReturnResult(sqlmock.NewResult(1, 1))
		upsertPrep.ExpectExec().WillReturnResult(sqlmock.NewResult(1, 1))
	}
	mock.ExpectCommit()

	if err := w.WriteSystemTierSamples(context.Background(), samples); err != nil {
		t.Fatalf("WriteSystemTierSamples: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestPGSystemTierWriter_WriteSamples_EmptyBatchNoOp asserts
// the no-op early return -- zero samples must NOT open a
// transaction (matches the in-memory writer's contract +
// avoids unnecessary PG round-trips for empty ticks).
func TestPGSystemTierWriter_WriteSamples_EmptyBatchNoOp(t *testing.T) {
	t.Parallel()
	w, mock, closeFn := newSQLMockSystemTierWriter(t)
	defer closeFn()

	// No mock.ExpectBegin -- if the writer opens a tx the
	// mock will fail.
	if err := w.WriteSystemTierSamples(context.Background(), nil); err != nil {
		t.Fatalf("WriteSystemTierSamples(nil): %v", err)
	}
	if err := w.WriteSystemTierSamples(context.Background(), []aggregator.SystemTierSample{}); err != nil {
		t.Fatalf("WriteSystemTierSamples([]): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock unexpected calls on empty batch: %v", err)
	}
}

// TestPGSystemTierWriter_WriteSamples_RollsBackOnInsertFailure
// is the failure path proof: when a metric_sample INSERT
// fails mid-batch, the transaction MUST roll back (no
// orphan rows) and the error surfaces to the caller with
// enough context to identify the offending sample.
func TestPGSystemTierWriter_WriteSamples_RollsBackOnInsertFailure(t *testing.T) {
	t.Parallel()
	w, mock, closeFn := newSQLMockSystemTierWriter(t)
	defer closeFn()

	samples := []aggregator.SystemTierSample{happySystemTierSample(t, "arch_debt_ratio")}

	mock.ExpectBegin()
	insertPrep := mock.ExpectPrepare(`INSERT INTO "clean_code_aggregator_test"."metric_sample"`)
	mock.ExpectPrepare(`INSERT INTO "clean_code_aggregator_test"."metric_sample_active"`)
	insertPrep.ExpectExec().WillReturnError(errors.New("pg: unique_violation on metric_sample_pkey"))
	mock.ExpectRollback()

	err := w.WriteSystemTierSamples(context.Background(), samples)
	if err == nil {
		t.Fatalf("WriteSystemTierSamples: err = nil, want non-nil (insert failure)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestPGSystemTierWriter_WriteSamples_RollsBackOnUpsertFailure
// is the failure path proof for the active-pointer upsert:
// when the upsert fails (e.g. FK violation because the
// `metric_kind` row is missing from the catalog), the whole
// batch MUST roll back -- there must be no `metric_sample`
// row without a corresponding `metric_sample_active` pointer.
func TestPGSystemTierWriter_WriteSamples_RollsBackOnUpsertFailure(t *testing.T) {
	t.Parallel()
	w, mock, closeFn := newSQLMockSystemTierWriter(t)
	defer closeFn()

	samples := []aggregator.SystemTierSample{happySystemTierSample(t, "arch_debt_ratio")}

	mock.ExpectBegin()
	insertPrep := mock.ExpectPrepare(`INSERT INTO "clean_code_aggregator_test"."metric_sample"`)
	upsertPrep := mock.ExpectPrepare(`INSERT INTO "clean_code_aggregator_test"."metric_sample_active"`)
	insertPrep.ExpectExec().WillReturnResult(sqlmock.NewResult(1, 1))
	upsertPrep.ExpectExec().WillReturnError(errors.New("pg: foreign_key_violation on metric_sample_active_metric_kind_fk"))
	mock.ExpectRollback()

	err := w.WriteSystemTierSamples(context.Background(), samples)
	if err == nil {
		t.Fatalf("WriteSystemTierSamples: err = nil, want non-nil (upsert failure)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestPGSystemTierWriter_WriteSamples_RejectsInvalidSample
// asserts the centralised invariant check runs BEFORE the
// transaction opens. A malformed sample (e.g. an invented
// metric_kind like `p50.system` -- iter-1 evaluator item 7)
// MUST fail fast without leaving a half-written transaction
// to roll back.
func TestPGSystemTierWriter_WriteSamples_RejectsInvalidSample(t *testing.T) {
	t.Parallel()
	w, mock, closeFn := newSQLMockSystemTierWriter(t)
	defer closeFn()

	bad := happySystemTierSample(t, "p50.system")

	err := w.WriteSystemTierSamples(context.Background(), []aggregator.SystemTierSample{bad})
	if err == nil {
		t.Fatalf("WriteSystemTierSamples: err = nil, want non-nil (invented metric_kind)")
	}
	// No BEGIN -- the validator must reject before any SQL
	// hits the driver.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock unexpected calls on invalid sample: %v", err)
	}
}

// TestPGSystemTierWriter_WriteSamples_UpsertShape_HasDoUpdate
// is a literal contract pin: the active-pointer upsert MUST
// use `ON CONFLICT ... DO UPDATE SET sample_id =
// EXCLUDED.sample_id` (per architecture Sec 5.2.1
// retract-then-reinsert + the foundation
// `metric_sample_active` table COMMENT at
// `0002_measurement.up.sql:539-552`). A `DO NOTHING` shape
// would silently drop re-composed rows -- a subtle but
// catastrophic regression because subsequent ticks would
// never update the active pointer to the freshly-composed
// row.
func TestPGSystemTierWriter_WriteSamples_UpsertShape_HasDoUpdate(t *testing.T) {
	t.Parallel()
	w, mock, closeFn := newSQLMockSystemTierWriter(t)
	defer closeFn()

	samples := []aggregator.SystemTierSample{happySystemTierSample(t, "arch_debt_ratio")}

	mock.ExpectBegin()
	mock.ExpectPrepare(`INSERT INTO "clean_code_aggregator_test"."metric_sample"`)
	// Require the EXACT upsert shape: DO UPDATE SET sample_id
	// = EXCLUDED.sample_id. A regex literal match here is the
	// canonical guard against accidental ON CONFLICT DO
	// NOTHING drift.
	upsertPrep := mock.ExpectPrepare(`ON CONFLICT \(repo_id, sha, scope_id, metric_kind, metric_version\)\s+DO UPDATE SET sample_id = EXCLUDED.sample_id`)
	mock.ExpectExec(`INSERT INTO "clean_code_aggregator_test"."metric_sample"`).WillReturnResult(sqlmock.NewResult(1, 1))
	upsertPrep.ExpectExec().WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := w.WriteSystemTierSamples(context.Background(), samples); err != nil {
		t.Fatalf("WriteSystemTierSamples: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations (upsert shape): %v", err)
	}
}
