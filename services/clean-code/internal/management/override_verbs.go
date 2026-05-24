package management

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// overrideWireRequest is the inbound wire shape for the
// `mgmt.override` verb. Mirrors [steward.OverrideRequest]
// MINUS the `actor_id` field (which is sourced from the
// `X-OIDC-Subject` header, not the body -- see
// [OIDCSubjectHeader] for the rationale).
//
// The handler decodes with `DisallowUnknownFields` so any
// caller-supplied `expires_at` field is REJECTED with 400 --
// pinning the tech-spec Sec 10A "mute lifecycle" invariant
// that v1 has no TTL column. A future operator scripting the
// verb against an older draft contract learns of the rejection
// at the first try rather than discovering at the database
// layer that the row never carried the field.
//
// `actor_id` is explicitly listed as an UNKNOWN field on the
// wire shape -- the JSON tag `-` on
// [steward.OverrideRequest.ActorID] keeps the struct from
// binding it, and `DisallowUnknownFields` rejects callers who
// try to spoof the subject in the body. The trust boundary is
// the auth gateway, not the JSON.
type overrideWireRequest struct {
	RuleID      string              `json:"rule_id"`
	ScopeFilter steward.ScopeFilter `json:"scope_filter"`
	Mute        bool                `json:"mute"`
	Reason      string              `json:"reason"`
}

// overrideResponse is the wire shape returned by
// [PolicyWriter.Override]. Architecture Sec 6.3 line 1357 pins
// the verb's return type as `OverrideId` -- a single id, not
// the full row. We honour that with a 1-field response so a
// future conformance test (`mgmt.override returns OverrideId`)
// passes without a body shape rewrite. The persisted row is
// still readable via the `mgmt.read.*` family or, in v1, via
// direct table reads against `clean_code.override`.
type overrideResponse struct {
	OverrideID string `json:"override_id"`
}

// Override serves `POST /v1/mgmt/override` (architecture Sec
// 6.3 line 1357 + Sec 1.5.1 row 5 + tech-spec Sec 10A pin).
// Delegates to [steward.Steward.Override] which appends an
// [steward.Override] row in the Policy / rules sub-store.
//
// The handler enforces these Stage 5.3 invariants at the wire:
//
//  1. **No `expires_at` field.** `DisallowUnknownFields` on
//     the decoder rejects any caller attempt with 400 -- the
//     row schema has no `expires_at` column (tech-spec Sec
//     10A pin "mute lifecycle") and the v1 verb refuses to
//     pretend otherwise.
//
//  2. **No body `actor_id`.** Same `DisallowUnknownFields`
//     rejection. The `actor_id` is sourced exclusively from
//     the `X-OIDC-Subject` header so a caller cannot spoof
//     the subject. Missing header -> 401.
//
//  3. **Append-only.** This handler INSERTs exactly one row;
//     unmute is a separate POST with `mute=false`. The
//     historical-draft `PUT /override/{id}` shape is NOT
//     supported -- attempting it lands on a 404 (no route).
//
// Status codes:
//
//   - 200: row inserted; body is `{"override_id": "..."}`.
//   - 400: malformed JSON, unknown body fields (including
//     `expires_at` / `actor_id`), or shape validation failure
//     ([steward.ErrInvalidOverride] / [steward.ErrUnknownRule]).
//   - 401: missing or empty `X-OIDC-Subject` header.
//   - 405: any method other than POST.
//   - 503: steward not wired.
//   - 500: any other internal error; opaque body.
func (pw *PolicyWriter) Override(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if pw.steward == nil {
		http.Error(w, "policy steward not wired", http.StatusServiceUnavailable)
		return
	}
	actor := strings.TrimSpace(r.Header.Get(OIDCSubjectHeader))
	if actor == "" {
		// 401, not 400: the request was syntactically fine
		// but the caller has not authenticated. Operators
		// reading the failure log should see "missing
		// auth", not "bad request".
		http.Error(w, fmt.Sprintf("missing or empty %s header (the OIDC subject is required)", OIDCSubjectHeader), http.StatusUnauthorized)
		return
	}
	var wire overrideWireRequest
	if !decodeStrict(w, r, &wire) {
		return
	}
	req := steward.OverrideRequest{
		RuleID:      wire.RuleID,
		ScopeFilter: wire.ScopeFilter,
		Mute:        wire.Mute,
		Reason:      wire.Reason,
		ActorID:     actor,
	}
	o, err := pw.steward.Override(r.Context(), req)
	if err != nil {
		writeStewardError(w, r, "mgmt.override", err)
		return
	}
	writeJSON(w, r, "mgmt.override", http.StatusOK, overrideResponse{
		OverrideID: o.OverrideID.String(),
	})
}
