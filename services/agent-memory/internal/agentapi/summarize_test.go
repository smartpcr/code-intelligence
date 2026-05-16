package agentapi

// Unit tests for Stage 5.4 — agent.summarize verb.
//
// Coverage matrix (one test per row):
//
//   * Caller-correctable validation
//       missing target, ambiguous target, concept-without-repo,
//       node-with-mismatched-repo, max_tokens clamping.
//   * Service unconfigured
//       no NeighborhoodResolver → ErrSummarizeUnconfigured.
//   * Node target happy path
//       seed + edges + unique dst nodes cited; reachability
//       holds against the resolver's known set; SummaryMD
//       equals the summariser output verbatim.
//   * Concept target happy path.
//   * TargetNotFound surfaces as ErrSummarizeTargetNotFound
//       even when the resolver returns an empty card with
//       no error (defensive branch).
//   * Graph store unavailable degrades to template envelope
//       with `degraded_reason=graph_store_unavailable` and
//       zero citations.
//   * Summariser timeout (1 ms budget, 50 ms sleep) degrades
//       to template, partial output is discarded, response
//       carries `degraded=true`.
//   * Summariser error returning partial-but-non-empty
//       SummaryMD: partial output MUST be discarded.
//   * Reranker freshness 8 days old → `reranker_model_stale`.
//   * Reranker freshness 1 hour old → `summariser_unavailable`.
//   * Reranker freshness lookup error → `summariser_unavailable`.
//   * Parent ctx cancellation propagates as hard error (no
//       degraded envelope).
//   * Context log append soft failure → response with empty
//       ContextID.
//   * Context log appender invoked with `verb=summarize` +
//       correct NodeIDs/EdgeIDs/ConceptIDs.
//   * Edge fan-out capped at maxSummarizeEdges; prompt + citations
//       both bounded.
//   * Self-loop edge does not duplicate seed in citations.
//
// All tests use fake summarisers / resolvers / freshness
// sources — no Postgres, no Qdrant, no HTTP. Determinism is
// the load-bearing property here; the OpenAI-compatible
// client has its own httptest-based suite in
// `summarize_openai_test.go`.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeSummariser is the deterministic counterpart to the
// production OpenAI client. Behaviour is parameterised by
// struct fields so a single type covers happy path,
// sleep-and-block (timeout path), and error-with-partial-
// output cases.
type fakeSummariser struct {
	output       SummariserOutput
	err          error
	sleep        time.Duration
	model        string
	calls        int
	lastPrompt   string
	lastMaxToks  int
	lastDeadline time.Time
}

func (f *fakeSummariser) Summarize(ctx context.Context, in SummariserInput) (SummariserOutput, error) {
	f.calls++
	f.lastPrompt = in.Prompt
	f.lastMaxToks = in.MaxTokens
	if dl, ok := ctx.Deadline(); ok {
		f.lastDeadline = dl
	}
	if f.sleep > 0 {
		select {
		case <-time.After(f.sleep):
		case <-ctx.Done():
			return SummariserOutput{}, ctx.Err()
		}
	}
	if f.err != nil {
		return f.output, f.err
	}
	return f.output, nil
}

func (f *fakeSummariser) ModelVersion() string {
	if f.model == "" {
		return "fake-summariser"
	}
	return f.model
}

// fakeResolver returns canned neighborhood / concept
// payloads keyed by id. Errors per-id let tests force
// not-found / graph-unavailable per call.
type fakeResolver struct {
	nodes      map[string]SummarizeNodeNeighborhood
	nodeErr    map[string]error
	concepts   map[string]SummarizeConceptCard
	concErr    map[string]error
	lastRepoID string
	calls      int
}

func (f *fakeResolver) NeighborhoodForNode(_ context.Context, nodeID string) (SummarizeNodeNeighborhood, error) {
	f.calls++
	if err, ok := f.nodeErr[nodeID]; ok {
		return SummarizeNodeNeighborhood{}, err
	}
	if n, ok := f.nodes[nodeID]; ok {
		return n, nil
	}
	return SummarizeNodeNeighborhood{}, ErrSummarizeTargetNotFound
}

func (f *fakeResolver) FetchConcept(_ context.Context, conceptID, repoID string) (SummarizeConceptCard, error) {
	f.calls++
	f.lastRepoID = repoID
	if err, ok := f.concErr[conceptID]; ok {
		return SummarizeConceptCard{}, err
	}
	if c, ok := f.concepts[conceptID]; ok {
		return c, nil
	}
	return SummarizeConceptCard{}, ErrSummarizeTargetNotFound
}

// fakeFreshness lets tests force the classifier's three
// (trainedAt, ok, err) shapes deterministically.
type fakeFreshness struct {
	trainedAt time.Time
	ok        bool
	err       error
	calls     int
}

func (f *fakeFreshness) LatestRerankerTrainedAt(_ context.Context) (time.Time, bool, error) {
	f.calls++
	return f.trainedAt, f.ok, f.err
}

