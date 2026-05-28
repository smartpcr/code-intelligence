// Package main is the entrypoint for the clean-code-aggregator service
// (Stage 7.1, architecture Sec 3.10 / Sec 5.2.4-5.2.6, tech-spec Sec
// 8.2 `aggregator_cadence=15min`).
//
// # Composition root
//
// This binary is the ONLY process that should run under the
// `clean_code_xrepo_aggregator` Postgres role in production. Per
// migration `0004_roles.up.sql` lines 392-418 the role has:
//
//   - INSERT, SELECT on `metric_sample`, `metric_retraction`,
//     `metric_sample_active` (read side; matches the Metric Ingestor
//     grants modulo the application-layer per-`metric_kind` filter that
//     pins this writer to `pack='system'` only).
//   - INSERT, SELECT on `repo_metric_snapshot`,
//     `cross_repo_percentile`, `portfolio_snapshot` (write side -- the
//     SOLE writer per architecture G1 / Phase 1.5 grants).
//   - Explicit REVOKE UPDATE, DELETE on all three snapshot tables so
//     the row-immutability invariant (architecture G6) survives an
//     application-layer regression.
//
// The wiring is intentionally thin -- composition root only:
//
//   - [openAndPingDB] opens a single libpq handle, fails fast on a
//     permanently unreachable DB.
//   - [buildAggregatorLoop] composes the foundation pass
//     ([aggregator.PGSampleSource] reader + [aggregator.PGSnapshotWriter]
//     writer) AND the system-tier pass
//     ([aggregator.SystemTierComposer] + [aggregator.PGSystemTierInputSource]
//     reader + [aggregator.PGSystemTierWriter] writer) through
//     [aggregator.NewAggregator] + [aggregator.WithSystemTier], and
//     wraps them in [aggregator.NewLoop] as the cadence driver. The
//     aggregator is the SOLE writer of `pack='system'` rows per
//     Phase 1.5 grants -- both the foundation snapshot pass and the
//     system-tier composition pass run inside one Tick. When the
//     operator opts out via [config.EnvDisableAggregator] the loop is
//     skipped and the binary serves a /healthz-only listener (matches
//     the metric_ingestor stale-sweep opt-out pattern).
//   - [buildMux] mounts `/healthz` and `/metrics` -- the always-on
//     surface that lets Kubernetes liveness probes succeed even on an
//     opted-out deployment.
//
// # Single-replica invariant
//
// The aggregator is a SINGLE-REPLICA service: two replicas writing
// snapshots in parallel would race on the same `(metric_kind,
// scope_kind, built_at)` cohorts and stamp rows with two different
// `built_at` values inside the same cadence window. The operator MUST
// pin the deployment replica count to 1 (see
// `services/clean-code/docs/rollout.md` Stage 7.1 section for the
// Kubernetes deployment shape).
//
// # Cadence + cancellation
//
// The loop is driven by [aggregator.Loop.Run] on the process ctx; a
// SIGTERM-derived cancel propagates through Tick (the SQL driver
// honors ctx mid-query) and the loop returns cleanly with
// `context.Canceled`. The `defer` chain then closes the listener,
// then the DB handle, in that order so an in-flight Tick is allowed
// to finish before the DB connection is yanked.
package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/lib/pq"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/aggregator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
)

