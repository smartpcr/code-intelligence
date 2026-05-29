package reconciler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// SQLReplayer is the production [Replayer] implementation.
// It issues `INSERT ... ON CONFLICT (<pk>) DO NOTHING`
// statements under the `clean_code_wal_reconciler` PostgreSQL
// role (migration 0004_roles.up.sql lines 455-465 grant
// INSERT, SELECT on the three Audit tables to that role).
// The role has UPDATE/DELETE revoked at the migration layer
// (0004_roles.up.sql lines 472-474), so a hypothetical bug
// here cannot escalate into an UPDATE / DELETE.
//
// Replay-only contract (architecture Sec 7.10 / Stage 9.2
// brief invariants 1-4):
//
//  1. Every INSERT carries `ON CONFLICT (<pk>) DO NOTHING`,
//     so a row that already exists is left untouched
//     (RowsAffected = 0 -> [OutcomeSkippedExisting]).
//  2. No DELETE / UPDATE statement exists in this file.
//  3. Only the three audit tables are referenced; the
//     statement constants are package-level, so a future
//     refactor cannot smuggle in a non-Audit table name.
//  4. The `caller` value for evaluation_run is bound
//     verbatim from [EvaluationRunRow.Caller]; no
//     substitution branch exists.
//
// Concurrency: safe for concurrent use across goroutines
// (`*sql.DB` is). The caller owns the `*sql.DB` lifecycle.
type SQLReplayer struct {
	db     *sql.DB
	schema string
}

// SQLReplayerConfig configures the production [SQLReplayer].
type SQLReplayerConfig struct {
	// DB is the `*sql.DB` handle. Required. Production
	// composition root MUST authenticate this handle as
	// `clean_code_wal_reconciler` (migration 0004 + tech-
	// spec Sec 7.2 lines 1258-1261).
	DB *sql.DB
	// Schema is the PostgreSQL schema name -- defaults to
	// `"clean_code"` when empty.
	Schema string
}

// NewSQLReplayer wires the production [SQLReplayer].
// Returns an error when the required `DB` is missing.
func NewSQLReplayer(cfg SQLReplayerConfig) (*SQLReplayer, error) {
	if cfg.DB == nil {
		return nil, errors.New("reconciler: NewSQLReplayer: DB is nil (production composition MUST authenticate as clean_code_wal_reconciler per migration 0004)")
	}
	schema := cfg.Schema
	if schema == "" {
		schema = "clean_code"
	}
	return &SQLReplayer{db: cfg.DB, schema: schema}, nil
}

// qual qualifies a bare table name with the configured schema.
func (r *SQLReplayer) qual(table string) string {
	return pq.QuoteIdentifier(r.schema) + "." + pq.QuoteIdentifier(table)
}

// ReplayRun is the `evaluation_run` re-insert path.
// PRESERVES `row.Caller` verbatim per Stage 9.2 brief
// bullet 3: the value is bound to the `caller` parameter
// with NO transformation in this function or any helper it
// calls.
func (r *SQLReplayer) ReplayRun(ctx context.Context, row EvaluationRunRow) (Outcome, error) {
	if row.EvaluationRunID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayRun: row.EvaluationRunID is the zero uuid")
	}
	if row.RepoID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayRun: row.RepoID is the zero uuid")
	}
	if row.SHA == "" {
		return 0, errors.New("reconciler: ReplayRun: row.SHA is empty")
	}
	if row.PolicyVersionID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayRun: row.PolicyVersionID is the zero uuid")
	}
	if row.Caller == "" {
		return 0, errors.New("reconciler: ReplayRun: row.Caller is empty (frame writer invariant violation)")
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s
		   (evaluation_run_id, repo_id, sha, policy_version_id, caller, scope_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6::uuid, $7)
		 ON CONFLICT (evaluation_run_id) DO NOTHING`,
		r.qual("evaluation_run"),
	)
	// scope_id is nullable; pass driver-NULL when the row
	// is whole-SHA (every batch_refresh by construction
	// and eval.gate calls without a scope argument).
	var scopeArg any
	if row.ScopeID != nil {
		scopeArg = row.ScopeID.String()
	}
	res, err := r.db.ExecContext(ctx, stmt,
		row.EvaluationRunID.String(),
		row.RepoID.String(),
		row.SHA,
		row.PolicyVersionID.String(),
		row.Caller, // VERBATIM -- Stage 9.2 brief bullet 3.
		scopeArg,
		row.CreatedAt.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("reconciler: ReplayRun: exec: %w", err)
	}
	return classifyOutcome(res, "ReplayRun")
}

// ReplayVerdict is the `evaluation_verdict` re-insert path.
// `degraded_reason` is passed through `NULLIF($5, '')` to
// preserve the writer's empty-string-as-NULL contract.
func (r *SQLReplayer) ReplayVerdict(ctx context.Context, row EvaluationVerdictRow) (Outcome, error) {
	if row.VerdictID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayVerdict: row.VerdictID is the zero uuid")
	}
	if row.EvaluationRunID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayVerdict: row.EvaluationRunID is the zero uuid")
	}
	if row.Verdict == "" {
		return 0, errors.New("reconciler: ReplayVerdict: row.Verdict is empty")
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s
		   (verdict_id, evaluation_run_id, verdict, degraded, degraded_reason, created_at)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)
		 ON CONFLICT (verdict_id) DO NOTHING`,
		r.qual("evaluation_verdict"),
	)
	res, err := r.db.ExecContext(ctx, stmt,
		row.VerdictID.String(),
		row.EvaluationRunID.String(),
		row.Verdict,
		row.Degraded,
		row.DegradedReason,
		row.CreatedAt.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("reconciler: ReplayVerdict: exec: %w", err)
	}
	return classifyOutcome(res, "ReplayVerdict")
}

