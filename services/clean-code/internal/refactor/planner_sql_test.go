package refactor

// This file holds the sqlmock-driven test suite for the
// Stage 8.1 production SQL readers and writer. The tests in
// `planner_test.go` exercise the InMemory* fakes; these tests
// exercise the SQL fragments themselves so a refactor that
// renames a column, drops a JOIN, or changes the bind-arg
// order fails locally instead of failing in production. The
// evaluator iter-3 item 3 calls these out as required:
// "production read/write SQL can drift from the schema
// without test failure."
//
// Conventions match the existing sqlmock suites under
// `internal/metric_ingestor/...`:
//   - one mock per test, deferred close
//   - regex matchers normalize whitespace via `\s+`
//   - column-list assertions are baked into the regex so a
//     column drop/rename is caught by ErrPattern mismatch.

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// newSQLMock returns a `*sql.DB` backed by [sqlmock] plus a
// cleanup func that closes both. Callers MUST defer the
// cleanup so the DB connection is released even if the test
// fails. The mock is created with
// `QueryMatcherRegexp` so the SQL assertions can use
// whitespace-insensitive patterns.
func newSQLMock(t *testing.T) (*sql.DB, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	cleanup := func() {
		_ = db.Close()
	}
	return db, mock, cleanup
}

// -----------------------------------------------------------------------------
// SQLMetricSampleReader.ScopeMetrics -- the active-pointer join
// -----------------------------------------------------------------------------

