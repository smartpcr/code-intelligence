package metric_ingestor_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/metric_ingestor"
	"forge/services/clean-code/internal/repo_indexer"
)

// ---------------------------------------------------------------------------
// Test fixtures.
// ---------------------------------------------------------------------------

// staleFixtureRepoID is shared across all stale-sweep
// tests so log lines stay readable and we don't pay the
// uuid-parse cost per test.
var staleFixtureRepoID = uuid.Must(uuid.FromString("a0000000-0000-0000-0000-000000000001"))

// staleFixtureNow is the "now" the fake clock returns in
// the canonical scenarios.
var staleFixtureNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

// fixedClockAtFor returns a clock that always reports `t`.
// Pinned here so the stale-sweep tests don't pull in the
// state-machine helper (the two test surfaces are
// independent).
func staleFixedClock(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

func staleScanRunID(idx int) uuid.UUID {
	return uuid.Must(uuid.FromString(fmt.Sprintf("b1111111-2222-3333-4444-%012d", idx)))
}

func staleSHA(idx int) string {
	// Use a 40-char hex SHA so the matchings to the
	// migration's `commit.sha` text column remain
	// realistic.
	return fmt.Sprintf("%040x", idx)
}

// ---------------------------------------------------------------------------
// Sweep scenarios from impl-plan Stage 3.5.
// ---------------------------------------------------------------------------

// TestSweep_StaleScanRunBecomesFailed is the canonical
// scenario: a `scan_run(status='running')` row older than
// 30 minutes transitions to `status='failed'` -- NOT
// `orphaned`, NOT `superseded` (iter 1 evaluator item 2).
func TestSweep_StaleScanRunBecomesFailed(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	scanRunID := staleScanRunID(1)
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:   scanRunID,
		RepoID:      staleFixtureRepoID,
		SHA:         staleSHA(1),
		Kind:        metric_ingestor.ScanRunKindFull,
		SHABinding:  metric_ingestor.SHABindingSingle,
		StartedAt:   staleFixtureNow.Add(-45 * time.Minute), // older than 30min
		CommittedAt: staleFixtureNow.Add(-1 * time.Hour),
	})

	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep returned err=%v, want nil", err)
	}
	if report.Scanned != 1 {
		t.Errorf("report.Scanned=%d, want 1", report.Scanned)
	}
	if report.ScanRunsTransitioned != 1 {
		t.Errorf("report.ScanRunsTransitioned=%d, want 1", report.ScanRunsTransitioned)
	}

	got := store.ScanRunStatus(scanRunID)
	if got != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("ScanRunStatus=%q, want %q (NOT 'orphaned', NOT 'superseded')",
			got, metric_ingestor.ScanRunStatusFailed)
	}
	// Canonical state set guard: the value MUST be one of
	// the three canonical members. A literal like
	// `orphaned` or `superseded` would fail
	// ValidateScanRunStatus.
	if err := metric_ingestor.ValidateScanRunStatus(got); err != nil {
		t.Errorf("ValidateScanRunStatus(%q) returned %v -- the sweep produced a non-canonical status", got, err)
	}

	rec, ok := store.ScanRunRecord(scanRunID)
	if !ok {
		t.Fatal("ScanRunRecord lookup returned ok=false")
	}
	if rec.EndedAt.IsZero() {
		t.Errorf("ScanRunRecord.EndedAt is zero, want sweep-stamped value")
	}
	if !rec.EndedAt.Equal(staleFixtureNow) {
		t.Errorf("ScanRunRecord.EndedAt=%v, want %v", rec.EndedAt, staleFixtureNow)
	}

	if got := sweep.Metrics().StaleScansTotal(); got != 1 {
		t.Errorf("StaleScansTotal=%d, want 1", got)
	}
}

// TestSweep_StaleCommitBecomesFailed is the second canonical
// scenario: a commit at `scan_status='scanning'` whose
// scan_run was just marked failed transitions to
// `scan_status='failed'` in the same sweep pass.
func TestSweep_StaleCommitBecomesFailed(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	scanRunID := staleScanRunID(2)
	sha := staleSHA(2)
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  scanRunID,
		RepoID:     staleFixtureRepoID,
		SHA:        sha,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		StartedAt:  staleFixtureNow.Add(-2 * time.Hour),
	})

	if got := store.CommitStatus(staleFixtureRepoID, sha); got != repo_indexer.ScanStatusScanning {
		t.Fatalf("pre-sweep CommitStatus=%q, want %q", got, repo_indexer.ScanStatusScanning)
	}

	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep returned err=%v, want nil", err)
	}
	if report.CommitsTransitioned != 1 {
		t.Errorf("report.CommitsTransitioned=%d, want 1", report.CommitsTransitioned)
	}

	got := store.CommitStatus(staleFixtureRepoID, sha)
	if got != repo_indexer.ScanStatusFailed {
		t.Errorf("post-sweep CommitStatus=%q, want %q", got, repo_indexer.ScanStatusFailed)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("post-sweep CommitStatus is non-canonical: %v", err)
	}
	if got := sweep.Metrics().FailedCommitsTotal(); got != 1 {
		t.Errorf("FailedCommitsTotal=%d, want 1", got)
	}
}

