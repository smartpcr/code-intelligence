package rerankertrainer

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"time"
)

// LinearTrainer is the in-process linear-logistic-regression
// baseline Trainer. It is the dev/CI default — the
// production deployment runs the BERT cross-encoder via
// `SidecarTrainer` (set `AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT`
// + `AGENT_MEMORY_RERANKER_TRAINER_KIND=sidecar`).
//
// As of iter-6, the binary (`cmd/reranker-trainer/main.go`)
// REFUSES to start when neither `AGENT_MEMORY_RERANKER_TRAINER_ENDPOINT`
// nor `AGENT_MEMORY_RERANKER_TRAINER_KIND` is set — operators
// must explicitly opt in to this baseline by setting
// `AGENT_MEMORY_RERANKER_TRAINER_KIND=linear` (the previous
// silent linear default was a production footgun because the
// binary would happily train a ≪200M baseline where the
// operator expected the BERT cross-encoder, and the §6.4
// brief mandates the BERT trainer).
//
// Why a linear baseline at all: the cross-encoder lives in
// Python (transformers + torch). The Go service hosts this
// in-process linear baseline to keep CI hermetic and to
// provide a non-trivial signal when the sidecar is down or
// has not yet been stood up. Operators wire the BERT model
// via `SidecarTrainer`, which POSTs the labelled pairs to a
// configured HTTP endpoint and stores the returned artifact
// blob verbatim. Both paths share the same `Trainer` contract
// and feed the same `reranker_model` schema, so the recall
// hot path is agnostic to which trainer produced the weights.
//
// The linear baseline is bounded WELL under the 200M-param
// requirement cap: the feature vector is 6-dimensional (see
// `linearFeatureDim` + the feature-index constants below for
// the exact schema), so the fitted model is a fixed-size
// 6-float weight vector plus bias.
//
// Determinism: the same TrainingInput always produces the same
// fitted weights -- the SGD seed is derived from
// DeriveVersion(in) (which already hashes the pair set) so the
// version fingerprint and the artifact bytes co-vary. This is
// the property the §6.4 step-4 "ON CONFLICT DO NOTHING"
// idempotency contract relies on.
type LinearTrainer struct {
	// MaxEpochs caps the SGD pass count. Defaults to 50 if
	// zero; tighter loops are a knob for tests that want
	// short iterations.
	MaxEpochs int

	// LearningRate is the SGD step size. Defaults to 0.1
	// if zero.
	LearningRate float64

	// HoldoutFraction is the fraction of pairs reserved as
	// the eval split (deterministically partitioned by a
	// hash of EpisodeID). Defaults to 0.1 (10%) if zero.
	// The eval split drives mrr/recall/rank-of-correct-node
	// reporting; if the fraction sets aside 0 pairs in
	// practice (tiny training sets), the metrics fall back
	// to the train split so the row still carries non-zero
	// values.
	HoldoutFraction float64

	// TagOverride lets a caller pin the Trainer Tag for
	// fingerprint reproducibility. Defaults to "linear" if
	// zero-valued.
	TagOverride string
}

// linearFeatureDim is the fixed feature-vector size the
// reranker scoring uses. Exported via FeatureCount so the
// recall-side consumer can validate the published weights
// vector matches.
//
// The schema is INTENTIONALLY symmetric across training and
// recall — see linearFeatureNames for the documented index
// mapping. A 6-dim shared vector lets the LinearTrainer
// learn weights at the Episode/Observation level AND lets
// the recall path (`agentapi.PublishedReranker`) extract the
// SAME vector shape per Candidate so the trained weights are
// genuinely consumed at rank-time instead of being a label
// the recall path advertises but ignores.
const linearFeatureDim = 6

// FeatureCount returns the linear model's feature dimension
// (matches LinearWeights.Weights length).
func FeatureCount() int { return linearFeatureDim }

// LinearWeights is the JSON-serialisable shape that lives in
// `reranker_model.artifact_uri` (inlined as
// `data:application/json;base64,...`). The recall-side
// `PublishedReranker` decodes this struct and applies the
// affine transform to each candidate.
type LinearWeights struct {
	// Schema is a literal pinning the on-disk format so
	// the recall-side decoder can refuse a payload of a
	// future trainer shape without crashing.
	Schema string `json:"schema"`

	// Weights is the fitted per-feature weight vector,
	// length FeatureCount().
	Weights []float64 `json:"weights"`

	// Bias is the intercept term added after the dot
	// product.
	Bias float64 `json:"bias"`

	// FeatureNames documents the feature index order so
	// any debugging session can interpret a weight vector
	// without re-deriving the order from source.
	FeatureNames []string `json:"feature_names"`

	// TrainerTag echoes the trainer's Tag() at fit time
	// (e.g. "linear", "sidecar:bert-base"). Carried inside
	// the artifact so a misconfigured recall-side loader
	// surfaces the trainer-class mismatch in its error
	// message.
	TrainerTag string `json:"trainer_tag"`

	// FitMetrics carries the same metrics that get written
	// to `reranker_model.metrics_json` -- duplicated inside
	// the artifact so an out-of-band consumer that fetches
	// only the artifact still sees the eval numbers.
	FitMetrics map[string]float64 `json:"fit_metrics"`
}

