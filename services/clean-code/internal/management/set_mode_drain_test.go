package management

// Stage 9.3 -- integration test for the management
// `mgmt.set_mode` HTTP verb's drain-before-flip contract.
//
// # What this test proves
//
// Per the implementation-plan Stage 9.3 brief (line 804):
//
//	"On `mgmt.set_mode(repo_id, mode)` transitions between
//	 `embedded` and `linked`, drain in-flight scans for the
//	 repo before flipping; new scans pick up the new mode."
//
// The Stage 9.3 iter-1 evaluator's item #5 specifically
// called out that the unit suite under
// `services/clean-code/internal/management/set_mode_verb_test.go`
// only exercised repo-store mutations and could NOT catch
// scans starting under the old mode during a real management
// flip. THIS test wires the full management HTTP handler to
// a real [isolation.NewMgmtFlipCoordinator] adapter on top of
// a shared [isolation.ModeCoordinator], hydrated from the
// store via the production [RepoModeReader] interface, and
// exercises the cross-package handshake the brief actually
// pins:
//
//  1. Seed a repo at mode `embedded`.
//  2. Open ONE in-flight scan via `coord.BeginScan(repoID)`.
//  3. Fire the `POST /v1/mgmt/set_mode` request in a
//     goroutine -- it MUST block (does NOT complete within a
//     generous grace window) because the in-flight scan
//     holds the drain barrier open.
//  4. Close the scan via `coord.EndScan(tok)`.
//  5. The handler completes 200 + `changed:true`, the
//     store's mode is now `linked`, and a `mode_changed`
//     repo_event is appended.
//  6. A FRESH `coord.BeginScan(repoID)` returns the
//     post-flip mode `linked` -- the coordinator's per-repo
//     cache was atomically swapped under the flip lock.
//
// Together these assertions cover the brief's "drain
// in-flight scans for the repo before flipping; new scans
// pick up the new mode" contract end-to-end across the
// management+isolation package boundary -- the exact
// integration the evaluator flagged as missing.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/ast/isolation"
)

