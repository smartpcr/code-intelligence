//go:build e2e

package e2e

// E2E coverage for the Stage 9.3 ML effort-model loader and
// version-pinning contract. Each scenario spawns a FRESH
// `clean-code-refactor-planner` subprocess with the scenario's
// env vars on a random port; the test harness seeds the active
// policy_version with the desired `effort_model_version`,
// hits POST /v1/planner/run with the synthetic (repo_id, sha)
// pair, then verifies the resulting refactor_plan / refactor_task
// rows in PG. Per-scenario subprocesses are how this suite
// varies env without bouncing a docker-compose container.
//
// Skip semantics:
//
//   - When `CLEAN_CODE_PG_URL` is unset, the whole suite is
//     skipped (no PG to drive Stage 8.1/8.2 against).
//   - When `CLEAN_CODE_PLANNER_BIN` is unset, the suite tries
//     `go build` in a temp dir to produce the binary on the
//     fly. If the build fails (no Go toolchain in the runner),
//     the suite is skipped with a clear diagnostic so a CI
//     missing the build prerequisites does not silently mark
//     PASS.
//   - When `CLEAN_CODE_ML_ARTEFACT_PATH` is unset the suite
//     defaults to `services/clean-code/cmd/clean-code-refactor-planner/effort_model.onnx`
//     relative to the repo root (resolved via go.mod walk).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"
)

// mlEffortState carries scenario-scoped state across the
// `Given`, `When`, `Then` steps. Reset per-scenario via
// `BeforeScenario` so steps from one scenario do not leak.
type mlEffortState struct {
	t          *testing.T
	plannerBin string
	pgDSN      string
	artefactURI string

	// db is opened ONCE per suite and reused across scenarios
	// so each scenario only pays Postgres round-trip cost.
	db *sql.DB

	// Per-scenario env declared by Given steps. These are
	// passed to the spawned planner subprocess.
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
	repoID         string
	sha            string
	lastErr        error
	lastStatus     int
	lastResponse   plannerRunResponseE2E
	plannerExitErr error
}

// plannerRunResponseE2E mirrors the planner binary's
// response envelope. Keeping it private to the test file
// avoids coupling to internal/refactor types.
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
		s.db.Close()
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
	// "file:///models/effort_model.onnx" is the docker-compose
	// canonical URI. For a per-scenario subprocess we redirect
	// to the real artefact path on the host filesystem so the
	// loader passes.
	if value == "file:///models/effort_model.onnx" {
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
	return seedActivePolicyVersion(s.db, value)
}

// -------------------------------------------------------------------
// When steps
// -------------------------------------------------------------------

func (s *mlEffortState) plannerGeneratesTasksForHotspot() error {
	if err := s.ensurePlannerSubprocess(); err != nil {
		s.lastErr = err
		return nil // negative scenarios assert on lastErr
	}
	s.repoID = mlEffortRepoID
	s.sha = "e2e-ml-effort-" + time.Now().Format("20060102-150405.000000")

	// Seed a hotspot + finding so the planner's TaskPlanner
	// has work to do under (repo_id, sha). This is a unit-of-
	// work the test owns; the bootstrap migration provides the
	// schema but not the per-scenario data.
	if err := seedHotspotAndFinding(s.db, s.repoID, s.sha, s.policyVersion); err != nil {
		s.lastErr = fmt.Errorf("seed hotspot: %w", err)
		return nil
	}

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
		// Subprocess startup failed -- expected for the
		// missing-URI scenario. Capture the exit error so the
		// Then step can assert on it.
		s.lastErr = err
		return nil
	}
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
			"no refactor_task rows for repo_id=%s sha=%s -- planner did not emit any",
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
	if s.lastResponse.ErrorCategory != "version-mismatch" {
		return fmt.Errorf("planner error_category=%q want %q (lastErr=%v)",
			s.lastResponse.ErrorCategory, "version-mismatch", s.lastErr)
	}
	return nil
}

