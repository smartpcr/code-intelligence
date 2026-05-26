package rule_engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// DefaultSchema is the canonical PostgreSQL schema the
// CLEAN-CODE service owns (tech-spec C9 / Sec 8.1.3).
const DefaultSchema = "clean_code"

// SQLStore is the production [Store] implementation. It
// uses `database/sql` + `lib/pq` against the canonical
// `clean_code` schema -- the post-Stage-5.6 audit tables
// (`evaluation_run`, `evaluation_verdict`, `finding`),
// the Stage-5.2 policy / rule / threshold tables, and the
// Stage-1.x measurement tables (`metric_sample`,
// `scope_binding`, `commit`).
//
// Writer ownership (tech-spec Sec 7.2 lines 1256-1261, G1):
// SQLStore is the writer-of-record for the three Audit
// tables when invoked under `caller='batch_refresh'` (the
// batch worker path) OR `caller='eval_gate'` (the
// synchronous gate path -- the engine, NOT the gate,
// writes the run+verdict+findings triple on the happy
// path). The Evaluator's degraded-path writer and the
// Audit WAL Reconciler share the same grant for their own
// narrow paths.
//
// Concurrency: SQLStore is safe for concurrent use across
// goroutines; the engine's read-modify-write window is
// serialised by [SQLStore.WithEvaluationLock] (PostgreSQL
// `pg_advisory_xact_lock`).
//
// The caller owns the `*sql.DB` lifecycle -- SQLStore does
// not call `Close`.
type SQLStore struct {
	db     *sql.DB
	schema string

	// steward is the read-path delegate for policy_version,
	// rule, threshold, and override lookups. The Stage 5.2
	// SQLStore already implements these; we wrap rather
	// than duplicating the canonical-bytes / signature
	// round-trip the steward owns.
	steward *steward.SQLStore
}

// SQLStoreConfig configures the production [SQLStore].
type SQLStoreConfig struct {
	// DB is the canonical `*sql.DB` handle. Required.
	DB *sql.DB
	// Schema is the PostgreSQL schema name -- defaults to
	// [DefaultSchema] when empty. The same schema MUST be
	// used by the [steward.SQLStore] this Store wraps,
	// otherwise reads land in the wrong table.
	Schema string
	// Steward is the policy/rule/threshold/override reader.
	// Required -- the engine cannot evaluate without it.
	Steward *steward.SQLStore
}

// NewSQLStore constructs a production [Store] backed by
// PostgreSQL. Returns an error when any required dependency
// is missing.
func NewSQLStore(cfg SQLStoreConfig) (*SQLStore, error) {
	if cfg.DB == nil {
		return nil, errors.New("rule_engine: NewSQLStore: DB is nil")
	}
	if cfg.Steward == nil {
		return nil, errors.New("rule_engine: NewSQLStore: Steward is nil (cannot resolve policy / rule / threshold rows)")
	}
	schema := cfg.Schema
	if schema == "" {
		schema = DefaultSchema
	}
	return &SQLStore{db: cfg.DB, schema: schema, steward: cfg.Steward}, nil
}

// qualify quotes the schema identifier and joins it with
// the table name.
func (s *SQLStore) qualify(table string) string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(table)
}

// GetPolicyVersion implements [Store] by delegating to the
// embedded steward SQLStore.
func (s *SQLStore) GetPolicyVersion(ctx context.Context, policyVersionID uuid.UUID) (steward.PolicyVersion, error) {
	return s.steward.GetPolicyVersion(ctx, policyVersionID)
}

// GetRule implements [Store] via direct SQL against
// `clean_code.rule`. The Stage 5.2 steward.SQLStore does
// not expose a single-rule fetch; the rule_engine writes
// its own minimal query rather than extending the steward
// API for one caller.
func (s *SQLStore) GetRule(ctx context.Context, ruleID string, version int) (steward.Rule, error) {
	return getRuleRow(ctx, s.db, s.schema, ruleID, version)
}

// GetThreshold implements [Store] via direct SQL against
// `clean_code.threshold`. See [SQLStore.GetRule] for why
// the lookup is not on the steward API.
func (s *SQLStore) GetThreshold(ctx context.Context, thresholdID uuid.UUID) (steward.Threshold, error) {
	return getThresholdRow(ctx, s.db, s.schema, thresholdID)
}

// LatestMatchingOverride implements [Store] by delegating
// to the steward's override matcher.
func (s *SQLStore) LatestMatchingOverride(ctx context.Context, ruleID string, candidate steward.CandidateScope) (steward.Override, bool, error) {
	return s.steward.LatestMatchingOverride(ctx, ruleID, candidate)
}

