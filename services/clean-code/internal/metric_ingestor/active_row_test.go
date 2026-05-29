package metric_ingestor_test

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
	"forge/services/clean-code/internal/metrics/recipes"
)

// Stage 3.3 -- Active row uniqueness enforcement.
//
// These tests pin the writer's SQL trace for the
// implementation-plan Stage 3.3 (architecture Sec 5.2.1 G2
// / tech-spec Sec 7.1.b lines 1070-1119 / Sec 10A pin lines
// 1659-1675):
//
//  1. After each metric_sample INSERT, the writer UPSERTs
//     `metric_sample_active` via
//     `INSERT ... ON CONFLICT (repo_id, sha, scope_id,
//     metric_kind, metric_version) DO UPDATE SET sample_id
//     = EXCLUDED.sample_id` in the SAME transaction. No
//     procedural `swap_active` verb / trigger / stored
//     function is used (implementation-plan Stage 3.3 iter
//     1 evaluator item 1).
//  2. `metric_sample` is INSERT-only: the writer never
//     issues UPDATE or DELETE against it (G3 / C2).
//  3. On re-ingest of the same `(sha, scope_id, metric_kind,
//     metric_version)`, the writer appends a NEW
//     `metric_sample` row (with a fresh `sample_id`) and the
//     UPSERT's `ON CONFLICT ... DO UPDATE` branch re-points
//     `metric_sample_active.sample_id`. The prior
//     `metric_sample` row stays in place forever per G3 --
//     the writer NEVER deletes it.
//  4. On a re-ingest that follows a `metric_retraction`
//     append, the same path still succeeds: the writer
//     INSERTs the new sample and the UPSERT re-points the
//     pointer. The `metric_sample_active` row itself was
//     LEFT IN PLACE during the retraction (DELETE is
//     REVOKEd on `metric_sample_active` per tech-spec Sec
//     7.2 / migration 0004_roles.up.sql:415). Readers join
//     through `metric_retraction` to filter the tombstone.

// activeRowFixedQuintuple returns the (repo_id, sha,
// scope_id, metric_kind, metric_version) quintuple shared
// by every re-ingest record in this suite. The quintuple is
// the PRIMARY KEY of `metric_sample_active`; two writes at
// the same quintuple MUST hit the UPSERT's ON CONFLICT
// branch.
func activeRowFixedQuintuple() (repoID, scopeID uuid.UUID, sha, metricKind string, metricVersion int) {
	return uuid.Must(uuid.FromString("22222222-2222-2222-2222-222222222222")),
		uuid.Must(uuid.FromString("33333333-3333-3333-3333-333333333333")),
		"abc1234567890123456789012345678901234567",
		"cyclo",
		1
}

// activeRowMakeRecord builds a [MetricSampleRecord] sharing
// the fixed quintuple, with a per-call `sampleID` so re-
// ingest tests can produce two records that differ ONLY in
// `sample_id` and `producer_run_id`.
func activeRowMakeRecord(sampleID, producerRunID uuid.UUID) metric_ingestor.MetricSampleRecord {
	repoID, scopeID, sha, kind, version := activeRowFixedQuintuple()
	return metric_ingestor.MetricSampleRecord{
		SampleID:      sampleID,
		RepoID:        repoID,
		SHA:           sha,
		ScopeID:       scopeID,
		MetricKind:    kind,
		MetricVersion: version,
		Pack:          recipes.PackBase,
		Source:        recipes.SourceComputed,
		Value:         3,
		ProducerRunID: producerRunID,
	}
}