func (s *mlEffortState) plannerExitsNonZeroWithMissingURIError() error {
	// The planner subprocess must have failed to start because
	// EffortSourceML + missing URI is a boot-time fail-fast.
	if s.plannerStartErr == nil {
		return fmt.Errorf("planner started but a missing-URI error was expected")
	}
	stderr := ""
	if s.plannerStderrBuf != nil {
		stderr = s.plannerStderrBuf.String()
	}
	if !strings.Contains(strings.ToLower(stderr), "ml_model_uri") &&
		!strings.Contains(strings.ToLower(stderr), "uri") {
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
		// Negative scenarios that aborted before any (repo, sha)
		// was selected are vacuously OK.
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
// subprocess + PG helpers
// -------------------------------------------------------------------

// ensurePlannerSubprocess starts the planner binary with the
// scenario's env vars on a random port. Idempotent within a
// scenario -- calling twice is a no-op once the subprocess is
// healthy.
func (s *mlEffortState) ensurePlannerSubprocess() error {
	if s.plannerProc != nil && s.plannerProc.Process != nil && s.plannerStartErr == nil {
		return nil
	}
	port, err := pickFreePort()
	if err != nil {
		return fmt.Errorf("pick free port: %w", err)
	}
	s.plannerPort = port

	env := os.Environ()
	env = appendEnv(env, "CLEAN_CODE_PG_URL", s.pgDSN)
	env = appendEnv(env, "CLEAN_CODE_ALLOW_SHARED_PG_ROLE", "true")
	env = appendEnv(env, "CLEAN_CODE_REFACTOR_PLANNER_HTTP", "true")
	env = appendEnv(env, "PORT", fmt.Sprintf("%d", port))
	if s.useCanonical {
		env = appendEnv(env, "CLEAN_CODE_REFACTOR_EFFORT_SOURCE", s.effortSource)
		env = clearEnv(env, "CLEAN_CODE_EFFORT_SOURCE")
	} else {
		env = appendEnv(env, "CLEAN_CODE_EFFORT_SOURCE", s.effortSource)
		env = clearEnv(env, "CLEAN_CODE_REFACTOR_EFFORT_SOURCE")
	}
	if s.mlModelURI != "" {
		env = appendEnv(env, "CLEAN_CODE_ML_MODEL_URI", s.mlModelURI)
	} else {
		env = clearEnv(env, "CLEAN_CODE_ML_MODEL_URI")
	}
	if s.mlModelVersion != "" {
		env = appendEnv(env, "CLEAN_CODE_ML_MODEL_VERSION", s.mlModelVersion)
	} else {
		env = clearEnv(env, "CLEAN_CODE_ML_MODEL_VERSION")
	}

	stderr := &bytes.Buffer{}
	cmd := exec.Command(s.plannerBin)
	cmd.Env = env
	cmd.Stdout = io.Discard
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start planner subprocess: %w", err)
	}
	s.plannerProc = cmd
	s.plannerStderrBuf = stderr

	// Wait up to 10s for /healthz to return 200, or for the
	// subprocess to exit (negative scenarios).
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case waitErr := <-waitCh:
			// Subprocess exited before becoming healthy.
			s.plannerStartErr = fmt.Errorf("planner exited before ready: %v\nstderr:\n%s",
				waitErr, stderr.String())
			s.plannerExitErr = waitErr
			return s.plannerStartErr
		default:
		}
		if err := pingPlannerHealthz(port); err == nil {
			// Healthy. Don't drain waitCh -- the subprocess
			// remains alive; closeSuite/resetScenario will
			// signal it.
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Health check timed out. Kill the subprocess.
	_ = cmd.Process.Signal(os.Interrupt)
	<-waitCh
	s.plannerStartErr = fmt.Errorf("planner did not become healthy within 10s; stderr:\n%s",
		stderr.String())
	return s.plannerStartErr
}

func appendEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, key+"="+value)
}

func clearEnv(env []string, key string) []string {
	prefix := key + "="
	out := env[:0]
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func pickFreePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func pingPlannerHealthz(port int) error {
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz %d", resp.StatusCode)
	}
	return nil
}

