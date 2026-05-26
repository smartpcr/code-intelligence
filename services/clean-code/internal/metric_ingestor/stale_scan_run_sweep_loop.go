package metric_ingestor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/config"
)

// StaleScanRunSweepLoopOption configures a
// [StaleScanRunSweepLoop]. Functional-options pattern,
// matching the rest of the package.
type StaleScanRunSweepLoopOption func(*StaleScanRunSweepLoop)

// WithStaleSweepLoopCadence sets the period between
// stale-sweep passes. Default:
// [config.DefaultPeriodicSweepCadence] (5 min, tech-spec
// Sec 8.2 `periodic_sweep_cadence`). PANICS at
// [NewStaleScanRunSweepLoop] when <= 0 (a zero cadence is
// a busy loop and always a wiring bug).
func WithStaleSweepLoopCadence(d time.Duration) StaleScanRunSweepLoopOption {
	return func(l *StaleScanRunSweepLoop) { l.cadence = d }
}

// WithStaleSweepLoopErrorBackoff sets the wait between the
// loop's last Sweep error and the next attempt. A separate
// knob from cadence so an operator can dial up the backoff
// when the DB is degraded without changing the
// healthy-state cadence. Default: equal to cadence.
func WithStaleSweepLoopErrorBackoff(d time.Duration) StaleScanRunSweepLoopOption {
	return func(l *StaleScanRunSweepLoop) { l.errorBackoff = d }
}

// WithStaleSweepLoopSleep overrides the timer factory.
// Tests inject a channel-driven clock so the loop is
// deterministic; production uses [time.After] (the
// default).
//
// The factory MUST return a channel that fires after the
// requested duration; the loop drains the channel before
// the next call.
func WithStaleSweepLoopSleep(sleep func(d time.Duration) <-chan time.Time) StaleScanRunSweepLoopOption {
	return func(l *StaleScanRunSweepLoop) { l.sleep = sleep }
}

// WithStaleSweepLoopLogger overrides the logger. nil
// silences the loop.
func WithStaleSweepLoopLogger(log *slog.Logger) StaleScanRunSweepLoopOption {
	return func(l *StaleScanRunSweepLoop) { l.log = log }
}

// WithStaleSweepLoopRunOnStart toggles the
// run-immediately-on-start behaviour. When true (default),
// [StaleScanRunSweepLoop.Run] performs ONE Sweep before the
// first cadence wait so a crash-restarted service does
// not wait `cadence` (5 min) before cleaning up
// scan_runs that were already stale at restart.
//
// Tests that need to control the first-tick timing set
// this to false.
func WithStaleSweepLoopRunOnStart(b bool) StaleScanRunSweepLoopOption {
	return func(l *StaleScanRunSweepLoop) { l.runOnStart = b }
}

// StaleScanRunSweepLoop is the Stage 3.5 production driver
// for the stale-ScanRun sweep. `Run(ctx)` blocks until ctx
// is cancelled, calling [StaleScanRunSweep.Sweep] once per
// `cadence` (default 5 min, tech-spec Sec 8.2
// `periodic_sweep_cadence`).
//
// # Composition root wiring
//
// The future Phase 3.5 service composition root constructs
// one [StaleScanRunSweepLoop] per process, alongside the
// existing [Sweeper] (which drives the StateMachine's
// per-commit foundation scans). Both loops share the same
// service slog instance and ctx so SIGTERM cancels both in
// parallel.
//
// # Resilience contract
//
// A Sweep error does NOT terminate the loop -- the loop
// logs the error and continues with the next tick. The
// only way Run returns is ctx cancellation; the returned
// error is ctx.Err() (typically context.Canceled or
// context.DeadlineExceeded). Tests that simulate a degraded
// store assert the loop keeps ticking through the failures.
type StaleScanRunSweepLoop struct {
	sweep        *StaleScanRunSweep
	cadence      time.Duration
	errorBackoff time.Duration
	sleep        func(d time.Duration) <-chan time.Time
	log          *slog.Logger
	runOnStart   bool
}

// ErrStaleSweepLoopNilSweep surfaces a nil sweep at
// composition-root wiring time. Returned wrapped in a
// panic from [NewStaleScanRunSweepLoop] since the
// dependency is non-optional.
var ErrStaleSweepLoopNilSweep = errors.New("metric_ingestor: NewStaleScanRunSweepLoop: sweep is nil")

// ErrStaleSweepLoopNonPositiveCadence is returned when the
// configured cadence is <= 0.
var ErrStaleSweepLoopNonPositiveCadence = errors.New("metric_ingestor: NewStaleScanRunSweepLoop: cadence must be > 0")

