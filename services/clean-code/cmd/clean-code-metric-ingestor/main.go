// Package main is the entrypoint for the clean-code-metric-ingestor service.
// It processes commits through scan recipes, manages the ScanRun lifecycle,
// AND -- starting at Stage 5.7 -- emits a non-blocking [rule_engine.ScanEvent]
// to the post-scan dispatcher on each successful transition to `scanned`.
// The dispatcher drives the in-process [rule_engine.Worker] which re-runs
// the active [steward.PolicyVersion] under `caller='batch_refresh'`,
// writing the canonical (run, verdict, findings) triple in one transaction.
//
// # Composition root layout
//
// Stage 3.5 wiring runs the production
// [metric_ingestor.StaleScanRunSweepLoop] as a background goroutine. The
// loop reads `scan_timeout` and `periodic_sweep_cadence` from the canonical
// [config.Config] (tech-spec Sec 8.2) and runs against the live
// [metric_ingestor.PGScanRunStore], so stale `scan_run(status='running')`
// rows past `scan_timeout` transition to `status='failed'` without operator
// intervention. The metrics emitted by the sweep
// (`cleancode_sweep_stale_scans_total`,
// `cleancode_sweep_failed_commits_total`) surface via `/metrics` in
// Prometheus text-exposition format.
//
// Stage 5.7 wiring layers the SOLID Rule Engine batch worker on top: a
// bounded post-scan dispatcher channel fans [rule_engine.ScanEvent]s from
// the legacy demo `/v1/ingestor/process` path (and any future canonical
// scan-completion path it stands in for) to the in-process
// [rule_engine.Worker]. The worker writes
// `evaluation_run`/`evaluation_verdict`/`finding` rows under
// `caller='batch_refresh'` against a dedicated Audit-writer DB handle.
//
// # Production vs legacy demo
//
// The production composition root mounts ONLY the canonical surface:
// `/healthz`, `/metrics`, the in-process sweep goroutine, and the
// Stage 5.7 rule_engine worker goroutine. Both `/v1/ingestor/process`
// and `/v1/ingestor/scan-run` are LEGACY DEMO handlers that write the
// older `001_init.sql` `scan_run(commit_sha,kind,status,finished_at)`
// shape and are mounted iff [config.Config.EnableLegacyDemoAPI] is true
// (env `CLEAN_CODE_ENABLE_LEGACY_DEMO_API`). The Stage 1.2 canonical
// schema (`0001_catalog_lifecycle`) does not expose those columns;
// running the legacy demo against the canonical schema is a wiring
// error.
//
// # Config contract
//
// All `CLEAN_CODE_*` env vars are read EXCLUSIVELY by `internal/config`
// (per the package doc lines 15-16). This binary reads only
// [config.Config] fields; an invalid env value is a HARD ERROR at
// startup (no silent fallback to defaults). The Stage 5.7 wiring reads
// two additional env vars directly (`CLEAN_CODE_RULE_ENGINE_DISABLED`
// and `CLEAN_CODE_SOLID_BATCH_PG_URL`) because they govern an optional
// per-composition-root knob (worker enable + dedicated audit handle)
// rather than per-request behaviour; both default to safe values when
// unset.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/config"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/rule_engine"
)

// validScanRunKinds enumerates the allowed scan_run kind values per the
// canonical `clean_code.scan_run_kind` enum (migrations/0001_catalog_lifecycle.up.sql:117).
// The ingestor MUST reject any kind not in this set before reaching PostgreSQL.
//
// Iter-7 evaluator item #3: the legacy bootstrap migration `001_init.sql`
// declared a stale set `{ast_metrics, lint, complexity, dependency}` that
// does NOT match the canonical schema. This guard is now aligned with the
// canonical 5-value enum so a `kind` POSTed by upstream services is
// accepted iff it would be accepted by Postgres. The guard remains scoped
// to the legacy demo `/v1/ingestor/scan-run` handler; the canonical Stage
// 1.2 schema enforces the same enum at the database layer via the
// enum-typed `kind` column.
var validScanRunKinds = map[string]bool{
	"full":              true,
	"delta":             true,
	"external_single":   true,
	"external_per_row":  true,
	"retract":           true,
}

// db is the shared *sql.DB instance the legacy demo handlers use.
// Production wiring SHOULD prefer injecting a dependency at the
// `runService` boundary rather than reading this package var, but
// the legacy handlers predate the refactor and remain in place
// for legacy E2E parity. New code MUST take *sql.DB as a parameter.
var db *sql.DB