// newSummarizeService spins up a Service with the bare
// minimum recall-path deps (everything is a stub) and the
// supplied summarize-path opts.
func newSummarizeService(t *testing.T, opts ...Option) *Service {
	t.Helper()
	emb := fakeEmbedder{vec: []float32{0.1}}
	search := &recordingSearcher{}
	filter := &allowListFilter{allow: map[string]struct{}{}}
	all := append([]Option{WithLogger(quietLogger())}, opts...)
	return NewService(emb, search, filter, all...)
}

// sampleNodeNeighborhood is the canonical test fixture: one
// seed Method node with three outbound edges to three distinct
// callee Methods, plus one self-loop edge that the dedup
// helper MUST drop from the citation set.
func sampleNodeNeighborhood() SummarizeNodeNeighborhood {
	seed := SummarizeNodeCard{
		NodeID:             "11111111-1111-1111-1111-111111111111",
		RepoID:             "repo-a",
		Kind:               "method",
		CanonicalSignature: "pkg.Seed.Run()",
	}
	dst1 := SummarizeNodeCard{
		NodeID:             "22222222-2222-2222-2222-222222222222",
		RepoID:             "repo-a",
		Kind:               "method",
		CanonicalSignature: "pkg.Dst.A()",
	}
	dst2 := SummarizeNodeCard{
		NodeID:             "33333333-3333-3333-3333-333333333333",
		RepoID:             "repo-a",
		Kind:               "method",
		CanonicalSignature: "pkg.Dst.B()",
	}
	dst3 := SummarizeNodeCard{
		NodeID:             "44444444-4444-4444-4444-444444444444",
		RepoID:             "repo-a",
		Kind:               "method",
		CanonicalSignature: "pkg.Dst.C()",
	}
	return SummarizeNodeNeighborhood{
		Node: seed,
		Edges: []SummarizeEdgeCard{
			{EdgeID: "e1", RepoID: "repo-a", Kind: "calls", SrcNodeID: seed.NodeID, DstNodeID: dst1.NodeID, DstSignature: dst1.CanonicalSignature, ObservationCount: 5},
			{EdgeID: "e2", RepoID: "repo-a", Kind: "calls", SrcNodeID: seed.NodeID, DstNodeID: dst2.NodeID, DstSignature: dst2.CanonicalSignature, ObservationCount: 3},
			{EdgeID: "e3", RepoID: "repo-a", Kind: "calls", SrcNodeID: seed.NodeID, DstNodeID: dst3.NodeID, DstSignature: dst3.CanonicalSignature, ObservationCount: 1},
			// Self-loop — citation list MUST NOT include
			// a duplicate of the seed.
			{EdgeID: "e4", RepoID: "repo-a", Kind: "calls", SrcNodeID: seed.NodeID, DstNodeID: seed.NodeID, DstSignature: seed.CanonicalSignature, ObservationCount: 1},
		},
		Targets: []SummarizeNodeCard{dst1, dst2, dst3},
	}
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

func TestSummarize_validation_missingTarget(t *testing.T) {
	svc := newSummarizeService(t, WithNeighborhoodResolver(&fakeResolver{}))
	_, err := svc.Summarize(context.Background(), SummarizeRequest{})
	if !errors.Is(err, ErrSummarizeMissingTarget) {
		t.Fatalf("err = %v; want ErrSummarizeMissingTarget", err)
	}
}

func TestSummarize_validation_ambiguousTarget(t *testing.T) {
	svc := newSummarizeService(t, WithNeighborhoodResolver(&fakeResolver{}))
	_, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID:    "11111111-1111-1111-1111-111111111111",
		ConceptID: "concept-1",
		RepoID:    "repo-a",
	})
	if !errors.Is(err, ErrSummarizeAmbiguousTarget) {
		t.Fatalf("err = %v; want ErrSummarizeAmbiguousTarget", err)
	}
}

func TestSummarize_validation_conceptWithoutRepoID(t *testing.T) {
	svc := newSummarizeService(t, WithNeighborhoodResolver(&fakeResolver{}))
	_, err := svc.Summarize(context.Background(), SummarizeRequest{
		ConceptID: "concept-1",
	})
	if !errors.Is(err, ErrSummarizeRepoIDRequired) {
		t.Fatalf("err = %v; want ErrSummarizeRepoIDRequired", err)
	}
}

func TestSummarize_validation_repoMismatch(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	svc := newSummarizeService(t, WithNeighborhoodResolver(resolver))
	_, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
		RepoID: "repo-different",
	})
	if !errors.Is(err, ErrSummarizeRepoMismatch) {
		t.Fatalf("err = %v; want ErrSummarizeRepoMismatch", err)
	}
}

func TestSummarize_unconfigured_returnsErrSummarizeUnconfigured(t *testing.T) {
	// No NeighborhoodResolver wired BUT a well-formed
	// request → ErrSummarizeUnconfigured (Unimplemented).
	// Iter-3 fix item #4: validation now runs BEFORE the
	// unconfigured check, so a malformed request against an
	// unwired service surfaces InvalidArgument (see
	// TestSummarize_validation_runsBeforeUnconfiguredCheck).
	// Well-formed-but-unwired keeps Unimplemented so callers
	// can tell "this binary doesn't ship Stage 5.4" apart
	// from "your input is wrong".
	svc := newSummarizeService(t)
	_, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: "11111111-1111-1111-1111-111111111111",
	})
	if !errors.Is(err, ErrSummarizeUnconfigured) {
		t.Fatalf("err = %v; want ErrSummarizeUnconfigured", err)
	}
}

