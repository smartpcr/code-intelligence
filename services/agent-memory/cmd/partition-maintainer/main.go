// Command partition-maintainer is the Stage 8.2 partition-
// rotation automation binary per implementation-plan.md §8.2.
// The process hosts:
//
//   - a singleton `partitionmaintainer.Service` whose Run loop
//     ticks every `AGENT_MEMORY_PARTITION_MAINTENANCE_INTERVAL`
//     (default 10m) -- calling `partman.run_maintenance` over
//     either every part_config-registered parent or the
//     configured schema / explicit subset -- and every
//     `AGENT_MEMORY_PARTITION_LAG_SCRAPE_INTERVAL` (default 1m)
//     -- refreshing the `partition_provision_lag`
//     gauge (whole seconds) that the §8.2 alert rule keys off.
//
//   - a tiny HTTP surface on `AGENT_MEMORY_LISTEN_ADDR`
//     (default `:8087`) exposing `/healthz` for liveness and
//     `/metrics` for the Stage 8.2 metric contract.
//
// Configuration (env vars; no flags)
// ----------------------------------
//
//	AGENT_MEMORY_PG_URL                          postgres:// DSN (REQUIRED).
//	                                             MUST connect as a role that
//	                                             OWNS every partitioned parent
//	                                             (the migration-runner owner
//	                                             role). partman.run_maintenance
//	                                             issues CREATE TABLE / ALTER
//	                                             TABLE DETACH PARTITION which
//	                                             both require ownership.
//	AGENT_MEMORY_PARTITION_TABLES                Comma-separated list of
//	                                             schema-qualified parent
//	                                             tables to scope to (e.g.
//	                                             `public.episode,public.observation`).
//	                                             When empty the binary
//	                                             defaults to cluster-wide
//	                                             via partman.part_config.
//	AGENT_MEMORY_PARTITION_SCHEMA_FILTER         Restrict the part_config
//	                                             lookup to one schema.
//	                                             Ignored when
//	                                             AGENT_MEMORY_PARTITION_TABLES
//	                                             is set.
//	AGENT_MEMORY_PARTITION_MAINTENANCE_INTERVAL  RunMaintenance tick.
//	                                             Default 10m.
//	AGENT_MEMORY_PARTITION_MAINTENANCE_TIMEOUT   Per-invocation timeout for
//	                                             partman.run_maintenance.
//	                                             Default 2m.
//	AGENT_MEMORY_PARTITION_LAG_SCRAPE_INTERVAL   ScrapeLag tick. Default 1m.
//	AGENT_MEMORY_PARTITION_LAG_SCRAPE_TIMEOUT    Per-invocation timeout for
//	                                             ScrapeLag. Default 30s.
//	AGENT_MEMORY_LISTEN_ADDR                     HTTP bind for /healthz +
//	                                             /metrics. Default `:8087`.
//	AGENT_MEMORY_SHUTDOWN_TIMEOUT                Graceful-shutdown budget.
//	                                             Default 30s.
//
// Exit codes
// ----------
//
//	0  graceful shutdown (SIGINT / SIGTERM)
//	2  configuration error (missing required env, malformed DSN,
//	   blank or unqualified parent table entry)
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
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/partitionmaintainer"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("partition-maintainer.config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openPG(ctx, cfg, logger)
	if err != nil {
		logger.Error("partition-maintainer.pg", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer func() { _ = db.Close() }()

	svc, err := partitionmaintainer.New(db, partitionmaintainer.Config{
		ParentTables:        cfg.ParentTables,
		SchemaFilter:        cfg.SchemaFilter,
		MaintenanceInterval: cfg.MaintenanceInterval,
		MaintenanceTimeout:  cfg.MaintenanceTimeout,
		LagScrapeInterval:   cfg.LagScrapeInterval,
		LagScrapeTimeout:    cfg.LagScrapeTimeout,
	}, logger)
	if err != nil {
		logger.Error("partition-maintainer.service", slog.String("error", err.Error()))
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
		logger.Info("partition-maintainer.listen", slog.String("addr", cfg.ListenAddr))
		serveErr <- srv.ListenAndServe()
	}()

	logger.Info("partition-maintainer.ready",
		slog.Int("explicit_parents", len(cfg.ParentTables)),
		slog.String("schema_filter", cfg.SchemaFilter),
		slog.Duration("maintenance_interval", cfg.MaintenanceInterval),
		slog.Duration("maintenance_timeout", cfg.MaintenanceTimeout),
		slog.Duration("lag_scrape_interval", cfg.LagScrapeInterval),
		slog.Duration("lag_scrape_timeout", cfg.LagScrapeTimeout),
		slog.String("listen_addr", cfg.ListenAddr))

	select {
	case <-ctx.Done():
		logger.Info("partition-maintainer.shutdown.signal")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Warn("partition-maintainer.shutdown.error",
				slog.String("error", err.Error()))
		}
		<-serveErr
		select {
		case <-runErr:
		case <-shutCtx.Done():
			logger.Warn("partition-maintainer.shutdown.run_timeout")
		}
		logger.Info("partition-maintainer.shutdown.done")
		return
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("partition-maintainer.serve", slog.String("error", err.Error()))
			os.Exit(4)
		}
	case err := <-runErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("partition-maintainer.run", slog.String("error", err.Error()))
			os.Exit(4)
		}
	}
}

