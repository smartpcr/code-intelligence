package evaluator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"

	"forge/services/clean-code/internal/audit/wal"
)

// SQLDegradedRunStore is the production [DegradedRunStore]
// implementation. It writes the gate's degraded-path audit
// rows -- ONE `evaluation_run` + ONE `evaluation_verdict`
// -- inside a single transaction under the
// `clean_code_evaluator` grant.
//
// Writer-ownership (tech-spec Sec 7.2 lines 1256-1261):
// the Audit tables are INSERT-granted to
// `clean_code_evaluator` (this writer), `clean_code_solid_batch`
// (the Rule Engine's `SQLStore.AppendEvaluation`), AND
// `clean_code_wal_reconciler` (replay-only). The gate's
// happy path delegates to the engine; ONLY the two
// degraded short-circuits go through this writer.
//
// Concurrency: SQLDegradedRunStore is safe for concurrent
// use across goroutines (`*sql.DB` is). The caller owns
// the `*sql.DB` lifecycle -- this Store does not call
// `Close`.
type SQLDegradedRunStore struct {
	db     *sql.DB
	schema string

	// walWriter is the Audit WAL writer (Stage 9.1 /
	// architecture Sec 7.10). REQUIRED -- never nil after
	// construction. Every degraded `evaluation_run` +
	// `evaluation_verdict` INSERT this store performs is
	// mirrored as a signed WAL frame fsynced BEFORE the
	// SQL transaction commits. The constructor rejects a
	// nil WalWriter so the gate's degraded short-circuits
	// cannot silently bypass the WAL.
	walWriter *wal.Writer
}

// SQLDegradedRunStoreConfig configures the production
// [SQLDegradedRunStore].
type SQLDegradedRunStoreConfig struct {
	// DB is the `*sql.DB` handle. Required. Production
	// composition root MUST authenticate this handle as
	// `clean_code_evaluator` per Stage 1.5 / tech-spec
	// Sec 7.2 lines 1256-1261.
	DB *sql.DB
	// Schema is the PostgreSQL schema name -- defaults to
	// `"clean_code"` (the canonical CLEAN-CODE schema)
	// when empty.
	Schema string
	// WalWriter is the Audit WAL writer. REQUIRED -- the
	// constructor rejects a nil writer. Every degraded
	// audit write is mirrored to the WAL inside the same
	// SQL transaction; WAL fsync precedes SQL commit
	// (architecture Sec 7.10). Tests supply a
	// [wal.Writer] backed by `t.TempDir()` so the
	// degraded-write path is exercised identically to
	// production.
	WalWriter *wal.Writer
}

// NewSQLDegradedRunStore wires the production
// [SQLDegradedRunStore]. Returns an error when any required
// dependency is missing.
//
// REQUIRED -- WalWriter (Stage 9.1 brief, iter-1 evaluator
// item #2): every successful degraded `evaluation_run` +
// `evaluation_verdict` INSERT this store performs MUST be
// paired with a WAL frame fsynced before the SQL commit
// (architecture Sec 7.10 / tech-spec Sec 4.13). The
// constructor refuses a nil WalWriter so a production
// composition cannot silently degrade to SQL-only writes.
func NewSQLDegradedRunStore(cfg SQLDegradedRunStoreConfig) (*SQLDegradedRunStore, error) {
	if cfg.DB == nil {
		return nil, errors.New("evaluator: NewSQLDegradedRunStore: DB is nil")
	}
	if cfg.WalWriter == nil {
		return nil, errors.New("evaluator: NewSQLDegradedRunStore: WalWriter is nil (Stage 9.1: every degraded Audit INSERT MUST stage a WAL frame fsynced before SQL commit; supply a *wal.Writer rooted at CLEAN_CODE_AUDIT_WAL_DIR in production or t.TempDir() in tests)")
	}
	schema := cfg.Schema
	if schema == "" {
		schema = "clean_code"
	}
	return &SQLDegradedRunStore{db: cfg.DB, schema: schema, walWriter: cfg.WalWriter}, nil
}

