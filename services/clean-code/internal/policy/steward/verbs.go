package steward

import "fmt"

// Canonical verb names per tech-spec Sec 8.5 lines 963-970 +
// architecture Sec 6.5. These are the ONLY `policy.*` write
// verb names registered on the v1 surface. A future stage may
// add NEW canonical verbs (e.g. a hypothetical `policy.retire`)
// by appending to this list; rulepack lifecycle CANNOT grow new
// verbs because the architecture Sec 1.5.1 row 4 and tech-spec
// Sec 8.5 note both pin `policy.publish_rulepack` as the SOLE
// rulepack writer.
const (
	// VerbPolicyPublish is the canonical name of the policy
	// publish verb. Note the dot-separated namespace
	// (`policy.publish`, NOT `policy_publish` or
	// `Policy.Publish`).
	VerbPolicyPublish = "policy.publish"
	// VerbPolicyActivate is the canonical name of the
	// activation verb.
	VerbPolicyActivate = "policy.activate"
	// VerbPolicyPublishRulepack is the canonical name of the
	// rulepack publish verb. Note the underscore between
	// `publish` and `rulepack` (`policy.publish_rulepack`,
	// NOT `policy.publish-rulepack` or
	// `policy.publishRulepack`) -- pinned at tech-spec Sec
	// 8.5 lines 963-970 verbatim.
	VerbPolicyPublishRulepack = "policy.publish_rulepack"
)

// canonicalVerbs is the immutable closed set of `policy.*`
// write-verb names. Returned (defensively copied) by
// [Registry.Verbs]. The order matches the architecture Sec 6.5
// listing.
//
// Kept unexported so importing packages cannot mutate the
// backing array (e.g. `steward.CanonicalVerbs[0] = "evil"`),
// which would otherwise leave [canonicalVerbSet] -- built once
// at init -- in an inconsistent state relative to
// [Registry.Verbs]. External callers must go through
// [Registry.Verbs] (read) or [Registry.Lookup] (membership).
var canonicalVerbs = []string{
	VerbPolicyPublish,
	VerbPolicyActivate,
	VerbPolicyPublishRulepack,
}

// canonicalVerbSet is the same closed set materialised as a
// map for O(1) lookup. Built once at package init from
// [canonicalVerbs] so the two views can never drift -- safe
// because [canonicalVerbs] is unexported and cannot be
// reassigned or mutated from outside this package.
var canonicalVerbSet = func() map[string]struct{} {
	out := make(map[string]struct{}, len(canonicalVerbs))
	for _, v := range canonicalVerbs {
		out[v] = struct{}{}
	}
	return out
}()

// Registry is the in-process witness of the canonical
// `policy.*` verb surface. The Stage 5.2 "canonical-rulepack-
// verb-name" scenario pins the contract:
//
//   - `Verbs()` returns exactly the 3-element closed set.
//
//   - `Lookup(name)` returns the canonical name on hit and
//     [ErrUnimplementedVerb] on miss. Specifically, the
//     historical drafts `policy.rulepack.add` and
//     `policy.rulepack.remove` MUST resolve to
//     [ErrUnimplementedVerb] -- the gRPC `UNIMPLEMENTED`
//     semantic from tech-spec Sec 8.5.
//
// Registry has zero state -- a `var R Registry` is a valid
// instance. We make it a type (rather than free functions) so
// tests can shadow it via dependency injection if a future
// stage needs a per-deployment override.
type Registry struct{}

// Verbs returns the canonical closed set of `policy.*` write
// verb names. The returned slice is a defensive copy; callers
// may not mutate it.
func (Registry) Verbs() []string {
	out := make([]string, len(canonicalVerbs))
	copy(out, canonicalVerbs)
	return out
}

// Lookup returns `name` verbatim when it is in the canonical
// set, or [ErrUnimplementedVerb] (wrapped with the queried
// name) otherwise.
//
// The Stage 5.2 contract pins these explicit rejections:
//
//   - `policy.rulepack.add` -> [ErrUnimplementedVerb]
//   - `policy.rulepack.remove` -> [ErrUnimplementedVerb]
//   - `policy.override` -> [ErrUnimplementedVerb] (architecture
//     Sec 6.5 explicit note: operator mute / unmute is the
//     Management surface verb `mgmt.override`, never a
//     `policy.*` verb)
//
// Any other unknown name -- `policy.foo`, `mgmt.publish`, the
// empty string -- also returns [ErrUnimplementedVerb].
func (Registry) Lookup(name string) (string, error) {
	if _, ok := canonicalVerbSet[name]; ok {
		return name, nil
	}
	return "", fmt.Errorf("%w: %q (canonical set: %v)", ErrUnimplementedVerb, name, canonicalVerbs)
}