// ListMetricSamples implements [Store]. Joins
// `metric_sample_active x metric_sample x scope_binding` so
// the engine receives the denormalised, active-row-only
// view it expects (architecture Sec 5.2.1 lines 991-1003 +
// Sec 5.2.3 line 1046).
//
// Reading through `metric_sample_active` is mandatory: the
// raw `metric_sample` table is append-only and includes
// retracted rows. The pointer table tracks the active row
// per (repo, sha, scope_id, metric_kind, metric_version)
// quintuple (G2), so a retract-then-reinsert sequence
// re-points the active row without ever deleting a sample.
// Evaluating rules against raw `metric_sample` would let
// retracted (stale) rows trigger findings, violating C2 /
// architecture Sec 5.2.2 lines 1023-1042.
//
// All four DSL-exposed fields are hydrated:
//   - `pack` (`base|solid|system|ingested`),
//   - `source` (`computed|derived|ingested`),
//   - `degraded`, `degraded_reason`.
//
// When `scopeID` is non-nil the SQL `WHERE` is narrowed to
// that scope so the PR-diff path streams only the affected
// rows.
func (s *SQLStore) ListMetricSamples(ctx context.Context, repoID uuid.UUID, sha string, scopeID *uuid.UUID) ([]Sample, error) {
	return listMetricSamplesQuery(ctx, s.db, s.schema, repoID, sha, scopeID)
}

// listMetricSamplesQuery is the shared body used by both
// [SQLStore.ListMetricSamples] (auto-committing reads) and
// [txStore.ListMetricSamples] (tx-scoped reads inside
// `WithEvaluationLock`). Keeping it in one place ensures the
// active-row JOIN + the four extra-field hydration stays in
// sync across the two call sites (rubber-duck #2: a fix that
// landed only in SQLStore would not flow through the engine
// hot path because RunSync/RunBatch route via txStore).
//
// The querier interface is satisfied by both `*sql.DB` and
// `*sql.Tx`.
func listMetricSamplesQuery(ctx context.Context, q sampleQuerier, schema string, repoID uuid.UUID, sha string, scopeID *uuid.UUID) ([]Sample, error) {
	qual := func(t string) string { return pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(t) }
	stmt := fmt.Sprintf(
		`SELECT ms.sample_id, ms.repo_id, ms.sha, ms.scope_id,
		        sb.scope_kind, sb.canonical_signature,
		        ms.metric_kind, ms.metric_version, ms.value,
		        ms.pack::text, ms.source::text,
		        ms.degraded, COALESCE(ms.degraded_reason, '')
		 FROM %s msa
		 JOIN %s ms ON ms.sample_id = msa.sample_id
		 JOIN %s sb ON sb.scope_id = msa.scope_id
		 WHERE msa.repo_id = $1
		   AND msa.sha = $2`,
		qual("metric_sample_active"),
		qual("metric_sample"),
		qual("scope_binding"),
	)
	args := []any{repoID.String(), sha}
	if scopeID != nil {
		stmt += " AND msa.scope_id = $3"
		args = append(args, scopeID.String())
	}
	stmt += " ORDER BY ms.sample_id"

	rows, err := q.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("rule_engine: ListMetricSamples: query: %w", err)
	}
	defer rows.Close()

	out := make([]Sample, 0)
	for rows.Next() {
		var (
			sampleID       string
			rid            string
			sampleSHA      string
			scopeIDRaw     string
			scopeKind      string
			canonicalSig   string
			metricKind     string
			metricVer      int
			value          sql.NullFloat64
			pack           string
			source         string
			degraded       bool
			degradedReason string
		)
		if err := rows.Scan(&sampleID, &rid, &sampleSHA, &scopeIDRaw, &scopeKind, &canonicalSig, &metricKind, &metricVer, &value, &pack, &source, &degraded, &degradedReason); err != nil {
			return nil, fmt.Errorf("rule_engine: ListMetricSamples: scan: %w", err)
		}
		sampUUID, err := uuid.FromString(sampleID)
		if err != nil {
			return nil, fmt.Errorf("rule_engine: ListMetricSamples: parse sample_id=%q: %w", sampleID, err)
		}
		repoUUID, err := uuid.FromString(rid)
		if err != nil {
			return nil, fmt.Errorf("rule_engine: ListMetricSamples: parse repo_id=%q: %w", rid, err)
		}
		scopeUUID, err := uuid.FromString(scopeIDRaw)
		if err != nil {
			return nil, fmt.Errorf("rule_engine: ListMetricSamples: parse scope_id=%q: %w", scopeIDRaw, err)
		}
		v := 0.0
		if value.Valid {
			v = value.Float64
		}
		out = append(out, Sample{
			ScopeSignature: canonicalSig,
			Sample: dsl.Sample{
				SampleID:       sampUUID,
				RepoID:         repoUUID,
				SHA:            sampleSHA,
				ScopeID:        scopeUUID,
				ScopeKind:      scopeKind,
				MetricKind:     metricKind,
				MetricVersion:  metricVer,
				Value:          v,
				HasValue:       value.Valid,
				Pack:           pack,
				Source:         source,
				Degraded:       degraded,
				DegradedReason: degradedReason,
			},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rule_engine: ListMetricSamples: rows: %w", err)
	}
	return out, nil
}

// sampleQuerier is the minimal QueryContext-only surface
// shared by `*sql.DB` and `*sql.Tx`. Lets
// listMetricSamplesQuery service both [SQLStore] and
// [txStore] without code duplication.
type sampleQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// ParentSHA implements [Store]. Reads
// `clean_code.commit.parent_sha`. Returns `ok=false` when
// the row is missing OR the parent is NULL (root commit) --
// the engine treats both as "no prior".
func (s *SQLStore) ParentSHA(ctx context.Context, repoID uuid.UUID, sha string) (string, bool, error) {
	stmt := fmt.Sprintf(
		`SELECT parent_sha FROM %s WHERE repo_id = $1 AND sha = $2`,
		s.qualify("commit"),
	)
	var parent sql.NullString
	err := s.db.QueryRowContext(ctx, stmt, repoID.String(), sha).Scan(&parent)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("rule_engine: SQLStore.ParentSHA: query: %w", err)
	}
	if !parent.Valid || parent.String == "" {
		return "", false, nil
	}
	return parent.String, true, nil
}

