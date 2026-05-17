package agentapi

// Unit tests for the Stage 6.4 PublishedReranker wrapper that
// surfaces the latest published `reranker_model.version` on
// every recall response AND ranks Candidates with the trained
// artifact's weight vector.
//
// Tests cover:
//
//  1. ModelVersion returns the published version when the
//     source reports one and falls back to the inner Reranker
//     when it does not.
//  2. The source is consulted on EVERY call -- no cache
//     (impl-plan §1115).
//  3. A transient source error degrades to inner on that
//     request; the NEXT request re-attempts the lookup
//     (self-healing without a cache window).
//  4. Rank uses the decoded LinearWeights to score Candidates;
//     the order can differ from the inner V0 reranker when
//     the trained weights weight features differently.
//  5. Rank falls back to inner when the decoder declines the
//     URI scheme (e.g. a sidecar `s3://` artifact).
//  6. Rank falls back to inner when no published row exists.
//  7. NewPublishedReranker panics on a nil source / nil inner
//     so a mis-wired binary fails fast at construction.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/rerankertrainer"
)

type fakeSource struct {
	calls       int32
	artifactURI string
	version     string
	trainedAt   time.Time
	ok          bool
	err         error
}

func (f *fakeSource) Latest(_ context.Context) (PublishedArtifact, bool, error) {
	atomic.AddInt32(&f.calls, 1)
	return PublishedArtifact{
		Version:     f.version,
		ArtifactURI: f.artifactURI,
		TrainedAt:   f.trainedAt,
	}, f.ok, f.err
}

func (f *fakeSource) callCount() int32 { return atomic.LoadInt32(&f.calls) }

// linearArtifactURI builds a synthetic LinearWeights artifact
// URI with the supplied feature coefficients. Used by the
// Rank tests so the trained weight vector is fully controlled
// without going through the SGD optimizer.
func linearArtifactURI(t *testing.T, weights []float64, bias float64) string {
	t.Helper()
	if len(weights) != rerankertrainer.FeatureCount() {
		t.Fatalf("test setup: weights length %d != FeatureCount %d", len(weights), rerankertrainer.FeatureCount())
	}
	w := rerankertrainer.LinearWeights{
		Schema:       rerankertrainer.LinearWeightsSchemaV2,
		Weights:      weights,
		Bias:         bias,
		FeatureNames: nil,
		TrainerTag:   "test-linear",
		FitMetrics:   map[string]float64{"train_loss": 0.5},
	}
	payload, err := json.Marshal(w)
	if err != nil {
		t.Fatalf("test setup: marshal weights: %v", err)
	}
	return "data:application/json;base64," + base64.StdEncoding.EncodeToString(payload)
}

func TestPublishedReranker_ReportsPublishedVersionWhenAvailable(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		version:     "v-trained-001",
		artifactURI: "noop-uri",
		ok:          true,
	}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, nil)

	if got := wr.ModelVersion(); got != "v-trained-001" {
		t.Errorf("ModelVersion() = %q, want %q", got, "v-trained-001")
	}
}

func TestPublishedReranker_FallsBackToInnerWhenNoPublishedRow(t *testing.T) {
	t.Parallel()
	src := &fakeSource{ok: false}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, nil)

	if got := wr.ModelVersion(); got != V0ModelVersion {
		t.Errorf("ModelVersion() = %q, want fallback %q", got, V0ModelVersion)
	}
}

func TestPublishedReranker_ReadsOnEveryRequest(t *testing.T) {
	t.Parallel()
	// Impl-plan §1115: "GraphReader reads the latest
	// published version on every request." Five consecutive
	// ModelVersion() calls MUST produce five source lookups.
	src := &fakeSource{version: "v-fresh", artifactURI: "noop", ok: true}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, nil)

	for i := 0; i < 5; i++ {
		if got := wr.ModelVersion(); got != "v-fresh" {
			t.Fatalf("ModelVersion()[%d] = %q, want %q", i, got, "v-fresh")
		}
	}
	if got := src.callCount(); got != 5 {
		t.Errorf("source.Latest call count = %d, want 5 (one per request, no cache)", got)
	}
}

