package aggregator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// ErrPGSnapshotWriterNilDB surfaces a nil *sql.DB at
// composition-root wiring time.
var ErrPGSnapshotWriterNilDB = errors.New("aggregator: NewPGSnapshotWriter: *sql.DB is nil")

// ErrPGSnapshotWriterEmptySchema surfaces an empty schema name
// at wiring time.
var ErrPGSnapshotWriterEmptySchema = errors.New("aggregator: NewPGSnapshotWriterWithSchema: schema is empty")

// PGSnapshotWriter is the production [SnapshotWriter]. Every
// [WriteSnapshots] call:
//
//  1. BEGINs a single transaction.
//  2. INSERTs every [RepoMetricSnapshotRow] via a prepared
//     statement (one prepare + N exec).
//  3. INSERTs every [CrossRepoPercentileRow] via a prepared
//     statement.
//  4. INSERTs every [PortfolioSnapshotRow] via a prepared
//     statement.
//  5. COMMITs.
//
// All three INSERTs share the same transaction so a partial
// write cannot leave readers with one tick's
// `repo_metric_snapshot` rows and another tick's
// `cross_repo_percentile` rows. The Postgres role
// `clean_code_xrepo_aggregator` has `INSERT, SELECT` on the
// three snapshot tables and explicit `REVOKE UPDATE, DELETE`
// (migration 0004_roles.up.sql lines 395-397 / 416-418); the
// writer NEVER issues UPDATE or DELETE per architecture G6
// (snapshot rows are append-only derivative views).
//
// `snapshot_id` / `percentile_id` / `portfolio_snapshot_id` are
// omitted from the INSERT column list -- their PK columns have
// `DEFAULT gen_random_uuid()` so the database generates them
// (migration 0002_measurement.up.sql lines 560-561 / 595-596 /
// 627-628).
type PGSnapshotWriter struct {
	db     *sql.DB
	schema string
}

// NewPGSnapshotWriter wraps `db` using the canonical `clean_code`
// schema.
func NewPGSnapshotWriter(db *sql.DB) (*PGSnapshotWriter, error) {
	return NewPGSnapshotWriterWithSchema(db, pgDefaultSchema)
}

// NewPGSnapshotWriterWithSchema is the test-friendly schema-
// isolated constructor.
func NewPGSnapshotWriterWithSchema(db *sql.DB, schema string) (*PGSnapshotWriter, error) {
	if db == nil {
		return nil, ErrPGSnapshotWriterNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGSnapshotWriterEmptySchema
	}
	return &PGSnapshotWriter{db: db, schema: schema}, nil
}

func (w *PGSnapshotWriter) qual(table string) string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(table)
}

// scopeKindEnum returns the schema-qualified, properly-quoted
// name of the `scope_kind` enum type. The PG cast in every
// snapshot-table INSERT (`$N::<schema>.scope_kind`) MUST match
// the schema the writer is configured against so the
// schema-injected test path (e.g. `clean_code_aggregator_test`)
// resolves the enum from its own namespace rather than the
// canonical `clean_code` schema (which may not exist in the
// test DB at all). Fixes iter-2 evaluator finding #3.
func (w *PGSnapshotWriter) scopeKindEnum() string {
	return pq.QuoteIdentifier(w.schema) + ".scope_kind"
}

// insertRepoMetricSnapshotStmt returns the prepared-statement
// shape used for each [RepoMetricSnapshotRow]. Eight positional
// args: repo_id, metric_kind, scope_kind, count, mean, p50, p90,
// p99, built_at -- nine total (snapshot_id is server-generated).
func (w *PGSnapshotWriter) insertRepoMetricSnapshotStmt() string {
	return fmt.Sprintf(
		`INSERT INTO %s (repo_id, metric_kind, scope_kind, count, mean, p50, p90, p99, built_at) VALUES ($1, $2, $3::%s, $4, $5, $6, $7, $8, $9)`,
		w.qual("repo_metric_snapshot"),
		w.scopeKindEnum(),
	)
}

// insertCrossRepoPercentileStmt returns the prepared-statement
// shape used for each [CrossRepoPercentileRow]. Seven positional
// args: metric_kind, scope_kind, histogram_json, p50, p90, p99,
// built_at.
func (w *PGSnapshotWriter) insertCrossRepoPercentileStmt() string {
	return fmt.Sprintf(
		`INSERT INTO %s (metric_kind, scope_kind, histogram_json, p50, p90, p99, built_at) VALUES ($1, $2::%s, $3::jsonb, $4, $5, $6, $7)`,
		w.qual("cross_repo_percentile"),
		w.scopeKindEnum(),
	)
}

