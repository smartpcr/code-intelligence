//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// requireEnv returns the value of the named environment variable,
// calling t.Skip when unset or empty.
func requireEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping e2e test", name)
	}
	return v
}

// ---------------------------------------------------------------------------
// shared state for ML effort model loader and version pinning scenarios
// ---------------------------------------------------------------------------

type mlEffortModelState struct {
	plannerURL string
	db         *sql.DB

	// scenario 1: missing-model-blocks-startup
	exitCode int
	stdErr   string

	// scenario 2: effort-model-version-pinned-via-hotspot
	taskID             string
	planID             string
	hotspotID          string
	policyVersionID    string
	effortModelVersion string
	loadedModelVersion string
}

func newMLEffortModelState() *mlEffortModelState {
	return &mlEffortModelState{}
}

func (s *mlEffortModelState) initEnv() error {
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

func (s *mlEffortModelState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Scenario 1: missing-model-blocks-startup
//
// Launches a one-shot refactor-planner container with
// CLEAN_CODE_EFFORT_SOURCE=ml but CLEAN_CODE_ML_MODEL_URI unset,
// and asserts it exits non-zero with a message naming the missing config.
// ---------------------------------------------------------------------------

func (s *mlEffortModelState) refactorEffortSourceMLModelAndNoModelURIConfigured() error {
	return nil
}

func (s *mlEffortModelState) thePlannerInitialises() error {
	composeFile := os.Getenv("COMPOSE_FILE")
	if composeFile == "" {
		composeFile = "tests/e2e/phase-08-refactor-planner/docker-compose.yml"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Start a disposable container overriding env: ML effort source enabled,
	// model URI explicitly blank → planner must refuse to start.
	cmd := exec.CommandContext(ctx,
		"docker", "compose", "-f", composeFile,
		"run", "--rm", "--no-deps",
		"-e", "CLEAN_CODE_EFFORT_SOURCE=ml",
		"-e", "CLEAN_CODE_ML_MODEL_URI=",
		"refactor-planner",
	)
	out, err := cmd.CombinedOutput()
	s.stdErr = string(out)

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			s.exitCode = exitErr.ExitCode()
		} else {
			s.exitCode = 1
		}
	} else {
		s.exitCode = 0
	}

	return nil
}

func (s *mlEffortModelState) startupExitsNonZeroWithErrorNamingMissingConfig() error {
	if s.exitCode == 0 {
		return fmt.Errorf("expected non-zero exit code, got 0")
	}

	lower := strings.ToLower(s.stdErr)
	// The planner should mention the missing config key in its error output.
	indicators := []string{
		"clean_code_ml_model_uri",
		"model_uri",
		"model uri",
		"effort_source",
		"effort source",
		"ml model",
		"missing",
	}
	for _, ind := range indicators {
		if strings.Contains(lower, ind) {
			return nil
		}
	}
	return fmt.Errorf(
		"expected error output to name the missing config key; got: %s",
		s.stdErr,
	)
}

// ---------------------------------------------------------------------------
// Scenario 2: effort-model-version-pinned-via-hotspot
//
// Architecture §5.5.1-5.5.2: refactor_plan has a hotspot_ids UUID[] column
// (no policy_version_id column on refactor_plan). The traversal path is:
//   refactor_plan.hotspot_ids[0]
//     → hot_spot.policy_version_id
//       → policy_version.refactor_weights->>'effort_model_version'
// The test reads the loaded model version from the compose-configured env
// var CLEAN_CODE_ML_MODEL_VERSION (injected into the refactor-planner
// service) rather than calling an unverified health endpoint.
// ---------------------------------------------------------------------------

func (s *mlEffortModelState) aGeneratedRefactorTaskAndTheRefactorPlanThatOwnsIt() error {
	if s.db == nil {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Look for a seeded refactor_task linked to a refactor_plan.
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT rt.task_id, rt.plan_id
			FROM clean_code.refactor_task rt
			WHERE rt.plan_id IS NOT NULL
			LIMIT 1
		`).Scan(&s.taskID, &s.planID)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return s.generateTaskViaAPI()
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func (s *mlEffortModelState) generateTaskViaAPI() error {
	url := strings.TrimRight(s.plannerURL, "/") + "/v1/planner/tasks/generate"
	body := `{
		"repo_id":   "00000000-0000-0000-0000-000000000001",
		"sha":       "e2edeadbeef5678",
		"file_path": "src/e2e_ml_effort_target.go",
		"rule_id":   "solid.srp"
	}`

	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating generate-tasks request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("calling planner generate-tasks: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("planner generate-tasks returned HTTP %d", resp.StatusCode)
	}

	var result struct {
		Tasks []struct {
			TaskID string `json:"task_id"`
			PlanID string `json:"plan_id"`
		} `json:"tasks"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding generate-tasks response: %w", err)
	}
	if len(result.Tasks) == 0 {
		return fmt.Errorf("planner returned zero tasks")
	}
	s.taskID = result.Tasks[0].TaskID
	s.planID = result.Tasks[0].PlanID
	return nil
}

func (s *mlEffortModelState) traversingHotspotPolicyVersionEffortModelVersion() error {
	if s.db == nil {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Step 1: Extract hotspot_ids[0] from refactor_plan (UUID[] column).
	// Architecture §5.5.1: refactor_plan.hotspot_ids is a UUID array;
	// refactor_plan has NO policy_version_id column.
	err := s.db.QueryRowContext(ctx, `
		SELECT hotspot_ids[1]::text
		FROM clean_code.refactor_plan
		WHERE plan_id = $1
	`, s.planID).Scan(&s.hotspotID)
	if err != nil {
		return fmt.Errorf("extracting hotspot_ids[0] from refactor_plan (plan_id=%s): %w",
			s.planID, err)
	}
	if s.hotspotID == "" {
		return fmt.Errorf("hotspot_ids[0] is NULL for plan_id=%s", s.planID)
	}

	// Step 2: hot_spot.policy_version_id
	err = s.db.QueryRowContext(ctx, `
		SELECT policy_version_id::text
		FROM clean_code.hot_spot
		WHERE hot_spot_id = $1
	`, s.hotspotID).Scan(&s.policyVersionID)
	if err != nil {
		return fmt.Errorf("querying policy_version_id for hot_spot_id=%s: %w",
			s.hotspotID, err)
	}

	// Step 3: policy_version.refactor_weights->>'effort_model_version'
	err = s.db.QueryRowContext(ctx, `
		SELECT refactor_weights->>'effort_model_version'
		FROM clean_code.policy_version
		WHERE policy_version_id = $1
	`, s.policyVersionID).Scan(&s.effortModelVersion)
	if err != nil {
		return fmt.Errorf("querying effort_model_version for policy_version_id=%s: %w",
			s.policyVersionID, err)
	}

	// Step 4: Determine the loaded model artefact version.
	// The compose service is configured with CLEAN_CODE_ML_MODEL_VERSION;
	// read it from the environment (the CI pipeline exports it).
	// Fall back to the planner's /v1/planner/status endpoint if the env
	// var is not available in the test runner.
	s.loadedModelVersion = os.Getenv("CLEAN_CODE_ML_MODEL_VERSION")
	if s.loadedModelVersion == "" {
		v, err := s.fetchModelVersionFromPlanner()
		if err != nil {
			return fmt.Errorf("determining loaded model version: %w", err)
		}
		s.loadedModelVersion = v
	}
	return nil
}

func (s *mlEffortModelState) fetchModelVersionFromPlanner() (string, error) {
	url := strings.TrimRight(s.plannerURL, "/") + "/v1/planner/status"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("calling planner status endpoint: %w", err)
	}
	defer resp.Body.Close()

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding planner status response: %w", err)
	}

	// Try known keys in decreasing specificity.
	for _, key := range []string{"effort_model_version", "model_version", "version"} {
		if v, ok := body[key]; ok {
			if sv, ok := v.(string); ok && sv != "" {
				return sv, nil
			}
		}
	}
	return "", fmt.Errorf("planner status response has no effort_model_version field: %v", body)
}