func TestPublishedReranker_NewPublishVisibleOnNextRequest(t *testing.T) {
	t.Parallel()
	// The cache-free design's acceptance bar: a new publish
	// landed between two ModelVersion calls MUST be visible
	// on the second call.
	src := &fakeSource{version: "v-old", artifactURI: "noop", ok: true}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, nil)

	if got := wr.ModelVersion(); got != "v-old" {
		t.Fatalf("seed: ModelVersion() = %q, want %q", got, "v-old")
	}
	// Simulate a publish landing between the two calls.
	src.version = "v-new"
	if got := wr.ModelVersion(); got != "v-new" {
		t.Errorf("after publish: ModelVersion() = %q, want %q (the next request must see the new row)", got, "v-new")
	}
}

func TestPublishedReranker_TransientErrorFallsBackThisRequest(t *testing.T) {
	t.Parallel()
	// With the cache dropped, a source error fails THIS
	// request to the inner; the next request retries.
	src := &fakeSource{version: "v-healthy", artifactURI: "noop", ok: true}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, nil)

	if got := wr.ModelVersion(); got != "v-healthy" {
		t.Fatalf("seed: ModelVersion() = %q, want %q", got, "v-healthy")
	}
	src.err = errors.New("simulated transient DB outage")

	// Erroring source: falls back to inner (no cache to
	// shield the request).
	if got := wr.ModelVersion(); got != V0ModelVersion {
		t.Errorf("during outage: ModelVersion() = %q, want fallback %q", got, V0ModelVersion)
	}

	// Recovery: next request sees the healthy source again.
	src.err = nil
	if got := wr.ModelVersion(); got != "v-healthy" {
		t.Errorf("after recovery: ModelVersion() = %q, want %q", got, "v-healthy")
	}
}

func TestPublishedReranker_RankUsesTrainedWeights(t *testing.T) {
	t.Parallel()
	// Trained weight vector that DEMOTES concept hits
	// (negative kind_concept weight) and REWARDS high
	// score_signal. This contradicts V0 (which gives
	// concept a +0.05 bonus), so the resulting ranking
	// MUST differ from V0.
	weights := make([]float64, rerankertrainer.FeatureCount())
	// kind_concept (index 1) gets a HEAVY negative weight
	// so a concept Candidate ranks below a method Candidate
	// with the same cosine.
	weights[1] = -10.0
	// score_signal (index 4) keeps a positive coefficient
	// so the ranker is not degenerate.
	weights[4] = 1.0
	uri := linearArtifactURI(t, weights, 0.0)

	src := &fakeSource{
		artifactURI: uri,
		version:     "v-trained-rank",
		ok:          true,
	}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, NewLinearWeightsDecoder())

	candidates := []Candidate{
		{PointID: "p-method", Score: 0.5, Kind: "method", StructuralDistance: 0},
		{PointID: "p-concept", Score: 0.5, Kind: NodeKindConcept, StructuralDistance: 0},
	}

	got := wr.Rank(candidates)
	if len(got) != 2 {
		t.Fatalf("len(Rank) = %d, want 2", len(got))
	}
	// With kind_concept=-10, the concept candidate's score
	// is much lower than the method candidate's even though
	// V0 would have favoured the concept (concept bonus).
	if got[0].PointID != "p-method" {
		t.Errorf("Rank[0].PointID = %q, want %q (trained weights demote concept; V0 would have ranked it first)",
			got[0].PointID, "p-method")
	}
	if got[1].PointID != "p-concept" {
		t.Errorf("Rank[1].PointID = %q, want %q", got[1].PointID, "p-concept")
	}
}