// TestSummarize_validation_runsBeforeUnconfiguredCheck pins
// the iter-3 fix for evaluator item #4: malformed requests
// against a binary that hasn't wired the verb resolver MUST
// surface `ErrSummarizeMissingTarget` (→ InvalidArgument)
// rather than `ErrSummarizeUnconfigured` (→ Unimplemented).
// Caller-correctable errors take precedence over the
// binary-composition signal so the caller sees the most
// actionable failure first.
func TestSummarize_validation_runsBeforeUnconfiguredCheck(t *testing.T) {
	svc := newSummarizeService(t) // no resolver wired
	_, err := svc.Summarize(context.Background(), SummarizeRequest{})
	if !errors.Is(err, ErrSummarizeMissingTarget) {
		t.Fatalf("err = %v; want ErrSummarizeMissingTarget (validation must run before unconfigured check)", err)
	}
	if errors.Is(err, ErrSummarizeUnconfigured) {
		t.Fatalf("err = %v; got unconfigured but request is malformed", err)
	}

	_, err = svc.Summarize(context.Background(), SummarizeRequest{
		NodeID:    "11111111-1111-1111-1111-111111111111",
		ConceptID: "22222222-2222-2222-2222-222222222222",
	})
	if !errors.Is(err, ErrSummarizeAmbiguousTarget) {
		t.Fatalf("err = %v; want ErrSummarizeAmbiguousTarget for both-set request", err)
	}

	_, err = svc.Summarize(context.Background(), SummarizeRequest{
		ConceptID: "22222222-2222-2222-2222-222222222222",
	})
	if !errors.Is(err, ErrSummarizeRepoIDRequired) {
		t.Fatalf("err = %v; want ErrSummarizeRepoIDRequired for concept-without-repo", err)
	}
}

// ---------------------------------------------------------------------------
// Node target happy path
// ---------------------------------------------------------------------------

// TestSummarize_node_happyPath_citesResolvedNodes is the
// Stage 5.4 acceptance scenario "summary cites resolved
// nodes": every citation references a row the resolver
// returned AND the SummaryMD is the summariser's verbatim
// output.
func TestSummarize_node_happyPath_citesResolvedNodes(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{
		output: SummariserOutput{SummaryMD: "## Summary\nSeed orchestrates A, B, C."},
		model:  "fake-llm-v1",
	}
	contextLog := &recordingContextLog{returnID: "ctx-happy-001"}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithContextLog(contextLog),
	)

	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	if resp.SummaryMD != summariser.output.SummaryMD {
		t.Fatalf("SummaryMD = %q; want %q", resp.SummaryMD, summariser.output.SummaryMD)
	}
	if resp.Degraded {
		t.Fatalf("Degraded = true; want false on happy path")
	}
	if resp.DegradedReason != "" {
		t.Fatalf("DegradedReason = %q; want empty", resp.DegradedReason)
	}
	if resp.TargetKind != "node" {
		t.Fatalf("TargetKind = %q; want %q", resp.TargetKind, "node")
	}
	if resp.TargetID != nb.Node.NodeID {
		t.Fatalf("TargetID = %q; want %q", resp.TargetID, nb.Node.NodeID)
	}
	if resp.ContextID != "ctx-happy-001" {
		t.Fatalf("ContextID = %q; want %q", resp.ContextID, "ctx-happy-001")
	}

	// Citations: seed + 4 edges (incl self-loop) + 3 unique
	// distinct destinations (self-loop dedupes against seed).
	// That's 1 + 4 + 3 = 8 entries.
	wantCount := 1 + len(nb.Edges) + 3
	if len(resp.Citations) != wantCount {
		t.Fatalf("len(Citations) = %d; want %d", len(resp.Citations), wantCount)
	}

	// Citation[0] is the seed.
	if resp.Citations[0].NodeID != nb.Node.NodeID {
		t.Fatalf("Citations[0].NodeID = %q; want %q", resp.Citations[0].NodeID, nb.Node.NodeID)
	}
	if resp.Citations[0].EdgeID != "" {
		t.Fatalf("Citations[0].EdgeID = %q; want empty (seed)", resp.Citations[0].EdgeID)
	}

	// Citations[1..4] are the edges in input order.
	for i, want := range nb.Edges {
		got := resp.Citations[1+i]
		if got.EdgeID != want.EdgeID {
			t.Fatalf("Citations[%d].EdgeID = %q; want %q", 1+i, got.EdgeID, want.EdgeID)
		}
		if got.NodeID != "" {
			t.Fatalf("Citations[%d].NodeID = %q; want empty (edge)", 1+i, got.NodeID)
		}
	}

	// Citations[5..7] are the unique destination nodes.
	// Self-loop dst (seed) must NOT appear here.
	dstIDs := []string{
		"22222222-2222-2222-2222-222222222222",
		"33333333-3333-3333-3333-333333333333",
		"44444444-4444-4444-4444-444444444444",
	}
	for i, wantID := range dstIDs {
		got := resp.Citations[1+len(nb.Edges)+i]
		if got.NodeID != wantID {
			t.Fatalf("Citations[%d].NodeID = %q; want %q", 1+len(nb.Edges)+i, got.NodeID, wantID)
		}
	}
	for _, c := range resp.Citations {
		if c.NodeID == nb.Node.NodeID && c != resp.Citations[0] {
			t.Fatalf("seed NodeID %q duplicated in citations beyond index 0: %+v", nb.Node.NodeID, c)
		}
	}

	// The prompt MUST be well-formed Markdown with the
	// canonical signature inlined (the rendering invariant
	// from architecture §6.1.4).
	if !strings.Contains(summariser.lastPrompt, "## Seed") {
		t.Fatalf("prompt missing `## Seed` heading: %q", summariser.lastPrompt)
	}
	if !strings.Contains(summariser.lastPrompt, "pkg.Seed.Run()") {
		t.Fatalf("prompt missing seed signature: %q", summariser.lastPrompt)
	}

	// Context log invariant: exactly one row, verb=summarize.
	if contextLog.callCount != 1 {
		t.Fatalf("contextLog appended %d rows; want 1", contextLog.callCount)
	}
	if contextLog.lastInput.Verb != "summarize" {
		t.Fatalf("contextLog.Verb = %q; want %q", contextLog.lastInput.Verb, "summarize")
	}
	if contextLog.lastInput.RepoID != "repo-a" {
		t.Fatalf("contextLog.RepoID = %q; want %q", contextLog.lastInput.RepoID, "repo-a")
	}
	if contextLog.lastInput.ServedUnderDegraded {
		t.Fatalf("contextLog.ServedUnderDegraded = true; want false")
	}
	if len(contextLog.lastInput.NodeIDs) != 4 {
		// 1 seed + 3 unique dsts.
		t.Fatalf("contextLog.NodeIDs len = %d; want 4", len(contextLog.lastInput.NodeIDs))
	}
	if len(contextLog.lastInput.EdgeIDs) != len(nb.Edges) {
		t.Fatalf("contextLog.EdgeIDs len = %d; want %d", len(contextLog.lastInput.EdgeIDs), len(nb.Edges))
	}
	if contextLog.lastInput.RerankerModelVersion != V0ModelVersion {
		// No Reranker wired → falls back to v0 cold start.
		t.Fatalf("contextLog.RerankerModelVersion = %q; want %q",
			contextLog.lastInput.RerankerModelVersion, V0ModelVersion)
	}

	// query_json round-trips back to readable shape.
	var got map[string]any
	if err := json.Unmarshal(contextLog.lastInput.QueryJSON, &got); err != nil {
		t.Fatalf("QueryJSON: %v", err)
	}
	if got["node_id"] != nb.Node.NodeID {
		t.Fatalf("query_json.node_id = %v; want %q", got["node_id"], nb.Node.NodeID)
	}
}

