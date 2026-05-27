package evaluator

import (
	"context"
	"errors"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
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

// stubActivation is the [PolicyActivationReader] test
// double. Records the last call so a test can assert
// the gate invoked it.
type stubActivation struct {
	pvID       uuid.UUID
	ok         bool
	err        error
	callCount  int
}

func (s *stubActivation) ActivePolicyVersionID(ctx context.Context) (uuid.UUID, bool, error) {
	s.callCount++
	if s.err != nil {
		return uuid.Nil, false, s.err
	}
	return s.pvID, s.ok, nil
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

// newWiredGateWithActivation wires the full Stage 6.1 gate
// (Evaluate dependencies + [PolicyActivationReader]) so
// the [Gate.Gate] verb path is exercisable.
func newWiredGateWithActivation(t *testing.T, eng *stubEngine, ready *stubReadiness, pr *stubPolicyReader, ver *stubVerifier, deg *stubDegradedStore, act *stubActivation) *Gate {
	t.Helper()
	g := NewGateWithEngine(NewGate(nil), EvaluateConfig{
		Engine:          eng,
		Readiness:       ready,
		PolicyReader:    pr,
		SignatureVerify: ver,
		DegradedStore:   deg,
		Activation:      act,
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

// --- Stage 6.1 scenarios -----------------------------------
//
// `gate-delegates-synchronous-rule-pass` is covered by
// `TestGate_Evaluate_HappyPath_DelegatesToEngine` above.
// `degraded-maps-to-warn` is covered by
// `TestGate_Evaluate_Degraded_SignatureInvalid` +
// `TestGate_Evaluate_Degraded_SamplesPending`.
// These additional tests address the remaining brief items.

// TestGate_Evaluate_BlockOnSeverityBlockRollup pins the
// "block on severity=block finding via the rollup" item
// from the Stage 6.1 brief. The rollup itself is computed
// inside the rule engine (architecture Sec 3.6 +
// rule_engine.Engine.computeVerdict); the evaluator's
// responsibility is to FORWARD the engine's verdict
// faithfully, without inventing its own rollup or
// double-writing. This test stubs the engine to return
// VerdictBlock and asserts the gate surfaces it on the
// happy path with no degraded short-circuit.
func TestGate_Evaluate_BlockOnSeverityBlockRollup(t *testing.T) {
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
		Verdict:             VerdictBlock,
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
	if got.Verdict != VerdictBlock {
		t.Errorf("got.Verdict=%q; want VerdictBlock", got.Verdict)
	}
	if got.Degraded {
		t.Error("got.Degraded=true on block path; want false (block is a happy-path verdict)")
	}
	if got.EvaluationVerdictID != wantVerdictID {
		t.Errorf("verdict_id not forwarded from engine: got=%s want=%s", got.EvaluationVerdictID, wantVerdictID)
	}
}

// TestGate_Evaluate_HappyPath_DoesNotDoubleWriteVerdict pins
// the Stage 6.1 brief item "eval.gate does NOT write its own
// `evaluation_verdict` row on the clean path -- the verdict
// that the engine wrote is the canonical one and eval.gate
// simply reads it back to shape the response." The
// invariant: on the happy path the degraded store is NEVER
// invoked (the gate's only writer) and the engine is invoked
// exactly once.
func TestGate_Evaluate_HappyPath_DoesNotDoubleWriteVerdict(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())
	engineRunID := uuid.Must(uuid.NewV4())
	engineVerdictID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{out: EngineRunResult{
		EvaluationRunID:     engineRunID,
		EvaluationVerdictID: engineVerdictID,
		FindingIDs:          nil,
		Verdict:             VerdictPass,
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

	// 1. Engine invoked exactly once.
	if !eng.called {
		t.Fatal("engine.RunSync was not called on the clean path")
	}

	// 2. Degraded store NEVER invoked on the clean path.
	if len(deg.calls) != 0 {
		t.Errorf("degraded store invoked %d times on clean path; want 0 (eval.gate must not double-write the verdict)", len(deg.calls))
	}

	// 3. The verdict_id returned to the caller is the
	//    engine's verdict_id -- proof the gate did not
	//    invent a second one.
	if got.EvaluationVerdictID != engineVerdictID {
		t.Errorf("got.EvaluationVerdictID=%s; want engine's %s (eval.gate must FORWARD, not mint)", got.EvaluationVerdictID, engineVerdictID)
	}
	if got.EvaluationRunID != engineRunID {
		t.Errorf("got.EvaluationRunID=%s; want engine's %s", got.EvaluationRunID, engineRunID)
	}
	if got.Degraded {
		t.Error("got.Degraded=true on clean path; want false")
	}
	if got.DegradedReason != "" {
		t.Errorf("got.DegradedReason=%q on clean path; want empty", got.DegradedReason)
	}
}

// TestGate_Evaluate_RejectsNonCanonicalEngineVerdict pins the
// defensive trust-boundary check (Stage 6.1 brief: "Implement
// as a Go enum `Verdict { Pass, Warn, Block }` with no other
// values"). A broken / stale rule_engine adapter that returns
// `Verdict("fail")` must be REJECTED by the gate, NOT
// surfaced to the caller as a fourth verdict value. The
// engine adapter has its own validator; this is defence in
// depth.
func TestGate_Evaluate_RejectsNonCanonicalEngineVerdict(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{out: EngineRunResult{
		EvaluationRunID:     uuid.Must(uuid.NewV4()),
		EvaluationVerdictID: uuid.Must(uuid.NewV4()),
		Verdict:             Verdict("fail"), // non-canonical
	}}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	_, err := g.Evaluate(context.Background(), repoID, "sha1", nil, pvID)
	if err == nil {
		t.Fatal("Evaluate: err=nil; want rejection of non-canonical engine verdict")
	}
}

// TestGate_Evaluate_Degraded_VerdictIsTypedWarn pins the
// typed-enum invariant for the degraded verdict the gate
// writes. The architecture's `gate-degraded-policy=warn`
// pin (Sec 1.6) says degraded paths MUST surface `warn`;
// this test confirms the typed [VerdictWarn] constant is
// the value, not a hand-spelled string literal.
func TestGate_Evaluate_Degraded_VerdictIsTypedWarn(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	pvID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{err: errors.New("ed25519: invalid signature")}
	deg := &stubDegradedStore{}
	g := newWiredGate(t, eng, ready, pr, ver, deg)

	got, _ := g.Evaluate(context.Background(), repoID, "sha1", nil, pvID)

	// Use the typed constant for comparison so a future
	// rename of the canonical spelling stays in sync with
	// this assertion. Comparing to the bare string
	// literal "warn" would not break here, but the typed
	// comparison is the documenting form.
	if got.Verdict != VerdictWarn {
		t.Errorf("got.Verdict=%q; want VerdictWarn (degraded path must surface typed warn)", got.Verdict)
	}
	if len(deg.calls) != 1 {
		t.Fatalf("degraded store calls=%d; want 1", len(deg.calls))
	}
	if deg.calls[0].verdict.Verdict != VerdictWarn {
		t.Errorf("persisted verdict=%q; want VerdictWarn", deg.calls[0].verdict.Verdict)
	}
	if deg.calls[0].verdict.DegradedReason != DegradedReasonPolicySignatureInvalid {
		t.Errorf("persisted reason=%q; want DegradedReasonPolicySignatureInvalid", deg.calls[0].verdict.DegradedReason)
	}
}

// --- Stage 6.1: `Gate.Gate` verb (resolves active policy) ---
//
// The Stage 6.1 brief defines the canonical
// `eval.gate(repo_id, sha, scope?)` verb. Step (1):
// "resolve active `policy_version_id` via latest
// `policy_activation` row". The verb signature
// deliberately does NOT take `policy_version_id` -- the
// gate resolves it itself so the caller cannot pin an
// evaluation to a non-active policy.

// TestGate_Gate_HappyPath_ResolvesActiveAndDelegates pins
// step (1) of the Stage 6.1 brief: `Gate.Gate` resolves
// the active policy_version_id via the
// [PolicyActivationReader] and delegates to
// [Gate.Evaluate] with that resolved value. The engine
// is then invoked exactly once and its verdict + IDs are
// forwarded to the caller.
func TestGate_Gate_HappyPath_ResolvesActiveAndDelegates(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	activePVID := uuid.Must(uuid.NewV4())
	wantRunID := uuid.Must(uuid.NewV4())
	wantVerdictID := uuid.Must(uuid.NewV4())
	wantFinding := uuid.Must(uuid.NewV4())

	eng := &stubEngine{out: EngineRunResult{
		EvaluationRunID:     wantRunID,
		EvaluationVerdictID: wantVerdictID,
		FindingIDs:          []uuid.UUID{wantFinding},
		Verdict:             VerdictPass,
	}}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	act := &stubActivation{pvID: activePVID, ok: true}
	g := newWiredGateWithActivation(t, eng, ready, pr, ver, deg, act)

	got, err := g.Gate(context.Background(), repoID, "sha1", nil)
	if err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if act.callCount != 1 {
		t.Errorf("activation reader called %d times; want exactly 1", act.callCount)
	}
	if !eng.called {
		t.Fatal("engine.RunSync was not called -- Gate must delegate on the happy path")
	}
	// The engine must have been called with the
	// resolver's policy_version_id, NOT some other
	// value. This is the linchpin assertion for
	// step (1).
	if eng.lastPolicy != activePVID {
		t.Errorf("engine saw policy_version_id=%s; want resolver's %s", eng.lastPolicy, activePVID)
	}
	// And the signature verifier saw the same one.
	if ver.receivedPVID != activePVID {
		t.Errorf("signature verifier saw pvid=%s; want active %s", ver.receivedPVID, activePVID)
	}
	if got.EvaluationRunID != wantRunID || got.EvaluationVerdictID != wantVerdictID {
		t.Errorf("IDs not forwarded from engine: got=%+v", got)
	}
	if got.Verdict != VerdictPass {
		t.Errorf("got.Verdict=%q; want pass", got.Verdict)
	}
	if got.Degraded {
		t.Error("got.Degraded=true on happy path; want false")
	}
	if len(deg.calls) != 0 {
		t.Errorf("degraded store written %d times on happy path; want 0", len(deg.calls))
	}
}

// TestGate_Gate_NoActivation_ReturnsErrNoActivePolicy pins
// the fresh-deploy steady state: when the activation
// reader reports `ok=false`, `Gate.Gate` returns
// [ErrNoActivePolicy] and writes NO audit row. The Stage
// 6.1 brief's canonical degraded reasons are
// `samples_pending | policy_signature_invalid |
// xrepo_edges_unavailable` only, and
// `evaluation_run.policy_version_id` is non-nullable --
// there is no policy_version_id to write on this path.
func TestGate_Gate_NoActivation_ReturnsErrNoActivePolicy(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	act := &stubActivation{ok: false}
	g := newWiredGateWithActivation(t, eng, ready, pr, ver, deg, act)

	_, err := g.Gate(context.Background(), repoID, "sha1", nil)
	if !errors.Is(err, ErrNoActivePolicy) {
		t.Fatalf("err=%v; want errors.Is(.., ErrNoActivePolicy)", err)
	}
	if eng.called {
		t.Error("engine.RunSync called; want NOT called on no-activation path")
	}
	if len(deg.calls) != 0 {
		t.Errorf("degraded store calls=%d; want 0 (no audit row on no-activation path)", len(deg.calls))
	}
	if ver.verifyCallCnt != 0 {
		t.Errorf("signature verifier called %d times; want 0 (resolver short-circuited)", ver.verifyCallCnt)
	}
}

// TestGate_Gate_ActivationLookupError_WrappedAndPropagated
// pins that a transient activation-lookup error (DB outage,
// etc.) is surfaced to the caller WITHOUT a misleading
// degraded short-circuit. The error is wrapped (so
// errors.Is on the underlying sentinel works) but NOT
// silently masked.
func TestGate_Gate_ActivationLookupError_WrappedAndPropagated(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	underlying := errors.New("postgres: connection refused")

	eng := &stubEngine{}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	act := &stubActivation{err: underlying}
	g := newWiredGateWithActivation(t, eng, ready, pr, ver, deg, act)

	_, err := g.Gate(context.Background(), repoID, "sha1", nil)
	if err == nil {
		t.Fatal("err=nil; want activation-lookup failure to propagate")
	}
	if !errors.Is(err, underlying) {
		t.Errorf("err=%v; want errors.Is(.., underlying %q)", err, underlying)
	}
	// Critically: ErrNoActivePolicy MUST NOT match -- a
	// transient lookup failure is not the fresh-deploy
	// steady state.
	if errors.Is(err, ErrNoActivePolicy) {
		t.Error("err matches ErrNoActivePolicy on transient failure; should be distinct")
	}
	if eng.called {
		t.Error("engine.RunSync called; want NOT called when activation lookup fails")
	}
	if len(deg.calls) != 0 {
		t.Errorf("degraded store calls=%d; want 0", len(deg.calls))
	}
}

// TestGate_Gate_ActivationReturnsZeroUUIDWithOK_Rejected
// pins the defence-in-depth check: a broken activation
// adapter that returns `(zero uuid, ok=true, nil)` MUST
// be rejected by the gate rather than silently smuggled
// into [Gate.Evaluate] (which would reject it but with a
// less informative error).
func TestGate_Gate_ActivationReturnsZeroUUIDWithOK_Rejected(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	act := &stubActivation{pvID: uuid.Nil, ok: true}
	g := newWiredGateWithActivation(t, eng, ready, pr, ver, deg, act)

	_, err := g.Gate(context.Background(), repoID, "sha1", nil)
	if err == nil {
		t.Fatal("err=nil; want rejection of (zero uuid, ok=true) reply")
	}
	if eng.called {
		t.Error("engine called despite zero policy_version_id")
	}
}

// TestGate_Gate_UnwiredActivation_ReturnsErrActivationUnwired
// pins the composition-root wiring contract: a gate built
// without an activation reader returns
// [ErrActivationUnwired] -- distinct from
// [ErrNoActivePolicy] so a wiring bug is not silently
// misclassified as the fresh-deploy steady state.
func TestGate_Gate_UnwiredActivation_ReturnsErrActivationUnwired(t *testing.T) {
	t.Parallel()
	// Wire everything EXCEPT the activation reader.
	g := newWiredGate(t, &stubEngine{}, &stubReadiness{ready: true}, &stubPolicyReader{}, &stubVerifier{}, &stubDegradedStore{})

	_, err := g.Gate(context.Background(), uuid.Must(uuid.NewV4()), "sha1", nil)
	if !errors.Is(err, ErrActivationUnwired) {
		t.Fatalf("err=%v; want errors.Is(.., ErrActivationUnwired)", err)
	}
	// Critically: do NOT match ErrNoActivePolicy --
	// "no activation row" and "no activation reader
	// wired" are different states.
	if errors.Is(err, ErrNoActivePolicy) {
		t.Error("err matches ErrNoActivePolicy on unwired gate; should be distinct")
	}
}

// TestGate_Gate_PropagatesScopeToEvaluate pins that a
// scoped `eval.gate(repo_id, sha, scope)` call propagates
// the scope through [Gate.Gate] -> [Gate.Evaluate] -> the
// engine (and onto degraded rows when the short-circuit
// fires). The engine stub records the scope it saw on the
// call.
func TestGate_Gate_PropagatesScopeToEvaluate(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	activePVID := uuid.Must(uuid.NewV4())
	scope := uuid.Must(uuid.NewV4())

	eng := &stubEngine{out: EngineRunResult{
		EvaluationRunID:     uuid.Must(uuid.NewV4()),
		EvaluationVerdictID: uuid.Must(uuid.NewV4()),
		Verdict:             VerdictPass,
	}}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	act := &stubActivation{pvID: activePVID, ok: true}
	g := newWiredGateWithActivation(t, eng, ready, pr, ver, deg, act)

	if _, err := g.Gate(context.Background(), repoID, "sha1", &scope); err != nil {
		t.Fatalf("Gate: %v", err)
	}
	if eng.scopeRecorded == nil {
		t.Fatal("engine saw scope=nil; want non-nil")
	}
	if *eng.scopeRecorded != scope {
		t.Errorf("engine saw scope=%s; want %s", *eng.scopeRecorded, scope)
	}
}

// TestGate_Gate_NilReceiver_ReturnsErrGateUnwired pins
// nil-receiver safety: calling `Gate.Gate` on a nil
// gate returns [ErrGateUnwired], NOT a panic.
func TestGate_Gate_NilReceiver_ReturnsErrGateUnwired(t *testing.T) {
	t.Parallel()
	var g *Gate
	_, err := g.Gate(context.Background(), uuid.Must(uuid.NewV4()), "sha1", nil)
	if !errors.Is(err, ErrGateUnwired) {
		t.Fatalf("err=%v; want errors.Is(.., ErrGateUnwired)", err)
	}
}

// TestGate_Gate_Degraded_SignatureInvalid_AppliesViaResolvedPV
// pins that the signature-invalid degraded short-circuit
// (Stage 6.1 step 2) still fires correctly when Gate is
// entered via the verb path. The degraded audit row carries
// the resolver's policy_version_id, not zero or some other
// value. The Rule Engine is NOT invoked.
func TestGate_Gate_Degraded_SignatureInvalid_AppliesViaResolvedPV(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	activePVID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: true}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{err: errors.New("ed25519: invalid signature")}
	deg := &stubDegradedStore{}
	act := &stubActivation{pvID: activePVID, ok: true}
	g := newWiredGateWithActivation(t, eng, ready, pr, ver, deg, act)

	got, err := g.Gate(context.Background(), repoID, "sha1", nil)
	if !errors.Is(err, ErrPolicySignatureInvalid) {
		t.Errorf("err=%v; want errors.Is(.., ErrPolicySignatureInvalid)", err)
	}
	if eng.called {
		t.Error("engine.RunSync called on signature-invalid path; degraded short-circuit must not invoke the engine")
	}
	if !got.Degraded || got.DegradedReason != DegradedReasonPolicySignatureInvalid {
		t.Errorf("got.Degraded=%t reason=%q; want true / policy_signature_invalid", got.Degraded, got.DegradedReason)
	}
	if got.Verdict != VerdictWarn {
		t.Errorf("got.Verdict=%q; want warn (degraded path)", got.Verdict)
	}
	if len(deg.calls) != 1 {
		t.Fatalf("degraded store calls=%d; want 1", len(deg.calls))
	}
	if deg.calls[0].run.PolicyVersionID != activePVID {
		t.Errorf("degraded run wrote policy_version_id=%s; want resolver's %s",
			deg.calls[0].run.PolicyVersionID, activePVID)
	}
}

// TestGate_Gate_Degraded_SamplesPending_AppliesViaResolvedPV
// pins that the samples-pending degraded short-circuit
// (Stage 6.1 step 3) fires correctly via the verb path.
// The degraded audit row carries the resolver's
// policy_version_id and verdict='warn'; the Rule Engine
// is NOT invoked.
func TestGate_Gate_Degraded_SamplesPending_AppliesViaResolvedPV(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	activePVID := uuid.Must(uuid.NewV4())

	eng := &stubEngine{}
	ready := &stubReadiness{ready: false}
	pr := &stubPolicyReader{}
	ver := &stubVerifier{}
	deg := &stubDegradedStore{}
	act := &stubActivation{pvID: activePVID, ok: true}
	g := newWiredGateWithActivation(t, eng, ready, pr, ver, deg, act)

	got, err := g.Gate(context.Background(), repoID, "sha1", nil)
	if !errors.Is(err, ErrSamplesPending) {
		t.Errorf("err=%v; want errors.Is(.., ErrSamplesPending)", err)
	}
	if eng.called {
		t.Error("engine.RunSync called on samples-pending path")
	}
	if !got.Degraded || got.DegradedReason != DegradedReasonSamplesPending {
		t.Errorf("got.Degraded=%t reason=%q; want true / samples_pending", got.Degraded, got.DegradedReason)
	}
	if got.Verdict != VerdictWarn {
		t.Errorf("got.Verdict=%q; want warn", got.Verdict)
	}
	if len(deg.calls) != 1 {
		t.Fatalf("degraded store calls=%d; want 1", len(deg.calls))
	}
	if deg.calls[0].run.PolicyVersionID != activePVID {
		t.Errorf("degraded run wrote policy_version_id=%s; want resolver's %s",
			deg.calls[0].run.PolicyVersionID, activePVID)
	}
}
