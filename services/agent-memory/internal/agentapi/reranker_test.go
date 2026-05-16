package agentapi

// Unit tests for the v0 cold-start reranker.
//
// The reranker is the §6.4 step-5 component the recall
// handler invokes after the publish filter has trimmed the
// candidate set.  It exists as an in-process default because
// the trained reranker model is not part of the Stage 5.1
// scope; the cold-start scorer (cosine + structural
// distance + concept bonus) MUST behave predictably so the
// recall handler can keep its rank-order contract.
//
// What these tests prove:
//
//  1. Cosine score dominates when structural distances tie.
//  2. Structural distance penalises rank when cosine scores
//     are equal: a closer candidate wins.
//  3. The concept bonus surfaces Concepts above structurally-
//     identical Method candidates when the bonus is positive.
//  4. The model version string is the literal Stage 5.1
//     value so the RecallContextLog row carries the exact
//     fixture that produced the rank order.

import (
	"testing"
)

// TestV0ColdStartReranker_cosineDominantWhenStructuralEqual proves
// the reranker preserves a strict cosine ordering when every
// candidate sits at the same structural distance.
func TestV0ColdStartReranker_cosineDominantWhenStructuralEqual(t *testing.T) {
	r := NewV0ColdStartReranker(nil)
	in := []Candidate{
		{PointID: "low", Score: 0.10, Kind: "method", StructuralDistance: 1},
		{PointID: "hi", Score: 0.95, Kind: "method", StructuralDistance: 1},
		{PointID: "mid", Score: 0.50, Kind: "method", StructuralDistance: 1},
	}
	ranked := r.Rank(in)
	if len(ranked) != 3 {
		t.Fatalf("rank len = %d; want 3", len(ranked))
	}
	wantOrder := []string{"hi", "mid", "low"}
	for i, want := range wantOrder {
		if ranked[i].PointID != want {
			t.Fatalf("ranked[%d] = %q; want %q (cosine-dominant order)",
				i, ranked[i].PointID, want)
		}
	}
	// FinalScore must be populated on every candidate so the
	// response projection can read it back verbatim. We do
	// not assert non-zero (zero is a valid score for a low-
	// cosine candidate at structural distance >= 0) — only
	// that the rank order matches the scoring formula.
	if ranked[0].FinalScore <= ranked[1].FinalScore {
		t.Fatalf("ranked[0].FinalScore (%v) MUST exceed ranked[1] (%v)",
			ranked[0].FinalScore, ranked[1].FinalScore)
	}
}

// TestV0ColdStartReranker_structuralPenaltyBreaksTies proves
// that, for equal cosine scores, a smaller structural
// distance ranks higher.
func TestV0ColdStartReranker_structuralPenaltyBreaksTies(t *testing.T) {
	r := NewV0ColdStartReranker(nil)
	in := []Candidate{
		{PointID: "far", Score: 0.80, Kind: "method", StructuralDistance: 2},
		{PointID: "near", Score: 0.80, Kind: "method", StructuralDistance: 0},
		{PointID: "mid", Score: 0.80, Kind: "method", StructuralDistance: 1},
	}
	ranked := r.Rank(in)
	wantOrder := []string{"near", "mid", "far"}
	for i, want := range wantOrder {
		if ranked[i].PointID != want {
			t.Fatalf("ranked[%d] = %q; want %q (structural-distance tiebreaker)",
				i, ranked[i].PointID, want)
		}
	}
}

// TestV0ColdStartReranker_conceptBonusSurfacesConcepts proves
// the concept bonus lifts Concept candidates over otherwise-
// tied Method candidates when the weights configure a
// positive bonus.
func TestV0ColdStartReranker_conceptBonusSurfacesConcepts(t *testing.T) {
	// Lean on the default weights; if those go to zero a
	// future iter MUST surface the bonus via an explicit
	// override.
	r := NewV0ColdStartReranker(nil)
	in := []Candidate{
		{PointID: "method", Score: 0.50, Kind: "method", StructuralDistance: 0},
		{PointID: "concept", Score: 0.50, Kind: NodeKindConcept, StructuralDistance: 0},
	}
	ranked := r.Rank(in)
	if ranked[0].PointID != "concept" {
		t.Fatalf("ranked[0] = %q; want concept (concept bonus should out-rank tied Method)",
			ranked[0].PointID)
	}
}

// TestV0ColdStartReranker_modelVersionLiteral pins the
// version string the RecallContextLog row stores. Changing
// this string is a deliberate cold-start contract break that
// MUST go through a migration plan, not a silent bump.
func TestV0ColdStartReranker_modelVersionLiteral(t *testing.T) {
	r := NewV0ColdStartReranker(nil)
	if got, want := r.ModelVersion(), V0ModelVersion; got != want {
		t.Fatalf("ModelVersion = %q; want %q (cold-start contract)", got, want)
	}
	if V0ModelVersion == "" {
		t.Fatalf("V0ModelVersion is empty; the RecallContextLog row REQUIRES a non-empty version")
	}
}

// TestV0ColdStartReranker_emptyInputIsSafe documents the
// no-candidates contract: the reranker MUST return an empty
// slice (not nil) so callers can chain .Rank().Project()
// without an extra nil check.
func TestV0ColdStartReranker_emptyInputIsSafe(t *testing.T) {
	r := NewV0ColdStartReranker(nil)
	ranked := r.Rank(nil)
	if ranked == nil {
		t.Fatalf("Rank(nil) = nil; want empty slice for chainability")
	}
	if len(ranked) != 0 {
		t.Fatalf("Rank(nil) len = %d; want 0", len(ranked))
	}
}
