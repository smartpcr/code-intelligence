package metric_ingestor

// Stage 3.4 -- production PostgreSQL implementations of
// [RetractScanRunStore], [RetractionStore], and
// [SampleResolver].
//
// The composition root wires PGRetractScanRunStore +
// PGRetractionStore into a single [RetractDispatcher] and
// uses PGRetractionStore as the [SampleResolver] (it has
// access to the `metric_sample` table).
//
// # Writer ownership (architecture Sec 1.5.1 + tech-spec Sec 7.2)
//
// The Metric Ingestor role holds:
//
//	GRANT INSERT, SELECT ON clean_code.scan_run    TO clean_code_metric_ingestor;
//	GRANT INSERT, SELECT ON clean_code.metric_retraction TO clean_code_metric_ingestor;
//	REVOKE DELETE        ON clean_code.metric_retraction FROM clean_code_metric_ingestor;
//
// (migrations/0004_roles.up.sql:370-414). The PG stores
// in this file ONLY emit INSERT/UPDATE/SELECT against
// those two tables; no DELETE is ever issued so the
// schema-level role enforcement is honoured automatically.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"
)

// Sentinel errors emitted by the PG-backed retract stores
// at composition-root wiring time.
var (
	// ErrPGRetractStoreNilDB surfaces a nil *sql.DB at
	// wiring time so the operator log names the missing
	// seam.
	ErrPGRetractStoreNilDB = errors.New("metric_ingestor: NewPGRetract*Store: *sql.DB is nil")
	// ErrPGRetractStoreEmptySchema surfaces an empty
	// schema name at wiring time.
	ErrPGRetractStoreEmptySchema = errors.New("metric_ingestor: NewPGRetract*StoreWithSchema: schema is empty")
)

// pgRetractMetricSampleTable / pgRetractMetricRetractionTable
// are the unqualified table names targeted by the PG retract
// stores. Schema-qualified at statement-build time via
// [pq.QuoteIdentifier], mirroring [PGScanRunStore].
const (
	pgRetractMetricSampleTable      = "metric_sample"
	pgRetractMetricRetractionTable  = "metric_retraction"
)

// --- PGRetractScanRunStore -------------------------------------------------

// PGRetractScanRunStore is the production
// PostgreSQL-backed [RetractScanRunStore]. It owns ONLY
// the `scan_run(kind='retract', ...)` lifecycle -- no
// `commit.scan_status` writes (retract scans do NOT
// transition the four canonical commit states; that
// invariant is owned by [PGScanRunStore] for the
// foundation-tier scan path).
//
// # Open shape
//
//	INSERT INTO <schema>.scan_run
//	  (scan_run_id, repo_id, kind, sha_binding, to_sha, started_at, status)
//	  VALUES ($1, $2, 'retract', 'single', $3, $4, 'running')
//	RETURNING scan_run_id;
//
// # Finalize shape
//
//	UPDATE <schema>.scan_run
//	   SET status = $2, ended_at = $3
//	 WHERE scan_run_id = $1 AND status = 'running'
//	RETURNING scan_run_id;
//
// Both statements are short and run on a single connection
// from the pool; no transaction is required because there
// is no parent `commit.scan_status` row to keep atomic
// with the scan_run write (retract scans don't claim a
// commit).
type PGRetractScanRunStore struct {
	db     *sql.DB
	schema string
}

// NewPGRetractScanRunStore wraps `db` using the canonical
// `clean_code` schema.
func NewPGRetractScanRunStore(db *sql.DB) (*PGRetractScanRunStore, error) {
	return NewPGRetractScanRunStoreWithSchema(db, pgScanRunDefaultSchema)
}

// NewPGRetractScanRunStoreWithSchema is the test-friendly
// constructor used by the schema-isolated PG live tests.
func NewPGRetractScanRunStoreWithSchema(db *sql.DB, schema string) (*PGRetractScanRunStore, error) {
	if db == nil {
		return nil, ErrPGRetractStoreNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGRetractStoreEmptySchema
	}
	return &PGRetractScanRunStore{db: db, schema: schema}, nil
}

// qualifyScanRun returns `"<schema>"."scan_run"`.
func (s *PGRetractScanRunStore) qualifyScanRun() string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(pgScanRunRunTable)
}