// insertPortfolioSnapshotStmt returns the prepared-statement
// shape used for each [PortfolioSnapshotRow]. Five positional
// args: metric_kind, scope_kind, repo_count, aggregate_json,
// built_at.
func (w *PGSnapshotWriter) insertPortfolioSnapshotStmt() string {
	return fmt.Sprintf(
		`INSERT INTO %s (metric_kind, scope_kind, repo_count, aggregate_json, built_at) VALUES ($1, $2::%s, $3, $4::jsonb, $5)`,
		w.qual("portfolio_snapshot"),
		w.scopeKindEnum(),
	)
}

// WriteSnapshots implements [SnapshotWriter]. Atomic over the
// three INSERT batches via a single transaction. The three
// `scope_kind::<schema>.scope_kind` casts are explicit and use
// the WRITER's configured schema (see [PGSnapshotWriter.scopeKindEnum])
// so the driver doesn't have to guess the enum mapping for a
// text-typed Go value, and so the schema-injected test path
// (e.g. `clean_code_aggregator_test.scope_kind`) resolves the
// enum from its own namespace.
func (w *PGSnapshotWriter) WriteSnapshots(ctx context.Context, snap Snapshots) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("aggregator.PGSnapshotWriter: BEGIN: %w", err)
	}
	// Roll back on any error path. Commit zeros out the
	// rollback (Tx.Rollback after Commit returns ErrTxDone, which
	// we ignore).
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if len(snap.RepoMetric) > 0 {
		stmt, err := tx.PrepareContext(ctx, w.insertRepoMetricSnapshotStmt())
		if err != nil {
			return fmt.Errorf("aggregator.PGSnapshotWriter: prepare repo_metric_snapshot insert: %w", err)
		}
		for _, r := range snap.RepoMetric {
			if _, err := stmt.ExecContext(ctx,
				r.RepoID, r.MetricKind, r.ScopeKind,
				r.Count, r.Mean, r.P50, r.P90, r.P99,
				r.BuiltAt,
			); err != nil {
				stmt.Close()
				return fmt.Errorf("aggregator.PGSnapshotWriter: insert repo_metric_snapshot (repo_id=%s, metric_kind=%s, scope_kind=%s): %w", r.RepoID, r.MetricKind, r.ScopeKind, err)
			}
		}
		stmt.Close()
	}

	if len(snap.CrossRepoPercent) > 0 {
		stmt, err := tx.PrepareContext(ctx, w.insertCrossRepoPercentileStmt())
		if err != nil {
			return fmt.Errorf("aggregator.PGSnapshotWriter: prepare cross_repo_percentile insert: %w", err)
		}
		for _, r := range snap.CrossRepoPercent {
			if _, err := stmt.ExecContext(ctx,
				r.MetricKind, r.ScopeKind, string(r.HistogramJSON),
				r.P50, r.P90, r.P99,
				r.BuiltAt,
			); err != nil {
				stmt.Close()
				return fmt.Errorf("aggregator.PGSnapshotWriter: insert cross_repo_percentile (metric_kind=%s, scope_kind=%s): %w", r.MetricKind, r.ScopeKind, err)
			}
		}
		stmt.Close()
	}

	if len(snap.Portfolio) > 0 {
		stmt, err := tx.PrepareContext(ctx, w.insertPortfolioSnapshotStmt())
		if err != nil {
			return fmt.Errorf("aggregator.PGSnapshotWriter: prepare portfolio_snapshot insert: %w", err)
		}
		for _, r := range snap.Portfolio {
			if _, err := stmt.ExecContext(ctx,
				r.MetricKind, r.ScopeKind, r.RepoCount, string(r.AggregateJSON),
				r.BuiltAt,
			); err != nil {
				stmt.Close()
				return fmt.Errorf("aggregator.PGSnapshotWriter: insert portfolio_snapshot (metric_kind=%s, scope_kind=%s): %w", r.MetricKind, r.ScopeKind, err)
			}
		}
		stmt.Close()
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("aggregator.PGSnapshotWriter: COMMIT: %w", err)
	}
	committed = true
	return nil
}
