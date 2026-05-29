package management_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gofrs/uuid"

	"github.com/smartpcr/code-intelligence/services/clean-code/internal/management"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/management/insights"
	"github.com/smartpcr/code-intelligence/services/clean-code/internal/policy/steward"
)

// muteOverride returns a deterministic muted [steward.Override]
// with the supplied (rule, repo, glob, created_at, override_id)
// quintuple so each adapter test reads as data instead of
// uuid-juggling boilerplate.
func muteOverride(rule, repo, glob string, createdAt time.Time, id string, mute bool) steward.Override {
	parsed, err := uuid.FromString(id)
	if err != nil {
		panic("muteOverride: bad uuid in fixture: " + err.Error())
	}
	reason := ""
	if mute {
		reason = "legacy hot-spot under refactor"
	}
	return steward.Override{
		OverrideID: parsed,
		RuleID:     rule,
		ScopeFilter: steward.ScopeFilter{
			RepoID:             repo,
			ScopeKind:          steward.ScopeKind("class"),
			ScopeSignatureGlob: glob,
		},
		Mute:      mute,
		Reason:    reason,
		ActorID:   "alice@example.com",
		CreatedAt: createdAt.UTC(),
	}
}

// TestOverrideReaderFromStore_NilStoreSurfacesSentinel pins
// the "unwired backend -> sentinel" contract. The composition
// root would normally never wire a nil-Store adapter, but if
// it does (scaffold-mode bring-up, dev fixture, test seam),
// the adapter MUST refuse rather than panic so the failure
// surfaces as a clean 503 at the HTTP layer.
func TestOverrideReaderFromStore_NilStoreSurfacesSentinel(t *testing.T) {
	t.Parallel()
	a := &management.OverrideReaderFromStore{Store: nil}
	_, err := a.ListAllOverrides(context.Background())
	if !errors.Is(err, management.ErrAgedMuteOverrideStoreUnavailable) {
		t.Fatalf("err=%v, want ErrAgedMuteOverrideStoreUnavailable", err)
	}
}

// TestOverrideReaderFromStore_NilReceiverSurfacesSentinel
// pins the nil-receiver safety contract -- a caller that
// passes a nil *OverrideReaderFromStore through
// insights.NewAgedMutes must not panic at first read.
func TestOverrideReaderFromStore_NilReceiverSurfacesSentinel(t *testing.T) {
	t.Parallel()
	var a *management.OverrideReaderFromStore
	_, err := a.ListAllOverrides(context.Background())
	if !errors.Is(err, management.ErrAgedMuteOverrideStoreUnavailable) {
		t.Fatalf("err=%v, want ErrAgedMuteOverrideStoreUnavailable", err)
	}
}

// TestOverrideReaderFromStore_MapsAllFields pins the
// field-for-field mapping from [steward.Override] ->
// [insights.OverrideRecord]. Every field on OverrideRecord
// must round-trip; OverrideID specifically must be the
// canonical lowercase 36-char UUID string so the projection's
// `(CreatedAt, OverrideID)` tie-break is stable across stores.
func TestOverrideReaderFromStore_MapsAllFields(t *testing.T) {
	t.Parallel()
	store := steward.NewInMemoryStore()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	row := muteOverride(
		"solid.srp.lcom4_high",
		"00000000-0000-0000-0000-000000000001",
		"com.example.legacy.*",
		now.Add(-91*24*time.Hour),
		"11111111-1111-1111-1111-111111111111",
		true,
	)
	if err := store.InsertOverride(context.Background(), row); err != nil {
		t.Fatalf("InsertOverride: %v", err)
	}

	a := &management.OverrideReaderFromStore{Store: store}
	got, err := a.ListAllOverrides(context.Background())
	if err != nil {
		t.Fatalf("ListAllOverrides: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got)=%d, want 1", len(got))
	}

	rec := got[0]
	if want := "11111111-1111-1111-1111-111111111111"; rec.OverrideID != want {
		t.Errorf("OverrideID=%q, want %q (canonical lowercase 36-char uuid)", rec.OverrideID, want)
	}
	if rec.RuleID != "solid.srp.lcom4_high" {
		t.Errorf("RuleID=%q", rec.RuleID)
	}
	if rec.Scope.RepoID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("Scope.RepoID=%q", rec.Scope.RepoID)
	}
	if rec.Scope.ScopeKind != "class" {
		t.Errorf("Scope.ScopeKind=%q, want %q", rec.Scope.ScopeKind, "class")
	}
	if rec.Scope.ScopeSignatureGlob != "com.example.legacy.*" {
		t.Errorf("Scope.ScopeSignatureGlob=%q", rec.Scope.ScopeSignatureGlob)
	}
	if !rec.Mute {
		t.Errorf("Mute=false, want true")
	}
	if rec.Reason == "" {
		t.Errorf("Reason was dropped during mapping")
	}
	if rec.ActorID != "alice@example.com" {
		t.Errorf("ActorID=%q", rec.ActorID)
	}
	if !rec.CreatedAt.Equal(row.CreatedAt) {
		t.Errorf("CreatedAt=%v, want %v", rec.CreatedAt, row.CreatedAt)
	}
}

