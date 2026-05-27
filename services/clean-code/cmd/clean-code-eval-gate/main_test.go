package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/evaluator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// These tests pin the Stage 6.1 verb behaviour at the HTTP
// gateway. They exercise the same `makeEvalHandler` the
// production binary mounts at `/v1/eval/gate`, against a
// gate wired with in-memory stubs so the test does not
// depend on PostgreSQL.

// fakeEngine is a [evaluator.RuleEngine] stub.
type fakeEngine struct {
	out evaluator.EngineRunResult
	err error
}

func (f *fakeEngine) RunSync(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID, pvID uuid.UUID) (evaluator.EngineRunResult, error) {
	if f.err != nil {
		return evaluator.EngineRunResult{}, f.err
	}
	return f.out, nil
}

// fakeReadiness is a [evaluator.SampleReadinessReader] stub.
type fakeReadiness struct{ ready bool }

func (f *fakeReadiness) SamplesReady(ctx context.Context, repoID uuid.UUID, sha string) (bool, error) {
	return f.ready, nil
}

// fakePolicyReader is a [evaluator.PolicyVersionReader] stub.
type fakePolicyReader struct{}

func (f *fakePolicyReader) GetPolicyVersion(ctx context.Context, id uuid.UUID) (steward.PolicyVersion, error) {
	return steward.PolicyVersion{PolicyVersionID: id}, nil
}

// fakeVerifier is a [evaluator.PolicySignatureVerifier] stub.
type fakeVerifier struct{ err error }

func (f *fakeVerifier) VerifyPolicyVersionSignature(ctx context.Context, pv steward.PolicyVersion) error {
	return f.err
}

// fakeDegradedStore is a [evaluator.DegradedRunStore] stub.
type fakeDegradedStore struct{}

func (f *fakeDegradedStore) AppendDegradedRun(ctx context.Context, run evaluator.DegradedRun, verdict evaluator.DegradedVerdict) error {
	return nil
}

// fakeActivation is a [evaluator.PolicyActivationReader] stub.
type fakeActivation struct {
	pvID uuid.UUID
	ok   bool
	err  error
}

func (f *fakeActivation) ActivePolicyVersionID(ctx context.Context) (uuid.UUID, bool, error) {
	if f.err != nil {
		return uuid.Nil, false, f.err
	}
	return f.pvID, f.ok, nil
}

func newTestGate(t *testing.T, eng *fakeEngine, ready *fakeReadiness, ver *fakeVerifier, act *fakeActivation) *evaluator.Gate {
	t.Helper()
	g := evaluator.NewGateWithEngine(evaluator.NewGate(nil), evaluator.EvaluateConfig{
		Engine:          eng,
		Readiness:       ready,
		PolicyReader:    &fakePolicyReader{},
		SignatureVerify: ver,
		DegradedStore:   &fakeDegradedStore{},
		Activation:      act,
		NewID:           uuid.NewV4,
	})
	return g
}

// TestEvalHandler_OmitsPolicyVersionID_InvokesGateVerb pins
// the Stage 6.1 canonical verb path: when the request body
// does not carry `policy_version_id`, the handler invokes
// [evaluator.Gate.Gate] which resolves the active
// policy_version_id internally.
func TestEvalHandler_OmitsPolicyVersionID_InvokesGateVerb(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	activePVID := uuid.Must(uuid.NewV4())

	eng := &fakeEngine{out: evaluator.EngineRunResult{
		EvaluationRunID:     uuid.Must(uuid.NewV4()),
		EvaluationVerdictID: uuid.Must(uuid.NewV4()),
		Verdict:             evaluator.VerdictPass,
	}}
	act := &fakeActivation{pvID: activePVID, ok: true}
	gate := newTestGate(t, eng, &fakeReadiness{ready: true}, &fakeVerifier{}, act)

	body, _ := json.Marshal(map[string]string{
		"repo_id": repoID.String(),
		"sha":     "abc123",
		// policy_version_id intentionally omitted.
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/gate", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	makeEvalHandler(gate)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q; want 200", rec.Code, rec.Body.String())
	}
	var resp evalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%q", err, rec.Body.String())
	}
	if resp.Verdict != string(evaluator.VerdictPass) {
		t.Errorf("verdict=%q; want pass", resp.Verdict)
	}
	if resp.Degraded {
		t.Error("degraded=true; want false on happy path")
	}
}

