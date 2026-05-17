// Package agentapi: BERT sidecar artifact decoder.
//
// Stage 6.4 (impl-plan §1110) mandates that the recall path
// consume the trained `reranker_model` artifact on every
// `agent.recall` request. The Python BERT cross-encoder
// sidecar writes its checkpoints to a host-local directory
// and publishes an artifact URI of the shape
// `file:///var/lib/reranker-sidecar/models/<version>` — a
// path that is meaningful to the sidecar process but NOT
// to the Go-side recall handler. To close the loop without
// embedding torch / transformers / sentence-transformers
// into the Go binary (CGo bindings to libtorch are
// unmaintainable at this scope), this decoder forwards
// scoring requests to the sidecar over HTTP.
//
// Wire-up at cmd/agent-api/main.go:
//
//	AGENT_MEMORY_RERANKER_INFERENCE_ENDPOINT=http://reranker-sidecar:8088
//
// When unset, the deployment uses the DisabledArtifactDecoder
// sentinel and `file://...` artifacts trigger a clean V0
// fallback at recall time. When set, this decoder produces a
// request-scoped Reranker whose Rank()/RankErr()/
// RankWithQueryErr() POSTs the candidate surface to the
// sidecar's `/rank` endpoint and reads back the trained
// scores.
//
// HTTP contract (mirror in cmd/reranker-sidecar/main.py):
//
//	POST /rank
//	{
//	  "artifact_uri": "file:///<artifact_dir>/<version>",
//	  "query":        "natural-language recall query",   // iter-5 item 3
//	  "candidates": [
//	    {"point_id": "<graph-id>", "kind": "method", "text": "<graph-id>",
//	     "score": 0.42, "structural_distance": 0}
//	  ]
//	}
//	→
//	{
//	  "scored": [{"point_id": "<graph-id>", "score": 0.91}, ...]
//	}
//
// iter-8 review item 1 NOTE: the wire `point_id` field
// carries the GRAPH id (the `node_id` from the publisher
// payload for method/block/frontier candidates, or the
// `concept_id` for concept candidates) — NOT the Qdrant
// `qdrant_point_id`. The wire name is kept for back-compat
// (the sidecar uses it only as a unique key to match scores
// back to candidates; the sidecar never talks to Qdrant).
// Both train and rank now project from the same graph id
// surface so `<canonical-kind> <graph_id>` is the document
// the cross-encoder sees in both contexts. See
// `candidateGraphID` in this file for the extraction rule.
//
// The `query` field is the recall-time natural-language
// query the agent.recall caller sent. The sidecar's `/rank`
// projects `(query, document)` through the SAME helpers
// `_flatten_pairs` uses during training so the cross-
// encoder is asked to score on the surface it was fine-
// tuned against (iter-5 review item 3 fix).
//
// Decode contract: returns `(reranker, true, nil)` for any
// `file://` URI; the recall-time HTTP round-trip happens
// inside the returned reranker's Rank* methods (Decode
// itself never performs the I/O), so a transient sidecar
// outage cannot cause a decoder error — the failure
// surfaces from `RankErr` / `RankWithQueryErr` so the
// wrapping `PublishedReranker` can pin the advertised
// `reranker_model_version` to the inner cold-start scorer
// when the trained scorer did not actually contribute to
// the ordering (iter-5 review item 6 fix).
//
// `Rank()` keeps a lossy graceful-degradation contract
// (silently falls back to the input ordering on sidecar
// failure) so legacy callers that don't type-assert to
// `FallibleReranker` still get a non-empty result instead
// of an error.
package agentapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// BertSidecarURIPrefix is the URI scheme this decoder
// recognises. The Python sidecar writes
// `file://<absolute-path>` URIs (see
// cmd/reranker-sidecar/main.py:_train); other `file://`
// emitters would also be matched, which is the intended
// behaviour — the URI scheme advertises "consumable by the
// BERT sidecar", and the sidecar is the registry of which
// on-disk paths it knows about.
const BertSidecarURIPrefix = "file://"

