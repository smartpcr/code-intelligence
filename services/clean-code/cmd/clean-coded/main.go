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
	"log/slog"
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
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metrics/recipes"
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

	// Construct the canonical Stage 2.3 base-pack metric
	// recipe registry and emit the startup log line listing
	// the registered recipes (implementation-plan Stage 2.3
	// line 201). The registry is constructed unconditionally
	// so the startup snapshot lands in every boot's log even
	// before the Compute Engine wiring (later stages) consumes
	// the registry to dispatch recipes per AstFile.
	_ = recipes.DefaultRegistryWithLog(log)

	// Construct the canonical Stage 2.5 project-level base-
	// pack registry (cycle_member + duplication_ratio) and
	// emit its startup log line. These recipes are
	// dispatched separately from the per-file [Registry]
	// because their value requires the WHOLE project's
	// `*AstFile` set (SCC detection / cross-file token
	// concat) and they implement [ProjectRecipe.ComputeProject]
	// in addition to the per-file [Recipe] interface. Wiring
	// the project-level registry HERE -- in the same
	// composition-root section as the per-file registry --
	// makes both base-pack registries visible in the boot
	// snapshot, mirroring implementation-plan Stage 2.5
	// expectations alongside Stage 2.3 line 201.
	_ = recipes.DefaultProjectRegistryWithLog(log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	healthHandler := health.New(version.Version, version.Commit, version.BuildTime)

	var (
		db          *sql.DB
		keysResult  *keys.BuildResult
		stopRefresh func()
		mgmt        *management.Handler
		signer      steward.Signer
	)

	// --- Policy Steward signing-key wiring (Stage 5.1) ---
	// Scaffold-mode (`KMSProvider == ""`) leaves the signing-
	// key cache unwired and the signing-key-dependent read
	// verb (`/v1/policy/keys/list_active`) keeps its 503 by
	// design; production deploys set
	// `CLEAN_CODE_KMS_PROVIDER=local` and pair it with
	// `CLEAN_CODE_KMS_MASTER_KEY_HEX` + `CLEAN_CODE_PG_URL` so
	// a real `keys.Manager` is constructed.
	//
	// Stage 5.3 contract: the override write verb
	// (`POST /v1/mgmt/override`) is the operator's emergency
	// kill switch and MUST keep serving 200 even when the
	// signing-key cache is unwired. The Policy Steward + write
	// verbs are therefore built UNCONDITIONALLY below, after
	// this signing-key branch. The Stage 5.2 verbs
	// (publish / activate / publish_rulepack) still refuse with
	// 503 in scaffold mode because the steward's null-object
	// signer reports an empty active-key set, which is exactly
	// the [ErrNoActiveSigningKey] precondition those verbs
	// already enforce.
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
		signer = built.Manager
		log.Info("policy signing-key cache wired",
			"kms_provider", cfg.KMSProvider,
			"postgres_configured", db != nil,
			"overlap_seconds", cfg.PolicyPublishOverlapSeconds,
			"refresh_interval", signingKeyCacheRefreshInterval.String(),
		)
	} else {
		log.Warn("policy signing-key cache NOT wired (scaffold mode: CLEAN_CODE_KMS_PROVIDER is empty)")
	}

	// --- Policy Steward write verbs (Stage 5.2 + 5.3) ---
	// Built UNCONDITIONALLY so the Stage 5.3 kill-switch verb
	// (`mgmt.override`) is always reachable. In scaffold mode
	// `signer` is the zero interface; `steward.New` installs a
	// null-object signer in that case. The persistence backend
	// follows the same `db != nil` rule as the keys subsystem.
	policy, policyCloseDB, policyErr := buildPolicyWriter(db, signer, log)
	if policyErr != nil {
		if policyCloseDB && db != nil {
			_ = db.Close()
		}
		return fmt.Errorf("policy/steward: %w", policyErr)
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

// buildPolicyWriter constructs the Policy Steward (Stage 5.2 +
// 5.3 write verbs) and wraps it in a [management.PolicyWriter].
// It is the testable composition seam that pins the Stage 5.3
// kill-switch contract at the wiring layer:
//
//   - When `db != nil`, the Steward is backed by an
//     `[steward.SQLStore]` so override rows persist across
//     process restarts.
//   - When `db == nil` (scaffold mode), the Steward is backed
//     by an `[steward.InMemoryStore]`; the operator gets a
//     warning that rows are ephemeral.
//   - `signer` MAY be nil. `[steward.New]` installs a null
//     object in that case, so `Steward.Override` (which does
//     not require a signing key) keeps serving 200 while the
//     Stage 5.2 verbs return `[steward.ErrNoActiveSigningKey]`
//     via the existing `len(ListActive)==0` branch.
//
// Returns the `*management.PolicyWriter`, a `closeDBOnError`
// boolean the caller uses to decide whether to close `db` on a
// failure (true when this helper opened internal state that
// makes the db handle unsafe to reuse), and an error.
//
// Pinned by:
//   - `TestBuildPolicyWriter_ScaffoldModeProducesWriter` (the
//     wiring invariant: nil signer + nil db -> non-nil writer)
//   - `TestRootMux_ScaffoldModeOverrideMounted_200` (the
//     composition-root + kill-switch invariant: rootMux wired
//     with a steward built via this helper serves 200 at
//     `POST /v1/mgmt/override` AND still serves 503 at
//     `POST /v1/policy/publish` under the SAME scaffold-mode
//     mux)
func buildPolicyWriter(db *sql.DB, signer steward.Signer, log *slog.Logger) (*management.PolicyWriter, bool, error) {
	var (
		stewStore    steward.Store
		closeDBOnErr bool
	)
	if db != nil {
		sqlStore, err := steward.NewSQLStore(db)
		if err != nil {
			return nil, true, fmt.Errorf("NewSQLStore: %w", err)
		}
		stewStore = sqlStore
		if log != nil {
			log.Info("policy steward backed by postgres")
		}
	} else {
		stewStore = steward.NewInMemoryStore()
		if log != nil {
			log.Warn("policy steward backed by in-memory store (rows are lost on process restart; set CLEAN_CODE_PG_URL to persist)")
		}
	}
	stew, err := steward.New(steward.Config{
		Store:  stewStore,
		Signer: signer, // MAY be nil in scaffold mode -- steward.New installs a null-object signer
	})
	if err != nil {
		return nil, closeDBOnErr, fmt.Errorf("New: %w", err)
	}
	if log != nil {
		log.Info("policy steward wired",
			"backend", map[bool]string{true: "postgres", false: "memory"}[db != nil],
			"signing_key_cache", signer != nil,
		)
	}
	return management.NewPolicyWriter(stew), closeDBOnErr, nil
}
