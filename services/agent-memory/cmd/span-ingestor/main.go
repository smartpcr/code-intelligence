// Command span-ingestor is the Stage 4.2 long-running process
// per implementation-plan.md §4.2 and architecture.md §3.3. It
// hosts an OTLP/HTTP receiver, resolves incoming spans via the
// Stage 4.1 attribute-mapping ladder, and persists
// `observed_calls` Edges + `TraceObservation` aggregates + log
// rows via GraphWriter. A background supervisor watches the
// queue depth and UPSERTs `repo_health` to surface
// `span_ingestor_backpressure` when the §8.3 sustained envelope
// is exceeded; the agent-api recall handler reads that row and
// surfaces `degraded=true` on its responses.
//
// Configuration (env vars; no flags)
// ----------------------------------
//
//	AGENT_MEMORY_PG_URL                  postgres:// DSN (REQUIRED)
//	AGENT_MEMORY_LISTEN_ADDR             bind address (default `:4318`)
//	                                     -- the OTLP/HTTP default port
//	AGENT_MEMORY_OTLP_GRPC_LISTEN        gRPC OTLP listen address.
//	                                     Unset / empty defaults to
//	                                     `:4317` (the OTLP/gRPC
//	                                     spec default port). Set
//	                                     to the literal `-`
//	                                     sentinel to disable the
//	                                     gRPC receiver entirely
//	                                     (HTTP-only mode).
//	AGENT_MEMORY_SHUTDOWN_TIMEOUT        graceful-shutdown budget
//	                                     (default 30s)
//	AGENT_MEMORY_OTLP_MAX_BYTES          per-request body cap
//	                                     (default 4194304 = 4 MiB)
//	AGENT_MEMORY_INGESTOR_QUEUE_DEPTH    bounded queue capacity
//	                                     (default 1024)
//	AGENT_MEMORY_INGESTOR_BP_THRESHOLD   sustained queue depth at or
//	                                     above which the supervisor
//	                                     counts toward degraded
//	                                     transition (default 100,
//	                                     i.e. 2x §8.3 envelope)
//	AGENT_MEMORY_INGESTOR_BP_SUSTAIN     window the depth must stay
//	                                     above threshold (default 30s)
//	AGENT_MEMORY_INGESTOR_BP_CLEAR       cooldown for clearing
//	                                     (default same as sustain)
//	AGENT_MEMORY_SERVICE_NAME_MAP        comma-separated
//	                                     `service.name=repo_id` pairs
//	                                     the receiver uses to translate
//	                                     OTel `service.name` into a
//	                                     `repo_id`. Spans whose service
//	                                     is not listed are dropped at
//	                                     the receiver so an unknown
//	                                     deploy can't spam the table
//	                                     with rows for repos we don't
//	                                     know about.
//
// Exit codes
// ----------
//
//	0  graceful shutdown (SIGINT/SIGTERM)
//	2  configuration error (missing required env, malformed DSN)
//	3  startup failure (DB ping, listener bind)
//	4  runtime failure (http.Server returned non-ErrServerClosed)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/spaningestor"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("span-ingestor.config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openPG(ctx, cfg, logger)
	if err != nil {
		logger.Error("span-ingestor.pg", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer func() { _ = db.Close() }()

	writer := graphwriter.New(db, logger)
	lookup := spaningestor.NewPGLookup(db)
	resolverMetrics := spaningestor.NewMetrics()
	resolver := spaningestor.New(lookup, resolverMetrics, logger)

	ingestor := spaningestor.NewIngestor(
		resolver,
		writer,
		writer, // *graphwriter.Writer satisfies HealthWriter
		spaningestor.Config{
			QueueDepth:            cfg.QueueDepth,
			BackpressureThreshold: cfg.BackpressureThreshold,
			BackpressureSustain:   cfg.BackpressureSustain,
			BackpressureClearance: cfg.BackpressureClearance,
		},
		logger,
	)
	// Evaluator iter-1 #7: thread the per-repo current_head_sha
	// through so observed-call edges carry the real SHA on
	// EdgeInput.FromSHA instead of the "observed" sentinel.
	// PGLookup implements both the Lookup AND SHAReader
	// interfaces over the same *sql.DB pool.
	ingestor.SetSHAReader(lookup)

	receiver := spaningestor.NewOTLPReceiver(
		ingestor,
		cfg.serviceNameToRepoID,
		spaningestor.OTLPConfig{MaxBytes: cfg.OTLPMaxBytes},
		logger,
	)
	// Evaluator iter-1 #1: wire the gRPC OTLP server on its
	// own listener so the Collector's gRPC exporter
	// (the default) works against this binary.
	var grpcServer *grpc.Server
	if cfg.OTLPGRPCAddr != "" {
		grpcServer = grpc.NewServer()
		coltracepb.RegisterTraceServiceServer(grpcServer,
			spaningestor.NewOTLPGRPCServer(ingestor, cfg.serviceNameToRepoID, logger))
	}

	mux := http.NewServeMux()
	mux.Handle("/v1/traces", receiver)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		writeMetrics(w, ingestor, resolverMetrics.Snapshot())
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	// Worker goroutine drains the queue. We run the worker
	// loop in-process; a future scale-out variant would run
	// the ingestor as a separate binary fed by Kafka.
	workerErr := make(chan error, 1)
	go func() {
		workerErr <- ingestor.Run(ctx)
	}()

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("span-ingestor.listen", slog.String("addr", cfg.ListenAddr))
		serveErr <- srv.ListenAndServe()
	}()

	// Optional gRPC OTLP listener — separate goroutine so
	// either transport going down doesn't affect the other.
	grpcErr := make(chan error, 1)
	if grpcServer != nil {
		grpcLis, err := net.Listen("tcp", cfg.OTLPGRPCAddr)
		if err != nil {
			logger.Error("span-ingestor.grpc_listen",
				slog.String("addr", cfg.OTLPGRPCAddr),
				slog.String("error", err.Error()))
			os.Exit(3)
		}
		go func() {
			logger.Info("span-ingestor.grpc_listen", slog.String("addr", cfg.OTLPGRPCAddr))
			grpcErr <- grpcServer.Serve(grpcLis)
		}()
	}

	logger.Info("span-ingestor.ready",
		slog.String("addr", cfg.ListenAddr),
		slog.String("grpc_addr", cfg.OTLPGRPCAddr),
		slog.Int("queue_depth", cfg.QueueDepth),
		slog.Int("backpressure_threshold", cfg.BackpressureThreshold),
		slog.Duration("backpressure_sustain", cfg.BackpressureSustain),
	)

	select {
	case <-ctx.Done():
		logger.Info("span-ingestor.shutdown.signal")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Warn("span-ingestor.shutdown.error",
				slog.String("error", err.Error()))
		}
		if grpcServer != nil {
			stopped := make(chan struct{})
			go func() {
				grpcServer.GracefulStop()
				close(stopped)
			}()
			select {
			case <-stopped:
			case <-shutCtx.Done():
				logger.Warn("span-ingestor.shutdown.grpc_timeout")
				grpcServer.Stop()
			}
		}
		<-serveErr
		select {
		case <-workerErr:
		case <-shutCtx.Done():
			logger.Warn("span-ingestor.shutdown.worker_timeout")
		}
		logger.Info("span-ingestor.shutdown.done")
		return
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("span-ingestor.serve", slog.String("error", err.Error()))
			os.Exit(4)
		}
	case err := <-grpcErr:
		if err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			logger.Error("span-ingestor.grpc_serve", slog.String("error", err.Error()))
			os.Exit(4)
		}
	case err := <-workerErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("span-ingestor.worker", slog.String("error", err.Error()))
			os.Exit(4)
		}
	}
}

