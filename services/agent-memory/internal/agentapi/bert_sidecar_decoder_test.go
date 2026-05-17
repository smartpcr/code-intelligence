package agentapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBertSidecarDecoder_RejectsNonFileURI is the negative
// gate: the decoder must NOT claim recognition over schemes
// it cannot serve. The MultiArtifactDecoder relies on this
// to fall through to the next decoder in the chain.
func TestBertSidecarDecoder_RejectsNonFileURI(t *testing.T) {
	t.Parallel()
	dec := NewBertSidecarDecoder("http://sidecar:8088", nil)
	for _, uri := range []string{
		"",
		"data:application/json;base64,abc",
		"s3://bucket/object",
		"http://example.com/model",
	} {
		r, ok, err := dec.Decode(uri)
		if r != nil || ok || err != nil {
			t.Fatalf("uri=%q: want (nil,false,nil); got (%v,%v,%v)", uri, r, ok, err)
		}
	}
}

// TestBertSidecarDecoder_RecognisesFileURI is the positive
// gate. The returned Reranker is request-scoped — the
// PublishedReranker calls Rank() on it once per recall.
func TestBertSidecarDecoder_RecognisesFileURI(t *testing.T) {
	t.Parallel()
	dec := NewBertSidecarDecoder("http://sidecar:8088", nil)
	r, ok, err := dec.Decode("file:///var/lib/reranker-sidecar/models/abc123")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if !ok {
		t.Fatalf("file:// URI not recognised")
	}
	if r == nil {
		t.Fatalf("recognised file:// URI but got nil Reranker")
	}
	if !strings.Contains(r.ModelVersion(), "abc123") {
		t.Fatalf("ModelVersion=%q does not carry version suffix", r.ModelVersion())
	}
}

// TestBertSidecarDecoder_PostsCandidatesAndMergesScores
// asserts the recall-time HTTP round-trip:
//   - POSTs the candidates with the artifact URI to /rank
//   - merges the sidecar's per-PointID scores back onto the
//     input candidates
//   - returns them sorted descending by the new FinalScore
//
// This is the iter-3 review item 1 acceptance test: a
// trained BERT artifact must actually be consumed at recall
// time, not just published.
func TestBertSidecarDecoder_PostsCandidatesAndMergesScores(t *testing.T) {
	t.Parallel()
	var gotPath string
	var gotBody bertRankRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		gotPath = req.URL.Path
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scored":[{"point_id":"p2","score":0.95},{"point_id":"p1","score":0.42}]}`))
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{
		HTTPClient: srv.Client(),
	})
	r, ok, err := dec.Decode("file:///models/v9")
	if err != nil || !ok {
		t.Fatalf("decode: ok=%v err=%v", ok, err)
	}

	in := []Candidate{
		{PointID: "p1", Score: 0.5, Kind: "method", Payload: map[string]any{"text": "alpha"}},
		{PointID: "p2", Score: 0.4, Kind: "method", Payload: map[string]any{"text": "beta"}},
	}
	out := r.Rank(in)

	if gotPath != "/rank" {
		t.Fatalf("sidecar received path %q, want /rank", gotPath)
	}
	if gotBody.ArtifactURI != "file:///models/v9" {
		t.Fatalf("sidecar got artifact_uri=%q, want file:///models/v9", gotBody.ArtifactURI)
	}
	if len(gotBody.Candidates) != 2 {
		t.Fatalf("sidecar got %d candidates, want 2", len(gotBody.Candidates))
	}
	// iter-8 review item 1: the wire `point_id` and `text`
	// fields carry the GRAPH id (from payload.node_id /
	// payload.concept_id) when available. When the payload
	// does NOT carry a graph id (this test's case: only
	// "text" in payload, no node_id), `candidateGraphID`
	// defensively falls back to `c.PointID`, so the wire
	// `text` MUST equal the input PointID. The
	// `TestBertSidecarDecoder_PostsGraphIDFromPayload` test
	// (below) covers the production-shaped path where the
	// payload carries node_id and the wire fields receive
	// the graph id instead of the qdrant_point_id.
	if gotBody.Candidates[0].Text != "p1" || gotBody.Candidates[1].Text != "p2" {
		t.Fatalf("iter-8 item 1: candidate.text MUST be the bare graph id (falls back to PointID when payload lacks node_id): %+v", gotBody.Candidates)
	}
	if gotBody.Candidates[0].PointID != "p1" || gotBody.Candidates[1].PointID != "p2" {
		t.Fatalf("iter-8 item 1: candidate.point_id wire field MUST be the bare graph id (falls back to PointID when payload lacks node_id): %+v", gotBody.Candidates)
	}

	if len(out) != 2 {
		t.Fatalf("Rank returned %d items, want 2", len(out))
	}
	// p2 should rank first because its sidecar score (0.95)
	// exceeds p1's (0.42).
	if out[0].PointID != "p2" || out[1].PointID != "p1" {
		t.Fatalf("Rank order wrong: %v", []string{out[0].PointID, out[1].PointID})
	}
	if out[0].FinalScore < 0.94 || out[0].FinalScore > 0.96 {
		t.Fatalf("FinalScore not merged: p2.FinalScore=%v", out[0].FinalScore)
	}
}

