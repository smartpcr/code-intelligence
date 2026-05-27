package metric_ingestor

// This file implements the Stage 3.5 stale-ScanRun sweep -- a
// periodic cleanup pass that finds `scan_run(status='running')`
// rows older than `scan_timeout` (tech-spec Sec 8.2 = 30 min)
// and transitions them to `status='failed'`. The sweep also
// cleans up `commit.scan_status='scanning'` rows whose owning
// `scan_run` is already terminal (failed) -- the Metric
// Ingestor is the sole writer of `commit.scan_status`, so the
// cleanup is fully in-process (architecture Sec 1.5.1 row 1).
//
// # Why a separate file from sweep.go
//
// The brief at implementation-plan Stage 3.5 names
// `internal/metric_ingestor/sweep.go` as the target, but that
// path is already owned by the per-call [ChurnSweep] (Stage
// 2.6). Renaming or repurposing the existing file would
// break the ChurnSweep stage's already-evaluator-approved
// invariants. This file is the additive landing site for the
// stale-sweep types, and the package-level docstring on
// sweep.go remains accurate for the materialiser pipeline.
//
// # Canonical state set (iter 1 evaluator item 2)
//
// The sweep ONLY uses the canonical [ScanRunStatus] values --
// `running -> failed`. There is no `orphaned`, no
// `superseded`. Tests pin this so an iter-2 regression that
// re-introduces a non-canonical literal fails fast.
//
// # Same-process invariant
//
// The brief pins: "Metric Ingestor is the sole writer, so
// this is in-process." Every `commit.scan_status` UPDATE
// the sweep issues goes through the SAME application path
// the [StateMachine] uses (the [StaleScanRunSweepStore]
// implementation calls the same [pq] driver against the same
// DB role grants). No cross-service RPC, no work queue.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/repo_indexer"
)

// Sentinel errors emitted by the stale-sweep layer.
var (
	// ErrStaleSweepNilStore is returned by
	// [NewStaleScanRunSweep] when the store argument is nil.
	// The dependency is non-optional; a nil here is always a
	// composition-root wiring bug.
	ErrStaleSweepNilStore = errors.New("metric_ingestor: NewStaleScanRunSweep: store is nil")
	// ErrStaleSweepNonPositiveTimeout is returned when the
	// configured scan_timeout is <= 0. A zero timeout would
	// mark EVERY in-flight scan stale on the first sweep
	// tick, which is always a wiring bug. Pinned at
	// construction time so the failure mode is loud.
	ErrStaleSweepNonPositiveTimeout = errors.New("metric_ingestor: NewStaleScanRunSweep: scan_timeout must be > 0")
	// ErrStaleSweepNonPositiveBatch is returned when the
	// configured batch limit is <= 0. A zero limit would
	// silently no-op the sweep; pinned at construction.
	ErrStaleSweepNonPositiveBatch = errors.New("metric_ingestor: NewStaleScanRunSweep: batch_limit must be > 0")
)

// StaleSweepReason is the canonical reason string the sweep
// stamps on its structured log line when transitioning a
// `scan_run.status='running'` to `failed`. There is no
// `scan_run.reason` column on the schema (migration 0001),
// so the reason lives ONLY in logs and metrics labels
// (architecture Sec 5.7 / runbook "Failure handling").
const StaleSweepReason = "stale_scan_run_timeout"

// FailedCommitSweepReason is the structured log reason
// stamped when cleaning up a `commit.scan_status='scanning'`
// row whose owning `scan_run` is already terminal-failed.
// Same lives-in-logs rationale as [StaleSweepReason].
const FailedCommitSweepReason = "stale_scanning_commit_for_failed_run"