// config is the env-derived configuration the binary uses. The
// ParentTables / SchemaFilter pair mirrors the package's
// `Config` semantics: ParentTables wins when non-empty, else
// SchemaFilter narrows the part_config lookup.
type config struct {
	PGURL               string
	ParentTables        []string
	SchemaFilter        string
	MaintenanceInterval time.Duration
	MaintenanceTimeout  time.Duration
	LagScrapeInterval   time.Duration
	LagScrapeTimeout    time.Duration
	ListenAddr          string
	ShutdownTimeout     time.Duration
}

func loadConfig() (config, error) {
	c := config{
		PGURL:               os.Getenv("AGENT_MEMORY_PG_URL"),
		SchemaFilter:        os.Getenv("AGENT_MEMORY_PARTITION_SCHEMA_FILTER"),
		MaintenanceInterval: partitionmaintainer.DefaultMaintenanceInterval,
		MaintenanceTimeout:  partitionmaintainer.DefaultMaintenanceTimeout,
		LagScrapeInterval:   partitionmaintainer.DefaultLagScrapeInterval,
		LagScrapeTimeout:    partitionmaintainer.DefaultLagScrapeTimeout,
		ListenAddr:          os.Getenv("AGENT_MEMORY_LISTEN_ADDR"),
		ShutdownTimeout:     30 * time.Second,
	}
	if c.PGURL == "" {
		return c, errors.New("AGENT_MEMORY_PG_URL is required")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8087"
	}
	if raw := os.Getenv("AGENT_MEMORY_PARTITION_TABLES"); strings.TrimSpace(raw) != "" {
		// Comma-separated list. Empty entries (e.g. from a
		// stray trailing comma) are stripped silently; blank /
		// unqualified entries are caught by
		// partitionmaintainer.New, not here.
		for _, p := range strings.Split(raw, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			c.ParentTables = append(c.ParentTables, p)
		}
	}
	if v := os.Getenv("AGENT_MEMORY_PARTITION_MAINTENANCE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_PARTITION_MAINTENANCE_INTERVAL: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_PARTITION_MAINTENANCE_INTERVAL: must be positive, got %v", d)
		}
		c.MaintenanceInterval = d
	}
	if v := os.Getenv("AGENT_MEMORY_PARTITION_MAINTENANCE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_PARTITION_MAINTENANCE_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_PARTITION_MAINTENANCE_TIMEOUT: must be positive, got %v", d)
		}
		c.MaintenanceTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_PARTITION_LAG_SCRAPE_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_PARTITION_LAG_SCRAPE_INTERVAL: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_PARTITION_LAG_SCRAPE_INTERVAL: must be positive, got %v", d)
		}
		c.LagScrapeInterval = d
	}
	if v := os.Getenv("AGENT_MEMORY_PARTITION_LAG_SCRAPE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_PARTITION_LAG_SCRAPE_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_PARTITION_LAG_SCRAPE_TIMEOUT: must be positive, got %v", d)
		}
		c.LagScrapeTimeout = d
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
	// The maintainer has two long-running goroutines
	// (maintenance + scrape). A small pool of 3 connections
	// lets them interleave without contention; the third slot
	// is for an ad-hoc /metrics handler that needs a transient
	// scrape (none today, but the headroom is cheap).
	pool.SetMaxOpenConns(3)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	logger.Info("partition-maintainer.pg.connected")
	return pool, nil
}

