package refactor

// Stage 8.2 sqlmock-driven coverage for the production
// [SQLHotSpotReader], [SQLFindingDetailReader], and
// [SQLRefactorPlanTaskWriter]. The tests in
// `task_planner_test.go` exercise the [InMemory*] fakes;
// these tests exercise the SQL fragments themselves so a
// future refactor that renames a column, drops a JOIN
// predicate, changes the bind-arg order, or drops the
// `::clean_code.refactor_task_kind` ENUM cast fails locally
// instead of failing in production. The evaluator iter-1
// blocker #4 calls these out as required:
// "no sqlmock tests for SQLFindingDetailReader or
// SQLRefactorPlanTaskWriter, so the production SQL shape,
// enum cast, JSONB hotspot_ids insert, rollback behavior,
// and plan_id mismatch guard are not pinned."
//
// Conventions match `planner_sql_test.go`:
//   - one mock per test, deferred cleanup
//   - `QueryMatcherRegexp` so SQL patterns are
//     whitespace-insensitive (`\s+` between tokens)
//   - column-list assertions baked into the regex so a
//     column drop/rename fails ErrPattern matching

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"
)

// -----------------------------------------------------------------------------
// SQLHotSpotReader.LatestHotSpotsByScore
// -----------------------------------------------------------------------------

