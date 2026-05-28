// Package main is the entrypoint for the clean-code-gateway
// service -- the HTTP/JSON gateway from Stage 6.4 of the
// implementation plan.
//
// Per architecture Sec 6 + tech-spec Sec 7 the gateway is the
// single authenticated front door for every canonical verb
// (`/v1/{namespace}/{verb}`). The binary composes:
//
//  1. [api.Authenticator] -- production wiring uses
//     [api.OIDCAuthenticator] against the operator's IdP;
//     scaffold-mode (explicit opt-in via
//     [envGatewayAuthMode] = "dev-hmac") falls back to
//     [api.StaticHMACAuthenticator] so an integration test
//     fixture can mint a token with a shared HMAC secret.
//     Missing OIDC config in the default mode FAILS LOUDLY
//     -- a silent fallback to HMAC in production would
//     downgrade authentication to a shared-secret model.
//
//  2. [api.VerbRegistry] -- the canonical verb table from
//     [api.CanonicalVerbs] (architecture Sec 6.2-6.5),
//     pre-populated with 503 "VERB_NOT_WIRED" stubs and
//     selectively REPLACED with real handlers via
//     [api.NewProductionRegistry] driven by the wiring deps
//     this binary constructs from the PostgreSQL handle.
//
//  3. [api.Tracer] -- defaults to
//     [api.NewOTelTracerFromGlobal] so the OTel exporter the
//     operator's collector configures via the standard
//     environment variables receives per-request spans
//     tagged with `verb`, `caller_subject`, and `repo_id`
//     (architecture Sec 8).
//
//  4. A parent mux that mounts:
//       - `/healthz` -> 200 ok (smoke / liveness)
//       - `/v1/...`  -> [api.GatewayHandler]
//     served via this binary's OWN [http.Server] (not the
//     [api.Server] embedded handle) so timeouts, graceful
//     shutdown, and the parent mux are all driven from the
//     same lifecycle.
//
// # Wired verbs (this stage)
//
// The gateway exposes every canonical verb from
// [api.CanonicalVerbs]; the slot table partitions them into
// (wired, missing) at boot via [api.Wiring.WiredVerbs] /
// [api.Wiring.MissingVerbs]. The wiring is composed
// CONDITIONALLY at boot from the operator's env-var set so
// the same binary can be deployed as a read-only surface,
// a full-stack co-mounted gateway, or anything in between:
//
//   - all 8 `mgmt.read.*` verbs (via
//     [management.NewReader] + [management.NewPGMetricsBackend]);
//   - all 4 `policy.*` write verbs (`publish`, `activate`,
//     `publish_rulepack`, `override`) via
//     [management.NewPolicyWriter] over a real
//     [steward.Steward];
//   - `policy.keys.list_active` (read-only) via
//     [management.NewHandler] over the Reader's
//     [keys.Manager];
//   - `mgmt.override` (steward Override) via the
//     PolicyWriter.
//
// Additionally, the following verbs are wired
// CONDITIONALLY -- ONLY when the matching env vars are set
// (see [buildProductionDeps] at services/clean-code/cmd/clean-code-gateway/main.go:596-672
// for the exact gates). When the env vars are unset, the
// affected verbs surface as 503 stubs with a structured
// boot-time warning so operators can detect the gap:
//
//   - `mgmt.{register_repo,set_mode,retract_sample,rescan}`
//     (4 verbs) -- wired via
//     [composition.BuildMgmtWriter] when
//     [envMgmtPGURL] (CLEAN_CODE_MGMT_PG_URL) is set; the
//     mgmt-role handle is opened lazily and registered on
//     the closers list so SIGTERM drains cleanly.
//   - `ingest.{churn,coverage,test_balance,defects}`
//     (4 verbs) -- wired via
//     [composition.BuildIngestRouter] when BOTH
//     [envWebhookSigningKeyID] and [envWebhookHMACSecret]
//     are set; the ingest pipeline runs entirely under the
//     gateway's primary `clean_code_metric_ingestor` DSN
//     per migrations/0004.
//   - `eval.gate` -- wired via [composition.BuildEvalGate]
//     when BOTH [envEvaluatorPGURL] and
//     [envSolidBatchPGURL] are set; the canonical verb
//     accepts the architecture-pinned optional
//     `policy_version_id` (architecture Sec 6.3, line
//     1338) and dispatches to either
//     [evaluator.Gate.Gate] (active-policy) or
//     [evaluator.Gate.Evaluate] (pinned-PVID) accordingly.
//
// Verbs that remain UNWIRED in a given deployment are
// listed at boot via [api.Wiring.MissingVerbs] and the
// gateway logs a `gateway: ... stays as 503 stub` warning
// per affected env var, so a stale runbook never silently
// masks a misconfiguration.
//
// # Trust boundary
//
// `internal/api/adapters.go` is the ONE production call site
// for [webhook.NewOIDCGatewayTrust]; this binary uses that
// adapter (via [api.NewProductionRegistry]) and does NOT
// construct the witness directly. The structural enforcement
// is in [TestNewOIDCGatewayTrust_LimitedToGatewayComposition]
// (services/clean-code/internal/api/trust_boundary_test.go).
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/api"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/composition"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/management"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// Env vars consumed by this binary. Defined as exported
// constants so a runbook / smoke test can grep this file
// rather than scattering literals across the composition
// root.
const (
	// envPort overrides the listener port. Default 8082 so a
	// developer can run the gateway alongside the indexer
	// (8080) and the eval-gate (8081) without a clash.
	envPort = "PORT"

	// envGatewayAuthMode selects the authenticator. Closed
	// set:
	//
	//   - "oidc"      (default) -- production. Requires
	//     [envOIDCIssuer] + [envOIDCAudience]; uses
	//     [api.OIDCAuthenticator]. Missing config fails loud.
	//   - "dev-hmac"  -- scaffold / integration tests. Uses
	//     [api.StaticHMACAuthenticator] with the b64-decoded
	//     [envGatewayHMACSecretB64]. NEVER use in production
	//     -- a downgrade to a shared-secret model bypasses
	//     the OIDC trust boundary the gateway is meant to
	//     establish.
	envGatewayAuthMode = "CLEAN_CODE_GATEWAY_AUTH_MODE"

	envOIDCIssuer   = "CLEAN_CODE_OIDC_ISSUER"
	envOIDCAudience = "CLEAN_CODE_OIDC_AUDIENCE"
	// envOIDCJWKSURL is optional. Empty triggers OIDC
	// Discovery 1.0 via {issuer}/.well-known/openid-configuration.
	envOIDCJWKSURL = "CLEAN_CODE_OIDC_JWKS_URL"

	// envGatewayHMACSecretB64 is the base64-encoded HMAC-SHA256
	// shared secret used by [api.StaticHMACAuthenticator]
	// when [envGatewayAuthMode] == "dev-hmac". Required in
	// scaffold mode; the base64 form keeps the secret out of
	// shell quoting hazards.
	envGatewayHMACSecretB64 = "CLEAN_CODE_GATEWAY_HMAC_SECRET_B64"
	// envGatewayHMACAudience is the `aud` claim the static
	// HMAC authenticator pins. Required in scaffold mode.
	envGatewayHMACAudience = "CLEAN_CODE_GATEWAY_HMAC_AUDIENCE"

	// envGatewayPGURL is the gateway's PostgreSQL DSN. When
	// empty, falls back to the generic [envPGURL] with a
	// loud WARN: the existing binaries split DSNs by role
	// (clean_code_management, clean_code_metric_ingestor,
	// clean_code_evaluator) and a single shared DSN may
	// fail at runtime with permission errors.
	envGatewayPGURL = "CLEAN_CODE_GATEWAY_PG_URL"
	envPGURL        = "CLEAN_CODE_PG_URL"

	// envMgmtPGURL is the management-role PG DSN. Optional;
	// when set, the gateway wires the [composition.BuildMgmtWriter]
	// chain so the four mgmt write verbs (`mgmt.register_repo`,
	// `mgmt.set_mode`, `mgmt.retract_sample`, `mgmt.rescan`)
	// are bound to real handlers rather than 503 stubs. When
	// unset, the write verbs stay as 503 stubs.
	envMgmtPGURL = "CLEAN_CODE_MGMT_PG_URL"

	// envWebhookSigningKeyID + envWebhookHMACSecret enable
	// the ingest router. Both REQUIRED to wire the four
	// `ingest.*` verbs; either-missing leaves them as 503.
	envWebhookSigningKeyID = "CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID"
	envWebhookHMACSecret   = "CLEAN_CODE_WEBHOOK_HMAC_SECRET"

	// envEvaluatorPGURL + envSolidBatchPGURL enable the
	// eval.gate verb. Both REQUIRED to wire the verb; either-
	// missing leaves eval.gate as a 503 stub.
	envEvaluatorPGURL  = "CLEAN_CODE_EVALUATOR_PG_URL"
	envSolidBatchPGURL = "CLEAN_CODE_SOLID_BATCH_PG_URL"

	// envAuditWALDir is the canonical Audit WAL partition
	// root (Stage 9.1 / architecture Sec 7.10). The gateway
	// reads it ONLY when eval.gate is being wired (both
	// PG URLs present). Defaults to [defaultAuditWALDir]
	// when unset; production deployments override to point
	// at a durable volume.
	envAuditWALDir = "CLEAN_CODE_AUDIT_WAL_DIR"

	// envKMSProvider / envKMSMasterKeyHex configure the
	// signing-key Manager that backs the
	// `policy.keys.list_active` reader AND the
	// `policy.publish` writer's Signer. Same env var names
	// as cmd/clean-code-eval-gate so an operator can use
	// one configuration profile.
	envKMSProvider     = "CLEAN_CODE_KMS_PROVIDER"
	envKMSMasterKeyHex = "CLEAN_CODE_KMS_MASTER_KEY_HEX"

	// envShutdownTimeoutSeconds caps the graceful-drain
	// window on SIGINT/SIGTERM. Default 30s -- long enough
	// for in-flight verbs to complete (eval.gate is the
	// slowest, bounded by the Rule Engine's hot path).
	envShutdownTimeoutSeconds = "CLEAN_CODE_GATEWAY_SHUTDOWN_SECONDS"

	defaultPort            = "8082"
	defaultShutdownSeconds = 30
	defaultAuthMode        = authModeOIDC
	defaultAuditWALDir     = "data/wal/audit"
	authModeOIDC           = "oidc"
	authModeDevHMAC        = "dev-hmac"
	pingAttempts           = 30
	pingDelay              = time.Second
	gatewayServiceLabel    = "clean-code-gateway"
)