// LinearWeightsSchemaV2 is the on-disk schema literal for the
// current LinearWeights JSON document. Bumped from v1 to v2
// when the feature space was redesigned to the candidate-
// overlapping 6-dim shared schema (the v1 8-dim Episode-only
// vector could not be evaluated against a Candidate at
// recall time). The recall-side decoder pins on this literal.
const LinearWeightsSchemaV2 = "rerankertrainer.linear/v2"

// LinearWeightsSchemaV1 is the prior schema literal, retained
// as a constant so test fixtures + migration tooling can
// reference it. Decoders MUST reject v1 payloads — the
// feature semantics changed incompatibly.
const LinearWeightsSchemaV1 = "rerankertrainer.linear/v1"

// linearFeatureNames documents the index→name mapping that
// drives BOTH per-pair training-vector extraction
// (`extractFeatures`) AND per-Candidate recall-vector
// extraction (`ExtractCandidateFeatures`). Kept as the single
// source of truth so the trainer and the recall path cannot
// disagree on offset 0 vs offset 1.
//
// Symmetric semantics (TRAIN/RECALL):
//   - bias         : always 1.0 / always 1.0
//   - kind_concept : 1 if any Observation.Role=="concept_hit" /
//                    1 if Candidate.Kind=="concept"
//   - kind_node    : 1 if any Observation.Role IN
//                    {"node_hit","call_edge_hit"} / 1 if
//                    Candidate.Kind IN {"method","block"}
//   - kind_edge    : 1 if any Observation.Role=="edge_hit" /
//                    0 (edges are not surfaced as Candidates;
//                    this feature is train-only and the
//                    learned weight is wasted at recall —
//                    accepted because the model is still
//                    correct, just slightly sparser)
//   - score_signal : mean(Observation.Weight) clamped [0,1] /
//                    Candidate.Score clamped [0,1]
//   - distance_pen : 0 (training labels carry no recall-time
//                    structural context) /
//                    float64(Candidate.StructuralDistance)
//                    clamped to [0,3]. The trained weight
//                    will be ~0 because the train signal is
//                    constant; recall-side scoring falls back
//                    to V0's distance penalty via the
//                    rerankertrainer.DistancePenaltyResidual
//                    constant so the trained model still
//                    suppresses distant hits.
var linearFeatureNames = []string{
	"bias",
	"kind_concept",
	"kind_node",
	"kind_edge",
	"score_signal",
	"distance_penalty",
}

// Feature index offsets — kept as named constants so
// extractFeatures / ExtractCandidateFeatures / ScoreCandidate
// do not silently drift if the schema is reordered.
const (
	featureBias            = 0
	featureKindConcept     = 1
	featureKindNode        = 2
	featureKindEdge        = 3
	featureScoreSignal     = 4
	featureDistancePenalty = 5
)

// DistancePenaltyResidual is the V0-style additive distance
// penalty PublishedReranker applies on top of the learned
// score when ranking Candidates. Pinned to match
// `agentapi.V0Weights.StructuralWeight` (0.1) so a trained
// model never regresses worse than V0 on a candidate set
// whose only differentiator is structural distance — the
// trained features simply add a learned kind/score prior on
// top of V0's distance baseline.
const DistancePenaltyResidual = 0.1

