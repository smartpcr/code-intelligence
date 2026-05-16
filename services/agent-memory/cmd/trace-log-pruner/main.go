// Command trace-log-pruner is the Stage 4.3 daily-cron pruner
// for the `trace_observation_log` partitioned table per
// implementation-plan.md §4.3. The process hosts:
//
//   - a singleton `tracelogpruner.Service` whose Run loop
//     ticks once per `AGENT_MEMORY_PRUNE_INTERVAL` (default 24h)
//     and calls `partman.drop_partition_time` to detach
//     partitions older than `AGENT_MEMORY_PRUNE_RETENTION`
//     (default 30 days, the §8.1 default);
//
//   - a tiny HTTP surface on `AGENT_MEMORY_LISTEN_ADDR`
//     (default `:8085`) exposing `/healthz` for liveness and
//     `/metrics` for the Stage 4.3 metric contract
//     (`trace_log_partitions_dropped_total`).
//
// Configuration (env vars; no flags)
// ----------------------------------
//
//	AGENT_MEMORY_PG_URL              postgres:// DSN (REQUIRED).
//	                                 MUST connect as a role that
//	                                 OWNS the partitioned parent —
//	                                 typically the migration-runner
//	                                 owner role. ALTER TABLE
//	                                 DETACH PARTITION requires
//	                                 ownership, not merely GRANT
//	                                 ALL PRIVILEGES.
//	AGENT_MEMORY_PARENT_TABLE        Schema-qualified parent
//	                                 table name. Default
//	                                 `public.trace_observation_log`.
//	AGENT_MEMORY_PRUNE_RETENTION     Retention window as a Go
//	                                 duration (e.g. `720h` for
//	                                 30 days, `168h` for the
//	                                 §8.1 7-day lower bound).
//	                                 Default 720h (30 days).
//	AGENT_MEMORY_PRUNE_INTERVAL      Cron tick. Default 24h.
//	AGENT_MEMORY_PRUNE_TIMEOUT       Per-invocation timeout for
//	                                 `partman.drop_partition_time`.
//	                                 Default 5m.
//	AGENT_MEMORY_PRUNE_KEEP_TABLE    `true` => detach (default
//	                                 per §8.1); `false` => DROP
//	                                 the standalone child after
//	                                 detach.
//	AGENT_MEMORY_LISTEN_ADDR         HTTP bind for /healthz +
//	                                 /metrics. Default `:8085`.
//	AGENT_MEMORY_SHUTDOWN_TIMEOUT    Graceful-shutdown budget.
//	                                 Default 30s.
//
// Exit codes
// ----------
//
//	0  graceful shutdown (SIGINT / SIGTERM)
//	2  configuration error (missing required env, malformed DSN,
//	   blank or unqualified parent table)
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

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/tracelogpruner"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("trace-log-pruner.config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openPG(ctx, cfg, logger)
	if err != nil {
		logger.Error("trace-log-pruner.pg", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer func() { _ = db.Close() }()

	svc, err := tracelogpruner.New(db, tracelogpruner.Config{
		ParentTable:  cfg.ParentTable,
		Retention:    cfg.Retention,
		KeepTable:    cfg.KeepTable,
		RunInterval:  cfg.Interval,
		PruneTimeout: cfg.PruneTimeout,
	}, logger)
	if err != nil {
		logger.Error("trace-log-pruner.service", slog.String("error", err.Error()))
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
		logger.Info("trace-log-pruner.listen", slog.String("addr", cfg.ListenAddr))
		serveErr <- srv.ListenAndServe()
	}()

	logger.Info("trace-log-pruner.ready",
		slog.String("parent_table", cfg.ParentTable),
		slog.Duration("retention", cfg.Retention),
		slog.Duration("interval", cfg.Interval),
		slog.Duration("prune_timeout", cfg.PruneTimeout),
		slog.Bool("keep_table", derefBool(cfg.KeepTable, true)),
		slog.String("listen_addr", cfg.ListenAddr))

	select {
	case <-ctx.Done():
		logger.Info("trace-log-pruner.shutdown.signal")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Warn("trace-log-pruner.shutdown.error",
				slog.String("error", err.Error()))
		}
		<-serveErr
		select {
		case <-runErr:
		case <-shutCtx.Done():
			logger.Warn("trace-log-pruner.shutdown.run_timeout")
		}
		logger.Info("trace-log-pruner.shutdown.done")
		return
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("trace-log-pruner.serve", slog.String("error", err.Error()))
			os.Exit(4)
		}
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("trace-log-pruner.run", slog.String("error", err.Error()))
			os.Exit(4)
		}
	}
}