// TestBertSidecarDecoder_FailureFallsBackToInput asserts the
// graceful-degradation contract: a sidecar outage must NOT
// drop the recall, it must surface the input ordering with
// FinalScore=Score. The PublishedReranker wrapping this
// still advertises the trained version on the envelope.
func TestBertSidecarDecoder_FailureFallsBackToInput(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{HTTPClient: srv.Client()})
	r, _, _ := dec.Decode("file:///models/v9")

	in := []Candidate{
		{PointID: "p1", Score: 0.5, Kind: "method"},
		{PointID: "p2", Score: 0.4, Kind: "method"},
	}
	out := r.Rank(in)
	if len(out) != 2 || out[0].PointID != "p1" || out[1].PointID != "p2" {
		t.Fatalf("expected input order preserved on failure, got %v", out)
	}
	if out[0].FinalScore != 0.5 {
		t.Fatalf("expected FinalScore=Score fallback, got %v", out[0].FinalScore)
	}
}

// TestBertSidecarDecoder_EmptyCandidatesSkipsRoundTrip
// asserts no HTTP call fires when the input is empty —
// avoids load on the sidecar for trivial requests.
func TestBertSidecarDecoder_EmptyCandidatesSkipsRoundTrip(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		called = true
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{HTTPClient: srv.Client()})
	r, _, _ := dec.Decode("file:///models/v9")
	out := r.Rank(nil)
	if called {
		t.Fatalf("Rank(nil) round-tripped to sidecar; should short-circuit")
	}
	if out != nil {
		t.Fatalf("Rank(nil) returned %v; want nil", out)
	}
}

// TestBertSidecarDecoder_EmptyScoredArrayIsFailure asserts
// that a 200-OK with an empty scored array is treated as a
// failure (and falls back), not as a successful "rank
// nothing" response. The sidecar is contractually obligated
// to return one score per requested candidate.
func TestBertSidecarDecoder_EmptyScoredArrayIsFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scored":[]}`))
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{HTTPClient: srv.Client()})
	r, _, _ := dec.Decode("file:///models/v9")
	in := []Candidate{{PointID: "p1", Score: 0.7, Kind: "method"}}
	out := r.Rank(in)
	if len(out) != 1 || out[0].PointID != "p1" {
		t.Fatalf("fallback did not preserve input: %v", out)
	}
	if out[0].FinalScore != 0.7 {
		t.Fatalf("fallback should set FinalScore=Score, got %v", out[0].FinalScore)
	}
}

