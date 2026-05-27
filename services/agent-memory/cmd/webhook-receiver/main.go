// Command webhook-receiver is the Stage 3.5 long-running process
// per implementation-plan.md §3.5 and architecture.md §3.1. It
// accepts authenticated push / merge events from any configured
// git host, writes a `repo_event` audit row, and enqueues an
// `ingest_jobs(mode=delta)` row that the Repo Indexer's delta
// worker (Stage 3.4) consumes.
//
// The handler itself lives in `internal/webhookreceiver`; this
// binary is the cloud-agent composition root that wires up
// PostgreSQL, TLS, signal handling, and the HTTP mux. The
// composition shape mirrors `cmd/repoindexer` and
// `cmd/agent-api` (env-only config, no flags, single
// long-running goroutine, exit-code-discipline) so all three
// binaries deploy through the same helm chart skeleton.
//
// Configuration (env vars; no flags)
// ----------------------------------
//
//	AGENT_MEMORY_PG_URL              postgres:// DSN (REQUIRED)
//	AGENT_MEMORY_LISTEN_ADDR         bind address (default `:8443`)
//	AGENT_MEMORY_TLS_CERT_FILE       PEM-encoded server cert
//	AGENT_MEMORY_TLS_KEY_FILE        PEM-encoded server key
//	                                 (BOTH required for HTTPS unless
//	                                 ALLOW_PLAINTEXT is set)
//	AGENT_MEMORY_ALLOW_PLAINTEXT     if "true", serves plain HTTP
//	                                 instead of HTTPS. Intended for
//	                                 local-dev only -- production
//	                                 deployments MUST terminate TLS
//	                                 at this hop because the HMAC
//	                                 secret travels over the wire
//	                                 on every request and a
//	                                 plaintext channel exposes it
//	                                 to any on-path attacker.
//	AGENT_MEMORY_SIGNATURE_HEADER    HTTP header to read the HMAC
//	                                 from (default
//	                                 `X-Hub-Signature-256`).
//	AGENT_MEMORY_MAX_BODY_BYTES      request-body cap in bytes
//	                                 (default 1048576 = 1 MiB).
//	AGENT_MEMORY_READ_TIMEOUT        per-request read timeout
//	                                 (default 30s).
//	AGENT_MEMORY_WRITE_TIMEOUT       per-request write timeout
//	                                 (default 30s).
//	AGENT_MEMORY_SHUTDOWN_TIMEOUT    graceful-shutdown budget
//	                                 (default 30s).
//
// Exit codes
// ----------
//
//	0  graceful shutdown (SIGINT/SIGTERM)
//	2  configuration error (missing required env, malformed DSN,
//	                        TLS files unreadable)
//	3  startup failure (DB ping, listener bind)
//	4  runtime failure (http.Server returned non-ErrServerClosed)
package main