// StaleScanRun is the projection of one
// `scan_run(status='running', started_at < olderThan)` row
// the sweep's store layer hands back.
//
// The SHA-binding split:
//   - SHABinding == "single": [ToSHA] is the matching commit's
//     SHA; the sweep WILL transition that row's
//     `commit.scan_status` from `scanning` to `failed`.
//   - SHABinding == "per_row": [ToSHA] is empty (the schema
//     CHECK `scan_run_sha_binding_consistent` enforces
//     to_sha IS NULL for per_row); the sweep marks the
//     scan_run failed but DOES NOT attempt a commit update
//     (per-row runs have no single (repo_id, sha) anchor).
type StaleScanRun struct {
	// ScanRunID is the offending row's primary key.
	ScanRunID uuid.UUID
	// RepoID is the owning repo's `repo_id`. Carried so the
	// sweep can target the `commit` row without re-reading.
	RepoID uuid.UUID
	// Kind is the `scan_run.kind` literal (e.g. "full",
	// "delta", "external_per_row"). Used for structured
	// logs.
	Kind string
	// SHABinding is the `scan_run.sha_binding` literal --
	// "single" or "per_row" per migration 0001.
	SHABinding string
	// ToSHA is the `scan_run.to_sha` value when
	// SHABinding=="single"; empty otherwise.
	ToSHA string
	// StartedAt is `scan_run.started_at`. Used in the
	// sweep's log line so the operator can correlate.
	StartedAt time.Time
}

// FailStaleScanRunResult reports which rows the
// [StaleScanRunSweepStore.FailStaleScanRun] actually
// transitioned.
//
// The two booleans are independent because:
//   - ScanRunTransitioned can be false when another writer
//     (e.g. the state machine's own finalize) already raced
//     the row to a terminal state. The sweep treats this as
//     a benign no-op (the desired end-state is reached
//     either way) and does NOT count it.
//   - CommitTransitioned can be false when [SHABinding] is
//     "per_row" (no commit linkage exists) OR when the
//     matching commit row already raced past `scanning`.
//     Both are benign.
type FailStaleScanRunResult struct {
	// ScanRunTransitioned is true iff the
	// `scan_run.status = 'failed'` UPDATE affected 1 row.
	ScanRunTransitioned bool
	// CommitTransitioned is true iff the
	// `commit.scan_status = 'failed'` UPDATE affected 1 row.
	CommitTransitioned bool
}

// StaleScanRunSweepStore is the persistence seam every
// stale-sweep call writes through. The interface is
// intentionally separate from [ScanRunStore] (Interface
// Segregation): the sweep does not need claim/finalize
// surfaces, and existing [ScanRunStore] callers should not
// gain stale-sweep surface area.
//
// Implementations: [InMemoryScanRunStore] (test double) and
// [PGScanRunStore] (production).
type StaleScanRunSweepStore interface {
	// FindStaleRunningScanRuns returns up to `limit`
	// scan_run rows with `status='running'` AND
	// `started_at < olderThan`. Results are ordered by
	// `started_at ASC` so the OLDEST stale row is processed
	// first (matching the human "drain the longest-stuck
	// first" expectation).
	//
	// `limit` MUST be >= 1; implementations return a
	// validation error otherwise.
	//
	// The method does NOT lock the returned rows -- the
	// matching [FailStaleScanRun] call uses a
	// `WHERE status='running'` guard inside its UPDATE so a
	// concurrent writer that races ahead is detected via
	// RowsAffected==0 (no error, just
	// [FailStaleScanRunResult.ScanRunTransitioned]=false).
	FindStaleRunningScanRuns(ctx context.Context, olderThan time.Time, limit int) ([]StaleScanRun, error)

	// FailStaleScanRun atomically transitions:
	//   - `scan_run.status` from `running` to `failed`
	//     (only when currently `running`),
	//   - `scan_run.ended_at` to `endedAt`,
	//   - and (when `stale.SHABinding=="single"`)
	//     `commit.scan_status` from `scanning` to `failed`
	//     (only when currently `scanning`).
	//
	// Returns the per-row outcome via
	// [FailStaleScanRunResult]. Both transitions go through
	// [repo_indexer.ValidateTransition] BEFORE the store
	// touches the DB so the canonical state diagram is
	// enforced at the application layer.
	//
	// A non-nil error indicates infrastructure failure or a
	// canon-guard violation. Concurrent-race detection
	// (RowsAffected==0) is NOT an error.
	FailStaleScanRun(ctx context.Context, stale StaleScanRun, endedAt time.Time) (FailStaleScanRunResult, error)

	// FailScanningCommitsForFailedScanRuns is the
	// "orphaned scanning commit" cleanup step. Given the
	// brief: "Sweep also cleans up commit.scan_status =
	// 'scanning' rows whose owning scan_run has failed".
	//
	// The store transitions every `commit.scan_status =
	// 'scanning'` row whose matching
	// `scan_run(status='failed', sha_binding='single',
	//          to_sha=commit.sha)` is already failed -- in
	// other words, any commit that was abandoned by an
	// already-failed (but not yet cleaned up) scan_run.
	//
	// Implementations MUST honour the
	// `pending->scanning->{scanned|failed}` transition
	// diagram via [repo_indexer.ValidateTransition].
	//
	// Returns the count of commit rows transitioned. A
	// zero count is not an error.
	FailScanningCommitsForFailedScanRuns(ctx context.Context, limit int) (int, error)
}

