package aggregator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"forge/services/clean-code/internal/config"
)

// LoopOption configures a [Loop].
type LoopOption func(*Loop)

// WithLoopCadence sets the period between aggregator ticks.
// Default: [config.DefaultAggregatorCadence] (15 min, tech-spec
// Sec 8.2 `aggregator_cadence`). PANICS at [NewLoop] when <= 0.
func WithLoopCadence(d time.Duration) LoopOption {
	return func(l *Loop) { l.cadence = d }
}

// WithLoopErrorBackoff sets the wait between a failed
// [Aggregator.Tick] and the next attempt. Default: equal to
// cadence. A separate knob so an operator can dial the backoff
// up when the DB is degraded without changing the healthy-state
// cadence.
func WithLoopErrorBackoff(d time.Duration) LoopOption {
	return func(l *Loop) { l.errorBackoff = d }
}

// WithLoopLogger overrides the slog logger. Nil silences the
// loop; the production composition root wires the service slog
// instance.
func WithLoopLogger(log *slog.Logger) LoopOption {
	return func(l *Loop) { l.log = log }
}

// WithLoopSleep overrides the timer factory used for the
// cadence / error-backoff sleep. Tests inject a channel-driven
// clock so the loop is deterministic; the production path leaves
// it nil and the loop uses [time.NewTimer] directly (so the
// timer can be Stop()ped on ctx cancel, matching the
// metric_ingestor stale-sweep loop pattern).
func WithLoopSleep(sleep func(d time.Duration) <-chan time.Time) LoopOption {
	return func(l *Loop) { l.sleep = sleep }
}

// WithLoopRunOnStart toggles the run-immediately-on-start
// behaviour. When true (default), [Loop.Run] performs one Tick
// before the first cadence wait so a crash-restarted service
// does not wait `cadence` (15 min) before materialising fresh
// snapshots.
func WithLoopRunOnStart(b bool) LoopOption {
	return func(l *Loop) { l.runOnStart = b }
}

// Loop is the production driver for the Cross-Repo Aggregator.
// `Run(ctx)` blocks until ctx is cancelled, calling
// [Aggregator.Tick] once per `cadence` (default 15 min,
// tech-spec Sec 8.2 `aggregator_cadence`).
//
// # Composition root wiring
//
// The future Phase 7 service composition root constructs one
// [Loop] per process, alongside the metric_ingestor's stale-sweep
// loop and the sweep loop that drives the state machine. All
// three loops share the same service slog instance and ctx so
// SIGTERM cancels them in parallel.
//
// # Resilience contract
//
// A Tick error does NOT terminate the loop -- the loop logs the
// error and continues after `errorBackoff`. The only way Run
// returns is ctx cancellation; the returned error is ctx.Err()
// (typically context.Canceled or context.DeadlineExceeded). Tests
// that simulate a degraded snapshot writer assert the loop keeps
// ticking through the failures.
//
// # Timer hygiene
//
// The wait between ticks uses [time.NewTimer] + [time.Timer.Stop]
// in the cancel path so an early cancel does NOT leak a pending
// timer. The injected [WithLoopSleep] test path is channel-only
// and intentionally does NOT call Stop -- test mock clocks are
// GC'd with the test goroutine. Mirrors
// `metric_ingestor/stale_scan_run_sweep_loop.go` lines 281-343.
type Loop struct {
	aggregator   *Aggregator
	cadence      time.Duration
	errorBackoff time.Duration
	sleep        func(d time.Duration) <-chan time.Time
	log          *slog.Logger
	runOnStart   bool
}

// ErrLoopNilAggregator surfaces a nil aggregator at composition-
// root wiring time.
var ErrLoopNilAggregator = errors.New("aggregator: NewLoop: aggregator is nil")

// ErrLoopNonPositiveCadence is returned when the configured
// cadence is <= 0.
var ErrLoopNonPositiveCadence = errors.New("aggregator: NewLoop: cadence must be > 0")

