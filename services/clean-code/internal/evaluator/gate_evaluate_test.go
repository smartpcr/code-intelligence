package evaluator

import (
	"context"
	"errors"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// --- Test doubles -----------------------------------------

type stubEngine struct {
	called          bool
	lastRepo        uuid.UUID
	lastSHA         string
	lastPolicy      uuid.UUID
	out             EngineRunResult
	err             error
	expectedScope   *uuid.UUID
	scopeAssertedAt int
	scopeRecorded   *uuid.UUID
}

func (s *stubEngine) RunSync(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID, pv uuid.UUID) (EngineRunResult, error) {
	s.called = true
	s.lastRepo = repoID
	s.lastSHA = sha
	s.lastPolicy = pv
	s.scopeRecorded = scope
	if s.err != nil {
		return EngineRunResult{}, s.err
	}
	return s.out, nil
}

type stubReadiness struct {
	ready bool
	err   error
}

func (s *stubReadiness) SamplesReady(ctx context.Context, repoID uuid.UUID, sha string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	return s.ready, nil
}

type stubPolicyReader struct {
	pv  steward.PolicyVersion
	err error
}

func (s *stubPolicyReader) GetPolicyVersion(ctx context.Context, id uuid.UUID) (steward.PolicyVersion, error) {
	if s.err != nil {
		return steward.PolicyVersion{}, s.err
	}
	out := s.pv
	if out.PolicyVersionID == uuid.Nil {
		out.PolicyVersionID = id
	}
	return out, nil
}

type stubVerifier struct {
	err           error
	receivedPVID  uuid.UUID
	verifyCallCnt int
}

func (s *stubVerifier) VerifyPolicyVersionSignature(ctx context.Context, pv steward.PolicyVersion) error {
	s.verifyCallCnt++
	s.receivedPVID = pv.PolicyVersionID
	return s.err
}

type stubDegradedStore struct {
	calls    []degradedCall
	failNext error
}

type degradedCall struct {
	run     DegradedRun
	verdict DegradedVerdict
}

func (s *stubDegradedStore) AppendDegradedRun(ctx context.Context, run DegradedRun, verdict DegradedVerdict) error {
	if s.failNext != nil {
		return s.failNext
	}
	s.calls = append(s.calls, degradedCall{run: run, verdict: verdict})
	return nil
}

func newWiredGate(t *testing.T, eng *stubEngine, ready *stubReadiness, pr *stubPolicyReader, ver *stubVerifier, deg *stubDegradedStore) *Gate {
	t.Helper()
	g := NewGateWithEngine(NewGate(nil), EvaluateConfig{
		Engine:          eng,
		Readiness:       ready,
		PolicyReader:    pr,
		SignatureVerify: ver,
		DegradedStore:   deg,
		NewID:           uuid.NewV4,
		Now:             func() int64 { return 1717000000000000000 },
	})
	return g
}

// --- Happy path ----------------------------------------------

func TestGate_Evaluate_HappyPath_DelegatesToEngine(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	wantRunID := uuid.Must(uuid.NewV4())
	wantVerdictID := uuid.Must(uuid.NewV4())
	wantFinding := uuid.Must(uuid.NewV4())

	eng := &stubEngine{out: EngineRunResult{
		EvaluationRunID:     wantRunID,
		EvaluationVerdictID: wantVerdictID,
		FindingIDs:          []uuid.UUID{wantFinding},
		Verdict:             "block",
	}}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	got, err := g.Evaluate(context.Background(), repoID, "sha1", nil, pvID)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !eng.called {
		t.Fatal("engine.RunSync was not called on the happy path")
	}
	if got.EvaluationRunID != wantRunID || got.EvaluationVerdictID != wantVerdictID {
		t.Errorf("IDs not forwarded from engine: got=%+v", got)
	}
	if got.Verdict != "block" {
		t.Errorf("verdict=%q; want block", got.Verdict)
	}
	if got.Degraded {
		t.Error("got Degraded=true on happy path; want false")
	}
	if len(deg.calls) != 0 {
		t.Errorf("degraded store written %d times on happy path; want 0", len(deg.calls))
	}
	if ver.verifyCallCnt != 1 {
		t.Errorf("signature verifier called %d times; want exactly 1 (gate must verify the persisted signature against the requested policy_version_id)", ver.verifyCallCnt)
	}
	if ver.receivedPVID != pvID {
		t.Errorf("signature verifier saw pvid=%s; want %s -- binding to the request's policy_version_id is required", ver.receivedPVID, pvID)
	}
}

// --- Degraded: signature invalid ----------------------------

func TestGate_Evaluate_Degraded_SignatureInvalid(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{err: errors.New("ed25519: invalid signature")}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	got, err := g.Evaluate(context.Background(), repoID, "sha1", nil, pvID)
	if !errors.Is(err, ErrPolicySignatureInvalid) {
		t.Errorf("err=%v; want errors.Is(.., ErrPolicySignatureInvalid)", err)
	}
	if !got.Degraded || got.DegradedReason != "policy_signature_invalid" {
		t.Errorf("result.Degraded=%t reason=%q; want true / policy_signature_invalid", got.Degraded, got.DegradedReason)
	}
	// Architecture Sec 3.7 line 568-575 + operator pin
	// gate-degraded-policy=warn: degraded paths produce
	// verdict='warn' (the gate never blocks but never
	// silently passes).
	if got.Verdict != "warn" {
		t.Errorf("result.Verdict=%q; want 'warn' (degraded path must surface warn per architecture Sec 3.7)", got.Verdict)
	}
	if eng.called {
		t.Error("engine.RunSync called on signature-invalid path; degraded short-circuit must not invoke the engine")
	}
	if len(deg.calls) != 1 {
		t.Fatalf("degraded store calls=%d; want exactly 1 (run+verdict pair)", len(deg.calls))
	}
	if deg.calls[0].verdict.DegradedReason != "policy_signature_invalid" {
		t.Errorf("persisted reason=%q; want policy_signature_invalid", deg.calls[0].verdict.DegradedReason)
	}
	if deg.calls[0].verdict.Verdict != "warn" {
		t.Errorf("persisted verdict=%q; want 'warn' -- degraded audit row must record warn per architecture", deg.calls[0].verdict.Verdict)
	}
	if deg.calls[0].verdict.EvaluationRunID != deg.calls[0].run.EvaluationRunID {
		t.Error("verdict.EvaluationRunID FK does not match run.EvaluationRunID -- canonical audit schema violated")
	}
	if deg.calls[0].run.CreatedAt == 0 {
		t.Error("created_at must be non-zero on the degraded run -- preserves canonical Audit schema")
	}
}

// --- Degraded: samples pending ------------------------------

func TestGate_Evaluate_Degraded_SamplesPending(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: false}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	got, err := g.Evaluate(context.Background(), repoID, "sha1", nil, pvID)
	if !errors.Is(err, ErrSamplesPending) {
		t.Errorf("err=%v; want errors.Is(.., ErrSamplesPending)", err)
	}
	if !got.Degraded || got.DegradedReason != "samples_pending" {
		t.Errorf("result.Degraded=%t reason=%q; want true / samples_pending", got.Degraded, got.DegradedReason)
	}
	// Architecture Sec 3.7 line 571-575: "the gate **never**
	// blocks on missing samples" -- but it also never
	// silently passes; the verdict is `warn` so the caller's
	// UI surfaces the run as needing attention.
	if got.Verdict != "warn" {
		t.Errorf("result.Verdict=%q; want 'warn' (samples-pending degraded path must surface warn)", got.Verdict)
	}
	if eng.called {
		t.Error("engine.RunSync called on samples-pending path; degraded short-circuit must not invoke the engine")
	}
	if len(deg.calls) != 1 {
		t.Fatalf("degraded store calls=%d; want exactly 1", len(deg.calls))
	}
	if deg.calls[0].verdict.Verdict != "warn" {
		t.Errorf("persisted verdict=%q; want 'warn'", deg.calls[0].verdict.Verdict)
	}
}