// TestSweep_FreshScanRunIsNotTouched pins the "younger
// than scan_timeout" guard. A scan_run.started_at within
// the last 30 minutes must NOT be transitioned.
func TestSweep_FreshScanRunIsNotTouched(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	scanRunID := staleScanRunID(3)
	sha := staleSHA(3)
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  scanRunID,
		RepoID:     staleFixtureRepoID,
		SHA:        sha,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		StartedAt:  staleFixtureNow.Add(-29 * time.Minute), // fresh
	})

	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep err=%v", err)
	}
	if report.ScanRunsTransitioned != 0 {
		t.Errorf("ScanRunsTransitioned=%d, want 0 (fresh row must not be swept)", report.ScanRunsTransitioned)
	}
	if got := store.ScanRunStatus(scanRunID); got != metric_ingestor.ScanRunStatusRunning {
		t.Errorf("ScanRunStatus=%q, want still 'running'", got)
	}
	if got := store.CommitStatus(staleFixtureRepoID, sha); got != repo_indexer.ScanStatusScanning {
		t.Errorf("CommitStatus=%q, want still 'scanning'", got)
	}
	if got := sweep.Metrics().StaleScansTotal(); got != 0 {
		t.Errorf("StaleScansTotal=%d, want 0", got)
	}
}

// TestSweep_TerminalScanRunIsIgnored: a scan_run already at
// 'succeeded' is not picked up by Find (defence: the Find
// filter is `status='running'`). And the commit, if at
// 'scanned' or 'failed', is not touched.
func TestSweep_TerminalScanRunIsIgnored(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	store.SeedTerminalScanRun(metric_ingestor.SeedTerminalScanRunInput{
		ScanRunID:  staleScanRunID(4),
		RepoID:     staleFixtureRepoID,
		SHA:        staleSHA(4),
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		Status:     metric_ingestor.ScanRunStatusSucceeded,
		StartedAt:  staleFixtureNow.Add(-90 * time.Minute), // very stale
		EndedAt:    staleFixtureNow.Add(-60 * time.Minute),
	})

	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep err=%v", err)
	}
	if report.Scanned != 0 || report.ScanRunsTransitioned != 0 {
		t.Errorf("Terminal scan_run was picked up: Scanned=%d ScanRunsTransitioned=%d, want 0/0",
			report.Scanned, report.ScanRunsTransitioned)
	}
}

// TestSweep_PerRowBindingTransitionsScanRunOnly: a
// sha_binding='per_row' stale run is marked failed; the
// sweep does NOT attempt a commit update (per-row runs
// have no single-commit anchor).
func TestSweep_PerRowBindingTransitionsScanRunOnly(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	scanRunID := staleScanRunID(5)
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  scanRunID,
		RepoID:     staleFixtureRepoID,
		SHA:        "", // empty per the schema CHECK for per_row
		Kind:       metric_ingestor.ScanRunKindExternalPerRow,
		SHABinding: metric_ingestor.SHABindingPerRow,
		StartedAt:  staleFixtureNow.Add(-2 * time.Hour),
	})

	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep err=%v", err)
	}
	if report.ScanRunsTransitioned != 1 {
		t.Errorf("ScanRunsTransitioned=%d, want 1", report.ScanRunsTransitioned)
	}
	if report.CommitsTransitioned != 0 {
		t.Errorf("CommitsTransitioned=%d, want 0 (per-row has no commit linkage)", report.CommitsTransitioned)
	}
	if got := store.ScanRunStatus(scanRunID); got != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("ScanRunStatus=%q, want 'failed'", got)
	}
	if got := sweep.Metrics().StaleScansTotal(); got != 1 {
		t.Errorf("StaleScansTotal=%d, want 1", got)
	}
	if got := sweep.Metrics().FailedCommitsTotal(); got != 0 {
		t.Errorf("FailedCommitsTotal=%d, want 0", got)
	}
}

// TestSweep_CommitRacedToTerminalNotCounted: when the
// stale scan_run's matching commit is no longer
// `scanning` (raced by another path), the commit counter
// is NOT incremented even though the scan_run is.
func TestSweep_CommitRacedToTerminalNotCounted(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	scanRunID := staleScanRunID(6)
	sha := staleSHA(6)
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  scanRunID,
		RepoID:     staleFixtureRepoID,
		SHA:        sha,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		StartedAt:  staleFixtureNow.Add(-2 * time.Hour),
	})
	// Pre-race: commit transitions to 'failed' (e.g. an
	// operator state-edit) BEFORE the sweep runs.
	store.ForceCommitStatus(staleFixtureRepoID, sha, repo_indexer.ScanStatusFailed)

	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep err=%v", err)
	}
	if report.ScanRunsTransitioned != 1 {
		t.Errorf("ScanRunsTransitioned=%d, want 1", report.ScanRunsTransitioned)
	}
	if report.CommitsTransitioned != 0 {
		t.Errorf("CommitsTransitioned=%d, want 0 (commit was raced past 'scanning')", report.CommitsTransitioned)
	}
	if got := store.CommitStatus(staleFixtureRepoID, sha); got != repo_indexer.ScanStatusFailed {
		t.Errorf("CommitStatus=%q, want 'failed' (untouched by sweep)", got)
	}
	if got := sweep.Metrics().FailedCommitsTotal(); got != 0 {
		t.Errorf("FailedCommitsTotal=%d, want 0", got)
	}
}

