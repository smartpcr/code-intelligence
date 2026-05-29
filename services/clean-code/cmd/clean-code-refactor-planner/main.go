// Package main is the entrypoint for the clean-code-refactor-planner
// service (Stage 8.1 + Stage 8.2, architecture Sec 5.5 / tech-spec
// Sec 7.4). The binary composes both passes into ONE one-shot
// invocation per (repo_id, sha):
//
//  1. Stage 8.1 [refactor.Planner.Plan] -- reads the active
//     [steward.PolicyVersion] + metric_sample + finding rows,
//     computes scores, and writes the `clean_code.hot_spot` batch.
//  2. Stage 8.2 [refactor.TaskPlanner.PlanFromSnapshot] -- reads
//     the top-N hot_spot rows we just wrote, batches their
//     qualifying finding details, and writes the
//     `clean_code.refactor_plan` row + N `clean_code.refactor_task`
//     rows in one transaction.
//
// # Race-safe wiring: PlanFromSnapshot
//
// The Stage 8.2 pass uses [refactor.TaskPlanner.PlanFromSnapshot]
// rather than [refactor.TaskPlanner.Plan]. This is critical: if
// Stage 8.2 re-read the active policy, a concurrent
// `policy.activate` between the two passes could return a
// different `policy_version_id` than the one already stamped on
// every hot_spot the Stage 8.1 pass just wrote. The resulting
// refactor_plan would reference hot_spot rows whose policy_version
// does not match -- the architecture Sec 5.5.1 reproducibility
// invariant is violated. Passing the Stage 8.1 [PlanResult.Snapshot]
// closes the race at the type level (rubber-duck iter-2 finding #1).
//
// # One-shot K8s Job semantics
//
// Unlike the aggregator (a long-running cadence loop), the
// refactor-planner is a per-(repo, sha) JOB: the operator
// schedules it via a Kubernetes Job (typically tied to a scan_run
// completion event), the binary runs the two passes, and exits.
// There is no cadence loop. This matches the architecture's
// "refactor pass runs after the scan completes for a repo+sha"
// flow.
//
// # Composition root
//
// This binary is the ONLY process that should run under the
// `clean_code_refactor_planner` Postgres role in production. Per
// migration `0004_roles.up.sql` lines 482-509 the role has:
//
//   - SELECT on `metric_sample`, `metric_sample_active`,
//     `finding`, `policy_version` (read side -- needs the
//     same view the Stage 8.1 Planner Stage 8.1 reader uses).
//   - INSERT, SELECT on `hot_spot`, `refactor_plan`,
//     `refactor_task` (write side -- the SOLE writer of all
//     three per architecture G1 / Phase 1.6 grants).
//   - Explicit REVOKE UPDATE, DELETE on `refactor_plan` +
//     `refactor_task` so the row-immutability invariant
//     (architecture G6) survives an application-layer bug.
//
// # Env vars
//
// The binary reads:
//
//   - [config.EnvPGURL] -- the libpq DSN. Required.
//   - [EnvRepoID] -- the repo_id UUID. Required.
//   - [EnvSHA] -- the commit sha. Required.
//   - [EnvDisableRefactorPlanner] -- operator opt-out for staging
//     environments that don't have the hot_spot/refactor_plan
//     schema. Default false (planner runs). When true, the
//     binary serves /healthz-only and exits cleanly without
//     touching the DB.
//
// Exit codes:
//
//   - 0 -- the two-pass plan completed (or the planner was
//     opted out and /healthz served).
//   - 1 -- a fatal configuration, DB, or planner error.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/config"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
)

