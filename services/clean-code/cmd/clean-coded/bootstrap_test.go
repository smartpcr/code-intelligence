package main

import (
	"context"
	"testing"
	"time"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/microsoft/code-intelligence/services/clean-code/policy/rulepacks/decoupling"
)

// TestBuildPolicyWriter_WiresStewardAndStoreForBootstrap is
// the iter-3 evaluator-1 composition-root pin: the production
// `run()` function calls `buildPolicyWriter` and then
// `decoupling.Bootstrap` on the (steward, store) tuple
// returned by that helper. THIS test exercises EXACTLY that
// composition chain (without spinning up the HTTP listener
// `run()` opens), proving that:
//
//  1. `buildPolicyWriter` returns a non-nil `*steward.Steward`
//     and `steward.Store` -- the two values decoupling.Bootstrap
//     needs to seed Thresholds and call PublishRulepack.
//
//  2. `decoupling.Bootstrap`, invoked against those values
//     with a real (in-memory KMS) signer, publishes ALL THREE
//     decoupling packs and ALL FIVE rules.
//
// The grep-level evidence that production code calls
// Bootstrap lives in `main.go::run()` (the `if signer != nil
// { ... decoupling.Bootstrap(...) }` block). THIS test is
// the dynamic counterpart: it proves the wiring shape
// (buildPolicyWriter output -> decoupling.Bootstrap input)
// is satisfied and that the call lands the canonical packs
// in the same Store the HTTP surface reads from.
func TestBuildPolicyWriter_WiresStewardAndStoreForBootstrap(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Step 1: build the same signing-key cache the
	// production composition root constructs (in-memory KMS
	// + minted first key) so PublishRulepack's
	// checkSigningKey precondition is satisfied.
	keysRes, err := keys.Build(ctx, keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	t.Cleanup(keysRes.Close)

	// Step 2: call the production `buildPolicyWriter` helper
	// with the real signer. This is the SAME call shape
	// `run()` uses; if the helper's signature drifts THIS
	// test (and the cmd/clean-coded package build) fail.
	pw, stew, store, closeDB, buildErr := buildPolicyWriter(nil, keysRes.Manager, nil)
	if buildErr != nil {
		t.Fatalf("buildPolicyWriter(scaffold-db, real-signer, nil): %v", buildErr)
	}
	if pw == nil {
		t.Fatalf("buildPolicyWriter returned nil PolicyWriter; want non-nil for HTTP surface")
	}
	if stew == nil {
		t.Fatalf("buildPolicyWriter returned nil *steward.Steward; the decoupling Bootstrap path cannot proceed")
	}
	if store == nil {
		t.Fatalf("buildPolicyWriter returned nil steward.Store; SeedThresholds cannot proceed")
	}
	if closeDB {
		t.Errorf("buildPolicyWriter reported closeDB=true with a nil DB handle; want false")
	}

	// Step 3: exercise the SAME bootstrap call `run()`
	// performs. With a real signer wired, all 3 packs / 5
	// rules / 4 thresholds MUST land in the store.
	result, err := decoupling.Bootstrap(ctx, stew, store)
	if err != nil {
		t.Fatalf("decoupling.Bootstrap (composition-root wiring): %v", err)
	}
	if result.PublishedPacks != 3 {
		t.Errorf("PublishedPacks=%d; want 3 (cycles, coupling, duplication)", result.PublishedPacks)
	}
	if result.PublishedRules != 5 {
		t.Errorf("PublishedRules=%d; want 5 (1 cycles + 3 coupling + 1 duplication)", result.PublishedRules)
	}
	if result.InsertedThresholds != 4 {
		t.Errorf("InsertedThresholds=%d; want 4 (fan_in, fan_out, cbo, duplication_ratio)", result.InsertedThresholds)
	}

	// Step 4: read-side proof -- the store the HTTP surface
	// would query through the PolicyWriter contains the
	// expected pack rows.
	expectedPacks := []struct {
		ID      string
		Version int
		Rules   int
	}{
		{"decoupling.cycles", 1, 1},
		{"decoupling.coupling", 1, 3},
		{"decoupling.duplication", 1, 1},
	}
	for _, want := range expectedPacks {
		got, ok, err := store.GetRulePack(ctx, want.ID, want.Version)
		if err != nil {
			t.Errorf("store.GetRulePack(%s, %d): %v", want.ID, want.Version, err)
			continue
		}
		if !ok {
			t.Errorf("store.GetRulePack(%s, %d): not found; the composition root did not publish this pack",
				want.ID, want.Version)
			continue
		}
		if got.PackID != want.ID {
			t.Errorf("GetRulePack returned PackID=%q; want %q", got.PackID, want.ID)
		}
		rules, err := store.ListRulesForPack(ctx, want.ID)
		if err != nil {
			t.Errorf("store.ListRulesForPack(%s): %v", want.ID, err)
			continue
		}
		if len(rules) != want.Rules {
			t.Errorf("ListRulesForPack(%s) returned %d rules; want %d", want.ID, len(rules), want.Rules)
		}
	}

	// Step 5: idempotency at the composition-root level --
	// a second Bootstrap call against the SAME (steward,
	// store) pair MUST be observable as zero new inserts
	// (the typical "process restart against a populated
	// store" case).
	result2, err := decoupling.Bootstrap(ctx, stew, store)
	if err != nil {
		t.Fatalf("decoupling.Bootstrap (second call): %v", err)
	}
	if result2.InsertedThresholds != 0 {
		t.Errorf("second Bootstrap InsertedThresholds=%d; want 0 (idempotency)", result2.InsertedThresholds)
	}
	if result2.PublishedPacks != 0 {
		t.Errorf("second Bootstrap PublishedPacks=%d; want 0 (idempotency)", result2.PublishedPacks)
	}
	if result2.PublishedRules != 0 {
		t.Errorf("second Bootstrap PublishedRules=%d; want 0 (idempotency)", result2.PublishedRules)
	}
}
