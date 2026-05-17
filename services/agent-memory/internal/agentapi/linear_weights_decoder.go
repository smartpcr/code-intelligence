// Package agentapi: LinearWeights artifact decoder.
//
// Stage 6.4 (impl-plan §1113-1115) requires the recall path
// to consume the trained `reranker_model` artifact on every
// `agent.recall` request — not just advertise the version.
// The trainer's `LinearTrainer` emits artifacts as inlined
// `data:application/json;base64,...` URIs carrying a
// `LinearWeights` JSON document. This file is the recall-side
// decoder that:
//
//  1. recognises the `data:application/json;base64,` URI
//     scheme,
//  2. decodes the embedded LinearWeights via the canonical
//     trainer-side decoder (so the recall path cannot drift
//     from the trainer-side schema invariant),
//  3. returns a request-scoped Reranker whose Rank() scores
//     each Candidate via the learned weight vector applied
//     to the shared 6-dim feature space documented on
//     `rerankertrainer.linearFeatureNames`.
//
// The sidecar (BERT cross-encoder) trainer emits a
// different URI scheme (`file:///<artifact_dir>/<version>`)
// — those are unrecognised here and handled by the
// `BertSidecarDecoder` (`bert_sidecar_decoder.go`) that lives
// alongside this one. The two decoders are composed via the
// `MultiArtifactDecoder` chain at agent-api startup. The
// inline construction lives at `cmd/agent-api/main.go`
// (the `decoderChain := agentapi.NewMultiArtifactDecoder(...)`
// expression around the reranker / coldStart wiring), so
// each request resolves to the decoder whose URI scheme
// matches the published `reranker_model.artifact_uri`.
package agentapi

import (
	"errors"
	"sort"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/rerankertrainer"
)

// LinearWeightsURIPrefix is the data-URI prefix the
// LinearTrainer's `buildLinearOutput` emits. Mirrored here as
// a constant so the decoder can early-skip unrecognised URIs
// without paying the cost of a base64 decode attempt.
const LinearWeightsURIPrefix = "data:application/json;base64,"

// NewLinearWeightsDecoder returns the ArtifactDecoder that
// recognises the LinearTrainer's artifact URI scheme.
// Constructed once at agent-api startup and passed to
// NewPublishedReranker; the decoder is stateless and safe
// for concurrent use.
func NewLinearWeightsDecoder() ArtifactDecoder {
	return ArtifactDecoderFunc(decodeLinearWeightsArtifact)
}

// decodeLinearWeightsArtifact is the ArtifactDecoderFunc body.
// Returns (_, false, nil) when the URI is not the inlined
// data: scheme — the PublishedReranker treats this as "this
// decoder does not handle that artifact format" and falls
// back to the cold-start path. Returns (_, false, err) when
// the URI matches the scheme but the payload is corrupt; the
// PublishedReranker treats this as a transient anomaly and
// still falls back without hard-failing the recall request.
func decodeLinearWeightsArtifact(uri string) (Reranker, bool, error) {
	if len(uri) <= len(LinearWeightsURIPrefix) || uri[:len(LinearWeightsURIPrefix)] != LinearWeightsURIPrefix {
		return nil, false, nil
	}
	weights, err := rerankertrainer.DecodeLinearWeights(uri)
	if err != nil {
		return nil, false, err
	}
	if len(weights.Weights) != rerankertrainer.FeatureCount() {
		return nil, false, errors.New("agentapi: decoded LinearWeights vector length mismatch")
	}
	return &linearWeightsReranker{weights: weights}, true, nil
}

// linearWeightsReranker is the request-scoped Reranker the
// LinearWeights decoder produces. Holds the decoded weight
// vector and applies it to each Candidate via the shared
// feature-extractor `rerankertrainer.ExtractCandidateFeatures`.
//
// Scoring formula (recall side):
//
//	final = bias + dot(weights, features) - D · distance
//
// where `features` is the 6-dim shared feature vector and
// `D = rerankertrainer.DistancePenaltyResidual`. The
// distance residual is added on top of the learned score
// (rather than baked into a learned feature) because the
// training data carries no recall-time structural distance
// signal — see the doc-comment on
// `rerankertrainer.linearFeatureNames` for the rationale.
type linearWeightsReranker struct {
	weights rerankertrainer.LinearWeights
}

// Rank scores every candidate via the learned weight vector
// and returns the slice sorted by FinalScore descending.
// Stable sort: candidates with identical scores preserve
// their input order so the result is deterministic across
// runs.
func (l *linearWeightsReranker) Rank(candidates []Candidate) []Candidate {
	out := make([]Candidate, len(candidates))
	copy(out, candidates)
	for i := range out {
		features := rerankertrainer.ExtractCandidateFeatures(
			out[i].Kind,
			float64(out[i].Score),
			out[i].StructuralDistance,
		)
		learned := rerankertrainer.ScoreCandidate(l.weights, features[:])
		// V0-style distance residual: training data is
		// always-0 for distance, so the learned weight on
		// that feature is also 0. Apply the documented
		// constant penalty here so a trained model NEVER
		// regresses worse than V0 on a candidate set whose
		// only differentiator is structural distance.
		residual := rerankertrainer.DistancePenaltyResidual * float64(out[i].StructuralDistance)
		out[i].FinalScore = float32(learned - residual)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].FinalScore > out[j].FinalScore
	})
	return out
}

// ModelVersion returns the trainer-tag stamped into the
// LinearWeights artifact. Note: PublishedReranker.ModelVersion
// already advertises the reranker_model.version on the
// envelope (the trainer-tag here is the model class, not the
// per-publish version). This impl exists so the linearWeightsReranker
// satisfies the Reranker interface; it is NEVER invoked on
// the recall hot path because PublishedReranker.ModelVersion
// always reads from the source, not the inner scorer.
func (l *linearWeightsReranker) ModelVersion() string {
	if l.weights.TrainerTag == "" {
		return "linear"
	}
	return l.weights.TrainerTag
}
