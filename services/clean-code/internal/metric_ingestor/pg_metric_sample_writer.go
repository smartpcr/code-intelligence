package metric_ingestor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// pgMetricSampleTable is the unqualified `metric_sample`
// table name the writer targets. Schema-qualified at
// statement-build time via [pq.QuoteIdentifier].
const pgMetricSampleTable = "metric_sample"

// pgScanRunTableForGuard is the unqualified table name the
// per-batch producer-status guard SELECT targets. Pinned as
// a named constant so the guard SQL has one source of truth.
const pgScanRunTableForGuard = "scan_run"

// ErrPGMetricSampleWriterNilDB surfaces a nil *sql.DB at
// composition-root wiring time.
var ErrPGMetricSampleWriterNilDB = errors.New("metric_ingestor: NewPGMetricSampleWriter: *sql.DB is nil")

// ErrPGMetricSampleWriterEmptySchema surfaces an empty
// schema name at composition-root wiring time.
var ErrPGMetricSampleWriterEmptySchema = errors.New("metric_ingestor: NewPGMetricSampleWriterWithSchema: schema is empty")

// ErrPGMetricSampleWriterBatchMixedProducerRunIDs is returned
// by [PGMetricSampleWriter.WriteBatch] when records in a
// single batch carry different [MetricSampleRecord.ProducerRunID]
// values. The post-finalize guard (`SELECT status FROM
// scan_run WHERE scan_run_id = $1`) is per-batch, and
// `foundation_dispatch.go` stamps `ProducerRunID = scanRun.ID`
// uniformly on every record in a dispatch -- mixing
// producer_run_ids inside one batch is a programmer bug, not
// a recoverable state.
var ErrPGMetricSampleWriterBatchMixedProducerRunIDs = errors.New("metric_ingestor: PGMetricSampleWriter.WriteBatch: records in a single batch carry different ProducerRunIDs (every record in a batch MUST share the same producer scan_run_id)")

// ErrPGMetricSampleWriterPostFinalizeWrite is returned by
// [PGMetricSampleWriter.WriteBatch] when the producer
// `scan_run.status` is no longer `'running'` at the moment
// the per-batch guard SELECT runs (immediately after BEGIN
// and BEFORE the per-row INSERTs). This is a POST-FINALIZE
// ONLY fence: it refuses NEW batches that arrive after the
// state machine has transitioned the run out of `'running'`.
//
// What this fence does NOT do (deliberately):
//
//   - It does NOT stop a scanner goroutine that is already
//     mid-WriteBatch when [PGScanRunStore.FinalizeScanRun]
//     issues its UPDATE. If the scanner goroutine wins the
//     race and the guard SELECT observes `status='running'`
//     before finalize commits, the batch is allowed to
//     complete; the FOR SHARE row lock then makes the
//     finalize UPDATE wait behind that batch's commit.
//   - It does NOT close the application-level race between
//     the state machine's hard-timeout `return` from
//     `runScan` (`state.go:1210-1224`) and the finalize
//     UPDATE -- a scanner goroutine that calls WriteBatch
//     in that window still commits its batch.
//
// The single invariant this sentinel encodes is precisely:
// "no new `metric_sample` write COMMITS once
// `scan_run.status != 'running'` has become VISIBLE to a
// guard-SELECT statement". That is a strictly weaker
// guarantee than "stop the scan when timeout fires" -- the
// runbook's "failure reason is logged, not persisted"
// contract continues to acknowledge that timeout-failed
// runs may have accumulated samples before the fence
// activated.
//
// Operators reading this in a postmortem should pair the
// sentinel hit with the
// `metric_ingestor.scan_run.terminal_status` log line at
// `state.go:97` to determine WHICH finalize transition
// preceded the refused batch.
var ErrPGMetricSampleWriterPostFinalizeWrite = errors.New("metric_ingestor: PGMetricSampleWriter.WriteBatch: producer scan_run.status is no longer 'running' (the run has been finalized; refusing post-finalize write)")

