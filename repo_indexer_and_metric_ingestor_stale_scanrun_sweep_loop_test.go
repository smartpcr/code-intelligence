// Root-module test file. Originally an `//go:build e2e`-tagged
// godog scenario suite; the build tag was REMOVED in iter-4 so
// the package always compiles, giving the Forge per-iter
// `go test ./...` gate at least one package to discover from
// the worktree root. A package-level [TestMain] proxy below
// shells out to the real test suite inside
// `services/clean-code/`. The original godog scenario
// `TestE2E_repo_indexer_..._sweep_loop` remains intact below
// for the CI lane that explicitly opts into the e2e suite via
// env variables -- but in proxy mode the [TestMain] short-
// circuits before `m.Run()` so the original test never fires
// during gate runs.
package e2e

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cucumber/godog"
	_ "github.com/lib/pq"
)

// envGateProxyNested is the recursion-guard sentinel set on
// the INNER `go test ./...` invocation. When [TestMain] sees
// this env var it skips the proxy logic and runs [m.Run]
// directly so the inner test process behaves like a normal
// `go test` of the root package (the existing e2e scenario
// will then `t.Skip` because no PG env vars are set, which is
// the desired no-op behaviour).
const envGateProxyNested = "CLEAN_CODE_GATE_PROXY_NESTED"

// cleanCodeModuleRelPath is the path FROM the worktree root TO
// the inner Go module that owns the real Clean Code service
// code. The proxy shells `go test ./...` here.
const cleanCodeModuleRelPath = "services/clean-code"

// TestMain is a proxy that delegates `go test ./...` runs from
// the WORKTREE ROOT (this `go.mod` covers only this file) to
// the inner `services/clean-code` module which holds every
// real production package + test. Forge's per-iter test gate
// runs `go test ./... -run '<regex>'` from the worktree root;
// before this proxy the gate failed with `matched no packages`
// because the root module had only one e2e-tagged file.
//
// Behaviour summary:
//
//   - When env `CLEAN_CODE_GATE_PROXY_NESTED=1` is set, skip
//     the proxy and run [m.Run] (the recursion-guard path; lets
//     the inner shell-out behave like a normal test invocation
//     and prevents an infinite shell-out loop).
//
//   - Otherwise, locate the worktree root by walking up for
//     `.git` (file OR directory; submodule and worktree shapes
//     both qualify), then exec `go test ./...` in
//     `services/clean-code` forwarding `-run`, `-v`,
//     `-timeout`, `-cpu`, `-short` from the outer flag set and
//     ALWAYS force inner `-count=1` so the inner cache cannot
//     mask a regression. The recursion-guard env var is set on
//     the inner process. Exit code of the inner process is
//     propagated faithfully so the outer `go test` reports the
//     same pass/fail signal.
//
// What is NOT forwarded:
//
//   - `-race`: a `go test` BUILD flag handled by the toolchain
//     wrapper, NOT a runtime flag on the test binary, so it is
//     not always visible via `flag.Lookup("test.race")` on
//     every Go build. If a gate requires race detection, set
//     `GOFLAGS=-race` in the gate environment so it applies to
//     the inner build too.
//
//   - `-count` (other than the forced `-count=1`): forcing
//     `-count=1` is the cache-bypass guarantee. Outer
//     `-count=N` with N>1 would multiply inner test work
//     against the gate's budget; the gate's design assumes
//     `-count=1` semantics.
//
// `flag.Parse` is called defensively at the top in case a
// future runtime defers parsing past `TestMain` entry.
//
// Note that even in proxy mode the test binary IS compiled and
// must therefore satisfy the build gate: every import in this
// file (and every other tracked test file in the root package)
// must resolve.
func TestMain(m *testing.M) {
	if os.Getenv(envGateProxyNested) == "1" {
		os.Exit(m.Run())
	}
	if !flag.Parsed() {
		flag.Parse()
	}

	root, err := findWorktreeRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "clean-code gate proxy: locate worktree root: %v\n", err)
		os.Exit(1)
	}
	innerDir := filepath.Join(root, cleanCodeModuleRelPath)
	if st, statErr := os.Stat(innerDir); statErr != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "clean-code gate proxy: inner module dir %q not found: %v\n", innerDir, statErr)
		os.Exit(1)
	}

	args := buildProxyArgs()
	cmd := exec.Command("go", args...)
	cmd.Dir = innerDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), envGateProxyNested+"=1")

	runErr := cmd.Run()
	if runErr == nil {
		os.Exit(0)
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		os.Exit(exitErr.ExitCode())
	}
	fmt.Fprintf(os.Stderr, "clean-code gate proxy: inner `go test ./...` failed to launch: %v\n", runErr)
	os.Exit(1)
}

// findWorktreeRoot walks UP from the current working directory
// until it finds an entry named `.git`. The entry may be a
// directory (regular checkout) OR a file (submodule / linked
// worktree shape); both are accepted. Returns the directory
// that CONTAINS the `.git` entry. The gate is expected to run
// with cwd == worktree root so the first iteration usually
// succeeds; the walk-up is belt-and-braces for an operator
// invocation from a subdir.
func findWorktreeRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("os.Getwd: %w", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no .git entry found walking up from cwd")
		}
		dir = parent
	}
}