// LatestPriorFinding implements [Store]. Selects the most-
// recent finding at SHA `parentSHA` matching the
// `(repo, scope, rule, policy_version)` tuple.
func (s *SQLStore) LatestPriorFinding(ctx context.Context, repoID uuid.UUID, parentSHA string, scopeID uuid.UUID, ruleID string, policyVersionID uuid.UUID) (Finding, bool, error) {
	if parentSHA == "" {
		return Finding{}, false, errors.New("rule_engine: SQLStore.LatestPriorFinding: parentSHA is empty")
	}
	if scopeID == uuid.Nil {
		return Finding{}, false, errors.New("rule_engine: SQLStore.LatestPriorFinding: scopeID is the zero uuid")
	}
	if ruleID == "" {
		return Finding{}, false, errors.New("rule_engine: SQLStore.LatestPriorFinding: ruleID is empty")
	}
	if policyVersionID == uuid.Nil {
		return Finding{}, false, errors.New("rule_engine: SQLStore.LatestPriorFinding: policyVersionID is the zero uuid")
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
	row := s.db.QueryRowContext(ctx, stmt, repoID.String(), parentSHA, scopeID.String(), ruleID, policyVersionID.String())
	f, err := scanFindingRow(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Finding{}, false, nil
		}
		return Finding{}, false, fmt.Errorf("rule_engine: SQLStore.LatestPriorFinding: %w", err)
	}
	return f, true, nil
}