import (
	"context"
	"crypto/tls"
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

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/obs"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/webhookreceiver"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("webhook-receiver.config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Stage 8.3 step 2 — OTel trace export. Best-effort
	// noop when no endpoint configured.
	tracerSetup, err := obs.SetupTracer(ctx, obs.ServiceNameWebhookReceiver, logger)
	if err != nil {
		logger.Error("webhook-receiver.otel.setup_failed", slog.String("error", err.Error()))
		os.Exit(2)
	}
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracerSetup.Shutdown(shutCtx)
	}()
	logger.Info("webhook-receiver.otel.ready",
		slog.Bool("exporting", tracerSetup.Exporting),
		slog.String("endpoint", tracerSetup.EndpointResolved))

	db, err := openPG(ctx, cfg, logger)
	if err != nil {
		logger.Error("webhook-receiver.pg", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer func() { _ = db.Close() }()

	handler := webhookreceiver.NewHandler(db, webhookreceiver.Options{
		Logger:          logger,
		SignatureHeader: cfg.SignatureHeader,
		MaxBodyBytes:    cfg.MaxBodyBytes,
		// Stage 8.3 step 2 -- wire the OTel tracer so each
		// inbound webhook produces a
		// `webhookreceiver.receive` span.
		Tracer: tracerSetup.Tracer,
	})

	mux := http.NewServeMux()
	mux.Handle(webhookreceiver.RoutePrefix, handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Stage 8.3 step 1 — /metrics surface. The receiver now
	// exposes a per-status request counter on top of the
	// up-gauge so `sum by (status) (rate(...))` queries on the
	// dashboard render real data without a manual log scrape.
	// Iter-3 evaluator fix #2 added the counter to close the
	// "every binary emits counters" gap.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		fmt.Fprintf(w, "# HELP webhook_receiver_up Always 1 -- presence indicates the binary is reachable for Prometheus scrape (Stage 8.3 step 1).\n")
		fmt.Fprintf(w, "# TYPE webhook_receiver_up gauge\n")
		fmt.Fprintf(w, "webhook_receiver_up 1\n")
		handler.WriteMetrics(w)
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		// Pin TLS to 1.2+ -- 1.0 / 1.1 have been deprecated
		// for years and tlsv1 ciphersuites have known
		// confidentiality holes. The git-host fleet supports
		// 1.2+ universally.
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	serveErr := make(chan error, 1)
	go func() {
		if cfg.AllowPlaintext {
			logger.Warn("webhook-receiver.plaintext_listen",
				slog.String("addr", cfg.ListenAddr),
				slog.String("warning",
					"serving plain HTTP; HMAC secret will travel unencrypted -- NOT FIT FOR PRODUCTION"),
			)
			serveErr <- srv.ListenAndServe()
			return
		}
		logger.Info("webhook-receiver.tls_listen",
			slog.String("addr", cfg.ListenAddr),
			slog.String("cert_file", cfg.TLSCertFile),
		)
		serveErr <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	}()

	logger.Info("webhook-receiver.ready",
		slog.String("addr", cfg.ListenAddr),
		slog.Bool("plaintext", cfg.AllowPlaintext),
		slog.String("signature_header", cfg.SignatureHeader),
		slog.Int64("max_body_bytes", cfg.MaxBodyBytes),
	)

	select {
	case <-ctx.Done():
		logger.Info("webhook-receiver.shutdown.signal")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Warn("webhook-receiver.shutdown.error",
				slog.String("error", err.Error()))
		}
		// Drain the goroutine so the binary exits cleanly.
		<-serveErr
		logger.Info("webhook-receiver.shutdown.done")
		return
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("webhook-receiver.serve",
				slog.String("error", err.Error()))
			os.Exit(4)
		}
		logger.Info("webhook-receiver.serve.exit")
	}
}

// config is the env-derived configuration the binary uses.
type config struct {
	PGURL           string
	ListenAddr      string
	TLSCertFile     string
	TLSKeyFile      string
	AllowPlaintext  bool
	SignatureHeader string
	MaxBodyBytes    int64
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration
}

func loadConfig() (config, error) {
	c := config{
		PGURL:           os.Getenv("AGENT_MEMORY_PG_URL"),
		ListenAddr:      os.Getenv("AGENT_MEMORY_LISTEN_ADDR"),
		TLSCertFile:     os.Getenv("AGENT_MEMORY_TLS_CERT_FILE"),
		TLSKeyFile:      os.Getenv("AGENT_MEMORY_TLS_KEY_FILE"),
		SignatureHeader: os.Getenv("AGENT_MEMORY_SIGNATURE_HEADER"),
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		ShutdownTimeout: 30 * time.Second,
	}
	if c.PGURL == "" {
		return c, errors.New("AGENT_MEMORY_PG_URL is required")
	}
	if c.ListenAddr == "" {
		c.ListenAddr = ":8443"
	}
	if c.SignatureHeader == "" {
		c.SignatureHeader = webhookreceiver.DefaultSignatureHeader
	}
	c.MaxBodyBytes = webhookreceiver.DefaultMaxBodyBytes
	if v := os.Getenv("AGENT_MEMORY_MAX_BODY_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			return c, fmt.Errorf("AGENT_MEMORY_MAX_BODY_BYTES: must be a positive integer, got %q", v)
		}
		c.MaxBodyBytes = n
	}
	if v := os.Getenv("AGENT_MEMORY_READ_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_READ_TIMEOUT: %w", err)
		}
		c.ReadTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_WRITE_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_WRITE_TIMEOUT: %w", err)
		}
		c.WriteTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_SHUTDOWN_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_SHUTDOWN_TIMEOUT: %w", err)
		}
		c.ShutdownTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_ALLOW_PLAINTEXT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_ALLOW_PLAINTEXT: %w", err)
		}
		c.AllowPlaintext = b
	}
	if !c.AllowPlaintext {
		if c.TLSCertFile == "" || c.TLSKeyFile == "" {
			return c, errors.New(
				"AGENT_MEMORY_TLS_CERT_FILE and AGENT_MEMORY_TLS_KEY_FILE are required " +
					"unless AGENT_MEMORY_ALLOW_PLAINTEXT=true")
		}
		// Verify both files exist and are readable BEFORE the
		// listener tries to consume them, so a missing cert
		// surfaces as a config error (exit 2) rather than a
		// runtime crash (exit 4).
		for _, p := range []string{c.TLSCertFile, c.TLSKeyFile} {
			f, err := os.Open(p)
			if err != nil {
				return c, fmt.Errorf("TLS file %q unreadable: %w", p, err)
			}
			_ = f.Close()
		}
	}
	return c, nil
}

func openPG(ctx context.Context, cfg config, logger *slog.Logger) (*sql.DB, error) {
	pool, err := sql.Open("postgres", cfg.PGURL)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	pool.SetMaxOpenConns(8)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxIdleTime(5 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	logger.Info("webhook-receiver.pg.connected")
	return pool, nil
}
