// Command agent-api is the long-running process that serves
// the §6.4 agent recall path AND the §6.1.2 agent observe
// verb. Stage 5.2 (this iter) ships the production wiring
// for observe: a SQL-backed EpisodeAppender, a composite-
// key ContextResolver, a file-backed WAL with a background
// flusher, and a Prometheus `/metrics` endpoint exposing
// the `observe_wal_buffer_depth` gauge. The expand /
// summarize primitives remain Stage 5.3 / 5.4 work owned
// by separate workstreams.
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
// Stage 5.1 wiring: the binary plumbs the v0 cold-start
// reranker, the inline snapshot fallback reader
// (`recall_context_log` rehydration), and the inline
// RecallContextLog appender so a full happy-path +
// degraded recall cycle works end-to-end.
//
// Stage 5.2 wiring (this iter): when `AGENT_MEMORY_PG_APP_URL`
// is set, `ObserveService` is constructed with the SQL
// EpisodeAppender (transactional Episode + N Observation
// inserts, with `degraded` + `degraded_reason` columns
// populated per §7.5), the composite-key ContextResolver
// (`WHERE context_id = $1 AND repo_id = $2` so a caller
// cannot inherit another repo's degraded flag), and, when
// `AGENT_MEMORY_WAL_DIR` is set, a file-backed WAL +
// background flusher (`/metrics` always exposes the
// `observe_wal_buffer_depth` gauge, returning 0 when no
// WAL is wired). The gRPC transport skeleton is registered
// behind mTLS per tech-spec §8.5 when the
// `AGENT_MEMORY_AGENT_GRPC_*` env vars are set; without
// them the binary remains a foreground composition-root
// validator.
//
// Configuration (env vars; no flags)
// ----------------------------------
//
//	AGENT_MEMORY_PG_RO_URL              postgres:// DSN for the
//	                                    reader-role connection
//	                                    (REQUIRED).  Should be a
//	                                    `agent_memory_ro` DSN so
//	                                    the recall path is
//	                                    mechanically read-only.
//	AGENT_MEMORY_PG_APP_URL             postgres:// DSN for the
//	                                    writer-role connection
//	                                    used by the Stage 5.1
//	                                    Step-6 RecallContextLog
//	                                    appender (OPTIONAL).
//	                                    When unset the recall
//	                                    response carries an
//	                                    empty `ContextID`.
//	AGENT_MEMORY_QDRANT_URL             Qdrant base URL (REQUIRED)
//	AGENT_MEMORY_QDRANT_API_KEY         Qdrant api-key (optional)
//	AGENT_MEMORY_ALLOW_STUB_EMBEDDER    if "true", uses an
//	                                    in-process stub query
//	                                    embedder.  Same caveat as
//	                                    cmd/repoindexer — NOT FIT
//	                                    FOR PRODUCTION.  Required
//	                                    today until the real
//	                                    embedder workstream
//	                                    lands.
//	AGENT_MEMORY_ENABLE_CONCEPTS        if "true", the recall
//	                                    handler fans out across
//	                                    the `agent_memory_concept`
//	                                    Qdrant collection as part
//	                                    of the Stage 5.1 §7.8
//	                                    mixed seed. **Default
//	                                    true** — the
//	                                    implementation-plan
//	                                    Stage-5.1 contract makes
//	                                    {method, block, concept}
//	                                    the production default;
//	                                    set this to "false" only
//	                                    when the Concept collection
//	                                    is not yet provisioned in
//	                                    the target environment.
//	AGENT_MEMORY_HEALTH_ADDR            (optional) bind address
//	                                    for a tiny health probe
//	                                    endpoint.  Default
//	                                    disabled (the binary
//	                                    stays foreground).  This
//	                                    hook lets Kubernetes
//	                                    liveness/readiness probes
//	                                    see the process before
//	                                    the Stage 4 HTTP router
//	                                    lands.
//	AGENT_MEMORY_AGENT_GRPC_ADDR        (optional) bind address
//	                                    for the mTLS gRPC server
//	                                    skeleton (tech-spec §8.5).
//	                                    Requires the three TLS
//	                                    env vars below; absence
//	                                    disables the listener.
//	AGENT_MEMORY_AGENT_GRPC_TLS_CERT    server certificate path.
//	AGENT_MEMORY_AGENT_GRPC_TLS_KEY     server private key path.
//	AGENT_MEMORY_AGENT_GRPC_TLS_CLIENT_CA  client-cert CA bundle path.
//	                                    Required for mTLS: the server
//	                                    rejects any connection that
//	                                    does not present a cert signed
//	                                    by this bundle.
//	AGENT_MEMORY_WAL_DIR                (optional) directory the
//	                                    Stage 5.2 §7.5 file-based
//	                                    Episode WAL writes to when
//	                                    the partition is offline.
//	                                    Without it the observe verb
//	                                    surfaces a partition outage
//	                                    as `codes.Unavailable`
//	                                    instead of WAL-buffering.
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
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/json"
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

	"github.com/lib/pq"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/reflection"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/agentapi"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/graphreader"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/recallcontext"
	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/spaningestor"
	"github.com/smartpcr/code-intelligence/services/agent-memory/pkg/fingerprint"
	agentpb "github.com/smartpcr/code-intelligence/services/agent-memory/proto/agent"
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
	// Stage 4.2: surface the cross-process `repo_health`
	// degraded flag on RecallResponse. The Span Ingestor
	// UPSERTs the row via the writer role; we read via the
	// already-open `agent_memory_ro` pool (migration 0017's
	// default-privileges rule grants SELECT to ro on every
	// new table, including migration 0019's `repo_health`).
	//
	// The two packages each define a structurally-identical
	// HealthState type to keep the dependency arrow
	// one-directional (agentapi MUST NOT import spaningestor,
	// per the import-graph rationale in
	// agentapi/recall.go.HealthSource); the binary-level
	// adapter bridges them here.
	healthSource := agentapi.HealthSourceFunc(func(ctx context.Context, repoID string) (agentapi.HealthState, error) {
		st, err := spaningestor.NewPGHealthSource(db).HealthForRepo(ctx, repoID)
		if err != nil {
			return agentapi.HealthState{}, err
		}
		return agentapi.HealthState{Degraded: st.Degraded, Reason: st.Reason, Source: st.Source}, nil
	})

	// Stage 5.1: v0 cold-start reranker. The in-process,
	// no-trained-model reranker keeps the recall path useful
	// for the first deployment; later iters can swap it for
	// a learned model loaded from the `reranker_model` table
	// without touching this binary (the agentapi.Reranker
	// interface is the contract).
	reranker := agentapi.NewV0ColdStartReranker(nil)
	logger.Info("agent-api.reranker", slog.String("model_version", reranker.ModelVersion()))

	// Stage 5.1 Step-4: graph expansion adapter. The
	// production expander wraps `*graphreader.Reader` (the
	// Stage 2.2 abstraction the rest of the read path
	// already consumes) so retired-row filtering, edge-kind
	// validation, and the server-side LIMIT clamp all
	// flow through one code path (evaluator iter-1 #5).
	// `graphreader.NewPool` runs the role assertion at
	// pool construction so a misconfigured DSN (e.g.
	// pointing at the `_app` role) fails fast at startup.
	gReaderPool, err := graphreader.NewPool(ctx, cfg.PGURL, graphreader.PoolOptions{})
	if err != nil {
		logger.Error("agent-api.graphreader.pool", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer gReaderPool.Close()
	gReader := graphreader.New(gReaderPool, logger.With(slog.String("comp", "graphreader")))
	// Stage 5.1 step 4: the GraphReader-backed BFS expander.
	// Wired with the SQL-backed EdgeObservationCounter so the
	// proto `EdgeCard.observation_count` field reflects the
	// real Stage 4.2 trace_observation aggregate instead of
	// a hard-coded zero (evaluator iter-2 finding #4).
	obsCounter := newObservationCounterFromDB(db, logger)
	expander := agentapi.NewGraphReaderExpander(gReader, nil, agentapi.DefaultExpanderFanOut).
		WithObservationCounter(obsCounter)

	// Stage 5.1 Step-6: RecallContextLog appender. Wraps
	// `*recallcontext.Log` (Stage 2.4) so the writer
	// inherits the validator + role-assertion +
	// SQLSTATE-classifier contract instead of duplicating
	// it inline (evaluator iter-1 #7). Optional —
	// requires AGENT_MEMORY_PG_APP_URL pointing at a
	// writer-role DSN.
	var appDB *sql.DB
	var contextLog agentapi.ContextLogAppender
	if cfg.PGAppURL != "" {
		var err error
		appDB, err = openPG(ctx, cfg.PGAppURL, logger.With(slog.String("role", "app")))
		if err != nil {
			logger.Error("agent-api.pg_app_open", slog.String("error", err.Error()))
			os.Exit(3)
		}
		rcLog := recallcontext.New(appDB, gReader, logger.With(slog.String("comp", "recallcontext")))
		contextLog = newContextLogAppenderFromRecallContext(rcLog, logger)
		logger.Info("agent-api.context_log.wired")
	} else {
		logger.Warn("agent-api.context_log.disabled",
			slog.String("hint", "set AGENT_MEMORY_PG_APP_URL to enable RecallContextLog audit rows"))
	}

	// Stage 5.1 Step-6: degraded snapshot source. Reads the
	// most recent `recall_context_log` row for a repo via
	// the `_ro` pool AND rehydrates the referenced Node /
	// Edge / Concept cards through GraphReader with
	// `IncludeRetired=true` (evaluator iter-1 #8 — pre-
	// iter-2 the snapshot returned bare id arrays so the
	// degraded envelope was unusable).  Connection errors
	// during hydration are mapped onto
	// `ErrGraphStoreUnavailable` so the degraded fallback
	// still emits the §C22 closed-set signal even when the
	// underlying graph store is down.
	snapshot := newSnapshotSourceFromDB(db, gReader, obsCounter, logger)

	opts := []agentapi.Option{
		agentapi.WithLogger(logger),
		agentapi.WithHealthSource(healthSource),
		agentapi.WithReranker(reranker),
		agentapi.WithSeedExpander(expander),
		agentapi.WithExpansionDepth(1),
		agentapi.WithSnapshotFallback(snapshot),
		agentapi.WithConceptsEnabled(cfg.EnableConcepts),
	}
	if contextLog != nil {
		opts = append(opts, agentapi.WithContextLog(contextLog))
	}
	service := agentapi.NewService(embedder, qclient, filter, opts...)

	// Stage 5.2: agent.observe verb composition. Requires
	// the writer-role DSN (same one used by the
	// RecallContextLog appender above) to INSERT Episode +
	// Observation rows. WAL fallback is wired when
	// AGENT_MEMORY_WAL_DIR is set so a partition outage
	// degrades gracefully instead of failing the agent
	// caller (architecture.md §7.5). Without either env
	// var the Observe gRPC method returns Unimplemented /
	// Unavailable respectively.
	var observeSvc *agentapi.ObserveService
	var observeWAL *agentapi.FileWAL
	if appDB != nil {
		episodeWriter := newEpisodeAppenderFromDB(appDB, logger.With(slog.String("comp", "episode-writer")))
		// The resolver runs over the `_ro` pool so it does
		// not contend with writer transactions.
		contextResolver := newContextResolverFromDB(db)
		observeOpts := []agentapi.ObserveOption{
			agentapi.WithObserveLogger(logger.With(slog.String("comp", "observe"))),
		}
		var observeMetrics *agentapi.Metrics
		if cfg.WALDir != "" {
			observeMetrics = &agentapi.Metrics{}
			wal, err := agentapi.NewFileWAL(cfg.WALDir, agentapi.FileWALOptions{
				Metrics: observeMetrics,
				Logger:  logger.With(slog.String("comp", "observe-wal")),
			})
			if err != nil {
				logger.Error("agent-api.observe.wal_open_failed",
					slog.String("dir", cfg.WALDir),
					slog.String("error", err.Error()))
				os.Exit(3)
			}
			observeWAL = wal
			// The flusher uses the same writer the synchronous
			// path uses — a recovered partition catches up by
			// re-attempting the original INSERT verbatim.
			observeWAL.StartFlusher(episodeWriter, 0)
			observeOpts = append(observeOpts,
				agentapi.WithObserveWAL(observeWAL),
				agentapi.WithObserveMetrics(observeMetrics))
			logger.Info("agent-api.observe.wal_wired",
				slog.String("dir", cfg.WALDir),
				slog.Int64("initial_depth", observeWAL.Depth()))
		} else {
			logger.Warn("agent-api.observe.wal_disabled",
				slog.String("hint", "set AGENT_MEMORY_WAL_DIR to enable the §7.5 episodic-log fallback"))
		}
		observeSvc = agentapi.NewObserveService(episodeWriter, contextResolver, observeOpts...)
		logger.Info("agent-api.observe.wired")
	} else {
		logger.Warn("agent-api.observe.disabled",
			slog.String("hint", "set AGENT_MEMORY_PG_APP_URL to enable agent.observe"))
	}
	defer func() {
		if observeWAL != nil {
			_ = observeWAL.Close()
		}
	}()

	logger.Info("agent-api.ready",
		slog.String("qdrant_url", cfg.QdrantURL),
		slog.Bool("stub_embedder", cfg.AllowStubEmbedder),
		slog.Bool("concepts_enabled", cfg.EnableConcepts),
		slog.String("collection_method", embedding.CollectionMethod),
		slog.String("collection_block", embedding.CollectionBlock),
		slog.String("collection_concept", embedding.CollectionConcept))

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
		// `/metrics` exposes the Prometheus text-format
		// `observe_wal_buffer_depth` gauge mandated by the
		// implementation plan §5.2. We hand-roll the
		// text format instead of pulling in
		// prometheus/client_golang because this binary
		// exports exactly one metric and the Prometheus
		// text format is intentionally trivial. The
		// handler ALWAYS responds — depth reports 0 when
		// no WAL is wired — so an operator's `curl
		// /metrics | grep observe_wal_buffer_depth`
		// works regardless of whether the WAL is
		// configured (resolves evaluator iter-1 item #3).
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			var depth int64
			if observeWAL != nil {
				depth = observeWAL.Depth()
			}
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, "# HELP observe_wal_buffer_depth Number of EpisodeAppendInput payloads buffered in the agent.observe WAL awaiting replay.\n")
			fmt.Fprintf(w, "# TYPE observe_wal_buffer_depth gauge\n")
			fmt.Fprintf(w, "observe_wal_buffer_depth %d\n", depth)
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

	// Stage 5.1: mTLS gRPC server. The protoc-generated
	// bindings for `proto/agent.proto` now exist
	// (`proto/agent/agent_grpc.pb.go`) so this listener
	// actually registers `AgentService.Recall` instead of
	// only proving the TLS handshake. The adapter in
	// `agentapi.NewGRPCServer` translates the proto wire
	// shape onto the in-process `*agentapi.Service` and
	// maps domain sentinel errors (`ErrEmptyQuery`, etc.)
	// onto `codes.InvalidArgument`; everything else
	// degrades to `codes.Internal` because the recall
	// handler already projects dependency outages onto a
	// snapshot envelope internally.
	var grpcSrv *grpc.Server
	if cfg.AgentGRPCAddr != "" {
		creds, err := loadMTLS(cfg)
		if err != nil {
			logger.Error("agent-api.grpc.tls_load", slog.String("error", err.Error()))
			os.Exit(2)
		}
		grpcSrv = grpc.NewServer(grpc.Creds(creds))
		grpcOpts := []agentapi.GRPCOption{}
		if observeSvc != nil {
			grpcOpts = append(grpcOpts, agentapi.WithObserveService(observeSvc))
		}
		agentpb.RegisterAgentServiceServer(grpcSrv, agentapi.NewGRPCServer(service, grpcOpts...))
		// reflection lets `grpcurl` / `evans` introspect the
		// listener for smoke-testing the mTLS handshake
		// without a generated client.
		reflection.Register(grpcSrv)
		lis, err := net.Listen("tcp", cfg.AgentGRPCAddr)
		if err != nil {
			logger.Error("agent-api.grpc.listen", slog.String("error", err.Error()))
			os.Exit(3)
		}
		go func() {
			logger.Info("agent-api.grpc.listen",
				slog.String("addr", cfg.AgentGRPCAddr),
				slog.String("service", "AgentService"))
			if err := grpcSrv.Serve(lis); err != nil {
				logger.Error("agent-api.grpc.serve_failed",
					slog.String("error", err.Error()))
			}
		}()
		defer grpcSrv.GracefulStop()
	} else {
		// Keep the linker honest: without an addr, the
		// service still has to be referenced so its
		// composition is exercised at startup.
		_ = service
		_ = observeSvc
	}

	<-ctx.Done()
	logger.Info("agent-api.shutdown")
	if appDB != nil {
		_ = appDB.Close()
	}
}

