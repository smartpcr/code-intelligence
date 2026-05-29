package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Group authorization: tech-spec Sec 8.5 line 1003
// ("REST surface enforces OIDC group claims").
//
// The gateway's [Authenticator] verifies the bearer token's
// signature / issuer / audience / expiry; the [Authorizer]
// runs AFTER auth succeeds and enforces caller-vs-verb
// access policy. Splitting authn from authz keeps each
// concern testable in isolation -- the OIDC test suite
// exercises token verification regardless of policy, and the
// authz test suite exercises policy regardless of token
// shape.
//
// # Why a separate interface, not a flag on Authenticator
//
// 1.  Authn (token verification) is symmetric: every verb
//     uses the same JWKS / issuer / audience pin. Authz
//     (group-vs-verb policy) is per-verb. Bundling them
//     into one Authenticator interface would force every
//     impl to carry per-verb policy tables.
// 2.  Authorizer is composition-root configuration: which
//     groups gate which verb is a deployment choice (e.g.
//     `policy.publish` requires `clean-code-admins`; the
//     mgmt.read.* verbs require `clean-code-readers`). The
//     IdP provides claims; the gateway maps claims -> verb
//     access policy.
// 3.  A NoopAuthorizer keeps the gateway runnable in
//     scaffold deployments where policy enforcement is
//     deferred (e.g. local docker-compose, integration test
//     harnesses); production composition roots wire a
//     [GroupClaimAuthorizer] instead.

// Sentinel error a custom Authorizer wraps to signal
// "caller is authenticated but their groups do not include
// any of the verb's required groups". The gateway maps this
// to HTTP 403 with [CodeInsufficientGroup].
//
// Pattern in custom impls:
//
//	return fmt.Errorf("%w: caller groups %v missing required %v",
//	    api.ErrInsufficientGroup, gotGroups, wantGroups)
//
// Any other error returned by Authorize is treated as an
// internal failure (500 with [CodeInternalError]).
var ErrInsufficientGroup = errors.New("api: caller groups do not satisfy verb requirement")

// Authorizer enforces caller-vs-verb access policy after
// the [Authenticator] verifies the bearer token. The gateway
// calls Authorize AFTER auth succeeds and BEFORE the span
// opens (so a denial is logged but does NOT create a span
// for an unauthorised request).
//
// The verb string is the canonical dotted name
// (`{namespace}.{name}`) so an Authorizer can apply a
// per-verb policy table without parsing the URL.
//
// Authorize MUST return [ErrInsufficientGroup] (wrapped if
// needed) for the deny case so the gateway can map to 403.
// Any other error type is treated as an internal authz
// failure -- logged server-side, surfaced as 500 with the
// opaque [CodeInternalError] body.
type Authorizer interface {
	Authorize(ctx context.Context, identity *Identity, verb string) error
}

// NoopAuthorizer permits every authenticated request. The
// default Authorizer when [ServerConfig.Authorizer] is nil.
// Use ONLY in scaffold / test composition roots; production
// MUST install a [GroupClaimAuthorizer] (or a custom
// equivalent) so the OIDC-group-claim contract from
// tech-spec Sec 8.5 is enforced.
type NoopAuthorizer struct{}

// Authorize implements [Authorizer]. Always returns nil.
func (NoopAuthorizer) Authorize(_ context.Context, _ *Identity, _ string) error {
	return nil
}

// GroupClaimAuthorizer enforces an OIDC group-claim policy
// keyed by canonical verb name. The policy is a map of
// dotted verb name -> required group set; a caller is
// admitted iff the verified Identity's groups (sourced from
// the configured claim name) include at least ONE entry
// from the verb's required-group set.
//
// # Policy shape -- per-verb required groups
//
// Each entry in [VerbGroupPolicy] maps a dotted verb name
// to its required-group set. The default policy
// ([DefaultVerbGroupPolicy]) reflects the per-namespace
// access tier matrix from tech-spec Sec 8.5:
//
//   - `eval.gate`               -- `clean-code-ci`
//   - `mgmt.read.*`             -- `clean-code-readers`
//   - `mgmt.register_repo`      -- `clean-code-admins`
//   - `mgmt.set_mode`           -- `clean-code-admins`
//   - `mgmt.retract_sample`     -- `clean-code-admins`
//   - `mgmt.rescan`             -- `clean-code-admins`
//   - `mgmt.override`           -- `clean-code-admins`
//   - `policy.*`                -- `clean-code-admins`
//   - `ingest.*`                -- `clean-code-ci`
//
// A composition root MAY override or replace the default
// policy at boot. A verb with no policy entry is open to
// every authenticated caller (the default Authorizer
// contract -- a missing policy is NOT an implicit deny).
//
// # Claim name
//
// The OIDC group claim name is configurable via
// [GroupClaimAuthorizer.ClaimName]. Defaults to "groups"
// (the de-facto convention; OIDC core does not specify a
// canonical group claim, so deployments use "groups",
// "roles", or "wids" -- the composition root picks).
//
// # Membership semantics -- ANY-of, not ALL-of
//
// A caller satisfies a verb's policy when their group set
// intersects the verb's required-group set (set-membership
// "any of"). This matches the canonical RBAC pattern: a
// `clean-code-admins` member can perform every admin verb
// regardless of whether they also carry the `clean-code-ci`
// group, and a multi-role caller is not blocked by the
// strictest required group.
type GroupClaimAuthorizer struct {
	// ClaimName is the JWT claim whose value is the
	// caller's group list. Empty defaults to "groups".
	// The claim value MAY be a single JSON string or a
	// JSON array of strings; both forms are accepted.
	ClaimName string

	// VerbGroupPolicy maps a canonical dotted verb name
	// to its required-group set. A verb absent from this
	// map is open to every authenticated caller; the
	// authz layer is opt-in per verb.
	VerbGroupPolicy map[string][]string
}