func main() {
	logger := slog.Default()
	if err := run(logger); err != nil {
		// log + exit non-zero so a process supervisor restarts.
		logger.Error("clean-code-gateway: startup failed", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// run is the testable boot path. It returns an error rather
// than calling log.Fatal so a future smoke test can assert
// the failure mode without spinning a subprocess.
func run(logger *slog.Logger) error {
	cfg, err := loadGatewayConfig()
	if err != nil {
		return fmt.Errorf("loadGatewayConfig: %w", err)
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	// Authenticator: production path = OIDC, scaffold path =
	// StaticHMAC. Strict opt-in for the HMAC fallback per
	// rubber-duck #1.
	auth, err := buildAuthenticator(cfg, logger)
	if err != nil {
		return fmt.Errorf("buildAuthenticator(%s): %w", cfg.AuthMode, err)
	}

	// Postgres handle -- shared across the wiring deps. A
	// future iter may split this into role-distinct DSNs
	// matching `migrations/0004_roles.up.sql`.
	db, err := openDB(cfg.PGURL)
	if err != nil {
		return fmt.Errorf("openDB: %w", err)
	}
	defer db.Close()
	if err := pingDBWithRetry(rootCtx, db); err != nil {
		return fmt.Errorf("pingDBWithRetry: %w", err)
	}

	// Build the signing-key Manager. Required for
	// `policy.publish` (Signer) and `policy.keys.list_active`
	// (Reader). Scaffold mode (KMS provider unset) wires a
	// nil signer; the publish path then 503s on the
	// no-active-key error, which is the intended contract.
	signingKeys, keysClose, err := buildSigningKeys(rootCtx, cfg, db, logger)
	if err != nil {
		return fmt.Errorf("buildSigningKeys: %w", err)
	}
	if keysClose != nil {
		defer keysClose()
	}

	// Build the production wiring deps. Each non-nil dep
	// surfaces 1..N canonical verb slots via
	// [api.NewProductionWiring]; nil deps leave their slots
	// as 503 stubs. The returned closers list owns any
	// additional DB handles opened for the mgmt/evaluator
	// roles.
	deps, closers, err := buildProductionDeps(rootCtx, cfg, db, signingKeys, logger)
	for _, c := range closers {
		defer c()
	}
	if err != nil {
		return fmt.Errorf("buildProductionDeps: %w", err)
	}

	wiring := api.NewProductionWiring(deps)
	registry := api.NewWiredRegistry(wiring)

	logBootSummary(logger, cfg, wiring)

	// The gateway handler is the api package's top-level
	// http.Handler. We mount it under `/v1/` on a parent mux
	// so this binary owns `/healthz` and any future
	// non-gateway routes alongside.
	//
	// Authorizer: the production gateway honours tech-spec
	// Sec 8.5 ("REST surface enforces OIDC group claims")
	// via [api.NewGroupClaimAuthorizer] which gates every
	// canonical verb against the default per-tier group
	// policy (clean-code-readers, clean-code-ci,
	// clean-code-admins). Scaffold / dev-hmac mode uses the
	// same authorizer so a misconfigured test harness fails
	// the SAME way production does -- the only way to bypass
	// the policy is to inject NoopAuthorizer{} via the
	// [api.ServerConfig.Authorizer] field in test code.
	authz := api.NewGroupClaimAuthorizer("")
	gatewayHandler := api.NewGatewayHandlerWithAuthorizer(auth, authz, registry, api.NewOTelTracerFromGlobal(), logger)

	rootMux := http.NewServeMux()
	rootMux.HandleFunc("/healthz", healthzHandler)
	// `/v1/` MUST end in a slash so the stdlib mux performs
	// the longest-prefix match -- a literal `/v1` would only
	// catch the exact path. The gateway's own
	// [api.ParseVerbPath] then rejects anything that is not
	// exactly `/v1/{namespace}/{verb}` with 404.
	rootMux.Handle("/v1/", gatewayHandler)

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           rootMux,
		ReadHeaderTimeout: api.DefaultReadHeaderTimeout,
		ReadTimeout:       api.DefaultReadTimeout,
		WriteTimeout:      api.DefaultWriteTimeout,
		IdleTimeout:       api.DefaultIdleTimeout,
		// BaseContext threads the cancellable rootCtx into
		// every in-flight request so a SIGTERM propagates
		// to verb handlers' Context.Done() channels.
		BaseContext: func(_ net.Listener) context.Context { return rootCtx },
	}

	return serveWithGracefulShutdown(srv, cfg.ShutdownTimeout, logger)
}

// gatewayConfig bundles the env-sourced inputs `run` consumes.
// Defined as a struct so the loader is testable in isolation
// and so the boot-time invariants (mode-consistent OIDC vs
// HMAC fields) live in [validateGatewayConfig].
type gatewayConfig struct {
	Port            string
	AuthMode        string
	OIDCIssuer      string
	OIDCAudience    string
	OIDCJWKSURL     string
	HMACSecret      []byte
	HMACAudience    string
	PGURL           string
	MgmtPGURL       string
	EvaluatorPGURL  string
	SolidBatchPGURL string
	WebhookKeyID    string
	WebhookSecret   string
	KMSProvider     string
	KMSMasterKeyHex string
	AuditWALDir     string
	ShutdownTimeout time.Duration
}

// loadGatewayConfig reads the canonical env vars and returns
// a validated [gatewayConfig]. Returns an error for any
// missing-required / malformed input so the operator log
// names the failing var.
func loadGatewayConfig() (gatewayConfig, error) {
	cfg := gatewayConfig{
		Port:            envOrDefault(envPort, defaultPort),
		AuthMode:        strings.ToLower(envOrDefault(envGatewayAuthMode, defaultAuthMode)),
		OIDCIssuer:      os.Getenv(envOIDCIssuer),
		OIDCAudience:    os.Getenv(envOIDCAudience),
		OIDCJWKSURL:     os.Getenv(envOIDCJWKSURL),
		HMACAudience:    os.Getenv(envGatewayHMACAudience),
		PGURL:           pickPGURL(),
		MgmtPGURL:       os.Getenv(envMgmtPGURL),
		EvaluatorPGURL:  os.Getenv(envEvaluatorPGURL),
		SolidBatchPGURL: os.Getenv(envSolidBatchPGURL),
		WebhookKeyID:    os.Getenv(envWebhookSigningKeyID),
		WebhookSecret:   os.Getenv(envWebhookHMACSecret),
		KMSProvider:     strings.ToLower(os.Getenv(envKMSProvider)),
		KMSMasterKeyHex: os.Getenv(envKMSMasterKeyHex),
		AuditWALDir:     envOrDefault(envAuditWALDir, defaultAuditWALDir),
	}
	shutdownSecs, err := envSecondsOrDefault(envShutdownTimeoutSeconds, defaultShutdownSeconds)
	if err != nil {
		return gatewayConfig{}, err
	}
	cfg.ShutdownTimeout = time.Duration(shutdownSecs) * time.Second

	if hmacB64 := os.Getenv(envGatewayHMACSecretB64); hmacB64 != "" {
		secret, derr := base64.StdEncoding.DecodeString(hmacB64)
		if derr != nil {
			return gatewayConfig{}, fmt.Errorf("%s: not valid base64: %w", envGatewayHMACSecretB64, derr)
		}
		cfg.HMACSecret = secret
	}

	if err := validateGatewayConfig(cfg); err != nil {
		return gatewayConfig{}, err
	}
	return cfg, nil
}

// validateGatewayConfig pins the mode-specific invariants.
// Production mode requires OIDC issuer + audience; scaffold
// mode requires the HMAC secret + audience. The PGURL is
// always required.
func validateGatewayConfig(cfg gatewayConfig) error {
	if cfg.PGURL == "" {
		return fmt.Errorf("%s (or %s) is required", envGatewayPGURL, envPGURL)
	}
	switch cfg.AuthMode {
	case authModeOIDC:
		if cfg.OIDCIssuer == "" {
			return fmt.Errorf("%s=oidc requires %s", envGatewayAuthMode, envOIDCIssuer)
		}
		if cfg.OIDCAudience == "" {
			return fmt.Errorf("%s=oidc requires %s", envGatewayAuthMode, envOIDCAudience)
		}
	case authModeDevHMAC:
		if len(cfg.HMACSecret) == 0 {
			return fmt.Errorf("%s=dev-hmac requires %s (base64-encoded HMAC key)", envGatewayAuthMode, envGatewayHMACSecretB64)
		}
		if len(cfg.HMACSecret) < 32 {
			return fmt.Errorf("%s=dev-hmac requires %s of at least 32 raw bytes (HMAC-SHA256 minimum)", envGatewayAuthMode, envGatewayHMACSecretB64)
		}
		if cfg.HMACAudience == "" {
			return fmt.Errorf("%s=dev-hmac requires %s", envGatewayAuthMode, envGatewayHMACAudience)
		}
	default:
		return fmt.Errorf("%s=%q is not in the closed set {%q,%q}",
			envGatewayAuthMode, cfg.AuthMode, authModeOIDC, authModeDevHMAC)
	}
	return nil
}

// pickPGURL prefers the gateway-specific DSN; falls back to
// the generic CLEAN_CODE_PG_URL. Returns "" when both are
// unset so the caller can produce a precise error.
func pickPGURL() string {
	if v := os.Getenv(envGatewayPGURL); v != "" {
		return v
	}
	return os.Getenv(envPGURL)
}

// buildAuthenticator constructs the [api.Authenticator] per
// the resolved auth mode. Both branches return a non-nil
// authenticator on success; an error otherwise.
func buildAuthenticator(cfg gatewayConfig, logger *slog.Logger) (api.Authenticator, error) {
	switch cfg.AuthMode {
	case authModeOIDC:
		// Production. Use the api package's full RS256/ES256
		// JWKS-backed verifier.
		auth, err := api.NewOIDCAuthenticator(api.OIDCAuthenticatorConfig{
			Issuer:   cfg.OIDCIssuer,
			Audience: cfg.OIDCAudience,
			JWKSURL:  cfg.OIDCJWKSURL,
		})
		if err != nil {
			return nil, fmt.Errorf("api.NewOIDCAuthenticator: %w", err)
		}
		logger.Info("gateway: OIDC authenticator wired",
			slog.String("issuer", cfg.OIDCIssuer),
			slog.String("audience", cfg.OIDCAudience),
			slog.String("jwks_url", fallbackString(cfg.OIDCJWKSURL, "<discovery>")),
		)
		return auth, nil
	case authModeDevHMAC:
		// Scaffold / integration-test path. Log loudly so
		// the operator can spot accidental production use.
		logger.Warn("gateway: STATIC HMAC AUTHENTICATOR -- scaffold mode (NOT for production)",
			slog.String(envGatewayAuthMode, cfg.AuthMode),
			slog.String("audience", cfg.HMACAudience),
		)
		return &api.StaticHMACAuthenticator{
			Secret:   cfg.HMACSecret,
			Audience: cfg.HMACAudience,
		}, nil
	default:
		return nil, fmt.Errorf("unknown auth mode %q (validate should have caught this)", cfg.AuthMode)
	}
}

// openDB opens a libpq handle. Does NOT ping -- the caller
// runs `pingDBWithRetry` so the bounded-retry loop is
// testable in isolation.
func openDB(dsn string) (*sql.DB, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	return db, nil
}

// pingDBWithRetry runs up to [pingAttempts] PingContext calls
// spaced by [pingDelay]. Returns nil on the first success;
// the last error otherwise. The retry budget exists because
// the gateway is typically started in the same container
// orchestrator pass as Postgres -- a fresh deploy may see
// `connection refused` for a few seconds before pg accepts.
func pingDBWithRetry(ctx context.Context, db *sql.DB) error {
	var lastErr error
	for i := 0; i < pingAttempts; i++ {
		if err := db.PingContext(ctx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pingDelay):
		}
	}
	return fmt.Errorf("postgres ping failed after %d attempts: %w", pingAttempts, lastErr)
}

// buildSigningKeys constructs the policy signing-key Manager
// the policy verbs and `policy.keys.list_active` reader
// depend on. The function is a thin wrapper around
// [keys.Build] honouring the same env vars as
// cmd/clean-code-eval-gate so operator config carries
// across binaries unchanged.
//
// Scaffold mode (KMS provider unset) returns (nil, nil, nil)
// -- the wiring then surfaces:
//
//   - `policy.keys.list_active` -> 503 (ErrManagerUnavailable
//     bubbles through the reader's nil-signing-keys branch);
//   - `policy.publish` -> 503 (steward's noActiveSigner emits
//     ErrNoActiveSigningKey).
func buildSigningKeys(ctx context.Context, cfg gatewayConfig, db *sql.DB, logger *slog.Logger) (*keys.Manager, func(), error) {
	switch cfg.KMSProvider {
	case "":
		logger.Warn("gateway: signing-key Manager unwired (CLEAN_CODE_KMS_PROVIDER unset) -- policy.publish and policy.keys.list_active will surface 503")
		return nil, nil, nil
	case keys.KMSProviderLocal, keys.KMSProviderInMemory:
		buildCfg := keys.BuildConfig{
			KMSProvider:     cfg.KMSProvider,
			KMSMasterKeyHex: cfg.KMSMasterKeyHex,
			// Gateway is a CONSUMER of signing keys for the
			// verify path (steward.VerifyAny on
			// policy.publish round-trip); the publisher
			// Steward is responsible for minting. Setting
			// MintFirstKeyIfEmpty=true here would race the
			// dedicated publisher and is wrong.
			MintFirstKeyIfEmpty: false,
		}
		if cfg.KMSProvider == keys.KMSProviderLocal {
			buildCfg.DB = db
		}
		res, err := keys.Build(ctx, buildCfg)
		if err != nil {
			return nil, nil, fmt.Errorf("keys.Build(provider=%s): %w", cfg.KMSProvider, err)
		}
		logger.Info("gateway: signing-key Manager wired", slog.String("provider", cfg.KMSProvider))
		return res.Manager, res.Close, nil
	default:
		return nil, nil, fmt.Errorf("%s=%q is not in %v", envKMSProvider, cfg.KMSProvider, keys.AllKMSProviders)
	}
}

// buildProductionDeps assembles the [api.ProductionWiringDeps]
// from the constructed Postgres-backed stores and the
// optional signing-key Manager. The function is intentionally
// nil-tolerant: a missing dep leaves the matching verb slot
// at 503 rather than failing boot.
//
// Verb coverage produced by this assembly when ALL env vars
// are supplied:
//
//   - `policy.keys.list_active`            -- via MgmtHandler
//   - `mgmt.read.*` (8 verbs)              -- via MgmtReader
//   - `mgmt.override`, `policy.{publish,activate,publish_rulepack}` -- via PolicyWriter
//   - `mgmt.{register_repo,set_mode,retract_sample,rescan}` (4 verbs) -- via MgmtWriter
//     (requires CLEAN_CODE_MGMT_PG_URL)
//   - `ingest.{churn,coverage,test_balance,defects}` (4 verbs) -- via IngestRouter
//     (requires CLEAN_CODE_WEBHOOK_SIGNING_KEY_ID +
//     CLEAN_CODE_WEBHOOK_HMAC_SECRET)
//   - `eval.gate` -- via EvalGateHandler (requires
//     CLEAN_CODE_EVALUATOR_PG_URL +
//     CLEAN_CODE_SOLID_BATCH_PG_URL)
//
// Verbs left as 503 stubs when their prerequisite env vars
// are absent are logged at boot via
// [api.Wiring.MissingVerbs] / [logBootSummary].
func buildProductionDeps(ctx context.Context, cfg gatewayConfig, db *sql.DB, signingKeys *keys.Manager, logger *slog.Logger) (api.ProductionWiringDeps, []func(), error) {
	deps := api.ProductionWiringDeps{}
	var closers []func()

	// Reader -- backed by PG metrics backend so the eight
	// `mgmt.read.*` verbs return real data.
	metricsBackend, err := management.NewPGMetricsBackend(db)
	if err != nil {
		return api.ProductionWiringDeps{}, nil, fmt.Errorf("management.NewPGMetricsBackend: %w", err)
	}
	reader := management.NewReader(signingKeys, management.WithMetricsBackend(metricsBackend))
	deps.MgmtReader = reader
	deps.MgmtHandler = management.NewHandler(reader)

	// PolicyWriter -- needs a Steward. Scaffold mode wires
	// a Steward over the SQLStore but without a Signer; the
	// publish verb then surfaces 503 (steward's
	// noActiveSigner) until [envKMSProvider] is set.
	stewardStore, err := steward.NewSQLStore(db)
	if err != nil {
		return api.ProductionWiringDeps{}, nil, fmt.Errorf("steward.NewSQLStore: %w", err)
	}
	var stewardSigner steward.Signer
	if signingKeys != nil {
		stewardSigner = signingKeys
	}
	stew, err := steward.New(steward.Config{Store: stewardStore, Signer: stewardSigner})
	if err != nil {
		return api.ProductionWiringDeps{}, nil, fmt.Errorf("steward.New: %w", err)
	}
	deps.PolicyWriter = management.NewPolicyWriter(stew)

	// MgmtWriter -- wired when CLEAN_CODE_MGMT_PG_URL is
	// supplied. The mgmt-role handle is opened lazily here
	// (closed via the returned closers list) so a deployment
	// that doesn't set the env var doesn't pay the cost of
	// a second pool.
	if cfg.MgmtPGURL != "" {
		mgmtDB, oerr := openDB(cfg.MgmtPGURL)
		if oerr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("openDB(mgmt): %w", oerr)
		}
		closers = append(closers, func() { _ = mgmtDB.Close() })
		if perr := pingDBWithRetry(ctx, mgmtDB); perr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("pingDBWithRetry(mgmt): %w", perr)
		}
		mgmtWriter, werr := composition.BuildMgmtWriter(db, mgmtDB, logger)
		if werr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("composition.BuildMgmtWriter: %w", werr)
		}
		deps.MgmtWriter = mgmtWriter
	} else {
		logger.Warn("gateway: " + envMgmtPGURL + " unset; mgmt.{register_repo,set_mode,retract_sample,rescan} stay as 503 stubs")
	}

	// IngestRouter -- wired when both webhook env vars are
	// supplied. Single-DB composition: the ingest pipeline
	// runs ENTIRELY under `clean_code_metric_ingestor` per
	// migrations/0004, so the same `db` handle suffices.
	if cfg.WebhookKeyID != "" && cfg.WebhookSecret != "" {
		ingestRouter, ierr := composition.BuildIngestRouter(db, composition.IngestRouterConfig{
			SigningKeyID: cfg.WebhookKeyID,
			HMACSecret:   cfg.WebhookSecret,
		}, logger)
		if ierr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("composition.BuildIngestRouter: %w", ierr)
		}
		deps.IngestRouter = ingestRouter
	} else {
		logger.Warn("gateway: " + envWebhookSigningKeyID + " / " + envWebhookHMACSecret + " unset; ingest.* verbs stay as 503 stubs")
	}

	// EvalGateHandler -- wired when both evaluator DSNs are
	// supplied. Two-DB composition: the evaluator handle
	// covers the degraded short-circuit writes; the
	// solid_batch handle covers the canonical rule-pass
	// Audit triple (separation enforced by PG role grants).
	if cfg.EvaluatorPGURL != "" && cfg.SolidBatchPGURL != "" {
		evalDB, oerr := openDB(cfg.EvaluatorPGURL)
		if oerr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("openDB(evaluator): %w", oerr)
		}
		closers = append(closers, func() { _ = evalDB.Close() })
		if perr := pingDBWithRetry(ctx, evalDB); perr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("pingDBWithRetry(evaluator): %w", perr)
		}
		solidDB, oerr := openDB(cfg.SolidBatchPGURL)
		if oerr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("openDB(solid_batch): %w", oerr)
		}
		closers = append(closers, func() { _ = solidDB.Close() })
		if perr := pingDBWithRetry(ctx, solidDB); perr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("pingDBWithRetry(solid_batch): %w", perr)
		}
		// Stage 9.1 -- Audit WAL writer (architecture
		// Sec 7.10 / tech-spec Sec 4.13). REQUIRED by
		// `composition.BuildEvalGate` because the gate's
		// rule-pass AND degraded paths now mirror every
		// audit INSERT to a signed WAL frame fsynced
		// before SQL commit. The gateway reads
		// CLEAN_CODE_AUDIT_WAL_DIR (defaults to
		// `data/wal/audit`) and constructs the writer
		// here.
		//
		// Signer choice (iter-3 evaluator item #1):
		// when `signingKeys` is non-nil
		// (CLEAN_CODE_KMS_PROVIDER=local | in-memory),
		// the WAL signer is the production Ed25519
		// path
		// ([composition.NewKeysManagerWALSigner]). In
		// scaffold mode (KMS provider unset) the signer
		// falls back to `wal.NoopSigner`, since the
		// gate's signature-verify path is already
		// degraded -- frames on disk carry the SHA-256
		// stand-in but the binary stays serviceable for
		// dev/test. Production deployments MUST set
		// CLEAN_CODE_KMS_PROVIDER.
		var walSigner wal.Signer
		if signingKeys != nil {
			walSigner = composition.NewKeysManagerWALSigner(signingKeys)
			logger.Info("gateway: Audit WAL signer = keys.Manager-backed Ed25519", "provider", cfg.KMSProvider)
		} else {
			walSigner = wal.NoopSigner{}
			logger.Warn("gateway: Audit WAL signer = wal.NoopSigner (SHA-256 stand-in, signing_key_id=uuid.Nil). This is acceptable ONLY in scaffold mode (" + envKMSProvider + " unset); production deployments MUST set " + envKMSProvider + "=local + " + envKMSMasterKeyHex + " so the WAL signer becomes the Ed25519 keys.Manager path.")
		}
		walWriter, werr := wal.NewWriter(wal.WriterConfig{
			Dir:    cfg.AuditWALDir,
			Signer: walSigner,
		})
		if werr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("wal.NewWriter(dir=%s): %w", cfg.AuditWALDir, werr)
		}
		logger.Info("gateway: Audit WAL writer wired", "dir", cfg.AuditWALDir)

		gate, gerr := composition.BuildEvalGate(ctx, composition.EvalGateConfig{
			EvaluatorDB:  evalDB,
			SolidBatchDB: solidDB,
			Signer:       stewardSigner,
			WalWriter:    walWriter,
		}, logger)
		if gerr != nil {
			return api.ProductionWiringDeps{}, closers, fmt.Errorf("composition.BuildEvalGate: %w", gerr)
		}
		deps.EvalGateHandler = composition.EvalGateHandler(gate, logger)
	} else {
		logger.Warn("gateway: " + envEvaluatorPGURL + " / " + envSolidBatchPGURL + " unset; eval.gate stays a 503 stub")
	}

	return deps, closers, nil
}

