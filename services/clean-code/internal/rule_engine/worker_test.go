package rule_engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/policy/steward"
)

// fakeActivation is a [PolicyActivationReader] test double.
// We inject our own (rather than reusing
// [staticActivation]) so the worker tests can exercise the
// (uuid, false, err) branches without re-running the
// [staticActivation] state machine.
type fakeActivation struct {
	mu       sync.Mutex
	id       uuid.UUID
	hasID    bool
	err      error
	calls    int
	respChan chan struct{} // optional barrier
}

func (f *fakeActivation) ActivePolicyVersionID(ctx context.Context) (uuid.UUID, bool, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.respChan != nil {
		<-f.respChan
	}
	return f.id, f.hasID, f.err
}

func TestWorker_NewWorker_RefusesNilDeps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  WorkerConfig
	}{
		{"nil engine", WorkerConfig{Activation: &fakeActivation{}, Events: make(<-chan ScanEvent), Logger: quietLogger()}},
		{"nil activation", WorkerConfig{Engine: &Engine{}, Events: make(<-chan ScanEvent), Logger: quietLogger()}},
		{"nil events", WorkerConfig{Engine: &Engine{}, Activation: &fakeActivation{}, Logger: quietLogger()}},
		{"nil logger", WorkerConfig{Engine: &Engine{}, Activation: &fakeActivation{}, Events: make(<-chan ScanEvent)}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewWorker(tc.cfg)
			if err == nil {
				t.Fatal("expected error; got nil")
			}
		})
	}
}

func TestWorker_Run_ProcessesEvent_WritesBatchRefreshRow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	f.store.InsertSamples(f.repoID, "sha1", []Sample{f.sample(scopeID, 12)})

	activation := &fakeActivation{id: f.policyVersionID, hasID: true}
	events := make(chan ScanEvent, 1)
	w, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	events <- ScanEvent{RepoID: f.repoID, SHA: "sha1"}
	close(events)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	runs := f.store.Runs()
	if len(runs) != 1 {
		t.Fatalf("runs=%d; want 1", len(runs))
	}
	if runs[0].Caller != CallerBatchRefresh {
		t.Errorf("caller=%s; want batch_refresh", runs[0].Caller)
	}
	if runs[0].SHA != "sha1" {
		t.Errorf("sha=%q; want sha1", runs[0].SHA)
	}
	if activation.calls != 1 {
		t.Errorf("activation.calls=%d; want 1", activation.calls)
	}
}

func TestWorker_Run_SkipsEventWhenNoActivePolicy(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	activation := &fakeActivation{hasID: false}
	events := make(chan ScanEvent, 1)
	w, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	events <- ScanEvent{RepoID: f.repoID, SHA: "sha1"}
	close(events)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.store.Runs()) != 0 {
		t.Errorf("runs=%d; want 0 (no active policy)", len(f.store.Runs()))
	}
}

func TestWorker_Run_LogsAndContinuesOnActivationError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	activation := &fakeActivation{err: errors.New("transient db error")}
	events := make(chan ScanEvent, 2)
	w, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	events <- ScanEvent{RepoID: f.repoID, SHA: "sha1"}
	events <- ScanEvent{RepoID: f.repoID, SHA: "sha2"}
	close(events)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if activation.calls != 2 {
		t.Errorf("activation.calls=%d; want 2 (worker must NOT short-circuit on the first error)", activation.calls)
	}
	if len(f.store.Runs()) != 0 {
		t.Errorf("runs=%d; want 0 (every activation lookup failed)", len(f.store.Runs()))
	}
}

func TestWorker_Run_LogsAndContinuesOnEngineError(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	// Use a policy_version_id the engine cannot resolve --
	// engine returns ErrUnknownPolicyVersion. Worker must
	// log and proceed.
	unknown := uuid.Must(uuid.NewV4())
	activation := &fakeActivation{id: unknown, hasID: true}
	events := make(chan ScanEvent, 2)
	w, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	events <- ScanEvent{RepoID: f.repoID, SHA: "sha1"}
	events <- ScanEvent{RepoID: f.repoID, SHA: "sha2"}
	close(events)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Both lookups happened (worker is not short-circuiting).
	if activation.calls != 2 {
		t.Errorf("activation.calls=%d; want 2", activation.calls)
	}
}

