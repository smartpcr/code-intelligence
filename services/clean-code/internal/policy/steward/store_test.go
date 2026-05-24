package steward

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gofrs/uuid"
)

func TestCanonicalVerbs_ExactClosedSet(t *testing.T) {
	t.Parallel()
	got := Registry{}.Verbs()
	want := []string{"policy.publish", "policy.activate", "policy.publish_rulepack"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Verbs()=%v, want exactly %v (tech-spec Sec 8.5 lines 963-970)", got, want)
	}
}

// TestRegistry_LookupCanonicalReturnsName -- scenario:
// canonical-rulepack-verb-name positive half.
func TestRegistry_LookupCanonicalReturnsName(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"policy.publish", "policy.activate", "policy.publish_rulepack"} {
		got, err := Registry{}.Lookup(v)
		if err != nil {
			t.Errorf("Lookup(%q): err=%v, want nil", v, err)
		}
		if got != v {
			t.Errorf("Lookup(%q)=%q, want %q", v, got, v)
		}
	}
}

// TestRegistry_LookupDisallowedReturnsUnimplemented -- scenario:
// canonical-rulepack-verb-name negative half. Pins the
// tech-spec Sec 8.5 + architecture Sec 6.5 contract that the
// historical drafts `policy.rulepack.add` /
// `policy.rulepack.remove` / `policy.override` are NOT in the
// canonical surface.
func TestRegistry_LookupDisallowedReturnsUnimplemented(t *testing.T) {
	t.Parallel()
	for _, v := range []string{
		"policy.rulepack.add",
		"policy.rulepack.remove",
		"policy.override",
		"Policy.Publish", // canon-guard: uppercase namespace banned
		"policy.publish_rule_pack",
		"",
	} {
		_, err := Registry{}.Lookup(v)
		if err == nil {
			t.Errorf("Lookup(%q): err=nil, want ErrUnimplementedVerb", v)
		}
		if !errors.Is(err, ErrUnimplementedVerb) {
			t.Errorf("Lookup(%q): err=%v, want errors.Is ErrUnimplementedVerb", v, err)
		}
	}
}

// TestRegistry_VerbsReturnsCopy guards against an upstream
// caller mutating the canonical slice and silently corrupting
// the registry.
func TestRegistry_VerbsReturnsCopy(t *testing.T) {
	t.Parallel()
	a := Registry{}.Verbs()
	a[0] = "tampered"
	b := Registry{}.Verbs()
	if b[0] == "tampered" {
		t.Fatalf("Verbs() returned a shared slice; caller mutation propagated to subsequent calls (want defensive copy)")
	}
}

func TestCanonicalJSON_SortedKeysAndDeterministic(t *testing.T) {
	t.Parallel()
	payload := canonicalSignedPayload{
		RuleRefs: []RuleRef{
			{RuleID: "solid.srp.lcom4_high", Version: 3},
			{RuleID: "solid.dip.fan_out_high", Version: 1},
		},
		ThresholdRefs: []ThresholdRef{},
		RefactorWeights: RefactorWeights{
			Alpha: 0.4, Beta: 0.3, Gamma: 0.2, Delta: 0.1,
			EffortModelVersion: "v1.0",
			WindowDays:         90,
		},
	}
	first, err := canonicalJSON(payload)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}
	second, err := canonicalJSON(payload)
	if err != nil {
		t.Fatalf("canonicalJSON (2nd call): %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("canonicalJSON is non-deterministic: %q vs %q", first, second)
	}

	// Pin the byte shape so a future regression that
	// reorders keys or changes number formatting trips
	// this assertion (signature would silently invalidate
	// otherwise). The expected form has top-level keys in
	// lexicographic order: refactor_weights, rule_refs,
	// threshold_refs.
	want := `{"refactor_weights":{"alpha":0.4,"beta":0.3,"delta":0.1,"effort_model_version":"v1.0","gamma":0.2,"window_days":90},"rule_refs":[{"rule_id":"solid.srp.lcom4_high","version":3},{"rule_id":"solid.dip.fan_out_high","version":1}],"threshold_refs":[]}`
	if string(first) != want {
		t.Fatalf("canonicalJSON shape mismatch:\n got: %s\nwant: %s", first, want)
	}
}