// config is the env-derived configuration the binary uses.
//
// KeepTable is `*bool` (not `bool`) to mirror
// `tracelogpruner.Config.KeepTable`'s default-true semantic.
// A nil value here means "operator did not set the env var,
// use the package default (true / detach only)". An explicit
// `AGENT_MEMORY_PRUNE_KEEP_TABLE=false` will produce
// `&falseVal` and faithfully select the destructive DROP path.
type config struct {
	PGURL           string
	ParentTable     string
	Retention       time.Duration
	Interval        time.Duration
	PruneTimeout    time.Duration
	KeepTable       *bool
	ListenAddr      string
	ShutdownTimeout time.Duration
}

// derefBool returns *p when non-nil, else fallback. Used for
// log lines that want the resolved boolean value even when the
// operator did not set the env var.
func derefBool(p *bool, fallback bool) bool {
	if p == nil {
		return fallback
	}
	return *p
}

func loadConfig() (config, error) {
	c := config{
		PGURL:           os.Getenv("AGENT_MEMORY_PG_URL"),
		ParentTable:     os.Getenv("AGENT_MEMORY_PARENT_TABLE"),
		Retention:       tracelogpruner.DefaultRetention,
		Interval:        tracelogpruner.DefaultRunInterval,
		PruneTimeout:    tracelogpruner.DefaultPruneTimeout,
		KeepTable:       nil, // nil → tracelogpruner.New defaults to true.
		ListenAddr:      os.Getenv("AGENT_MEMORY_LISTEN_ADDR"),
		ShutdownTimeout: 30 * time.Second,
	}
	if c.PGURL == "" {
		return c, errors.New("AGENT_MEMORY_PG_URL is required")
	}
	if c.ParentTable == "" {
		c.ParentTable = "public.trace_observation_log"
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8085"
	}
	if v := os.Getenv("AGENT_MEMORY_PRUNE_RETENTION"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_PRUNE_RETENTION: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_PRUNE_RETENTION: must be positive, got %v", d)
		}
		c.Retention = d
	}
	if v := os.Getenv("AGENT_MEMORY_PRUNE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_PRUNE_INTERVAL: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_PRUNE_INTERVAL: must be positive, got %v", d)
		}
		c.Interval = d
	}
	if v := os.Getenv("AGENT_MEMORY_PRUNE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_PRUNE_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_PRUNE_TIMEOUT: must be positive, got %v", d)
		}
		c.PruneTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_PRUNE_KEEP_TABLE"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_PRUNE_KEEP_TABLE: %w", err)
		}
		// Allocate so a later mutation of the local `b`
		// cannot affect downstream config consumers.
		bv := b
		c.KeepTable = &bv
	}
	if v := os.Getenv("AGENT_MEMORY_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_SHUTDOWN_TIMEOUT: %w", err)
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
	// The pruner is single-threaded by design (one Run loop,
	// one Prune at a time). A small pool is plenty.
	pool.SetMaxOpenConns(2)
	pool.SetMaxIdleConns(1)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	logger.Info("trace-log-pruner.pg.connected")
	return pool, nil
}

// writeMetrics renders the pruner's counter snapshot in
// Prometheus text-format. Matches the hand-rolled exposition
// the span-ingestor binary uses so the two binaries' /metrics
// endpoints parse identically.
func writeMetrics(w http.ResponseWriter, m *tracelogpruner.Metrics) {
	snap := m.Snapshot()
	helps := map[string]string{
		tracelogpruner.MetricTraceLogPartitionsDroppedTotal: "TraceObservationLog partitions detached by the Stage 4.3 retention pruner since binary start.",
		tracelogpruner.MetricTraceLogPruneRunsTotal:         "TraceObservationLog retention-pruner Prune invocations since binary start (success or failure).",
		tracelogpruner.MetricTraceLogPruneErrorsTotal:       "TraceObservationLog retention-pruner Prune invocations that surfaced a non-nil error since binary start.",
	}
	// Stable iteration order so a scrape-vs-scrape diff is
	// deterministic.
	for _, name := range []string{
		tracelogpruner.MetricTraceLogPartitionsDroppedTotal,
		tracelogpruner.MetricTraceLogPruneRunsTotal,
		tracelogpruner.MetricTraceLogPruneErrorsTotal,
	} {
		// fmt.Fprintf into an http.ResponseWriter returns
		// (int, error); a broken pipe from a client that
		// hangs up mid-scrape is the only realistic failure
		// and is not actionable here (the next scrape will
		// either reconnect or stay broken). The blank
		// receiver pacifies errcheck without papering over a
		// real write failure to the local /metrics scraper.
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, helps[name])
		_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", name)
		_, _ = fmt.Fprintf(w, "%s %d\n", name, snap[name])
	}
}
