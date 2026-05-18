// Command mgmt-api is the long-running process that serves
// the Stage 7.1 Onboarding write verbs of the Management
// Surface (architecture.md §3.8 / §6.2.1; tech-spec.md §8.5):
//
//	POST /v1/repos                       mgmt.register
//	POST /v1/repos/{repo_id}/ingest      mgmt.ingest
//	POST /v1/repos/{repo_id}/ingest_delta mgmt.ingest_delta
//	POST /v1/spans                       mgmt.ingest_spans (Stage 7.2)
//
// It also exposes operator endpoints:
//
//	GET  /healthz                        liveness probe
//	GET  /metrics                        Prometheus text-format
//	                                     exposition of the Stage
//	                                     7.2 mandated
//	                                     `mgmt_ingest_spans_total{repo_id,status}`
//	                                     counter.
//
// The handler itself lives in internal/mgmtapi; this binary is
// the composition root that wires up PostgreSQL, TLS, the OIDC
// token verifier, the HEAD resolver, signal handling, and the
// HTTP mux. The shape mirrors cmd/webhook-receiver so all
// agent-memory binaries deploy through the same helm chart
// skeleton.
//
// AuthN
// -----
// The verifier the binary uses is selected at boot:
//
//   - If AGENT_MEMORY_OIDC_ISSUER, AGENT_MEMORY_OIDC_AUDIENCE
//     and AGENT_MEMORY_OIDC_JWKS_URL are all set, the binary
//     uses [mgmtapi.OIDCVerifier] — a real JWKS-backed RSA
//     signature verifier with full iss / aud / exp / nbf /
//     sub claim validation. This is the production setting.
//
//   - Otherwise, if AGENT_MEMORY_OIDC_DEV_TOKEN is set, the
//     binary uses [mgmtapi.StaticBearerVerifier] — a single
//     shared-secret token. This is the development /
//     docker-compose setting. The binary logs a WARN line
//     at boot when this branch is taken.
//
//   - If neither is set, the binary refuses to start (exit 2).
//
// HEAD resolution
// ---------------
// The resolver the binary uses is also selected at boot:
//
//   - AGENT_MEMORY_HEAD_RESOLVER=git-ls-remote (default):
//     [mgmtapi.GitLsRemoteResolver] — invokes the local
//     `git` binary against the operator-supplied repo_url.
//     This is the production setting.
//
//   - AGENT_MEMORY_HEAD_RESOLVER=static: uses
//     [mgmtapi.StaticHeadResolver] with the SHA from
//     AGENT_MEMORY_HEAD_RESOLVER_STATIC_SHA. For docker-
//     compose / unit-test deployments where a real remote
//     is unreachable.
//
// Configuration (env vars; no flags)
// ----------------------------------
//
//	AGENT_MEMORY_PG_URL                          postgres:// DSN (REQUIRED).
//	                                             Should be the
//	                                             `agent_memory_app`
//	                                             DSN so the write
//	                                             surface holds
//	                                             INSERT/UPDATE on
//	                                             repo / repo_event /
//	                                             repo_webhook_secret
//	                                             / ingest_jobs.
//	AGENT_MEMORY_LISTEN_ADDR                     bind address
//	                                             (default `:8444`).
//	AGENT_MEMORY_TLS_CERT_FILE                   PEM server cert.
//	AGENT_MEMORY_TLS_KEY_FILE                    PEM server key.
//	                                             Both REQUIRED unless
//	                                             ALLOW_PLAINTEXT is set.
//	AGENT_MEMORY_ALLOW_PLAINTEXT                 if "true", serves plain
//	                                             HTTP. Dev only — the
//	                                             webhook secret returned
//	                                             on register travels
//	                                             over the wire in
//	                                             cleartext otherwise.
//	AGENT_MEMORY_OIDC_ISSUER                     expected `iss` claim
//	                                             (REQUIRED for prod
//	                                             OIDC mode).
//	AGENT_MEMORY_OIDC_AUDIENCE                   expected `aud` claim
//	                                             (REQUIRED for prod
//	                                             OIDC mode).
//	AGENT_MEMORY_OIDC_JWKS_URL                   JWKS document URL
//	                                             (REQUIRED for prod
//	                                             OIDC mode).
//	AGENT_MEMORY_OIDC_LEEWAY                     clock-skew tolerance
//	                                             for exp/nbf checks
//	                                             (default 0s).
//	AGENT_MEMORY_OIDC_JWKS_TTL                   JWKS cache TTL
//	                                             (default 5m).
//	AGENT_MEMORY_OIDC_DEV_TOKEN                  the dev/local shared
//	                                             bearer token (only
//	                                             when no OIDC trio is
//	                                             configured).
//	AGENT_MEMORY_OIDC_DEV_SUBJECT                opaque operator id
//	                                             returned in the audit
//	                                             log for a successful
//	                                             dev-token call.
//	                                             Default `dev-operator`.
//	AGENT_MEMORY_HEAD_RESOLVER                   `git-ls-remote`
//	                                             (default) or
//	                                             `static`.
//	AGENT_MEMORY_HEAD_RESOLVER_GIT_PATH          absolute path to the
//	                                             git binary (default:
//	                                             PATH lookup at exec
//	                                             time).
//	AGENT_MEMORY_HEAD_RESOLVER_TIMEOUT           per-call resolver
//	                                             timeout (default
//	                                             15s).
//	AGENT_MEMORY_HEAD_RESOLVER_STATIC_SHA        40- or 64-char
//	                                             lower-case hex SHA
//	                                             returned by the
//	                                             static HEAD resolver.
//	                                             REQUIRED when
//	                                             AGENT_MEMORY_HEAD_RESOLVER=static.
//	AGENT_MEMORY_READ_TIMEOUT                    per-request read
//	                                             timeout (default 30s).
//	AGENT_MEMORY_WRITE_TIMEOUT                   per-request write
//	                                             timeout (default 30s).
//	AGENT_MEMORY_SHUTDOWN_TIMEOUT                graceful-shutdown
//	                                             budget (default 30s).
//	AGENT_MEMORY_SPAN_FORWARD_URL                OTLP/HTTP endpoint
//	                                             the `mgmt.ingest_spans`
//	                                             verb forwards
//	                                             validated batches
//	                                             to (e.g.
//	                                             `https://span-ingestor:4318/v1/traces`).
//	                                             When unset, the
//	                                             verb returns 503
//	                                             `forwarder_not_configured`
//	                                             on every call -- a
//	                                             deliberate fail-
//	                                             CLOSED so a
//	                                             half-deployed
//	                                             mgmt-api never
//	                                             silently drops
//	                                             spans.
//	AGENT_MEMORY_SPAN_FORWARD_TIMEOUT            per-forward
//	                                             deadline (default
//	                                             10s).
//	AGENT_MEMORY_SPAN_SERVICE_MAP                comma-separated
//	                                             `service.name=repo_uuid`
//	                                             pairs the
//	                                             `mgmt.ingest_spans`
//	                                             verb uses to map
//	                                             OTel `service.name`
//	                                             attributes to
//	                                             `repo_id`s
//	                                             (example:
//	                                             `worker-a=550e8400-...,worker-b=6ba7b810-...`).
//	                                             Unmapped names
//	                                             cause the verb to
//	                                             count
//	                                             `mgmt_ingest_spans_total{status="unknown_service"}`
//	                                             and reject 400.
//	                                             Empty map => every
//	                                             span is rejected;
//	                                             use the empty
//	                                             config only for
//	                                             smoke tests.
//
// Exit codes
// ----------
//
//	0  graceful shutdown (SIGINT/SIGTERM)
//	2  configuration error (missing required env, malformed
//	                        DSN, TLS files unreadable)
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
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/mgmtapi"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)

	cfg, err := loadConfig()
	if err != nil {
		logger.Error("mgmt-api.config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(),
		os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := openPG(ctx, cfg, logger)
	if err != nil {
		logger.Error("mgmt-api.pg", slog.String("error", err.Error()))
		os.Exit(3)
	}
	defer func() { _ = db.Close() }()

	verifier, err := buildVerifier(cfg, logger)
	if err != nil {
		logger.Error("mgmt-api.verifier", slog.String("error", err.Error()))
		os.Exit(2)
	}
	resolver, err := buildResolver(cfg, logger)
	if err != nil {
		logger.Error("mgmt-api.resolver", slog.String("error", err.Error()))
		os.Exit(2)
	}
	spanForwarder := buildSpanForwarder(cfg, logger)
	spanMetrics := mgmtapi.NewDefaultSpanMetrics()
	handler := mgmtapi.NewHandler(db, verifier, resolver, mgmtapi.Options{
		Logger:        logger,
		SpanForwarder: spanForwarder,
		SpanMetrics:   spanMetrics,
		SpanLookup:    staticSpanLookup(cfg.SpanServiceMap),
	})

	mux := http.NewServeMux()
	mux.Handle("/v1/repos", handler)
	mux.Handle("/v1/repos/", handler)
	mux.Handle("/v1/spans", handler)
	mux.Handle("/v1/spans/", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// `/metrics` exposes the Stage 7.2 mandated
	// `mgmt_ingest_spans_total{repo_id, status}` counter in
	// the Prometheus text-format. We hand-roll the format
	// (matching the existing `cmd/agent-api/main.go`
	// pattern) instead of importing prometheus/client_golang
	// because this binary exposes a single counter family
	// and the text format is intentionally trivial. HELP
	// and TYPE are always emitted so the scrape works on a
	// freshly-started binary before any traffic.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := spanMetrics.WritePrometheus(w); err != nil {
			logger.Warn("mgmt-api.metrics.write_failed",
				slog.String("error", err.Error()))
		}
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		TLSConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	serveErr := make(chan error, 1)
	go func() {
		if cfg.AllowPlaintext {
			logger.Warn("mgmt-api.plaintext_listen",
				slog.String("addr", cfg.ListenAddr),
				slog.String("warning",
					"serving plain HTTP; bearer tokens AND webhook secret travel unencrypted -- NOT FIT FOR PRODUCTION"),
			)
			serveErr <- srv.ListenAndServe()
			return
		}
		logger.Info("mgmt-api.tls_listen",
			slog.String("addr", cfg.ListenAddr),
			slog.String("cert_file", cfg.TLSCertFile),
		)
		serveErr <- srv.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile)
	}()

	logger.Info("mgmt-api.ready",
		slog.String("addr", cfg.ListenAddr),
		slog.Bool("plaintext", cfg.AllowPlaintext),
		slog.String("verifier", cfg.AuthMode),
		slog.String("resolver", cfg.ResolverMode),
	)

	select {
	case <-ctx.Done():
		logger.Info("mgmt-api.shutdown.signal")
		shutCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutCtx); err != nil {
			logger.Warn("mgmt-api.shutdown.error",
				slog.String("error", err.Error()))
		}
		<-serveErr
		logger.Info("mgmt-api.shutdown.done")
		return
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("mgmt-api.serve",
				slog.String("error", err.Error()))
			os.Exit(4)
		}
		logger.Info("mgmt-api.serve.exit")
	}
}