// writeMetrics renders the maintainer's counter / gauge snapshot
// in Prometheus text-format. Matches the hand-rolled exposition
// the trace-log-pruner binary uses so the two binaries'
// /metrics endpoints parse identically.
func writeMetrics(w http.ResponseWriter, m *partitionmaintainer.Metrics) {
	snap := m.Snapshot()
	helps := map[string]string{
		partitionmaintainer.MetricPartitionMaintenanceRunsTotal:   "partman.run_maintenance invocations since binary start (success or failure).",
		partitionmaintainer.MetricPartitionMaintenanceErrorsTotal: "partman.run_maintenance invocations that surfaced a non-nil error since binary start.",
		partitionmaintainer.MetricPartitionLagScrapesTotal:        "partition_provision_lag scrapes since binary start (success or failure).",
		partitionmaintainer.MetricPartitionLagScrapeErrorsTotal:   "partition_provision_lag scrapes that surfaced a non-nil error since binary start.",
		partitionmaintainer.MetricPartitionProvisionLagSeconds:    "Maximum across in-scope parents of (now() + 1 day - latest_child_end_time), clamped to >= 0. Stage 8.2 §8.2 alert threshold = 86400 (1 day).",
		partitionmaintainer.MetricPartitionParentsObservedGauge:   "Count of in-scope parents the most recent partition_provision_lag scrape iterated over.",
	}
	types := map[string]string{
		partitionmaintainer.MetricPartitionMaintenanceRunsTotal:   "counter",
		partitionmaintainer.MetricPartitionMaintenanceErrorsTotal: "counter",
		partitionmaintainer.MetricPartitionLagScrapesTotal:        "counter",
		partitionmaintainer.MetricPartitionLagScrapeErrorsTotal:   "counter",
		partitionmaintainer.MetricPartitionProvisionLagSeconds:    "gauge",
		partitionmaintainer.MetricPartitionParentsObservedGauge:   "gauge",
	}
	for _, name := range []string{
		partitionmaintainer.MetricPartitionMaintenanceRunsTotal,
		partitionmaintainer.MetricPartitionMaintenanceErrorsTotal,
		partitionmaintainer.MetricPartitionLagScrapesTotal,
		partitionmaintainer.MetricPartitionLagScrapeErrorsTotal,
		partitionmaintainer.MetricPartitionProvisionLagSeconds,
		partitionmaintainer.MetricPartitionParentsObservedGauge,
	} {
		// fmt.Fprintf into an http.ResponseWriter returns
		// (int, error); a broken pipe from a client that
		// hangs up mid-scrape is the only realistic failure
		// and is not actionable here. The blank receiver
		// pacifies errcheck without papering over a real
		// write failure to the local /metrics scraper.
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, helps[name])
		_, _ = fmt.Fprintf(w, "# TYPE %s %s\n", name, types[name])
		_, _ = fmt.Fprintf(w, "%s %d\n", name, snap[name])
	}
}
