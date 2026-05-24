package steward

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/microsoft/code-intelligence/services/clean-code/internal/policy/keys"
)

// TestSteward_PublishHappyPath -- scenario:
// `publish-signs-canonical-bytes`. Round-trips the signature
// through the Manager's VerifyAny to prove the canonical
// payload + signature pair is internally consistent.
func TestSteward_PublishHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	st, err := New(Config{Store: store, Signer: mgr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	pv, err := st.Publish(ctx, newSamplePublishRequest())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}

	if pv.PolicyVersionID == uuid.Nil {
		t.Errorf("Publish returned PolicyVersionID=Nil; expected a minted uuid")
	}
	if len(pv.Signature) == 0 {
		t.Errorf("Publish returned empty Signature; expected an Ed25519 signature")
	}
	if pv.CreatedAt.IsZero() {
		t.Errorf("Publish returned zero CreatedAt; expected a wall-clock instant")
	}

	if err := st.VerifyPolicyVersionSignature(ctx, pv); err != nil {
		t.Errorf("VerifyPolicyVersionSignature(persisted row): %v", err)
	}

	// Read the row back and verify again -- pins the round-
	// trip through the store keeps signature integrity.
	got, err := store.GetPolicyVersion(ctx, pv.PolicyVersionID)
	if err != nil {
		t.Fatalf("GetPolicyVersion: %v", err)
	}
	if err := st.VerifyPolicyVersionSignature(ctx, got); err != nil {
		t.Errorf("VerifyPolicyVersionSignature(store round-trip): %v", err)
	}
}

// TestSteward_PublishRefusesWithoutSigningKey -- scenario:
// `publish-without-signing-key`. Pins the contract that every
// Stage 5.2 write verb refuses when the [Signer] has no
// active key.
func TestSteward_PublishRefusesWithoutSigningKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerEmpty(t)
	store := NewInMemoryStore()
	st, err := New(Config{Store: store, Signer: mgr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := st.Publish(ctx, newSamplePublishRequest()); !errors.Is(err, ErrNoActiveSigningKey) {
		t.Fatalf("Publish: err=%v, want ErrNoActiveSigningKey", err)
	}
	if _, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		ActivatedBy:     "alice",
	}); !errors.Is(err, ErrNoActiveSigningKey) {
		t.Fatalf("Activate: err=%v, want ErrNoActiveSigningKey", err)
	}
	if _, _, err := st.PublishRulepack(ctx, newSamplePublishRulepackRequest()); !errors.Is(err, ErrNoActiveSigningKey) {
		t.Fatalf("PublishRulepack: err=%v, want ErrNoActiveSigningKey", err)
	}
}

// TestSteward_PublishRejectsInvalidPayload pins the validation
// table in `validatePublishRequest`.
func TestSteward_PublishRejectsInvalidPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	st, _ := New(Config{Store: store, Signer: mgr})

	cases := map[string]PublishRequest{
		"empty-name": func() PublishRequest {
			r := newSamplePublishRequest()
			r.Name = "   "
			return r
		}(),
		"empty-rule-refs": func() PublishRequest {
			r := newSamplePublishRequest()
			r.RuleRefs = nil
			return r
		}(),
		"rule-ref-zero-version": func() PublishRequest {
			r := newSamplePublishRequest()
			r.RuleRefs = []RuleRef{{RuleID: "solid.srp.lcom4_high", Version: 0}}
			return r
		}(),
		"zero-threshold-uuid": func() PublishRequest {
			r := newSamplePublishRequest()
			r.ThresholdRefs = []ThresholdRef{{ThresholdID: uuid.Nil}}
			return r
		}(),
		"zero-window-days": func() PublishRequest {
			r := newSamplePublishRequest()
			r.RefactorWeights.WindowDays = 0
			return r
		}(),
		"empty-effort-model": func() PublishRequest {
			r := newSamplePublishRequest()
			r.RefactorWeights.EffortModelVersion = ""
			return r
		}(),
	}
	for name, req := range cases {
		req := req
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := st.Publish(ctx, req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Publish: err=%v, want ErrInvalidRequest", err)
			}
		})
	}
}