// buildProxyArgs assembles the `go test ./... ...` argv for
// the inner `go test` invocation. Forwards the test flags the
// gate cares about and ALWAYS appends `-count=1` so the inner
// test cache cannot mask a regression.
func buildProxyArgs() []string {
	args := []string{"test", "./..."}

	if v := flag.Lookup("test.run"); v != nil && v.Value.String() != "" {
		args = append(args, "-run", v.Value.String())
	}
	if v := flag.Lookup("test.timeout"); v != nil && v.Value.String() != "" && v.Value.String() != "0s" {
		args = append(args, "-timeout", v.Value.String())
	}
	if v := flag.Lookup("test.cpu"); v != nil && v.Value.String() != "" {
		args = append(args, "-cpu", v.Value.String())
	}
	if hasFlag("test.v") {
		args = append(args, "-v")
	}
	if hasFlag("test.short") {
		args = append(args, "-short")
	}
	args = append(args, "-count=1")
	if strings.TrimSpace(strings.Join(args, "")) == "" {
		args = []string{"test", "./...", "-count=1"}
	}
	return args
}

// hasFlag returns true when the named bool test flag exists
// and is set to "true". Used for `-v` / `-short` which are
// boolean flags whose mere presence on the outer command line
// implies the inner side should also see them.
func hasFlag(name string) bool {
	v := flag.Lookup(name)
	if v == nil {
		return false
	}
	return v.Value.String() == "true"
}

// requireEnvSweep returns the value of the named environment variable,
// calling t.Skip when unset or empty.  Each *_test.go file in the e2e
// package may carry its own copy so files stay self-contained.
func requireEnvSweep(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("environment variable %s is not set; skipping", name)
	}
	return v
}

// ---------- shared state ----------

type sweepState struct {
	db        *sql.DB
	scanRunID string
	commitID  string
}

func newSweepState() (*sweepState, error) {
	dsn := os.Getenv("CLEAN_CODE_PG_URL")
	if dsn == "" {
		return nil, fmt.Errorf("CLEAN_CODE_PG_URL is not set")
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening postgres: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("pinging postgres: %w", err)
	}
	return &sweepState{db: db}, nil
}

func (s *sweepState) cleanup() {
	if s.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if s.scanRunID != "" {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM scan_runs WHERE id = $1`, s.scanRunID)
	}
	if s.commitID != "" {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM commits WHERE id = $1`, s.commitID)
	}
	_ = s.db.Close()
}

// ---------- Scenario: stale-scan-run-becomes-failed ----------

func (s *sweepState) aScanRunRowWithStatusWhoseUpdatedAtIsOlderThan30Minutes(status string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	staleTime := time.Now().UTC().Add(-31 * time.Minute)
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO scan_runs (status, created_at, updated_at)
		 VALUES ($1, $2, $2)
		 RETURNING id`, status, staleTime).Scan(&s.scanRunID)
	if err != nil {
		return fmt.Errorf("inserting stale scan_run: %w", err)
	}
	return nil
}

func (s *sweepState) theSweepLoopExecutes() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The sweep is expected to run on a short interval in the compose
	// environment.  Poll the scan_run row until it leaves "running" or
	// until the context deadline.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for sweep to process scan_run %s", s.scanRunID)
		case <-ticker.C:
			var status string
			err := s.db.QueryRowContext(ctx,
				`SELECT status FROM scan_runs WHERE id = $1`, s.scanRunID).Scan(&status)
			if err != nil {
				return fmt.Errorf("querying scan_run status: %w", err)
			}
			if status != "running" {
				return nil
			}
		}
	}
}

func (s *sweepState) theScanRunRowTransitionsToStatus(expected string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM scan_runs WHERE id = $1`, s.scanRunID).Scan(&status)
	if err != nil {
		return fmt.Errorf("querying scan_run status: %w", err)
	}
	if status != expected {
		return fmt.Errorf("scan_run %s: want status %q, got %q", s.scanRunID, expected, status)
	}
	return nil
}

func (s *sweepState) theScanRunRowDoesNOTHaveStatusOrStatus(forbidden1, forbidden2 string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var status string
	err := s.db.QueryRowContext(ctx,
		`SELECT status FROM scan_runs WHERE id = $1`, s.scanRunID).Scan(&status)
	if err != nil {
		return fmt.Errorf("querying scan_run status: %w", err)
	}
	if status == forbidden1 || status == forbidden2 {
		return fmt.Errorf("scan_run %s: has forbidden status %q", s.scanRunID, status)
	}
	return nil
}

// ---------- Scenario: stale-commit-becomes-failed ----------

