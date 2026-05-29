//go:build e2e

package mlEffortModelE2E

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
)

// TestRefactorPlannerMLEffortModelLoaderAndVersionPinning runs
// the feature file's scenarios under godog. Each scenario gets
// a fresh `mlEffortState` via `BeforeScenario` so step state
// cannot leak across scenarios. The planner subprocess is
// killed + restarted between scenarios so each Given block
// actually mutates the planner's env (the iteration-9 blocker
// was a fixed docker-compose env that could not vary
// scenario-by-scenario).
func TestRefactorPlannerMLEffortModelLoaderAndVersionPinning(t *testing.T) {
	pgDSN := requireEnv(t, "CLEAN_CODE_PG_URL")

	root, err := findServicesCleanCodeRoot()
	if err != nil {
		t.Skipf("services/clean-code root not found: %v", err)
	}

	plannerBin, err := resolvePlannerBinary(t, root)
	if err != nil {
		t.Skipf("planner binary unavailable: %v", err)
	}

	artefactURI, err := resolveArtefactURI(root)
	if err != nil {
		t.Skipf("ML artefact unavailable: %v", err)
	}

	state := newMLEffortState(t)
	state.pgDSN = pgDSN
	state.plannerBin = plannerBin
	state.artefactURI = artefactURI

	db, err := openPG(pgDSN)
	if err != nil {
		t.Skipf("postgres open/ping failed: %v", err)
	}
	state.db = db
	defer state.closeSuite()

	if !schemaHasRefactorPlan(db) {
		t.Skip("clean_code.refactor_plan table not present; run migrations first")
	}

	suite := godog.TestSuite{
		ScenarioInitializer: func(ctx *godog.ScenarioContext) {
			ctx.Before(func(_ context.Context, _ *godog.Scenario) (context.Context, error) {
				state.resetScenario()
				return nil, nil
			})
			ctx.Step(`^the refactor-planner is started with CLEAN_CODE_EFFORT_SOURCE=(.+)$`,
				state.plannerStartedWithEffortSourceAlias)
			ctx.Step(`^the refactor-planner is started with CLEAN_CODE_REFACTOR_EFFORT_SOURCE="(.+)"$`,
				state.plannerStartedWithCanonicalEffortSource)
			ctx.Step(`^CLEAN_CODE_ML_MODEL_URI is "(.+)"$`, state.mlModelURIIsResolved)
			ctx.Step(`^CLEAN_CODE_ML_MODEL_URI is unset$`, state.mlModelURIIsUnset)
			ctx.Step(`^CLEAN_CODE_ML_MODEL_VERSION is "(.+)"$`, state.mlModelVersionIs)
			ctx.Step(`^the active policy_version pins effort_model_version to "(.+)"$`,
				state.activePolicyVersionPinsEffortModelVersion)
			ctx.Step(`^the planner generates refactor tasks for a hotspot$`,
				state.plannerGeneratesTasksForHotspot)
			ctx.Step(`^the planner starts$`, state.plannerStarts)
			ctx.Step(`^every refactor_task row has effort_hours > 0$`,
				state.everyRefactorTaskHasEffortHoursGreaterThanZero)
			ctx.Step(`^every refactor_task row has effort_hours <= 40\.0$`,
				func() error { return state.everyRefactorTaskHasEffortHoursAtMost(40.0) })
			ctx.Step(`^every refactor_task row has effort_hours equal to 0\.0$`,
				state.everyRefactorTaskHasEffortHoursEqualToZero)
			ctx.Step(`^the planner emits no version-mismatch error$`,
				state.plannerEmitsNoVersionMismatchError)
			ctx.Step(`^the planner exits with a version-mismatch error$`,
				state.plannerExitsWithVersionMismatchError)
			ctx.Step(`^the planner exits non-zero with a missing-model-URI error$`,
				state.plannerExitsNonZeroWithMissingURIError)
			ctx.Step(`^no refactor_plan row is written for this run$`,
				state.noRefactorPlanRowIsWrittenForRun)
			ctx.Step(`^no refactor_task rows are written for this run$`,
				state.noRefactorTaskRowsAreWrittenForRun)
			ctx.Step(`^no refactor_plan or refactor_task rows are written$`,
				state.noRefactorPlanOrTaskRowsWritten)
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

// mlModelURIIsResolved is a thin wrapper around mlModelURIIs
// that resolves the in-container docker path
// `file:///models/effort_model.onnx` (used in the feature
// file's verbatim text) to the bundled in-repo artefact URI
// when the planner is being spawned as a host subprocess.
// This lets ONE feature file drive both the docker-compose
// E2E and the per-scenario subprocess E2E.
func (s *mlEffortState) mlModelURIIsResolved(value string) error {
	resolved := value
	if value == "file:///models/effort_model.onnx" && s.artefactURI != "" {
		resolved = s.artefactURI
	}
	return s.mlModelURIIs(resolved)
}

// openPG opens + pings the Postgres DSN. Returns a closed-on-
// ping-failure handle so callers always get a clean leak-free
// state when the PG is unreachable.
func openPG(dsn string) (*sql.DB, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("postgres DSN is empty")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return db, nil
}