// ---------------------------------------------------------------------------
// Concept target happy path
// ---------------------------------------------------------------------------

func TestSummarize_concept_happyPath(t *testing.T) {
	concept := SummarizeConceptCard{
		ConceptID:     "55555555-5555-5555-5555-555555555555",
		RepoID:        "repo-a",
		Name:          "Locking strategy",
		DescriptionMD: "Concept describes the lock acquisition order.",
	}
	resolver := &fakeResolver{
		concepts: map[string]SummarizeConceptCard{concept.ConceptID: concept},
	}
	summariser := &fakeSummariser{
		output: SummariserOutput{SummaryMD: "## Locking strategy\nDescribes lock order."},
	}
	contextLog := &recordingContextLog{returnID: "ctx-concept-001"}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithContextLog(contextLog),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		ConceptID: concept.ConceptID,
		RepoID:    "repo-a",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if resp.TargetKind != "concept" {
		t.Fatalf("TargetKind = %q; want %q", resp.TargetKind, "concept")
	}
	if resp.TargetID != concept.ConceptID {
		t.Fatalf("TargetID = %q; want %q", resp.TargetID, concept.ConceptID)
	}
	if len(resp.Citations) != 1 || resp.Citations[0].ConceptID != concept.ConceptID {
		t.Fatalf("Citations = %+v; want single concept", resp.Citations)
	}
	if contextLog.lastInput.Verb != "summarize" {
		t.Fatalf("Verb = %q; want summarize", contextLog.lastInput.Verb)
	}
	if len(contextLog.lastInput.ConceptIDs) != 1 {
		t.Fatalf("ConceptIDs len = %d; want 1", len(contextLog.lastInput.ConceptIDs))
	}
	if !strings.Contains(summariser.lastPrompt, "## Concept") {
		t.Fatalf("prompt missing `## Concept` heading")
	}
	if resolver.lastRepoID != "repo-a" {
		t.Fatalf("resolver.lastRepoID = %q; want repo-a (FetchConcept must receive repo scope)",
			resolver.lastRepoID)
	}
}