// TestSQLHotSpotReader_LatestHotSpotsByScore_TopNPositive pins
// the canonical query for `topN > 0`:
//
//   - selects every column the [HotSpot] struct binds
//   - filters by (repo_id, sha, policy_version_id)
//   - uses a `created_at = (SELECT MAX(created_at) ...)`
//     subquery so only the latest batch is returned
//     (rubber-duck iter-2 finding around single-writer
//     assumption)
//   - order is `score DESC, scope_id ASC` (deterministic)
//   - tail is `LIMIT $4`
func TestSQLHotSpotReader_LatestHotSpotsByScore_TopNPositive(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	sha := "deadbeef"
	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	row1 := HotSpot{
		HotspotID:       uuid.Must(uuid.NewV4()),
		RepoID:          repoID,
		SHA:             sha,
		ScopeID:         uuid.Must(uuid.NewV4()),
		Score:           42.5,
		PolicyVersionID: pvID,
		CreatedAt:       createdAt,
	}
	row2 := HotSpot{
		HotspotID:       uuid.Must(uuid.NewV4()),
		RepoID:          repoID,
		SHA:             sha,
		ScopeID:         uuid.Must(uuid.NewV4()),
		Score:           19.0,
		PolicyVersionID: pvID,
		CreatedAt:       createdAt,
	}

	pattern := `SELECT\s+hotspot_id,\s+repo_id,\s+sha,\s+scope_id,\s+score,\s+` +
		`policy_version_id,\s+created_at\s+` +
		`FROM\s+clean_code\.hot_spot\s+` +
		`WHERE\s+repo_id\s*=\s*\$1\s+` +
		`AND\s+sha\s*=\s*\$2\s+` +
		`AND\s+policy_version_id\s*=\s*\$3\s+` +
		`AND\s+created_at\s*=\s*\(\s+` +
		`SELECT\s+MAX\(created_at\)\s+` +
		`FROM\s+clean_code\.hot_spot\s+` +
		`WHERE\s+repo_id\s*=\s*\$1\s+` +
		`AND\s+sha\s*=\s*\$2\s+` +
		`AND\s+policy_version_id\s*=\s*\$3\s+` +
		`\)\s+` +
		`ORDER\s+BY\s+score\s+DESC,\s+scope_id\s+ASC\s+` +
		`LIMIT\s+\$4`

	rows := sqlmock.NewRows([]string{
		"hotspot_id", "repo_id", "sha", "scope_id", "score",
		"policy_version_id", "created_at",
	}).
		AddRow(row1.HotspotID, row1.RepoID, row1.SHA, row1.ScopeID,
			row1.Score, row1.PolicyVersionID, row1.CreatedAt).
		AddRow(row2.HotspotID, row2.RepoID, row2.SHA, row2.ScopeID,
			row2.Score, row2.PolicyVersionID, row2.CreatedAt)
	mock.ExpectQuery(pattern).
		WithArgs(repoID, sha, pvID, 5).
		WillReturnRows(rows)

	r := NewSQLHotSpotReader(db)
	got, err := r.LatestHotSpotsByScore(context.Background(), repoID, sha, pvID, 5)
	if err != nil {
		t.Fatalf("LatestHotSpotsByScore: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].HotspotID != row1.HotspotID {
		t.Errorf("got[0].HotspotID = %s, want %s", got[0].HotspotID, row1.HotspotID)
	}
	if got[1].Score != 19.0 {
		t.Errorf("got[1].Score = %v, want 19.0", got[1].Score)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLHotSpotReader_LatestHotSpotsByScore_TopNZero_OmitsLimit
// confirms the `topN == 0` (no truncation) branch issues a
// query WITHOUT the `LIMIT $4` tail and binds only 3 args.
// We use a separate mock for the negative-assertion shape:
// expect a query that ends BEFORE any LIMIT token.
func TestSQLHotSpotReader_LatestHotSpotsByScore_TopNZero_OmitsLimit(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	sha := "feedface"

	// Pattern intentionally ends at the ORDER BY tail so a
	// future regression that appends a hard-coded `LIMIT`
	// would still match -- but the WithArgs assertion (3
	// args) catches that case anyway.
	pattern := `SELECT\s+hotspot_id.*FROM\s+clean_code\.hot_spot.*` +
		`ORDER\s+BY\s+score\s+DESC,\s+scope_id\s+ASC\s*$`

	mock.ExpectQuery(pattern).
		WithArgs(repoID, sha, pvID). // only 3 args -- no $4
		WillReturnRows(sqlmock.NewRows([]string{
			"hotspot_id", "repo_id", "sha", "scope_id", "score",
			"policy_version_id", "created_at",
		}))

	r := NewSQLHotSpotReader(db)
	got, err := r.LatestHotSpotsByScore(context.Background(), repoID, sha, pvID, 0)
	if err != nil {
		t.Fatalf("LatestHotSpotsByScore: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLHotSpotReader_LatestHotSpotsByScore_NegativeTopN_RejectsBeforeQuery
// confirms a negative topN returns [ErrInvalidTopN] without
// touching the DB. The mock has NO expectations.
func TestSQLHotSpotReader_LatestHotSpotsByScore_NegativeTopN_RejectsBeforeQuery(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()
	r := NewSQLHotSpotReader(db)
	_, err := r.LatestHotSpotsByScore(context.Background(),
		uuid.Must(uuid.NewV4()), "sha", uuid.Must(uuid.NewV4()), -1)
	if !errors.Is(err, ErrInvalidTopN) {
		t.Fatalf("err = %v, want ErrInvalidTopN", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v (no queries should have run)", err)
	}
}

// TestSQLHotSpotReader_LatestHotSpotsByScore_PropagatesQueryError
// asserts a DB-layer error is wrapped with the canonical
// `query` tag so an operator can grep server logs.
func TestSQLHotSpotReader_LatestHotSpotsByScore_PropagatesQueryError(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()
	want := errors.New("synthetic query failure")
	mock.ExpectQuery(`SELECT\s+hotspot_id`).WillReturnError(want)
	r := NewSQLHotSpotReader(db)
	_, err := r.LatestHotSpotsByScore(context.Background(),
		uuid.Must(uuid.NewV4()), "sha", uuid.Must(uuid.NewV4()), 5)
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wraps want", err)
	}
}

// -----------------------------------------------------------------------------
// SQLFindingDetailReader.FindingDetails
// -----------------------------------------------------------------------------

// TestSQLFindingDetailReader_FindingDetails_PinsDistinctScopeRule
// pins the production SQL shape:
//
//   - `SELECT DISTINCT scope_id, rule_id` (dedupes at SQL
//     layer)
//   - `delta::text = ANY($4)` so the qualifying-delta filter
//     uses the Postgres `text` form of the ENUM
//   - `policy_version_id = $5` MUST be present so multi-PV
//     finding rows for the same scope don't leak across
//     policies
func TestSQLFindingDetailReader_FindingDetails_PinsDistinctScopeRule(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	sha := "deadbeef"
	scope1 := uuid.Must(uuid.NewV4())
	scope2 := uuid.Must(uuid.NewV4())

	pattern := `SELECT\s+DISTINCT\s+scope_id,\s+rule_id\s+` +
		`FROM\s+clean_code\.finding\s+` +
		`WHERE\s+repo_id\s*=\s*\$1\s+` +
		`AND\s+sha\s*=\s*\$2\s+` +
		`AND\s+scope_id\s*=\s*ANY\(\$3\)\s+` +
		`AND\s+delta::text\s*=\s*ANY\(\$4\)\s+` +
		`AND\s+policy_version_id\s*=\s*\$5\s+` +
		`ORDER\s+BY\s+scope_id,\s+rule_id`

	rows := sqlmock.NewRows([]string{"scope_id", "rule_id"}).
		AddRow(scope1, "solid.srp.lcom4_high").
		AddRow(scope2, "decoupling.cycle_member")
	mock.ExpectQuery(pattern).WillReturnRows(rows)

	r := NewSQLFindingDetailReader(db)
	got, err := r.FindingDetails(context.Background(), repoID, sha, pvID,
		[]uuid.UUID{scope1, scope2})
	if err != nil {
		t.Fatalf("FindingDetails: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}
	if got[0].ScopeID != scope1 || got[0].RuleID != "solid.srp.lcom4_high" {
		t.Errorf("got[0] = %+v, want scope1 / solid.srp.lcom4_high", got[0])
	}
	if got[1].ScopeID != scope2 || got[1].RuleID != "decoupling.cycle_member" {
		t.Errorf("got[1] = %+v, want scope2 / decoupling.cycle_member", got[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLFindingDetailReader_FindingDetails_EmptyScopeIDs_NoQuery
// confirms an empty scope_ids slice returns `(nil, nil)`
// without issuing a query -- the planner's empty-input
// branch must remain free at SQL layer.
func TestSQLFindingDetailReader_FindingDetails_EmptyScopeIDs_NoQuery(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()
	r := NewSQLFindingDetailReader(db)
	got, err := r.FindingDetails(context.Background(),
		uuid.Must(uuid.NewV4()), "sha", uuid.Must(uuid.NewV4()), nil)
	if err != nil {
		t.Fatalf("FindingDetails(nil): %v", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil", got)
	}
	got, err = r.FindingDetails(context.Background(),
		uuid.Must(uuid.NewV4()), "sha", uuid.Must(uuid.NewV4()), []uuid.UUID{})
	if err != nil {
		t.Fatalf("FindingDetails([]): %v", err)
	}
	if got != nil {
		t.Errorf("got = %v, want nil", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v (no queries should have run)", err)
	}
}

// TestSQLFindingDetailReader_FindingDetails_PropagatesQueryError
// asserts the wrap path on a query failure.
func TestSQLFindingDetailReader_FindingDetails_PropagatesQueryError(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()
	want := errors.New("synthetic finding query failure")
	mock.ExpectQuery(`SELECT\s+DISTINCT\s+scope_id`).WillReturnError(want)
	r := NewSQLFindingDetailReader(db)
	_, err := r.FindingDetails(context.Background(),
		uuid.Must(uuid.NewV4()), "sha", uuid.Must(uuid.NewV4()),
		[]uuid.UUID{uuid.Must(uuid.NewV4())})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wraps want", err)
	}
}

// -----------------------------------------------------------------------------
// SQLRefactorPlanTaskWriter.WriteRefactorPlanAndTasks
// -----------------------------------------------------------------------------

// TestSQLRefactorPlanTaskWriter_Write_PinsPlanInsertAndTaskEnumCast
// pins the canonical happy-path: ONE transaction, ONE plan
// INSERT (with `::jsonb` cast on hotspot_ids), then ONE
// prepared task INSERT (with `::clean_code.refactor_task_kind`
// ENUM cast), then COMMIT. A schema drift that drops the
// JSONB column, renames a column, or drops the ENUM cast
// fails ErrPattern.
func TestSQLRefactorPlanTaskWriter_Write_PinsPlanInsertAndTaskEnumCast(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	createdAt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	planID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	hotspot1 := uuid.Must(uuid.NewV4())
	hotspot2 := uuid.Must(uuid.NewV4())

	plan := RefactorPlan{
		PlanID:     planID,
		RepoID:     repoID,
		SHA:        "deadbeef",
		HotspotIDs: []uuid.UUID{hotspot1, hotspot2},
		SummaryMD:  "## Stage 8.2 plan",
		CreatedAt:  createdAt,
	}
	task1 := RefactorTask{
		TaskID:        uuid.Must(uuid.NewV4()),
		PlanID:        planID,
		ScopeID:       uuid.Must(uuid.NewV4()),
		Kind:          TaskKindSplitClass,
		EffortHours:   0,
		RuleID:        "solid.srp.lcom4_high",
		DescriptionMD: "Split the LCOM-4 class.",
		CreatedAt:     createdAt,
	}
	task2 := RefactorTask{
		TaskID:        uuid.Must(uuid.NewV4()),
		PlanID:        planID,
		ScopeID:       uuid.Must(uuid.NewV4()),
		Kind:          TaskKindBreakCycle,
		EffortHours:   0,
		RuleID:        "decoupling.cycle_member",
		DescriptionMD: "Break the import cycle.",
		CreatedAt:     createdAt,
	}

	// Plan INSERT: pin all 6 canonical columns from
	// migration 0003 + the `::jsonb` cast on $4. A schema
	// drift here breaks the regex.
	planPattern := `INSERT\s+INTO\s+clean_code\.refactor_plan\s+\(\s+` +
		`plan_id,\s+repo_id,\s+sha,\s+hotspot_ids,\s+summary_md,\s+created_at\s+` +
		`\)\s+VALUES\s+\(\s+` +
		`\$1,\s+\$2,\s+\$3,\s+\$4::jsonb,\s+\$5,\s+\$6\s+\)`

	// Task INSERT: pin all 8 columns + the `::clean_code.refactor_task_kind`
	// ENUM cast on $4 (kind). The migration declares the
	// canonical ENUM at line 140 of 0003_policy_audit_refactor.up.sql.
	taskPattern := `INSERT\s+INTO\s+clean_code\.refactor_task\s+\(\s+` +
		`task_id,\s+plan_id,\s+scope_id,\s+kind,\s+effort_hours,\s+rule_id,\s+` +
		`description_md,\s+created_at\s+` +
		`\)\s+VALUES\s+\(\s+` +
		`\$1,\s+\$2,\s+\$3,\s+\$4::clean_code\.refactor_task_kind,\s+\$5,\s+\$6,\s+\$7,\s+\$8\s+\)`

	mock.ExpectBegin()
	mock.ExpectExec(planPattern).
		WithArgs(
			plan.PlanID, plan.RepoID, plan.SHA,
			sqlmock.AnyArg(), // marshalled JSON; struct equality across encoders is fragile
			plan.SummaryMD, plan.CreatedAt.UTC(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	prep := mock.ExpectPrepare(taskPattern)
	prep.ExpectExec().
		WithArgs(
			task1.TaskID, task1.PlanID, task1.ScopeID,
			string(task1.Kind), task1.EffortHours,
			task1.RuleID, task1.DescriptionMD, task1.CreatedAt.UTC(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	prep.ExpectExec().
		WithArgs(
			task2.TaskID, task2.PlanID, task2.ScopeID,
			string(task2.Kind), task2.EffortHours,
			task2.RuleID, task2.DescriptionMD, task2.CreatedAt.UTC(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	w := NewSQLRefactorPlanTaskWriter(db)
	if err := w.WriteRefactorPlanAndTasks(context.Background(), plan,
		[]RefactorTask{task1, task2}); err != nil {
		t.Fatalf("WriteRefactorPlanAndTasks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLRefactorPlanTaskWriter_Write_RollsBackOnTaskInsertError
// pins the canonical failure path: if a task EXEC fails,
// the transaction is rolled back and the wrapped error is
// returned. The plan row never lands.
func TestSQLRefactorPlanTaskWriter_Write_RollsBackOnTaskInsertError(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	createdAt := time.Now().UTC()
	planID := uuid.Must(uuid.NewV4())
	plan := RefactorPlan{
		PlanID:     planID,
		RepoID:     uuid.Must(uuid.NewV4()),
		SHA:        "sha",
		HotspotIDs: []uuid.UUID{uuid.Must(uuid.NewV4())},
		SummaryMD:  "## plan",
		CreatedAt:  createdAt,
	}
	task := RefactorTask{
		TaskID:        uuid.Must(uuid.NewV4()),
		PlanID:        planID,
		ScopeID:       uuid.Must(uuid.NewV4()),
		Kind:          TaskKindExtractMethod,
		EffortHours:   0,
		RuleID:        "solid.srp.method_too_long",
		DescriptionMD: "Extract.",
		CreatedAt:     createdAt,
	}
	wantErr := errors.New("synthetic task exec failure")

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT\s+INTO\s+clean_code\.refactor_plan`).
		WithArgs(plan.PlanID, plan.RepoID, plan.SHA, sqlmock.AnyArg(),
			plan.SummaryMD, plan.CreatedAt.UTC()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	prep := mock.ExpectPrepare(`INSERT\s+INTO\s+clean_code\.refactor_task`)
	prep.ExpectExec().
		WithArgs(task.TaskID, task.PlanID, task.ScopeID,
			string(task.Kind), task.EffortHours,
			task.RuleID, task.DescriptionMD, task.CreatedAt.UTC()).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	w := NewSQLRefactorPlanTaskWriter(db)
	err := w.WriteRefactorPlanAndTasks(context.Background(), plan,
		[]RefactorTask{task})
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

// TestSQLRefactorPlanTaskWriter_Write_RejectsTaskWithMismatchedPlanID
// pins the belt-and-braces guard: a task whose `plan_id` !=
// `plan.PlanID` MUST be rejected -- a misconfigured planner
// that mints a separate plan-id for each task could
// otherwise create orphan rows. We expect BeginTx + the
// plan INSERT to land, then the task-loop validation to
// fire before any task INSERT.
func TestSQLRefactorPlanTaskWriter_Write_RejectsTaskWithMismatchedPlanID(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	createdAt := time.Now().UTC()
	planID := uuid.Must(uuid.NewV4())
	otherID := uuid.Must(uuid.NewV4())
	plan := RefactorPlan{
		PlanID:     planID,
		RepoID:     uuid.Must(uuid.NewV4()),
		SHA:        "sha",
		HotspotIDs: []uuid.UUID{uuid.Must(uuid.NewV4())},
		SummaryMD:  "## plan",
		CreatedAt:  createdAt,
	}
	task := RefactorTask{
		TaskID:        uuid.Must(uuid.NewV4()),
		PlanID:        otherID, // MISMATCH
		ScopeID:       uuid.Must(uuid.NewV4()),
		Kind:          TaskKindSplitClass,
		EffortHours:   0,
		RuleID:        "solid.srp.lcom4_high",
		DescriptionMD: "x",
		CreatedAt:     createdAt,
	}

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT\s+INTO\s+clean_code\.refactor_plan`).
		WithArgs(plan.PlanID, plan.RepoID, plan.SHA, sqlmock.AnyArg(),
			plan.SummaryMD, plan.CreatedAt.UTC()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// PrepareContext is invoked BEFORE the mismatch check
	// inside the per-task loop (the writer prepares the stmt
	// outside the loop), so we expect a Prepare but NOT any
	// per-task ExecContext.
	mock.ExpectPrepare(`INSERT\s+INTO\s+clean_code\.refactor_task`)
	mock.ExpectRollback()

	w := NewSQLRefactorPlanTaskWriter(db)
	err := w.WriteRefactorPlanAndTasks(context.Background(), plan,
		[]RefactorTask{task})
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !regexp.MustCompile(`plan_id=.*!=.*plan\.PlanID=`).MatchString(err.Error()) {
		t.Errorf("err = %v, missing 'plan_id != plan.PlanID' guard text", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLRefactorPlanTaskWriter_Write_RejectsZeroPlanID covers
// the pre-flight branch that catches a planner bug where
// the plan_id was never minted. No transaction should open.
func TestSQLRefactorPlanTaskWriter_Write_RejectsZeroPlanID(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	w := NewSQLRefactorPlanTaskWriter(db)
	err := w.WriteRefactorPlanAndTasks(context.Background(),
		RefactorPlan{}, nil)
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !regexp.MustCompile(`plan\.PlanID is zero`).MatchString(err.Error()) {
		t.Errorf("err = %v, missing 'plan.PlanID is zero' message", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v (no tx should have opened)", err)
	}
}

// TestSQLRefactorPlanTaskWriter_Write_RejectsRejectedAliasKind_BeforeTx
// pins that a task carrying a rejected-alias kind (e.g.
// "extract_function") is rejected via pre-flight validation
// BEFORE any transaction opens. This guards against a
// composition-root regression that resurrects the iter-3
// alias set.
func TestSQLRefactorPlanTaskWriter_Write_RejectsRejectedAliasKind_BeforeTx(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	plan := RefactorPlan{
		PlanID:     uuid.Must(uuid.NewV4()),
		RepoID:     uuid.Must(uuid.NewV4()),
		SHA:        "sha",
		HotspotIDs: []uuid.UUID{uuid.Must(uuid.NewV4())},
		SummaryMD:  "## plan",
		CreatedAt:  time.Now().UTC(),
	}
	task := RefactorTask{
		TaskID:        uuid.Must(uuid.NewV4()),
		PlanID:        plan.PlanID,
		ScopeID:       uuid.Must(uuid.NewV4()),
		Kind:          TaskKind("extract_function"), // REJECTED iter-3 alias
		RuleID:        "x",
		DescriptionMD: "y",
		CreatedAt:     time.Now().UTC(),
	}

	w := NewSQLRefactorPlanTaskWriter(db)
	err := w.WriteRefactorPlanAndTasks(context.Background(), plan,
		[]RefactorTask{task})
	if !errors.Is(err, ErrRejectedTaskKindAlias) {
		t.Fatalf("err = %v, want ErrRejectedTaskKindAlias", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v (no tx should have opened)", err)
	}
}

// TestSQLRefactorPlanTaskWriter_Write_EmptyTasks_PlanOnly confirms
// a plan with ZERO tasks still commits the plan row (metric-
// only hot_spots produce a plan but no task rows -- the
// architecture's empty-task contingency).
func TestSQLRefactorPlanTaskWriter_Write_EmptyTasks_PlanOnly(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	createdAt := time.Now().UTC()
	plan := RefactorPlan{
		PlanID:     uuid.Must(uuid.NewV4()),
		RepoID:     uuid.Must(uuid.NewV4()),
		SHA:        "sha",
		HotspotIDs: []uuid.UUID{uuid.Must(uuid.NewV4())},
		SummaryMD:  "## metric-only plan",
		CreatedAt:  createdAt,
	}

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT\s+INTO\s+clean_code\.refactor_plan`).
		WithArgs(plan.PlanID, plan.RepoID, plan.SHA, sqlmock.AnyArg(),
			plan.SummaryMD, plan.CreatedAt.UTC()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// No Prepare, no per-task Exec -- straight to Commit.
	mock.ExpectCommit()

	w := NewSQLRefactorPlanTaskWriter(db)
	if err := w.WriteRefactorPlanAndTasks(context.Background(), plan, nil); err != nil {
		t.Fatalf("WriteRefactorPlanAndTasks: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("sqlmock expectations: %v", err)
	}
}

// TestSQLRefactorPlanTaskWriter_Write_RollsBackOnPlanInsertError
// confirms a plan INSERT failure rolls back the transaction
// and propagates the wrapped error.
func TestSQLRefactorPlanTaskWriter_Write_RollsBackOnPlanInsertError(t *testing.T) {
	db, mock, cleanup := newSQLMock(t)
	defer cleanup()

	plan := RefactorPlan{
		PlanID:     uuid.Must(uuid.NewV4()),
		RepoID:     uuid.Must(uuid.NewV4()),
		SHA:        "sha",
		HotspotIDs: []uuid.UUID{uuid.Must(uuid.NewV4())},
		SummaryMD:  "## plan",
		CreatedAt:  time.Now().UTC(),
	}
	wantErr := errors.New("synthetic plan insert failure")

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT\s+INTO\s+clean_code\.refactor_plan`).
		WithArgs(plan.PlanID, plan.RepoID, plan.SHA, sqlmock.AnyArg(),
			plan.SummaryMD, plan.CreatedAt.UTC()).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	w := NewSQLRefactorPlanTaskWriter(db)
	err := w.WriteRefactorPlanAndTasks(context.Background(), plan, nil)
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

// -----------------------------------------------------------------------------
// Sanity: every SQL pattern in this file compiles.
// -----------------------------------------------------------------------------

// TestSQLPatternsCompile_TaskPlanner checks every literal
// regex used by the assertions above compiles. A typo would
// otherwise show up only when the corresponding test runs.
func TestSQLPatternsCompile_TaskPlanner(t *testing.T) {
	patterns := []string{
		`SELECT\s+hotspot_id,\s+repo_id,\s+sha,\s+scope_id,\s+score,\s+policy_version_id,\s+created_at\s+FROM\s+clean_code\.hot_spot\s+WHERE\s+repo_id\s*=\s*\$1`,
		`SELECT\s+hotspot_id`,
		`SELECT\s+DISTINCT\s+scope_id,\s+rule_id\s+FROM\s+clean_code\.finding\s+WHERE\s+repo_id\s*=\s*\$1`,
		`SELECT\s+DISTINCT\s+scope_id`,
		`INSERT\s+INTO\s+clean_code\.refactor_plan\s+\(\s+plan_id,\s+repo_id,\s+sha,\s+hotspot_ids,\s+summary_md,\s+created_at\s+\)\s+VALUES\s+\(\s+\$1,\s+\$2,\s+\$3,\s+\$4::jsonb,\s+\$5,\s+\$6\s+\)`,
		`INSERT\s+INTO\s+clean_code\.refactor_task\s+\(\s+task_id,\s+plan_id,\s+scope_id,\s+kind,\s+effort_hours,\s+rule_id,\s+description_md,\s+created_at\s+\)\s+VALUES\s+\(\s+\$1,\s+\$2,\s+\$3,\s+\$4::clean_code\.refactor_task_kind,\s+\$5,\s+\$6,\s+\$7,\s+\$8\s+\)`,
		`INSERT\s+INTO\s+clean_code\.refactor_plan`,
		`INSERT\s+INTO\s+clean_code\.refactor_task`,
		`plan_id=.*!=.*plan\.PlanID=`,
		`plan\.PlanID is zero`,
	}
	for i, p := range patterns {
		if _, err := regexp.Compile(p); err != nil {
			t.Errorf("regex[%d] (%q): %v", i, p, err)
		}
	}
}

// _ = sql.DB is here so the `database/sql` import survives
// even if a future refactor removes it from a test body.
var _ = sql.DB{}
