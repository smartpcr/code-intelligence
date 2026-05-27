package solid

import (
	"context"
	"errors"
	"testing"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// newWiredSteward constructs a fully-wired [steward.Steward]
// against an in-memory KMS + Store. Mirrors the composition
// root startup -- `keys.Build(KMSProvider=in-memory,
// MintFirstKeyIfEmpty=true)` + `steward.New(Store=InMemoryStore,
// Signer=keys.Manager)`.
func newWiredSteward(t *testing.T) (*steward.Steward, steward.Store) {
	t.Helper()
	ctx := context.Background()
	res, err := keys.Build(ctx, keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	t.Cleanup(res.Close)
	store := steward.NewInMemoryStore()
	st, err := steward.New(steward.Config{
		Store:  store,
		Signer: res.Manager,
	})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	return st, store
}

// TestBootstrap_PublishesFivePacksAndNineRules is the
// implementation-plan Stage 5.5 e2e scenario
// `solid-rulepacks-load`: "Given the five SOLID rulepack
// files, When the Steward loads them, Then `pack='solid'`
// rule_packs exist with parsed predicates."
//
// We construct a real [steward.Steward] against an
// in-memory KMS+Store, invoke [Bootstrap], and assert that
// every canonical pack + rule landed in the store.
func TestBootstrap_PublishesFivePacksAndNineRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, store := newWiredSteward(t)

	result, err := Bootstrap(ctx, st)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Counters.
	if result.PublishedPacks != 5 {
		t.Errorf("PublishedPacks = %d; want 5", result.PublishedPacks)
	}
	// srp=2, ocp=2, lsp=2, isp=1, dip=2 -> 9 rules.
	if result.PublishedRules != 9 {
		t.Errorf("PublishedRules = %d; want 9", result.PublishedRules)
	}

	// Each canonical pack is now in the store.
	for _, want := range []PublishedPackRef{
		{PackID: "solid.srp", Version: 1},
		{PackID: "solid.ocp", Version: 1},
		{PackID: "solid.lsp", Version: 1},
		{PackID: "solid.isp", Version: 1},
		{PackID: "solid.dip", Version: 1},
	} {
		got, ok, err := store.GetRulePack(ctx, want.PackID, want.Version)
		if err != nil {
			t.Errorf("GetRulePack(%s, %d): %v", want.PackID, want.Version, err)
			continue
		}
		if !ok {
			t.Errorf("GetRulePack(%s, %d): not present after Bootstrap", want.PackID, want.Version)
			continue
		}
		if got.PackID != want.PackID || got.Version != want.Version {
			t.Errorf("GetRulePack returned (%s, %d); want (%s, %d)",
				got.PackID, got.Version, want.PackID, want.Version)
		}
	}

	// The Rule rows reference the parsed predicate text (so
	// the "rule_packs exist with parsed predicates" clause
	// of the scenario is observable end-to-end).
	for _, packID := range []string{"solid.srp", "solid.ocp", "solid.lsp", "solid.isp", "solid.dip"} {
		rules, err := store.ListRulesForPack(ctx, packID)
		if err != nil {
			t.Errorf("ListRulesForPack(%s): %v", packID, err)
			continue
		}
		if len(rules) == 0 {
			t.Errorf("ListRulesForPack(%s): no rules persisted", packID)
		}
		for _, r := range rules {
			if r.PredicateDSL == "" {
				t.Errorf("%s rule_id=%s: predicate_dsl empty in persisted row", packID, r.RuleID)
			}
			// Parse the persisted predicate text -- this
			// is the "parsed predicates" assertion of the
			// scenario.
			if _, err := dsl.Parse(r.PredicateDSL); err != nil {
				t.Errorf("%s rule_id=%s: persisted predicate fails to parse: %v",
					packID, r.RuleID, err)
			}
		}
	}
}

// TestBootstrap_IsIdempotent pins that a second Bootstrap
// against an already-bootstrapped store returns
// `PublishedPacks=0, PublishedRules=0` and nil error -- the
// "already converged" outcome the composition root depends
// on for safe re-runs.
func TestBootstrap_IsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := newWiredSteward(t)
	if _, err := Bootstrap(ctx, st); err != nil {
		t.Fatalf("Bootstrap first call: %v", err)
	}
	result, err := Bootstrap(ctx, st)
	if err != nil {
		t.Fatalf("Bootstrap second call: %v", err)
	}
	if result.PublishedPacks != 0 || result.PublishedRules != 0 {
		t.Errorf("second Bootstrap should be a no-op; got %+v", result)
	}
	if len(result.Packs) != 0 {
		t.Errorf("second Bootstrap result.Packs = %v; want empty", result.Packs)
	}
}