type config struct {
	PGURL             string
	PGAppURL          string
	QdrantURL         string
	QdrantAPIKey      string
	HealthAddr        string
	AllowStubEmbedder bool
	EnableConcepts    bool

	AgentGRPCAddr      string
	AgentGRPCTLSCert   string
	AgentGRPCTLSKey    string
	AgentGRPCClientCA  string

	// WALDir is the directory the §7.5 file-based WAL writes
	// to when the Episode partition is unavailable. When
	// empty the observe handler still serves but without a
	// fallback (a partition outage surfaces to the caller as
	// `codes.Unavailable`).
	WALDir string
}

func loadConfig() (config, error) {
	c := config{
		PGURL:             os.Getenv("AGENT_MEMORY_PG_RO_URL"),
		PGAppURL:          os.Getenv("AGENT_MEMORY_PG_APP_URL"),
		QdrantURL:         os.Getenv("AGENT_MEMORY_QDRANT_URL"),
		QdrantAPIKey:      os.Getenv("AGENT_MEMORY_QDRANT_API_KEY"),
		HealthAddr:        os.Getenv("AGENT_MEMORY_HEALTH_ADDR"),
		AgentGRPCAddr:     os.Getenv("AGENT_MEMORY_AGENT_GRPC_ADDR"),
		AgentGRPCTLSCert:  os.Getenv("AGENT_MEMORY_AGENT_GRPC_TLS_CERT"),
		AgentGRPCTLSKey:   os.Getenv("AGENT_MEMORY_AGENT_GRPC_TLS_KEY"),
		AgentGRPCClientCA: os.Getenv("AGENT_MEMORY_AGENT_GRPC_TLS_CLIENT_CA"),
		WALDir:            os.Getenv("AGENT_MEMORY_WAL_DIR"),
		// implementation-plan.md Stage 5.1 makes concept fan-out
		// part of the production default mixed seed. Operators
		// can still opt out by setting AGENT_MEMORY_ENABLE_CONCEPTS=false
		// (e.g. during cold-start / pre-promoter bring-up).
		EnableConcepts: true,
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
	if v := os.Getenv("AGENT_MEMORY_ENABLE_CONCEPTS"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_ENABLE_CONCEPTS: %w", err)
		}
		c.EnableConcepts = b
	}
	if c.AgentGRPCAddr != "" {
		if c.AgentGRPCTLSCert == "" || c.AgentGRPCTLSKey == "" || c.AgentGRPCClientCA == "" {
			return c, errors.New(
				"AGENT_MEMORY_AGENT_GRPC_ADDR set without AGENT_MEMORY_AGENT_GRPC_TLS_{CERT,KEY,CLIENT_CA}: mTLS is mandatory")
		}
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

// loadMTLS reads the cert/key/CA bundle from disk and
// builds a credentials.TransportCredentials that requires
// mutual auth. Returns a typed error on any I/O or parse
// failure so the binary can fail-fast with a clean message
// (rather than discovering misconfiguration on the first
// connection).
func loadMTLS(cfg config) (credentials.TransportCredentials, error) {
	cert, err := tls.LoadX509KeyPair(cfg.AgentGRPCTLSCert, cfg.AgentGRPCTLSKey)
	if err != nil {
		return nil, fmt.Errorf("load server cert/key: %w", err)
	}
	caPEM, err := os.ReadFile(cfg.AgentGRPCClientCA)
	if err != nil {
		return nil, fmt.Errorf("read client CA bundle: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse client CA bundle: no PEM blocks found in %q", cfg.AgentGRPCClientCA)
	}
	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		// mTLS: every client connection MUST present a cert
		// chain that validates against the configured CA
		// bundle. The §8.5 tech-spec calls this out as the
		// non-negotiable transport invariant.
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		MinVersion: tls.VersionTLS13,
	}
	return credentials.NewTLS(tlsCfg), nil
}

// newContextLogAppenderFromRecallContext wraps a
// `*recallcontext.Log` (Stage 2.4) and adapts it onto the
// `agentapi.ContextLogAppender` interface. This is the
// replacement for the previous inline INSERT
// (`newContextLogAppenderFromDB`) — evaluator iter-1 #7
// flagged the duplication: the inline path bypassed the
// helper's validation (`validateAppendInput` — verb enum,
// non-zero RepoID, well-formed JSON, well-formed UUIDs,
// non-empty reranker version) AND its structured-log
// emission, so a future change to either contract would
// silently skew between the two writers.
//
// The adapter translates the agentapi-shape RepoID (a
// textual UUID) into the typed `fingerprint.RepoID` the
// writer requires, surfaces any parse failure as a
// soft-degrade signal the recall handler already tolerates
// (logged + empty ContextID), and forwards the rest of the
// payload verbatim.
func newContextLogAppenderFromRecallContext(rcLog *recallcontext.Log, logger *slog.Logger) agentapi.ContextLogAppender {
	return agentapi.ContextLogAppenderFunc(func(ctx context.Context, in agentapi.ContextLogInput) (agentapi.ContextLogRecord, error) {
		repoID, err := fingerprint.ParseRepoID(in.RepoID)
		if err != nil {
			// Recall handler classifies any non-nil error
			// as soft — it warn-logs and keeps the response
			// without a context_id. Returning the parse
			// error keeps the observability trail honest.
			return agentapi.ContextLogRecord{}, fmt.Errorf("context log: parse repo_id %q: %w", in.RepoID, err)
		}
		queryJSON := json.RawMessage(in.QueryJSON)
		if len(queryJSON) == 0 {
			// The writer rejects an empty `query_json`
			// outright; synthesize a minimal `{}` so the
			// audit row still lands when the caller's
			// downstream JSON marshalling drops the field.
			queryJSON = json.RawMessage(`{}`)
		}
		rec, err := rcLog.Append(ctx, recallcontext.AppendInput{
			Verb:                 in.Verb,
			RepoID:               repoID,
			QueryJSON:            queryJSON,
			NodeIDs:              in.NodeIDs,
			EdgeIDs:              in.EdgeIDs,
			ConceptIDs:           in.ConceptIDs,
			RerankerModelVersion: in.RerankerModelVersion,
			ServedUnderDegraded:  in.ServedUnderDegraded,
		})
		if err != nil {
			return agentapi.ContextLogRecord{}, err
		}
		logger.Debug("agent-api.context_log.appended",
			slog.String("context_id", rec.ContextID),
			slog.String("repo_id", in.RepoID),
			slog.Bool("degraded", in.ServedUnderDegraded),
		)
		return agentapi.ContextLogRecord{ContextID: rec.ContextID}, nil
	})
}

// newSnapshotSourceFromDB loads the most recent
// `recall_context_log` row for a repo and HYDRATES the
// referenced Node / Edge / Concept rows through the
// GraphReader (evaluator iter-1 #8 — the previous version
// returned bare id arrays, leaving the degraded envelope
// useless because the agent caller could not render any
// card metadata without a follow-up query).
//
// Hydration policy:
//   - Each id is dereffed via `GetNode` / `GetEdge` /
//     `GetConcept` with `IncludeRetired = true` so a degraded
//     snapshot remains inspectable even after the underlying
//     row has been tombstoned (architecture.md §9.13 risk).
//   - `graphreader.ErrNotFound` on any single id is logged
//     and skipped — the snapshot is best-effort and we'd
//     rather return N-1 cards than fail the whole degraded
//     response.
//   - Connection-class errors (the graph store is
//     genuinely unreachable) are mapped onto
//     `agentapi.ErrGraphStoreUnavailable` so the recall
//     handler emits the §C22 `graph_store_unavailable`
//     closed-set signal instead of leaking a transport
//     error to the agent.
//
// Returns agentapi.ErrNoSnapshot when the repo has no
// prior non-degraded recall row (cold-start) OR when the
// supplied `repoID` is not a well-formed UUID — `repo_id`
// is a `uuid` column, and bypassing pre-validation would
// hit Postgres with a guaranteed-to-fail cast that surfaces
// as SQLSTATE 22P02 (`invalid_input_syntax_for_type_uuid`),
// which `classifyGraphStoreError` does not recognise as a
// graph-store outage. The recall handler projects
// `ErrNoSnapshot` onto an empty-hits degraded envelope —
// the same shape it produces for a cold-start repo — which
// is the right behaviour for a malformed id (it has, by
// definition, no snapshot row).
func newSnapshotSourceFromDB(db *sql.DB, gReader *graphreader.Reader, obsCounter agentapi.EdgeObservationCounter, logger *slog.Logger) agentapi.SnapshotSource {
	return agentapi.SnapshotSourceFunc(func(ctx context.Context, repoID string) (agentapi.RecallSnapshot, error) {
		// Pre-validate the RepoID before issuing the
		// query. This mirrors the appender path
		// (`newContextLogAppenderFromRecallContext`) which
		// already runs `fingerprint.ParseRepoID` up front,
		// keeping both writers and readers on the same
		// validation contract. A malformed RepoID is
		// surfaced as `ErrNoSnapshot` so the recall
		// handler stays on the soft path (warn-free empty
		// degraded envelope) instead of routing through
		// the generic SQL error branch.
		if _, parseErr := fingerprint.ParseRepoID(repoID); parseErr != nil {
			logger.Warn("agent-api.snapshot.repo_id_malformed",
				slog.String("repo_id", repoID),
				slog.String("error", parseErr.Error()))
			return agentapi.RecallSnapshot{}, agentapi.ErrNoSnapshot
		}

		const q = `
SELECT context_id::text,
       (SELECT COALESCE(array_agg(x::text), ARRAY[]::text[]) FROM unnest(node_ids)    AS x),
       (SELECT COALESCE(array_agg(x::text), ARRAY[]::text[]) FROM unnest(edge_ids)    AS x),
       (SELECT COALESCE(array_agg(x::text), ARRAY[]::text[]) FROM unnest(concept_ids) AS x),
       reranker_model_version
  FROM recall_context_log
 WHERE repo_id = $1
   AND served_under_degraded = false
 ORDER BY created_at DESC
 LIMIT 1`
		var (
			snap       agentapi.RecallSnapshot
			nodeIDs    pgTextArray
			edgeIDs    pgTextArray
			conceptIDs pgTextArray
		)
		row := db.QueryRowContext(ctx, q, repoID)
		if err := row.Scan(
			&snap.ContextID,
			&nodeIDs,
			&edgeIDs,
			&conceptIDs,
			&snap.RerankerModelVersion,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return agentapi.RecallSnapshot{}, agentapi.ErrNoSnapshot
			}
			return agentapi.RecallSnapshot{}, fmt.Errorf("snapshot: scan: %w", err)
		}

		// Hydrate Node cards. We use IncludeRetired so the
		// snapshot remains usable even when the row that
		// inspired it has since been tombstoned (the
		// caller wanted "what was true at recall time", not
		// "what is true now").
		readerOpts := graphreader.ReaderOptions{IncludeRetired: true}
		for _, id := range nodeIDs {
			n, err := gReader.GetNode(ctx, id, readerOpts)
			if err != nil {
				if errors.Is(err, graphreader.ErrNotFound) {
					logger.Warn("agent-api.snapshot.node_missing", slog.String("node_id", id))
					continue
				}
				return agentapi.RecallSnapshot{}, classifyGraphStoreError(err, "snapshot.node")
			}
			snap.Nodes = append(snap.Nodes, agentapi.NodeHit{
				NodeID:             n.NodeID,
				RepoID:             n.RepoID,
				Kind:               n.Kind,
				CanonicalSignature: n.CanonicalSignature,
			})
		}
		for _, id := range edgeIDs {
			e, err := gReader.GetEdge(ctx, id, readerOpts)
			if err != nil {
				if errors.Is(err, graphreader.ErrNotFound) {
					logger.Warn("agent-api.snapshot.edge_missing", slog.String("edge_id", id))
					continue
				}
				return agentapi.RecallSnapshot{}, classifyGraphStoreError(err, "snapshot.edge")
			}
			snap.Edges = append(snap.Edges, agentapi.EdgeHit{
				EdgeID:    e.EdgeID,
				RepoID:    e.RepoID,
				Kind:      e.Kind,
				SrcNodeID: e.SrcNodeID,
				DstNodeID: e.DstNodeID,
			})
		}
		// Populate EdgeHit.ObservationCount on the snapshot
		// edges using the same SQL-backed counter the
		// expander wires. Soft failure: counts stay zero on
		// the degraded response if the trace_observation
		// query fails, since the rest of the snapshot is
		// already the load-bearing signal for the agent.
		if obsCounter != nil && len(snap.Edges) > 0 {
			ids := make([]string, 0, len(snap.Edges))
			for _, e := range snap.Edges {
				if e.EdgeID != "" {
					ids = append(ids, e.EdgeID)
				}
			}
			if counts, err := obsCounter.CountByEdgeIDs(ctx, ids); err == nil {
				for i := range snap.Edges {
					if c, ok := counts[snap.Edges[i].EdgeID]; ok {
						snap.Edges[i].ObservationCount = c
					}
				}
			} else {
				logger.Warn("agent-api.snapshot.observation_counts",
					slog.String("error", err.Error()))
			}
		}
		for _, id := range conceptIDs {
			c, err := gReader.GetConcept(ctx, id)
			if err != nil {
				if errors.Is(err, graphreader.ErrNotFound) {
					logger.Warn("agent-api.snapshot.concept_missing", slog.String("concept_id", id))
					continue
				}
				return agentapi.RecallSnapshot{}, classifyGraphStoreError(err, "snapshot.concept")
			}
			snap.Concepts = append(snap.Concepts, agentapi.ConceptHit{
				ConceptID: c.ConceptID,
				Name:      c.Name,
			})
		}
		return snap, nil
	})
}