func TestPublishedReranker_RankFallsBackOnUnrecognizedArtifactScheme(t *testing.T) {
	t.Parallel()
	// Sidecar (BERT) artifact URIs use a scheme the
	// LinearWeights decoder politely declines; the wrapper
	// MUST fall back to the inner V0 reranker for the
	// request rather than refusing to rank.
	src := &fakeSource{
		artifactURI: "s3://reranker-artifacts/bert-2024-01-01.bin",
		version:     "v-bert-001",
		ok:          true,
	}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, NewLinearWeightsDecoder())

	candidates := []Candidate{
		{PointID: "p1", Score: 0.9, Kind: "method", StructuralDistance: 0},
		{PointID: "p2", Score: 0.7, Kind: NodeKindConcept, StructuralDistance: 1},
	}
	got := wr.Rank(candidates)
	want := inner.Rank(candidates)
	if len(got) != len(want) {
		t.Fatalf("len(Rank) = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].PointID != want[i].PointID {
			t.Errorf("Rank[%d].PointID = %q, want %q (sidecar URI should fall back to V0)",
				i, got[i].PointID, want[i].PointID)
		}
	}
}

func TestPublishedReranker_RankFallsBackWhenNoPublishedRow(t *testing.T) {
	t.Parallel()
	src := &fakeSource{ok: false}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, NewLinearWeightsDecoder())

	candidates := []Candidate{
		{PointID: "p1", Score: 0.9, Kind: "method", StructuralDistance: 0},
	}
	got := wr.Rank(candidates)
	want := inner.Rank(candidates)
	if len(got) != len(want) || got[0].PointID != want[0].PointID {
		t.Errorf("Rank with no published row should match V0; got=%+v want=%+v", got, want)
	}
}

func TestPublishedReranker_RankFallsBackWhenDecoderNil(t *testing.T) {
	t.Parallel()
	// A nil decoder is valid (early-adoption mode where the
	// version is advertised but the trained scorer is not
	// yet plumbed); Rank must still work via V0.
	src := &fakeSource{artifactURI: "data:application/json;base64,...", version: "v-no-decoder", ok: true}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, nil)

	candidates := []Candidate{
		{PointID: "p1", Score: 0.9, Kind: "method", StructuralDistance: 0},
	}
	got := wr.Rank(candidates)
	want := inner.Rank(candidates)
	if len(got) != len(want) || got[0].PointID != want[0].PointID {
		t.Errorf("Rank with nil decoder should match V0; got=%+v want=%+v", got, want)
	}
}

func TestPublishedReranker_PanicsOnNilSource(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected a panic on nil source, got none")
		}
	}()
	inner := NewV0ColdStartReranker(nil)
	_ = NewPublishedReranker(nil, inner, nil)
}

func TestPublishedReranker_PanicsOnNilInner(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected a panic on nil inner, got none")
		}
	}()
	src := &fakeSource{}
	_ = NewPublishedReranker(src, nil, nil)
}

func TestPublishedRerankerSourceFunc_IsAdapter(t *testing.T) {
	t.Parallel()
	wantURI := "data:application/json;base64,abc"
	wantVersion := "v-from-func"
	wantTrained := time.Now().UTC()
	called := false
	var src PublishedRerankerSource = PublishedRerankerSourceFunc(func(_ context.Context) (PublishedArtifact, bool, error) {
		called = true
		return PublishedArtifact{Version: wantVersion, ArtifactURI: wantURI, TrainedAt: wantTrained}, true, nil
	})
	got, ok, err := src.Latest(context.Background())
	if err != nil {
		t.Fatalf("Latest: %v", err)
	}
	if !ok {
		t.Errorf("ok=false, want true")
	}
	if !called {
		t.Errorf("inner func was not invoked")
	}
	if got.ArtifactURI != wantURI {
		t.Errorf("artifactURI = %q, want %q", got.ArtifactURI, wantURI)
	}
	if got.Version != wantVersion {
		t.Errorf("version = %q, want %q", got.Version, wantVersion)
	}
	if !got.TrainedAt.Equal(wantTrained) {
		t.Errorf("trainedAt = %v, want %v", got.TrainedAt, wantTrained)
	}
}

