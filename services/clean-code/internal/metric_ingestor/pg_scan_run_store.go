package metric_ingestor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gofrs/uuid"
	"github.com/lib/pq"

	"forge/services/clean-code/internal/repo_indexer"
)

// pgScanRunCommitTable and pgScanRunRunTable are the
// unqualified table names the PG-backed [ScanRunStore]
// targets. Schema-qualified at statement-build time via
// [pq.QuoteIdentifier].
const (
	pgScanRunCommitTable = "commit"
	pgScanRunRunTable    = "scan_run"
)

// pgScanRunDefaultSchema mirrors `internal/storage.SchemaName`.
// Pinned here so this package does not import storage --
// the dependency would be one-way only (storage today does
// not import metric_ingestor) but the constant is small
// enough that the duplication is preferable to the
// cross-package coupling.
const pgScanRunDefaultSchema = "clean_code"

// PGScanRunStore is the production PostgreSQL-backed
// implementation of [ScanRunStore]. Both lifecycle methods
// run inside a single transaction so the
// (`scan_run` INSERT/UPDATE + `commit.scan_status` UPDATE)
// pair lands atomically -- the architecture's "sole-writer
// of commit.scan_status" invariant (Sec 1.5.1 row 1) is
// enforced at the application layer here AND at the DB role
// layer via Phase 1.5 grants.
//
// # Claim shape
//
//	BEGIN;
//	SELECT repo_id, sha, committed_at
//	    FROM <schema>.commit
//	    WHERE scan_status = 'pending'
//	    ORDER BY committed_at ASC, sha ASC
//	    LIMIT 1
//	    FOR UPDATE SKIP LOCKED;
//	-- (if no row) ROLLBACK; return (zero, didClaim=false, nil)
//	INSERT INTO <schema>.scan_run
//	    (scan_run_id, repo_id, kind, sha_binding, to_sha, started_at, status)
//	    VALUES ($1, $2, $3, $4, $5, $6, 'running');
//	UPDATE <schema>.commit
//	    SET scan_status = 'scanning'
//	    WHERE repo_id = $2 AND sha = $5 AND scan_status = 'pending';
//	-- assert rowsAffected = 1; else ROLLBACK + ErrConcurrentClaim
//	COMMIT;
//
// `FOR UPDATE SKIP LOCKED` lets multiple sweeper workers
// run concurrently against the same DB without serialising
// on the queue head. The single-worker Stage 3.2 wiring
// does not exercise the fanout path, but the SQL shape is
// the same and Phase 3.5 fans out by adding workers, NOT
// by changing this store.
//
// # Finalize shape
//
//	BEGIN;
//	UPDATE <schema>.scan_run
//	    SET status = $runStatus, ended_at = $endedAt
//	    WHERE scan_run_id = $1 AND status = 'running';
//	-- assert rowsAffected = 1; else ROLLBACK + ErrConcurrentFinalize
//	UPDATE <schema>.commit
//	    SET scan_status = $commitStatus
//	    WHERE repo_id = $2 AND sha = $3 AND scan_status = 'scanning';
//	-- assert rowsAffected = 1; else ROLLBACK + ErrConcurrentFinalize
//	COMMIT;
//
// Both UPDATEs name the EXPECTED previous status in the
// WHERE clause (`status='running'`, `scan_status='scanning'`)
// so a second concurrent finalize -- e.g. an erroneous
// retry after a network blip -- is caught by `rowsAffected=0`
// rather than producing a duplicate state transition.
type PGScanRunStore struct {
	db     *sql.DB
	schema string
}

// ErrPGScanRunStoreNilDB surfaces a nil *sql.DB at
// composition-root wiring time.
var ErrPGScanRunStoreNilDB = errors.New("metric_ingestor: NewPGScanRunStore: *sql.DB is nil")

// ErrPGScanRunStoreEmptySchema surfaces an empty schema
// name at composition-root wiring time.
var ErrPGScanRunStoreEmptySchema = errors.New("metric_ingestor: NewPGScanRunStoreWithSchema: schema is empty")