func (s *sweepState) aCommitRowWithScanStatusLinkedToAScanRunThatWasJustMarked(scanStatus, runStatus string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// The feature wording "scan_run that was just marked '<runStatus>'" describes
	// the *post-sweep* status of the scan_run, not its pre-sweep status.  Per the
	// feature file, the sweep only targets scan_runs stuck in "running"; a row
	// already inserted in a terminal status (e.g. "failed") would never be
	// revisited and the commit cascade would never fire, causing
	// theSweepFinalises to hang for 30 s before timing out.
	//
	// We therefore set up a stale "running" scan_run and rely on the sweep to
	// (1) transition the run "running" -> runStatus and (2) cascade the linked
	// commit's "scanning" scan_status in the same pass.  runStatus is preserved
	// as a parameter so the step expression still binds the feature wording,
	// and is validated below to keep that contract explicit.
	if runStatus == "running" {
		return fmt.Errorf(
			"feature must specify a terminal post-sweep run status, got %q", runStatus)
	}

	staleTime := time.Now().UTC().Add(-31 * time.Minute)
	err := s.db.QueryRowContext(ctx,
		`INSERT INTO scan_runs (status, created_at, updated_at)
		 VALUES ('running', $1, $1)
		 RETURNING id`, staleTime).Scan(&s.scanRunID)
	if err != nil {
		return fmt.Errorf("inserting stale running scan_run: %w", err)
	}

	// Insert a commit linked to that scan_run still in scanStatus ("scanning").
	// When the sweep runs, the run transitions to runStatus and the commit
	// scan_status is cascaded.
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO commits (scan_run_id, scan_status, created_at, updated_at)
		 VALUES ($1, $2, now(), now())
		 RETURNING id`, s.scanRunID, scanStatus).Scan(&s.commitID)
	if err != nil {
		return fmt.Errorf("inserting commit with scan_status %q: %w", scanStatus, err)
	}
	return nil
}

func (s *sweepState) theSweepFinalises() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Poll the commit row until it leaves "scanning" or until timeout.
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for sweep to finalise commit %s", s.commitID)
		case <-ticker.C:
			var scanStatus string
			err := s.db.QueryRowContext(ctx,
				`SELECT scan_status FROM commits WHERE id = $1`, s.commitID).Scan(&scanStatus)
			if err != nil {
				return fmt.Errorf("querying commit scan_status: %w", err)
			}
			if scanStatus != "scanning" {
				return nil
			}
		}
	}
}

func (s *sweepState) theCommitRowTransitionsToScanStatus(expected string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var scanStatus string
	err := s.db.QueryRowContext(ctx,
		`SELECT scan_status FROM commits WHERE id = $1`, s.commitID).Scan(&scanStatus)
	if err != nil {
		return fmt.Errorf("querying commit scan_status: %w", err)
	}
	if scanStatus != expected {
		return fmt.Errorf("commit %s: want scan_status %q, got %q", s.commitID, expected, scanStatus)
	}
	return nil
}

// ---------- Godog wiring ----------

func InitializeScenario_repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop(ctx *godog.ScenarioContext) {
	var s *sweepState

	ctx.Before(func(ctx context.Context, sc *godog.Scenario) (context.Context, error) {
		var err error
		s, err = newSweepState()
		if err != nil {
			return ctx, fmt.Errorf("sweep state init: %w", err)
		}
		return ctx, nil
	})

	ctx.After(func(ctx context.Context, sc *godog.Scenario, err error) (context.Context, error) {
		if s != nil {
			s.cleanup()
		}
		return ctx, nil
	})

	// stale-scan-run-becomes-failed
	ctx.Step(`^a scan_run row with status "([^"]*)" whose updated_at is older than 30 minutes$`,
		func(status string) error { return s.aScanRunRowWithStatusWhoseUpdatedAtIsOlderThan30Minutes(status) })
	ctx.Step(`^the sweep loop executes$`,
		func() error { return s.theSweepLoopExecutes() })
	ctx.Step(`^the scan_run row transitions to status "([^"]*)"$`,
		func(expected string) error { return s.theScanRunRowTransitionsToStatus(expected) })
	ctx.Step(`^the scan_run row does NOT have status "([^"]*)" or "([^"]*)"$`,
		func(f1, f2 string) error { return s.theScanRunRowDoesNOTHaveStatusOrStatus(f1, f2) })

	// stale-commit-becomes-failed
	ctx.Step(`^a commit row with scan_status "([^"]*)" linked to a scan_run that was just marked "([^"]*)"$`,
		func(scanStatus, runStatus string) error {
			return s.aCommitRowWithScanStatusLinkedToAScanRunThatWasJustMarked(scanStatus, runStatus)
		})
	ctx.Step(`^the sweep finalises$`,
		func() error { return s.theSweepFinalises() })
	ctx.Step(`^the commit row transitions to scan_status "([^"]*)"$`,
		func(expected string) error { return s.theCommitRowTransitionsToScanStatus(expected) })
}

func TestE2E_repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop(t *testing.T) {
	requireEnvSweep(t, "CLEAN_CODE_PG_URL")

	suite := godog.TestSuite{
		ScenarioInitializer: InitializeScenario_repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop,
		Options: &godog.Options{
			Format:   "pretty",
			Paths:    []string{"repo_indexer_and_metric_ingestor_stale_scanrun_sweep_loop.feature"},
			TestingT: t,
			Strict:   true,
		},
	}
	if suite.Run() != 0 {
		t.Fatal("non-zero status returned, failed to run feature tests")
	}
}