// TestSummarize_concept_citesSupportingNodesAndEpisodes pins
// the iter-3 fix for evaluator item #2: a concept summary
// MUST cite each supporting `concept_support` row (Node /
// Episode) the resolver returned. Mirrors the e2e scenario
// "summary cites resolved nodes" (concept-target branch).
func TestSummarize_concept_citesSupportingNodesAndEpisodes(t *testing.T) {
	concept := SummarizeConceptCard{
		ConceptID:     "55555555-5555-5555-5555-555555555555",
		RepoID:        "repo-a",
		Name:          "Locking strategy",
		DescriptionMD: "Acquisition order.",
		Supports: []SummarizeConceptSupport{
			{SupportID: "s1", NodeID: "n-node-1", NodeKind: "method", NodeSignature: "pkg.Mu.Lock()", Polarity: "positive"},
			{SupportID: "s2", NodeID: "n-node-2", NodeKind: "method", NodeSignature: "pkg.Mu.Unlock()", Polarity: "positive"},
			{SupportID: "s3", EpisodeID: "ep-deadlock-001", Polarity: "negative"},
			// Duplicate node + duplicate episode (only one
			// citation each should appear).
			{SupportID: "s4", NodeID: "n-node-1", NodeKind: "method", NodeSignature: "pkg.Mu.Lock()", Polarity: "positive"},
			{SupportID: "s5", EpisodeID: "ep-deadlock-001", Polarity: "negative"},
		},
	}
	resolver := &fakeResolver{
		concepts: map[string]SummarizeConceptCard{concept.ConceptID: concept},
	}
	summariser := &fakeSummariser{
		output: SummariserOutput{SummaryMD: "## Locking\nLock then unlock."},
	}
	contextLog := &recordingContextLog{returnID: "ctx-concept-supports-001"}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithContextLog(contextLog),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		ConceptID: concept.ConceptID,
		RepoID:    "repo-a",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}

	// Expected citations: 1 concept + 2 unique Nodes + 1 unique Episode = 4.
	wantCitations := 4
	if len(resp.Citations) != wantCitations {
		t.Fatalf("len(Citations) = %d; want %d. citations=%+v",
			len(resp.Citations), wantCitations, resp.Citations)
	}
	if resp.Citations[0].ConceptID != concept.ConceptID {
		t.Fatalf("Citations[0].ConceptID = %q; want %q",
			resp.Citations[0].ConceptID, concept.ConceptID)
	}
	// Nodes appear before episodes.
	if resp.Citations[1].NodeID != "n-node-1" {
		t.Fatalf("Citations[1].NodeID = %q; want n-node-1", resp.Citations[1].NodeID)
	}
	if resp.Citations[2].NodeID != "n-node-2" {
		t.Fatalf("Citations[2].NodeID = %q; want n-node-2", resp.Citations[2].NodeID)
	}
	if resp.Citations[3].EpisodeID != "ep-deadlock-001" {
		t.Fatalf("Citations[3].EpisodeID = %q; want ep-deadlock-001", resp.Citations[3].EpisodeID)
	}
	// Episode citation has no NodeID/ConceptID set.
	if resp.Citations[3].NodeID != "" || resp.Citations[3].ConceptID != "" {
		t.Fatalf("Citations[3] should be episode-only; got %+v", resp.Citations[3])
	}

	// Context log: NodeIDs include unique node supports;
	// ConceptIDs include the seed concept. EpisodeIDs are
	// NOT persisted (recall_context_log schema only has
	// node/edge/concept arrays per migration 0015).
	if len(contextLog.lastInput.NodeIDs) != 2 {
		t.Fatalf("contextLog.NodeIDs = %v; want 2 (unique support nodes)",
			contextLog.lastInput.NodeIDs)
	}
	if len(contextLog.lastInput.ConceptIDs) != 1 {
		t.Fatalf("contextLog.ConceptIDs = %v; want 1 (seed concept)",
			contextLog.lastInput.ConceptIDs)
	}

	// Prompt + template both surface the supports section
	// so the LLM can ground the summary AND the degraded
	// path stays informative.
	if !strings.Contains(summariser.lastPrompt, "## Supports") {
		t.Fatalf("prompt missing `## Supports` section: %q", summariser.lastPrompt)
	}
	if !strings.Contains(summariser.lastPrompt, "pkg.Mu.Lock()") {
		t.Fatalf("prompt missing support node signature: %q", summariser.lastPrompt)
	}
	if !strings.Contains(summariser.lastPrompt, "ep-deadlock-001") {
		t.Fatalf("prompt missing support episode id: %q", summariser.lastPrompt)
	}
}

// TestSummarize_concept_supportsBounded caps support fan-out
// at maxSummarizeConceptSupports so a concept that accrued
// thousands of `concept_support` rows over time cannot
// inflate the citation array unboundedly.
func TestSummarize_concept_supportsBounded(t *testing.T) {
	supports := make([]SummarizeConceptSupport, 0, maxSummarizeConceptSupports+10)
	for i := 0; i < maxSummarizeConceptSupports+10; i++ {
		supports = append(supports, SummarizeConceptSupport{
			SupportID:     fmt.Sprintf("s%d", i),
			NodeID:        fmt.Sprintf("node-%d", i),
			NodeKind:      "method",
			NodeSignature: fmt.Sprintf("pkg.N%d()", i),
			Polarity:      "positive",
		})
	}
	concept := SummarizeConceptCard{
		ConceptID: "55555555-5555-5555-5555-555555555555",
		RepoID:    "repo-a",
		Name:      "Big concept",
		Supports:  supports,
	}
	resolver := &fakeResolver{
		concepts: map[string]SummarizeConceptCard{concept.ConceptID: concept},
	}
	summariser := &fakeSummariser{output: SummariserOutput{SummaryMD: "ok"}}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		ConceptID: concept.ConceptID,
		RepoID:    "repo-a",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	// 1 concept + maxSummarizeConceptSupports unique nodes.
	want := 1 + maxSummarizeConceptSupports
	if len(resp.Citations) != want {
		t.Fatalf("len(Citations) = %d; want %d (cap = %d)",
			len(resp.Citations), want, maxSummarizeConceptSupports)
	}
}