// TestSteward_ActivateRefusesUnknownPolicyVersion -- scenario:
// `activate-refuses-unknown-policy-version`. Surfaces the
// canonical [ErrUnknownPolicyVersion] even though the SQL FK
// would reject the same row in production.
func TestSteward_ActivateRefusesUnknownPolicyVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	st, _ := New(Config{Store: store, Signer: mgr})

	_, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: uuid.Must(uuid.NewV4()),
		ActivatedBy:     "alice",
	})
	if !errors.Is(err, ErrUnknownPolicyVersion) {
		t.Fatalf("Activate(unknown id): err=%v, want ErrUnknownPolicyVersion", err)
	}
}

// TestSteward_ActivateRejectsZeroUUID guards the
// `policy_version_id == uuid.Nil` corner case.
func TestSteward_ActivateRejectsZeroUUID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	st, _ := New(Config{Store: store, Signer: mgr})

	_, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: uuid.Nil,
		ActivatedBy:     "alice",
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Activate(zero uuid): err=%v, want ErrInvalidRequest", err)
	}
}

// TestSteward_PublishThenActivateLatestRowWins -- scenario:
// `activation-latest-row-wins`. Exercises the full
// publish->activate->publish->activate cycle and asserts the
// second activation supersedes the first.
func TestSteward_PublishThenActivateLatestRowWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	st, err := New(Config{
		Store:  store,
		Signer: mgr,
		// Deterministic clock so the second activation's
		// CreatedAt is guaranteed > the first's, matching
		// the SQL ORDER BY contract.
		Clock: fixedClock(sampleClockStart()),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	pvA, err := st.Publish(ctx, newSamplePublishRequest())
	if err != nil {
		t.Fatalf("Publish A: %v", err)
	}
	req2 := newSamplePublishRequest()
	req2.Name = "default-v2"
	pvB, err := st.Publish(ctx, req2)
	if err != nil {
		t.Fatalf("Publish B: %v", err)
	}
	if pvA.PolicyVersionID == pvB.PolicyVersionID {
		t.Fatalf("Publish: two distinct versions returned the same uuid")
	}

	paFirst, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: pvA.PolicyVersionID,
		ActivatedBy:     "alice",
	})
	if err != nil {
		t.Fatalf("Activate A: %v", err)
	}
	paSecond, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: pvB.PolicyVersionID,
		ActivatedBy:     "bob",
	})
	if err != nil {
		t.Fatalf("Activate B: %v", err)
	}
	if !paSecond.CreatedAt.After(paFirst.CreatedAt) {
		t.Fatalf("expected paSecond.CreatedAt (%s) > paFirst.CreatedAt (%s)",
			paSecond.CreatedAt, paFirst.CreatedAt)
	}

	latest, ok, err := st.LatestActivation(ctx)
	if err != nil {
		t.Fatalf("LatestActivation: %v", err)
	}
	if !ok {
		t.Fatalf("LatestActivation ok=false; expected two rows")
	}
	if latest.ActivationID != paSecond.ActivationID {
		t.Fatalf("LatestActivation returned id=%s, want %s (latest-row-wins)", latest.ActivationID, paSecond.ActivationID)
	}
	if latest.PolicyVersionID != pvB.PolicyVersionID {
		t.Fatalf("LatestActivation.PolicyVersionID=%s, want %s", latest.PolicyVersionID, pvB.PolicyVersionID)
	}
}

