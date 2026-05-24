// Command clean-coded is the long-running process for the
// clean-code service. The Stage 5.1 composition root has four
// responsibilities:
//
//  1. Load runtime configuration from CLEAN_CODE_* env vars +
//     optional config file (`internal/config`).
//
//  2. Initialise a structured JSON logger with request-id
//     propagation (`internal/logging`).
//
//  3. Wire the Policy Steward signing-key cache
//     (`internal/policy/keys`). The provider, master-key, and
//     PostgreSQL handle come from config; the resulting
//     `keys.Manager` is shared by the management read-side
//     verbs and the evaluator gate. A `signing_key_cache`
//     readiness check is registered against the health
//     handler so `/readyz` only turns green once the KMS
//     responds and the first key is loaded. A background
//     refresh ticker reloads the cache every
//     `signingKeyCacheRefreshInterval` so a sibling replica
//     that rotates the active key is picked up by this
//     replica within the refresh window.
//
//  4. Serve `/healthz`, `/readyz`, and the
//     `/v1/policy/keys/list_active` read verb on the
//     configured HTTP listener.
//
// Future stages bolt the gRPC surfaces, the Metric Ingestor,
// the Rule Engine, and the Refactor Planner onto this same
// composition root.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/config"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/health"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/logging"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/management"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/version"
)

// signingKeyCacheRefreshInterval is the cadence at which the
// long-running process re-reads the policy_signing_keys store
// into its in-process cache. Five minutes is two orders of
// magnitude faster than the 24h overlap window from tech-spec
// Sec 8.2, so a sibling-replica rotation always propagates
// well before the old key would have expired -- giving signing
// the architectural slack the overlap was designed for
// without delaying observability of a freshly-rotated key on
// this replica.
const signingKeyCacheRefreshInterval = 5 * time.Minute

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "clean-coded: %v\n", err)
		os.Exit(1)
	}
}

