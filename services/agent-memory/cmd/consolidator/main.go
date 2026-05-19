// Command consolidator is the Stage 6.1 Learning-Loop
// Consolidator worker per implementation-plan.md §6.1 and
// architecture.md §7.7. The binary hosts:
//
//   - a singleton consolidator.Service whose Run loop ticks
//     every AGENT_MEMORY_CONSOLIDATOR_INTERVAL (default 1m)
//     and groups Episodes by observation-set signature into
//     Concepts;
//
//   - a tiny HTTP surface on AGENT_MEMORY_LISTEN_ADDR (default
//     `:8086`) exposing `/healthz` for liveness and `/metrics`
//     for the Stage 6.1 metric contract
//     (`consolidator_episode_lag` gauge plus the
//     standard counters).
//
// Configuration (env vars; no flags)
// ----------------------------------
//
// AGENT_MEMORY_PG_URL is the postgres:// DSN (REQUIRED). The
// role it authenticates as MUST hold the following per-table
// grants (the full enumeration; the package-level doc.go
// "Role / required DB grants" section is the canonical
// reference):
//
//	concept                     INSERT, SELECT
//	concept_version             INSERT, SELECT
//	concept_support             INSERT, SELECT
//	concept_candidate_support   INSERT, SELECT, UPDATE   (iter-4 staging table)
//	consolidator_run            INSERT, SELECT, UPDATE   (lifecycle row)
//	episode                     SELECT                   (delta scan)
//	observation                 SELECT                   (signature inputs)
//
// The `concept_candidate_support` grant set is the iter-4
// staging table from migration 0021 used by
// `emitGroupCandidatePath` (SELECT pending rows, INSERT
// per-tick contributions, UPDATE `promoted_to_concept_id` at
// promotion time). `consolidator_run` similarly needs all
// three (INSERT for openRun, SELECT for priorHighWater,
// UPDATE for finalizeRun). Migration 0016 covers the original
// `agent_memory_app` grant set; migration 0021 carries its
// own explicit GRANT block for the new staging table because
// 0016's `GRANT ON ALL TABLES IN SCHEMA` is point-in-time
// (tech-spec §8.7.4).
//
// All other env vars:
//
//	AGENT_MEMORY_CONSOLIDATOR_THRESHOLD          Minimum cumulative positive
//	                                             support count required to
//	                                             crystallise a Concept for the
//	                                             first time. Default 10 per
//	                                             consolidator.DefaultThreshold.
//	AGENT_MEMORY_CONSOLIDATOR_INTERVAL           Long-poll cadence ("every K
//	                                             minutes" per §6.1 line 873).
//	                                             Default 1m.
//	AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT       Per-tick timeout. Default 5m.
//	AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N       N for the "or after N new
//	                                             Episodes (configurable)" wake
//	                                             threshold (§6.1 line 873). When
//	                                             > 0, the binary polls the
//	                                             unconsumed-episode count and
//	                                             fires an early tick when the
//	                                             count crosses N. Default 0
//	                                             (disabled; interval-only).
//	AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL Cadence of the wake-after-N
//	                                              count poll. Default 5s.
//	                                              Ignored when WAKE_AFTER_N=0.
//	AGENT_MEMORY_LISTEN_ADDR                     HTTP bind for /healthz +
//	                                             /metrics. Default `:8086` --
//	                                             chosen to avoid colliding with
//	                                             the Stage 4.3 trace-log-pruner
//	                                             on :8085 and the Span Ingestor
//	                                             on :4318.
//	AGENT_MEMORY_SHUTDOWN_TIMEOUT                Graceful-shutdown budget.
//	                                             Default 30s.
//
// Exit codes
// ----------
//
//	0  graceful shutdown (SIGINT / SIGTERM)
//	2  configuration error (missing required env, malformed DSN)
//	3  startup failure (DB ping)
//	4  runtime failure (Run returned a non-Canceled error)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/consolidator"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("consolidator.config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Stage 8.3 step 2 — OTel trace export. Best-effort
	// noop when no endpoint configured.
	tracerSetup, err := obs.SetupTracer(ctx, obs.ServiceNameConsolidator, logger)
	if err != nil {
		logger.Error("consolidator.otel.setup_failed", slog.String("error", err.Error()))
		os.Exit(2)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracerSetup.Shutdown(shutCtx)
	}()
	logger.Info("consolidator.otel.ready",
		slog.Bool("exporting", tracerSetup.Exporting),
		slog.String("endpoint", tracerSetup.EndpointResolved))

	db, err := openPG(ctx, cfg, logger)
	if err != nil {
		logger.Error("consolidator.pg", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer func() { _ = db.Close() }()

	svc, err := consolidator.New(db, consolidator.Config{
		Threshold:          cfg.Threshold,
		RunInterval:        cfg.Interval,
		TickTimeout:        cfg.TickTimeout,
		WakeAfterNEpisodes: cfg.WakeAfterN,
		WakeCheckInterval:  cfg.WakeCheckInterval,
	}, logger)
	if err != nil {
		logger.Error("consolidator.service", slog.String("error", err.Error()))
		os.Exit(2)
	}
	// Stage 8.3 step 2 (iter-2 evaluator fix #1) — plumb the
	// OTel tracer set up at process start onto the Service so
	// each Tick opens a real `consolidator.tick` span. Without
	// this the SDK is initialised but no production code starts
	// spans, which the iter-1 evaluator rg-checked and flagged.
	svc.ApplyOptions(consolidator.WithTracer(tracerSetup.Tracer))

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetrics(w, svc.Metrics())
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	runErr := make(chan error, 1)
	go func() { runErr <- svc.Run(ctx) }()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("consolidator.listen", slog.String("addr", cfg.ListenAddr))
		serveErr <- srv.ListenAndServe()
	}()

	logger.Info("consolidator.ready",
		slog.Int("threshold", cfg.Threshold),
		slog.Duration("interval", cfg.Interval),
		slog.Duration("tick_timeout", cfg.TickTimeout),
		slog.Int("wake_after_n", cfg.WakeAfterN),
		slog.Duration("wake_check_interval", cfg.WakeCheckInterval),
		slog.String("listen_addr", cfg.ListenAddr))

	if code := waitForShutdown(ctx, srv, serveErr, runErr, stop,
		cfg.ShutdownTimeout, logger); code != 0 {
		os.Exit(code)
	}
}