const (
	// EnvRepoID is the canonical env var the operator sets on
	// the K8s Job spec to scope the planner to ONE repo. The
	// value MUST parse as a UUID; a malformed value fails
	// fast at startup.
	EnvRepoID = "CLEAN_CODE_REFACTOR_PLANNER_REPO_ID"

	// EnvSHA is the canonical env var the operator sets on the
	// K8s Job spec to scope the planner to ONE commit. Empty
	// values fail fast at startup.
	EnvSHA = "CLEAN_CODE_REFACTOR_PLANNER_SHA"

	// EnvDisableRefactorPlanner is the explicit operator
	// opt-out for the Stage 8.1 + Stage 8.2 passes. Default
	// false. When true, the planner is SKIPPED entirely and
	// the binary serves /healthz-only. Intended for staging
	// environments that don't have the hot_spot /
	// refactor_plan / refactor_task schema yet.
	EnvDisableRefactorPlanner = "CLEAN_CODE_DISABLE_REFACTOR_PLANNER"

	// EnvHTTPServerMode toggles the multi-shot HTTP server
	// mode. When set to a truthy value the binary does NOT
	// require [EnvRepoID] / [EnvSHA] at startup and instead
	// listens for POST /v1/planner/run requests. Each request
	// supplies its own (repo_id, sha) target in the JSON body
	// and the handler invokes [executeTwoPassPlan]. The PG
	// handle + [refactor.EffortModel] are constructed ONCE at
	// boot and shared across requests so per-request latency
	// is bounded by Postgres round-trips, not model loading.
	//
	// This is the E2E + integration entrypoint: it lets the
	// docker-compose stack hold a long-running planner that
	// the test harness can drive without spawning a K8s Job
	// per scenario.
	EnvHTTPServerMode = "CLEAN_CODE_REFACTOR_PLANNER_HTTP"
)

func main() {
	exitCode := run()
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// run is the testable body of main. Returns the desired process
// exit code so unit tests can assert on the env-validation +
// startup branches without invoking os.Exit.
func run() int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	cfg, err := config.Load()
	if err != nil {
		log.Printf("clean-code-refactor-planner: config.Load: %v", err)
		return 1
	}
	if cfg.PostgresURL == "" {
		log.Printf("clean-code-refactor-planner: %s is required", config.EnvPGURL)
		return 1
	}

	// Stage 8.3: ML effort-model loading happens lazily inside
	// runPlanner / runHTTPMode via refactor.NewEffortModelFromConfig
	// (the canonical entrypoint after PR #148's rename).
	// Previously a startup-time refactor.LoadFromConfig block lived
	// here, but that API was removed and the per-request loader
	// is the only supported path. Removed: 2026-05 to unblock the
	// per-iter build gate (the dead block referenced
	// `cfg.RefactorEffortModelURI` which was renamed to
	// `cfg.MLModelURI` in the same refactor).

	disabled := parseBoolEnv(os.Getenv(EnvDisableRefactorPlanner))
	httpMode := parseBoolEnv(os.Getenv(EnvHTTPServerMode))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Wire the cancel chain: SIGTERM / SIGINT -> ctx cancel.
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// HTTP-server mode: long-running, multi-shot. The binary
	// stays alive serving /v1/planner/run; each request supplies
	// its own (repo_id, sha) target. This is the E2E +
	// integration entrypoint.
	if httpMode && !disabled {
		return runHTTPMode(ctx, cfg, logger, port)
	}

	// /healthz listener is ALWAYS on so K8s liveness probes
	// succeed even on an opted-out deployment. Match the
	// aggregator + metric_ingestor pattern.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           buildMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpErrCh := make(chan error, 1)
	go func() { httpErrCh <- srv.ListenAndServe() }()
	logger.Info("clean-code-refactor-planner: listening", "addr", srv.Addr)

	exitCode := 0
	if disabled {
		logger.Warn("clean-code-refactor-planner: planner disabled via " +
			EnvDisableRefactorPlanner + "; serving /healthz only")
		<-ctx.Done()
	} else {
		repoID, sha, vErr := parseTargetEnv()
		if vErr != nil {
			logger.Error("clean-code-refactor-planner: env validation failed", "err", vErr)
			exitCode = 1
		} else {
			db, dbErr := openAndPingDB(ctx, cfg.PostgresURL, "refactor-planner")
			if dbErr != nil {
				logger.Error("clean-code-refactor-planner: open db", "err", dbErr)
				exitCode = 1
			} else {
				defer db.Close()
				planRes, taskRes, pErr := runPlanner(ctx, cfg, db, logger, repoID, sha)
				if pErr != nil {
					if errors.Is(pErr, context.Canceled) ||
						errors.Is(pErr, context.DeadlineExceeded) {
						logger.Info("clean-code-refactor-planner: cancelled", "err", pErr)
					} else {
						logger.Error("clean-code-refactor-planner: planner failed", "err", pErr)
						exitCode = 1
					}
				} else if taskRes.Plan.PlanID == uuid.Nil {
					// ErrNoActivePolicy path: executeTwoPassPlan
					// returned (planRes, zero PlanAndTasksResult,
					// nil) because Stage 8.1 found no active
					// [steward.PolicyVersion]. The Stage 8.1 WARN
					// already explained why; emit a distinct
					// INFO so operators scanning structured logs
					// don't see a contradictory "planner OK"
					// line with all-zero policy_version_id /
					// plan_id UUIDs.
					logger.Info("clean-code-refactor-planner: no work performed",
						"repo_id", repoID.String(),
						"sha", sha,
						"reason", "no active policy",
					)
				} else {
					logger.Info("clean-code-refactor-planner: planner OK",
						"repo_id", repoID.String(),
						"sha", sha,
						"policy_version_id", planRes.PolicyVersionID.String(),
						"hot_spots_written", len(planRes.HotSpots),
						"tasks_emitted", len(taskRes.Tasks),
						"plan_id", taskRes.Plan.PlanID.String(),
					)
				}
			}
		}
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("clean-code-refactor-planner: server shutdown error", "err", err)
	}
	// Drain the http goroutine without leaking it.
	select {
	case err := <-httpErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("clean-code-refactor-planner: http listener exited",
				"err", err)
			if exitCode == 0 {
				exitCode = 1
			}
		}
	case <-shutdownCtx.Done():
		logger.Warn("clean-code-refactor-planner: http listener did not stop within shutdown timeout")
	}
	return exitCode
}