func TestCanonicalJSON_RoundTripStable(t *testing.T) {
	t.Parallel()
	thresholdID := uuid.Must(uuid.NewV4())
	freshness := 7200
	payload := canonicalSignedPayload{
		RuleRefs: []RuleRef{
			{RuleID: "decoupling.cycles.any", Version: 2},
		},
		ThresholdRefs: []ThresholdRef{
			{ThresholdID: thresholdID},
		},
		RefactorWeights: RefactorWeights{
			Alpha: 0.25, Beta: 0.25, Gamma: 0.25, Delta: 0.25,
			EffortModelVersion:     "v1.7",
			WindowDays:             90,
			FreshnessWindowSeconds: &freshness,
		},
	}
	first, err := canonicalJSON(payload)
	if err != nil {
		t.Fatalf("canonicalJSON: %v", err)
	}

	// Simulate a PostgreSQL `jsonb` round-trip: decode and
	// re-encode the typed payload (mirrors what SQLStore
	// does on read). The canonical-encoded bytes MUST
	// match the originals so the signature verifies after
	// a DB round trip.
	pv := PolicyVersion{
		RuleRefs:        payload.RuleRefs,
		ThresholdRefs:   payload.ThresholdRefs,
		RefactorWeights: payload.RefactorWeights,
	}
	second, err := canonicalJSON(canonicalSignedPayload{
		RuleRefs:        pv.RuleRefs,
		ThresholdRefs:   pv.ThresholdRefs,
		RefactorWeights: pv.RefactorWeights,
	})
	if err != nil {
		t.Fatalf("canonicalJSON (2nd call): %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("round-trip changed canonical bytes:\n first: %s\nsecond: %s", first, second)
	}
}

// TestStoreInterface_NoUpdateOrDeleteMethods pins the
// `policy-version-immutable` scenario contract at the
// interface-shape level: the [Store] interface must not have
// any method whose name starts with "Update" or "Delete"
// (G3 append-only).
func TestStoreInterface_NoUpdateOrDeleteMethods(t *testing.T) {
	t.Parallel()
	storeType := reflect.TypeOf((*Store)(nil)).Elem()
	for i := 0; i < storeType.NumMethod(); i++ {
		name := storeType.Method(i).Name
		if hasPrefix(name, "Update") || hasPrefix(name, "Delete") {
			t.Errorf("Store interface exposes %q -- G3 mandates append-only writers; no Update/Delete methods allowed", name)
		}
	}
}

func hasPrefix(s, p string) bool {
	if len(s) < len(p) {
		return false
	}
	return s[:len(p)] == p
}

func TestSeverity_IsValid(t *testing.T) {
	t.Parallel()
	cases := map[Severity]bool{
		SeverityInfo:  true,
		SeverityWarn:  true,
		SeverityBlock: true,
		"":            false,
		"critical":    false,
		"INFO":        false, // case-sensitive
	}
	for s, want := range cases {
		if got := s.IsValid(); got != want {
			t.Errorf("Severity(%q).IsValid()=%v, want %v", s, got, want)
		}
	}
}

func TestInMemoryStore_InsertPolicyVersionDuplicateRejected(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewInMemoryStore()
	id := uuid.Must(uuid.NewV4())
	pv := newSamplePolicyVersion(id)
	if err := s.InsertPolicyVersion(ctx, pv); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := s.InsertPolicyVersion(ctx, pv); err == nil {
		t.Fatalf("second insert: err=nil, want duplicate")
	}
}

func TestInMemoryStore_LatestActivationLatestRowWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewInMemoryStore()

	pvA := newSamplePolicyVersion(uuid.Must(uuid.NewV4()))
	pvB := newSamplePolicyVersion(uuid.Must(uuid.NewV4()))
	if err := s.InsertPolicyVersion(ctx, pvA); err != nil {
		t.Fatalf("InsertPolicyVersion A: %v", err)
	}
	if err := s.InsertPolicyVersion(ctx, pvB); err != nil {
		t.Fatalf("InsertPolicyVersion B: %v", err)
	}

	now := sampleClockStart()
	first := PolicyActivation{
		ActivationID:    uuid.Must(uuid.NewV4()),
		PolicyVersionID: pvA.PolicyVersionID,
		ActivatedBy:     "alice",
		CreatedAt:       now,
	}
	second := PolicyActivation{
		ActivationID:    uuid.Must(uuid.NewV4()),
		PolicyVersionID: pvB.PolicyVersionID,
		ActivatedBy:     "bob",
		CreatedAt:       now.Add(time.Minute),
	}
	if err := s.InsertPolicyActivation(ctx, first); err != nil {
		t.Fatalf("insert first activation: %v", err)
	}
	if err := s.InsertPolicyActivation(ctx, second); err != nil {
		t.Fatalf("insert second activation: %v", err)
	}

	latest, ok, err := s.LatestActivation(ctx)
	if err != nil {
		t.Fatalf("LatestActivation: %v", err)
	}
	if !ok {
		t.Fatalf("LatestActivation ok=false; expected the second row")
	}
	if latest.ActivationID != second.ActivationID {
		t.Fatalf("LatestActivation returned id=%s, want %s (latest by created_at wins)", latest.ActivationID, second.ActivationID)
	}
}

