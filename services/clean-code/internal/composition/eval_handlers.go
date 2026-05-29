package composition

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gofrs/uuid"

	"forge/services/clean-code/internal/evaluator"
)

// MaxEvalRequestBodyBytes caps the request body size on
// the canonical `/v1/eval/gate` and the non-canonical
// `/v1/eval/replay` admin handlers. The canonical payload
// is a handful of UUIDs + strings (well under 1 KiB in
// practice); 1 MiB is two orders of magnitude of headroom
// and keeps a malicious client from streaming an unbounded
// body and OOM'ing the gate process.
//
// The cap is enforced with [http.MaxBytesReader] so the
// excess bytes are NEVER buffered into memory: the reader
// returns a *[http.MaxBytesError] synchronously the moment
// the limit is crossed and the handler responds with 413
// Request Entity Too Large.
const MaxEvalRequestBodyBytes = 1 << 20

// evalGateRequest is the JSON body the CANONICAL
// `/v1/eval/gate` handler accepts. Mirrors the Stage 6.1
// + architecture Sec 6.3 verb signature
// `eval.gate(repo_id, sha, scope?, policy_version_id?)`.
//
// `policy_version_id` is OPTIONAL on the canonical verb
// (architecture.md:1338-1339): when present, the handler
// pins the evaluation to that policy version via
// [evaluator.Gate.Evaluate]; when absent, the handler
// resolves the currently-active policy via
// [evaluator.Gate.Gate]. The non-canonical
// `/v1/eval/replay` admin surface is retained for callers
// that want an explicit-pvid-only route (e.g. CLI
// idempotency), but the canonical verb now accepts both
// shapes per the architecture pin.
type evalGateRequest struct {
	RepoID          string  `json:"repo_id"`
	SHA             string  `json:"sha"`
	Scope           *string `json:"scope,omitempty"`
	PolicyVersionID *string `json:"policy_version_id,omitempty"`
}

// replayRequest is the JSON body the NON-CANONICAL
// `/v1/eval/replay` admin/replay handler accepts. It
// REQUIRES `policy_version_id` -- callers that want the
// canonical active-policy resolution should hit
// `/v1/eval/gate` instead.
type replayRequest struct {
	RepoID          string  `json:"repo_id"`
	SHA             string  `json:"sha"`
	PolicyVersionID string  `json:"policy_version_id"`
	Scope           *string `json:"scope,omitempty"`
}

// evalResponse is the JSON body BOTH eval handlers
// return. Includes all three audit IDs (run, verdict,
// findings) on both happy and degraded paths so the
// caller can render the audit trail uniformly.
type evalResponse struct {
	EvaluationRunID     string   `json:"evaluation_run_id"`
	EvaluationVerdictID string   `json:"evaluation_verdict_id"`
	FindingIDs          []string `json:"finding_ids"`
	Verdict             string   `json:"verdict"`
	Degraded            bool     `json:"degraded"`
	DegradedReason      string   `json:"degraded_reason,omitempty"`
}

// rejectExtraPolicyVersionField is retained as a no-op
// shim for backward compatibility with internal tests
// that referenced the prior canonical-verb behaviour.
// The canonical `/v1/eval/gate` verb NOW accepts an
// optional `policy_version_id` per architecture.md:1338-1339,
// so this function always returns nil. Kept in the
// package surface so a future tightening of the contract
// can restore the rejection at a single edit point
// rather than touching every test that asserted the prior
// shape.
func rejectExtraPolicyVersionField(raw []byte) error {
	_ = raw
	return nil
}

// writeEvalBodyReadError maps a request-body read/decode
// error to the canonical HTTP status. A
// *[http.MaxBytesError] surfaces as 413 Request Entity
// Too Large; every other read/decode error surfaces as
// 400 Bad Request.
func writeEvalBodyReadError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		http.Error(w,
			fmt.Sprintf("bad request: body exceeds %d bytes", maxErr.Limit),
			http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
}

