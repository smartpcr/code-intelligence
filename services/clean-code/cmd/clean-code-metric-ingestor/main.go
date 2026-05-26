// Package main is the entrypoint for the clean-code-metric-ingestor service.
// It processes commits through scan recipes and manages the ScanRun lifecycle.
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
// # Production vs legacy demo
//
// The production composition root mounts ONLY the canonical surface:
// `/healthz`, `/metrics`, and the in-process sweep goroutine. Both
// `/v1/ingestor/process` and `/v1/ingestor/scan-run` are LEGACY DEMO
// handlers that write the older `001_init.sql`
// `scan_run(commit_sha,kind,status,finished_at)` shape and are mounted
// iff [config.Config.EnableLegacyDemoAPI] is true (env
// `CLEAN_CODE_ENABLE_LEGACY_DEMO_API`). The Stage 1.2 canonical schema
// (`0001_catalog_lifecycle`) does not expose those columns; running the
// legacy demo against the canonical schema is a wiring error.
//
// # Config contract
//
// All `CLEAN_CODE_*` env vars are read EXCLUSIVELY by `internal/config`
// (per the package doc lines 15-16). This binary reads only
// [config.Config] fields; an invalid env value is a HARD ERROR at
// startup (no silent fallback to defaults).
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

	_ "github.com/lib/pq"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/config"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
)

// validScanRunKinds enumerates the allowed scan_run kind values
// the legacy demo `/v1/ingestor/scan-run` handler accepts. It is
// scoped to the legacy demo only -- the canonical Stage 1.2 schema
// uses an enum-typed `kind` column whose constraint is enforced
// by PostgreSQL. The ingestor MUST reject any kind not in this set
// before reaching PostgreSQL.
var validScanRunKinds = map[string]bool{
	"ast_metrics": true,
	"lint":        true,
	"complexity":  true,
	"dependency":  true,
}

// db is the shared *sql.DB instance the legacy demo handlers use.
// Production wiring SHOULD prefer injecting a dependency at the
// `runService` boundary rather than reading this package var, but
// the legacy handlers predate the refactor and remain in place
// for legacy E2E parity. New code MUST take *sql.DB as a parameter.
var db *sql.DB

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

	sweepLoopCtx, cancelSweep := context.WithCancel(context.Background())
	defer cancelSweep()

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
			if rerr := staleSweepLoop.Run(sweepLoopCtx); rerr != nil &&
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

	// SIGINT / SIGTERM trigger graceful shutdown: cancel the sweep
	// ctx, give the HTTP server up to 10s to drain in-flight
	// requests, then exit.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigs
		log.Print("clean-code-metric-ingestor: shutdown signal received")
		cancelSweep()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	log.Printf("clean-code-metric-ingestor listening on :%s (stale_sweep=%v legacy_demo=%v)",
		port, loopStarted, cfg.EnableLegacyDemoAPI)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("http server: %v", err)
	}
	cancelSweep()
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

	ctx := r.Context()

	// Transition: pending -> scanning (committed to DB before any work begins
	// so that concurrent observers can witness the intermediate state).
	if _, err := db.ExecContext(ctx, `UPDATE clean_code.commit SET scan_status = 'scanning'::clean_code.scan_status, updated_at = now() WHERE sha = $1`, req.CommitSHA); err != nil {
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
		if err := finalizeScanRun(ctx, req.CommitSHA, "failed", "failed"); err != nil {
			log.Printf("finalizing failed scan_run: %v", err)
		}
		http.Error(w, fmt.Sprintf("recipe panicked: %v", panicValue), http.StatusInternalServerError)
		return
	}

	// Happy path: atomically record the succeeded scan_run AND transition
	// the commit to 'scanned'. See finalizeScanRun for the atomicity rationale.
	if err := finalizeScanRun(ctx, req.CommitSHA, "succeeded", "scanned"); err != nil {
		http.Error(w, "finalizing scan_run: "+err.Error(), http.StatusInternalServerError)
		return
	}

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
// clean_code.scan_status enum value ('scanned' | 'failed').
func finalizeScanRun(ctx context.Context, commitSHA, scanRunStatus, commitStatus string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Safe even after a successful Commit: returns sql.ErrTxDone which we ignore.
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.scan_run (commit_sha, kind, status, finished_at) VALUES ($1, 'ast_metrics'::clean_code.scan_run_kind, $2::clean_code.scan_run_status, now())`,
		commitSHA, scanRunStatus); err != nil {
		return fmt.Errorf("insert scan_run: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE clean_code.commit SET scan_status = $2::clean_code.scan_status, updated_at = now() WHERE sha = $1`,
		commitSHA, commitStatus); err != nil {
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
	if !validScanRunKinds[req.Kind] {
		http.Error(w, fmt.Sprintf("invalid scan_run kind %q: must be one of ast_metrics, lint, complexity, dependency", req.Kind), http.StatusBadRequest)
		return
	}

	if _, err := db.Exec(`INSERT INTO clean_code.scan_run (commit_sha, kind, status) VALUES ($1, $2::clean_code.scan_run_kind, 'running'::clean_code.scan_run_status)`,
		req.CommitSHA, req.Kind); err != nil {
		http.Error(w, "inserting scan_run: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusCreated)
	fmt.Fprintln(w, `{"status":"created"}`)
}