// ErrPGMetricSampleWriterUnknownProducerRunID is returned by
// [PGMetricSampleWriter.WriteBatch] when the per-batch guard
// SELECT finds no `scan_run` row for the records'
// [MetricSampleRecord.ProducerRunID]. Distinct sentinel from
// [ErrPGMetricSampleWriterPostFinalizeWrite] so callers can
// tell "run was finalized and is now non-running" apart from
// "run never existed in the first place" (which is always a
// data-integrity bug).
var ErrPGMetricSampleWriterUnknownProducerRunID = errors.New("metric_ingestor: PGMetricSampleWriter.WriteBatch: producer scan_run_id has no row in scan_run (programmer / data-integrity bug)")

// PGMetricSampleWriter is the production
// PostgreSQL-backed [MetricSampleWriter]. WriteBatch
// inserts every record in a single transaction; on any
// error the whole batch is rolled back (no partial-write
// surface).
//
// The writer targets `clean_code.metric_sample` per
// architecture Sec 5.2.1 / migration `0002_measurement.up.sql`
// lines 257-380. Insert column list:
//
//	(sample_id, repo_id, sha, scope_id,
//	 metric_kind, metric_version,
//	 value, pack, source,
//	 producer_run_id, attrs_json)
//
// The writer does NOT name `degraded` (default false) /
// `degraded_reason` (NULL when degraded=false) /
// `created_at` (DEFAULT now()) / `sample_date_bucket`
// (GENERATED). Per G3 the foundation/ingested rows this
// writer produces are always `degraded=false`; the CHECK
// `metric_sample_value_present_unless_degraded` (migration
// line 367) requires a non-null value here -- the writer
// rejects records with NaN Value as the conservative
// shape (a NaN would technically satisfy "NOT NULL" but
// would poison downstream aggregations).
//
// # Active-row UPSERT is NOT in this writer
//
// The `metric_sample_active` pointer relation (migration
// line 506) is updated by a separate writer (Phase 3.3,
// `stage-active-row-and-retraction-writer`). This writer
// is the SAMPLE-side INSERT only. Conflating the two
// would couple foundation-tier persistence to the
// pointer-relation transaction lifecycle, which the
// architecture explicitly separates.
//
// # Post-finalize write fence (Stage 3.2 iter 17, scope
// clarified iter 18)
//
// Before issuing the per-row INSERTs, WriteBatch issues a
// per-batch guard SELECT that:
//
//   - reads the producer `scan_run.status` row via
//     `SELECT status FROM <schema>.scan_run
//      WHERE scan_run_id = $1 FOR SHARE`
//   - acquires a SHARE row-lock so a concurrent
//     [PGScanRunStore.FinalizeScanRun] UPDATE waits behind
//     this batch's commit;
//   - returns [ErrPGMetricSampleWriterPostFinalizeWrite] when
//     the status is no longer `'running'`;
//   - returns [ErrPGMetricSampleWriterUnknownProducerRunID]
//     when no row exists for the producer_run_id.
//
// SCOPE -- this is a post-finalize-only fence. It refuses
// NEW WriteBatch calls whose guard SELECT observes a
// non-running status. It does NOT stop a WriteBatch that
// was already past the guard SELECT when the state machine
// decided to finalize the run; that batch holds a FOR SHARE
// row-lock on the producer row, so the FinalizeScanRun
// UPDATE blocks behind it -- the in-flight batch commits
// first and its rows are visible. See
// [ErrPGMetricSampleWriterPostFinalizeWrite] for the
// precise wording of the invariant this sentinel encodes
// and what it does NOT promise.
//
// The guard is per-batch, NOT per-row, because
// [foundation_dispatch.go:401-413] stamps
// `ProducerRunID = scanRun.ID` uniformly on every record in
// a dispatch -- so a single SELECT per batch is exactly the
// right granularity. Mixed-producer batches are rejected up
// front with
// [ErrPGMetricSampleWriterBatchMixedProducerRunIDs].
type PGMetricSampleWriter struct {
	db     *sql.DB
	schema string
}

// NewPGMetricSampleWriter wraps `db` using the canonical
// `clean_code` schema. Returns an error on misconfiguration
// so the surface fires at composition-root wire-up rather
// than at first write.
func NewPGMetricSampleWriter(db *sql.DB) (*PGMetricSampleWriter, error) {
	return NewPGMetricSampleWriterWithSchema(db, pgScanRunDefaultSchema)
}