// --- Signature binding (rubber-duck #3) ---------------------

func TestGate_Evaluate_SignatureBinding_PassesRequestedPolicyVersion(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	requestedPVID := uuid.Must(uuid.NewV4())

	// The PolicyVersion the reader returns carries
	// requestedPVID (the policy_reader's contract).
	eng := &stubEngine{out: EngineRunResult{
		EvaluationRunID:     uuid.Must(uuid.NewV4()),
		EvaluationVerdictID: uuid.Must(uuid.NewV4()),
		Verdict:             "pass",
	}}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	if _, err := g.Evaluate(context.Background(), repoID, "sha1", nil, requestedPVID); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	// The signature verifier MUST be called with the
	// policy_version row matching the request's
	// policy_version_id.
	if ver.receivedPVID != requestedPVID {
		t.Errorf("verifier saw pvid=%s; want %s (signature binding broken)", ver.receivedPVID, requestedPVID)
	}
}

// --- Wiring errors ------------------------------------------

func TestGate_Evaluate_UnwiredEngine(t *testing.T) {
	t.Parallel()
	g := NewGate(nil) // no Evaluate wiring at all
	_, err := g.Evaluate(context.Background(), uuid.Must(uuid.NewV4()), "sha1", nil, uuid.Must(uuid.NewV4()))
	if !errors.Is(err, ErrEngineUnwired) {
		t.Errorf("err=%v; want errors.Is(.., ErrEngineUnwired)", err)
	}
}