// ErrConcurrentClaim is returned by ClaimNextPendingCommit
// when the INSERT scan_run succeeded but the UPDATE commit
// affected 0 rows -- a concurrent worker raced ahead between
// the SELECT FOR UPDATE and the UPDATE. The state machine
// treats this as a "no claim" surface (the row was
// successfully claimed by another worker); the caller's
// transaction is rolled back.
//
// In single-worker mode this never fires; it exists for
// the multi-worker fan-out shape Phase 3.5 lights up.
var ErrConcurrentClaim = errors.New("metric_ingestor: PGScanRunStore.ClaimNextPendingCommit: concurrent worker raced ahead, claim aborted")

// ErrConcurrentFinalize is returned by FinalizeScanRun
// when either UPDATE affects 0 rows -- typically because
// the row was already finalized by a concurrent retry, or
// because the row was never in `running`/`scanning` to
// begin with (a sequencing bug).
var ErrConcurrentFinalize = errors.New("metric_ingestor: PGScanRunStore.FinalizeScanRun: row not in expected pre-state (already finalized? sequencing bug?)")

// NewPGScanRunStore wraps `db` using the canonical
// `clean_code` schema. Production callers reach this
// constructor; test code MAY use [NewPGScanRunStoreWithSchema]
// to land on an isolated schema.
func NewPGScanRunStore(db *sql.DB) (*PGScanRunStore, error) {
	return NewPGScanRunStoreWithSchema(db, pgScanRunDefaultSchema)
}

// NewPGScanRunStoreWithSchema is the test-friendly
// constructor. Returns a non-nil error on misconfiguration.
func NewPGScanRunStoreWithSchema(db *sql.DB, schema string) (*PGScanRunStore, error) {
	if db == nil {
		return nil, ErrPGScanRunStoreNilDB
	}
	if strings.TrimSpace(schema) == "" {
		return nil, ErrPGScanRunStoreEmptySchema
	}
	return &PGScanRunStore{db: db, schema: schema}, nil
}

// qualifyCommit returns `"<schema>"."commit"`.
func (s *PGScanRunStore) qualifyCommit() string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(pgScanRunCommitTable)
}

// qualifyScanRun returns `"<schema>"."scan_run"`.
func (s *PGScanRunStore) qualifyScanRun() string {
	return pq.QuoteIdentifier(s.schema) + "." + pq.QuoteIdentifier(pgScanRunRunTable)
}

// ClaimNextPendingCommit implements [ScanRunStore].
//
// Validates `req` (via [ClaimRequest.Validate]) BEFORE
// opening a transaction so a misconfigured caller does not
// burn a connection. Inside the transaction:
//
//  1. SELECT oldest pending commit FOR UPDATE SKIP LOCKED.
//  2. If no row: ROLLBACK, return (zero, false, nil).
//  3. INSERT scan_run with kind/sha_binding/started_at from req.
//  4. UPDATE commit set scan_status='scanning' WHERE
//     scan_status='pending' (the FOR UPDATE already locked the
//     row; the WHERE clause is defence-in-depth against a
//     race).
//  5. COMMIT.
//
// Returns the populated [ScanRunClaim] on success.
func (s *PGScanRunStore) ClaimNextPendingCommit(ctx context.Context, req ClaimRequest) (ScanRunClaim, bool, error) {
	if err := req.Validate(); err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore.ClaimNextPendingCommit: %w", err)
	}
	if err := repo_indexer.ValidateTransition(repo_indexer.ScanStatusPending, repo_indexer.ScanStatusScanning); err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore.ClaimNextPendingCommit: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore.BeginTx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	selectSQL := fmt.Sprintf(
		`SELECT repo_id, sha, committed_at
		 FROM %s
		 WHERE scan_status = 'pending'
		 ORDER BY committed_at ASC, sha ASC
		 LIMIT 1
		 FOR UPDATE SKIP LOCKED`,
		s.qualifyCommit(),
	)
	var (
		repoID      uuid.UUID
		sha         string
		committedAt time.Time
	)
	switch err := tx.QueryRowContext(ctx, selectSQL).Scan(&repoID, &sha, &committedAt); {
	case errors.Is(err, sql.ErrNoRows):
		// No pending commit -- not an error. Rollback the
		// (empty) tx and return the no-claim sentinel.
		return ScanRunClaim{}, false, nil
	case err != nil:
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore SELECT pending commit: %w", err)
	}
	_ = committedAt // selected for ORDER BY locking; not stored on the claim

	scanRunID, err := uuid.NewV4()
	if err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore mint scan_run_id: %w", err)
	}

	insertSQL := fmt.Sprintf(
		`INSERT INTO %s
		     (scan_run_id, repo_id, kind, sha_binding, to_sha, started_at, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'running')`,
		s.qualifyScanRun(),
	)
	if _, err := tx.ExecContext(ctx, insertSQL,
		scanRunID, repoID, req.Kind, req.SHABinding, sha, req.OpenedAt.UTC(),
	); err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore INSERT scan_run: %w", err)
	}

	updateSQL := fmt.Sprintf(
		`UPDATE %s
		    SET scan_status = 'scanning'
		  WHERE repo_id = $1 AND sha = $2 AND scan_status = 'pending'`,
		s.qualifyCommit(),
	)
	res, err := tx.ExecContext(ctx, updateSQL, repoID, sha)
	if err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore UPDATE commit.scan_status (pending->scanning): %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore RowsAffected (claim): %w", err)
	}
	if affected != 1 {
		return ScanRunClaim{}, false, fmt.Errorf("%w: rowsAffected=%d (want 1)", ErrConcurrentClaim, affected)
	}

	if err := tx.Commit(); err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore Commit (claim): %w", err)
	}

	return ScanRunClaim{
		ScanRunID:  scanRunID,
		RepoID:     repoID,
		SHA:        sha,
		Kind:       req.Kind,
		SHABinding: req.SHABinding,
		OpenedAt:   req.OpenedAt.UTC(),
	}, true, nil
}

