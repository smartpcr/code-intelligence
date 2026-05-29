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
// `clean_code.metric_sample` and points the new sample at the
// `clean_code.metric_sample_active` pointer table inside a
// single transaction per [WriteSystemTierSamples] call.
//
// # Architecture-canonical SKIP-on-active contract
//
// Per architecture Sec 5.2.1 lines 1040-1048 ("for
// source='derived' rows the Cross-Repo Aggregator writes at
// most one row per quintuple per HEAD SHA per tick; if its
// tick lands on a SHA where an **active** derived row already
// exists (degraded or not), it **skips the insert** for that
// SHA and waits for the next HEAD SHA"), the writer's per-sample
// flow is:
//
//  1. EXISTS check: SELECT 1 FROM metric_sample_active msa
//                   LEFT JOIN metric_retraction mr
//                          ON mr.sample_id = msa.sample_id
//                   WHERE msa.repo_id=$1 AND msa.sha=$2
//                     AND msa.scope_id=$3
//                     AND msa.metric_kind=$4
//                     AND msa.metric_version=$5
//                     AND mr.sample_id IS NULL
//                   LIMIT 1
//     The `mr.sample_id IS NULL` anti-join treats a retracted
//     active row as ABSENT (the retraction is a tombstone per
//     Sec 5.2.1 lines 1023-1030), so a tick following a
//     `mgmt.retract_sample` correctly writes a fresh active
//     derived row at the same quintuple.
//  2. If step 1 returned a row, SKIP both inserts for this
//     sample. The writer's `SystemTierSamplesSkipped` counter
//     ticks up by one and the per-call invariant
//     "len(written) + len(skipped) == len(samples)" holds.
//  3. If step 1 returned zero rows, INSERT into metric_sample
//     (fresh sample_id from the composer) AND INSERT into
//     metric_sample_active. The active-pointer insert is a
//     bare INSERT (no ON CONFLICT) because the EXISTS check
//     just confirmed there is no row at the quintuple; a
//     concurrent writer racing in between would surface as a
//     PG UNIQUE-violation error, which is the right behaviour
//     under the single-replica invariant (see the binary's
//     package doc -- two replicas writing system rows is a
//     deployment misconfiguration).
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
// # Transactional scope
//
// All three statements (EXISTS check + metric_sample INSERT +
// metric_sample_active INSERT) for the batch run inside ONE
// PG transaction so a partial write cannot leave readers with
// a metric_sample row whose active pointer does not exist (or,
// conversely, an active pointer whose sample_id references a
// metric_sample row that was never inserted). Either ALL
// non-skipped samples persist atomically or NONE do.
//
// # Role grants
//
// Migration `0004_roles.up.sql:392-394` grants the
// `clean_code_xrepo_aggregator` role
// `INSERT, SELECT` on `metric_sample` and
// `INSERT, SELECT, UPDATE` on `metric_sample_active`. These
// are the EXACT grants the writer's SELECT (EXISTS check) +
// INSERT pattern requires; no additional migration is needed.
// (The UPDATE grant is unused now that we no longer
// ON CONFLICT DO UPDATE on the active pointer but stays as
// defense-in-depth for future writers.)
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