// TestSweep_OrphanedScanningCommitCleanedUp covers the
// SECOND cleanup step: a commit at `scan_status='scanning'`
// whose owning scan_run is ALREADY failed (pre-seeded) MUST
// be transitioned to `scan_status='failed'` by the sweep's
// commit-cleanup pass.
func TestSweep_OrphanedScanningCommitCleanedUp(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	scanRunID := staleScanRunID(7)
	sha := staleSHA(7)
	store.SeedTerminalScanRun(metric_ingestor.SeedTerminalScanRunInput{
		ScanRunID:          scanRunID,
		RepoID:             staleFixtureRepoID,
		SHA:                sha,
		Kind:               metric_ingestor.ScanRunKindFull,
		SHABinding:         metric_ingestor.SHABindingSingle,
		Status:             metric_ingestor.ScanRunStatusFailed,
		StartedAt:          staleFixtureNow.Add(-90 * time.Minute),
		EndedAt:            staleFixtureNow.Add(-60 * time.Minute),
		SeedScanningCommit: true,
	})

	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep err=%v", err)
	}
	if report.OrphanedCommitsCleaned != 1 {
		t.Errorf("OrphanedCommitsCleaned=%d, want 1", report.OrphanedCommitsCleaned)
	}
	if got := store.CommitStatus(staleFixtureRepoID, sha); got != repo_indexer.ScanStatusFailed {
		t.Errorf("CommitStatus=%q, want 'failed'", got)
	}
	if got := sweep.Metrics().FailedCommitsTotal(); got != 1 {
		t.Errorf("FailedCommitsTotal=%d, want 1", got)
	}
}

// TestSweep_MultipleStaleRowsAllTransitioned: seed several
// stale rows + a fresh row; only the stale ones get swept.
func TestSweep_MultipleStaleRowsAllTransitioned(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()

	// 3 stale, 1 fresh.
	for i := 10; i <= 12; i++ {
		store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
			ScanRunID:  staleScanRunID(i),
			RepoID:     staleFixtureRepoID,
			SHA:        staleSHA(i),
			Kind:       metric_ingestor.ScanRunKindFull,
			SHABinding: metric_ingestor.SHABindingSingle,
			StartedAt:  staleFixtureNow.Add(time.Duration(-31-i) * time.Minute),
		})
	}
	freshSHA := staleSHA(99)
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  staleScanRunID(99),
		RepoID:     staleFixtureRepoID,
		SHA:        freshSHA,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		StartedAt:  staleFixtureNow.Add(-5 * time.Minute),
	})

	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep err=%v", err)
	}
	if report.Scanned != 3 {
		t.Errorf("Scanned=%d, want 3", report.Scanned)
	}
	if report.ScanRunsTransitioned != 3 {
		t.Errorf("ScanRunsTransitioned=%d, want 3", report.ScanRunsTransitioned)
	}
	if report.CommitsTransitioned != 3 {
		t.Errorf("CommitsTransitioned=%d, want 3", report.CommitsTransitioned)
	}
	// Fresh row untouched.
	if got := store.ScanRunStatus(staleScanRunID(99)); got != metric_ingestor.ScanRunStatusRunning {
		t.Errorf("fresh ScanRunStatus=%q, want 'running'", got)
	}
	if got := store.CommitStatus(staleFixtureRepoID, freshSHA); got != repo_indexer.ScanStatusScanning {
		t.Errorf("fresh CommitStatus=%q, want 'scanning'", got)
	}
	if got := sweep.Metrics().StaleScansTotal(); got != 3 {
		t.Errorf("StaleScansTotal=%d, want 3", got)
	}
	if got := sweep.Metrics().FailedCommitsTotal(); got != 3 {
		t.Errorf("FailedCommitsTotal=%d, want 3", got)
	}
}

// TestSweep_DrainMode: with drain=true and batch_limit=2,
// 5 stale rows should still all be transitioned in ONE
// Sweep call.
func TestSweep_DrainMode(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	for i := 20; i < 25; i++ {
		store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
			ScanRunID:  staleScanRunID(i),
			RepoID:     staleFixtureRepoID,
			SHA:        staleSHA(i),
			Kind:       metric_ingestor.ScanRunKindFull,
			SHABinding: metric_ingestor.SHABindingSingle,
			StartedAt:  staleFixtureNow.Add(time.Duration(-100-i) * time.Minute),
		})
	}
	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepBatchLimit(2),
		metric_ingestor.WithStaleSweepDrain(true),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)
	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep err=%v", err)
	}
	if report.ScanRunsTransitioned != 5 {
		t.Errorf("ScanRunsTransitioned=%d, want 5 (drain across 3 batches of 2/2/1)",
			report.ScanRunsTransitioned)
	}
}

// TestSweep_SingleBatchMode: drain=false stops after one
// batch even when more stale rows are queued.
func TestSweep_SingleBatchMode(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	for i := 30; i < 35; i++ {
		store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
			ScanRunID:  staleScanRunID(i),
			RepoID:     staleFixtureRepoID,
			SHA:        staleSHA(i),
			Kind:       metric_ingestor.ScanRunKindFull,
			SHABinding: metric_ingestor.SHABindingSingle,
			StartedAt:  staleFixtureNow.Add(time.Duration(-100-i) * time.Minute),
		})
	}
	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepBatchLimit(2),
		metric_ingestor.WithStaleSweepDrain(false),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)
	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep err=%v", err)
	}
	if report.ScanRunsTransitioned != 2 {
		t.Errorf("ScanRunsTransitioned=%d, want 2 (single-batch caps at batch_limit)",
			report.ScanRunsTransitioned)
	}
}