func TestWorker_Run_IgnoresMalformedEvent(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	activation := &fakeActivation{id: f.policyVersionID, hasID: true}
	events := make(chan ScanEvent, 3)
	w, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	events <- ScanEvent{RepoID: uuid.Nil, SHA: "sha1"} // bad
	events <- ScanEvent{RepoID: f.repoID, SHA: ""}     // bad
	events <- ScanEvent{RepoID: f.repoID, SHA: "sha-ok"}
	close(events)

	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Only the well-formed event triggered an activation
	// lookup.
	if activation.calls != 1 {
		t.Errorf("activation.calls=%d; want 1 (only well-formed event processed)", activation.calls)
	}
	if len(f.store.Runs()) != 1 {
		t.Errorf("runs=%d; want 1", len(f.store.Runs()))
	}
	if f.store.Runs()[0].SHA != "sha-ok" {
		t.Errorf("ran for wrong sha=%q", f.store.Runs()[0].SHA)
	}
}

func TestWorker_Run_ExitsOnContextCancellation(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	activation := &fakeActivation{id: f.policyVersionID, hasID: true}
	events := make(chan ScanEvent) // never sends
	w, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.Run(ctx)
	}()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("err=%v; want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit within 2s of context cancellation")
	}
}

func TestWorker_StaticActivation_RoundTrip(t *testing.T) {
	t.Parallel()
	id := uuid.Must(uuid.NewV4())
	a := NewStaticActivation(id)
	gotID, ok, err := a.ActivePolicyVersionID(context.Background())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !ok || gotID != id {
		t.Errorf("got=(%s,%v); want=(%s,true)", gotID, ok, id)
	}

	a2 := NewStaticActivation(uuid.Nil)
	gotID2, ok2, err2 := a2.ActivePolicyVersionID(context.Background())
	if err2 != nil {
		t.Fatalf("err=%v", err2)
	}
	if ok2 || gotID2 != uuid.Nil {
		t.Errorf("got=(%s,%v); want=(uuid.Nil,false)", gotID2, ok2)
	}
}

func TestWorker_StaticActivation_RespectsCanceledContext(t *testing.T) {
	t.Parallel()
	a := NewStaticActivation(uuid.Must(uuid.NewV4()))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := a.ActivePolicyVersionID(ctx)
	if err == nil {
		t.Fatal("expected error on cancelled context; got nil")
	}
}

// TestWorker_FindingPersistedAcrossWorkerRuns is the
// brief-pinned scenario: a finding emitted by the worker
// survives in the audit store across worker restarts (the
// finding row is the unit of evidence for the gate's "block"
// decision -- losing one would let a regression slip).
func TestWorker_FindingPersistedAcrossWorkerRuns(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	f.store.InsertSamples(f.repoID, "sha1", []Sample{f.sample(scopeID, 12)})

	activation := &fakeActivation{id: f.policyVersionID, hasID: true}
	events := make(chan ScanEvent, 1)
	w, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	events <- ScanEvent{RepoID: f.repoID, SHA: "sha1"}
	close(events)
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	findingsBefore := f.store.Findings()
	if len(findingsBefore) != 1 {
		t.Fatalf("findings=%d after first run; want 1", len(findingsBefore))
	}

	// Restart the worker for a NEW SHA. The prior finding
	// must still be visible.
	f.store.RegisterCommit(f.repoID, "sha2", "sha1")
	f.store.InsertSamples(f.repoID, "sha2", []Sample{f.sample(scopeID, 12)})
	events2 := make(chan ScanEvent, 1)
	w2, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events2,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker (restart): %v", err)
	}
	events2 <- ScanEvent{RepoID: f.repoID, SHA: "sha2"}
	close(events2)
	if err := w2.Run(context.Background()); err != nil {
		t.Fatalf("Run (restart): %v", err)
	}

	findingsAfter := f.store.Findings()
	if len(findingsAfter) != 2 {
		t.Fatalf("findings=%d after restart; want 2 (one per SHA)", len(findingsAfter))
	}
	// Find the sha2 row -- its delta should be `unchanged`
	// because the prior sha1 row at the same scope was
	// already a block.
	var sha2Finding *Finding
	for _, fnd := range findingsAfter {
		fnd := fnd
		if fnd.SHA == "sha2" {
			sha2Finding = &fnd
			break
		}
	}
	if sha2Finding == nil {
		t.Fatal("no finding for sha2")
	}
	if sha2Finding.Delta != DeltaUnchanged {
		t.Errorf("delta=%s; want unchanged (prior block at same scope)", sha2Finding.Delta)
	}
}