// NewPGMetricSampleWriterWithSchema is the test-friendly
// constructor: tests inject a non-default schema name to
// keep their SQL assertions visibly diff-able from
// production.
func NewPGMetricSampleWriterWithSchema(db *sql.DB, schema string) (*PGMetricSampleWriter, error) {
	if db == nil {
		return nil, ErrPGMetricSampleWriterNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGMetricSampleWriterEmptySchema
	}
	return &PGMetricSampleWriter{db: db, schema: schema}, nil
}

// qualifyMetricSample returns `"<schema>"."metric_sample"`.
func (w *PGMetricSampleWriter) qualifyMetricSample() string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(pgMetricSampleTable)
}

// qualifyScanRunForGuard returns `"<schema>"."scan_run"` for
// the per-batch post-finalize guard SELECT.
func (w *PGMetricSampleWriter) qualifyScanRunForGuard() string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(pgScanRunTableForGuard)
}

// WriteBatch implements [MetricSampleWriter]. Inserts the
// entire batch inside one transaction; on any per-row
// error the whole transaction rolls back (atomic-batch
// semantics).
//
// Empty `records` slice is a no-op per the
// [MetricSampleWriter] contract (no transaction opened,
// no error returned).
//
// Each row's `attrs_json` is encoded as a Postgres jsonb
// literal via [json.Marshal]; a nil map encodes as
// `null` which the column accepts (it is nullable).
func (w *PGMetricSampleWriter) WriteBatch(ctx context.Context, records []MetricSampleRecord) error {
	if len(records) == 0 {
		return nil
	}

	// Validate batch-level producer_run_id invariant BEFORE
	// opening a transaction so a programmer bug surfaces
	// without any DB round-trip. Every record in a single
	// foundation dispatch shares `ProducerRunID = scanRun.ID`
	// (see `foundation_dispatch.go:401-413`); mixing producer
	// IDs in one batch is a recoverable failure only by
	// re-batching, which the writer refuses to do silently.
	producerRunID, perr := batchProducerRunID(records)
	if perr != nil {
		return perr
	}

	insertSQL := fmt.Sprintf(
		`INSERT INTO %s
		    (sample_id, repo_id, sha, scope_id,
		     metric_kind, metric_version,
		     value, pack, source,
		     producer_run_id, attrs_json)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		w.qualifyMetricSample(),
	)

	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGMetricSampleWriter.BeginTx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Per-batch post-finalize fence (Stage 3.2 iter 17).
	// `SELECT ... FOR SHARE` acquires a row-level SHARE lock
	// on the producer's `scan_run` row. PostgreSQL's UPDATE
	// (the lock mode taken by [PGScanRunStore.FinalizeScanRun])
	// implicitly acquires FOR NO KEY UPDATE which conflicts
	// with FOR SHARE -- so a concurrent finalize is forced to
	// wait until this batch commits or rolls back. The next
	// batch then sees the updated status and is refused.
	//
	// The guard is a per-batch SELECT (not per-row) because
	// the foundation dispatcher stamps a single producer_run_id
	// across the batch. Once status flips off `'running'`,
	// every subsequent batch is refused regardless of whether
	// the scanner goroutine ignored `ctx.Done()`.
	guardSQL := fmt.Sprintf(
		`SELECT status FROM %s WHERE scan_run_id = $1 FOR SHARE`,
		w.qualifyScanRunForGuard(),
	)
	var observedStatus string
	if err := tx.QueryRowContext(ctx, guardSQL, producerRunID).Scan(&observedStatus); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("metric_ingestor: PGMetricSampleWriter producer_run_id=%s: %w", producerRunID, ErrPGMetricSampleWriterUnknownProducerRunID)
		}
		return fmt.Errorf("metric_ingestor: PGMetricSampleWriter guard SELECT: %w", err)
	}
	if observedStatus != string(ScanRunStatusRunning) {
		return fmt.Errorf("metric_ingestor: PGMetricSampleWriter producer_run_id=%s observed_status=%q: %w", producerRunID, observedStatus, ErrPGMetricSampleWriterPostFinalizeWrite)
	}

	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGMetricSampleWriter.Prepare INSERT: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for i, rec := range records {
		if err := validateMetricSampleRecord(rec); err != nil {
			return fmt.Errorf("metric_ingestor: PGMetricSampleWriter records[%d] invalid: %w", i, err)
		}
		attrsJSON, err := encodeAttrsJSON(rec.Attrs)
		if err != nil {
			return fmt.Errorf("metric_ingestor: PGMetricSampleWriter records[%d] attrs encode: %w", i, err)
		}
		if _, err := stmt.ExecContext(ctx,
			rec.SampleID,
			rec.RepoID,
			rec.SHA,
			rec.ScopeID,
			rec.MetricKind,
			rec.MetricVersion,
			rec.Value,
			string(rec.Pack),
			string(rec.Source),
			rec.ProducerRunID,
			attrsJSON,
		); err != nil {
			return fmt.Errorf("metric_ingestor: PGMetricSampleWriter records[%d] INSERT: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metric_ingestor: PGMetricSampleWriter Commit: %w", err)
	}
	return nil
}

// batchProducerRunID returns the producer_run_id shared by
// every record in the batch, or
// [ErrPGMetricSampleWriterBatchMixedProducerRunIDs] when the
// records do NOT share a producer_run_id. The first record's
// id is taken as the canonical value; any subsequent record
// disagreeing causes a refusal. Empty input is a precondition
// (caller checks `len(records) == 0` first).
func batchProducerRunID(records []MetricSampleRecord) (uuid.UUID, error) {
	canonical := records[0].ProducerRunID
	for i := 1; i < len(records); i++ {
		if records[i].ProducerRunID != canonical {
			return uuid.Nil, fmt.Errorf("metric_ingestor: PGMetricSampleWriter records[0].ProducerRunID=%s, records[%d].ProducerRunID=%s: %w",
				canonical, i, records[i].ProducerRunID, ErrPGMetricSampleWriterBatchMixedProducerRunIDs)
		}
	}
	return canonical, nil
}

// validateMetricSampleRecord rejects records the schema's
// CHECK constraints or FKs would reject AT THE DB layer,
// preserving the "fail fast at the writer" contract --
// surfacing the bug at the application boundary is
// cheaper than surfacing it as a PG SQLSTATE.
func validateMetricSampleRecord(rec MetricSampleRecord) error {
	if rec.SampleID.IsNil() {
		return errors.New("SampleID is the zero UUID")
	}
	if rec.RepoID.IsNil() {
		return errors.New("RepoID is the zero UUID")
	}
	if rec.SHA == "" {
		return errors.New("SHA is empty")
	}
	if rec.ScopeID.IsNil() {
		return errors.New("ScopeID is the zero UUID")
	}
	if rec.MetricKind == "" {
		return errors.New("MetricKind is empty")
	}
	if rec.MetricVersion < 1 {
		return fmt.Errorf("MetricVersion=%d (want >= 1)", rec.MetricVersion)
	}
	if rec.ProducerRunID.IsNil() {
		return errors.New("ProducerRunID is the zero UUID")
	}
	if rec.Pack == "" {
		return errors.New("Pack is empty")
	}
	if rec.Source == "" {
		return errors.New("Source is empty")
	}
	// `value` must be a real number: a NaN would satisfy
	// NOT NULL but would poison downstream aggregations.
	if isNaN(rec.Value) {
		return errors.New("Value is NaN")
	}
	return nil
}

// encodeAttrsJSON encodes `attrs` as a jsonb-safe byte
// slice. A nil/empty map encodes as the JSON null literal
// so the column receives SQL NULL (the column is
// nullable per migration 0002 line 314).
func encodeAttrsJSON(attrs map[string]string) (interface{}, error) {
	if len(attrs) == 0 {
		return nil, nil
	}
	b, err := json.Marshal(attrs)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// isNaN returns true iff `v` is the IEEE-754 NaN. Pulled
// into a tiny helper so the validation site stays compact
// and the import surface is contained.
func isNaN(v float64) bool {
	return v != v
}