// TestSweep_ContextCanceledMidPass: a ctx cancelled mid-sweep
// returns ctx.Err() and stops at the current row.
func TestSweep_ContextCanceledMidPass(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	for i := 40; i < 43; i++ {
		store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
			ScanRunID:  staleScanRunID(i),
			RepoID:     staleFixtureRepoID,
			SHA:        staleSHA(i),
			Kind:       metric_ingestor.ScanRunKindFull,
			SHABinding: metric_ingestor.SHABindingSingle,
			StartedAt:  staleFixtureNow.Add(-2 * time.Hour),
		})
	}
	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := sweep.Sweep(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Sweep err=%v, want context.Canceled", err)
	}
}

// TestSweep_NoStaleRowsIsNoOp: empty store should produce
// a zero-counts report with no error.
func TestSweep_NoStaleRowsIsNoOp(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)
	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep err=%v", err)
	}
	if report.Scanned != 0 || report.ScanRunsTransitioned != 0 ||
		report.CommitsTransitioned != 0 || report.OrphanedCommitsCleaned != 0 {
		t.Errorf("non-zero report on empty store: %+v", report)
	}
}

// TestSweep_FailStaleScanRunRejectsBadProjection: feeds an
// invalid stale projection straight at the store to pin
// the canon-guards.
func TestSweep_FailStaleScanRunRejectsBadProjection(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	cases := []struct {
		name         string
		stale        metric_ingestor.StaleScanRun
		wantContains string
	}{
		{
			name: "zero scan_run_id",
			stale: metric_ingestor.StaleScanRun{
				RepoID:     staleFixtureRepoID,
				Kind:       metric_ingestor.ScanRunKindFull,
				SHABinding: metric_ingestor.SHABindingSingle,
				ToSHA:      staleSHA(1),
				StartedAt:  staleFixtureNow,
			},
			wantContains: "ScanRunID",
		},
		{
			name: "zero repo_id",
			stale: metric_ingestor.StaleScanRun{
				ScanRunID:  staleScanRunID(1),
				Kind:       metric_ingestor.ScanRunKindFull,
				SHABinding: metric_ingestor.SHABindingSingle,
				ToSHA:      staleSHA(1),
				StartedAt:  staleFixtureNow,
			},
			wantContains: "RepoID",
		},
		{
			name: "single binding with empty to_sha",
			stale: metric_ingestor.StaleScanRun{
				ScanRunID:  staleScanRunID(1),
				RepoID:     staleFixtureRepoID,
				Kind:       metric_ingestor.ScanRunKindFull,
				SHABinding: metric_ingestor.SHABindingSingle,
				StartedAt:  staleFixtureNow,
			},
			wantContains: "sha_binding='single'",
		},
		{
			name: "per_row binding with non-empty to_sha",
			stale: metric_ingestor.StaleScanRun{
				ScanRunID:  staleScanRunID(1),
				RepoID:     staleFixtureRepoID,
				Kind:       metric_ingestor.ScanRunKindExternalPerRow,
				SHABinding: metric_ingestor.SHABindingPerRow,
				ToSHA:      staleSHA(1),
				StartedAt:  staleFixtureNow,
			},
			wantContains: "sha_binding='per_row'",
		},
		{
			name: "bogus kind",
			stale: metric_ingestor.StaleScanRun{
				ScanRunID:  staleScanRunID(1),
				RepoID:     staleFixtureRepoID,
				Kind:       "orphaned", // forbidden literal
				SHABinding: metric_ingestor.SHABindingSingle,
				ToSHA:      staleSHA(1),
				StartedAt:  staleFixtureNow,
			},
			wantContains: "scan run kind",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.FailStaleScanRun(context.Background(), tc.stale, staleFixtureNow)
			if err == nil {
				t.Fatal("FailStaleScanRun returned nil err, want validation failure")
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("err=%v, want substring %q", err, tc.wantContains)
			}
		})
	}
}

// TestSweep_FailStaleScanRunUnknownIDIsNoOp: feeding a
// well-formed projection whose ScanRunID is unknown to the
// store returns the zero-result with no error (treated as
// a raced no-op).
func TestSweep_FailStaleScanRunUnknownIDIsNoOp(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	result, err := store.FailStaleScanRun(
		context.Background(),
		metric_ingestor.StaleScanRun{
			ScanRunID:  staleScanRunID(123),
			RepoID:     staleFixtureRepoID,
			Kind:       metric_ingestor.ScanRunKindFull,
			SHABinding: metric_ingestor.SHABindingSingle,
			ToSHA:      staleSHA(123),
			StartedAt:  staleFixtureNow,
		},
		staleFixtureNow,
	)
	if err != nil {
		t.Errorf("err=%v, want nil for unknown id (raced no-op)", err)
	}
	if result.ScanRunTransitioned || result.CommitTransitioned {
		t.Errorf("result=%+v, want zero (no row transitioned for unknown id)", result)
	}
}