// PeekNextPendingCommit implements [ScanRunStore] for
// PostgreSQL. iter-4 evaluator item 2 structural
// pre-flight: returns the oldest
// `commit.scan_status='pending'` row WITHOUT taking a
// row-level lock, WITHOUT opening a transaction, and
// WITHOUT INSERTing a `scan_run`. The result feeds an
// [AstSourceAvailability] probe so the state machine can
// SKIP the claim when the upstream artefact is not yet
// materialised; the commit stays `pending` and the next
// sweep tick retries.
//
// The query intentionally OMITS `FOR UPDATE SKIP LOCKED`:
// the peek is read-only and MUST NOT serialise concurrent
// workers. The authoritative serialisation point remains
// [ClaimNextPendingCommit], which takes the row lock.
//
// On `sql.ErrNoRows` returns (zero, false, nil) -- no
// pending commit is not an error condition.
func (s *PGScanRunStore) PeekNextPendingCommit(ctx context.Context) (PendingCommit, bool, error) {
	if err := ctx.Err(); err != nil {
		return PendingCommit{}, false, err
	}
	rows, err := s.PeekNextPendingCommits(ctx, 1)
	if err != nil {
		return PendingCommit{}, false, err
	}
	if len(rows) == 0 {
		return PendingCommit{}, false, nil
	}
	return rows[0], true, nil
}

