//go:build e2e

// Package mlEffortModelE2E houses the Stage 9.3 iter-10 E2E
// scenarios that drive the refactor-planner's ML effort-model
// loader and version-pinning contract.
//
// This package is deliberately kept SEPARATE from the
// long-standing `e2e` package at
// `services/clean-code/test/e2e/code-intelligence-CLEAN-CODE/`
// because that package contains sibling `*_test.go` files
// that redeclare the same helpers (`requireEnv`, `serviceRoot`,
// `readModulePath`, ...) and therefore fails to compile under
// the `e2e` build tag. Splitting this suite into its own Go
// package lets the CI workflow build + run JUST this suite
// (`go test -tags e2e ./test/e2e/refactor_planner_ml_effort_model/...`)
// without being blocked by unrelated package-level breakage.
//
// Scenario topology:
//
//   - Each scenario spawns a FRESH clean-code-refactor-planner
//     subprocess with the scenario's env vars on a random
//     127.0.0.1 port. This is how the suite varies env per
//     scenario without bouncing a docker-compose container.
//   - PG is shared across scenarios (opened once per suite);
//     the seeder owns its own (repo, sha) namespace so
//     scenarios do not pollute each other.
//   - Each scenario seeds: repo + commit + scope_binding +
//     rule + policy_version (activated) + evaluation_run +
//     finding (delta='new') + hot_spot, all linked by the
//     same policy_version_id. The planner's first PG read
//     (Stage 8.1) loads the active policy and the seeded
//     hot_spot; the second pass (Stage 8.2) joins the
//     hot_spot to its qualifying finding and emits exactly
//     one refactor_task with ML-backed effort_hours.
//
// Skip semantics:
//
//   - When CLEAN_CODE_PG_URL is unset, the whole suite is
//     skipped (no PG to drive Stage 8.1/8.2 against).
//   - When CLEAN_CODE_PLANNER_BIN is unset, the suite tries
//     `go build` in a temp dir to produce the binary on the
//     fly. If the build fails (no Go toolchain in the
//     runner), the suite is skipped with a clear diagnostic
//     so a CI missing the build prerequisites does not
//     silently mark PASS.
//   - When CLEAN_CODE_ML_ARTEFACT_PATH is unset the suite
//     defaults to the in-repo placeholder bundled with the
//     planner binary at
//     `services/clean-code/cmd/clean-code-refactor-planner/effort_model.onnx`.
package mlEffortModelE2E

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"
)

// mlEffortRepoID is the synthetic repo_id the scenarios stamp
// a hotspot under. It is deterministic so a re-run reuses the
// same repo row; the seeder inserts ON CONFLICT DO NOTHING.
const mlEffortRepoID = "00000000-0000-0000-0000-00000000ef01"

// mlEffortRuleID is the synthetic rule_id the seeded finding
// references. It is stamped into `clean_code.rule` as a
// real row so the finding's composite (rule_id, rule_version)
// FK is satisfied.
const mlEffortRuleID = "stage-9-3.ml-effort-model.synthetic"

// mlEffortRuleVersion is the version stamped on the synthetic
// rule. The composite (rule_id, version) primary key on
// `clean_code.rule` makes this a stable target.
const mlEffortRuleVersion = 1

// mlEffortPackID is the synthetic rule_pack identifier the
// rule row references. The FK is logical (no SQL FK), so a
// free-form string is fine.
const mlEffortPackID = "stage-9-3.ml-effort-model.pack"

// requireEnv returns the value of the named env var or
// t.Skipf-skips the test when the var is empty. Kept local
// to this package so no cross-package helper dependency
// exists.
func requireEnv(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping ML effort-model E2E", key)
	}
	return v
}

// plannerRunResponseE2E mirrors the planner binary's response
// envelope. Kept private to this package so no coupling to
// `internal/refactor` types exists.
type plannerRunResponseE2E struct {
	Status          string `json:"status"`
	RepoID          string `json:"repo_id"`
	SHA             string `json:"sha"`
	PolicyVersionID string `json:"policy_version_id"`
	HotSpotsWritten int    `json:"hot_spots_written"`
	TasksEmitted    int    `json:"tasks_emitted"`
	PlanID          string `json:"plan_id"`
	Reason          string `json:"reason"`
	Error           string `json:"error"`
	ErrorCategory   string `json:"error_category"`
}