// ---------------------------------------------------------------------------
// Target not found
// ---------------------------------------------------------------------------

func TestSummarize_node_targetNotFound_returnsErr(t *testing.T) {
	resolver := &fakeResolver{
		nodeErr: map[string]error{
			"99999999-9999-9999-9999-999999999999": ErrSummarizeTargetNotFound,
		},
	}
	svc := newSummarizeService(t, WithNeighborhoodResolver(resolver))
	_, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: "99999999-9999-9999-9999-999999999999",
	})
	if !errors.Is(err, ErrSummarizeTargetNotFound) {
		t.Fatalf("err = %v; want ErrSummarizeTargetNotFound", err)
	}
}

func TestSummarize_node_emptyCardTreatedAsNotFound(t *testing.T) {
	// Resolver returns (zero card, nil) — defensive branch
	// must still surface NotFound.
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{
			"88888888-8888-8888-8888-888888888888": {},
		},
	}
	svc := newSummarizeService(t, WithNeighborhoodResolver(resolver))
	_, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: "88888888-8888-8888-8888-888888888888",
	})
	if !errors.Is(err, ErrSummarizeTargetNotFound) {
		t.Fatalf("err = %v; want ErrSummarizeTargetNotFound", err)
	}
}

// ---------------------------------------------------------------------------
// Graph store unavailable
// ---------------------------------------------------------------------------

func TestSummarize_node_graphUnavailable_degradedEnvelope(t *testing.T) {
	resolver := &fakeResolver{
		nodeErr: map[string]error{
			"77777777-7777-7777-7777-777777777777": ErrGraphStoreUnavailable,
		},
	}
	contextLog := &recordingContextLog{returnID: "ctx-graph-down-001"}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithContextLog(contextLog),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: "77777777-7777-7777-7777-777777777777",
		RepoID: "repo-a",
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("Degraded = false; want true")
	}
	if resp.DegradedReason != DegradedReasonGraphStoreUnavailable {
		t.Fatalf("DegradedReason = %q; want %q",
			resp.DegradedReason, DegradedReasonGraphStoreUnavailable)
	}
	if len(resp.Citations) != 0 {
		t.Fatalf("Citations = %+v; want empty under graph outage", resp.Citations)
	}
	if !strings.Contains(resp.SummaryMD, "graph store unreachable") {
		t.Fatalf("SummaryMD missing graph-unreachable template marker: %q", resp.SummaryMD)
	}
	// Iter-3 fix item #3: implementation-plan §843-844
	// requires the recall_context_log row even on the
	// graph-outage path. We assert the appender ran AND
	// the response carries the resulting context id.
	if contextLog.callCount != 1 {
		t.Fatalf("contextLog.callCount = %d; want 1 (graph-outage MUST still append)",
			contextLog.callCount)
	}
	if contextLog.lastInput.Verb != "summarize" {
		t.Fatalf("contextLog.Verb = %q; want summarize", contextLog.lastInput.Verb)
	}
	if !contextLog.lastInput.ServedUnderDegraded {
		t.Fatalf("contextLog.ServedUnderDegraded = false; want true on graph outage")
	}
	if resp.ContextID != "ctx-graph-down-001" {
		t.Fatalf("ContextID = %q; want %q", resp.ContextID, "ctx-graph-down-001")
	}
}

// TestSummarize_node_graphUnavailable_emptyRepoID_stillAppends
// covers the iter-3 fix for item #3 in the corner case
// where the caller omitted repo_id on a node target AND
// the graph store is down (so the seed lookup can't derive
// one). The appender adapter soft-fails on empty repo (the
// recall path's existing semantics — log + empty context_id)
// but the verb MUST still invoke it; silently skipping the
// append was the iter-2 regression.
func TestSummarize_node_graphUnavailable_emptyRepoID_stillAppends(t *testing.T) {
	resolver := &fakeResolver{
		nodeErr: map[string]error{
			"77777777-7777-7777-7777-777777777777": ErrGraphStoreUnavailable,
		},
	}
	// The appender returns an error to simulate the
	// "empty repo_id rejected by the writer" soft-failure
	// shape — the verb should swallow it and return an
	// empty ContextID, NOT skip the call.
	contextLog := &recordingContextLog{returnErr: errors.New("invalid repo_id")}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithContextLog(contextLog),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: "77777777-7777-7777-7777-777777777777",
		// RepoID intentionally empty.
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !resp.Degraded || resp.DegradedReason != DegradedReasonGraphStoreUnavailable {
		t.Fatalf("response not in graph-unavailable degraded shape: %+v", resp)
	}
	if contextLog.callCount != 1 {
		t.Fatalf("contextLog.callCount = %d; want 1 (must attempt append even with empty repo_id)",
			contextLog.callCount)
	}
	if resp.ContextID != "" {
		t.Fatalf("ContextID = %q; want empty (soft-failure swallowed)", resp.ContextID)
	}
}