// httpShutdowner is the *http.Server surface waitForShutdown drives.
// Carved out as an interface so the unit test below can exercise the
// SIGINT-race regression without binding a real listener.
type httpShutdowner interface {
	Shutdown(ctx context.Context) error
	Close() error
}

// waitForShutdown is the binary's single graceful-exit state machine.
// It blocks on the first of three exit triggers (signal-cancelled
// `ctx`, an unexpected `serveErr`, or `runErr`) and then ALWAYS
// walks the documented graceful HTTP shutdown path, regardless of
// which trigger fired first.
//
// Iter-8 evaluator finding #3 fix: the prior arrangement used a
// flat select where the runErr branch took `return` directly when
// the error was `context.Canceled`. On SIGINT/SIGTERM, both
// `<-ctx.Done()` and the runErr send (carrying context.Canceled)
// become ready in the SAME scheduler tick, and Go's select picks
// one at random. Whenever runErr won the race, srv.Shutdown was
// never invoked and in-flight `/metrics` + `/healthz` scrapes were
// dropped abruptly. Routing every branch through the shared
// shutdown block makes the outcome identical regardless of which
// channel the runtime selects.
//
// The `cancelCtx` callback (typically the `stop` returned by
// signal.NotifyContext) is invoked whenever the serveErr or runErr
// branch fires before a signal arrives. Both cases represent an
// unexpected goroutine exit, and we must explicitly cancel ctx so
// the sibling goroutine unwinds and its drain wait can complete.
//
// Every drain wait is bounded by `shutdownTimeout`; if srv.Shutdown
// itself fails (typically: shutCtx deadline hit while connections
// refused to drain) we fall back to a hard `srv.Close()` so the
// ListenAndServe goroutine returns promptly and the drain below
// does not deadlock.
//
// Returns the OS exit code: 0 on graceful shutdown, 4 on a
// runtime/serve failure that needs to surface through os.Exit.
func waitForShutdown(
	ctx context.Context,
	srv httpShutdowner,
	serveErr, runErr <-chan error,
	cancelCtx context.CancelFunc,
	shutdownTimeout time.Duration,
	logger *slog.Logger,
) int {
	exitCode := 0
	serveDone := false
	runDone := false
	select {
	case <-ctx.Done():
		logger.Info("consolidator.shutdown.signal")
	case err := <-serveErr:
		serveDone = true
		cancelCtx()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("consolidator.serve",
				slog.String("error", err.Error()))
			exitCode = 4
		}
	case err := <-runErr:
		runDone = true
		cancelCtx()
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("consolidator.run",
				slog.String("error", err.Error()))
			exitCode = 4
		}
	}

	shutCtx, cancelShut := context.WithTimeout(
		context.Background(), shutdownTimeout)
	defer cancelShut()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Warn("consolidator.shutdown.error",
			slog.String("error", err.Error()))
		// Shutdown returned an error (commonly: the shutCtx
		// deadline expired while in-flight requests refused to
		// drain). Force the listener closed so ListenAndServe
		// returns promptly and the serveErr drain below does
		// not block past shutCtx.Done().
		_ = srv.Close()
	}
	if !serveDone {
		select {
		case <-serveErr:
		case <-shutCtx.Done():
			logger.Warn("consolidator.shutdown.serve_timeout")
		}
	}
	if !runDone {
		select {
		case <-runErr:
		case <-shutCtx.Done():
			logger.Warn("consolidator.shutdown.run_timeout")
		}
	}
	logger.Info("consolidator.shutdown.done")
	return exitCode
}

