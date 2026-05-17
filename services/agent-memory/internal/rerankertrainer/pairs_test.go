package rerankertrainer

import (
	"reflect"
	"sort"
	"testing"
	"time"
)

// TestApplyActorCap_DisabledByZero asserts that
// ActorCapPerWindow<=0 disables the cap entirely -- every
// pair passes through and no drops are recorded. This guards
// against a regression that silently always-on'd the cap.
func TestApplyActorCap_DisabledByZero(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	pairs := []LabelledPair{
		{EpisodeID: "ep1", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-5 * time.Minute)},
		{EpisodeID: "ep2", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-10 * time.Minute)},
		{EpisodeID: "ep3", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-15 * time.Minute)},
	}
	out := applyActorCap(pairs, PullOpts{
		Now:               now,
		ActorCapPerWindow: 0,
		ActorCapWindow:    time.Hour,
	})
	if len(out.kept) != 3 {
		t.Fatalf("ActorCap disabled but kept=%d (want 3)", len(out.kept))
	}
	if len(out.dropped) != 0 {
		t.Fatalf("ActorCap disabled but dropped=%v", out.dropped)
	}
}

// TestApplyActorCap_NoCorrectionMarker asserts pairs with an
// empty CorrectionActor (= plain failure / degraded episodes,
// NOT correction-derived) are NEVER capped, even when the cap
// is engaged. The §6.4 contract is explicit: capping is only
// against the poisoning vector "operator floods a stale
// model with thousands of human_corrected feedback rows".
func TestApplyActorCap_NoCorrectionMarker(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	pairs := []LabelledPair{
		{EpisodeID: "ep1" /* no correction marker */},
		{EpisodeID: "ep2"},
		{EpisodeID: "ep3"},
		{EpisodeID: "ep4"},
		{EpisodeID: "ep5"},
	}
	out := applyActorCap(pairs, PullOpts{
		Now:               now,
		ActorCapPerWindow: 2,
		ActorCapWindow:    time.Hour,
	})
	if len(out.kept) != 5 {
		t.Fatalf("plain pairs should NOT be capped (kept=%d want 5)", len(out.kept))
	}
	if len(out.dropped) != 0 {
		t.Fatalf("plain pairs should NOT register drops; got %v", out.dropped)
	}
}

// TestApplyActorCap_OutsideWindow asserts an old correction
// (CorrectionUpdateAt before the sliding-window start) does
// NOT count against the cap. The cap is a sliding-window
// rate limit, not an all-time budget.
func TestApplyActorCap_OutsideWindow(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	pairs := []LabelledPair{
		// 2 within the 1h window:
		{EpisodeID: "ep1", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-10 * time.Minute)},
		{EpisodeID: "ep2", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-20 * time.Minute)},
		// 2 outside:
		{EpisodeID: "ep3", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-2 * time.Hour)},
		{EpisodeID: "ep4", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-3 * time.Hour)},
	}
	out := applyActorCap(pairs, PullOpts{
		Now:               now,
		ActorCapPerWindow: 5, // generous; we are testing window not cap
		ActorCapWindow:    time.Hour,
	})
	if len(out.kept) != 4 {
		t.Fatalf("all 4 pairs should be kept (2 in-window + 2 out-of-window): got %d", len(out.kept))
	}
	if len(out.dropped) != 0 {
		t.Fatalf("dropped should be empty: %v", out.dropped)
	}
}

// TestApplyActorCap_EngagesAtThreshold is the §6.4 scenario
// "per-operator rate cap engages": same actor produces N>cap
// correction-derived negatives inside the sliding window;
// only `cap` survive, the rest are tallied as dropped under
// the actor's label.
func TestApplyActorCap_EngagesAtThreshold(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	pairs := []LabelledPair{
		{EpisodeID: "ep1", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-5 * time.Minute)},
		{EpisodeID: "ep2", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-10 * time.Minute)},
		{EpisodeID: "ep3", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-15 * time.Minute)},
		{EpisodeID: "ep4", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-20 * time.Minute)},
		{EpisodeID: "ep5", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-25 * time.Minute)},
	}
	out := applyActorCap(pairs, PullOpts{
		Now:               now,
		ActorCapPerWindow: 2, // cap fires at 2 per hour
		ActorCapWindow:    time.Hour,
	})
	if len(out.kept) != 2 {
		t.Fatalf("cap=2 but kept=%d (%v)", len(out.kept), pairIDs(out.kept))
	}
	if got := out.dropped["operator"]; got != 3 {
		t.Fatalf("operator dropped: got %d want 3", got)
	}
	// Deterministic selection: oldest correction wins so a
	// retry with the same input set picks the same kept rows
	// (the version fingerprint depends on this).
	wantKept := []string{"ep4", "ep5"} // -20m and -25m are the oldest within-window
	gotKept := pairIDs(out.kept)
	sort.Strings(gotKept)
	if !reflect.DeepEqual(gotKept, wantKept) {
		t.Fatalf("cap selection not deterministic (oldest-first): got %v want %v",
			gotKept, wantKept)
	}
}