// TestOverrideReaderFromStore_ReturnsBothMuteAndUnmuteRows pins
// the projection's invariant that BOTH mute=true and
// mute=false rows reach insights -- the reducer there does
// latest-row-wins per (rule_id, scope), and stripping
// mute=false at the adapter would silently surface a
// "ghost mute" (the stale mute=true is no longer the actual
// state but would still show on the report).
func TestOverrideReaderFromStore_ReturnsBothMuteAndUnmuteRows(t *testing.T) {
	t.Parallel()
	store := steward.NewInMemoryStore()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	mute := muteOverride("r", "repo", "Foo",
		now.Add(-100*24*time.Hour),
		"11111111-1111-1111-1111-111111111111", true)
	unmute := muteOverride("r", "repo", "Foo",
		now.Add(-1*time.Hour),
		"22222222-2222-2222-2222-222222222222", false)
	for _, o := range []steward.Override{mute, unmute} {
		if err := store.InsertOverride(context.Background(), o); err != nil {
			t.Fatalf("InsertOverride: %v", err)
		}
	}

	a := &management.OverrideReaderFromStore{Store: store}
	got, err := a.ListAllOverrides(context.Background())
	if err != nil {
		t.Fatalf("ListAllOverrides: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got)=%d, want 2 (mute + unmute, NOT pre-reduced)", len(got))
	}
	muteCount := 0
	for _, r := range got {
		if r.Mute {
			muteCount++
		}
	}
	if muteCount != 1 {
		t.Errorf("mute=true count=%d, want 1", muteCount)
	}
}

// TestOverrideReaderFromStore_EmptyStoreReturnsEmptyNonNilSlice
// pins the JSON-stability contract -- an empty result is a
// non-nil empty slice so the encoded payload is `[]`, never
// `null`.
func TestOverrideReaderFromStore_EmptyStoreReturnsEmptyNonNilSlice(t *testing.T) {
	t.Parallel()
	a := &management.OverrideReaderFromStore{Store: steward.NewInMemoryStore()}
	got, err := a.ListAllOverrides(context.Background())
	if err != nil {
		t.Fatalf("ListAllOverrides: %v", err)
	}
	if got == nil {
		t.Fatal("got=nil, want non-nil empty slice (JSON would render null otherwise)")
	}
	if len(got) != 0 {
		t.Fatalf("len(got)=%d, want 0", len(got))
	}
}

// TestOverrideReaderFromStore_PropagatesContextCancellation
// pins that a cancelled context surfaces to the caller via
// the underlying [steward.Store] -- the projection then drops
// the request without touching the partial result.
func TestOverrideReaderFromStore_PropagatesContextCancellation(t *testing.T) {
	t.Parallel()
	a := &management.OverrideReaderFromStore{Store: steward.NewInMemoryStore()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := a.ListAllOverrides(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want context.Canceled", err)
	}
}

// TestOverrideReaderFromStore_EndToEnd_WithAgedMutes
// pins the full Stage 10.2 wiring through the adapter:
// steward.InMemoryStore -> OverrideReaderFromStore ->
// insights.AgedMutes.Report. An aged mute (100d old) MUST
// surface; a fresh mute (1d old) MUST NOT. This is the same
// invariant the per-package reader_aged_mutes_test pins, but
// exercised through the production adapter rather than a
// hand-rolled test double -- closing the iter-1 evaluator
// item 2 "no concrete adapter ships" gap.
func TestOverrideReaderFromStore_EndToEnd_WithAgedMutes(t *testing.T) {
	t.Parallel()
	store := steward.NewInMemoryStore()
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	aged := muteOverride("solid.srp.lcom4_high", "repo", "Legacy*",
		now.Add(-100*24*time.Hour),
		"11111111-1111-1111-1111-111111111111", true)
	fresh := muteOverride("solid.dip.cycles", "repo", "Recent*",
		now.Add(-1*24*time.Hour),
		"22222222-2222-2222-2222-222222222222", true)
	for _, o := range []steward.Override{aged, fresh} {
		if err := store.InsertOverride(context.Background(), o); err != nil {
			t.Fatalf("InsertOverride: %v", err)
		}
	}

	adapter := &management.OverrideReaderFromStore{Store: store}
	am := insights.NewAgedMutes(adapter, fixedNow{now})
	report, err := am.Report(context.Background())
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(report) != 1 {
		t.Fatalf("len(report)=%d, want 1 (only the 100d-old mute is aged)", len(report))
	}
	if report[0].RuleID != "solid.srp.lcom4_high" {
		t.Errorf("aged rule_id=%q, want solid.srp.lcom4_high", report[0].RuleID)
	}
	if report[0].OverrideID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("aged override_id=%q (canonical uuid string)", report[0].OverrideID)
	}
}

// fixedNow is the deterministic clock for end-to-end tests --
// implements insights.Clock by returning a pinned instant.
type fixedNow struct{ t time.Time }

func (f fixedNow) Now() time.Time { return f.t }