// existsActiveStmt returns the EXISTS-check SQL pinned to
// architecture Sec 5.2.1 lines 1040-1048: "active" means a
// row in `metric_sample_active` whose `sample_id` is NOT in
// `metric_retraction`. The LEFT JOIN + `mr.sample_id IS NULL`
// is the same canonical anti-join used by [PGSampleSource]
// and [PGSystemTierInputSource]. Returns ZERO rows when the
// quintuple has no active derived row (writer proceeds with
// INSERTs) or ONE row when an active derived row already
// exists (writer skips both INSERTs for this sample).
func (w *PGSystemTierWriter) existsActiveStmt() string {
	return fmt.Sprintf(
		`SELECT 1
		   FROM %s msa
		   LEFT JOIN %s mr ON mr.sample_id = msa.sample_id
		  WHERE msa.repo_id = $1 AND msa.sha = $2
		    AND msa.scope_id = $3 AND msa.metric_kind = $4
		    AND msa.metric_version = $5
		    AND mr.sample_id IS NULL
		  LIMIT 1`,
		w.qual("metric_sample_active"),
		w.qual("metric_retraction"),
	)
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

// insertMetricSampleActiveStmt returns the prepared-statement
// shape for one [SystemTierSample] -> `metric_sample_active`
// INSERT. Per the architecture-canonical SKIP-on-active
// contract documented on [PGSystemTierWriter], this is a BARE
// INSERT (no ON CONFLICT clause): the caller has already
// verified no active row exists at the quintuple via
// [existsActiveStmt]. A racing concurrent writer would surface
// as a PK-violation error, which is the correct behaviour
// under the single-replica deployment invariant (see the
// binary's package doc comment for the rationale).
func (w *PGSystemTierWriter) insertMetricSampleActiveStmt() string {
	return fmt.Sprintf(
		`INSERT INTO %s (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
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
// Per the architecture-canonical SKIP-on-active contract
// (Sec 5.2.1 lines 1040-1048; documented on
// [PGSystemTierWriter]): for each sample, the writer first
// SELECTs for an existing active row at the quintuple. If
// one exists, BOTH inserts (metric_sample AND
// metric_sample_active) are SKIPPED for that sample; the
// writer's prior tick already landed a row for this SHA and
// the architecture forbids appending a second active derived
// row at the same quintuple. If none exists, both inserts
// run atomically.
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

	existsStmt, err := tx.PrepareContext(ctx, w.existsActiveStmt())
	if err != nil {
		return fmt.Errorf("aggregator.PGSystemTierWriter: prepare exists-active check: %w", err)
	}
	defer existsStmt.Close()

	insertStmt, err := tx.PrepareContext(ctx, w.insertMetricSampleStmt())
	if err != nil {
		return fmt.Errorf("aggregator.PGSystemTierWriter: prepare metric_sample insert: %w", err)
	}
	defer insertStmt.Close()

	insertActiveStmt, err := tx.PrepareContext(ctx, w.insertMetricSampleActiveStmt())
	if err != nil {
		return fmt.Errorf("aggregator.PGSystemTierWriter: prepare metric_sample_active insert: %w", err)
	}
	defer insertActiveStmt.Close()

	for i := range samples {
		s := &samples[i]

		// Architecture-canonical EXISTS check. The
		// retraction anti-join lets a tick following a
		// `mgmt.retract_sample` correctly write a fresh
		// active row at the same quintuple (the retracted
		// row is a tombstone per Sec 5.2.1 lines 1023-1030).
		var dummy int
		switch err := existsStmt.QueryRowContext(ctx,
			s.RepoID, s.SHA, s.ScopeID, s.MetricKind, s.MetricVersion,
		).Scan(&dummy); {
		case err == nil:
			// Active row already exists -- skip both
			// inserts for this sample per architecture
			// Sec 5.2.1 lines 1040-1048. Continue to the
			// next sample.
			continue
		case errors.Is(err, sql.ErrNoRows):
			// No active row at this quintuple -- proceed
			// with the two inserts below.
		default:
			return fmt.Errorf("aggregator.PGSystemTierWriter: exists-active check (sample_id=%s, metric_kind=%s): %w", s.SampleID, s.MetricKind, err)
		}

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
		if _, err := insertActiveStmt.ExecContext(ctx,
			s.RepoID, s.SHA, s.ScopeID, s.MetricKind, s.MetricVersion, s.SampleID,
		); err != nil {
			return fmt.Errorf("aggregator.PGSystemTierWriter: insert metric_sample_active (sample_id=%s, metric_kind=%s): %w", s.SampleID, s.MetricKind, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("aggregator.PGSystemTierWriter: COMMIT: %w", err)
	}
	committed = true
	return nil
}