// NewStaleScanRunSweepLoop returns a loop driver wrapping
// `sweep`. PANICS on nil sweep or non-positive cadence --
// composition-root wiring bugs surface immediately.
//
// Defaults:
//   - cadence       = 5 min (tech-spec Sec 8.2)
//   - errorBackoff  = cadence
//   - sleep         = time.After
//   - log           = nil (silent)
//   - runOnStart    = true
func NewStaleScanRunSweepLoop(sweep *StaleScanRunSweep, opts ...StaleScanRunSweepLoopOption) *StaleScanRunSweepLoop {
	if sweep == nil {
		panic(ErrStaleSweepLoopNilSweep)
	}
	// Default cadence is sourced from
	// [config.DefaultPeriodicSweepCadence] so the canonical
	// tech-spec Sec 8.2 value is the single source of truth
	// (iter 2 evaluator item 2).
	l := &StaleScanRunSweepLoop{
		sweep:      sweep,
		cadence:    config.DefaultPeriodicSweepCadence,
		sleep:      time.After,
		runOnStart: true,
	}
	for _, opt := range opts {
		opt(l)
	}
	if l.cadence <= 0 {
		panic(fmt.Errorf("%w: got %s", ErrStaleSweepLoopNonPositiveCadence, l.cadence))
	}
	if l.errorBackoff <= 0 {
		l.errorBackoff = l.cadence
	}
	if l.sleep == nil {
		l.sleep = time.After
	}
	return l
}

// Cadence returns the configured period. Exposed for tests
// and structured-log lines.
func (l *StaleScanRunSweepLoop) Cadence() time.Duration {
	return l.cadence
}

// Sweep returns the underlying [StaleScanRunSweep] so the
// composition root can share the metrics holder with a
// Prometheus exporter and so tests can drive an explicit
// Sweep without going through the loop.
func (l *StaleScanRunSweepLoop) Sweep() *StaleScanRunSweep {
	return l.sweep
}

// Run blocks until ctx is cancelled, calling
// [StaleScanRunSweep.Sweep] every `cadence`. Returns
// ctx.Err() on cancellation.
//
// The loop:
//
//  1. (Optional) Runs ONE Sweep on entry when
//     [WithStaleSweepLoopRunOnStart] is true (the default).
//     This means a crash-restarted process cleans up
//     already-stale rows immediately rather than waiting
//     `cadence` (5 min) for the first tick.
//  2. Waits `cadence`.
//  3. Re-checks ctx; on cancel, returns ctx.Err().
//  4. Calls [StaleScanRunSweep.Sweep]; logs the report.
//     A Sweep error is logged and the loop backs off
//     `errorBackoff` instead of the normal cadence.
//  5. Repeats from step 2.
//
// Cancellation is honoured at every checkpoint -- the
// loop never blocks more than one cadence/backoff period
// past a cancel.
func (l *StaleScanRunSweepLoop) Run(ctx context.Context) error {
	if l.log != nil {
		l.log.Info("metric_ingestor stale sweep loop: started",
			"cadence", l.cadence,
			"error_backoff", l.errorBackoff,
			"scan_timeout", l.sweep.ScanTimeout(),
			"run_on_start", l.runOnStart,
		)
		defer l.log.Info("metric_ingestor stale sweep loop: stopped")
	}

	// When runOnStart is false, sleep one cadence BEFORE the
	// first Sweep. When true (default), the first Sweep
	// runs immediately on entry -- a crash-restarted
	// service cleans up already-stale rows without waiting
	// `cadence` for the first tick.
	if !l.runOnStart {
		if err := l.wait(ctx, l.cadence); err != nil {
			return err
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nextWait := l.runOnce(ctx)
		if err := l.wait(ctx, nextWait); err != nil {
			return err
		}
	}
}

// runOnce executes a single Sweep call and returns the
// recommended wait before the next call (cadence on
// success, errorBackoff on failure). Cancellation is
// surfaced by the underlying Sweep call returning
// ctx.Err(); the loop's outer ctx check catches it on the
// next iteration.
func (l *StaleScanRunSweepLoop) runOnce(ctx context.Context) time.Duration {
	report, err := l.sweep.Sweep(ctx)
	if err != nil {
		// ctx cancellation is not a "loop error" -- the
		// outer ctx.Err() check ends the loop cleanly on
		// the next iteration. We still return the cadence
		// here so a partially-completed sweep does not get
		// an extra backoff.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return l.cadence
		}
		if l.log != nil {
			l.log.Warn("metric_ingestor stale sweep loop: Sweep failed",
				"err", err,
				"scanned", report.Scanned,
				"scan_runs_transitioned", report.ScanRunsTransitioned,
				"commits_transitioned", report.CommitsTransitioned,
				"orphaned_commits_cleaned", report.OrphanedCommitsCleaned,
			)
		}
		return l.errorBackoff
	}
	if l.log != nil && (report.Scanned > 0 || report.OrphanedCommitsCleaned > 0) {
		l.log.Info("metric_ingestor stale sweep loop: pass succeeded",
			"scanned", report.Scanned,
			"scan_runs_transitioned", report.ScanRunsTransitioned,
			"commits_transitioned", report.CommitsTransitioned,
			"orphaned_commits_cleaned", report.OrphanedCommitsCleaned,
		)
	}
	return l.cadence
}

// wait sleeps for `d` or returns ctx.Err() on cancellation.
// Goes through the injected [WithStaleSweepLoopSleep]
// factory so tests can drive the loop deterministically.
func (l *StaleScanRunSweepLoop) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := l.sleep(d)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer:
		return nil
	}
}