// ListPriorBlockFindings implements [Store]. Returns one
// row per `(scope_id, rule_id)` tuple at `parentSHA` whose
// latest state is `severity=block AND delta != resolved`,
// filtered to `policy_version_id`.
//
// The query uses a `DISTINCT ON` to take the latest row per
// tuple, ordered by `created_at DESC, finding_id DESC`
// (matching the in-memory fake's tie-break).
func (s *SQLStore) ListPriorBlockFindings(ctx context.Context, repoID uuid.UUID, parentSHA string, policyVersionID uuid.UUID) ([]Finding, error) {
	if parentSHA == "" {
		return nil, errors.New("rule_engine: SQLStore.ListPriorBlockFindings: parentSHA is empty")
	}
	if policyVersionID == uuid.Nil {
		return nil, errors.New("rule_engine: SQLStore.ListPriorBlockFindings: policyVersionID is the zero uuid")
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
	rows, err := s.db.QueryContext(ctx, stmt, repoID.String(), parentSHA, policyVersionID.String())
	if err != nil {
		return nil, fmt.Errorf("rule_engine: SQLStore.ListPriorBlockFindings: query: %w", err)
	}
	defer rows.Close()
	out := make([]Finding, 0)
	for rows.Next() {
		f, err := scanFindingRow(rows)
		if err != nil {
			return nil, fmt.Errorf("rule_engine: SQLStore.ListPriorBlockFindings: scan: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rule_engine: SQLStore.ListPriorBlockFindings: rows: %w", err)
	}
	return out, nil
}

// WithEvaluationLock implements [Store]. Runs `fn` inside
// ONE `BEGIN; pg_advisory_xact_lock(...); ...; COMMIT;`
// envelope -- the transaction-bound store passed to `fn` is
// a [txStore] handle so AppendEvaluation lands in the same
// transaction as the prior-finding reads.
//
// The advisory-lock key is a 64-bit FNV-1a hash over
// `(repo_id || ':' || sha)` so the lock is per-(repo, sha)
// just like the in-memory mutex pool.
func (s *SQLStore) WithEvaluationLock(ctx context.Context, repoID uuid.UUID, sha string, fn func(Store) error) error {
	if fn == nil {
		return errors.New("rule_engine: SQLStore.WithEvaluationLock: fn is nil")
	}
	if repoID == uuid.Nil {
		return errors.New("rule_engine: SQLStore.WithEvaluationLock: repoID is the zero uuid")
	}
	if sha == "" {
		return errors.New("rule_engine: SQLStore.WithEvaluationLock: sha is empty")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("rule_engine: SQLStore.WithEvaluationLock: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	lockKey := evaluationLockKey(repoID, sha)
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return fmt.Errorf("rule_engine: SQLStore.WithEvaluationLock: acquire advisory lock: %w", err)
	}

	if err := fn(&txStore{tx: tx, schema: s.schema, steward: s.steward}); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rule_engine: SQLStore.WithEvaluationLock: commit: %w", err)
	}
	committed = true
	return nil
}

// evaluationLockKey hashes `(repo_id || ':' || sha)` to a
// 64-bit integer suitable for
// `pg_advisory_xact_lock(bigint)`. FNV-1a gives a stable
// hash across goroutines + Go runtimes.
func evaluationLockKey(repoID uuid.UUID, sha string) int64 {
	h := fnv.New64a()
	_, _ = h.Write(repoID.Bytes())
	_, _ = h.Write([]byte(":"))
	_, _ = h.Write([]byte(sha))
	return int64(h.Sum64())
}

// AppendEvaluation implements [Store] for the non-locked
// path -- the engine routes here via WithEvaluationLock,
// but tests / replay code may call it directly when no
// cross-process serialisation is required.
func (s *SQLStore) AppendEvaluation(ctx context.Context, run EvaluationRun, verdict EvaluationVerdict, findings []Finding) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rule_engine: SQLStore.AppendEvaluation: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := appendEvaluationInTx(ctx, tx, s.schema, run, verdict, findings); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("rule_engine: SQLStore.AppendEvaluation: commit: %w", err)
	}
	committed = true
	return nil
}

// scanFindingRow scans a finding row from either `*sql.Row`
// or `*sql.Rows`. Both implement `Scan(...)` with the same
// signature so we accept the narrow interface.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanFindingRow(r rowScanner) (Finding, error) {
	var (
		findingID    string
		evalRunID    string
		repoID       string
		sha          string
		scopeID      string
		ruleID       string
		ruleVersion  int
		policyVerID  string
		sampleIDsRaw []byte
		severity     string
		delta        string
		explanation  string
		createdAt    time.Time
	)
	if err := r.Scan(&findingID, &evalRunID, &repoID, &sha, &scopeID, &ruleID, &ruleVersion, &policyVerID, &sampleIDsRaw, &severity, &delta, &explanation, &createdAt); err != nil {
		return Finding{}, err
	}
	fid, err := uuid.FromString(findingID)
	if err != nil {
		return Finding{}, fmt.Errorf("parse finding_id=%q: %w", findingID, err)
	}
	erid, err := uuid.FromString(evalRunID)
	if err != nil {
		return Finding{}, fmt.Errorf("parse evaluation_run_id=%q: %w", evalRunID, err)
	}
	rid, err := uuid.FromString(repoID)
	if err != nil {
		return Finding{}, fmt.Errorf("parse repo_id=%q: %w", repoID, err)
	}
	sid, err := uuid.FromString(scopeID)
	if err != nil {
		return Finding{}, fmt.Errorf("parse scope_id=%q: %w", scopeID, err)
	}
	pvid, err := uuid.FromString(policyVerID)
	if err != nil {
		return Finding{}, fmt.Errorf("parse policy_version_id=%q: %w", policyVerID, err)
	}
	var sampleIDStrs []string
	if len(sampleIDsRaw) > 0 {
		if err := json.Unmarshal(sampleIDsRaw, &sampleIDStrs); err != nil {
			return Finding{}, fmt.Errorf("unmarshal metric_sample_ids: %w", err)
		}
	}
	sampleIDs := make([]uuid.UUID, 0, len(sampleIDStrs))
	for _, raw := range sampleIDStrs {
		id, err := uuid.FromString(raw)
		if err != nil {
			return Finding{}, fmt.Errorf("parse metric_sample_id=%q: %w", raw, err)
		}
		sampleIDs = append(sampleIDs, id)
	}
	return Finding{
		FindingID:       fid,
		EvaluationRunID: erid,
		RepoID:          rid,
		SHA:             sha,
		ScopeID:         sid,
		RuleID:          ruleID,
		RuleVersion:     ruleVersion,
		PolicyVersionID: pvid,
		MetricSampleIDs: sampleIDs,
		Severity:        steward.Severity(severity),
		Delta:           Delta(delta),
		ExplanationMD:   explanation,
		CreatedAt:       createdAt.UTC(),
	}, nil
}