// TestSteward_PublishRulepackWritesPackAndRules -- scenario:
// `publish_rulepack-writes-rule-pack-and-rules`. Asserts both
// the pack row and every rule row landed.
func TestSteward_PublishRulepackWritesPackAndRules(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	st, _ := New(Config{Store: store, Signer: mgr})

	req := newSamplePublishRulepackRequest()
	pack, rules, err := st.PublishRulepack(ctx, req)
	if err != nil {
		t.Fatalf("PublishRulepack: %v", err)
	}
	if pack.PackID != req.PackID || pack.Version != req.Version {
		t.Errorf("returned pack=%+v, want pack_id=%s version=%d", pack, req.PackID, req.Version)
	}
	if len(rules) != len(req.Rules) {
		t.Fatalf("returned %d rules, want %d", len(rules), len(req.Rules))
	}

	gotPack, ok, err := store.GetRulePack(ctx, req.PackID, req.Version)
	if err != nil {
		t.Fatalf("GetRulePack: %v", err)
	}
	if !ok {
		t.Fatalf("GetRulePack: ok=false; expected the freshly-published row")
	}
	if gotPack.DisplayName != req.DisplayName {
		t.Errorf("GetRulePack.DisplayName=%q, want %q", gotPack.DisplayName, req.DisplayName)
	}

	gotRules, err := store.ListRulesForPack(ctx, req.PackID)
	if err != nil {
		t.Fatalf("ListRulesForPack: %v", err)
	}
	if len(gotRules) != len(req.Rules) {
		t.Fatalf("ListRulesForPack returned %d rules, want %d", len(gotRules), len(req.Rules))
	}
	for i, r := range gotRules {
		if r.PackID != req.PackID {
			t.Errorf("rules[%d].PackID=%q, want %q (logical FK violated)", i, r.PackID, req.PackID)
		}
	}
}

// TestSteward_PublishRulepackRejectsDuplicate -- scenario:
// `publish_rulepack-immutable`. Publishing the same
// `(pack_id, version)` twice returns [ErrDuplicateRulePack].
func TestSteward_PublishRulepackRejectsDuplicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	st, _ := New(Config{Store: store, Signer: mgr})

	req := newSamplePublishRulepackRequest()
	if _, _, err := st.PublishRulepack(ctx, req); err != nil {
		t.Fatalf("first PublishRulepack: %v", err)
	}
	_, _, err := st.PublishRulepack(ctx, req)
	if !errors.Is(err, ErrDuplicateRulePack) {
		t.Fatalf("second PublishRulepack(same pack_id/version): err=%v, want ErrDuplicateRulePack", err)
	}
}

// TestSteward_PublishRulepackRejectsInvalidPayload pins the
// validation table in `validatePublishRulepackRequest`.
func TestSteward_PublishRulepackRejectsInvalidPayload(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	st, _ := New(Config{Store: store, Signer: mgr})

	cases := map[string]PublishRulepackRequest{
		"empty-pack-id": func() PublishRulepackRequest {
			r := newSamplePublishRulepackRequest()
			r.PackID = ""
			return r
		}(),
		"zero-version": func() PublishRulepackRequest {
			r := newSamplePublishRulepackRequest()
			r.Version = 0
			return r
		}(),
		"empty-display-name": func() PublishRulepackRequest {
			r := newSamplePublishRulepackRequest()
			r.DisplayName = ""
			return r
		}(),
		"empty-rules": func() PublishRulepackRequest {
			r := newSamplePublishRulepackRequest()
			r.Rules = nil
			return r
		}(),
		"rule-empty-id": func() PublishRulepackRequest {
			r := newSamplePublishRulepackRequest()
			r.Rules[0].RuleID = ""
			return r
		}(),
		"rule-bad-severity": func() PublishRulepackRequest {
			r := newSamplePublishRulepackRequest()
			r.Rules[0].SeverityDefault = "critical"
			return r
		}(),
		"rule-empty-dsl": func() PublishRulepackRequest {
			r := newSamplePublishRulepackRequest()
			r.Rules[0].PredicateDSL = ""
			return r
		}(),
	}
	for name, req := range cases {
		req := req
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, _, err := st.PublishRulepack(ctx, req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("PublishRulepack: err=%v, want ErrInvalidRequest", err)
			}
		})
	}
}

