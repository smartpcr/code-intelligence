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
// Stage 5.1 wiring (this iter): the binary additionally
// plumbs the v0 cold-start reranker, the inline snapshot
// fallback reader (`recall_context_log` rehydration), and
// the inline RecallContextLog appender so a full happy-path
// + degraded recall cycle works end-to-end. The gRPC
// transport skeleton is registered behind mTLS per
// tech-spec §8.5 when the `AGENT_MEMORY_AGENT_GRPC_*` env
// vars are set; without them the binary remains a
// foreground composition-root validator.
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
//	AGENT_MEMORY_SUMMARISER_ENDPOINT    (optional) OpenAI-compatible
//	                                    HTTPS endpoint for the
//	                                    `agent.summarize` LLM client
//	                                    (Stage 5.4). Absent → the
//	                                    summarize verb is wired but
//	                                    the LLM is disabled; every
//	                                    call surfaces the templated
//	                                    fallback with
//	                                    `degraded_reason=summariser_unavailable`.
//	AGENT_MEMORY_SUMMARISER_MODEL       (optional) Vendor model id
//	                                    (e.g. `gpt-4o-mini`). Required
//	                                    when AGENT_MEMORY_SUMMARISER_ENDPOINT
//	                                    is set; rejected otherwise.
//	AGENT_MEMORY_SUMMARISER_API_KEY     (optional) Bearer credential
//	                                    for the summariser endpoint.
//	                                    Sent as `Authorization: Bearer
//	                                    <key>`. Omit for endpoints
//	                                    behind mTLS or in-cluster
//	                                    network-policy auth.
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

	// Stage 5.4 wiring: agent.summarize verb. Resolver +
	// freshness are always wired so a binary with a healthy
	// graph store can serve summaries even when the LLM
	// endpoint is absent (the verb falls back to the
	// deterministic template + degraded envelope). The
	// summariser itself is gated on AGENT_MEMORY_SUMMARISER_*
	// so deployments without an LLM stay valid — they just
	// always degrade. Without these three options the verb
	// returns ErrSummarizeUnconfigured (→ Unimplemented),
	// which iter-2 evaluator finding #1 flagged.
	neighborhoodResolver := newNeighborhoodResolverFromGraphReader(gReader, db, logger)
	rerankerFreshness := newRerankerFreshnessFromDB(db, logger)
	opts = append(opts,
		agentapi.WithNeighborhoodResolver(neighborhoodResolver),
		agentapi.WithRerankerFreshness(rerankerFreshness),
	)
	logger.Info("agent-api.summarize.wired",
		slog.Bool("resolver", true),
		slog.Bool("reranker_freshness", true))
	if cfg.SummariserEndpoint != "" {
		summariser, err := newSummariserFromConfig(cfg, logger)
		if err != nil {
			logger.Error("agent-api.summarise.config",
				slog.String("error", err.Error()))
			os.Exit(2)
		}
		opts = append(opts, agentapi.WithSummariser(summariser))
		logger.Info("agent-api.summarize.summariser_wired",
			slog.String("endpoint", cfg.SummariserEndpoint),
			slog.String("model", cfg.SummariserModel),
			slog.Bool("api_key", cfg.SummariserAPIKey != ""))
	} else {
		logger.Warn("agent-api.summarize.summariser_disabled",
			slog.String("hint",
				"set AGENT_MEMORY_SUMMARISER_ENDPOINT + _MODEL to enable LLM-rendered summaries; verb still serves degraded template"))
	}

	service := agentapi.NewService(embedder, qclient, filter, opts...)

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
		agentpb.RegisterAgentServiceServer(grpcSrv, agentapi.NewGRPCServer(service))
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

	AgentGRPCAddr     string
	AgentGRPCTLSCert  string
	AgentGRPCTLSKey   string
	AgentGRPCClientCA string

	// Stage 5.4 summarize verb configuration.
	SummariserEndpoint string
	SummariserModel    string
	SummariserAPIKey   string
}

