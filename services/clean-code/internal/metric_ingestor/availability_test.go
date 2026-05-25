package metric_ingestor_test

// availability_test.go covers iter-4 evaluator item 2 --
// the structural pre-flight that lets the
// [metric_ingestor.StateMachine] short-circuit a claim when
// the upstream AST source is not yet ready. The commit
// stays `pending` (no canonical transition occurs) and the
// next sweep tick retries -- the Metric Ingestor's
// sole-writer contract on `commit.scan_status` is preserved
// without forcing the commit to `failed`.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/metric_ingestor"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/repo_indexer"
)

// fakeProbe is a programmable [AstSourceAvailability]
// the tests use to pin the state-machine behavior at each
// probe outcome.
type fakeProbe struct {
	ready bool
	err   error
	calls int32
	last  metric_ingestor.PendingCommit
}

func (p *fakeProbe) HasFilesFor(_ context.Context, c metric_ingestor.PendingCommit) (bool, error) {
	atomic.AddInt32(&p.calls, 1)
	p.last = c
	return p.ready, p.err
}

// TestStateMachine_PreFlight_SkipsClaimWhenProbeNotReady
// pins the canonical iter-4 item 2 behavior: a peek+probe
// pair that returns "not ready" must SKIP the claim --
// the commit stays `pending` (no canonical transition).
func TestStateMachine_PreFlight_SkipsClaimWhenProbeNotReady(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	committed := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	pending := metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "aaaaaaa1111111111111111111111111111aaaaa",
		CommittedAt: committed,
	}
	store.SeedPending(pending)

	scanner := scannerFunc(func(_ context.Context, _ metric_ingestor.ScanRunClaim) error {
		t.Errorf("scanner.Scan called -- pre-flight should have skipped the claim")
		return nil
	})
	probe := &fakeProbe{ready: false}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
		metric_ingestor.WithStateMachineSourceProbe(probe),
	)

	result, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne: err=%v, want nil", err)
	}
	if result.DidWork {
		t.Errorf("DidWork=true, want false (pre-flight skipped)")
	}
	if result.SkipReason != metric_ingestor.SkipReasonSourceNotReady {
		t.Errorf("SkipReason=%q, want %q", result.SkipReason, metric_ingestor.SkipReasonSourceNotReady)
	}
	if result.Pending.RepoID != pending.RepoID || result.Pending.SHA != pending.SHA {
		t.Errorf("Pending=%+v, want %+v", result.Pending, pending)
	}
	if got := atomic.LoadInt32(&probe.calls); got != 1 {
		t.Errorf("probe.calls=%d, want 1", got)
	}

	// CRITICAL: commit.scan_status MUST stay pending. The
	// state machine MUST NOT have crossed any canonical
	// edge -- this is the whole point of the pre-flight.
	if got := store.CommitStatus(pending.RepoID, pending.SHA); got != repo_indexer.ScanStatusPending {
		t.Errorf("commit.scan_status=%q, want %q (pre-flight must NOT transition)",
			got, repo_indexer.ScanStatusPending)
	}

	// The next ProcessOne call (without changing the probe)
	// should see the SAME commit at the head of the queue
	// -- i.e. the peek was non-destructive.
	result2, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne #2: err=%v, want nil", err)
	}
	if result2.SkipReason != metric_ingestor.SkipReasonSourceNotReady {
		t.Errorf("ProcessOne #2 SkipReason=%q, want %q", result2.SkipReason, metric_ingestor.SkipReasonSourceNotReady)
	}
	if got := atomic.LoadInt32(&probe.calls); got != 2 {
		t.Errorf("probe.calls=%d after second call, want 2", got)
	}
}