// TestPublishedArtifact_IsZero proves the zero-value
// convenience: callers reading PublishedArtifact off the
// wire (e.g. snapshot inspectors) can ask `IsZero()` to
// detect the fallback path instead of comparing every
// field by hand.
func TestPublishedArtifact_IsZero(t *testing.T) {
	t.Parallel()
	if !(PublishedArtifact{}).IsZero() {
		t.Errorf("zero struct should report IsZero()=true")
	}
	if (PublishedArtifact{Version: "x"}).IsZero() {
		t.Errorf("non-empty version should NOT report IsZero()=true")
	}
	if (PublishedArtifact{ArtifactURI: "data:..."}).IsZero() {
		t.Errorf("non-empty URI should NOT report IsZero()=true")
	}
}

// TestPublishedReranker_RankWithVersionPinsSingleSourceRead
// proves the atomic-pinning contract (iter-3 review item 4):
// RankWithVersion fetches the source EXACTLY ONCE per call,
// so the returned ordering and the returned version cannot
// drift even if a publish lands between two would-be
// separate calls.
func TestPublishedReranker_RankWithVersionPinsSingleSourceRead(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		version:     "v-pinned-001",
		artifactURI: linearArtifactURI(t, []float64{0, 0, 0, 0, 0, 0}, 0),
		ok:          true,
	}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, NewLinearWeightsDecoder())

	candidates := []Candidate{
		{PointID: "p1", Kind: "method", Score: 0.5},
		{PointID: "p2", Kind: "concept", Score: 0.3},
	}
	startCalls := src.callCount()
	ranked, version := wr.RankWithVersion(context.Background(), candidates)
	if version != "v-pinned-001" {
		t.Errorf("version = %q, want %q", version, "v-pinned-001")
	}
	if len(ranked) != 2 {
		t.Errorf("ranked len = %d, want 2", len(ranked))
	}
	calls := src.callCount() - startCalls
	if calls != 1 {
		t.Errorf("source consulted %d times; want exactly 1 (atomic pin)", calls)
	}
}

// TestPublishedReranker_RankWithVersion_FallsBackToInner
// proves the wrapper still returns a self-consistent
// (ranked, version) pair when the source has no published
// row: both come from the inner Reranker so callers can
// never observe a trained ordering with the cold-start
// version (or vice versa).
func TestPublishedReranker_RankWithVersion_FallsBackToInner(t *testing.T) {
	t.Parallel()
	src := &fakeSource{ok: false}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, NewLinearWeightsDecoder())

	candidates := []Candidate{
		{PointID: "p1", Kind: "method", Score: 0.5},
	}
	ranked, version := wr.RankWithVersion(context.Background(), candidates)
	if version != V0ModelVersion {
		t.Errorf("version = %q, want %q (inner fallback)", version, V0ModelVersion)
	}
	if len(ranked) != 1 {
		t.Errorf("ranked len = %d, want 1", len(ranked))
	}
}

// TestPublishedReranker_RankWithVersion_AdvertisesVersionEvenWhenDecoderDeclines
// proves the "publish landed but recall-side consumer is
// lagging" path: when the source has a trained row but the
// decoder cannot consume the artifact, the response still
// CARRIES the trained version (so operators see the
// publish) while the ordering falls back to inner.
func TestPublishedReranker_RankWithVersion_AdvertisesVersionEvenWhenDecoderDeclines(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		version:     "v-trained-but-unknown-scheme",
		artifactURI: "s3://bucket/path", // not a data: URI -> linear decoder declines
		ok:          true,
	}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, NewLinearWeightsDecoder())

	candidates := []Candidate{
		{PointID: "p1", Kind: "method", Score: 0.5},
	}
	_, version := wr.RankWithVersion(context.Background(), candidates)
	if version != "v-trained-but-unknown-scheme" {
		t.Errorf("version = %q, want trained version even with decoder declining", version)
	}
}

