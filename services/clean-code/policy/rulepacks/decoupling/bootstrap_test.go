package decoupling

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/dsl"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/keys"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// newWiredSteward constructs a fully-wired [steward.Steward]
// against an in-memory KMS + Store. Mirrors the
// scaffold-mode startup the composition root performs --
// `keys.Build(KMSProvider=in-memory, MintFirstKeyIfEmpty=true)`
// + `steward.New(Store=InMemoryStore, Signer=keys.Manager)`.
//
// Returns the Steward + Store + a t.Cleanup() that releases
// any factory-owned resources (the in-memory KMS does not
// hold OS handles, but Build still registers a Close hook
// for symmetry with the LocalSealedKMS path).
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

// expectedThresholdUUIDStrings is the literal pinning table
// every Stage 5.6 YAML file embeds verbatim. Drift here OR in
// any of (a) `thresholds.go`'s Namespace seed, (b) the metric_kind
// labels passed to uuid.NewV5, (c) the YAML's
// `threshold('...')` literals, will trip [TestCanonicalThresholdIDs_MatchYAML].
var expectedThresholdUUIDStrings = map[string]string{
	"fan_in":                   "1e105766-3e02-5b49-b5be-49a787fd8e04",
	"fan_out":                  "d3a40d02-4b95-54c6-98fa-22793b425346",
	"coupling_between_objects": "fd6a0f05-ecbf-56cb-9e97-e6a83cc6a13e",
	"duplication_ratio":        "04d9f035-5183-5cb7-86bb-b582f85d3241",
}

// TestNamespace_Pinned pins the v5 namespace UUID derived
// from the literal seed string. A future drift on the seed
// would silently shift every threshold UUID; this test
// catches it before bootstrap publishes a mismatched
// rulepack against unrelated Threshold rows.
func TestNamespace_Pinned(t *testing.T) {
	t.Parallel()
	const wantStr = "81be548a-4498-5b6b-a564-c452103a7703"
	want, err := uuid.FromString(wantStr)
	if err != nil {
		t.Fatalf("uuid.FromString(%q): %v", wantStr, err)
	}
	if Namespace != want {
		t.Errorf("decoupling.Namespace = %s; want %s -- the v5 seed string drifted",
			Namespace, want)
	}
}

// TestCanonicalThresholdIDs_MatchYAML pins each canonical
// Threshold UUID against the literal embedded in the YAML
// rulepacks. Drift on EITHER side (a Go-side rename to the
// uuid.NewV5 seed name, or a manual edit to the YAML
// `threshold('...')` text) trips this test.
func TestCanonicalThresholdIDs_MatchYAML(t *testing.T) {
	t.Parallel()
	cases := []struct {
		metricKind string
		got        uuid.UUID
	}{
		{"fan_in", FanInThresholdID},
		{"fan_out", FanOutThresholdID},
		{"coupling_between_objects", CBOThresholdID},
		{"duplication_ratio", DuplicationRatioThresholdID},
	}
	// Phase 1: Go constant matches the pinned string literal.
	for _, c := range cases {
		want, err := uuid.FromString(expectedThresholdUUIDStrings[c.metricKind])
		if err != nil {
			t.Fatalf("uuid.FromString(%q): %v", expectedThresholdUUIDStrings[c.metricKind], err)
		}
		if c.got != want {
			t.Errorf("%s: ThresholdID = %s; want %s", c.metricKind, c.got, want)
		}
	}
	// Phase 2: every pinned UUID appears verbatim in one of
	// the loaded YAML predicates. Catches a YAML rename that
	// would otherwise pass Phase 1 by accident.
	packs := loadAllOrFail(t)
	var allPredicates []string
	for _, p := range packs {
		for _, r := range p.Rules {
			allPredicates = append(allPredicates, r.PredicateDSL)
		}
	}
	combined := strings.Join(allPredicates, "\n")
	for kind, want := range expectedThresholdUUIDStrings {
		// The cycle_member metric has no threshold-UUID in
		// the YAML (cycles.yaml uses a literal `value > 0`
		// comparison, no threshold atom).
		if !strings.Contains(combined, want) {
			t.Errorf("expected UUID %s for metric_kind=%s NOT found in any YAML predicate:\n%s",
				want, kind, combined)
		}
	}
}

// TestListCanonicalThresholds_FreshCopy verifies the helper
// returns a fresh slice each call so caller mutation does
// not leak into the package's source of truth.
func TestListCanonicalThresholds_FreshCopy(t *testing.T) {
	t.Parallel()
	a := ListCanonicalThresholds()
	b := ListCanonicalThresholds()
	if len(a) != 4 {
		t.Fatalf("ListCanonicalThresholds: len = %d; want 4", len(a))
	}
	if len(a) != len(b) {
		t.Fatalf("two calls returned different lengths: %d vs %d", len(a), len(b))
	}
	// Mutate a; b MUST be unaffected.
	a[0].Value = 999
	if b[0].Value == 999 {
		t.Errorf("mutation of one returned slice leaked into a second call -- the helper is sharing backing storage")
	}
}

