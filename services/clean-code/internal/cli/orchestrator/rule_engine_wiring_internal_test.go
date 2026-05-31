// -----------------------------------------------------------------------
// <copyright file="rule_engine_wiring_internal_test.go" company="Microsoft Corp.">
//     Copyright (c) Microsoft Corp. All rights reserved.
// </copyright>
// -----------------------------------------------------------------------

package orchestrator

import (
	"testing"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/devpolicy"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// TestLoadStoreHelper_BriefSignatureSeedsPolicyAndRules
// pins the brief-shape contract:
//
//	loadStore(bundle devpolicy.Bundle, samples []rule_engine.Sample) *rule_engine.InMemoryStore
//
// Steps 1 and 2 from `architecture.md` Sec 3.4 (policy
// version + rules) MUST be observable on the returned
// store. Step 3 (samples insertion) is intentionally
// deferred to the exported [LoadStore] wrapper because it
// requires `repoCtx` for the `(repoID, sha)` coordinates,
// so this test only asserts that `samples` is accepted
// (not lost) and that the seeding side-effects happened.
func TestLoadStoreHelper_BriefSignatureSeedsPolicyAndRules(t *testing.T) {
	t.Parallel()

	pvID := uuid.Must(uuid.FromString("eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"))
	bundle := devpolicy.Bundle{
		PolicyVersion: steward.PolicyVersion{
			PolicyVersionID: pvID,
			Name:            "cleanc-dev-policy",
		},
		Rules: []steward.Rule{
			{RuleID: "r.one", Version: 1, PackID: "base", PredicateDSL: "metric_kind == 'loc' AND value >= 1", SeverityDefault: steward.SeverityWarn},
			{RuleID: "r.two", Version: 2, PackID: "base", PredicateDSL: "metric_kind == 'loc' AND value >= 2", SeverityDefault: steward.SeverityWarn},
		},
	}

	// Brief-shape call: no repoCtx, no error return.
	var fn func(devpolicy.Bundle, []rule_engine.Sample) *rule_engine.InMemoryStore = loadStore
	store := fn(bundle, nil)
	if store == nil {
		t.Fatalf("loadStore returned nil store")
	}

	if _, err := store.GetPolicyVersion(t.Context(), pvID); err != nil {
		t.Errorf("GetPolicyVersion: %v", err)
	}
	if _, err := store.GetRule(t.Context(), "r.one", 1); err != nil {
		t.Errorf("GetRule(r.one, 1): %v", err)
	}
	if _, err := store.GetRule(t.Context(), "r.two", 2); err != nil {
		t.Errorf("GetRule(r.two, 2): %v", err)
	}

	// Accepts non-nil empty and non-empty `samples` too --
	// the helper must not panic on either shape, even
	// though it does not insert them itself.
	if got := loadStore(bundle, []rule_engine.Sample{}); got == nil {
		t.Errorf("loadStore(bundle, []) returned nil store")
	}
	if got := loadStore(bundle, []rule_engine.Sample{{ScopeSignature: "x"}}); got == nil {
		t.Errorf("loadStore(bundle, [{...}]) returned nil store")
	}
}
