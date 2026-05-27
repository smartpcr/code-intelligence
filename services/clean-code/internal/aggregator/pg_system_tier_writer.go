package aggregator

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lib/pq"
)

// ErrPGSystemTierWriterNilDB surfaces a nil *sql.DB at
// composition-root wiring time.
var ErrPGSystemTierWriterNilDB = errors.New("aggregator: NewPGSystemTierWriter: *sql.DB is nil")

// ErrPGSystemTierWriterEmptySchema surfaces an empty schema
// name at wiring time.
var ErrPGSystemTierWriterEmptySchema = errors.New("aggregator: NewPGSystemTierWriterWithSchema: schema is empty")

// PGSystemTierWriter is the production [SystemTierWriter]. It
// persists composer-emitted system-tier samples into
// `clean_code.metric_sample` and re-points
// `clean_code.metric_sample_active` to the new sample inside
// a single transaction per [WriteSystemTierSamples] call.
//
// # Insert shape
//
// Per migration `0002_measurement.up.sql:257-380` (metric_sample
// DDL) the writer INSERTs the explicit column tuple:
//
//	(sample_id, repo_id, sha, scope_id, metric_kind, metric_version,
//	 value, pack, source, degraded, degraded_reason, producer_run_id,
//	 attrs_json)
//
// The two server-defaulted columns (`created_at` /
// `sample_date_bucket`) are omitted. The pack / source /
// degraded_reason columns are cast to their canonical ENUM
// types so the driver doesn't have to guess the mapping for a
// text-typed Go value (mirrors the established
// [PGSnapshotWriter] pattern).
//
// # Active-pointer semantics
//
// Per migration `0002_measurement.up.sql:506-537` (metric_sample_active
// DDL) the writer maintains the active pointer with:
//
//	INSERT INTO metric_sample_active
//	    (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
//	VALUES ($1, $2, $3, $4, $5, $6)
//	ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version)
//	    DO UPDATE SET sample_id = EXCLUDED.sample_id
//
// The `DO UPDATE SET sample_id = EXCLUDED.sample_id` shape
// mirrors the established Metric Ingestor pattern (see the
// `metric_sample_active` table COMMENT at
// `0002_measurement.up.sql:539-552`) and the architecture's
// "retract-then-reinsert => re-point" semantics (Sec 5.2.1
// line 1041 / the COMMENT at 547-552). Each composer-tick at
// the same `(repo_id, sha, scope_id, metric_kind,
// metric_version)` quintuple writes a NEW `metric_sample` row
// (with a fresh `sample_id`) and re-points the active pointer
// to it; the prior `metric_sample` row remains in the
// append-only history as audit evidence of the prior
// composition, matching architecture G3 row-immutability.
//
// # Transactional scope
//
// All inserts (metric_sample then metric_sample_active) for
// the batch run inside ONE PG transaction so a partial write
// cannot leave readers with a metric_sample row whose active
// pointer does not exist (or, conversely, an active pointer
// whose sample_id references a metric_sample row that was
// never inserted). Either ALL samples persist atomically or
// NONE do.
//
// # Role grants
//
// Migration `0004_roles.up.sql:392-394` grants the
// `clean_code_xrepo_aggregator` role
// `INSERT, SELECT` on `metric_sample` and
// `INSERT, SELECT, UPDATE` on `metric_sample_active`. These
// are the EXACT grants the writer's INSERT + ON CONFLICT
// DO UPDATE pattern requires; no additional migration is
// needed.
//
// # FK precondition
//
// Migration `0011_seed_system_tier_metric_kinds.up.sql` seeds
// the seven canonical system-tier `(metric_kind,
// metric_version)` rows into `clean_code.metric_kind` so the
// writer's `metric_sample_metric_kind_fk` composite FK
// resolves at INSERT time.
type PGSystemTierWriter struct {
	db     *sql.DB
	schema string
}

// NewPGSystemTierWriter wraps `db` using the canonical
// `clean_code` schema.
func NewPGSystemTierWriter(db *sql.DB) (*PGSystemTierWriter, error) {
	return NewPGSystemTierWriterWithSchema(db, pgDefaultSchema)
}

// NewPGSystemTierWriterWithSchema is the test-friendly
// schema-isolated constructor. Tests inject a non-default
// schema (e.g. `clean_code_aggregator_test`) to keep their
// SQL assertions visibly diff-able from production.
func NewPGSystemTierWriterWithSchema(db *sql.DB, schema string) (*PGSystemTierWriter, error) {
	if db == nil {
		return nil, ErrPGSystemTierWriterNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGSystemTierWriterEmptySchema
	}
	return &PGSystemTierWriter{db: db, schema: schema}, nil
}

// qual returns `"<schema>"."<table>"` with both halves
// individually quoted via [pq.QuoteIdentifier].
func (w *PGSystemTierWriter) qual(table string) string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(table)
}

// qualType returns `"<schema>"."<typename>"` for an enum
// or domain type reference inside a column CAST.
func (w *PGSystemTierWriter) qualType(typename string) string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(typename)
}

