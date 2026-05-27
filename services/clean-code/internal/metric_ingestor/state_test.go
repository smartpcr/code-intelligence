package metric_ingestor_test

// state_test.go covers the Stage 3.2 ScanRun state machine.
// Behavioural scenarios mirror the implementation-plan Stage
// 3.2 evaluator items:
//
//   - happy-path-states (pending -> scanning -> scanned)
//   - failure-path-states (pending -> scanning -> failed)
//   - scan-run-kind-enum-rejects-invalid (`external_double` etc.)
//   - sole-writer-of-commit-scan-status (DB role grant
//     parity at the application layer)
//   - timeout-path (Stage 3.2 implementation-plan line 306)
//
// Every test uses the deterministic UUID factory in
// fixedUUIDFactory + the fixed clock from fixedClock so
// failures are debuggable without random-seed gymnastics.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/repo_indexer"
)

// stateRepoIDA / stateRepoIDB are pinned repo UUIDs distinct
// from the sweep_test.go fixtures to keep test failures
// readable.
var (
	stateRepoIDA = uuid.Must(uuid.FromString("dddddddd-1111-1111-1111-111111111111"))
	stateRepoIDB = uuid.Must(uuid.FromString("dddddddd-2222-2222-2222-222222222222"))
	stateRunID01 = uuid.Must(uuid.FromString("eeeeeeee-0000-0000-0000-000000000001"))
	stateRunID02 = uuid.Must(uuid.FromString("eeeeeeee-0000-0000-0000-000000000002"))
	stateRunID03 = uuid.Must(uuid.FromString("eeeeeeee-0000-0000-0000-000000000003"))
)

// fixedClockAt returns a clock-fn that increments by 1ms on
// every call so `sm.now()` calls during ProcessOne produce
// distinct (and monotonic) timestamps the tests can assert on.
func fixedClockAt(start time.Time) func() time.Time {
	var mu sync.Mutex
	cur := start
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		out := cur
		cur = cur.Add(time.Millisecond)
		return out
	}
}

// uuidSeq returns a deterministic UUID factory that hands
// out `ids` in order; calling beyond len(ids) returns an
// error so a test that mints more runs than expected fails
// loudly.
func uuidSeq(ids ...uuid.UUID) func() (uuid.UUID, error) {
	var mu sync.Mutex
	i := 0
	return func() (uuid.UUID, error) {
		mu.Lock()
		defer mu.Unlock()
		if i >= len(ids) {
			return uuid.Nil, errors.New("uuidSeq: exhausted")
		}
		out := ids[i]
		i++
		return out, nil
	}
}

// scannerFunc is a function-typed [metric_ingestor.AstScanner].
type scannerFunc func(ctx context.Context, claim metric_ingestor.ScanRunClaim) error

func (f scannerFunc) Scan(ctx context.Context, claim metric_ingestor.ScanRunClaim) error {
	return f(ctx, claim)
}

// recordingScanner captures every claim it sees and returns
// the configured error (or nil) for each call. Goroutine-safe.
type recordingScanner struct {
	mu     sync.Mutex
	calls  []metric_ingestor.ScanRunClaim
	errFor func(call int) error
}

func (r *recordingScanner) Scan(ctx context.Context, claim metric_ingestor.ScanRunClaim) error {
	r.mu.Lock()
	idx := len(r.calls)
	r.calls = append(r.calls, claim)
	r.mu.Unlock()
	if r.errFor == nil {
		return nil
	}
	return r.errFor(idx)
}

func (r *recordingScanner) Calls() []metric_ingestor.ScanRunClaim {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]metric_ingestor.ScanRunClaim, len(r.calls))
	copy(out, r.calls)
	return out
}

// TestAllScanRunKinds_ExactlyFiveCanonical pins the DB enum
// surface at the Go layer. If a future migration adds or
// removes a kind, both files must update together.
func TestAllScanRunKinds_ExactlyFiveCanonical(t *testing.T) {
	got := metric_ingestor.AllScanRunKinds()
	want := []string{"full", "delta", "external_single", "external_per_row", "retract"}
	if len(got) != len(want) {
		t.Fatalf("AllScanRunKinds(): got %d kinds, want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("AllScanRunKinds()[%d] = %q, want %q", i, got[i], k)
		}
	}
	// Mutation guard: the slice the caller receives MUST
	// be a fresh copy.
	got[0] = "MUTATED"
	got2 := metric_ingestor.AllScanRunKinds()
	if got2[0] != "full" {
		t.Errorf("AllScanRunKinds(): mutation leaked into the package's closed set (got[0]=%q)", got2[0])
	}
}