// ---------------------------------------------------------------------------
// Summariser timeout — partial output discarded, template
// fallback, degraded=true
// ---------------------------------------------------------------------------

// TestSummarize_summariserTimeout_returnsDegradedTemplate is
// the Stage 5.4 acceptance scenario "timeout returns degraded
// summary". The summariser sleeps past the 1 ms budget; the
// verb must (a) abandon the LLM call, (b) render the
// deterministic template, (c) mark degraded=true with
// `summariser_unavailable` (no freshness source wired).
func TestSummarize_summariserTimeout_returnsDegradedTemplate(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{
		// Sleeps far longer than the 1 ms budget. The fake
		// honours ctx.Done so the goroutine actually returns
		// instead of leaking.
		sleep:  50 * time.Millisecond,
		output: SummariserOutput{SummaryMD: "## partial that must not leak"},
	}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithSummariserTimeout(1*time.Millisecond),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("Degraded = false; want true on timeout")
	}
	if resp.DegradedReason != DegradedReasonSummariserUnavailable {
		t.Fatalf("DegradedReason = %q; want %q",
			resp.DegradedReason, DegradedReasonSummariserUnavailable)
	}
	// CRITICAL: partial output MUST NOT leak into the
	// response. Even though the fake's `output` field is
	// non-empty, the timeout path returns ctx.Err() before
	// reading it — but the bigger guard is that the verb
	// renders the template instead.
	if strings.Contains(resp.SummaryMD, "partial that must not leak") {
		t.Fatalf("SummaryMD leaked partial LLM output: %q", resp.SummaryMD)
	}
	if !strings.Contains(resp.SummaryMD, "Summariser unavailable") {
		t.Fatalf("SummaryMD missing template marker: %q", resp.SummaryMD)
	}
	// Citations MUST still be populated — the timeout did
	// not break the graph resolution.
	if len(resp.Citations) == 0 {
		t.Fatalf("Citations empty on degraded path; want graph citations")
	}
}

// TestSummarize_summariserError_partialOutputDiscarded
// proves a summariser that returns BOTH a partial SummaryMD
// AND a non-nil error has its partial output discarded.
func TestSummarize_summariserError_partialOutputDiscarded(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{
		output: SummariserOutput{SummaryMD: "partial response that must NOT surface"},
		err:    errors.New("upstream 500"),
	}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if !resp.Degraded {
		t.Fatalf("Degraded = false; want true on summariser err")
	}
	if strings.Contains(resp.SummaryMD, "partial response that must NOT surface") {
		t.Fatalf("partial output leaked: %q", resp.SummaryMD)
	}
}

// ---------------------------------------------------------------------------
// Freshness classifier
// ---------------------------------------------------------------------------

func TestSummarize_rerankerStale_surfacesStaleReason(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{err: errors.New("summariser down")}
	freshness := &fakeFreshness{
		trainedAt: time.Now().Add(-8 * 24 * time.Hour),
		ok:        true,
	}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithRerankerFreshness(freshness),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if resp.DegradedReason != DegradedReasonRerankerModelStale {
		t.Fatalf("DegradedReason = %q; want %q",
			resp.DegradedReason, DegradedReasonRerankerModelStale)
	}
	if freshness.calls == 0 {
		t.Fatalf("freshness source not consulted")
	}
}

func TestSummarize_rerankerFresh_surfacesSummariserUnavailable(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{err: errors.New("summariser down")}
	freshness := &fakeFreshness{
		trainedAt: time.Now().Add(-1 * time.Hour),
		ok:        true,
	}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithRerankerFreshness(freshness),
	)
	resp, _ := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if resp.DegradedReason != DegradedReasonSummariserUnavailable {
		t.Fatalf("DegradedReason = %q; want %q",
			resp.DegradedReason, DegradedReasonSummariserUnavailable)
	}
}

func TestSummarize_freshnessSourceError_fallsBackToSummariserUnavailable(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{err: errors.New("summariser down")}
	freshness := &fakeFreshness{err: errors.New("pg down")}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithRerankerFreshness(freshness),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if resp.DegradedReason != DegradedReasonSummariserUnavailable {
		t.Fatalf("DegradedReason = %q; want %q (freshness lookup err → conservative)",
			resp.DegradedReason, DegradedReasonSummariserUnavailable)
	}
}

func TestSummarize_freshnessSourceNoRow_fallsBackToSummariserUnavailable(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{err: errors.New("summariser down")}
	freshness := &fakeFreshness{ok: false}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithRerankerFreshness(freshness),
	)
	resp, _ := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if resp.DegradedReason != DegradedReasonSummariserUnavailable {
		t.Fatalf("DegradedReason = %q; want %q (cold-start: no row)",
			resp.DegradedReason, DegradedReasonSummariserUnavailable)
	}
}