// runHTTPMode runs the planner as a long-running HTTP server
// that accepts /v1/planner/run requests. The PG handle +
// [refactor.EffortModel] are constructed ONCE at boot so
// per-request latency reflects Postgres round-trips, not model
// loading / config parsing.
//
// Exit codes:
//
//   - 0 -- the server returned cleanly (SIGTERM / SIGINT).
//   - 1 -- a fatal startup error (PG unreachable, ML model
//     artefact missing, EffortModel construction failed,
//     listener bind failed).
//
// Per-request errors are logged + surfaced in the HTTP response
// envelope; they do NOT exit the server.
func runHTTPMode(ctx context.Context, cfg config.Config, logger *slog.Logger, port string) int {
	db, dbErr := openAndPingDB(ctx, cfg.PostgresURL, "refactor-planner")
	if dbErr != nil {
		logger.Error("clean-code-refactor-planner: open db", "err", dbErr)
		return 1
	}
	defer db.Close()

	// The EffortModel is constructed at boot so a missing
	// artefact (ErrMLModelArtefactInvalid) or a missing pin
	// (ErrMLModelURIMissing / ErrMLModelVersionMissing) is a
	// fail-fast at startup -- the operator sees the typed
	// error in the boot log rather than per-request 500s with
	// the same message duplicated thousands of times.
	effortModel, emErr := refactor.NewEffortModelFromConfig(refactor.EffortModelConfig{
		Source:         cfg.RefactorEffortSource,
		MLModelURI:     cfg.MLModelURI,
		MLModelVersion: cfg.MLModelVersion,
	})
	if emErr != nil {
		logger.Error("clean-code-refactor-planner: EffortModel construction failed",
			"err", emErr,
			"effort_source", cfg.RefactorEffortSource,
			"ml_uri_set", cfg.MLModelURI != "",
			"ml_version_set", cfg.MLModelVersion != "",
		)
		return 1
	}
	logger.Info("clean-code-refactor-planner: HTTP mode effort model wired",
		"source", cfg.RefactorEffortSource,
		"ml_model_uri_set", cfg.MLModelURI != "",
		"ml_model_version_set", cfg.MLModelVersion != "",
	)

	handler := &plannerRunHandler{
		cfg:         cfg,
		db:          db,
		logger:      logger,
		effortModel: effortModel,
	}

	mux := buildMux()
	mux.Handle("/v1/planner/run", handler)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpErrCh := make(chan error, 1)
	go func() { httpErrCh <- srv.ListenAndServe() }()
	logger.Info("clean-code-refactor-planner: HTTP mode listening",
		"addr", srv.Addr,
		"endpoint", "/v1/planner/run")

	<-ctx.Done()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("clean-code-refactor-planner: HTTP mode shutdown error", "err", err)
	}
	select {
	case err := <-httpErrCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Warn("clean-code-refactor-planner: HTTP mode listener exited", "err", err)
			return 1
		}
	case <-shutdownCtx.Done():
	}
	return 0
}