// PeekNextPendingCommits implements [ScanRunStore] for
// PostgreSQL. iter-5 evaluator item 4 multi-row pre-flight:
// returns up to `limit` oldest pending commits in
// (committed_at ASC, sha ASC) order without locking or
// mutating. Used by [StateMachine.ProcessOne] when an
// [AstSourceAvailability] probe is wired so the state
// machine can skip past a not-yet-ready oldest commit and
// claim a newer ready commit instead.
//
// The query OMITS `FOR UPDATE`: peeks are read-only and
// MUST NOT serialise concurrent workers. The authoritative
// serialisation point is [ClaimSpecificPendingCommit] /
// [ClaimNextPendingCommit].
func (s *PGScanRunStore) PeekNextPendingCommits(ctx context.Context, limit int) ([]PendingCommit, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("metric_ingestor: PGScanRunStore.PeekNextPendingCommits: limit=%d must be > 0", limit)
	}
	selectSQL := fmt.Sprintf(
		`SELECT repo_id, sha, committed_at
		 FROM %s
		 WHERE scan_status = 'pending'
		 ORDER BY committed_at ASC, sha ASC
		 LIMIT $1`,
		s.qualifyCommit(),
	)
	rows, err := s.db.QueryContext(ctx, selectSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("metric_ingestor: PGScanRunStore.PeekNextPendingCommits SELECT: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]PendingCommit, 0, limit)
	for rows.Next() {
		var (
			repoID      uuid.UUID
			sha         string
			committedAt time.Time
		)
		if err := rows.Scan(&repoID, &sha, &committedAt); err != nil {
			return nil, fmt.Errorf("metric_ingestor: PGScanRunStore.PeekNextPendingCommits Scan: %w", err)
		}
		out = append(out, PendingCommit{
			RepoID:      repoID,
			SHA:         sha,
			CommittedAt: committedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metric_ingestor: PGScanRunStore.PeekNextPendingCommits rows.Err: %w", err)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// ClaimSpecificPendingCommit implements [ScanRunStore]
// for PostgreSQL. iter-5 evaluator item 4: claims a
// SPECIFIC pending row named by (repoID, sha) so the
// state machine's pre-flight pipeline can skip past a
// not-yet-ready oldest commit and claim a newer ready
// commit instead.
//
// The transaction:
//
//  1. SELECT the named row FOR UPDATE SKIP LOCKED filtered
//     on `scan_status='pending'`. A missed row (raced away
//     or already scanning) returns (zero, false, nil).
//  2. INSERT scan_run.
//  3. UPDATE commit SET scan_status='scanning' WHERE
//     scan_status='pending' AND repo_id=$1 AND sha=$2.
//  4. COMMIT.
//
// On any row-count mismatch (concurrent transition between
// SELECT and UPDATE) the transaction returns
// [ErrConcurrentClaim].
func (s *PGScanRunStore) ClaimSpecificPendingCommit(ctx context.Context, repoID uuid.UUID, sha string, req ClaimRequest) (ScanRunClaim, bool, error) {
	if err := req.Validate(); err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore.ClaimSpecificPendingCommit: %w", err)
	}
	if err := repo_indexer.ValidateTransition(repo_indexer.ScanStatusPending, repo_indexer.ScanStatusScanning); err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore.ClaimSpecificPendingCommit: %w", err)
	}
	if repoID == uuid.Nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore.ClaimSpecificPendingCommit: zero RepoID")
	}
	if sha == "" {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore.ClaimSpecificPendingCommit: empty SHA")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore.BeginTx (specific): %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	selectSQL := fmt.Sprintf(
		`SELECT repo_id, sha, committed_at
		 FROM %s
		 WHERE repo_id = $1 AND sha = $2 AND scan_status = 'pending'
		 FOR UPDATE SKIP LOCKED`,
		s.qualifyCommit(),
	)
	var (
		gotRepoID   uuid.UUID
		gotSHA      string
		committedAt time.Time
	)
	switch err := tx.QueryRowContext(ctx, selectSQL, repoID, sha).Scan(&gotRepoID, &gotSHA, &committedAt); {
	case errors.Is(err, sql.ErrNoRows):
		// Row raced away (claimed by another worker, or
		// already terminal). Not an error -- caller
		// re-peeks.
		return ScanRunClaim{}, false, nil
	case err != nil:
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore SELECT specific pending commit (%s @ %s): %w", repoID, sha, err)
	}
	_ = committedAt

	scanRunID, err := uuid.NewV4()
	if err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore mint scan_run_id (specific): %w", err)
	}

	insertSQL := fmt.Sprintf(
		`INSERT INTO %s
		     (scan_run_id, repo_id, kind, sha_binding, to_sha, started_at, status)
		 VALUES ($1, $2, $3, $4, $5, $6, 'running')`,
		s.qualifyScanRun(),
	)
	if _, err := tx.ExecContext(ctx, insertSQL,
		scanRunID, gotRepoID, req.Kind, req.SHABinding, gotSHA, req.OpenedAt.UTC(),
	); err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore INSERT scan_run (specific): %w", err)
	}

	updateSQL := fmt.Sprintf(
		`UPDATE %s
		    SET scan_status = 'scanning'
		  WHERE repo_id = $1 AND sha = $2 AND scan_status = 'pending'`,
		s.qualifyCommit(),
	)
	res, err := tx.ExecContext(ctx, updateSQL, gotRepoID, gotSHA)
	if err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore UPDATE commit.scan_status (specific pending->scanning): %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore RowsAffected (specific claim): %w", err)
	}
	if affected != 1 {
		return ScanRunClaim{}, false, fmt.Errorf("%w: rowsAffected=%d (want 1)", ErrConcurrentClaim, affected)
	}
	if err := tx.Commit(); err != nil {
		return ScanRunClaim{}, false, fmt.Errorf("metric_ingestor: PGScanRunStore Commit (specific claim): %w", err)
	}
	return ScanRunClaim{
		ScanRunID:  scanRunID,
		RepoID:     gotRepoID,
		SHA:        gotSHA,
		Kind:       req.Kind,
		SHABinding: req.SHABinding,
		OpenedAt:   req.OpenedAt.UTC(),
	}, true, nil
}