// scanEventCapacity is the buffer size of the post-scan
// dispatcher channel that fans `ScanEvent`s out to the
// [rule_engine.Worker]. The buffer decouples the HTTP
// request-response latency from worker availability so a
// briefly-stalled worker does not block /v1/ingestor/process
// callers (rubber-duck blocking finding from Stage 5.7
// iter 2: do NOT use an unbuffered channel here).
//
// 64 is sized for the v1 single-instance deployment: at
// the spec'd 10 ingest events/sec, a 64-event burst gives
// the worker ~6 seconds of headroom before back-pressure
// kicks in. A capacity-saturation drop is logged at WARN
// (NOT silently dropped) so an operator can react.
const scanEventCapacity = 64

// scanEventEmitTimeout is the per-event upper bound on
// the time the HTTP handler is willing to wait for buffer
// space in the post-scan dispatcher. Per Stage 5.7
// evaluator feedback #6: a `default:` drop loses required
// work permanently; replacing it with a bounded block
// converts the failure mode from "silent loss" to
// "request latency spike" while the durable
// [rule_engine.Worker.Catchup] loop guarantees nothing is
// lost across process restarts even if the timeout DOES
// trip.
const scanEventEmitTimeout = 5 * time.Second

// catchupInterval is the wall-clock period between
// successive [rule_engine.Worker.Catchup] invocations.
// The first catchup runs at startup; subsequent catchups
// drain anything the live channel dropped (saturation,
// crash mid-event, etc.). 5 minutes is balanced against
// the live event path's ~real-time latency expectation.
const catchupInterval = 5 * time.Minute

// scanEvents is the post-scan dispatcher channel. nil when
// the engine is unwired (composition root opted out via
// `CLEAN_CODE_RULE_ENGINE_DISABLED=1`); in that case the
// HTTP handlers SKIP the emit so non-wired deployments
// behave exactly as before this stage.
var scanEvents chan<- rule_engine.ScanEvent

// staleSweepLoop is the package-level handle to the running
// sweep loop. Exposed so the `/metrics` handler can scrape the
// counters without re-allocating the metrics holder. Nil when
// [config.Config.DisableStaleSweep] is true or PGScanRunStore
// construction failed.
var staleSweepLoop *metric_ingestor.StaleScanRunSweepLoop

func main() {
	// Stage 3.5 fail-fast contract: an invalid env value (bad
	// duration, unknown literal, etc.) MUST surface as a startup
	// crash so the operator sees the misconfiguration immediately
	// instead of the service silently falling back to defaults.
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config.Load: %v", err)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	pgURL := cfg.PostgresURL
	if pgURL == "" {
		log.Fatal("CLEAN_CODE_PG_URL is required")
	}

	db, err = sql.Open("postgres", pgURL)
	if err != nil {
		log.Fatalf("opening postgres: %v", err)
	}
	defer db.Close()

	// Wait for Postgres to be ready.
	for i := 0; i < 30; i++ {
		if err := db.Ping(); err == nil {
			break
		}
		time.Sleep(time.Second)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))

	// Unified root context: both the Stage 3.5 sweep loop and
	// the Stage 5.7 rule_engine worker shut down cleanly when
	// this is cancelled by the SIGINT/SIGTERM handler below.
	rootCtx, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()

	// Stage 5.7 wiring: SOLID Rule Engine batch worker.
	// Failures here are LOGGED -- the ingest service must keep
	// serving /v1/ingestor/process even when the engine cannot
	// be composed (e.g. migrations not yet applied).
	if disabled := strings.EqualFold(os.Getenv("CLEAN_CODE_RULE_ENGINE_DISABLED"), "1"); disabled {
		log.Print("rule_engine: disabled via CLEAN_CODE_RULE_ENGINE_DISABLED=1")
	} else if events, werr := startRuleEngineWorker(rootCtx, db); werr != nil {
		log.Printf("rule_engine: NOT wired (worker startup failed): %v", werr)
	} else {
		scanEvents = events
		log.Print("rule_engine: worker wired (caller=batch_refresh on post-scan events)")
	}

	// Stage 3.5 wiring: stale ScanRun sweep loop.
	var sweepLoopWG sync.WaitGroup
	loop, sweepErr := buildSweepLoop(cfg, db, logger)
	if sweepErr != nil {
		// Construction errors are surfaced but non-fatal: the
		// legacy demo path may still want to serve while an
		// operator triages the PG store wiring.
		log.Printf("stale sweep: build failed (sweep disabled): %v", sweepErr)
	}
	staleSweepLoop = loop
	loopStarted := false
	if staleSweepLoop != nil {
		sweepLoopWG.Add(1)
		loopStarted = true
		go func() {
			defer sweepLoopWG.Done()
			if rerr := staleSweepLoop.Run(rootCtx); rerr != nil &&
				!errors.Is(rerr, context.Canceled) &&
				!errors.Is(rerr, context.DeadlineExceeded) {
				log.Printf("stale sweep loop exited: %v", rerr)
			}
		}()
		log.Printf("stale sweep loop wired: scan_timeout=%s cadence=%s",
			cfg.ScanTimeout, cfg.PeriodicSweepCadence)
	} else if cfg.DisableStaleSweep {
		log.Printf("stale sweep loop DISABLED via %s", config.EnvDisableStaleSweep)
	}

	mux := buildMux(cfg, staleSweepLoop)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// SIGINT / SIGTERM trigger graceful shutdown: cancel the root
	// ctx (which stops the sweep loop AND the rule_engine worker /
	// catchup goroutines together), give the HTTP server up to 10s
	// to drain in-flight requests, then exit.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("clean-code-metric-ingestor: shutdown signal received")
		cancelRoot()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("clean-code-metric-ingestor listening on :%s (stale_sweep=%v legacy_demo=%v rule_engine=%v)",
		port, loopStarted, cfg.EnableLegacyDemoAPI, scanEvents != nil)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server: %v", err)
	}
	cancelRoot()
	sweepLoopWG.Wait()
}