// plannerRunHandler implements POST /v1/planner/run. The
// handler is serialized via plannerMu so two concurrent E2E
// scenarios can't tear each other's state -- the per-request
// latency budget is dominated by PG anyway, so the mutex
// is not a throughput concern.
type plannerRunHandler struct {
	cfg         config.Config
	db          *sql.DB
	logger      *slog.Logger
	effortModel refactor.EffortModel

	plannerMu sync.Mutex
}

type plannerRunRequest struct {
	RepoID string `json:"repo_id"`
	SHA    string `json:"sha"`
}

type plannerRunResponse struct {
	Status          string `json:"status"`
	RepoID          string `json:"repo_id"`
	SHA             string `json:"sha"`
	PolicyVersionID string `json:"policy_version_id,omitempty"`
	HotSpotsWritten int    `json:"hot_spots_written,omitempty"`
	TasksEmitted    int    `json:"tasks_emitted,omitempty"`
	PlanID          string `json:"plan_id,omitempty"`
	Reason          string `json:"reason,omitempty"`
	Error           string `json:"error,omitempty"`
	ErrorCategory   string `json:"error_category,omitempty"`
}

func (h *plannerRunHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writePlannerJSON(w, http.StatusMethodNotAllowed, plannerRunResponse{
			Status: "error",
			Error:  "method not allowed; use POST",
		})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64*1024))
	if err != nil {
		writePlannerJSON(w, http.StatusBadRequest, plannerRunResponse{
			Status: "error",
			Error:  fmt.Sprintf("read body: %v", err),
		})
		return
	}
	var req plannerRunRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writePlannerJSON(w, http.StatusBadRequest, plannerRunResponse{
			Status: "error",
			Error:  fmt.Sprintf("decode body: %v", err),
		})
		return
	}
	repoRaw := strings.TrimSpace(req.RepoID)
	sha := strings.TrimSpace(req.SHA)
	if repoRaw == "" || sha == "" {
		writePlannerJSON(w, http.StatusBadRequest, plannerRunResponse{
			Status: "error",
			Error:  "repo_id and sha are required",
		})
		return
	}
	repoID, err := uuid.FromString(repoRaw)
	if err != nil || repoID == uuid.Nil {
		writePlannerJSON(w, http.StatusBadRequest, plannerRunResponse{
			Status: "error",
			Error:  fmt.Sprintf("repo_id %q is not a valid UUID", repoRaw),
		})
		return
	}

	h.plannerMu.Lock()
	defer h.plannerMu.Unlock()

	planRes, taskRes, pErr := runPlannerWithEffortModel(
		r.Context(), h.cfg, h.db, h.logger, h.effortModel, repoID, sha)

	resp := plannerRunResponse{RepoID: repoID.String(), SHA: sha}
	if pErr != nil {
		category := classifyPlannerError(pErr)
		resp.Status = "error"
		resp.Error = pErr.Error()
		resp.ErrorCategory = category
		status := http.StatusInternalServerError
		if category == "version-mismatch" || category == "ml-model" {
			status = http.StatusUnprocessableEntity
		}
		writePlannerJSON(w, status, resp)
		return
	}
	if taskRes.Plan.PlanID == uuid.Nil {
		resp.Status = "no-op"
		resp.Reason = "no active policy"
		writePlannerJSON(w, http.StatusOK, resp)
		return
	}
	resp.Status = "ok"
	resp.PolicyVersionID = planRes.PolicyVersionID.String()
	resp.HotSpotsWritten = len(planRes.HotSpots)
	resp.TasksEmitted = len(taskRes.Tasks)
	resp.PlanID = taskRes.Plan.PlanID.String()
	writePlannerJSON(w, http.StatusOK, resp)
}