// mlEffortState carries scenario-scoped state across the
// `Given`, `When`, `Then` steps. Reset per-scenario via
// `BeforeScenario` so steps from one scenario do not leak.
type mlEffortState struct {
	t           *testing.T
	plannerBin  string
	pgDSN       string
	artefactURI string

	// db is opened ONCE per suite and reused across
	// scenarios; each scenario only pays Postgres
	// round-trip cost.
	db *sql.DB

	// Per-scenario env declared by Given steps. Passed to
	// the spawned planner subprocess.
	effortSource   string
	mlModelURI     string
	mlModelVersion string
	policyVersion  string
	useCanonical   bool // true → declare via CLEAN_CODE_REFACTOR_EFFORT_SOURCE

	// Per-scenario subprocess state.
	plannerProc      *exec.Cmd
	plannerPort      int
	plannerStderrBuf *bytes.Buffer
	plannerStartErr  error

	// Per-scenario run identifiers + last response captured
	// by When steps so Then steps can assert.
	repoID          string
	sha             string
	scopeID         uuid.UUID
	policyVersionID uuid.UUID

	lastErr        error
	lastStatus     int
	lastResponse   plannerRunResponseE2E
	plannerExitErr error
}

func newMLEffortState(t *testing.T) *mlEffortState { return &mlEffortState{t: t} }

func (s *mlEffortState) resetScenario() {
	if s.plannerProc != nil && s.plannerProc.Process != nil {
		_ = s.plannerProc.Process.Signal(os.Interrupt)
		_ = s.plannerProc.Wait()
	}
	s.effortSource = ""
	s.mlModelURI = ""
	s.mlModelVersion = ""
	s.policyVersion = ""
	s.useCanonical = false
	s.plannerProc = nil
	s.plannerPort = 0
	s.plannerStderrBuf = nil
	s.plannerStartErr = nil
	s.repoID = ""
	s.sha = ""
	s.scopeID = uuid.Nil
	s.policyVersionID = uuid.Nil
	s.lastErr = nil
	s.lastStatus = 0
	s.lastResponse = plannerRunResponseE2E{}
	s.plannerExitErr = nil
}

func (s *mlEffortState) closeSuite() {
	if s.plannerProc != nil && s.plannerProc.Process != nil {
		_ = s.plannerProc.Process.Signal(os.Interrupt)
		_ = s.plannerProc.Wait()
	}
	if s.db != nil {
		_ = s.db.Close()
	}
}

// -------------------------------------------------------------------
// Given steps
// -------------------------------------------------------------------

func (s *mlEffortState) plannerStartedWithEffortSourceAlias(value string) error {
	s.effortSource = value
	s.useCanonical = false
	return nil
}

func (s *mlEffortState) plannerStartedWithCanonicalEffortSource(value string) error {
	s.effortSource = value
	s.useCanonical = true
	return nil
}

func (s *mlEffortState) mlModelURIIs(value string) error {
	if value == "<bundled>" {
		s.mlModelURI = s.artefactURI
		return nil
	}
	s.mlModelURI = value
	return nil
}

func (s *mlEffortState) mlModelURIIsUnset() error {
	s.mlModelURI = ""
	return nil
}

func (s *mlEffortState) mlModelVersionIs(value string) error {
	s.mlModelVersion = value
	return nil
}

func (s *mlEffortState) activePolicyVersionPinsEffortModelVersion(value string) error {
	if s.db == nil {
		return fmt.Errorf("PG is not wired; cannot seed active policy_version")
	}
	s.policyVersion = value
	pvID, err := seedActivePolicyVersion(s.db, value)
	if err != nil {
		return err
	}
	s.policyVersionID = pvID
	return nil
}

// -------------------------------------------------------------------
// When steps
// -------------------------------------------------------------------

func (s *mlEffortState) plannerGeneratesTasksForHotspot() error {
	if err := s.ensurePlannerSubprocess(); err != nil {
		s.lastErr = err
		return nil
	}
	s.repoID = mlEffortRepoID
	s.sha = "e2e-ml-effort-" + time.Now().Format("20060102-150405.000000")

	if s.policyVersionID == uuid.Nil {
		return fmt.Errorf("scenario did not seed an active policy_version " +
			"before invoking the planner")
	}
	scopeID, err := seedHotspotAndFinding(s.db, s.repoID, s.sha, s.policyVersionID)
	if err != nil {
		return fmt.Errorf("seed hotspot/finding: %w", err)
	}
	s.scopeID = scopeID

	resp, status, callErr := postPlannerRun(s.plannerPort, s.repoID, s.sha)
	s.lastStatus = status
	s.lastResponse = resp
	if callErr != nil {
		s.lastErr = callErr
		return nil
	}
	if resp.Status != "ok" {
		s.lastErr = fmt.Errorf("planner status=%q error=%q category=%q",
			resp.Status, resp.Error, resp.ErrorCategory)
		return nil
	}
	s.lastErr = nil
	return nil
}

func (s *mlEffortState) plannerStarts() error {
	if err := s.ensurePlannerSubprocess(); err != nil {
		s.lastErr = err
		return nil
	}
	return nil
}

// -------------------------------------------------------------------
// Then steps
// -------------------------------------------------------------------

func (s *mlEffortState) everyRefactorTaskHasEffortHoursGreaterThanZero() error {
	return s.assertEffortHours(func(eff float64) error {
		if !(eff > 0) {
			return fmt.Errorf(
				"refactor_task.effort_hours = %v, want > 0 (ML model should have populated it)",
				eff)
		}
		return nil
	}, true /* requireRows */)
}