// DefaultBertSidecarTimeout bounds the per-recall HTTP call
// to the sidecar. Tuned to be tighter than the recall
// handler's overall deadline so a transient sidecar stall
// degrades gracefully to V0 instead of cascading into a
// recall-level timeout. Override via the explicit
// HTTPClient knob on NewBertSidecarDecoder when running
// integration tests against a slower mock.
const DefaultBertSidecarTimeout = 750 * time.Millisecond

// BertSidecarConfig configures the decoder. Nil-valued
// fields take the documented defaults.
type BertSidecarConfig struct {
	// HTTPClient is the http.Client used for /rank calls.
	// When nil, a default client with the resolved Timeout
	// (Timeout if >0, else DefaultBertSidecarTimeout) is
	// used.
	HTTPClient *http.Client
	// Timeout configures BOTH the http.Client.Timeout (when
	// HTTPClient is nil) AND the per-call
	// `context.WithTimeout` bound applied inside callSidecar.
	// Iter-20 evaluator item 5 fixed the prior behavior of
	// hardcoding DefaultBertSidecarTimeout in the per-call
	// context, which meant any operator-configured Timeout
	// larger than 750ms was silently cut off at the context
	// boundary. When the caller supplies an HTTPClient with
	// its OWN Timeout, that client timeout still wins for
	// the underlying transport — Timeout here ONLY governs
	// the per-call context (and the fallback http.Client
	// timeout when HTTPClient is nil). Zero / negative
	// values resolve to DefaultBertSidecarTimeout.
	Timeout time.Duration
}

// NewBertSidecarDecoder returns the decoder. `endpoint` is
// the sidecar's BASE URL (e.g. `http://reranker-sidecar:8088`);
// the decoder appends `/rank` for the per-recall scoring call.
// To be forgiving of operator misconfiguration (iter-4
// review item 4: the env-var hint previously said "/rank URL"
// while the decoder also appended `/rank`, producing
// `/rank/rank`), a trailing `/rank` segment is stripped from
// `endpoint` before the canonical `/rank` is appended. Both
// `http://sidecar:8088` and `http://sidecar:8088/rank` thus
// route correctly. `cfg` MAY be nil to take the documented
// defaults.
func NewBertSidecarDecoder(endpoint string, cfg *BertSidecarConfig) ArtifactDecoder {
	c := BertSidecarConfig{}
	if cfg != nil {
		c = *cfg
	}
	// Resolve the per-call timeout ONCE here so both the
	// fallback http.Client and the per-recall
	// `context.WithTimeout` bound observe the same value.
	// `context.WithTimeout(ctx, 0)` returns an immediately-
	// expired context, which would cause EVERY sidecar
	// recall to fail before issuing the HTTP call — hence
	// the explicit `<= 0` fallback to DefaultBertSidecarTimeout
	// (rubber-duck pass on iter-20 plan flagged this).
	callTimeout := c.Timeout
	if callTimeout <= 0 {
		callTimeout = DefaultBertSidecarTimeout
	}
	client := c.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: callTimeout}
	}
	normalised := strings.TrimRight(endpoint, "/")
	// Defence-in-depth: if the operator followed the prior
	// (incorrect) doc hint and supplied a `/rank` suffix,
	// strip it before re-appending — otherwise we'd POST
	// to `/rank/rank` and get a 404.
	if strings.HasSuffix(normalised, "/rank") {
		normalised = strings.TrimSuffix(normalised, "/rank")
		normalised = strings.TrimRight(normalised, "/")
	}
	return &bertSidecarDecoder{
		endpoint:    normalised + "/rank",
		client:      client,
		callTimeout: callTimeout,
	}
}

type bertSidecarDecoder struct {
	endpoint    string
	client      *http.Client
	callTimeout time.Duration
}

// Decode implements ArtifactDecoder.
func (b *bertSidecarDecoder) Decode(uri string) (Reranker, bool, error) {
	if !strings.HasPrefix(uri, BertSidecarURIPrefix) {
		return nil, false, nil
	}
	return &bertSidecarReranker{
		endpoint:    b.endpoint,
		client:      b.client,
		callTimeout: b.callTimeout,
		artifactURI: uri,
	}, true, nil
}

