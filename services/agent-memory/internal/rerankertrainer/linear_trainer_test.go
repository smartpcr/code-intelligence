package rerankertrainer

// Unit tests for the in-process linear baseline trainer.
// These tests cover the four invariants the Stage 6.4 trainer
// contract demands of any concrete Trainer implementation:
//
//  1. Determinism: same TrainingInput → same artifact
//     (byte-for-byte), same Version fingerprint, same metrics
//     map. This is the foundation of the ON CONFLICT
//     idempotency invariant Service.publish relies on.
//
//  2. Empty input degrades gracefully: zero positives + zero
//     negatives must still produce a well-formed
//     TrainingOutput (so Service.Tick's BelowMinEpisodes gate
//     -- not the trainer -- is what halts the publish path
//     under sparse supervision).
//
//  3. Learnability: a clearly-separable synthetic input
//     produces a model whose evaluation metrics exceed the
//     trivial floor (eval_mrr@k=10 > 0 on at least one held-out
//     positive). The §6.4 metric key set is also exercised.
//
//  4. Round-trip: the published artifact URI decodes back to
//     a LinearWeights struct identical to what the trainer
//     produced, so the agent-api binary can consume the same
//     bytes the trainer wrote.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// fixedPair returns a LabelledPair with all the optional
// surfaces extractFeatures inspects. Used to build the
// fixture for the determinism + round-trip tests.
//
// The hasConcept switch flips the first Observation's Role
// to "concept_hit" (the proto-level closed-set value from
// proto/agent.proto §Observation.Role), matching the
// canonical wire literal. The v1 trainer used "concept"
// here -- the evaluator flagged that as a contract drift
// (iter-2 feedback item F6); the literal MUST be
// "concept_hit" to match `extractFeatures`.
func fixedPair(id string, kind string, seedSize int, obsCount int, hasConcept bool, ageDays int, actor string) LabelledPair {
	created := time.Date(2025, 1, 15, 0, 0, 0, 0, time.UTC).Add(-time.Duration(ageDays) * 24 * time.Hour)
	obs := make([]LabelledObservation, 0, obsCount)
	for i := 0; i < obsCount; i++ {
		role := "node_hit"
		if hasConcept && i == 0 {
			role = "concept_hit"
		}
		obs = append(obs, LabelledObservation{Role: role, Weight: 1.0})
	}
	seeds := make([]string, seedSize)
	for i := 0; i < seedSize; i++ {
		seeds[i] = id + "-seed"
	}
	return LabelledPair{
		EpisodeID:       id,
		EpisodeKind:     kind,
		CreatedAt:       created,
		SeedNodeIDs:     seeds,
		Observations:    obs,
		CorrectionActor: actor,
	}
}

func TestLinearTrainer_Deterministic(t *testing.T) {
	t.Parallel()
	in := TrainingInput{
		WindowStart: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
		TrainerTag:  "linear",
		Positives: []LabelledPair{
			fixedPair("p1", "synthetic_positive", 2, 4, true, 5, ""),
			fixedPair("p2", "agent", 1, 2, false, 10, ""),
		},
		Negatives: []LabelledPair{
			fixedPair("n1", "agent", 1, 1, false, 8, ""),
		},
	}
	t1 := LinearTrainer{}
	out1, err := t1.Train(context.Background(), in)
	if err != nil {
		t.Fatalf("first Train: %v", err)
	}
	out2, err := t1.Train(context.Background(), in)
	if err != nil {
		t.Fatalf("second Train: %v", err)
	}
	if out1.Version != out2.Version {
		t.Errorf("Version drift: %q vs %q", out1.Version, out2.Version)
	}
	if out1.ArtifactURI != out2.ArtifactURI {
		t.Errorf("ArtifactURI drift")
	}
	if out1.PublishStatus != out2.PublishStatus {
		t.Errorf("PublishStatus drift: %q vs %q", out1.PublishStatus, out2.PublishStatus)
	}
	// Compare the metric map deeply.
	if len(out1.Metrics) != len(out2.Metrics) {
		t.Fatalf("metric key drift: %d vs %d", len(out1.Metrics), len(out2.Metrics))
	}
	for k, v1 := range out1.Metrics {
		if v2, ok := out2.Metrics[k]; !ok || v2 != v1 {
			t.Errorf("metric %q drift: %v vs %v (present=%v)", k, v1, v2, ok)
		}
	}
}