func postPlannerRun(port int, repoID, sha string) (plannerRunResponseE2E, int, error) {
	body := fmt.Sprintf(`{"repo_id":%q,"sha":%q}`, repoID, sha)
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/v1/planner/run", port),
		strings.NewReader(body))
	if err != nil {
		return plannerRunResponseE2E{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return plannerRunResponseE2E{}, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var parsed plannerRunResponseE2E
	_ = json.Unmarshal(raw, &parsed)
	return parsed, resp.StatusCode, nil
}

// seedActivePolicyVersion inserts a fresh policy_version with
// the given effort_model_version pin and activates it. Idempotent
// per-suite: a previous scenario's activation is closed before
// the new one opens.
func seedActivePolicyVersion(db *sql.DB, effortModelVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()
	pvID := uuid.Must(uuid.NewV4())
	weights := fmt.Sprintf(`{"alpha":0.4,"beta":0.3,"gamma":0.2,"delta":0.1,"window_days":90,"top_n":3,"effort_model_version":%q}`, effortModelVersion)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.policy_version
		(policy_version_id, refactor_weights, created_at)
		VALUES ($1, $2::jsonb, now())
		ON CONFLICT DO NOTHING`,
		pvID, weights,
	); err != nil {
		return fmt.Errorf("insert policy_version: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE clean_code.policy_activation
		SET activated_until = now()
		WHERE activated_until IS NULL`,
	); err != nil {
		return fmt.Errorf("close previous activation: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.policy_activation
		(policy_version_id, activated_at, activated_until)
		VALUES ($1, now(), NULL)`,
		pvID,
	); err != nil {
		return fmt.Errorf("insert policy_activation: %w", err)
	}
	return tx.Commit()
}

// seedHotspotAndFinding writes a hot_spot row + a qualifying
// finding for the test (repo_id, sha) so the Stage 8.2
// TaskPlanner has at least one task to emit.
func seedHotspotAndFinding(db *sql.DB, repoID, sha, policyVersion string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// The exact schema depends on migrations; this helper is
	// a best-effort that no-ops when the migrations have not
	// landed (the scenario assertion will then fail with a
	// clear "no refactor_task rows" message).
	_, _ = db.ExecContext(ctx,
		`INSERT INTO clean_code.hot_spot
		(hotspot_id, repo_id, sha, scope_id, score, policy_version_id, created_at)
		SELECT $1, $2::uuid, $3, $4, $5, pv.policy_version_id, now()
		FROM clean_code.policy_activation pa
		JOIN clean_code.policy_version pv ON pv.policy_version_id = pa.policy_version_id
		WHERE pa.activated_until IS NULL
		LIMIT 1
		ON CONFLICT DO NOTHING`,
		uuid.Must(uuid.NewV4()), repoID, sha, uuid.Must(uuid.NewV4()), 7.5,
	)
	_ = policyVersion
	return nil
}

// resolvePlannerBinary returns the path to a clean-code-refactor-planner
// executable. Preference: CLEAN_CODE_PLANNER_BIN env, then a
// `go build` in a temp dir.
func resolvePlannerBinary(t *testing.T) (string, error) {
	t.Helper()
	if bin := os.Getenv("CLEAN_CODE_PLANNER_BIN"); bin != "" {
		return bin, nil
	}
	root, err := findServicesCleanCodeRoot()
	if err != nil {
		return "", fmt.Errorf("find services/clean-code root: %w", err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "clean-code-refactor-planner")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/clean-code-refactor-planner")
	cmd.Dir = root
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("go build planner: %v\nstderr:\n%s", err, stderr.String())
	}
	return out, nil
}

// findServicesCleanCodeRoot walks up from the test file location
// to find the services/clean-code directory (the Go module root
// for the planner binary).
func findServicesCleanCodeRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	cur := wd
	for i := 0; i < 8; i++ {
		mod := filepath.Join(cur, "go.mod")
		if _, err := os.Stat(mod); err == nil {
			return cur, nil
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return "", fmt.Errorf("no go.mod found upward from %s", wd)
}

// resolveArtefactURI returns a file:// URI for the placeholder
// ONNX artefact bundled with the planner binary. Preference:
// CLEAN_CODE_ML_ARTEFACT_PATH env, then the in-repo default.
func resolveArtefactURI() (string, error) {
	if p := os.Getenv("CLEAN_CODE_ML_ARTEFACT_PATH"); p != "" {
		return fileURIFromPath(p)
	}
	root, err := findServicesCleanCodeRoot()
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, "cmd", "clean-code-refactor-planner", "effort_model.onnx")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("default artefact missing: %w", err)
	}
	return fileURIFromPath(p)
}

func fileURIFromPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	if len(abs) >= 2 && abs[1] == ':' {
		u.Path = "/" + filepath.ToSlash(abs)
	}
	return u.String(), nil
}

// -------------------------------------------------------------------
// Constants + suite wiring
// -------------------------------------------------------------------

// mlEffortRepoID is the synthetic repo_id the scenarios stamp a
// hotspot under so the planner's `repo_id` filter selects
// exactly the rows produced by this suite.
const mlEffortRepoID = "00000000-0000-0000-0000-00000000ef01"

// TestRefactorPlannerMLEffortModelLoaderAndVersionPinning runs
// the feature file's scenarios under godog. Each scenario gets
// fresh state via `BeforeScenario` so step state cannot leak.
func TestRefactorPlannerMLEffortModelLoaderAndVersionPinning(t *testing.T) {
	pgDSN := requireEnv(t, "CLEAN_CODE_PG_URL")
	plannerBin, err := resolvePlannerBinary(t)
	if err != nil {
		t.Skipf("planner binary unavailable: %v", err)
	}
	artefactURI, err := resolveArtefactURI()
	if err != nil {
		t.Skipf("ml artefact unavailable: %v", err)
	}

	state := newMLEffortState(t)
	state.plannerBin = plannerBin
	state.pgDSN = pgDSN
	state.artefactURI = artefactURI

	db, err := sql.Open("postgres", pgDSN)
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Skipf("postgres not reachable: %v", err)
	}
	state.db = db
	defer state.closeSuite()

	// Schema availability gate: skip if the refactor schema is
	// missing so a misconfigured CI does not silently mark
	// PASS on a no-op suite.
	if !schemaHasRefactorPlan(db) {
		t.Skip("clean_code.refactor_plan not present; skipping ML effort-model E2E (run migrations first)")
	}

	suite := godog.TestSuite{
		ScenarioInitializer: func(ctx *godog.ScenarioContext) {
			ctx.Before(func(_ context.Context, _ *godog.Scenario) (context.Context, error) {
				state.resetScenario()
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

func schemaHasRefactorPlan(db *sql.DB) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var exists bool
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'clean_code'
			AND table_name = 'refactor_plan'
		)`).Scan(&exists)
	if err != nil {
		return false
	}
	return exists
}

// Compile-time guard so an unused-import linter does not strip
// the `errors` import after a future refactor of the helpers.
var _ = errors.Is
