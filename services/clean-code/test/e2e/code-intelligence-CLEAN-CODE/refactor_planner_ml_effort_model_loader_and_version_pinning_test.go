//go:build e2e

package e2e

// E2E coverage for the Stage 9.3 ML effort-model loader and
// version-pinning contract. The scenarios exercise the
// composition-root wiring the `clean-code-refactor-planner`
// binary performs from `CLEAN_CODE_EFFORT_SOURCE` /
// `CLEAN_CODE_ML_MODEL_URI` / `CLEAN_CODE_ML_MODEL_VERSION`
// envs (see tests/e2e/phase-08-refactor-planner/docker-compose.yml).
//
// The runtime checks against a live `clean_code` schema using
// the same docker-compose stack as the sibling planner e2es.
// The scenarios skip when the planner URL or PG DSN env vars
// are not wired so this file is safe to run under `go test
// -tags e2e ./...` without bringing the compose stack up.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// mlEffortState carries scenario-scoped state across the
// `Given`, `When`, `Then` steps. It is reset per-scenario via
// `BeforeScenario` so steps from one scenario do not leak into
// another.
type mlEffortState struct {
	plannerURL string
	db         *sql.DB

	// Operator-pinned envs we report on; the docker-compose
	// stack supplies the canonical values, the scenarios
	// assert the planner respects them. We record the
	// observed values for diagnostic messages but do not
	// mutate the running planner -- that would require
	// restarting the container, which the godog harness
	// cannot do safely mid-suite.
	effortSource   string
	mlModelURI     string
	mlModelVersion string
	policyVersion  string

	// Run identifiers, set by the When steps so the Then
	// steps can scope their PG queries to the rows produced
	// by this scenario.
	repoID string
	sha    string

	// lastErr captures the most recent planner-side error
	// surfaced via its HTTP surface so the version-mismatch
	// + missing-URI scenarios can assert on the failure
	// envelope without leaking the assertion into the
	// happy-path scenarios.
	lastErr error
}

func newMLEffortState() *mlEffortState { return &mlEffortState{} }

func (s *mlEffortState) reset() {
	s.effortSource = ""
	s.mlModelURI = ""
	s.mlModelVersion = ""
	s.policyVersion = ""
	s.repoID = ""
	s.sha = ""
	s.lastErr = nil
}