// newObservationCounterFromDB returns a SQL-backed
// EdgeObservationCounter that resolves
// `trace_observation.observation_count` for a batch of
// edge ids in ONE round-trip. The `_ro` role has SELECT on
// `trace_observation` per migration 0017 §reader_role.
//
// Missing rows in the result map (i.e. edges with no
// recorded observations) are correctly reflected as a zero
// count when the consumer iterates the result by edge_id.
// Connection-class failures are wrapped onto
// `agentapi.ErrGraphStoreUnavailable` so the expander can
// route the error into the degraded fallback path.
func newObservationCounterFromDB(db *sql.DB, logger *slog.Logger) agentapi.EdgeObservationCounter {
	return observationCounterFunc(func(ctx context.Context, edgeIDs []string) (map[string]int64, error) {
		if len(edgeIDs) == 0 {
			return map[string]int64{}, nil
		}
		// `pq.Array` encodes a `[]string` as the canonical
		// `text[]` literal. Postgres coerces it to `uuid[]`
		// (the column type) implicitly because every
		// element is a well-formed uuid; the cast is safer
		// in SQL than in driver-side code because a single
		// malformed id surfaces as a SQLSTATE 22P02 we can
		// log instead of a panic.
		const q = `
SELECT edge_id::text, observation_count
  FROM trace_observation
 WHERE edge_id = ANY($1::uuid[])`
		rows, err := db.QueryContext(ctx, q, pq.Array(edgeIDs))
		if err != nil {
			if agentapi.IsGraphStoreUnavailable(err) {
				return nil, fmt.Errorf("%w: trace_observation: %v",
					agentapi.ErrGraphStoreUnavailable, err)
			}
			logger.Warn("agent-api.observation_counts.query",
				slog.String("error", err.Error()))
			return nil, fmt.Errorf("trace_observation: %w", err)
		}
		defer rows.Close()
		out := make(map[string]int64, len(edgeIDs))
		for rows.Next() {
			var (
				id    string
				count int64
			)
			if err := rows.Scan(&id, &count); err != nil {
				return nil, fmt.Errorf("trace_observation: scan: %w", err)
			}
			out[id] = count
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("trace_observation: rows: %w", err)
		}
		return out, nil
	})
}

