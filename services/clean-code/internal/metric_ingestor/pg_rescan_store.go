package metric_ingestor

// Stage 3.4 -- production PostgreSQL implementation of
// [RescanScanRunStore].
//
// The rescan flow is a strict subset of the retract flow:
// open one `scan_run(kind='full', sha_binding='single',
// status='running')` row, return the id, done. No
// equivalent "finalize" is needed at the rescan-enqueue
// boundary -- the downstream Aggregator + scanners drive
// the lifecycle to `succeeded`/`failed` via [PGScanRunStore]
// when the actual scan completes.

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

// Sentinel errors emitted by the PG-backed rescan store at
// composition-root wiring time. These mirror the retract
// store's sentinels but name the rescan seam explicitly so
// an operator debugging a rescan wiring failure sees an
// error that points at the rescan store rather than the
// retract store.
var (
	// ErrPGRescanStoreNilDB surfaces a nil *sql.DB at
	// wiring time so the operator log names the missing
	// seam.
	ErrPGRescanStoreNilDB = errors.New("metric_ingestor: NewPGRescanScanRunStore: *sql.DB is nil")
	// ErrPGRescanStoreEmptySchema surfaces an empty
	// schema name at wiring time.
	ErrPGRescanStoreEmptySchema = errors.New("metric_ingestor: NewPGRescanScanRunStoreWithSchema: schema is empty")
)

// PGRescanScanRunStore is the production
// PostgreSQL-backed [RescanScanRunStore]. It exposes ONLY
// `OpenRescanRun`; the row's eventual terminal transition
// is owned by [PGScanRunStore.FinalizeScanRun] which the
// scan workers already call.
//
// # Open shape
//
//	INSERT INTO <schema>.scan_run
//	  (scan_run_id, repo_id, kind, sha_binding, to_sha, started_at, status)
//	  VALUES ($1, $2, 'full', 'single', $3, $4, 'running')
type PGRescanScanRunStore struct {
	db     *sql.DB
	schema string
}

// NewPGRescanScanRunStore wraps `db` using the canonical
// `clean_code` schema.
func NewPGRescanScanRunStore(db *sql.DB) (*PGRescanScanRunStore, error) {
	return NewPGRescanScanRunStoreWithSchema(db, pgScanRunDefaultSchema)
}

// NewPGRescanScanRunStoreWithSchema is the test-friendly
// schema-isolated constructor.
func NewPGRescanScanRunStoreWithSchema(db *sql.DB, schema string) (*PGRescanScanRunStore, error) {
	if db == nil {
		return nil, ErrPGRescanStoreNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGRescanStoreEmptySchema
	}
	return &PGRescanScanRunStore{db: db, schema: schema}, nil
}

// qualifyScanRun returns `"<schema>"."scan_run"`.
func (s *PGRescanScanRunStore) qualifyScanRun() string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(pgScanRunRunTable)
}

// OpenRescanRun implements [RescanScanRunStore]. INSERTs
// a fresh scan_run row in `running` state.
func (s *PGRescanScanRunStore) OpenRescanRun(ctx context.Context, repoID uuid.UUID, sha string, openedAt time.Time) (uuid.UUID, error) {
	if err := ctx.Err(); err != nil {
		return uuid.Nil, err
	}
	if repoID == uuid.Nil {
		return uuid.Nil, ErrRescanZeroRepoID
	}
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return uuid.Nil, ErrRescanEmptySHA
	}
	if openedAt.IsZero() {
		return uuid.Nil, errors.New("metric_ingestor: PGRescanScanRunStore.OpenRescanRun: openedAt is the zero time")
	}
	scanRunID, err := uuid.NewV4()
	if err != nil {
		return uuid.Nil, fmt.Errorf("metric_ingestor: PGRescanScanRunStore mint scan_run_id: %w", err)
	}
	stmt := fmt.Sprintf(
		`INSERT INTO %s
		     (scan_run_id, repo_id, kind, sha_binding, to_sha, started_at, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'running')`,
		s.qualifyScanRun(),
	)
	if _, err := s.db.ExecContext(ctx, stmt,
		scanRunID, repoID, ScanRunKindFull, SHABindingSingle, sha, openedAt.UTC(),
	); err != nil {
		return uuid.Nil, fmt.Errorf("metric_ingestor: PGRescanScanRunStore INSERT scan_run (repo_id=%s sha=%s): %w", repoID, sha, err)
	}
	return scanRunID, nil
}

// Compile-time interface guard.
var _ RescanScanRunStore = (*PGRescanScanRunStore)(nil)