func (s *mlEffortModelState) theValueMatchesTheLoadedModelArtefactVersion() error {
	if s.effortModelVersion == "" {
		return fmt.Errorf("effort_model_version from DB traversal is empty")
	}
	if s.loadedModelVersion == "" {
		return fmt.Errorf("loaded model artefact version is empty")
	}
	if s.effortModelVersion != s.loadedModelVersion {
		return fmt.Errorf(
			"effort_model_version mismatch: DB chain=%q, loaded=%q",
			s.effortModelVersion, s.loadedModelVersion,
		)
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_refactor_planner_ml_effort_model_loader_and_version_pinning registers
// all Given/When/Then steps for the ml-effort-model-loader-and-version-pinning stage.
func InitializeScenario_refactor_planner_ml_effort_model_loader_and_version_pinning(ctx *godog.ScenarioContext) {
	state := newMLEffortModelState()

	ctx.Before(func(bctx context.Context, sc *godog.Scenario) (context.Context, error) {
		if err := state.initEnv(); err != nil {
			return bctx, err
		}
		return bctx, nil
	})

	ctx.After(func(actx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		state.close()
		return actx, nil
	})

	// Scenario 1: missing-model-blocks-startup
	ctx.Given(
		`^refactor-effort-source=ML model from historical commits and no model URI configured$`,
		func() error {
			return state.refactorEffortSourceMLModelAndNoModelURIConfigured()
		},
	)
	ctx.When(`^the planner initialises$`, func() error {
		return state.thePlannerInitialises()
	})
	ctx.Then(`^startup exits non-zero with an error naming the missing config$`, func() error {
		return state.startupExitsNonZeroWithErrorNamingMissingConfig()
	})

	// Scenario 2: effort-model-version-pinned-via-hotspot
	ctx.Given(
		`^a generated refactor_task and the refactor_plan that owns it$`,
		func() error {
			return state.aGeneratedRefactorTaskAndTheRefactorPlanThatOwnsIt()
		},
	)
	ctx.When(
		`^traversing refactor_plan\.hotspot_ids\[0\] -> hot_spot\.policy_version_id -> policy_version\.refactor_weights\.effort_model_version$`,
		func() error {
			return state.traversingHotspotPolicyVersionEffortModelVersion()
		},
	)
	ctx.Then(`^the value matches the loaded model artefact version$`, func() error {
		return state.theValueMatchesTheLoadedModelArtefactVersion()
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_refactor_planner_ml_effort_model_loader_and_version_pinning(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_refactor_planner_ml_effort_model_loader_and_version_pinning,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"refactor_planner_ml_effort_model_loader_and_version_pinning.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}