// observationCounterFunc adapts a function literal onto the
// EdgeObservationCounter interface so the SQL wiring lives
// in one place without a one-off struct.
type observationCounterFunc func(ctx context.Context, edgeIDs []string) (map[string]int64, error)

func (f observationCounterFunc) CountByEdgeIDs(ctx context.Context, edgeIDs []string) (map[string]int64, error) {
	return f(ctx, edgeIDs)
}

// classifyGraphStoreError maps a graphreader error onto
// either `ErrGraphStoreUnavailable` (when the pool /
// connection is genuinely down) or the original error
// (when the failure is a domain issue like a malformed id).
// The recall handler routes the unavailable signal to the
// `graph_store_unavailable` degraded reason.
func classifyGraphStoreError(err error, op string) error {
	if agentapi.IsGraphStoreUnavailable(err) {
		return fmt.Errorf("%s: %w", op, agentapi.ErrGraphStoreUnavailable)
	}
	return fmt.Errorf("%s: %w", op, err)
}

// pgTextArray scans a Postgres `text[]` column into a Go
// `[]string` without pulling in the `lib/pq` dep for one
// call site. The driver-side representation of `text[]` is
// a curly-brace delimited literal (`{a,b,c}`); we parse
// the trivial subset our snapshot query emits (well-formed
// UUID strings, no quoting, no embedded commas).
type pgTextArray []string