// appendEvaluationInTx is the shared body used by both
// [SQLStore.AppendEvaluation] (auto-committing) and the
// txStore variant invoked under WithEvaluationLock.
func appendEvaluationInTx(ctx context.Context, tx *sql.Tx, schema string, run EvaluationRun, verdict EvaluationVerdict, findings []Finding) error {
	if run.EvaluationRunID == uuid.Nil {
		return errors.New("rule_engine: AppendEvaluation: run.EvaluationRunID is the zero uuid")
	}
	if verdict.VerdictID == uuid.Nil {
		return errors.New("rule_engine: AppendEvaluation: verdict.VerdictID is the zero uuid")
	}
	if verdict.EvaluationRunID == uuid.Nil {
		return errors.New("rule_engine: AppendEvaluation: verdict.EvaluationRunID FK is the zero uuid")
	}
	if verdict.EvaluationRunID != run.EvaluationRunID {
		return fmt.Errorf("rule_engine: AppendEvaluation: verdict.EvaluationRunID=%s does not match run.EvaluationRunID=%s", verdict.EvaluationRunID, run.EvaluationRunID)
	}

	qual := func(t string) string { return pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(t) }

	runStmt := fmt.Sprintf(
		`INSERT INTO %s
		   (evaluation_run_id, repo_id, sha, policy_version_id, caller, scope_id, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6::uuid, $7)`,
		qual("evaluation_run"),
	)
	// scope_id is nullable; pass driver-NULL when the run
	// is whole-SHA (every batch_refresh by construction
	// and eval.gate calls without a scope argument).
	var scopeArg any
	if run.ScopeID != nil {
		scopeArg = run.ScopeID.String()
	} else {
		scopeArg = nil
	}
	if _, err := tx.ExecContext(ctx, runStmt,
		run.EvaluationRunID.String(),
		run.RepoID.String(),
		run.SHA,
		run.PolicyVersionID.String(),
		string(run.Caller),
		scopeArg,
		run.CreatedAt.UTC(),
	); err != nil {
		return fmt.Errorf("rule_engine: AppendEvaluation: insert evaluation_run: %w", err)
	}

	verdictStmt := fmt.Sprintf(
		`INSERT INTO %s
		   (verdict_id, evaluation_run_id, verdict, degraded, degraded_reason, created_at)
		 VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6)`,
		qual("evaluation_verdict"),
	)
	if _, err := tx.ExecContext(ctx, verdictStmt,
		verdict.VerdictID.String(),
		verdict.EvaluationRunID.String(),
		string(verdict.Verdict),
		verdict.Degraded,
		verdict.DegradedReason,
		verdict.CreatedAt.UTC(),
	); err != nil {
		return fmt.Errorf("rule_engine: AppendEvaluation: insert evaluation_verdict: %w", err)
	}

	if len(findings) == 0 {
		return nil
	}

	findingStmt := fmt.Sprintf(
		`INSERT INTO %s
		   (finding_id, evaluation_run_id, repo_id, sha, scope_id,
		    rule_id, rule_version, policy_version_id, metric_sample_ids,
		    severity, delta, explanation_md, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10, $11, $12, $13)`,
		qual("finding"),
	)
	for _, f := range findings {
		if f.FindingID == uuid.Nil {
			return errors.New("rule_engine: AppendEvaluation: finding.FindingID is the zero uuid")
		}
		if f.EvaluationRunID != run.EvaluationRunID {
			return fmt.Errorf("rule_engine: AppendEvaluation: finding %s carries evaluation_run_id=%s but the run is %s",
				f.FindingID, f.EvaluationRunID, run.EvaluationRunID)
		}
		samples := f.MetricSampleIDs
		if samples == nil {
			samples = []uuid.UUID{}
		}
		sampleStrs := make([]string, len(samples))
		for i, s := range samples {
			sampleStrs[i] = s.String()
		}
		raw, err := json.Marshal(sampleStrs)
		if err != nil {
			return fmt.Errorf("rule_engine: AppendEvaluation: marshal metric_sample_ids: %w", err)
		}
		if _, err := tx.ExecContext(ctx, findingStmt,
			f.FindingID.String(),
			f.EvaluationRunID.String(),
			f.RepoID.String(),
			f.SHA,
			f.ScopeID.String(),
			f.RuleID,
			f.RuleVersion,
			f.PolicyVersionID.String(),
			string(raw),
			string(f.Severity),
			string(f.Delta),
			f.ExplanationMD,
			f.CreatedAt.UTC(),
		); err != nil {
			return fmt.Errorf("rule_engine: AppendEvaluation: insert finding %s: %w", f.FindingID, err)
		}
	}
	return nil
}

