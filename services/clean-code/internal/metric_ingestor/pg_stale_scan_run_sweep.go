package metric_ingestor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/repo_indexer"
)

// This file teaches [PGScanRunStore] the
// [StaleScanRunSweepStore] interface. Kept in its own file
// (matching the pattern of the in-memory store extension
// next to it) so the diff for Stage 3.5 is localised.
//
// # Query shapes (production)
//
// FindStaleRunningScanRuns:
//
//	SELECT scan_run_id, repo_id, kind, sha_binding, to_sha, started_at
//	  FROM <schema>.scan_run
//	 WHERE status = 'running' AND started_at < $1
//	 ORDER BY started_at ASC, scan_run_id ASC
//	 LIMIT $2;
//
// (Deliberately NO `FOR UPDATE SKIP LOCKED` -- the per-row
// FailStaleScanRun guards the UPDATE with
// `WHERE status='running'` which is the authoritative
// race detection. A cross-statement lock would only buy us
// throughput, and the sweep's 5-min cadence makes that
// irrelevant; the simpler shape is the canonical one.)
//
// FailStaleScanRun (single tx per row):
//
//	BEGIN;
//	UPDATE <schema>.scan_run
//	    SET status = 'failed', ended_at = $1
//	  WHERE scan_run_id = $2 AND status = 'running';
//	-- if rowsAffected==0: COMMIT, ScanRunTransitioned=false
//	UPDATE <schema>.commit
//	    SET scan_status = 'failed'
//	  WHERE repo_id = $3 AND sha = $4 AND scan_status = 'scanning';
//	-- regardless of commit rowsAffected we COMMIT;
//	COMMIT;
//
// FailScanningCommitsForFailedScanRuns (single bulk
// UPDATE in one tx):
//
//	UPDATE <schema>.commit c
//	   SET scan_status = 'failed'
//	  FROM <schema>.scan_run sr
//	 WHERE sr.repo_id = c.repo_id
//	   AND sr.to_sha = c.sha
//	   AND sr.sha_binding = 'single'
//	   AND sr.status = 'failed'
//	   AND c.scan_status = 'scanning'
//	   AND c.repo_id IN (
//	     SELECT repo_id FROM <schema>.commit
//	      WHERE scan_status = 'scanning' LIMIT $1
//	   );
//
// The LIMIT subselect is a defence against unbounded
// updates; the sweep caps it at `batchLimit * maxBatches`.
// (Postgres does not support `UPDATE ... LIMIT` directly,
// so we use a CTE / subquery; the actual SQL we issue uses
// a CTE for clarity.)

// FindStaleRunningScanRuns implements
// [StaleScanRunSweepStore] for the PG-backed store.
func (s *PGScanRunStore) FindStaleRunningScanRuns(ctx context.Context, olderThan time.Time, limit int) ([]StaleScanRun, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("metric_ingestor: PGScanRunStore.FindStaleRunningScanRuns: limit must be > 0, got %d", limit)
	}

	query := fmt.Sprintf(
		`SELECT scan_run_id, repo_id, kind::text, sha_binding::text, to_sha, started_at
		   FROM %s
		  WHERE status = 'running' AND started_at < $1
		  ORDER BY started_at ASC, scan_run_id ASC
		  LIMIT $2`,
		s.qualifyScanRun(),
	)
	rows, err := s.db.QueryContext(ctx, query, olderThan.UTC(), limit)
	if err != nil {
		return nil, fmt.Errorf("metric_ingestor: PGScanRunStore.FindStaleRunningScanRuns SELECT: %w", err)
	}
	defer rows.Close()

	out := make([]StaleScanRun, 0)
	for rows.Next() {
		var (
			scanRunID  uuid.UUID
			repoID     uuid.UUID
			kind       string
			shaBinding string
			toSHA      sql.NullString
			startedAt  time.Time
		)
		if err := rows.Scan(&scanRunID, &repoID, &kind, &shaBinding, &toSHA, &startedAt); err != nil {
			return nil, fmt.Errorf("metric_ingestor: PGScanRunStore.FindStaleRunningScanRuns Scan: %w", err)
		}
		sr := StaleScanRun{
			ScanRunID:  scanRunID,
			RepoID:     repoID,
			Kind:       kind,
			SHABinding: shaBinding,
			StartedAt:  startedAt,
		}
		if toSHA.Valid {
			sr.ToSHA = toSHA.String
		}
		out = append(out, sr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("metric_ingestor: PGScanRunStore.FindStaleRunningScanRuns rows.Err: %w", err)
	}
	return out, nil
}