// classifyPlannerError maps a planner-side error to a stable
// category string the E2E harness can branch on without
// parsing free-form text. Categories track the architecture
// Sec 8.3 failure taxonomy.
func classifyPlannerError(err error) string {
	switch {
	case errors.Is(err, refactor.ErrMLModelVersionMismatch):
		return "version-mismatch"
	case errors.Is(err, refactor.ErrMLModelURIMissing),
		errors.Is(err, refactor.ErrMLModelVersionMissing),
		errors.Is(err, refactor.ErrMLModelArtefactInvalid),
		errors.Is(err, refactor.ErrUnknownEffortSource),
		errors.Is(err, refactor.ErrNilEffortModel),
		errors.Is(err, refactor.ErrInvalidEffortEstimate):
		return "ml-model"
	case errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return "cancelled"
	default:
		return "internal"
	}
}

func writePlannerJSON(w http.ResponseWriter, status int, body plannerRunResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// parseTargetEnv reads + validates [EnvRepoID] and [EnvSHA].
// Splitting this out lets unit tests cover every malformed-env
// branch without needing a real DB or a real planner.
func parseTargetEnv() (uuid.UUID, string, error) {
	repoRaw := strings.TrimSpace(os.Getenv(EnvRepoID))
	if repoRaw == "" {
		return uuid.Nil, "", fmt.Errorf("%s is required", EnvRepoID)
	}
	repoID, err := uuid.FromString(repoRaw)
	if err != nil {
		return uuid.Nil, "", fmt.Errorf("%s: not a UUID: %w", EnvRepoID, err)
	}
	if repoID == uuid.Nil {
		return uuid.Nil, "", fmt.Errorf("%s is the zero UUID -- a real repo is required", EnvRepoID)
	}
	sha := strings.TrimSpace(os.Getenv(EnvSHA))
	if sha == "" {
		return uuid.Nil, "", fmt.Errorf("%s is required", EnvSHA)
	}
	return repoID, sha, nil
}

// parseBoolEnv matches the convention the rest of the binaries
// use: empty -> false; truthy ("1", "true", "yes") -> true;
// anything else -> false.
func parseBoolEnv(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// openAndPingDB opens a libpq handle against dsn and pings it
// with a bounded retry budget so the binary fails fast if
// Postgres is permanently unreachable. Mirrors the
// aggregator binary's shape so an operator who reads both
// sees the same retry contract.
//
// ctx is the signal-cancellation context wired up in [run]; it
// is threaded into [sql.DB.PingContext] AND used to short-circuit
// the inter-attempt sleep so a SIGTERM received during DB startup
// (e.g. a K8s Job hitting its `terminationGracePeriodSeconds`)
// unblocks the loop promptly instead of burning the full
// pingAttempts*1s budget.
func openAndPingDB(ctx context.Context, dsn, role string) (*sql.DB, error) {
	h, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("sql.Open(%s): %w", role, err)
	}
	const pingAttempts = 30
	var pingErr error
	for i := 0; i < pingAttempts; i++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			_ = h.Close()
			return nil, fmt.Errorf("postgres %s ping cancelled: %w", role, ctxErr)
		}
		if pingErr = h.PingContext(ctx); pingErr == nil {
			return h, nil
		}
		select {
		case <-ctx.Done():
			_ = h.Close()
			return nil, fmt.Errorf("postgres %s ping cancelled: %w", role, ctx.Err())
		case <-time.After(time.Second):
		}
	}
	_ = h.Close()
	return nil, fmt.Errorf("postgres %s not reachable after %d attempts: %w",
		role, pingAttempts, pingErr)
}

// stage1Planner is the narrow surface
// [executeTwoPassPlan] needs from the Stage 8.1 hot_spot
// scorer. Pinning this as an interface (rather than the
// concrete `*refactor.Planner`) lets `main_test.go` inject a
// fake that records `Plan` invocations and asserts the
// composition root's two-pass orchestration without spinning
// up Postgres.
type stage1Planner interface {
	Plan(ctx context.Context, repoID uuid.UUID, sha string) (refactor.PlanResult, error)
}