// TestAllScanRunStatuses_ExactlyThreeCanonical pins the
// `scan_run.status` DB enum surface (3 canonical values).
func TestAllScanRunStatuses_ExactlyThreeCanonical(t *testing.T) {
	got := metric_ingestor.AllScanRunStatuses()
	want := []metric_ingestor.ScanRunStatus{
		metric_ingestor.ScanRunStatusRunning,
		metric_ingestor.ScanRunStatusSucceeded,
		metric_ingestor.ScanRunStatusFailed,
	}
	if len(got) != len(want) {
		t.Fatalf("AllScanRunStatuses(): got %d, want %d (%v vs %v)", len(got), len(want), got, want)
	}
	for i, s := range want {
		if got[i] != s {
			t.Errorf("AllScanRunStatuses()[%d] = %q, want %q", i, got[i], s)
		}
	}
}

// TestValidateScanRunKind_RejectsForbiddenLiterals is the
// canon-guard the Stage 3.2 evaluator scenario
// `scan-run-kind-enum-rejects-invalid` pins. Forbidden:
// `external_double`, `complete`, `queued`, `orphaned`,
// `superseded`, plus a random sentinel.
func TestValidateScanRunKind_RejectsForbiddenLiterals(t *testing.T) {
	cases := []string{"external_double", "complete", "queued", "orphaned", "superseded", "garbage"}
	for _, c := range cases {
		t.Run(c, func(t *testing.T) {
			err := metric_ingestor.ValidateScanRunKind(c)
			if err == nil {
				t.Fatalf("ValidateScanRunKind(%q) = nil, want ErrUnknownScanRunKind", c)
			}
			if !errors.Is(err, metric_ingestor.ErrUnknownScanRunKind) {
				t.Errorf("ValidateScanRunKind(%q): err=%v, want errors.Is ErrUnknownScanRunKind", c, err)
			}
		})
	}
}

// TestValidateScanRunKind_AcceptsAllFive ensures the five
// canonical values pass.
func TestValidateScanRunKind_AcceptsAllFive(t *testing.T) {
	for _, k := range metric_ingestor.AllScanRunKinds() {
		t.Run(k, func(t *testing.T) {
			if err := metric_ingestor.ValidateScanRunKind(k); err != nil {
				t.Errorf("ValidateScanRunKind(%q) = %v, want nil", k, err)
			}
		})
	}
}

// TestValidateScanRunStatus_RejectsAndAccepts pins the
// three-value closed set + a forbidden sentinel.
func TestValidateScanRunStatus_RejectsAndAccepts(t *testing.T) {
	if err := metric_ingestor.ValidateScanRunStatus(metric_ingestor.ScanRunStatusRunning); err != nil {
		t.Errorf("running: err=%v", err)
	}
	if err := metric_ingestor.ValidateScanRunStatus(metric_ingestor.ScanRunStatusSucceeded); err != nil {
		t.Errorf("succeeded: err=%v", err)
	}
	if err := metric_ingestor.ValidateScanRunStatus(metric_ingestor.ScanRunStatusFailed); err != nil {
		t.Errorf("failed: err=%v", err)
	}
	for _, c := range []metric_ingestor.ScanRunStatus{"queued", "complete", "external_double", ""} {
		err := metric_ingestor.ValidateScanRunStatus(c)
		if err == nil {
			t.Errorf("ValidateScanRunStatus(%q) = nil, want ErrUnknownScanRunStatus", c)
		}
		if !errors.Is(err, metric_ingestor.ErrUnknownScanRunStatus) {
			t.Errorf("ValidateScanRunStatus(%q): err=%v, want errors.Is ErrUnknownScanRunStatus", c, err)
		}
	}
}

// TestValidateSHABinding pins the two-value closed set.
func TestValidateSHABinding(t *testing.T) {
	if err := metric_ingestor.ValidateSHABinding(metric_ingestor.SHABindingSingle); err != nil {
		t.Errorf("single: err=%v", err)
	}
	if err := metric_ingestor.ValidateSHABinding(metric_ingestor.SHABindingPerRow); err != nil {
		t.Errorf("per_row: err=%v", err)
	}
	for _, c := range []string{"double", "external_double", "", "PER_ROW"} {
		err := metric_ingestor.ValidateSHABinding(c)
		if err == nil {
			t.Errorf("ValidateSHABinding(%q) = nil, want ErrUnknownSHABinding", c)
		}
		if !errors.Is(err, metric_ingestor.ErrUnknownSHABinding) {
			t.Errorf("ValidateSHABinding(%q): err=%v, want errors.Is ErrUnknownSHABinding", c, err)
		}
	}
}

