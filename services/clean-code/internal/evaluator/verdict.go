package evaluator

import "errors"

// Verdict is the canonical `evaluation_verdict.verdict` enum
// per architecture Sec 5.4.2 lines 1192-1201 and tech-spec
// Sec 4.8. The Stage 6.1 brief calls it out verbatim:
//
//	"Verdict is the canonical enum `pass | warn | block`
//	 (architecture Sec 5.4.2 / tech-spec Sec 4.8) -- NOT
//	 `pass|fail|gated` (iter 1 evaluator item 6). Implement
//	 as a Go enum `Verdict { Pass, Warn, Block }` with no
//	 other values."
//
// The Go type system cannot truly close a string-derived
// enum (any caller can write `Verdict("fail")`), so the
// closure is enforced at runtime via [Verdict.IsValid] at
// every trust boundary: the engine adapter, the degraded
// store writer, and [Gate.Evaluate]'s post-engine
// inspection. The e2e-scenarios "verdict-enum-only-
// canonical" test pins the constant set.
//
// Migration 0003's `clean_code.evaluation_verdict_value`
// ENUM is the DB-side equivalent: the same three labels,
// rejected at INSERT time if a writer tries to smuggle a
// fourth value past the application layer.
type Verdict string

// Canonical verdict labels. MUST match
// `clean_code.evaluation_verdict_value` (migration 0003
// lines 126-130) verbatim.
const (
	VerdictPass  Verdict = "pass"
	VerdictWarn  Verdict = "warn"
	VerdictBlock Verdict = "block"
)

// IsValid reports whether v is a member of the closed
// {pass, warn, block} set. Used at every trust boundary
// (engine adapter, degraded store, gate) so a smuggled
// non-canonical verdict is caught before it lands in the
// audit row -- the DB ENUM constraint would catch it on
// the way in, but the application-layer check produces a
// clearer error.
func (v Verdict) IsValid() bool {
	switch v {
	case VerdictPass, VerdictWarn, VerdictBlock:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer so log lines / error
// messages render the verdict identically whether the
// caller has the typed value or the raw string.
func (v Verdict) String() string { return string(v) }

// DegradedReason is the canonical `evaluation_verdict.degraded_reason`
// enum per architecture Sec 8.2 / tech-spec Sec 7.7 C21.
// The DB CHECK constraint
// (`evaluation_verdict_degraded_reason_canonical`,
// migration 0003 lines 620-628) admits FOUR values:
//
//   - `xrepo_edges_unavailable`
//   - `samples_pending`
//   - `policy_signature_invalid`
//   - `percentile_stale`
//
// BUT `eval.gate` itself only ever raises the first THREE.
// `percentile_stale` is Insights-surface-only (tech-spec
// C17, architecture Sec 8.2): the gate evaluates a SHA at
// the time the caller asks for a verdict, so by
// construction the gate's percentile inputs are never
// "stale" -- they're either available (rule engine runs)
// or pending (samples_pending degraded path). The Stage
// 6.1 brief calls this out explicitly:
//
//	"`degraded_reason` is constrained to `samples_pending |
//	 policy_signature_invalid | xrepo_edges_unavailable`
//	 for eval.gate -- `percentile_stale` is NOT a valid
//	 eval.gate reason (it lives on the Insights surface per
//	 Stage 7.3; iter 1 evaluator item 8)."
//
// [DegradedReason.IsValidForGate] enforces this narrower
// closed set so the gate cannot accidentally surface
// `percentile_stale` -- a defence-in-depth check on top of
// the DB CHECK that mirrors the iter-3 evaluator item-16
// guard.
type DegradedReason string

// Canonical degraded reasons for the eval.gate surface.
// MUST match the DB CHECK constraint verbatim (the gate's
// closed set is the {samples_pending,
// policy_signature_invalid, xrepo_edges_unavailable}
// subset of the DB's four-value enum).
const (
	DegradedReasonSamplesPending         DegradedReason = "samples_pending"
	DegradedReasonPolicySignatureInvalid DegradedReason = "policy_signature_invalid"
	DegradedReasonXRepoEdgesUnavailable  DegradedReason = "xrepo_edges_unavailable"

	// degradedReasonPercentileStale is the value the gate
	// MUST refuse. Declared as an unexported constant so
	// the validator's switch can reject it by name (and so
	// tests can pin the literal that triggers rejection
	// without re-spelling the string). It is intentionally
	// NOT exported -- no eval.gate code path should ever
	// reach for this label.
	degradedReasonPercentileStale DegradedReason = "percentile_stale"
)

// ErrInvalidGateDegradedReason is the sentinel returned by
// the gate / degraded store when a caller tries to write a
// degraded row whose reason is not in the eval.gate closed
// set. `percentile_stale` is the canonical trigger: it is
// admitted by the DB CHECK but rejected by the gate so the
// audit trail cannot conflate an Insights-surface staleness
// claim with a gate-surface degraded path.
var ErrInvalidGateDegradedReason = errors.New("evaluator: degraded_reason is not a valid eval.gate reason")

// IsValidForGate reports whether r is one of the three
// degraded reasons that eval.gate may emit. Rejects the
// empty string (the writer requires a reason when
// `degraded=true`), `percentile_stale` (Insights-only),
// and any unknown value.
func (r DegradedReason) IsValidForGate() bool {
	switch r {
	case DegradedReasonSamplesPending,
		DegradedReasonPolicySignatureInvalid,
		DegradedReasonXRepoEdgesUnavailable:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (r DegradedReason) String() string { return string(r) }