// config is the env-derived configuration the binary uses.
type config struct {
	PGURL                 string
	ListenAddr            string
	OTLPGRPCAddr          string
	ShutdownTimeout       time.Duration
	OTLPMaxBytes          int64
	QueueDepth            int
	BackpressureThreshold int
	BackpressureSustain   time.Duration
	BackpressureClearance time.Duration
	serviceMap            map[string]string
}

// serviceNameToRepoID is the closure the OTLPReceiver consumes;
// returns "" for unknown service names.
func (c *config) serviceNameToRepoID(serviceName string) string {
	if v, ok := c.serviceMap[serviceName]; ok {
		return v
	}
	return ""
}

func loadConfig() (config, error) {
	c := config{
		PGURL:                 os.Getenv("AGENT_MEMORY_PG_URL"),
		ListenAddr:            os.Getenv("AGENT_MEMORY_LISTEN_ADDR"),
		OTLPGRPCAddr:          os.Getenv("AGENT_MEMORY_OTLP_GRPC_LISTEN"),
		ShutdownTimeout:       30 * time.Second,
		OTLPMaxBytes:          4 * 1024 * 1024,
		QueueDepth:            1024,
		BackpressureThreshold: 100,
		BackpressureSustain:   30 * time.Second,
		BackpressureClearance: 30 * time.Second,
		serviceMap:            map[string]string{},
	}
	if c.PGURL == "" {
		return c, errors.New("AGENT_MEMORY_PG_URL is required")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":4318"
	}
	// Default gRPC address is :4317 per the OTLP spec. To
	// explicitly disable the gRPC receiver, set
	// AGENT_MEMORY_OTLP_GRPC_LISTEN="-" (sentinel that turns
	// into an empty string after the check below).
	if c.OTLPGRPCAddr == "" {
		c.OTLPGRPCAddr = ":4317"
	}
	if c.OTLPGRPCAddr == "-" {
		c.OTLPGRPCAddr = ""
	}
	if v := os.Getenv("AGENT_MEMORY_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_SHUTDOWN_TIMEOUT: %w", err)
		}
		c.ShutdownTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_OTLP_MAX_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_OTLP_MAX_BYTES: must be positive int, got %q", v)
		}
		c.OTLPMaxBytes = n
	}
	if v := os.Getenv("AGENT_MEMORY_INGESTOR_QUEUE_DEPTH"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_INGESTOR_QUEUE_DEPTH: must be positive int, got %q", v)
		}
		c.QueueDepth = n
	}
	if v := os.Getenv("AGENT_MEMORY_INGESTOR_BP_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_INGESTOR_BP_THRESHOLD: must be positive int, got %q", v)
		}
		c.BackpressureThreshold = n
	}
	if v := os.Getenv("AGENT_MEMORY_INGESTOR_BP_SUSTAIN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_INGESTOR_BP_SUSTAIN: %w", err)
		}
		c.BackpressureSustain = d
	}
	if v := os.Getenv("AGENT_MEMORY_INGESTOR_BP_CLEAR"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_INGESTOR_BP_CLEAR: %w", err)
		}
		c.BackpressureClearance = d
	}
	if v := os.Getenv("AGENT_MEMORY_SERVICE_NAME_MAP"); v != "" {
		for _, pair := range strings.Split(v, ",") {
			kv := strings.SplitN(strings.TrimSpace(pair), "=", 2)
			if len(kv) != 2 || kv[0] == "" || kv[1] == "" {
				return c, fmt.Errorf("AGENT_MEMORY_SERVICE_NAME_MAP: malformed pair %q", pair)
			}
			c.serviceMap[kv[0]] = kv[1]
		}
	}
	return c, nil
}