// DefaultGroupClaimName is the JWT claim name the gateway
// reads when [GroupClaimAuthorizer.ClaimName] is empty.
// "groups" is the de-facto OIDC convention for the
// human-friendly group/role list (Okta, Auth0, Keycloak,
// AAD all support emitting groups under this claim).
const DefaultGroupClaimName = "groups"

// Canonical group identifiers for the default per-verb
// policy. Exported so composition roots / tests can refer
// to them without hard-coding the literal strings.
const (
	GroupReaders = "clean-code-readers"
	GroupCI      = "clean-code-ci"
	GroupAdmins  = "clean-code-admins"
)

// DefaultVerbGroupPolicy is the per-verb required-group set
// the production composition root installs by default.
// Mirrors the access-tier matrix sketched in tech-spec Sec
// 8.5 (`OIDC group claims`) and architecture Sec 8
// authorisation discussion:
//
//   - `clean-code-readers` -- read-only mgmt surface.
//   - `clean-code-ci`      -- automated callers (eval gate
//     blocking a PR; webhook publishers replaying
//     ingest payloads).
//   - `clean-code-admins`  -- destructive writes (policy
//     publish/activate, repo mode flip, sample retraction,
//     rule override).
//
// `clean-code-admins` is granted membership in the lower
// tiers by IdP convention (an admin can read and CI),
// modelled here as multiple required-group entries on the
// LOW-tier verbs so an admin's single group claim still
// satisfies a reader-tier policy.
func DefaultVerbGroupPolicy() map[string][]string {
	return map[string][]string{
		// Eval is the CI hot path (gating PR merges).
		// Admins can also drive it for incident replay.
		"eval.gate": {GroupCI, GroupAdmins},

		// Read surface: readers + admins (admins can read
		// without holding the reader group separately).
		"mgmt.read.repo":           {GroupReaders, GroupAdmins},
		"mgmt.read.metric_sample":  {GroupReaders, GroupAdmins},
		"mgmt.read.metric_samples": {GroupReaders, GroupAdmins},
		"mgmt.read.findings":       {GroupReaders, GroupAdmins},
		"mgmt.read.regressions":    {GroupReaders, GroupAdmins},
		"mgmt.read.cross_repo":     {GroupReaders, GroupAdmins},
		"mgmt.read.portfolio":      {GroupReaders, GroupAdmins},
		"mgmt.read.refactor_plan":  {GroupReaders, GroupAdmins},

		// Write surface: admins only.
		"mgmt.register_repo":  {GroupAdmins},
		"mgmt.set_mode":       {GroupAdmins},
		"mgmt.retract_sample": {GroupAdmins},
		"mgmt.rescan":         {GroupAdmins},
		"mgmt.override":       {GroupAdmins},

		// Policy surface: admins only (publishing /
		// activating policy versions is a governed
		// operation).
		"policy.publish":          {GroupAdmins},
		"policy.activate":         {GroupAdmins},
		"policy.publish_rulepack": {GroupAdmins},
		// Read-only key listing is open to readers /
		// admins (an admin needs to confirm which key id
		// matches the in-cache signing material; a
		// reader needs the key id to verify a signature
		// out-of-band).
		"policy.keys.list_active": {GroupReaders, GroupAdmins},

		// Ingest: CI publishers + admins (admins drive
		// manual replay of stuck CI payloads).
		"ingest.coverage":     {GroupCI, GroupAdmins},
		"ingest.test_balance": {GroupCI, GroupAdmins},
		"ingest.churn":        {GroupCI, GroupAdmins},
		"ingest.defects":      {GroupCI, GroupAdmins},
	}
}

// NewGroupClaimAuthorizer constructs a Group-claim
// authorizer using [DefaultVerbGroupPolicy]. The claim name
// is configurable; a caller passing "" uses the default
// "groups".
//
// A composition root that wants to override the default
// policy constructs `GroupClaimAuthorizer{...}` directly
// rather than calling this helper.
func NewGroupClaimAuthorizer(claimName string) *GroupClaimAuthorizer {
	return &GroupClaimAuthorizer{
		ClaimName:       claimName,
		VerbGroupPolicy: DefaultVerbGroupPolicy(),
	}
}

