// Command repoindexer is the Stage 3.1 full-mode + Stage 3.3
// EmbeddingIndex worker process per implementation-plan.md
// ┬º3.1 / ┬º3.3 and tech-spec.md ┬º9.6a.  It composes the
// architecture-owned write side (graphwriter), the Stage 3.2
// AST dispatcher, the Stage 3.3 ┬º9.6a publisher, and the
// Stage 3.3 background retry flusher into a single
// long-running process.
//
// The composition mirrors `internal/embedding/doc.go`'s
// "Production wiring" example verbatim ΓÇö the binary is the
// load-bearing demonstration that the publisher hook is
// actually invoked by the worker (per evaluator iter-1
// finding #5).  Without this main package the Stage 3.3
// publisher could be reached only by opt-in test composition.
//
// Configuration
// -------------
// All knobs are env vars; no flags.  This matches the
// `cmd/qdrant-bootstrap` convention and keeps the cloud-agent
// helm chart simple.
//
//	AGENT_MEMORY_PG_URL              postgres:// DSN (REQUIRED)
//	AGENT_MEMORY_QDRANT_URL          Qdrant base URL (REQUIRED)
//	AGENT_MEMORY_QDRANT_API_KEY      Qdrant api-key (optional)
//	AGENT_MEMORY_WORKER_ID           worker identity (default: hostname-pid)
//	AGENT_MEMORY_POLL_EVERY          worker poll interval (default: 1s)
//	AGENT_MEMORY_FLUSH_EVERY         flusher tick (default: 30s; 0 disables)
//	AGENT_MEMORY_ALLOW_STUB_EMBEDDER if "true", uses an in-process
//	                                 stub embedder when no real
//	                                 embedder is configured.  The
//	                                 stub returns a fixed
//	                                 zero-vector and IS NOT FIT
//	                                 FOR PRODUCTION RECALL ΓÇö it
//	                                 exists so the ┬º9.6a wiring
//	                                 can be exercised end-to-end
//	                                 before the embedding-model
//	                                 workstream lands.  Production
//	                                 deployment MUST swap to a
//	                                 real Embedder.
//
// Exit codes
// ----------
//
//	0  graceful shutdown (SIGINT/SIGTERM)
//	2  configuration error (missing required env, malformed DSN)
//	3  startup failure (DB ping, Qdrant unreachable)
//	4  runtime failure (worker.Run returned non-context error)
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphwriter"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/repoindexer/ast"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/retirement"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/snapshot"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("repoindexer.config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("postgres", cfg.PGURL)
	if err != nil {
		logger.Error("repoindexer.pg_open", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer db.Close()
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(2)

	pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
	if err := db.PingContext(pingCtx); err != nil {
		pingCancel()
		logger.Error("repoindexer.pg_ping", slog.String("error", err.Error()))
		os.Exit(3)
	}
	pingCancel()

	// Construct the ┬º9.6a wiring per
	// internal/embedding/doc.go "Production wiring":
	//   embedder + qdrant + db -> Publisher
	//   Publisher -> AsASTPublisher -> WithEmbeddingPublisher
	//   Dispatcher (with publisher hook) -> WorkerOptions.Emitter
	//   Publisher + ContentResolver -> Flusher (background)
	embedder := selectEmbedder(cfg, logger)
	qdrant := embedding.NewHTTPQdrant(cfg.QdrantURL)
	if cfg.QdrantAPIKey != "" {
		// HTTPQdrant does not expose a typed API-key field; the
		// publisher-side client is intentionally narrow.  Wrap
		// the underlying http.Client with a header-injecting
		// transport so the api-key still travels on every
		// upsert / fetch request without leaking the secret
		// into UserAgent or query string.
		qdrant.Client = &http.Client{
			Timeout:   30 * time.Second,
			Transport: &apiKeyTransport{key: cfg.QdrantAPIKey, base: http.DefaultTransport},
		}
	}

	// Iter-2 fix #4: instantiate the snapshot.Metrics
	// counter that this binary's /metrics endpoint exposes
	// and that the publisher's post-publish hook increments
	// when a publish supersedes a prior (i.e. the publish
	// was enqueued by the mgmt.snapshot verb). Without this
	// hook the snapshot_published_total counter the ┬º6.2.1
	// observability contract requires would never advance.
	snapMetrics := snapshot.NewMetrics()
	publisher := embedding.NewPublisher(db, embedder, qdrant,
		embedding.WithLogger(logger),
		embedding.WithPostPublishHook(func(ev embedding.PublishedEvent) {
			// Attribute publishes to snapshot ONLY when
			// the queued event carried a
			// supersedes_publish_id (i.e. the publish was
			// enqueued by the snapshot verb, not by a
			// normal ingest path).
			if ev.SupersededPublishID == "" {
				return
			}
			snapMetrics.IncPublished(1)
		}),
	)

	gw := graphwriter.New(db, logger)

	dispatcher := ast.NewDispatcher(gw,
		ast.WithEmbeddingPublisher(embedding.AsASTPublisher(publisher)))

	notifyPub := repoindexer.NewPGNotifyPublisher(db, logger)

	// Stage 3.4 (delta re-index) dependencies. The Differ
	// self-manages a temp bare clone per Diff() call (see
	// internal/repoindexer/diff.go); the Retirer wraps
	// retirement.Service so the worker can write tombstones
	// for removed / renamed Nodes. Both are required for
	// delta jobs to dispatch ΓÇö full-only deployments could
	// leave these nil, but production wiring always supplies
	// them so a misrouted delta job surfaces as a job-level
	// failure rather than silently sitting in `pending`.
	differ := &repoindexer.GitDeltaDiffer{}
	retirer := repoindexer.NewRetirementAdapter(retirement.New(db, logger))

	worker := repoindexer.NewWorker(db, gw, repoindexer.WorkerOptions{
		WorkerID:     cfg.WorkerID,
		PollEvery:    cfg.PollEvery,
		Materializer: &repoindexer.GitMaterializer{},
		Emitter:      dispatcher,
		Publisher:    notifyPub,
		Differ:       differ,
		Retirer:      retirer,
		Logger:       logger,
	})

	// Optionally start the ┬º9.6a background flusher.  The
	// resolver reads the persisted `queued`-event snapshot
	// from `embedding_publish_event.details_json` so the
	// long-running worker process does NOT need to keep
	// source bytes in memory across a crash.  The snapshot
	// itself is written by the publisher at every
	// `Publish` / `Retry` (see publisher.go's
	// `marshalQueuedDetails`).
	if cfg.FlushEvery > 0 {
		resolver := embedding.NewPublishEventContentResolver(db)
		flusher := embedding.NewFlusher(db, publisher, resolver,
			embedding.WithFlusherLogger(logger))
		go func() {
			err := flusher.Run(ctx, cfg.FlushEvery)
			if err != nil && !errors.Is(err, context.Canceled) {
				logger.Warn("repoindexer.flusher",
					slog.String("error", err.Error()))
			}
		}()
		logger.Info("repoindexer.flusher.started",
			slog.Duration("every", cfg.FlushEvery))
	}

	// Iter-2 fix #4: start the /metrics + /healthz HTTP
	// listener so Prometheus can scrape this binary's
	// snapshot_published_total contribution. The listener
	// runs in a background goroutine so a metrics-port
	// bind failure does not block the publish/ingest
	// worker ΓÇö operators get a startup log line either way.
	startMetricsServer(ctx, cfg.MetricsListenAddr, snapMetrics, logger)

	logger.Info("repoindexer.start",
		slog.String("worker_id", cfg.WorkerID),
		slog.Duration("poll_every", cfg.PollEvery),
		slog.String("qdrant_url", cfg.QdrantURL),
		slog.String("embedder_model", embedder.ModelVersion()))

	if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("repoindexer.run", slog.String("error", err.Error()))
		os.Exit(4)
	}
	logger.Info("repoindexer.shutdown")
}

// config is the env-derived configuration the binary uses.
// Centralised here so `loadConfig` can fail fast with one
// error per missing setting rather than scattering
// `os.Getenv` calls through the wiring.
type config struct {
	PGURL             string
	QdrantURL         string
	QdrantAPIKey      string
	WorkerID          string
	PollEvery         time.Duration
	FlushEvery        time.Duration
	AllowStubEmbedder bool
	// MetricsListenAddr is the host:port the /metrics +
	// /healthz HTTP listener binds to. Empty disables the
	// listener (operators running a single-process dev
	// deploy may not want a second open port). Default
	// `:9101` chosen to not collide with the typical
	// mgmt-api `:8080`.
	MetricsListenAddr string
}

func loadConfig() (config, error) {
	c := config{
		PGURL:             os.Getenv("AGENT_MEMORY_PG_URL"),
		QdrantURL:         os.Getenv("AGENT_MEMORY_QDRANT_URL"),
		QdrantAPIKey:      os.Getenv("AGENT_MEMORY_QDRANT_API_KEY"),
		WorkerID:          os.Getenv("AGENT_MEMORY_WORKER_ID"),
		PollEvery:         1 * time.Second,
		FlushEvery:        30 * time.Second,
		MetricsListenAddr: ":9101",
	}
	if c.PGURL == "" {
		return c, errors.New("AGENT_MEMORY_PG_URL is required")
	}
	if c.QdrantURL == "" {
		return c, errors.New("AGENT_MEMORY_QDRANT_URL is required")
	}
	if v := os.Getenv("AGENT_MEMORY_POLL_EVERY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_POLL_EVERY: %w", err)
		}
		c.PollEvery = d
	}
	if v := os.Getenv("AGENT_MEMORY_FLUSH_EVERY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_FLUSH_EVERY: %w", err)
		}
		c.FlushEvery = d
	}
	if v := os.Getenv("AGENT_MEMORY_ALLOW_STUB_EMBEDDER"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_ALLOW_STUB_EMBEDDER: %w", err)
		}
		c.AllowStubEmbedder = b
	}
	if v, ok := os.LookupEnv("AGENT_MEMORY_METRICS_LISTEN_ADDR"); ok {
		// Explicit empty string disables the listener.
		c.MetricsListenAddr = v
	}
	if c.WorkerID == "" {
		host, _ := os.Hostname()
		c.WorkerID = fmt.Sprintf("repoindexer-%s-%d", host, os.Getpid())
	}
	return c, nil
}