func openPG(ctx context.Context, cfg config, logger *slog.Logger) (*sql.DB, error) {
	pool, err := sql.Open("postgres", cfg.PGURL)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	pool.SetMaxOpenConns(16)
	pool.SetMaxIdleConns(4)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	logger.Info("span-ingestor.pg.connected")
	return pool, nil
}

// writeMetrics renders the Ingestor's per-repo counters in a
// Prometheus-text-format compatible shape. We don't import the
// prom client to keep the binary's dep set small; the
// exposition format is a stable plain-text contract.
func writeMetrics(w http.ResponseWriter, ing *spaningestor.Ingestor, resolverUnresolved map[string]int64) {
	snap := ing.Metrics().SnapshotCounters()
	for name, counters := range snap {
		fmt.Fprintf(w, "# HELP %s Span Ingestor counter\n", name)
		fmt.Fprintf(w, "# TYPE %s counter\n", name)
		for repoID, v := range counters {
			fmt.Fprintf(w, "%s{repo_id=%q} %d\n", name, repoID, v)
		}
	}
	fmt.Fprintf(w, "# HELP span_unresolved_total Resolver miss counter\n")
	fmt.Fprintf(w, "# TYPE span_unresolved_total counter\n")
	for repoID, v := range resolverUnresolved {
		fmt.Fprintf(w, "%s{repo_id=%q} %d\n", "span_unresolved_total", repoID, v)
	}
	winEv, keyEv := ing.Aggregator().Snapshot()
	fmt.Fprintf(w, "# HELP latency_window_evictions_total Per-key sample evictions\n")
	fmt.Fprintf(w, "# TYPE latency_window_evictions_total counter\n")
	fmt.Fprintf(w, "latency_window_evictions_total %d\n", winEv)
	fmt.Fprintf(w, "# HELP latency_key_evictions_total LRU evictions on the aggregator key map\n")
	fmt.Fprintf(w, "# TYPE latency_key_evictions_total counter\n")
	fmt.Fprintf(w, "latency_key_evictions_total %d\n", keyEv)
	fmt.Fprintf(w, "# HELP span_ingestor_queue_depth Current queue depth\n")
	fmt.Fprintf(w, "# TYPE span_ingestor_queue_depth gauge\n")
	fmt.Fprintf(w, "span_ingestor_queue_depth %d\n", ing.QueueDepth())
}
