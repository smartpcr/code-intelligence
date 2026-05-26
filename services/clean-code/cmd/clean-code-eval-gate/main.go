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
		Engine:       &engineAdapter{e: engine},
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
	mux.HandleFunc("/v1/eval/gate", makeEvalHandler(gate))

	log.Printf("clean-code-eval-gate listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

// engineAdapter bridges the in-process [rule_engine.Engine]
// onto the evaluator package's [evaluator.RuleEngine]
// interface. The evaluator declares its own [RunResult]
// shape ([evaluator.EngineRunResult]) so it does not have
// to import `rule_engine` (which would create a cycle if
// the engine ever needed to call back into evaluator).
// This adapter is the canonical bridge between the two
// shapes.
type engineAdapter struct {
	e *rule_engine.Engine
}

func (a *engineAdapter) RunSync(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID, policyVersionID uuid.UUID) (evaluator.EngineRunResult, error) {
	out, err := a.e.RunSync(ctx, repoID, sha, scope, policyVersionID)
	if err != nil {
		return evaluator.EngineRunResult{}, err
	}
	return evaluator.EngineRunResult{
		EvaluationRunID:     out.EvaluationRunID,
		EvaluationVerdictID: out.EvaluationVerdictID,
		FindingIDs:          out.FindingIDs,
		Verdict:             string(out.Verdict),
	}, nil
}

// evalRequest is the JSON body the /v1/eval/gate handler
// accepts. `Scope` is optional -- the synchronous gate
// path supports a per-scope evaluation when set.
type evalRequest struct {
	RepoID          string  `json:"repo_id"`
	SHA             string  `json:"sha"`
	PolicyVersionID string  `json:"policy_version_id"`
	Scope           *string `json:"scope,omitempty"`
}

// evalResponse is the JSON body the /v1/eval/gate handler
// returns. Includes all three audit IDs (run, verdict,
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

// makeEvalHandler binds an HTTP handler to the supplied
// [evaluator.Gate]. The handler decodes the request body,
// delegates to [evaluator.Gate.Evaluate], and projects the
// reply onto the canonical JSON shape above. Degraded
// outcomes are returned with HTTP 200 + `degraded=true` so
// the caller's UI can render them inline; only true error
// conditions (invalid input, DB outage, ...) surface as
// non-2xx.
func makeEvalHandler(gate *evaluator.Gate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req evalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
			return
		}
		repoID, err := uuid.FromString(req.RepoID)
		if err != nil {
			http.Error(w, "bad request: invalid repo_id: "+err.Error(), http.StatusBadRequest)
			return
		}
		policyVersionID, err := uuid.FromString(req.PolicyVersionID)
		if err != nil {
			http.Error(w, "bad request: invalid policy_version_id: "+err.Error(), http.StatusBadRequest)
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
			Verdict:             result.Verdict,
			Degraded:            result.Degraded,
			DegradedReason:      result.DegradedReason,
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(resp); err != nil {
			log.Printf("clean-code-eval-gate: encode response: %v", err)
		}
	}
}