// Compile-time check that SQLStore satisfies [Store].
var _ Store = (*SQLStore)(nil)

// rowQuerier is the minimal interface satisfied by both
// `*sql.DB` and `*sql.Tx` so the rule / threshold lookups
// can be reused under [SQLStore] (auto-committing) and the
// [txStore] tx-bound variant without code duplication.
type rowQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func qualifyTable(schema, table string) string {
	return pq.QuoteIdentifier(schema) + "." + pq.QuoteIdentifier(table)
}

// getRuleRow fetches a single (rule_id, version) row from
// `clean_code.rule`. Returns [steward.Rule] populated from
// the canonical columns; the caller maps `sql.ErrNoRows`
// to its own not-found sentinel.
func getRuleRow(ctx context.Context, q rowQuerier, schema, ruleID string, version int) (steward.Rule, error) {
	stmt := fmt.Sprintf(
		`SELECT rule_id, version, pack_id, predicate_dsl,
		        severity_default::text, description_md, created_at
		 FROM %s
		 WHERE rule_id = $1 AND version = $2`,
		qualifyTable(schema, "rule"),
	)
	var (
		gotRuleID, packID, predicate, severity, description string
		gotVersion                                          int
		createdAt                                           time.Time
	)
	err := q.QueryRowContext(ctx, stmt, ruleID, version).Scan(
		&gotRuleID, &gotVersion, &packID, &predicate, &severity, &description, &createdAt,
	)
	if err != nil {
		return steward.Rule{}, fmt.Errorf("rule_engine: GetRule(%s, %d): %w", ruleID, version, err)
	}
	return steward.Rule{
		RuleID:          gotRuleID,
		Version:         gotVersion,
		PackID:          packID,
		PredicateDSL:    predicate,
		SeverityDefault: steward.Severity(severity),
		DescriptionMD:   description,
		CreatedAt:       createdAt.UTC(),
	}, nil
}

// getThresholdRow fetches a single threshold row from
// `clean_code.threshold` by primary key.
func getThresholdRow(ctx context.Context, q rowQuerier, schema string, thresholdID uuid.UUID) (steward.Threshold, error) {
	if thresholdID == uuid.Nil {
		return steward.Threshold{}, errors.New("rule_engine: GetThreshold: thresholdID is the zero uuid")
	}
	stmt := fmt.Sprintf(
		`SELECT threshold_id, metric_kind, scope_kind, op::text, value, created_at
		 FROM %s
		 WHERE threshold_id = $1`,
		qualifyTable(schema, "threshold"),
	)
	var (
		gotID, metricKind, scopeKind, op string
		value                            float64
		createdAt                        time.Time
	)
	err := q.QueryRowContext(ctx, stmt, thresholdID.String()).Scan(
		&gotID, &metricKind, &scopeKind, &op, &value, &createdAt,
	)
	if err != nil {
		return steward.Threshold{}, fmt.Errorf("rule_engine: GetThreshold(%s): %w", thresholdID, err)
	}
	parsedID, err := uuid.FromString(gotID)
	if err != nil {
		return steward.Threshold{}, fmt.Errorf("rule_engine: GetThreshold: parse threshold_id=%q: %w", gotID, err)
	}
	return steward.Threshold{
		ThresholdID: parsedID,
		MetricKind:  metricKind,
		ScopeKind:   scopeKind,
		Op:          op,
		Value:       value,
		CreatedAt:   createdAt.UTC(),
	}, nil
}