// TestSweep_FindStaleRejectsNonPositiveLimit: the limit
// arg is a hard input guard.
func TestSweep_FindStaleRejectsNonPositiveLimit(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	if _, err := store.FindStaleRunningScanRuns(context.Background(), staleFixtureNow, 0); err == nil {
		t.Error("FindStaleRunningScanRuns(limit=0) returned nil err")
	}
	if _, err := store.FindStaleRunningScanRuns(context.Background(), staleFixtureNow, -1); err == nil {
		t.Error("FindStaleRunningScanRuns(limit=-1) returned nil err")
	}
}

// TestSweep_FindStaleOrdersOldestFirst: ordering is the
// store's contract.
func TestSweep_FindStaleOrdersOldestFirst(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	// Insert OUT of order to expose any sort bugs.
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  staleScanRunID(51),
		RepoID:     staleFixtureRepoID,
		SHA:        staleSHA(51),
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		StartedAt:  staleFixtureNow.Add(-50 * time.Minute),
	})
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  staleScanRunID(52),
		RepoID:     staleFixtureRepoID,
		SHA:        staleSHA(52),
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		StartedAt:  staleFixtureNow.Add(-100 * time.Minute), // OLDEST
	})
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  staleScanRunID(53),
		RepoID:     staleFixtureRepoID,
		SHA:        staleSHA(53),
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		StartedAt:  staleFixtureNow.Add(-75 * time.Minute),
	})

	rows, err := store.FindStaleRunningScanRuns(context.Background(),
		staleFixtureNow.Add(-30*time.Minute), 10)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("rows=%d, want 3", len(rows))
	}
	wantOrder := []uuid.UUID{
		staleScanRunID(52),
		staleScanRunID(53),
		staleScanRunID(51),
	}
	for i, want := range wantOrder {
		if rows[i].ScanRunID != want {
			t.Errorf("row[%d].ScanRunID=%s, want %s", i, rows[i].ScanRunID, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Metrics exposition (Prometheus text format).
// ---------------------------------------------------------------------------

// TestMetrics_WriteTextEmitsBothCounters: the
// Prometheus-text exposition output MUST include both
// canonical counter names with their current values.
func TestMetrics_WriteTextEmitsBothCounters(t *testing.T) {
	m := metric_ingestor.NewStaleScanRunSweepMetrics()
	m.IncStaleScans(3)
	m.IncFailedCommits(7)

	var buf bytes.Buffer
	if _, err := m.WriteText(&buf); err != nil {
		t.Fatalf("WriteText err=%v", err)
	}
	text := buf.String()
	wantSubstrings := []string{
		"# TYPE " + metric_ingestor.MetricNameStaleScansTotal + " counter",
		metric_ingestor.MetricNameStaleScansTotal + " 3",
		"# TYPE " + metric_ingestor.MetricNameFailedCommitsTotal + " counter",
		metric_ingestor.MetricNameFailedCommitsTotal + " 7",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(text, want) {
			t.Errorf("WriteText output missing %q:\n%s", want, text)
		}
	}
}

// TestMetrics_NameStability pins the canonical metric
// names so a rename anywhere flips this test. The brief
// explicitly names these counters (impl-plan Stage 3.5
// line 345).
func TestMetrics_NameStability(t *testing.T) {
	if got := metric_ingestor.MetricNameStaleScansTotal; got != "cleancode_sweep_stale_scans_total" {
		t.Errorf("MetricNameStaleScansTotal=%q, want %q", got, "cleancode_sweep_stale_scans_total")
	}
	if got := metric_ingestor.MetricNameFailedCommitsTotal; got != "cleancode_sweep_failed_commits_total" {
		t.Errorf("MetricNameFailedCommitsTotal=%q, want %q", got, "cleancode_sweep_failed_commits_total")
	}
}

// TestMetrics_NilSafeReaders: reading the counters from a
// nil holder returns 0 (defensive shape so a future
// composition-root forgetting to pass a holder doesn't
// crash).
func TestMetrics_NilSafeReaders(t *testing.T) {
	var m *metric_ingestor.StaleScanRunSweepMetrics
	if got := m.StaleScansTotal(); got != 0 {
		t.Errorf("nil holder StaleScansTotal=%d, want 0", got)
	}
	if got := m.FailedCommitsTotal(); got != 0 {
		t.Errorf("nil holder FailedCommitsTotal=%d, want 0", got)
	}
	// IncX on nil holder is also a no-op.
	m.IncStaleScans(1)
	m.IncFailedCommits(1)
}

// ---------------------------------------------------------------------------
// Constructor guards.
// ---------------------------------------------------------------------------

func TestNewStaleScanRunSweep_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewStaleScanRunSweep(nil) did not panic")
		}
	}()
	_ = metric_ingestor.NewStaleScanRunSweep(nil)
}

func TestNewStaleScanRunSweep_PanicsOnNonPositiveTimeout(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewStaleScanRunSweep(timeout=0) did not panic")
		}
	}()
	store := metric_ingestor.NewInMemoryScanRunStore()
	_ = metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(0))
}

func TestNewStaleScanRunSweep_PanicsOnNonPositiveBatchLimit(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewStaleScanRunSweep(batch=0) did not panic")
		}
	}()
	store := metric_ingestor.NewInMemoryScanRunStore()
	_ = metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepBatchLimit(0))
}