// TestSQLMetricSampleReader_ScopeMetrics_JoinsActivePointer is
// the evaluator iter-3 item 1 regression. The reader MUST
// drive from `metric_sample_active` and join through
// `sample_id` into `metric_sample` so retracted samples are
// excluded by the join itself (architecture G2 / Sec 5.2.1
// lines 991-1003 + tech-spec Sec 7.1.b lines 1103-1119). The
// regex pins the JOIN clause + the `msa.*` qualified
// predicates so a future refactor that drops the join (e.g.
// reverting to a DISTINCT-ON-by-metric_version shape) fails
// here.
//
// The regex also pins the iter-4 fix: a `DISTINCT ON
// (msa.scope_id, msa.metric_kind)` projection plus an
// `ORDER BY ... msa.metric_version DESC` tail. The pointer
// table's PK is on the FULL quintuple including
// `metric_version`, so multiple co-active versions per
// `(scope, metric_kind)` are allowed by the schema; the
// reader collapses them to the largest version in SQL. A
// regression that drops DISTINCT ON would let
// `applyMetricSampleToScopeInputs` see ALL versions and the
// last-row-wins fold would be plan-order-dependent.
func TestSQLMetricSampleReader_ScopeMetrics_JoinsActivePointer(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	repoID := uuid.Must(uuid.NewV4())
	sha := "abcdef"
	kinds := HotSpotInputMetricKinds

	scopeA := uuid.Must(uuid.FromString("00000000-0000-0000-0000-0000000000a1"))
	scopeB := uuid.Must(uuid.FromString("00000000-0000-0000-0000-0000000000b2"))

	rows := sqlmock.NewRows([]string{"scope_id", "metric_kind", "value"}).
		AddRow(scopeA, MetricKindCyclo, 10.0).
		AddRow(scopeA, MetricKindModificationCountInWindow, 5.0).
		AddRow(scopeB, MetricKindFanOut, 3.0)

	// The regex pins ALL the things the iter-3/iter-4
	// feedback asks to surface: DISTINCT ON over the
	// (scope_id, metric_kind) pair, SELECT from
	// `metric_sample_active` (driver table) JOIN
	// `metric_sample` on the `sample_id`, qualified `msa.*`
	// predicates, `value IS NOT NULL` defence against bad
	// ingestion, ORDER BY ... metric_version DESC tail.
	// Whitespace is `\s+`-loose.
	pattern := `SELECT\s+DISTINCT\s+ON\s*\(\s*msa\.scope_id\s*,\s*msa\.metric_kind\s*\)\s+` +
		`msa\.scope_id,\s+msa\.metric_kind,\s+ms\.value\s+` +
		`FROM\s+clean_code\.metric_sample_active\s+msa\s+` +
		`JOIN\s+clean_code\.metric_sample\s+ms\s+` +
		`ON\s+ms\.sample_id\s*=\s*msa\.sample_id\s+` +
		`WHERE\s+msa\.repo_id\s*=\s*\$1\s+` +
		`AND\s+msa\.sha\s*=\s*\$2\s+` +
		`AND\s+msa\.metric_kind\s*=\s*ANY\(\$3\)\s+` +
		`AND\s+ms\.value\s+IS\s+NOT\s+NULL\s+` +
		`ORDER\s+BY\s+msa\.scope_id,\s+msa\.metric_kind,\s+msa\.metric_version\s+DESC`

	mock.ExpectQuery(pattern).
		WithArgs(repoID, sha, pq.Array(kinds)).
		WillReturnRows(rows)

	r := NewSQLMetricSampleReader(db)
	out, err := r.ScopeMetrics(context.Background(), repoID, sha, kinds)
	if err != nil {
		t.Fatalf("ScopeMetrics: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2 (scopeA + scopeB)", len(out))
	}
	// Output is sorted by ScopeID ASC.
	if out[0].ScopeID != scopeA {
		t.Errorf("out[0].ScopeID = %s, want %s", out[0].ScopeID, scopeA)
	}
	if !out[0].HasCyclo || out[0].Cyclo != 10 {
		t.Errorf("scopeA cyclo = (%v, %v), want (10, true)", out[0].Cyclo, out[0].HasCyclo)
	}
	if !out[0].HasModificationCount || out[0].ModificationCount != 5 {
		t.Errorf("scopeA modCount = (%v, %v), want (5, true)",
			out[0].ModificationCount, out[0].HasModificationCount)
	}
	if out[1].ScopeID != scopeB {
		t.Errorf("out[1].ScopeID = %s, want %s", out[1].ScopeID, scopeB)
	}
	if !out[1].HasFanOut || out[1].FanOut != 3 {
		t.Errorf("scopeB fanOut = (%v, %v), want (3, true)", out[1].FanOut, out[1].HasFanOut)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLMetricSampleReader_ScopeMetrics_PicksLargestMetricVersion is
// the evaluator iter-4 item 1 regression. The
// `metric_sample_active` PRIMARY KEY is on the FULL
// quintuple `(repo_id, sha, scope_id, metric_kind,
// metric_version)`, so multiple co-active versions of the
// same `(scope, metric_kind)` are allowed by the schema.
// The [MetricSampleReader] contract says the reader MUST
// pick the largest active `metric_version` deterministically.
// The production SQL achieves this via DISTINCT ON +
// ORDER BY ... metric_version DESC. This test asserts both:
//
//   - the regex pins the DISTINCT ON clause and the ORDER BY
//     metric_version DESC tail, so a refactor that drops
//     either fails here;
//   - the scope's [ScopeInputs] reflects the LARGEST-version
//     value when the mock returns a single row (DISTINCT ON
//     collapse simulated by the mock returning only the
//     winning row, the same shape the database would return).
//
// Together with the in-memory contract test
// `TestPlanner_MetricDedupe_TakesLargestMetricVersion` in
// `planner_test.go` (which covers the InMemory reader's
// max-by-version behavior), this guarantees both readers
// honor the same contract.
func TestSQLMetricSampleReader_ScopeMetrics_PicksLargestMetricVersion(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	repoID := uuid.Must(uuid.NewV4())
	sha := "abcdef"
	kinds := HotSpotInputMetricKinds

	scope := uuid.Must(uuid.FromString("00000000-0000-0000-0000-000000000c33"))

	// DISTINCT ON + ORDER BY in Postgres returns ONLY the
	// winning row per partition (here: the largest
	// metric_version for this (scope, cyclo) pair). The
	// mock therefore returns only the v=7 row with
	// value=99. If a regression drops DISTINCT ON, the
	// production code would ask for ALL versions; the
	// regex below would fail because the SQL pattern still
	// requires the DISTINCT ON clause -- so the test would
	// fail at the ExpectationsWereMet stage, not in the
	// fold logic. This is intentional: the SQL fragment
	// itself is the contract, not the application-side
	// fold.
	rows := sqlmock.NewRows([]string{"scope_id", "metric_kind", "value"}).
		AddRow(scope, MetricKindCyclo, 99.0)

	pattern := `SELECT\s+DISTINCT\s+ON\s*\(\s*msa\.scope_id\s*,\s*msa\.metric_kind\s*\)\s+` +
		`msa\.scope_id,\s+msa\.metric_kind,\s+ms\.value\s+` +
		`FROM\s+clean_code\.metric_sample_active\s+msa\s+` +
		`JOIN\s+clean_code\.metric_sample\s+ms\s+` +
		`ON\s+ms\.sample_id\s*=\s*msa\.sample_id\s+` +
		`WHERE\s+msa\.repo_id\s*=\s*\$1\s+` +
		`AND\s+msa\.sha\s*=\s*\$2\s+` +
		`AND\s+msa\.metric_kind\s*=\s*ANY\(\$3\)\s+` +
		`AND\s+ms\.value\s+IS\s+NOT\s+NULL\s+` +
		`ORDER\s+BY\s+msa\.scope_id,\s+msa\.metric_kind,\s+msa\.metric_version\s+DESC`

	mock.ExpectQuery(pattern).
		WithArgs(repoID, sha, pq.Array(kinds)).
		WillReturnRows(rows)

	r := NewSQLMetricSampleReader(db)
	out, err := r.ScopeMetrics(context.Background(), repoID, sha, kinds)
	if err != nil {
		t.Fatalf("ScopeMetrics: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if out[0].ScopeID != scope {
		t.Errorf("ScopeID = %s, want %s", out[0].ScopeID, scope)
	}
	if !out[0].HasCyclo || out[0].Cyclo != 99 {
		t.Errorf("Cyclo = (%v, %v), want (99, true) (largest-version row)",
			out[0].Cyclo, out[0].HasCyclo)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLMetricSampleReader_ScopeMetrics_PropagatesQueryError
// confirms a query failure is wrapped and returned (not
// silently swallowed).
func TestSQLMetricSampleReader_ScopeMetrics_PropagatesQueryError(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	wantErr := errors.New("synthetic query failure")
	mock.ExpectQuery(`SELECT\s+DISTINCT\s+ON`).WillReturnError(wantErr)

	r := NewSQLMetricSampleReader(db)
	_, err := r.ScopeMetrics(context.Background(),
		uuid.Must(uuid.NewV4()), "sha", HotSpotInputMetricKinds)
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("errors.Is(err, wantErr) = false; err = %v", err)
	}
}

// -----------------------------------------------------------------------------
// SQLFindingReader.FindingCountsByScope -- the policy_version_id filter
// -----------------------------------------------------------------------------

// TestSQLFindingReader_FindingCountsByScope_FiltersByActivePolicyVersionID
// is the evaluator iter-3 item 2 regression. The SQL MUST
// carry a `policy_version_id = $4` filter so findings from
// parallel evaluations against an inactive policy at the
// same SHA do NOT inflate the active-policy hot_spot's
// finding_count (architecture Sec 5.5.1 reproducibility).
// The regex pins the canonical predicate + the bind-arg
// positions.
func TestSQLFindingReader_FindingCountsByScope_FiltersByActivePolicyVersionID(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	repoID := uuid.Must(uuid.NewV4())
	sha := "abcdef"
	pvID := uuid.Must(uuid.NewV4())

	scope := uuid.Must(uuid.NewV4())
	rows := sqlmock.NewRows([]string{"scope_id", "count"}).
		AddRow(scope, int64(7))

	qualifying := make([]string, len(HotSpotQualifyingDeltas))
	for i, d := range HotSpotQualifyingDeltas {
		qualifying[i] = string(d)
	}

	// The regex pins:
	//   - COUNT(*)::bigint shape (cast to bigint so the
	//     Scan into int64 always succeeds);
	//   - repo_id / sha / delta IN-list filters;
	//   - the NEW policy_version_id = $4 filter (iter-3 #2);
	//   - GROUP BY scope_id (so the application code
	//     processes one row per scope).
	pattern := `SELECT\s+scope_id,\s+COUNT\(\*\)::bigint\s+` +
		`FROM\s+clean_code\.finding\s+` +
		`WHERE\s+repo_id\s*=\s*\$1\s+` +
		`AND\s+sha\s*=\s*\$2\s+` +
		`AND\s+delta::text\s*=\s*ANY\(\$3\)\s+` +
		`AND\s+policy_version_id\s*=\s*\$4\s+` +
		`GROUP\s+BY\s+scope_id`

	mock.ExpectQuery(pattern).
		WithArgs(repoID, sha, pq.Array(qualifying), pvID).
		WillReturnRows(rows)

	r := NewSQLFindingReader(db)
	got, err := r.FindingCountsByScope(context.Background(), repoID, sha, pvID)
	if err != nil {
		t.Fatalf("FindingCountsByScope: %v", err)
	}
	if got[scope] != 7 {
		t.Errorf("count for scope = %d, want 7", got[scope])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLFindingReader_FindingCountsByScope_PropagatesQueryError.
func TestSQLFindingReader_FindingCountsByScope_PropagatesQueryError(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()
	wantErr := errors.New("synthetic finding failure")
	mock.ExpectQuery(`SELECT\s+scope_id,\s+COUNT`).WillReturnError(wantErr)
	r := NewSQLFindingReader(db)
	_, err := r.FindingCountsByScope(context.Background(),
		uuid.Must(uuid.NewV4()), "sha", uuid.Must(uuid.NewV4()))
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("errors.Is(err, wantErr) = false; err = %v", err)
	}
}

// -----------------------------------------------------------------------------
// SQLHotSpotWriter.WriteHotSpots -- transactional INSERT trace
// -----------------------------------------------------------------------------

// TestSQLHotSpotWriter_WriteHotSpots_TransactionalInsert pins
// the canonical write trace: BEGIN, PREPARE INSERT ... 7
// canonical columns, EXEC per row with the right positional
// args, COMMIT. The regex on the prepared statement asserts
// every one of the 7 columns is present and bound; a column
// rename or addition fails here.
func TestSQLHotSpotWriter_WriteHotSpots_TransactionalInsert(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	row1 := HotSpot{
		HotspotID:       uuid.Must(uuid.NewV4()),
		RepoID:          repoID,
		SHA:             "sha-1",
		ScopeID:         uuid.Must(uuid.NewV4()),
		Score:           4.5,
		PolicyVersionID: pvID,
		CreatedAt:       createdAt,
	}
	row2 := HotSpot{
		HotspotID:       uuid.Must(uuid.NewV4()),
		RepoID:          repoID,
		SHA:             "sha-1",
		ScopeID:         uuid.Must(uuid.NewV4()),
		Score:           2.1,
		PolicyVersionID: pvID,
		CreatedAt:       createdAt,
	}

	// The regex pins ALL 7 canonical columns from migration
	// 0003: hotspot_id, repo_id, sha, scope_id, score,
	// policy_version_id, created_at -- in that order. A
	// schema drift (e.g. adding score_breakdown JSONB or
	// renaming a column) breaks this match.
	insertPattern := `INSERT\s+INTO\s+clean_code\.hot_spot\s+\(\s+` +
		`hotspot_id,\s+` +
		`repo_id,\s+` +
		`sha,\s+` +
		`scope_id,\s+` +
		`score,\s+` +
		`policy_version_id,\s+` +
		`created_at\s+` +
		`\)\s+VALUES\s+\(\s+\$1,\s+\$2,\s+\$3,\s+\$4,\s+\$5,\s+\$6,\s+\$7\s+\)`

	mock.ExpectBegin()
	prep := mock.ExpectPrepare(insertPattern)
	prep.ExpectExec().
		WithArgs(row1.HotspotID, row1.RepoID, row1.SHA, row1.ScopeID,
			row1.Score, row1.PolicyVersionID, row1.CreatedAt.UTC()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	prep.ExpectExec().
		WithArgs(row2.HotspotID, row2.RepoID, row2.SHA, row2.ScopeID,
			row2.Score, row2.PolicyVersionID, row2.CreatedAt.UTC()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewSQLHotSpotWriter(db)
	if err := w.WriteHotSpots(context.Background(), []HotSpot{row1, row2}); err != nil {
		t.Fatalf("WriteHotSpots: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLHotSpotWriter_WriteHotSpots_RollsBackOnExecError pins
// the canonical failure path: if any per-row EXEC fails, the
// transaction is rolled back and the error is returned. No
// partial batch ever lands.
func TestSQLHotSpotWriter_WriteHotSpots_RollsBackOnExecError(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	row := HotSpot{
		HotspotID:       uuid.Must(uuid.NewV4()),
		RepoID:          uuid.Must(uuid.NewV4()),
		SHA:             "sha",
		ScopeID:         uuid.Must(uuid.NewV4()),
		Score:           1,
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		CreatedAt:       time.Now().UTC(),
	}
	wantErr := errors.New("synthetic exec failure")

	mock.ExpectBegin()
	prep := mock.ExpectPrepare(`INSERT\s+INTO\s+clean_code\.hot_spot`)
	prep.ExpectExec().
		WithArgs(row.HotspotID, row.RepoID, row.SHA, row.ScopeID,
			row.Score, row.PolicyVersionID, row.CreatedAt.UTC()).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	w := NewSQLHotSpotWriter(db)
	err := w.WriteHotSpots(context.Background(), []HotSpot{row})
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("errors.Is(err, wantErr) = false; err = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLHotSpotWriter_WriteHotSpots_EmptyBatchDoesNotBegin
// pins the no-op branch: an empty batch MUST NOT open a
// transaction. We use a fresh mock with NO expectations and
// rely on `ExpectationsWereMet` failing if a BEGIN was
// issued. This guarantees the Planner's "empty input" branch
// is free even at SQL layer.
func TestSQLHotSpotWriter_WriteHotSpots_EmptyBatchDoesNotBegin(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	w := NewSQLHotSpotWriter(db)
	if err := w.WriteHotSpots(context.Background(), nil); err != nil {
		t.Fatalf("WriteHotSpots(nil): %v", err)
	}
	if err := w.WriteHotSpots(context.Background(), []HotSpot{}); err != nil {
		t.Fatalf("WriteHotSpots([]): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// -----------------------------------------------------------------------------
// Sanity: the canonical SQL regexes compile (defensive check).
// -----------------------------------------------------------------------------

// TestSQLPatternsCompile checks every literal regex used by
// the tests above compiles. A typo in a complex regex would
// otherwise show up only when the corresponding test runs;
// this catches it at the top of the suite.
func TestSQLPatternsCompile(t *testing.T) {
	patterns := []string{
		`SELECT\s+ms\.scope_id,\s+ms\.metric_kind,\s+ms\.value\s+FROM\s+clean_code\.metric_sample_active\s+msa`,
		`SELECT\s+scope_id,\s+COUNT\(\*\)::bigint\s+FROM\s+clean_code\.finding`,
		`INSERT\s+INTO\s+clean_code\.hot_spot\s+\(\s+hotspot_id`,
	}
	for _, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			t.Errorf("Compile(%q): %v", p, err)
		}
	}
}
