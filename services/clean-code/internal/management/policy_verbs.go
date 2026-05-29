package management

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/telemetry"
)

// Canonical HTTP paths for the Stage 5.2 write verbs. Pinned
// here as exported constants so dashboards, runbooks, and the
// integration test harness can reference them without
// re-typing the path string.
//
// The URL pattern mirrors the verb name verbatim (dot ->
// slash); `policy.publish_rulepack` keeps the underscore in the
// last segment (per tech-spec Sec 8.5 lines 963-970).
const (
	// VerbPublishPath mounts `policy.publish`.
	VerbPublishPath = "/v1/policy/publish"
	// VerbActivatePath mounts `policy.activate`.
	VerbActivatePath = "/v1/policy/activate"
	// VerbPublishRulepackPath mounts `policy.publish_rulepack`.
	VerbPublishRulepackPath = "/v1/policy/publish_rulepack"

	// VerbMgmtOverridePath mounts `mgmt.override` (Stage
	// 5.3) -- the canonical operator-mute / unmute verb per
	// architecture Sec 6.3 line 1357 and Sec 1.5.1 row 5.
	// Sits on the `/v1/mgmt/...` namespace to match the
	// `mgmt.*` verb family (architecture Sec 6.3); MUST NOT
	// collide with the historical-draft 501 path
	// [VerbOverridePath] which lives on `/v1/policy/...`.
	VerbMgmtOverridePath = "/v1/mgmt/override"

	// VerbRulepackAddPath is the historical-draft verb name
	// the canonical surface explicitly REJECTS. The handler
	// returns 501 Not Implemented at this path so a stray
	// caller hitting it learns the verb is not available
	// rather than getting a 404 (ambiguous: "is the route
	// down? typo?").
	VerbRulepackAddPath = "/v1/policy/rulepack/add"
	// VerbRulepackRemovePath is the sibling historical-draft
	// REMOVE verb. Same 501 rationale.
	VerbRulepackRemovePath = "/v1/policy/rulepack/remove"
	// VerbOverridePath is the historical-draft
	// `policy.override` verb. Architecture Sec 6.5 pins the
	// canonical name as `mgmt.override`; this 501 surfaces
	// the rename without a 404. The canonical write path
	// lives at [VerbMgmtOverridePath].
	VerbOverridePath = "/v1/policy/override"

	// OIDCSubjectHeader is the HTTP header the authenticating
	// gateway (architecture Sec 8.4 + tech-spec Sec 8.5
	// "OIDC bearer tokens") fills with the caller's OIDC
	// subject. Stage 5.3 reads it to populate
	// `Override.actor_id`. The trust boundary is the gateway
	// -- in any deployment where clean-coded is directly
	// reachable from untrusted clients this header MUST be
	// stripped at the edge and re-injected by the auth proxy,
	// or attackers can spoof the actor.
	//
	// We choose `X-OIDC-Subject` rather than reading the
	// `sub` JWT claim ourselves because the gateway already
	// validates the token signature; re-validating here
	// would duplicate the verification path and require us
	// to plumb JWKS into the composition root. The runbook
	// pins this contract for operators.
	OIDCSubjectHeader = "X-OIDC-Subject"
)

// stewardWriter is the narrow subset of [steward.Steward] the
// HTTP layer consumes. Defined here (rather than importing
// `*steward.Steward` everywhere) so tests can inject a fake
// without touching the keys/signing dependency tree.
type stewardWriter interface {
	Publish(ctx context.Context, req steward.PublishRequest) (steward.PolicyVersion, error)
	Activate(ctx context.Context, req steward.ActivateRequest) (steward.PolicyActivation, error)
	PublishRulepack(ctx context.Context, req steward.PublishRulepackRequest) (steward.RulePack, []steward.Rule, error)
	Override(ctx context.Context, req steward.OverrideRequest) (steward.Override, error)
}

// PolicyWriter is the HTTP write-side surface of the
// management package. Wraps a [steward.Steward] (or any
// [stewardWriter]) and serves the three canonical Stage 5.2
// write verbs.
//
// The struct field is typed against the narrow [stewardWriter]
// interface so a test can inject a stub; production wiring
// passes a real `*steward.Steward`.
type PolicyWriter struct {
	steward stewardWriter
}

