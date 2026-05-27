// Bootstrap is the canonical startup hook that materialises
// the Stage 5.5 SOLID rulepacks into a clean-coded
// deployment's persistent stores.
//
// The implementation-plan Stage 5.5 brief (line 517) lists
// the criterion "Each rulepack is signed and ingested via
// `policy.publish_rulepack` at startup if absent (tech-spec
// Sec 8.5 lines 963-970 canonical verb name)". [Bootstrap]
// is the call site that realises this: it invokes the Stage
// 5.2 `policy.publish_rulepack` verb once per loaded YAML
// pack, persisting the [steward.RulePack] + [steward.Rule]
// rows the e2e scenario `solid-rulepacks-load` asserts on.
//
// # Idempotency
//
// The Stage 5.2 verb `policy.publish_rulepack` refuses a
// `(pack_id, version)` collision with [steward.ErrDuplicateRulePack]
// (architecture Sec 5.3.2). [Bootstrap] treats that sentinel
// as the "already bootstrapped" no-op outcome -- the typical
// production wiring runs Bootstrap on every clean-coded
// startup, and a second run MUST be observable as
// `PublishedPacks == 0 / unchanged` rather than an error. The
// duplicate-rule sentinel ([steward.ErrDuplicateRule]) is
// likewise swallowed so a partial prior bootstrap (some
// packs present, some not) converges to "all present" on the
// next call without manual intervention.
//
// # Required precondition: signing key
//
// [steward.Steward.PublishRulepack] runs the
// `checkSigningKey` precondition before persisting. Bootstrap
// callers MUST therefore construct the Steward with a valid
// [steward.Signer] (a real [keys.Manager] with at least one
// active key); a Steward constructed in scaffold mode (no
// signer wired) will refuse with
// [steward.ErrNoActiveSigningKey], which Bootstrap surfaces
// to the caller verbatim so the operator sees a precise
// "signing key missing" diagnostic.
//
// # No Threshold seeding step
//
// Unlike the Stage 5.6 decoupling family ([decoupling.Bootstrap]
// which calls [decoupling.SeedThresholds] before publishing the
// rulepacks), the SOLID family ships with literal numeric
// cut-offs in the predicate text (e.g. `value >= 10` in
// `srp.yaml`). No [steward.Threshold] rows are referenced, so
// Bootstrap does NOT seed the threshold catalogue here.
// Operators re-tune by editing the literal in the YAML and
// publishing the pack at `version=2`.
package solid

import (
	"context"
	"errors"
	"fmt"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// BootstrapResult summarises what [Bootstrap] persisted on
// the just-finished call. Distinct from the input list
// length because Bootstrap is idempotent: a re-run reports
// `PublishedPacks == 0` if every row was already present.
type BootstrapResult struct {
	// PublishedPacks is the count of [steward.RulePack] rows
	// newly persisted (excluding skips due to
	// [steward.ErrDuplicateRulePack]). On the first call
	// against a fresh store this equals the number of YAML
	// files in this package (5: srp, ocp, lsp, isp, dip).
	PublishedPacks int

	// PublishedRules is the count of [steward.Rule] rows
	// newly persisted as the sum of the per-pack
	// `len(req.Rules)`. On the first call this equals 9:
	// 2 (srp) + 2 (ocp) + 2 (lsp) + 1 (isp) + 2 (dip). The
	// LSP pack carries TWO rules per Stage 5.5 brief: the
	// `depth_of_inheritance_high` class-scoped DIT rule AND
	// the `override_violation` method-scoped rule that
	// consumes the `lsp_violation` 0/1 indicator emitted by
	// the Stage 2.4 `recipes/lsp_violation.go` recipe.
	PublishedRules int

	// Packs is the list of (pack_id, version) pairs that
	// were published on THIS call. Empty when every pack
	// was already present (idempotent re-run).
	Packs []PublishedPackRef
}

// PublishedPackRef pins a `(pack_id, version)` pair Bootstrap
// just persisted. Used by callers (and tests) to assert
// "exactly these packs landed in the store on this call".
type PublishedPackRef struct {
	PackID  string
	Version int
}

// Bootstrap is the canonical wiring step the composition
// root (`cmd/clean-coded/main.go::run()`, gated on
// `signer != nil`) invokes on clean-coded startup. The steps:
//
//  1. LoadAll() -- parses + validates the five embedded
//     YAML rulepacks.
//
//  2. For each loaded pack, call
//     `pack.ToPublishRulepackRequest()` then
//     `steward.PublishRulepack(req)` -- swallowing
//     [steward.ErrDuplicateRulePack] / [steward.ErrDuplicateRule]
//     as the idempotent "already done" outcome.
//
// On any error other than the duplicate sentinels, Bootstrap
// returns immediately with the partial [BootstrapResult]
// the caller can use to log how far the bootstrap got. The
// caller MAY safely retry; the idempotency guarantee
// converges to "all present" on a successful retry.
//
// The `pol` Steward parameter MUST be constructed with a
// valid Signer; see the package doc for the rationale.
func Bootstrap(ctx context.Context, pol *steward.Steward) (BootstrapResult, error) {
	if pol == nil {
		return BootstrapResult{}, errors.New("solid.Bootstrap: steward.Steward is required")
	}
	var result BootstrapResult

	packs, err := LoadAll()
	if err != nil {
		return result, fmt.Errorf("solid.Bootstrap: LoadAll: %w", err)
	}
	for _, p := range packs {
		req, err := p.ToPublishRulepackRequest()
		if err != nil {
			return result, fmt.Errorf("solid.Bootstrap: %s: ToPublishRulepackRequest: %w", p.Filename, err)
		}
		_, _, err = pol.PublishRulepack(ctx, req)
		if err != nil {
			if errors.Is(err, steward.ErrDuplicateRulePack) || errors.Is(err, steward.ErrDuplicateRule) {
				// Idempotent re-bootstrap. The pack /
				// rules are already persisted; nothing
				// to add to the result counters for
				// THIS pack.
				continue
			}
			return result, fmt.Errorf("solid.Bootstrap: %s: PublishRulepack(%s, v%d): %w",
				p.Filename, req.PackID, req.Version, err)
		}
		result.PublishedPacks++
		result.PublishedRules += len(req.Rules)
		result.Packs = append(result.Packs, PublishedPackRef{PackID: req.PackID, Version: req.Version})
	}
	return result, nil
}