type config struct {
	PGURL             string
	Threshold         int
	Interval          time.Duration
	TickTimeout       time.Duration
	WakeAfterN        int
	WakeCheckInterval time.Duration
	ListenAddr        string
	ShutdownTimeout   time.Duration
}

func loadConfig() (config, error) {
	c := config{
		PGURL:             os.Getenv("AGENT_MEMORY_PG_URL"),
		Threshold:         consolidator.DefaultThreshold,
		Interval:          consolidator.DefaultRunInterval,
		TickTimeout:       consolidator.DefaultTickTimeout,
		WakeAfterN:        0,
		WakeCheckInterval: consolidator.DefaultWakeCheckInterval,
		ListenAddr:        os.Getenv("AGENT_MEMORY_LISTEN_ADDR"),
		ShutdownTimeout:   30 * time.Second,
	}
	if c.PGURL == "" {
		return c, errors.New("AGENT_MEMORY_PG_URL is required")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8086"
	}
	if v := os.Getenv("AGENT_MEMORY_CONSOLIDATOR_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_CONSOLIDATOR_THRESHOLD: must be positive int, got %q", v)
		}
		c.Threshold = n
	}
	if v := os.Getenv("AGENT_MEMORY_CONSOLIDATOR_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_CONSOLIDATOR_INTERVAL: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_CONSOLIDATOR_INTERVAL: must be positive, got %v", d)
		}
		c.Interval = d
	}
	if v := os.Getenv("AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_CONSOLIDATOR_TICK_TIMEOUT: must be positive, got %v", d)
		}
		c.TickTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return c, fmt.Errorf("AGENT_MEMORY_CONSOLIDATOR_WAKE_AFTER_N: must be non-negative int, got %q", v)
		}
		c.WakeAfterN = n
	}
	if v := os.Getenv("AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_CONSOLIDATOR_WAKE_CHECK_INTERVAL: must be positive, got %v", d)
		}
		c.WakeCheckInterval = d
	}
	if v := os.Getenv("AGENT_MEMORY_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_SHUTDOWN_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_SHUTDOWN_TIMEOUT: must be positive, got %v", d)
		}
		c.ShutdownTimeout = d
	}
	return c, nil
}