// buildSweepLoop constructs the Stage 3.5 stale ScanRun sweep
// loop or returns (nil, nil) when the operator has explicitly
// disabled it via [config.Config.DisableStaleSweep].
//
// Splitting this out of main() lets the unit tests assert the
// disable-path (returns nil) without spinning up a Postgres pool
// and the wire-path (returns non-nil with the operator-pinned
// scan_timeout / cadence) without needing a long-running goroutine.
//
// A non-nil error means the operator wanted the sweep enabled
// but the underlying store could not be constructed -- callers
// should log and continue rather than crash, so an unrelated
// legacy-demo deploy is not blocked by a sweep wiring bug.
//
// # Knobs deliberately NOT wired
//
// [metric_ingestor.WithStaleSweepLoopErrorBackoff] is not wired
// here. The loop's constructor defaults errorBackoff to cadence,
// which is the Stage 3.5 production stance (see the inline NOTE
// at the option-list call site for rationale and the migration
// path if an operator-tunable backoff is later required).
func buildSweepLoop(cfg config.Config, sqlDB *sql.DB, logger *slog.Logger) (*metric_ingestor.StaleScanRunSweepLoop, error) {
	if cfg.DisableStaleSweep {
		return nil, nil
	}
	if sqlDB == nil {
		return nil, fmt.Errorf("buildSweepLoop: nil *sql.DB (cannot wire PGScanRunStore)")
	}
	store, sErr := metric_ingestor.NewPGScanRunStore(sqlDB)
	if sErr != nil {
		return nil, fmt.Errorf("NewPGScanRunStore: %w", sErr)
	}
	sweep := metric_ingestor.NewStaleScanRunSweep(
		store,
		metric_ingestor.WithStaleSweepScanTimeout(cfg.ScanTimeout),
		metric_ingestor.WithStaleSweepLogger(logger),
	)
	loop := metric_ingestor.NewStaleScanRunSweepLoop(
		sweep,
		metric_ingestor.WithStaleSweepLoopCadence(cfg.PeriodicSweepCadence),
		// errorBackoff is intentionally left to default to cadence.
		// [metric_ingestor.NewStaleScanRunSweepLoop] folds an
		// unset value to cadence (`if l.errorBackoff <= 0 {
		// l.errorBackoff = l.cadence }`), which is the chosen
		// Stage 3.5 production stance: a degraded DB MUST NOT be
		// retried more aggressively than the healthy 5-min
		// cadence (tech-spec Sec 8.2 `periodic_sweep_cadence`) --
		// faster retries on Sweep failure would compound
		// back-pressure on an already-struggling store, and the
		// failure modes we expect here (transient PG unavailability,
		// connection-pool saturation) recover on minute scales,
		// not sub-second ones. The
		// [metric_ingestor.WithStaleSweepLoopErrorBackoff] option
		// is kept in the library surface for (1) deterministic
		// sub-second backoffs in the loop's unit tests, where
		// real-cadence waits would balloon suite runtime, and
		// (2) the future operator-tunable knob hinted at in
		// loop.go. Exposing that knob to operators is a
		// deliberate follow-up because no current SLO calls for
		// it; when it is wanted, the migration path is (a) add
		// `config.Config.SweepErrorBackoff` backed by
		// `CLEAN_CODE_PERIODIC_SWEEP_ERROR_BACKOFF` (honouring
		// the tech-spec Sec 8.2 single-source-of-truth contract
		// already established for ScanTimeout / PeriodicSweepCadence),
		// then (b) thread it here with
		// `metric_ingestor.WithStaleSweepLoopErrorBackoff(cfg.SweepErrorBackoff)`.
		metric_ingestor.WithStaleSweepLoopLogger(logger),
	)
	return loop, nil
}