func (s *mlEffortState) everyRefactorTaskHasEffortHoursAtMost(maxHours float64) error {
	return s.assertEffortHours(func(eff float64) error {
		if eff > maxHours {
			return fmt.Errorf(
				"refactor_task.effort_hours = %v exceeds bound %v", eff, maxHours)
		}
		return nil
	}, false /* requireRows */)
}

func (s *mlEffortState) everyRefactorTaskHasEffortHoursEqualToZero() error {
	return s.assertEffortHours(func(eff float64) error {
		if eff != 0.0 {
			return fmt.Errorf("zero source: refactor_task.effort_hours = %v, want 0.0", eff)
		}
		return nil
	}, true /* requireRows */)
}

func (s *mlEffortState) assertEffortHours(predicate func(float64) error, requireRows bool) error {
	if s.db == nil {
		return fmt.Errorf("PG not wired")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.effort_hours
		FROM clean_code.refactor_task t
		JOIN clean_code.refactor_plan p ON p.plan_id = t.plan_id
		WHERE p.repo_id = $1 AND p.sha = $2`, s.repoID, s.sha)
	if err != nil {
		return fmt.Errorf("query refactor_task rows: %w", err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var eff float64
		if err := rows.Scan(&eff); err != nil {
			return fmt.Errorf("scan refactor_task.effort_hours: %w", err)
		}
		if err := predicate(eff); err != nil {
			return err
		}
		seen++
	}
	if requireRows && seen == 0 {
		return fmt.Errorf(
			"no refactor_task rows for repo_id=%s sha=%s -- planner did not emit any",
			s.repoID, s.sha)
	}
	return nil
}

func (s *mlEffortState) plannerEmitsNoVersionMismatchError() error {
	if s.lastErr != nil {
		return fmt.Errorf("planner surfaced an error: %v", s.lastErr)
	}
	if s.lastResponse.ErrorCategory == "version-mismatch" {
		return fmt.Errorf("planner reported version-mismatch but scenario expected none")
	}
	return nil
}

func (s *mlEffortState) plannerExitsWithVersionMismatchError() error {
	if s.lastResponse.ErrorCategory != "version-mismatch" {
		return fmt.Errorf("planner error_category=%q want %q (lastErr=%v)",
			s.lastResponse.ErrorCategory, "version-mismatch", s.lastErr)
	}
	return nil
}

func (s *mlEffortState) plannerExitsNonZeroWithMissingURIError() error {
	if s.plannerStartErr == nil {
		return fmt.Errorf("planner started but a missing-URI error was expected")
	}
	stderr := ""
	if s.plannerStderrBuf != nil {
		stderr = s.plannerStderrBuf.String()
	}
	low := strings.ToLower(stderr)
	if !strings.Contains(low, "ml_model_uri") &&
		!strings.Contains(low, "uri") {
		return fmt.Errorf(
			"planner exited but stderr does not mention URI: stderr=%q startErr=%v",
			stderr, s.plannerStartErr)
	}
	return nil
}

func (s *mlEffortState) noRefactorPlanRowIsWrittenForRun() error {
	return s.assertRowCount("refactor_plan", 0)
}

func (s *mlEffortState) noRefactorTaskRowsAreWrittenForRun() error {
	return s.assertRowCount("refactor_task", 0)
}

func (s *mlEffortState) noRefactorPlanOrTaskRowsWritten() error {
	if err := s.assertRowCount("refactor_plan", 0); err != nil {
		return err
	}
	return s.assertRowCount("refactor_task", 0)
}

func (s *mlEffortState) assertRowCount(table string, want int) error {
	if s.db == nil {
		return fmt.Errorf("PG not wired")
	}
	if s.repoID == "" || s.sha == "" {
		// Negative scenarios that aborted before any
		// (repo, sha) was selected are vacuously OK.
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var query string
	switch table {
	case "refactor_plan":
		query = `SELECT COUNT(*) FROM clean_code.refactor_plan WHERE repo_id = $1 AND sha = $2`
	case "refactor_task":
		query = `
			SELECT COUNT(*)
			FROM clean_code.refactor_task t
			JOIN clean_code.refactor_plan p ON p.plan_id = t.plan_id
			WHERE p.repo_id = $1 AND p.sha = $2`
	default:
		return fmt.Errorf("unknown table %q", table)
	}
	var got int
	if err := s.db.QueryRowContext(ctx, query, s.repoID, s.sha).Scan(&got); err != nil {
		return fmt.Errorf("count %s rows: %w", table, err)
	}
	if got != want {
		return fmt.Errorf("clean_code.%s rows for (repo=%s, sha=%s) = %d, want %d",
			table, s.repoID, s.sha, got, want)
	}
	return nil
}