// ---------------------------------------------------------------------------
// Loop driver tests.
// ---------------------------------------------------------------------------

// TestLoop_RunsSweepOnEntryAndOnCadence: with
// runOnStart=true and a 5-min cadence (test uses smaller
// values), the loop calls Sweep once immediately and then
// once per cadence tick.
func TestLoop_RunsSweepOnEntryAndOnCadence(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	// Seed one stale row that EACH sweep call will mark
	// failed (we re-seed in the test wrapper below). To
	// keep this test simple we instead count Sweep calls
	// via a wrapping store.
	wrap := &countingSweepStore{inner: store}

	sweep := metric_ingestor.NewStaleScanRunSweep(wrap,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	sleepCh := make(chan time.Time)
	var sleepCalls int32
	sleepFn := func(_ time.Duration) <-chan time.Time {
		atomic.AddInt32(&sleepCalls, 1)
		return sleepCh
	}

	loop := metric_ingestor.NewStaleScanRunSweepLoop(sweep,
		metric_ingestor.WithStaleSweepLoopCadence(5*time.Minute),
		metric_ingestor.WithStaleSweepLoopSleep(sleepFn),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- loop.Run(ctx) }()

	// The loop runs Sweep ONCE on entry (runOnStart=true),
	// then calls sleepFn(cadence) -- block. Drive the
	// channel twice to release two cadence sleeps -> two
	// more Sweep calls.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&sleepCalls) >= 1 && wrap.findCalls() >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if wrap.findCalls() < 1 {
		t.Fatalf("loop did not run initial Sweep within 2s, findCalls=%d", wrap.findCalls())
	}

	// Trigger 2 more cadence ticks.
	sleepCh <- time.Now()
	sleepCh <- time.Now()

	// Wait for the corresponding Sweep calls.
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if wrap.findCalls() >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := wrap.findCalls(); got < 3 {
		t.Errorf("findCalls=%d, want >=3 (1 initial + 2 cadence)", got)
	}

	cancel()
	// Drain any pending sleep so the wait returns.
	select {
	case sleepCh <- time.Now():
	default:
	}
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run err=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}
}

// TestLoop_SkipsImmediateRunWhenRunOnStartFalse pins the
// no-immediate-pass shape for tests that want to control
// the first tick.
func TestLoop_SkipsImmediateRunWhenRunOnStartFalse(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	wrap := &countingSweepStore{inner: store}
	sweep := metric_ingestor.NewStaleScanRunSweep(wrap,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	sleepCh := make(chan time.Time)
	sleepFn := func(_ time.Duration) <-chan time.Time {
		return sleepCh
	}
	loop := metric_ingestor.NewStaleScanRunSweepLoop(sweep,
		metric_ingestor.WithStaleSweepLoopCadence(5*time.Minute),
		metric_ingestor.WithStaleSweepLoopSleep(sleepFn),
		metric_ingestor.WithStaleSweepLoopRunOnStart(false),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	runErr := make(chan error, 1)
	go func() { runErr <- loop.Run(ctx) }()

	// Give the loop a chance to (incorrectly) run a
	// startup Sweep -- we want to assert it does NOT.
	time.Sleep(50 * time.Millisecond)
	if got := wrap.findCalls(); got != 0 {
		t.Errorf("findCalls before first cadence tick=%d, want 0 (runOnStart=false)", got)
	}

	cancel()
	select {
	case sleepCh <- time.Now():
	default:
	}
	<-runErr
}

// TestLoop_SurvivesSweepError: a failed Sweep call does
// NOT terminate the loop; it backs off errorBackoff and
// continues.
func TestLoop_SurvivesSweepError(t *testing.T) {
	store := &alwaysErrFindStore{}
	sweep := metric_ingestor.NewStaleScanRunSweep(store,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	sleepCh := make(chan time.Time)
	var sleepCalls int32
	sleepFn := func(_ time.Duration) <-chan time.Time {
		atomic.AddInt32(&sleepCalls, 1)
		return sleepCh
	}

	loop := metric_ingestor.NewStaleScanRunSweepLoop(sweep,
		metric_ingestor.WithStaleSweepLoopCadence(50*time.Millisecond),
		metric_ingestor.WithStaleSweepLoopErrorBackoff(50*time.Millisecond),
		metric_ingestor.WithStaleSweepLoopSleep(sleepFn),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := make(chan error, 1)
	go func() { runErr <- loop.Run(ctx) }()

	// Wait for at least 2 errors → 2 sleep calls.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.calls() >= 2 && atomic.LoadInt32(&sleepCalls) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if store.calls() < 1 {
		t.Errorf("store.calls=%d, want >=1 (loop must keep calling Sweep through errors)", store.calls())
	}

	cancel()
	select {
	case sleepCh <- time.Now():
	default:
	}
	select {
	case err := <-runErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run err=%v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s after cancel")
	}
}

// TestLoop_ExitsOnContextCancel: a loop with an already-
// cancelled ctx exits before any Sweep call.
func TestLoop_ExitsOnContextCancel(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	wrap := &countingSweepStore{inner: store}
	sweep := metric_ingestor.NewStaleScanRunSweep(wrap,
		metric_ingestor.WithStaleSweepScanTimeout(30*time.Minute),
		metric_ingestor.WithStaleSweepClock(staleFixedClock(staleFixtureNow)),
	)

	loop := metric_ingestor.NewStaleScanRunSweepLoop(sweep,
		metric_ingestor.WithStaleSweepLoopCadence(5*time.Minute),
	)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := loop.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run err=%v, want context.Canceled", err)
	}
	if got := wrap.findCalls(); got != 0 {
		t.Errorf("findCalls=%d, want 0 (cancelled-on-entry loop must not call Sweep)", got)
	}
}

// TestNewStaleScanRunSweepLoop_PanicsOnNilSweep pins the
// constructor's defensive contract.
func TestNewStaleScanRunSweepLoop_PanicsOnNilSweep(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewStaleScanRunSweepLoop(nil) did not panic")
		}
	}()
	_ = metric_ingestor.NewStaleScanRunSweepLoop(nil)
}

