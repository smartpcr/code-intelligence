package rule_engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// txStore is the transaction-scoped [Store] handed to the
// closure passed to [SQLStore.WithEvaluationLock]. It
// implements every [Store] method against the same `*sql.Tx`
// so prior-finding reads and the [Store.AppendEvaluation]
// write see the same snapshot AND commit atomically.
//
// Read-path delegates that don't need transaction-scope
// (policy/rule/threshold lookups via the steward) fall
// through to the embedded [steward.SQLStore] -- those
// rows are immutable post-publication so a sibling tx
// cannot mutate them mid-run.
//
// txStore does NOT implement nested [Store.WithEvaluationLock]:
// re-entering the lock from inside the closure is a logic
// bug (the engine never does it) and would deadlock against
// the already-held `pg_advisory_xact_lock`. The method
// returns an explicit error rather than silently misbehave.
type txStore struct {
	tx      *sql.Tx
	schema  string
	steward *steward.SQLStore
}

func (s *txStore) qualify(table string) string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(table)
}

func (s *txStore) GetPolicyVersion(ctx context.Context, policyVersionID uuid.UUID) (steward.PolicyVersion, error) {
	// Policy / rule / threshold rows are immutable
	// post-publication. We can safely read them outside
	// the engine's transaction -- the steward's own
	// `*sql.DB` handle suffices and avoids holding the
	// engine's tx open for a no-mutation lookup.
	return s.steward.GetPolicyVersion(ctx, policyVersionID)
}

func (s *txStore) GetRule(ctx context.Context, ruleID string, version int) (steward.Rule, error) {
	return getRuleRow(ctx, s.tx, s.schema, ruleID, version)
}

func (s *txStore) GetThreshold(ctx context.Context, thresholdID uuid.UUID) (steward.Threshold, error) {
	return getThresholdRow(ctx, s.tx, s.schema, thresholdID)
}

func (s *txStore) LatestMatchingOverride(ctx context.Context, ruleID string, candidate steward.CandidateScope) (steward.Override, bool, error) {
	return s.steward.LatestMatchingOverride(ctx, ruleID, candidate)
}

func (s *txStore) ListMetricSamples(ctx context.Context, repoID uuid.UUID, sha string, scopeID *uuid.UUID) ([]Sample, error) {
	// Share the active-row JOIN + four-field hydration with
	// SQLStore so the tx-scoped read sees exactly the same
	// row shape as the auto-committing path. See
	// [listMetricSamplesQuery] for the rationale.
	return listMetricSamplesQuery(ctx, s.tx, s.schema, repoID, sha, scopeID)
}

func (s *txStore) ParentSHA(ctx context.Context, repoID uuid.UUID, sha string) (string, bool, error) {
	stmt := fmt.Sprintf(
		`SELECT parent_sha FROM %s WHERE repo_id = $1 AND sha = $2`,
		s.qualify("commit"),
	)
	var parent sql.NullString
	err := s.tx.QueryRowContext(ctx, stmt, repoID.String(), sha).Scan(&parent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("rule_engine: txStore.ParentSHA: query: %w", err)
	}
	if !parent.Valid || parent.String == "" {
		return "", false, nil
	}
	return parent.String, true, nil
}

func (s *txStore) LatestPriorFinding(ctx context.Context, repoID uuid.UUID, parentSHA string, scopeID uuid.UUID, ruleID string, policyVersionID uuid.UUID) (Finding, bool, error) {
	if parentSHA == "" {
		return Finding{}, false, errors.New("rule_engine: txStore.LatestPriorFinding: parentSHA is empty")
	}
	if scopeID == uuid.Nil {
		return Finding{}, false, errors.New("rule_engine: txStore.LatestPriorFinding: scopeID is the zero uuid")
	}
	if ruleID == "" {
		return Finding{}, false, errors.New("rule_engine: txStore.LatestPriorFinding: ruleID is empty")
	}
	if policyVersionID == uuid.Nil {
		return Finding{}, false, errors.New("rule_engine: txStore.LatestPriorFinding: policyVersionID is the zero uuid")
	}
	stmt := fmt.Sprintf(
		`SELECT finding_id, evaluation_run_id, repo_id, sha, scope_id,
		        rule_id, rule_version, policy_version_id,
		        metric_sample_ids, severity::text, delta::text,
		        explanation_md, created_at
		 FROM %s
		 WHERE repo_id = $1 AND sha = $2 AND scope_id = $3
		   AND rule_id = $4 AND policy_version_id = $5
		 ORDER BY created_at DESC, finding_id DESC
		 LIMIT 1`,
		s.qualify("finding"),
	)
	row := s.tx.QueryRowContext(ctx, stmt, repoID.String(), parentSHA, scopeID.String(), ruleID, policyVersionID.String())
	f, err := scanFindingRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Finding{}, false, nil
		}
		return Finding{}, false, fmt.Errorf("rule_engine: txStore.LatestPriorFinding: %w", err)
	}
	return f, true, nil
}