// StaleScanRunSweepMetrics holds the two atomic counters
// the brief requires (impl-plan Stage 3.5 line 345):
//
//   - cleancode_sweep_stale_scans_total
//   - cleancode_sweep_failed_commits_total
//
// The two metric names are the canonical Prometheus
// identifiers; the exposed [StaleScanRunSweepMetrics.WriteText]
// method emits a Prometheus text-exposition snippet so an
// operator can plug the counters into an existing
// `/metrics` mux without taking a runtime dep on
// `prometheus/client_golang`. (The clean-code service has
// no Prometheus client lib wired today; the text-exposition
// shape is the canon and lets us layer on the official
// client lib later without renaming the metric.)
//
// All accessors are safe for concurrent use -- the
// underlying atomic.Uint64 values are read/written via
// atomic loads.
type StaleScanRunSweepMetrics struct {
	staleScansTotal    atomic.Uint64
	failedCommitsTotal atomic.Uint64
}

// MetricNameStaleScansTotal is the canonical Prometheus
// counter name for stale `scan_run(status='running')` rows
// the sweep transitioned to `status='failed'`.
const MetricNameStaleScansTotal = "cleancode_sweep_stale_scans_total"

// MetricNameFailedCommitsTotal is the canonical Prometheus
// counter name for `commit.scan_status='scanning'` rows the
// sweep transitioned to `scan_status='failed'`.
const MetricNameFailedCommitsTotal = "cleancode_sweep_failed_commits_total"

// NewStaleScanRunSweepMetrics returns a zero-initialised
// metrics holder. Returned as a pointer so the counters can
// be shared between the sweep and the future Prometheus
// exporter without copying.
func NewStaleScanRunSweepMetrics() *StaleScanRunSweepMetrics {
	return &StaleScanRunSweepMetrics{}
}

// StaleScansTotal returns the current value of the
// `cleancode_sweep_stale_scans_total` counter.
func (m *StaleScanRunSweepMetrics) StaleScansTotal() uint64 {
	if m == nil {
		return 0
	}
	return m.staleScansTotal.Load()
}

// FailedCommitsTotal returns the current value of the
// `cleancode_sweep_failed_commits_total` counter.
func (m *StaleScanRunSweepMetrics) FailedCommitsTotal() uint64 {
	if m == nil {
		return 0
	}
	return m.failedCommitsTotal.Load()
}

// IncStaleScans increments the stale-scans counter by `n`.
// Exported so future composition roots can pre-seed the
// counter (e.g. after a restart with an `--initial-counts`
// flag); the sweep itself calls this in-line.
func (m *StaleScanRunSweepMetrics) IncStaleScans(n uint64) {
	if m == nil || n == 0 {
		return
	}
	m.staleScansTotal.Add(n)
}