// TestBootstrap_RefusesWithoutSigningKey pins that a Steward
// without an active signing key refuses the publish step
// (matching the Stage 5.2 contract that every write verb
// runs the `checkSigningKey` precondition). This is the
// operator-facing failure mode if the KMS bootstrap step
// never minted a key.
func TestBootstrap_RefusesWithoutSigningKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	res, err := keys.Build(ctx, keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: false, // explicit: no key
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	t.Cleanup(res.Close)
	store := steward.NewInMemoryStore()
	st, err := steward.New(steward.Config{Store: store, Signer: res.Manager})
	if err != nil {
		t.Fatalf("steward.New: %v", err)
	}
	if _, err := Bootstrap(ctx, st); !errors.Is(err, steward.ErrNoActiveSigningKey) {
		t.Errorf("Bootstrap without signing key: err=%v; want ErrNoActiveSigningKey", err)
	}
}

// TestBootstrap_PredicatesCompileAgainstStore pins the
// stronger acceptance: every persisted Rule's predicate text
// COMPILES (parse + bind with a nil resolver, since SOLID v1
// uses pure literal predicates). Compile failure here would
// mean the YAML carries a typed predicate the parser would
// later reject at evaluator startup; either way the
// `solid-rulepacks-load` scenario's "with parsed predicates"
// clause is broken.
func TestBootstrap_PredicatesCompileAgainstStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, store := newWiredSteward(t)
	if _, err := Bootstrap(ctx, st); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	for _, packID := range []string{"solid.srp", "solid.ocp", "solid.lsp", "solid.isp", "solid.dip"} {
		rules, err := store.ListRulesForPack(ctx, packID)
		if err != nil {
			t.Fatalf("ListRulesForPack(%s): %v", packID, err)
		}
		for _, r := range rules {
			if _, err := dsl.Compile(r.PredicateDSL, nil); err != nil {
				t.Errorf("%s rule_id=%s: dsl.Compile(%q): %v",
					packID, r.RuleID, r.PredicateDSL, err)
			}
		}
	}
}

// TestBootstrap_PublishesAllExpectedPackRefs pins that the
// returned BootstrapResult.Packs slice carries exactly the
// five canonical (pack_id, version=1) pairs, in deterministic
// order matching the LoadAll filename order
// (alphabetical: dip, isp, lsp, ocp, srp).
func TestBootstrap_PublishesAllExpectedPackRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, _ := newWiredSteward(t)
	result, err := Bootstrap(ctx, st)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	want := []PublishedPackRef{
		{PackID: "solid.dip", Version: 1},
		{PackID: "solid.isp", Version: 1},
		{PackID: "solid.lsp", Version: 1},
		{PackID: "solid.ocp", Version: 1},
		{PackID: "solid.srp", Version: 1},
	}
	if len(result.Packs) != len(want) {
		t.Fatalf("Packs len = %d; want %d (Packs=%v)", len(result.Packs), len(want), result.Packs)
	}
	for i, w := range want {
		if result.Packs[i] != w {
			t.Errorf("Packs[%d] = %+v; want %+v", i, result.Packs[i], w)
		}
	}
}

// TestBootstrap_NilSteward returns a clear error rather than
// panicking. Belt-and-braces against a future caller that
// forgets to wire the dependency.
func TestBootstrap_NilSteward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := Bootstrap(ctx, nil); err == nil {
		t.Errorf("Bootstrap(nil steward): nil err; want non-nil")
	}
}
