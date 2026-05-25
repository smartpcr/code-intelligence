package metric_ingestor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// pgMetricSampleTable is the unqualified `metric_sample`
// table name the writer targets. Schema-qualified at
// statement-build time via [pq.QuoteIdentifier].
const pgMetricSampleTable = "metric_sample"

// pgMetricSampleActiveTable is the unqualified
// `metric_sample_active` side relation the writer UPSERTs to
// after each `metric_sample` INSERT. The composite FK
// `metric_sample_active_sample_consistent_fk` (migration
// `0002_measurement.up.sql:525-529`) pins the active-row
// pointer's denormalized quintuple to the referenced sample's
// own columns, so the UPSERT MUST land in the SAME transaction
// AFTER the INSERT that creates the target row -- the FK is
// immediate (not DEFERRABLE) and a UPSERT-before-INSERT order
// would fail with foreign_key_violation.
const pgMetricSampleActiveTable = "metric_sample_active"

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
// # Active-row UPSERT (Stage 3.3 -- this writer)
//
// After each `metric_sample` INSERT, WriteBatch UPSERTs the
// matching `metric_sample_active` pointer row inside the
// SAME transaction. The UPSERT shape is:
//
//	INSERT INTO clean_code.metric_sample_active
//	    (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
//	VALUES (...)
//	ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version)
//	DO UPDATE SET sample_id = EXCLUDED.sample_id
//
// The `metric_sample_active` table's PRIMARY KEY on the
// quintuple (migration `0002_measurement.up.sql:519`) carries
// the architecture-mandated "at most one active row per
// quintuple" invariant (architecture Sec 5.2.1 G2 lines
// 991-1003 / tech-spec Sec 7.1.b lines 1070-1119 / Sec 10A pin
// lines 1659-1675). The UPSERT is the ONLY canonical write
// shape -- no procedural `swap_active` verb / trigger /
// stored function exists in the canonical model (iter 1
// evaluator item 1).
//
// Re-ingest semantics (tech-spec Sec 7.1.b):
//
//   - First write at a given quintuple INSERTs the pointer
//     row pointing at the new sample_id (ON CONFLICT path
//     not taken).
//   - Re-ingest at the same quintuple either no-ops at the
//     application layer (idempotent computation skips
//     WriteBatch entirely) OR appends a NEW `metric_sample`
//     row and the UPSERT's ON CONFLICT branch re-points
//     `metric_sample_active.sample_id` to the new sample.
//     The PRIOR `metric_sample` row stays in the table
//     forever per G3 / C2 (no UPDATE / no DELETE on
//     `metric_sample`).
//
// Retraction semantics (Stage 3.4 owns the verb, but this
// writer's contract is pinned here for clarity): the
// retraction verb appends a `metric_retraction(sample_id)`
// row and LEAVES the `metric_sample_active` pointer row in
// place (tech-spec REVOKEs `DELETE` on
// `clean_code.metric_sample_active` from BOTH writer roles
// at `0004_roles.up.sql:415` -- the pointer is never
// removed). Readers filter retracted samples by joining
// through `metric_retraction` per architecture Sec 5.2.2
// lines 1035-1037. On a subsequent rescan at the same SHA,
// this writer's UPSERT re-points the pointer to the new
// `sample_id`; the prior retracted `metric_sample` row
// stays as a tombstone per G3.
//
// Atomicity: INSERT into `metric_sample` and UPSERT into
// `metric_sample_active` share ONE transaction per
// WriteBatch call. A failure on the UPSERT rolls back the
// preceding INSERT(s) so the active-row index is never left
// in a half-state.
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