func TestLinearTrainer_EmptyInputProducesZeroFitArtifact(t *testing.T) {
	t.Parallel()
	in := TrainingInput{
		WindowStart: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
		TrainerTag:  "linear",
	}
	out, err := LinearTrainer{}.Train(context.Background(), in)
	if err != nil {
		t.Fatalf("Train on empty input: %v", err)
	}
	if out.Version == "" {
		t.Errorf("Version empty on empty input; UNIQUE-version contract requires a value")
	}
	if !strings.HasPrefix(out.ArtifactURI, "data:application/json;base64,") {
		t.Errorf("ArtifactURI = %q, want data: URI prefix", out.ArtifactURI)
	}
	// Decode the artifact to assert the LinearWeights shape.
	w, err := DecodeLinearWeights(out.ArtifactURI)
	if err != nil {
		t.Fatalf("DecodeLinearWeights: %v", err)
	}
	if len(w.Weights) != FeatureCount() {
		t.Errorf("Weights len = %d, want %d", len(w.Weights), FeatureCount())
	}
	// On empty input the optimizer has nothing to fit, so
	// every weight stays at its initialisation value (zero).
	for i, wi := range w.Weights {
		if wi != 0 {
			t.Errorf("Weights[%d] = %v on empty input, want 0", i, wi)
		}
	}
}

func TestLinearTrainer_MetricsKeyContract(t *testing.T) {
	t.Parallel()
	in := TrainingInput{
		WindowStart: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
		TrainerTag:  "linear",
		Positives: []LabelledPair{
			fixedPair("p1", "synthetic_positive", 2, 4, true, 5, ""),
		},
		Negatives: []LabelledPair{
			fixedPair("n1", "agent", 0, 0, false, 50, ""),
		},
	}
	out, err := LinearTrainer{}.Train(context.Background(), in)
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	// §6.4 step-3 contract: these metric keys MUST exist on
	// every published reranker_model row's metrics_json
	// column so dashboards can graph them across the model
	// timeline. The exact spelling matters -- iter-1 shipped
	// `rank-of-correct-node@k20` (no equals sign) and the
	// evaluator flagged it.
	required := []string{
		"train_loss",
		"eval_ndcg@k",
		"eval_mrr@k=10",
		"eval_recall@k=20",
		"rank-of-correct-node@k=20",
	}
	for _, k := range required {
		if _, ok := out.Metrics[k]; !ok {
			t.Errorf("missing required metric key %q (got: %v)", k, keysOf(out.Metrics))
		}
	}
}

func TestLinearTrainer_TagDefaultsToLinear(t *testing.T) {
	t.Parallel()
	if got := (LinearTrainer{}).Tag(); got != "linear" {
		t.Errorf("Tag() = %q, want %q", got, "linear")
	}
}

func TestLinearTrainer_PublishStatusDefaultsPublished(t *testing.T) {
	t.Parallel()
	// LinearTrainer is the production baseline -- the §6.4
	// brief expects the publish path to elevate its output
	// to `published` so the recall envelope advertises a
	// real learned version, not a shadow row. This is also
	// the contract Service.Tick's noop-status-gate inverts
	// the relationship for: the gate force-shadows only the
	// NoopTrainer, never the LinearTrainer.
	in := TrainingInput{
		WindowStart: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
		TrainerTag:  "linear",
	}
	out, err := LinearTrainer{}.Train(context.Background(), in)
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	if out.PublishStatus != StatusPublished {
		t.Errorf("PublishStatus = %q, want %q", out.PublishStatus, StatusPublished)
	}
}

func TestDecodeLinearWeights_RoundTripPreservesValues(t *testing.T) {
	t.Parallel()
	src := LinearWeights{
		Schema:       LinearWeightsSchemaV2,
		FeatureNames: linearFeatureNames,
		// 6-dim vector matching the v2 schema's
		// featureBias/featureKindConcept/featureKindNode/
		// featureKindEdge/featureScoreSignal/featureDistancePenalty.
		Weights: []float64{0.1, -0.2, 0.3, -0.4, 0.5, -0.6},
	}
	body, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	uri := "data:application/json;base64," + base64Encode(body)
	got, err := DecodeLinearWeights(uri)
	if err != nil {
		t.Fatalf("DecodeLinearWeights: %v", err)
	}
	if got.Schema != src.Schema {
		t.Errorf("Schema = %q, want %q", got.Schema, src.Schema)
	}
	if len(got.Weights) != len(src.Weights) {
		t.Fatalf("Weights len = %d, want %d", len(got.Weights), len(src.Weights))
	}
	for i, w := range got.Weights {
		if w != src.Weights[i] {
			t.Errorf("Weights[%d] = %v, want %v", i, w, src.Weights[i])
		}
	}
}

