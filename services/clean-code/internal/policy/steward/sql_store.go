package steward

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// DefaultSchema is the canonical PostgreSQL schema the
// CLEAN-CODE service owns (tech-spec C9 / Sec 8.1.3). All
// Policy / rules sub-store rows live under this schema name.
const DefaultSchema = "clean_code"

// pgSQLStateUniqueViolation is the PostgreSQL SQLSTATE for a
// UNIQUE / PRIMARY KEY violation; mapped to
// [ErrDuplicateRulePack] / [ErrDuplicateRule] depending on the
// constraint.
const pgSQLStateUniqueViolation = "23505"

// pgSQLStateForeignKeyViolation is the PostgreSQL SQLSTATE for
// a FK violation. The `policy_activation.policy_version_id` FK
// surfaces this when an activation references a missing
// policy version.
const pgSQLStateForeignKeyViolation = "23503"

// SQLStore is the production [Store] implementation. It uses
// `database/sql` + `lib/pq` and the canonical
// `clean_code.policy_version` / `policy_activation` /
// `rule_pack` / `rule` tables from migration 0003.
//
// The caller owns the `*sql.DB` lifecycle -- SQLStore does not
// call `Close`.
type SQLStore struct {
	db     *sql.DB
	schema string
}

// NewSQLStore wraps db using the canonical [DefaultSchema].
func NewSQLStore(db *sql.DB) (*SQLStore, error) {
	return NewSQLStoreWithSchema(db, DefaultSchema)
}

// NewSQLStoreWithSchema is the test-friendly constructor;
// callers inject an isolated PostgreSQL schema so the
// integration tests don't race with the migrate round-trip.
func NewSQLStoreWithSchema(db *sql.DB, schema string) (*SQLStore, error) {
	if db == nil {
		return nil, errors.New("steward: NewSQLStore: *sql.DB is nil")
	}
	if schema == "" {
		return nil, errors.New("steward: NewSQLStoreWithSchema: schema is empty")
	}
	return &SQLStore{db: db, schema: schema}, nil
}

// qualify quotes the schema identifier and joins it with the
// table name. The `pq.QuoteIdentifier` call guarantees a
// schema containing special characters never produces a
// syntactically-broken statement.
func (s *SQLStore) qualify(table string) string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(table)
}