// OpenRetractScanRun implements [RetractScanRunStore].
// Mints a scan_run_id client-side (so the row INSERT is
// idempotent against retry: the same id can be reused if
// the call retries after a transient error) and INSERTs
// the canonical row shape per migrations/0001 line 393+.
func (s *PGRetractScanRunStore) OpenRetractScanRun(ctx context.Context, repoID uuid.UUID, sha string, openedAt time.Time) (uuid.UUID, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if repoID == uuid.Nil {
		return uuid.Nil, ErrZeroRepoID
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return uuid.Nil, errors.New("metric_ingestor: PGRetractScanRunStore.OpenRetractScanRun: sha is empty (CHECK sha_binding='single' AND to_sha IS NOT NULL would reject)")
	}
	if openedAt.IsZero() {
		return uuid.Nil, errors.New("metric_ingestor: PGRetractScanRunStore.OpenRetractScanRun: openedAt is the zero time")
	}
	scanRunID, err := uuid.NewV4()
	if err != nil {
		return uuid.Nil, fmt.Errorf("metric_ingestor: PGRetractScanRunStore mint scan_run_id: %w", err)
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s
		     (scan_run_id, repo_id, kind, sha_binding, to_sha, started_at, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'running')`,
		s.qualifyScanRun(),
	)
	if _, err := s.db.ExecContext(ctx, stmt,
		scanRunID, repoID, ScanRunKindRetract, SHABindingSingle, sha, openedAt.UTC(),
	); err != nil {
		return uuid.Nil, fmt.Errorf("metric_ingestor: PGRetractScanRunStore INSERT scan_run (repo_id=%s sha=%s): %w", repoID, sha, err)
	}
	return scanRunID, nil
}

// FinalizeRetractScanRun implements [RetractScanRunStore].
// Flips the row's status to `status` and stamps
// `ended_at`. Rejects `running` (cannot finalise to
// running) and rejects double-finalise via the
// `WHERE status='running'` predicate -- a second call
// affects 0 rows and surfaces as [ErrConcurrentFinalize].
func (s *PGRetractScanRunStore) FinalizeRetractScanRun(ctx context.Context, scanRunID uuid.UUID, status ScanRunStatus, endedAt time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if status == ScanRunStatusRunning {
		return fmt.Errorf("metric_ingestor: PGRetractScanRunStore.FinalizeRetractScanRun rejects 'running' as terminal: %w", ErrUnknownScanRunStatus)
	}
	if err := ValidateScanRunStatus(status); err != nil {
		return err
	}
	if scanRunID == uuid.Nil {
		return fmt.Errorf("%w: scan_run_id is the zero UUID", ErrUnknownScanRunID)
	}
	if endedAt.IsZero() {
		return errors.New("metric_ingestor: PGRetractScanRunStore.FinalizeRetractScanRun: endedAt is the zero time")
	}
	stmt := fmt.Sprintf(
		`UPDATE %s
		    SET status = $2, ended_at = $3
		  WHERE scan_run_id = $1 AND status = 'running'`,
		s.qualifyScanRun(),
	)
	res, err := s.db.ExecContext(ctx, stmt, scanRunID, status, endedAt.UTC())
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGRetractScanRunStore UPDATE scan_run.status (%s): %w", scanRunID, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGRetractScanRunStore RowsAffected (finalize %s): %w", scanRunID, err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: scan_run_id=%s rowsAffected=%d (want 1)", ErrConcurrentFinalize, scanRunID, affected)
	}
	return nil
}

// Compile-time interface guard.
var _ RetractScanRunStore = (*PGRetractScanRunStore)(nil)

// --- PGRetractionStore -----------------------------------------------------

// PGRetractionStore is the production PostgreSQL-backed
// [RetractionStore]. It ALSO implements [SampleResolver]
// because the (repo_id, sha) lookup is naturally a query
// against `metric_sample` -- making the same struct serve
// both seams keeps the composition root simple and avoids
// constructing two PG stores against the same database
// handle.
//
// # Lookup shape
//
//	SELECT retraction_id, sample_id, reason, appended_by, created_at
//	  FROM <schema>.metric_retraction
//	 WHERE sample_id = $1;
//
// # Append shape (idempotent on sample_id)
//
//	INSERT INTO <schema>.metric_retraction
//	  (retraction_id, sample_id, reason, appended_by, created_at)
//	  VALUES ($1, $2, $3, $4, $5)
//	  ON CONFLICT (sample_id) DO NOTHING
//	  RETURNING retraction_id, sample_id, reason, appended_by, created_at;
//
// If the ON CONFLICT path fires, the RETURNING is empty
// (zero rows) -- the store then SELECTs the existing row
// and returns (existing, inserted=false, nil). This
// matches the in-memory store's contract bit-for-bit and
// keeps the dispatcher's idempotency logic uniform across
// in-memory and PG.
//
// # ResolveSample shape
//
//	SELECT repo_id, sha FROM <schema>.metric_sample
//	 WHERE sample_id = $1;
//
// # SampleExists shape
//
//	SELECT 1 FROM <schema>.metric_sample WHERE sample_id = $1;
type PGRetractionStore struct {
	db     *sql.DB
	schema string
}

// NewPGRetractionStore wraps `db` using the canonical
// `clean_code` schema.
func NewPGRetractionStore(db *sql.DB) (*PGRetractionStore, error) {
	return NewPGRetractionStoreWithSchema(db, pgScanRunDefaultSchema)
}

// NewPGRetractionStoreWithSchema is the test-friendly
// constructor used by the schema-isolated live tests.
func NewPGRetractionStoreWithSchema(db *sql.DB, schema string) (*PGRetractionStore, error) {
	if db == nil {
		return nil, ErrPGRetractStoreNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGRetractStoreEmptySchema
	}
	return &PGRetractionStore{db: db, schema: schema}, nil
}

// qualify returns `"<schema>"."<table>"`.
func (s *PGRetractionStore) qualify(table string) string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(table)
}

// SampleExists implements [RetractionStore].
func (s *PGRetractionStore) SampleExists(ctx context.Context, sampleID uuid.UUID) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if sampleID == uuid.Nil {
		return false, ErrRetractZeroSampleID
	}
	stmt := fmt.Sprintf(
		`SELECT 1 FROM %s WHERE sample_id = $1`,
		s.qualify(pgRetractMetricSampleTable),
	)
	var one int
	switch err := s.db.QueryRowContext(ctx, stmt, sampleID).Scan(&one); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("metric_ingestor: PGRetractionStore.SampleExists (sample_id=%s): %w", sampleID, err)
	}
	return true, nil
}

// ResolveSample implements [SampleResolver]. The retract
// dispatcher uses this to fetch (repo_id, sha) for the
// scan_run row's CHECK-constrained `to_sha` column.
func (s *PGRetractionStore) ResolveSample(ctx context.Context, sampleID uuid.UUID) (uuid.UUID, string, bool, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, "", false, err
	}
	if sampleID == uuid.Nil {
		return uuid.Nil, "", false, ErrRetractZeroSampleID
	}
	stmt := fmt.Sprintf(
		`SELECT repo_id, sha FROM %s WHERE sample_id = $1`,
		s.qualify(pgRetractMetricSampleTable),
	)
	var (
		repoID uuid.UUID
		sha    string
	)
	switch err := s.db.QueryRowContext(ctx, stmt, sampleID).Scan(&repoID, &sha); {
	case errors.Is(err, sql.ErrNoRows):
		return uuid.Nil, "", false, nil
	case err != nil:
		return uuid.Nil, "", false, fmt.Errorf("metric_ingestor: PGRetractionStore.ResolveSample (sample_id=%s): %w", sampleID, err)
	}
	return repoID, sha, true, nil
}

// Lookup implements [RetractionStore]. Returns the
// existing retraction row keyed by sample_id if any.
func (s *PGRetractionStore) Lookup(ctx context.Context, sampleID uuid.UUID) (RetractionRow, bool, error) {
	if err := ctx.Err(); err != nil {
		return RetractionRow{}, false, err
	}
	if sampleID == uuid.Nil {
		return RetractionRow{}, false, ErrRetractZeroSampleID
	}
	stmt := fmt.Sprintf(
		`SELECT retraction_id, sample_id, reason, appended_by, created_at
		   FROM %s
		  WHERE sample_id = $1`,
		s.qualify(pgRetractMetricRetractionTable),
	)
	var row RetractionRow
	switch err := s.db.QueryRowContext(ctx, stmt, sampleID).Scan(
		&row.RetractionID, &row.SampleID, &row.Reason, &row.AppendedBy, &row.CreatedAt,
	); {
	case errors.Is(err, sql.ErrNoRows):
		return RetractionRow{}, false, nil
	case err != nil:
		return RetractionRow{}, false, fmt.Errorf("metric_ingestor: PGRetractionStore.Lookup (sample_id=%s): %w", sampleID, err)
	}
	return row, true, nil
}

// Append implements [RetractionStore] with the canonical
// idempotency contract:
//   - Fresh insert returns (row, inserted=true, nil).
//   - Already-retracted sample returns (existing,
//     inserted=false, nil).
//   - Infrastructure failure returns (zero, false, err).
//
// The `ON CONFLICT (sample_id) DO NOTHING RETURNING ...`
// pattern is the canonical "INSERT or read existing"
// PostgreSQL idiom. On a fresh insert RETURNING yields
// the inserted row; on conflict it yields zero rows and
// the store falls through to a Lookup.
func (s *PGRetractionStore) Append(ctx context.Context, row RetractionRow) (RetractionRow, bool, error) {
	if err := ctx.Err(); err != nil {
		return RetractionRow{}, false, err
	}
	if row.RetractionID == uuid.Nil {
		return RetractionRow{}, false, errors.New("metric_ingestor: PGRetractionStore.Append: RetractionID is zero (caller must mint)")
	}
	if row.SampleID == uuid.Nil {
		return RetractionRow{}, false, ErrRetractZeroSampleID
	}
	if strings.TrimSpace(row.Reason) == "" {
		return RetractionRow{}, false, ErrRetractEmptyReason
	}
	if strings.TrimSpace(row.AppendedBy) == "" {
		return RetractionRow{}, false, ErrRetractEmptyAppendedBy
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s
		     (retraction_id, sample_id, reason, appended_by, created_at)
		 VALUES ($1, $2, $3, $4, $5)
		 ON CONFLICT (sample_id) DO NOTHING
		 RETURNING retraction_id, sample_id, reason, appended_by, created_at`,
		s.qualify(pgRetractMetricRetractionTable),
	)
	var stored RetractionRow
	switch err := s.db.QueryRowContext(ctx, stmt,
		row.RetractionID, row.SampleID, strings.TrimSpace(row.Reason), strings.TrimSpace(row.AppendedBy), row.CreatedAt.UTC(),
	).Scan(&stored.RetractionID, &stored.SampleID, &stored.Reason, &stored.AppendedBy, &stored.CreatedAt); {
	case errors.Is(err, sql.ErrNoRows):
		// Conflict on UNIQUE(sample_id). Fall through to
		// Lookup so the caller receives the existing row.
		existing, found, lookupErr := s.Lookup(ctx, row.SampleID)
		if lookupErr != nil {
			return RetractionRow{}, false, fmt.Errorf("metric_ingestor: PGRetractionStore.Append: post-conflict lookup (sample_id=%s): %w", row.SampleID, lookupErr)
		}
		if !found {
			// Race: the conflict row was rolled back
			// or otherwise vanished. Treat as a
			// transient error so the caller can retry.
			return RetractionRow{}, false, fmt.Errorf("metric_ingestor: PGRetractionStore.Append: ON CONFLICT fired but row not found on lookup (sample_id=%s)", row.SampleID)
		}
		return existing, false, nil
	case err != nil:
		return RetractionRow{}, false, fmt.Errorf("metric_ingestor: PGRetractionStore.Append (sample_id=%s): %w", row.SampleID, err)
	}
	return stored, true, nil
}

// Compile-time interface guards. A future refactor that
// breaks any of the three contracts will fail to compile
// here rather than at the dispatcher call site.
var (
	_ RetractionStore = (*PGRetractionStore)(nil)
	_ SampleResolver  = (*PGRetractionStore)(nil)
)