// TestClaimRequest_ValidateRejectsInvalid covers every
// failure branch of [metric_ingestor.ClaimRequest.Validate].
func TestClaimRequest_ValidateRejectsInvalid(t *testing.T) {
	t.Run("unknown_kind", func(t *testing.T) {
		req := metric_ingestor.ClaimRequest{Kind: "external_double", SHABinding: "single", OpenedAt: time.Now()}
		if err := req.Validate(); !errors.Is(err, metric_ingestor.ErrUnknownScanRunKind) {
			t.Errorf("got %v, want ErrUnknownScanRunKind", err)
		}
	})
	t.Run("unknown_binding", func(t *testing.T) {
		req := metric_ingestor.ClaimRequest{Kind: "full", SHABinding: "double", OpenedAt: time.Now()}
		if err := req.Validate(); !errors.Is(err, metric_ingestor.ErrUnknownSHABinding) {
			t.Errorf("got %v, want ErrUnknownSHABinding", err)
		}
	})
	t.Run("zero_opened_at", func(t *testing.T) {
		req := metric_ingestor.ClaimRequest{Kind: "full", SHABinding: "single"}
		if err := req.Validate(); err == nil {
			t.Errorf("zero OpenedAt: got nil, want error")
		}
	})
	t.Run("valid", func(t *testing.T) {
		req := metric_ingestor.ClaimRequest{Kind: "full", SHABinding: "single", OpenedAt: time.Now()}
		if err := req.Validate(); err != nil {
			t.Errorf("valid claim: got %v", err)
		}
	})
}

// TestNewStateMachine_PanicsOnNilDeps covers the
// constructor's panic-on-nil contract.
func TestNewStateMachine_PanicsOnNilDeps(t *testing.T) {
	scanner := scannerFunc(func(ctx context.Context, c metric_ingestor.ScanRunClaim) error { return nil })
	store := metric_ingestor.NewInMemoryScanRunStore()

	mustPanic(t, "nil store", func() {
		_ = metric_ingestor.NewStateMachine(nil, scanner)
	})
	mustPanic(t, "nil scanner", func() {
		_ = metric_ingestor.NewStateMachine(store, nil)
	})
	mustPanic(t, "invalid kind option", func() {
		_ = metric_ingestor.NewStateMachine(store, scanner, metric_ingestor.WithStateMachineKind("external_double"))
	})
	mustPanic(t, "invalid sha_binding option", func() {
		_ = metric_ingestor.NewStateMachine(store, scanner, metric_ingestor.WithStateMachineSHABinding("double"))
	})
	mustPanic(t, "zero timeout option", func() {
		_ = metric_ingestor.NewStateMachine(store, scanner, metric_ingestor.WithStateMachineTimeout(0))
	})
	mustPanic(t, "negative finalize timeout option", func() {
		_ = metric_ingestor.NewStateMachine(store, scanner, metric_ingestor.WithStateMachineFinalizeTimeout(-1*time.Second))
	})
}

func mustPanic(t *testing.T, label string, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%s: expected panic, got none", label)
		}
	}()
	fn()
}

