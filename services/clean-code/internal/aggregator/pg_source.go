package aggregator

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

const pgDefaultSchema = "clean_code"

// ErrPGSampleSourceNilDB surfaces a nil *sql.DB at composition-
// root wiring time.
var ErrPGSampleSourceNilDB = errors.New("aggregator: NewPGSampleSource: *sql.DB is nil")

// ErrPGSampleSourceEmptySchema surfaces an empty schema name at
// wiring time.
var ErrPGSampleSourceEmptySchema = errors.New("aggregator: NewPGSampleSourceWithSchema: schema is empty")

// PGSampleSource is the production [SampleSource]. It reads the
// active observation set via the canonical
// `metric_sample_active msa JOIN metric_sample ms ON ms.sample_id
// = msa.sample_id JOIN scope_binding sb ON sb.scope_id =
// ms.scope_id LEFT JOIN metric_retraction mr ON mr.sample_id =
// msa.sample_id WHERE mr.sample_id IS NULL AND ms.value IS NOT
// NULL` projection.
//
// `ms.scope_id` is the FK pointing at `scope_binding.scope_id`
// (migration 0002_measurement.up.sql); the JOIN against
// `scope_binding` is the only way to surface `scope_kind` since
// the metric_sample row carries only `scope_id`.
//
// The aggregator filters NaN / +-Inf in Go (no clean SQL
// predicate for those float states portable across PG versions);
// the SQL guard catches NULL values from degraded rows so they
// never leave the source.
type PGSampleSource struct {
	db     *sql.DB
	schema string
}

// NewPGSampleSource wraps `db` using the canonical `clean_code`
// schema.
func NewPGSampleSource(db *sql.DB) (*PGSampleSource, error) {
	return NewPGSampleSourceWithSchema(db, pgDefaultSchema)
}

// NewPGSampleSourceWithSchema is the test-friendly constructor.
// Tests inject a non-default schema (e.g.
// `clean_code_aggregator_test`) to keep their SQL assertions
// visibly diff-able from production.
func NewPGSampleSourceWithSchema(db *sql.DB, schema string) (*PGSampleSource, error) {
	if db == nil {
		return nil, ErrPGSampleSourceNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGSampleSourceEmptySchema
	}
	return &PGSampleSource{db: db, schema: schema}, nil
}

// qual returns `"<schema>"."<table>"` with both halves
// individually quoted via [pq.QuoteIdentifier].
func (s *PGSampleSource) qual(table string) string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(table)
}

// readActiveQuery is the canonical SELECT shape -- pinned as a
// package-level helper so the sqlmock test can match the exact
// statement bytes.
func (s *PGSampleSource) readActiveQuery() string {
	return fmt.Sprintf(
		`SELECT ms.repo_id, ms.metric_kind, sb.scope_kind::text, ms.value
		   FROM %s msa
		   JOIN %s ms ON ms.sample_id = msa.sample_id
		   JOIN %s sb ON sb.scope_id  = ms.scope_id
		   LEFT JOIN %s mr ON mr.sample_id = msa.sample_id
		  WHERE mr.sample_id IS NULL
		    AND ms.value IS NOT NULL`,
		s.qual("metric_sample_active"),
		s.qual("metric_sample"),
		s.qual("scope_binding"),
		s.qual("metric_retraction"),
	)
}

// ReadActive implements [SampleSource]. Rows whose value parses
// as NaN / +-Inf are skipped in the row-scan loop; they never
// surface to the aggregator. Per iter-2 evaluator finding #5
// the dedicated skip counters were removed from [Report] (only
// the survivor count is surfaced via [Report.ObservationsRead]);
// a future telemetry workstream MAY re-add per-skip-reason
// counters via a new method on this interface.
//
// Connection ownership: the *sql.DB is owned by the caller; this
// method does NOT call Close. Rows.Close is deferred so a partial
// iteration on error releases the underlying connection.
func (s *PGSampleSource) ReadActive(ctx context.Context) ([]Observation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	stmt := s.readActiveQuery()
	rows, err := s.db.QueryContext(ctx, stmt)
	if err != nil {
		return nil, fmt.Errorf("aggregator.PGSampleSource: query: %w", err)
	}
	defer rows.Close()
	out := make([]Observation, 0)
	for rows.Next() {
		var (
			repoIDStr  string
			metricKind string
			scopeKind  string
			value      sql.NullFloat64
		)
		if err := rows.Scan(&repoIDStr, &metricKind, &scopeKind, &value); err != nil {
			return nil, fmt.Errorf("aggregator.PGSampleSource: scan: %w", err)
		}
		if !value.Valid {
			// The SQL guard already filters `ms.value IS NOT NULL`
			// so a NULL slipping through is a writer bug. Skip
			// defensively rather than crashing the loop.
			continue
		}
		if math.IsNaN(value.Float64) || math.IsInf(value.Float64, 0) {
			continue
		}
		rid, err := uuid.FromString(repoIDStr)
		if err != nil {
			return nil, fmt.Errorf("aggregator.PGSampleSource: parse repo_id=%q: %w", repoIDStr, err)
		}
		out = append(out, Observation{
			RepoID:     rid,
			MetricKind: metricKind,
			ScopeKind:  scopeKind,
			Value:      value.Float64,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregator.PGSampleSource: rows: %w", err)
	}
	return out, nil
}