// FinalizeScanRun implements [ScanRunStore].
//
// Pre-validates runStatus / commitStatus against the
// canonical terminal sets ({succeeded,failed} / {scanned,failed})
// AND the (succeeded,scanned)/(failed,failed) pair
// invariant BEFORE opening a transaction.
//
// Inside the transaction, both UPDATEs name the expected
// previous status in their WHERE clause so a concurrent
// double-finalize is rejected via `rowsAffected=0` rather
// than silently overwriting.
func (s *PGScanRunStore) FinalizeScanRun(
	ctx context.Context,
	claim ScanRunClaim,
	runStatus ScanRunStatus,
	commitStatus repo_indexer.ScanStatus,
	endedAt time.Time,
) error {
	if err := ValidateScanRunStatus(runStatus); err != nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.FinalizeScanRun: %w", err)
	}
	if runStatus != ScanRunStatusSucceeded && runStatus != ScanRunStatusFailed {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.FinalizeScanRun: non-terminal runStatus=%q (want one of succeeded|failed)", runStatus)
	}
	if commitStatus != repo_indexer.ScanStatusScanned && commitStatus != repo_indexer.ScanStatusFailed {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.FinalizeScanRun: non-terminal commitStatus=%q (want one of scanned|failed)", commitStatus)
	}
	// Pair invariant: succeeded<->scanned, failed<->failed
	// (the state machine's same-pair contract -- see
	// state.go::finalize for the rationale).
	if runStatus == ScanRunStatusSucceeded && commitStatus != repo_indexer.ScanStatusScanned {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.FinalizeScanRun: mismatched pair runStatus=succeeded but commitStatus=%q (want scanned)", commitStatus)
	}
	if runStatus == ScanRunStatusFailed && commitStatus != repo_indexer.ScanStatusFailed {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.FinalizeScanRun: mismatched pair runStatus=failed but commitStatus=%q (want failed)", commitStatus)
	}
	if err := repo_indexer.ValidateTransition(repo_indexer.ScanStatusScanning, commitStatus); err != nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.FinalizeScanRun transition guard: %w", err)
	}
	if claim.ScanRunID == uuid.Nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.FinalizeScanRun: claim.ScanRunID is zero")
	}
	if claim.RepoID == uuid.Nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.FinalizeScanRun: claim.RepoID is zero")
	}
	if claim.SHA == "" {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.FinalizeScanRun: claim.SHA is empty")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore.BeginTx (finalize): %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	updateRunSQL := fmt.Sprintf(
		`UPDATE %s
		    SET status = $1, ended_at = $2
		  WHERE scan_run_id = $3 AND status = 'running'`,
		s.qualifyScanRun(),
	)
	res, err := tx.ExecContext(ctx, updateRunSQL,
		string(runStatus), endedAt.UTC(), claim.ScanRunID,
	)
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore UPDATE scan_run.status: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore RowsAffected (scan_run): %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: scan_run rowsAffected=%d (want 1; scan_run_id=%s)",
			ErrConcurrentFinalize, affected, claim.ScanRunID)
	}

	updateCommitSQL := fmt.Sprintf(
		`UPDATE %s
		    SET scan_status = $1
		  WHERE repo_id = $2 AND sha = $3 AND scan_status = 'scanning'`,
		s.qualifyCommit(),
	)
	res, err = tx.ExecContext(ctx, updateCommitSQL,
		string(commitStatus), claim.RepoID, claim.SHA,
	)
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore UPDATE commit.scan_status (scanning->%s): %w", commitStatus, err)
	}
	affected, err = res.RowsAffected()
	if err != nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore RowsAffected (commit): %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("%w: commit rowsAffected=%d (want 1; repo_id=%s sha=%s)",
			ErrConcurrentFinalize, affected, claim.RepoID, claim.SHA)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("metric_ingestor: PGScanRunStore Commit (finalize): %w", err)
	}
	return nil
}