// insertMetricSampleStmt returns the prepared-statement shape
// for one [SystemTierSample] -> `metric_sample` INSERT.
// Thirteen positional args (see doc on this method).
func (w *PGSystemTierWriter) insertMetricSampleStmt() string {
	return fmt.Sprintf(
		`INSERT INTO %s (
		    sample_id, repo_id, sha, scope_id, metric_kind, metric_version,
		    value, pack, source, degraded, degraded_reason,
		    producer_run_id, attrs_json
		 ) VALUES (
		    $1, $2, $3, $4, $5, $6,
		    $7, $8::%s, $9::%s, $10, $11::%s,
		    $12, $13::jsonb
		 )`,
		w.qual("metric_sample"),
		w.qualType("metric_sample_pack"),
		w.qualType("metric_sample_source"),
		w.qualType("degraded_reason"),
	)
}

// upsertMetricSampleActiveStmt returns the prepared-statement
// shape for one [SystemTierSample] -> `metric_sample_active`
// INSERT-or-update. The `DO UPDATE SET sample_id` shape
// repoints an existing active row to the newly inserted
// `metric_sample` row per architecture Sec 5.2.1's
// retract-then-reinsert semantics.
func (w *PGSystemTierWriter) upsertMetricSampleActiveStmt() string {
	return fmt.Sprintf(
		`INSERT INTO %s (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version)
		     DO UPDATE SET sample_id = EXCLUDED.sample_id`,
		w.qual("metric_sample_active"),
	)
}

// WriteSystemTierSamples implements [SystemTierWriter]. All
// samples are persisted as a single PG transaction. Each
// sample's invariants are validated via
// [validateSystemTierSample] BEFORE the transaction begins
// so a malformed sample fails fast without leaving the
// database in a partial state.
//
// An empty `samples` slice is a no-op (no transaction, no
// error) -- matches the in-memory writer's contract.
func (w *PGSystemTierWriter) WriteSystemTierSamples(ctx context.Context, samples []SystemTierSample) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(samples) == 0 {
		return nil
	}

	// Validate the whole batch up front so we don't have to
	// roll back a partial transaction on the first invariant
	// violation -- the centralised validator is the same
	// check the composer applies at compose time, so any
	// violation here is a writer-caller bug rather than a
	// composer bug.
	for i := range samples {
		if err := validateSystemTierSample(&samples[i]); err != nil {
			return fmt.Errorf("aggregator.PGSystemTierWriter: sample %d invariant violated: %w", i, err)
		}
	}

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("aggregator.PGSystemTierWriter: BEGIN: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	insertStmt, err := tx.PrepareContext(ctx, w.insertMetricSampleStmt())
	if err != nil {
		return fmt.Errorf("aggregator.PGSystemTierWriter: prepare metric_sample insert: %w", err)
	}
	defer insertStmt.Close()

	upsertStmt, err := tx.PrepareContext(ctx, w.upsertMetricSampleActiveStmt())
	if err != nil {
		return fmt.Errorf("aggregator.PGSystemTierWriter: prepare metric_sample_active upsert: %w", err)
	}
	defer upsertStmt.Close()

	for i := range samples {
		s := &samples[i]
		// Translate the composer's domain shape into the
		// driver's wire shape:
		//   *float64 -> sql.NullFloat64 (NULL when degraded)
		//   ""        -> sql.NullString  (NULL when not degraded)
		//   nil map   -> "{}"            (jsonb-castable empty object)
		var nullValue sql.NullFloat64
		if s.Value != nil {
			nullValue = sql.NullFloat64{Float64: *s.Value, Valid: true}
		}
		var nullReason sql.NullString
		if s.DegradedReason != "" {
			nullReason = sql.NullString{String: s.DegradedReason, Valid: true}
		}
		attrsJSON := []byte("{}")
		if len(s.Attrs) > 0 {
			j, err := json.Marshal(s.Attrs)
			if err != nil {
				return fmt.Errorf("aggregator.PGSystemTierWriter: marshal attrs (sample_id=%s): %w", s.SampleID, err)
			}
			attrsJSON = j
		}
		if _, err := insertStmt.ExecContext(ctx,
			s.SampleID, s.RepoID, s.SHA, s.ScopeID, s.MetricKind, s.MetricVersion,
			nullValue, s.Pack, s.Source, s.Degraded, nullReason,
			s.ProducerRunID, string(attrsJSON),
		); err != nil {
			return fmt.Errorf("aggregator.PGSystemTierWriter: insert metric_sample (sample_id=%s, metric_kind=%s): %w", s.SampleID, s.MetricKind, err)
		}
		if _, err := upsertStmt.ExecContext(ctx,
			s.RepoID, s.SHA, s.ScopeID, s.MetricKind, s.MetricVersion, s.SampleID,
		); err != nil {
			return fmt.Errorf("aggregator.PGSystemTierWriter: upsert metric_sample_active (sample_id=%s, metric_kind=%s): %w", s.SampleID, s.MetricKind, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("aggregator.PGSystemTierWriter: COMMIT: %w", err)
	}
	committed = true
	return nil
}