// TestBertSidecarDecoder_Timeout_DegradesGracefully asserts
// the per-recall timeout doesn't cascade into a recall-level
// timeout — a slow sidecar degrades to V0-shaped fallback.
func TestBertSidecarDecoder_Timeout_DegradesGracefully(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"scored":[{"point_id":"p1","score":0.99}]}`))
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{
		HTTPClient: &http.Client{Timeout: 5 * time.Millisecond},
	})
	r, _, _ := dec.Decode("file:///models/v9")
	in := []Candidate{{PointID: "p1", Score: 0.3, Kind: "method"}}
	out := r.Rank(in)
	if len(out) != 1 || out[0].PointID != "p1" {
		t.Fatalf("timeout fallback did not preserve input")
	}
	if out[0].FinalScore != 0.3 {
		t.Fatalf("timeout fallback should set FinalScore=Score, got %v", out[0].FinalScore)
	}
}

// TestBertSidecarDecoder_ConfiguredTimeoutOverridesDefault is
// the regression guard for iter-19 evaluator item 5: the
// per-call `context.WithTimeout` MUST honour
// `BertSidecarConfig.Timeout` instead of hard-coding
// `DefaultBertSidecarTimeout`. Before iter-20, an operator
// configuring a larger timeout (e.g. 2s for a slower model)
// would still see every sidecar call cut off at 750ms.
//
// The test sleeps the sidecar for 900ms — longer than the
// default (750ms), shorter than the configured timeout (2s).
// With the fix: the call completes; without it: the per-ctx
// timeout cuts the call at 750ms and we fall back to V0.
func TestBertSidecarDecoder_ConfiguredTimeoutOverridesDefault(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		time.Sleep(900 * time.Millisecond)
		_, _ = w.Write([]byte(`{"scored":[{"point_id":"p1","score":0.99}]}`))
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{
		Timeout: 2 * time.Second,
	})
	r, _, _ := dec.Decode("file:///models/v9")

	// Use RankErr so we can distinguish "sidecar succeeded
	// and returned the trained score" from "sidecar failed
	// and we fell back to input order with FinalScore=Score"
	// — Rank() papers over the difference and would let the
	// regression slip through.
	fr, ok := r.(FallibleReranker)
	if !ok {
		t.Fatalf("bertSidecarReranker should satisfy FallibleReranker; got %T", r)
	}
	in := []Candidate{{PointID: "p1", Score: 0.3, Kind: "method", Payload: map[string]any{"node_id": "p1"}}}
	out, err := fr.RankErr(in)
	if err != nil {
		t.Fatalf("RankErr: configured 2s timeout should have honoured 900ms sidecar latency, got error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(out))
	}
	if out[0].FinalScore != 0.99 {
		t.Fatalf("expected trained sidecar score (0.99), got %v (per-ctx timeout likely capped at default)",
			out[0].FinalScore)
	}
}

// TestCandidateGraphID_PrefersPayloadGraphIDThenPointID
// locks in the iter-8 review item 1 + item 2 contract: the
// per-candidate document surface for the BERT sidecar is
// the GRAPH id (payload.node_id for method/block/frontier,
// payload.concept_id for concept), NOT the Qdrant
// qdrant_point_id. The qdrant_point_id and the graph
// node_id are minted separately by the publisher (see
// `internal/embedding/publisher.go:650-658`), so they are
// not equal in production — iter-7's bare-PointID fix
// silently scored on the wrong identifier.
//
// Cases below also cover the iter-7 item 2 regression
// (frontier candidates with PointID=""): they DO carry a
// graph id via their explicitly-stamped payload.node_id
// (`appendFrontierCandidates` in recall.go:1106-1107), so
// `candidateGraphID` returns that — keeps frontier
// candidates in the scored set instead of dropping the
// whole recall back to V0 with a sidecar 400.
func TestCandidateGraphID_PrefersPayloadGraphIDThenPointID(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		c    Candidate
		want string
	}{
		// iter-8 item 1: production shape — payload carries
		// the graph node_id, which differs from the Qdrant
		// qdrant_point_id (PointID). The graph node_id is
		// what training carries in
		// LabelledPair.SeedNodeIDs, so it MUST be the doc
		// surface.
		{
			"prefers payload.node_id over Qdrant PointID",
			Candidate{Kind: "method", PointID: "qdrant-uuid-99", Payload: map[string]any{"node_id": "graph-node-uuid-7"}},
			"graph-node-uuid-7",
		},
		// concept candidates: payload key is concept_id per
		// recall.go:1269.
		{
			"prefers payload.concept_id for concepts",
			Candidate{Kind: "concept", PointID: "qdrant-uuid-99", Payload: map[string]any{"concept_id": "concept-uuid-3"}},
			"concept-uuid-3",
		},
		// iter-8 item 2 regression guard: frontier
		// candidates have PointID="" (they were never
		// embedded into Qdrant — they're pure graph-only
		// nodes appended by appendFrontierCandidates).
		// Pre-iter-8 this caused the sidecar to 400 on the
		// whole batch and force PublishedReranker back to
		// V0. Now `candidateGraphID` returns the explicit
		// payload.node_id stamp and the candidate is
		// scorable.
		{
			"frontier candidate (PointID=\"\") still scorable via payload.node_id",
			Candidate{Kind: "method", PointID: "", Payload: map[string]any{"node_id": "frontier-node-uuid-42"}},
			"frontier-node-uuid-42",
		},
		// node_id wins over concept_id when both are
		// present (consistent with recall.go preferring
		// node-shaped reads).
		{
			"node_id wins over concept_id when both present",
			Candidate{Kind: "method", PointID: "p1", Payload: map[string]any{"node_id": "n7", "concept_id": "c3"}},
			"n7",
		},
		// Defensive fallback when payload is absent
		// entirely (test-only shape — production payloads
		// always carry a graph id because the publisher
		// writes one on every row).
		{
			"falls back to PointID when payload is nil",
			Candidate{Kind: "method", PointID: "p1"},
			"p1",
		},
		{
			"falls back to PointID when payload lacks both keys",
			Candidate{Kind: "method", PointID: "p1", Payload: map[string]any{"text": "T", "summary": "S"}},
			"p1",
		},
		// Defensive: empty-string payload values do NOT win
		// over PointID — falls through to the next key
		// then to PointID.
		{
			"ignores empty payload.node_id and falls through",
			Candidate{Kind: "method", PointID: "p1", Payload: map[string]any{"node_id": "", "concept_id": "c3"}},
			"c3",
		},
		// Worst case: nothing usable at all → empty string,
		// which `callSidecar` uses as the filter-out signal
		// to keep the rest of the batch scorable.
		{
			"returns empty when nothing is usable",
			Candidate{Kind: "method", PointID: "", Payload: map[string]any{"text": "T"}},
			"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := candidateGraphID(tc.c); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBertSidecarDecoder_PostsGraphIDFromPayload locks the
// production-shaped path: when a Candidate has
// payload.node_id set (every Qdrant-backed hit has this in
// production via the publisher's payload), the wire
// envelope's `point_id` and `text` fields MUST carry the
// graph id, NOT the Qdrant qdrant_point_id. This is what
// keeps the train and rank document surfaces byte-identical
// — iter-7's bare-PointID fix had this wrong.
func TestBertSidecarDecoder_PostsGraphIDFromPayload(t *testing.T) {
	t.Parallel()
	var gotBody bertRankRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		// Response keys MUST be the graph ids the sidecar
		// received, so the Go-side merge can match scores
		// back onto the input candidates.
		_, _ = w.Write([]byte(`{"scored":[{"point_id":"graph-node-a","score":0.91},{"point_id":"graph-node-b","score":0.42}]}`))
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{HTTPClient: srv.Client()})
	r, _, _ := dec.Decode("file:///models/v9")

	in := []Candidate{
		{PointID: "qdrant-pid-1", Score: 0.5, Kind: "method", Payload: map[string]any{"node_id": "graph-node-a"}},
		{PointID: "qdrant-pid-2", Score: 0.4, Kind: "method", Payload: map[string]any{"node_id": "graph-node-b"}},
	}
	out := r.Rank(in)

	if len(gotBody.Candidates) != 2 {
		t.Fatalf("sidecar got %d candidates, want 2", len(gotBody.Candidates))
	}
	if gotBody.Candidates[0].PointID != "graph-node-a" || gotBody.Candidates[1].PointID != "graph-node-b" {
		t.Fatalf("iter-8 item 1: wire point_id MUST be the graph node_id (from payload.node_id), got %v / %v", gotBody.Candidates[0].PointID, gotBody.Candidates[1].PointID)
	}
	if gotBody.Candidates[0].Text != "graph-node-a" || gotBody.Candidates[1].Text != "graph-node-b" {
		t.Fatalf("iter-8 item 1: wire text MUST be the graph node_id (from payload.node_id), got %v / %v", gotBody.Candidates[0].Text, gotBody.Candidates[1].Text)
	}

	if len(out) != 2 {
		t.Fatalf("Rank returned %d items, want 2", len(out))
	}
	// graph-node-a should rank first (sidecar score 0.91 >
	// 0.42). The output's PointID must remain the ORIGINAL
	// qdrant_point_id (preserved through the merge) — only
	// the wire envelope swaps in the graph id; the Candidate
	// struct still carries the qdrant pid for downstream
	// consumers that need it (e.g. the §9.6a publish filter).
	if out[0].PointID != "qdrant-pid-1" || out[1].PointID != "qdrant-pid-2" {
		t.Fatalf("Rank must preserve input PointIDs: got %v / %v", out[0].PointID, out[1].PointID)
	}
	if out[0].FinalScore < 0.90 || out[0].FinalScore > 0.92 {
		t.Fatalf("FinalScore not merged via graph id: out[0].FinalScore=%v", out[0].FinalScore)
	}
}

// TestBertSidecarDecoder_FrontierCandidatesGetScored locks
// iter-8 review item 2: candidates with PointID="" (graph-
// expansion / frontier hits — appendFrontierCandidates in
// recall.go:1106-1117) MUST be sent to the sidecar via
// their payload.node_id and MUST come back with a sidecar
// score. Pre-iter-8 the sidecar's `point_id: required
// string` 400-validation would reject the whole batch and
// PublishedReranker would silently degrade to V0 for the
// entire recall.
func TestBertSidecarDecoder_FrontierCandidatesGetScored(t *testing.T) {
	t.Parallel()
	var gotBody bertRankRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scored":[{"point_id":"regular-node","score":0.30},{"point_id":"frontier-node","score":0.99}]}`))
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{HTTPClient: srv.Client()})
	r, _, _ := dec.Decode("file:///models/v9")

	in := []Candidate{
		{PointID: "qdrant-pid-1", Score: 0.5, Kind: "method", Payload: map[string]any{"node_id": "regular-node"}},
		// Frontier candidate — PointID="", but payload
		// carries the explicit graph node_id stamp.
		{PointID: "", Score: 0.2, Kind: "method", StructuralDistance: 1, Payload: map[string]any{"node_id": "frontier-node"}},
	}
	out := r.Rank(in)

	if len(gotBody.Candidates) != 2 {
		t.Fatalf("sidecar MUST receive BOTH the regular hit AND the frontier candidate, got %d", len(gotBody.Candidates))
	}
	// Both candidates should be on the wire with their graph
	// ids.
	wirePIDs := []string{gotBody.Candidates[0].PointID, gotBody.Candidates[1].PointID}
	sawRegular, sawFrontier := false, false
	for _, p := range wirePIDs {
		if p == "regular-node" {
			sawRegular = true
		}
		if p == "frontier-node" {
			sawFrontier = true
		}
	}
	if !sawRegular || !sawFrontier {
		t.Fatalf("iter-8 item 2: BOTH regular and frontier graph ids MUST hit the wire; got %v", wirePIDs)
	}

	if len(out) != 2 {
		t.Fatalf("Rank returned %d items, want 2", len(out))
	}
	// Frontier should rank first (sidecar score 0.99).
	if out[0].PointID != "" {
		t.Fatalf("iter-8 item 2: frontier candidate must rank first (sidecar score 0.99 > 0.30), but out[0].PointID=%q (expected the frontier with empty PointID)", out[0].PointID)
	}
	if out[0].FinalScore < 0.98 || out[0].FinalScore > 1.00 {
		t.Fatalf("iter-8 item 2: frontier FinalScore not merged: out[0].FinalScore=%v", out[0].FinalScore)
	}
}

