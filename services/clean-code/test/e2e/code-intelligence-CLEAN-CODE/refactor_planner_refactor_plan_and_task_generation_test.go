//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
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
// shared state
// ---------------------------------------------------------------------------

type refactorTaskState struct {
	plannerURL string
	db         *sql.DB

	// scenario-scoped fields
	ruleID        string
	taskID        string
	effortHours   *float64
	columnNames   []string
	insertErr     error
}

func newRefactorTaskState() *refactorTaskState {
	return &refactorTaskState{}
}

func (s *refactorTaskState) initEnv() error {
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

func (s *refactorTaskState) close() {
	if s.db != nil {
		s.db.Close()
	}
}

// ---------------------------------------------------------------------------
// Scenario 1: plan-generates-canonical-task-kinds
// ---------------------------------------------------------------------------

type generateTasksRequest struct {
	RepoID   string `json:"repo_id"`
	SHA      string `json:"sha"`
	FilePath string `json:"file_path"`
	RuleID   string `json:"rule_id"`
}

type generateTasksResponse struct {
	Tasks []struct {
		TaskID string `json:"task_id"`
		Kind   string `json:"kind"`
		RuleID string `json:"rule_id"`
	} `json:"tasks"`
}

func (s *refactorTaskState) aHotspotFlaggedByRule(ruleID string) error {
	s.ruleID = ruleID
	return nil
}

func (s *refactorTaskState) thePlannerGeneratesTasks() error {
	reqBody := generateTasksRequest{
		RepoID:   "00000000-0000-0000-0000-000000000001",
		SHA:      "e2edeadbeef5678",
		FilePath: "src/e2e_refactor_target.go",
		RuleID:   s.ruleID,
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("marshalling generate-tasks request: %w", err)
	}

	url := strings.TrimRight(s.plannerURL, "/") + "/v1/planner/tasks/generate"
	req, err := http.NewRequestWithContext(
		context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating generate-tasks request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("calling planner generate-tasks endpoint: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("planner generate-tasks endpoint returned HTTP %d", resp.StatusCode)
	}

	var result generateTasksResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding generate-tasks response: %w", err)
	}

	if len(result.Tasks) == 0 {
		return fmt.Errorf("planner returned zero tasks for rule_id=%s", s.ruleID)
	}
	s.taskID = result.Tasks[0].TaskID
	return nil
}

func (s *refactorTaskState) aRefactorTaskRowExistsWithKindAndRuleID(kind, ruleID string) error {
	if s.db == nil {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set; cannot verify refactor_task row")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var actualKind, actualRuleID string
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT kind, rule_id FROM clean_code.refactor_task
			WHERE task_id = $1
		`, s.taskID).Scan(&actualKind, &actualRuleID)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for refactor_task row with task_id=%s: %w",
				s.taskID, err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if actualKind != kind {
		return fmt.Errorf("expected kind=%q, got %q", kind, actualKind)
	}
	if actualRuleID != ruleID {
		return fmt.Errorf("expected rule_id=%q, got %q", ruleID, actualRuleID)
	}
	return nil
}

func (s *refactorTaskState) insertingRefactorTaskWithKindIsRejected(invalidKind string) error {
	if s.db == nil {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set; cannot test CHECK constraint")
	}

	_, err := s.db.ExecContext(context.Background(), `
		INSERT INTO clean_code.refactor_task (
			task_id, hot_spot_id, kind, rule_id, effort_hours
		) VALUES (
			gen_random_uuid(),
			'00000000-0000-0000-0000-000000000000',
			$1,
			'solid.srp',
			1.0
		)
	`, invalidKind)

	if err == nil {
		return fmt.Errorf("expected CHECK constraint violation for kind=%q, but INSERT succeeded",
			invalidKind)
	}

	errMsg := strings.ToLower(err.Error())
	if !strings.Contains(errMsg, "check") && !strings.Contains(errMsg, "violates") &&
		!strings.Contains(errMsg, "constraint") {
		return fmt.Errorf("expected CHECK constraint error for kind=%q, got: %w",
			invalidKind, err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 2: no-effort-estimate-table
// ---------------------------------------------------------------------------

func (s *refactorTaskState) theSchemaAfterPhase1Migrations() error {
	// The schema is already applied via make migrate-up / seed-phase-08.
	// Verify we can query the refactor_task table.
	if s.db == nil {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	var exists bool
	err := s.db.QueryRowContext(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'clean_code' AND table_name = 'refactor_task'
		)
	`).Scan(&exists)
	if err != nil {
		return fmt.Errorf("checking refactor_task table existence: %w", err)
	}
	if !exists {
		return fmt.Errorf("refactor_task table does not exist after Phase 1 migrations")
	}
	return nil
}

func (s *refactorTaskState) thePlannerPersistsEffortForTheHotspot() error {
	// Reuse the task generated in scenario 1 context, or trigger a new one.
	if s.taskID == "" {
		s.ruleID = "solid.srp"
		if err := s.thePlannerGeneratesTasks(); err != nil {
			return fmt.Errorf("generating tasks for effort test: %w", err)
		}
	}
	return nil
}

func (s *refactorTaskState) refactorTaskEffortHoursIsPopulated() error {
	if s.db == nil {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var effortHours sql.NullFloat64
	for {
		err := s.db.QueryRowContext(ctx, `
			SELECT effort_hours FROM clean_code.refactor_task
			WHERE task_id = $1
		`, s.taskID).Scan(&effortHours)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return fmt.Errorf("timed out waiting for refactor_task effort_hours: %w", err)
		}
		time.Sleep(250 * time.Millisecond)
	}

	if !effortHours.Valid {
		return fmt.Errorf("effort_hours is NULL for task_id=%s", s.taskID)
	}
	s.effortHours = &effortHours.Float64
	return nil
}

func (s *refactorTaskState) noTableNamedExistsInTheSchema(tableName string) error {
	if s.db == nil {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}

	var exists bool
	err := s.db.QueryRowContext(context.Background(), `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'clean_code' AND table_name = $1
		)
	`, tableName).Scan(&exists)
	if err != nil {
		return fmt.Errorf("checking table %q existence: %w", tableName, err)
	}
	if exists {
		return fmt.Errorf("table %q unexpectedly exists in clean_code schema", tableName)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Scenario 3: refactor-task-has-no-status-column
// ---------------------------------------------------------------------------

func (s *refactorTaskState) theCanonicalRefactorTaskTable() error {
	// Same precondition as scenario 2 — table must exist.
	return s.theSchemaAfterPhase1Migrations()
}

func (s *refactorTaskState) informationSchemaColumnsIsQueriedForRefactorTask() error {
	if s.db == nil {
		return fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}

	rows, err := s.db.QueryContext(context.Background(), `
		SELECT column_name FROM information_schema.columns
		WHERE table_schema = 'clean_code' AND table_name = 'refactor_task'
	`)
	if err != nil {
		return fmt.Errorf("querying information_schema.columns: %w", err)
	}
	defer rows.Close()

	s.columnNames = nil
	for rows.Next() {
		var colName string
		if err := rows.Scan(&colName); err != nil {
			return fmt.Errorf("scanning column_name: %w", err)
		}
		s.columnNames = append(s.columnNames, colName)
	}
	return rows.Err()
}

func (s *refactorTaskState) noColumnNamedExists(colName string) error {
	for _, c := range s.columnNames {
		if c == colName {
			return fmt.Errorf("column %q unexpectedly exists in refactor_task", colName)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// scenario initializer
// ---------------------------------------------------------------------------

// InitializeScenario_refactor_planner_refactor_plan_and_task_generation registers
// all Given/When/Then steps for the refactor-plan-and-task-generation stage.
func InitializeScenario_refactor_planner_refactor_plan_and_task_generation(ctx *godog.ScenarioContext) {
	state := newRefactorTaskState()

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

	// Scenario 1: plan-generates-canonical-task-kinds
	ctx.Given(`^a hotspot flagged by rule "([^"]*)"$`, func(ruleID string) error {
		return state.aHotspotFlaggedByRule(ruleID)
	})
	ctx.When(`^the planner generates tasks$`, func() error {
		return state.thePlannerGeneratesTasks()
	})
	ctx.Then(
		`^a refactor_task row exists with kind "([^"]*)" and rule_id "([^"]*)"$`,
		func(kind, ruleID string) error {
			return state.aRefactorTaskRowExistsWithKindAndRuleID(kind, ruleID)
		},
	)
	ctx.Then(
		`^inserting a refactor_task with kind "([^"]*)" is rejected by the CHECK constraint$`,
		func(invalidKind string) error {
			return state.insertingRefactorTaskWithKindIsRejected(invalidKind)
		},
	)

	// Scenario 2: no-effort-estimate-table
	ctx.Given(`^the schema after Phase 1 migrations$`, func() error {
		return state.theSchemaAfterPhase1Migrations()
	})
	ctx.When(`^the planner persists effort for the hotspot$`, func() error {
		return state.thePlannerPersistsEffortForTheHotspot()
	})
	ctx.Then(`^refactor_task\.effort_hours is populated$`, func() error {
		return state.refactorTaskEffortHoursIsPopulated()
	})
	ctx.Then(`^no table named "([^"]*)" exists in the schema$`, func(tableName string) error {
		return state.noTableNamedExistsInTheSchema(tableName)
	})

	// Scenario 3: refactor-task-has-no-status-column
	ctx.Given(`^the canonical refactor_task table$`, func() error {
		return state.theCanonicalRefactorTaskTable()
	})
	ctx.When(`^information_schema\.columns is queried for refactor_task$`, func() error {
		return state.informationSchemaColumnsIsQueriedForRefactorTask()
	})
	ctx.Then(`^no column named "([^"]*)" exists$`, func(colName string) error {
		return state.noColumnNamedExists(colName)
	})
}

// ---------------------------------------------------------------------------
// test entrypoint
// ---------------------------------------------------------------------------

func TestE2E_refactor_planner_refactor_plan_and_task_generation(t *testing.T) {
	requireEnv(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_refactor_planner_refactor_plan_and_task_generation,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"refactor_planner_refactor_plan_and_task_generation.feature"},
			TestingT: t,
		},
	}

	if suite.Run() != 0 {
		t.Fatal("godog test suite failed")
	}
}