// stage2Planner is the narrow surface
// [executeTwoPassPlan] needs from the Stage 8.2 plan / task
// emitter. The test-only fake uses this to assert that the
// Stage 8.1 [PlanResult.Snapshot] is forwarded verbatim into
// Stage 8.2 (the race-safe wiring the architecture Sec 5.5.1
// reproducibility invariant requires).
type stage2Planner interface {
	PlanFromSnapshot(
		ctx context.Context,
		repoID uuid.UUID,
		sha string,
		snap refactor.PolicySnapshot,
	) (refactor.PlanAndTasksResult, error)
}

// executeTwoPassPlan is the TESTABLE body of the composition
// root's two-pass orchestration. It is independent of the
// SQL wiring [runPlanner] performs so unit tests can pin the
// orchestration without sqlmock'ing the underlying readers /
// writers (which are already covered by
// `internal/refactor/task_planner_sql_test.go` and
// `internal/refactor/planner_sql_test.go`).
//
// Contract:
//
//  1. Calls `p.Plan(ctx, repoID, sha)`. On
//     [refactor.ErrNoActivePolicy] returns the captured
//     PlanResult, an empty PlanAndTasksResult, and `nil`
//     error -- the binary exits 0 because "no active policy"
//     is a fresh-deploy signal, not a fault.
//  2. On any other Stage 8.1 error, wraps with
//     `planner.Plan: %w` and returns; Stage 8.2 is NOT
//     invoked.
//  3. Calls `tp.PlanFromSnapshot(ctx, repoID, sha,
//     planRes.Snapshot)`. The snapshot is FORWARDED VERBATIM
//     so the two passes pin the same `policy_version_id`
//     (rubber-duck iter-2 finding #1).
//  4. On Stage 8.2 error, wraps with
//     `taskPlanner.PlanFromSnapshot: %w` and returns the
//     populated PlanResult so the caller can log what the
//     Stage 8.1 pass actually produced.
func executeTwoPassPlan(
	ctx context.Context,
	logger *slog.Logger,
	repoID uuid.UUID,
	sha string,
	p stage1Planner,
	tp stage2Planner,
) (refactor.PlanResult, refactor.PlanAndTasksResult, error) {
	logger.Info("clean-code-refactor-planner: Stage 8.1 Plan starting",
		"repo_id", repoID.String(), "sha", sha)
	planRes, err := p.Plan(ctx, repoID, sha)
	if err != nil {
		if errors.Is(err, refactor.ErrNoActivePolicy) {
			logger.Warn("clean-code-refactor-planner: no active policy -- exiting cleanly",
				"err", err)
			return planRes, refactor.PlanAndTasksResult{}, nil
		}
		return planRes, refactor.PlanAndTasksResult{},
			fmt.Errorf("planner.Plan: %w", err)
	}
	logger.Info("clean-code-refactor-planner: Stage 8.1 Plan complete",
		"hot_spots", len(planRes.HotSpots),
		"policy_version_id", planRes.PolicyVersionID.String())

	logger.Info("clean-code-refactor-planner: Stage 8.2 PlanFromSnapshot starting",
		"snapshot_pv_id", planRes.Snapshot.PolicyVersionID.String(),
		"top_n", planRes.Snapshot.Weights.TopN)
	taskRes, err := tp.PlanFromSnapshot(ctx, repoID, sha, planRes.Snapshot)
	if err != nil {
		return planRes, taskRes,
			fmt.Errorf("taskPlanner.PlanFromSnapshot: %w", err)
	}
	logger.Info("clean-code-refactor-planner: Stage 8.2 PlanFromSnapshot complete",
		"plan_id", taskRes.Plan.PlanID.String(),
		"tasks_emitted", len(taskRes.Tasks))
	return planRes, taskRes, nil
}

