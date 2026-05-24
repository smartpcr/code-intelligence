// Command clean-coded is the long-running process for the
// clean-code service. Stage 1.1 (implementation-plan.md) ships
// this binary with three responsibilities:
//
//  1. Load runtime configuration from CLEAN_CODE_* env vars +
//     optional config file (`internal/config`).
//
//  2. Initialise a structured JSON logger with request-id
//     propagation (`internal/logging`).
//
//  3. Serve `/healthz` and `/readyz` on the configured HTTP
//     listener (`internal/health`). The handlers ship the
//     production implementation -- /healthz returns 200 with the
//     build identity, /readyz returns 503 until the mandatory PG
//     pool, OTel exporter, and signing-key cache readiness checks
//     have all registered green. Later stages wire those checks
//     against the real subsystems; Stage 1.1 leaves them
//     unregistered so /readyz stays 503 by design.
//
// The binary is intentionally minimal -- the gRPC surfaces, the
// Metric Ingestor, the Rule Engine, the Refactor Planner, etc.
// arrive in later stages and bolt onto this composition root
// without rewriting it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/config"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/health"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/logging"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/version"
)

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
	)

	healthHandler := health.New(version.Version, version.Commit, version.BuildTime)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           healthHandler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErrCh := make(chan error, 1)
	go func() {
		log.Info("http listener starting", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrCh <- err
			return
		}
		serveErrCh <- nil
	}()

	select {
	case <-ctx.Done():
		log.Info("shutdown signal received; draining http listener")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("http shutdown: %w", err)
		}
		// wait for the goroutine to drain
		if err := <-serveErrCh; err != nil {
			return fmt.Errorf("http serve: %w", err)
		}
		log.Info("clean-coded stopped")
		return nil
	case err := <-serveErrCh:
		if err != nil {
			return fmt.Errorf("http serve: %w", err)
		}
		return nil
	}
}