// TestResolver_ContainsCanonicalUUIDs pins that the
// [Resolver] returned by the package maps every canonical
// threshold UUID to a well-formed [dsl.Threshold] whose
// fields match the canonical row.
func TestResolver_ContainsCanonicalUUIDs(t *testing.T) {
	t.Parallel()
	r := Resolver()
	for _, want := range canonicalThresholds {
		got, err := r.Lookup(want.ThresholdID)
		if err != nil {
			t.Errorf("Resolver.Lookup(%s): %v", want.ThresholdID, err)
			continue
		}
		if got.ThresholdID != want.ThresholdID {
			t.Errorf("Lookup(%s): ThresholdID = %s; want %s",
				want.ThresholdID, got.ThresholdID, want.ThresholdID)
		}
		if got.MetricKind != want.MetricKind {
			t.Errorf("Lookup(%s): MetricKind = %q; want %q",
				want.ThresholdID, got.MetricKind, want.MetricKind)
		}
		if got.ScopeKind != want.ScopeKind {
			t.Errorf("Lookup(%s): ScopeKind = %q; want %q",
				want.ThresholdID, got.ScopeKind, want.ScopeKind)
		}
		if string(got.Op) != want.Op {
			t.Errorf("Lookup(%s): Op = %q; want %q",
				want.ThresholdID, got.Op, want.Op)
		}
		if got.Value != want.Value {
			t.Errorf("Lookup(%s): Value = %v; want %v",
				want.ThresholdID, got.Value, want.Value)
		}
	}
}

// TestSeedThresholds_PopulatesStore asserts the seeder
// inserts every canonical Threshold row into an empty
// Store on first call.
func TestSeedThresholds_PopulatesStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := steward.NewInMemoryStore()
	inserted, err := SeedThresholds(ctx, store)
	if err != nil {
		t.Fatalf("SeedThresholds: %v", err)
	}
	if len(inserted) != 4 {
		t.Errorf("SeedThresholds inserted %d rows; want 4", len(inserted))
	}
	for _, t0 := range canonicalThresholds {
		exists, err := store.ThresholdExists(ctx, t0.ThresholdID)
		if err != nil {
			t.Errorf("ThresholdExists(%s): %v", t0.ThresholdID, err)
		}
		if !exists {
			t.Errorf("ThresholdExists(%s) = false; want true (metric_kind=%s)",
				t0.ThresholdID, t0.MetricKind)
		}
	}
}

// TestSeedThresholds_Idempotent pins that a second call
// against a Store that already has the canonical rows is a
// no-op (no error, zero new insertions).
func TestSeedThresholds_Idempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := steward.NewInMemoryStore()
	if _, err := SeedThresholds(ctx, store); err != nil {
		t.Fatalf("SeedThresholds first call: %v", err)
	}
	inserted, err := SeedThresholds(ctx, store)
	if err != nil {
		t.Errorf("SeedThresholds second call: %v", err)
	}
	if len(inserted) != 0 {
		t.Errorf("idempotent re-seed inserted %d rows; want 0", len(inserted))
	}
}