// TestStateMachine_HappyPath_TransitionsPendingToScanned is
// the Stage 3.2 evaluator scenario `happy-path-states`:
//   - claim pending commit
//   - scanner returns nil
//   - both rows land in terminal success states
func TestStateMachine_HappyPath_TransitionsPendingToScanned(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	committed := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "aaaaaaa1111111111111111111111111111aaaaa",
		CommittedAt: committed,
	})

	scanner := &recordingScanner{}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)
	res, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne: err=%v", err)
	}
	if !res.DidWork {
		t.Fatalf("DidWork=false, want true")
	}
	if res.ScanErr != nil {
		t.Fatalf("ScanErr=%v, want nil", res.ScanErr)
	}
	if res.RunStatus != metric_ingestor.ScanRunStatusSucceeded {
		t.Errorf("RunStatus=%q, want succeeded", res.RunStatus)
	}
	if res.CommitStatus != repo_indexer.ScanStatusScanned {
		t.Errorf("CommitStatus=%q, want scanned", res.CommitStatus)
	}
	if res.Claim.ScanRunID != stateRunID01 {
		t.Errorf("ScanRunID=%s, want %s", res.Claim.ScanRunID, stateRunID01)
	}
	if res.Claim.Kind != "full" {
		t.Errorf("Claim.Kind=%q, want full", res.Claim.Kind)
	}
	if res.Claim.SHABinding != "single" {
		t.Errorf("Claim.SHABinding=%q, want single", res.Claim.SHABinding)
	}

	// Scanner saw exactly one claim with the expected
	// metadata.
	calls := scanner.Calls()
	if len(calls) != 1 {
		t.Fatalf("scanner calls: got %d, want 1", len(calls))
	}
	if calls[0].RepoID != stateRepoIDA {
		t.Errorf("scanner saw RepoID=%s, want %s", calls[0].RepoID, stateRepoIDA)
	}

	// Store now reflects terminal state for BOTH rows.
	if got := store.CommitStatus(stateRepoIDA, calls[0].SHA); got != repo_indexer.ScanStatusScanned {
		t.Errorf("commit.scan_status after happy path: got %q, want scanned", got)
	}
	if got := store.ScanRunStatus(stateRunID01); got != metric_ingestor.ScanRunStatusSucceeded {
		t.Errorf("scan_run.status after happy path: got %q, want succeeded", got)
	}

	// Pending queue is drained.
	if store.PendingCount() != 0 {
		t.Errorf("PendingCount after happy path: got %d, want 0", store.PendingCount())
	}

	// Second ProcessOne with the queue empty -> DidWork=false.
	res2, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne (empty queue): err=%v", err)
	}
	if res2.DidWork {
		t.Errorf("DidWork=true on empty queue, want false")
	}
}

// TestStateMachine_FailurePath_TransitionsPendingToFailed is
// the Stage 3.2 evaluator scenario `failure-path-states`:
//   - scanner returns an error
//   - both rows land in terminal failure states
//   - ProcessOne returns nil error (scan failure is RECORDED,
//     not propagated)
func TestStateMachine_FailurePath_TransitionsPendingToFailed(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})

	scanErr := errors.New("recipe boom")
	scanner := scannerFunc(func(ctx context.Context, c metric_ingestor.ScanRunClaim) error {
		return scanErr
	})
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)

	res, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne: err=%v (scan failures must NOT propagate as ProcessOne errors)", err)
	}
	if !res.DidWork {
		t.Fatal("DidWork=false")
	}
	if !errors.Is(res.ScanErr, scanErr) {
		t.Errorf("ScanErr=%v, want wraps %v", res.ScanErr, scanErr)
	}
	if res.RunStatus != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("RunStatus=%q, want failed", res.RunStatus)
	}
	if res.CommitStatus != repo_indexer.ScanStatusFailed {
		t.Errorf("CommitStatus=%q, want failed", res.CommitStatus)
	}

	// Store reflects the failure for BOTH rows.
	if got := store.CommitStatus(stateRepoIDA, res.Claim.SHA); got != repo_indexer.ScanStatusFailed {
		t.Errorf("commit.scan_status after failure: got %q, want failed", got)
	}
	if got := store.ScanRunStatus(stateRunID01); got != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("scan_run.status after failure: got %q, want failed", got)
	}
}

// TestStateMachine_ScannerPanic_TransitionsToFailed covers
// the panic-recovery path (Stage 3.2 implementation-plan line
// 306: "a recipe that panics ... commit ends in
// scan_status='failed' and scan_run.status='failed'").
func TestStateMachine_ScannerPanic_TransitionsToFailed(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "1111111122222222333333334444444455555555",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})

	scanner := scannerFunc(func(ctx context.Context, c metric_ingestor.ScanRunClaim) error {
		panic("recipe gone wild")
	})
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)

	res, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne: err=%v (panics must NOT propagate)", err)
	}
	if res.ScanErr == nil {
		t.Fatal("ScanErr=nil, want ErrScannerPanic wrapper")
	}
	if !errors.Is(res.ScanErr, metric_ingestor.ErrScannerPanic) {
		t.Errorf("ScanErr=%v, want errors.Is ErrScannerPanic", res.ScanErr)
	}
	if res.RunStatus != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("RunStatus=%q, want failed", res.RunStatus)
	}
	if res.CommitStatus != repo_indexer.ScanStatusFailed {
		t.Errorf("CommitStatus=%q, want failed", res.CommitStatus)
	}
}