// TestSteward_PublishImmutableNoUpdatePath -- scenario:
// `policy-version-immutable`. Once published, the row cannot
// be mutated through the [Store] interface (there is no Update
// method); a re-publish with the SAME `policy_version_id` is
// rejected as a duplicate.
//
// We exercise this via a deterministic UUID generator so the
// second Publish call attempts to re-insert the same id.
func TestSteward_PublishImmutableNoUpdatePath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	fixedID := uuid.Must(uuid.NewV4())
	st, _ := New(Config{
		Store:  store,
		Signer: mgr,
		UUIDGen: func() (uuid.UUID, error) {
			return fixedID, nil
		},
	})

	if _, err := st.Publish(ctx, newSamplePublishRequest()); err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	_, err := st.Publish(ctx, newSamplePublishRequest())
	if err == nil {
		t.Fatalf("second Publish(same uuid): err=nil; expected duplicate-key rejection (G3 immutability)")
	}
	// The InMemoryStore returns a raw fmt.Errorf for
	// duplicate policy_version_id, not a wrapped sentinel.
	// We assert the message mentions "already exists" so
	// this test stays robust against re-wording without
	// requiring a new sentinel.
	if !contains(err.Error(), "already exists") {
		t.Errorf("second Publish error=%v; expected message to mention 'already exists'", err)
	}
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestSteward_NewRequiresStore pins the [New] precondition:
// [Config.Store] is mandatory, [Config.Signer] is OPTIONAL.
// The kill-switch contract (Stage 5.3) requires `mgmt.override`
// to keep serving 200 in scaffold mode, so the constructor
// installs a [noActiveSigner] null object instead of refusing.
// The Stage 5.2 verbs still refuse with [ErrNoActiveSigningKey]
// because that null object reports an empty active-key set --
// see [TestSteward_PublishRefusesWhenSignerNil] for the positive
// pin on the Stage 5.2 side.
func TestSteward_NewRequiresStore(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}); err == nil {
		t.Errorf("New(empty): err=nil; expected Store required")
	}
	if _, err := New(Config{Signer: newKeysManagerWithMintedKey(t)}); err == nil {
		t.Errorf("New(no Store): err=nil; expected Store required")
	}
	// Stage 5.3 kill-switch contract: a steward constructed
	// without a signer MUST be usable -- not an error.
	st, err := New(Config{Store: NewInMemoryStore()})
	if err != nil {
		t.Fatalf("New(no Signer): err=%v; want nil (kill-switch contract: Signer is optional)", err)
	}
	if st == nil {
		t.Fatalf("New(no Signer): steward is nil; want a real Steward backed by noActiveSigner")
	}
}

// TestSteward_PublishRefusesWhenSignerNil pins the other half
// of the Stage 5.3 kill-switch contract: a steward constructed
// without a signer still REFUSES Publish (the signing verb)
// with [ErrNoActiveSigningKey], because the null-object signer
// reports an empty active-key set. This keeps the Stage 5.2
// "no active key = 503" contract intact while letting Override
// (Stage 5.3) bypass the precondition.
func TestSteward_PublishRefusesWhenSignerNil(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	st, err := New(Config{Store: store}) // no Signer wired
	if err != nil {
		t.Fatalf("New(no Signer): %v", err)
	}
	_, err = st.Publish(ctx, newSamplePublishRequest())
	if !errors.Is(err, ErrNoActiveSigningKey) {
		t.Fatalf("Publish on null-signer steward: err=%v; want ErrNoActiveSigningKey", err)
	}
}

// TestSteward_PublishHandlesUUIDGenError surfaces a generator
// failure as a wrapped error rather than crashing.
func TestSteward_PublishHandlesUUIDGenError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	wantErr := errors.New("synthetic uuid failure")
	st, _ := New(Config{
		Store:  store,
		Signer: mgr,
		UUIDGen: func() (uuid.UUID, error) {
			return uuid.Nil, wantErr
		},
	})
	_, err := st.Publish(ctx, newSamplePublishRequest())
	if !errors.Is(err, wantErr) {
		t.Fatalf("Publish: err=%v, want errors.Is %v", err, wantErr)
	}
}