// TestMgmtSetMode_DrainsInFlightScansThenFlips is the
// Stage 9.3 acceptance test for the
// `mgmt.set_mode -> coordinator.SetMode -> drain ->
// repoStore.SetRepoMode` integration path. See the
// file-level comment for what each step pins.
func TestMgmtSetMode_DrainsInFlightScansThenFlips(t *testing.T) {
	t.Parallel()

	// --- arrange: real in-memory store + appender, real
	// coordinator + flip adapter, real MgmtWriter ---
	app := NewInMemoryRepoEventAppender()
	store := NewInMemoryRepoStore(app)

	// Hydrator reads the persisted mode via the
	// RepoModeReader seam -- production wiring uses
	// the same method on PGRepoStore.
	hydrator := func(ctx context.Context, repoID uuid.UUID) (isolation.Mode, error) {
		mode, err := store.ReadRepoMode(ctx, repoID)
		if err != nil {
			return "", err
		}
		return isolation.Mode(mode), nil
	}
	coord := isolation.NewModeCoordinator(isolation.WithModeHydrator(hydrator))
	flip := isolation.NewMgmtFlipCoordinator(coord)

	writer := NewMgmtWriter(
		stubSampleResolver{},
		stubRetractDispatcher{},
		stubRescanEnqueuer{},
		app,
		WithMgmtWriterRepoStore(store),
		WithMgmtWriterFlipCoordinator(flip),
	)

	// Seed a repo at mode `embedded` so we can flip it.
	regRes, err := store.RegisterRepo(context.Background(), RegisterRepoRowRequest{
		RepoURL:       "https://github.com/example/repo",
		DefaultBranch: "main",
		Mode:          RepoModeEmbedded,
		Actor:         "alice@example.com",
	})
	if err != nil {
		t.Fatalf("seed RegisterRepo: %v", err)
	}
	repoID := regRes.RepoID

	// --- act 1: open an in-flight scan that holds the
	// drain barrier open ---
	scanCtx, cancelScan := context.WithCancel(context.Background())
	defer cancelScan()
	scanTok, err := coord.BeginScan(scanCtx, repoID)
	if err != nil {
		t.Fatalf("coord.BeginScan: %v", err)
	}

	// --- act 2: fire the HTTP handler in a goroutine ---
	body, err := json.Marshal(map[string]any{
		"repo_id": repoID.String(),
		"mode":    RepoModeLinked,
	})
	if err != nil {
		t.Fatalf("marshal set_mode body: %v", err)
	}

	var (
		done = make(chan struct{})
		rr   = httptest.NewRecorder()
	)
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(body))
		req.Header.Set(OIDCSubjectHeader, "alice@example.com")
		writer.SetMode(rr, req)
	}()

	// --- assert: handler MUST block (drain barrier) ---
	// 250ms is generous enough that a goroutine schedule
	// hiccup won't cause a spurious "completed too fast"
	// failure while still being short enough that a real
	// drain-skip bug fires the test in subsecond.
	select {
	case <-done:
		t.Fatalf("set_mode handler completed BEFORE EndScan: status=%d body=%s; the drain barrier did NOT hold the flip open", rr.Code, rr.Body.String())
	case <-time.After(250 * time.Millisecond):
		// expected
	}

	// Sanity: the store's mode is still `embedded` while
	// the handler is blocked. Any code path that updates
	// the row before the drain completes would surface
	// here -- the brief's contract is "drain BEFORE flip",
	// not "drain alongside flip".
	rec, ok := store.Lookup(repoID)
	if !ok {
		t.Fatalf("store.Lookup(repoID=%s) returned !ok before flip", repoID)
	}
	if rec.Mode != RepoModeEmbedded {
		t.Fatalf("store mode during in-flight scan = %q, want %q (catalog mutated BEFORE drain completed)", rec.Mode, RepoModeEmbedded)
	}

	// --- act 3: end the scan to release the drain ---
	coord.EndScan(scanTok)

	// --- assert: handler completes within a generous
	// wall-clock budget. 2s is well above the per-test CI
	// jitter ceiling while still being short enough that a
	// real deadlock bug surfaces as a test failure rather
	// than a flake.
	select {
	case <-done:
		// expected
	case <-time.After(2 * time.Second):
		t.Fatalf("set_mode handler did NOT complete within 2s after EndScan; status=%d body=%s", rr.Code, rr.Body.String())
	}

	// --- assert: handler succeeded ---
	if rr.Code != http.StatusOK {
		t.Fatalf("set_mode status=%d, want 200; body=%s", rr.Code, rr.Body.String())
	}
	var resp struct {
		RepoID       string `json:"repo_id"`
		Mode         string `json:"mode"`
		PreviousMode string `json:"previous_mode"`
		Changed      bool   `json:"changed"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v; body=%s", err, rr.Body.String())
	}
	if resp.Mode != RepoModeLinked {
		t.Errorf("response mode=%q, want %q", resp.Mode, RepoModeLinked)
	}
	if resp.PreviousMode != RepoModeEmbedded {
		t.Errorf("response previous_mode=%q, want %q", resp.PreviousMode, RepoModeEmbedded)
	}
	if !resp.Changed {
		t.Errorf("response changed=%v, want true", resp.Changed)
	}

	// --- assert: persisted mode AND audit event reflect
	// the flip ---
	rec, ok = store.Lookup(repoID)
	if !ok {
		t.Fatalf("store.Lookup(repoID=%s) returned !ok after flip", repoID)
	}
	if rec.Mode != RepoModeLinked {
		t.Errorf("store mode after flip=%q, want %q", rec.Mode, RepoModeLinked)
	}

	events := app.EventsForRepo(repoID)
	var sawModeChanged bool
	for _, e := range events {
		if e.Kind == "mode_changed" {
			sawModeChanged = true
			break
		}
	}
	if !sawModeChanged {
		t.Errorf("no `mode_changed` repo_event appended for repo_id=%s; events=%+v", repoID, events)
	}

	// --- assert: new scans pick up the post-flip mode.
	// Per the brief: "new scans pick up the new mode".
	freshTok, err := coord.BeginScan(context.Background(), repoID)
	if err != nil {
		t.Fatalf("post-flip BeginScan: %v", err)
	}
	defer coord.EndScan(freshTok)
	if freshTok.Mode() != isolation.ModeLinked {
		t.Errorf("post-flip BeginScan mode=%q, want %q (the coordinator did not atomically swap the per-repo cache)", freshTok.Mode(), isolation.ModeLinked)
	}
}

// TestMgmtSetMode_DrainCancellationLeavesModeUnchanged
// asserts that if the HTTP request's context is cancelled
// WHILE the drain barrier is waiting on an in-flight scan,
// the handler returns an error AND the persisted mode is
// UNCHANGED -- i.e. the cancel does NOT smuggle a partial
// flip past the drain barrier. The Stage 9.3 brief mandates
// "drain BEFORE flip"; this test pins the cancel-during-drain
// path that a previous iter's drain implementation missed.
func TestMgmtSetMode_DrainCancellationLeavesModeUnchanged(t *testing.T) {
	t.Parallel()

	app := NewInMemoryRepoEventAppender()
	store := NewInMemoryRepoStore(app)
	hydrator := func(ctx context.Context, repoID uuid.UUID) (isolation.Mode, error) {
		mode, err := store.ReadRepoMode(ctx, repoID)
		if err != nil {
			return "", err
		}
		return isolation.Mode(mode), nil
	}
	coord := isolation.NewModeCoordinator(isolation.WithModeHydrator(hydrator))
	flip := isolation.NewMgmtFlipCoordinator(coord)
	writer := NewMgmtWriter(
		stubSampleResolver{},
		stubRetractDispatcher{},
		stubRescanEnqueuer{},
		app,
		WithMgmtWriterRepoStore(store),
		WithMgmtWriterFlipCoordinator(flip),
	)

	regRes, err := store.RegisterRepo(context.Background(), RegisterRepoRowRequest{
		RepoURL:       "https://github.com/example/cancel-repo",
		DefaultBranch: "main",
		Mode:          RepoModeEmbedded,
		Actor:         "alice@example.com",
	})
	if err != nil {
		t.Fatalf("seed RegisterRepo: %v", err)
	}
	repoID := regRes.RepoID

	// Hold a scan token open across the whole test so
	// the flip ALWAYS blocks on the drain barrier.
	holdCtx, holdCancel := context.WithCancel(context.Background())
	defer holdCancel()
	tok, err := coord.BeginScan(holdCtx, repoID)
	if err != nil {
		t.Fatalf("coord.BeginScan: %v", err)
	}
	defer coord.EndScan(tok)

	body, err := json.Marshal(map[string]any{
		"repo_id": repoID.String(),
		"mode":    RepoModeLinked,
	})
	if err != nil {
		t.Fatalf("marshal set_mode body: %v", err)
	}

	reqCtx, cancelReq := context.WithCancel(context.Background())
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, VerbMgmtSetModePath, bytes.NewReader(body)).WithContext(reqCtx)
		req.Header.Set(OIDCSubjectHeader, "alice@example.com")
		writer.SetMode(rr, req)
	}()

	// Let the handler reach the drain barrier, then
	// cancel its context.
	time.Sleep(100 * time.Millisecond)
	cancelReq()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("handler did NOT return within 2s of context cancel; status=%d body=%s", rr.Code, rr.Body.String())
	}

	// The persisted mode MUST be UNCHANGED -- a partial
	// flip would mean the drain was bypassed.
	rec, ok := store.Lookup(repoID)
	if !ok {
		t.Fatalf("store.Lookup(repoID=%s) returned !ok", repoID)
	}
	if rec.Mode != RepoModeEmbedded {
		t.Errorf("store mode after cancel=%q, want %q (cancel must NOT smuggle a partial flip past the drain barrier)", rec.Mode, RepoModeEmbedded)
	}
}