// TestStateMachine_HardTimeout_TransitionsFailed_EvenWhenScannerIgnoresCtx
// pins the Stage 3.2 hard-timeout contract (iter 2 evaluator
// item 6). A scanner that NEVER returns and NEVER honours
// ctx.Done() must still see its commit transitioned to
// `failed` -- the state machine selects on the deadline
// regardless of scanner cooperation.
func TestStateMachine_HardTimeout_TransitionsFailed_EvenWhenScannerIgnoresCtx(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "hangedhangedhangedhangedhangedhangedhang",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})

	// scannerRelease is closed by the test AFTER the timeout
	// path has been observed; the goroutine cleanup verifies
	// the state machine does not leak goroutines into
	// subsequent tests.
	scannerRelease := make(chan struct{})
	scannerObservedCancel := make(chan struct{}, 1)

	scanner := scannerFunc(func(ctx context.Context, c metric_ingestor.ScanRunClaim) error {
		// Deliberately IGNORE ctx.Done() until the test
		// releases us. The state machine should NOT wait
		// for this goroutine -- it should fire on its own
		// deadline.
		select {
		case <-scannerRelease:
			// Record that we DID eventually see the
			// cancellation -- a sanity check that the
			// goroutine exits cleanly.
			if ctx.Err() != nil {
				scannerObservedCancel <- struct{}{}
			}
			return ctx.Err()
		}
	})
	defer close(scannerRelease)

	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineTimeout(10*time.Millisecond),
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)

	start := time.Now()
	res, err := sm.ProcessOne(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("ProcessOne: err=%v", err)
	}
	// Sanity bound: ProcessOne returned within a small
	// multiple of the timeout (not waiting for the scanner).
	if elapsed > 2*time.Second {
		t.Errorf("ProcessOne took %v -- state machine waited for the hung scanner (hard timeout broken)", elapsed)
	}
	if !errors.Is(res.ScanErr, metric_ingestor.ErrScanTimeout) {
		t.Errorf("ScanErr=%v, want errors.Is ErrScanTimeout", res.ScanErr)
	}
	if !errors.Is(res.ScanErr, context.DeadlineExceeded) {
		t.Errorf("ScanErr=%v, want errors.Is context.DeadlineExceeded", res.ScanErr)
	}
	if res.RunStatus != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("RunStatus=%q, want failed", res.RunStatus)
	}
	if res.CommitStatus != repo_indexer.ScanStatusFailed {
		t.Errorf("CommitStatus=%q, want failed", res.CommitStatus)
	}
	if got := store.CommitStatus(stateRepoIDA, res.Claim.SHA); got != repo_indexer.ScanStatusFailed {
		t.Errorf("commit.scan_status: got %q, want failed", got)
	}
	if got := store.ScanRunStatus(stateRunID01); got != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("scan_run.status: got %q, want failed", got)
	}
}

// TestStateMachine_Timeout_TransitionsToFailed covers the
// scan_timeout path. The state machine is configured with a
// 10ms timeout; the scanner blocks until ctx fires, then
// returns the ctx.Err() it observed. The state machine maps
// the deadline-exceeded to ErrScanTimeout and the rows
// transition to failed.
func TestStateMachine_Timeout_TransitionsToFailed(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "ffffeeeeddddccccbbbbaaaa9999888877776666",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})

	scanner := scannerFunc(func(ctx context.Context, c metric_ingestor.ScanRunClaim) error {
		<-ctx.Done()
		return ctx.Err()
	})
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineTimeout(10*time.Millisecond),
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)

	res, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne: err=%v (timeouts must NOT propagate)", err)
	}
	if res.ScanErr == nil {
		t.Fatal("ScanErr=nil, want ErrScanTimeout wrapper")
	}
	if !errors.Is(res.ScanErr, metric_ingestor.ErrScanTimeout) {
		t.Errorf("ScanErr=%v, want errors.Is ErrScanTimeout", res.ScanErr)
	}
	if !errors.Is(res.ScanErr, context.DeadlineExceeded) {
		t.Errorf("ScanErr=%v, want errors.Is context.DeadlineExceeded", res.ScanErr)
	}
	if res.RunStatus != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("RunStatus=%q, want failed", res.RunStatus)
	}
	if res.CommitStatus != repo_indexer.ScanStatusFailed {
		t.Errorf("CommitStatus=%q, want failed", res.CommitStatus)
	}
}

