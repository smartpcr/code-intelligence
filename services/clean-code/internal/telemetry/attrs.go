package telemetry

import (
	"context"

	"github.com/gofrs/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/evaluator"
)

// Canonical span-attribute keys per architecture Sec 8.
//
// The first three (verb, repo_id, caller_subject) are also
// stamped by the gateway in `internal/api` via the
// `api.Tracer` / `api.Span` seam. They are re-exported here
// so subsystems that emit their own spans (the aggregator,
// the WAL reconciler, future linked-mode RPCs) can use the
// same string keys without re-deriving them.
//
// The four eval-gate-specific keys (`policy_version_id`,
// `degraded`, `degraded_reason`, `verdict`) are populated by
// [AnnotateEvalGateSpan] when the verb handler knows the
// outcome; they are stamped with empty / `false` defaults by
// the gateway on every verb span so dashboards never see
// missing-key cardinality blowups for verbs that have no
// verdict semantics.
const (
	// AttrVerb is the canonical dotted verb name
	// (`mgmt.register_repo`, `eval.gate`, ...).
	AttrVerb = "verb"

	// AttrRepoID is the optional repo_id pulled from the
	// inbound request; empty string when the verb does
	// not take a repo or the request body has not yet
	// been parsed.
	AttrRepoID = "repo_id"

	// AttrCallerSubject is the verified bearer-token
	// `sub` claim.
	AttrCallerSubject = "caller_subject"

	// AttrPolicyVersionID is the `policy_version_id` the
	// verb evaluated against. For `eval.gate` this is the
	// resolved active policy (or the request-pinned PVID
	// on the replay surface); empty string on verbs that
	// do not bind to a policy_version.
	AttrPolicyVersionID = "policy_version_id"

	// AttrDegraded is the boolean "did this verb take the
	// degraded short-circuit path". False on the happy
	// path AND on verbs that have no degraded path.
	AttrDegraded = "degraded"

	// AttrDegradedReason is the canonical degraded-reason
	// enum value when AttrDegraded=true; empty string
	// otherwise. Canon for eval.gate is the closed set
	// {samples_pending, policy_signature_invalid,
	//  xrepo_edges_unavailable} from architecture Sec 6.1.
	AttrDegradedReason = "degraded_reason"

	// AttrVerdict is the canonical eval.gate verdict enum
	// {pass, warn, block}. Empty string on non-eval verbs.
	AttrVerdict = "verdict"
)

// AnnotateEvalGateSpan stamps the four eval-gate-specific
// canonical attributes (`policy_version_id`, `degraded`,
// `degraded_reason`, `verdict`) on the OTel span currently
// in `ctx`. Safe no-op when:
//
//   - `ctx` is nil
//   - no OTel span is bound to `ctx` (the SDK was not
//     initialised, or the gateway runs under a non-OTel
//     `api.Tracer` such as `NoopTracer` / `SlogTracer` /
//     `RecordingTracer`)
//
// The architecture pin is Stage 9.4 / Sec 8: every
// eval.gate span carries the verdict-side attributes so
// dashboards can filter spans by verdict + degraded
// cardinality without joining onto the audit DB.
func AnnotateEvalGateSpan(ctx context.Context, result evaluator.EvaluateResult) {
	if ctx == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		// IsRecording() is false for the OTel SDK's noop
		// span (returned when no provider has been
		// installed). Setting attributes on it is a no-op
		// but the IsRecording() short-circuit avoids the
		// (small) wasted attribute allocations.
		return
	}
	span.SetAttributes(
		attribute.String(AttrPolicyVersionID, uuidOrEmpty(result.PolicyVersionID)),
		attribute.Bool(AttrDegraded, result.Degraded),
		attribute.String(AttrDegradedReason, string(result.DegradedReason)),
		attribute.String(AttrVerdict, string(result.Verdict)),
	)
}

// AnnotateVerbDefaults stamps the eval-gate-specific
// attribute keys with their empty / false defaults on the
// OTel span in `ctx`. The gateway calls this on EVERY verb
// span (architecture Sec 8: every verb span carries the
// full canonical attribute set) so dashboards see a stable
// schema regardless of whether the verb has verdict
// semantics. Verbs that DO know the verdict (eval.gate) call
// [AnnotateEvalGateSpan] later in the handler to overwrite.
func AnnotateVerbDefaults(ctx context.Context) {
	if ctx == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	if span == nil || !span.IsRecording() {
		return
	}
	span.SetAttributes(
		attribute.String(AttrPolicyVersionID, ""),
		attribute.Bool(AttrDegraded, false),
		attribute.String(AttrDegradedReason, ""),
		attribute.String(AttrVerdict, ""),
	)
}

// uuidOrEmpty returns the canonical string form of a uuid
// or the empty string when the uuid is the zero value. The
// empty-string projection lets dashboards filter by
// `policy_version_id != ""` without first parsing.
func uuidOrEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