// IncFailedCommits increments the failed-commits counter
// by `n`.
func (m *StaleScanRunSweepMetrics) IncFailedCommits(n uint64) {
	if m == nil || n == 0 {
		return
	}
	m.failedCommitsTotal.Add(n)
}

// WriteText emits the two counters in Prometheus text
// exposition format (the v0.0.4 line-based encoding -- the
// stable scrape protocol every Prometheus version since
// 2017 has supported). Returns the number of bytes written
// and any I/O error. The format is:
//
//	# HELP cleancode_sweep_stale_scans_total ...
//	# TYPE cleancode_sweep_stale_scans_total counter
//	cleancode_sweep_stale_scans_total <value>
//	# HELP cleancode_sweep_failed_commits_total ...
//	# TYPE cleancode_sweep_failed_commits_total counter
//	cleancode_sweep_failed_commits_total <value>
//
// Operators wire this into an existing `/metrics`
// `http.HandlerFunc` -- the dedicated Prometheus client lib
// is not required to make the counters scrapeable.
func (m *StaleScanRunSweepMetrics) WriteText(w io.Writer) (int, error) {
	stale := m.StaleScansTotal()
	failed := m.FailedCommitsTotal()
	return fmt.Fprintf(w,
		"# HELP %s Stale scan_run(status='running') rows transitioned to 'failed' by the periodic sweep (tech-spec Sec 8.2).\n"+
			"# TYPE %s counter\n"+
			"%s %d\n"+
			"# HELP %s commit.scan_status='scanning' rows transitioned to 'failed' by the periodic sweep (orphaned scanning commits whose owning scan_run is already failed).\n"+
			"# TYPE %s counter\n"+
			"%s %d\n",
		MetricNameStaleScansTotal, MetricNameStaleScansTotal,
		MetricNameStaleScansTotal, stale,
		MetricNameFailedCommitsTotal, MetricNameFailedCommitsTotal,
		MetricNameFailedCommitsTotal, failed,
	)
}

// StaleScanRunSweepOption configures a
// [StaleScanRunSweep]. Functional-options pattern matching
// the rest of the package.
type StaleScanRunSweepOption func(*StaleScanRunSweep)

// WithStaleSweepScanTimeout overrides the staleness
// threshold. The sweep treats a `scan_run.status='running'`
// row as stale iff `started_at < now() - scanTimeout`.
// Default: [config.DefaultScanTimeout] (30 min, tech-spec
// Sec 8.2). PANICS at [NewStaleScanRunSweep] when <= 0.
func WithStaleSweepScanTimeout(d time.Duration) StaleScanRunSweepOption {
	return func(s *StaleScanRunSweep) { s.scanTimeout = d }
}

// WithStaleSweepBatchLimit overrides the maximum stale
// rows the sweep processes in ONE call to
// [StaleScanRunSweepStore.FindStaleRunningScanRuns].
// Default: 100. PANICS at construction when <= 0.
//
// When [WithStaleSweepDrain] is true, multiple batches
// drain per Sweep call; the batch limit just bounds each
// SELECT.
func WithStaleSweepBatchLimit(n int) StaleScanRunSweepOption {
	return func(s *StaleScanRunSweep) { s.batchLimit = n }
}

// WithStaleSweepDrain toggles drain-mode. When true, one
// [StaleScanRunSweep.Sweep] call repeatedly fetches batches
// until the store returns an empty result -- guarantees a
// large backlog is processed in one pass instead of waiting
// `cadence` between batches.
//
// Default: true (drain). Set to false in tests that want
// to assert single-batch behaviour.
func WithStaleSweepDrain(b bool) StaleScanRunSweepOption {
	return func(s *StaleScanRunSweep) { s.drain = b }
}

