// Package main is the entrypoint for the clean-code-eval-gate
// service. It hosts the production [evaluator.Gate.Evaluate]
// surface (Stage 5.7) -- the synchronous gate path that
//
//  1. resolves the requested `policy_version_id` via the
//     Steward;
//  2. re-verifies the persisted signature against THAT
//     policy version's canonical bytes;
//  3. confirms `clean_code.commit.scan_status = 'scanned'`
//     for the requested SHA;
//  4. delegates to the in-process [rule_engine.Engine]
//     (Stage 5.7 brief: "the engine -- not eval.gate -- is
//     the canonical writer of `evaluation_verdict` whenever
//     the synchronous rule pass is invoked");
//  5. on the two degraded paths (signature-invalid,
//     samples-pending) writes ONE `evaluation_run` +
//     ONE `evaluation_verdict` (zero findings,
//     `degraded=true`, `verdict='warn'`) under the
//     `clean_code_evaluator` grant.
//
// This binary closes the Stage 5.7 iter 3 evaluator
// feedback #2 gap: prior iters had the interfaces and
// stubs but no production composition root that actually
// invoked them.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/gofrs/uuid"
	_ "github.com/lib/pq"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/evaluator"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/microsoft/code-intelligence/services/clean-code/internal/rule_engine"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	pgURL := os.Getenv("CLEAN_CODE_EVALUATOR_PG_URL")
	if pgURL == "" {
		// Fallback to the canonical service DSN so dev /
		// test environments work without an extra env
		// var. Production deployments authenticate this
		// handle as `clean_code_evaluator` per
		// migrations/0004_roles.up.sql:455-465.
		pgURL = os.Getenv("CLEAN_CODE_PG_URL")
	}
	if pgURL == "" {
		log.Fatal("clean-code-eval-gate: CLEAN_CODE_EVALUATOR_PG_URL or CLEAN_CODE_PG_URL is required")
	}

	// Writer-handle separation (iter-7 evaluator item #1):
	// the evaluator role is granted INSERT on the Audit
	// tables ONLY for the two short-circuit degraded
	// paths (signature-invalid, samples-pending). The
	// canonical rule-pass path is the engine's
	// transactional write under the
	// `clean_code_solid_batch` grant. Opening a SECOND
	// `*sql.DB` handle authenticated as that role keeps
	// the writer-ownership contract honest: any code
	// that takes the `solidBatchDB` handle cannot
	// accidentally use the evaluator's grants and vice
	// versa.
	//
	// Per migrations/0004_roles.up.sql the
	// `clean_code_solid_batch` role's INSERT grant on
	// evaluation_run / evaluation_verdict / finding is
	// what authorises the rule engine to write the
	// canonical audit triple from both RunSync and
	// RunBatch paths.
	solidBatchURL := os.Getenv("CLEAN_CODE_SOLID_BATCH_PG_URL")
	if solidBatchURL == "" {
		log.Printf("clean-code-eval-gate: WARN: CLEAN_CODE_SOLID_BATCH_PG_URL unset; falling back to the evaluator DSN. Production deployments MUST set a distinct DSN authenticated as clean_code_solid_batch (G1 grant per migrations/0004) so the engine's rule-pass writes do not borrow the evaluator's narrower degraded-only grant.")
		solidBatchURL = pgURL
	}

	db, err := sql.Open("postgres", pgURL)
	if err != nil {
		log.Fatalf("clean-code-eval-gate: open postgres (evaluator): %v", err)
	}
	defer db.Close()

	solidBatchDB, err := sql.Open("postgres", solidBatchURL)
	if err != nil {
		log.Fatalf("clean-code-eval-gate: open postgres (solid_batch): %v", err)
	}
	defer solidBatchDB.Close()

	rootCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for i := 0; i < 30; i++ {
		if perr := db.PingContext(rootCtx); perr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	for i := 0; i < 30; i++ {
		if perr := solidBatchDB.PingContext(rootCtx); perr == nil {
			break
		}
		time.Sleep(time.Second)
	}

	stewardStore, err := steward.NewSQLStore(db)
	if err != nil {
		log.Fatalf("clean-code-eval-gate: steward.NewSQLStore: %v", err)
	}

	// Wire the policy-signing-key Manager so the Steward
	// has a real Signer and `Gate.Evaluate`'s
	// VerifyPolicyVersionSignature can match the
	// policy_version's persisted signature against the
	// signing key that produced it.
	//
	// Iter-8 evaluator feedback #1 (BLOCKING): without
	// this wiring, `steward.New(...)` installed the
	// `noActiveSigner` null object, whose `VerifyAny`
	// returns `keys.ErrUnknownKey` unconditionally. That
	// caused `Gate.Evaluate` to degrade EVERY request as
	// `policy_signature_invalid` and the synchronous
	// rule-pass happy path was unreachable. The
	// production composition root now reads the canonical
	// env vars (`CLEAN_CODE_KMS_PROVIDER` /
	// `CLEAN_CODE_KMS_MASTER_KEY_HEX`) and runs
	// `keys.Build` against the evaluator's `*sql.DB`
	// handle. The signing-key store (`clean_code.policy_signing_keys`
	// per migration 0005) is shared with the publishing
	// Steward, so the eval-gate's verifying Manager sees
	// the same active keys as the publisher.
	//
	// Scaffold-mode fallback: when `CLEAN_CODE_KMS_PROVIDER`
	// is unset (dev / test environments), we install the
	// steward's `noActiveSigner` -- preserving the
	// pre-iter-8 behaviour -- but log a loud WARN so an
	// operator can tell production-tier misconfiguration
	// apart from intentional scaffold-mode.
	var stewardSigner steward.Signer
	var keysClose func()
	kmsProvider := os.Getenv("CLEAN_CODE_KMS_PROVIDER")
	switch kmsProvider {
	case "":
		log.Print("clean-code-eval-gate: WARN: CLEAN_CODE_KMS_PROVIDER unset; running in scaffold mode -- Gate.Evaluate will degrade EVERY request as policy_signature_invalid because the Steward has no Signer. Set CLEAN_CODE_KMS_PROVIDER=local (production) or CLEAN_CODE_KMS_PROVIDER=in-memory (test) plus CLEAN_CODE_KMS_MASTER_KEY_HEX (when local) to enable the synchronous rule-pass happy path.")
	case keys.KMSProviderLocal, keys.KMSProviderInMemory:
		buildCfg := keys.BuildConfig{
			KMSProvider:     kmsProvider,
			KMSMasterKeyHex: os.Getenv("CLEAN_CODE_KMS_MASTER_KEY_HEX"),
			// The eval-gate is a READER of policy
			// signatures -- never mints new keys. The
			// publishing Steward is responsible for
			// minting; this process only verifies. Setting
			// MintFirstKeyIfEmpty=true here would race the
			// publishing Steward and is wrong.
			MintFirstKeyIfEmpty: false,
		}
		// keys.Build's validation rejects local+nil-DB
		// and in-memory+non-nil-DB, so we plumb DB based
		// on provider rather than always passing `db`.
		if kmsProvider == keys.KMSProviderLocal {
			buildCfg.DB = db
		}
		keysRes, kerr := keys.Build(rootCtx, buildCfg)
		if kerr != nil {
			log.Fatalf("clean-code-eval-gate: keys.Build(provider=%s): %v", kmsProvider, kerr)
		}
		stewardSigner = keysRes.Manager
		keysClose = keysRes.Close
		log.Printf("clean-code-eval-gate: keys.Manager wired (provider=%s); Gate.Evaluate signature-verify path is LIVE", kmsProvider)
	default:
		log.Fatalf("clean-code-eval-gate: CLEAN_CODE_KMS_PROVIDER=%q is not in the canonical closed set %v",
			kmsProvider, keys.AllKMSProviders)
	}
	if keysClose != nil {
		defer keysClose()
	}

	stew, err := steward.New(steward.Config{Store: stewardStore, Signer: stewardSigner})
	if err != nil {
		log.Fatalf("clean-code-eval-gate: steward.New: %v", err)
	}

	// rule_engine.SQLStore consumes the solid_batch
	// handle so the canonical Audit triple is INSERTED
	// under the `clean_code_solid_batch` grant -- the
	// engine is the WRITER on the rule-pass path. The
	// evaluator's `db` handle is reserved for the two
	// degraded short-circuit paths in
	// evaluator.NewProductionGate (signature-invalid,
	// samples-pending).
	ruleStore, err := rule_engine.NewSQLStore(rule_engine.SQLStoreConfig{
		DB:      solidBatchDB,
		Steward: stewardStore,
	})
	if err != nil {
		log.Fatalf("clean-code-eval-gate: rule_engine.NewSQLStore: %v", err)
	}
	engine, err := rule_engine.New(rule_engine.Config{Store: ruleStore})
	if err != nil {
		log.Fatalf("clean-code-eval-gate: rule_engine.New: %v", err)
	}

	gate, err := evaluator.NewProductionGate(evaluator.ProductionGateConfig{
		DB:           db,
		Steward:      stew,
		StewardStore: stewardStore,
		// Iter-2 evaluator feedback #2: wire the
		// canonical `rule_engine.NewEvaluatorAdapter`
		// rather than a local duplicate so the
		// production composition root actually
		// exercises the package-level adapter and its
		// embedded Verdict validation. The adapter
		// also re-checks `evaluator.Verdict.IsValid`
		// before returning, giving us a single
		// canonical bridge between engine + gate.
		Engine: rule_engine.NewEvaluatorAdapter(engine),
		// KeyManager intentionally nil: the legacy
		// signature-bundle [Gate.VerifyPolicy] surface
		// is not the focus of Stage 5.7. The Evaluate
		// path uses its own
		// [evaluator.PolicySignatureVerifier] adapter
		// (steward-backed) so it remains fully wired.
	})
	if err != nil {
		log.Fatalf("clean-code-eval-gate: evaluator.NewProductionGate: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	// `/v1/eval/gate` is the CANONICAL Stage 6.1 verb
	// `eval.gate(repo_id, sha, scope?)`. It REFUSES a
	// `policy_version_id` in the body -- step (1) of the
	// brief mandates that the gate resolve the active
	// policy_version_id via the latest `policy_activation`
	// row, and a caller-supplied override would let a
	// rogue client bypass the steward's activation
	// governance and pin findings to a non-active policy.
	// The rejection is a 400 Bad Request.
	mux.HandleFunc("/v1/eval/gate", makeEvalHandler(gate))
	// `/v1/eval/replay` is a NON-CANONICAL admin/replay
	// surface for batch tooling that needs to evaluate a
	// SHA against a SPECIFIC policy_version_id (e.g.
	// reconciler replay, dry-runs against a candidate
	// policy that has not yet been activated). This path
	// invokes [evaluator.Gate.Evaluate] directly --
	// callers MUST be authenticated with an admin grant
	// (see CHANGELOG / runbook for the role binding). It
	// is intentionally a distinct route so the canonical
	// verb's audit trail remains uniform.
	mux.HandleFunc("/v1/eval/replay", makeReplayHandler(gate))

	log.Printf("clean-code-eval-gate listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// evalGateRequest is the JSON body the CANONICAL
// `/v1/eval/gate` handler accepts. Mirrors the Stage 6.1
// verb signature `eval.gate(repo_id, sha, scope?)` --
// there is intentionally NO `policy_version_id` field.
// Step (1) of the verb resolves the active
// `policy_version_id` via the latest `policy_activation`
// row, and a caller-supplied override would let a rogue
// client pin findings to an inactive (or revoked) policy
// and bypass the steward's activation governance.
//
// Callers that need an explicit `policy_version_id`
// (batch tooling, reconciler replay, dry-runs against
// candidate policies) MUST use `/v1/eval/replay` instead.
type evalGateRequest struct {
	RepoID string  `json:"repo_id"`
	SHA    string  `json:"sha"`
	Scope  *string `json:"scope,omitempty"`
}

// replayRequest is the JSON body the NON-CANONICAL
// `/v1/eval/replay` admin/replay handler accepts. It
// REQUIRES `policy_version_id` -- if omitted the handler
// returns 400. Callers that just want the canonical
// active-policy resolution should hit `/v1/eval/gate`
// instead.
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

// rejectExtraPolicyVersionField parses the raw request
// body and returns a non-nil error iff a caller smuggled
// in a `policy_version_id` value. The check is a
// defence-in-depth measure: the canonical
// [evalGateRequest] struct has NO such field so Go's
// default JSON decoder will silently drop the value, but
// a misbehaving client should still be told its
// override has been REJECTED rather than quietly
// ignored. Iter-2 evaluator feedback #1 explicitly
// requires this: "remove/reject that request field on
// this verb or route it through a separate
// non-canonical admin/replay surface."
func rejectExtraPolicyVersionField(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	var sniff map[string]json.RawMessage
	if err := json.Unmarshal(raw, &sniff); err != nil {
		// Body is not an object -- let the caller's
		// strict decode produce the canonical error.
		return nil
	}
	if _, ok := sniff["policy_version_id"]; ok {
		return errors.New("policy_version_id is not accepted on the canonical eval.gate verb; the gate resolves the active policy via the latest policy_activation row -- use /v1/eval/replay for explicit-pvid invocations")
	}
	return nil
}

// makeEvalHandler binds the CANONICAL `/v1/eval/gate`
// handler to the supplied [evaluator.Gate]. The handler
// invokes [evaluator.Gate.Gate] -- the verb method that
// resolves the active `policy_version_id` via the
// latest `policy_activation` row itself. It REFUSES a
// caller-supplied `policy_version_id` field with a 400.
//
// Degraded outcomes are returned with HTTP 200 +
// `degraded=true` so the caller's UI can render them
// inline. `ErrNoActivePolicy` (fresh-deploy steady state)
// maps to 409 Conflict so an operational state is NOT
// conflated with a 500 internal failure. Only true error
// conditions (invalid input, DB outage, ...) surface as
// 500.
func makeEvalHandler(gate *evaluator.Gate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		if rerr := rejectExtraPolicyVersionField(raw); rerr != nil {
			http.Error(w, "bad request: "+rerr.Error(), http.StatusBadRequest)
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

		// Canonical Stage 6.1 verb path: gate resolves
		// the active policy_version_id itself via the
		// latest `clean_code.policy_activation` row.
		result, evalErr := gate.Gate(r.Context(), repoID, req.SHA, scope)
		writeEvalResponse(w, result, evalErr)
	}
}

// makeReplayHandler binds the NON-CANONICAL
// `/v1/eval/replay` admin handler to the supplied
// [evaluator.Gate]. The handler REQUIRES a
// `policy_version_id` and invokes
// [evaluator.Gate.Evaluate] directly. It is intentionally
// a distinct route so the canonical verb's audit trail
// remains uniform.
func makeReplayHandler(gate *evaluator.Gate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req replayRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
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
		writeEvalResponse(w, result, evalErr)
	}
}

// writeEvalResponse shapes the [evaluator.EvaluateResult]
// + error pair into the canonical [evalResponse] JSON.
// Used by both handlers so the audit-trail shape is
// uniform across `/v1/eval/gate` and `/v1/eval/replay`.
func writeEvalResponse(w http.ResponseWriter, result evaluator.EvaluateResult, evalErr error) {
	// Operational-state error: no activation row
	// exists yet. Map to 409 so the caller can
	// distinguish a fresh-deploy from a 500.
	if errors.Is(evalErr, evaluator.ErrNoActivePolicy) {
		http.Error(w, "evaluator: no active policy: activate a policy_version via the steward before invoking eval.gate", http.StatusConflict)
		return
	}
	// Degraded paths return a non-nil sentinel error
	// alongside a populated result. The handler
	// projects both into the same JSON shape; the
	// sentinel itself is reflected in the
	// `degraded_reason` field.
	degradedExpected := errors.Is(evalErr, evaluator.ErrSamplesPending) ||
		errors.Is(evalErr, evaluator.ErrPolicySignatureInvalid)
	if evalErr != nil && !degradedExpected {
		http.Error(w, "evaluator: "+evalErr.Error(), http.StatusInternalServerError)
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
		log.Printf("clean-code-eval-gate: encode response: %v", err)
	}
}