// TestRankWithVersion_HelperUsesAtomicWhenAvailable proves
// the recall.go shim's "prefer AtomicReranker" type-assert
// works: when given a PublishedReranker (which implements
// AtomicReranker), it goes through the atomic path (one
// source read). When given a plain V0 reranker (no atomic
// support), it falls back to Rank+ModelVersion separately.
func TestRankWithVersion_HelperUsesAtomicWhenAvailable(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		version:     "v-shim-001",
		artifactURI: linearArtifactURI(t, []float64{0, 0, 0, 0, 0, 0}, 0),
		ok:          true,
	}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, NewLinearWeightsDecoder())

	startCalls := src.callCount()
	_, version := rankWithVersion(context.Background(), wr, "", []Candidate{
		{PointID: "p1", Kind: "method", Score: 0.5},
	})
	if version != "v-shim-001" {
		t.Errorf("version = %q, want trained from atomic path", version)
	}
	if got := src.callCount() - startCalls; got != 1 {
		t.Errorf("source calls = %d, want 1 (single read via atomic)", got)
	}
}

// TestRankWithVersion_HelperFallsBackToInterfaceMethods
// proves the shim still works for legacy rerankers that do
// NOT implement AtomicReranker — it just calls
// Rank()+ModelVersion() separately. The cold-start V0
// reranker has process-immutable state so the race is
// harmless here, but the test guards against accidental
// type-assert tightening.
func TestRankWithVersion_HelperFallsBackToInterfaceMethods(t *testing.T) {
	t.Parallel()
	v0 := NewV0ColdStartReranker(nil)
	candidates := []Candidate{
		{PointID: "p1", Kind: "method", Score: 0.5},
	}
	ranked, version := rankWithVersion(context.Background(), v0, "", candidates)
	if version != V0ModelVersion {
		t.Errorf("version = %q, want %q", version, V0ModelVersion)
	}
	if len(ranked) != 1 {
		t.Errorf("ranked len = %d, want 1", len(ranked))
	}
}

// fallibleFakeScorer implements both Reranker and
// FallibleReranker; the err field controls whether
// RankErr signals scorer degradation. Used by the
// PublishedReranker pinning tests below.
type fallibleFakeScorer struct {
	version string
	out     []Candidate
	err     error
	calls   int32
}

func (f *fallibleFakeScorer) Rank(c []Candidate) []Candidate {
	atomic.AddInt32(&f.calls, 1)
	if f.err != nil {
		return c
	}
	return f.out
}
func (f *fallibleFakeScorer) ModelVersion() string { return f.version }
func (f *fallibleFakeScorer) RankErr(c []Candidate) ([]Candidate, error) {
	atomic.AddInt32(&f.calls, 1)
	if f.err != nil {
		return c, f.err
	}
	return f.out, nil
}

// queryAwareFallibleFakeScorer extends fallibleFakeScorer
// with the QueryAwareFallibleReranker interface so we can
// assert PublishedReranker.RankWithQuery threads the query
// through AND pins the version correctly when the scorer
// signals degradation.
type queryAwareFallibleFakeScorer struct {
	fallibleFakeScorer
	gotQuery string
}

func (q *queryAwareFallibleFakeScorer) RankWithQueryErr(_ context.Context, query string, c []Candidate) ([]Candidate, error) {
	q.gotQuery = query
	return q.RankErr(c)
}

// stubArtifactDecoder returns a fixed Reranker for every
// URI so the pinning tests can inject either a fallible
// or query-aware fallible scorer.
type stubArtifactDecoder struct{ r Reranker }

func (s stubArtifactDecoder) Decode(_ string) (Reranker, bool, error) { return s.r, true, nil }