// ---------------------------------------------------------------------------
// Parent ctx cancellation propagates as hard error
// ---------------------------------------------------------------------------

func TestSummarize_parentCtxCanceled_propagatesAsHardError(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{
		sleep:  100 * time.Millisecond,
		output: SummariserOutput{SummaryMD: "should never appear"},
	}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
	)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a brief delay so the call has reached
	// the summariser before the parent ctx dies.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	resp, err := svc.Summarize(ctx, SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err == nil {
		t.Fatalf("err = nil; want propagated cancellation. resp = %+v", resp)
	}
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want context.Canceled or DeadlineExceeded", err)
	}
}

// ---------------------------------------------------------------------------
// Context log soft-failure → empty ContextID
// ---------------------------------------------------------------------------

func TestSummarize_appendSoftFailure_returnsResponseWithEmptyContextID(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	summariser := &fakeSummariser{
		output: SummariserOutput{SummaryMD: "ok"},
	}
	contextLog := &recordingContextLog{returnErr: errors.New("pg dropped connection")}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
		WithContextLog(contextLog),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: nb.Node.NodeID,
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	if resp.ContextID != "" {
		t.Fatalf("ContextID = %q; want empty on appender failure", resp.ContextID)
	}
	if resp.SummaryMD != "ok" {
		t.Fatalf("SummaryMD = %q; want ok (append failure is soft)", resp.SummaryMD)
	}
	if contextLog.callCount != 1 {
		t.Fatalf("contextLog.callCount = %d; want 1", contextLog.callCount)
	}
}

// ---------------------------------------------------------------------------
// max_tokens clamping
// ---------------------------------------------------------------------------

func TestSummarize_maxTokensClamping(t *testing.T) {
	nb := sampleNodeNeighborhood()
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{nb.Node.NodeID: nb},
	}
	cases := []struct {
		name       string
		in         int
		wantPassed int
	}{
		{"zero→default", 0, defaultSummarizeMaxTokens},
		{"negative→default", -100, defaultSummarizeMaxTokens},
		{"too-large→cap", 99999, maxSummarizeMaxTokens},
		{"in-range→identity", 256, 256},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			summariser := &fakeSummariser{
				output: SummariserOutput{SummaryMD: "ok"},
			}
			svc := newSummarizeService(t,
				WithNeighborhoodResolver(resolver),
				WithSummariser(summariser),
			)
			_, err := svc.Summarize(context.Background(), SummarizeRequest{
				NodeID:    nb.Node.NodeID,
				MaxTokens: tc.in,
			})
			if err != nil {
				t.Fatalf("Summarize: %v", err)
			}
			if summariser.lastMaxToks != tc.wantPassed {
				t.Fatalf("summariser.MaxTokens = %d; want %d",
					summariser.lastMaxToks, tc.wantPassed)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Edge fan-out cap
// ---------------------------------------------------------------------------

func TestSummarize_edgeFanoutCapped(t *testing.T) {
	seed := SummarizeNodeCard{
		NodeID:             "11111111-1111-1111-1111-111111111111",
		RepoID:             "repo-a",
		Kind:               "method",
		CanonicalSignature: "pkg.Seed.Run()",
	}
	// Generate maxSummarizeEdges+10 distinct destinations.
	edges := make([]SummarizeEdgeCard, 0, maxSummarizeEdges+10)
	targets := make([]SummarizeNodeCard, 0, maxSummarizeEdges+10)
	for i := 0; i < maxSummarizeEdges+10; i++ {
		dstID := fmt.Sprintf("%08d-0000-0000-0000-000000000000", i+1)
		edges = append(edges, SummarizeEdgeCard{
			EdgeID: fmt.Sprintf("e%d", i), RepoID: "repo-a", Kind: "calls",
			SrcNodeID: seed.NodeID, DstNodeID: dstID,
			DstSignature: fmt.Sprintf("pkg.Dst.F%d()", i),
		})
		targets = append(targets, SummarizeNodeCard{
			NodeID: dstID, RepoID: "repo-a", Kind: "method",
			CanonicalSignature: fmt.Sprintf("pkg.Dst.F%d()", i),
		})
	}
	nb := SummarizeNodeNeighborhood{Node: seed, Edges: edges, Targets: targets}
	resolver := &fakeResolver{
		nodes: map[string]SummarizeNodeNeighborhood{seed.NodeID: nb},
	}
	summariser := &fakeSummariser{output: SummariserOutput{SummaryMD: "ok"}}
	svc := newSummarizeService(t,
		WithNeighborhoodResolver(resolver),
		WithSummariser(summariser),
	)
	resp, err := svc.Summarize(context.Background(), SummarizeRequest{
		NodeID: seed.NodeID,
	})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	// 1 seed + maxSummarizeEdges edges + maxSummarizeEdges
	// distinct dsts (one per edge).
	wantCitations := 1 + maxSummarizeEdges + maxSummarizeEdges
	if len(resp.Citations) != wantCitations {
		t.Fatalf("len(Citations) = %d; want %d (cap = %d)",
			len(resp.Citations), wantCitations, maxSummarizeEdges)
	}
}