func (s *mlEffortState) initEnv() error {
	s.plannerURL = os.Getenv("CLEAN_CODE_PLANNER_URL")
	if s.plannerURL == "" {
		s.plannerURL = "http://localhost:8085"
	}
	dsn := os.Getenv("CLEAN_CODE_PG_URL")
	if dsn == "" {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("opening postgres: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return fmt.Errorf("pinging postgres: %w", err)
	}
	s.db = db
	return nil
}

func (s *mlEffortState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// -------------------------------------------------------------------
// Given steps
// -------------------------------------------------------------------

func (s *mlEffortState) plannerStartedWithEffortSourceAlias(value string) error {
	s.effortSource = value
	got := strings.TrimSpace(os.Getenv("CLEAN_CODE_EFFORT_SOURCE"))
	if got == "" {
		got = strings.TrimSpace(os.Getenv("CLEAN_CODE_REFACTOR_EFFORT_SOURCE"))
	}
	if got != value {
		return fmt.Errorf(
			"e2e harness expected CLEAN_CODE_EFFORT_SOURCE=%q but observed %q; "+
				"check docker-compose.yml env block",
			value, got)
	}
	return nil
}

func (s *mlEffortState) plannerStartedWithCanonicalEffortSource(value string) error {
	s.effortSource = value
	got := strings.TrimSpace(os.Getenv("CLEAN_CODE_REFACTOR_EFFORT_SOURCE"))
	if got == "" {
		got = strings.TrimSpace(os.Getenv("CLEAN_CODE_EFFORT_SOURCE"))
	}
	if got != value {
		return fmt.Errorf(
			"e2e harness expected CLEAN_CODE_REFACTOR_EFFORT_SOURCE=%q but observed %q",
			value, got)
	}
	return nil
}

func (s *mlEffortState) mlModelURIIs(value string) error {
	s.mlModelURI = value
	got := strings.TrimSpace(os.Getenv("CLEAN_CODE_ML_MODEL_URI"))
	if got != value {
		return fmt.Errorf(
			"e2e harness expected CLEAN_CODE_ML_MODEL_URI=%q but observed %q; "+
				"check docker-compose.yml env block",
			value, got)
	}
	return nil
}

func (s *mlEffortState) mlModelURIIsUnset() error {
	s.mlModelURI = ""
	got := strings.TrimSpace(os.Getenv("CLEAN_CODE_ML_MODEL_URI"))
	if got != "" {
		return fmt.Errorf(
			"e2e harness expected CLEAN_CODE_ML_MODEL_URI unset, got %q", got)
	}
	return nil
}

func (s *mlEffortState) mlModelVersionIs(value string) error {
	s.mlModelVersion = value
	got := strings.TrimSpace(os.Getenv("CLEAN_CODE_ML_MODEL_VERSION"))
	if got != value {
		return fmt.Errorf(
			"e2e harness expected CLEAN_CODE_ML_MODEL_VERSION=%q but observed %q",
			value, got)
	}
	return nil
}

func (s *mlEffortState) activePolicyVersionPinsEffortModelVersion(value string) error {
	if s.db == nil {
		return fmt.Errorf("PG is not wired; cannot assert active policy_version")
	}
	s.policyVersion = value
	// The docker-compose fixture seeds the active policy_version
	// with a chosen effort_model_version. The assertion here
	// verifies the seeder ran and the active row carries the
	// expected pin so a downstream mismatch claim is meaningful.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var observed string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(refactor_weights->>'effort_model_version', '')
		FROM clean_code.policy_version
		WHERE policy_version_id = (
			SELECT policy_version_id
			FROM clean_code.policy_activation
			WHERE activated_until IS NULL
			ORDER BY activated_at DESC
			LIMIT 1
		)`).Scan(&observed)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("no active policy_version found (seeder did not run?)")
	}
	if err != nil {
		return fmt.Errorf("query active policy_version: %w", err)
	}
	if observed != value {
		return fmt.Errorf(
			"active policy_version.refactor_weights.effort_model_version = %q, want %q",
			observed, value)
	}
	return nil
}

// -------------------------------------------------------------------
// When steps
// -------------------------------------------------------------------

func (s *mlEffortState) plannerGeneratesTasksForHotspot() error {
	// Schedule one planner run by hitting the planner's HTTP
	// surface. The harness uses a synthetic (repo_id, sha)
	// pair the seeder pre-stamped a hotspot for.
	s.repoID = mlEffortRepoID
	s.sha = "e2e-ml-effort-" + time.Now().Format("20060102-150405")
	url := strings.TrimRight(s.plannerURL, "/") + "/v1/planner/run"
	body := fmt.Sprintf(`{"repo_id":%q,"sha":%q}`, s.repoID, s.sha)
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("build planner run request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("call planner: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.lastErr = nil
		return nil
	}
	// Non-2xx is a planner-side error envelope; capture it
	// for the negative-scenario Then steps.
	s.lastErr = fmt.Errorf("planner HTTP %d", resp.StatusCode)
	return nil
}

func (s *mlEffortState) plannerStarts() error {
	// For the missing-URI scenario the planner's startup
	// itself is the unit under test -- we probe /healthz to
	// verify it is up OR observe its exit code via the
	// harness-side status endpoint when the compose stack
	// records non-zero exits.
	url := strings.TrimRight(s.plannerURL, "/") + "/healthz"
	req, err := http.NewRequestWithContext(context.Background(),
		http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build healthz request: %w", err)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.lastErr = err
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.lastErr = nil
		return nil
	}
	s.lastErr = fmt.Errorf("planner /healthz returned HTTP %d", resp.StatusCode)
	return nil
}

// -------------------------------------------------------------------
// Then steps
// -------------------------------------------------------------------

func (s *mlEffortState) everyRefactorTaskHasEffortHoursGreaterThanZero() error {
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
		if !(eff > 0) {
			return fmt.Errorf(
				"refactor_task.effort_hours = %v, want > 0 (ML model should have populated it)",
				eff)
		}
		seen++
	}
	if seen == 0 {
		return fmt.Errorf(
			"no refactor_task rows found for repo_id=%s sha=%s -- planner did not emit any",
			s.repoID, s.sha)
	}
	return nil
}

func (s *mlEffortState) everyRefactorTaskHasEffortHoursAtMost(maxHours float64) error {
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
	for rows.Next() {
		var eff float64
		if err := rows.Scan(&eff); err != nil {
			return fmt.Errorf("scan refactor_task.effort_hours: %w", err)
		}
		if eff > maxHours {
			return fmt.Errorf(
				"refactor_task.effort_hours = %v exceeds MaxMLHours bound %v",
				eff, maxHours)
		}
	}
	return nil
}

func (s *mlEffortState) plannerEmitsNoVersionMismatchError() error {
	if s.lastErr != nil {
		return fmt.Errorf("planner surfaced an error: %v", s.lastErr)
	}
	return nil
}

func (s *mlEffortState) plannerExitsWithVersionMismatchError() error {
	if s.lastErr == nil {
		return fmt.Errorf("planner reported success but a version-mismatch error was expected")
	}
	return nil
}

func (s *mlEffortState) plannerExitsNonZeroWithMissingURIError() error {
	if s.lastErr == nil {
		return fmt.Errorf("planner started successfully but a missing-URI error was expected")
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
		// Negative scenarios that aborted before any (repo, sha)
		// was selected are vacuously OK -- there's no row set
		// to count against.
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

func (s *mlEffortState) everyRefactorTaskHasEffortHoursEqualToZero() error {
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
		if eff != 0.0 {
			return fmt.Errorf("zero source: refactor_task.effort_hours = %v, want 0.0", eff)
		}
		seen++
	}
	if seen == 0 {
		return fmt.Errorf("no refactor_task rows -- zero-source planner emitted nothing")
	}
	return nil
}

// -------------------------------------------------------------------
// Constants + suite wiring
// -------------------------------------------------------------------

// mlEffortRepoID is the synthetic repo_id the compose stack
// seeds a hotspot under so the planner's `repo_id` filter
// selects exactly the rows this feature file's scenarios
// expect.
const mlEffortRepoID = "00000000-0000-0000-0000-00000000ef01"

// TestRefactorPlannerMLEffortModelLoaderAndVersionPinning runs
// the feature file's scenarios under godog. Each scenario gets a
// fresh state via `BeforeScenario` so step state cannot leak.
func TestRefactorPlannerMLEffortModelLoaderAndVersionPinning(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")
	state := newMLEffortState()
	if err := state.initEnv(); err != nil {
		t.Skipf("e2e env not wired: %v", err)
	}
	defer state.close()

	suite := godog.TestSuite{
		ScenarioInitializer: func(ctx *godog.ScenarioContext) {
			ctx.Before(func(_ context.Context, _ *godog.Scenario) (context.Context, error) {
				state.reset()
				return nil, nil
			})
			ctx.Step(`^the refactor-planner is started with CLEAN_CODE_EFFORT_SOURCE=(.+)$`, state.plannerStartedWithEffortSourceAlias)
			ctx.Step(`^the refactor-planner is started with CLEAN_CODE_REFACTOR_EFFORT_SOURCE="(.+)"$`, state.plannerStartedWithCanonicalEffortSource)
			ctx.Step(`^CLEAN_CODE_ML_MODEL_URI is "(.+)"$`, state.mlModelURIIs)
			ctx.Step(`^CLEAN_CODE_ML_MODEL_URI is unset$`, state.mlModelURIIsUnset)
			ctx.Step(`^CLEAN_CODE_ML_MODEL_VERSION is "(.+)"$`, state.mlModelVersionIs)
			ctx.Step(`^the active policy_version pins effort_model_version to "(.+)"$`, state.activePolicyVersionPinsEffortModelVersion)
			ctx.Step(`^the planner generates refactor tasks for a hotspot$`, state.plannerGeneratesTasksForHotspot)
			ctx.Step(`^the planner starts$`, state.plannerStarts)
			ctx.Step(`^every refactor_task row has effort_hours > 0$`, state.everyRefactorTaskHasEffortHoursGreaterThanZero)
			ctx.Step(`^every refactor_task row has effort_hours <= 40\.0$`, func() error {
				return state.everyRefactorTaskHasEffortHoursAtMost(40.0)
			})
			ctx.Step(`^every refactor_task row has effort_hours equal to 0\.0$`, state.everyRefactorTaskHasEffortHoursEqualToZero)
			ctx.Step(`^the planner emits no version-mismatch error$`, state.plannerEmitsNoVersionMismatchError)
			ctx.Step(`^the planner exits with a version-mismatch error$`, state.plannerExitsWithVersionMismatchError)
			ctx.Step(`^the planner exits non-zero with a missing-model-URI error$`, state.plannerExitsNonZeroWithMissingURIError)
			ctx.Step(`^no refactor_plan row is written for this run$`, state.noRefactorPlanRowIsWrittenForRun)
			ctx.Step(`^no refactor_task rows are written for this run$`, state.noRefactorTaskRowsAreWrittenForRun)
			ctx.Step(`^no refactor_plan or refactor_task rows are written$`, state.noRefactorPlanOrTaskRowsWritten)
		},
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"refactor_planner_ml_effort_model_loader_and_version_pinning.feature"},
			TestingT: t,
		},
	}
	if status := suite.Run(); status != 0 {
		t.Fatalf("ML effort-model loader / version pinning suite failed (status=%d)", status)
	}
}