// TestBertSidecarDecoder_MixedScorableAndUnscorable locks the
// partition behaviour: a single unscorable candidate (no
// PointID AND no payload graph id) MUST be dropped from the
// sidecar request but kept in the output with FinalScore=
// Score, so the recall still surfaces SOMETHING for it
// rather than the whole batch hard-failing. Production
// candidates always carry a graph id; this test exercises
// the defensive partition path.
func TestBertSidecarDecoder_MixedScorableAndUnscorable(t *testing.T) {
	t.Parallel()
	var gotBody bertRankRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scored":[{"point_id":"keep","score":0.8}]}`))
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{HTTPClient: srv.Client()})
	r, _, _ := dec.Decode("file:///models/v9")

	in := []Candidate{
		{PointID: "", Score: 0.1, Kind: "method"}, // unscorable: no PointID, no payload
		{PointID: "qdrant-pid-2", Score: 0.3, Kind: "method", Payload: map[string]any{"node_id": "keep"}},
	}
	out := r.Rank(in)

	if len(gotBody.Candidates) != 1 {
		t.Fatalf("only the scorable candidate should hit the wire, got %d", len(gotBody.Candidates))
	}
	if gotBody.Candidates[0].PointID != "keep" {
		t.Fatalf("scorable graph id MUST be on the wire; got %q", gotBody.Candidates[0].PointID)
	}
	if len(out) != 2 {
		t.Fatalf("Rank MUST preserve the unscorable candidate in the output, got %d", len(out))
	}
	// scored one ranks first (0.8 > 0.1 fallback).
	if out[0].PointID != "qdrant-pid-2" || out[1].PointID != "" {
		t.Fatalf("unexpected merge order: %v", []string{out[0].PointID, out[1].PointID})
	}
	if out[1].FinalScore != 0.1 {
		t.Fatalf("unscorable MUST keep FinalScore=Score (V0 fallback), got %v", out[1].FinalScore)
	}
}