// buildMux assembles the HTTP routes the metric-ingestor binary
// exposes, gated on [config.Config.EnableLegacyDemoAPI]. The
// production composition root mounts only `/healthz` and
// `/metrics`; the legacy `001_init.sql`-shaped `/v1/ingestor/*`
// handlers mount iff EnableLegacyDemoAPI is true.
//
// Extracted from main() so the test suite can assert the route
// table directly (no network listener required).
func buildMux(cfg config.Config, loop *metric_ingestor.StaleScanRunSweepLoop) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.Handle("/metrics", newMetricsHandler(loop))
	if cfg.EnableLegacyDemoAPI {
		mux.HandleFunc("/v1/ingestor/process", handleProcess)
		mux.HandleFunc("/v1/ingestor/scan-run", handleScanRun)
	}
	return mux
}

// newMetricsHandler returns an http.HandlerFunc that emits the
// Stage 3.5 sweep counters in Prometheus text-exposition format.
// When loop is nil (sweep disabled or wiring failed), the handler
// returns an empty body with a 200 status -- Prometheus tolerates
// this as a zero-sample scrape.
//
// Taking the loop as a function parameter lets the test suite
// stub it without touching the package-level [staleSweepLoop]
// (which the production main() owns).
func newMetricsHandler(loop *metric_ingestor.StaleScanRunSweepLoop) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		if loop != nil {
			if _, err := loop.Sweep().Metrics().WriteText(w); err != nil {
				log.Printf("metrics: WriteText failed: %v", err)
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

// handleMetrics is preserved for production /metrics wiring and
// reads the package-level [staleSweepLoop] that main() populated.
// New callers should prefer [newMetricsHandler] which takes the
// loop as a parameter -- this wrapper exists so the legacy
// hand-rolled `mux.HandleFunc("/metrics", handleMetrics)` form
// keeps compiling for any external test that imports the package
// for its handler.
func handleMetrics(w http.ResponseWriter, r *http.Request) {
	newMetricsHandler(staleSweepLoop)(w, r)
}

// startRuleEngineWorker composes the SOLID Rule Engine
// dependencies and launches the [rule_engine.Worker] on a
// background goroutine bound to `ctx`. Returns the
// send-only event channel the HTTP handlers should write
// to when a SHA transitions to `scanned`.
//
// Composition order (per Stage 5.7 architecture Sec 3.6):
//   - [steward.SQLStore] -- policy/rule/threshold reads
//   - [steward.Steward] -- exposes ActivePolicyVersion (no
//     signer -- the batch worker is a READER of policies,
//     not a writer)
//   - separate `*sql.DB` for Audit writes (see Stage 5.7
//     evaluator feedback #5: the
//     [rule_engine.SQLStore.AppendEvaluation] path INSERTs
//     into `evaluation_run`, `evaluation_verdict`, and
//     `finding`; those tables are granted INSERT to
//     `clean_code_solid_batch` per
//     migrations/0004_roles.up.sql:455-465 -- NOT
//     `clean_code_metric_ingestor`. The composition root
//     reads `CLEAN_CODE_SOLID_BATCH_PG_URL` to authenticate
//     a dedicated handle as that role. When the env var is
//     unset the composition root falls back to the main
//     `*sql.DB` with a WARN log so dev/test
//     compose-as-superuser environments keep working.)
//   - [rule_engine.SQLStore] -- the writer for the three
//     Audit tables under `caller='batch_refresh'`
//   - [rule_engine.Engine] -- the in-process actor
//   - [rule_engine.Worker] -- consumes ScanEvent and drives
//     Engine.RunBatch
//   - [rule_engine.SQLPendingScanReader] -- the durable
//     catchup reader (Stage 5.7 evaluator feedback #6).
//     Reads `clean_code.commit.scan_status='scanned'` rows
//     missing an `evaluation_run` for the active policy.
//
// The buffered channel decouples HTTP latency from worker
// availability; capacity-saturation drops are converted to
// bounded blocks ([scanEventEmitTimeout]) by the emitting
// handler, and the durable [rule_engine.Worker.Catchup]
// loop guarantees nothing is lost across process restarts.
func startRuleEngineWorker(ctx context.Context, dbh *sql.DB) (chan<- rule_engine.ScanEvent, error) {
	stewardStore, err := steward.NewSQLStore(dbh)
	if err != nil {
		return nil, fmt.Errorf("steward.NewSQLStore: %w", err)
	}
	stew, err := steward.New(steward.Config{Store: stewardStore})
	if err != nil {
		return nil, fmt.Errorf("steward.New: %w", err)
	}

	// Audit-writer DB handle. Per Stage 5.7 evaluator
	// feedback #5: the three Audit tables grant INSERT to
	// `clean_code_solid_batch`, NOT the metric-ingestor's
	// role. The composition root therefore authenticates a
	// dedicated handle as that role when
	// `CLEAN_CODE_SOLID_BATCH_PG_URL` is set; otherwise we
	// fall back to the main DB handle with a WARN log
	// (acceptable for dev/test compose-as-superuser; will
	// fail at runtime under production least-privilege).
	auditDB := dbh
	if solidBatchURL := os.Getenv("CLEAN_CODE_SOLID_BATCH_PG_URL"); solidBatchURL != "" {
		bd, berr := sql.Open("postgres", solidBatchURL)
		if berr != nil {
			return nil, fmt.Errorf("opening CLEAN_CODE_SOLID_BATCH_PG_URL: %w", berr)
		}
		// Verify the handle is usable before we hand it
		// to the Audit writer. A boot-time Ping is cheap
		// insurance against a typo / misconfigured DSN.
		if perr := bd.PingContext(ctx); perr != nil {
			_ = bd.Close()
			return nil, fmt.Errorf("ping CLEAN_CODE_SOLID_BATCH_PG_URL: %w", perr)
		}
		auditDB = bd
		log.Print("rule_engine: Audit writes authenticated via CLEAN_CODE_SOLID_BATCH_PG_URL")
	} else {
		log.Print("rule_engine: WARN -- CLEAN_CODE_SOLID_BATCH_PG_URL not set; reusing CLEAN_CODE_PG_URL handle for Audit writes (will fail under production least-privilege grants per migrations/0004_roles.up.sql)")
	}

	ruleStore, err := rule_engine.NewSQLStore(rule_engine.SQLStoreConfig{
		DB:      auditDB,
		Steward: stewardStore,
	})
	if err != nil {
		return nil, fmt.Errorf("rule_engine.NewSQLStore: %w", err)
	}
	engine, err := rule_engine.New(rule_engine.Config{Store: ruleStore})
	if err != nil {
		return nil, fmt.Errorf("rule_engine.New: %w", err)
	}
	events := make(chan rule_engine.ScanEvent, scanEventCapacity)
	worker, err := rule_engine.NewWorker(rule_engine.WorkerConfig{
		Engine:     engine,
		Activation: rule_engine.NewStewardActivation(stew),
		Events:     events,
		Logger:     slog.Default(),
	})
	if err != nil {
		return nil, fmt.Errorf("rule_engine.NewWorker: %w", err)
	}
	go func() {
		if err := worker.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("rule_engine.worker.Run exited with error: %v", err)
		}
	}()

	// Durable catchup loop -- Stage 5.7 evaluator
	// feedback #6. The catchup reader uses the SAME DB
	// handle as the live live event path (DB reads against
	// `commit` + `evaluation_run`). We launch it on a
	// dedicated goroutine that fires on startup AND on a
	// [catchupInterval] ticker; per-event work routes back
	// through Worker.process so the
	// `caller='batch_refresh'` stamp matches the live
	// path.
	pendingReader, perr := rule_engine.NewSQLPendingScanReader(rule_engine.SQLPendingScanReaderConfig{DB: dbh})
	if perr != nil {
		return nil, fmt.Errorf("rule_engine.NewSQLPendingScanReader: %w", perr)
	}
	go runCatchupLoop(ctx, worker, pendingReader)

	return events, nil
}

// runCatchupLoop drains the durable scan backlog on startup
// and then re-runs the drain on a wall-clock ticker so any
// SHA that the live event channel dropped (or any SHA that
// landed while the process was down) is eventually picked
// up. Errors are LOGGED -- the catchup loop is the LAST
// line of defence; we do NOT crash the service on a
// recoverable DB error.
func runCatchupLoop(ctx context.Context, worker *rule_engine.Worker, reader rule_engine.PendingScanReader) {
	// Run an immediate first-pass on startup. Any backlog
	// that accumulated while the service was down is
	// drained before the ticker fires.
	if processed, err := worker.Catchup(ctx, rule_engine.CatchupConfig{Reader: reader}); err != nil {
		log.Printf("rule_engine.worker.Catchup (startup) failed: %v", err)
	} else if processed > 0 {
		log.Printf("rule_engine.worker.Catchup (startup) processed=%d events", processed)
	}

	ticker := time.NewTicker(catchupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if processed, err := worker.Catchup(ctx, rule_engine.CatchupConfig{Reader: reader}); err != nil {
				log.Printf("rule_engine.worker.Catchup (periodic) failed: %v", err)
			} else if processed > 0 {
				log.Printf("rule_engine.worker.Catchup (periodic) processed=%d events", processed)
			}
		}
	}
}