// WithStaleSweepMaxBatches caps the number of batches
// processed in drain mode. Guards against a pathological
// store that keeps returning rows (test bug or a clock-skew
// production scenario). Default: 1024 batches per pass --
// at batch_limit=100 that's ~100k stale rows per pass, far
// beyond any realistic backlog.
func WithStaleSweepMaxBatches(n int) StaleScanRunSweepOption {
	return func(s *StaleScanRunSweep) { s.maxBatches = n }
}

// WithStaleSweepClock overrides the wall clock used to
// compute the staleness cutoff (`now() - scan_timeout`) and
// to stamp `scan_run.ended_at`. Default: [time.Now].
// Tests inject a fixed clock for determinism.
func WithStaleSweepClock(now func() time.Time) StaleScanRunSweepOption {
	return func(s *StaleScanRunSweep) { s.now = now }
}

// WithStaleSweepLogger overrides the logger. nil silences
// the sweep (the zero value is permitted -- the production
// composition root wires the service slog instance).
func WithStaleSweepLogger(log *slog.Logger) StaleScanRunSweepOption {
	return func(s *StaleScanRunSweep) { s.log = log }
}

// WithStaleSweepMetrics overrides the metrics holder. The
// sweep allocates a fresh one if none is supplied. Pass an
// existing holder when wiring the sweep into a Prometheus
// exporter so the same counters are visible at the scrape
// endpoint.
func WithStaleSweepMetrics(m *StaleScanRunSweepMetrics) StaleScanRunSweepOption {
	return func(s *StaleScanRunSweep) { s.metrics = m }
}

// SweepReport summarises the outcome of one
// [StaleScanRunSweep.Sweep] call. Designed for the
// composition root's INFO-level structured log line and
// for tests' assertions. The counters in the report match
// the metric values incremented during the same call.
type SweepReport struct {
	// Scanned is the total number of stale rows the store
	// returned across every batch in the pass.
	Scanned int
	// ScanRunsTransitioned counts the scan_run rows whose
	// `status` we successfully UPDATEd from `running` to
	// `failed`. Always <= Scanned (a concurrent finalize
	// may race ahead and leave the count below the
	// scanned total).
	ScanRunsTransitioned int
	// CommitsTransitioned counts the commit rows whose
	// `scan_status` we successfully UPDATEd from `scanning`
	// to `failed`. Always <= ScanRunsTransitioned (per-row
	// runs have no commit linkage; concurrent races may
	// also leave the count below the run total).
	CommitsTransitioned int
	// OrphanedCommitsCleaned is the count returned by the
	// store's [StaleScanRunSweepStore.
	// FailScanningCommitsForFailedScanRuns] call -- the
	// second cleanup step over commits whose owning
	// scan_run was already terminal-failed before this
	// sweep ran.
	OrphanedCommitsCleaned int
}

// StaleScanRunSweep performs one stale-sweep pass per
// [StaleScanRunSweep.Sweep] call. The companion
// [StaleScanRunSweepLoop] (in stale_scan_run_sweep_loop.go)
// drives the pass periodically; this type is the per-call
// building block so unit tests can exercise the sweep
// without running an actual goroutine loop.
//
// # Per-row atomicity
//
// Each stale row's transition is a separate
// [StaleScanRunSweepStore.FailStaleScanRun] call -- a
// failed call does NOT abort the whole sweep. The sweep
// logs the failure and continues. Each successful call
// increments the matching metrics counters exactly once.
//
// # Race with the normal StateMachine finalize path
//
// The [StateMachine] in state.go has its own finalize path
// for in-process scan completion. When the sweep wins a
// race against the state machine, the state machine's
// subsequent finalize sees RowsAffected==0 (the WHERE
// `status='running'` guard fails) and returns
// [ErrConcurrentFinalize] -- expected behaviour, not a
// bug. The two writers are complementary, not
// duplicative.
type StaleScanRunSweep struct {
	store       StaleScanRunSweepStore
	scanTimeout time.Duration
	batchLimit  int
	drain       bool
	maxBatches  int
	now         func() time.Time
	log         *slog.Logger
	metrics     *StaleScanRunSweepMetrics
}