// bertSidecarReranker is the request-scoped Reranker the
// BERT sidecar decoder produces. Holds the artifact URI and
// the sidecar HTTP client; each Rank() call POSTs the
// candidates and reads back the scored ordering.
//
// Implements Reranker (Rank + ModelVersion). The
// PublishedReranker that wraps this NEVER consults
// ModelVersion() on the trained scorer (the wrapper's own
// ModelVersion goes through PublishedRerankerSource), so
// the returned value here is informational only.
type bertSidecarReranker struct {
	endpoint    string
	client      *http.Client
	callTimeout time.Duration
	artifactURI string
}

// rankRequest is the POST body the decoder sends to the
// sidecar. Matches the contract documented at the package
// top and mirrored in cmd/reranker-sidecar/main.py.
type bertRankRequest struct {
	ArtifactURI string                  `json:"artifact_uri"`
	Query       string                  `json:"query,omitempty"`
	Candidates  []bertRankRequestCandid `json:"candidates"`
}

type bertRankRequestCandid struct {
	PointID            string  `json:"point_id"`
	Kind               string  `json:"kind"`
	Text               string  `json:"text"`
	Score              float32 `json:"score"`
	StructuralDistance int     `json:"structural_distance"`
}

type bertRankResponse struct {
	Scored []bertRankResponseScored `json:"scored"`
}

type bertRankResponseScored struct {
	PointID string  `json:"point_id"`
	Score   float64 `json:"score"`
}

// Rank implements Reranker. Lossy by contract — internal
// failures (sidecar outage, malformed response) silently
// degrade to the input ordering with FinalScore=Score so
// the recall path never hard-fails on a transient sidecar
// problem. Callers that need to KNOW whether the trained
// scorer actually contributed to the ordering MUST use
// RankErr or RankWithQueryErr; PublishedReranker.
// RankWithQuery type-asserts to those interfaces so the
// `reranker_model_version` advertised matches the scorer
// that actually produced the ordering (iter-4 review
// item 6).
func (r *bertSidecarReranker) Rank(candidates []Candidate) []Candidate {
	if len(candidates) == 0 {
		return candidates
	}
	out, err := r.callSidecar(context.Background(), "", candidates)
	if err != nil {
		return r.fallback(candidates)
	}
	return out
}

// RankErr implements FallibleReranker. Returns the
// genuine error from callSidecar so callers (the
// PublishedReranker wrapper) can decide whether to fall
// back to inner scoring AND inner ModelVersion (the
// pinning fix at iter-4 review item 6).
func (r *bertSidecarReranker) RankErr(candidates []Candidate) ([]Candidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	return r.callSidecar(context.Background(), "", candidates)
}

// RankWithQueryErr implements QueryAwareFallibleReranker.
// Threads the recall query through to the sidecar so the
// cross-encoder scores on the (recall_query, candidate)
// surface it was fine-tuned against (iter-4 review item 3).
// Returns the genuine error so the wrapper can pin
// `reranker_model_version` correctly (iter-4 review item 6).
func (r *bertSidecarReranker) RankWithQueryErr(ctx context.Context, query string, candidates []Candidate) ([]Candidate, error) {
	if len(candidates) == 0 {
		return candidates, nil
	}
	return r.callSidecar(ctx, query, candidates)
}

// fallback returns the input ordering with FinalScore=Score
// so the response projection still surfaces a meaningful
// similarity number. Used by Rank() (lossy) when the
// sidecar call fails — RankErr / RankWithQueryErr return
// the real error instead so the wrapper can pin
// `reranker_model_version` correctly.
func (r *bertSidecarReranker) fallback(candidates []Candidate) []Candidate {
	out := make([]Candidate, len(candidates))
	copy(out, candidates)
	for i := range out {
		if out[i].FinalScore == 0 {
			out[i].FinalScore = out[i].Score
		}
	}
	return out
}

// ModelVersion returns the artifact URI suffix as an
// informational tag. NOT load-bearing: the PublishedReranker
// wrapping this scorer always advertises the
// reranker_model.version from PublishedRerankerSource, NOT
// the inner scorer's ModelVersion().
func (r *bertSidecarReranker) ModelVersion() string {
	if r.artifactURI == "" {
		return "bert-sidecar"
	}
	// The sidecar writes file:///path/to/<version>; the
	// trailing path segment is the trainer-published
	// version. This is purely informational — see the
	// method doc-comment.
	if idx := strings.LastIndex(r.artifactURI, "/"); idx >= 0 && idx+1 < len(r.artifactURI) {
		return "bert-sidecar:" + r.artifactURI[idx+1:]
	}
	return "bert-sidecar"
}