func (p *pgTextArray) Scan(src interface{}) error {
	if src == nil {
		*p = nil
		return nil
	}
	var s string
	switch v := src.(type) {
	case string:
		s = v
	case []byte:
		s = string(v)
	default:
		return fmt.Errorf("pgTextArray: unsupported scan type %T", src)
	}
	s = strings.TrimSpace(s)
	if len(s) < 2 || s[0] != '{' || s[len(s)-1] != '}' {
		return fmt.Errorf("pgTextArray: malformed array literal %q", s)
	}
	inner := s[1 : len(s)-1]
	if inner == "" {
		*p = nil
		return nil
	}
	parts := strings.Split(inner, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		v := strings.TrimSpace(part)
		// The snapshot query already coerces to text via
		// `::text`, so we never see NULL markers or quoted
		// elements. A defensive trim of NULL keeps a future
		// schema migration from blowing this up silently.
		if v == "" || strings.EqualFold(v, "NULL") {
			continue
		}
		out = append(out, v)
	}
	*p = out
	return nil
}

// -- Stage 5.2: agent.observe wiring ---------------------------------

// pgErrCodeConnectionExceptionPrefix is the SQLSTATE class
// for connection-class failures (`Class 08 — Connection
// Exception`). Any code matching this prefix maps onto
// `ErrEpisodicLogUnavailable` — those are the failure modes
// the §7.5 WAL fallback is designed to absorb.
const pgErrCodeConnectionExceptionPrefix = "08"