// startMetricsServer spins up an HTTP listener on `addr`
// exposing /metrics (Prometheus text format) and /healthz.
// Iter-2 fix #4: this is the binary surface for the
// `snapshot_published_total` counter ΓÇö without it a Prometheus
// scrape from the ┬º6.2.1 observability contract has nothing
// to read. The listener runs in a background goroutine; a
// non-empty `addr` that fails to bind is logged but does not
// abort the binary because the publish worker is the primary
// duty.
func startMetricsServer(ctx context.Context, addr string, m *snapshot.Metrics, logger *slog.Logger) {
	if addr == "" {
		logger.Info("repoindexer.metrics.disabled",
			slog.String("reason", "AGENT_MEMORY_METRICS_LISTEN_ADDR=\"\" (explicit disable)"))
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeSnapshotMetrics(w, m)
	})
	srv := &http.Server{
		Addr:    addr,
		Handler: mux,
		// Mirror the hardening posture of the mgmt-api /
		// span-ingestor / webhook-receiver listeners
		// (ReadHeaderTimeout 10s, Read/WriteTimeout 30s).
		// ReadHeaderTimeout alone mitigates slowloris header
		// attacks but a client that sends a valid header and
		// then drips the body — or reads /metrics output one
		// byte at a time — can otherwise hold the goroutine
		// open indefinitely. The /metrics + /healthz handlers
		// here finish in well under a second so 30s is a
		// generous safety net rather than a tight SLO.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	go func() {
		logger.Info("repoindexer.metrics.listen",
			slog.String("addr", addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("repoindexer.metrics.serve_failed",
				slog.String("addr", addr),
				slog.String("error", err.Error()))
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
}

// snapshotMetricsOrder pins the line order of the /metrics
// exposition; stable order keeps scrape diffs human-readable.
var snapshotMetricsOrder = []string{
	snapshot.MetricSnapshotPendingTotal,
	snapshot.MetricSnapshotPublishedTotal,
}

var snapshotMetricsHelp = map[string]string{
	snapshot.MetricSnapshotPendingTotal:   "Cumulative snapshot targets enqueued by the mgmt.snapshot verb. Always 0 in the repoindexer binary (the enqueue happens in mgmt-api); reported here for scrape-symmetry.",
	snapshot.MetricSnapshotPublishedTotal: "Cumulative snapshot supersedes that completed publish in this binary. Incremented by the embedding-publisher post-publish hook only when the queued event carried supersedes_publish_id.",
}

func writeSnapshotMetrics(w io.Writer, m *snapshot.Metrics) {
	if m == nil {
		m = snapshot.NewMetrics()
	}
	snap := m.Snapshot()
	for _, name := range snapshotMetricsOrder {
		help := snapshotMetricsHelp[name]
		if help == "" {
			help = "(no description)"
		}
		fmt.Fprintf(w, "# HELP %s %s\n", name, help)
		fmt.Fprintf(w, "# TYPE %s counter\n", name)
		fmt.Fprintf(w, "%s %d\n", name, snap[name])
	}
}

// selectEmbedder picks the embedding-model client based on
// configuration.  Today only the stub is available; once the
// real embedder workstream lands this function grows a
// switch on `AGENT_MEMORY_EMBEDDER` (e.g. "openai", "e5",
// "local-onnx") that constructs the chosen client.  Until
// then the binary refuses to start in non-stub mode so we
// don't accidentally deploy a no-op embedder.
func selectEmbedder(cfg config, logger *slog.Logger) embedding.Embedder {
	if !cfg.AllowStubEmbedder {
		logger.Error("repoindexer.embedder_missing",
			slog.String("hint", "set AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true to use the in-process stub for local development"))
		os.Exit(2)
	}
	logger.Warn("repoindexer.embedder_stub",
		slog.String("warning", "stub embedder returns a fixed zero-vector; NOT fit for production recall"))
	return stubEmbedder{}
}

// stubEmbedder is the local-development placeholder.  It
// returns a fixed all-zeros vector and a stable model
// version string.  The ┬º9.6a contract treats this as a real
// embedder for the purposes of the publish protocol ΓÇö the
// row carries the stub model version, the recall path
// surfaces the resulting vector, and a future operator can
// trigger re-embedding by switching to a real Embedder
// (the model_version bump cascades into Publisher.Retry's
// model-mismatch refusal so old vectors are NOT served as
// fresh-model results).
type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	// 768 dims chosen to match `cmd/qdrant-bootstrap`
	// default vector_size; an oversized stub trips the
	// collection's dim CHECK at upsert time.
	return make([]float32, 768), nil
}

func (stubEmbedder) ModelVersion() string {
	return "stub-zero-vector@v0"
}

// apiKeyTransport is the http.RoundTripper that adds the
// Qdrant `api-key` header to every outbound request.  The
// header lives in the request, not the URL, so it never
// shows up in proxy logs or http.Client.Timeout retries
// the way a `?api_key=` query string would.
type apiKeyTransport struct {
	key  string
	base http.RoundTripper
}

func (t *apiKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating: http.RoundTripper contract
	// requires the request to be left intact for retries.
	clone := req.Clone(req.Context())
	clone.Header.Set("api-key", t.key)
	rt := t.base
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(clone)
}