func TestNewStaleScanRunSweepLoop_PanicsOnNonPositiveCadence(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("NewStaleScanRunSweepLoop(cadence=0) did not panic")
		}
	}()
	store := metric_ingestor.NewInMemoryScanRunStore()
	sweep := metric_ingestor.NewStaleScanRunSweep(store)
	_ = metric_ingestor.NewStaleScanRunSweepLoop(sweep,
		metric_ingestor.WithStaleSweepLoopCadence(0))
}

// ---------------------------------------------------------------------------
// Test doubles.
// ---------------------------------------------------------------------------

// countingSweepStore wraps an InMemoryScanRunStore and
// counts the number of times Find / Fail were called.
// Used to assert the loop drove Sweep without
// interrogating internal store state.
type countingSweepStore struct {
	inner *metric_ingestor.InMemoryScanRunStore
	finds atomic.Int32
	fails atomic.Int32
}

func (c *countingSweepStore) findCalls() int { return int(c.finds.Load()) }

func (c *countingSweepStore) FindStaleRunningScanRuns(ctx context.Context, olderThan time.Time, limit int) ([]metric_ingestor.StaleScanRun, error) {
	c.finds.Add(1)
	return c.inner.FindStaleRunningScanRuns(ctx, olderThan, limit)
}

func (c *countingSweepStore) FailStaleScanRun(ctx context.Context, stale metric_ingestor.StaleScanRun, endedAt time.Time) (metric_ingestor.FailStaleScanRunResult, error) {
	c.fails.Add(1)
	return c.inner.FailStaleScanRun(ctx, stale, endedAt)
}

func (c *countingSweepStore) FailScanningCommitsForFailedScanRuns(ctx context.Context, limit int) (int, error) {
	return c.inner.FailScanningCommitsForFailedScanRuns(ctx, limit)
}

// alwaysErrFindStore returns an infrastructure error from
// every FindStaleRunningScanRuns call. Used to verify the
// loop survives Find failures.
type alwaysErrFindStore struct {
	count atomic.Int32
}

func (a *alwaysErrFindStore) calls() int { return int(a.count.Load()) }

func (a *alwaysErrFindStore) FindStaleRunningScanRuns(_ context.Context, _ time.Time, _ int) ([]metric_ingestor.StaleScanRun, error) {
	a.count.Add(1)
	return nil, errors.New("simulated find failure")
}

func (a *alwaysErrFindStore) FailStaleScanRun(_ context.Context, _ metric_ingestor.StaleScanRun, _ time.Time) (metric_ingestor.FailStaleScanRunResult, error) {
	return metric_ingestor.FailStaleScanRunResult{}, errors.New("unreachable -- Find errors before Fail")
}

func (a *alwaysErrFindStore) FailScanningCommitsForFailedScanRuns(_ context.Context, _ int) (int, error) {
	return 0, nil
}

// ---------------------------------------------------------------------------
// Reason-attribution tests (iter 2 evaluator item 4).
// ---------------------------------------------------------------------------
//
// The Stage 3.5 brief says: "transitions them to status='failed' with a
// sweep-attributed reason". The implementation-plan.md "Attribution
// surface" subsection makes structured logs the canonical attribution
// surface (no DB column added). These tests pin that contract.

// TestStaleSweepReason_IsCanonical guards the canonical
// reason string for scan_run transitions. A rename here is
// caught as a build break if a downstream test or log
// dashboard references the string literal; tests pin the
// string so accidental drift is loud.
func TestStaleSweepReason_IsCanonical(t *testing.T) {
	t.Parallel()
	if metric_ingestor.StaleSweepReason != "stale_scan_run_timeout" {
		t.Errorf("StaleSweepReason=%q, want %q",
			metric_ingestor.StaleSweepReason, "stale_scan_run_timeout")
	}
}

// TestFailedCommitSweepReason_IsCanonical guards the
// canonical reason string for commit cleanup transitions.
func TestFailedCommitSweepReason_IsCanonical(t *testing.T) {
	t.Parallel()
	if metric_ingestor.FailedCommitSweepReason != "stale_scanning_commit_for_failed_run" {
		t.Errorf("FailedCommitSweepReason=%q, want %q",
			metric_ingestor.FailedCommitSweepReason, "stale_scanning_commit_for_failed_run")
	}
}