// TestSeedThresholds_StampsCreatedAt is the iter-3 evaluator-2
// regression guard: every Threshold row SeedThresholds persists
// MUST carry a non-zero, recent, UTC `CreatedAt` timestamp so
// the audit trail and any "oldest seeded threshold" runbook
// query has a meaningful sort key.
//
// Why this matters: [SQLStore.InsertThreshold] (sql_store.go
// line 371) writes `t.CreatedAt.UTC()` VERBATIM into the
// `created_at` column -- if SeedThresholds passes a zero
// `time.Time` the DB row carries `0001-01-01 00:00:00 UTC`,
// which the operator cannot distinguish from a corrupted /
// uninitialised row. The InMemoryStore similarly stores the
// struct by value (store.go line 379), so the same flaw would
// surface in the test harness too.
//
// Mechanics:
//   - Capture `before = time.Now()` BEFORE the seed call.
//   - Capture `after  = time.Now()` AFTER the seed call.
//   - Assert every returned Threshold's CreatedAt is:
//     (a) NOT the zero value
//     (b) >= before AND <= after (sandwiched in the window)
//     (c) in UTC (Location() == time.UTC)
//   - Assert every returned row's CreatedAt is IDENTICAL --
//     SeedThresholds captures `now()` once and stamps every
//     row with the same instant so an operator sees the four
//     canonical rows as one atomic seeding event.
func TestSeedThresholds_StampsCreatedAt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := steward.NewInMemoryStore()
	before := time.Now().UTC()
	inserted, err := SeedThresholds(ctx, store)
	after := time.Now().UTC()
	if err != nil {
		t.Fatalf("SeedThresholds: %v", err)
	}
	if len(inserted) != 4 {
		t.Fatalf("SeedThresholds inserted %d rows; want 4 (every canonical row should have been new on a fresh store)", len(inserted))
	}
	// Padding: machines with low-resolution clocks may emit
	// identical Before/After values; allow a microsecond of
	// slack on either bound so a coincidentally-equal Now
	// inside SeedThresholds is not treated as out-of-window.
	lower := before.Add(-time.Microsecond)
	upper := after.Add(time.Microsecond)
	var first time.Time
	for i, row := range inserted {
		if row.CreatedAt.IsZero() {
			t.Errorf("inserted[%d] (metric_kind=%s) CreatedAt is the zero time; want a real timestamp",
				i, row.MetricKind)
		}
		if row.CreatedAt.Location() != time.UTC {
			t.Errorf("inserted[%d] (metric_kind=%s) CreatedAt.Location()=%v; want time.UTC",
				i, row.MetricKind, row.CreatedAt.Location())
		}
		if row.CreatedAt.Before(lower) || row.CreatedAt.After(upper) {
			t.Errorf("inserted[%d] (metric_kind=%s) CreatedAt=%v not within [%v, %v]",
				i, row.MetricKind, row.CreatedAt, lower, upper)
		}
		if i == 0 {
			first = row.CreatedAt
			continue
		}
		if !row.CreatedAt.Equal(first) {
			t.Errorf("inserted[%d] (metric_kind=%s) CreatedAt=%v; want identical to inserted[0].CreatedAt=%v (a single seed call should stamp every row with the same instant)",
				i, row.MetricKind, row.CreatedAt, first)
		}
	}
}

// TestBootstrap_PublishesThreePacksAndFiveRules is the
// implementation-plan Stage 5.6 e2e scenario
// `decoupling-loads`: "Given the three decoupling rulepack
// files, When the Steward loads them, Then
// `pack='decoupling'` rule_packs exist with parsed
// predicates."
//
// We construct a real [steward.Steward] against an
// in-memory KMS+Store, invoke [Bootstrap], and assert that
// every canonical pack + rule + threshold landed in the
// store.
func TestBootstrap_PublishesThreePacksAndFiveRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, store := newWiredSteward(t)

	result, err := Bootstrap(ctx, st, store)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Counters.
	if result.InsertedThresholds != 4 {
		t.Errorf("InsertedThresholds = %d; want 4", result.InsertedThresholds)
	}
	if result.PublishedPacks != 3 {
		t.Errorf("PublishedPacks = %d; want 3", result.PublishedPacks)
	}
	// cycles=1, coupling=3, duplication=1 -> 5 rules.
	if result.PublishedRules != 5 {
		t.Errorf("PublishedRules = %d; want 5", result.PublishedRules)
	}

	// Each canonical pack is now in the store.
	for _, want := range []PublishedPackRef{
		{PackID: "decoupling.coupling", Version: 1},
		{PackID: "decoupling.cycles", Version: 1},
		{PackID: "decoupling.duplication", Version: 1},
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
	for _, packID := range []string{"decoupling.coupling", "decoupling.cycles", "decoupling.duplication"} {
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
// `InsertedThresholds=0, PublishedPacks=0, PublishedRules=0`
// and nil error -- the "already converged" outcome the
// composition root depends on for safe re-runs.
func TestBootstrap_IsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, store := newWiredSteward(t)
	if _, err := Bootstrap(ctx, st, store); err != nil {
		t.Fatalf("Bootstrap first call: %v", err)
	}
	result, err := Bootstrap(ctx, st, store)
	if err != nil {
		t.Fatalf("Bootstrap second call: %v", err)
	}
	if result.InsertedThresholds != 0 || result.PublishedPacks != 0 || result.PublishedRules != 0 {
		t.Errorf("second Bootstrap should be a no-op; got %+v", result)
	}
}

// TestBootstrap_RefusesWithoutSigningKey pins that a Steward
// without an active signing key refuses the publish step
// (matching the Stage 5.2 contract every write verb runs the
// `checkSigningKey` precondition). This is the operator-
// facing failure mode if the KMS bootstrap step never minted
// a key.
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
	if _, err := Bootstrap(ctx, st, store); !errors.Is(err, steward.ErrNoActiveSigningKey) {
		t.Errorf("Bootstrap without signing key: err=%v; want ErrNoActiveSigningKey", err)
	}
}