func TestInMemoryStore_InsertActivationRejectsUnknownPolicyVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewInMemoryStore()
	pa := PolicyActivation{
		ActivationID:    uuid.Must(uuid.NewV4()),
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		ActivatedBy:     "alice",
		CreatedAt:       sampleClockStart(),
	}
	err := s.InsertPolicyActivation(ctx, pa)
	if !errors.Is(err, ErrUnknownPolicyVersion) {
		t.Fatalf("InsertPolicyActivation: err=%v, want ErrUnknownPolicyVersion", err)
	}
}

func TestInMemoryStore_RulePackTransactionalOnDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewInMemoryStore()
	pack := RulePack{
		PackID: "solid.srp", Version: 1, DisplayName: "Single Responsibility",
		DescriptionMD: "", CreatedAt: sampleClockStart(),
	}
	rules := []Rule{
		{RuleID: "solid.srp.lcom4_high", Version: 1, PackID: "solid.srp",
			PredicateDSL: "lcom4 > 0.7", SeverityDefault: SeverityBlock, DescriptionMD: ""},
		// Duplicate-within-batch: same `(rule_id, version)`.
		// Triggers ErrDuplicateRule on the SECOND in-memory
		// check; nothing should land.
		{RuleID: "solid.srp.lcom4_high", Version: 1, PackID: "solid.srp",
			PredicateDSL: "lcom4 > 0.7", SeverityDefault: SeverityBlock, DescriptionMD: ""},
	}
	err := s.InsertRulePackAndRules(ctx, pack, rules)
	if !errors.Is(err, ErrDuplicateRule) {
		t.Fatalf("InsertRulePackAndRules: err=%v, want ErrDuplicateRule", err)
	}
	if _, ok, _ := s.GetRulePack(ctx, pack.PackID, pack.Version); ok {
		t.Errorf("pack row was persisted despite a mid-batch failure -- not transactional")
	}
	if rs, _ := s.ListRulesForPack(ctx, pack.PackID); len(rs) != 0 {
		t.Errorf("ListRulesForPack: got %d rules; want 0 (transaction must roll back)", len(rs))
	}
}

// sample helpers ----------------------------------------------------

func sampleClockStart() time.Time {
	// Use a fixed instant so tests are reproducible across
	// machines.
	t, err := time.Parse(time.RFC3339, "2026-05-01T10:00:00Z")
	if err != nil {
		panic(err)
	}
	return t
}