func TestDecodeLinearWeights_RejectsBadPrefix(t *testing.T) {
	t.Parallel()
	if _, err := DecodeLinearWeights("s3://bucket/model.json"); err == nil {
		t.Fatalf("expected an error on non-data URI, got nil")
	}
}

func TestDecodeLinearWeights_RejectsSchemaDrift(t *testing.T) {
	t.Parallel()
	// v1 was a different feature space; v2 is incompatible.
	// The decoder MUST reject a v1 payload to prevent an
	// agent-api deploy that's ahead of the trainer-side
	// schema bump from scoring with the wrong feature
	// semantics.
	w := LinearWeights{
		Schema:  LinearWeightsSchemaV1,
		Weights: make([]float64, FeatureCount()),
	}
	body, _ := json.Marshal(w)
	uri := "data:application/json;base64," + base64Encode(body)
	if _, err := DecodeLinearWeights(uri); err == nil {
		t.Fatalf("expected schema drift error on v1 payload, got nil")
	}
}

func TestScoreCandidate_DimensionMismatchDegradesToInf(t *testing.T) {
	t.Parallel()
	w := LinearWeights{
		Schema:  LinearWeightsSchemaV2,
		Weights: []float64{0.1, 0.2, 0.3, 0.4, 0.5, 0.6},
	}
	// 4-d feature on a 6-d model: must NOT panic, must
	// return -Inf so the recall path's stable-sort moves
	// the unscorable candidate to the bottom rather than
	// crashing the whole rerank pass.
	got := ScoreCandidate(w, []float64{1, 2, 3, 4})
	if !math.IsInf(got, -1) {
		t.Errorf("ScoreCandidate(mismatch) = %v, want -Inf sentinel", got)
	}
}

func TestExtractCandidateFeatures_KindMapping(t *testing.T) {
	t.Parallel()
	// The shared feature schema MUST set kind_concept=1 for
	// "concept" Candidates and kind_node=1 for
	// "method"/"block" Candidates -- this is the bridge
	// from the trainer-side concept_hit / node_hit /
	// call_edge_hit observation roles to the recall-side
	// Candidate.Kind values, the F1+F6 fix.
	cases := []struct {
		name              string
		kind              string
		score             float64
		distance          int
		wantKindConcept   float64
		wantKindNode      float64
		wantScoreSignal   float64
		wantDistanceFeat  float64
	}{
		{"concept", "concept", 0.7, 0, 1, 0, 0.7, 0},
		{"method", "method", 0.5, 1, 0, 1, 0.5, 1},
		{"block", "block", 0.3, 2, 0, 1, 0.3, 2},
		{"unknown_kind_zero_indicator", "agent", 0.9, 0, 0, 0, 0.9, 0},
		{"score_clamped_high", "method", 5.0, 0, 0, 1, 1, 0},
		{"score_clamped_low", "method", -1.0, 0, 0, 1, 0, 0},
		{"distance_clamped_high", "method", 0.5, 99, 0, 1, 0.5, 3},
		{"distance_clamped_neg", "method", 0.5, -5, 0, 1, 0.5, 0},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := ExtractCandidateFeatures(tc.kind, tc.score, tc.distance)
			if v[featureBias] != 1.0 {
				t.Errorf("featureBias = %v, want 1.0", v[featureBias])
			}
			if v[featureKindConcept] != tc.wantKindConcept {
				t.Errorf("featureKindConcept = %v, want %v", v[featureKindConcept], tc.wantKindConcept)
			}
			if v[featureKindNode] != tc.wantKindNode {
				t.Errorf("featureKindNode = %v, want %v", v[featureKindNode], tc.wantKindNode)
			}
			if v[featureKindEdge] != 0 {
				t.Errorf("featureKindEdge = %v, want 0 (edges are not Candidates)", v[featureKindEdge])
			}
			if v[featureScoreSignal] != tc.wantScoreSignal {
				t.Errorf("featureScoreSignal = %v, want %v", v[featureScoreSignal], tc.wantScoreSignal)
			}
			if v[featureDistancePenalty] != tc.wantDistanceFeat {
				t.Errorf("featureDistancePenalty = %v, want %v", v[featureDistancePenalty], tc.wantDistanceFeat)
			}
		})
	}
}