// ReplayFinding is the `finding` re-insert path.
// `metric_sample_ids` is marshalled as a JSON array and
// cast via `$9::jsonb` to match the migration's column
// shape.
func (r *SQLReplayer) ReplayFinding(ctx context.Context, row FindingRow) (Outcome, error) {
	if row.FindingID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayFinding: row.FindingID is the zero uuid")
	}
	if row.EvaluationRunID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayFinding: row.EvaluationRunID is the zero uuid")
	}
	if row.RepoID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayFinding: row.RepoID is the zero uuid")
	}
	if row.SHA == "" {
		return 0, errors.New("reconciler: ReplayFinding: row.SHA is empty")
	}
	if row.ScopeID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayFinding: row.ScopeID is the zero uuid (NOT NULL in finding schema)")
	}
	if row.RuleID == "" {
		return 0, errors.New("reconciler: ReplayFinding: row.RuleID is empty")
	}
	if row.PolicyVersionID == uuid.Nil {
		return 0, errors.New("reconciler: ReplayFinding: row.PolicyVersionID is the zero uuid")
	}
	if row.Severity == "" {
		return 0, errors.New("reconciler: ReplayFinding: row.Severity is empty")
	}
	if row.Delta == "" {
		return 0, errors.New("reconciler: ReplayFinding: row.Delta is empty")
	}
	samples := row.MetricSampleIDs
	if samples == nil {
		samples = []uuid.UUID{}
	}
	sampleStrs := make([]string, len(samples))
	for i, s := range samples {
		sampleStrs[i] = s.String()
	}
	raw, err := json.Marshal(sampleStrs)
	if err != nil {
		return 0, fmt.Errorf("reconciler: ReplayFinding: marshal metric_sample_ids: %w", err)
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s
		   (finding_id, evaluation_run_id, repo_id, sha, scope_id,
		    rule_id, rule_version, policy_version_id, metric_sample_ids,
		    severity, delta, explanation_md, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13)
		 ON CONFLICT (finding_id) DO NOTHING`,
		r.qual("finding"),
	)
	res, err := r.db.ExecContext(ctx, stmt,
		row.FindingID.String(),
		row.EvaluationRunID.String(),
		row.RepoID.String(),
		row.SHA,
		row.ScopeID.String(),
		row.RuleID,
		row.RuleVersion,
		row.PolicyVersionID.String(),
		string(raw),
		row.Severity,
		row.Delta,
		row.ExplanationMD,
		row.CreatedAt.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("reconciler: ReplayFinding: exec: %w", err)
	}
	return classifyOutcome(res, "ReplayFinding")
}

// classifyOutcome maps PostgreSQL's RowsAffected to the
// public [Outcome] enum. ON CONFLICT DO NOTHING returns
// `RowsAffected = 1` on the insert path and `0` on the
// conflict path (the SQL standard's `INSERT ... RETURNING`
// would surface the same signal; we prefer RowsAffected
// because lib/pq returns it without an extra round-trip).
//
// The `op` parameter is the calling method name for error
// messages.
func classifyOutcome(res sql.Result, op string) (Outcome, error) {
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("reconciler: %s: RowsAffected: %w", op, err)
	}
	switch n {
	case 1:
		return OutcomeInserted, nil
	case 0:
		// ON CONFLICT DO NOTHING path: the row's PK
		// already existed. Brief invariant 1 honoured:
		// the existing row is left UNCHANGED.
		return OutcomeSkippedExisting, nil
	default:
		// Defensive: a single-row INSERT cannot return
		// anything other than 0 or 1 under PostgreSQL.
		// Surface loudly so an operator can investigate
		// rather than silently miscount.
		return 0, fmt.Errorf("reconciler: %s: RowsAffected=%d; expected 0 or 1 (PostgreSQL single-row INSERT under ON CONFLICT DO NOTHING)", op, n)
	}
}

// Compile-time check that SQLReplayer satisfies [Replayer].
var _ Replayer = (*SQLReplayer)(nil)
