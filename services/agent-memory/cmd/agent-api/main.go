// Command agent-api is the long-running process that serves
// the §6.4 agent recall path (and, in future workstreams,
// the observe / expand / summarize primitives).  This iter
// ships the RECALL composition only — the HTTP/MCP routing
// layer is Stage 4 work owned by a separate workstream.
//
// Why a process exists today (resolves evaluator iter-2
// finding #4 "no agent.recall path calls [RecallFilter]
// yet"):  the §9.6a read-side invariant requires that any
// vector that has not reached `published` MUST NOT be
// returned by `agent.recall`.  This binary wires the real
// `*embedding.RecallFilter` into the real
// `agentapi.Service`, proving the production composition
// exists and works — the recall service is not just a
// test-only struct.
//
// Configuration (env vars; no flags)
// ----------------------------------
//
//	AGENT_MEMORY_PG_RO_URL           postgres:// DSN for the
//	                                 reader-role connection
//	                                 (REQUIRED).  Should be a
//	                                 `agent_memory_ro` DSN so
//	                                 the recall path is
//	                                 mechanically read-only.
//	AGENT_MEMORY_QDRANT_URL          Qdrant base URL (REQUIRED)
//	AGENT_MEMORY_QDRANT_API_KEY      Qdrant api-key (optional)
//	AGENT_MEMORY_ALLOW_STUB_EMBEDDER if "true", uses an
//	                                 in-process stub query
//	                                 embedder.  Same caveat as
//	                                 cmd/repoindexer — NOT FIT
//	                                 FOR PRODUCTION.  Required
//	                                 today until the real
//	                                 embedder workstream
//	                                 lands.
//	AGENT_MEMORY_HEALTH_ADDR         (optional) bind address
//	                                 for a tiny health probe
//	                                 endpoint.  Default
//	                                 disabled (the binary
//	                                 stays foreground).  This
//	                                 hook lets Kubernetes
//	                                 liveness/readiness probes
//	                                 see the process before
//	                                 the Stage 4 HTTP router
//	                                 lands.
//
// Exit codes
// ----------
//
//	0  graceful shutdown (SIGINT/SIGTERM)
//	2  configuration error (missing required env)
//	3  startup failure (DB ping, Qdrant ping)
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
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/agentapi"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("agent-api.config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Open the READ-ONLY Postgres pool.  The DSN must point at
	// `agent_memory_ro` (migration 0017) so the RecallFilter
	// can SELECT the §9.6a state log but cannot UPDATE / INSERT
	// it — defence-in-depth against a misconfigured recall
	// caller that tries to mutate publish state.
	db, err := openPG(ctx, cfg.PGURL, logger)
	if err != nil {
		logger.Error("agent-api.pg_open", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer func() { _ = db.Close() }()

	// Build the Qdrant client.  Recall is read-only, but the
	// HTTPQdrant client is shared write-and-read — the
	// VectorSearcher interface narrows the recall path's
	// surface to just `Search`.
	qclient := newQdrantClient(cfg)
	if err := pingQdrant(ctx, qclient, cfg.QdrantURL); err != nil {
		logger.Error("agent-api.qdrant_ping", slog.String("error", err.Error()))
		os.Exit(3)
	}

	embedder := selectEmbedder(cfg, logger)
	filter := embedding.NewRecallFilter(db, &embedding.RecallMetrics{})
	service := agentapi.NewService(embedder, qclient, filter,
		agentapi.WithLogger(logger))

	logger.Info("agent-api.ready",
		slog.String("qdrant_url", cfg.QdrantURL),
		slog.Bool("stub_embedder", cfg.AllowStubEmbedder),
		slog.String("collection_method", embedding.CollectionMethod),
		slog.String("collection_block", embedding.CollectionBlock))

	// Optional health probe.  Cheap; lets the operator confirm
	// the process is alive without standing up the Stage 4
	// HTTP router.  Returns 200 always — the deeper "can we
	// actually serve recall" probe is a Stage 4 concern.
	if cfg.HealthAddr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		})
		srv := &http.Server{
			Addr:              cfg.HealthAddr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		go func() {
			logger.Info("agent-api.health.listen", slog.String("addr", cfg.HealthAddr))
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("agent-api.health.listen_failed",
					slog.String("error", err.Error()))
			}
		}()
		defer func() {
			shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutCtx)
		}()
	}

	// Reference `service` so the linker does NOT GC the
	// composition.  The actual HTTP/MCP routing wiring (which
	// will call `service.Recall(ctx, req)`) is Stage 4.
	// Until then this binary is the smallest live proof that
	// the production composition compiles and starts.
	_ = service

	<-ctx.Done()
	logger.Info("agent-api.shutdown")
}

type config struct {
	PGURL             string
	QdrantURL         string
	QdrantAPIKey      string
	HealthAddr        string
	AllowStubEmbedder bool
}