// config is the env-derived configuration the binary uses.
type config struct {
	PGURL           string
	ListenAddr      string
	TLSCertFile     string
	TLSKeyFile      string
	AllowPlaintext  bool
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	ShutdownTimeout time.Duration

	// Auth selection.
	AuthMode       string // "oidc" or "dev_static"
	OIDCIssuer     string
	OIDCAudience   string
	OIDCJWKSURL    string
	OIDCLeeway     time.Duration
	OIDCJWKSTTL    time.Duration
	OIDCDevToken   string
	OIDCDevSubject string

	// Head resolver selection.
	ResolverMode          string // "git-ls-remote" or "static"
	HeadResolverGitPath   string
	HeadResolverTimeout   time.Duration
	HeadResolverStaticSHA string

	// Span ingest verb wiring (Stage 7.2).
	SpanForwardURL     string
	SpanForwardTimeout time.Duration
	SpanServiceMap     map[string]string // service.name -> repo_id
}

// loadConfig reads the binary's configuration from the
// environment. Returns a typed error so the main loop can
// classify "exit 2" (config) from "exit 3" (startup).
//
// Required env vars are validated up-front so a misconfigured
// deployment fails closed at boot instead of silently using a
// dangerous default (e.g. accepting any token, returning a
// fake SHA).
func loadConfig() (config, error) {
	c := config{
		PGURL:                 os.Getenv("AGENT_MEMORY_PG_URL"),
		ListenAddr:            os.Getenv("AGENT_MEMORY_LISTEN_ADDR"),
		TLSCertFile:           os.Getenv("AGENT_MEMORY_TLS_CERT_FILE"),
		TLSKeyFile:            os.Getenv("AGENT_MEMORY_TLS_KEY_FILE"),
		OIDCIssuer:            os.Getenv("AGENT_MEMORY_OIDC_ISSUER"),
		OIDCAudience:          os.Getenv("AGENT_MEMORY_OIDC_AUDIENCE"),
		OIDCJWKSURL:           os.Getenv("AGENT_MEMORY_OIDC_JWKS_URL"),
		OIDCDevToken:          os.Getenv("AGENT_MEMORY_OIDC_DEV_TOKEN"),
		OIDCDevSubject:        os.Getenv("AGENT_MEMORY_OIDC_DEV_SUBJECT"),
		HeadResolverGitPath:   os.Getenv("AGENT_MEMORY_HEAD_RESOLVER_GIT_PATH"),
		HeadResolverStaticSHA: os.Getenv("AGENT_MEMORY_HEAD_RESOLVER_STATIC_SHA"),
		ResolverMode:          os.Getenv("AGENT_MEMORY_HEAD_RESOLVER"),
		SpanForwardURL:        os.Getenv("AGENT_MEMORY_SPAN_FORWARD_URL"),
		SpanForwardTimeout:    10 * time.Second,
		ReadTimeout:           30 * time.Second,
		WriteTimeout:          30 * time.Second,
		ShutdownTimeout:       30 * time.Second,
		HeadResolverTimeout:   mgmtapi.DefaultGitTimeout,
		OIDCJWKSTTL:           mgmtapi.DefaultJWKSCacheTTL,
	}
	if c.PGURL == "" {
		return c, errors.New("AGENT_MEMORY_PG_URL is required")
	}

	// Auth selection: prefer real OIDC when issuer +
	// audience + jwks_url are all set; otherwise fall back
	// to the dev static token. Refuse to start when neither
	// is configured so a fresh deployment can't accidentally
	// serve writes with a wide-open auth tier.
	hasOIDC := c.OIDCIssuer != "" && c.OIDCAudience != "" && c.OIDCJWKSURL != ""
	switch {
	case hasOIDC:
		c.AuthMode = "oidc"
	case c.OIDCDevToken != "":
		c.AuthMode = "dev_static"
	default:
		return c, errors.New(
			"no authenticator configured: set AGENT_MEMORY_OIDC_ISSUER + " +
				"AGENT_MEMORY_OIDC_AUDIENCE + AGENT_MEMORY_OIDC_JWKS_URL " +
				"for production, or AGENT_MEMORY_OIDC_DEV_TOKEN for dev")
	}

	// Head resolver selection: default to git-ls-remote
	// (production); `static` is the dev/test backdoor.
	if c.ResolverMode == "" {
		c.ResolverMode = "git-ls-remote"
	}
	switch c.ResolverMode {
	case "git-ls-remote":
		// no extra required env; git path may be empty
		// (PATH lookup at exec time).
	case "static":
		if !mgmtapi.IsHexGitSHA(c.HeadResolverStaticSHA) {
			return c, errors.New(
				"AGENT_MEMORY_HEAD_RESOLVER=static requires " +
					"AGENT_MEMORY_HEAD_RESOLVER_STATIC_SHA to be a 40- or " +
					"64-char lower-case hex git SHA")
		}
	default:
		return c, fmt.Errorf("AGENT_MEMORY_HEAD_RESOLVER: %q is not a known mode (use git-ls-remote or static)", c.ResolverMode)
	}

	if c.ListenAddr == "" {
		c.ListenAddr = ":8444"
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
	if v := os.Getenv("AGENT_MEMORY_OIDC_LEEWAY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_OIDC_LEEWAY: %w", err)
		}
		c.OIDCLeeway = d
	}
	if v := os.Getenv("AGENT_MEMORY_OIDC_JWKS_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_OIDC_JWKS_TTL: %w", err)
		}
		c.OIDCJWKSTTL = d
	}
	if v := os.Getenv("AGENT_MEMORY_HEAD_RESOLVER_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_HEAD_RESOLVER_TIMEOUT: %w", err)
		}
		c.HeadResolverTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_SPAN_FORWARD_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_SPAN_FORWARD_TIMEOUT: %w", err)
		}
		c.SpanForwardTimeout = d
	}
	if v := os.Getenv("AGENT_MEMORY_SPAN_SERVICE_MAP"); v != "" {
		m, err := parseSpanServiceMap(v)
		if err != nil {
			return c, fmt.Errorf("AGENT_MEMORY_SPAN_SERVICE_MAP: %w", err)
		}
		c.SpanServiceMap = m
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

// buildVerifier wires the [mgmtapi.TokenVerifier] selected by
// the config. Real OIDC is preferred; the dev static token is
// a deliberate fallback for local development.
func buildVerifier(cfg config, logger *slog.Logger) (mgmtapi.TokenVerifier, error) {
	switch cfg.AuthMode {
	case "oidc":
		v, err := mgmtapi.NewOIDCVerifier(cfg.OIDCIssuer, cfg.OIDCAudience, cfg.OIDCJWKSURL)
		if err != nil {
			return nil, fmt.Errorf("OIDC verifier: %w", err)
		}
		v.Leeway = cfg.OIDCLeeway
		v.CacheTTL = cfg.OIDCJWKSTTL
		logger.Info("mgmt-api.verifier.oidc",
			slog.String("issuer", cfg.OIDCIssuer),
			slog.String("audience", cfg.OIDCAudience),
			slog.String("jwks_url", cfg.OIDCJWKSURL),
			slog.Duration("cache_ttl", cfg.OIDCJWKSTTL),
			slog.Duration("leeway", cfg.OIDCLeeway),
		)
		return v, nil
	case "dev_static":
		logger.Warn("mgmt-api.verifier.dev_static",
			slog.String("warning",
				"using StaticBearerVerifier; NOT FIT FOR PRODUCTION -- "+
					"configure AGENT_MEMORY_OIDC_ISSUER / AUDIENCE / JWKS_URL "+
					"to enable real OIDC validation"),
		)
		return &mgmtapi.StaticBearerVerifier{
			Secret:  cfg.OIDCDevToken,
			Subject: cfg.OIDCDevSubject,
		}, nil
	default:
		return nil, fmt.Errorf("unknown auth mode %q", cfg.AuthMode)
	}
}

// buildResolver wires the [mgmtapi.HeadResolver] selected by
// the config. Production deployments use git-ls-remote; dev /
// docker-compose can opt-in to the static resolver via
// AGENT_MEMORY_HEAD_RESOLVER=static.
func buildResolver(cfg config, logger *slog.Logger) (mgmtapi.HeadResolver, error) {
	switch cfg.ResolverMode {
	case "git-ls-remote":
		logger.Info("mgmt-api.resolver.git_ls_remote",
			slog.String("git_path", cfg.HeadResolverGitPath),
			slog.Duration("timeout", cfg.HeadResolverTimeout),
		)
		return &mgmtapi.GitLsRemoteResolver{
			GitPath: cfg.HeadResolverGitPath,
			Timeout: cfg.HeadResolverTimeout,
			Env: []string{
				// Refuse to prompt for credentials on a private remote
				// — fail-fast keeps the resolver from hanging when the
				// operator forgot to configure auth.
				"GIT_TERMINAL_PROMPT=0",
			},
		}, nil
	case "static":
		logger.Warn("mgmt-api.resolver.static",
			slog.String("sha", cfg.HeadResolverStaticSHA),
			slog.String("warning",
				"using StaticHeadResolver; NOT FIT FOR PRODUCTION -- "+
					"set AGENT_MEMORY_HEAD_RESOLVER=git-ls-remote to resolve "+
					"the real HEAD of each repo"),
		)
		return &mgmtapi.StaticHeadResolver{SHA: cfg.HeadResolverStaticSHA}, nil
	default:
		return nil, fmt.Errorf("unknown resolver mode %q", cfg.ResolverMode)
	}
}

// buildSpanForwarder wires the [mgmtapi.SpanForwarder] selected
// by the config. When AGENT_MEMORY_SPAN_FORWARD_URL is unset the
// helper returns nil; NewHandler then installs its fail-CLOSED
// default ([mgmtapi.ErrForwarderNotConfigured]) so every
// `POST /v1/spans` call returns 503 and the operator can dashboard
// the misconfiguration distinctly from a real upstream outage.
// A previous draft returned a silent no-op here, which would have
// served 202 while dropping every span -- caught by design review.
func buildSpanForwarder(cfg config, logger *slog.Logger) mgmtapi.SpanForwarder {
	if cfg.SpanForwardURL == "" {
		logger.Warn("mgmt-api.span_forwarder.not_configured",
			slog.String("warning",
				"AGENT_MEMORY_SPAN_FORWARD_URL is unset; "+
					"POST /v1/spans will fail-closed with 503 "+
					"`forwarder_not_configured` on every call"),
		)
		return nil
	}
	logger.Info("mgmt-api.span_forwarder.http",
		slog.String("url", cfg.SpanForwardURL),
		slog.Duration("timeout", cfg.SpanForwardTimeout),
		slog.Int("service_map_size", len(cfg.SpanServiceMap)),
	)
	return &mgmtapi.HTTPSpanForwarder{
		URL:     cfg.SpanForwardURL,
		Timeout: cfg.SpanForwardTimeout,
	}
}

// staticSpanLookup returns a [mgmtapi.ServiceNameToRepoID]
// closure backed by the parsed AGENT_MEMORY_SPAN_SERVICE_MAP.
// Unmapped names yield the empty string -- the handler counts
// `mgmt_ingest_spans_total{repo_id="", status="unknown_service"}`
// and rejects the call 400.
//
// A nil/empty map yields a lookup that always returns "" --
// safe (every span is rejected), and intentional: leave the
// env var unset to smoke-test the unknown-service path.
func staticSpanLookup(m map[string]string) mgmtapi.ServiceNameToRepoID {
	if len(m) == 0 {
		return func(string) string { return "" }
	}
	// Defensive copy so a later config reload can't mutate
	// what an in-flight request reads.
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return func(name string) string { return cp[name] }
}

// parseSpanServiceMap parses
//
//	"worker-a=550e8400-e29b-41d4-a716-446655440000,worker-b=6ba7b810-9dad-11d1-80b4-00c04fd430c8"
//
// into map["worker-a"] = "550e8400-...". Whitespace around
// keys and values is trimmed; empty entries are skipped so
// trailing commas are tolerated. Returns an error on a
// malformed entry (no "=", empty key, empty value, or
// duplicate key) so an operator typo fails the boot loudly
// rather than silently dropping spans for a worker whose
// mapping was lost.
func parseSpanServiceMap(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, raw := range strings.Split(s, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		eq := strings.IndexByte(entry, '=')
		if eq <= 0 || eq == len(entry)-1 {
			return nil, fmt.Errorf("malformed entry %q (want service.name=repo_id)", entry)
		}
		k := strings.TrimSpace(entry[:eq])
		v := strings.TrimSpace(entry[eq+1:])
		if k == "" {
			return nil, fmt.Errorf("malformed entry %q (empty service.name)", entry)
		}
		if v == "" {
			return nil, fmt.Errorf("malformed entry %q (empty repo_id)", entry)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("duplicate service.name %q", k)
		}
		out[k] = v
	}
	return out, nil
}

// openPG opens a *sql.DB against cfg.PGURL with conservative
// pool limits, pings to verify connectivity, and returns the
// ready pool. Caller is responsible for Close on shutdown.
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
	logger.Info("mgmt-api.pg.connected")
	return pool, nil
}
