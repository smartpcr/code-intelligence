package evaluator

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"
)

// openTestDB returns a `*sql.DB` handle without actually
// connecting to PostgreSQL. The `lib/pq` driver defers
// connection until the first query, so a bogus DSN is
// fine for tests that only exercise in-memory validation
// guards (no SQL round-trip).
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", "postgres://localhost:1/none?sslmode=disable")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestSQLDegradedRunStore_New_RejectsNilDB pins the
// constructor's nil-DB rejection. A composition root that
// forgets to set DB is a deployment bug; the wiring helper
// MUST refuse it loudly.
func TestSQLDegradedRunStore_New_RejectsNilDB(t *testing.T) {
	_, err := NewSQLDegradedRunStore(SQLDegradedRunStoreConfig{})
	if err == nil {
		t.Fatal("NewSQLDegradedRunStore: want error, got nil")
	}
}

// TestSQLDegradedRunStore_AppendDegradedRun_ValidationContract
// pins the eight zero-uuid + empty-field invariants on
// AppendDegradedRun. We do NOT exercise the SQL round-trip
// here (that requires a live PG); these guards are pure
// in-memory validation.
func TestSQLDegradedRunStore_AppendDegradedRun_ValidationContract(t *testing.T) {
	t.Parallel()
	s, err := NewSQLDegradedRunStore(SQLDegradedRunStoreConfig{DB: openTestDB(t)})
	if err != nil {
		t.Fatalf("NewSQLDegradedRunStore: %v", err)
	}
	runID := uuid.Must(uuid.NewV4())
	verdictID := uuid.Must(uuid.NewV4())
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	cases := []struct {
		name    string
		run     DegradedRun
		verdict DegradedVerdict
		want    string
	}{
		{
			name: "zero run id",
			run:  DegradedRun{EvaluationRunID: uuid.Nil, RepoID: repoID, SHA: "sha", PolicyVersionID: pvID},
			verdict: DegradedVerdict{
				VerdictID: verdictID, EvaluationRunID: runID,
				Verdict: "warn", DegradedReason: "samples_pending",
			},
			want: "run.EvaluationRunID is the zero uuid",
		},
		{
			name: "zero repo",
			run:  DegradedRun{EvaluationRunID: runID, RepoID: uuid.Nil, SHA: "sha", PolicyVersionID: pvID},
			verdict: DegradedVerdict{
				VerdictID: verdictID, EvaluationRunID: runID,
				Verdict: "warn", DegradedReason: "samples_pending",
			},
			want: "run.RepoID is the zero uuid",
		},
		{
			name: "empty sha",
			run:  DegradedRun{EvaluationRunID: runID, RepoID: repoID, SHA: "", PolicyVersionID: pvID},
			verdict: DegradedVerdict{
				VerdictID: verdictID, EvaluationRunID: runID,
				Verdict: "warn", DegradedReason: "samples_pending",
			},
			want: "run.SHA is empty",
		},
		{
			name: "zero policy",
			run:  DegradedRun{EvaluationRunID: runID, RepoID: repoID, SHA: "sha", PolicyVersionID: uuid.Nil},
			verdict: DegradedVerdict{
				VerdictID: verdictID, EvaluationRunID: runID,
				Verdict: "warn", DegradedReason: "samples_pending",
			},
			want: "run.PolicyVersionID is the zero uuid",
		},
		{
			name: "zero verdict id",
			run:  DegradedRun{EvaluationRunID: runID, RepoID: repoID, SHA: "sha", PolicyVersionID: pvID},
			verdict: DegradedVerdict{
				VerdictID: uuid.Nil, EvaluationRunID: runID,
				Verdict: "warn", DegradedReason: "samples_pending",
			},
			want: "verdict.VerdictID is the zero uuid",
		},
		{
			name: "verdict run id mismatch",
			run:  DegradedRun{EvaluationRunID: runID, RepoID: repoID, SHA: "sha", PolicyVersionID: pvID},
			verdict: DegradedVerdict{
				VerdictID: verdictID, EvaluationRunID: uuid.Must(uuid.NewV4()),
				Verdict: "warn", DegradedReason: "samples_pending",
			},
			want: "does not match run.EvaluationRunID",
		},
		{
			name: "empty verdict string",
			run:  DegradedRun{EvaluationRunID: runID, RepoID: repoID, SHA: "sha", PolicyVersionID: pvID},
			verdict: DegradedVerdict{
				VerdictID: verdictID, EvaluationRunID: runID,
				Verdict: "", DegradedReason: "samples_pending",
			},
			want: "verdict.Verdict is empty",
		},
		{
			name: "empty degraded reason",
			run:  DegradedRun{EvaluationRunID: runID, RepoID: repoID, SHA: "sha", PolicyVersionID: pvID},
			verdict: DegradedVerdict{
				VerdictID: verdictID, EvaluationRunID: runID,
				Verdict: "warn", DegradedReason: "",
			},
			want: "verdict.DegradedReason is empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := s.AppendDegradedRun(context.Background(), tc.run, tc.verdict)
			if err == nil {
				t.Fatalf("want error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%q; want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// TestSQLSampleReadiness_New_RejectsNilDB pins the
// constructor's nil-DB rejection.
func TestSQLSampleReadiness_New_RejectsNilDB(t *testing.T) {
	_, err := NewSQLSampleReadiness(SQLSampleReadinessConfig{})
	if err == nil {
		t.Fatal("NewSQLSampleReadiness: want error, got nil")
	}
}

// TestSQLSampleReadiness_SamplesReady_ValidatesInputs pins
// the (repoID, sha) input validation. Both fields are
// required and must be non-zero / non-empty.
func TestSQLSampleReadiness_SamplesReady_ValidatesInputs(t *testing.T) {
	t.Parallel()
	r, err := NewSQLSampleReadiness(SQLSampleReadinessConfig{DB: openTestDB(t)})
	if err != nil {
		t.Fatalf("NewSQLSampleReadiness: %v", err)
	}
	if _, err := r.SamplesReady(context.Background(), uuid.Nil, "sha"); err == nil {
		t.Error("SamplesReady(zero repo): want error, got nil")
	}
	if _, err := r.SamplesReady(context.Background(), uuid.Must(uuid.NewV4()), ""); err == nil {
		t.Error("SamplesReady(empty sha): want error, got nil")
	}
}

// TestNewProductionGate_RequiresAllDependencies pins the
// required-field checks on the wiring helper. We only
// exercise the early-return cases that don't require a
// real *steward.Steward (which would force a real DB).
func TestNewProductionGate_RequiresAllDependencies(t *testing.T) {
	t.Parallel()
	_, err := NewProductionGate(ProductionGateConfig{})
	if err == nil {
		t.Fatal("want error for missing DB, got nil")
	}
	if !strings.Contains(err.Error(), "DB is required") {
		t.Errorf("err=%q; want substring %q", err.Error(), "DB is required")
	}

	_, err = NewProductionGate(ProductionGateConfig{DB: openTestDB(t)})
	if err == nil {
		t.Fatal("want error for missing Steward, got nil")
	}
	if !strings.Contains(err.Error(), "Steward is required") {
		t.Errorf("err=%q; want substring %q", err.Error(), "Steward is required")
	}
}

