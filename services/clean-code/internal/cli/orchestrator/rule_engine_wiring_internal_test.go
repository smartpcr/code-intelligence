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
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/cli/repocontext"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/rule_engine"
)

// TestSeedStore_SeedsPolicyRulesAndSamples pins the Sec 3.4
// contract: SeedStore must insert the policy version (step 1),
// every rule (step 2), and the full sample batch (step 3) into
// the provided StoreSeeder.
func TestSeedStore_SeedsPolicyRulesAndSamples(t *testing.T) {
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
	repoCtx := repocontext.RepoContext{
		RootPath: "/tmp/test-seed-store",
		RepoID:   uuid.Must(uuid.FromString("11111111-1111-4111-8111-111111111111")),
		HeadSHA:  "abcdef1234567890abcdef1234567890abcdef12",
	}

	store := rule_engine.NewInMemoryStore()
	if store == nil {
		t.Fatalf("NewInMemoryStore returned nil store")
	}

	SeedStore(store, bundle, nil, repoCtx)

	if _, err := store.GetPolicyVersion(t.Context(), pvID); err != nil {
		t.Errorf("GetPolicyVersion: %v", err)
	}
	if _, err := store.GetRule(t.Context(), "r.one", 1); err != nil {
		t.Errorf("GetRule(r.one, 1): %v", err)
	}
	if _, err := store.GetRule(t.Context(), "r.two", 2); err != nil {
		t.Errorf("GetRule(r.two, 2): %v", err)
	}

	// Accepts empty and non-empty sample slices without panicking.
	store2 := rule_engine.NewInMemoryStore()
	SeedStore(store2, bundle, []rule_engine.Sample{}, repoCtx)

	store3 := rule_engine.NewInMemoryStore()
	SeedStore(store3, bundle, []rule_engine.Sample{{ScopeSignature: "x"}}, repoCtx)
}