// InsertPolicyVersion appends pv. Maps unique-violation
// SQLSTATE 23505 to a sentinel-wrapped error.
func (s *SQLStore) InsertPolicyVersion(ctx context.Context, pv PolicyVersion) error {
	ruleRefsJSON, err := json.Marshal(pv.RuleRefs)
	if err != nil {
		return fmt.Errorf("steward: SQLStore.InsertPolicyVersion: marshal rule_refs: %w", err)
	}
	thresholdRefsJSON, err := json.Marshal(pv.ThresholdRefs)
	if err != nil {
		return fmt.Errorf("steward: SQLStore.InsertPolicyVersion: marshal threshold_refs: %w", err)
	}
	refactorJSON, err := json.Marshal(pv.RefactorWeights)
	if err != nil {
		return fmt.Errorf("steward: SQLStore.InsertPolicyVersion: marshal refactor_weights: %w", err)
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s
		   (policy_version_id, name, rule_refs, threshold_refs, refactor_weights, signature, created_at)
		 VALUES ($1, $2, $3::jsonb, $4::jsonb, $5::jsonb, $6, $7)`,
		s.qualify("policy_version"))
	_, err = s.db.ExecContext(ctx, stmt,
		pv.PolicyVersionID.String(),
		pv.Name,
		string(ruleRefsJSON),
		string(thresholdRefsJSON),
		string(refactorJSON),
		pv.Signature,
		pv.CreatedAt.UTC(),
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && string(pqErr.Code) == pgSQLStateUniqueViolation {
			return fmt.Errorf("steward: SQLStore.InsertPolicyVersion: duplicate policy_version_id=%s: %w", pv.PolicyVersionID, err)
		}
		return fmt.Errorf("steward: SQLStore.InsertPolicyVersion: %w", err)
	}
	return nil
}

// GetPolicyVersion reads the row keyed by id and re-canonicalises
// the JSONB columns into typed slices so the caller can verify
// the signature against the round-trip-stable bytes.
func (s *SQLStore) GetPolicyVersion(ctx context.Context, id uuid.UUID) (PolicyVersion, error) {
	stmt := fmt.Sprintf(
		`SELECT policy_version_id, name, rule_refs, threshold_refs, refactor_weights, signature, created_at
		 FROM %s WHERE policy_version_id = $1`,
		s.qualify("policy_version"))
	var (
		idText    string
		name      string
		ruleRefsB []byte
		threshB   []byte
		refactorB []byte
		signature []byte
		pv        PolicyVersion
	)
	row := s.db.QueryRowContext(ctx, stmt, id.String())
	if err := row.Scan(&idText, &name, &ruleRefsB, &threshB, &refactorB, &signature, &pv.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PolicyVersion{}, fmt.Errorf("%w: policy_version_id=%s", ErrUnknownPolicyVersion, id)
		}
		return PolicyVersion{}, fmt.Errorf("steward: SQLStore.GetPolicyVersion: scan: %w", err)
	}
	parsed, err := uuid.FromString(idText)
	if err != nil {
		return PolicyVersion{}, fmt.Errorf("steward: SQLStore.GetPolicyVersion: bad policy_version_id %q: %w", idText, err)
	}
	pv.PolicyVersionID = parsed
	pv.Name = name
	pv.Signature = signature
	if err := json.Unmarshal(ruleRefsB, &pv.RuleRefs); err != nil {
		return PolicyVersion{}, fmt.Errorf("steward: SQLStore.GetPolicyVersion: unmarshal rule_refs: %w", err)
	}
	if err := json.Unmarshal(threshB, &pv.ThresholdRefs); err != nil {
		return PolicyVersion{}, fmt.Errorf("steward: SQLStore.GetPolicyVersion: unmarshal threshold_refs: %w", err)
	}
	if err := json.Unmarshal(refactorB, &pv.RefactorWeights); err != nil {
		return PolicyVersion{}, fmt.Errorf("steward: SQLStore.GetPolicyVersion: unmarshal refactor_weights: %w", err)
	}
	return pv, nil
}

// InsertPolicyActivation appends pa. Maps FK violation
// SQLSTATE 23503 to [ErrUnknownPolicyVersion].
func (s *SQLStore) InsertPolicyActivation(ctx context.Context, pa PolicyActivation) error {
	stmt := fmt.Sprintf(
		`INSERT INTO %s (activation_id, policy_version_id, activated_by, created_at)
		 VALUES ($1, $2, $3, $4)`,
		s.qualify("policy_activation"))
	_, err := s.db.ExecContext(ctx, stmt,
		pa.ActivationID.String(),
		pa.PolicyVersionID.String(),
		pa.ActivatedBy,
		pa.CreatedAt.UTC(),
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) {
			switch string(pqErr.Code) {
			case pgSQLStateForeignKeyViolation:
				return fmt.Errorf("%w: policy_version_id=%s: %v", ErrUnknownPolicyVersion, pa.PolicyVersionID, err)
			case pgSQLStateUniqueViolation:
				return fmt.Errorf("steward: SQLStore.InsertPolicyActivation: duplicate activation_id=%s: %w", pa.ActivationID, err)
			}
		}
		return fmt.Errorf("steward: SQLStore.InsertPolicyActivation: %w", err)
	}
	return nil
}

// LatestActivation returns the activation row with the largest
// `(created_at, activation_id)` tuple. ORDER BY pins
// deterministic tie-breaking when two rows share the same
// `created_at` (a concern raised in the rubber-duck critique).
func (s *SQLStore) LatestActivation(ctx context.Context) (PolicyActivation, bool, error) {
	stmt := fmt.Sprintf(
		`SELECT activation_id, policy_version_id, activated_by, created_at
		 FROM %s
		 ORDER BY created_at DESC, activation_id DESC
		 LIMIT 1`,
		s.qualify("policy_activation"))
	var (
		actID    string
		policyID string
		actor    string
		pa       PolicyActivation
	)
	row := s.db.QueryRowContext(ctx, stmt)
	if err := row.Scan(&actID, &policyID, &actor, &pa.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PolicyActivation{}, false, nil
		}
		return PolicyActivation{}, false, fmt.Errorf("steward: SQLStore.LatestActivation: %w", err)
	}
	a, err := uuid.FromString(actID)
	if err != nil {
		return PolicyActivation{}, false, fmt.Errorf("steward: SQLStore.LatestActivation: bad activation_id %q: %w", actID, err)
	}
	p, err := uuid.FromString(policyID)
	if err != nil {
		return PolicyActivation{}, false, fmt.Errorf("steward: SQLStore.LatestActivation: bad policy_version_id %q: %w", policyID, err)
	}
	pa.ActivationID = a
	pa.PolicyVersionID = p
	pa.ActivatedBy = actor
	return pa, true, nil
}

// InsertRulePackAndRules runs the pack insert + every rule
// insert under a single transaction (per rubber-duck #3). Any
// SQL error rolls back the entire batch so an append-only
// store never carries a partial pack.
func (s *SQLStore) InsertRulePackAndRules(ctx context.Context, pack RulePack, rules []Rule) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("steward: SQLStore.InsertRulePackAndRules: begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()

	packStmt := fmt.Sprintf(
		`INSERT INTO %s (pack_id, version, display_name, description_md, created_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		s.qualify("rule_pack"))
	_, err = tx.ExecContext(ctx, packStmt,
		pack.PackID, pack.Version, pack.DisplayName, pack.DescriptionMD, pack.CreatedAt.UTC(),
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && string(pqErr.Code) == pgSQLStateUniqueViolation {
			return fmt.Errorf("%w: pack_id=%s version=%d: %v", ErrDuplicateRulePack, pack.PackID, pack.Version, err)
		}
		return fmt.Errorf("steward: SQLStore.InsertRulePackAndRules: insert rule_pack: %w", err)
	}

	ruleStmt := fmt.Sprintf(
		`INSERT INTO %s
		   (rule_id, version, pack_id, predicate_dsl, severity_default, description_md, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.qualify("rule"))
	for _, r := range rules {
		_, err = tx.ExecContext(ctx, ruleStmt,
			r.RuleID, r.Version, r.PackID, r.PredicateDSL, string(r.SeverityDefault), r.DescriptionMD, r.CreatedAt.UTC(),
		)
		if err != nil {
			var pqErr *pq.Error
			if errors.As(err, &pqErr) && string(pqErr.Code) == pgSQLStateUniqueViolation {
				return fmt.Errorf("%w: rule_id=%s version=%d: %v", ErrDuplicateRule, r.RuleID, r.Version, err)
			}
			return fmt.Errorf("steward: SQLStore.InsertRulePackAndRules: insert rule %s/%d: %w", r.RuleID, r.Version, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("steward: SQLStore.InsertRulePackAndRules: commit: %w", err)
	}
	return nil
}

// GetRulePack returns the row keyed by `(packID, version)`.
func (s *SQLStore) GetRulePack(ctx context.Context, packID string, version int) (RulePack, bool, error) {
	stmt := fmt.Sprintf(
		`SELECT pack_id, version, display_name, description_md, created_at
		 FROM %s WHERE pack_id = $1 AND version = $2`,
		s.qualify("rule_pack"))
	var pack RulePack
	row := s.db.QueryRowContext(ctx, stmt, packID, version)
	if err := row.Scan(&pack.PackID, &pack.Version, &pack.DisplayName, &pack.DescriptionMD, &pack.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RulePack{}, false, nil
		}
		return RulePack{}, false, fmt.Errorf("steward: SQLStore.GetRulePack: %w", err)
	}
	return pack, true, nil
}

// ListRulesForPack returns every Rule whose `pack_id` matches
// `packID`, sorted by `(rule_id ASC, version ASC)`.
func (s *SQLStore) ListRulesForPack(ctx context.Context, packID string) ([]Rule, error) {
	stmt := fmt.Sprintf(
		`SELECT rule_id, version, pack_id, predicate_dsl, severity_default, description_md, created_at
		 FROM %s WHERE pack_id = $1
		 ORDER BY rule_id ASC, version ASC`,
		s.qualify("rule"))
	rows, err := s.db.QueryContext(ctx, stmt, packID)
	if err != nil {
		return nil, fmt.Errorf("steward: SQLStore.ListRulesForPack: %w", err)
	}
	defer rows.Close()
	out := make([]Rule, 0)
	for rows.Next() {
		var r Rule
		var sev string
		if err := rows.Scan(&r.RuleID, &r.Version, &r.PackID, &r.PredicateDSL, &sev, &r.DescriptionMD, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("steward: SQLStore.ListRulesForPack: scan: %w", err)
		}
		r.SeverityDefault = Severity(sev)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("steward: SQLStore.ListRulesForPack: rows: %w", err)
	}
	return out, nil
}

// RuleExists reports whether a `(rule_id, version)` row is
// present in `clean_code.rule`. SELECT 1 plus LIMIT 1 so the
// query stops at the first hit; the composite PK guarantees at
// most one match anyway.
func (s *SQLStore) RuleExists(ctx context.Context, ruleID string, version int) (bool, error) {
	stmt := fmt.Sprintf(
		`SELECT 1 FROM %s WHERE rule_id = $1 AND version = $2 LIMIT 1`,
		s.qualify("rule"))
	var one int
	err := s.db.QueryRowContext(ctx, stmt, ruleID, version).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("steward: SQLStore.RuleExists: %w", err)
	}
	return true, nil
}

// ThresholdExists reports whether a `threshold_id` row is
// present in `clean_code.threshold`.
func (s *SQLStore) ThresholdExists(ctx context.Context, id uuid.UUID) (bool, error) {
	stmt := fmt.Sprintf(
		`SELECT 1 FROM %s WHERE threshold_id = $1 LIMIT 1`,
		s.qualify("threshold"))
	var one int
	err := s.db.QueryRowContext(ctx, stmt, id.String()).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("steward: SQLStore.ThresholdExists: %w", err)
	}
	return true, nil
}

// InsertThreshold appends t to `clean_code.threshold`. The
// Stage 5.2 canonical write surface does NOT expose a
// `policy.publish_threshold` verb; this primitive exists so
// tests can seed FK targets and so a future operator bootstrap
// tool can register thresholds outside the policy.* surface.
func (s *SQLStore) InsertThreshold(ctx context.Context, t Threshold) error {
	if t.ThresholdID == uuid.Nil {
		return fmt.Errorf("steward: SQLStore.InsertThreshold: threshold_id is the zero uuid")
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s (threshold_id, metric_kind, scope_kind, op, value, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		s.qualify("threshold"))
	_, err := s.db.ExecContext(ctx, stmt,
		t.ThresholdID.String(),
		t.MetricKind,
		t.ScopeKind,
		t.Op,
		t.Value,
		t.CreatedAt.UTC(),
	)
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && string(pqErr.Code) == pgSQLStateUniqueViolation {
			return fmt.Errorf("steward: SQLStore.InsertThreshold: duplicate threshold_id=%s: %w", t.ThresholdID, err)
		}
		return fmt.Errorf("steward: SQLStore.InsertThreshold: %w", err)
	}
	return nil
}

// Compile-time check that SQLStore satisfies Store.
var _ Store = (*SQLStore)(nil)
