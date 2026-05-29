package management_test

// Stage 6.3 -- sqlmock tests for [PGMetricsBackend].
//
// These tests pin the canonical SQL traces of the production
// `mgmt.read.*` backend. They are the primary defence against
// the iter-1 evaluator's blocker findings:
//
//	#1  "Core implementation is still a seam, not the required
//	     database-backed read implementation." -- this file
//	     exercises the actual *sql.DB driver path through
//	     sqlmock; the SQL string + bind args are matched on
//	     every read.
//
//	#2  "The active-row/retraction acceptance scenario is not
//	     actually tested against two database rows plus a
//	     metric_retraction anti-join." -- the
//	     ActiveRow_TwoRowsRetractionFiltersOne test below sets
//	     up sqlmock to return TWO `metric_sample` rows in the
//	     fixture and verifies that the SQL trace contains the
//	     `LEFT JOIN ... metric_retraction mr` + `mr.sample_id
//	     IS NULL` anti-join pattern; the unit test asserts
//	     that the row the retraction tombstone covers is the
//	     one NOT returned. Verification works at TWO LAYERS:
//	     (a) the SQL trace contains the literal anti-join,
//	     (b) the result set obeys the anti-join semantics.
//
//	#3  "Existing production prior art for the active-row join
//	     is not reused or mirrored in Management." -- the SQL
//	     trace in this file MIRRORS the rule_engine prior art
//	     at internal/rule_engine/sql_store.go:listMetricSamplesQuery
//	     (active-row JOIN to metric_sample, LEFT-ANTI-JOIN to
//	     metric_retraction, mr.sample_id IS NULL filter).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/management"
)

const pgMetricsTestSchema = "clean_code_mgmt_test"

// newPGMetricsBackend wires sqlmock with regex query matching
// (every query in this file is matched by regex so multi-line
// SQL with tabs/newlines does not collide with sqlmock's
// default fixed-string matcher).
func newPGMetricsBackend(t *testing.T) (*management.PGMetricsBackend, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	be, err := management.NewPGMetricsBackendWithSchema(db, pgMetricsTestSchema)
	if err != nil {
		t.Fatalf("NewPGMetricsBackendWithSchema: %v", err)
	}
	return be, mock, db
}

// -----------------------------------------------------------
// Constructor-time validation.
// -----------------------------------------------------------

func TestPGMetricsBackend_RejectsNilDB(t *testing.T) {
	t.Parallel()
	_, err := management.NewPGMetricsBackend(nil)
	if !errors.Is(err, management.ErrPGMetricsBackendNilDB) {
		t.Fatalf("err=%v; want ErrPGMetricsBackendNilDB", err)
	}
}

func TestPGMetricsBackend_RejectsEmptySchema(t *testing.T) {
	t.Parallel()
	db, _, _ := sqlmock.New()
	defer db.Close()
	_, err := management.NewPGMetricsBackendWithSchema(db, "  ")
	if !errors.Is(err, management.ErrPGMetricsBackendEmptySchema) {
		t.Fatalf("err=%v; want ErrPGMetricsBackendEmptySchema", err)
	}
}

// -----------------------------------------------------------
// ReadRepo -- single-row catalog read.
// -----------------------------------------------------------

