package steward

// Stage 8.2 publish-validation tests for the new
// `RefactorWeights.TopN` field. Negative values are rejected
// at publish time so a malformed policy can never become
// active; zero (no truncation) and positive values are
// accepted. The Refactor Planner consumes the field via
// [steward.PolicyVersion.RefactorWeights] read by
// `refactor.StewardPolicyReader`.

import (
	"context"
	"errors"
	"testing"
)

// TestSteward_PublishRejectsNegativeTopN exercises the new
// validation rule: `RefactorWeights.TopN < 0` returns
// [ErrInvalidRequest]. The Stage 8.1 planner has no concept
// of negative truncation; surfacing the rejection at publish
// time means an in-flight refactor never sees the invalid
// value.
func TestSteward_PublishRejectsNegativeTopN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	st, err := New(Config{Store: store, Signer: mgr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := newSamplePublishRequest()
	req.RefactorWeights.TopN = -1
	_, err = st.Publish(ctx, req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Publish err = %v, want ErrInvalidRequest", err)
	}
}

// TestSteward_PublishAcceptsZeroTopN confirms the
// backward-compatible interpretation -- TopN == 0 means
// "the operator did not configure truncation, plan covers
// all hot_spots". The validator MUST NOT treat zero as
// invalid.
func TestSteward_PublishAcceptsZeroTopN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	st, err := New(Config{Store: store, Signer: mgr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := newSamplePublishRequest()
	req.RefactorWeights.TopN = 0
	pv, err := st.Publish(ctx, req)
	if err != nil {
		t.Fatalf("Publish err = %v, want nil", err)
	}
	if pv.RefactorWeights.TopN != 0 {
		t.Errorf("persisted TopN = %d, want 0", pv.RefactorWeights.TopN)
	}
}

// TestSteward_PublishAcceptsPositiveTopN confirms a normal
// positive TopN flows through and is retrievable.
func TestSteward_PublishAcceptsPositiveTopN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	mgr := newKeysManagerWithMintedKey(t)
	store := NewInMemoryStore()
	seedSampleRulesInto(t, store)
	st, err := New(Config{Store: store, Signer: mgr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	req := newSamplePublishRequest()
	req.RefactorWeights.TopN = 25
	pv, err := st.Publish(ctx, req)
	if err != nil {
		t.Fatalf("Publish err = %v, want nil", err)
	}
	if pv.RefactorWeights.TopN != 25 {
		t.Errorf("persisted TopN = %d, want 25", pv.RefactorWeights.TopN)
	}
}