// TestBertSidecarDecoder_ChainStopsOnRecognisedURI verifies
// the contract with MultiArtifactDecoder: a recognised URI
// short-circuits the chain. We chain a BertSidecarDecoder
// in front of a counter-decoder; for a file:// URI the
// counter must never be hit.
func TestBertSidecarDecoder_ChainStopsOnRecognisedURI(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		_, _ = w.Write([]byte(`{"scored":[{"point_id":"p1","score":0.5}]}`))
	}))
	defer srv.Close()

	calls := 0
	counter := ArtifactDecoderFunc(func(uri string) (Reranker, bool, error) {
		calls++
		return nil, false, errors.New("should not be called")
	})
	chain := NewMultiArtifactDecoder(
		NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{HTTPClient: srv.Client()}),
		counter,
	)
	r, ok, err := chain.Decode("file:///models/v9")
	if err != nil || !ok || r == nil {
		t.Fatalf("chain.Decode unexpected: ok=%v err=%v r=%v", ok, err, r)
	}
	if calls != 0 {
		t.Fatalf("recognised URI did not short-circuit the chain: counter=%d", calls)
	}
}

// TestBertSidecarDecoder_StripsTrailingRank closes iter-4
// review item 4: the prior `cmd/agent-api/main.go` hint
// told operators to set the env-var to the sidecar's
// `/rank` URL, while the decoder also appended `/rank`,
// producing `/rank/rank`. The new contract strips a
// trailing `/rank` so BOTH `http://sidecar:8088` and
// `http://sidecar:8088/rank` resolve to the same
// canonical `/rank` endpoint.
func TestBertSidecarDecoder_StripsTrailingRank(t *testing.T) {
	t.Parallel()
	var observed string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		observed = req.URL.Path
		_, _ = w.Write([]byte(`{"scored":[{"point_id":"p1","score":0.7}]}`))
	}))
	defer srv.Close()

	for _, suffix := range []string{"", "/", "/rank", "/rank/"} {
		observed = ""
		ep := srv.URL + suffix
		dec := NewBertSidecarDecoder(ep, &BertSidecarConfig{HTTPClient: srv.Client()})
		r, ok, err := dec.Decode("file:///m/v1")
		if err != nil || !ok {
			t.Fatalf("suffix=%q decode: ok=%v err=%v", suffix, ok, err)
		}
		_ = r.Rank([]Candidate{{PointID: "p1", Score: 0.1, Kind: "method"}})
		if observed != "/rank" {
			t.Fatalf("suffix=%q produced path %q, want /rank (got /rank/rank?)", suffix, observed)
		}
	}
}

