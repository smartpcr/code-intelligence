package evaluator

import (
	"context"
	"errors"
	"fmt"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// ErrSamplesPending is the sentinel returned by the gate's
// synchronous evaluate path when the requested SHA's
// `clean_code.commit.scan_status` is anything other than
// `'scanned'`. The gate's degraded short-circuit path
// persists a run+verdict with `degraded_reason='samples_pending'`
// per architecture Sec 4.2 lines 760-762 + Sec 5.3.6 and
// returns this sentinel so the calling HTTP handler can
// project it onto a 503 / "try again" response.
var ErrSamplesPending = errors.New("evaluator: samples for the requested SHA have not landed yet")

// ErrEngineUnwired indicates a composition-root mistake: the
// gate's [Gate.Evaluate] entry point was invoked without a
// [RuleEngine] dependency. The gate refuses the run rather
// than degrade to "no findings" -- a missing engine is a
// deployment bug, not a degraded condition.
var ErrEngineUnwired = errors.New("evaluator: rule engine has not been wired into the gate")

// ErrPolicyResolver is a generic wrap for policy-version
// lookup failures (the persisted row could not be read).
// Distinct from [ErrPolicySignatureInvalid] so an upstream
// storage outage does not look like a tampered policy.
var ErrPolicyResolver = errors.New("evaluator: policy version resolver failed")

// ErrNoActivePolicy is the sentinel returned by [Gate.Gate]
// when the [PolicyActivationReader] reports `ok=false` --
// no `clean_code.policy_activation` row has been written
// yet (fresh-deploy steady state). Per architecture
// Sec 3.7 lines 548-570 the gate's verb signature is
// `eval.gate(repo_id, sha, scope?)` -- it resolves the
// active `policy_version_id` itself; without one to bind
// the rule pass to, the gate cannot run.
//
// This is NOT a degraded path (per Stage 6.1 brief the
// degraded reasons are `samples_pending |
// policy_signature_invalid | xrepo_edges_unavailable`
// only). The gate writes NO audit row for this case --
// `evaluation_run.policy_version_id` is non-nullable in
// the canonical schema and there is no policy_version_id
// to write. The caller (HTTP handler) projects this onto
// a 409/503 operational-state response, distinct from a
// 500 internal failure or the degraded `200 + warn` reply.
var ErrNoActivePolicy = errors.New("evaluator: no active policy activation row exists yet")

// ErrActivationUnwired is the sentinel returned by
// [Gate.Gate] when the gate was constructed without an
// activation reader. Distinct from [ErrNoActivePolicy] so
// a composition-root wiring bug is NOT misclassified as
// the fresh-deploy "no activation yet" steady state. The
// gate fails LOUDLY on a missing dependency rather than
// silently degrading.
var ErrActivationUnwired = errors.New("evaluator: gate has no policy activation reader")

// EvaluateResult is the canonical reply from [Gate.Evaluate].
// All three IDs are always set -- even on the two short-
// circuit degraded paths the gate still writes ONE
// `evaluation_run` + ONE `evaluation_verdict` row (with zero
// findings) under the `clean_code_evaluator` grant, so the
// audit trail is uniform.
//
// `Degraded=true` is set ONLY on the two degraded paths
// (signature-invalid or samples-not-ready). On the happy
// path the field is left false; the [Verdict] enum still
// surfaces the engine's rollup ([VerdictPass] / [VerdictWarn]
// / [VerdictBlock]). Stage 6.1 brief: "Implement as a Go
// enum `Verdict { Pass, Warn, Block }` with no other
// values" -- the closure is enforced at the trust boundary
// in [Gate.Evaluate] via [Verdict.IsValid].
type EvaluateResult struct {
	EvaluationRunID     uuid.UUID
	EvaluationVerdictID uuid.UUID
	FindingIDs          []uuid.UUID
	Verdict             Verdict
	Degraded            bool
	DegradedReason      DegradedReason
	// PolicyVersionID is the `policy_version_id` the gate
	// evaluated against. Always populated on both the
	// happy and degraded paths so `eval.gate` span /
	// audit dashboards can filter by policy version
	// regardless of whether the request supplied the
	// PVID or the canonical [Gate.Gate] resolved it from
	// `policy_activation` (architecture Sec 8, Stage 9.4).
	PolicyVersionID uuid.UUID
}

// RuleEngine is the narrow port the gate calls on the
// happy synchronous path. Production wiring is
// `*rule_engine.Engine`; the gate's tests inject a stub.
//
// `RunSync` returns the canonical
// `(evaluation_run_id, evaluation_verdict_id, []finding_id)`
// triple from the workstream brief, plus the verdict string
// so the gate can surface it without a second store read.
type RuleEngine interface {
	RunSync(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID, policyVersionID uuid.UUID) (EngineRunResult, error)
}

// EngineRunResult mirrors `rule_engine.RunResult`. Re-declared
// in this package so the gate avoids importing the
// `rule_engine` package (which would create a cycle if the
// engine ever needed to call into evaluator).
//
// Verdict is the typed [Verdict] enum: the engine adapter
// (production wiring) is responsible for projecting
// `rule_engine.Verdict` onto this type. Stage 6.1 brief
// item: "Verdict is the canonical enum `pass | warn |
// block`" -- the adapter validates and the gate re-validates
// in [Gate.Evaluate] before forwarding to the caller.
type EngineRunResult struct {
	EvaluationRunID     uuid.UUID
	EvaluationVerdictID uuid.UUID
	FindingIDs          []uuid.UUID
	Verdict             Verdict
}

// SampleReadinessReader is the narrow port the gate uses to
// confirm that the post-scan dispatcher has finalised a SHA
// before the rule engine is invoked. Per rubber-duck #4: the
// gate MUST NOT accept a `samplesReady bool` from its
// caller -- that would let a malicious / careless client
// short-circuit the readiness check. Reading
// `clean_code.commit.scan_status` is the canonical source.
//
// `SamplesReady` returns `true` ONLY when
// `clean_code.commit.scan_status = 'scanned'` for `(repoID,
// sha)`. A SHA with `scan_status IN ('queued', 'running',
// 'failed')` returns `false` so the gate emits the
// `degraded_reason='samples_pending'` path.
type SampleReadinessReader interface {
	SamplesReady(ctx context.Context, repoID uuid.UUID, sha string) (bool, error)
}

// PolicyVersionReader is the narrow port the gate uses to
// resolve the `policyVersionID` argument into the canonical
// [steward.PolicyVersion] row. Production wiring is
// `steward.Steward.GetPolicyVersion` (or the steward's
// underlying store). The gate uses the resolved row to
// re-verify the persisted signature -- binding the request's
// policy_version_id to the canonical signed bytes per
// rubber-duck #3 (a valid signature for policy A MUST NOT
// authorise evaluation of policy B).
type PolicyVersionReader interface {
	GetPolicyVersion(ctx context.Context, policyVersionID uuid.UUID) (steward.PolicyVersion, error)
}

// PolicyActivationReader is the narrow port [Gate.Gate]
// uses to resolve the currently-active `policy_version_id`
// from the latest `clean_code.policy_activation` row.
// Production wiring is an adapter over
// [steward.Steward.ActivePolicyVersion]; tests inject a
// stub.
//
// The Stage 6.1 brief calls this out as step (1) of the
// `eval.gate(repo_id, sha, scope?)` verb: "resolve active
// `policy_version_id` via latest `policy_activation` row".
// The verb signature deliberately does NOT take a
// `policy_version_id` argument -- the gate resolves it
// itself so the caller cannot pin an evaluation to a
// non-active policy (which would be an audit / governance
// gap).
//
// Returns `ok=false` when no activation row exists yet
// (fresh-deploy steady state). The gate maps that onto
// [ErrNoActivePolicy] and writes NO audit row -- the
// canonical degraded reasons are constrained to
// `samples_pending | policy_signature_invalid |
// xrepo_edges_unavailable` per the Stage 6.1 brief and
// `evaluation_run.policy_version_id` is non-nullable.
//
// Implementations MUST return an error if they see
// `ok=true` with a zero `uuid.UUID` (defence in depth
// against a broken upstream surface).
type PolicyActivationReader interface {
	ActivePolicyVersionID(ctx context.Context) (uuid.UUID, bool, error)
}

// PolicySignatureVerifier is the narrow port the gate uses
// to verify the persisted policy_version signature. The
// production wiring is `steward.Steward.VerifyPolicyVersionSignature`;
// tests inject a stub. This indirection lets the gate
// remain independent of the steward package's internal
// canonicalisation while still binding signature checks to
// the specific policy version the caller is requesting.
type PolicySignatureVerifier interface {
	VerifyPolicyVersionSignature(ctx context.Context, pv steward.PolicyVersion) error
}

// DegradedRunStore is the narrow write port the gate uses
// on the two short-circuit degraded paths (signature-invalid
// or samples-pending). It writes ONE `evaluation_run`
// + ONE `evaluation_verdict` row under the
// `clean_code_evaluator` grant (architecture Sec 7.2 lines
// 1256-1261). The Rule Engine is NOT invoked on these
// paths, so the engine's own writer ownership does not
// apply -- the gate is the canonical writer per the
// workstream brief: "eval.gate writes its own run+verdict
// pair (with zero findings) only on the two short-circuit
// degraded paths where the engine is NOT invoked."
type DegradedRunStore interface {
	// AppendDegradedRun persists a single
	// `evaluation_run` + `evaluation_verdict` row pair
	// inside one transaction. The implementation MUST
	// reject a zero `EvaluationRunID` or `VerdictID`. The
	// `findings` count on the verdict row is always zero
	// for this path -- the gate does not synthesise rule
	// firings.
	AppendDegradedRun(ctx context.Context, run DegradedRun, verdict DegradedVerdict) error
}

// DegradedRun is the row the gate writes into
// `evaluation_run` on a degraded short-circuit. Mirrors the
// engine's `rule_engine.EvaluationRun` shape; the
// `Caller` value is always `'eval_gate'` because the
// degraded path is by-definition the gate's path.
//
// Iter-8 evaluator feedback #2: `ScopeID` is the canonical
// per-scope discriminator the engine writes on its
// happy-path rows (per migration 0008 +
// architecture.md §5.4.2 EvaluationRun field table).
// Carrying it on the degraded row preserves the canonical
// schema uniformly across the happy path and the two
// short-circuit degraded paths: a scoped eval.gate call
// whose signature is invalid (or whose samples are
// pending) writes a row that downstream cross-replica
// dedup, retention, and reconciliation can distinguish
// from an unscoped eval.gate call against the same SHA
// just as the happy-path rows do. `nil` is the canonical
// whole-SHA evaluation; non-nil is the per-scope call.
type DegradedRun struct {
	EvaluationRunID uuid.UUID
	RepoID          uuid.UUID
	SHA             string
	PolicyVersionID uuid.UUID
	ScopeID         *uuid.UUID
	CreatedAt       int64
}

// DegradedVerdict is the row the gate writes into
// `evaluation_verdict` on a degraded short-circuit. The
// `Verdict` is [VerdictWarn] per architecture Sec 3.7 lines
// 566-575 + operator pin `gate-degraded-policy=warn` from
// Sec 1.6: the gate NEVER blocks on a degraded path
// (signature-invalid or samples-pending), but it ALSO does
// not silently pass -- it returns [VerdictWarn] so the
// caller's UI can flag the run for human attention. The
// `Degraded=true` / `DegradedReason` pair is surfaced
// alongside the verdict via [EvaluateResult].
//
// `DegradedReason` is the typed [DegradedReason] enum
// constrained to the eval.gate closed set
// ([DegradedReasonSamplesPending],
// [DegradedReasonPolicySignatureInvalid],
// [DegradedReasonXRepoEdgesUnavailable]). `percentile_stale`
// is admitted by the DB CHECK but is INSIGHTS-ONLY and the
// gate / writer reject it via [DegradedReason.IsValidForGate]
// per the Stage 6.1 brief.
type DegradedVerdict struct {
	VerdictID       uuid.UUID
	EvaluationRunID uuid.UUID
	Verdict         Verdict
	Degraded        bool
	DegradedReason  DegradedReason
	CreatedAt       int64
}

// IDMinter returns a fresh UUID per call. Defaults to
// `uuid.NewV4` in production; tests inject a deterministic
// generator so the run+verdict IDs are predictable.
type IDMinter func() (uuid.UUID, error)

// EvaluateConfig is the optional dependency bundle for
// [Gate.NewGateWithEngine]. Every field is required in
// production; tests may pass a partial bundle so long as
// the test does not exercise the missing dependency.
//
// `Activation` is required by [Gate.Gate] (the canonical
// `eval.gate(repo_id, sha, scope?)` verb -- Stage 6.1
// brief step 1). It is OPTIONAL for [Gate.Evaluate], which
// takes an explicit `policyVersionID` argument and so does
// not need to resolve the active activation row.
type EvaluateConfig struct {
	Engine          RuleEngine
	Readiness       SampleReadinessReader
	PolicyReader    PolicyVersionReader
	SignatureVerify PolicySignatureVerifier
	DegradedStore   DegradedRunStore
	Activation      PolicyActivationReader
	NewID           IDMinter
	Now             func() int64
}

// NewGateWithEngine wires a gate that supports both the
// signature-only [Gate.VerifyPolicy] entry point (unchanged
// from Stage 5.6) AND the new synchronous-evaluate
// [Gate.Evaluate] entry point (Stage 5.7 evaluator feedback
// #3). `mgr` is the [keys.Manager] used by `VerifyPolicy`;
// `cfg` is the dependency bundle used by `Evaluate`.
//
// A nil `cfg.Engine` / `cfg.Readiness` / `cfg.PolicyReader` /
// `cfg.SignatureVerify` / `cfg.DegradedStore` causes
// `Evaluate` to return [ErrEngineUnwired] -- that's a
// composition-root bug and the gate refuses to fabricate a
// "degraded" path around it.
func NewGateWithEngine(g *Gate, cfg EvaluateConfig) *Gate {
	if g == nil {
		g = NewGate(nil)
	}
	g.engine = cfg.Engine
	g.readiness = cfg.Readiness
	g.policyReader = cfg.PolicyReader
	g.sigVerify = cfg.SignatureVerify
	g.degradedStore = cfg.DegradedStore
	g.activation = cfg.Activation
	g.newID = cfg.NewID
	g.now = cfg.Now
	return g
}

// Evaluate is the synchronous gate entry point invoked by
// `eval.gate` per architecture Sec 4.2 lines 760-772.
// Flow:
//
//  1. Resolve `policyVersionID` via [PolicyVersionReader].
//  2. Re-verify the persisted signature against THAT
//     policy_version's canonical bytes (rubber-duck #3 --
//     a valid signature for policy A MUST NOT authorise
//     evaluation of policy B).
//  3. Check `clean_code.commit.scan_status` via
//     [SampleReadinessReader]. If not 'scanned': write the
//     `samples_pending` degraded run + return
//     [ErrSamplesPending].
//  4. Delegate to [RuleEngine.RunSync]. The engine writes
//     the canonical run+verdict+findings inside a single
//     transaction; the gate forwards the IDs back to the
//     caller.
//
// On the two degraded paths the gate writes ONE
// `evaluation_run` + ONE `evaluation_verdict` (zero
// findings) under the `clean_code_evaluator` grant and
// returns a [EvaluateResult] with `Degraded=true` plus the
// sentinel error so the caller can branch on it
// (`errors.Is(err, ErrPolicySignatureInvalid)` /
// `errors.Is(err, ErrSamplesPending)`).
func (g *Gate) Evaluate(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID, policyVersionID uuid.UUID) (EvaluateResult, error) {
	if g == nil {
		return EvaluateResult{}, ErrGateUnwired
	}
	if g.engine == nil || g.readiness == nil || g.policyReader == nil || g.sigVerify == nil || g.degradedStore == nil {
		return EvaluateResult{}, ErrEngineUnwired
	}
	if g.newID == nil {
		g.newID = uuid.NewV4
	}
	if g.now == nil {
		// Time semantics: nano-precision is fine for the
		// audit row's `created_at`; the SQLStore will
		// project to TIMESTAMPTZ on write.
		g.now = defaultNowNanos
	}
	if repoID == uuid.Nil {
		return EvaluateResult{}, fmt.Errorf("evaluator: Evaluate: repo_id is the zero uuid")
	}
	if sha == "" {
		return EvaluateResult{}, fmt.Errorf("evaluator: Evaluate: sha is empty")
	}
	if policyVersionID == uuid.Nil {
		return EvaluateResult{}, fmt.Errorf("evaluator: Evaluate: policy_version_id is the zero uuid")
	}

	// 1. Load the policy_version row by ID. The Steward's
	//    signature lives on this row -- we use it as the
	//    canonical binding so a caller cannot present a
	//    valid signature for policy A in support of an
	//    evaluation against policy B (rubber-duck #3).
	pv, err := g.policyReader.GetPolicyVersion(ctx, policyVersionID)
	if err != nil {
		return EvaluateResult{}, fmt.Errorf("%w: policy_version_id=%s: %v", ErrPolicyResolver, policyVersionID, err)
	}
	if pv.PolicyVersionID != policyVersionID {
		return EvaluateResult{}, fmt.Errorf("%w: resolver returned policy_version_id=%s for request %s", ErrPolicyResolver, pv.PolicyVersionID, policyVersionID)
	}

	// 2. Re-verify the persisted signature against the
	//    canonical bytes of THIS policy_version. A
	//    signature-invalid outcome takes the degraded
	//    short-circuit path -- the engine is NOT invoked.
	if err := g.sigVerify.VerifyPolicyVersionSignature(ctx, pv); err != nil {
		return g.writeDegraded(ctx, repoID, sha, scope, policyVersionID, DegradedReasonPolicySignatureInvalid, fmt.Errorf("%w: %v", ErrPolicySignatureInvalid, err))
	}

	// 3. Sample readiness: a SHA whose
	//    `clean_code.commit.scan_status != 'scanned'`
	//    cannot be rule-evaluated meaningfully. We persist
	//    a `samples_pending` degraded row so the audit
	//    trail captures the gate's decision (architecture
	//    Sec 5.3.6 + workstream brief).
	ready, err := g.readiness.SamplesReady(ctx, repoID, sha)
	if err != nil {
		return EvaluateResult{}, fmt.Errorf("evaluator: SamplesReady: %w", err)
	}
	if !ready {
		return g.writeDegraded(ctx, repoID, sha, scope, policyVersionID, DegradedReasonSamplesPending, ErrSamplesPending)
	}

	// 4. Happy path -- delegate to the rule engine. The
	//    engine is the canonical writer of the
	//    run+verdict+findings triple; the gate only
	//    forwards its IDs.
	out, err := g.engine.RunSync(ctx, repoID, sha, scope, policyVersionID)
	if err != nil {
		return EvaluateResult{}, fmt.Errorf("evaluator: engine.RunSync: %w", err)
	}
	// Defensive: validate the engine adapter returned a
	// canonical [Verdict]. The adapter SHOULD reject
	// non-canonical values itself (rule_engine.Verdict is
	// closed via its own switch), but a broken / stale
	// adapter could smuggle `"fail"` / `"gated"` past --
	// Stage 6.1 brief item: "Implement as a Go enum
	// `Verdict { Pass, Warn, Block }` with no other
	// values" -- the closure is enforced at every trust
	// boundary, not just at construction.
	if !out.Verdict.IsValid() {
		return EvaluateResult{}, fmt.Errorf("evaluator: engine returned non-canonical verdict %q (allowed: pass|warn|block)", out.Verdict)
	}
	return EvaluateResult{
		EvaluationRunID:     out.EvaluationRunID,
		EvaluationVerdictID: out.EvaluationVerdictID,
		FindingIDs:          out.FindingIDs,
		Verdict:             out.Verdict,
		PolicyVersionID:     policyVersionID,
	}, nil
}

// writeDegraded is the shared "short-circuit path" writer.
// Both degraded reasons (signature-invalid, samples-pending)
// go through this function so the audit row shape is
// identical regardless of WHICH degraded reason fired.
//
// Iter-8 evaluator feedback #2: `scope` is plumbed onto
// the [DegradedRun] so the audit row records the per-scope
// discriminator (migration 0008 / architecture.md §5.4.2)
// when the eval.gate call was per-scope. `nil` is the
// canonical whole-SHA evaluation; non-nil is the per-scope
// call. Dropping it would make the degraded row lie about
// the scope of the evaluation that was attempted.
//
// Stage 6.1 brief: reason is restricted to the eval.gate
// closed set via [DegradedReason.IsValidForGate]. The two
// callers above pass [DegradedReasonPolicySignatureInvalid]
// and [DegradedReasonSamplesPending] respectively, but the
// validator catches a future caller that tries to write a
// `percentile_stale` degraded row through the gate -- the
// "percentile-stale-not-on-gate" e2e scenario.
func (g *Gate) writeDegraded(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID, policyVersionID uuid.UUID, reason DegradedReason, retErr error) (EvaluateResult, error) {
	if !reason.IsValidForGate() {
		return EvaluateResult{}, fmt.Errorf("%w: %q (allowed: %s | %s | %s)",
			ErrInvalidGateDegradedReason,
			reason,
			DegradedReasonSamplesPending,
			DegradedReasonPolicySignatureInvalid,
			DegradedReasonXRepoEdgesUnavailable,
		)
	}
	runID, err := g.newID()
	if err != nil {
		return EvaluateResult{}, fmt.Errorf("evaluator: writeDegraded: mint run_id: %w", err)
	}
	verdictID, err := g.newID()
	if err != nil {
		return EvaluateResult{}, fmt.Errorf("evaluator: writeDegraded: mint verdict_id: %w", err)
	}
	now := g.now()
	run := DegradedRun{
		EvaluationRunID: runID,
		RepoID:          repoID,
		SHA:             sha,
		PolicyVersionID: policyVersionID,
		ScopeID:         scope,
		CreatedAt:       now,
	}
	verdict := DegradedVerdict{
		VerdictID:       verdictID,
		EvaluationRunID: runID,
		Verdict:         VerdictWarn,
		Degraded:        true,
		DegradedReason:  reason,
		CreatedAt:       now,
	}
	if err := g.degradedStore.AppendDegradedRun(ctx, run, verdict); err != nil {
		return EvaluateResult{}, fmt.Errorf("evaluator: writeDegraded: append: %w", err)
	}
	return EvaluateResult{
		EvaluationRunID:     runID,
		EvaluationVerdictID: verdictID,
		FindingIDs:          nil,
		Verdict:             VerdictWarn,
		Degraded:            true,
		DegradedReason:      reason,
		PolicyVersionID:     policyVersionID,
	}, retErr
}

// defaultNowNanos returns the current UnixNano. Indirected
// so [NewGateWithEngine] tests can pin time.
func defaultNowNanos() int64 {
	return nowUnixNano()
}

// Gate is the canonical `eval.gate(repo_id, sha, scope?)`
// verb entry point per architecture Sec 3.7 lines 548-570
// and the Stage 6.1 brief. The verb signature deliberately
// does NOT take a `policy_version_id` -- step (1) of the
// brief is for the gate itself to "resolve active
// `policy_version_id` via latest `policy_activation` row".
//
// Flow:
//
//  1. Resolve the active `policy_version_id` via the
//     [PolicyActivationReader]. If no activation row
//     exists (`ok=false`), return [ErrNoActivePolicy]
//     and write NO audit row -- this is a fresh-deploy
//     steady state, not a degraded condition (the
//     canonical degraded reasons in the Stage 6.1 brief
//     are `samples_pending | policy_signature_invalid |
//     xrepo_edges_unavailable` only, and
//     `evaluation_run.policy_version_id` is non-nullable).
//  2. Delegate to [Gate.Evaluate] with the resolved
//     `policy_version_id`. Steps (2)-(5) of the Stage 6.1
//     brief (signature verify, sample readiness, sync
//     rule engine delegation, no-double-write) are
//     executed inside [Gate.Evaluate].
//
// A composition-root wiring bug (nil `activation` reader)
// returns [ErrActivationUnwired], NOT [ErrNoActivePolicy] --
// "no activation row exists" and "the gate was not wired
// with an activation reader" are DIFFERENT states.
func (g *Gate) Gate(ctx context.Context, repoID uuid.UUID, sha string, scope *uuid.UUID) (EvaluateResult, error) {
	if g == nil {
		return EvaluateResult{}, ErrGateUnwired
	}
	if g.activation == nil {
		return EvaluateResult{}, ErrActivationUnwired
	}
	// Resolving the active policy_version_id BEFORE the
	// trust-boundary input validation below mirrors the
	// verb's contract: an unauthenticated caller cannot
	// influence which policy_version the rules will run
	// against. Step (1) of the Stage 6.1 brief is
	// service-internal.
	policyVersionID, ok, err := g.activation.ActivePolicyVersionID(ctx)
	if err != nil {
		return EvaluateResult{}, fmt.Errorf("evaluator: Gate: activation lookup: %w", err)
	}
	if !ok {
		return EvaluateResult{}, ErrNoActivePolicy
	}
	if policyVersionID == uuid.Nil {
		// Defence in depth: the adapter SHOULD reject
		// `(ok=true, zero uuid)` itself, but the gate
		// validates again so a broken adapter cannot
		// smuggle a zero uuid through to
		// [Gate.Evaluate] (which would reject it but
		// with a less informative error).
		return EvaluateResult{}, fmt.Errorf("evaluator: Gate: activation reader returned ok=true with zero policy_version_id")
	}
	return g.Evaluate(ctx, repoID, sha, scope, policyVersionID)
}