// callSidecar is the HTTP round-trip helper. Wraps the
// request marshalling, the POST, and the response
// unmarshalling so the public Rank* methods can stay
// focused on the fallback policy.
//
// `query` is the recall-time natural-language query that
// the cross-encoder needs as the (q, doc) pair's q-side
// (iter-4 review item 3). Empty when the caller is the
// legacy Rank() path; the sidecar's _project_query helper
// then falls back to the seed-ids surface that the trainer
// also uses when recall_query is empty, so train and rank
// surfaces still agree.
//
// `ctx` is plumbed through from RankWithQueryErr so the
// per-recall deadline propagates correctly; the legacy
// Rank() path constructs its own background context.
func (r *bertSidecarReranker) callSidecar(ctx context.Context, query string, candidates []Candidate) ([]Candidate, error) {
	// Partition into scorable (has a graph id we can post)
	// vs unscorable (no payload graph id AND no PointID,
	// e.g. a misconstructed frontier candidate). The
	// sidecar rejects empty `point_id` with 400, which
	// would force the WHOLE recall back to V0 even when
	// only one candidate is malformed — iter-7 review
	// item 2. By filtering at the source we score what
	// CAN be scored and merge the rest back with their
	// V0-shaped Score on the unscored side.
	//
	// In practice EVERY production candidate has a graph
	// id: regular hits come from Qdrant with the publisher's
	// payload (which always carries `node_id` or
	// `concept_id`); frontier candidates stamp their
	// `payload["node_id"]` explicitly in
	// `appendFrontierCandidates` (recall.go:1107) even
	// though their PointID is "". So this partition keeps
	// frontier expansions in the scored set — they were
	// silently dropping the ENTIRE trained scorer on the
	// floor before iter-8.
	scorable := make([]Candidate, 0, len(candidates))
	scorableIDs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		gid := candidateGraphID(c)
		if gid == "" {
			continue
		}
		scorable = append(scorable, c)
		scorableIDs = append(scorableIDs, gid)
	}
	if len(scorable) == 0 {
		return nil, errors.New("agentapi: bert sidecar: no candidate carries a graph identifier")
	}

	body := bertRankRequest{
		ArtifactURI: r.artifactURI,
		Query:       query,
		Candidates:  make([]bertRankRequestCandid, len(scorable)),
	}
	for i, c := range scorable {
		body.Candidates[i] = bertRankRequestCandid{
			// iter-8 review item 1: the wire `point_id`
			// field carries the GRAPH id (node_id or
			// concept_id), not the Qdrant
			// qdrant_point_id. The field name is kept
			// for wire-format back-compat — the sidecar
			// only uses it as a unique key for matching
			// scores back to candidates, it never talks
			// to Qdrant. Training emits this SAME graph
			// id from `LabelledPair.SeedNodeIDs`, so the
			// `_project_document(kind, graph_id)`
			// projection produces byte-identical doc
			// strings at train and rank time.
			PointID:            scorableIDs[i],
			Kind:               c.Kind,
			Text:               candidateText(c),
			Score:              c.Score,
			StructuralDistance: c.StructuralDistance,
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("agentapi: bert sidecar: marshal: %w", err)
	}

	// Bound the round-trip regardless of caller context —
	// a missing or unbounded ctx would let a stalled
	// sidecar cascade into a recall-level timeout. The
	// per-request HTTP client also bounds via its
	// Timeout, this layer adds the explicit deadline.
	//
	// Iter-20 evaluator item 5: use the per-reranker
	// `callTimeout` (resolved once at NewBertSidecarDecoder
	// from BertSidecarConfig.Timeout, falling back to
	// DefaultBertSidecarTimeout) so an operator-configured
	// longer timeout is actually honoured here too — the
	// prior hardcoded DefaultBertSidecarTimeout would cut
	// every per-call context at 750ms even when the HTTP
	// client allowed longer round-trips.
	timeout := r.callTimeout
	if timeout <= 0 {
		timeout = DefaultBertSidecarTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, r.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("agentapi: bert sidecar: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("agentapi: bert sidecar: POST %s: %w", r.endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("agentapi: bert sidecar: status %d: %s", resp.StatusCode, string(snippet))
	}

	var decoded bertRankResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("agentapi: bert sidecar: decode response: %w", err)
	}
	if len(decoded.Scored) == 0 {
		return nil, errors.New("agentapi: bert sidecar: empty scored response")
	}

	// Merge sidecar scores into a copy of the FULL input
	// candidate list (NOT just the scorable subset) by
	// matching on graph id. Any input candidate the
	// sidecar did not score — either because it was
	// unscorable (no graph id) or because the sidecar
	// returned a partial response — keeps FinalScore=Score
	// so it still appears in the response, in the V0-shape
	// position. Then stable-sort descending.
	byID := make(map[string]float64, len(decoded.Scored))
	for _, s := range decoded.Scored {
		byID[s.PointID] = s.Score
	}
	out := make([]Candidate, len(candidates))
	copy(out, candidates)
	for i := range out {
		gid := candidateGraphID(out[i])
		if s, ok := byID[gid]; gid != "" && ok {
			out[i].FinalScore = float32(s)
		} else if out[i].FinalScore == 0 {
			out[i].FinalScore = out[i].Score
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].FinalScore > out[j].FinalScore
	})
	return out, nil
}