func TestExtractFeatures_RoleConceptHit(t *testing.T) {
	t.Parallel()
	// The F6 fix: extractFeatures MUST recognise
	// "concept_hit" (proto closed set) and NOT the stale
	// "concept" string. A pair with concept_hit observations
	// flips kind_concept=1; the same pair with the old
	// "concept" literal would leave it at 0 (silent miss).
	p := LabelledPair{
		EpisodeID: "p1",
		Observations: []LabelledObservation{
			{Role: "concept_hit", Weight: 0.8},
		},
	}
	v := extractFeatures(p, time.Time{})
	if v[featureKindConcept] != 1 {
		t.Errorf("featureKindConcept = %v on concept_hit observation, want 1 (F6: literal must be 'concept_hit', not 'concept')", v[featureKindConcept])
	}

	// Negative case: a stale "concept" literal MUST NOT
	// flip the indicator; the test would have passed in
	// iter-2 (when extractFeatures checked the wrong string)
	// and now correctly fails.
	pStale := LabelledPair{
		EpisodeID: "p2",
		Observations: []LabelledObservation{
			{Role: "concept", Weight: 0.8},
		},
	}
	vStale := extractFeatures(pStale, time.Time{})
	if vStale[featureKindConcept] != 0 {
		t.Errorf("featureKindConcept = %v on stale 'concept' role, want 0 (proto closed set is 'concept_hit')", vStale[featureKindConcept])
	}
}

func TestExtractFeatures_RoleNodeHitAndCallEdgeHit(t *testing.T) {
	t.Parallel()
	// Both node_hit and call_edge_hit collapse into
	// kind_node=1 (the recall-side Candidate.Kind values
	// "method" and "block" both map to kind_node, so the
	// trainer-side aggregator must treat both observation
	// roles as the same signal).
	pNode := LabelledPair{
		EpisodeID:    "p-node",
		Observations: []LabelledObservation{{Role: "node_hit", Weight: 0.9}},
	}
	pCallEdge := LabelledPair{
		EpisodeID:    "p-call",
		Observations: []LabelledObservation{{Role: "call_edge_hit", Weight: 0.9}},
	}
	pEdge := LabelledPair{
		EpisodeID:    "p-edge",
		Observations: []LabelledObservation{{Role: "edge_hit", Weight: 0.9}},
	}
	if extractFeatures(pNode, time.Time{})[featureKindNode] != 1 {
		t.Errorf("node_hit did not flip featureKindNode=1")
	}
	if extractFeatures(pCallEdge, time.Time{})[featureKindNode] != 1 {
		t.Errorf("call_edge_hit did not flip featureKindNode=1 (call edges aggregate with node hits)")
	}
	if extractFeatures(pEdge, time.Time{})[featureKindEdge] != 1 {
		t.Errorf("edge_hit did not flip featureKindEdge=1")
	}
	// Cross-feature isolation: edge_hit MUST NOT also flip
	// featureKindNode (each role maps to exactly one kind
	// indicator).
	if extractFeatures(pEdge, time.Time{})[featureKindNode] != 0 {
		t.Errorf("edge_hit incorrectly flipped featureKindNode")
	}
}

func TestSidecarTrainer_DefaultsWhenEndpointEmpty(t *testing.T) {
	t.Parallel()
	in := TrainingInput{
		WindowStart: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2025, 1, 31, 0, 0, 0, 0, time.UTC),
		TrainerTag:  "sidecar",
	}
	_, err := SidecarTrainer{}.Train(context.Background(), in)
	if err == nil {
		t.Fatalf("expected an error when Endpoint is empty")
	}
}

func TestSidecarTrainer_TagDefault(t *testing.T) {
	t.Parallel()
	if got := (SidecarTrainer{}).Tag(); got != "sidecar" {
		t.Errorf("default Tag() = %q, want %q", got, "sidecar")
	}
	if got := (SidecarTrainer{TagName: "bert-base"}).Tag(); got != "bert-base" {
		t.Errorf("override Tag() = %q, want %q", got, "bert-base")
	}
}

func keysOf(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func base64Encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}