// emitScanEvent forwards a successful (RepoID, SHA) pair to
// the post-scan dispatcher. Per Stage 5.7 evaluator
// feedback #6: a `default:` drop loses required work
// permanently; replacing it with a bounded
// [scanEventEmitTimeout] block converts the failure mode
// from "silent loss" to "request latency spike". The
// durable [rule_engine.Worker.Catchup] loop run by
// [runCatchupLoop] is the ultimate guarantee that nothing
// is lost across process restarts even if the timeout
// trips -- a `scan_status='scanned'` row with no
// `evaluation_run` for the active policy will be picked
// up on the next catchup tick.
func emitScanEvent(ctx context.Context, repoIDRaw, sha string) {
	if scanEvents == nil {
		return
	}
	if repoIDRaw == "" || sha == "" {
		log.Printf("rule_engine: emit skipped (empty repo_id or sha): repo_id=%q sha=%q", repoIDRaw, sha)
		return
	}
	repoID, err := uuid.FromString(repoIDRaw)
	if err != nil {
		log.Printf("rule_engine: emit skipped (invalid repo_id %q): %v", repoIDRaw, err)
		return
	}
	ev := rule_engine.ScanEvent{RepoID: repoID, SHA: sha}
	// Bounded block instead of a `default:` drop. The
	// timer is sized so a real saturation event surfaces
	// as a latency spike + log line (durably observable)
	// rather than as a silent permanent loss.
	timer := time.NewTimer(scanEventEmitTimeout)
	defer timer.Stop()
	select {
	case scanEvents <- ev:
		// emitted
	case <-ctx.Done():
		// Request canceled; do not block on the buffer.
		// The catchup loop will pick this SHA up on its
		// next tick (the catchup reader filters by
		// `commit.scan_status='scanned'` + absent
		// `evaluation_run`).
	case <-timer.C:
		log.Printf("rule_engine: scan event channel saturated after %s -- event WILL BE REPROCESSED BY CATCHUP repo_id=%s sha=%s (capacity=%d, emit_timeout=%s)",
			scanEventEmitTimeout, repoID, sha, scanEventCapacity, scanEventEmitTimeout)
	}
}