// TestBertSidecarDecoder_PostsRecallQuery closes iter-4
// review item 3: the trained cross-encoder was fine-tuned
// against (recall_query, candidate_text) pairs but the
// recall-time path was POSTing without the query. The
// decoder's RankWithQueryErr surface threads the query
// through; this test asserts the wire shape carries it.
func TestBertSidecarDecoder_PostsRecallQuery(t *testing.T) {
	t.Parallel()
	var gotBody bertRankRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		_ = json.Unmarshal(raw, &gotBody)
		_, _ = w.Write([]byte(`{"scored":[{"point_id":"p1","score":0.7}]}`))
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{HTTPClient: srv.Client()})
	r, _, _ := dec.Decode("file:///m/v3")

	qaf, ok := r.(QueryAwareFallibleReranker)
	if !ok {
		t.Fatalf("bertSidecarReranker does not implement QueryAwareFallibleReranker — iter-4 item 3 not addressed")
	}

	in := []Candidate{{PointID: "p1", Score: 0.1, Kind: "method", Payload: map[string]any{"text": "alpha"}}}
	out, err := qaf.RankWithQueryErr(t.Context(), "how do auth flows handle 401?", in)
	if err != nil {
		t.Fatalf("RankWithQueryErr err: %v", err)
	}
	if len(out) != 1 || out[0].PointID != "p1" {
		t.Fatalf("RankWithQueryErr out wrong: %+v", out)
	}
	if gotBody.Query != "how do auth flows handle 401?" {
		t.Fatalf("sidecar body did not carry query, got %q", gotBody.Query)
	}
}

