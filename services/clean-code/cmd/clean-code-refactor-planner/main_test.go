package main

// Stage 8.2 sanity tests for the [clean-code-refactor-planner]
// composition root. These tests intentionally do NOT spin up a
// real Postgres; they cover the env-validation surface, the
// /healthz handler, and the two-pass orchestration in
// [executeTwoPassPlan]. End-to-end coverage of the planner
// itself lives in `internal/refactor/`. The cmd-level tests
// focus on what `runPlanner` cannot: malformed env vars,
// missing keys, the opt-out branch, and the snapshot-pinning
// orchestration the production binary performs across the
// Stage 8.1 -> Stage 8.2 hand-off.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/refactor"
)

// -----------------------------------------------------------------------------
// parseTargetEnv
// -----------------------------------------------------------------------------

// TestParseTargetEnv_Happy confirms the canonical (repo_id, sha)
// pair round-trips.
func TestParseTargetEnv_Happy(t *testing.T) {
	want := uuid.Must(uuid.NewV4())
	t.Setenv(EnvRepoID, want.String())
	t.Setenv(EnvSHA, "deadbeef")
	gotRepo, gotSHA, err := parseTargetEnv()
	if err != nil {
		t.Fatalf("parseTargetEnv: %v", err)
	}
	if gotRepo != want {
		t.Errorf("repoID = %s, want %s", gotRepo, want)
	}
	if gotSHA != "deadbeef" {
		t.Errorf("sha = %q, want %q", gotSHA, "deadbeef")
	}
}

// TestParseTargetEnv_MissingRepoID confirms an empty repo_id is
// rejected with a clear error.
func TestParseTargetEnv_MissingRepoID(t *testing.T) {
	t.Setenv(EnvRepoID, "")
	t.Setenv(EnvSHA, "deadbeef")
	_, _, err := parseTargetEnv()
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), EnvRepoID) {
		t.Errorf("err = %v, missing %q in message", err, EnvRepoID)
	}
}

// TestParseTargetEnv_MalformedRepoID confirms a non-UUID
// repo_id is rejected.
func TestParseTargetEnv_MalformedRepoID(t *testing.T) {
	t.Setenv(EnvRepoID, "not-a-uuid")
	t.Setenv(EnvSHA, "deadbeef")
	_, _, err := parseTargetEnv()
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "not a UUID") {
		t.Errorf("err = %v, missing 'not a UUID' in message", err)
	}
}

// TestParseTargetEnv_ZeroRepoID confirms the all-zeros UUID is
// rejected -- a defensive guard against a misconfigured job
// that left the env unset and a parser that happens to accept
// "00000000-0000-0000-0000-000000000000".
func TestParseTargetEnv_ZeroRepoID(t *testing.T) {
	t.Setenv(EnvRepoID, uuid.Nil.String())
	t.Setenv(EnvSHA, "deadbeef")
	_, _, err := parseTargetEnv()
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "zero UUID") {
		t.Errorf("err = %v, missing 'zero UUID' in message", err)
	}
}

// TestParseTargetEnv_MissingSHA confirms an empty sha is
// rejected.
func TestParseTargetEnv_MissingSHA(t *testing.T) {
	t.Setenv(EnvRepoID, uuid.Must(uuid.NewV4()).String())
	t.Setenv(EnvSHA, "")
	_, _, err := parseTargetEnv()
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !strings.Contains(err.Error(), EnvSHA) {
		t.Errorf("err = %v, missing %q in message", err, EnvSHA)
	}
}

// TestParseTargetEnv_TrimsWhitespace confirms accidental
// trailing/leading whitespace in the env value does not break
// the parser -- common YAML-pasting hazard.
func TestParseTargetEnv_TrimsWhitespace(t *testing.T) {
	want := uuid.Must(uuid.NewV4())
	t.Setenv(EnvRepoID, "  "+want.String()+"\n")
	t.Setenv(EnvSHA, "\tdeadbeef ")
	gotRepo, gotSHA, err := parseTargetEnv()
	if err != nil {
		t.Fatalf("parseTargetEnv: %v", err)
	}
	if gotRepo != want {
		t.Errorf("repoID = %s, want %s", gotRepo, want)
	}
	if gotSHA != "deadbeef" {
		t.Errorf("sha = %q, want %q", gotSHA, "deadbeef")
	}
}

// -----------------------------------------------------------------------------
// parseBoolEnv
// -----------------------------------------------------------------------------

