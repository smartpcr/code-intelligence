package reconciler

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/gofrs/uuid"
)

// helper: pre-known fixed UUIDs so the bind-arg matchers
// don't drift between sub-tests.
func mustUUID(t *testing.T, s string) uuid.UUID {
	t.Helper()
	u, err := uuid.FromString(s)
	if err != nil {
		t.Fatalf("uuid.FromString(%q): %v", s, err)
	}
	return u
}

func TestNewSQLReplayer_RejectsNilDB(t *testing.T) {
	t.Parallel()
	_, err := NewSQLReplayer(SQLReplayerConfig{})
	if err == nil {
		t.Fatal("NewSQLReplayer(nil DB): want error, got nil")
	}
	if !strings.Contains(err.Error(), "DB is nil") {
		t.Fatalf("NewSQLReplayer error: %v; want it to mention nil DB", err)
	}
}

func TestNewSQLReplayer_DefaultSchema(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	defer mock.ExpectationsWereMet()

	r, err := NewSQLReplayer(SQLReplayerConfig{DB: db})
	if err != nil {
		t.Fatalf("NewSQLReplayer: %v", err)
	}
	if got := r.qual("evaluation_run"); got != `"clean_code"."evaluation_run"` {
		t.Fatalf("qual default schema: got %q want %q", got, `"clean_code"."evaluation_run"`)
	}
}