// TestPublishedReranker_PinsInnerVersionWhenScorerFails is
// the iter-4 review item 6 regression test on the wrapper
// side: when the trained scorer is decoded but its
// FallibleReranker.RankErr returns a non-nil error, the
// wrapper MUST pin reranker_model_version to the inner
// cold-start version — NOT to the trained artifact version
// — because the trained scorer did not actually
// contribute to the ordering on the wire.
func TestPublishedReranker_PinsInnerVersionWhenScorerFails(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		version:     "v-trained-but-degraded",
		artifactURI: "file:///models/v-trained-but-degraded",
		ok:          true,
	}
	scorer := &fallibleFakeScorer{
		version: "trained-tag",
		err:     errors.New("sidecar 500"),
	}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, stubArtifactDecoder{r: scorer})

	candidates := []Candidate{{PointID: "p1", Kind: "method", Score: 0.5}}
	ranked, version := wr.RankWithVersion(context.Background(), candidates)

	if version == "v-trained-but-degraded" {
		t.Fatalf("version pinned to trained artifact despite scorer failure — iter-4 item 6 not addressed")
	}
	if version != V0ModelVersion {
		t.Fatalf("version = %q, want %q (inner pin on scorer failure)", version, V0ModelVersion)
	}
	if len(ranked) != 1 {
		t.Fatalf("ranked len = %d, want 1", len(ranked))
	}
}

// TestPublishedReranker_RankWithQuery_ThreadsQuery is the
// iter-4 review item 3 regression test on the wrapper
// side: the recall-time natural-language query MUST reach
// the trained scorer when the scorer implements
// QueryAwareFallibleReranker. Without this, the wrapper
// drops the query at the boundary and the cross-encoder
// scores on a surface that does not match its training
// distribution.
func TestPublishedReranker_RankWithQuery_ThreadsQuery(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		version:     "v-trained-query-aware",
		artifactURI: "file:///models/v-trained-query-aware",
		ok:          true,
	}
	scorer := &queryAwareFallibleFakeScorer{
		fallibleFakeScorer: fallibleFakeScorer{
			version: "trained-tag",
			out:     []Candidate{{PointID: "p1", Kind: "method", FinalScore: 0.99}},
		},
	}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, stubArtifactDecoder{r: scorer})

	if _, ok := any(wr).(QueryAwareAtomicReranker); !ok {
		t.Fatalf("PublishedReranker does not implement QueryAwareAtomicReranker — iter-4 item 3 not addressed")
	}

	in := []Candidate{{PointID: "p1", Kind: "method", Score: 0.5}}
	_, version := wr.RankWithQuery(
		context.Background(),
		"how does auth refresh handle 401?",
		in,
	)
	if scorer.gotQuery != "how does auth refresh handle 401?" {
		t.Fatalf("scorer.gotQuery = %q, want the threaded recall query", scorer.gotQuery)
	}
	if version != "v-trained-query-aware" {
		t.Fatalf("version = %q, want trained on success", version)
	}
}

// TestPublishedReranker_RankWithQuery_PinsInnerOnQueryAwareFailure
// is the joint iter-4 items 3 + 6 regression test: when
// the scorer implements QueryAwareFallibleReranker and
// returns an error from RankWithQueryErr, the wrapper
// MUST pin reranker_model_version to inner (not to the
// trained artifact version) AND fall back to inner
// ordering.
func TestPublishedReranker_RankWithQuery_PinsInnerOnQueryAwareFailure(t *testing.T) {
	t.Parallel()
	src := &fakeSource{
		version:     "v-trained-query-aware-degraded",
		artifactURI: "file:///models/v-trained-query-aware-degraded",
		ok:          true,
	}
	scorer := &queryAwareFallibleFakeScorer{
		fallibleFakeScorer: fallibleFakeScorer{
			version: "trained-tag",
			err:     errors.New("sidecar unreachable"),
		},
	}
	inner := NewV0ColdStartReranker(nil)
	wr := NewPublishedReranker(src, inner, stubArtifactDecoder{r: scorer})

	in := []Candidate{{PointID: "p1", Kind: "method", Score: 0.5}}
	_, version := wr.RankWithQuery(
		context.Background(),
		"any query",
		in,
	)
	if version != V0ModelVersion {
		t.Fatalf("version = %q, want %q (inner pin on query-aware scorer failure)", version, V0ModelVersion)
	}
}