// expectMetricSampleInsertOnce wires the per-record EXEC on
// the INSERT prepared statement with the canonical positional
// arguments (the full 11-column tuple).
func expectMetricSampleInsertOnce(t *testing.T, prep *sqlmock.ExpectedPrepare, rec metric_ingestor.MetricSampleRecord) {
	t.Helper()
	prep.ExpectExec().
		WithArgs(
			rec.SampleID, rec.RepoID, rec.SHA, rec.ScopeID,
			rec.MetricKind, rec.MetricVersion,
			rec.Value, string(rec.Pack), string(rec.Source),
			rec.ProducerRunID,
			sqlmock.AnyArg(), // attrs_json: nil when Attrs is empty
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectMetricSampleActiveUpsertOnce wires the per-record
// EXEC on the UPSERT prepared statement (the
// `metric_sample_active` ON CONFLICT DO UPDATE) with the
// six-column positional arguments
// `(repo_id, sha, scope_id, metric_kind, metric_version,
// sample_id)`.
func expectMetricSampleActiveUpsertOnce(t *testing.T, prep *sqlmock.ExpectedPrepare, rec metric_ingestor.MetricSampleRecord) {
	t.Helper()
	prep.ExpectExec().
		WithArgs(
			rec.RepoID, rec.SHA, rec.ScopeID,
			rec.MetricKind, rec.MetricVersion,
			rec.SampleID,
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// TestPGMetricSampleWriter_ActiveRow_FirstWrite_InsertsPointer
// pins the canonical first-write SQL trace: BEGIN, guard
// SELECT, INSERT into metric_sample, INSERT into
// metric_sample_active (ON CONFLICT branch NOT taken -- the
// pointer row does not yet exist for this quintuple),
// COMMIT.
func TestPGMetricSampleWriter_ActiveRow_FirstWrite_InsertsPointer(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec := activeRowMakeRecord(
		uuid.Must(uuid.FromString("11111111-1111-1111-1111-1111111111aa")),
		uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444")),
	)

	mock.ExpectBegin()
	expectScanRunGuard(t, mock, rec.ProducerRunID, "running")
	prepInsert := expectMetricSampleInsertPrepare(t, mock)
	expectMetricSampleInsertOnce(t, prepInsert, rec)
	prepActive := expectMetricSampleActiveUpsertPrepare(t, mock)
	expectMetricSampleActiveUpsertOnce(t, prepActive, rec)
	mock.ExpectCommit()

	if err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec}); err != nil {
		t.Fatalf("WriteBatch(first write): err=%v, want nil", err)
	}
}

// TestPGMetricSampleWriter_ActiveRow_ReingestSameSHA_RepointsPointer
// covers implementation-plan Stage 3.3 Scenario
// "re-ingest-without-retract-is-idempotent" (impl-plan line
// 321):
//
//	Given a `metric_sample` row already present and pointed-
//	to by `metric_sample_active`, When the Metric Ingestor
//	re-ingests the same SHA (here the writer path that
//	appends a fresh row -- the non-idempotent-skip branch),
//	Then `metric_sample` grows by one row and
//	`metric_sample_active.sample_id` is UPSERTed to the new
//	`sample_id`. The prior `metric_sample` row remains in
//	place forever (G3 / C2).
//
// The test drives TWO sequential `WriteBatch` calls against
// the SAME quintuple with DIFFERENT `sample_id`s. The second
// batch must NOT emit any UPDATE or DELETE against
// `metric_sample`; only the active-row UPSERT may MODIFY
// existing data, and it modifies a SEPARATE table
// (`metric_sample_active`).
func TestPGMetricSampleWriter_ActiveRow_ReingestSameSHA_RepointsPointer(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	producerRunID := uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444"))
	rec1 := activeRowMakeRecord(
		uuid.Must(uuid.FromString("11111111-1111-1111-1111-1111111111aa")),
		producerRunID,
	)
	// Re-ingest at the SAME quintuple with a fresh sample_id
	// and a fresh producer_run_id (a second scan_run).
	rec2 := activeRowMakeRecord(
		uuid.Must(uuid.FromString("11111111-1111-1111-1111-1111111111bb")),
		uuid.Must(uuid.FromString("55555555-5555-5555-5555-555555555555")),
	)
	// SANITY: rec1 and rec2 differ ONLY in sample_id and
	// producer_run_id. If they ever diverged on the quintuple
	// (repo / sha / scope / kind / version) the ON CONFLICT
	// branch would NOT fire, defeating the test.
	if rec1.SampleID == rec2.SampleID {
		t.Fatalf("test fixture bug: rec1 and rec2 share sample_id %s", rec1.SampleID)
	}
	if rec1.RepoID != rec2.RepoID ||
		rec1.SHA != rec2.SHA ||
		rec1.ScopeID != rec2.ScopeID ||
		rec1.MetricKind != rec2.MetricKind ||
		rec1.MetricVersion != rec2.MetricVersion {
		t.Fatalf("test fixture bug: rec1/rec2 differ on the quintuple; UPSERT would INSERT instead of UPDATE")
	}

	// ---- First WriteBatch (initial scan) ----
	mock.ExpectBegin()
	expectScanRunGuard(t, mock, rec1.ProducerRunID, "running")
	prepInsert1 := expectMetricSampleInsertPrepare(t, mock)
	expectMetricSampleInsertOnce(t, prepInsert1, rec1)
	prepActive1 := expectMetricSampleActiveUpsertPrepare(t, mock)
	expectMetricSampleActiveUpsertOnce(t, prepActive1, rec1)
	mock.ExpectCommit()

	if err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec1}); err != nil {
		t.Fatalf("WriteBatch(initial): err=%v, want nil", err)
	}

	// ---- Second WriteBatch (rescan at same SHA / same
	// quintuple, fresh sample_id, fresh scan_run) ----
	//
	// CRITICAL: the SQL trace below is the SAME shape as the
	// first call -- one INSERT into metric_sample, one
	// UPSERT into metric_sample_active. There is NO
	// `DELETE FROM metric_sample`, NO `UPDATE metric_sample
	// SET ...`, NO `DELETE FROM metric_sample_active`. G3
	// / C2 hold across re-ingest: only the side-relation
	// pointer is modified, and the modification is the
	// canonical ON CONFLICT DO UPDATE (no `swap_active`
	// verb, no trigger).
	mock.ExpectBegin()
	expectScanRunGuard(t, mock, rec2.ProducerRunID, "running")
	prepInsert2 := expectMetricSampleInsertPrepare(t, mock)
	expectMetricSampleInsertOnce(t, prepInsert2, rec2)
	prepActive2 := expectMetricSampleActiveUpsertPrepare(t, mock)
	expectMetricSampleActiveUpsertOnce(t, prepActive2, rec2)
	mock.ExpectCommit()

	if err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec2}); err != nil {
		t.Fatalf("WriteBatch(re-ingest): err=%v, want nil", err)
	}

	// `mock.ExpectationsWereMet` in `cleanup` will fail the
	// test if the writer emitted any unexpected statement
	// (e.g. an UPDATE / DELETE / extra INSERT). That is the
	// G3 / C2 "metric_sample is INSERT-only" enforcement at
	// the SQL-trace level.
}

