package steward

import (
	"context"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
)

// newSamplePolicyVersion mints a typical [PolicyVersion]
// value for tests that need a populated row without writing
// every field by hand.
func newSamplePolicyVersion(id uuid.UUID) PolicyVersion {
	if id == uuid.Nil {
		id = uuid.Must(uuid.NewV4())
	}
	return PolicyVersion{
		PolicyVersionID: id,
		Name:            "default-v1",
		RuleRefs: []RuleRef{
			{RuleID: "solid.srp.lcom4_high", Version: 1},
		},
		ThresholdRefs: []ThresholdRef{},
		RefactorWeights: RefactorWeights{
			Alpha: 0.4, Beta: 0.3, Gamma: 0.2, Delta: 0.1,
			EffortModelVersion: "v1.0",
			WindowDays:         90,
		},
		Signature: []byte("test-signature"),
		CreatedAt: sampleClockStart(),
	}
}

// newSamplePublishRequest mints a canonical [PublishRequest]
// for tests that exercise the happy path.
func newSamplePublishRequest() PublishRequest {
	return PublishRequest{
		Name: "default-v1",
		RuleRefs: []RuleRef{
			{RuleID: "solid.srp.lcom4_high", Version: 1},
		},
		ThresholdRefs: []ThresholdRef{},
		RefactorWeights: RefactorWeights{
			Alpha: 0.4, Beta: 0.3, Gamma: 0.2, Delta: 0.1,
			EffortModelVersion: "v1.0",
			WindowDays:         90,
		},
	}
}

// seedSampleRulesInto registers the rule pack + rules that
// `newSamplePublishRequest` references. Required because
// Steward.Publish enforces the rule_refs FK contract -- a
// publish call with refs to unregistered rules now returns
// [ErrUnknownRuleRef] instead of succeeding.
//
// Returns the persisted Rule rows so a test can assert on
// them. Callers MUST invoke this BEFORE the first
// `Steward.Publish` happy-path call.
func seedSampleRulesInto(t *testing.T, store Store) []Rule {
	t.Helper()
	ctx := context.Background()
	pack := RulePack{
		PackID:        "solid.srp",
		Version:       1,
		DisplayName:   "Single Responsibility",
		DescriptionMD: "SOLID SRP rulepack.",
		CreatedAt:     sampleClockStart(),
	}
	rules := []Rule{
		{RuleID: "solid.srp.lcom4_high", Version: 1, PackID: "solid.srp",
			PredicateDSL: "lcom4 > 0.7", SeverityDefault: SeverityBlock,
			DescriptionMD: "High LCOM4.", CreatedAt: sampleClockStart()},
	}
	if err := store.InsertRulePackAndRules(ctx, pack, rules); err != nil {
		t.Fatalf("seedSampleRulesInto: %v", err)
	}
	return rules
}

// seedThresholdInto registers a Threshold row with the given
// id so a test can construct a `policy.publish` payload with
// a valid `threshold_refs` entry. Returns the persisted
// Threshold (with CreatedAt populated).
func seedThresholdInto(t *testing.T, store Store, id uuid.UUID) Threshold {
	t.Helper()
	th := Threshold{
		ThresholdID: id,
		MetricKind:  "lcom4",
		ScopeKind:   "class",
		Op:          "gt",
		Value:       0.7,
		CreatedAt:   sampleClockStart(),
	}
	if err := store.InsertThreshold(context.Background(), th); err != nil {
		t.Fatalf("seedThresholdInto: %v", err)
	}
	return th
}

// newSamplePublishRulepackRequest mints a canonical
// [PublishRulepackRequest] with two rules.
func newSamplePublishRulepackRequest() PublishRulepackRequest {
	return PublishRulepackRequest{
		PackID:        "solid.srp",
		Version:       1,
		DisplayName:   "Single Responsibility",
		DescriptionMD: "SOLID SRP rulepack.",
		Rules: []RuleSpec{
			{RuleID: "solid.srp.lcom4_high", Version: 1,
				PredicateDSL: "lcom4 > 0.7", SeverityDefault: SeverityBlock, DescriptionMD: "High LCOM4."},
			{RuleID: "solid.srp.interface_width_high", Version: 1,
				PredicateDSL: "interface_width > 10", SeverityDefault: SeverityWarn, DescriptionMD: "Wide interface."},
		},
	}
}

// newKeysManagerWithMintedKey returns a real [keys.Manager]
// wired against the in-memory KMS+Store with one minted key.
// Mirrors the scaffold-mode startup the composition root
// performs. Used by the Steward write-verb tests so the
// signature is exercised end-to-end.
func newKeysManagerWithMintedKey(t *testing.T) *keys.Manager {
	t.Helper()
	res, err := keys.Build(context.Background(), keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: true,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	t.Cleanup(res.Close)
	return res.Manager
}

// newKeysManagerEmpty returns a [keys.Manager] with NO minted
// key. Used by the no-signing-key refusal tests.
func newKeysManagerEmpty(t *testing.T) *keys.Manager {
	t.Helper()
	res, err := keys.Build(context.Background(), keys.BuildConfig{
		KMSProvider:         keys.KMSProviderInMemory,
		MintFirstKeyIfEmpty: false,
	})
	if err != nil {
		t.Fatalf("keys.Build: %v", err)
	}
	t.Cleanup(res.Close)
	return res.Manager
}

// fixedClock returns a deterministic clock that ticks one
// second forward on each call. Used by tests that need
// distinct `created_at` values so latest-row-wins ordering is
// unambiguous.
func fixedClock(start time.Time) func() time.Time {
	var ticks int64
	return func() time.Time {
		out := start.Add(time.Duration(ticks) * time.Second)
		ticks++
		return out
	}
}