const (
	defaultStaleSweepBatchLimit = 100
	defaultStaleSweepMaxBatches = 1024
)

// NewStaleScanRunSweep constructs a [StaleScanRunSweep]
// wired to `store`. PANICS on a nil store or non-positive
// timeout / batch limit -- these are composition-root
// wiring errors and always surface immediately rather than
// at first Sweep call.
//
// Defaults:
//   - scan_timeout = 30 min (tech-spec Sec 8.2)
//   - batch_limit  = 100
//   - drain        = true
//   - max_batches  = 1024
//   - clock        = time.Now
//   - log          = nil (silent)
//   - metrics      = a fresh zero-initialised holder
func NewStaleScanRunSweep(store StaleScanRunSweepStore, opts ...StaleScanRunSweepOption) *StaleScanRunSweep {
	if store == nil {
		panic(ErrStaleSweepNilStore)
	}
	// Default scan_timeout is sourced from
	// [config.DefaultScanTimeout] so the canonical tech-spec
	// Sec 8.2 value is the single source of truth -- a
	// future tech-spec edit ripples here automatically (iter
	// 2 evaluator item 2).
	s := &StaleScanRunSweep{
		store:       store,
		scanTimeout: config.DefaultScanTimeout,
		batchLimit:  defaultStaleSweepBatchLimit,
		drain:       true,
		maxBatches:  defaultStaleSweepMaxBatches,
		now:         time.Now,
		metrics:     NewStaleScanRunSweepMetrics(),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.scanTimeout <= 0 {
		panic(ErrStaleSweepNonPositiveTimeout)
	}
	if s.batchLimit <= 0 {
		panic(ErrStaleSweepNonPositiveBatch)
	}
	if s.maxBatches <= 0 {
		s.maxBatches = defaultStaleSweepMaxBatches
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.metrics == nil {
		s.metrics = NewStaleScanRunSweepMetrics()
	}
	return s
}

// Metrics returns the metrics holder backing the sweep.
// Exposed so the composition root can register the same
// counters with a Prometheus exporter without re-allocating.
func (s *StaleScanRunSweep) Metrics() *StaleScanRunSweepMetrics {
	return s.metrics
}

// ScanTimeout returns the configured staleness threshold.
// Exposed for tests and for the loop driver's startup log.
func (s *StaleScanRunSweep) ScanTimeout() time.Duration {
	return s.scanTimeout
}

// Sweep performs ONE stale-sweep pass.
//
// Steps:
//
//  1. Compute the staleness cutoff as `now() - scan_timeout`.
//  2. Fetch up to `batch_limit` stale scan_runs from the
//     store. If drain-mode is on, repeat (up to
//     max_batches times) until the store returns 0 rows.
//  3. For each stale row, call
//     [StaleScanRunSweepStore.FailStaleScanRun]. Increment
//     metrics on success; log + continue on failure.
//  4. After the run-level pass, call
//     [StaleScanRunSweepStore.FailScanningCommitsForFailedScanRuns]
//     to clean up any commit at `scan_status='scanning'`
//     whose owning scan_run is ALREADY terminal-failed
//     (covers operator-edited or pre-existing failed runs
//     whose commit cleanup did not happen earlier).
//
// Returns a [SweepReport] summarising the pass and an
// error iff the FIRST store call fails (subsequent
// per-row failures are logged + folded into the report
// without aborting -- a single bad row should not block
// the rest of the queue).
//
// The function honours ctx cancellation between batches /
// rows; a cancelled ctx returns ctx.Err() with whatever
// counts have already accumulated.
func (s *StaleScanRunSweep) Sweep(ctx context.Context) (SweepReport, error) {
	if err := ctx.Err(); err != nil {
		return SweepReport{}, err
	}
	// Capture `now` ONCE so cutoff and endedAt come from
	// the same instant. Under real wall-clock the two
	// separate s.now() calls drift by ns/us (usually
	// harmless but logically incoherent -- endedAt should
	// be >= cutoff); under a test clock that advances on
	// each call, the drift is observable and makes the
	// sweep non-deterministic.
	now := s.now()
	cutoff := now.Add(-s.scanTimeout)
	endedAt := now

	var report SweepReport
	maxBatches := s.maxBatches
	if !s.drain {
		maxBatches = 1
	}

	if s.log != nil {
		s.log.Debug("metric_ingestor stale sweep: starting",
			"cutoff", cutoff,
			"scan_timeout", s.scanTimeout,
			"batch_limit", s.batchLimit,
			"drain", s.drain,
		)
	}

	for batch := 0; batch < maxBatches; batch++ {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		stale, err := s.store.FindStaleRunningScanRuns(ctx, cutoff, s.batchLimit)
		if err != nil {
			if s.log != nil {
				s.log.Warn("metric_ingestor stale sweep: FindStaleRunningScanRuns failed",
					"err", err,
					"batch", batch,
					"scanned_so_far", report.Scanned,
				)
			}
			return report, fmt.Errorf("metric_ingestor: stale sweep find: %w", err)
		}
		if len(stale) == 0 {
			break
		}
		report.Scanned += len(stale)
		for _, sr := range stale {
			if err := ctx.Err(); err != nil {
				return report, err
			}
			res, ferr := s.store.FailStaleScanRun(ctx, sr, endedAt)
			if ferr != nil {
				if s.log != nil {
					s.log.Warn("metric_ingestor stale sweep: FailStaleScanRun failed",
						"err", ferr,
						"scan_run_id", sr.ScanRunID,
						"repo_id", sr.RepoID,
						"sha", sr.ToSHA,
						"sha_binding", sr.SHABinding,
						"started_at", sr.StartedAt,
						"reason", StaleSweepReason,
					)
				}
				continue
			}
			if res.ScanRunTransitioned {
				report.ScanRunsTransitioned++
				s.metrics.IncStaleScans(1)
				if s.log != nil {
					s.log.Info("metric_ingestor stale sweep: scan_run transitioned",
						"scan_run_id", sr.ScanRunID,
						"repo_id", sr.RepoID,
						"sha", sr.ToSHA,
						"sha_binding", sr.SHABinding,
						"started_at", sr.StartedAt,
						"ended_at", endedAt,
						"reason", StaleSweepReason,
						"scan_timeout", s.scanTimeout,
					)
				}
			}
			if res.CommitTransitioned {
				report.CommitsTransitioned++
				s.metrics.IncFailedCommits(1)
				if s.log != nil {
					s.log.Info("metric_ingestor stale sweep: commit transitioned",
						"scan_run_id", sr.ScanRunID,
						"repo_id", sr.RepoID,
						"sha", sr.ToSHA,
						"reason", StaleSweepReason,
					)
				}
			}
		}
		// If the store returned fewer than batchLimit rows,
		// we've drained the queue -- no need to round-trip
		// again.
		if len(stale) < s.batchLimit {
			break
		}
	}

	// Step 4: orphaned-scanning-commit cleanup. This is
	// SEPARATE from the per-row stale-scan transition pass
	// because it covers commits whose owning scan_run is
	// already terminal-failed -- runs that were finalised
	// (manually or by a previous sweep) but whose commit
	// row was left stranded at `scanning`. The brief
	// explicitly calls this out: "Sweep also cleans up
	// commit.scan_status='scanning' rows whose owning
	// scan_run has failed".
	if err := ctx.Err(); err == nil {
		cleaned, cerr := s.store.FailScanningCommitsForFailedScanRuns(ctx, s.batchLimit*s.maxBatches)
		if cerr != nil {
			if s.log != nil {
				s.log.Warn("metric_ingestor stale sweep: FailScanningCommitsForFailedScanRuns failed",
					"err", cerr,
					"reason", FailedCommitSweepReason,
				)
			}
			// Soft failure -- the run-level pass already
			// succeeded for whatever it could land. Surface
			// the per-step failure via the returned error,
			// but do NOT discard the partial report.
			return report, fmt.Errorf("metric_ingestor: stale sweep commit-cleanup: %w", cerr)
		}
		report.OrphanedCommitsCleaned = cleaned
		if cleaned > 0 {
			s.metrics.IncFailedCommits(uint64(cleaned))
			if s.log != nil {
				s.log.Info("metric_ingestor stale sweep: orphaned scanning commits transitioned",
					"count", cleaned,
					"reason", FailedCommitSweepReason,
				)
			}
		}
	}

	if s.log != nil {
		s.log.Info("metric_ingestor stale sweep: pass complete",
			"scanned", report.Scanned,
			"scan_runs_transitioned", report.ScanRunsTransitioned,
			"commits_transitioned", report.CommitsTransitioned,
			"orphaned_commits_cleaned", report.OrphanedCommitsCleaned,
		)
	}

	return report, nil
}

// validateStaleProjection runs the canon-guards a store
// implementation MUST apply before persisting a
// [StaleScanRun] projection. Centralised here so the
// in-memory and PG stores enforce the SAME invariants.
//
// Checks (in order):
//   - ScanRunID is not the zero UUID.
//   - RepoID is not the zero UUID.
//   - Kind is in [AllScanRunKinds].
//   - SHABinding is in {single, per_row}.
//   - When SHABinding == "single", ToSHA is non-empty.
//   - When SHABinding == "per_row", ToSHA is empty.
//
// The last two checks mirror the schema-level
// `scan_run_sha_binding_consistent` CHECK constraint
// (migration 0001_catalog_lifecycle.up.sql:385).
func validateStaleProjection(sr StaleScanRun) error {
	if sr.ScanRunID == uuid.Nil {
		return errors.New("metric_ingestor: StaleScanRun.ScanRunID is the zero UUID")
	}
	if sr.RepoID == uuid.Nil {
		return errors.New("metric_ingestor: StaleScanRun.RepoID is the zero UUID")
	}
	if err := ValidateScanRunKind(sr.Kind); err != nil {
		return err
	}
	if err := ValidateSHABinding(sr.SHABinding); err != nil {
		return err
	}
	if sr.SHABinding == SHABindingSingle && sr.ToSHA == "" {
		return fmt.Errorf("metric_ingestor: StaleScanRun.ToSHA is empty for sha_binding='single' (violates scan_run_sha_binding_consistent)")
	}
	if sr.SHABinding == SHABindingPerRow && sr.ToSHA != "" {
		return fmt.Errorf("metric_ingestor: StaleScanRun.ToSHA=%q for sha_binding='per_row' (violates scan_run_sha_binding_consistent)", sr.ToSHA)
	}
	return nil
}

// validateStaleTransitions runs the [repo_indexer.
// ValidateTransition] guard for the two transitions the
// sweep performs:
//
//   - `commit.scan_status: scanning -> failed` (only for
//     single-bound stale rows; per_row rows skip this).
//
// The `scan_run.status: running -> failed` transition is
// validated via [ValidateScanRunStatus] inside the store;
// the canonical [ScanRunStatus] set has no transition graph
// (it's just the closed enum), so we just confirm the
// terminal value is canon.
func validateStaleTransitions(sr StaleScanRun) error {
	if sr.SHABinding == SHABindingSingle {
		if err := repo_indexer.ValidateTransition(
			repo_indexer.ScanStatusScanning,
			repo_indexer.ScanStatusFailed,
		); err != nil {
			return fmt.Errorf("metric_ingestor: stale sweep commit transition: %w", err)
		}
	}
	if err := ValidateScanRunStatus(ScanRunStatusFailed); err != nil {
		return fmt.Errorf("metric_ingestor: stale sweep scan_run status: %w", err)
	}
	return nil
}