// TestPGMetricSampleWriter_ActiveRow_ReingestAfterRetraction_Succeeds
// covers implementation-plan Stage 3.3 Scenario
// "re-ingest-after-retract-succeeds" (impl-plan line 322):
//
//	Given a sample retracted via `metric_retraction(sample_id)`
//	(the pointer row remains in `metric_sample_active` per
//	tech-spec Sec 7.2 line 1248 REVOKE DELETE), When the
//	Metric Ingestor re-ingests, Then a new `metric_sample`
//	row appears with a fresh `sample_id` AND
//	`metric_sample_active` is UPSERTed to point at the new
//	row; the original `metric_sample` row remains in place
//	(G3 / C2) and reader queries join through
//	`metric_retraction` to filter the prior tombstone.
//
// Concretely, this test models the writer-visible state
// AFTER the Stage 3.4 `mgmt.retract_sample` verb has
// appended a `metric_retraction(sample_id)` row for the
// prior sample. The writer never inspects
// `metric_retraction` -- retracted samples are filtered at
// READ time per architecture Sec 5.2.2 lines 1035-1037. The
// re-ingest path the writer executes is therefore
// indistinguishable at the SQL-trace layer from the
// no-prior-retraction rescan in
// `TestPGMetricSampleWriter_ActiveRow_ReingestSameSHA_RepointsPointer`.
// What differs is the CONTRACT the writer must honour: the
// re-ingest MUST succeed even though a retraction tombstone
// already exists; the active-row PRIMARY KEY does not
// conflict with `metric_retraction` (the two relations have
// disjoint PKs); and the existing
// `metric_sample_active.sample_id` pointer is re-pointed at
// the fresh sample by the same `ON CONFLICT ... DO UPDATE`
// branch.
func TestPGMetricSampleWriter_ActiveRow_ReingestAfterRetraction_Succeeds(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec := activeRowMakeRecord(
		uuid.Must(uuid.FromString("11111111-1111-1111-1111-1111111111cc")),
		uuid.Must(uuid.FromString("66666666-6666-6666-6666-666666666666")),
	)

	// The pre-condition (prior sample exists, retraction row
	// exists, active-row pointer still in place) is invisible
	// to the writer's WriteBatch -- it manipulates ONLY the
	// rows it is asked to write. The writer's per-batch SQL
	// trace is therefore:
	//
	//   BEGIN
	//   SELECT status FROM scan_run ... FOR SHARE
	//   INSERT INTO metric_sample (...)            -- new row
	//   INSERT INTO metric_sample_active (...)     -- UPSERT
	//     ON CONFLICT (...) DO UPDATE SET sample_id = EXCLUDED.sample_id
	//   COMMIT
	//
	// CRITICAL: the writer does NOT issue:
	//   - DELETE FROM metric_sample_active ...      (REVOKEd)
	//   - DELETE FROM metric_retraction ...         (REVOKEd)
	//   - UPDATE  metric_sample SET ...             (G3 / C2)
	//   - any procedural `swap_active(...)` call    (no such verb)
	//
	// `mock.ExpectationsWereMet` in cleanup catches any
	// unexpected statement; the listed Expects are the FULL
	// set of statements the writer is permitted to emit.
	mock.ExpectBegin()
	expectScanRunGuard(t, mock, rec.ProducerRunID, "running")
	prepInsert := expectMetricSampleInsertPrepare(t, mock)
	expectMetricSampleInsertOnce(t, prepInsert, rec)
	prepActive := expectMetricSampleActiveUpsertPrepare(t, mock)
	expectMetricSampleActiveUpsertOnce(t, prepActive, rec)
	mock.ExpectCommit()

	if err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec}); err != nil {
		t.Fatalf("WriteBatch(re-ingest after retraction): err=%v, want nil", err)
	}
}