// run is the testable composition root. Returns nil on graceful
// shutdown and a wrapped error otherwise.
func run(args []string) error {
	fs := flag.NewFlagSet("clean-coded", flag.ContinueOnError)
	versionFlag := fs.Bool("version", false, "print version + commit + build_time and exit")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *versionFlag {
		fmt.Printf("clean-coded %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildTime)
		return nil
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	log := logging.New(logging.Config{
		Level: logging.ParseLevel(cfg.LogLevel),
	})
	log.Info("clean-coded starting",
		"version", version.Version,
		"commit", version.Commit,
		"build_time", version.BuildTime,
		"http_addr", cfg.HTTPAddr,
		"ast_mode_default", cfg.ASTModeDefault,
		"external_metric_coverage_format", cfg.ExternalMetricCoverageFormat,
		"gate_degraded_policy", cfg.GateDegradedPolicy,
		"policy_signing_required", cfg.PolicySigningRequired,
		"refactor_effort_source", cfg.RefactorEffortSource,
		"kms_provider", cfg.KMSProvider,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	healthHandler := health.New(version.Version, version.Commit, version.BuildTime)

	var (
		db          *sql.DB
		keysResult  *keys.BuildResult
		stopRefresh func()
		mgmt        *management.Handler
		policy      *management.PolicyWriter
	)

	// --- Policy Steward signing-key wiring (Stage 5.1) ---
	// Scaffold-mode (`KMSProvider == ""`) leaves the signing-
	// key cache unwired and /readyz keeps its 503 by design;
	// production deploys set `CLEAN_CODE_KMS_PROVIDER=local`
	// and pair it with `CLEAN_CODE_KMS_MASTER_KEY_HEX` +
	// `CLEAN_CODE_PG_URL` so a real `keys.Manager` is
	// constructed.
	if cfg.KMSProvider != "" {
		bc := keys.BuildConfig{
			KMSProvider:         cfg.KMSProvider,
			KMSMasterKeyHex:     cfg.KMSMasterKeyHex,
			Overlap:             time.Duration(cfg.PolicyPublishOverlapSeconds) * time.Second,
			MintFirstKeyIfEmpty: true,
		}
		if cfg.PostgresURL != "" && cfg.KMSProvider == keys.KMSProviderLocal {
			handle, openErr := sql.Open("postgres", cfg.PostgresURL)
			if openErr != nil {
				return fmt.Errorf("opening postgres: %w", openErr)
			}
			pingCtx, pingCancel := context.WithTimeout(ctx, 10*time.Second)
			pingErr := handle.PingContext(pingCtx)
			pingCancel()
			if pingErr != nil {
				_ = handle.Close()
				return fmt.Errorf("pinging postgres: %w", pingErr)
			}
			db = handle
			bc.DB = handle
		}
		buildCtx, buildCancel := context.WithTimeout(ctx, 30*time.Second)
		built, buildErr := keys.Build(buildCtx, bc)
		buildCancel()
		if buildErr != nil {
			if db != nil {
				_ = db.Close()
			}
			return fmt.Errorf("policy/keys: Build: %w", buildErr)
		}
		keysResult = built
		healthHandler.AddReadyCheck("signing_key_cache", health.Check(built.HealthCheck))
		stopRefresh = built.Manager.StartRefresh(ctx, signingKeyCacheRefreshInterval, func(refreshErr error) {
			log.Warn("policy/keys: background refresh failed",
				"error", refreshErr.Error(),
			)
		})
		mgmt = management.NewHandler(management.NewReader(built.Manager))

		// --- Policy Steward write verbs (Stage 5.2) ---
		// The steward signs the policy-version row at
		// publish time using the same `keys.Manager` the
		// read-side verb (`policy.keys.list_active`)
		// exposes. We pick the persistence backend the
		// same way as the keys subsystem: when a PostgreSQL
		// handle is wired, use the SQL store; otherwise
		// fall back to the in-memory store so a developer
		// can exercise the verbs end-to-end without
		// spinning up a database.
		var stewStore steward.Store
		if db != nil {
			sqlStore, ssErr := steward.NewSQLStore(db)
			if ssErr != nil {
				_ = db.Close()
				return fmt.Errorf("policy/steward: NewSQLStore: %w", ssErr)
			}
			stewStore = sqlStore
			log.Info("policy steward backed by postgres")
		} else {
			stewStore = steward.NewInMemoryStore()
			log.Warn("policy steward backed by in-memory store (rows are lost on process restart; set CLEAN_CODE_PG_URL to persist)")
		}
		stew, stewErr := steward.New(steward.Config{
			Store:  stewStore,
			Signer: built.Manager,
		})
		if stewErr != nil {
			if db != nil {
				_ = db.Close()
			}
			return fmt.Errorf("policy/steward: New: %w", stewErr)
		}
		policy = management.NewPolicyWriter(stew)
		log.Info("policy steward wired",
			"backend", map[bool]string{true: "postgres", false: "memory"}[db != nil],
		)
		log.Info("policy signing-key cache wired",
			"kms_provider", cfg.KMSProvider,
			"postgres_configured", db != nil,
			"overlap_seconds", cfg.PolicyPublishOverlapSeconds,
			"refresh_interval", signingKeyCacheRefreshInterval.String(),
		)
	} else {
		log.Warn("policy signing-key cache NOT wired (scaffold mode: CLEAN_CODE_KMS_PROVIDER is empty)")
	}

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           rootMux(healthHandler, mgmt, policy),
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErrCh := make(chan error, 1)
	go func() {
		log.Info("http listener starting", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrCh <- err
			return
		}
		serveErrCh <- nil
	}()

	closeAll := func() {
		if stopRefresh != nil {
			stopRefresh()
		}
		if keysResult != nil && keysResult.Close != nil {
			keysResult.Close()
		}
		if db != nil {
			_ = db.Close()
		}
	}

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received; draining http listener")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			closeAll()
			return fmt.Errorf("http shutdown: %w", err)
		}
		if err := <-serveErrCh; err != nil {
			closeAll()
			return fmt.Errorf("http serve: %w", err)
		}
		closeAll()
		log.Info("clean-coded stopped")
		return nil
	case err := <-serveErrCh:
		closeAll()
		if err != nil {
			return fmt.Errorf("http serve: %w", err)
		}
		return nil
	}
}