func main() {
	// exitCode is the process exit status, set to a non-zero value
	// on a fatal startup or runtime failure (e.g. an HTTP listener
	// bind error). It is wired through a deferred os.Exit so the
	// db.Close / srv.Shutdown / cancel defers below all run BEFORE
	// the process exits non-zero -- registering this defer FIRST
	// means it fires LAST in the LIFO defer chain.
	var exitCode int
	defer func() {
		if exitCode != 0 {
			os.Exit(exitCode)
		}
	}()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config.Load: %v", err)
	}

	if cfg.PostgresURL == "" {
		log.Fatalf("%s is required", config.EnvPGURL)
	}

	db, err := openAndPingDB(cfg.PostgresURL, "aggregator")
	if err != nil {
		log.Fatalf("opening aggregator postgres handle: %v", err)
	}
	defer db.Close()

	loop, err := buildAggregatorLoop(cfg, db, logger)
	if err != nil {
		log.Fatalf("buildAggregatorLoop: %v", err)
	}

	// Wire the cancel chain: SIGTERM / SIGINT -> ctx cancel ->
	// loop.Run returns context.Canceled -> server.Shutdown ->
	// db.Close (via defer).
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	loopErrCh := make(chan error, 1)
	if loop != nil {
		go func() { loopErrCh <- loop.Run(ctx) }()
	} else {
		logger.Warn("aggregator: loop disabled via " + config.EnvDisableAggregator + "; serving /healthz only")
	}

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           buildMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpErrCh := make(chan error, 1)
	go func() { httpErrCh <- srv.ListenAndServe() }()
	logger.Info("aggregator: listening", "addr", srv.Addr)

	// loopConsumed tracks whether the loop goroutine's result
	// has already been read off `loopErrCh`. The initial select
	// races three exit conditions: a ctx-cancel (normal
	// shutdown), an http listener error, or a loop exit. When
	// the loop is the one that exits, we record the result here
	// so the post-shutdown drain (below) does not block on an
	// already-empty channel until the shutdown timeout fires.
	// Iter-3 evaluator finding #1 fix.
	var loopConsumed bool
	select {
	case <-ctx.Done():
		// Normal shutdown path: SIGTERM/SIGINT was received.
		// The loop goroutine will return context.Canceled on
		// its own; do NOT log that as unexpected.
		logger.Info("aggregator: shutdown signal received")
	case err := <-httpErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			// A non-ErrServerClosed listener error (typically a
			// bind failure -- EADDRINUSE on :8080, permission
			// denied on a privileged port, etc.) is fatal: the
			// aggregator binary cannot serve its /healthz +
			// /metrics surface, so Kubernetes liveness probes
			// will fail and the operator will not get the
			// aggregator's tick counters. Cancel the loop ctx
			// so the worker goroutine exits cleanly, set a
			// non-zero exit code so the process surfaces the
			// failure to its supervisor (K8s CrashLoopBackoff,
			// systemd Restart=always, etc.), and fall through
			// to the shutdown block so db.Close + the loop
			// drain still complete in an orderly fashion.
			// Iter-4 evaluator finding #3 fix.
			logger.Error("aggregator: http listener failed; aborting", "err", err)
			exitCode = 1
			cancel()
		} else {
			// ErrServerClosed means srv.Shutdown was called
			// (impossible here -- we have not entered the
			// shutdown block yet) or a nil result; treat as a
			// clean exit and propagate ctx cancel anyway so
			// the loop exits cleanly.
			cancel()
		}
	case err := <-loopErrCh:
		loopConsumed = true
		// Even when SIGTERM races ahead of ctx.Done() and we
		// land here first, `context.Canceled` (and its
		// timeout cousin) are normal-shutdown signals, not
		// loop failures. Only treat anything else as
		// "exited unexpectedly".
		switch {
		case err == nil:
			logger.Info("aggregator: loop stopped cleanly")
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			logger.Info("aggregator: loop stopped via ctx cancel", "err", err)
		default:
			logger.Error("aggregator: loop exited unexpectedly", "err", err)
		}
		cancel()
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("aggregator: server shutdown error", "err", err)
	}
	if loop != nil && !loopConsumed {
		// Wait for the loop goroutine to surface its final
		// error (typically context.Canceled). We do NOT block
		// past the shutdown timeout -- a stuck Tick must not
		// wedge the process. Treat ctx.Canceled /
		// DeadlineExceeded as normal shutdown (the same
		// classification rule the initial select uses).
		select {
		case err := <-loopErrCh:
			switch {
			case err == nil:
				logger.Info("aggregator: loop stopped cleanly")
			case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
				logger.Info("aggregator: loop stopped via ctx cancel", "err", err)
			default:
				logger.Warn("aggregator: loop returned error during shutdown", "err", err)
			}
		case <-shutdownCtx.Done():
			logger.Warn("aggregator: loop did not stop within shutdown timeout")
		}
	}
}

// openAndPingDB opens a libpq handle against dsn and pings it with a
// bounded retry budget so the binary fails fast if Postgres is
// permanently unreachable instead of accepting traffic that would
// 500 on every request. Mirrors the metric_ingestor binary's
// openAndPingDB shape so an operator who reads both binaries sees
// the same retry contract.
func openAndPingDB(dsn, role string) (*sql.DB, error) {
	h, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open(%s): %w", role, err)
	}
	const pingAttempts = 30
	var pingErr error
	for i := 0; i < pingAttempts; i++ {
		if pingErr = h.Ping(); pingErr == nil {
			return h, nil
		}
		time.Sleep(time.Second)
	}
	_ = h.Close()
	return nil, fmt.Errorf("postgres %s not reachable after %d attempts: %w", role, pingAttempts, pingErr)
}