// Train implements Trainer.
func (t LinearTrainer) Train(_ context.Context, in TrainingInput) (TrainingOutput, error) {
	version := DeriveVersion(in)

	maxEpochs := t.MaxEpochs
	if maxEpochs <= 0 {
		maxEpochs = 50
	}
	lr := t.LearningRate
	if lr <= 0 {
		lr = 0.1
	}
	hold := t.HoldoutFraction
	if hold <= 0 {
		hold = 0.1
	}
	if hold >= 1 {
		hold = 0.5
	}

	// Materialise (features, label) once so we do not
	// re-extract on every epoch. Uses the package-level
	// evalSample type so the metrics evaluator can consume
	// the same slice without a per-sample copy.
	all := make([]evalSample, 0, len(in.Positives)+len(in.Negatives))
	addAll := func(pairs []LabelledPair, label float64) {
		for _, p := range pairs {
			v := extractFeatures(p, in.WindowEnd)
			holdout := deterministicHoldout(p.EpisodeID, hold)
			all = append(all, evalSample{x: v, y: label, hold: holdout, epID: p.EpisodeID})
		}
	}
	addAll(in.Positives, +1.0)
	addAll(in.Negatives, -1.0)

	// Deterministic ordering: by EpisodeID. SGD over
	// deterministically-ordered samples is reproducible
	// without an RNG seed -- the same TrainingInput
	// produces the same fit.
	sort.Slice(all, func(i, j int) bool { return all[i].epID < all[j].epID })

	weights := make([]float64, linearFeatureDim)
	bias := 0.0
	trainLoss := 0.0

	if len(all) == 0 {
		// Empty training set: emit a zero model rather
		// than failing. The Service's MinEpisodes gate
		// normally precludes this, but the trainer must
		// degrade gracefully when MinEpisodes is set very
		// low in dev.
		out := buildLinearOutput(t.tag(), version, weights, bias,
			map[string]float64{
				"train_loss":                0,
				"eval_ndcg@k":               0,
				"rank-of-correct-node@k=20": 0,
				"positives":                 0,
				"negatives":                 0,
				"pairs_trained":             0,
			})
		return out, nil
	}

	// Mini-batch SGD with logistic loss. The per-sample
	// gradient is (sigmoid(y * f(x)) - 1) * y * x.
	for epoch := 0; epoch < maxEpochs; epoch++ {
		var lossSum float64
		var nSeen int
		for _, s := range all {
			if s.hold {
				continue
			}
			z := dot(weights, s.x[:]) + bias
			yz := s.y * z
			// numerically stable log(1+exp(-yz))
			loss := softplus(-yz)
			lossSum += loss
			nSeen++
			// gradient of log(1 + exp(-y*z)) wrt z =
			//   -y * sigmoid(-y*z)
			gradZ := -s.y * sigmoid(-yz)
			for k := 0; k < linearFeatureDim; k++ {
				weights[k] -= lr * gradZ * s.x[k]
			}
			bias -= lr * gradZ
		}
		if nSeen > 0 {
			trainLoss = lossSum / float64(nSeen)
		}
	}

	metrics := evaluateLinearFit(all, weights, bias, trainLoss)
	out := buildLinearOutput(t.tag(), version, weights, bias, metrics)
	return out, nil
}

// Tag implements Trainer.
func (t LinearTrainer) Tag() string { return t.tag() }

func (t LinearTrainer) tag() string {
	if t.TagOverride != "" {
		return t.TagOverride
	}
	return "linear"
}

// buildLinearOutput packages the fitted weights into a
// TrainingOutput with the artifact inlined as a
// `data:application/json;base64,...` URI. The recall-side
// PublishedReranker decodes this URI to reconstruct the
// scoring function with no filesystem dependency.
func buildLinearOutput(
	tag, version string,
	weights []float64, bias float64,
	metrics map[string]float64,
) TrainingOutput {
	artifact := LinearWeights{
		Schema:       LinearWeightsSchemaV2,
		Weights:      append([]float64(nil), weights...),
		Bias:         bias,
		FeatureNames: linearFeatureNames,
		TrainerTag:   tag,
		FitMetrics:   metrics,
	}
	payload, _ := json.Marshal(artifact)
	uri := "data:application/json;base64," +
		base64.StdEncoding.EncodeToString(payload)
	return TrainingOutput{
		Version:       version,
		ArtifactURI:   uri,
		Metrics:       metrics,
		PublishStatus: StatusPublished,
	}
}

// extractFeatures builds the linear model's training-time
// feature vector from a LabelledPair. The mapping is
// documented on `linearFeatureNames` — see that comment for
// the train↔recall semantics.
//
// `windowEnd` is unused in the v2 schema (recency was dropped
// because there is no Candidate-side analogue at recall time
// — the v1 8-dim model included it as a recency_days feature
// but the trained weight was uninterpretable at recall). The
// parameter is retained for ABI continuity with the
// evaluator harness.
func extractFeatures(p LabelledPair, _ time.Time) [linearFeatureDim]float64 {
	var v [linearFeatureDim]float64
	v[featureBias] = 1.0

	// Kind indicators are mutually compatible (an Episode
	// can have BOTH concept_hit and node_hit observations);
	// the trained weights pick up the per-kind prior even
	// when multiple kinds co-occur.
	var sumWeight float64
	var nWeight int
	for _, o := range p.Observations {
		switch o.Role {
		case "concept_hit":
			v[featureKindConcept] = 1
		case "node_hit", "call_edge_hit":
			v[featureKindNode] = 1
		case "edge_hit":
			v[featureKindEdge] = 1
		}
		if o.Weight > 0 {
			sumWeight += o.Weight
			nWeight++
		}
	}
	if nWeight > 0 {
		mean := sumWeight / float64(nWeight)
		v[featureScoreSignal] = clamp01(mean)
	}
	// distance_penalty stays 0 at train time — no recall
	// context attached to a LabelledPair.
	return v
}