// runPlanner composes Stage 8.1 + Stage 8.2 against the supplied
// db and invokes the two-pass sequence in order. Resolves the
// [refactor.EffortModel] from cfg, then delegates to
// [runPlannerWithEffortModel] so the HTTP server mode can
// reuse a single shared model across requests.
func runPlanner(
	ctx context.Context,
	cfg config.Config,
	db *sql.DB,
	logger *slog.Logger,
	repoID uuid.UUID,
	sha string,
) (refactor.PlanResult, refactor.PlanAndTasksResult, error) {
	// Stage 9.3: select the EffortModel from operator-pinned
	// envs so refactor_task.effort_hours carries a real
	// estimate rather than the legacy 0.0 placeholder.
	effortModel, err := refactor.NewEffortModelFromConfig(refactor.EffortModelConfig{
		Source:         cfg.RefactorEffortSource,
		MLModelURI:     cfg.MLModelURI,
		MLModelVersion: cfg.MLModelVersion,
	})
	if err != nil {
		return refactor.PlanResult{}, refactor.PlanAndTasksResult{},
			fmt.Errorf("refactor.NewEffortModelFromConfig: %w", err)
	}
	logger.Info("clean-code-refactor-planner: effort model wired",
		"source", cfg.RefactorEffortSource,
		"ml_model_uri_set", cfg.MLModelURI != "",
		"ml_model_version_set", cfg.MLModelVersion != "",
	)
	return runPlannerWithEffortModel(ctx, cfg, db, logger, effortModel, repoID, sha)
}

// runPlannerWithEffortModel is the wiring core that builds the
// Stage 8.1 [refactor.Planner] + Stage 8.2 [refactor.TaskPlanner]
// against the supplied db + pre-constructed [refactor.EffortModel]
// and invokes the two-pass sequence. The Stage 8.2 pass uses
// [refactor.TaskPlanner.PlanFromSnapshot] with the Stage 8.1
// [PlanResult.Snapshot] so the two passes pin the SAME
// `policy_version_id` -- the race-safe wiring the
// architecture Sec 5.5.1 reproducibility invariant requires.
//
// Split out from [runPlanner] so the HTTP server mode can
// construct the [refactor.EffortModel] ONCE at boot (so model
// artefact / version errors fail-fast at startup) and reuse
// it across multi-shot /v1/planner/run requests.
func runPlannerWithEffortModel(
	ctx context.Context,
	cfg config.Config,
	db *sql.DB,
	logger *slog.Logger,
	effortModel refactor.EffortModel,
	repoID uuid.UUID,
	sha string,
) (refactor.PlanResult, refactor.PlanAndTasksResult, error) {
	_ = cfg // unused after EffortModel construction; kept for future config-driven options.
	stewardStore, err := steward.NewSQLStore(db)
	if err != nil {
		return refactor.PlanResult{}, refactor.PlanAndTasksResult{},
			fmt.Errorf("steward.NewSQLStore: %w", err)
	}
	stew, err := steward.New(steward.Config{Store: stewardStore, Signer: nil})
	if err != nil {
		return refactor.PlanResult{}, refactor.PlanAndTasksResult{},
			fmt.Errorf("steward.New: %w", err)
	}
	policy := &refactor.StewardPolicyReader{Steward: stew}

	planner, err := refactor.NewPlanner(
		policy,
		refactor.NewSQLMetricSampleReader(db),
		refactor.NewSQLFindingReader(db),
		refactor.NewSQLHotSpotWriter(db),
	)
	if err != nil {
		return refactor.PlanResult{}, refactor.PlanAndTasksResult{},
			fmt.Errorf("refactor.NewPlanner: %w", err)
	}

	taskPlanner, err := refactor.NewTaskPlanner(
		policy,
		refactor.NewSQLHotSpotReader(db),
		refactor.NewSQLFindingDetailReader(db),
		refactor.NewSQLRefactorPlanTaskWriter(db),
		refactor.WithEffortModel(effortModel),
	)
	if err != nil {
		return refactor.PlanResult{}, refactor.PlanAndTasksResult{},
			fmt.Errorf("refactor.NewTaskPlanner: %w", err)
	}

	return executeTwoPassPlan(ctx, logger, repoID, sha, planner, taskPlanner)
}

// buildMux mounts the always-on operational surface: `/healthz`
// (Kubernetes liveness) and `/metrics` (Prometheus stub). Kept
// tiny so the binary stays a thin composition root.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("# clean-code-refactor-planner metrics placeholder\n"))
	})
	return mux
}
