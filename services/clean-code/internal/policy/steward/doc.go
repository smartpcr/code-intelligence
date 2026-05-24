// Package steward implements the Policy Steward write verbs
// (architecture Sec 3.11, Sec 6.5).
//
// The Stage 5.2 surface ships THREE canonical write verbs:
//
//   - `policy.publish(rulepack_set, refactor_weights) -> PolicyVersion`
//     appends an immutable [PolicyVersion] row signed by the
//     [keys.Manager]. The signature covers the canonical JSON
//     of `(rule_refs, threshold_refs, refactor_weights)` per
//     architecture Sec 5.3.3.
//
//   - `policy.activate(policy_version_id) -> PolicyActivation`
//     appends a [PolicyActivation] row. Activation is GLOBAL
//     per deployment (v1 single-tenant pin) -- the verb refuses
//     any caller-supplied `scope` field. Latest row by
//     `created_at` defines the active policy.
//
//   - `policy.publish_rulepack(rulepack) -> RulePack` appends
//     one [RulePack] row + N [Rule] rows in a single
//     transaction. Append-only.
//
// There is NO `policy.rulepack.add` and NO `policy.rulepack.remove`
// verb (tech-spec Sec 8.5: the only write verb for rulepack
// lifecycle is `policy.publish_rulepack`). The [Registry]
// returns [ErrUnimplementedVerb] for any non-canonical name --
// the gRPC `UNIMPLEMENTED` semantic the Stage 5.2
// canonical-rulepack-verb-name scenario pins.
//
// # Signing invariant
//
// All three verbs refuse to execute when the [keys.Manager]
// has no active signing key -- a scaffold-mode bring-up MUST
// NOT be able to publish unsigned policy state. The verb
// surface returns [ErrNoActiveSigningKey] (wrapped from
// [keys.ErrNoActiveKey]) when this precondition fails.
//
// # Canonical-JSON signing
//
// Signing the BYTES of the inbound JSON payload would not
// survive a PostgreSQL `jsonb` round-trip: `jsonb` strips
// whitespace, normalises numerics, and does not preserve key
// order. The Steward therefore canonicalises the payload
// (sorted keys, compact form) BEFORE signing and re-runs the
// same canonicalisation on read for verification. See
// [canonicalJSON] for the deterministic shape.
//
// # Append-only invariant
//
// The [Store] interface intentionally exposes only INSERT and
// SELECT verbs -- no Update / Delete methods. The DB-level
// `REVOKE UPDATE, DELETE` grants in Stage 1.5
// (`0004_roles.up.sql`) enforce the same contract at the
// storage layer; this package is the in-process witness.
package steward