// FailStaleScanRun implements [StaleScanRunSweepStore]
// for the PG-backed store. One transaction per row -- a
// row-update failure does not bleed into the next sweep
// iteration.
func (s *PGScanRunStore) FailStaleScanRun(ctx context.Context, stale StaleScanRun, endedAt time.Time) (FailStaleScanRunResult, error) {
	if err := validateStaleProjection(stale); err != nil {
		return FailStaleScanRunResult{}, err
	}
	if err := validateStaleTransitions(stale); err != nil {
		return FailStaleScanRunResult{}, err
	}
	if endedAt.IsZero() {
		return FailStaleScanRunResult{}, errors.New("metric_ingestor: PGScanRunStore.FailStaleScanRun: endedAt is the zero time")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return FailStaleScanRunResult{}, fmt.Errorf("metric_ingestor: PGScanRunStore.BeginTx (stale fail): %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var result FailStaleScanRunResult

	updateRunSQL := fmt.Sprintf(
		`UPDATE %s
		    SET status = 'failed', ended_at = $1
		  WHERE scan_run_id = $2 AND status = 'running'`,
		s.qualifyScanRun(),
	)
	res, err := tx.ExecContext(ctx, updateRunSQL, endedAt.UTC(), stale.ScanRunID)
	if err != nil {
		return FailStaleScanRunResult{}, fmt.Errorf("metric_ingestor: PGScanRunStore.FailStaleScanRun UPDATE scan_run: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return FailStaleScanRunResult{}, fmt.Errorf("metric_ingestor: PGScanRunStore.FailStaleScanRun RowsAffected (scan_run): %w", err)
	}
	if affected == 1 {
		result.ScanRunTransitioned = true
	}

	// Commit-side transition only when the binding is
	// single-SHA AND we have a target SHA. Per-row runs
	// have no (repo_id, sha) anchor, so the UPDATE would
	// match zero rows by definition.
	if stale.SHABinding == SHABindingSingle && stale.ToSHA != "" {
		updateCommitSQL := fmt.Sprintf(
			`UPDATE %s
			    SET scan_status = 'failed'
			  WHERE repo_id = $1 AND sha = $2 AND scan_status = 'scanning'`,
			s.qualifyCommit(),
		)
		cres, err := tx.ExecContext(ctx, updateCommitSQL, stale.RepoID, stale.ToSHA)
		if err != nil {
			return FailStaleScanRunResult{}, fmt.Errorf("metric_ingestor: PGScanRunStore.FailStaleScanRun UPDATE commit: %w", err)
		}
		caffected, err := cres.RowsAffected()
		if err != nil {
			return FailStaleScanRunResult{}, fmt.Errorf("metric_ingestor: PGScanRunStore.FailStaleScanRun RowsAffected (commit): %w", err)
		}
		if caffected == 1 {
			result.CommitTransitioned = true
		}
	}

	if err := tx.Commit(); err != nil {
		return FailStaleScanRunResult{}, fmt.Errorf("metric_ingestor: PGScanRunStore.FailStaleScanRun Commit: %w", err)
	}
	return result, nil
}

// FailScanningCommitsForFailedScanRuns implements the
// orphaned-scanning-commit cleanup step in
// [StaleScanRunSweepStore] for the PG-backed store.
//
// One bulk UPDATE inside a single transaction. The
// LIMIT-cap is enforced via a CTE so the UPDATE never
// affects more than `limit` rows -- a defence-in-depth
// guard against a pathological backlog overwhelming a
// single sweep tick.
func (s *PGScanRunStore) FailScanningCommitsForFailedScanRuns(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		return 0, fmt.Errorf("metric_ingestor: PGScanRunStore.FailScanningCommitsForFailedScanRuns: limit must be > 0, got %d", limit)
	}
	if err := repo_indexer.ValidateTransition(
		repo_indexer.ScanStatusScanning,
		repo_indexer.ScanStatusFailed,
	); err != nil {
		return 0, fmt.Errorf("metric_ingestor: PGScanRunStore.FailScanningCommitsForFailedScanRuns transition guard: %w", err)
	}

	commitTable := s.qualifyCommit()
	scanRunTable := s.qualifyScanRun()
	// We name the commit table twice (once for the inner
	// candidate CTE, once for the outer UPDATE). The CTE
	// captures the bounded candidate set BEFORE the
	// UPDATE; without it Postgres has no LIMIT in UPDATE.
	query := fmt.Sprintf(
		`WITH candidates AS (
		     SELECT c.repo_id, c.sha
		       FROM %s c
		       JOIN %s sr
		         ON sr.repo_id = c.repo_id
		        AND sr.to_sha = c.sha
		      WHERE c.scan_status = 'scanning'
		        AND sr.status = 'failed'
		        AND sr.sha_binding = 'single'
		      LIMIT $1
		 )
		 UPDATE %s c
		    SET scan_status = 'failed'
		   FROM candidates
		  WHERE c.repo_id = candidates.repo_id
		    AND c.sha = candidates.sha
		    AND c.scan_status = 'scanning'`,
		commitTable, scanRunTable, commitTable,
	)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("metric_ingestor: PGScanRunStore.BeginTx (commit cleanup): %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, query, limit)
	if err != nil {
		return 0, fmt.Errorf("metric_ingestor: PGScanRunStore.FailScanningCommitsForFailedScanRuns UPDATE: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("metric_ingestor: PGScanRunStore.FailScanningCommitsForFailedScanRuns RowsAffected: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return int(affected), fmt.Errorf("metric_ingestor: PGScanRunStore.FailScanningCommitsForFailedScanRuns Commit: %w", err)
	}
	return int(affected), nil
}

// compile-time interface assertion -- both stores satisfy
// [StaleScanRunSweepStore] so a forgotten method on either
// implementation is a build break, not a runtime
// `interface{}.(StaleScanRunSweepStore)` panic.
var _ StaleScanRunSweepStore = (*PGScanRunStore)(nil)
var _ StaleScanRunSweepStore = (*InMemoryScanRunStore)(nil)