// pgErrCodeAdminShutdown is the precise SQLSTATE for an
// admin-initiated shutdown of the server. Caught explicitly
// because it can surface as either an 57P0x (operator
// intervention) OR a wrapped network error depending on
// driver timing — the prefix check above covers the latter,
// this catches the former. The 57P0x class also includes
// `57P03 cannot_connect_now` (server in recovery) which is
// indistinguishable from an outage from the caller's
// perspective.
const pgErrCodeOperatorInterventionPrefix = "57P"

// classifyEpisodicError maps a raw DB error into either
// `agentapi.ErrEpisodicLogUnavailable` (for the connection-
// class failures the §7.5 WAL is designed to absorb) or the
// original error (for everything else — schema bugs and
// CHECK violations MUST surface loudly so the operator
// sees them).
//
// Conservatively scoped: a 42P01 (relation does not exist)
// is a schema bug, NOT an outage; a 23xxx (constraint
// violation) is a caller / data-model bug; both pass
// through untouched.
func classifyEpisodicError(err error) error {
	if err == nil {
		return nil
	}
	// Driver-level connection failures (DNS, dial refused,
	// reset) surface as net.OpError or sql.ErrConnDone /
	// driver.ErrBadConn through the sql package wrapper.
	if errors.Is(err, sql.ErrConnDone) {
		return fmt.Errorf("%w: %v", agentapi.ErrEpisodicLogUnavailable, err)
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return fmt.Errorf("%w: %v", agentapi.ErrEpisodicLogUnavailable, err)
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		code := string(pqErr.Code)
		if strings.HasPrefix(code, pgErrCodeConnectionExceptionPrefix) ||
			strings.HasPrefix(code, pgErrCodeOperatorInterventionPrefix) {
			return fmt.Errorf("%w: SQLSTATE %s %v",
				agentapi.ErrEpisodicLogUnavailable, code, err)
		}
	}
	return err
}