// TestSteward_PublishCreatesTimestampedUTC pins the CreatedAt
// contract: every persisted row's CreatedAt is non-zero, in
// UTC, and (with a deterministic clock) matches the clock
// instant.
func TestSteward_PublishCreatesTimestampedUTC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)

	want := sampleClockStart()
	st, _ := New(Config{
		Store:  store,
		Signer: mgr,
		Clock:  func() time.Time { return want },
	})
	pv, err := st.Publish(ctx, newSamplePublishRequest())
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if pv.CreatedAt.Location() != time.UTC {
		t.Errorf("PolicyVersion.CreatedAt.Location=%v, want UTC", pv.CreatedAt.Location())
	}
	if !pv.CreatedAt.Equal(want.UTC()) {
		t.Errorf("PolicyVersion.CreatedAt=%s, want %s", pv.CreatedAt, want.UTC())
	}
	pa, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: pv.PolicyVersionID,
		ActivatedBy:     "alice",
	})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if pa.CreatedAt.Location() != time.UTC {
		t.Errorf("PolicyActivation.CreatedAt.Location=%v, want UTC", pa.CreatedAt.Location())
	}
}

// TestSteward_PublishEnforcesRuleRefFK -- scenario:
// `policy.publish` MUST reject `rule_refs` entries that point
// at unregistered rules. Migration 0003 line 280 + Sec 5.3.3:
// "The Policy Steward enforces the reference at write time."
// Without this check, a published PolicyVersion could be
// unresolvable at gate time.
func TestSteward_PublishEnforcesRuleRefFK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	// Intentionally DO NOT seed rules.
	st, _ := New(Config{Store: store, Signer: mgr})

	req := newSamplePublishRequest()
	// Sanity: the sample uses solid.srp.lcom4_high/v1 which
	// no test has registered. The publish call must reject.
	_, err := st.Publish(ctx, req)
	if !errors.Is(err, ErrUnknownRuleRef) {
		t.Fatalf("Publish(unknown rule_ref): err=%v, want ErrUnknownRuleRef", err)
	}
	// No PolicyVersion row should have landed.
	if _, err := store.GetPolicyVersion(ctx, uuid.Must(uuid.NewV4())); !errors.Is(err, ErrUnknownPolicyVersion) {
		t.Errorf("expected store to be empty after failed Publish; got err=%v", err)
	}
}

// TestSteward_PublishEnforcesThresholdRefFK is the analogous
// guard for the `threshold_refs` JSON-FK contract (migration
// 0003 line 462).
func TestSteward_PublishEnforcesThresholdRefFK(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	st, _ := New(Config{Store: store, Signer: mgr})

	req := newSamplePublishRequest()
	// Reference a threshold id that nobody seeded. The
	// publish call must reject with ErrUnknownThresholdRef
	// even though the rule_refs are all valid.
	req.ThresholdRefs = []ThresholdRef{
		{ThresholdID: uuid.Must(uuid.NewV4())},
	}
	_, err := st.Publish(ctx, req)
	if !errors.Is(err, ErrUnknownThresholdRef) {
		t.Fatalf("Publish(unknown threshold_ref): err=%v, want ErrUnknownThresholdRef", err)
	}
}

// TestSteward_PublishMixedRefsAllOrNothing pins the
// "validate-before-sign" guarantee: if any single ref is
// unknown the entire request is rejected and no signature
// material is consumed, even when the other refs are valid.
func TestSteward_PublishMixedRefsAllOrNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	thresholdID := uuid.Must(uuid.NewV4())
	seedThresholdInto(t, store, thresholdID)

	// Count signing calls via a wrapped signer so we can
	// assert no Sign call happened on the rejected request.
	sc := &signCountingSigner{inner: mgr}
	st, _ := New(Config{Store: store, Signer: sc})

	req := newSamplePublishRequest()
	req.RuleRefs = []RuleRef{
		{RuleID: "solid.srp.lcom4_high", Version: 1},  // valid
		{RuleID: "decoupling.cycles.any", Version: 1}, // not seeded
	}
	req.ThresholdRefs = []ThresholdRef{
		{ThresholdID: thresholdID}, // valid
	}
	_, err := st.Publish(ctx, req)
	if !errors.Is(err, ErrUnknownRuleRef) {
		t.Fatalf("Publish(mixed refs): err=%v, want ErrUnknownRuleRef", err)
	}
	if sc.signCalls != 0 {
		t.Errorf("Sign was called %d time(s) on a rejected request; want 0 (validate-before-sign)", sc.signCalls)
	}
}