// buildAggregatorLoop composes the Stage 7.1 cadence loop when the
// operator has not opted out via [config.EnvDisableAggregator]. The
// loop is composed of six units that the unit tests pin:
//
//  1. [aggregator.NewPGSampleSource] -- reads ACTIVE
//     `metric_sample` rows via the canonical
//     `metric_sample_active` join + `metric_retraction` anti-join.
//  2. [aggregator.NewPGSnapshotWriter] -- INSERTs into all three
//     snapshot tables under one transaction (BEGIN ... COMMIT).
//  3. [aggregator.NewSystemTierComposer] -- pure-function Stage
//     7.2 composer that writes the SEVEN canonical system-tier
//     `metric_kind` rows per `(repo_id, sha, scope_id)` per
//     architecture Sec 1.4.2 + the embedded-mode fail-safe
//     contract from Sec 3.10 step 4.
//  4. [aggregator.NewPGSystemTierInputSource] -- per-tick PG
//     read of `metric_sample_active` + `scope_binding` +
//     `scan_run` that feeds the composer one
//     [aggregator.SystemTierInput] per active `(repo_id, sha)`
//     pair.
//  5. [aggregator.NewPGSystemTierWriter] -- single-tx writer
//     that runs the architecture-canonical SKIP-on-active
//     check then INSERTs into `metric_sample` and
//     `metric_sample_active` (bare INSERT, no ON CONFLICT)
//     per Phase 1.5 grants and architecture Sec 5.2.1
//     lines 1040-1048 (sole writer of `pack='system'`).
//  6. [aggregator.NewAggregator] + [aggregator.NewLoop] -- the
//     in-process per-cohort percentile math + the cadence loop.
//     [aggregator.WithSystemTier] wires the system-tier composer
//     + source + writer into the same tick; the aggregator runs
//     foundation-snapshot AND system-tier passes per Tick.
//
// Returns (nil, nil) when the operator has opted out; the caller
// then runs the /healthz-only listener so K8s liveness probes still
// succeed.
func buildAggregatorLoop(cfg config.Config, db *sql.DB, logger *slog.Logger) (*aggregator.Loop, error) {
	if cfg.DisableAggregator {
		return nil, nil
	}
	if db == nil {
		return nil, fmt.Errorf("buildAggregatorLoop: aggregator is enabled but no *sql.DB was provided")
	}

	source, err := aggregator.NewPGSampleSource(db)
	if err != nil {
		return nil, fmt.Errorf("buildAggregatorLoop: NewPGSampleSource: %w", err)
	}
	writer, err := aggregator.NewPGSnapshotWriter(db)
	if err != nil {
		return nil, fmt.Errorf("buildAggregatorLoop: NewPGSnapshotWriter: %w", err)
	}
	composer, err := aggregator.NewSystemTierComposer()
	if err != nil {
		return nil, fmt.Errorf("buildAggregatorLoop: NewSystemTierComposer: %w", err)
	}
	sysSource, err := aggregator.NewPGSystemTierInputSource(db)
	if err != nil {
		return nil, fmt.Errorf("buildAggregatorLoop: NewPGSystemTierInputSource: %w", err)
	}
	sysWriter, err := aggregator.NewPGSystemTierWriter(db)
	if err != nil {
		return nil, fmt.Errorf("buildAggregatorLoop: NewPGSystemTierWriter: %w", err)
	}
	agg, err := aggregator.NewAggregator(source, writer,
		aggregator.WithSystemTier(composer, sysSource, sysWriter),
	)
	if err != nil {
		return nil, fmt.Errorf("buildAggregatorLoop: NewAggregator: %w", err)
	}
	return aggregator.NewLoop(agg,
		aggregator.WithLoopCadence(cfg.AggregatorCadence),
		aggregator.WithLoopLogger(logger),
	), nil
}

// buildMux mounts the always-on operational surface: `/healthz`
// (Kubernetes liveness) and `/metrics` (Prometheus stub -- the Stage
// 9.1 Prometheus exporter will be wired here once the exporter
// lands). Kept tiny so the binary stays a thin composition root.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Prometheus exporter placeholder. Stage 9.1 will mount the
	// real handler against aggregator.Report counters.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# clean-code-aggregator metrics placeholder\n"))
	})
	return mux
}