// TestPGMetricSampleWriter_ActiveRow_UpsertFailure_RollsBackInsert
// pins the atomicity contract: a failure on the
// `metric_sample_active` UPSERT rolls back the PRECEDING
// `metric_sample` INSERT inside the same transaction. The
// active-row PRIMARY KEY uniqueness invariant cannot be left
// in a half-state where a new `metric_sample` row landed
// without its pointer.
func TestPGMetricSampleWriter_ActiveRow_UpsertFailure_RollsBackInsert(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	rec := activeRowMakeRecord(
		uuid.Must(uuid.FromString("11111111-1111-1111-1111-1111111111dd")),
		uuid.Must(uuid.FromString("77777777-7777-7777-7777-777777777777")),
	)

	wantErr := errors.New("simulated metric_sample_active UPSERT failure")

	mock.ExpectBegin()
	expectScanRunGuard(t, mock, rec.ProducerRunID, "running")
	prepInsert := expectMetricSampleInsertPrepare(t, mock)
	expectMetricSampleInsertOnce(t, prepInsert, rec)
	prepActive := expectMetricSampleActiveUpsertPrepare(t, mock)
	prepActive.ExpectExec().
		WithArgs(
			rec.RepoID, rec.SHA, rec.ScopeID,
			rec.MetricKind, rec.MetricVersion,
			rec.SampleID,
		).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	err := w.WriteBatch(context.Background(), []metric_ingestor.MetricSampleRecord{rec})
	if err == nil {
		t.Fatalf("WriteBatch(UPSERT failure): err=nil, want wrapping %v", wantErr)
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("WriteBatch(UPSERT failure): err=%v, want errors.Is %v", err, wantErr)
	}
}

// TestPGMetricSampleWriter_ActiveRow_NoSwapActiveVerb_NoStoredFunction
// is a SQL-trace pin: the active-row swap is the canonical
// `INSERT ... ON CONFLICT (repo_id, sha, scope_id,
// metric_kind, metric_version) DO UPDATE SET sample_id =
// EXCLUDED.sample_id`, NOT a procedural `swap_active(...)`
// call, NOT a `SELECT swap_active(...)`, NOT a trigger
// invocation (implementation-plan Stage 3.3 iter 1 evaluator
// item 1). This test compiles a single multi-record batch
// and asserts the trace contains EXACTLY one INSERT-into-
// metric_sample-prepared-statement and EXACTLY one
// INSERT-INTO-metric_sample_active-prepared-statement (no
// `SELECT swap_active`, no `CALL`, no trigger-fire-out-of-
// band).
func TestPGMetricSampleWriter_ActiveRow_NoSwapActiveVerb_NoStoredFunction(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	producerRunID := uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444"))
	mk := func(seed byte) metric_ingestor.MetricSampleRecord {
		rec := activeRowMakeRecord(
			uuid.Must(uuid.FromString("11111111-1111-1111-1111-1111111111"+twoHex(uint32(seed)))),
			producerRunID,
		)
		// Vary the scope_id so the batch members do NOT
		// collide on the active-row PRIMARY KEY (the writer
		// would still allow it but the test is clearer when
		// each row is a distinct quintuple).
		rec.ScopeID = uuid.Must(uuid.FromString("33333333-3333-3333-3333-3333333333" + twoHex(uint32(seed))))
		return rec
	}
	recs := []metric_ingestor.MetricSampleRecord{mk(1), mk(2), mk(3)}

	mock.ExpectBegin()
	expectScanRunGuard(t, mock, recs[0].ProducerRunID, "running")

	// EXACTLY one prepared statement for the metric_sample
	// INSERT (a `swap_active` verb would surface as an extra
	// Prepare / Query against an unknown function name and
	// fail the unmet-expectations check in cleanup).
	prepInsert := expectMetricSampleInsertPrepare(t, mock)
	for _, rec := range recs {
		expectMetricSampleInsertOnce(t, prepInsert, rec)
	}

	// EXACTLY one prepared statement for the active-row
	// UPSERT. The regex on
	// `expectMetricSampleActiveUpsertPrepare` ALREADY pins
	// `INSERT INTO ... metric_sample_active ... ON CONFLICT
	// (...) DO UPDATE SET sample_id = EXCLUDED.sample_id` --
	// a stored-function call (`SELECT swap_active(...)`) or a
	// trigger-out-of-band write would not match this regex
	// and would surface as an unmet expectation.
	prepActive := expectMetricSampleActiveUpsertPrepare(t, mock)
	for _, rec := range recs {
		expectMetricSampleActiveUpsertOnce(t, prepActive, rec)
	}
	mock.ExpectCommit()

	if err := w.WriteBatch(context.Background(), recs); err != nil {
		t.Fatalf("WriteBatch(no swap_active): err=%v, want nil", err)
	}
}