// logBootSummary emits the canonical boot log lines:
// auth mode, listen addr, wired/missing verbs. The
// "missing verbs" line is the deliberate operator handoff
// -- a follow-up stage that wires e.g. ingest.* will see
// the line shrink.
func logBootSummary(logger *slog.Logger, cfg gatewayConfig, wiring api.Wiring) {
	wired := wiring.WiredVerbs()
	missing := wiring.MissingVerbs()
	logger.Info("gateway: boot summary",
		slog.String("service", gatewayServiceLabel),
		slog.String("port", cfg.Port),
		slog.String("auth_mode", cfg.AuthMode),
		slog.Int("wired_verbs", len(wired)),
		slog.Int("missing_verbs", len(missing)),
	)
	logger.Info("gateway: wired verbs", slog.Any("verbs", wired))
	if len(missing) > 0 {
		logger.Warn("gateway: verbs unwired (503 stubs)",
			slog.Any("verbs", missing),
			slog.String("note", "these mount as VERB_NOT_WIRED until the relevant subsystem composition root plumbs them in"),
		)
	}
}

// healthzHandler is the gateway's smoke / liveness endpoint.
// Returns 200 + "ok" so a load balancer's TCP-probe sees a
// living process; does not exercise the DB / IdP (those are
// the gateway's normal-request concerns and a healthz that
// hits them would flap on transient outages downstream).
func healthzHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// serveWithGracefulShutdown runs srv until SIGINT/SIGTERM
// arrives, then drains in-flight requests within
// `shutdownTimeout`. Returns nil on a clean drain;
// [http.ErrServerClosed] is suppressed so a graceful exit
// reads as success.
func serveWithGracefulShutdown(srv *http.Server, shutdownTimeout time.Duration, logger *slog.Logger) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	errCh := make(chan error, 1)
	go func() {
		logger.Info("gateway: listening", slog.String("addr", srv.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("ListenAndServe: %w", err)
			return
		}
		errCh <- nil
	}()

	select {
	case sig := <-signals:
		logger.Info("gateway: shutdown signal received", slog.String("signal", sig.String()))
		drainCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(drainCtx); err != nil {
			return fmt.Errorf("Shutdown: %w", err)
		}
		// Wait for the serve goroutine to exit; ignore the
		// expected ErrServerClosed it produces.
		if err := <-errCh; err != nil {
			return err
		}
		logger.Info("gateway: shutdown complete")
		return nil
	case err := <-errCh:
		return err
	}
}

// envOrDefault is the tiny helper used across the loader.
func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envSecondsOrDefault parses an env var as a non-negative
// integer number of seconds; returns the supplied default on
// empty input and an error on malformed input.
func envSecondsOrDefault(key string, fallback int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	var secs int
	if _, err := fmt.Sscanf(raw, "%d", &secs); err != nil {
		return 0, fmt.Errorf("%s=%q: %w", key, raw, err)
	}
	if secs < 0 {
		return 0, fmt.Errorf("%s=%d: must be >= 0", key, secs)
	}
	return secs, nil
}

// fallbackString returns `s` when non-empty, otherwise `f`.
// Used only for log-formatting of optional values.
func fallbackString(s, f string) string {
	if s == "" {
		return f
	}
	return s
}