// NewPolicyWriter constructs a PolicyWriter. `s` MAY be nil
// for scaffold-mode bring-ups; every write verb then returns
// 503 (the contract pinned alongside the read-side handler:
// "the verb exists here, the backing subsystem is down").
func NewPolicyWriter(s *steward.Steward) *PolicyWriter {
	// Avoid wrapping a typed-nil in an interface value -- that
	// would make `pw.steward != nil` true but the underlying
	// concrete value still be nil, hiding the misconfig
	// behind a delayed nil-pointer panic.
	if s == nil {
		return &PolicyWriter{steward: nil}
	}
	return &PolicyWriter{steward: s}
}

// newPolicyWriterFromInterface is the test-only constructor
// used to inject a fake [stewardWriter]. The production
// constructor takes a concrete `*steward.Steward` so the
// composition root cannot accidentally pass a stub.
func newPolicyWriterFromInterface(s stewardWriter) *PolicyWriter {
	return &PolicyWriter{steward: s}
}

// publishWireRequest is the inbound wire shape for the
// `policy.publish` verb. Mirrors [steward.PublishRequest] one-
// to-one; defined here so the HTTP surface owns the wire
// vocabulary independently of the steward's in-memory shape.
type publishWireRequest struct {
	Name            string                  `json:"name"`
	RuleRefs        []steward.RuleRef       `json:"rule_refs"`
	ThresholdRefs   []steward.ThresholdRef  `json:"threshold_refs"`
	RefactorWeights steward.RefactorWeights `json:"refactor_weights"`
}

// activateWireRequest is the inbound wire shape for the
// `policy.activate` verb. The handler decodes with
// `DisallowUnknownFields` so a body containing an unknown
// `scope` field is rejected with 400 -- pinning the
// architecture Sec 5.3.4 + brief invariant that v1 activation
// is global per deployment.
type activateWireRequest struct {
	PolicyVersionID string `json:"policy_version_id"`
	ActivatedBy     string `json:"activated_by"`
}

// publishRulepackWireRequest is the inbound wire shape for
// `policy.publish_rulepack`.
type publishRulepackWireRequest struct {
	PackID        string             `json:"pack_id"`
	Version       int                `json:"version"`
	DisplayName   string             `json:"display_name"`
	DescriptionMD string             `json:"description_md"`
	Rules         []steward.RuleSpec `json:"rules"`
}

// Publish serves `POST /v1/policy/publish`.
//
// Status codes:
//
//   - 200: row inserted; body is the persisted
//     [steward.PolicyVersion] with the freshly-minted
//     `policy_version_id`, `signature`, and `created_at`.
//   - 400: malformed JSON, unknown body fields, or shape
//     validation failure ([steward.ErrInvalidRequest]).
//   - 405: any method other than POST.
//   - 503: steward not wired, or no active signing key
//     ([steward.ErrNoActiveSigningKey]).
//   - 500: any other internal error; opaque body.
func (pw *PolicyWriter) Publish(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if pw.steward == nil {
		http.Error(w, "policy steward not wired", http.StatusServiceUnavailable)
		return
	}
	var wire publishWireRequest
	if !decodeStrict(w, r, &wire) {
		return
	}
	req := steward.PublishRequest{
		Name:            wire.Name,
		RuleRefs:        wire.RuleRefs,
		ThresholdRefs:   wire.ThresholdRefs,
		RefactorWeights: wire.RefactorWeights,
	}
	pv, err := pw.steward.Publish(r.Context(), req)
	if err != nil {
		writeStewardError(w, r, "policy.publish", err)
		return
	}
	// Stage 9.4 iter-3 follow-up: stamp the freshly-minted
	// `policy_version_id` on the verb span so dashboards can
	// correlate the publish event with the downstream
	// `policy.activate` that pins this PVID and the
	// `eval.gate` spans that evaluate against it. Mirror of
	// the `policy.activate` annotator call (line 230) and
	// `register_repo` repo_id annotator pattern (where the
	// verb CREATES the identifier rather than receives it
	// from the wire). Safe no-op when no OTel span is in
	// ctx or when the steward returned a zero UUID.
	telemetry.AnnotateVerbSpanPolicyVersionID(r.Context(), pv.PolicyVersionID)
	writeJSON(w, r, "policy.publish", http.StatusOK, pv)
}