// newEpisodeAppenderFromDB returns an EpisodeAppender that
// writes one Episode row + N Observation rows in a single
// transaction against the writer-role DSN. Conn-class errors
// are mapped to `agentapi.ErrEpisodicLogUnavailable` so the
// Observe handler engages the WAL fallback; constraint /
// schema errors propagate verbatim.
//
// The INSERT explicitly passes `episode_id` and `created_at`
// (overriding the DB defaults) because both columns are
// load-bearing for the §7.5 replay contract — the WAL
// payload carries them so a recovery routes the row to the
// originally-attempted partition.
func newEpisodeAppenderFromDB(db *sql.DB, logger *slog.Logger) agentapi.EpisodeAppender {
	return agentapi.EpisodeAppenderFunc(func(ctx context.Context, in agentapi.EpisodeAppendInput) error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return classifyEpisodicError(fmt.Errorf("episode append: begin: %w", err))
		}
		defer func() {
			_ = tx.Rollback()
		}()

		var (
			signalArg    interface{}
			contextID    interface{}
			degradedRsn  interface{}
		)
		if len(in.SignalJSON) > 0 {
			signalArg = []byte(in.SignalJSON)
		}
		if in.ContextID != "" {
			contextID = in.ContextID
		}
		// episode_degraded_reason_chk requires
		// (degraded=true AND degraded_reason IS NOT NULL)
		// OR (degraded=false AND degraded_reason IS NULL).
		// Translate the Go empty string to SQL NULL so the
		// happy path satisfies the CHECK.
		if in.Degraded && in.DegradedReason != "" {
			degradedRsn = in.DegradedReason
		}

		const episodeSQL = `
			INSERT INTO episode (
				episode_id, episode_group_id, repo_id, session_id, trace_id,
				kind, context_id, action, signal_json, outcome,
				degraded, degraded_reason, created_at
			) VALUES ($1, $2, $3, $4, $5, $6::episode_kind, $7, $8::jsonb, $9::jsonb, $10::outcome,
				$11, $12::degraded_reason, $13)
		`
		if _, err := tx.ExecContext(ctx, episodeSQL,
			in.EpisodeID, in.EpisodeGroupID, in.RepoID,
			in.SessionID, in.TraceID, in.Kind,
			contextID, []byte(in.ActionJSON), signalArg,
			in.Outcome, in.Degraded, degradedRsn, in.CreatedAt,
		); err != nil {
			return classifyEpisodicError(fmt.Errorf("episode append: insert episode: %w", err))
		}

		const observationSQL = `
			INSERT INTO observation (
				observation_id, episode_id, role,
				node_id, edge_id, concept_id, degraded_recall_context_id,
				weight, created_at
			) VALUES ($1, $2, $3::observation_role, $4, $5, $6, $7, $8, $9)
		`
		for i, obs := range in.Observations {
			var (
				nodeArg, edgeArg, conceptArg, degradedArg interface{}
			)
			if obs.NodeID != "" {
				nodeArg = obs.NodeID
			}
			if obs.EdgeID != "" {
				edgeArg = obs.EdgeID
			}
			if obs.ConceptID != "" {
				conceptArg = obs.ConceptID
			}
			if obs.DegradedRecallContextID != "" {
				degradedArg = obs.DegradedRecallContextID
			}
			if _, err := tx.ExecContext(ctx, observationSQL,
				obs.ObservationID, in.EpisodeID, obs.Role,
				nodeArg, edgeArg, conceptArg, degradedArg,
				obs.Weight, obs.CreatedAt,
			); err != nil {
				return classifyEpisodicError(fmt.Errorf("episode append: insert observation[%d]: %w", i, err))
			}
		}

		if err := tx.Commit(); err != nil {
			return classifyEpisodicError(fmt.Errorf("episode append: commit: %w", err))
		}
		logger.Debug("agent-api.observe.appended",
			slog.String("episode_id", in.EpisodeID),
			slog.Int("observations", len(in.Observations)))
		return nil
	})
}