// ExtractCandidateFeatures builds the recall-time feature
// vector for a single Candidate using the SAME index mapping
// as extractFeatures. Pure (no I/O); safe to call from the
// recall hot path.
//
// Takes primitives (kind / score / structuralDistance) rather
// than the `agentapi.Candidate` struct so this package stays
// import-cycle free — the recall-side caller adapts its
// Candidate fields to these primitives at the call site.
func ExtractCandidateFeatures(kind string, score float64, structuralDistance int) [linearFeatureDim]float64 {
	var v [linearFeatureDim]float64
	v[featureBias] = 1.0
	switch kind {
	case "concept":
		v[featureKindConcept] = 1
	case "method", "block":
		v[featureKindNode] = 1
	}
	// kind_edge stays 0 at recall — edges are not surfaced
	// as Candidates. Documented on linearFeatureNames.
	v[featureScoreSignal] = clamp01(score)
	d := float64(structuralDistance)
	if d < 0 {
		d = 0
	}
	if d > 3 {
		d = 3
	}
	v[featureDistancePenalty] = d
	return v
}

// clamp01 collapses a real-valued signal into [0,1]. Used
// for the score_signal feature so a stray cosine outside
// [0,1] (rare; Qdrant should keep cosine in [-1,1]) does
// not skew the trained weights.
func clamp01(v float64) float64 {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 1
	}
	return v
}

// ScoreCandidate computes the linear model's raw score for a
// feature vector. Used by the recall-side PublishedReranker.
// Returns +Inf-safe (NaN guarded) output so a corrupt artifact
// cannot produce a candidate ranking that violates Go's sort
// invariants.
func ScoreCandidate(w LinearWeights, features []float64) float64 {
	if len(features) != len(w.Weights) {
		return math.Inf(-1)
	}
	s := w.Bias + dot(w.Weights, features)
	if math.IsNaN(s) || math.IsInf(s, 0) {
		return 0
	}
	return s
}

// evaluateLinearFit computes the §6.4 contract metric set
// (train_loss, eval_ndcg@k, rank-of-correct-node@k=20) plus
// supplementary mrr/recall@k. Uses the held-out split when
// non-empty; falls back to the train split otherwise so the
// metrics row still carries non-zero numbers on small dev
// datasets.
func evaluateLinearFit(
	samples []evalSample,
	weights []float64, bias float64,
	trainLoss float64,
) map[string]float64 {
	posCount := 0
	negCount := 0
	for _, s := range samples {
		if s.y > 0 {
			posCount++
		} else {
			negCount++
		}
	}

	holdout := make([]evalSample, 0)
	train := make([]evalSample, 0)
	for _, s := range samples {
		if s.hold {
			holdout = append(holdout, s)
		} else {
			train = append(train, s)
		}
	}
	evalSet := holdout
	if len(evalSet) == 0 {
		evalSet = train
	}

	// Score each eval sample. The rank metrics treat the
	// score as a "is this a positive?" decision, ordering
	// the eval set descending. rank-of-correct-node@k=20
	// is the average 1-indexed rank of true positives
	// inside the top 20.
	type scored struct {
		score float64
		y     float64
	}
	out := make([]scored, len(evalSet))
	for i, s := range evalSet {
		out[i] = scored{
			score: bias + dot(weights, s.x[:]),
			y:     s.y,
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].score > out[j].score
	})

	const k = 20
	var (
		rankSum  float64
		hits     int
		mrr      float64
		recallAt int
	)
	for rank0, sc := range out {
		if sc.y <= 0 {
			continue
		}
		if rank0 < k {
			rankSum += float64(rank0 + 1)
			hits++
			if mrr == 0 {
				mrr = 1.0 / float64(rank0+1)
			}
			recallAt++
		}
	}
	var avgRank float64
	if hits > 0 {
		avgRank = rankSum / float64(hits)
	}
	totalPositives := 0
	for _, sc := range out {
		if sc.y > 0 {
			totalPositives++
		}
	}
	var recall float64
	if totalPositives > 0 {
		recall = float64(recallAt) / float64(totalPositives)
	}

	// NDCG@k: simple binary-relevance variant. dcg = sum(rel/log2(rank+2)); idcg = sum over the top-k positives.
	dcg := 0.0
	for rank0 := 0; rank0 < len(out) && rank0 < k; rank0++ {
		if out[rank0].y > 0 {
			dcg += 1.0 / math.Log2(float64(rank0+2))
		}
	}
	idealRel := totalPositives
	if idealRel > k {
		idealRel = k
	}
	idcg := 0.0
	for i := 0; i < idealRel; i++ {
		idcg += 1.0 / math.Log2(float64(i+2))
	}
	ndcg := 0.0
	if idcg > 0 {
		ndcg = dcg / idcg
	}

	return map[string]float64{
		"train_loss":                trainLoss,
		"eval_ndcg@k":               ndcg,
		"eval_mrr@k=10":             mrr,
		"eval_recall@k=20":          recall,
		"rank-of-correct-node@k=20": avgRank,
		"positives":                 float64(posCount),
		"negatives":                 float64(negCount),
		"pairs_trained":             float64(len(train)),
		"pairs_evaluated":           float64(len(evalSet)),
	}
}