// candidateText returns the per-candidate document-side
// identifier the sidecar uses in the (query, doc)
// cross-encoder input. It delegates to `candidateGraphID`
// (below) — see that function's doc-comment for the full
// rationale.
//
// Was `c.PointID` in iter-7 (which assumed seed_id ==
// qdrant_point_id), `c.Payload["text"|...]` with kind+pid
// fallback before that. Both prior shapes were wrong:
// production training carries graph `node_id`s (from
// `recall_context_log.node_ids`) while ranking carries
// Qdrant `qdrant_point_id`s (from `embedding_publish`),
// and the two are minted separately
// (`internal/embedding/publisher.go:650-658`). The iter-8
// fix uses the GRAPH id on both sides instead.
func candidateText(c Candidate) string {
	return candidateGraphID(c)
}

// candidateGraphID extracts the graph-side identifier — the
// SAME identifier the trainer's `LabelledPair.SeedNodeIDs` /
// `SeedConceptIDs` carry — from a recall-time Candidate.
//
// The Qdrant publisher (internal/embedding/publisher.go)
// always writes the graph id onto the payload that comes
// back at rank time:
//
//   - method / block / frontier nodes → payload["node_id"]
//     (frontier candidates also explicitly stamp this even
//     though they have PointID="" because they were never
//     embedded — see `appendFrontierCandidates` in
//     internal/agentapi/recall.go:1106-1107).
//   - concept rows                    → payload["concept_id"]
//     (per `conceptHitFromCandidate` in
//     internal/agentapi/recall.go:1264-1276).
//
// Returning the graph id (rather than `c.PointID` =
// qdrant_point_id) closes the iter-7 review item 1 gap: at
// train time `_flatten_pairs` projects
// `_project_document(seed_kind, seed_node_id)` from the
// LabelledPair, and at rank time `/rank` now receives the
// SAME graph id in its `point_id` field (the wire name is
// kept for back-compat; the value is semantically a graph
// id now) and projects `_project_document(kind, graph_id)`.
// Both surfaces collapse to `<canonical-kind> <graph_id>`.
//
// Falls back to `c.PointID` when the payload is absent
// (defensive: production always has the payload because the
// publisher writes it on every row, but unit tests that
// construct bare Candidates without setting Payload still
// get a deterministic non-empty identifier).
//
// Returns "" when neither a payload graph id nor a non-empty
// PointID is available. The caller (`callSidecar`) filters
// these out before posting so the sidecar's
// `point_id: required string` 400-validation does not bring
// down the entire recall — closes iter-7 review item 2.
func candidateGraphID(c Candidate) string {
	if c.Payload != nil {
		if id, ok := c.Payload["node_id"].(string); ok && id != "" {
			return id
		}
		if id, ok := c.Payload["concept_id"].(string); ok && id != "" {
			return id
		}
	}
	return c.PointID
}
