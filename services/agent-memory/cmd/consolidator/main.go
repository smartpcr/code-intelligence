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
//	AGENT_MEMORY_PG_URL                          postgres:// DSN (REQUIRED).
//	                                             MUST connect as a role with
//	                                             INSERT+SELECT on concept,
//	                                             concept_version, concept_support,
//	                                             episode, observation,
//	                                             INSERT+SELECT+UPDATE on
//	                                             consolidator_run, AND
//	                                             INSERT+SELECT+UPDATE on
//	                                             concept_candidate_support
//	                                             (the iter-4 staging table from
//	                                             migration 0021 used by
//	                                             emitGroupCandidatePath -- reads
//	                                             pending rows, inserts per-tick
//	                                             contributions, and updates
//	                                             promoted_to_concept_id at
//	                                             promotion time). Migration 0016
//	                                             covers the original
//	                                             `agent_memory_app` grant set;
//	                                             migration 0021 carries its own
//	                                             explicit GRANT block for the
//	                                             new staging table because
//	                                             0016's `GRANT ON ALL TABLES IN
//	                                             SCHEMA` is point-in-time
//	                                             (tech-spec §8.7.4).
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

	select {
	case <-ctx.Done():
		logger.Info("consolidator.shutdown.signal")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Warn("consolidator.shutdown.error",
				slog.String("error", err.Error()))
		}
		<-serveErr
		select {
		case <-runErr:
		case <-shutCtx.Done():
			logger.Warn("consolidator.shutdown.run_timeout")
		}
		logger.Info("consolidator.shutdown.done")
		return
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("consolidator.serve", slog.String("error", err.Error()))
			os.Exit(4)
		}
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("consolidator.run", slog.String("error", err.Error()))
			os.Exit(4)
		}
	}
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
		consolidator.MetricConsolidatorRunsTotal:             "Consolidator Tick invocations since binary start (success or failure).",
		consolidator.MetricConsolidatorErrorsTotal:           "Consolidator Tick invocations that surfaced a non-nil error since binary start.",
		consolidator.MetricConsolidatorEpisodesScannedTotal:  "Episode rows the Consolidator has cumulatively scanned across all ticks.",
		consolidator.MetricConsolidatorConceptsCreatedTotal:  "Concept rows the Consolidator has INSERTed (first-time crystallisations).",
		consolidator.MetricConsolidatorVersionsAppendedTotal: "ConceptVersion rows the Consolidator has appended across all ticks.",
		consolidator.MetricConsolidatorSupportsAppendedTotal: "concept_support rows the Consolidator has appended across all ticks.",
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
