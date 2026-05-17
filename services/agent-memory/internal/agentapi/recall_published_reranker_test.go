package agentapi

// Stage 6.4 production-composition tests.
//
// These tests close the wiring gap surfaced by iter-19
// evaluator items 1-4: the unit tests around
// `applyRerankerStaleness` and `PublishedReranker` proved the
// helpers worked in isolation, but they did NOT prove that
// `Service.Recall` actually CALLS them on the hot path. The
// production composition is what the operator-pinned
// acceptance criteria require:
//
//   - the rendered `RecallResponse.RerankerModelVersion`
//     must come from the latest published `reranker_model`
//     row (per PublishedRerankerSource), not the v0
//     cold-start tag.
//   - the rendered `RecallResponse.Degraded` /
//     `DegradedReason` must flip to `reranker_model_stale`
//     when the latest published row is older than the
//     `rerankerStaleAfter` budget.
//
// To prove BOTH, the test wires the production wrapper end
// to end (PublishedRerankerSource + ArtifactDecoder +
// PublishedReranker) AND a separate RerankerFreshnessSource
// (per the rubber-duck review on the iter-20 plan — they're
// distinct interfaces wired by different `With…` options).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// staleArtifactSource yields a single published row with a
// stale TrainedAt timestamp. The RerankerFreshnessSource also
// returns the same TrainedAt so both the version-advertising
// and the staleness-gating paths fire on the same publish.
type staleArtifactSource struct {
	version   string
	uri       string
	trainedAt time.Time
}

func (s staleArtifactSource) Latest(_ context.Context) (PublishedArtifact, bool, error) {
	return PublishedArtifact{
		Version:     s.version,
		ArtifactURI: s.uri,
		TrainedAt:   s.trainedAt,
	}, true, nil
}

func (s staleArtifactSource) LatestRerankerTrainedAt(_ context.Context) (time.Time, bool, error) {
	return s.trainedAt, true, nil
}

// declineDecoder returns (nil, false, nil) for every URI so
// the PublishedReranker falls back to the inner v0 scorer
// while still advertising the published row's version (per
// the wrapper's documented pin contract). Keeps the test
// trainer-artifact-format-agnostic — we only care that the
// wire-up actually reads the published row and the staleness
// gate fires.
type declineDecoder struct{}

func (declineDecoder) Decode(_ string) (Reranker, bool, error) { return nil, false, nil }

// TestRecall_PublishedReranker_StaleModelFires verifies the
// production composition: when Recall encounters a published
// `reranker_model` row whose TrainedAt is older than the
// stale-after window, the returned RecallResponse advertises
// the trained version AND surfaces
// `degraded_reason='reranker_model_stale'`.
//
// Resolves iter-19 evaluator items 1-3: the bug was the
// production binary never called rankWithVersion + the
// staleness gate on real recall responses, so this test acts
// as a regression guard that the composition lives.
func TestRecall_PublishedReranker_StaleModelFires(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	// One real hit so the recall path reaches the happy-path
	// return at recall.go:~860 (NOT the empty-candidates
	// branch at ~707 nor the degraded-fallback at ~1158).
	search := &recordingSearcher{hits: []embedding.SearchHit{
		{PointID: "pt-1", Score: 0.91, Payload: map[string]any{
			"kind":    "method",
			"node_id": "n-1",
		}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pt-1": {}}}

	// Build the production wrapper composition: cold-start
	// inner + published-row source + decoder chain. Stale
	// TrainedAt = 30 days ago (well past the 7-day budget).
	stale := staleArtifactSource{
		version:   "v-trained-007",
		uri:       "data:application/json;base64,dummy",
		trainedAt: time.Now().Add(-30 * 24 * time.Hour),
	}
	wrapper := NewPublishedReranker(stale, NewV0ColdStartReranker(nil), declineDecoder{})

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithReranker(wrapper),
		WithRerankerFreshness(stale),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "what calls Bar()?",
		RepoID: "repo-1",
		K:      5,
		Kinds:  []string{"method"},
	})
	if err != nil {
		t.Fatalf("Recall: unexpected error: %v", err)
	}

	// Wrapper-advertised version surfaces on the envelope
	// (item 1 + item 2: rankWithVersion is being invoked
	// with the wrapper, not Rank()+ModelVersion() separately).
	if resp.RerankerModelVersion != "v-trained-007" {
		t.Errorf("RerankerModelVersion = %q; want %q (published wrapper not pinning version)",
			resp.RerankerModelVersion, "v-trained-007")
	}
	// Staleness gate fires on the production happy path
	// (item 3: applyRerankerStaleness is being called).
	if !resp.Degraded {
		t.Errorf("Degraded = false; want true (stale gate did not fire on hot path)")
	}
	if resp.DegradedReason != DegradedReasonRerankerModelStale {
		t.Errorf("DegradedReason = %q; want %q", resp.DegradedReason, DegradedReasonRerankerModelStale)
	}
}

// TestRecall_PublishedReranker_StaleModelFiresOnEmptyPath
// proves the staleness gate fires on the empty-candidates
// branch too — resolves iter-19 item 4 (first half).
//
// The Qdrant search returns zero hits, so the recall path
// reaches the early-return at recall.go:~707 instead of the
// main happy path. Before iter-20, that branch never called
// applyRerankerStaleness, so a stale model was invisible on
// any recall request that produced no candidates.
func TestRecall_PublishedReranker_StaleModelFiresOnEmptyPath(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	search := &recordingSearcher{hits: nil} // empty: forces early return
	filter := &allowListFilter{allow: map[string]struct{}{}}

	stale := staleArtifactSource{
		version:   "v-trained-008",
		uri:       "data:application/json;base64,dummy",
		trainedAt: time.Now().Add(-30 * 24 * time.Hour),
	}
	wrapper := NewPublishedReranker(stale, NewV0ColdStartReranker(nil), declineDecoder{})

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithReranker(wrapper),
		WithRerankerFreshness(stale),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "no hits here",
		RepoID: "repo-1",
		K:      5,
		Kinds:  []string{"method"},
	})
	if err != nil {
		t.Fatalf("Recall (empty path): unexpected error: %v", err)
	}
	if len(resp.Hits) != 0 {
		t.Fatalf("Hits = %d; want 0 (test fixture should reach the empty-candidates branch)",
			len(resp.Hits))
	}
	if !resp.Degraded {
		t.Errorf("Degraded = false on empty path; want true (stale gate not invoked)")
	}
	if resp.DegradedReason != DegradedReasonRerankerModelStale {
		t.Errorf("DegradedReason = %q on empty path; want %q",
			resp.DegradedReason, DegradedReasonRerankerModelStale)
	}
}