// evalSample is the internal alias for the per-sample struct
// inside Train. Kept as a package-level type so
// evaluateLinearFit can take it as a parameter.
type evalSample struct {
	x    [linearFeatureDim]float64
	y    float64
	hold bool
	epID string
}

func dot(a []float64, b []float64) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	s := 0.0
	for i := 0; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}

func sigmoid(z float64) float64 {
	if z >= 0 {
		ez := math.Exp(-z)
		return 1 / (1 + ez)
	}
	ez := math.Exp(z)
	return ez / (1 + ez)
}

// softplus is log(1 + exp(z)) computed in a numerically
// stable way so a very negative z does not underflow and a
// very positive z does not overflow.
func softplus(z float64) float64 {
	if z > 30 {
		return z
	}
	if z < -30 {
		return 0
	}
	return math.Log1p(math.Exp(z))
}

// deterministicHoldout returns true when the EpisodeID hashes
// into the holdout fraction. Using a stable hash (FNV) makes
// the train/eval split reproducible across runs against the
// same TrainingInput -- the version fingerprint and the
// metrics row co-vary as a result.
func deterministicHoldout(episodeID string, fraction float64) bool {
	if fraction <= 0 {
		return false
	}
	if fraction >= 1 {
		return true
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(episodeID))
	// Normalise into [0, 1).
	v := float64(h.Sum64()&((1<<53)-1)) / float64(1<<53)
	return v < fraction
}

// DecodeLinearWeights extracts the LinearWeights struct from
// an artifact URI produced by buildLinearOutput. Returns an
// error when the URI is not the expected
// `data:application/json;base64,...` shape or when the
// embedded payload does not parse as LinearWeights.
//
// Exported so the recall-side `PublishedReranker` can use the
// same canonical decoder rather than re-implementing the URI
// shape understanding.
func DecodeLinearWeights(artifactURI string) (LinearWeights, error) {
	const prefix = "data:application/json;base64,"
	if len(artifactURI) <= len(prefix) || artifactURI[:len(prefix)] != prefix {
		return LinearWeights{}, fmt.Errorf("rerankertrainer: artifact uri not data:application/json;base64 (got %q)", artifactURI)
	}
	payload, err := base64.StdEncoding.DecodeString(artifactURI[len(prefix):])
	if err != nil {
		return LinearWeights{}, fmt.Errorf("rerankertrainer: artifact base64 decode: %w", err)
	}
	var w LinearWeights
	if err := json.Unmarshal(payload, &w); err != nil {
		return LinearWeights{}, fmt.Errorf("rerankertrainer: artifact json unmarshal: %w", err)
	}
	if w.Schema != LinearWeightsSchemaV2 {
		return LinearWeights{}, fmt.Errorf("rerankertrainer: artifact schema %q != %q",
			w.Schema, LinearWeightsSchemaV2)
	}
	if len(w.Weights) != linearFeatureDim {
		return LinearWeights{}, fmt.Errorf("rerankertrainer: artifact weights len %d != %d",
			len(w.Weights), linearFeatureDim)
	}
	return w, nil
}

// ErrLinearWeightsAbsent is the typed error returned when the
// recall-side loader can not find a published artifact to
// decode. Exported so the recall wrapper can errors.Is gate on
// it rather than string-matching.
var ErrLinearWeightsAbsent = errors.New("rerankertrainer: no published linear weights")