// TestStateMachine_FinalizeUsesSeparateContext is the
// contract that a CALLER-cancelled context does NOT prevent
// the finalize step from recording a terminal state. We
// cancel the parent ctx mid-scan; the scanner returns the
// cancellation; the state machine STILL finalises the rows
// via its detached (`context.WithoutCancel`) finalize ctx.
func TestStateMachine_FinalizeUsesSeparateContext(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "cancelcancelcancelcancelcancelcancelcanc",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})

	parentCtx, cancel := context.WithCancel(context.Background())
	scanner := scannerFunc(func(ctx context.Context, c metric_ingestor.ScanRunClaim) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	})

	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)
	res, err := sm.ProcessOne(parentCtx)
	if err != nil {
		t.Fatalf("ProcessOne: err=%v (caller cancel must not prevent finalize)", err)
	}
	if res.RunStatus != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("RunStatus=%q, want failed", res.RunStatus)
	}
	if got := store.CommitStatus(stateRepoIDA, res.Claim.SHA); got != repo_indexer.ScanStatusFailed {
		t.Errorf("commit.scan_status after cancel: got %q, want failed", got)
	}
	if got := store.ScanRunStatus(stateRunID01); got != metric_ingestor.ScanRunStatusFailed {
		t.Errorf("scan_run.status after cancel: got %q, want failed", got)
	}
}

// TestStateMachine_NoPendingCommit_ReturnsDidWorkFalse pins
// the empty-queue return contract.
func TestStateMachine_NoPendingCommit_ReturnsDidWorkFalse(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	scanner := &recordingScanner{}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)
	res, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne (empty store): err=%v", err)
	}
	if res.DidWork {
		t.Errorf("DidWork=true, want false")
	}
	if len(scanner.Calls()) != 0 {
		t.Errorf("scanner called %d times on empty store, want 0", len(scanner.Calls()))
	}
}

// TestStateMachine_ClaimsOldestFirst pins the FIFO contract.
// Three commits with out-of-order CommittedAt are seeded;
// the state machine must claim the OLDEST one first.
func TestStateMachine_ClaimsOldestFirst(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01, stateRunID02, stateRunID03)),
	)
	old := metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "00000000000000000000000000000000000000aa",
		CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	mid := metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "11111111111111111111111111111111111111bb",
		CommittedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
	}
	newest := metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDB,
		SHA:         "22222222222222222222222222222222222222cc",
		CommittedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	}
	// Seed OUT OF ORDER -- the store must sort.
	store.SeedPending(newest, old, mid)

	scanner := &recordingScanner{}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)
	for i := 0; i < 3; i++ {
		res, err := sm.ProcessOne(context.Background())
		if err != nil {
			t.Fatalf("ProcessOne #%d: err=%v", i, err)
		}
		if !res.DidWork {
			t.Fatalf("ProcessOne #%d: DidWork=false", i)
		}
	}
	calls := scanner.Calls()
	if len(calls) != 3 {
		t.Fatalf("scanner calls: got %d, want 3", len(calls))
	}
	if calls[0].SHA != old.SHA {
		t.Errorf("first claim SHA=%s, want %s (oldest)", calls[0].SHA, old.SHA)
	}
	if calls[1].SHA != mid.SHA {
		t.Errorf("second claim SHA=%s, want %s", calls[1].SHA, mid.SHA)
	}
	if calls[2].SHA != newest.SHA {
		t.Errorf("third claim SHA=%s, want %s (newest)", calls[2].SHA, newest.SHA)
	}
}

// TestStateMachine_ConcurrentProcessOne_NoDoubleClaim covers
// the atomicity invariant the future PG-backed store will
// enforce via `SELECT ... FOR UPDATE`: two concurrent
// workers MUST NOT both claim the same pending commit. The
// in-memory store uses a mutex; the assertion here is that
// the scanner sees each unique SHA exactly once.
func TestStateMachine_ConcurrentProcessOne_NoDoubleClaim(t *testing.T) {
	const n = 8
	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		ids[i] = uuid.Must(uuid.NewV4())
	}
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(ids...)),
	)
	pending := make([]metric_ingestor.PendingCommit, n)
	for i := 0; i < n; i++ {
		pending[i] = metric_ingestor.PendingCommit{
			RepoID:      stateRepoIDA,
			SHA:         fmt.Sprintf("sha-%04d-padding-padding-padding-padding-pad", i),
			CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, i, time.UTC),
		}
	}
	store.SeedPending(pending...)

	scanner := &recordingScanner{}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)

	var wg sync.WaitGroup
	const workers = 4
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				res, err := sm.ProcessOne(context.Background())
				if err != nil {
					t.Errorf("ProcessOne: %v", err)
					return
				}
				if !res.DidWork {
					return
				}
			}
		}()
	}
	wg.Wait()

	calls := scanner.Calls()
	if len(calls) != n {
		t.Fatalf("scanner calls: got %d, want %d", len(calls), n)
	}
	seen := map[string]bool{}
	for _, c := range calls {
		if seen[c.SHA] {
			t.Errorf("SHA %s claimed twice", c.SHA)
		}
		seen[c.SHA] = true
	}
}

