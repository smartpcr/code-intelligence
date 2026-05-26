package evaluator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// SQLSampleReadiness is the production
// [SampleReadinessReader] implementation. It reads
// `clean_code.commit.scan_status` for the requested
// `(repo_id, sha)` and reports readiness IFF the value
// equals `'scanned'`.
//
// Per architecture Sec 5.1.2: the post-scan dispatcher
// flips `scan_status` from `'queued'/'running'` to
// `'scanned'` only when every metric sampler has finalised
// its rows for the SHA. Reading `'scanned'` is therefore
// the canonical witness that the gate may safely invoke
// the rule engine.
//
// Per rubber-duck #4 (carried over from Stage 5.7 iter 3):
// the gate MUST NOT accept a `samplesReady bool` from its
// caller; only this reader -- backed by the persistent
// commit row -- is authoritative. The handler's per-request
// `repo_id, sha` pair flows in unchanged.
type SQLSampleReadiness struct {
	db     *sql.DB
	schema string
}

// SQLSampleReadinessConfig configures the production
// [SQLSampleReadiness].
type SQLSampleReadinessConfig struct {
	// DB is the `*sql.DB` handle. Required. Production
	// composition root MUST authenticate this handle with
	// a role that has SELECT on `clean_code.commit` --
	// `clean_code_evaluator` is the canonical choice per
	// Stage 1.5 / migrations/0004_roles.up.sql.
	DB *sql.DB
	// Schema is the PostgreSQL schema name -- defaults to
	// `"clean_code"` when empty.
	Schema string
}

// NewSQLSampleReadiness wires the production
// [SQLSampleReadiness] readiness reader.
func NewSQLSampleReadiness(cfg SQLSampleReadinessConfig) (*SQLSampleReadiness, error) {
	if cfg.DB == nil {
		return nil, errors.New("evaluator: NewSQLSampleReadiness: DB is nil")
	}
	schema := cfg.Schema
	if schema == "" {
		schema = "clean_code"
	}
	return &SQLSampleReadiness{db: cfg.DB, schema: schema}, nil
}

// SamplesReady reports whether the commit row for the
// `(repoID, sha)` pair carries `scan_status = 'scanned'`.
// A missing commit row returns `(false, nil)` -- treated
// the same as `scan_status IN ('queued', 'running')` (the
// dispatcher has not stamped the SHA as scanned yet, so
// the gate must take the degraded `samples_pending` path
// rather than racing the scanner).
//
// A query / driver error returns `(false, err)`; the gate
// must NOT degrade silently when the readiness query
// itself failed -- that's a different failure mode
// (likely a configuration / connectivity bug).
func (r *SQLSampleReadiness) SamplesReady(ctx context.Context, repoID uuid.UUID, sha string) (bool, error) {
	if repoID == uuid.Nil {
		return false, errors.New("evaluator: SamplesReady: repoID is the zero uuid")
	}
	if sha == "" {
		return false, errors.New("evaluator: SamplesReady: sha is empty")
	}
	qual := pq.QuoteIdentifier(r.schema) + "." + pq.QuoteIdentifier("commit")
	stmt := fmt.Sprintf(
		`SELECT scan_status::text FROM %s WHERE repo_id = $1 AND sha = $2`,
		qual,
	)
	row := r.db.QueryRowContext(ctx, stmt, repoID.String(), sha)
	var status string
	if err := row.Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("evaluator: SamplesReady: scan: %w", err)
	}
	return status == "scanned", nil
}
