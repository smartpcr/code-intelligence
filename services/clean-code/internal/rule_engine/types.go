package rule_engine

import (
	"errors"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// Caller is the canonical `evaluation_run.caller` enum
// declared in `clean_code.evaluation_run_caller` (migration
// 0003 lines 115-118). Two values:
//
//   - [CallerEvalGate] -- `RunSync` invoked from `eval.gate`.
//   - [CallerBatchRefresh] -- `RunBatch` invoked from the
//     post-scan dispatcher.
//
// The closed set is enforced at the DB level by the
// `evaluation_run_caller` ENUM type; this Go alias mirrors
// the labels verbatim so the writer can pass the string
// straight through to `INSERT ... caller = $4`.
type Caller string

// Canonical caller labels. MUST match the ENUM in migration
// 0003 verbatim.
const (
	CallerEvalGate     Caller = "eval_gate"
	CallerBatchRefresh Caller = "batch_refresh"
)

// IsValid reports whether c is a member of the closed
// caller set.
func (c Caller) IsValid() bool {
	switch c {
	case CallerEvalGate, CallerBatchRefresh:
		return true
	default:
		return false
	}
}

// Verdict is the canonical `evaluation_verdict.verdict`
// enum declared in `clean_code.evaluation_verdict_value`
// (migration 0003 lines 126-130). Three values: `pass`,
// `warn`, `block`. The strings `fail` / `gated` are NOT
// canonical and the e2e-scenarios "verdict-enum-only-
// canonical" test rejects them.
type Verdict string

// Canonical verdict labels.
const (
	VerdictPass  Verdict = "pass"
	VerdictWarn  Verdict = "warn"
	VerdictBlock Verdict = "block"
)

// IsValid reports whether v is a member of the closed
// verdict set.
func (v Verdict) IsValid() bool {
	switch v {
	case VerdictPass, VerdictWarn, VerdictBlock:
		return true
	default:
		return false
	}
}

// rank returns the severity rank used by the rollup. Higher
// values dominate: `block` > `warn` > `pass`.
func (v Verdict) rank() int {
	switch v {
	case VerdictBlock:
		return 2
	case VerdictWarn:
		return 1
	default:
		return 0
	}
}

// Delta is the canonical `finding.delta` enum from migration
// 0003 lines 103-108. Four values per architecture Sec 5.4.1
// line 1215 (normative semantics):
//
//   - [DeltaNew] -- rule fired at this scope for the FIRST
//     SHA ever (no prior finding row for the
//     `(repo_id, scope_id, rule_id)` tuple at any earlier
//     SHA).
//   - [DeltaNewlyFailing] -- regression bucket. Rule was
//     previously NOT severity='block' at this scope (either
//     it didn't fire or fired at a lower severity) and is
//     NOW severity='block' at this scope at this SHA. Read
//     by `mgmt.read.regressions` (architecture Sec 6.3).
//   - [DeltaUnchanged] -- rule was already severity='block'
//     at this scope at the previous SHA and is still
//     severity='block' at this SHA.
//   - [DeltaResolved] -- rule was severity='block' at the
//     previous SHA and is no longer present (or fires at a
//     lower severity) at this SHA. The engine still emits a
//     finding row to record the resolution; severity is
//     pinned to 'info' so the rollup does not block on a
//     bug that just got fixed.
//
// The legacy strings `regression | improvement | flat` were
// considered and rejected by the canon-guard in the
// e2e-scenarios "finding-delta-canonical" test; they MUST
// NEVER appear here.
type Delta string

// Canonical delta labels. MUST match `clean_code.finding_delta`
// migration 0003 verbatim.
const (
	DeltaNew          Delta = "new"
	DeltaNewlyFailing Delta = "newly_failing"
	DeltaUnchanged    Delta = "unchanged"
	DeltaResolved     Delta = "resolved"
)

// IsValid reports whether d is a member of the closed delta
// set.
func (d Delta) IsValid() bool {
	switch d {
	case DeltaNew, DeltaNewlyFailing, DeltaUnchanged, DeltaResolved:
		return true
	default:
		return false
	}
}

// Sample is the denormalised MetricSample view the engine
// consumes. Extends [dsl.Sample] (which carries everything
// the DSL evaluator needs) with [Sample.ScopeSignature], the
// LITERAL scope coordinate the engine passes to the
// [steward.Steward.LatestMatchingOverride] override matcher
// to decide "is this scope muted for this rule?".
//
// The join `metric_sample x scope_binding` produces the
// `scope_kind` + `signature` pair; the engine does NOT
// reach back into the store during evaluation.
type Sample struct {
	dsl.Sample
	// ScopeSignature is the LITERAL scope identifier from
	// `scope_binding.signature` (architecture Sec 5.2.3 line
	// 1046). Used to build a [steward.CandidateScope] for
	// override matching. The empty string is invalid -- the
	// engine refuses to look up an override against an empty
	// signature (would match a `*` glob and silently mute
	// the scope).
	ScopeSignature string
}

// Finding is the canonical finding row per architecture Sec
// 5.4.1 lines 1201-1217 and migration 0003 lines 670-716.
// Fields mirror the SQL column shape verbatim:
//
//   - `finding_id` -- the row PK.
//   - `evaluation_run_id` -- non-null FK to [EvaluationRun].
//     EVERY finding row belongs to exactly one run.
//   - `repo_id`, `sha`, `scope_id` -- the scope-coordinate
//     triple. `scope_id` is a logical FK to
//     `scope_binding.scope_id` (Stage 1.3) enforced by the
//     writer until a follow-up migration adds the SQL FK.
//   - `rule_id`, `rule_version` -- the composite FK to
//     `rule(rule_id, version)`.
//   - `policy_version_id` -- the policy whose rule fired.
//   - `metric_sample_ids` -- the EXACT
//     [dsl.Sample.SampleID]s that triggered the predicate
//     (architecture Sec 5.4.1 line 1213 + Stage 5.7 brief:
//     "NOT a single metric_kind+value pair"). JSONB array on
//     the SQL side.
//   - `severity` -- per architecture Sec 5.4.1 line 1214 the
//     finding carries the rule's severity at the time of
//     evaluation. On [DeltaResolved] the engine pins it to
//     `info` so the resolution does not re-block.
//   - `delta` -- the [Delta] discriminator (see above).
//   - `explanation_md` -- human-readable explanation slot
//     (G4). The engine leaves it empty; the optional
//     "explain this finding" LLM service fills it in
//     post-hoc.
//   - `created_at` -- append-only timestamp.
type Finding struct {
	FindingID       uuid.UUID
	EvaluationRunID uuid.UUID
	RepoID          uuid.UUID
	SHA             string
	ScopeID         uuid.UUID
	RuleID          string
	RuleVersion     int
	PolicyVersionID uuid.UUID
	MetricSampleIDs []uuid.UUID
	Severity        steward.Severity
	Delta           Delta
	ExplanationMD   string
	CreatedAt       time.Time
}

// EvaluationRun mirrors a row in `clean_code.evaluation_run`
// per architecture Sec 5.4.2 lines 1219-1228 and migration
// 0003 lines 557-578. One row per evaluation invocation
// regardless of mode.
//
// `ScopeID` mirrors the nullable `scope_id` column added by
// migration 0008 (Stage 5.7 evaluator iter-6 feedback #2): a
// `nil` value represents the canonical whole-SHA evaluation
// (every `batch_refresh` by construction, and the `eval.gate`
// happy path when the caller did not pass a `scope` argument);
// a non-nil value represents a per-scope synchronous gate
// evaluation. The new column is the row-level discriminator
// that lets Store-level cross-replica dedup distinguish a
// scoped `eval_gate` row from an unscoped one safely.
type EvaluationRun struct {
	EvaluationRunID uuid.UUID
	RepoID          uuid.UUID
	SHA             string
	PolicyVersionID uuid.UUID
	Caller          Caller
	ScopeID         *uuid.UUID
	CreatedAt       time.Time
}

// EvaluationVerdict mirrors a row in
// `clean_code.evaluation_verdict` per architecture Sec
// 5.4.3 lines 1230-1239 and migration 0003 lines 599-629.
// One row per [EvaluationRun] (the FK from
// `evaluation_verdict.evaluation_run_id` -> `evaluation_run`
// is non-null per migration 0003 line 602).
//
// `degraded_reason` is nullable in SQL but represented here
// as a Go string. The empty string MUST be interpreted as
// "no reason" by the writer (translated to SQL NULL); a
// non-empty value MUST be one of the four members of the
// architecture Sec 8.2 closed set (`xrepo_edges_unavailable`,
// `samples_pending`, `policy_signature_invalid`,
// `percentile_stale`). The engine itself never raises a
// degraded reason on the rule-pass paths; the gate writes
// degraded verdicts directly on its two short-circuit
// paths.
type EvaluationVerdict struct {
	VerdictID       uuid.UUID
	EvaluationRunID uuid.UUID
	Verdict         Verdict
	Degraded        bool
	DegradedReason  string
	CreatedAt       time.Time
}

// RunResult is the value [Engine.RunSync] returns. Fields:
//
//   - `EvaluationRunID` -- the freshly-written
//     `evaluation_run.evaluation_run_id`.
//   - `EvaluationVerdictID` -- the freshly-written
//     `evaluation_verdict.verdict_id`.
//   - `FindingIDs` -- the freshly-written
//     `finding.finding_id` set. Sorted ASC by finding_id
//     so the caller observes a deterministic order across
//     replays.
//   - `Verdict` -- the severity-rollup verdict
//     ([Engine.computeVerdict]). Caller may avoid a second
//     read against the audit store by reading this field.
//
// The gate uses these three IDs to assemble the
// `eval.gate` HTTP response without re-querying the audit
// tables (architecture Sec 4.2 line 773 -- the gate returns
// `(verdict, findings[], degraded?, degraded_reason?)`).
type RunResult struct {
	EvaluationRunID     uuid.UUID
	EvaluationVerdictID uuid.UUID
	FindingIDs          []uuid.UUID
	Verdict             Verdict
}

// Sentinel errors. Each [errors.Is]-checkable so callers
// can branch on a single sentinel rather than string-
// matching a wrapped message.
var (
	// ErrStoreUnwired is returned by [New] when the
	// composition root passes a nil [Store]. Caught at
	// construction so a wiring bug does not panic on the
	// first RunSync call.
	ErrStoreUnwired = errors.New("rule_engine: New: Store is required")

	// ErrInvalidCaller is returned by [Engine.run] when an
	// internal caller is supplied a non-canonical [Caller]
	// label. Defensive: the public entry points
	// [Engine.RunSync] / [Engine.RunBatch] never trip this.
	ErrInvalidCaller = errors.New("rule_engine: caller is not in the canonical {eval_gate, batch_refresh} set")

	// ErrUnknownPolicyVersion is returned by [Engine.run]
	// when the caller-supplied policy_version_id is absent
	// from [Store]. Wraps the underlying store error so
	// `errors.Is(err, ErrUnknownPolicyVersion)` catches the
	// shape independent of the storage implementation.
	ErrUnknownPolicyVersion = errors.New("rule_engine: policy_version_id does not resolve")

	// ErrUnknownRuleRef is returned when a [steward.RuleRef]
	// inside the loaded policy fails to resolve. Distinct
	// from [ErrUnknownPolicyVersion] so a degraded policy
	// surface (one bad rule_id out of many) does not look
	// like a missing-policy error.
	ErrUnknownRuleRef = errors.New("rule_engine: rule_refs[i] does not resolve to a persisted rule")

	// ErrUnknownThresholdRef is the same shape but for the
	// `threshold_refs[i]` JSON-FK.
	ErrUnknownThresholdRef = errors.New("rule_engine: threshold_refs[i] does not resolve to a persisted threshold")

	// ErrPredicateCompile is returned when a rule's
	// `predicate_dsl` text fails to parse / bind. The
	// underlying [dsl.Error] is wrapped so callers can
	// extract the source position via `errors.As`.
	ErrPredicateCompile = errors.New("rule_engine: rule predicate failed to compile")

	// ErrPredicateEval is returned when a compiled
	// [dsl.Predicate] fails at evaluation time (an
	// unresolved threshold node or an unhandled AST node
	// kind -- both internal invariant violations the Stage
	// 5.4 Bind step should prevent, but the engine
	// surfaces them rather than silently dropping the
	// rule).
	ErrPredicateEval = errors.New("rule_engine: rule predicate failed to evaluate")

	// ErrSampleSourceEmpty is returned when [Store.ListMetricSamples]
	// returns a nil slice AND no error. Defensive: the
	// engine treats "no samples" as a legitimate outcome
	// (verdict=pass with zero findings); but a nil-without-
	// error slice combined with a non-empty rule set is a
	// strong signal of a misconfigured store, so we expose
	// the distinction for tests rather than masking it as
	// a successful no-op.
	ErrSampleSourceEmpty = errors.New("rule_engine: sample source returned (nil, nil)")
)