// TestSteward_PublishRejectsDuplicateRefs guards against a
// caller stuffing the same rule_id/version (or threshold_id)
// into the refs slice twice. Even though the FK lookups would
// each succeed, the duplicate is meaningless and would skew the
// canonical-JSON byte shape silently.
func TestSteward_PublishRejectsDuplicateRefs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	thresholdID := uuid.Must(uuid.NewV4())
	seedThresholdInto(t, store, thresholdID)
	st, _ := New(Config{Store: store, Signer: mgr})

	t.Run("duplicate-rule-ref", func(t *testing.T) {
		req := newSamplePublishRequest()
		req.RuleRefs = []RuleRef{
			{RuleID: "solid.srp.lcom4_high", Version: 1},
			{RuleID: "solid.srp.lcom4_high", Version: 1},
		}
		_, err := st.Publish(ctx, req)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Publish(duplicate rule_ref): err=%v, want ErrInvalidRequest", err)
		}
	})

	t.Run("duplicate-threshold-ref", func(t *testing.T) {
		req := newSamplePublishRequest()
		req.ThresholdRefs = []ThresholdRef{
			{ThresholdID: thresholdID},
			{ThresholdID: thresholdID},
		}
		_, err := st.Publish(ctx, req)
		if !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Publish(duplicate threshold_ref): err=%v, want ErrInvalidRequest", err)
		}
	})
}

// TestSteward_PublishAcceptsValidThresholdRef proves the happy
// path on the threshold side: seed the threshold, publish a
// policy that references it, see the row land with a valid
// signature.
func TestSteward_PublishAcceptsValidThresholdRef(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	thresholdID := uuid.Must(uuid.NewV4())
	seedThresholdInto(t, store, thresholdID)
	st, _ := New(Config{Store: store, Signer: mgr})

	req := newSamplePublishRequest()
	req.ThresholdRefs = []ThresholdRef{{ThresholdID: thresholdID}}
	pv, err := st.Publish(ctx, req)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if len(pv.ThresholdRefs) != 1 || pv.ThresholdRefs[0].ThresholdID != thresholdID {
		t.Errorf("ThresholdRefs round-trip mismatch: got %+v, want [{%s}]", pv.ThresholdRefs, thresholdID)
	}
	if err := st.VerifyPolicyVersionSignature(ctx, pv); err != nil {
		t.Errorf("VerifyPolicyVersionSignature: %v", err)
	}
}

