package metric_ingestor

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/repo_indexer"
)

// This file teaches [InMemoryScanRunStore] the
// [StaleScanRunSweepStore] interface. Kept in its own file
// so the new surface area is locally diff-able and the
// Stage 3.2 state-machine fixture in state.go remains
// focused on the claim/finalize lifecycle.
//
// Methods are added as receivers on the existing
// [InMemoryScanRunStore] type; Go's package-level method
// scoping lets us extend the type from any file in the
// package.
//
// # Test-fixture helpers
//
// The stale-sweep test scenarios (see
// stale_scan_run_sweep_test.go) need a way to seed
// `scan_run(status='running')` rows DIRECTLY -- without
// going through the full ClaimNextPendingCommit lifecycle
// -- so they can construct synthetic stale fixtures with
// an arbitrary `started_at`. The [SeedRunningScanRun] and
// [SeedTerminalScanRun] helpers fill that gap.

// SeedRunningScanRunInput configures a synthetic
// `scan_run(status='running')` row plus its matching
// `commit(scan_status='scanning')` row for the stale-sweep
// test fixtures.
//
// Sets up the same canonical pair the StateMachine's
// ClaimNextPendingCommit produces -- but at an arbitrary
// past `StartedAt`, so the test can assert the sweep
// transitions a row that's older than `scan_timeout`.
type SeedRunningScanRunInput struct {
	// ScanRunID is the synthesised primary key. The
	// caller MUST supply a non-zero UUID so the test's
	// assertions can identify the row.
	ScanRunID uuid.UUID
	// RepoID identifies the parent repo. Non-zero.
	RepoID uuid.UUID
	// SHA is the matching commit's SHA. When SHABinding
	// is "single", SHA MUST be a non-empty 40-char hex
	// string; when "per_row", SHA must be empty (the
	// schema CHECK enforces this in production).
	SHA string
	// Kind is one of [AllScanRunKinds].
	Kind string
	// SHABinding is "single" or "per_row".
	SHABinding string
	// StartedAt is stamped on `scan_run.started_at` AND
	// is the field the sweep's "older than cutoff" guard
	// compares against.
	StartedAt time.Time
	// CommittedAt is stamped on the synthesised commit
	// row's `committed_at`. Only meaningful for
	// SHABinding="single". Defaults to StartedAt if zero.
	CommittedAt time.Time
}