// TestBootstrap_PredicatesCompileAgainstResolver pins the
// stronger acceptance: every persisted Rule's predicate
// text COMPILES (parse + bind) against the canonical
// decoupling [Resolver]. Compile failure here would mean
// the YAML carries a typo'd UUID or the canonical Threshold
// catalogue and the YAML have drifted; either way the
// `decoupling-loads` scenario's "with parsed predicates"
// clause is broken.
func TestBootstrap_PredicatesCompileAgainstResolver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, store := newWiredSteward(t)
	if _, err := Bootstrap(ctx, st, store); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	resolver := Resolver()
	for _, packID := range []string{"decoupling.coupling", "decoupling.cycles", "decoupling.duplication"} {
		rules, err := store.ListRulesForPack(ctx, packID)
		if err != nil {
			t.Fatalf("ListRulesForPack(%s): %v", packID, err)
		}
		for _, r := range rules {
			if _, err := dsl.Compile(r.PredicateDSL, resolver); err != nil {
				t.Errorf("%s rule_id=%s: dsl.Compile(%q): %v",
					packID, r.RuleID, r.PredicateDSL, err)
			}
		}
	}
}

// TestBootstrap_CouplingRuleFiresOnFanInAbove20 exercises the
// canonical fan_in rule end-to-end: compile its predicate
// against the resolver (so the threshold('<uuid>') atom
// binds to the canonical FanInThresholdID row), eval against
// a synthetic sample, and assert the rule fires for
// value=21 and does NOT fire for value=20.
func TestBootstrap_CouplingRuleFiresOnFanInAbove20(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "coupling.yaml")
	var fanInRule LoadedRuleSpec
	for _, r := range pack.Rules {
		if r.RuleID == "decoupling.fan_in_high" {
			fanInRule = r
			break
		}
	}
	if fanInRule.RuleID == "" {
		t.Fatalf("coupling.yaml: rule_id=decoupling.fan_in_high not found")
	}
	pred, err := dsl.Compile(fanInRule.PredicateDSL, Resolver())
	if err != nil {
		t.Fatalf("dsl.Compile(%q): %v", fanInRule.PredicateDSL, err)
	}
	cases := []struct {
		name   string
		value  float64
		wantOK bool
	}{
		{"below_threshold_does_not_fire", 15, false},
		{"equal_to_threshold_does_not_fire", 20, false},
		{"above_threshold_fires", 21, true},
		{"well_above_threshold_fires", 100, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := dsl.Sample{
				ScopeKind:     "class",
				MetricKind:    "fan_in",
				MetricVersion: 1,
				Value:         c.value,
				HasValue:      true,
				Pack:          "solid",
				Source:        "computed",
			}
			ok, err := pred.Eval(s)
			if err != nil {
				t.Fatalf("Eval(value=%v): %v", c.value, err)
			}
			if ok != c.wantOK {
				t.Errorf("Eval(value=%v) = %v; want %v", c.value, ok, c.wantOK)
			}
		})
	}
}

// TestBootstrap_DuplicationRuleFiresOnRatioAbove20pct
// exercises the canonical duplication rule end-to-end.
func TestBootstrap_DuplicationRuleFiresOnRatioAbove20pct(t *testing.T) {
	t.Parallel()
	pack := findPackByFilename(t, "duplication.yaml")
	if len(pack.Rules) != 1 {
		t.Fatalf("duplication.yaml: expected exactly one rule, got %d", len(pack.Rules))
	}
	rule := pack.Rules[0]
	pred, err := dsl.Compile(rule.PredicateDSL, Resolver())
	if err != nil {
		t.Fatalf("dsl.Compile(%q): %v", rule.PredicateDSL, err)
	}
	cases := []struct {
		name   string
		value  float64
		wantOK bool
	}{
		{"low_ratio_does_not_fire", 0.05, false},
		{"at_threshold_does_not_fire", 0.20, false},
		{"above_threshold_fires", 0.25, true},
		{"all_duplicated_fires", 1.0, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := dsl.Sample{
				ScopeKind:     "file",
				MetricKind:    "duplication_ratio",
				MetricVersion: 1,
				Value:         c.value,
				HasValue:      true,
				Pack:          "base",
				Source:        "computed",
			}
			ok, err := pred.Eval(s)
			if err != nil {
				t.Fatalf("Eval(value=%v): %v", c.value, err)
			}
			if ok != c.wantOK {
				t.Errorf("Eval(value=%v) = %v; want %v", c.value, ok, c.wantOK)
			}
		})
	}
}

// TestBootstrap_NilStewardOrStore returns a clear error
// rather than panicking. Belt-and-braces against a future
// caller that forgets to wire the dependency.
func TestBootstrap_NilStewardOrStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := steward.NewInMemoryStore()
	if _, err := Bootstrap(ctx, nil, store); err == nil {
		t.Errorf("Bootstrap(nil steward): nil err; want non-nil")
	}
	st, _ := newWiredSteward(t)
	if _, err := Bootstrap(ctx, st, nil); err == nil {
		t.Errorf("Bootstrap(nil store): nil err; want non-nil")
	}
}
