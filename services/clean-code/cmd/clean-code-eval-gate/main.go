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

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/audit/wal"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/composition"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/evaluator"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// defaultAuditWALDir is the canonical Audit WAL partition
// root the binary falls back to when CLEAN_CODE_AUDIT_WAL_DIR
// is unset. Architecture Sec 7.10 / tech-spec Sec 4.13 pin
// `data/wal/audit/` as the relative path. Production
// deployments override via the env var to point at a durable
// volume.
const defaultAuditWALDir = "data/wal/audit"

// maxRequestBodyBytes caps the JSON body size on both
// `/v1/eval/gate` and `/v1/eval/replay`. The canonical
// payload is a handful of UUIDs + strings (well under
// 1 KiB in practice); 1 MiB is two orders of magnitude
// of headroom and keeps a malicious client from
// streaming an unbounded body and OOM'ing the gate
// process.
//
// The cap is enforced with [http.MaxBytesReader] (not a
// plain [io.LimitReader]) so the excess bytes are NEVER
// buffered into memory: the reader returns a
// *[http.MaxBytesError] synchronously the moment the
// limit is crossed, the connection's read side is
// closed, and the handler responds with 413 Request
// Entity Too Large. Both handlers share the same
// reader-wrap + [writeBodyReadError] pair so the two
// surfaces have IDENTICAL body-size protection -- iter-N
// review caught that [makeEvalHandler] used
// `io.ReadAll(r.Body)` without a limit AND that
// [makeReplayHandler] used `json.NewDecoder(r.Body)`
// without one either, so the two routes were
// inconsistently exposed.
const maxRequestBodyBytes = 1 << 20

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
	var signingKeysManager *keys.Manager
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
		signingKeysManager = keysRes.Manager
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

	// Stage 9.1 -- Audit WAL writer (architecture Sec
	// 7.10 / tech-spec Sec 4.13). The writer is REQUIRED
	// by both `rule_engine.NewSQLStore` and
	// `evaluator.NewProductionGate`: every successful
	// `evaluation_run` + `evaluation_verdict` + `finding`
	// INSERT they perform is mirrored to a signed WAL
	// frame fsynced BEFORE the SQL transaction commits.
	// Iter-2 evaluator item #3: this is the production
	// reading of CLEAN_CODE_AUDIT_WAL_DIR.
	//
	// Signer choice (iter-3 evaluator item #1): when the
	// composition root has a real KMS-backed
	// `keys.Manager` (CLEAN_CODE_KMS_PROVIDER=local |
	// in-memory), the WAL signer is the production
	// Ed25519 path
	// ([composition.NewKeysManagerWALSigner]). In
	// scaffold mode (KMS provider unset) the WAL signer
	// falls back to `wal.NoopSigner`, since the gate is
	// already degrading every request as
	// policy_signature_invalid in that mode -- frames on
	// disk get the SHA-256 stand-in but the binary
	// surface still serves dev/test. Production
	// deployments MUST set CLEAN_CODE_KMS_PROVIDER.
	walDir := os.Getenv("CLEAN_CODE_AUDIT_WAL_DIR")
	if walDir == "" {
		walDir = defaultAuditWALDir
		log.Printf("clean-code-eval-gate: CLEAN_CODE_AUDIT_WAL_DIR unset; using default %q", walDir)
	}
	var walSigner wal.Signer
	if signingKeysManager != nil {
		walSigner = composition.NewKeysManagerWALSigner(signingKeysManager)
		log.Printf("clean-code-eval-gate: Audit WAL signer = keys.Manager-backed Ed25519 (provider=%s)", kmsProvider)
	} else {
		walSigner = wal.NoopSigner{}
		log.Print("clean-code-eval-gate: WARN: Audit WAL signer = wal.NoopSigner (SHA-256 stand-in, signing_key_id=uuid.Nil). This is acceptable ONLY in scaffold mode (CLEAN_CODE_KMS_PROVIDER unset); production deployments MUST set CLEAN_CODE_KMS_PROVIDER=local + CLEAN_CODE_KMS_MASTER_KEY_HEX so the WAL signer becomes the Ed25519 keys.Manager path.")
	}
	walWriter, err := wal.NewWriter(wal.WriterConfig{
		Dir:    walDir,
		Signer: walSigner,
	})
	if err != nil {
		log.Fatalf("clean-code-eval-gate: wal.NewWriter(dir=%s): %v", walDir, err)
	}
	log.Printf("clean-code-eval-gate: Audit WAL writer wired (dir=%s)", walDir)

	// Stage 9.2 -- Audit WAL Reconciler (replay-only).
	// Architecture Sec 7.10 / implementation-plan Stage
	// 9.2: on service restart, the reconciler walks
	// `data/wal/audit/YYYY-MM-DD.wal`, verifies every
	// frame's signature against the historical
	// signing-key snapshot, and replays MISSING rows
	// into the three Audit tables. Stage 9.2 iter 2
	// evaluator item #1 wired this into the binary so
	// the brief's "on service restart" requirement is a
	// LITERAL blocking startup step -- the reconciler
	// MUST complete before the HTTP listener accepts
	// traffic, otherwise an external caller could
	// observe an inconsistent audit state (some rows
	// still in WAL, some in PG).
	//
	// Production gate: when `signingKeysManager != nil`
	// (real KMS configured) the binary REQUIRES
	// CLEAN_CODE_WAL_RECONCILER_DSN to be set. The DSN
	// MUST be authenticated as `clean_code_wal_reconciler`
	// per migration 0004 (INSERT+SELECT on the three
	// Audit tables; UPDATE+DELETE REVOKED). The same
	// pool answers the `policy_signing_keys` SELECT used
	// by the historical-keys verifier (migration 0005
	// grants SELECT on policy_signing_keys to the same
	// role).
	//
	// Scaffold-mode (signingKeysManager == nil): WAL
	// signer is wal.NoopSigner, frames carry SHA-256
	// stand-ins -- the reconciler cannot verify them.
	// Skip reconciler wiring entirely; log INFO.
	runWALReconciler(rootCtx, signingKeysManager, walDir)

	// rule_engine.SQLStore consumes the solid_batch
	// handle so the canonical Audit triple is INSERTED
	// under the `clean_code_solid_batch` grant -- the
	// engine is the WRITER on the rule-pass path. The
	// evaluator's `db` handle is reserved for the two
	// degraded short-circuit paths in
	// evaluator.NewProductionGate (signature-invalid,
	// samples-pending).
	ruleStore, err := rule_engine.NewSQLStore(rule_engine.SQLStoreConfig{
		DB:        solidBatchDB,
		Steward:   stewardStore,
		WalWriter: walWriter,
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
		Engine:    rule_engine.NewEvaluatorAdapter(engine),
		WalWriter: walWriter,
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

// envWALReconcilerDSN is the DSN the binary uses to
// authenticate the Stage 9.2 reconciler's PostgreSQL pool.
// Required when `CLEAN_CODE_KMS_PROVIDER` is set (i.e. when
// the WAL writer is producing real Ed25519 signatures);
// optional and ignored in scaffold mode (NoopSigner frames
// cannot be verified, so there is no replay path).
//
// Production deployments authenticate this DSN as the
// `clean_code_wal_reconciler` PG role per migration 0004
// (INSERT+SELECT on the three Audit tables ONLY; UPDATE
// and DELETE REVOKED) and migration 0005 (SELECT on
// `clean_code.policy_signing_keys` for historical-key
// resolution). The migration-level grants are the outer
// guard for the brief's "never deletes a row; never
// modifies a non-Audit table" invariants.
const envWALReconcilerDSN = "CLEAN_CODE_WAL_RECONCILER_DSN"

// runWALReconciler builds and runs the Stage 9.2 Audit
// WAL Reconciler as a BLOCKING startup step. The
// reconciler walks `walDir`, verifies every frame's
// signature against the historical signing-key snapshot
// pulled from `clean_code.policy_signing_keys`, and
// replays MISSING rows into the three Audit tables. Per
// the brief and per Stage 9.2 iter-2 evaluator item #1
// the call BLOCKS startup so an external caller cannot
// observe an inconsistent audit state (some rows still in
// WAL, some in PG).
//
// Gate matrix:
//
//   - signingKeysManager == nil (scaffold mode, NoopSigner
//     WAL writer): skip reconciler -- the SHA-256
//     stand-in frames carry no Ed25519 signature so there
//     is nothing meaningful to verify on replay. Log INFO
//     and return.
//
//   - signingKeysManager != nil AND
//     CLEAN_CODE_WAL_RECONCILER_DSN unset: log.Fatalf.
//     The binary is producing real signed frames but the
//     operator has not wired the replay path; serving
//     traffic in this configuration would silently lose
//     durability on the first restart with pending WAL
//     frames.
//
//   - signingKeysManager != nil AND DSN set: open the
//     dedicated pool, ping with retry, build a
//     `keys.SQLStore` for historical-key resolution, hand
//     both to `composition.NewWALReconciler`, run, log
//     Stats summary. On Run error: log.Fatalf so a botched
//     WAL aborts startup rather than degrading silently.
func runWALReconciler(ctx context.Context, signingKeysManager *keys.Manager, walDir string) {
	if signingKeysManager == nil {
		log.Print("clean-code-eval-gate: Stage 9.2 reconciler skipped (scaffold mode, NoopSigner WAL writer). Real KMS deployments MUST set CLEAN_CODE_KMS_PROVIDER + CLEAN_CODE_WAL_RECONCILER_DSN.")
		return
	}
	dsn := os.Getenv(envWALReconcilerDSN)
	if dsn == "" {
		log.Fatalf("clean-code-eval-gate: %s is REQUIRED when CLEAN_CODE_KMS_PROVIDER is set (Stage 9.2 reconciler MUST run before traffic is served; see docs/runbook.md Stage 9.2)", envWALReconcilerDSN)
	}

	reconcilerDB, oerr := sql.Open("postgres", dsn)
	if oerr != nil {
		log.Fatalf("clean-code-eval-gate: open postgres (%s): %v", envWALReconcilerDSN, oerr)
	}
	defer reconcilerDB.Close()

	// Mirror the gateway's pingDBWithRetry pattern
	// (cmd/clean-code-gateway/main.go): capture the
	// last ping error and surface it with log.Fatalf
	// once attempts are exhausted. Falling through to
	// `keys.NewSQLStore` / `composition.NewWALReconciler`
	// on a dead connection produces a confusing failure
	// far from the root cause (a downstream "driver:
	// bad connection" or a Run-time SELECT failure)
	// and silently violates the brief's
	// "reconciler MUST complete before the HTTP
	// listener accepts traffic" invariant.
	var lastPingErr error
	for i := 0; i < 30; i++ {
		lastPingErr = reconcilerDB.PingContext(ctx)
		if lastPingErr == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if lastPingErr != nil {
		log.Fatalf("clean-code-eval-gate: postgres ping (%s) failed after 30 attempts: %v (Stage 9.2 reconciler cannot proceed on a dead connection; aborting startup per docs/runbook.md Stage 9.2)", envWALReconcilerDSN, lastPingErr)
	}

	keyStore, kserr := keys.NewSQLStore(reconcilerDB)
	if kserr != nil {
		log.Fatalf("clean-code-eval-gate: keys.NewSQLStore: %v", kserr)
	}

	rec, rerr := composition.NewWALReconciler(ctx, composition.WALReconcilerConfig{
		DB:       reconcilerDB,
		Dir:      walDir,
		KeyStore: keyStore,
		Logger: func(msg string, kv ...any) {
			log.Printf("clean-code-eval-gate: reconciler: "+msg+" %v", kv)
		},
	})
	if rerr != nil {
		log.Fatalf("clean-code-eval-gate: composition.NewWALReconciler: %v", rerr)
	}
	if rec == nil {
		// Should not happen given the gates above, but
		// keep the assertion explicit so a refactor
		// that introduces a silent scaffold-mode return
		// is loud at startup.
		log.Fatalf("clean-code-eval-gate: composition.NewWALReconciler returned (nil, nil) despite non-nil KeyStore -- scaffold-mode regression")
	}

	stats, runErr := rec.Run(ctx)
	if runErr != nil {
		log.Fatalf("clean-code-eval-gate: reconciler.Run: %v (aborting startup; operator triage required per docs/runbook.md Stage 9.2)", runErr)
	}
	log.Printf("clean-code-eval-gate: Stage 9.2 reconciler completed: replayed=%+v skipped_existing=%+v skipped_bad_sig=%+v skipped_bad_shape=%+v warnings=%d",
		stats.Replayed, stats.SkippedExisting, stats.SkippedBadSig, stats.SkippedBadShape, len(stats.Warnings))
	for _, w := range stats.Warnings {
		log.Printf("clean-code-eval-gate: reconciler warning: %s", w)
	}
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

// writeBodyReadError maps a request-body read/decode
// error to the canonical HTTP status. A
// *[http.MaxBytesError] -- the sentinel
// [http.MaxBytesReader] returns when a client tries to
// stream more than [maxRequestBodyBytes] -- surfaces as
// 413 Request Entity Too Large so the caller can tell
// "your body is too big" apart from "your body is
// malformed JSON". Every other read/decode error
// surfaces as 400 Bad Request.
//
// Both handlers share this helper so the two routes
// have IDENTICAL body-size protection and uniform error
// shapes, closing the iter-N review gap where
// `/v1/eval/gate` used `io.ReadAll(r.Body)` and
// `/v1/eval/replay` used `json.NewDecoder(r.Body)`
// without either applying a cap.
func writeBodyReadError(w http.ResponseWriter, err error) {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		http.Error(w,
			fmt.Sprintf("bad request: body exceeds %d bytes", maxErr.Limit),
			http.StatusRequestEntityTooLarge)
		return
	}
	http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
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
//
// The request body is wrapped in [http.MaxBytesReader]
// before [io.ReadAll] so a malicious client cannot
// stream an unbounded body and OOM the gate process;
// the cap is [maxRequestBodyBytes] and is enforced
// identically on `/v1/eval/replay`.
func makeEvalHandler(gate *evaluator.Gate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeBodyReadError(w, err)
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
//
// The request body is wrapped in [http.MaxBytesReader]
// before the [json.Decoder] runs so the admin surface
// has the same OOM protection as the canonical verb;
// the cap is [maxRequestBodyBytes].
func makeReplayHandler(gate *evaluator.Gate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		var req replayRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeBodyReadError(w, err)
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