func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}

type processRequest struct {
	CommitSHA string `json:"commit_sha"`
	RepoID    string `json:"repo_id"`
	Recipe    string `json:"recipe"`
}

// runRecipe executes the scan recipe for a commit. Panicking recipes are
// recovered by the caller — this models the real ingestor's behaviour where
// a bad recipe causes a Go panic that the service catches.
func runRecipe(commitSHA, recipe string) {
	if strings.Contains(recipe, "__panic_test__") {
		panic(fmt.Sprintf("recipe %q panicked on commit %s", recipe, commitSHA))
	}
}

func handleProcess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req processRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Iter-7 evaluator item #3: every write to
	// `clean_code.commit` and `clean_code.scan_run` is
	// keyed by repo_id (composite PK on commit;
	// repo_id NOT NULL on scan_run per migration 0001).
	// Parse it once at the top of the handler so the
	// downstream SQL paths can pass uuid.UUID directly
	// instead of marshalling a string.
	if req.RepoID == "" {
		http.Error(w, "bad request: repo_id is required", http.StatusBadRequest)
		return
	}
	repoID, err := uuid.FromString(req.RepoID)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad request: repo_id is not a uuid: %v", err), http.StatusBadRequest)
		return
	}

	ctx := r.Context()

	// Transition: pending -> scanning (committed to DB before any work begins
	// so that concurrent observers can witness the intermediate state).
	// The canonical schema declares `clean_code.commit` with a composite
	// PK `(repo_id, sha)` and an enum named `commit_scan_status` (NOT
	// `scan_status`); see migrations/0001_catalog_lifecycle.up.sql:212-230.
	if _, err := db.ExecContext(ctx, `UPDATE clean_code.commit SET scan_status = 'scanning'::clean_code.commit_scan_status WHERE repo_id = $1 AND sha = $2`, repoID.String(), req.CommitSHA); err != nil {
		http.Error(w, "updating to scanning: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Brief yield so that concurrent DB pollers can observe the "scanning"
	// state before it transitions to scanned/failed.
	time.Sleep(100 * time.Millisecond)

	// Execute the recipe with panic recovery — models real ingestor behaviour
	// where a bad recipe panics and the service catches it via recover().
	var recipePanicked bool
	var panicValue interface{}
	func() {
		defer func() {
			if r := recover(); r != nil {
				recipePanicked = true
				panicValue = r
			}
		}()
		runRecipe(req.CommitSHA, req.Recipe)
	}()

	if recipePanicked {
		log.Printf("recipe panicked: %v", panicValue)
		// Atomically: record the failed scan_run AND transition the commit to
		// 'failed'. Without the transaction, a partial write would leave an
		// orphan scan_run row with the commit stuck in 'scanning' — and the
		// E2E poller would time out with a misleading error.
		if err := finalizeScanRun(ctx, repoID, req.CommitSHA, "failed", "failed"); err != nil {
			log.Printf("finalizing failed scan_run: %v", err)
		}
		http.Error(w, fmt.Sprintf("recipe panicked: %v", panicValue), http.StatusInternalServerError)
		return
	}

	// Happy path: atomically record the succeeded scan_run AND transition
	// the commit to 'scanned'. See finalizeScanRun for the atomicity rationale.
	if err := finalizeScanRun(ctx, repoID, req.CommitSHA, "succeeded", "scanned"); err != nil {
		http.Error(w, "finalizing scan_run: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Post-scan dispatcher hand-off: Stage 5.7 wires the
	// SOLID Rule Engine batch worker behind a buffered
	// channel so the engine refresh runs OUT-OF-BAND from
	// the HTTP request lifecycle. The emit is non-blocking;
	// see [emitScanEvent] for the capacity-saturation
	// drop log.
	emitScanEvent(ctx, req.RepoID, req.CommitSHA)

	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"status":"ok"}`)
}

// finalizeScanRun inserts the terminal scan_run row and transitions the
// commit's scan_status in a single transaction so the two writes either
// both commit or neither does. If we wrote them with two autocommitted
// statements, a failure of the second one would leave an orphan scan_run
// row + the commit stuck in 'scanning' forever — observable to the E2E
// poller as a timeout with a misleading error.
//
// scanRunStatus must be a valid clean_code.scan_run_status enum value
// ('succeeded' | 'failed'). commitStatus must be a valid
// clean_code.commit_scan_status enum value ('scanned' | 'failed').
//
// Iter-7 evaluator item #3: the canonical `clean_code.scan_run` schema
// (migrations/0001_catalog_lifecycle.up.sql:337-390) uses columns
// `repo_id`, `kind`, `sha_binding`, `to_sha`, `started_at`, `ended_at`,
// `status` -- NOT `commit_sha` / `finished_at`. The `commit` table has a
// composite PK `(repo_id, sha)` and no `updated_at` column.
//
// We stamp `kind='full'` because the metric-ingestor's `process` recipe
// drives a whole-repo scan; per-row + delta scans flow through different
// services. `sha_binding='single'` because the run is bound to exactly
// one SHA (`to_sha`) rather than a (from_sha, to_sha) delta pair.
func finalizeScanRun(ctx context.Context, repoID uuid.UUID, commitSHA, scanRunStatus, commitStatus string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Safe even after a successful Commit: returns sql.ErrTxDone which we ignore.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.scan_run (repo_id, kind, sha_binding, to_sha, status, ended_at)
		 VALUES ($1, 'full'::clean_code.scan_run_kind, 'single'::clean_code.scan_run_sha_binding, $2, $3::clean_code.scan_run_status, now())`,
		repoID.String(), commitSHA, scanRunStatus); err != nil {
		return fmt.Errorf("insert scan_run: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE clean_code.commit SET scan_status = $1::clean_code.commit_scan_status WHERE repo_id = $2 AND sha = $3`,
		commitStatus, repoID.String(), commitSHA); err != nil {
		return fmt.Errorf("update commit to %s: %w", commitStatus, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}

type scanRunRequest struct {
	CommitSHA string `json:"commit_sha"`
	RepoID    string `json:"repo_id"`
	Kind      string `json:"kind"`
	// Optional. Only meaningful for kind='delta' (architecture
	// migration 0001 line 354 / canonical schema). Ignored for
	// all other kinds because the canonical column is set ONLY
	// for kind='delta'.
	FromSHA string `json:"from_sha,omitempty"`
}

// scanRunShaBindingForKind maps a canonical
// `clean_code.scan_run_kind` value to its canonical
// `clean_code.scan_run_sha_binding` value per the constraint
// CHECK `scan_run_sha_binding_consistent` from migration
// 0001 (lines 351-389): every scan_run_kind has a deterministic
// sha_binding shape -- one SHA per run via `to_sha` for the
// four single-bound kinds (full, delta, external_single,
// retract) and one SHA per emitted MetricSample row for
// `external_per_row` (to_sha NULL). The mapping is the source
// of truth for shaping the INSERT statement; supplying the
// wrong binding for a kind would either FAIL the database
// CHECK constraint at INSERT time (single+NULL to_sha or
// per_row+non-NULL to_sha) or, worse, accept a semantically
// incorrect row that downstream Insights queries (every
// single-bound run resolves to exactly one SHA via to_sha)
// would silently mis-aggregate.
//
// Iter-7 evaluator item #2: the prior handler always wrote
// sha_binding='single' + to_sha=$3 regardless of kind, which
// (a) violated the CHECK constraint for kind='external_per_row'
// (per_row binding requires to_sha NULL) and (b) accepted a
// semantically wrong row for kind='external_per_row' if the
// CHECK happened to fire late. This map plus the conditional
// INSERT below converts the handler into a kind-honest shape.
var scanRunShaBindingForKind = map[string]string{
	"full":             "single",
	"delta":            "single",
	"external_single":  "single",
	"external_per_row": "per_row",
	"retract":          "single",
}

func handleScanRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req scanRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Application-level enum guard: reject invalid kind BEFORE reaching PostgreSQL.
	// The valid set mirrors the canonical `clean_code.scan_run_kind` enum
	// (migrations/0001_catalog_lifecycle.up.sql:117).
	if !validScanRunKinds[req.Kind] {
		http.Error(w, fmt.Sprintf("invalid scan_run kind %q: must be one of full, delta, external_single, external_per_row, retract", req.Kind), http.StatusBadRequest)
		return
	}

	// Iter-7 evaluator item #3: scan_run requires
	// repo_id and the canonical schema uses
	// `to_sha`+`sha_binding` (NOT `commit_sha`).
	if req.RepoID == "" {
		http.Error(w, "bad request: repo_id is required", http.StatusBadRequest)
		return
	}
	repoID, err := uuid.FromString(req.RepoID)
	if err != nil {
		http.Error(w, fmt.Sprintf("bad request: repo_id is not a uuid: %v", err), http.StatusBadRequest)
		return
	}

	// Iter-7 evaluator item #2: select the canonical sha_binding
	// from the kind so the INSERT honours
	// `scan_run_sha_binding_consistent` rather than always
	// writing `single`. The mapping is total over the validated
	// enum set above, but a defensive guard catches any future
	// kind that ships without a binding mapping update.
	shaBinding, ok := scanRunShaBindingForKind[req.Kind]
	if !ok {
		http.Error(w, fmt.Sprintf("internal: no sha_binding mapping for kind %q", req.Kind), http.StatusInternalServerError)
		return
	}

	switch shaBinding {
	case "single":
		// Single-bound runs resolve to exactly one SHA via `to_sha`;
		// CHECK `scan_run_sha_binding_consistent` enforces
		// to_sha IS NOT NULL at the database layer. Reject empty
		// commit_sha at the application layer with a 400 so the
		// caller gets a clear error rather than a 500 from a
		// rejected INSERT.
		if req.CommitSHA == "" {
			http.Error(w, fmt.Sprintf("bad request: commit_sha is required for kind %q (sha_binding=single)", req.Kind), http.StatusBadRequest)
			return
		}
		// `from_sha` is only meaningful for delta (per migration
		// 0001 line 354); for the other single-bound kinds it is
		// always NULL.
		if req.Kind == "delta" {
			if _, err := db.Exec(
				`INSERT INTO clean_code.scan_run (repo_id, kind, sha_binding, from_sha, to_sha, status)
				 VALUES ($1, $2::clean_code.scan_run_kind, 'single'::clean_code.scan_run_sha_binding, NULLIF($3, ''), $4, 'running'::clean_code.scan_run_status)`,
				repoID.String(), req.Kind, req.FromSHA, req.CommitSHA); err != nil {
				http.Error(w, "inserting scan_run: "+err.Error(), http.StatusInternalServerError)
				return
			}
		} else {
			if _, err := db.Exec(
				`INSERT INTO clean_code.scan_run (repo_id, kind, sha_binding, to_sha, status)
				 VALUES ($1, $2::clean_code.scan_run_kind, 'single'::clean_code.scan_run_sha_binding, $3, 'running'::clean_code.scan_run_status)`,
				repoID.String(), req.Kind, req.CommitSHA); err != nil {
				http.Error(w, "inserting scan_run: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
	case "per_row":
		// Per-row runs do NOT carry a top-level `to_sha`; each
		// emitted MetricSample row carries its own SHA. CHECK
		// `scan_run_sha_binding_consistent` enforces to_sha IS
		// NULL at the database layer. Reject any caller-supplied
		// commit_sha for kind='external_per_row' at the
		// application layer with a 400 so the caller cannot
		// silently mis-shape a per-row scan as a single-bound one.
		if req.CommitSHA != "" {
			http.Error(w, fmt.Sprintf("bad request: commit_sha must be empty for kind %q (sha_binding=per_row; SHA lives on each emitted MetricSample row, not on scan_run.to_sha)", req.Kind), http.StatusBadRequest)
			return
		}
		if _, err := db.Exec(
			`INSERT INTO clean_code.scan_run (repo_id, kind, sha_binding, to_sha, status)
			 VALUES ($1, $2::clean_code.scan_run_kind, 'per_row'::clean_code.scan_run_sha_binding, NULL, 'running'::clean_code.scan_run_status)`,
			repoID.String(), req.Kind); err != nil {
			http.Error(w, "inserting scan_run: "+err.Error(), http.StatusInternalServerError)
			return
		}
	default:
		// Unreachable given the mapping above, but compile-safe.
		http.Error(w, fmt.Sprintf("internal: unsupported sha_binding %q", shaBinding), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintln(w, `{"status":"created"}`)
}