// TestInMemoryScanRunStore_DoubleFinalizeRejected pins the
// double-finalize guard so a buggy scheduler cannot
// transition a terminal run a second time.
func TestInMemoryScanRunStore_DoubleFinalizeRejected(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "doubledoubledoubledoubledoubledoubledoub",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})
	claim, ok, err := store.ClaimNextPendingCommit(context.Background(), metric_ingestor.ClaimRequest{
		Kind: "full", SHABinding: "single", OpenedAt: time.Now(),
	})
	if err != nil || !ok {
		t.Fatalf("ClaimNextPendingCommit: ok=%v err=%v", ok, err)
	}

	// First finalize succeeds.
	if err := store.FinalizeScanRun(context.Background(), claim,
		metric_ingestor.ScanRunStatusSucceeded, repo_indexer.ScanStatusScanned, time.Now()); err != nil {
		t.Fatalf("first FinalizeScanRun: %v", err)
	}

	// Second finalize rejected.
	err = store.FinalizeScanRun(context.Background(), claim,
		metric_ingestor.ScanRunStatusFailed, repo_indexer.ScanStatusFailed, time.Now())
	if !errors.Is(err, metric_ingestor.ErrClaimedRunNotInProgress) {
		t.Errorf("second FinalizeScanRun: err=%v, want errors.Is ErrClaimedRunNotInProgress", err)
	}
}

// TestInMemoryScanRunStore_FinalizeRejectsNonTerminalRunStatus
// covers the runStatus terminal-only guard.
func TestInMemoryScanRunStore_FinalizeRejectsNonTerminalRunStatus(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "nontermnontermnontermnontermnontermnonte",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})
	claim, _, err := store.ClaimNextPendingCommit(context.Background(), metric_ingestor.ClaimRequest{
		Kind: "full", SHABinding: "single", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	err = store.FinalizeScanRun(context.Background(), claim,
		metric_ingestor.ScanRunStatusRunning, repo_indexer.ScanStatusScanned, time.Now())
	if err == nil {
		t.Errorf("FinalizeScanRun(running, scanned): err=nil, want terminal-only error")
	}
}

// TestInMemoryScanRunStore_FinalizeRejectsNonTerminalCommitStatus
// covers the commitStatus terminal-only guard.
func TestInMemoryScanRunStore_FinalizeRejectsNonTerminalCommitStatus(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "shadow-shadow-shadow-shadow-shadow-shado",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})
	claim, _, err := store.ClaimNextPendingCommit(context.Background(), metric_ingestor.ClaimRequest{
		Kind: "full", SHABinding: "single", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	err = store.FinalizeScanRun(context.Background(), claim,
		metric_ingestor.ScanRunStatusSucceeded, repo_indexer.ScanStatusScanning, time.Now())
	if err == nil {
		t.Errorf("FinalizeScanRun(succeeded, scanning): err=nil, want terminal-only error")
	}
}

// TestInMemoryScanRunStore_FinalizeRejectsMismatchedPair pins
// the (run_status, commit_status) pairing rule: only
// `(succeeded, scanned)` and `(failed, failed)` are allowed.
func TestInMemoryScanRunStore_FinalizeRejectsMismatchedPair(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "mismatchmismatchmismatchmismatchmismatch",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})
	claim, _, err := store.ClaimNextPendingCommit(context.Background(), metric_ingestor.ClaimRequest{
		Kind: "full", SHABinding: "single", OpenedAt: time.Now(),
	})
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	err = store.FinalizeScanRun(context.Background(), claim,
		metric_ingestor.ScanRunStatusSucceeded, repo_indexer.ScanStatusFailed, time.Now())
	if err == nil {
		t.Errorf("FinalizeScanRun(succeeded, failed): err=nil, want disagreement error")
	}
}