// Authorize implements [Authorizer]. Returns nil when the
// caller's groups intersect the verb's required-group set;
// returns an [ErrInsufficientGroup]-wrapped error when the
// caller is authenticated but lacks any required group;
// returns nil (open) when the verb has no policy entry.
//
// A nil identity returns an internal error -- the gateway
// MUST NOT call Authorize before authenticate succeeds, so
// a nil identity at this layer is a programmer error.
func (a *GroupClaimAuthorizer) Authorize(_ context.Context, identity *Identity, verb string) error {
	if a == nil {
		return errors.New("api: nil GroupClaimAuthorizer")
	}
	if identity == nil {
		return errors.New("api: nil identity passed to Authorize (programmer error -- authenticate must succeed first)")
	}
	required, hasPolicy := a.VerbGroupPolicy[verb]
	if !hasPolicy {
		// No policy entry -> open to every authenticated
		// caller. Composition roots that want a strict
		// "default deny" install a custom Authorizer
		// (e.g. wrap GroupClaimAuthorizer and return
		// ErrInsufficientGroup for missing-policy verbs).
		return nil
	}
	callerGroups, err := extractGroupClaim(identity, a.claimName())
	if err != nil {
		return fmt.Errorf("%w: cannot extract group claim %q: %v", ErrInsufficientGroup, a.claimName(), err)
	}
	if hasGroupIntersection(callerGroups, required) {
		return nil
	}
	return fmt.Errorf("%w: caller groups %v missing required (any of) %v",
		ErrInsufficientGroup, sortedCopy(callerGroups), sortedCopy(required))
}

func (a *GroupClaimAuthorizer) claimName() string {
	if a.ClaimName == "" {
		return DefaultGroupClaimName
	}
	return a.ClaimName
}

// extractGroupClaim reads the named claim from
// `identity.RawClaims`. Accepts both the JSON-string form
// (single group) and the JSON-array form (multiple groups);
// rejects any other shape because OIDC groups MUST be
// human-readable identifiers, not nested objects.
func extractGroupClaim(identity *Identity, claimName string) ([]string, error) {
	if identity.RawClaims == nil {
		return nil, fmt.Errorf("identity has no raw claims (authenticator stripped them)")
	}
	raw, ok := identity.RawClaims[claimName]
	if !ok || len(raw) == 0 {
		// Missing claim -> empty caller-group set; the
		// caller carries no admissible identity for any
		// gated verb. The Authorize caller maps this to
		// ErrInsufficientGroup via the empty-intersection
		// check.
		return nil, nil
	}
	// Try array first (the multi-group common case).
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many, nil
	}
	// Fall back to single string.
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, nil
		}
		return []string{single}, nil
	}
	return nil, fmt.Errorf("claim %q is neither a JSON string nor a JSON array of strings", claimName)
}

// hasGroupIntersection returns true iff any element of
// `caller` appears in `required`. Comparison is
// case-sensitive (matches the OIDC convention -- group
// identifiers are URL-shaped and case-sensitive). Empty
// `caller` always returns false; empty `required` always
// returns false (a verb that pins no required groups
// shouldn't be using GroupClaimAuthorizer to begin with --
// the absent-policy branch in Authorize handles that
// case).
func hasGroupIntersection(caller, required []string) bool {
	if len(caller) == 0 || len(required) == 0 {
		return false
	}
	want := make(map[string]struct{}, len(required))
	for _, g := range required {
		want[g] = struct{}{}
	}
	for _, g := range caller {
		if _, ok := want[g]; ok {
			return true
		}
	}
	return false
}

// sortedCopy returns a sorted copy of `s` so error messages
// are deterministic across calls (the underlying maps /
// JSON arrays do not preserve order). Used ONLY for error
// formatting -- the authz decision itself is order-
// independent.
func sortedCopy(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	sort.Strings(out)
	return out
}

// AdmittedGroups is a debug helper that returns the
// intersection of `caller` and `required` (the groups that
// would admit the caller). Used by the gateway log for the
// audit trail when a request is admitted, and by tests to
// assert the matching group set without re-parsing the
// authz error message.
func AdmittedGroups(caller, required []string) []string {
	if len(caller) == 0 || len(required) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(required))
	for _, g := range required {
		want[g] = struct{}{}
	}
	var out []string
	for _, g := range caller {
		if _, ok := want[g]; ok {
			out = append(out, g)
		}
	}
	return out
}

// joinGroupList formats a group set for the WWW-Authenticate
// header (`error_description` parameter). Joins with `","`
// after RFC 7235 quoting rules. Used by the gateway's 403
// response so the caller's tooling can extract the missing
// scope from the standard challenge header.
func joinGroupList(groups []string) string {
	if len(groups) == 0 {
		return ""
	}
	return strings.Join(sortedCopy(groups), ",")
}