// TestApplyActorCap_PerActorIndependent asserts the cap is
// PER-ACTOR -- one noisy operator does NOT consume the
// budget of a quiet one. In v1 single-tenant the bucket
// label is "operator", but the path-shape applies as soon
// as the §A.5 per-human identifier ships.
func TestApplyActorCap_PerActorIndependent(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	pairs := []LabelledPair{
		// Actor A: 4 corrections (over cap=2)
		{EpisodeID: "a1", CorrectionActor: "actor-A", CorrectionUpdateAt: now.Add(-5 * time.Minute)},
		{EpisodeID: "a2", CorrectionActor: "actor-A", CorrectionUpdateAt: now.Add(-10 * time.Minute)},
		{EpisodeID: "a3", CorrectionActor: "actor-A", CorrectionUpdateAt: now.Add(-15 * time.Minute)},
		{EpisodeID: "a4", CorrectionActor: "actor-A", CorrectionUpdateAt: now.Add(-20 * time.Minute)},
		// Actor B: 1 correction (under cap)
		{EpisodeID: "b1", CorrectionActor: "actor-B", CorrectionUpdateAt: now.Add(-30 * time.Minute)},
	}
	out := applyActorCap(pairs, PullOpts{
		Now:               now,
		ActorCapPerWindow: 2,
		ActorCapWindow:    time.Hour,
	})
	if len(out.kept) != 3 { // 2 from A + 1 from B
		t.Fatalf("kept count: got %d (%v) want 3", len(out.kept), pairIDs(out.kept))
	}
	if out.dropped["actor-A"] != 2 {
		t.Fatalf("actor-A drops: got %d want 2", out.dropped["actor-A"])
	}
	if _, ok := out.dropped["actor-B"]; ok {
		t.Fatalf("actor-B should NOT register drops: %v", out.dropped)
	}
}

// TestApplyActorCap_DefaultWindowOneHour asserts an unset
// ActorCapWindow falls back to 1h (matches the tech-spec §9.4
// wording "per operator per hour").
func TestApplyActorCap_DefaultWindowOneHour(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	pairs := []LabelledPair{
		// Inside 1h (default):
		{EpisodeID: "in", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-30 * time.Minute)},
		// Just outside 1h:
		{EpisodeID: "out", CorrectionActor: "operator", CorrectionUpdateAt: now.Add(-90 * time.Minute)},
	}
	out := applyActorCap(pairs, PullOpts{
		Now:               now,
		ActorCapPerWindow: 1,
		// ActorCapWindow unset -> default 1h
	})
	if len(out.kept) != 2 {
		t.Fatalf("both pairs should be kept (in-window <=cap; out-of-window not subject): got %v",
			pairIDs(out.kept))
	}
	if len(out.dropped) != 0 {
		t.Fatalf("no drops expected: %v", out.dropped)
	}
}

// TestApplyActorCap_TieBreakDeterministic asserts that when
// two corrections share the SAME CorrectionUpdateAt timestamp,
// the tie is broken by EpisodeID lexicographic order. This
// keeps the cap deterministic so a retry over identical input
// produces the same fingerprint.
func TestApplyActorCap_TieBreakDeterministic(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ts := now.Add(-10 * time.Minute) // identical timestamp
	pairs := []LabelledPair{
		{EpisodeID: "ep-c", CorrectionActor: "operator", CorrectionUpdateAt: ts},
		{EpisodeID: "ep-a", CorrectionActor: "operator", CorrectionUpdateAt: ts},
		{EpisodeID: "ep-b", CorrectionActor: "operator", CorrectionUpdateAt: ts},
	}
	out1 := applyActorCap(pairs, PullOpts{
		Now:               now,
		ActorCapPerWindow: 1,
		ActorCapWindow:    time.Hour,
	})
	out2 := applyActorCap(pairs, PullOpts{
		Now:               now,
		ActorCapPerWindow: 1,
		ActorCapWindow:    time.Hour,
	})
	if !reflect.DeepEqual(pairIDs(out1.kept), pairIDs(out2.kept)) {
		t.Fatalf("cap selection not deterministic across calls: %v vs %v",
			pairIDs(out1.kept), pairIDs(out2.kept))
	}
	if len(out1.kept) != 1 || out1.kept[0].EpisodeID != "ep-a" {
		t.Fatalf("tie-break should pick lexicographically-first id 'ep-a'; got %v",
			pairIDs(out1.kept))
	}
}

func pairIDs(pairs []LabelledPair) []string {
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.EpisodeID
	}
	return out
}