// TestParseBoolEnv covers the truthy / falsy + whitespace
// matrix.
func TestParseBoolEnv(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"  ", false},
		{"true", true},
		{"TRUE", true},
		{"1", true},
		{"yes", true},
		{"on", true},
		{"false", false},
		{"0", false},
		{"no", false},
		{"garbage", false},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q", c.in), func(t *testing.T) {
			if got := parseBoolEnv(c.in); got != c.want {
				t.Errorf("parseBoolEnv(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// buildMux
// -----------------------------------------------------------------------------

// TestBuildMux_HealthzOK confirms `/healthz` always returns 200
// even on opted-out deployments -- K8s liveness probes succeed.
func TestBuildMux_HealthzOK(t *testing.T) {
	mux := buildMux()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if got := w.Body.String(); got != "ok" {
		t.Errorf("body = %q, want %q", got, "ok")
	}
}

// TestBuildMux_MetricsPlaceholderOK confirms the
// `/metrics` placeholder responds 200 so Prometheus scrapes do
// not flap on the unconfigured exporter.
func TestBuildMux_MetricsPlaceholderOK(t *testing.T) {
	mux := buildMux()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "clean-code-refactor-planner") {
		t.Errorf("body = %q, missing service name marker", w.Body.String())
	}
}

// TestBuildMux_UnknownPath404 confirms an unknown path returns
// 404 -- the mux does not over-respond.
func TestBuildMux_UnknownPath404(t *testing.T) {
	mux := buildMux()
	req := httptest.NewRequest(http.MethodGet, "/api/foo", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

// -----------------------------------------------------------------------------
// executeTwoPassPlan -- the testable body of the production
// composition root. These tests pin the two-pass orchestration
// without sqlmock'ing the underlying readers / writers
// (those are covered by `internal/refactor/task_planner_sql_test.go`
// and `internal/refactor/planner_sql_test.go`). The contract
// we pin here:
//
//  1. The Stage 8.1 [refactor.PlanResult.Snapshot] is forwarded
//     VERBATIM to Stage 8.2 [refactor.TaskPlanner.PlanFromSnapshot].
//     This closes the policy_version_id race rubber-duck
//     iter-2 finding #1 identified.
//  2. [refactor.ErrNoActivePolicy] from Stage 8.1 returns
//     cleanly (binary exits 0) without invoking Stage 8.2.
//  3. Any other Stage 8.1 error wraps with `planner.Plan: ...`
//     and does NOT invoke Stage 8.2.
//  4. Any Stage 8.2 error wraps with
//     `taskPlanner.PlanFromSnapshot: ...` and still returns
//     the Stage 8.1 PlanResult to the caller (so the operator
//     log captures what Stage 8.1 produced).
// -----------------------------------------------------------------------------

// fakeStage1Planner records every Plan(repoID, sha) call and
// returns a fixed PlanResult + err.
type fakeStage1Planner struct {
	mu     sync.Mutex
	calls  []fakeStage1Call
	result refactor.PlanResult
	err    error
}

type fakeStage1Call struct {
	repoID uuid.UUID
	sha    string
}

func (f *fakeStage1Planner) Plan(_ context.Context, repoID uuid.UUID, sha string) (refactor.PlanResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeStage1Call{repoID: repoID, sha: sha})
	return f.result, f.err
}

// fakeStage2Planner records every PlanFromSnapshot call so
// the test can pin the snapshot was forwarded verbatim.
type fakeStage2Planner struct {
	mu     sync.Mutex
	calls  []fakeStage2Call
	result refactor.PlanAndTasksResult
	err    error
}

type fakeStage2Call struct {
	repoID uuid.UUID
	sha    string
	snap   refactor.PolicySnapshot
}

func (f *fakeStage2Planner) PlanFromSnapshot(
	_ context.Context,
	repoID uuid.UUID,
	sha string,
	snap refactor.PolicySnapshot,
) (refactor.PlanAndTasksResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeStage2Call{repoID: repoID, sha: sha, snap: snap})
	return f.result, f.err
}

// quietLogger returns a slog.Logger that discards all output
// so the test runner stays clean.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestExecuteTwoPassPlan_PinsSnapshotAcrossPasses is THE
// critical assertion: the snapshot Stage 8.1 returns MUST
// reach Stage 8.2 verbatim. If a future refactor accidentally
// has executeTwoPassPlan re-read the policy between passes,
// the policy_version_id pin breaks and a concurrent
// policy.activate produces a torn plan whose hot_spots and
// refactor_plan reference different policy_version_ids
// (rubber-duck iter-2 finding #1).
func TestExecuteTwoPassPlan_PinsSnapshotAcrossPasses(t *testing.T) {
	repoID := uuid.Must(uuid.NewV4())
	wantPVID := uuid.Must(uuid.NewV4())
	wantPlanID := uuid.Must(uuid.NewV4())
	wantSnap := refactor.PolicySnapshot{
		PolicyVersionID: wantPVID,
		Weights: steward.RefactorWeights{
			TopN: 7,
		},
	}
	stage1 := &fakeStage1Planner{
		result: refactor.PlanResult{
			PolicyVersionID: wantPVID,
			Snapshot:        wantSnap,
			HotSpots:        []refactor.HotSpot{{ScopeID: uuid.Must(uuid.NewV4()), PolicyVersionID: wantPVID}},
		},
	}
	stage2 := &fakeStage2Planner{
		result: refactor.PlanAndTasksResult{
			PolicyVersionID: wantPVID,
			Plan: refactor.RefactorPlan{
				PlanID: wantPlanID,
				RepoID: repoID,
				SHA:    "deadbeef",
			},
		},
	}

	planRes, taskRes, err := executeTwoPassPlan(
		context.Background(), quietLogger(), repoID, "deadbeef", stage1, stage2,
	)
	if err != nil {
		t.Fatalf("executeTwoPassPlan: %v", err)
	}

	if got := len(stage1.calls); got != 1 {
		t.Fatalf("stage1 invocations = %d, want 1", got)
	}
	if stage1.calls[0].repoID != repoID || stage1.calls[0].sha != "deadbeef" {
		t.Errorf("stage1 call args = (%s, %q), want (%s, %q)",
			stage1.calls[0].repoID, stage1.calls[0].sha, repoID, "deadbeef")
	}

	if got := len(stage2.calls); got != 1 {
		t.Fatalf("stage2 invocations = %d, want 1", got)
	}
	got2 := stage2.calls[0]
	if got2.repoID != repoID || got2.sha != "deadbeef" {
		t.Errorf("stage2 call args = (%s, %q), want (%s, %q)",
			got2.repoID, got2.sha, repoID, "deadbeef")
	}
	// THE pin: the snapshot Stage 8.1 produced MUST reach
	// Stage 8.2 verbatim.
	if got2.snap.PolicyVersionID != wantPVID {
		t.Errorf("stage2 snapshot pv_id = %s, want %s (snapshot pin broken)",
			got2.snap.PolicyVersionID, wantPVID)
	}
	if got2.snap.Weights.TopN != 7 {
		t.Errorf("stage2 snapshot TopN = %d, want 7 (snapshot pin broken)",
			got2.snap.Weights.TopN)
	}

	if planRes.PolicyVersionID != wantPVID {
		t.Errorf("planRes.PolicyVersionID = %s, want %s",
			planRes.PolicyVersionID, wantPVID)
	}
	if taskRes.Plan.PlanID != wantPlanID {
		t.Errorf("taskRes.Plan.PlanID = %s, want %s",
			taskRes.Plan.PlanID, wantPlanID)
	}
}

// TestExecuteTwoPassPlan_NoActivePolicySkipsStage82 pins the
// fresh-deploy semantic: a Stage 8.1 [refactor.ErrNoActivePolicy]
// MUST NOT cause Stage 8.2 to run (because there is no
// policy_version_id to pin against) and MUST NOT surface as
// an error (so the binary exits 0).
func TestExecuteTwoPassPlan_NoActivePolicySkipsStage82(t *testing.T) {
	repoID := uuid.Must(uuid.NewV4())
	stage1 := &fakeStage1Planner{
		err: refactor.ErrNoActivePolicy,
	}
	stage2 := &fakeStage2Planner{}

	_, taskRes, err := executeTwoPassPlan(
		context.Background(), quietLogger(), repoID, "deadbeef", stage1, stage2,
	)
	if err != nil {
		t.Fatalf("executeTwoPassPlan: err = %v, want nil (ErrNoActivePolicy must surface as clean exit)", err)
	}
	if got := len(stage1.calls); got != 1 {
		t.Errorf("stage1 invocations = %d, want 1", got)
	}
	if got := len(stage2.calls); got != 0 {
		t.Errorf("stage2 invocations = %d, want 0 (ErrNoActivePolicy must skip Stage 8.2)", got)
	}
	if taskRes.Plan.PlanID != uuid.Nil {
		t.Errorf("taskRes.Plan.PlanID = %s, want zero (no plan emitted)", taskRes.Plan.PlanID)
	}
}

// TestExecuteTwoPassPlan_Stage81ErrorPropagates pins that a
// non-ErrNoActivePolicy Stage 8.1 failure wraps with the
// canonical `planner.Plan: ...` prefix the operator runbook
// instructs SREs to grep for, and does NOT invoke Stage 8.2.
func TestExecuteTwoPassPlan_Stage81ErrorPropagates(t *testing.T) {
	repoID := uuid.Must(uuid.NewV4())
	syntheticErr := errors.New("synthetic stage-8.1 failure")
	stage1 := &fakeStage1Planner{err: syntheticErr}
	stage2 := &fakeStage2Planner{}

	_, _, err := executeTwoPassPlan(
		context.Background(), quietLogger(), repoID, "deadbeef", stage1, stage2,
	)
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !errors.Is(err, syntheticErr) {
		t.Errorf("err = %v, want errors.Is(..., syntheticErr)", err)
	}
	if !strings.Contains(err.Error(), "planner.Plan") {
		t.Errorf("err = %q, want canonical %q prefix", err.Error(), "planner.Plan")
	}
	if got := len(stage2.calls); got != 0 {
		t.Errorf("stage2 invocations = %d, want 0 (Stage 8.1 failure must short-circuit Stage 8.2)", got)
	}
}

// TestExecuteTwoPassPlan_Stage82ErrorPropagates pins the
// Stage 8.2 failure path: a non-nil error from
// PlanFromSnapshot wraps with `taskPlanner.PlanFromSnapshot: ...`
// AND surfaces the Stage 8.1 PlanResult so the caller log
// captures what Stage 8.1 produced before the failure.
func TestExecuteTwoPassPlan_Stage82ErrorPropagates(t *testing.T) {
	repoID := uuid.Must(uuid.NewV4())
	wantPVID := uuid.Must(uuid.NewV4())
	syntheticErr := errors.New("synthetic stage-8.2 failure")
	stage1 := &fakeStage1Planner{
		result: refactor.PlanResult{
			PolicyVersionID: wantPVID,
			Snapshot:        refactor.PolicySnapshot{PolicyVersionID: wantPVID},
			HotSpots:        []refactor.HotSpot{{ScopeID: uuid.Must(uuid.NewV4()), PolicyVersionID: wantPVID}},
		},
	}
	stage2 := &fakeStage2Planner{err: syntheticErr}

	planRes, _, err := executeTwoPassPlan(
		context.Background(), quietLogger(), repoID, "deadbeef", stage1, stage2,
	)
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !errors.Is(err, syntheticErr) {
		t.Errorf("err = %v, want errors.Is(..., syntheticErr)", err)
	}
	if !strings.Contains(err.Error(), "taskPlanner.PlanFromSnapshot") {
		t.Errorf("err = %q, want canonical %q prefix", err.Error(), "taskPlanner.PlanFromSnapshot")
	}
	// The Stage 8.1 PlanResult must still surface so the caller
	// can log what landed before the Stage 8.2 failure.
	if planRes.PolicyVersionID != wantPVID {
		t.Errorf("planRes.PolicyVersionID = %s, want %s (Stage 8.1 result must surface on Stage 8.2 failure)",
			planRes.PolicyVersionID, wantPVID)
	}
	if len(planRes.HotSpots) != 1 {
		t.Errorf("len(planRes.HotSpots) = %d, want 1 (Stage 8.1 hot_spots must surface on Stage 8.2 failure)",
			len(planRes.HotSpots))
	}
}

// TestExecuteTwoPassPlan_ContextCancelled pins that a
// cancelled context surfaces from Stage 8.1's err (the fake
// stage1 returns ctx.Err() to mimic the real
// [refactor.Planner.Plan] behaviour). Defensive coverage:
// the executor MUST NOT swallow context cancellation as a
// "no active policy" or any other clean-exit signal.
func TestExecuteTwoPassPlan_ContextCancelled(t *testing.T) {
	repoID := uuid.Must(uuid.NewV4())
	stage1 := &fakeStage1Planner{err: context.Canceled}
	stage2 := &fakeStage2Planner{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := executeTwoPassPlan(ctx, quietLogger(), repoID, "deadbeef", stage1, stage2)
	if err == nil {
		t.Fatalf("err = nil, want non-nil")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want errors.Is(..., context.Canceled)", err)
	}
	if got := len(stage2.calls); got != 0 {
		t.Errorf("stage2 invocations = %d, want 0 (cancelled ctx must short-circuit Stage 8.2)", got)
	}
}

// Compile-time assertion that the production
// [*refactor.Planner] satisfies our [stage1Planner] interface.
// If a future refactor renames the Plan method or changes its
// signature, this fails to compile -- the test is a
// belt-and-braces guard against silent runtime drift between
// the test fakes and the production type.
var _ stage1Planner = (*refactor.Planner)(nil)

// Compile-time assertion that the production
// [*refactor.TaskPlanner] satisfies our [stage2Planner]
// interface. Same drift-guard semantic as above.
var _ stage2Planner = (*refactor.TaskPlanner)(nil)