// Activate serves `POST /v1/policy/activate`.
//
// Status codes:
//
//   - 200: activation row inserted.
//   - 400: malformed JSON, unknown body fields (specifically
//     including `scope` -- v1 is single-tenant per
//     architecture Sec 5.3.4), invalid
//     `policy_version_id`, or unknown policy version.
//   - 405: any method other than POST.
//   - 503: steward not wired, or no active signing key.
//   - 500: any other internal error.
func (pw *PolicyWriter) Activate(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if pw.steward == nil {
		http.Error(w, "policy steward not wired", http.StatusServiceUnavailable)
		return
	}
	var wire activateWireRequest
	if !decodeStrict(w, r, &wire) {
		return
	}
	id, err := uuid.FromString(wire.PolicyVersionID)
	if err != nil {
		http.Error(w, fmt.Sprintf("invalid policy_version_id: %s", err.Error()), http.StatusBadRequest)
		return
	}
	// Stage 9.4 iter-4: overwrite the verb-span's
	// `policy_version_id=""` open-time placeholder now that
	// the wire request has parsed and the UUID is valid.
	// Lets dashboards correlate `policy.activate` spans
	// with downstream `eval.gate` spans bound to the same
	// PVID. Safe no-op when no OTel span is in ctx.
	telemetry.AnnotateVerbSpanPolicyVersionID(r.Context(), id)
	pa, err := pw.steward.Activate(r.Context(), steward.ActivateRequest{
		PolicyVersionID: id,
		ActivatedBy:     wire.ActivatedBy,
	})
	if err != nil {
		writeStewardError(w, r, "policy.activate", err)
		return
	}
	writeJSON(w, r, "policy.activate", http.StatusOK, pa)
}

// publishRulepackResponse is the wire-shape returned by the
// `policy.publish_rulepack` handler.
type publishRulepackResponse struct {
	RulePack steward.RulePack `json:"rule_pack"`
	Rules    []steward.Rule   `json:"rules"`
}

// PublishRulepack serves `POST /v1/policy/publish_rulepack`.
//
// Status codes:
//
//   - 200: rule_pack + every rule row inserted in a single
//     transaction; body is `{rule_pack, rules}`.
//   - 400: malformed JSON, unknown body fields, or shape
//     validation failure.
//   - 409: duplicate `(pack_id, version)` or duplicate
//     `(rule_id, version)`. Pins the append-only contract --
//     a re-publish must surface as 409 (not 200, not 500).
//   - 405: any method other than POST.
//   - 503: steward not wired, or no active signing key.
//   - 500: any other internal error.
func (pw *PolicyWriter) PublishRulepack(w http.ResponseWriter, r *http.Request) {
	if !requirePOST(w, r) {
		return
	}
	if pw.steward == nil {
		http.Error(w, "policy steward not wired", http.StatusServiceUnavailable)
		return
	}
	var wire publishRulepackWireRequest
	if !decodeStrict(w, r, &wire) {
		return
	}
	req := steward.PublishRulepackRequest{
		PackID:        wire.PackID,
		Version:       wire.Version,
		DisplayName:   wire.DisplayName,
		DescriptionMD: wire.DescriptionMD,
		Rules:         wire.Rules,
	}
	pack, rules, err := pw.steward.PublishRulepack(r.Context(), req)
	if err != nil {
		writeStewardError(w, r, "policy.publish_rulepack", err)
		return
	}
	writeJSON(w, r, "policy.publish_rulepack", http.StatusOK, publishRulepackResponse{
		RulePack: pack,
		Rules:    rules,
	})
}

// UnimplementedVerb serves any disallowed `policy.*` path
// (`/v1/policy/rulepack/add`, `/v1/policy/rulepack/remove`,
// `/v1/policy/override`). Returns 501 Not Implemented with a
// canonical body so a stray caller learns the verb is NOT
// part of the v1 surface.
//
// The `verb` query field doubles as a discriminator so a single
// route can serve any disallowed verb name; the rootMux mounts
// one dedicated path per banned name so curl users land on the
// correct path verbatim.
func UnimplementedVerb(verbName string) http.HandlerFunc {
	body := map[string]string{
		"error": "unimplemented_verb",
		"verb":  verbName,
		"note":  "this verb is NOT in the canonical policy.* surface (tech-spec Sec 8.5 lines 963-970)",
	}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotImplemented)
		if err := json.NewEncoder(w).Encode(body); err != nil {
			slog.ErrorContext(r.Context(), "management.unimplemented_verb encode failed",
				"verb", verbName,
				"error", err.Error(),
			)
		}
	}
}