// TestStateMachine_PreFlight_ProceedsWhenProbeReady pins
// the positive-case behavior: when the probe says "ready",
// the claim proceeds and the canonical
// `pending->scanning->scanned` transition completes.
func TestStateMachine_PreFlight_ProceedsWhenProbeReady(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	committed := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	pending := metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "aaaaaaa1111111111111111111111111111aaaaa",
		CommittedAt: committed,
	}
	store.SeedPending(pending)

	var scanCalled int32
	scanner := scannerFunc(func(_ context.Context, c metric_ingestor.ScanRunClaim) error {
		atomic.AddInt32(&scanCalled, 1)
		if c.SHA != pending.SHA {
			t.Errorf("scanner saw SHA=%q, want %q", c.SHA, pending.SHA)
		}
		return nil
	})
	probe := &fakeProbe{ready: true}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
		metric_ingestor.WithStateMachineSourceProbe(probe),
	)

	result, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne: err=%v, want nil", err)
	}
	if !result.DidWork {
		t.Errorf("DidWork=false, want true")
	}
	if result.SkipReason != "" {
		t.Errorf("SkipReason=%q, want empty", result.SkipReason)
	}
	if result.CommitStatus != repo_indexer.ScanStatusScanned {
		t.Errorf("CommitStatus=%q, want %q", result.CommitStatus, repo_indexer.ScanStatusScanned)
	}
	if got := atomic.LoadInt32(&scanCalled); got != 1 {
		t.Errorf("scanner.Scan calls=%d, want 1", got)
	}
}

// TestStateMachine_PreFlight_PropagatesProbeError pins
// that an infrastructure-class probe error surfaces as a
// non-nil ProcessOne error (not silently swallowed) and
// leaves the commit `pending`.
func TestStateMachine_PreFlight_PropagatesProbeError(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	committed := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	pending := metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "aaaaaaa1111111111111111111111111111aaaaa",
		CommittedAt: committed,
	}
	store.SeedPending(pending)

	probeErr := errors.New("availability: stat permission denied")
	scanner := scannerFunc(func(_ context.Context, _ metric_ingestor.ScanRunClaim) error {
		t.Errorf("scanner.Scan called -- pre-flight error should have aborted")
		return nil
	})
	probe := &fakeProbe{ready: false, err: probeErr}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
		metric_ingestor.WithStateMachineSourceProbe(probe),
	)

	_, err := sm.ProcessOne(context.Background())
	if err == nil {
		t.Fatal("ProcessOne: err=nil, want propagated probe error")
	}
	if !errors.Is(err, probeErr) {
		t.Errorf("ProcessOne: err=%v, want errors.Is(probeErr)", err)
	}
	if got := store.CommitStatus(pending.RepoID, pending.SHA); got != repo_indexer.ScanStatusPending {
		t.Errorf("commit.scan_status=%q, want %q (probe error must not transition)",
			got, repo_indexer.ScanStatusPending)
	}
}

// TestStateMachine_PreFlight_NilProbeRetainsLegacyBehavior
// pins that omitting [WithStateMachineSourceProbe] disables
// the pre-flight entirely -- the state machine behaves as
// it did before iter-4 (no peek call, claim proceeds for
// every pending row).
func TestStateMachine_PreFlight_NilProbeRetainsLegacyBehavior(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	store.SeedPending(metric_ingestor.PendingCommit{
		RepoID:      stateRepoIDA,
		SHA:         "aaaaaaa1111111111111111111111111111aaaaa",
		CommittedAt: time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC),
	})

	var scanCalled int32
	scanner := scannerFunc(func(_ context.Context, _ metric_ingestor.ScanRunClaim) error {
		atomic.AddInt32(&scanCalled, 1)
		return nil
	})
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
	)

	result, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne: err=%v, want nil", err)
	}
	if !result.DidWork {
		t.Errorf("DidWork=false, want true")
	}
	if result.SkipReason != "" {
		t.Errorf("SkipReason=%q, want empty (no probe wired)", result.SkipReason)
	}
	if got := atomic.LoadInt32(&scanCalled); got != 1 {
		t.Errorf("scanner.Scan calls=%d, want 1", got)
	}
}

// TestStateMachine_PreFlight_NoPendingReturnsNoWork pins
// the "empty queue" branch of the pre-flight path: when
// PeekNextPendingCommit returns (zero, false, nil), the
// state machine returns DidWork=false without invoking the
// probe.
func TestStateMachine_PreFlight_NoPendingReturnsNoWork(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore() // empty
	scanner := scannerFunc(func(_ context.Context, _ metric_ingestor.ScanRunClaim) error { return nil })
	probe := &fakeProbe{ready: true}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineSourceProbe(probe),
	)

	result, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne: err=%v, want nil", err)
	}
	if result.DidWork {
		t.Errorf("DidWork=true, want false (empty queue)")
	}
	if got := atomic.LoadInt32(&probe.calls); got != 0 {
		t.Errorf("probe.calls=%d, want 0 (no pending row to probe)", got)
	}
}