// TestEvalHandler_NoActivation_Returns409 pins the
// fresh-deploy operational-state mapping: when the
// activation reader reports `ok=false`, the handler
// returns 409 Conflict (not 500) so the caller can
// distinguish an operational state from an internal
// failure.
func TestEvalHandler_NoActivation_Returns409(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())

	eng := &fakeEngine{}
	act := &fakeActivation{ok: false}
	gate := newTestGate(t, eng, &fakeReadiness{ready: true}, &fakeVerifier{}, act)

	body, _ := json.Marshal(map[string]string{
		"repo_id": repoID.String(),
		"sha":     "abc123",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/gate", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	makeEvalHandler(gate)(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%q; want 409 Conflict", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no active policy") {
		t.Errorf("body=%q; want substring 'no active policy'", rec.Body.String())
	}
}

// TestEvalHandler_ExplicitPolicyVersion_Rejected400 pins
// the iter-2 evaluator feedback #1 fix: the canonical
// `/v1/eval/gate` verb MUST reject a caller-supplied
// `policy_version_id` because step (1) of the verb
// mandates resolving the active policy via the latest
// `policy_activation` row. A caller-supplied override
// would let a rogue client bypass the steward's
// activation governance and pin findings to an
// inactive (or revoked) policy.
//
// This test INVERTS the prior iter-1
// `..._StillSupported` shape; that test blessed the
// bypass and was rejected by the evaluator.
func TestEvalHandler_ExplicitPolicyVersion_Rejected400(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	explicitPVID := uuid.Must(uuid.NewV4())

	eng := &fakeEngine{out: evaluator.EngineRunResult{
		EvaluationRunID:     uuid.Must(uuid.NewV4()),
		EvaluationVerdictID: uuid.Must(uuid.NewV4()),
		Verdict:             evaluator.VerdictPass,
	}}
	act := &fakeActivation{pvID: uuid.Must(uuid.NewV4()), ok: true}
	gate := newTestGate(t, eng, &fakeReadiness{ready: true}, &fakeVerifier{}, act)

	body, _ := json.Marshal(map[string]string{
		"repo_id":           repoID.String(),
		"sha":               "abc123",
		"policy_version_id": explicitPVID.String(),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/gate", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	makeEvalHandler(gate)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%q; want 400 (policy_version_id rejected on canonical verb)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "policy_version_id is not accepted") {
		t.Errorf("body=%q; want substring 'policy_version_id is not accepted'", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "/v1/eval/replay") {
		t.Errorf("body=%q; want substring '/v1/eval/replay' pointing the caller at the admin route", rec.Body.String())
	}
}

// TestReplayHandler_AcceptsExplicitPolicyVersion pins the
// admin/replay surface: `/v1/eval/replay` REQUIRES an
// explicit `policy_version_id` and invokes
// [evaluator.Gate.Evaluate] directly. This is the surface
// batch tooling / reconciler replay / dry-runs MUST use
// for explicit-pvid invocations.
func TestReplayHandler_AcceptsExplicitPolicyVersion(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	explicitPVID := uuid.Must(uuid.NewV4())

	eng := &fakeEngine{out: evaluator.EngineRunResult{
		EvaluationRunID:     uuid.Must(uuid.NewV4()),
		EvaluationVerdictID: uuid.Must(uuid.NewV4()),
		Verdict:             evaluator.VerdictPass,
	}}
	// Activation reader intentionally unwired -- the
	// replay path must NOT consult it.
	gate := newTestGate(t, eng, &fakeReadiness{ready: true}, &fakeVerifier{}, &fakeActivation{ok: false})

	body, _ := json.Marshal(map[string]string{
		"repo_id":           repoID.String(),
		"sha":               "abc123",
		"policy_version_id": explicitPVID.String(),
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/replay", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	makeReplayHandler(gate)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q; want 200", rec.Code, rec.Body.String())
	}
	var resp evalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v body=%q", err, rec.Body.String())
	}
	if resp.Verdict != string(evaluator.VerdictPass) {
		t.Errorf("verdict=%q; want pass", resp.Verdict)
	}
}

// TestReplayHandler_MissingPolicyVersion_Returns400 pins
// the contract of `/v1/eval/replay`: it requires a
// `policy_version_id`. A caller that omits the field is
// pointed at the canonical `/v1/eval/gate` verb.
func TestReplayHandler_MissingPolicyVersion_Returns400(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	gate := newTestGate(t, &fakeEngine{}, &fakeReadiness{ready: true}, &fakeVerifier{}, &fakeActivation{ok: false})
	body, _ := json.Marshal(map[string]string{
		"repo_id": repoID.String(),
		"sha":     "abc123",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/replay", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	makeReplayHandler(gate)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d; want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "policy_version_id is required") {
		t.Errorf("body=%q; want substring 'policy_version_id is required'", rec.Body.String())
	}
}

// TestEvalHandler_DegradedSignatureInvalid_Returns200 pins
// the degraded short-circuit projection: a degraded
// outcome is rendered as HTTP 200 with `degraded=true` so
// the caller's UI can show the warn verdict inline.
func TestEvalHandler_DegradedSignatureInvalid_Returns200(t *testing.T) {
	t.Parallel()
	repoID := uuid.Must(uuid.NewV4())
	activePVID := uuid.Must(uuid.NewV4())

	eng := &fakeEngine{}
	act := &fakeActivation{pvID: activePVID, ok: true}
	ver := &fakeVerifier{err: errors.New("ed25519: invalid signature")}
	gate := newTestGate(t, eng, &fakeReadiness{ready: true}, ver, act)

	body, _ := json.Marshal(map[string]string{
		"repo_id": repoID.String(),
		"sha":     "abc123",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/gate", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	makeEvalHandler(gate)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q; want 200 (degraded path)", rec.Code, rec.Body.String())
	}
	var resp evalResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v body=%q", err, rec.Body.String())
	}
	if !resp.Degraded {
		t.Error("degraded=false; want true")
	}
	if resp.DegradedReason != string(evaluator.DegradedReasonPolicySignatureInvalid) {
		t.Errorf("degraded_reason=%q; want policy_signature_invalid", resp.DegradedReason)
	}
	if resp.Verdict != string(evaluator.VerdictWarn) {
		t.Errorf("verdict=%q; want warn", resp.Verdict)
	}
}

// TestEvalHandler_BadMethod_Returns405 pins the HTTP
// contract for non-POST methods.
func TestEvalHandler_BadMethod_Returns405(t *testing.T) {
	t.Parallel()
	gate := newTestGate(t, &fakeEngine{}, &fakeReadiness{}, &fakeVerifier{}, &fakeActivation{ok: false})
	req := httptest.NewRequest(http.MethodGet, "/v1/eval/gate", nil)
	rec := httptest.NewRecorder()
	makeEvalHandler(gate)(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status=%d; want 405", rec.Code)
	}
}

// TestEvalHandler_InvalidRepoID_Returns400 pins input
// validation: an invalid repo_id is a client error
// (400), not a server error.
func TestEvalHandler_InvalidRepoID_Returns400(t *testing.T) {
	t.Parallel()
	gate := newTestGate(t, &fakeEngine{}, &fakeReadiness{ready: true}, &fakeVerifier{}, &fakeActivation{ok: true, pvID: uuid.Must(uuid.NewV4())})
	body, _ := json.Marshal(map[string]string{
		"repo_id": "not-a-uuid",
		"sha":     "abc123",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/eval/gate", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	makeEvalHandler(gate)(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status=%d; want 400", rec.Code)
	}
}