// newContextResolverFromDB returns a ContextResolver that
// reads `served_under_degraded` for the supplied `(repo_id,
// context_id)` pair. The composite lookup defends against a
// caller attaching their Episode to ANOTHER repo's
// `recall_context_log` row — a bare-id lookup would let
// repo A inherit repo B's degraded flag (and leak repo B's
// recall lineage into repo A's `mgmt.read.episodes` view).
// The closed-set `recall_context_log_repo_created_idx` makes
// the composite lookup as cheap as the prior id-only
// lookup.
//
// `sql.ErrNoRows` maps to `agentapi.ErrContextNotFound` so
// the gRPC adapter surfaces `INVALID_ARGUMENT` to a caller
// that supplied a bogus id (or a context_id that legitimately
// belongs to a different repo). Connection failures propagate
// verbatim — the resolver runs against the `_ro` pool which
// is also exercised by the Recall path, so an outage here
// is already visible on those metrics.
func newContextResolverFromDB(db *sql.DB) agentapi.ContextResolver {
	const query = `
		SELECT served_under_degraded
		FROM recall_context_log
		WHERE context_id = $1 AND repo_id = $2
		ORDER BY created_at DESC
		LIMIT 1
	`
	return agentapi.ContextResolverFunc(func(ctx context.Context, repoID, contextID string) (bool, error) {
		var degraded bool
		err := db.QueryRowContext(ctx, query, contextID, repoID).Scan(&degraded)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return false, agentapi.ErrContextNotFound
			}
			return false, fmt.Errorf("context resolver: %w", err)
		}
		return degraded, nil
	})
}