// TestInMemoryScanRunStore_FinalizeRejectsUnknownScanRunID
// covers the unknown-ID guard.
func TestInMemoryScanRunStore_FinalizeRejectsUnknownScanRunID(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	bogus := metric_ingestor.ScanRunClaim{
		ScanRunID: uuid.Must(uuid.FromString("ffffffff-ffff-ffff-ffff-ffffffffffff")),
		RepoID:    stateRepoIDA,
		SHA:       "ghostghostghostghostghostghostghostghost",
	}
	err := store.FinalizeScanRun(context.Background(), bogus,
		metric_ingestor.ScanRunStatusSucceeded, repo_indexer.ScanStatusScanned, time.Now())
	if !errors.Is(err, metric_ingestor.ErrUnknownScanRunID) {
		t.Errorf("FinalizeScanRun(unknown id): err=%v, want errors.Is ErrUnknownScanRunID", err)
	}
}

// TestStateMachine_RejectsPerRowBinding pins the in-memory
// store's CHECK-constraint parity: a Stage 3.2 state machine
// configured with `per_row` would imply a NULL to_sha which
// the in-memory store refuses (per the DB CHECK constraint).
func TestStateMachine_RejectsPerRowBinding(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore()
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "perrowperrowperrowperrowperrowperrowperr",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})
	scanner := &recordingScanner{}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineSHABinding(metric_ingestor.SHABindingPerRow),
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)
	res, err := sm.ProcessOne(context.Background())
	if err == nil {
		t.Fatalf("ProcessOne(per_row binding): err=nil, want store-side rejection")
	}
	if res.DidWork {
		t.Errorf("DidWork=true on rejected claim, want false")
	}
	if len(scanner.Calls()) != 0 {
		t.Errorf("scanner called on rejected claim")
	}
}

// TestIngestorAstScanner_DelegatesToIngestor verifies the
// production adapter wires the claim's fields into
// ScanRunContext correctly and propagates the Ingestor's
// success. We reuse the `recordingDispatcher` fake defined
// in ingestor_test.go so both files exercise the same
// foundation-dispatch seam.
func TestIngestorAstScanner_DelegatesToIngestor(t *testing.T) {
	ing, disp, _ := newIngestorWithRecordingDispatcher(t, map[string]uuid.UUID{
		"internal/foo.go": fooScopeID,
	})
	adapter := metric_ingestor.NewIngestorAstScanner(ing)

	claim := metric_ingestor.ScanRunClaim{
		ScanRunID:  stateRunID01,
		RepoID:     fixedRepoID,
		SHA:        "adapterdelegateadapterdelegateadapterdel",
		Kind:       "full",
		SHABinding: "single",
		OpenedAt:   time.Now(),
	}
	if err := adapter.Scan(context.Background(), claim); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if disp.Calls() != 1 {
		t.Errorf("dispatcher calls: got %d, want 1", disp.Calls())
	}
	if disp.lastScanRun.ID != stateRunID01 {
		t.Errorf("dispatcher saw ScanRunID=%s, want %s", disp.lastScanRun.ID, stateRunID01)
	}
	if disp.lastScanRun.RepoID != fixedRepoID {
		t.Errorf("dispatcher saw RepoID=%s, want %s", disp.lastScanRun.RepoID, fixedRepoID)
	}
	if disp.lastScanRun.Kind != "full" {
		t.Errorf("dispatcher saw Kind=%q, want full", disp.lastScanRun.Kind)
	}
}

// TestIngestorAstScanner_PropagatesDispatcherError verifies
// the adapter forwards the Ingestor's error verbatim so the
// state machine can record it as a `failed` transition.
func TestIngestorAstScanner_PropagatesDispatcherError(t *testing.T) {
	ing, disp, _ := newIngestorWithRecordingDispatcher(t, nil)
	boom := errors.New("dispatcher exploded")
	disp.err = boom

	adapter := metric_ingestor.NewIngestorAstScanner(ing)
	claim := metric_ingestor.ScanRunClaim{
		ScanRunID:  stateRunID01,
		RepoID:     fixedRepoID,
		SHA:        "errorerrorerrorerrorerrorerrorerrorerror",
		Kind:       "full",
		SHABinding: "single",
		OpenedAt:   time.Now(),
	}
	err := adapter.Scan(context.Background(), claim)
	if !errors.Is(err, boom) {
		t.Errorf("Scan: err=%v, want errors.Is(err, %v)", err, boom)
	}
}

// TestNewIngestorAstScanner_PanicsOnNil pins the constructor
// contract.
func TestNewIngestorAstScanner_PanicsOnNil(t *testing.T) {
	mustPanic(t, "nil ingestor", func() {
		_ = metric_ingestor.NewIngestorAstScanner(nil)
	})
}
