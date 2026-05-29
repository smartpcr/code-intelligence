//go:build e2e

package mlEffortModelE2E

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/uuid"
)

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
	env = upsertEnv(env, "CLEAN_CODE_PG_URL", s.pgDSN)
	env = upsertEnv(env, "CLEAN_CODE_ALLOW_SHARED_PG_ROLE", "true")
	env = upsertEnv(env, "CLEAN_CODE_REFACTOR_PLANNER_HTTP", "true")
	env = upsertEnv(env, "PORT", fmt.Sprintf("%d", port))
	if s.useCanonical {
		env = upsertEnv(env, "CLEAN_CODE_REFACTOR_EFFORT_SOURCE", s.effortSource)
		env = clearEnv(env, "CLEAN_CODE_EFFORT_SOURCE")
	} else {
		env = upsertEnv(env, "CLEAN_CODE_EFFORT_SOURCE", s.effortSource)
		env = clearEnv(env, "CLEAN_CODE_REFACTOR_EFFORT_SOURCE")
	}
	if s.mlModelURI != "" {
		env = upsertEnv(env, "CLEAN_CODE_ML_MODEL_URI", s.mlModelURI)
	} else {
		env = clearEnv(env, "CLEAN_CODE_ML_MODEL_URI")
	}
	if s.mlModelVersion != "" {
		env = upsertEnv(env, "CLEAN_CODE_ML_MODEL_VERSION", s.mlModelVersion)
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

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case waitErr := <-waitCh:
			s.plannerStartErr = fmt.Errorf("planner exited before ready: %v\nstderr:\n%s",
				waitErr, stderr.String())
			s.plannerExitErr = waitErr
			return s.plannerStartErr
		default:
		}
		if err := pingPlannerHealthz(port); err == nil {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	_ = cmd.Process.Signal(os.Interrupt)
	<-waitCh
	s.plannerStartErr = fmt.Errorf("planner did not become healthy within 10s; stderr:\n%s",
		stderr.String())
	return s.plannerStartErr
}

// upsertEnv replaces (or appends) the key=value entry in env.
// Returns a NEW slice so the caller's input is not mutated.
func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, e := range env {
		if !strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return append(out, key+"="+value)
}

// clearEnv removes any key=... entry from env. Returns a NEW
// slice so the caller's input is not mutated.
func clearEnv(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
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
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
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
	if len(raw) > 0 {
		_ = decodePlannerJSON(raw, &parsed)
	}
	return parsed, resp.StatusCode, nil
}

func decodePlannerJSON(raw []byte, out *plannerRunResponseE2E) error {
	return json.Unmarshal(raw, out)
}

// -------------------------------------------------------------------
// PG seeders -- transactional, errors propagate
// -------------------------------------------------------------------

// seedActivePolicyVersion inserts a fresh policy_version with
// the given effort_model_version pin and activates it by
// appending a fresh `policy_activation` row. Activation in
// `clean_code.policy_activation` is decided by
// `MAX(created_at)` (architecture Sec 5.3.4, G5 latest-row-
// wins) -- the table has NO `activated_until` column and the
// previous activation row is NOT mutated; the in-body comment
// at the `INSERT INTO clean_code.policy_activation` site
// repeats this so the seed reads correctly in isolation.
//
// Returns the new policy_version_id so the caller can wire it
// into the finding + hot_spot rows.
func seedActivePolicyVersion(db *sql.DB, effortModelVersion string) (uuid.UUID, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	pvID := uuid.Must(uuid.NewV4())
	weights := fmt.Sprintf(
		`{"alpha":0.4,"beta":0.3,"gamma":0.2,"delta":0.1,"window_days":90,"top_n":3,"effort_model_version":%q}`,
		effortModelVersion)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.policy_version
		(policy_version_id, name, rule_refs, threshold_refs,
		 refactor_weights, signature, created_at)
		VALUES ($1::uuid, $2, $3::jsonb, $4::jsonb,
		 $5::jsonb, $6::bytea, now())
		ON CONFLICT DO NOTHING`,
		pvID, "stage-9.3-ml-effort-e2e",
		"[]", "[]", weights,
		// 64-byte placeholder signature (Ed25519 is 64 bytes
		// per tech-spec Sec 8.4). The planner does not verify
		// the signature -- only the Evaluator Surface does --
		// so a stand-in is sufficient for this E2E.
		strings.Repeat("\x00", 64),
	); err != nil {
		return uuid.Nil, fmt.Errorf("insert policy_version: %w", err)
	}
	// `policy_activation` has NO `activated_until` column;
	// activation is decided by `MAX(created_at)`. A fresh
	// append-only row makes this policy_version the active
	// one without mutating any prior row.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.policy_activation
		(policy_version_id, activated_by, created_at)
		VALUES ($1::uuid, $2, now())`,
		pvID, "stage-9.3-ml-effort-e2e",
	); err != nil {
		return uuid.Nil, fmt.Errorf("insert policy_activation: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return uuid.Nil, fmt.Errorf("commit policy seed: %w", err)
	}
	return pvID, nil
}