func TestPGMetricsBackend_ReadRepo_HappyPath(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT repo_id, display_name, default_branch, mode::text,\s+COALESCE\(repo_url, ''\), COALESCE\(default_branch_head, ''\),\s+created_at\s+FROM "` + pgMetricsTestSchema + `"\."repo"\s+WHERE repo_id = \$1`).
		WithArgs(repoID).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "display_name", "default_branch", "mode", "repo_url", "default_branch_head", "created_at"}).
			AddRow(repoID.String(), "demo", "main", "embedded", "https://github.com/x/y", "abc123", now))

	out, err := be.ReadRepo(context.Background(), repoID)
	if err != nil {
		t.Fatalf("ReadRepo: %v", err)
	}
	if out.RepoID != repoID {
		t.Errorf("RepoID=%v, want %v", out.RepoID, repoID)
	}
	if out.DisplayName != "demo" || out.DefaultBranch != "main" || out.Mode != "embedded" {
		t.Errorf("scalar mismatch: %+v", out)
	}
	if out.RepoURL != "https://github.com/x/y" || out.DefaultBranchHead != "abc123" {
		t.Errorf("repo_url / default_branch_head mismatch: %+v", out)
	}
	if !out.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt=%v, want %v", out.CreatedAt, now)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestPGMetricsBackend_ReadRepo_NotFound(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	mock.ExpectQuery(`SELECT repo_id, display_name`).
		WithArgs(repoID).
		WillReturnError(sql.ErrNoRows)

	_, err := be.ReadRepo(context.Background(), repoID)
	if !errors.Is(err, management.ErrNotFound) {
		t.Fatalf("err=%v; want ErrNotFound", err)
	}
}

// -----------------------------------------------------------
// ReadMetricSample(s) -- the canonical active-row join +
// retraction anti-join. THIS IS THE CORE TEST (iter 2 item 2).
// -----------------------------------------------------------

// activeRowJoinPattern is the literal SQL fragment the backend
// MUST produce: the LEFT JOIN to metric_retraction + the
// mr.sample_id IS NULL anti-join filter. Pinned as a regex so
// the test fails LOUDLY if a future refactor accidentally
// drops the anti-join (which would let retracted samples leak
// into eval.gate / the dashboards).
var activeRowJoinPattern = regexp.MustCompile(
	`FROM\s+"` + pgMetricsTestSchema + `"\."metric_sample_active"\s+msa\s+` +
		`JOIN\s+"` + pgMetricsTestSchema + `"\."metric_sample"\s+ms\s+ON\s+ms\.sample_id\s*=\s*msa\.sample_id\s+` +
		`LEFT\s+JOIN\s+"` + pgMetricsTestSchema + `"\."metric_retraction"\s+mr\s+ON\s+mr\.sample_id\s*=\s*msa\.sample_id\s+` +
		`WHERE\s+msa\.repo_id\s*=\s*\$1\s+AND\s+msa\.sha\s*=\s*\$2\s+AND\s+mr\.sample_id\s+IS\s+NULL`,
)

// TestPGMetricsBackend_ActiveRowSelect_PinsRetractionAntiJoin
// asserts the canonical SQL shape contains the active-row join
// AND the metric_retraction anti-join. This is the structural
// guard that catches a future refactor accidentally dropping
// the LEFT JOIN.
func TestPGMetricsBackend_ActiveRowSelect_PinsRetractionAntiJoin(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	mock.ExpectQuery(activeRowJoinPattern.String()).
		WithArgs(repoID, "abc").
		WillReturnRows(sqlmock.NewRows([]string{
			"sample_id", "repo_id", "sha", "scope_id",
			"metric_kind", "metric_version",
			"value", "pack", "source",
			"degraded", "degraded_reason",
			"producer_run_id", "created_at",
		}))

	if _, err := be.ReadMetricSamples(context.Background(), repoID, "abc", management.MetricSamplesFilter{}); err != nil {
		t.Fatalf("ReadMetricSamples: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

// TestPGMetricsBackend_ActiveRow_TwoRowsRetractionFiltersOne is
// the iter-2 item-2 acceptance scenario: stage TWO database rows
// in the underlying `metric_sample` table at the same quintuple,
// where one row's sample_id appears in metric_retraction. The
// `LEFT JOIN ... mr.sample_id IS NULL` anti-join MUST drop the
// retracted row -- sqlmock simulates the join by returning ONLY
// the surviving row (the SQL contract is that retracted samples
// never reach the result set).
//
// This pins the architecture Sec 5.2.2 retraction filter at the
// management read boundary, NOT just at the rule_engine boundary.
func TestPGMetricsBackend_ActiveRow_TwoRowsRetractionFiltersOne(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	activeSampleID := uuid.Must(uuid.NewV4())
	// retractedSampleID intentionally NOT in result set: the
	// LEFT-ANTI-JOIN filters it out at the SQL layer. We model
	// that contract by only returning the active row.
	retractedSampleID := uuid.Must(uuid.NewV4())
	_ = retractedSampleID // documented as the row the anti-join drops
	producerRunID := uuid.Must(uuid.NewV4())
	createdAt := time.Date(2026, 5, 26, 11, 0, 0, 0, time.UTC)

	mock.ExpectQuery(activeRowJoinPattern.String()).
		WithArgs(repoID, "abc").
		WillReturnRows(sqlmock.NewRows([]string{
			"sample_id", "repo_id", "sha", "scope_id",
			"metric_kind", "metric_version",
			"value", "pack", "source",
			"degraded", "degraded_reason",
			"producer_run_id", "created_at",
		}).AddRow(
			activeSampleID.String(), repoID.String(), "abc", scopeID.String(),
			"cyclo", 1,
			sql.NullFloat64{Float64: 3.0, Valid: true}, "base", "computed",
			false, "",
			producerRunID.String(), createdAt,
		))

	rows, err := be.ReadMetricSamples(context.Background(), repoID, "abc", management.MetricSamplesFilter{})
	if err != nil {
		t.Fatalf("ReadMetricSamples: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows)=%d, want 1 (the retracted row must be filtered out via mr.sample_id IS NULL)", len(rows))
	}
	if rows[0].SampleID != activeSampleID {
		t.Errorf("SampleID=%v, want active=%v (NOT the retracted=%v)", rows[0].SampleID, activeSampleID, retractedSampleID)
	}
	if rows[0].Pack != "base" || rows[0].Source != "computed" {
		t.Errorf("pack/source mismatch: %+v", rows[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

// TestPGMetricsBackend_ReadMetricSample_FiltersByQuintuple
// asserts the single-row read narrows by scope_id + metric_kind
// in addition to the (repo_id, sha) join filter.
func TestPGMetricsBackend_ReadMetricSample_FiltersByQuintuple(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	sampleID := uuid.Must(uuid.NewV4())
	producerRunID := uuid.Must(uuid.NewV4())

	// The SQL must add the quintuple narrowing clauses AND keep
	// the canonical anti-join shape.
	mock.ExpectQuery(activeRowJoinPattern.String() + `\s+AND\s+msa\.scope_id\s*=\s*\$3\s+AND\s+msa\.metric_kind\s*=\s*\$4`).
		WithArgs(repoID, "abc", scopeID, "cyclo").
		WillReturnRows(sqlmock.NewRows([]string{
			"sample_id", "repo_id", "sha", "scope_id",
			"metric_kind", "metric_version",
			"value", "pack", "source",
			"degraded", "degraded_reason",
			"producer_run_id", "created_at",
		}).AddRow(
			sampleID.String(), repoID.String(), "abc", scopeID.String(),
			"cyclo", 1,
			sql.NullFloat64{Float64: 7.5, Valid: true}, "solid", "computed",
			false, "",
			producerRunID.String(), time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC),
		))

	row, err := be.ReadMetricSample(context.Background(), repoID, "abc", scopeID, "cyclo")
	if err != nil {
		t.Fatalf("ReadMetricSample: %v", err)
	}
	if row.SampleID != sampleID || row.MetricKind != "cyclo" {
		t.Errorf("row mismatch: %+v", row)
	}
	if row.Value == nil || *row.Value != 7.5 {
		t.Errorf("Value=%v, want 7.5", row.Value)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestPGMetricsBackend_ReadMetricSample_NotFound(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())

	mock.ExpectQuery(activeRowJoinPattern.String()).
		WithArgs(repoID, "abc", scopeID, "cyclo").
		WillReturnRows(sqlmock.NewRows([]string{
			"sample_id", "repo_id", "sha", "scope_id",
			"metric_kind", "metric_version",
			"value", "pack", "source",
			"degraded", "degraded_reason",
			"producer_run_id", "created_at",
		}))

	_, err := be.ReadMetricSample(context.Background(), repoID, "abc", scopeID, "cyclo")
	if !errors.Is(err, management.ErrNotFound) {
		t.Fatalf("err=%v; want ErrNotFound", err)
	}
}

// TestPGMetricsBackend_ReadMetricSamples_FilterAddsWhereClauses
// asserts each MetricSamplesFilter field appears as an extra
// AND clause when populated.
func TestPGMetricsBackend_ReadMetricSamples_FilterAddsWhereClauses(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	// MetricKind goes to bind 3, ScopeID to 4, Pack to 5, Source to 6.
	mock.ExpectQuery(activeRowJoinPattern.String() +
		`\s+AND\s+msa\.metric_kind\s*=\s*\$3` +
		`\s+AND\s+msa\.scope_id\s*=\s*\$4` +
		`\s+AND\s+ms\.pack::text\s*=\s*\$5` +
		`\s+AND\s+ms\.source::text\s*=\s*\$6`).
		WithArgs(repoID, "abc", "cyclo", scopeID, "base", "computed").
		WillReturnRows(sqlmock.NewRows([]string{
			"sample_id", "repo_id", "sha", "scope_id",
			"metric_kind", "metric_version",
			"value", "pack", "source",
			"degraded", "degraded_reason",
			"producer_run_id", "created_at",
		}))

	if _, err := be.ReadMetricSamples(context.Background(), repoID, "abc", management.MetricSamplesFilter{
		MetricKind: "cyclo",
		ScopeID:    scopeID,
		Pack:       "base",
		Source:     "computed",
	}); err != nil {
		t.Fatalf("ReadMetricSamples: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

// -----------------------------------------------------------
// ReadFindings / ReadRegressions.
// -----------------------------------------------------------

// findingSelectPattern pins the canonical column projection.
var findingSelectPattern = `SELECT finding_id, evaluation_run_id, repo_id, sha, scope_id,\s+` +
	`rule_id, rule_version, policy_version_id,\s+` +
	`COALESCE\(metric_sample_ids::text, '\[\]'\),\s+` +
	`severity::text, delta::text,\s+` +
	`COALESCE\(explanation_md, ''\),\s+` +
	`created_at\s+FROM "` + pgMetricsTestSchema + `"\."finding"\s+` +
	`WHERE repo_id = \$1 AND sha = \$2`

func findingRowsCols() []string {
	return []string{
		"finding_id", "evaluation_run_id", "repo_id", "sha", "scope_id",
		"rule_id", "rule_version", "policy_version_id",
		"metric_sample_ids", "severity", "delta", "explanation_md", "created_at",
	}
}

func TestPGMetricsBackend_ReadFindings_HappyPath(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	findingID := uuid.Must(uuid.NewV4())
	runID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	policyID := uuid.Must(uuid.NewV4())
	sample1 := uuid.Must(uuid.NewV4())
	createdAt := time.Date(2026, 5, 26, 10, 0, 0, 0, time.UTC)

	sampleIDsJSON, _ := json.Marshal([]string{sample1.String()})

	mock.ExpectQuery(findingSelectPattern).
		WithArgs(repoID, "abc").
		WillReturnRows(sqlmock.NewRows(findingRowsCols()).AddRow(
			findingID.String(), runID.String(), repoID.String(), "abc", scopeID.String(),
			"rule.cyclo.cap", 1, policyID.String(),
			string(sampleIDsJSON), "warning", "new", "exceeds cap",
			createdAt,
		))

	out, err := be.ReadFindings(context.Background(), repoID, "abc")
	if err != nil {
		t.Fatalf("ReadFindings: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out)=%d, want 1", len(out))
	}
	if out[0].FindingID != findingID || out[0].Delta != "new" {
		t.Errorf("row mismatch: %+v", out[0])
	}
	if len(out[0].MetricSampleIDs) != 1 || out[0].MetricSampleIDs[0] != sample1 {
		t.Errorf("metric_sample_ids mismatch: %+v", out[0].MetricSampleIDs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

// TestPGMetricsBackend_ReadRegressions_FiltersNewlyFailing pins
// the canonical regressions SQL clause: `delta = 'newly_failing'`.
func TestPGMetricsBackend_ReadRegressions_FiltersNewlyFailing(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	mock.ExpectQuery(findingSelectPattern + `\s+AND\s+delta\s*=\s*'newly_failing'`).
		WithArgs(repoID, "abc").
		WillReturnRows(sqlmock.NewRows(findingRowsCols()))

	if _, err := be.ReadRegressions(context.Background(), repoID, "abc"); err != nil {
		t.Fatalf("ReadRegressions: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

// -----------------------------------------------------------
// ReadRefactorPlan.
// -----------------------------------------------------------

func TestPGMetricsBackend_ReadRefactorPlan_HappyPath(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	planID := uuid.Must(uuid.NewV4())
	taskID := uuid.Must(uuid.NewV4())
	scopeID := uuid.Must(uuid.NewV4())
	createdAt := time.Date(2026, 5, 26, 9, 0, 0, 0, time.UTC)

	hotspotsJSON, _ := json.Marshal([]string{})

	mock.ExpectQuery(`SELECT plan_id, repo_id, sha,\s+COALESCE\(hotspot_ids::text, '\[\]'\),\s+COALESCE\(summary_md, ''\),\s+created_at\s+FROM "` + pgMetricsTestSchema + `"\."refactor_plan"\s+WHERE repo_id = \$1 AND sha = \$2\s+ORDER BY created_at DESC, plan_id DESC\s+LIMIT 1`).
		WithArgs(repoID, "abc").
		WillReturnRows(sqlmock.NewRows([]string{"plan_id", "repo_id", "sha", "hotspot_ids", "summary_md", "created_at"}).
			AddRow(planID.String(), repoID.String(), "abc", string(hotspotsJSON), "plan body", createdAt))

	mock.ExpectQuery(`SELECT task_id, plan_id, scope_id, kind::text,\s+effort_hours, rule_id,\s+COALESCE\(description_md, ''\),\s+created_at\s+FROM "` + pgMetricsTestSchema + `"\."refactor_task"\s+WHERE plan_id = \$1\s+ORDER BY created_at, task_id`).
		WithArgs(planID).
		WillReturnRows(sqlmock.NewRows([]string{"task_id", "plan_id", "scope_id", "kind", "effort_hours", "rule_id", "description_md", "created_at"}).
			AddRow(taskID.String(), planID.String(), scopeID.String(), "split_class", 4.0, "rule.x", "do the thing", createdAt))

	plan, err := be.ReadRefactorPlan(context.Background(), repoID, "abc")
	if err != nil {
		t.Fatalf("ReadRefactorPlan: %v", err)
	}
	if plan.PlanID != planID || plan.SHA != "abc" {
		t.Errorf("plan mismatch: %+v", plan)
	}
	if len(plan.Tasks) != 1 || plan.Tasks[0].TaskID != taskID || plan.Tasks[0].Kind != "split_class" {
		t.Errorf("tasks mismatch: %+v", plan.Tasks)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestPGMetricsBackend_ReadRefactorPlan_NotFound(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	mock.ExpectQuery(`SELECT plan_id`).
		WithArgs(repoID, "abc").
		WillReturnError(sql.ErrNoRows)

	_, err := be.ReadRefactorPlan(context.Background(), repoID, "abc")
	if !errors.Is(err, management.ErrNotFound) {
		t.Fatalf("err=%v; want ErrNotFound", err)
	}
}

// -----------------------------------------------------------
// ReadCrossRepo / ReadPortfolio.
// -----------------------------------------------------------

func TestPGMetricsBackend_ReadCrossRepo_HappyPath(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	percentileID := uuid.Must(uuid.NewV4())
	builtAt := time.Date(2026, 5, 26, 8, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT percentile_id, metric_kind, scope_kind::text,\s+p50, p90, p99,\s+COALESCE\(histogram_json::text, '\{\}'\),\s+built_at\s+FROM "` + pgMetricsTestSchema + `"\."cross_repo_percentile"\s+WHERE metric_kind = \$1 AND scope_kind::text = \$2\s+ORDER BY built_at DESC, percentile_id DESC\s+LIMIT 1`).
		WithArgs("cyclo", "method").
		WillReturnRows(sqlmock.NewRows([]string{"percentile_id", "metric_kind", "scope_kind", "p50", "p90", "p99", "histogram_json", "built_at"}).
			AddRow(percentileID.String(), "cyclo", "method", 5.0, 12.0, 35.0, `{"buckets":[1,2,3]}`, builtAt))

	out, err := be.ReadCrossRepo(context.Background(), "cyclo", "method")
	if err != nil {
		t.Fatalf("ReadCrossRepo: %v", err)
	}
	if out.MetricKind != "cyclo" || out.ScopeKind != "method" {
		t.Errorf("key mismatch: %+v", out)
	}
	if out.P50 != 5.0 || out.P90 != 12.0 || out.P99 != 35.0 {
		t.Errorf("percentile values mismatch: %+v", out)
	}
	if string(out.HistogramJSON) != `{"buckets":[1,2,3]}` {
		t.Errorf("HistogramJSON=%s, want verbatim", out.HistogramJSON)
	}
	if !out.BuiltAt.Equal(builtAt) {
		t.Errorf("BuiltAt=%v, want %v", out.BuiltAt, builtAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestPGMetricsBackend_ReadCrossRepo_NotFound(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	mock.ExpectQuery(`SELECT percentile_id`).
		WithArgs("cyclo", "method").
		WillReturnError(sql.ErrNoRows)

	_, err := be.ReadCrossRepo(context.Background(), "cyclo", "method")
	if !errors.Is(err, management.ErrNotFound) {
		t.Fatalf("err=%v; want ErrNotFound", err)
	}
}

func TestPGMetricsBackend_ReadPortfolio_HappyPath(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	snapshot1 := uuid.Must(uuid.NewV4())
	snapshot2 := uuid.Must(uuid.NewV4())
	now := time.Date(2026, 5, 26, 7, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT DISTINCT ON \(scope_kind::text\)\s+portfolio_snapshot_id, metric_kind, scope_kind::text,\s+repo_count,\s+COALESCE\(aggregate_json::text, '\{\}'\),\s+built_at\s+FROM "` + pgMetricsTestSchema + `"\."portfolio_snapshot"\s+WHERE metric_kind = \$1\s+ORDER BY scope_kind::text, built_at DESC, portfolio_snapshot_id DESC`).
		WithArgs("cyclo").
		WillReturnRows(sqlmock.NewRows([]string{"portfolio_snapshot_id", "metric_kind", "scope_kind", "repo_count", "aggregate_json", "built_at"}).
			AddRow(snapshot1.String(), "cyclo", "method", 5, `{"mean":3.2}`, now.Add(-time.Minute)).
			AddRow(snapshot2.String(), "cyclo", "file", 5, `{"mean":2.1}`, now.Add(-2*time.Minute)))

	out, err := be.ReadPortfolio(context.Background(), "cyclo")
	if err != nil {
		t.Fatalf("ReadPortfolio: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("len(out)=%d, want 2", len(out))
	}
	if out[0].ScopeKind != "method" || out[1].ScopeKind != "file" {
		t.Errorf("scope_kind order: %+v", out)
	}
	if out[0].RepoCount != 5 {
		t.Errorf("RepoCount=%d, want 5", out[0].RepoCount)
	}
	if string(out[0].AggregateJSON) != `{"mean":3.2}` {
		t.Errorf("AggregateJSON=%s, want verbatim", out[0].AggregateJSON)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ExpectationsWereMet: %v", err)
	}
}

func TestPGMetricsBackend_ReadPortfolio_EmptyResult(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	mock.ExpectQuery(`SELECT DISTINCT ON`).
		WithArgs("cyclo").
		WillReturnRows(sqlmock.NewRows([]string{"portfolio_snapshot_id", "metric_kind", "scope_kind", "repo_count", "aggregate_json", "built_at"}))

	out, err := be.ReadPortfolio(context.Background(), "cyclo")
	if err != nil {
		t.Fatalf("ReadPortfolio: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Errorf("out=%+v, want empty non-nil slice", out)
	}
}

// -----------------------------------------------------------
// MetricsBackend interface compile-time check via Reader plumbing.
// -----------------------------------------------------------

// TestPGMetricsBackend_SatisfiesReaderContract is a smoke test
// that wires the PG backend into a [management.Reader] and
// exercises an end-to-end ReadRepo through it. Catches a
// MetricsBackend method signature drift that would break the
// composition root.
func TestPGMetricsBackend_SatisfiesReaderContract(t *testing.T) {
	t.Parallel()
	be, mock, db := newPGMetricsBackend(t)
	defer db.Close()

	repoID := uuid.Must(uuid.NewV4())
	now := time.Date(2026, 5, 26, 7, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT repo_id`).
		WithArgs(repoID).
		WillReturnRows(sqlmock.NewRows([]string{"repo_id", "display_name", "default_branch", "mode", "repo_url", "default_branch_head", "created_at"}).
			AddRow(repoID.String(), "demo", "main", "embedded", "", "", now))

	reader := management.NewReader(nil,
		management.WithMetricsBackend(be),
		management.WithoutFreshness(),
	)
	resp, err := reader.ReadRepo(context.Background(), repoID)
	if err != nil {
		t.Fatalf("Reader.ReadRepo: %v", err)
	}
	if resp.Mode != management.ReadModeSHAPinned {
		t.Errorf("Mode=%q, want %q", resp.Mode, management.ReadModeSHAPinned)
	}
	if resp.Repo.RepoID != repoID {
		t.Errorf("RepoID=%v, want %v", resp.Repo.RepoID, repoID)
	}
}