// runFixtureRow returns a canonical EvaluationRunRow.
func runFixtureRow(t *testing.T, caller string) EvaluationRunRow {
	t.Helper()
	return EvaluationRunRow{
		EvaluationRunID: mustUUID(t, "11111111-1111-1111-1111-111111111111"),
		RepoID:          mustUUID(t, "22222222-2222-2222-2222-222222222222"),
		SHA:             "abcdef0123456789abcdef0123456789abcdef01",
		PolicyVersionID: mustUUID(t, "33333333-3333-3333-3333-333333333333"),
		Caller:          caller,
		ScopeID:         nil,
		CreatedAt:       time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

func TestSQLReplayer_ReplayRun_InsertedOnMissingRow(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, err := NewSQLReplayer(SQLReplayerConfig{DB: db})
	if err != nil {
		t.Fatalf("NewSQLReplayer: %v", err)
	}
	row := runFixtureRow(t, "eval_gate")

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
		WithArgs(
			row.EvaluationRunID.String(),
			row.RepoID.String(),
			row.SHA,
			row.PolicyVersionID.String(),
			"eval_gate",
			nil,
			row.CreatedAt.UTC(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	outcome, err := r.ReplayRun(context.Background(), row)
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}
	if outcome != OutcomeInserted {
		t.Fatalf("outcome: got %v want %v", outcome, OutcomeInserted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSQLReplayer_ReplayRun_SkippedExistingOnConflict(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, _ := NewSQLReplayer(SQLReplayerConfig{DB: db})
	row := runFixtureRow(t, "batch_refresh")

	mock.ExpectExec(regexp.QuoteMeta(`ON CONFLICT (evaluation_run_id) DO NOTHING`)).
		WillReturnResult(sqlmock.NewResult(0, 0)) // RowsAffected = 0 -> existing row

	outcome, err := r.ReplayRun(context.Background(), row)
	if err != nil {
		t.Fatalf("ReplayRun: %v", err)
	}
	if outcome != OutcomeSkippedExisting {
		t.Fatalf("outcome: got %v want %v", outcome, OutcomeSkippedExisting)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSQLReplayer_ReplayRun_PreservesCallerVerbatim(t *testing.T) {
	t.Parallel()
	// CRITICAL BRIEF INVARIANT: caller must be bound to the
	// SQL parameter EXACTLY as recorded in the frame, with
	// no transformation.
	for _, caller := range []string{"eval_gate", "batch_refresh"} {
		caller := caller
		t.Run(caller, func(t *testing.T) {
			t.Parallel()
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock.New: %v", err)
			}
			defer db.Close()

			r, _ := NewSQLReplayer(SQLReplayerConfig{DB: db})
			row := runFixtureRow(t, caller)
			scope := mustUUID(t, "44444444-4444-4444-4444-444444444444")
			row.ScopeID = &scope

			mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
				WithArgs(
					row.EvaluationRunID.String(),
					row.RepoID.String(),
					row.SHA,
					row.PolicyVersionID.String(),
					caller, // verbatim
					scope.String(),
					row.CreatedAt.UTC(),
				).
				WillReturnResult(sqlmock.NewResult(0, 1))

			if _, err := r.ReplayRun(context.Background(), row); err != nil {
				t.Fatalf("ReplayRun: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("unmet expectations: %v", err)
			}
		})
	}
}

func TestSQLReplayer_ReplayRun_RejectsZeroFields(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()
	defer mock.ExpectationsWereMet() // no exec expected

	r, _ := NewSQLReplayer(SQLReplayerConfig{DB: db})
	tests := []struct {
		name string
		mut  func(row *EvaluationRunRow)
	}{
		{"zero EvaluationRunID", func(r *EvaluationRunRow) { r.EvaluationRunID = uuid.Nil }},
		{"zero RepoID", func(r *EvaluationRunRow) { r.RepoID = uuid.Nil }},
		{"empty SHA", func(r *EvaluationRunRow) { r.SHA = "" }},
		{"zero PolicyVersionID", func(r *EvaluationRunRow) { r.PolicyVersionID = uuid.Nil }},
		{"empty Caller", func(r *EvaluationRunRow) { r.Caller = "" }},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			row := runFixtureRow(t, "eval_gate")
			tc.mut(&row)
			if _, err := r.ReplayRun(context.Background(), row); err == nil {
				t.Fatalf("ReplayRun(%s): want error, got nil", tc.name)
			}
		})
	}
}

func verdictFixtureRow(t *testing.T, reason string) EvaluationVerdictRow {
	t.Helper()
	return EvaluationVerdictRow{
		VerdictID:       mustUUID(t, "55555555-5555-5555-5555-555555555555"),
		EvaluationRunID: mustUUID(t, "11111111-1111-1111-1111-111111111111"),
		Verdict:         "pass",
		Degraded:        reason != "",
		DegradedReason:  reason,
		CreatedAt:       time.Date(2025, 1, 2, 3, 4, 6, 0, time.UTC),
	}
}

func TestSQLReplayer_ReplayVerdict_InsertedOnMissingRow(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, _ := NewSQLReplayer(SQLReplayerConfig{DB: db})
	row := verdictFixtureRow(t, "")

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_verdict"`)).
		WithArgs(
			row.VerdictID.String(),
			row.EvaluationRunID.String(),
			"pass",
			false,
			"", // empty string -> NULLIF($5,'') in SQL
			row.CreatedAt.UTC(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	outcome, err := r.ReplayVerdict(context.Background(), row)
	if err != nil {
		t.Fatalf("ReplayVerdict: %v", err)
	}
	if outcome != OutcomeInserted {
		t.Fatalf("outcome: got %v want %v", outcome, OutcomeInserted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSQLReplayer_ReplayVerdict_SkippedExistingOnConflict(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, _ := NewSQLReplayer(SQLReplayerConfig{DB: db})
	row := verdictFixtureRow(t, "policy_overrides_unavailable")

	mock.ExpectExec(regexp.QuoteMeta(`ON CONFLICT (verdict_id) DO NOTHING`)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	outcome, err := r.ReplayVerdict(context.Background(), row)
	if err != nil {
		t.Fatalf("ReplayVerdict: %v", err)
	}
	if outcome != OutcomeSkippedExisting {
		t.Fatalf("outcome: got %v want %v", outcome, OutcomeSkippedExisting)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func findingFixtureRow(t *testing.T, sampleIDs []uuid.UUID) FindingRow {
	t.Helper()
	return FindingRow{
		FindingID:       mustUUID(t, "66666666-6666-6666-6666-666666666666"),
		EvaluationRunID: mustUUID(t, "11111111-1111-1111-1111-111111111111"),
		RepoID:          mustUUID(t, "22222222-2222-2222-2222-222222222222"),
		SHA:             "abcdef0123456789abcdef0123456789abcdef01",
		ScopeID:         mustUUID(t, "77777777-7777-7777-7777-777777777777"),
		RuleID:          "complexity.cyclomatic",
		RuleVersion:     3,
		PolicyVersionID: mustUUID(t, "33333333-3333-3333-3333-333333333333"),
		MetricSampleIDs: sampleIDs,
		Severity:        "block",
		Delta:           "regressed",
		ExplanationMD:   "Cyclomatic complexity regressed by 5 points.",
		CreatedAt:       time.Date(2025, 1, 2, 3, 4, 7, 0, time.UTC),
	}
}

func TestSQLReplayer_ReplayFinding_InsertedOnMissingRow(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, _ := NewSQLReplayer(SQLReplayerConfig{DB: db})
	sample := mustUUID(t, "88888888-8888-8888-8888-888888888888")
	row := findingFixtureRow(t, []uuid.UUID{sample})

	wantSamplesJSON := `["88888888-8888-8888-8888-888888888888"]`
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."finding"`)).
		WithArgs(
			row.FindingID.String(),
			row.EvaluationRunID.String(),
			row.RepoID.String(),
			row.SHA,
			row.ScopeID.String(),
			row.RuleID,
			row.RuleVersion,
			row.PolicyVersionID.String(),
			wantSamplesJSON,
			row.Severity,
			row.Delta,
			row.ExplanationMD,
			row.CreatedAt.UTC(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	outcome, err := r.ReplayFinding(context.Background(), row)
	if err != nil {
		t.Fatalf("ReplayFinding: %v", err)
	}
	if outcome != OutcomeInserted {
		t.Fatalf("outcome: got %v want %v", outcome, OutcomeInserted)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSQLReplayer_ReplayFinding_EmptySamplesRendersEmptyJSONArray(t *testing.T) {
	t.Parallel()
	// Stage 9.1 writer guarantees metric_sample_ids is
	// ALWAYS a JSON array (never null). The reconciler
	// must round-trip an empty slice as `[]`, not null,
	// so the `$9::jsonb` cast succeeds.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, _ := NewSQLReplayer(SQLReplayerConfig{DB: db})
	row := findingFixtureRow(t, nil)

	mock.ExpectExec(regexp.QuoteMeta(`ON CONFLICT (finding_id) DO NOTHING`)).
		WithArgs(
			row.FindingID.String(),
			row.EvaluationRunID.String(),
			row.RepoID.String(),
			row.SHA,
			row.ScopeID.String(),
			row.RuleID,
			row.RuleVersion,
			row.PolicyVersionID.String(),
			`[]`, // empty array, NOT null
			row.Severity,
			row.Delta,
			row.ExplanationMD,
			row.CreatedAt.UTC(),
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := r.ReplayFinding(context.Background(), row); err != nil {
		t.Fatalf("ReplayFinding: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSQLReplayer_RowsAffectedAbove1IsLoudError(t *testing.T) {
	t.Parallel()
	// Defensive guard: a single-row INSERT under ON
	// CONFLICT DO NOTHING can only return 0 or 1 affected
	// rows. Anything else MUST surface loudly rather than
	// be silently classified.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, _ := NewSQLReplayer(SQLReplayerConfig{DB: db})
	row := runFixtureRow(t, "eval_gate")

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
		WillReturnResult(sqlmock.NewResult(0, 2)) // forbidden

	_, err = r.ReplayRun(context.Background(), row)
	if err == nil {
		t.Fatal("ReplayRun with RowsAffected=2: want loud error, got nil")
	}
	if !strings.Contains(err.Error(), "RowsAffected=2") {
		t.Fatalf("ReplayRun error: %v; want it to mention RowsAffected=2", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestSQLReplayer_ExecErrorPropagates(t *testing.T) {
	t.Parallel()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	defer db.Close()

	r, _ := NewSQLReplayer(SQLReplayerConfig{DB: db})
	row := runFixtureRow(t, "eval_gate")

	want := errors.New("connection refused")
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO "clean_code"."evaluation_run"`)).
		WillReturnError(want)

	_, err = r.ReplayRun(context.Background(), row)
	if err == nil {
		t.Fatal("ReplayRun: want exec error to propagate")
	}
	if !errors.Is(err, want) {
		t.Fatalf("ReplayRun error: %v; want it to wrap %v", err, want)
	}
}