// AppendDegradedRun persists the run + verdict pair inside
// ONE transaction. The verdict's `caller` is always
// `'eval_gate'` because the gate is the only writer that
// uses this path. The verdict carries `degraded=true` and
// the closed-set `degraded_reason` value
// (`'samples_pending'` or `'policy_signature_invalid'`).
//
// Validation: zero UUIDs are rejected up front (G4 -- a
// finding/verdict row with a zero FK is unrecoverable).
// The verdict's `EvaluationRunID` MUST match the run's
// `EvaluationRunID`.
func (s *SQLDegradedRunStore) AppendDegradedRun(ctx context.Context, run DegradedRun, verdict DegradedVerdict) error {
	if run.EvaluationRunID == uuid.Nil {
		return errors.New("evaluator: AppendDegradedRun: run.EvaluationRunID is the zero uuid")
	}
	if run.RepoID == uuid.Nil {
		return errors.New("evaluator: AppendDegradedRun: run.RepoID is the zero uuid")
	}
	if run.SHA == "" {
		return errors.New("evaluator: AppendDegradedRun: run.SHA is empty")
	}
	if run.PolicyVersionID == uuid.Nil {
		return errors.New("evaluator: AppendDegradedRun: run.PolicyVersionID is the zero uuid")
	}
	if verdict.VerdictID == uuid.Nil {
		return errors.New("evaluator: AppendDegradedRun: verdict.VerdictID is the zero uuid")
	}
	if verdict.EvaluationRunID != run.EvaluationRunID {
		return fmt.Errorf("evaluator: AppendDegradedRun: verdict.EvaluationRunID=%s does not match run.EvaluationRunID=%s",
			verdict.EvaluationRunID, run.EvaluationRunID)
	}
	if verdict.Verdict == "" {
		return errors.New("evaluator: AppendDegradedRun: verdict.Verdict is empty")
	}
	if !verdict.Verdict.IsValid() {
		return fmt.Errorf("evaluator: AppendDegradedRun: verdict.Verdict=%q is not in the canonical {pass, warn, block} set", verdict.Verdict)
	}
	// Architecture Sec 3.7 lines 566-575 + operator pin
	// `gate-degraded-policy=warn` (Sec 1.6): a degraded
	// audit row MUST surface `warn`. The gate code path
	// always passes [VerdictWarn], so this check defends
	// against a future caller that mints a degraded row
	// with `block` or `pass` and corrupts the audit trail.
	if verdict.Degraded && verdict.Verdict != VerdictWarn {
		return fmt.Errorf("evaluator: AppendDegradedRun: degraded=true requires verdict='warn' per architecture Sec 3.7; got %q", verdict.Verdict)
	}
	if verdict.DegradedReason == "" {
		return errors.New("evaluator: AppendDegradedRun: verdict.DegradedReason is empty (degraded paths MUST carry a reason)")
	}
	// Stage 6.1 brief: `percentile_stale` is INSIGHTS-ONLY
	// (tech-spec C17). The DB CHECK admits the value but
	// eval.gate MUST reject it -- defence in depth so a
	// programming mistake cannot conflate an Insights-
	// surface staleness with a gate-surface degraded path.
	if !verdict.DegradedReason.IsValidForGate() {
		return fmt.Errorf("%w: %q", ErrInvalidGateDegradedReason, verdict.DegradedReason)
	}

	qual := func(t string) string { return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(t) }

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("evaluator: AppendDegradedRun: BeginTx: %w", err)
	}
	defer func() {
		// Best-effort rollback; ignored error here (a
		// successful COMMIT makes Rollback a no-op).
		_ = tx.Rollback()
	}()

	// Allocate the per-tx WAL batch. The constructor
	// guarantees `s.walWriter != nil` (Stage 9.1 brief,
	// iter-2 evaluator item #2). `defer batch.Cancel()`
	// ensures discard on every error / panic path;
	// `batch.Commit(ctx)` on the happy path renders the
	// later Cancel a no-op.
	walBatch := s.walWriter.NewTxBatch()
	defer walBatch.Cancel()

	createdAt := time.Unix(0, run.CreatedAt).UTC()

	// Iter-8 evaluator feedback #2: the degraded write
	// inserts `scope_id` so the audit row matches the
	// engine's happy-path schema (migration 0008 +
	// architecture.md §5.4.2). PostgreSQL accepts a
	// driver-side `nil` for the `uuid NULL` column,
	// which preserves the canonical whole-SHA
	// semantics (`scope_id IS NULL` -- every
	// batch_refresh and every eval_gate call with no
	// scope argument). Non-nil records the per-scope
	// evaluation. Driver-side: pass the string
	// representation for non-nil and untyped Go `nil`
	// for nil; lib/pq accepts both.
	var scopeArg interface{}
	if run.ScopeID != nil {
		scopeArg = run.ScopeID.String()
	}

	runStmt := fmt.Sprintf(
		`INSERT INTO %s
		   (evaluation_run_id, repo_id, sha, policy_version_id, caller, scope_id, created_at)
		 VALUES ($1, $2, $3, $4, 'eval_gate', $5, $6)`,
		qual("evaluation_run"),
	)
	if _, err := tx.ExecContext(ctx, runStmt,
		run.EvaluationRunID.String(),
		run.RepoID.String(),
		run.SHA,
		run.PolicyVersionID.String(),
		scopeArg,
		createdAt,
	); err != nil {
		return fmt.Errorf("evaluator: AppendDegradedRun: insert evaluation_run: %w", err)
	}
	{
		rowJSON, err := walDegradedRunRowJSON(run)
		if err != nil {
			return fmt.Errorf("evaluator: AppendDegradedRun: WAL row shape evaluation_run: %w", err)
		}
		if _, err := walBatch.StageNew(ctx, wal.TableEvaluationRun, run.EvaluationRunID, rowJSON); err != nil {
			return fmt.Errorf("evaluator: AppendDegradedRun: WAL stage evaluation_run: %w", err)
		}
	}

	verdictCreatedAt := time.Unix(0, verdict.CreatedAt).UTC()
	verdictStmt := fmt.Sprintf(
		`INSERT INTO %s
		   (verdict_id, evaluation_run_id, verdict, degraded, degraded_reason, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		qual("evaluation_verdict"),
	)
	if _, err := tx.ExecContext(ctx, verdictStmt,
		verdict.VerdictID.String(),
		verdict.EvaluationRunID.String(),
		string(verdict.Verdict),
		verdict.Degraded,
		string(verdict.DegradedReason),
		verdictCreatedAt,
	); err != nil {
		return fmt.Errorf("evaluator: AppendDegradedRun: insert evaluation_verdict: %w", err)
	}
	{
		rowJSON, err := walDegradedVerdictRowJSON(verdict)
		if err != nil {
			return fmt.Errorf("evaluator: AppendDegradedRun: WAL row shape evaluation_verdict: %w", err)
		}
		if _, err := walBatch.StageNew(ctx, wal.TableEvaluationVerdict, verdict.VerdictID, rowJSON); err != nil {
			return fmt.Errorf("evaluator: AppendDegradedRun: WAL stage evaluation_verdict: %w", err)
		}
	}

	// WAL fsync MUST precede SQL commit (architecture
	// Sec 7.1 / honest four-state contract): if the
	// sync fails the SQL tx rolls back and the
	// reconciler treats any readable speculative frame
	// as a replay candidate.
	if err := walBatch.Commit(ctx); err != nil {
		return fmt.Errorf("evaluator: AppendDegradedRun: WAL flush before SQL commit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("evaluator: AppendDegradedRun: commit: %w", err)
	}
	return nil
}