// TestBertSidecarDecoder_RankErrSurfaceReturnsError closes
// iter-4 review item 6 on the scorer-side: the lossy
// Rank() must KEEP its graceful-degradation contract (so
// the recall path never hard-fails on a transient sidecar
// blip), but the RankErr / RankWithQueryErr surfaces
// MUST return the genuine error so the PublishedReranker
// wrapper can pin reranker_model_version to the inner
// scorer when the trained scorer didn't actually
// contribute to the ordering.
func TestBertSidecarDecoder_RankErrSurfaceReturnsError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	dec := NewBertSidecarDecoder(srv.URL, &BertSidecarConfig{HTTPClient: srv.Client()})
	r, _, _ := dec.Decode("file:///m/v4")

	// Lossy Rank() must NOT propagate the error — that
	// behaviour is contract.
	in := []Candidate{{PointID: "p1", Score: 0.5, Kind: "method"}}
	lossy := r.Rank(in)
	if len(lossy) != 1 {
		t.Fatalf("Rank() should fall back to input ordering, got %d items", len(lossy))
	}

	// But the fallible surface MUST surface the error so
	// the wrapper can pin correctly.
	fallible, ok := r.(FallibleReranker)
	if !ok {
		t.Fatalf("bertSidecarReranker does not implement FallibleReranker — iter-4 item 6 not addressed")
	}
	if _, err := fallible.RankErr(in); err == nil {
		t.Fatalf("FallibleReranker.RankErr returned nil err on sidecar 500 — iter-4 item 6 not addressed")
	}

	qaf, ok := r.(QueryAwareFallibleReranker)
	if !ok {
		t.Fatalf("bertSidecarReranker does not implement QueryAwareFallibleReranker")
	}
	if _, err := qaf.RankWithQueryErr(t.Context(), "q", in); err == nil {
		t.Fatalf("RankWithQueryErr returned nil err on sidecar 500")
	}
}