// NewLoop returns a loop driver wrapping `agg`. PANICS on nil
// aggregator or non-positive cadence -- composition-root wiring
// bugs surface immediately.
//
// Defaults:
//   - cadence       = 15 min ([config.DefaultAggregatorCadence])
//   - errorBackoff  = cadence
//   - sleep         = nil (production path uses [time.NewTimer]
//     directly)
//   - log           = nil (silent)
//   - runOnStart    = true
func NewLoop(agg *Aggregator, opts ...LoopOption) *Loop {
	if agg == nil {
		panic(ErrLoopNilAggregator)
	}
	l := &Loop{
		aggregator: agg,
		cadence:    config.DefaultAggregatorCadence,
		runOnStart: true,
	}
	for _, opt := range opts {
		opt(l)
	}
	if l.cadence <= 0 {
		panic(fmt.Errorf("%w: got %s", ErrLoopNonPositiveCadence, l.cadence))
	}
	if l.errorBackoff <= 0 {
		l.errorBackoff = l.cadence
	}
	return l
}

// Cadence returns the configured period. Exposed for tests +
// structured-log lines.
func (l *Loop) Cadence() time.Duration { return l.cadence }

// Aggregator returns the wrapped aggregator so the composition
// root can share dependencies (e.g. with a Prometheus exporter
// that observes Tick counters) and so tests can drive an
// explicit Tick without going through the loop.
func (l *Loop) Aggregator() *Aggregator { return l.aggregator }

// Run blocks until ctx is cancelled, calling
// [Aggregator.Tick] every `cadence`. Returns ctx.Err() on
// cancellation.
//
// The loop:
//
//  1. (Optional) Runs one Tick on entry when [WithLoopRunOnStart]
//     is true (the default). A crash-restarted process
//     materialises fresh snapshots immediately rather than
//     waiting `cadence` for the first tick.
//  2. Waits `cadence` (or `errorBackoff` if the previous Tick
//     failed).
//  3. Re-checks ctx; on cancel, returns ctx.Err().
//  4. Calls [Aggregator.Tick]; logs the report.
//  5. Repeats from step 2.
//
// Cancellation is honoured at every checkpoint -- the loop never
// blocks more than one cadence / backoff period past a cancel.
func (l *Loop) Run(ctx context.Context) error {
	if l.log != nil {
		l.log.Info("aggregator loop: started",
			"cadence", l.cadence,
			"error_backoff", l.errorBackoff,
			"run_on_start", l.runOnStart,
		)
		defer l.log.Info("aggregator loop: stopped")
	}

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

// runOnce executes a single Tick and returns the recommended
// wait before the next call (cadence on success, errorBackoff on
// failure). ctx cancellation is not treated as a loop error --
// the outer ctx.Err() check ends the loop cleanly on the next
// iteration.
func (l *Loop) runOnce(ctx context.Context) time.Duration {
	report, err := l.aggregator.Tick(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return l.cadence
		}
		if l.log != nil {
			l.log.Warn("aggregator loop: Tick failed",
				"err", err,
				"observations_read", report.ObservationsRead,
				"repo_metric_snapshot_rows", report.RepoMetricSnapshotRowsWritten,
				"cross_repo_percentile_rows", report.CrossRepoPercentileRowsWritten,
				"portfolio_snapshot_rows", report.PortfolioSnapshotRowsWritten,
			)
		}
		return l.errorBackoff
	}
	if l.log != nil {
		l.log.Info("aggregator loop: Tick succeeded",
			"built_at", report.BuiltAt,
			"observations_read", report.ObservationsRead,
			"cohorts_aggregated", report.CohortsAggregated,
			"repo_metric_snapshot_rows", report.RepoMetricSnapshotRowsWritten,
			"cross_repo_percentile_rows", report.CrossRepoPercentileRowsWritten,
			"portfolio_snapshot_rows", report.PortfolioSnapshotRowsWritten,
		)
	}
	return l.cadence
}

// wait sleeps for `d` or returns ctx.Err() on cancellation.
// Production path uses [time.NewTimer] directly so the timer is
// Stop()ped on the cancel branch. The test path routes through
// the injected channel-based factory for determinism. Mirrors
// `metric_ingestor/stale_scan_run_sweep_loop.go:wait`.
func (l *Loop) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if l.sleep != nil {
		ch := l.sleep(d)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ch:
			return nil
		}
	}
	timer := time.NewTimer(d)
	select {
	case <-ctx.Done():
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