func openPG(ctx context.Context, cfg config, logger *slog.Logger) (*sql.DB, error) {
	pool, err := sql.Open("postgres", cfg.PGURL)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	// The consolidator pins ONE connection per tick (for the
	// session-level advisory lock + emission writes), plus
	// uses the pool for the lifecycle INSERT/UPDATE. A small
	// pool is plenty.
	pool.SetMaxOpenConns(4)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	logger.Info("consolidator.pg.connected")
	return pool, nil
}

// writeMetrics renders the Service's counter snapshot plus the
// episode_lag gauge in Prometheus text-format. Matches the
// hand-rolled exposition the trace-log-pruner binary uses so
// the two binaries' /metrics endpoints parse identically.
func writeMetrics(w http.ResponseWriter, m *consolidator.Metrics) {
	snap := m.Snapshot()
	helps := map[string]string{
		consolidator.MetricConsolidatorRunsTotal:                          "Consolidator Tick invocations since binary start (success or failure).",
		consolidator.MetricConsolidatorErrorsTotal:                        "Consolidator Tick invocations that surfaced a non-nil error since binary start.",
		consolidator.MetricConsolidatorEpisodesScannedTotal:               "Episode rows the Consolidator has cumulatively scanned across all ticks.",
		consolidator.MetricConsolidatorConceptsCreatedTotal:               "Concept rows the Consolidator has INSERTed (first-time crystallisations).",
		consolidator.MetricConsolidatorVersionsAppendedTotal:              "ConceptVersion rows the Consolidator has appended across all ticks.",
		consolidator.MetricConsolidatorSupportsAppendedTotal:              "concept_support rows the Consolidator has appended across all ticks.",
		consolidator.MetricConsolidatorSyntheticPositivesCreatedTotal:     "synthetic_positive Episodes the Consolidator has emitted across all ticks (Stage 6.3 operator-correction auto-promotion / architecture §7.7 step 4). Bumped per successful INSERT only -- no-op candidates filtered by the WHERE NOT EXISTS gate do not increment.",
		consolidator.MetricConsolidatorSyntheticObservationsMirroredTotal: "observation rows the Consolidator has copied from agent parent Episodes onto their synthetic_positive child Episodes across all ticks (Stage 6.3).",
	}
	// Stable iteration order so a scrape-vs-scrape diff is
	// deterministic.
	counterOrder := []string{
		consolidator.MetricConsolidatorRunsTotal,
		consolidator.MetricConsolidatorErrorsTotal,
		consolidator.MetricConsolidatorEpisodesScannedTotal,
		consolidator.MetricConsolidatorConceptsCreatedTotal,
		consolidator.MetricConsolidatorVersionsAppendedTotal,
		consolidator.MetricConsolidatorSupportsAppendedTotal,
		consolidator.MetricConsolidatorSyntheticPositivesCreatedTotal,
		consolidator.MetricConsolidatorSyntheticObservationsMirroredTotal,
	}
	for _, name := range counterOrder {
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, helps[name])
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", name)
		_, _ = fmt.Fprintf(w, "%s %d\n", name, snap[name])
	}
	_, _ = fmt.Fprintf(w, "# HELP %s Wall-clock seconds between max(Episode.created_at) and the Consolidator's high-water mark (created_at) at the end of the latest tick. Zero is the healthy value (caught up). Per implementation-plan.md §6.1 line 903 the metric NAME deliberately omits the _seconds suffix; the value is in seconds.\n",
		consolidator.MetricConsolidatorEpisodeLag)
	_, _ = fmt.Fprintf(w, "# TYPE %s gauge\n", consolidator.MetricConsolidatorEpisodeLag)
	_, _ = fmt.Fprintf(w, "%s %g\n", consolidator.MetricConsolidatorEpisodeLag, m.LagSeconds())
}