// requirePOST returns true when the request is POST. On any
// other method it writes 405 + Allow header and returns false.
func requirePOST(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodPost {
		return true
	}
	w.Header().Set("Allow", http.MethodPost)
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

// decodeStrict decodes r.Body into v with
// `DisallowUnknownFields()` so a body containing a typo (or
// the rejected `scope` field on `policy.activate`) returns
// 400 rather than silently dropping the value. Also rejects
// extra trailing data so a caller cannot send two JSON
// documents in one body and have only the first read.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		http.Error(w, fmt.Sprintf("invalid request body: %s", err.Error()), http.StatusBadRequest)
		return false
	}
	if dec.More() {
		http.Error(w, "invalid request body: trailing data after JSON document", http.StatusBadRequest)
		return false
	}
	return true
}

// writeJSON encodes body as JSON to w with the supplied status
// code. Logs encode failures using the same slog shape as the
// Stage 5.1 handler.
func writeJSON(w http.ResponseWriter, r *http.Request, verb string, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.ErrorContext(r.Context(), "management.write encode failed",
			"verb", verb,
			"error", err.Error(),
		)
	}
}

// writeStewardError translates a [steward] sentinel into the
// matching HTTP status. The mapping table:
//
//   - [steward.ErrNoActiveSigningKey] -> 503
//   - [steward.ErrInvalidRequest]     -> 400
//   - [steward.ErrInvalidOverride]    -> 400
//   - [steward.ErrUnknownRule]        -> 400
//   - [steward.ErrUnknownPolicyVersion] -> 400 (the caller
//     supplied an id we don't know -- a client error)
//   - [steward.ErrDuplicateRulePack]  -> 409
//   - [steward.ErrDuplicateRule]      -> 409
//   - anything else                   -> 500 + opaque body
//
// The 500 branch logs the raw error server-side and writes the
// fixed string `internal error` to the wire so unauthenticated
// clients don't see driver / package details.
func writeStewardError(w http.ResponseWriter, r *http.Request, verb string, err error) {
	switch {
	case errors.Is(err, steward.ErrNoActiveSigningKey):
		http.Error(w, "no active signing key", http.StatusServiceUnavailable)
	case errors.Is(err, steward.ErrInvalidRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, steward.ErrInvalidOverride):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, steward.ErrUnknownRule):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, steward.ErrUnknownPolicyVersion):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, steward.ErrUnknownRuleRef):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, steward.ErrUnknownThresholdRef):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, steward.ErrDuplicateRulePack):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, steward.ErrDuplicateRule):
		http.Error(w, err.Error(), http.StatusConflict)
	default:
		slog.ErrorContext(r.Context(), "management.write_verb failed",
			"verb", verb,
			"error", err.Error(),
		)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Routes returns an `http.ServeMux` ready to mount onto the
// service's HTTP listener. Mounts the three canonical Stage 5.2
// write verb paths plus the three banned-historical-draft
// paths (each backed by [UnimplementedVerb] -> 501) plus the
// canonical Stage 5.3 `mgmt.override` path.
//
// The composition root can register this mux as a child of a
// parent mux or mount each path manually via the constants
// (`VerbPublishPath` etc.).
func (pw *PolicyWriter) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc(VerbPublishPath, pw.Publish)
	mux.HandleFunc(VerbActivatePath, pw.Activate)
	mux.HandleFunc(VerbPublishRulepackPath, pw.PublishRulepack)
	mux.HandleFunc(VerbMgmtOverridePath, pw.Override)
	mux.HandleFunc(VerbRulepackAddPath, UnimplementedVerb("policy.rulepack.add"))
	mux.HandleFunc(VerbRulepackRemovePath, UnimplementedVerb("policy.rulepack.remove"))
	mux.HandleFunc(VerbOverridePath, UnimplementedVerb("policy.override"))
	return mux
}