// qualifyMetricSampleActive returns
// `"<schema>"."metric_sample_active"` for the active-row
// UPSERT statement.
func (w *PGMetricSampleWriter) qualifyMetricSampleActive() string {
	return pq.QuoteIdentifier(w.schema) + "." + pq.QuoteIdentifier(pgMetricSampleActiveTable)
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
// Transaction shape (Stage 3.3):
//
//  1. BEGIN.
//  2. Per-batch post-finalize guard SELECT
//     (`scan_run FOR SHARE`).
//  3. Prepare INSERT into `metric_sample`.
//  4. For each record: validate + EXEC the INSERT.
//  5. Prepare INSERT ... ON CONFLICT ... DO UPDATE on
//     `metric_sample_active`.
//  6. For each record: EXEC the UPSERT (re-points the
//     active-row pointer to the new `sample_id` if a prior
//     pointer existed, else INSERTs a fresh pointer).
//  7. COMMIT.
//
// Empty `records` slice is a no-op per the
// [MetricSampleWriter] contract (no transaction opened,
// no error returned).
//
// Each row's `attrs_json` is encoded as a Postgres jsonb
// literal via [json.Marshal]; a nil map encodes as
// `null` which the column accepts (it is nullable).
//
// Atomicity: a failure on ANY of the INSERT or UPSERT EXECs
// rolls back the entire batch (including any preceding
// INSERTs into `metric_sample`). This guarantees the
// `metric_sample_active` PRIMARY KEY uniqueness invariant
// holds across the writer's commit boundary -- a partial
// write cannot land a `metric_sample` row whose active
// pointer is missing or stale.
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

	// Sort a defensive copy of `records` by the active-row
	// quintuple (plus sample_id as a stable tiebreaker) BEFORE
	// the INSERT / UPSERT passes. This pins a deterministic
	// row-lock acquisition order on `metric_sample_active` so
	// two concurrent `WriteBatch` calls with overlapping
	// quintuples (e.g. parallel scans of related SHAs) cannot
	// cross-lock and deadlock. The caller-owned slice is left
	// untouched -- callers may iterate `records` after
	// WriteBatch returns and observe the original order.
	//
	// This also pins the metric_sample INSERT order so the
	// two passes see the same iteration, satisfying the
	// composite FK `metric_sample_active_sample_consistent_fk`
	// trivially (the metric_sample row for record N is
	// inserted before the metric_sample_active UPSERT for
	// record N).
	sorted := make([]MetricSampleRecord, len(records))
	copy(sorted, records)
	sortMetricSampleRecordsForActiveRow(sorted)

	insertSQL := fmt.Sprintf(
		`INSERT INTO %s
		    (sample_id, repo_id, sha, scope_id,
		     metric_kind, metric_version,
		     value, pack, source,
		     producer_run_id, attrs_json)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		w.qualifyMetricSample(),
	)

	// Active-row UPSERT (Stage 3.3). The ON CONFLICT target
	// is the quintuple PRIMARY KEY on `metric_sample_active`
	// (migration `0002_measurement.up.sql:519`). On conflict
	// the writer re-points the pointer to the new sample_id;
	// the composite FK back to `metric_sample` is satisfied
	// because the INSERT into `metric_sample` for this
	// `sample_id` already committed-on-statement-end above.
	//
	// IMPORTANT: no procedural `swap_active` verb / trigger /
	// stored function exists in the canonical model
	// (implementation-plan Stage 3.3 iter 1 evaluator item 1).
	// This UPSERT IS the canonical active-row swap.
	upsertActiveSQL := fmt.Sprintf(
		`INSERT INTO %s
		    (repo_id, sha, scope_id, metric_kind, metric_version, sample_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (repo_id, sha, scope_id, metric_kind, metric_version)
		 DO UPDATE SET sample_id = EXCLUDED.sample_id`,
		w.qualifyMetricSampleActive(),
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

	for i, rec := range sorted {
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

	// Stage 3.3 active-row UPSERT pass. The metric_sample
	// INSERTs above are visible to subsequent statements in
	// this transaction (statement-end visibility), so the
	// composite FK on metric_sample_active is satisfied for
	// every record we are about to UPSERT. We iterate the
	// same sorted slice so the lock-acquisition order is
	// identical to the INSERT pass (and stable across calls).
	stmtActive, err := tx.PrepareContext(ctx, upsertActiveSQL)
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGMetricSampleWriter.Prepare UPSERT metric_sample_active: %w", err)
	}
	defer func() { _ = stmtActive.Close() }()

	for i, rec := range sorted {
		if _, err := stmtActive.ExecContext(ctx,
			rec.RepoID,
			rec.SHA,
			rec.ScopeID,
			rec.MetricKind,
			rec.MetricVersion,
			rec.SampleID,
		); err != nil {
			return fmt.Errorf("metric_ingestor: PGMetricSampleWriter records[%d] UPSERT metric_sample_active: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metric_ingestor: PGMetricSampleWriter Commit: %w", err)
	}
	return nil
}

// sortMetricSampleRecordsForActiveRow sorts `records` in
// place by the active-row quintuple plus `sample_id` as a
// stable tiebreaker. The ordering pins the per-tx
// row-lock acquisition order on `metric_sample_active`
// across the INSERT and UPSERT passes so two concurrent
// `WriteBatch` calls with overlapping quintuples cannot
// cross-lock. UUID columns are compared as raw bytes (via
// [bytes.Compare]) so the order is stable across UUID
// versions (V4 / V7).
func sortMetricSampleRecordsForActiveRow(records []MetricSampleRecord) {
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if c := bytes.Compare(a.RepoID[:], b.RepoID[:]); c != 0 {
			return c < 0
		}
		if a.SHA != b.SHA {
			return a.SHA < b.SHA
		}
		if c := bytes.Compare(a.ScopeID[:], b.ScopeID[:]); c != 0 {
			return c < 0
		}
		if a.MetricKind != b.MetricKind {
			return a.MetricKind < b.MetricKind
		}
		if a.MetricVersion != b.MetricVersion {
			return a.MetricVersion < b.MetricVersion
		}
		return bytes.Compare(a.SampleID[:], b.SampleID[:]) < 0
	})
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
