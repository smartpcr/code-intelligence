package steward

import (
	"context"
	"fmt"
	"strings"
)

// Override implements the `mgmt.override` write verb's Policy
// Steward delegate per architecture Sec 6.3 line 1357: the
// Management surface accepts the operator's mute / unmute
// request and delegates here, which appends the [Override]
// row in the Policy / rules sub-store (architecture Sec 5.3.6).
//
// Architecture Sec 1.5.1 row 5: "Operator mute / unmute uses
// the `mgmt.override` verb on the Management surface
// (Section 6.3); Management delegates to the Policy Steward,
// which appends the `Override` row. There is intentionally
// no `policy.override` verb on the Policy Steward surface."
//
// The Stage 5.3 brief pins three invariants this method
// encodes:
//
//  1. **No signing-key precondition.** Unlike the other three
//     Steward write verbs (Publish, Activate, PublishRulepack),
//     the override row carries NO signature column and the
//     operator MUST be able to mute a noisy / broken rule even
//     when the signing-key cache is unwired (the kill-switch
//     purpose, architecture Sec 4.6). Requiring a signing key
//     would invert the kill-switch contract -- the worst time
//     to deny an emergency mute is during a signing-key
//     outage.
//
//  2. **Append-only.** This method INSERTs exactly one row.
//     Unmute (`mute=false`) is also an INSERT (a NEW row that
//     supersedes a prior `mute=true` row by latest-row-wins).
//     Updates / deletes are structurally impossible because
//     [Store] has no UpdateOverride / DeleteOverride method.
//
//  3. **Reason required when mute=true.** Architecture Sec
//     5.3.6 line 1169 + migration 0003 CHECK constraint
//     `override_reason_required_when_muted`. The Steward
//     validator rejects whitespace-only reasons here so a
//     partial-init SQL writer never lands a row with a
//     logically empty reason that nonetheless passes the
//     NULL check.
//
// Returns [ErrInvalidOverride] for shape validation failures
// and [ErrUnknownRule] when the inbound `rule_id` does not
// reference any persisted rule_id lineage (the logical FK on
// `Override.rule_id` -> `Rule.rule_id` documented in
// architecture Sec 5.3.6 line 1166).
func (s *Steward) Override(ctx context.Context, req OverrideRequest) (Override, error) {
	if err := validateOverrideRequest(req); err != nil {
		return Override{}, err
	}
	// Enforce the logical FK from architecture Sec 5.3.6
	// line 1166 ("FK -> Rule.rule_id"). Migration 0003
	// lines 490-493 explicitly carry the FK as logical
	// (not SQL) because `rule` has the composite PK
	// `(rule_id, version)` -- so the writer enforces "some
	// version of this rule_id has been registered".
	ok, err := s.store.RuleExistsByID(ctx, req.RuleID)
	if err != nil {
		return Override{}, fmt.Errorf("steward.Override: rule existence check: %w", err)
	}
	if !ok {
		return Override{}, fmt.Errorf("%w: rule_id=%q", ErrUnknownRule, req.RuleID)
	}
	id, err := s.newID()
	if err != nil {
		return Override{}, fmt.Errorf("steward.Override: uuid: %w", err)
	}
	o := Override{
		OverrideID:  id,
		RuleID:      req.RuleID,
		ScopeFilter: req.ScopeFilter,
		Mute:        req.Mute,
		Reason:      req.Reason,
		ActorID:     req.ActorID,
		CreatedAt:   s.clock().UTC(),
	}
	if err := s.store.InsertOverride(ctx, o); err != nil {
		return Override{}, fmt.Errorf("steward.Override: insert: %w", err)
	}
	return o, nil
}

// LatestMatchingOverride is the gate-time read helper the
// evaluator surface calls to decide "is this candidate scope
// currently muted for this rule?". Delegates to
// [Store.LatestMatchingOverride] -- see its docstring for the
// glob-matching semantic on `candidate`.
//
// The Steward layer validates `candidate` up front and refuses
// nonsensical reads (empty repo_id / signature, unknown
// scope_kind) with [ErrInvalidCandidateScope]. The gate-time
// hot path SHOULD treat that error as a configuration bug, not
// fall through to "no mute" -- the evaluator should never ask
// the Steward to match against an empty candidate signature.
func (s *Steward) LatestMatchingOverride(ctx context.Context, ruleID string, candidate CandidateScope) (Override, bool, error) {
	if strings.TrimSpace(ruleID) == "" {
		return Override{}, false, fmt.Errorf("%w: rule_id must be non-empty", ErrInvalidCandidateScope)
	}
	if !candidate.IsValid() {
		return Override{}, false, fmt.Errorf("%w: repo_id=%q scope_kind=%q signature=%q",
			ErrInvalidCandidateScope, candidate.RepoID, candidate.ScopeKind, candidate.Signature)
	}
	return s.store.LatestMatchingOverride(ctx, ruleID, candidate)
}

// validateOverrideRequest enforces the shape contract on the
// `mgmt.override` payload before any persistence work. Refuses:
//
//   - empty rule_id;
//   - empty scope_filter.repo_id;
//   - scope_kind outside the canonical seven-value set;
//   - empty scope_signature_glob (use `"*"` for repo-wide);
//   - empty actor_id (the HTTP layer should have rejected the
//     missing OIDC subject upstream with a 401; the Steward
//     check is belt-and-braces);
//   - mute=true with whitespace-only reason.
func validateOverrideRequest(req OverrideRequest) error {
	if strings.TrimSpace(req.RuleID) == "" {
		return fmt.Errorf("%w: rule_id must be non-empty", ErrInvalidOverride)
	}
	if strings.TrimSpace(req.ScopeFilter.RepoID) == "" {
		return fmt.Errorf("%w: scope_filter.repo_id must be non-empty (v1 has no global-mute shape)", ErrInvalidOverride)
	}
	if !req.ScopeFilter.ScopeKind.IsValid() {
		return fmt.Errorf("%w: scope_filter.scope_kind=%q not in {repo,package,file,class,interface,method,block}",
			ErrInvalidOverride, req.ScopeFilter.ScopeKind)
	}
	if strings.TrimSpace(req.ScopeFilter.ScopeSignatureGlob) == "" {
		return fmt.Errorf("%w: scope_filter.scope_signature_glob must be non-empty (use \"*\" for the repo-wide wildcard)",
			ErrInvalidOverride)
	}
	if strings.TrimSpace(req.ActorID) == "" {
		return fmt.Errorf("%w: actor_id must be non-empty (OIDC subject)", ErrInvalidOverride)
	}
	if req.Mute && strings.TrimSpace(req.Reason) == "" {
		return fmt.Errorf("%w: reason is required when mute=true (architecture Sec 5.3.6 line 1169)", ErrInvalidOverride)
	}
	return nil
}
