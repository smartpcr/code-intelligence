// Command mgmt-api is the long-running process that serves
// the Stage 7.1 Onboarding write verbs of the Management
// Surface (architecture.md §3.8 / §6.2.1; tech-spec.md §8.5):
//
//	POST /v1/repos                       mgmt.register
//	POST /v1/repos/{repo_id}/ingest      mgmt.ingest
//	POST /v1/repos/{repo_id}/ingest_delta mgmt.ingest_delta
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
	handler := mgmtapi.NewHandler(db, verifier, resolver, mgmtapi.Options{
		Logger: logger,
	})

	mux := http.NewServeMux()
	// Single `/v1/` registration: the mgmtapi handler owns
	// all method dispatch and per-verb routing under v1 (POST
	// writes from Stage 7.1; GET reads from Stage 7.5). This
	// keeps 404/405 envelopes uniform — any unknown path under
	// /v1/ flows through the same JSON envelope as a known
	// verb that received the wrong method, rather than falling
	// back to net/http's default text/plain 404.
	mux.Handle("/v1/", handler)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
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