// seedHotspotAndFinding writes the full FK closure required
// by TaskPlanner.Plan so the planner has work to do under
// (repoID, sha):
//
//  1. clean_code.repo  -- ON CONFLICT DO NOTHING; idempotent
//  2. clean_code.commit -- the (repo_id, sha) commit row
//  3. clean_code.scope_binding -- the synthetic scope this
//     run uses (deterministic UUIDv5 not used here -- a
//     fresh v4 is fine because each scenario uses its own
//     sha and the binding's natural key is unique on
//     (repo_id, scope_kind, canonical_signature,
//     first_seen_sha)).
//  4. clean_code.rule -- the synthetic rule the finding
//     references; composite (rule_id, version) PK.
//  5. clean_code.evaluation_run -- carries policy_version_id
//     and is the FK target for the finding.
//  6. clean_code.finding -- delta='new', policy_version_id,
//     scope_id, rule pair so SQLFindingDetailReader returns
//     a qualifying detail.
//  7. clean_code.hot_spot -- references the SAME scope_id
//     and policy_version_id so SQLHotSpotReader.Top returns
//     a row and TaskPlanner.Plan joins the finding onto it.
//
// Returns the scope_id so the caller (test) can correlate
// the seeded data with the assertion queries.
func seedHotspotAndFinding(db *sql.DB, repoID, sha string, policyVersionID uuid.UUID) (uuid.UUID, error) {
	if policyVersionID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("policyVersionID must be non-nil")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin seed tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.repo
		(repo_id, display_name, default_branch, created_at)
		VALUES ($1::uuid, $2, 'main', now())
		ON CONFLICT (repo_id) DO NOTHING`,
		repoID, "stage-9.3-ml-effort-e2e",
	); err != nil {
		return uuid.Nil, fmt.Errorf("seed repo: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.commit
		(repo_id, sha, parent_sha, committed_at, scan_status)
		VALUES ($1::uuid, $2, NULL, now(), 'pending')
		ON CONFLICT (repo_id, sha) DO NOTHING`,
		repoID, sha,
	); err != nil {
		return uuid.Nil, fmt.Errorf("seed commit: %w", err)
	}

	scopeID := uuid.Must(uuid.NewV4())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.scope_binding
		(scope_id, repo_id, scope_kind, canonical_signature,
		 first_seen_sha, attrs_json, created_at)
		VALUES ($1::uuid, $2::uuid, 'function',
		 $3, $4, '{}'::jsonb, now())
		ON CONFLICT DO NOTHING`,
		scopeID, repoID,
		fmt.Sprintf("stage-9-3-e2e:%s:%s", repoID, sha), sha,
	); err != nil {
		return uuid.Nil, fmt.Errorf("seed scope_binding: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.rule
		(rule_id, version, pack_id, predicate_dsl,
		 severity_default, description_md, created_at)
		VALUES ($1, $2, $3, 'true',
		 'warn', 'stage-9.3 e2e synthetic rule', now())
		ON CONFLICT (rule_id, version) DO NOTHING`,
		mlEffortRuleID, mlEffortRuleVersion, mlEffortPackID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("seed rule: %w", err)
	}

	evalRunID := uuid.Must(uuid.NewV4())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.evaluation_run
		(evaluation_run_id, repo_id, sha, policy_version_id,
		 caller, created_at)
		VALUES ($1::uuid, $2::uuid, $3, $4::uuid,
		 'eval_gate', now())`,
		evalRunID, repoID, sha, policyVersionID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("seed evaluation_run: %w", err)
	}

	findingID := uuid.Must(uuid.NewV4())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.finding
		(finding_id, evaluation_run_id, repo_id, sha,
		 scope_id, rule_id, rule_version, policy_version_id,
		 metric_sample_ids, severity, delta, explanation_md,
		 created_at)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4,
		 $5::uuid, $6, $7, $8::uuid,
		 '[]'::jsonb, 'warn', 'new', '',
		 now())`,
		findingID, evalRunID, repoID, sha,
		scopeID, mlEffortRuleID, mlEffortRuleVersion, policyVersionID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("seed finding: %w", err)
	}

	hotspotID := uuid.Must(uuid.NewV4())
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO clean_code.hot_spot
		(hotspot_id, repo_id, sha, scope_id, score,
		 policy_version_id, created_at)
		VALUES ($1::uuid, $2::uuid, $3, $4::uuid, $5,
		 $6::uuid, now())`,
		hotspotID, repoID, sha, scopeID, 7.5,
		policyVersionID,
	); err != nil {
		return uuid.Nil, fmt.Errorf("seed hot_spot: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return uuid.Nil, fmt.Errorf("commit seed: %w", err)
	}
	return scopeID, nil
}

// -------------------------------------------------------------------
// Planner-binary + artefact resolution
// -------------------------------------------------------------------

// resolvePlannerBinary returns the path to a
// clean-code-refactor-planner executable. Preference:
// CLEAN_CODE_PLANNER_BIN env, then a `go build` in a temp dir.
func resolvePlannerBinary(t testingTB, root string) (string, error) {
	if bin := strings.TrimSpace(os.Getenv("CLEAN_CODE_PLANNER_BIN")); bin != "" {
		return bin, nil
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

// findServicesCleanCodeRoot walks up from the test file
// location to find the services/clean-code directory (the Go
// module root for the planner binary).
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
// ONNX artefact bundled with the planner binary.
func resolveArtefactURI(root string) (string, error) {
	if p := strings.TrimSpace(os.Getenv("CLEAN_CODE_ML_ARTEFACT_PATH")); p != "" {
		return fileURIFromPath(p)
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
	if len(abs) >= 2 && abs[1] == ':' {
		return "file:///" + strings.ReplaceAll(abs, "\\", "/"), nil
	}
	return "file://" + strings.ReplaceAll(abs, "\\", "/"), nil
}

// schemaHasRefactorPlan returns true iff the migration that
// creates `clean_code.refactor_plan` has been applied.
func schemaHasRefactorPlan(db *sql.DB) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var exists bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM information_schema.tables
			WHERE table_schema = 'clean_code' AND table_name = 'refactor_plan'
		)`).Scan(&exists); err != nil {
		return false
	}
	return exists
}

// testingTB is a tiny shim so resolvePlannerBinary can take
// `*testing.T` without importing testing here (which would
// cycle the build).
type testingTB interface {
	TempDir() string
	Helper()
}