// TestDirectoryAstFileSource_HasFilesFor pins the
// directory source's [HasFilesFor] implementation: present
// dir = (true, nil); absent = (false, nil); not-a-dir =
// (false, err); empty Root = (false, err).
func TestDirectoryAstFileSource_HasFilesFor(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	repoID := uuid.Must(uuid.FromString("dddddddd-cccc-bbbb-aaaa-444444444444"))
	sha := "feedbeeffeedbeeffeedbeeffeedbeeffeedbeef"
	// Materialise the canonical layout for the present case.
	commitRoot := filepath.Join(tmp, repoID.String(), sha)
	if err := os.MkdirAll(commitRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	src := &metric_ingestor.DirectoryAstFileSource{Root: tmp}

	t.Run("present_dir", func(t *testing.T) {
		ok, err := src.HasFilesFor(context.Background(), metric_ingestor.PendingCommit{
			RepoID: repoID, SHA: sha,
		})
		if err != nil {
			t.Fatalf("err=%v, want nil", err)
		}
		if !ok {
			t.Error("ok=false, want true")
		}
	})

	t.Run("absent_dir", func(t *testing.T) {
		absentSHA := "00000000000000000000000000000000000000aa"
		ok, err := src.HasFilesFor(context.Background(), metric_ingestor.PendingCommit{
			RepoID: repoID, SHA: absentSHA,
		})
		if err != nil {
			t.Fatalf("err=%v, want nil (absent dir is not an error)", err)
		}
		if ok {
			t.Error("ok=true, want false")
		}
	})

	t.Run("not_a_directory", func(t *testing.T) {
		fileSHA := "11111111111111111111111111111111111111aa"
		filePath := filepath.Join(tmp, repoID.String(), fileSHA)
		if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
			t.Fatalf("MkdirAll parent: %v", err)
		}
		if err := os.WriteFile(filePath, []byte("not a dir"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		ok, err := src.HasFilesFor(context.Background(), metric_ingestor.PendingCommit{
			RepoID: repoID, SHA: fileSHA,
		})
		if err == nil {
			t.Error("err=nil, want non-nil for not-a-directory")
		}
		if ok {
			t.Error("ok=true, want false")
		}
	})

	t.Run("empty_root", func(t *testing.T) {
		empty := &metric_ingestor.DirectoryAstFileSource{}
		ok, err := empty.HasFilesFor(context.Background(), metric_ingestor.PendingCommit{
			RepoID: repoID, SHA: sha,
		})
		if err == nil {
			t.Error("err=nil, want ErrDirectoryAstSourceMissingRoot")
		}
		if !errors.Is(err, metric_ingestor.ErrDirectoryAstSourceMissingRoot) {
			t.Errorf("err=%v, want errors.Is ErrDirectoryAstSourceMissingRoot", err)
		}
		if ok {
			t.Error("ok=true, want false")
		}
	})

	t.Run("zero_repo_id", func(t *testing.T) {
		ok, err := src.HasFilesFor(context.Background(), metric_ingestor.PendingCommit{SHA: sha})
		if err == nil {
			t.Error("err=nil, want non-nil for zero repo_id")
		}
		if ok {
			t.Error("ok=true, want false")
		}
	})

	t.Run("empty_sha", func(t *testing.T) {
		ok, err := src.HasFilesFor(context.Background(), metric_ingestor.PendingCommit{RepoID: repoID})
		if err == nil {
			t.Error("err=nil, want non-nil for empty sha")
		}
		if ok {
			t.Error("ok=true, want false")
		}
	})
}

// TestAlwaysAvailable_ReturnsReady pins the no-op probe's
// `(true, nil)` contract -- used by tests that want a
// non-nil probe to assert the pre-flight path executes
// without changing the verdict.
func TestAlwaysAvailable_ReturnsReady(t *testing.T) {
	t.Parallel()
	ok, err := metric_ingestor.AlwaysAvailable{}.HasFilesFor(context.Background(), metric_ingestor.PendingCommit{
		RepoID: uuid.Must(uuid.NewV4()), SHA: "deadbeef",
	})
	if err != nil {
		t.Errorf("err=%v, want nil", err)
	}
	if !ok {
		t.Error("ok=false, want true")
	}
}

// TestInMemoryScanRunStore_PeekNextPendingCommit pins the
// non-destructive peek semantics: peek does NOT pop, does
// NOT mutate scan_status, returns (zero, false, nil) on
// empty queue.
func TestInMemoryScanRunStore_PeekNextPendingCommit(t *testing.T) {
	t.Parallel()
	t.Run("empty_queue", func(t *testing.T) {
		store := metric_ingestor.NewInMemoryScanRunStore()
		_, ok, err := store.PeekNextPendingCommit(context.Background())
		if err != nil {
			t.Errorf("err=%v, want nil", err)
		}
		if ok {
			t.Error("ok=true on empty queue, want false")
		}
	})
	t.Run("returns_oldest_without_popping", func(t *testing.T) {
		store := metric_ingestor.NewInMemoryScanRunStore()
		older := metric_ingestor.PendingCommit{
			RepoID:      stateRepoIDA,
			SHA:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		newer := metric_ingestor.PendingCommit{
			RepoID:      stateRepoIDA,
			SHA:         "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			CommittedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		}
		store.SeedPending(newer, older) // seed out-of-order on purpose

		got, ok, err := store.PeekNextPendingCommit(context.Background())
		if err != nil {
			t.Fatalf("err=%v, want nil", err)
		}
		if !ok {
			t.Fatal("ok=false, want true")
		}
		if got.SHA != older.SHA {
			t.Errorf("Peek returned SHA=%q, want oldest %q", got.SHA, older.SHA)
		}
		// Peek must NOT pop -- a second peek returns the same row.
		got2, ok2, err := store.PeekNextPendingCommit(context.Background())
		if err != nil {
			t.Fatalf("second peek err=%v", err)
		}
		if !ok2 {
			t.Error("second peek ok=false, want true (peek must not pop)")
		}
		if got2.SHA != older.SHA {
			t.Errorf("second peek SHA=%q, want %q", got2.SHA, older.SHA)
		}
		if got := store.CommitStatus(older.RepoID, older.SHA); got != repo_indexer.ScanStatusPending {
			t.Errorf("CommitStatus after peek = %q, want pending (peek must not mutate)", got)
		}
	})
}

// TestInMemoryScanRunStore_PeekNextPendingCommits pins
// the multi-row peek used by the iter-5 head-of-line
// traversal pre-flight: returns up to `limit` rows in
// (committed_at ASC, sha ASC) order, never mutates the
// queue.
func TestInMemoryScanRunStore_PeekNextPendingCommits(t *testing.T) {
	t.Parallel()
	t.Run("rejects_zero_limit", func(t *testing.T) {
		store := metric_ingestor.NewInMemoryScanRunStore()
		_, err := store.PeekNextPendingCommits(context.Background(), 0)
		if err == nil {
			t.Error("PeekNextPendingCommits(limit=0) returned nil err, want error")
		}
	})
	t.Run("returns_oldest_n_without_popping", func(t *testing.T) {
		store := metric_ingestor.NewInMemoryScanRunStore()
		a := metric_ingestor.PendingCommit{
			RepoID: stateRepoIDA, SHA: "1111111111111111111111111111111111111111",
			CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		}
		b := metric_ingestor.PendingCommit{
			RepoID: stateRepoIDA, SHA: "2222222222222222222222222222222222222222",
			CommittedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		}
		c := metric_ingestor.PendingCommit{
			RepoID: stateRepoIDA, SHA: "3333333333333333333333333333333333333333",
			CommittedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		}
		store.SeedPending(c, a, b)

		got, err := store.PeekNextPendingCommits(context.Background(), 2)
		if err != nil {
			t.Fatalf("err=%v, want nil", err)
		}
		if len(got) != 2 {
			t.Fatalf("len(got)=%d, want 2", len(got))
		}
		if got[0].SHA != a.SHA || got[1].SHA != b.SHA {
			t.Errorf("order=[%q,%q], want oldest-first [%q,%q]", got[0].SHA, got[1].SHA, a.SHA, b.SHA)
		}
		// Re-peek with larger limit returns ALL three -- proof we didn't pop.
		got2, err := store.PeekNextPendingCommits(context.Background(), 10)
		if err != nil {
			t.Fatalf("re-peek err=%v", err)
		}
		if len(got2) != 3 {
			t.Errorf("len(re-peek)=%d, want 3 (peek must not pop)", len(got2))
		}
	})
}

// TestInMemoryScanRunStore_ClaimSpecificPendingCommit_Targeted
// pins the iter-5 item 4 specific-claim path: a claim
// targeted by (repoID, sha) MUST skip over older pending
// commits and only mutate the named row.
func TestInMemoryScanRunStore_ClaimSpecificPendingCommit_Targeted(t *testing.T) {
	t.Parallel()
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	older := metric_ingestor.PendingCommit{
		RepoID: stateRepoIDA, SHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	newer := metric_ingestor.PendingCommit{
		RepoID: stateRepoIDA, SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		CommittedAt: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	store.SeedPending(older, newer)

	req := metric_ingestor.ClaimRequest{
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		OpenedAt:   time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	}
	claim, ok, err := store.ClaimSpecificPendingCommit(context.Background(), newer.RepoID, newer.SHA, req)
	if err != nil {
		t.Fatalf("ClaimSpecificPendingCommit err=%v", err)
	}
	if !ok {
		t.Fatal("ClaimSpecificPendingCommit ok=false, want true")
	}
	if claim.SHA != newer.SHA {
		t.Errorf("claim.SHA=%q, want %q (specific claim must hit the named row, not the oldest)", claim.SHA, newer.SHA)
	}
	if got := store.CommitStatus(newer.RepoID, newer.SHA); got != repo_indexer.ScanStatusScanning {
		t.Errorf("newer.scan_status=%q, want scanning", got)
	}
	if got := store.CommitStatus(older.RepoID, older.SHA); got != repo_indexer.ScanStatusPending {
		t.Errorf("older.scan_status=%q, want pending (untouched)", got)
	}
}

// TestInMemoryScanRunStore_ClaimSpecificPendingCommit_NotFound
// pins the "row raced away" branch: claiming a row that is
// no longer pending returns (zero, false, nil) and is NOT
// an error.
func TestInMemoryScanRunStore_ClaimSpecificPendingCommit_NotFound(t *testing.T) {
	t.Parallel()
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	req := metric_ingestor.ClaimRequest{
		Kind:       metric_ingestor.ScanRunKindFull,
		SHABinding: metric_ingestor.SHABindingSingle,
		OpenedAt:   time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
	}
	_, ok, err := store.ClaimSpecificPendingCommit(context.Background(), stateRepoIDA, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", req)
	if err != nil {
		t.Errorf("ClaimSpecificPendingCommit err=%v, want nil for missing row", err)
	}
	if ok {
		t.Error("ok=true for missing row, want false")
	}
}

// headOfLineProbe is a deterministic probe used by the
// head-of-line traversal test: the FIRST sha is "not
// ready", every other sha returns "ready". The test pins
// that the state machine traverses past the not-ready
// head to claim the ready newer commit.
type headOfLineProbe struct {
	notReadySHA string
	calls       int32
	seenSHAs    []string
}

func (p *headOfLineProbe) HasFilesFor(_ context.Context, c metric_ingestor.PendingCommit) (bool, error) {
	atomic.AddInt32(&p.calls, 1)
	p.seenSHAs = append(p.seenSHAs, c.SHA)
	return c.SHA != p.notReadySHA, nil
}

// TestStateMachine_PreFlight_SkipsHeadOfLineToReachReady
// pins the iter-5 evaluator item 4 fix: when the OLDEST
// pending commit's source is not yet materialised, the
// state machine MUST traverse the peeked fanout and claim
// the next READY commit -- not block the whole queue
// waiting for the oldest.
func TestStateMachine_PreFlight_SkipsHeadOfLineToReachReady(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	notReady := metric_ingestor.PendingCommit{
		RepoID: stateRepoIDA, SHA: "1111111111111111111111111111111111111111",
		CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	ready := metric_ingestor.PendingCommit{
		RepoID: stateRepoIDA, SHA: "2222222222222222222222222222222222222222",
		CommittedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	store.SeedPending(notReady, ready)

	var (
		scanCalled int32
		seenClaim  metric_ingestor.ScanRunClaim
	)
	scanner := scannerFunc(func(_ context.Context, c metric_ingestor.ScanRunClaim) error {
		atomic.AddInt32(&scanCalled, 1)
		seenClaim = c
		return nil
	})
	probe := &headOfLineProbe{notReadySHA: notReady.SHA}
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
		metric_ingestor.WithStateMachineSourceProbe(probe),
		metric_ingestor.WithStateMachineProbeFanout(4),
	)

	result, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne err=%v", err)
	}
	if !result.DidWork {
		t.Fatal("DidWork=false, want true (newer ready commit should have been claimed)")
	}
	if result.SkipReason != "" {
		t.Errorf("SkipReason=%q, want empty (newer ready commit claimed)", result.SkipReason)
	}
	if result.CommitStatus != repo_indexer.ScanStatusScanned {
		t.Errorf("CommitStatus=%q, want %q", result.CommitStatus, repo_indexer.ScanStatusScanned)
	}
	if got := atomic.LoadInt32(&scanCalled); got != 1 {
		t.Fatalf("scanner.Scan calls=%d, want 1", got)
	}
	if seenClaim.SHA != ready.SHA {
		t.Errorf("scanner saw SHA=%q, want %q (head-of-line traversal failed)", seenClaim.SHA, ready.SHA)
	}
	// The probe MUST have been called twice -- once for
	// the not-ready head, once for the ready newer one.
	if got := atomic.LoadInt32(&probe.calls); got != 2 {
		t.Errorf("probe.calls=%d, want 2 (head + first ready)", got)
	}
	if got := store.CommitStatus(notReady.RepoID, notReady.SHA); got != repo_indexer.ScanStatusPending {
		t.Errorf("not-ready commit.scan_status=%q, want pending (untouched)", got)
	}
	if got := store.CommitStatus(ready.RepoID, ready.SHA); got != repo_indexer.ScanStatusScanned {
		t.Errorf("ready commit.scan_status=%q, want scanned", got)
	}
}

// TestStateMachine_PreFlight_AllNotReadyReturnsSkip pins
// the iter-5 evaluator item 4 fallback: if EVERY peeked
// commit returns "not ready", the state machine MUST
// return SkipReason=SourceNotReady with the OLDEST
// pending row surfaced as Pending (canonical "skipped"
// row for callers).
func TestStateMachine_PreFlight_AllNotReadyReturnsSkip(t *testing.T) {
	store := metric_ingestor.NewInMemoryScanRunStore(
		metric_ingestor.WithInMemoryStoreIDFactory(uuidSeq(stateRunID01)),
	)
	older := metric_ingestor.PendingCommit{
		RepoID: stateRepoIDA, SHA: "1111111111111111111111111111111111111111",
		CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	newer := metric_ingestor.PendingCommit{
		RepoID: stateRepoIDA, SHA: "2222222222222222222222222222222222222222",
		CommittedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}
	store.SeedPending(older, newer)

	scanner := scannerFunc(func(_ context.Context, _ metric_ingestor.ScanRunClaim) error {
		t.Errorf("scanner.Scan called -- every peeked commit was not-ready")
		return nil
	})
	probe := &fakeProbe{ready: false} // every probe says "not ready"
	sm := metric_ingestor.NewStateMachine(store, scanner,
		metric_ingestor.WithStateMachineClock(fixedClockAt(time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC))),
		metric_ingestor.WithStateMachineSourceProbe(probe),
		metric_ingestor.WithStateMachineProbeFanout(8),
	)

	result, err := sm.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("ProcessOne err=%v", err)
	}
	if result.DidWork {
		t.Errorf("DidWork=true, want false")
	}
	if result.SkipReason != metric_ingestor.SkipReasonSourceNotReady {
		t.Errorf("SkipReason=%q, want %q", result.SkipReason, metric_ingestor.SkipReasonSourceNotReady)
	}
	if result.Pending.SHA != older.SHA {
		t.Errorf("Pending.SHA=%q, want oldest %q", result.Pending.SHA, older.SHA)
	}
	if got := atomic.LoadInt32(&probe.calls); got != 2 {
		t.Errorf("probe.calls=%d, want 2 (both peeked candidates probed)", got)
	}
	// Both commits MUST stay pending -- no canonical
	// transition crossed.
	for _, p := range []metric_ingestor.PendingCommit{older, newer} {
		if got := store.CommitStatus(p.RepoID, p.SHA); got != repo_indexer.ScanStatusPending {
			t.Errorf("commit %s status=%q, want pending", p.SHA, got)
		}
	}
}