// EvalGateHandler binds the canonical Stage 6.1 verb
// `eval.gate(repo_id, sha, scope?, policy_version_id?)`
// (architecture Sec 6.3, line 1338) to an
// [http.HandlerFunc] for `/v1/eval/gate`.
//
// When the caller supplies `policy_version_id`, the
// handler dispatches to [evaluator.Gate.Evaluate] (pinned
// PVID); when absent, the handler dispatches to
// [evaluator.Gate.Gate] (active-policy resolution). Both
// paths share the same response shape so a downstream
// consumer's audit-trail rendering remains uniform.
//
// Degraded outcomes are returned with HTTP 200 +
// `degraded=true` so the caller's UI can render them
// inline. [evaluator.ErrNoActivePolicy] (fresh-deploy
// steady state) maps to 409 Conflict so an operational
// state is NOT conflated with a 500 internal failure.
// Other (unexpected) errors surface as 500 with an
// OPAQUE JSON error envelope -- the raw evaluator/db
// error is logged server-side via [slog] but never
// echoed to the client, consistent with the gateway and
// webhook 5xx error-surface policy.
//
// A nil gate returns a 503 envelope on every call --
// consistent with the gateway's verb-not-wired shape so
// composition roots that fail to plumb the gate at boot
// surface a recognisable error to the caller.
func EvalGateHandler(gate *evaluator.Gate, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	if gate == nil {
		return func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"eval gate not wired","code":"VERB_NOT_WIRED"}`, http.StatusServiceUnavailable)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxEvalRequestBodyBytes)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeEvalBodyReadError(w, err)
			return
		}
		var req evalGateRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		repoID, err := uuid.FromString(req.RepoID)
		if err != nil {
			http.Error(w, "bad request: invalid repo_id: "+err.Error(), http.StatusBadRequest)
			return
		}
		var scope *uuid.UUID
		if req.Scope != nil && *req.Scope != "" {
			s, perr := uuid.FromString(*req.Scope)
			if perr != nil {
				http.Error(w, "bad request: invalid scope: "+perr.Error(), http.StatusBadRequest)
				return
			}
			scope = &s
		}
		// Branch on optional policy_version_id. When
		// present, pin the evaluation; when absent,
		// resolve the active policy. Architecture pin:
		// architecture.md:1338-1339.
		var (
			result   evaluator.EvaluateResult
			evalErr  error
		)
		if req.PolicyVersionID != nil && strings.TrimSpace(*req.PolicyVersionID) != "" {
			policyVersionID, perr := uuid.FromString(*req.PolicyVersionID)
			if perr != nil {
				http.Error(w, "bad request: invalid policy_version_id: "+perr.Error(), http.StatusBadRequest)
				return
			}
			if policyVersionID == uuid.Nil {
				http.Error(w, "bad request: policy_version_id is the zero uuid", http.StatusBadRequest)
				return
			}
			result, evalErr = gate.Evaluate(r.Context(), repoID, req.SHA, scope, policyVersionID)
		} else {
			result, evalErr = gate.Gate(r.Context(), repoID, req.SHA, scope)
		}
		writeEvalResponse(w, result, evalErr, logger)
	}
}

// EvalReplayHandler binds [evaluator.Gate.Evaluate] (the
// non-canonical admin / replay surface) to an
// [http.HandlerFunc] for `/v1/eval/replay`. Intentionally
// distinct from the canonical verb so the canonical audit
// trail remains uniform.
func EvalReplayHandler(gate *evaluator.Gate, logger *slog.Logger) http.HandlerFunc {
	if logger == nil {
		logger = slog.Default()
	}
	if gate == nil {
		return func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":"eval replay not wired","code":"VERB_NOT_WIRED"}`, http.StatusServiceUnavailable)
		}
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, MaxEvalRequestBodyBytes)
		var req replayRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeEvalBodyReadError(w, err)
			return
		}
		repoID, err := uuid.FromString(req.RepoID)
		if err != nil {
			http.Error(w, "bad request: invalid repo_id: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.PolicyVersionID == "" {
			http.Error(w, "bad request: policy_version_id is required on /v1/eval/replay; use /v1/eval/gate for the canonical active-policy verb", http.StatusBadRequest)
			return
		}
		policyVersionID, perr := uuid.FromString(req.PolicyVersionID)
		if perr != nil {
			http.Error(w, "bad request: invalid policy_version_id: "+perr.Error(), http.StatusBadRequest)
			return
		}
		if policyVersionID == uuid.Nil {
			http.Error(w, "bad request: policy_version_id is the zero uuid", http.StatusBadRequest)
			return
		}
		var scope *uuid.UUID
		if req.Scope != nil && *req.Scope != "" {
			s, perr := uuid.FromString(*req.Scope)
			if perr != nil {
				http.Error(w, "bad request: invalid scope: "+perr.Error(), http.StatusBadRequest)
				return
			}
			scope = &s
		}
		result, evalErr := gate.Evaluate(r.Context(), repoID, req.SHA, scope, policyVersionID)
		writeEvalResponse(w, result, evalErr, logger)
	}
}