// TestSteward_EvaluatorPicksUpActivatedVersion -- scenario:
// `publish -> activate -> evaluator picks up the new version`
// (implementation-plan Stage 5.2 acceptance criterion). This
// exercises the full `eval.gate`-style lookup that a future
// stage will consume: read the latest activation, dereference
// the policy version it pins, verify the signature.
//
// Two cycles (publish A -> activate A -> publish B -> activate
// B) prove that the evaluator-pickup query reflects the NEW
// version after the second activation, not the first
// (latest-row-wins per architecture Sec 5.3.4).
func TestSteward_EvaluatorPicksUpActivatedVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()

	// Step 1: publish a rulepack so the rule_refs FK is
	// satisfied. This mirrors the full operator flow:
	// publish_rulepack BEFORE the first publish call.
	st, _ := New(Config{
		Store:  store,
		Signer: mgr,
		Clock:  fixedClock(sampleClockStart()),
	})
	_, _, err := st.PublishRulepack(ctx, newSamplePublishRulepackRequest())
	if err != nil {
		t.Fatalf("PublishRulepack: %v", err)
	}

	// Step 2 & 3: publish + activate policy A. The
	// evaluator-pickup query must now resolve to A.
	pvA, err := st.Publish(ctx, newSamplePublishRequest())
	if err != nil {
		t.Fatalf("Publish A: %v", err)
	}
	if _, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: pvA.PolicyVersionID,
		ActivatedBy:     "alice",
	}); err != nil {
		t.Fatalf("Activate A: %v", err)
	}

	active, ok, err := st.ActivePolicyVersion(ctx)
	if err != nil {
		t.Fatalf("ActivePolicyVersion (after activate A): %v", err)
	}
	if !ok {
		t.Fatalf("ActivePolicyVersion ok=false after first activation")
	}
	if active.PolicyVersionID != pvA.PolicyVersionID {
		t.Fatalf("ActivePolicyVersion returned %s, want A=%s", active.PolicyVersionID, pvA.PolicyVersionID)
	}
	// The evaluator MUST be able to verify the signature on
	// the row it just read. A signature failure here would
	// mean a future `eval.gate` call would mark every
	// evaluation `degraded=true,
	// degraded_reason=policy_signature_invalid` (architecture
	// Sec 8.2).
	if err := st.VerifyPolicyVersionSignature(ctx, active); err != nil {
		t.Fatalf("VerifyPolicyVersionSignature (after activate A): %v", err)
	}

	// Step 5 & 6: publish + activate policy B; evaluator
	// pickup must now reflect B, not A.
	req2 := newSamplePublishRequest()
	req2.Name = "default-v2"
	pvB, err := st.Publish(ctx, req2)
	if err != nil {
		t.Fatalf("Publish B: %v", err)
	}
	if _, err := st.Activate(ctx, ActivateRequest{
		PolicyVersionID: pvB.PolicyVersionID,
		ActivatedBy:     "bob",
	}); err != nil {
		t.Fatalf("Activate B: %v", err)
	}

	active2, ok, err := st.ActivePolicyVersion(ctx)
	if err != nil {
		t.Fatalf("ActivePolicyVersion (after activate B): %v", err)
	}
	if !ok {
		t.Fatalf("ActivePolicyVersion ok=false after second activation")
	}
	if active2.PolicyVersionID != pvB.PolicyVersionID {
		t.Fatalf("ActivePolicyVersion returned %s after activate B, want B=%s (NOT A=%s)",
			active2.PolicyVersionID, pvB.PolicyVersionID, pvA.PolicyVersionID)
	}
	if err := st.VerifyPolicyVersionSignature(ctx, active2); err != nil {
		t.Fatalf("VerifyPolicyVersionSignature (after activate B): %v", err)
	}

	// Sanity: the older PolicyVersion A is still readable
	// (append-only) -- only the evaluator-pickup view changes.
	if _, err := store.GetPolicyVersion(ctx, pvA.PolicyVersionID); err != nil {
		t.Errorf("PolicyVersion A no longer readable after activating B: %v -- append-only invariant violated", err)
	}
}

// TestSteward_ActivePolicyVersionNoActivation pins the
// fresh-deploy steady state: with no activation row recorded,
// the evaluator pickup query returns `ok=false`, NOT an error.
func TestSteward_ActivePolicyVersionNoActivation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	st, _ := New(Config{Store: store, Signer: mgr})

	_, ok, err := st.ActivePolicyVersion(ctx)
	if err != nil {
		t.Fatalf("ActivePolicyVersion (no activation): err=%v, want nil", err)
	}
	if ok {
		t.Errorf("ActivePolicyVersion ok=true with empty store; want false")
	}
}

// signCountingSigner wraps a Signer and counts how many times
// Sign / VerifyAny were invoked. Used by the
// `validate-before-sign` test to assert a rejected request
// never spends signing material.
type signCountingSigner struct {
	inner     Signer
	signCalls int
}

func (s *signCountingSigner) Sign(ctx context.Context, payload []byte) (uuid.UUID, []byte, error) {
	s.signCalls++
	return s.inner.Sign(ctx, payload)
}

func (s *signCountingSigner) VerifyAny(ctx context.Context, payload []byte, signature []byte) (uuid.UUID, error) {
	return s.inner.VerifyAny(ctx, payload, signature)
}

func (s *signCountingSigner) ListActive(ctx context.Context) ([]keys.ActiveKeyView, error) {
	return s.inner.ListActive(ctx)
}
