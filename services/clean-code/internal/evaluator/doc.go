// Package evaluator hosts the verification gate that policy
// consumers run against every incoming signed policy bundle.
//
// Stage 5.1 ships only the signature-verification surface
// ([Gate.VerifyPolicy]); later stages add the rule-evaluation
// surface (the actual `evaluate(policy, payload) -> verdict`
// pipeline). They are deliberately decoupled: signature
// verification is a pure cryptographic check against the
// signing-key cache and has no dependency on the rule engine.
//
// Per the Stage 5.1 brief, the evaluator MUST accept both
// signing keys during the 24h rotation overlap window so the
// Policy Steward can publish a fresh bundle without a
// service-wide cutover. The implementation delegates to
// [keys.Manager.Verify] which itself walks every cached key
// inside its `[valid_from, valid_until)` window -- the overlap
// is encoded once in the manager and re-used here.
package evaluator