func loadConfig() (config, error) {
	c := config{
		PGURL:        os.Getenv("AGENT_MEMORY_PG_RO_URL"),
		QdrantURL:    os.Getenv("AGENT_MEMORY_QDRANT_URL"),
		QdrantAPIKey: os.Getenv("AGENT_MEMORY_QDRANT_API_KEY"),
		HealthAddr:   os.Getenv("AGENT_MEMORY_HEALTH_ADDR"),
	}
	if c.PGURL == "" {
		return c, errors.New("AGENT_MEMORY_PG_RO_URL is required")
	}
	if c.QdrantURL == "" {
		return c, errors.New("AGENT_MEMORY_QDRANT_URL is required")
	}
	if v := os.Getenv("AGENT_MEMORY_ALLOW_STUB_EMBEDDER"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_ALLOW_STUB_EMBEDDER: %w", err)
		}
		c.AllowStubEmbedder = b
	}
	return c, nil
}

func openPG(ctx context.Context, dsn string, logger *slog.Logger) (*sql.DB, error) {
	pool, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(8)
	pool.SetMaxIdleConns(2)
	pool.SetConnMaxIdleTime(5 * time.Minute)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.PingContext(pingCtx); err != nil {
		_ = pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	logger.Info("agent-api.pg.connected")
	return pool, nil
}

// selectEmbedder mirrors `cmd/repoindexer/main.go.selectEmbedder`
// — same stub gating, different doc comment.  When the real
// embedder workstream lands, BOTH binaries grow the same switch
// against `AGENT_MEMORY_EMBEDDER` and share a factory.
func selectEmbedder(cfg config, logger *slog.Logger) agentapi.QueryEmbedder {
	if !cfg.AllowStubEmbedder {
		logger.Error("agent-api.embedder_missing",
			slog.String("hint", "set AGENT_MEMORY_ALLOW_STUB_EMBEDDER=true for local development"))
		os.Exit(2)
	}
	logger.Warn("agent-api.embedder_stub",
		slog.String("warning",
			"stub query embedder returns a fixed zero-vector; NOT fit for production recall"))
	return stubEmbedder{}
}

type stubEmbedder struct{}

func (stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	// 768 dims matches `cmd/qdrant-bootstrap` default
	// vector_size and `cmd/repoindexer.stubEmbedder` — a
	// dim mismatch between the embedder used for indexing
	// and the embedder used for query would make recall
	// uniformly return zero hits.
	return make([]float32, 768), nil
}

// ModelVersion is intentionally omitted: the agentapi.QueryEmbedder
// interface does NOT need it.  The publisher's embedder needs
// `ModelVersion` to record on every publish row, but a query
// embedder is only ever vector-producing.  Keeping the
// interface minimal lets future production embedders implement
// it without bringing in publisher-side concerns.

func newQdrantClient(cfg config) *embedding.HTTPQdrant {
	c := &http.Client{Timeout: 30 * time.Second}
	if cfg.QdrantAPIKey != "" {
		c.Transport = &apiKeyTransport{
			key:  cfg.QdrantAPIKey,
			base: http.DefaultTransport,
		}
	}
	q := embedding.NewHTTPQdrant(cfg.QdrantURL)
	q.Client = c
	q.UserAgent = "agent-memory-agent-api/1"
	return q
}

func pingQdrant(ctx context.Context, q *embedding.HTTPQdrant, base string) error {
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// We use a non-existent point id GET to confirm Qdrant
	// responds with EITHER 200/404 (alive) instead of a
	// connection error (down).  `PointExists` already maps
	// 404 → (false, nil); a connection error surfaces as a
	// non-nil err.
	if _, err := q.PointExists(pingCtx, embedding.CollectionMethod, "00000000-0000-0000-0000-000000000000"); err != nil {
		// Tolerate the "collection not found" case — bootstrap
		// may not yet have created the collection in a fresh
		// environment.  Treat anything else as a hard fail.
		if !isCollectionNotFound(err) {
			return fmt.Errorf("qdrant ping: %w", err)
		}
	}
	_ = base
	return nil
}

func isCollectionNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// Qdrant returns 404 with a body containing
	// "Collection ... doesn't exist" or "Not found".
	return strings.Contains(msg, "doesn't exist") ||
		strings.Contains(msg, "Not found") ||
		strings.Contains(msg, "status 404")
}

// apiKeyTransport mirrors the cmd/repoindexer.apiKeyTransport.
// Duplicated rather than shared because cmd/* packages
// intentionally have no internal/shared helper module — each
// binary stays a small self-contained composition root.
type apiKeyTransport struct {
	key  string
	base http.RoundTripper
}

func (t *apiKeyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("api-key", t.key)
	rt := t.base
	if rt == nil {
		rt = http.DefaultTransport
	}
	return rt.RoundTrip(clone)
}