func (s *txStore) ListPriorBlockFindings(ctx context.Context, repoID uuid.UUID, parentSHA string, policyVersionID uuid.UUID) ([]Finding, error) {
	if parentSHA == "" {
		return nil, errors.New("rule_engine: txStore.ListPriorBlockFindings: parentSHA is empty")
	}
	if policyVersionID == uuid.Nil {
		return nil, errors.New("rule_engine: txStore.ListPriorBlockFindings: policyVersionID is the zero uuid")
	}
	stmt := fmt.Sprintf(
		`SELECT finding_id, evaluation_run_id, repo_id, sha, scope_id,
		        rule_id, rule_version, policy_version_id,
		        metric_sample_ids, severity::text, delta::text,
		        explanation_md, created_at
		 FROM (
		   SELECT DISTINCT ON (scope_id, rule_id)
		          finding_id, evaluation_run_id, repo_id, sha,
		          scope_id, rule_id, rule_version, policy_version_id,
		          metric_sample_ids, severity, delta,
		          explanation_md, created_at
		   FROM %s
		   WHERE repo_id = $1 AND sha = $2 AND policy_version_id = $3
		   ORDER BY scope_id, rule_id, created_at DESC, finding_id DESC
		 ) latest
		 WHERE severity = 'block' AND delta != 'resolved'
		 ORDER BY rule_id, scope_id`,
		s.qualify("finding"),
	)
	rows, err := s.tx.QueryContext(ctx, stmt, repoID.String(), parentSHA, policyVersionID.String())
	if err != nil {
		return nil, fmt.Errorf("rule_engine: txStore.ListPriorBlockFindings: query: %w", err)
	}
	defer rows.Close()
	out := make([]Finding, 0)
	for rows.Next() {
		f, err := scanFindingRow(rows)
		if err != nil {
			return nil, fmt.Errorf("rule_engine: txStore.ListPriorBlockFindings: scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rule_engine: txStore.ListPriorBlockFindings: rows: %w", err)
	}
	return out, nil
}

func (s *txStore) AppendEvaluation(ctx context.Context, run EvaluationRun, verdict EvaluationVerdict, findings []Finding) error {
	return appendEvaluationInTx(ctx, s.tx, s.schema, run, verdict, findings)
}

// LookupRecentCanonicalRun implements [Store] for the
// tx-bound path. CRITICAL: this runs INSIDE the
// `pg_advisory_xact_lock` envelope opened by
// [SQLStore.WithEvaluationLock], so a parallel replica
// that has already committed its canonical row (and thus
// released its advisory lock) is observed by this
// caller's RC-isolated SELECT -- which is the
// cross-replica dedup guarantee for iter-6 evaluator
// item #3.
//
// SCOPE-AWARE LOOKUP (iter-7 evaluator item #2): the
// query now filters on `evaluation_run.scope_id` using
// the null-safe `IS NOT DISTINCT FROM` operator (migration
// 0008). This means the engine can safely consult the
// Store-level lookup for BOTH callers including the
// previously-excluded eval.gate scoped path: a SHA-level
// eval_gate call (scope_id = NULL) will not collide with
// a per-scope eval_gate call (scope_id = <uuid>) for the
// same (repo, sha, policy_version_id) tuple.
func (s *txStore) LookupRecentCanonicalRun(ctx context.Context, repoID uuid.UUID, sha string, policyVersionID uuid.UUID, caller Caller, scopeID *uuid.UUID, since time.Duration) (RunResult, bool, error) {
	if err := validateLookupArgs(repoID, sha, policyVersionID, caller); err != nil {
		return RunResult{}, false, err
	}
	return lookupRecentCanonicalRunQuery(ctx, s.tx, s.schema, repoID, sha, policyVersionID, caller, scopeID, since)
}

// WithEvaluationLock refuses re-entry: the engine never
// nests locks, and the underlying advisory-xact-lock would
// deadlock against itself anyway. We surface the misuse
// loudly rather than silently hang the worker.
func (s *txStore) WithEvaluationLock(ctx context.Context, repoID uuid.UUID, sha string, fn func(Store) error) error {
	return errors.New("rule_engine: txStore.WithEvaluationLock: re-entrant lock acquisition is forbidden (engine bug)")
}

// Compile-time check.
var _ Store = (*txStore)(nil)