// SeedRunningScanRun inserts a synthetic
// `scan_run(status='running')` row into the in-memory
// store. When SHABinding=="single" the matching commit
// row is recorded at `scan_status='scanning'` so the
// sweep can transition it.
//
// PANICS on a zero ScanRunID / RepoID, an unknown Kind /
// SHABinding, or a SHA inconsistent with the binding --
// these are test-fixture wiring bugs and should fail
// loudly.
func (s *InMemoryScanRunStore) SeedRunningScanRun(in SeedRunningScanRunInput) {
	if in.ScanRunID == uuid.Nil {
		panic("metric_ingestor: SeedRunningScanRun: ScanRunID is zero")
	}
	if in.RepoID == uuid.Nil {
		panic("metric_ingestor: SeedRunningScanRun: RepoID is zero")
	}
	if err := ValidateScanRunKind(in.Kind); err != nil {
		panic(fmt.Sprintf("metric_ingestor: SeedRunningScanRun: %v", err))
	}
	if err := ValidateSHABinding(in.SHABinding); err != nil {
		panic(fmt.Sprintf("metric_ingestor: SeedRunningScanRun: %v", err))
	}
	if in.SHABinding == SHABindingSingle && in.SHA == "" {
		panic("metric_ingestor: SeedRunningScanRun: SHA required for sha_binding='single'")
	}
	if in.SHABinding == SHABindingPerRow && in.SHA != "" {
		panic("metric_ingestor: SeedRunningScanRun: SHA must be empty for sha_binding='per_row'")
	}
	if in.StartedAt.IsZero() {
		panic("metric_ingestor: SeedRunningScanRun: StartedAt is zero")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.runs[in.ScanRunID] = &inMemoryScanRunRecord{
		ScanRunID:  in.ScanRunID,
		RepoID:     in.RepoID,
		ToSHA:      in.SHA,
		Kind:       in.Kind,
		SHABinding: in.SHABinding,
		Status:     ScanRunStatusRunning,
		StartedAt:  in.StartedAt,
	}

	// Only single-bound runs have a (repo_id, sha) commit
	// linkage. Per-row runs hang their SHA off each
	// emitted metric_sample row, not a single commit.
	if in.SHABinding == SHABindingSingle {
		s.commitStatus[commitKey(in.RepoID, in.SHA)] = repo_indexer.ScanStatusScanning
	}
}

// SeedTerminalScanRunInput configures a synthetic
// `scan_run(status=...)` row at a terminal status (one of
// `succeeded` / `failed`), plus an optional commit row at
// `scan_status='scanning'` -- the "orphaned scanning
// commit" fixture the sweep's second cleanup pass tests
// against.
type SeedTerminalScanRunInput struct {
	ScanRunID  uuid.UUID
	RepoID     uuid.UUID
	SHA        string
	Kind       string
	SHABinding string
	Status     ScanRunStatus
	StartedAt  time.Time
	EndedAt    time.Time
	// SeedScanningCommit: when true AND
	// SHABinding=="single", the matching commit row is
	// recorded at `scan_status='scanning'` so the sweep's
	// second cleanup pass observes the orphaned row.
	SeedScanningCommit bool
}

// SeedTerminalScanRun inserts a synthetic terminal
// `scan_run` row (status='succeeded' or 'failed') into the
// in-memory store. PANICS on invalid inputs.
func (s *InMemoryScanRunStore) SeedTerminalScanRun(in SeedTerminalScanRunInput) {
	if in.ScanRunID == uuid.Nil {
		panic("metric_ingestor: SeedTerminalScanRun: ScanRunID is zero")
	}
	if in.RepoID == uuid.Nil {
		panic("metric_ingestor: SeedTerminalScanRun: RepoID is zero")
	}
	if err := ValidateScanRunKind(in.Kind); err != nil {
		panic(fmt.Sprintf("metric_ingestor: SeedTerminalScanRun: %v", err))
	}
	if err := ValidateSHABinding(in.SHABinding); err != nil {
		panic(fmt.Sprintf("metric_ingestor: SeedTerminalScanRun: %v", err))
	}
	if in.Status != ScanRunStatusSucceeded && in.Status != ScanRunStatusFailed {
		panic(fmt.Sprintf("metric_ingestor: SeedTerminalScanRun: Status must be terminal (succeeded|failed), got %q", in.Status))
	}
	if in.SHABinding == SHABindingSingle && in.SHA == "" {
		panic("metric_ingestor: SeedTerminalScanRun: SHA required for sha_binding='single'")
	}
	if in.StartedAt.IsZero() {
		panic("metric_ingestor: SeedTerminalScanRun: StartedAt is zero")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.runs[in.ScanRunID] = &inMemoryScanRunRecord{
		ScanRunID:  in.ScanRunID,
		RepoID:     in.RepoID,
		ToSHA:      in.SHA,
		Kind:       in.Kind,
		SHABinding: in.SHABinding,
		Status:     in.Status,
		StartedAt:  in.StartedAt,
		EndedAt:    in.EndedAt,
	}

	if in.SeedScanningCommit && in.SHABinding == SHABindingSingle {
		s.commitStatus[commitKey(in.RepoID, in.SHA)] = repo_indexer.ScanStatusScanning
	}
}

// FindStaleRunningScanRuns implements
// [StaleScanRunSweepStore]. Returns up to `limit` scan_run
// rows in `running` status with `started_at < olderThan`,
// ordered by `started_at ASC` (oldest first).
func (s *InMemoryScanRunStore) FindStaleRunningScanRuns(ctx context.Context, olderThan time.Time, limit int) ([]StaleScanRun, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, fmt.Errorf("metric_ingestor: InMemoryScanRunStore.FindStaleRunningScanRuns: limit must be > 0, got %d", limit)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Snapshot matching rows under the lock so we can sort
	// and slice without holding the lock during the
	// caller's processing loop.
	matches := make([]StaleScanRun, 0)
	for _, r := range s.runs {
		if r == nil {
			continue
		}
		if r.Status != ScanRunStatusRunning {
			continue
		}
		if !r.StartedAt.Before(olderThan) {
			continue
		}
		matches = append(matches, StaleScanRun{
			ScanRunID:  r.ScanRunID,
			RepoID:     r.RepoID,
			Kind:       r.Kind,
			SHABinding: r.SHABinding,
			ToSHA:      r.ToSHA,
			StartedAt:  r.StartedAt,
		})
	}

	// Deterministic order: started_at ASC, then ScanRunID
	// ASC for ties. The state-machine's tests already rely
	// on UUID-tie-break determinism elsewhere; staying
	// consistent.
	sort.Slice(matches, func(i, j int) bool {
		if !matches[i].StartedAt.Equal(matches[j].StartedAt) {
			return matches[i].StartedAt.Before(matches[j].StartedAt)
		}
		return uuidLess(matches[i].ScanRunID, matches[j].ScanRunID)
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}
	return matches, nil
}

// FailStaleScanRun implements [StaleScanRunSweepStore].
// Mutations are performed under the store mutex so the
// scan_run + commit pair lands atomically (mirroring the
// PG store's single-transaction shape).
//
// The two transitions:
//
//  1. scan_run.status: `running` -> `failed` (no-op when
//     already terminal -- raced finalize by another
//     writer).
//  2. commit.scan_status: `scanning` -> `failed` (only
//     when SHABinding=='single' AND the commit row exists
//     AND its current status is `scanning`).
func (s *InMemoryScanRunStore) FailStaleScanRun(ctx context.Context, stale StaleScanRun, endedAt time.Time) (FailStaleScanRunResult, error) {
	if err := ctx.Err(); err != nil {
		return FailStaleScanRunResult{}, err
	}
	if err := validateStaleProjection(stale); err != nil {
		return FailStaleScanRunResult{}, err
	}
	if err := validateStaleTransitions(stale); err != nil {
		return FailStaleScanRunResult{}, err
	}
	if endedAt.IsZero() {
		return FailStaleScanRunResult{}, fmt.Errorf("metric_ingestor: FailStaleScanRun: endedAt is the zero time")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	var result FailStaleScanRunResult

	rec, ok := s.runs[stale.ScanRunID]
	if !ok {
		// Unknown row -- treat as raced no-op rather than
		// a hard error so the sweep's outer loop is
		// resilient to a store that was wiped between
		// Find and Fail.
		return result, nil
	}
	if rec.Status == ScanRunStatusRunning {
		rec.Status = ScanRunStatusFailed
		rec.EndedAt = endedAt
		result.ScanRunTransitioned = true
	}
	// Even when ScanRunTransitioned is false (the run was
	// raced to terminal by another writer) we still want
	// to clean up any commit that ended up at `scanning`
	// for THIS run -- the orphaned-commit pass would catch
	// it next tick, but doing it inline is cheaper and
	// keeps the per-row contract tight.
	if stale.SHABinding == SHABindingSingle && stale.ToSHA != "" {
		key := commitKey(stale.RepoID, stale.ToSHA)
		if cur, exists := s.commitStatus[key]; exists && cur == repo_indexer.ScanStatusScanning {
			if err := repo_indexer.ValidateTransition(cur, repo_indexer.ScanStatusFailed); err != nil {
				return result, fmt.Errorf("metric_ingestor: FailStaleScanRun: commit transition guard: %w", err)
			}
			s.commitStatus[key] = repo_indexer.ScanStatusFailed
			result.CommitTransitioned = true
		}
	}
	return result, nil
}

// FailScanningCommitsForFailedScanRuns implements the
// "orphaned scanning commit" cleanup step in
// [StaleScanRunSweepStore]. Walks every commit at
// `scan_status='scanning'` and, if its owning scan_run is
// `status='failed'` AND `sha_binding='single'`, transitions
// the commit to `scan_status='failed'`.
func (s *InMemoryScanRunStore) FailScanningCommitsForFailedScanRuns(ctx context.Context, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if limit <= 0 {
		return 0, fmt.Errorf("metric_ingestor: FailScanningCommitsForFailedScanRuns: limit must be > 0, got %d", limit)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Build a (repo_id, to_sha) -> failed? lookup over
	// scan_runs so we don't repeat the O(scan_runs) scan
	// per commit. Single-bound failed runs only -- per-row
	// runs have no (repo_id, single_sha) anchor.
	failedSingle := make(map[string]bool, len(s.runs))
	for _, r := range s.runs {
		if r == nil {
			continue
		}
		if r.Status != ScanRunStatusFailed {
			continue
		}
		if r.SHABinding != SHABindingSingle {
			continue
		}
		if r.ToSHA == "" {
			continue
		}
		failedSingle[commitKey(r.RepoID, r.ToSHA)] = true
	}

	// Collect candidate keys deterministically (the map
	// iteration order is non-deterministic). Stable order
	// makes tests reproducible.
	type candidate struct {
		key string
	}
	candidates := make([]candidate, 0)
	for key, status := range s.commitStatus {
		if status != repo_indexer.ScanStatusScanning {
			continue
		}
		if !failedSingle[key] {
			continue
		}
		candidates = append(candidates, candidate{key: key})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].key < candidates[j].key
	})

	transitioned := 0
	for _, c := range candidates {
		if transitioned >= limit {
			break
		}
		cur := s.commitStatus[c.key]
		if cur != repo_indexer.ScanStatusScanning {
			// Raced between snapshot and write -- skip.
			continue
		}
		if err := repo_indexer.ValidateTransition(cur, repo_indexer.ScanStatusFailed); err != nil {
			return transitioned, fmt.Errorf("metric_ingestor: FailScanningCommitsForFailedScanRuns: transition guard: %w", err)
		}
		s.commitStatus[c.key] = repo_indexer.ScanStatusFailed
		transitioned++
	}
	return transitioned, nil
}

// uuidLess returns true iff a < b in big-endian byte
// order. Used for deterministic tie-breaking when two
// rows share a timestamp.
func uuidLess(a, b uuid.UUID) bool {
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// ForceCommitStatus is a TEST-ONLY helper that sets the
// `commit.scan_status` for `(repoID, sha)` directly,
// bypassing the canonical transition graph. Used by the
// stale-sweep tests to construct race-condition fixtures
// (e.g. a commit that ALREADY transitioned to 'failed'
// before the sweep ran).
//
// PANICS if `status` is not a canonical [repo_indexer.
// ScanStatus] value -- the test fixture is expected to
// land at a valid state.
//
// Do NOT use this from production code -- the production
// path goes through ClaimNextPendingCommit /
// FinalizeScanRun / FailStaleScanRun which honour the
// transition graph.
func (s *InMemoryScanRunStore) ForceCommitStatus(repoID uuid.UUID, sha string, status repo_indexer.ScanStatus) {
	if err := status.Validate(); err != nil {
		panic(fmt.Sprintf("metric_ingestor: ForceCommitStatus: %v", err))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commitStatus[commitKey(repoID, sha)] = status
}