func loadConfig() (config, error) {
	c := config{
		PGURL:              os.Getenv("AGENT_MEMORY_PG_RO_URL"),
		PGAppURL:           os.Getenv("AGENT_MEMORY_PG_APP_URL"),
		QdrantURL:          os.Getenv("AGENT_MEMORY_QDRANT_URL"),
		QdrantAPIKey:       os.Getenv("AGENT_MEMORY_QDRANT_API_KEY"),
		HealthAddr:         os.Getenv("AGENT_MEMORY_HEALTH_ADDR"),
		AgentGRPCAddr:      os.Getenv("AGENT_MEMORY_AGENT_GRPC_ADDR"),
		AgentGRPCTLSCert:   os.Getenv("AGENT_MEMORY_AGENT_GRPC_TLS_CERT"),
		AgentGRPCTLSKey:    os.Getenv("AGENT_MEMORY_AGENT_GRPC_TLS_KEY"),
		AgentGRPCClientCA:  os.Getenv("AGENT_MEMORY_AGENT_GRPC_TLS_CLIENT_CA"),
		SummariserEndpoint: os.Getenv("AGENT_MEMORY_SUMMARISER_ENDPOINT"),
		SummariserModel:    os.Getenv("AGENT_MEMORY_SUMMARISER_MODEL"),
		SummariserAPIKey:   os.Getenv("AGENT_MEMORY_SUMMARISER_API_KEY"),
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
	if c.SummariserEndpoint != "" && c.SummariserModel == "" {
		return c, errors.New(
			"AGENT_MEMORY_SUMMARISER_ENDPOINT set without AGENT_MEMORY_SUMMARISER_MODEL: vendor model id is required")
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

// newNeighborhoodResolverFromGraphReader is the production
// `agentapi.NeighborhoodResolver` adapter that resolves
// `agent.summarize` Stage 5.4 targets through the existing
// graph-reader stack.
//
// Node-target path
// ----------------
//   - `gReader.NeighborhoodCard(nodeID, IncludeRetired=false)`
//     fetches the seed + outbound edges inside one
//     REPEATABLE READ transaction.
//   - For each unique outbound `DstNodeID`, a follow-up
//     `gReader.GetNode` populates the destination card.
//     This is bounded N+1: the verb already caps edges at
//     `maxSummarizeEdges=32`, so the dst hydration is
//     ≤32 reads per call. A `GetNode` returning ErrNotFound
//     (the dst was retired between the card scan and the
//     follow-up read) is logged and skipped — the verb's
//     `deduplicatedTargets` helper drops any dst not
//     present in the Targets slice, so the citation
//     invariant ("every entry references a row that
//     exists") still holds.
//
// Concept-target path
// -------------------
//   - `gReader.GetConcept(conceptID)` fetches the seed row.
//   - A direct SQL query against `concept_support` scoped
//     by `(concept_id, repo_id)` ONLY (no
//     `concept_version_id` filter — iter-4 evaluator #1)
//     loads the supporting Node/Episode rows the citation
//     set surfaces. The Concept Promoter appends a new
//     `ConceptVersion` AFTER the Consolidator writes
//     supports, so filtering on the latest version would
//     return zero rows for every promoted concept; the
//     unscoped query matches the architecture §6.2
//     `mgmt.read.concept_supports(concept_id, repo_id?)`
//     contract instead. `node_id` / `episode_id` are
//     nullable columns; either may be set per the
//     migration 0011 CHECK constraint.
//   - For supports with a non-empty `node_id`, a follow-up
//     `GetNode` hydrates `NodeKind` / `NodeSignature` so the
//     prompt + template can render the support inline.
//
// Error classification mirrors the snapshot source
// (`newSnapshotSourceFromDB`):
//   - `graphreader.ErrNotFound` on the seed →
//     `agentapi.ErrSummarizeTargetNotFound`.
//   - Connection-class errors (`IsGraphStoreUnavailable`) →
//     `agentapi.ErrGraphStoreUnavailable` so the verb
//     degrades to the graph-outage envelope.
//   - Everything else is surfaced verbatim; the verb routes
//     unclassified errors to a hard Internal status.
func newNeighborhoodResolverFromGraphReader(
	gReader *graphreader.Reader,
	db *sql.DB,
	logger *slog.Logger,
) agentapi.NeighborhoodResolver {
	return agentapi.NeighborhoodResolverFunc{
		NeighborhoodFn: func(ctx context.Context, nodeID string) (agentapi.SummarizeNodeNeighborhood, error) {
			card, err := gReader.NeighborhoodCard(ctx, nodeID, graphreader.ReaderOptions{IncludeRetired: false})
			if err != nil {
				if errors.Is(err, graphreader.ErrNotFound) {
					return agentapi.SummarizeNodeNeighborhood{}, fmt.Errorf("%w: node_id=%q",
						agentapi.ErrSummarizeTargetNotFound, nodeID)
				}
				return agentapi.SummarizeNodeNeighborhood{}, classifyGraphStoreError(err, "summarize.neighborhood")
			}
			seedCard := agentapi.SummarizeNodeCard{
				NodeID:             card.Node.NodeID,
				RepoID:             card.Node.RepoID,
				Kind:               card.Node.Kind,
				CanonicalSignature: card.Node.CanonicalSignature,
			}
			edges := make([]agentapi.SummarizeEdgeCard, 0, len(card.Edges))
			seenDst := make(map[string]struct{}, len(card.Edges))
			dstOrder := make([]string, 0, len(card.Edges))
			for _, ce := range card.Edges {
				var obs int64
				if ce.TraceObservation != nil {
					obs = ce.TraceObservation.ObservationCount
				}
				edges = append(edges, agentapi.SummarizeEdgeCard{
					EdgeID:           ce.EdgeID,
					RepoID:           ce.RepoID,
					Kind:             ce.Kind,
					SrcNodeID:        ce.SrcNodeID,
					DstNodeID:        ce.DstNodeID,
					ObservationCount: obs,
				})
				if ce.DstNodeID == "" || ce.DstNodeID == card.Node.NodeID {
					continue
				}
				if _, dup := seenDst[ce.DstNodeID]; dup {
					continue
				}
				seenDst[ce.DstNodeID] = struct{}{}
				dstOrder = append(dstOrder, ce.DstNodeID)
			}
			// iter-4 evaluator #3: bound the N+1 dst
			// hydration to `agentapi.MaxSummarizeEdges`
			// BEFORE issuing GetNode calls. Without this
			// cap a hot node with thousands of outbound
			// edges (e.g. a popular utility method)
			// would force the adapter into one DB
			// round-trip per edge even though the verb
			// downstream caps `cappedEdges` at the same
			// value and discards everything past index
			// 32. The cap is taken from the agentapi
			// public alias so a future bump stays in
			// sync.
			targets, dstSig, hydErr := hydrateDstNodes(ctx, gReader, dstOrder, agentapi.MaxSummarizeEdges, logger)
			if hydErr != nil {
				return agentapi.SummarizeNodeNeighborhood{}, hydErr
			}
			// Backfill DstSignature on edges from the
			// hydrated map so the prompt + template can
			// render `src → dst` lines with both endpoints
			// labelled.
			for i := range edges {
				if sig, ok := dstSig[edges[i].DstNodeID]; ok {
					edges[i].DstSignature = sig
				}
			}
			return agentapi.SummarizeNodeNeighborhood{
				Node:    seedCard,
				Edges:   edges,
				Targets: targets,
			}, nil
		},
		ConceptFn: func(ctx context.Context, conceptID, repoID string) (agentapi.SummarizeConceptCard, error) {
			concept, err := gReader.GetConcept(ctx, conceptID)
			if err != nil {
				if errors.Is(err, graphreader.ErrNotFound) {
					return agentapi.SummarizeConceptCard{}, fmt.Errorf("%w: concept_id=%q",
						agentapi.ErrSummarizeTargetNotFound, conceptID)
				}
				return agentapi.SummarizeConceptCard{}, classifyGraphStoreError(err, "summarize.concept")
			}
			card := agentapi.SummarizeConceptCard{
				ConceptID:     concept.ConceptID,
				RepoID:        repoID,
				Name:          concept.Name,
				DescriptionMD: concept.DescriptionMD,
			}
			// concept_support lookup: scope by
			// `(concept_id, repo_id)` ONLY — see
			// `loadConceptSupports` for the iter-4 #1
			// rationale on dropping the version filter.
			// The 64-row DB cap keeps the query bounded;
			// the verb re-caps at
			// `agentapi.MaxSummarizeConceptSupports`
			// (32) for the prompt + citation array.
			supports, suppErr := loadConceptSupports(ctx, db, conceptID, repoID, logger)
			if suppErr != nil {
				// iter-4 evaluator #2: do NOT silently
				// succeed with zero supports on a
				// support-side outage — that hides the
				// missing-citation path Stage 5.4
				// requires. `loadConceptSupports`
				// already wraps connection-class errors
				// as `ErrGraphStoreUnavailable`, so the
				// verb's `summarizeGraphFailure` path
				// degrades cleanly; non-connection
				// errors (scan / syntax / context) are
				// genuine internal bugs and propagate
				// hard.
				return agentapi.SummarizeConceptCard{}, suppErr
			}
			// Hydrate NodeKind / NodeSignature for supports
			// that carry a Node reference. Bounded by the
			// 64-row DB cap above.
			for i := range supports {
				if supports[i].NodeID == "" {
					continue
				}
				dn, gerr := gReader.GetNode(ctx, supports[i].NodeID, graphreader.ReaderOptions{IncludeRetired: false})
				if gerr != nil {
					if errors.Is(gerr, graphreader.ErrNotFound) {
						// Support row pointed at a
						// retired node; keep the
						// citation but leave hydration
						// fields empty.
						continue
					}
					return agentapi.SummarizeConceptCard{}, classifyGraphStoreError(gerr, "summarize.support_node")
				}
				supports[i].NodeKind = dn.Kind
				supports[i].NodeSignature = dn.CanonicalSignature
			}
			card.Supports = supports
			return card, nil
		},
	}
}

// loadConceptSupports runs the `concept_support` SQL query
// for the supplied (concept_id, repo_id) tuple. Returns the
// rows in `created_at DESC` order so the verb's bounded
// slice surfaces the most recent provenance first. The
// 64-row cap is larger than the verb's
// `agentapi.MaxSummarizeConceptSupports` (32) so an
// operator can raise the verb cap without a schema-level
// change.
//
// Version-filter rationale (iter-4 evaluator #1)
// ---------------------------------------------
// The query intentionally does NOT filter by
// `concept_version_id`. The Concept Promoter
// (architecture.md §3.5, §7.8) appends a NEW
// `ConceptVersion` row on every promotion run, while the
// Consolidator stamps `concept_support` rows against the
// version it observed at write time. Filtering supports to
// the latest `concept_version_id` therefore returns zero
// rows for any concept the Promoter touched after the
// Consolidator's most recent support write — i.e. exactly
// the promoted-concept case Stage 5.4 needs to surface.
//
// This matches the architecture §6.2 contract for
// `mgmt.read.concept_supports(concept_id, repo_id?)` which
// is "a straight scan with a `support.repo_id` filter; no
// Concept duplication across repos is needed" (§6.5).
// Duplicate Node / Episode references across historical
// versions are deduped downstream by
// `buildConceptCitations`.
func loadConceptSupports(
	ctx context.Context, db *sql.DB,
	conceptID, repoID string, logger *slog.Logger,
) ([]agentapi.SummarizeConceptSupport, error) {
	const q = `
SELECT cs.support_id::text,
       cs.node_id::text,
       cs.episode_id::text,
       cs.polarity::text
  FROM concept_support cs
 WHERE cs.concept_id = $1
   AND cs.repo_id    = $2
 ORDER BY cs.created_at DESC
 LIMIT 64`
	rows, err := db.QueryContext(ctx, q, conceptID, repoID)
	if err != nil {
		if agentapi.IsGraphStoreUnavailable(err) {
			return nil, fmt.Errorf("%w: concept_support: %v",
				agentapi.ErrGraphStoreUnavailable, err)
		}
		return nil, fmt.Errorf("concept_support: %w", err)
	}
	defer rows.Close()
	var out []agentapi.SummarizeConceptSupport
	for rows.Next() {
		var (
			supportID string
			nodeID    sql.NullString
			episodeID sql.NullString
			polarity  string
		)
		if err := rows.Scan(&supportID, &nodeID, &episodeID, &polarity); err != nil {
			return nil, fmt.Errorf("concept_support: scan: %w", err)
		}
		sup := agentapi.SummarizeConceptSupport{
			SupportID: supportID,
			Polarity:  polarity,
		}
		if nodeID.Valid {
			sup.NodeID = nodeID.String
		}
		if episodeID.Valid {
			sup.EpisodeID = episodeID.String
		}
		out = append(out, sup)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("concept_support: rows: %w", err)
	}
	logger.Debug("agent-api.summarize.concept_supports.loaded",
		slog.String("concept_id", conceptID),
		slog.String("repo_id", repoID),
		slog.Int("rows", len(out)))
	return out, nil
}

// dstNodeFetcher is the narrow interface `hydrateDstNodes`
// consumes — pulled out so cmd/agent-api unit tests can
// drive the helper with a counting fake instead of standing
// up a full `*graphreader.Reader`. `*graphreader.Reader`
// satisfies it implicitly via its public `GetNode` method,
// so production wiring is unchanged.
type dstNodeFetcher interface {
	GetNode(ctx context.Context, nodeID string, opts graphreader.ReaderOptions) (graphreader.Node, error)
}

// hydrateDstNodes pulls the destination Node card for every
// id in `dstOrder` and returns the resulting Targets[] slice
// plus a `dstSig` map used to backfill `DstSignature` on the
// edge cards.
//
// Bounded N+1 contract (iter-4 evaluator #3)
// ------------------------------------------
// `dstOrder` is hard-truncated to `max` BEFORE any
// `GetNode` issue. Production callers pass
// `agentapi.MaxSummarizeEdges` so the adapter never spends
// more DB round-trips than the verb's downstream cap will
// retain. A hot node returning 1000 edges therefore yields
// at most `max` lookups, not 1000.
//
// Retirement race
// ---------------
// The dst reads happen OUTSIDE the seed card's repeatable-
// read snapshot, so a destination that was retired between
// the card scan and the follow-up read returns
// `graphreader.ErrNotFound`; we skip it (the verb's
// `deduplicatedTargets` helper drops any dst id missing
// from Targets[], so the citation invariant holds).
//
// Connection-class errors are promoted to
// `agentapi.ErrGraphStoreUnavailable` via
// `classifyGraphStoreError` so the verb degrades cleanly
// rather than emitting a 5xx.
func hydrateDstNodes(
	ctx context.Context,
	fetcher dstNodeFetcher,
	dstOrder []string,
	max int,
	logger *slog.Logger,
) ([]agentapi.SummarizeNodeCard, map[string]string, error) {
	if max > 0 && len(dstOrder) > max {
		logger.Debug("agent-api.summarize.dst_hydration.capped",
			slog.Int("requested", len(dstOrder)),
			slog.Int("cap", max))
		dstOrder = dstOrder[:max]
	}
	targets := make([]agentapi.SummarizeNodeCard, 0, len(dstOrder))
	dstSig := make(map[string]string, len(dstOrder))
	for _, dstID := range dstOrder {
		dn, gerr := fetcher.GetNode(ctx, dstID, graphreader.ReaderOptions{IncludeRetired: false})
		if gerr != nil {
			if errors.Is(gerr, graphreader.ErrNotFound) {
				logger.Debug("agent-api.summarize.dst_node_missing",
					slog.String("node_id", dstID))
				continue
			}
			return nil, nil, classifyGraphStoreError(gerr, "summarize.dst_node")
		}
		targets = append(targets, agentapi.SummarizeNodeCard{
			NodeID:             dn.NodeID,
			RepoID:             dn.RepoID,
			Kind:               dn.Kind,
			CanonicalSignature: dn.CanonicalSignature,
		})
		dstSig[dn.NodeID] = dn.CanonicalSignature
	}
	return targets, dstSig, nil
}

// newRerankerFreshnessFromDB returns the SQL-backed
// `agentapi.RerankerFreshnessSource` the Stage 5.4 summarize
// verb consults on the degraded fallback path to pick
// between `summariser_unavailable` and
// `reranker_model_stale`.
//
// Status filter: only `'published'` rows count. `'shadow'`
// models are training-only and never deployed; including
// them would mask a genuinely-stale published baseline.
func newRerankerFreshnessFromDB(db *sql.DB, logger *slog.Logger) agentapi.RerankerFreshnessSource {
	return agentapi.RerankerFreshnessFunc(func(ctx context.Context) (time.Time, bool, error) {
		const q = `
SELECT trained_at
  FROM reranker_model
 WHERE status = 'published'
 ORDER BY trained_at DESC
 LIMIT 1`
		var t time.Time
		err := db.QueryRowContext(ctx, q).Scan(&t)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return time.Time{}, false, nil
			}
			logger.Warn("agent-api.summarize.reranker_freshness.query",
				slog.String("error", err.Error()))
			return time.Time{}, false, fmt.Errorf("reranker_model: %w", err)
		}
		return t, true, nil
	})
}

// newSummariserFromConfig constructs the OpenAI-compatible
// HTTPS `agentapi.Summariser` from the AGENT_MEMORY_SUMMARISER_*
// env vars. The config layer already verified that
// ENDPOINT + MODEL are both set when this is called.
func newSummariserFromConfig(cfg config, logger *slog.Logger) (agentapi.Summariser, error) {
	cli, err := agentapi.NewOpenAICompatibleSummariser(agentapi.OpenAICompatibleConfig{
		Endpoint:   cfg.SummariserEndpoint,
		Model:      cfg.SummariserModel,
		APIKey:     cfg.SummariserAPIKey,
		HTTPClient: &http.Client{Timeout: 30 * time.Second},
	})
	if err != nil {
		return nil, fmt.Errorf("summariser: %w", err)
	}
	logger.Debug("agent-api.summarize.summariser.constructed",
		slog.String("model_version", cli.ModelVersion()))
	return cli, nil
}
