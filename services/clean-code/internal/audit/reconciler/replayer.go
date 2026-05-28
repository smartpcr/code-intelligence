package reconciler

import (
	"context"
	"time"

	"github.com/gofrs/uuid"
)

// Outcome classifies the result of a single replay attempt.
// The two values map cleanly to PostgreSQL's
// `INSERT ... ON CONFLICT (<pk>) DO NOTHING` semantics:
// `RowsAffected = 1` means the row was newly inserted;
// `RowsAffected = 0` means the row already existed and the
// statement was a no-op.
type Outcome int

const (
	// OutcomeInserted reports a fresh INSERT (the
	// reconciler replayed a missing row).
	OutcomeInserted Outcome = iota
	// OutcomeSkippedExisting reports the row already
	// existed in PostgreSQL. The reconciler's brief
	// invariant 1 ("NEVER inserts a row whose `(table,
	// row_pk)` already exists") is satisfied here: ON
	// CONFLICT DO NOTHING leaves the existing row's
	// columns untouched.
	OutcomeSkippedExisting
)

// String renders the [Outcome] for log lines and tests.
func (o Outcome) String() string {
	switch o {
	case OutcomeInserted:
		return "inserted"
	case OutcomeSkippedExisting:
		return "skipped_existing"
	default:
		return "unknown"
	}
}

// Replayer is the writer-of-record port the [Reconciler]
// uses to replay each frame's row back into PostgreSQL. The
// interface has exactly three methods -- one per Audit
// table -- so the dispatcher's switch on [wal.AuditFrame.Table]
// becomes a static call site. There is no fourth method
// "ReplayAny" / "ReplayDynamic": the brief's "never modifies
// a non-Audit table" invariant is enforced by the absence of
// a dynamic table name code path.
//
// The production implementation is [SQLReplayer]; tests use
// a small in-memory fake (see `replay_test.go`).
type Replayer interface {
	// ReplayRun re-inserts an `evaluation_run` row if it
	// is not already present, keyed by
	// `row.EvaluationRunID`. PRESERVES `row.Caller`
	// verbatim per Stage 9.2 brief bullet 3.
	ReplayRun(ctx context.Context, row EvaluationRunRow) (Outcome, error)
	// ReplayVerdict re-inserts an `evaluation_verdict`
	// row if it is not already present, keyed by
	// `row.VerdictID`.
	ReplayVerdict(ctx context.Context, row EvaluationVerdictRow) (Outcome, error)
	// ReplayFinding re-inserts a `finding` row if it is
	// not already present, keyed by `row.FindingID`.
	ReplayFinding(ctx context.Context, row FindingRow) (Outcome, error)
}

// EvaluationRunRow is the parsed shape of an
// `evaluation_run` WAL frame's `row_json`. The field names
// mirror the snake_case JSON keys (and the SQL column
// names) one-to-one; struct tags pin the JSON shape.
//
// `Caller` is stored as a Go string -- NOT as the
// `rule_engine.Caller` typed alias -- so this package does
// NOT depend on `internal/rule_engine`. The writer
// invariants already guarantee the on-disk value is in the
// canonical {`eval_gate`, `batch_refresh`} set; if a
// corrupt frame carries an out-of-set value, the PostgreSQL
// `clean_code.evaluation_run_caller` ENUM cast on INSERT
// rejects it. The brief's "preserve caller" guarantee is
// implemented by passing this string verbatim to the SQL
// bind variable -- there is NO substitution code path in
// this package.
type EvaluationRunRow struct {
	EvaluationRunID uuid.UUID  `json:"evaluation_run_id"`
	RepoID          uuid.UUID  `json:"repo_id"`
	SHA             string     `json:"sha"`
	PolicyVersionID uuid.UUID  `json:"policy_version_id"`
	Caller          string     `json:"caller"`
	ScopeID         *uuid.UUID `json:"scope_id"`
	CreatedAt       time.Time  `json:"created_at"`
}

// EvaluationVerdictRow is the parsed shape of an
// `evaluation_verdict` WAL frame's `row_json`.
//
// `DegradedReason` is the Go string form of a nullable SQL
// column: the empty string MUST be interpreted as "no
// reason" by the writer (translated to SQL NULL via
// `NULLIF($N, '')`). A non-empty value is a member of the
// architecture Sec 8.2 closed set; the PostgreSQL CHECK
// constraint enforces it on INSERT.
type EvaluationVerdictRow struct {
	VerdictID       uuid.UUID `json:"verdict_id"`
	EvaluationRunID uuid.UUID `json:"evaluation_run_id"`
	Verdict         string    `json:"verdict"`
	Degraded        bool      `json:"degraded"`
	DegradedReason  string    `json:"degraded_reason"`
	CreatedAt       time.Time `json:"created_at"`
}

// UnmarshalDegradedReason captures the JSON-null vs
// JSON-string distinction. We hand-roll this for the
// `degraded_reason` field because the wal frame uses
// `null` for the SQL-NULL case (matching `NULLIF($5,'')`),
// and `encoding/json` would happily decode `null` into a
// non-pointer `string` as zero -- which is exactly what we
// want. So no custom UnmarshalJSON is required; the zero
// string IS the "null reason" sentinel.

// FindingRow is the parsed shape of a `finding` WAL
// frame's `row_json`. `MetricSampleIDs` is a slice of
// UUIDs that the SQL replayer marshals as a JSON array
// for the `$9::jsonb` cast.
type FindingRow struct {
	FindingID       uuid.UUID   `json:"finding_id"`
	EvaluationRunID uuid.UUID   `json:"evaluation_run_id"`
	RepoID          uuid.UUID   `json:"repo_id"`
	SHA             string      `json:"sha"`
	ScopeID         uuid.UUID   `json:"scope_id"`
	RuleID          string      `json:"rule_id"`
	RuleVersion     int         `json:"rule_version"`
	PolicyVersionID uuid.UUID   `json:"policy_version_id"`
	MetricSampleIDs []uuid.UUID `json:"metric_sample_ids"`
	Severity        string      `json:"severity"`
	Delta           string      `json:"delta"`
	ExplanationMD   string      `json:"explanation_md"`
	CreatedAt       time.Time   `json:"created_at"`
}