func TestGate_Evaluate_RejectsZeroRequestArgs(t *testing.T) {
	t.Parallel()
	g := newWiredGate(t, &stubEngine{}, &stubReadiness{ready: true}, &stubPolicyReader{}, &stubVerifier{}, &stubDegradedStore{})
	cases := []struct {
		name  string
		repo  uuid.UUID
		sha   string
		pv    uuid.UUID
		match string
	}{
		{"zero repo", uuid.Nil, "sha", uuid.Must(uuid.NewV4()), "repo_id"},
		{"empty sha", uuid.Must(uuid.NewV4()), "", uuid.Must(uuid.NewV4()), "sha"},
		{"zero policy", uuid.Must(uuid.NewV4()), "sha", uuid.Nil, "policy_version_id"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := g.Evaluate(context.Background(), tc.repo, tc.sha, nil, tc.pv)
			if err == nil {
				t.Fatalf("Evaluate(%s) returned nil; want zero-arg error", tc.name)
			}
		})
	}
}

// --- Iter-8 evaluator feedback #2: scope propagation to degraded rows ---
//
// Before iter 9, a scoped eval.gate call that took the
// signature-invalid or samples-pending degraded short-
// circuit would write an evaluation_run row with
// scope_id=NULL, even though the call itself was per-
// scope. That breaks the canonical schema (migration
// 0008 + architecture.md §5.4.2): the audit row would
// lie about the scope of the evaluation that was
// attempted, and cross-replica dedup would conflate
// scoped vs unscoped degraded rows.
//
// These tests pin that the [Gate.Evaluate] `scope`
// argument propagates onto [DegradedRun.ScopeID] on
// BOTH degraded short-circuit paths.

func TestGate_Evaluate_Degraded_SignatureInvalid_PropagatesScope(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	scope := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{err: errors.New("ed25519: invalid signature")}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	_, _ = g.Evaluate(context.Background(), repoID, "sha1", &scope, pvID)

	if len(deg.calls) != 1 {
		t.Fatalf("degraded store calls=%d; want 1", len(deg.calls))
	}
	got := deg.calls[0].run
	if got.ScopeID == nil {
		t.Fatalf("DegradedRun.ScopeID == nil; want non-nil scope=%s (iter-8 evaluator feedback #2)", scope)
	}
	if *got.ScopeID != scope {
		t.Errorf("DegradedRun.ScopeID=%s; want %s", *got.ScopeID, scope)
	}
}

func TestGate_Evaluate_Degraded_SamplesPending_PropagatesScope(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	scope := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: false}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	_, _ = g.Evaluate(context.Background(), repoID, "sha1", &scope, pvID)

	if len(deg.calls) != 1 {
		t.Fatalf("degraded store calls=%d; want 1", len(deg.calls))
	}
	got := deg.calls[0].run
	if got.ScopeID == nil {
		t.Fatalf("DegradedRun.ScopeID == nil; want non-nil scope=%s (iter-8 evaluator feedback #2)", scope)
	}
	if *got.ScopeID != scope {
		t.Errorf("DegradedRun.ScopeID=%s; want %s", *got.ScopeID, scope)
	}
}

func TestGate_Evaluate_Degraded_UnscopedCall_RecordsNilScope(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{err: errors.New("ed25519: invalid signature")}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	// scope=nil (the canonical whole-SHA call) must
	// produce a DegradedRun whose ScopeID is also nil,
	// mirroring the engine's happy-path schema where
	// scope_id IS NULL is the canonical whole-SHA row.
	_, _ = g.Evaluate(context.Background(), repoID, "sha1", nil, pvID)

	if len(deg.calls) != 1 {
		t.Fatalf("degraded store calls=%d; want 1", len(deg.calls))
	}
	if deg.calls[0].run.ScopeID != nil {
		t.Errorf("DegradedRun.ScopeID=%v; want nil for unscoped eval.gate call", *deg.calls[0].run.ScopeID)
	}
}