// writeEvalResponse shapes the [evaluator.EvaluateResult]
// + error pair into the canonical [evalResponse] JSON.
// Used by both handlers so the audit-trail shape is
// uniform across `/v1/eval/gate` and `/v1/eval/replay`.
//
// Error-surface policy (consistent with the gateway and
// webhook 5xx policy): unexpected evaluator errors are
// LOGGED server-side via [slog] with the raw error
// detail but the wire response is an OPAQUE JSON envelope
// `{"error":"internal evaluator failure","code":"INTERNAL_ERROR"}`
// so DB driver / store-internal text never leaks to the
// untrusted caller. Operational sentinels
// ([evaluator.ErrNoActivePolicy], samples-pending /
// signature-invalid degraded paths) map to their
// pinned codes (409 / 200+degraded) BEFORE the opaque
// 500 fallback.
func writeEvalResponse(w http.ResponseWriter, result evaluator.EvaluateResult, evalErr error, logger *slog.Logger) {
	// Operational-state error: no activation row exists
	// yet. Map to 409 so the caller can distinguish
	// fresh-deploy from a 500.
	if errors.Is(evalErr, evaluator.ErrNoActivePolicy) {
		http.Error(w, "evaluator: no active policy: activate a policy_version via the steward before invoking eval.gate", http.StatusConflict)
		return
	}
	// Degraded paths return a non-nil sentinel error
	// alongside a populated result; both project into the
	// same JSON shape with `degraded=true`.
	degradedExpected := errors.Is(evalErr, evaluator.ErrSamplesPending) ||
		errors.Is(evalErr, evaluator.ErrPolicySignatureInvalid)
	if evalErr != nil && !degradedExpected {
		// Log the raw error server-side; respond with an
		// opaque envelope so DB / store internals do not
		// leak to the untrusted caller.
		if logger != nil {
			logger.Error("composition: eval verb failed",
				slog.String("error", evalErr.Error()))
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal evaluator failure","code":"INTERNAL_ERROR"}` + "\n"))
		return
	}
	findingIDs := make([]string, 0, len(result.FindingIDs))
	for _, id := range result.FindingIDs {
		findingIDs = append(findingIDs, id.String())
	}
	resp := evalResponse{
		EvaluationRunID:     result.EvaluationRunID.String(),
		EvaluationVerdictID: result.EvaluationVerdictID.String(),
		FindingIDs:          findingIDs,
		Verdict:             string(result.Verdict),
		Degraded:            result.Degraded,
		DegradedReason:      string(result.DegradedReason),
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.Warn("composition: encode eval response", slog.String("error", err.Error()))
	}
}
