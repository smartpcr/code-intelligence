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
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
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

	disabled := parseBoolEnv(os.Getenv(EnvDisableRefactorPlanner))

	// /healthz listener is ALWAYS on so K8s liveness probes
	// succeed even on an opted-out deployment. Match the
	// aggregator + metric_ingestor pattern.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           buildMux(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	httpErrCh := make(chan error, 1)
	go func() { httpErrCh <- srv.ListenAndServe() }()
	logger.Info("clean-code-refactor-planner: listening", "addr", srv.Addr)

	// Wire the cancel chain: SIGTERM / SIGINT -> ctx cancel.
	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

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
				planRes, taskRes, pErr := runPlanner(ctx, db, logger, repoID, sha)
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
// db and invokes the two-pass sequence in order. The Stage 8.2
// pass uses [refactor.TaskPlanner.PlanFromSnapshot] with the
// Stage 8.1 [PlanResult.Snapshot] so the two passes pin the
// SAME `policy_version_id` -- the race-safe wiring the
// architecture Sec 5.5.1 reproducibility invariant requires.
//
// The function is a thin SQL-wiring adapter over
// [executeTwoPassPlan]; the two-pass orchestration itself is
// pinned by `TestExecuteTwoPassPlan_*` cases.
func runPlanner(
	ctx context.Context,
	db *sql.DB,
	logger *slog.Logger,
	repoID uuid.UUID,
	sha string,
) (refactor.PlanResult, refactor.PlanAndTasksResult, error) {
	stewardStore, err := steward.NewSQLStore(db)
	if err != nil {
		return refactor.PlanResult{}, refactor.PlanAndTasksResult{},
			fmt.Errorf("steward.NewSQLStore: %w", err)
	}
	// Signer: nil -- the refactor-planner binary only READS
	// the active policy_version. It never signs anything. The
	// steward installs an in-memory verify-only fallback when
	// Signer is nil; production deployments are validated by
	// the Stage 5.1 Steward bootstrap test.
	stew, err := steward.New(steward.Config{Store: stewardStore, Signer: nil})
	if err != nil {
		return refactor.PlanResult{}, refactor.PlanAndTasksResult{},
			fmt.Errorf("steward.New: %w", err)
	}
	policy := &refactor.StewardPolicyReader{Steward: stew}

	// Stage 8.1 wiring.
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

	// Stage 8.2 wiring. Note PlanFromSnapshot pins the SAME
	// policy_version_id as the hot_spot batch we just wrote.
	taskPlanner, err := refactor.NewTaskPlanner(
		policy,
		refactor.NewSQLHotSpotReader(db),
		refactor.NewSQLFindingDetailReader(db),
		refactor.NewSQLRefactorPlanTaskWriter(db),
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
