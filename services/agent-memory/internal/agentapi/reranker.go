// Package agentapi: v0 cold-start reranker.
//
// Stage 5.1 step 7 of implementation-plan.md mandates a
// "v0 cold-start reranker: pure cosine + structural distance
// fallback per risk §9.5; loaded if no published
// `reranker_model` row exists." Risk §9.5 names this as the
// safe default for a freshly-onboarded repo whose Episode
// stream is too thin for a trained cross-encoder to
// outperform a hand-tuned linear model.
//
// Cold-start scoring formula
// --------------------------
// For each candidate Hit `h` returned by the mixed-collection
// search (Method / Block / Concept), the reranker computes:
//
//	final_score(h) =   CosineWeight  * cos(h)
//	                 - StructuralWeight * structural_distance(h)
//	                 + KindPrior(h.kind)
//
// where:
//
//	cos(h)                  = h.Score (Qdrant cosine similarity,
//	                          higher = more similar).
//	structural_distance(h)  = 0 for a seed hit, +1 per expansion
//	                          hop from the seed (so a 1-hop
//	                          neighbor scores below a direct
//	                          match, a 2-hop neighbor below
//	                          that, etc.).
//	KindPrior(kind)         = small additive bias by node kind
//	                          (Concepts get a tiny boost over
//	                          raw Method hits because they
//	                          embody distilled support).
//
// Defaults reflect the Stage 5.1 cold-start judgement:
//
//	CosineWeight     = 1.0   (the dominant signal pre-training)
//	StructuralWeight = 0.1   (expansion costs ~10% of one cos
//	                          unit per hop -- enough to prefer
//	                          a direct hit over a 1-hop fanout
//	                          but not so much that a strong
//	                          structural neighbor is suppressed)
//	KindPrior:
//	  - "concept"            +0.05 (distilled signal bonus)
//	  - "method" / "block"   +0.00
//
// The version string the reranker reports is
// "v0-cold-start" so the e2e scenario
// "Given a reranker_model row exists with version='v0-cold-start'"
// (e2e-scenarios.md §1 Background line 68) holds against this
// implementation when no trained row supersedes it.
package agentapi

import (
	"sort"
)

// Reranker scores and orders a candidate set.
// Implementations MUST be deterministic for the same input.
type Reranker interface {
	// Rank returns the input candidates re-ordered by the
	// reranker's final score (descending).  The returned slice
	// MUST NOT contain entries the input did not.  The caller
	// owns trimming to `k`.
	Rank(candidates []Candidate) []Candidate
	// ModelVersion is the stable identifier the recall handler
	// records on the RecallContextLog row and surfaces on the
	// `reranker_model_version` field of the response envelope.
	ModelVersion() string
}

// Candidate is the per-entry input shape the reranker sees.
// Carries the raw cosine score from Qdrant, the structural
// distance from the seed (0 for direct hits, +1 per
// expansion hop), and enough payload to pass through to the
// response envelope.
type Candidate struct {
	// PointID is the Qdrant point id (textual UUID) — the
	// stable handle used by the §9.6a publish filter.
	PointID string
	// Score is the raw cosine similarity Qdrant returned
	// (higher = more similar).
	Score float32
	// Kind is one of "method" / "block" / "concept".
	Kind string
	// StructuralDistance is the integer hop count from the
	// nearest seed hit.  0 for seed hits; the expansion path
	// (Stage 5.1 step 4) tags expanded candidates with 1 or
	// 2 depending on the configured expansion depth.
	StructuralDistance int
	// Payload echoes the Qdrant payload so the recall
	// handler can dereference node_id / repo_id / etc on
	// the way back out without re-querying.
	Payload map[string]any
	// FinalScore is populated by the reranker on the return
	// path; callers read this for the ordering and the
	// per-hit response score.
	FinalScore float32
}

// V0ColdStartReranker implements the §9.5 cold-start
// fallback. Construct with `NewV0ColdStartReranker(nil)` for
// the defaults; pass a non-nil `Weights` to override any
// subset.  Safe for concurrent use (state is immutable
// post-construction).
type V0ColdStartReranker struct {
	weights V0Weights
}

// V0Weights configures the cold-start reranker.  Zero-valued
// fields are replaced with the doc-comment defaults; this
// makes "I just want to tweak StructuralWeight" a one-field
// override rather than re-specifying every weight.
type V0Weights struct {
	// CosineWeight scales the Qdrant cosine score before it
	// enters the final sum.  Defaults to 1.0.
	CosineWeight float32
	// StructuralWeight scales the structural-distance
	// penalty.  Defaults to 0.1.
	StructuralWeight float32
	// ConceptKindBonus is the additive bias granted to
	// Concept hits (architecture.md §5.5.1 — distilled
	// support).  Defaults to 0.05.
	ConceptKindBonus float32
}

// v0DefaultWeights pins the cold-start defaults that the
// e2e-scenarios "cold start" Gherkin holds against.  Kept
// private so the doc-comment defaults and the actual
// fallback values cannot drift.
var v0DefaultWeights = V0Weights{
	CosineWeight:     1.0,
	StructuralWeight: 0.1,
	ConceptKindBonus: 0.05,
}

// NewV0ColdStartReranker constructs the cold-start fallback.
// A nil `weights` selects the production defaults.  A
// non-nil `weights` keeps any zero-valued field at its
// default so callers can override one knob without
// re-listing the rest.
func NewV0ColdStartReranker(weights *V0Weights) *V0ColdStartReranker {
	w := v0DefaultWeights
	if weights != nil {
		if weights.CosineWeight != 0 {
			w.CosineWeight = weights.CosineWeight
		}
		if weights.StructuralWeight != 0 {
			w.StructuralWeight = weights.StructuralWeight
		}
		if weights.ConceptKindBonus != 0 {
			w.ConceptKindBonus = weights.ConceptKindBonus
		}
	}
	return &V0ColdStartReranker{weights: w}
}

// V0ModelVersion is the stable identifier the cold-start
// reranker reports on every recall response.  Pinned as a
// constant so the e2e-scenarios.md §1 background assertion
// (`reranker_model.version='v0-cold-start'`) is satisfied
// even when no `reranker_model` row exists in the database
// yet — the recall path falls back to this version literal.
const V0ModelVersion = "v0-cold-start"

// ModelVersion implements Reranker.
func (r *V0ColdStartReranker) ModelVersion() string { return V0ModelVersion }

// Rank computes the v0 score for each candidate, writes it
// to `FinalScore`, and returns the slice sorted by
// FinalScore descending.  Stable sort: candidates with
// identical scores preserve their input order so the result
// is deterministic across runs.
func (r *V0ColdStartReranker) Rank(candidates []Candidate) []Candidate {
	out := make([]Candidate, len(candidates))
	copy(out, candidates)
	for i := range out {
		out[i].FinalScore = r.scoreOne(out[i])
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].FinalScore > out[j].FinalScore
	})
	return out
}

func (r *V0ColdStartReranker) scoreOne(c Candidate) float32 {
	score := r.weights.CosineWeight * c.Score
	score -= r.weights.StructuralWeight * float32(c.StructuralDistance)
	if c.Kind == NodeKindConcept {
		score += r.weights.ConceptKindBonus
	}
	return score
}

// NodeKindConcept is the kind value Concept candidates carry
// when they enter the reranker.  Mirrors the publisher
// constant `embedding.CollectionConcept` ("agent_memory_concept")
// at the type level.
const NodeKindConcept = "concept"