// dualQuerier is the minimal interface satisfied by both
// `*sql.DB` and `*sql.Tx` so [lookupRecentCanonicalRunQuery]
// can be reused under the auto-committing [SQLStore] path
// AND the tx-bound [txStore] path.
type dualQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// lookupRecentCanonicalRunQuery is the shared body used by
// [SQLStore.LookupRecentCanonicalRun] (auto-committing) and
// [txStore.LookupRecentCanonicalRun] (tx-scoped). It runs
// TWO queries:
//
//  1. Newest non-degraded `evaluation_run` matching
//     `(repo_id, sha, policy_version_id, caller)` -- joined
//     with `evaluation_verdict` so degraded short-circuits
//     are excluded.
//  2. Findings for that run, ordered by `finding_id` for
//     determinism.
//
// The `since` parameter is applied as `er.created_at >
// now() - $since` when non-zero. When `since == 0` the
// recency filter is disabled.
//
// CRITICAL: the production txStore path (rule_engine
// [Engine.runLocked]) MUST invoke this AFTER acquiring
// `pg_advisory_xact_lock` so a parallel replica's
// just-committed canonical row is observed under RC
// isolation -- the lock-release happens-before the second
// caller's lock-acquire, and the SELECT under the new tx
// sees the committed row. iter-6 evaluator item #3.
//
// SCOPE-AWARE MATCH (iter-7 evaluator item #2): the query
// filters on `evaluation_run.scope_id` using the null-safe
// `IS NOT DISTINCT FROM` operator. A `nil` `scopeID`
// argument matches rows with `scope_id IS NULL` (whole-SHA
// runs); a non-nil argument matches rows whose
// `scope_id = $5` exactly. The column was added by
// migration 0008.
func lookupRecentCanonicalRunQuery(ctx context.Context, q dualQuerier, schema string, repoID uuid.UUID, sha string, policyVersionID uuid.UUID, caller Caller, scopeID *uuid.UUID, since time.Duration) (RunResult, bool, error) {
	runTbl := qualifyTable(schema, "evaluation_run")
	verdictTbl := qualifyTable(schema, "evaluation_verdict")
	findingTbl := qualifyTable(schema, "finding")

	// Marshal scopeID for the `IS NOT DISTINCT FROM` arg.
	// PostgreSQL treats `NULL IS NOT DISTINCT FROM NULL`
	// as TRUE and `<uuid> IS NOT DISTINCT FROM NULL` as
	// FALSE, which is the semantics we want. We pass the
	// driver's `nil` so lib/pq translates to SQL NULL.
	var scopeArg any
	if scopeID != nil {
		scopeArg = scopeID.String()
	} else {
		scopeArg = nil
	}

	// Build the run+verdict query. `since == 0` disables
	// the recency filter; non-zero uses a parameterised
	// interval. Using `make_interval(secs := $N)` instead
	// of string-interpolating an interval literal keeps
	// the query injection-safe and lets the planner cache
	// a single prepared statement shape.
	var (
		runQ string
		args []any
	)
	if since <= 0 {
		runQ = fmt.Sprintf(
			`SELECT er.evaluation_run_id, ev.verdict_id, ev.verdict::text
			   FROM %s AS er
			   JOIN %s AS ev
			     ON ev.evaluation_run_id = er.evaluation_run_id
			  WHERE er.repo_id           = $1
			    AND er.sha               = $2
			    AND er.policy_version_id = $3
			    AND er.caller            = $4
			    AND er.scope_id IS NOT DISTINCT FROM $5::uuid
			    AND ev.degraded          = false
			  ORDER BY er.created_at DESC, er.evaluation_run_id DESC
			  LIMIT 1`,
			runTbl, verdictTbl,
		)
		args = []any{repoID.String(), sha, policyVersionID.String(), string(caller), scopeArg}
	} else {
		runQ = fmt.Sprintf(
			`SELECT er.evaluation_run_id, ev.verdict_id, ev.verdict::text
			   FROM %s AS er
			   JOIN %s AS ev
			     ON ev.evaluation_run_id = er.evaluation_run_id
			  WHERE er.repo_id           = $1
			    AND er.sha               = $2
			    AND er.policy_version_id = $3
			    AND er.caller            = $4
			    AND er.scope_id IS NOT DISTINCT FROM $5::uuid
			    AND ev.degraded          = false
			    AND er.created_at        > now() - make_interval(secs => $6::double precision)
			  ORDER BY er.created_at DESC, er.evaluation_run_id DESC
			  LIMIT 1`,
			runTbl, verdictTbl,
		)
		args = []any{repoID.String(), sha, policyVersionID.String(), string(caller), scopeArg, since.Seconds()}
	}

	var (
		runIDRaw     string
		verdictIDRaw string
		verdictText  string
	)
	if err := q.QueryRowContext(ctx, runQ, args...).Scan(&runIDRaw, &verdictIDRaw, &verdictText); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RunResult{}, false, nil
		}
		return RunResult{}, false, fmt.Errorf("rule_engine: LookupRecentCanonicalRun: run+verdict query: %w", err)
	}
	runID, err := uuid.FromString(runIDRaw)
	if err != nil {
		return RunResult{}, false, fmt.Errorf("rule_engine: LookupRecentCanonicalRun: parse evaluation_run_id=%q: %w", runIDRaw, err)
	}
	verdictID, err := uuid.FromString(verdictIDRaw)
	if err != nil {
		return RunResult{}, false, fmt.Errorf("rule_engine: LookupRecentCanonicalRun: parse verdict_id=%q: %w", verdictIDRaw, err)
	}

	// Fetch findings for the chosen run, ordered by
	// finding_id (lexicographic uuid) for determinism so
	// two replicas observing the same row return the same
	// FindingIDs slice ordering.
	findingsQ := fmt.Sprintf(
		`SELECT finding_id FROM %s WHERE evaluation_run_id = $1 ORDER BY finding_id ASC`,
		findingTbl,
	)
	rows, err := q.QueryContext(ctx, findingsQ, runID.String())
	if err != nil {
		return RunResult{}, false, fmt.Errorf("rule_engine: LookupRecentCanonicalRun: findings query: %w", err)
	}
	defer rows.Close()
	var findingIDs []uuid.UUID
	for rows.Next() {
		var fidRaw string
		if err := rows.Scan(&fidRaw); err != nil {
			return RunResult{}, false, fmt.Errorf("rule_engine: LookupRecentCanonicalRun: findings scan: %w", err)
		}
		fid, err := uuid.FromString(fidRaw)
		if err != nil {
			return RunResult{}, false, fmt.Errorf("rule_engine: LookupRecentCanonicalRun: parse finding_id=%q: %w", fidRaw, err)
		}
		findingIDs = append(findingIDs, fid)
	}
	if err := rows.Err(); err != nil {
		return RunResult{}, false, fmt.Errorf("rule_engine: LookupRecentCanonicalRun: findings rows: %w", err)
	}

	return RunResult{
		EvaluationRunID:     runID,
		EvaluationVerdictID: verdictID,
		FindingIDs:          findingIDs,
		Verdict:             Verdict(verdictText),
	}, true, nil
}