// TestPGMetricSampleWriter_ActiveRow_DeterministicLockOrder_PreventsDeadlock
// pins the writer's defensive deadlock-prevention contract:
// when given records in arbitrary order, the writer
// acquires `metric_sample_active` row locks in canonical
// (repo_id, sha, scope_id, metric_kind, metric_version,
// sample_id) order. Two concurrent `WriteBatch` calls with
// overlapping quintuples cannot cross-lock and deadlock
// because both will lock the shared quintuples in the same
// canonical sequence.
//
// This is asserted by passing records to `WriteBatch` in
// REVERSE quintuple order; the test then expects the per-
// record EXECs in CANONICAL (ascending scope_id) order. The
// sqlmock `MatchExpectationsInOrder` (default) catches any
// drift from canonical order at the EXEC layer.
func TestPGMetricSampleWriter_ActiveRow_DeterministicLockOrder_PreventsDeadlock(t *testing.T) {
	t.Parallel()
	w, mock, cleanup := newSQLMockMetricSampleWriter(t)
	defer cleanup()

	producerRunID := uuid.Must(uuid.FromString("44444444-4444-4444-4444-444444444444"))
	// Build three records whose only varying quintuple
	// component is scope_id; the canonical sort is then
	// strictly by scope_id ascending.
	mk := func(seed byte) metric_ingestor.MetricSampleRecord {
		rec := activeRowMakeRecord(
			uuid.Must(uuid.FromString("11111111-1111-1111-1111-1111111111"+twoHex(uint32(seed)))),
			producerRunID,
		)
		rec.ScopeID = uuid.Must(uuid.FromString("33333333-3333-3333-3333-3333333333" + twoHex(uint32(seed))))
		return rec
	}
	r1, r2, r3 := mk(1), mk(2), mk(3)
	// Caller-provided order: REVERSE of canonical.
	callerOrder := []metric_ingestor.MetricSampleRecord{r3, r2, r1}
	// Canonical (ascending scope_id) order -- the order the
	// writer MUST iterate in.
	canonicalOrder := []metric_ingestor.MetricSampleRecord{r1, r2, r3}

	mock.ExpectBegin()
	expectScanRunGuard(t, mock, producerRunID, "running")
	prepInsert := expectMetricSampleInsertPrepare(t, mock)
	for _, rec := range canonicalOrder {
		expectMetricSampleInsertOnce(t, prepInsert, rec)
	}
	prepActive := expectMetricSampleActiveUpsertPrepare(t, mock)
	for _, rec := range canonicalOrder {
		expectMetricSampleActiveUpsertOnce(t, prepActive, rec)
	}
	mock.ExpectCommit()

	if err := w.WriteBatch(context.Background(), callerOrder); err != nil {
		t.Fatalf("WriteBatch(reverse order): err=%v, want nil", err)
	}

	// The caller-provided slice MUST remain in caller order
	// (defensive copy contract); the writer is forbidden from
	// mutating the caller's slice in place.
	if callerOrder[0].ScopeID != r3.ScopeID ||
		callerOrder[1].ScopeID != r2.ScopeID ||
		callerOrder[2].ScopeID != r1.ScopeID {
		t.Fatalf("WriteBatch mutated caller's slice; want [r3, r2, r1], got order: [%s, %s, %s]",
			callerOrder[0].ScopeID, callerOrder[1].ScopeID, callerOrder[2].ScopeID)
	}
}