// TestSweep_EmitsCanonicalReasonForScanRunTransition pins
// the scenario "sweep-emits-canonical-reason" from
// implementation-plan.md: when the sweep transitions a
// stale scan_run, the structured log line carries
// reason="stale_scan_run_timeout". This is the queryable
// attribution surface: an operator running
// `kubectl logs | grep reason=stale_scan_run_timeout`
// must land on every sweep-attributed failure.
func TestSweep_EmitsCanonicalReasonForScanRunTransition(t *testing.T) {
	t.Parallel()
	store := metric_ingestor.NewInMemoryScanRunStore()

	// Seed one stale scan_run that the sweep will transition.
	scanRunID := uuid.Must(uuid.FromString("bbbbbbbb-cccc-dddd-eeee-ffff00000001"))
	staleStartedAt := staleFixtureNow.Add(-31 * time.Minute)
	store.SeedRunningScanRun(metric_ingestor.SeedRunningScanRunInput{
		ScanRunID:  scanRunID,
		RepoID:     staleFixtureRepoID,
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		SHA:        "deadbeef00000000000000000000000000000001",
		StartedAt:  staleStartedAt,
	})
	// SeedRunningScanRun above already sets the commit to
	// scanning when SHABinding=single, but we re-assert
	// it explicitly so the test is robust to a future
	// refactor of the seed helper.
	store.ForceCommitStatus(staleFixtureRepoID,
		"deadbeef00000000000000000000000000000001",
		repo_indexer.ScanStatusScanning)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sweep := metric_ingestor.NewStaleScanRunSweep(
		store,
		metric_ingestor.WithStaleSweepClock(func() time.Time { return staleFixtureNow }),
		metric_ingestor.WithStaleSweepLogger(logger),
	)
	if _, err := sweep.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: err=%v, want nil", err)
	}

	out := buf.String()
	// Each JSON log line is a separate object. Parse and
	// filter for the canonical events.
	var sawScanRunTransition bool
	var sawCommitTransition bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("log line is not JSON: %q (%v)", line, err)
			continue
		}
		msg, _ := rec["msg"].(string)
		reason, _ := rec["reason"].(string)
		switch {
		case strings.Contains(msg, "scan_run transitioned"):
			if reason != metric_ingestor.StaleSweepReason {
				t.Errorf("scan_run transition log: reason=%q, want %q",
					reason, metric_ingestor.StaleSweepReason)
			}
			sawScanRunTransition = true
		case strings.Contains(msg, "commit transitioned"):
			if reason != metric_ingestor.StaleSweepReason {
				t.Errorf("commit transition log: reason=%q, want %q",
					reason, metric_ingestor.StaleSweepReason)
			}
			sawCommitTransition = true
		}
	}
	if !sawScanRunTransition {
		t.Errorf("did not see scan_run transition log line; full log:\n%s", out)
	}
	if !sawCommitTransition {
		t.Errorf("did not see commit transition log line; full log:\n%s", out)
	}
}

// TestSweep_EmitsCanonicalReasonForOrphanedCommitCleanup
// pins the cleanup-step reason: when the sweep cleans up
// an orphaned commit at scanning state whose owning
// scan_run is already failed, the structured log line
// carries reason="stale_scanning_commit_for_failed_run".
func TestSweep_EmitsCanonicalReasonForOrphanedCommitCleanup(t *testing.T) {
	t.Parallel()
	store := metric_ingestor.NewInMemoryScanRunStore()

	// Seed a TERMINAL-FAILED scan_run + orphaned scanning
	// commit. The first sweep step finds nothing (no
	// 'running' rows). The cleanup step picks up the
	// orphaned commit and transitions it; that's the log
	// line under test.
	failedScanRunID := uuid.Must(uuid.FromString("bbbbbbbb-cccc-dddd-eeee-ffff00000002"))
	failedSHA := "deadbeef00000000000000000000000000000002"
	store.SeedTerminalScanRun(metric_ingestor.SeedTerminalScanRunInput{
		ScanRunID:          failedScanRunID,
		RepoID:             staleFixtureRepoID,
		Kind:               metric_ingestor.ScanRunKindFull,
		SHABinding:         metric_ingestor.SHABindingSingle,
		SHA:                failedSHA,
		StartedAt:          staleFixtureNow.Add(-1 * time.Hour),
		EndedAt:            staleFixtureNow.Add(-45 * time.Minute),
		Status:             metric_ingestor.ScanRunStatusFailed,
		SeedScanningCommit: true,
	})

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	sweep := metric_ingestor.NewStaleScanRunSweep(
		store,
		metric_ingestor.WithStaleSweepClock(func() time.Time { return staleFixtureNow }),
		metric_ingestor.WithStaleSweepLogger(logger),
	)
	report, err := sweep.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: err=%v, want nil", err)
	}
	if report.OrphanedCommitsCleaned != 1 {
		t.Fatalf("OrphanedCommitsCleaned=%d, want 1", report.OrphanedCommitsCleaned)
	}

	out := buf.String()
	var sawCleanup bool
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		msg, _ := rec["msg"].(string)
		reason, _ := rec["reason"].(string)
		if strings.Contains(msg, "orphaned scanning commits transitioned") {
			if reason != metric_ingestor.FailedCommitSweepReason {
				t.Errorf("orphaned commit cleanup log: reason=%q, want %q",
					reason, metric_ingestor.FailedCommitSweepReason)
			}
			sawCleanup = true
		}
	}
	if !sawCleanup {
		t.Errorf("did not see orphaned commit cleanup log line; full log:\n%s", out)
	}
}