// TestRecall_PublishedReranker_FreshModelDoesNotDegrade is
// the negative control: when the published row is fresh, the
// staleness gate MUST NOT flip the response to degraded.
// Without this control the previous tests could pass via a
// regression that flips every response to degraded.
func TestRecall_PublishedReranker_FreshModelDoesNotDegrade(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	search := &recordingSearcher{hits: []embedding.SearchHit{
		{PointID: "pt-1", Score: 0.91, Payload: map[string]any{
			"kind":    "method",
			"node_id": "n-1",
		}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pt-1": {}}}

	fresh := staleArtifactSource{
		version:   "v-trained-009",
		uri:       "data:application/json;base64,dummy",
		trainedAt: time.Now().Add(-1 * time.Hour), // well within freshness window
	}
	wrapper := NewPublishedReranker(fresh, NewV0ColdStartReranker(nil), declineDecoder{})

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithReranker(wrapper),
		WithRerankerFreshness(fresh),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "still fresh",
		RepoID: "repo-1",
		K:      5,
		Kinds:  []string{"method"},
	})
	if err != nil {
		t.Fatalf("Recall (fresh path): unexpected error: %v", err)
	}
	if resp.Degraded {
		t.Errorf("Degraded = true on fresh path; want false (false-positive on stale gate)")
	}
	if resp.DegradedReason == DegradedReasonRerankerModelStale {
		t.Errorf("DegradedReason = reranker_model_stale on fresh path; freshness gate misfired")
	}
	if resp.RerankerModelVersion != "v-trained-009" {
		t.Errorf("RerankerModelVersion = %q; want %q", resp.RerankerModelVersion, "v-trained-009")
	}
}

// erroringFreshnessSource simulates a freshness lookup
// outage. Used by the next test to lock in the rubber-duck
// finding that a degraded-reason source going down MUST NOT
// flip the response to degraded (priority rule #3 in
// recall_stale.go).
type erroringFreshnessSource struct{ err error }

func (e erroringFreshnessSource) LatestRerankerTrainedAt(_ context.Context) (time.Time, bool, error) {
	return time.Time{}, false, e.err
}

func TestRecall_PublishedReranker_FreshnessSourceErrorIsSilent(t *testing.T) {
	emb := fakeEmbedder{vec: []float32{0.1, 0.2}}
	search := &recordingSearcher{hits: []embedding.SearchHit{
		{PointID: "pt-1", Score: 0.91, Payload: map[string]any{"kind": "method", "node_id": "n-1"}},
	}}
	filter := &allowListFilter{allow: map[string]struct{}{"pt-1": {}}}

	src := staleArtifactSource{
		version:   "v-trained-010",
		uri:       "data:application/json;base64,dummy",
		trainedAt: time.Now().Add(-1 * time.Hour),
	}
	wrapper := NewPublishedReranker(src, NewV0ColdStartReranker(nil), declineDecoder{})

	svc := NewService(emb, search, filter,
		WithLogger(quietLogger()),
		WithReranker(wrapper),
		WithRerankerFreshness(erroringFreshnessSource{err: errors.New("simulated outage")}),
	)

	resp, err := svc.Recall(context.Background(), RecallRequest{
		Query:  "freshness outage",
		RepoID: "repo-1",
		K:      5,
		Kinds:  []string{"method"},
	})
	if err != nil {
		t.Fatalf("Recall: unexpected error: %v", err)
	}
	if resp.Degraded {
		t.Errorf("Degraded = true on freshness outage; want false (outage in degraded-reason source must be silent)")
	}
}