// TestWorker_OverrideUnmuteAfterMute_FindingsResume covers
// the long-haul scenario: a scope is muted, the worker
// passes through several SHAs without emitting findings,
// then the override is set to `mute=false` and a subsequent
// SHA emits a finding again. (Architecture Sec 5.3.6 line
// 1166: an override is canonical state -- the worker must
// honour the latest row.)
func TestWorker_OverrideUnmuteAfterMute_FindingsResume(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	scopeID := uuid.Must(uuid.NewV4())
	sample := f.sample(scopeID, 12)
	f.store.InsertSamples(f.repoID, "sha1", []Sample{sample})
	f.store.InsertSamples(f.repoID, "sha2", []Sample{sample})

	muteAt := time.Date(2026, 5, 25, 10, 0, 0, 0, time.UTC)
	unmuteAt := muteAt.Add(2 * time.Hour)
	f.store.InsertOverride(steward.Override{
		OverrideID: uuid.Must(uuid.NewV4()),
		RuleID:     f.ruleID,
		ScopeFilter: steward.ScopeFilter{
			RepoID:             f.repoID.String(),
			ScopeKind:          steward.ScopeKindClass,
			ScopeSignatureGlob: sample.ScopeSignature,
		},
		Mute:      true,
		Reason:    "noisy",
		ActorID:   "op",
		CreatedAt: muteAt,
	})

	activation := &fakeActivation{id: f.policyVersionID, hasID: true}
	events := make(chan ScanEvent, 1)
	w, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker: %v", err)
	}
	events <- ScanEvent{RepoID: f.repoID, SHA: "sha1"}
	close(events)
	if err := w.Run(context.Background()); err != nil {
		t.Fatalf("Run sha1: %v", err)
	}
	if len(f.store.Findings()) != 0 {
		t.Errorf("findings after muted sha1=%d; want 0", len(f.store.Findings()))
	}

	// Unmute, then process sha2.
	f.store.InsertOverride(steward.Override{
		OverrideID: uuid.Must(uuid.NewV4()),
		RuleID:     f.ruleID,
		ScopeFilter: steward.ScopeFilter{
			RepoID:             f.repoID.String(),
			ScopeKind:          steward.ScopeKindClass,
			ScopeSignatureGlob: sample.ScopeSignature,
		},
		Mute:      false,
		Reason:    "",
		ActorID:   "op",
		CreatedAt: unmuteAt,
	})

	events2 := make(chan ScanEvent, 1)
	w2, err := NewWorker(WorkerConfig{
		Engine:     f.engine,
		Activation: activation,
		Events:     events2,
		Logger:     quietLogger(),
	})
	if err != nil {
		t.Fatalf("NewWorker (restart): %v", err)
	}
	events2 <- ScanEvent{RepoID: f.repoID, SHA: "sha2"}
	close(events2)
	if err := w2.Run(context.Background()); err != nil {
		t.Fatalf("Run sha2: %v", err)
	}
	if len(f.store.Findings()) != 1 {
		t.Errorf("findings after unmuted sha2=%d; want 1", len(f.store.Findings()))
	}
}