// LookupRecentCanonicalRun implements [Store]. Production
// path runs OUTSIDE the engine's `pg_advisory_xact_lock`
// (auto-committing read against the canonical pool); the
// engine routes via [SQLStore.WithEvaluationLock] which
// gives it a [txStore] handle whose
// [txStore.LookupRecentCanonicalRun] runs INSIDE the lock
// for the cross-replica RC-isolation guarantee. This
// auto-committing variant exists for tests and replay
// tooling that need the lookup outside an evaluation
// envelope.
func (s *SQLStore) LookupRecentCanonicalRun(ctx context.Context, repoID uuid.UUID, sha string, policyVersionID uuid.UUID, caller Caller, scopeID *uuid.UUID, since time.Duration) (RunResult, bool, error) {
	if err := validateLookupArgs(repoID, sha, policyVersionID, caller); err != nil {
		return RunResult{}, false, err
	}
	return lookupRecentCanonicalRunQuery(ctx, s.db, s.schema, repoID, sha, policyVersionID, caller, scopeID, since)
}

// validateLookupArgs centralises the argument-guard checks
// for both [SQLStore.LookupRecentCanonicalRun] and
// [txStore.LookupRecentCanonicalRun] so the two paths
// surface the exact same error messages.
func validateLookupArgs(repoID uuid.UUID, sha string, policyVersionID uuid.UUID, caller Caller) error {
	if repoID == uuid.Nil {
		return errors.New("rule_engine: LookupRecentCanonicalRun: repoID is the zero uuid")
	}
	if sha == "" {
		return errors.New("rule_engine: LookupRecentCanonicalRun: sha is empty")
	}
	if policyVersionID == uuid.Nil {
		return errors.New("rule_engine: LookupRecentCanonicalRun: policyVersionID is the zero uuid")
	}
	if !caller.IsValid() {
		return fmt.Errorf("rule_engine: LookupRecentCanonicalRun: caller=%q not in canonical set", caller)
	}
	return nil
}
