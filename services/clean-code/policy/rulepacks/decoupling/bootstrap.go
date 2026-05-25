// Bootstrap is the canonical startup hook that materialises
// the Stage 5.6 decoupling rulepacks into a clean-coded
// deployment's persistent stores.
//
// The implementation-plan Stage 5.6 brief (line 536) lists
// the criterion "Signed and loaded as `pack='decoupling'`
// rule_packs" and the e2e scenario `decoupling-loads` ("Given
// the three decoupling rulepack files, When the Steward
// loads them, Then `pack='decoupling'` rule_packs exist with
// parsed predicates"). [Bootstrap] is the call site that
// realises both: it seeds the four canonical Threshold rows
// (`thresholds.go`) into `clean_code.threshold` and then
// invokes the Stage 5.2 `policy.publish_rulepack` verb once
// per YAML file, persisting the [steward.RulePack] +
// [steward.Rule] rows the e2e scenario asserts on.
//
// # Idempotency
//
// The Stage 5.2 verb `policy.publish_rulepack` refuses a
// `(pack_id, version)` collision with [steward.ErrDuplicateRulePack]
// (architecture Sec 5.3.2). [Bootstrap] treats that sentinel
// as the "already bootstrapped" no-op outcome -- the typical
// production wiring runs Bootstrap on every clean-coded
// startup, and a second run MUST be observable as
// `inserted == 0 / unchanged` rather than an error. The
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
package decoupling

import (
	"context"
	"errors"
	"fmt"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/steward"
)

// BootstrapResult summarises what [Bootstrap] persisted on
// the just-finished call. Distinct from the input list
// length because Bootstrap is idempotent: a re-run reports
// `InsertedThresholds == 0` and `PublishedPacks == 0` if
// every row was already present.
type BootstrapResult struct {
	// InsertedThresholds is the count of [steward.Threshold]
	// rows newly inserted (excluding skips due to
	// idempotency). On the first call against a fresh store
	// this equals `len(ListCanonicalThresholds())` (4).
	InsertedThresholds int

	// PublishedPacks is the count of [steward.RulePack] rows
	// newly persisted (excluding skips due to
	// [steward.ErrDuplicateRulePack]). On the first call
	// against a fresh store this equals 3 (cycles, coupling,
	// duplication).
	PublishedPacks int

	// PublishedRules is the count of [steward.Rule] rows
	// newly persisted as the sum of the per-pack
	// `len(req.Rules)`. On the first call this equals 5:
	// 1 (cycles) + 3 (coupling) + 1 (duplication).
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
//  1. SeedThresholds(ctx, store) -- inserts the four
//     canonical decoupling Threshold rows if absent.
//
//  2. LoadAll() -- parses + validates the three embedded
//     YAML rulepacks.
//
//  3. For each loaded pack, call
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
func Bootstrap(ctx context.Context, pol *steward.Steward, store steward.Store) (BootstrapResult, error) {
	if pol == nil {
		return BootstrapResult{}, errors.New("decoupling.Bootstrap: steward.Steward is required")
	}
	if store == nil {
		return BootstrapResult{}, errors.New("decoupling.Bootstrap: steward.Store is required")
	}
	var result BootstrapResult

	inserted, err := SeedThresholds(ctx, store)
	if err != nil {
		return result, fmt.Errorf("decoupling.Bootstrap: SeedThresholds: %w", err)
	}
	result.InsertedThresholds = len(inserted)

	packs, err := LoadAll()
	if err != nil {
		return result, fmt.Errorf("decoupling.Bootstrap: LoadAll: %w", err)
	}
	for _, p := range packs {
		req, err := p.ToPublishRulepackRequest()
		if err != nil {
			return result, fmt.Errorf("decoupling.Bootstrap: %s: ToPublishRulepackRequest: %w", p.Filename, err)
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
			return result, fmt.Errorf("decoupling.Bootstrap: %s: PublishRulepack(%s, v%d): %w",
				p.Filename, req.PackID, req.Version, err)
		}
		result.PublishedPacks++
		result.PublishedRules += len(req.Rules)
		result.Packs = append(result.Packs, PublishedPackRef{PackID: req.PackID, Version: req.Version})
	}
	return result, nil
}
