// Package agentapi serves the agent-facing read API for the
// agent-memory service.  This file implements the §6.4 recall
// path, which is the canonical read counterpart to the
// embedding publisher (`internal/embedding/publisher.go`):
//
//   1. The publisher writes every Method / Block embedding to
//      Qdrant AND records a §9.6a state log in PostgreSQL.
//   2. The recall service (this file) accepts a natural-
//      language query, embeds it, searches Qdrant, and
//      filters the candidate hits through
//      `embedding.RecallFilter` so any vector that has NOT
//      reached the §9.6a `published` terminal state is
//      excluded from the answer.
//
// The filter step is the §9.6a read-side invariant that
// tech-spec.md §8.7.1 names: "An EmbeddingPublish row that
// has not reached `published` MUST NOT be returnable from
// agent.recall."  Stage 3.3's third acceptance scenario
// ("vector excluded until published") asserts exactly this
// behaviour end-to-end via `recall_integration_test.go`.
//
// Scope limitations (intentional)
// -------------------------------
//   - This file implements the RECALL primitive only.  The
//     full Stage 4 `agent-api` HTTP/MCP server is a separate
//     workstream.  `cmd/agent-api/main.go` wires this
//     service into a long-running process today so the
//     production read path is exercised, but the public
//     transport (HTTP routes, MCP tool definitions, auth)
//     belongs to Stage 4.
//   - Reranking, hybrid (vector+BM25), and observe/expand
//     primitives are intentionally NOT implemented here —
//     they are out of scope for Stage 3.3 and have their
//     own workstreams (observe.go / expand.go /
//     summarize.go target files).
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/embedding"
)

// QueryEmbedder is the narrow read-side view of
// `embedding.Embedder`.  The recall service ONLY needs the
// `Embed` half — it has no reason to record an embedding
// model version on a database row.  Defining a separate
// interface keeps the wiring direction clean (the recall
// path does not pull in the publisher's write surface) and
// makes mocking trivial in tests.
type QueryEmbedder interface {
	Embed(ctx context.Context, content string) ([]float32, error)
}

// VectorSearcher is the narrow Qdrant interface the recall
// service depends on.  Both `*embedding.HTTPQdrant` and the
// in-memory test fake satisfy it.  Kept separate from
// `embedding.Qdrant` (which carries write-only methods like
// `Upsert`) to enforce that the recall path cannot
// accidentally mutate the vector store.
type VectorSearcher interface {
	// Search runs a k-NN scan against `collection` per
	// `req`.  Returns candidates in descending score order;
	// an empty result is `([]embedding.SearchHit{}, nil)`.
	// MUST NOT apply any §9.6a publish-state gating — that
	// is `embedding.RecallFilter`'s job, intentionally
	// split off so a single Qdrant client can serve both
	// the write-confirm path and the read path without
	// duplicated state.
	Search(ctx context.Context, collection string, req embedding.SearchRequest) ([]embedding.SearchHit, error)
}

// PublishFilter is the narrow read-side view of
// `*embedding.RecallFilter`.  Defining an interface lets
// the integration test substitute a deterministic fake
// without standing up a full Postgres fixture for every
// test, and keeps the dependency graph one-way (the recall
// service depends on the filter abstraction; the filter
// does not know about the recall service).
//
// Contract mirrors `*RecallFilter.FilterPublishedPoints`:
// returns the SUBSET of input ids whose §9.6a state is
// `published`; preserves input order; an empty input is
// answered with `(nil, nil)`.
type PublishFilter interface {
	FilterPublishedPoints(ctx context.Context, pointIDs []string) ([]string, error)
}

// HealthSource is the read-side abstraction the recall
// handler consults to surface per-repo degraded state on the
// response envelope. The Span Ingestor (Stage 4.2) populates
// the underlying `repo_health` rows via
// `graphwriter.UpsertRepoHealth`; the production
// implementation is `spaningestor.PGHealthSource`.
//
// We define a structurally-identical local interface here
// (instead of importing `internal/spaningestor`) so this
// package keeps a one-directional dependency arrow — the
// agent-api consumer wires the concrete spaningestor type at
// the binary level via the `HealthSourceFunc` adapter. Test
// fakes can implement HealthSource without dragging the
// spaningestor package in.
//
// Returning `(zero-value, nil)` on a healthy repo is the
// contract; the recall handler treats any error from
// HealthForRepo as a soft failure (logged, ignored) — a
// degraded read of the health table itself should NOT also
// fail the recall response (rubber-duck #1 on the cross-
// process design).
type HealthSource interface {
	HealthForRepo(ctx context.Context, repoID string) (HealthState, error)
}

// HealthSourceFunc adapts a plain function into a HealthSource.
// Used by the binary's composition root to bridge the
// `spaningestor.HealthSource` type to this package's
// structurally-identical interface without an import cycle.
type HealthSourceFunc func(ctx context.Context, repoID string) (HealthState, error)

// HealthForRepo implements HealthSource.
func (f HealthSourceFunc) HealthForRepo(ctx context.Context, repoID string) (HealthState, error) {
	return f(ctx, repoID)
}

// HealthState mirrors `spaningestor.HealthState` (see the
// HealthSource doc above for why). The `Reason` value is the
// closed-set `degraded_reason` ENUM literal from migration
// 0001 (e.g. "span_ingestor_backpressure"), passed through
// verbatim onto `RecallResponse.DegradedReason`.
type HealthState struct {
	Degraded bool
	Reason   string
	Source   string
}

// Service is the §6.4 recall implementation.  Construct via
// `NewService`; `Recall` is the only exported method today.
// Future Stage-4 work will add `Observe` / `Expand` /
// `Summarize` to this struct.
//
// Stage 5.1 dependencies
// ----------------------
// The three required dependencies (`embedder`, `searcher`,
// `filter`) implement Steps 1–3 of §6.4 — embed query,
// k-NN against Qdrant, filter unpublished hits. The four
// optional dependencies wired via `With*` options light up
// Steps 4–6 + the degraded-snapshot fallback:
//
//   - `seedExpander` -> Step 4 (1–2 hop expansion).
//   - `reranker`     -> Step 5 (final ordering).
//   - `contextLog`   -> Step 6 (RecallContextLog append).
//   - `snapshot`     -> Step 9 (degraded fallback).
//   - `conceptsEnabled` plumbs the §7.8 mixed seed (Method +
//     Block + Concept fan-out).
//
// All optionals default to "off" so existing test fixtures
// (which assemble the three core deps only) continue to see
// the legacy two-step behaviour; production wiring in
// `cmd/agent-api/main.go` sets every option.
type Service struct {
	embedder QueryEmbedder
	searcher VectorSearcher
	filter   PublishFilter
	health   HealthSource
	logger   *slog.Logger

	// overFetchMultiplier is the headroom the service
	// requests from Qdrant before §9.6a filtering.  See
	// `RecallRequest.K` for the contract.
	overFetchMultiplier int

	// Stage 5.1 optional dependencies — see the Service doc
	// comment for the per-field rationale.
	seedExpander    SeedExpander
	reranker        Reranker
	contextLog      ContextLogAppender
	snapshot        SnapshotSource
	conceptsEnabled bool
	expansionDepth  int
}

// Option configures a `Service`.
type Option func(*Service)

// WithLogger plumbs a structured logger.  Defaults to
// `slog.Default()`.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithOverFetchMultiplier overrides the over-fetch headroom
// the service requests from Qdrant before filtering.  Defaults
// to 3 (i.e. for a caller `K=10`, the service requests 30
// candidates from Qdrant so that, after `RecallFilter`
// removes any unpublished hits, the response can still cap
// at 10).  Values < 1 are coerced to 1.
//
// Operational note: a very large multiplier increases
// Qdrant CPU per call; the §9.6a window between insert and
// `published` is typically sub-second in production, so
// `3x` is the recommended default.  Bump only if the
// `recall_filter_unpublished_total` counter shows a
// sustained high filter rate.
func WithOverFetchMultiplier(n int) Option {
	return func(s *Service) {
		if n < 1 {
			n = 1
		}
		s.overFetchMultiplier = n
	}
}

// WithHealthSource plumbs an optional cross-process degraded-
// state source the recall handler consults to populate
// `RecallResponse.Degraded` / `RecallResponse.DegradedReason`
// per tech-spec §C22. A nil source is a no-op (the response
// reports Degraded=false always); when a real source returns
// an error the recall call still succeeds — the error is
// logged but the degraded flags fall back to defaults.
//
// The Span Ingestor (Stage 4.2) writes `repo_health` rows that
// `spaningestor.NewPGHealthSource` reads here; the binary's
// composition root in `cmd/agent-api/main.go` wires the
// connection.
func WithHealthSource(h HealthSource) Option {
	return func(s *Service) {
		s.health = h
	}
}

// WithReranker plumbs the v0 cold-start (or trained) reranker
// the recall handler invokes at Step 5 of §6.4. Without it the
// handler returns hits in raw Qdrant cosine order and reports
// the empty string for `reranker_model_version`. The
// production binary wires `NewV0ColdStartReranker(nil)` until
// the Stage 6.4 trainer publishes its first row.
func WithReranker(r Reranker) Option {
	return func(s *Service) {
		s.reranker = r
	}
}

// WithSeedExpander plumbs the Step-4 graph-expansion source.
// The recall handler walks up to `WithExpansionDepth` hops
// outward from each candidate seed and folds the discovered
// edges into the response envelope.
//
// Without it Step 4 short-circuits to the seed set only (no
// edges in the response). The expander is OPTIONAL because
// the §9.5 cold-start fallback path can serve useful answers
// from raw vector similarity alone — the structural fan-out
// is a quality boost, not a correctness gate.
func WithSeedExpander(e SeedExpander) Option {
	return func(s *Service) {
		s.seedExpander = e
	}
}

// WithExpansionDepth overrides the per-call hop count Step 4
// walks. Defaults to 1; clamped to [1, maxExpansionDepth=2]
// to keep the per-request fan-out linear in seed count.
func WithExpansionDepth(d int) Option {
	return func(s *Service) {
		if d < 1 {
			d = 1
		}
		if d > maxExpansionDepth {
			d = maxExpansionDepth
		}
		s.expansionDepth = d
	}
}

// WithContextLog plumbs the Step-6 RecallContextLog writer.
// On every successful recall (happy path AND degraded
// fallback) the handler appends exactly one row so the
// caller can replay the snapshot via `agent.observe`. A nil
// writer disables Step 6 and the response carries an empty
// `ContextID`.
//
// The handler treats any error returned by the appender as
// a soft failure: the recall response still surfaces, just
// without a context_id. A hard-failure model would mean a
// transient PostgreSQL outage takes down the read path,
// which is the failure mode the §9.5 degraded contract
// explicitly avoids.
func WithContextLog(a ContextLogAppender) Option {
	return func(s *Service) {
		s.contextLog = a
	}
}

// WithSnapshotFallback plumbs the Step-9 degraded snapshot
// source. When Qdrant or GraphReader is unreachable the
// handler degrades to the most recent valid snapshot for
// the requesting repo, stamps `Degraded=true` with the
// appropriate `degraded_reason`, and appends a new
// RecallContextLog row with `ServedUnderDegraded=true`.
//
// A nil source disables the fallback (the handler returns
// the underlying dependency error verbatim, matching the
// legacy behaviour). Production wiring should always supply
// one.
func WithSnapshotFallback(s2 SnapshotSource) Option {
	return func(s *Service) {
		s.snapshot = s2
	}
}

// WithConceptsEnabled toggles the §7.8 mixed-seed fan-out
// across the `agent_memory_concept` collection. When set
// the handler issues an additional Qdrant Search call
// against the concept collection and surfaces matching
// Concept hits in `RecallResponse.Concepts`.
//
// Default is OFF so existing fixtures (which assert that
// `Search` is called exactly once against the Method
// collection) continue to pass. Production wiring sets this
// to true.
func WithConceptsEnabled(enabled bool) Option {
	return func(s *Service) {
		s.conceptsEnabled = enabled
	}
}

// NewService constructs a recall service.  All three
// dependencies (`embedder`, `searcher`, `filter`) are
// REQUIRED; a nil dependency panics at construction rather
// than at the first request, surfacing wiring bugs at
// process start.
func NewService(embedder QueryEmbedder, searcher VectorSearcher, filter PublishFilter, opts ...Option) *Service {
	if embedder == nil {
		panic("agentapi: NewService: nil QueryEmbedder")
	}
	if searcher == nil {
		panic("agentapi: NewService: nil VectorSearcher")
	}
	if filter == nil {
		panic("agentapi: NewService: nil PublishFilter")
	}
	s := &Service{
		embedder:            embedder,
		searcher:            searcher,
		filter:              filter,
		logger:              slog.Default(),
		overFetchMultiplier: 3,
		expansionDepth:      defaultExpansionDepth,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// RecallRequest is the input to `Service.Recall`.  Carries
// the natural-language query, the scope filters (repo /
// kind), and the result cap.
type RecallRequest struct {
	// Query is the natural-language string the caller
	// wants to find similar nodes for.  Empty queries are
	// rejected — `Service.Recall` returns a non-sentinel
	// error so callers do not waste an embedder call on a
	// guaranteed-zero result.
	Query string

	// Kinds restricts the mixed seed fan-out to the supplied
	// kinds. Each entry MUST be one of "method", "block",
	// "concept" (matches the proto `agent.v1.RecallRequest.kinds`
	// closed set). Duplicates are deduped.
	//
	// When both Kinds and Kind are empty the recall handler
	// defaults to the §7.8 mixed-seed set:
	//   - {method, block}            when conceptsEnabled = false
	//   - {method, block, concept}   when conceptsEnabled = true
	// matching the Stage 5.1 critique #2 "mixed search
	// across agent_memory_method, agent_memory_block, and
	// agent_memory_concept".
	//
	// When Kind is set and Kinds is empty, Kinds resolves to
	// {Kind} (legacy single-collection lookup); concept
	// fan-out is appended on top when conceptsEnabled = true
	// so the existing `TestRecall_mixedSeedIncludesConcepts`
	// contract (single Kind + concepts enabled = method
	// + concept fan-out) keeps holding.
	//
	// When both Kinds and Kind are set, Kind MUST appear in
	// Kinds — a mismatch returns ErrInvalidKind so a caller
	// that accidentally narrows past the proto contract sees
	// the error rather than silently dropping the legacy
	// constraint.
	Kinds []string

	// Kind is the §6.2 node kind to search.  Either
	// `embedding.NodeKindMethod` ("method") or
	// `embedding.NodeKindBlock` ("block").
	//
	// Deprecated: prefer Kinds. Retained as a single-kind
	// alias so existing fixtures that pre-date the multi-
	// kind contract keep compiling. The deprecated field
	// rejects "concept" so the legacy single-kind validator
	// stays strict; pass `Kinds = []string{"concept"}` if
	// you want the concept-only recall path.
	Kind string

	// RepoID, when non-empty, restricts the search to
	// vectors whose payload `repo_id` matches.  This is
	// the recommended scoping mode for cross-tenant
	// safety — the publisher writes `req.RepoID` on
	// every point payload (see `publisher.go.buildPayload`).
	// Leaving this blank produces a global-top-k query;
	// the service logs a warning so the operator can
	// audit any unscoped lookups.
	RepoID string

	// K is the maximum number of hits to return.  Must be
	// > 0; values > 256 are coerced to 256 (a single agent
	// turn cannot meaningfully consume more, and the
	// over-fetch multiplier means a `K=1000` would
	// request `>=3000` candidates per call).
	K int
}

// Hit is a single recall result.  Mirrors
// `embedding.SearchHit` so callers can read back the same
// payload the publisher wrote, while keeping the agentapi
// package free of a direct dependency on the publisher's
// internal types in its public surface.
type Hit struct {
	// PointID is the Qdrant `qdrant_point_id` — equal to
	// the `embedding_publish.qdrant_point_id` for the
	// surviving publish row.
	PointID string
	// Score is the Qdrant similarity score (cosine).
	Score float32
	// Payload is the raw Qdrant payload Qdrant returned.
	// Production callers typically read `node_id`,
	// `repo_id`, `kind`, `canonical_signature`, and
	// `embedding_model_version` from this map.
	Payload map[string]any
}

// RecallResponse is the output of `Service.Recall`.
type RecallResponse struct {
	// Hits is the ordered (descending score) list of
	// surviving candidates, capped at `RecallRequest.K`.
	// All entries are guaranteed to have reached §9.6a
	// `published`; the `embedding.RecallFilter` removed
	// any queued / failed / orphan entries.
	Hits []Hit
	// OverFetched is the number of candidates Qdrant
	// returned BEFORE filtering — useful for tests and
	// the operator dashboard to gauge filter overhead.
	OverFetched int
	// Filtered is the number of candidates the §9.6a
	// filter removed.  Equal to the increment of
	// `recall_filter_unpublished_total` for this call.
	// Computed as `len(pointIDs) - len(allowed)` so it
	// reflects ONLY hits the filter examined and rejected
	// for being unpublished — candidates with empty
	// `PointID` (invalid Qdrant responses) are skipped
	// before the filter runs and are deliberately NOT
	// counted here, keeping the operator dashboard signal
	// for "publish-state lag" clean.
	// When `Filtered > 0 && len(Hits) < K`, the over-fetch
	// budget was insufficient — operators should consider
	// bumping `WithOverFetchMultiplier`.
	Filtered int

	// Degraded surfaces the cross-process degraded-state
	// flag the Span Ingestor (Stage 4.2) raises when its
	// queue depth exceeds the §8.3 sustained envelope. The
	// agent-api recall handler populates this from the
	// `HealthSource` plumbed via `WithHealthSource`; when no
	// HealthSource is configured (or it errors), this is
	// always false.
	//
	// Per tech-spec §C22, the closed-set value the recall
	// surface contract expects on `DegradedReason` is one of
	// the `degraded_reason` ENUM literals (e.g.
	// "span_ingestor_backpressure"). `DegradedReason` is
	// empty when `Degraded` is false.
	Degraded       bool
	DegradedReason string

	// ContextID is the durable RecallContextLog row id the
	// handler appended at Step 6. Empty when no
	// `ContextLogAppender` was wired or when the append
	// failed (soft-error contract).  The caller stores this
	// id and can later replay the snapshot via
	// `agent.observe(context_id=ContextID)`.
	ContextID string

	// RerankerModelVersion is the version string the
	// reranker reported at Step 5. Pinned onto the
	// RecallContextLog row for reproducibility per
	// architecture.md §5.4.1. Empty when no `Reranker` was
	// wired (legacy two-step path).
	RerankerModelVersion string

	// Nodes is the typed Node-card surface. Mirrors `Hits`
	// for Method/Block kinds; downstream gRPC projection
	// uses this slice. The legacy `Hits` slice is preserved
	// for backward compatibility with existing tests.
	Nodes []NodeHit

	// Edges is the structural edge fan-out the Step-4
	// `SeedExpander` discovered. Empty when no expander was
	// wired.
	Edges []EdgeHit

	// Concepts is the Concept-layer hits the mixed-seed
	// fan-out surfaced (Stage 5.1 step 2). Empty when
	// `WithConceptsEnabled(false)` (the default) or the
	// concept collection returned no hits.
	Concepts []ConceptHit
}

// ErrEmptyQuery is returned by `Service.Recall` when the
// caller passes an empty / whitespace-only `Query`.
var ErrEmptyQuery = errors.New("agentapi: recall: empty query")

// ErrInvalidKind is returned when `RecallRequest.Kind` is
// neither "method" nor "block".
var ErrInvalidKind = errors.New("agentapi: recall: invalid kind")

// ErrInvalidK is returned when `RecallRequest.K <= 0`.
var ErrInvalidK = errors.New("agentapi: recall: K must be > 0")

// maxK is the per-call ceiling for `RecallRequest.K`.
const maxK = 256

// Recall implements the §6.4 read path. The full Stage 5.1
// pipeline (when every optional dependency is wired):
//
//  1. Embed the query into a vector via `s.embedder`.
//  2. Issue a MIXED k-NN search against the Method
//     collection AND, when `WithConceptsEnabled(true)`, the
//     Concept collection — both filtered by `repo_id`
//     server-side per §7.8 mixed seed.
//  3. Extract candidate point ids and feed them to
//     `s.filter.FilterPublishedPoints`, which returns the
//     subset that has reached §9.6a `published`. The filter
//     increments `recall_filter_unpublished_total` per
//     filtered hit on its own (we do not double-count here).
//  4. When a `SeedExpander` is wired, walk 1–2 hops from the
//     seed Node ids and fold the discovered edges into the
//     response envelope; tag any newly-discovered candidates
//     with a non-zero structural distance for the reranker.
//  5. When a `Reranker` is wired, recompute the per-hit
//     score and re-order. Otherwise hits surface in raw
//     cosine order.
//  6. When a `ContextLogAppender` is wired, append one
//     RecallContextLog row carrying the rank-ordered ids +
//     the reranker model version + the degraded flag. The
//     returned context_id surfaces on the response so the
//     caller can replay via `agent.observe`.
//
// Degraded fallback (Step 9). When Qdrant returns a
// dependency error AND a `SnapshotSource` is wired, the
// handler:
//
//   - Loads the most recent valid RecallContextLog snapshot
//     for the repo via `SnapshotSource.LatestForRepo`.
//   - Projects it onto a `RecallResponse` stamped with
//     `Degraded=true` and
//     `DegradedReason='embedding_index_unavailable'`.
//   - Appends a fresh RecallContextLog row with
//     `ServedUnderDegraded=true` so a later observe knows.
//   - Returns nil error so the agent keeps its loop alive.
//
// The legacy two-step path (no Reranker, no Expander, no
// ContextLog, no Snapshot, no Concepts) is preserved when
// none of the optionals are wired so existing fixtures keep
// passing.
//
// The function NEVER returns an empty `Hits` slice with a
// nil error AND `OverFetched > 0` silently — when the
// filter would have rejected every candidate, the response
// still carries `OverFetched` and `Filtered` so the
// operator can distinguish "no vectors matched the query"
// from "all matching vectors are still queued/failed".
func (s *Service) Recall(ctx context.Context, req RecallRequest) (RecallResponse, error) {
	if strings.TrimSpace(req.Query) == "" {
		return RecallResponse{}, ErrEmptyQuery
	}
	if req.K <= 0 {
		return RecallResponse{}, ErrInvalidK
	}
	if req.K > maxK {
		req.K = maxK
	}
	kinds, err := s.normalizeKinds(req)
	if err != nil {
		return RecallResponse{}, err
	}

	vec, err := s.embedder.Embed(ctx, req.Query)
	if err != nil {
		return RecallResponse{}, fmt.Errorf("agentapi: recall: embed query: %w", err)
	}
	if len(vec) == 0 {
		return RecallResponse{}, errors.New(
			"agentapi: recall: embedder returned empty vector")
	}

	overFetch := req.K * s.overFetchMultiplier
	if overFetch < req.K {
		overFetch = req.K // overflow guard for absurd multipliers
	}

	if req.RepoID == "" {
		s.logger.Warn("agentapi.recall.unscoped",
			slog.String("collections", strings.Join(kindsToCollections(kinds), ",")),
			slog.Int("k", req.K),
			slog.String("hint", "set RecallRequest.RepoID for cross-tenant scoping"))
	}

	// Step 2 — mixed seed search across every requested
	// kind's Qdrant collection. One Search call per kind so
	// the per-collection filters stay scoped; the candidate
	// lists merge downstream before the §9.6a filter runs.
	type collectionResult struct {
		kind string
		hits []embedding.SearchHit
	}
	results := make([]collectionResult, 0, len(kinds))
	totalOverFetched := 0
	for _, kind := range kinds {
		collection, cerr := collectionFor(kind)
		if cerr != nil {
			// Defence-in-depth: normalizeKinds already
			// validated the kind, but if a future kind is
			// added without updating the collection table
			// the error surfaces here instead of a nil-
			// collection Qdrant request.
			return RecallResponse{}, fmt.Errorf("agentapi: recall: %w", cerr)
		}
		hits, serr := s.searcher.Search(ctx, collection, embedding.SearchRequest{
			Vector:       vec,
			Limit:        overFetch,
			RepoIDFilter: req.RepoID,
		})
		if serr != nil {
			return s.degradedFallback(ctx, req, "qdrant", serr)
		}
		results = append(results, collectionResult{kind: kind, hits: hits})
		totalOverFetched += len(hits)
	}

	if totalOverFetched == 0 {
		resp := RecallResponse{
			Hits:        []Hit{},
			Nodes:       []NodeHit{},
			Edges:       []EdgeHit{},
			Concepts:    []ConceptHit{},
			OverFetched: 0,
			Filtered:    0,
		}
		// Surface the cross-process degraded flag even on
		// the empty-candidates path. Evaluator iter-1 #4:
		// before this fix, a backpressured repo with zero
		// Qdrant hits returned a Degraded=false envelope,
		// suppressing the §C22 contract signal.
		s.populateDegraded(ctx, req, &resp)
		if s.reranker != nil {
			resp.RerankerModelVersion = s.reranker.ModelVersion()
		}
		s.appendContextLog(ctx, req, &resp, kinds)
		return resp, nil
	}

	// Step 3 — §9.6a publish filter. We collect point ids
	// across EVERY collection in one round-trip so the
	// counter and the network call stay bounded by the
	// total candidate count, not the per-collection count.
	pointIDs := make([]string, 0, totalOverFetched)
	for _, r := range results {
		for _, c := range r.hits {
			if c.PointID == "" {
				continue
			}
			pointIDs = append(pointIDs, c.PointID)
		}
	}
	allowed, err := s.filter.FilterPublishedPoints(ctx, pointIDs)
	if err != nil {
		return RecallResponse{}, fmt.Errorf("agentapi: recall: filter published: %w", err)
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		allowSet[id] = struct{}{}
	}

	// Build the candidate slice preserving per-collection
	// cosine-order (Method/Block first, Concept last per
	// the iteration order of `kinds`).  The reranker
	// re-orders before the K cap.
	candidates := make([]Candidate, 0, totalOverFetched)
	for _, r := range results {
		for _, c := range r.hits {
			if _, ok := allowSet[c.PointID]; !ok {
				continue
			}
			candidates = append(candidates, Candidate{
				PointID:            c.PointID,
				Score:              c.Score,
				Kind:               r.kind,
				StructuralDistance: 0,
				Payload:            c.Payload,
			})
		}
	}

	// Step 4 — graph expansion. Walks outward from each seed
	// Node id discovered in the Method/Block collections;
	// Concept hits do NOT seed expansion (they live in the
	// Concept space, not the structural graph).
	var expansion ExpansionResult
	if s.seedExpander != nil {
		seedNodeIDs := collectSeedNodeIDs(candidates)
		if len(seedNodeIDs) > 0 {
			depth := s.expansionDepth
			if depth < 1 {
				depth = defaultExpansionDepth
			}
			expanded, expErr := s.seedExpander.Expand(ctx, seedNodeIDs, depth)
			if expErr != nil {
				// Graph-store outages map to the §C22
				// `graph_store_unavailable` reason; the
				// recall falls back to the most recent
				// snapshot for the repo (evaluator iter-1
				// #6 — pre-iter-2 this path silently
				// swallowed graph outages).
				if errors.Is(expErr, ErrGraphStoreUnavailable) {
					return s.degradedFallback(ctx, req, "graph", expErr)
				}
				// Other expander errors stay soft: a
				// transient hiccup in one expander call
				// should NOT take down the recall response.
				s.logger.Warn("agentapi.recall.expand_failed",
					slog.String("err", expErr.Error()),
					slog.Int("seed_count", len(seedNodeIDs)))
			} else {
				expansion = expanded
				candidates = appendFrontierCandidates(candidates, expansion, allowSet)
			}
		}
	}

	// Step 5 — rerank. v0 cold-start (cosine + structural
	// distance) or the trained model when one exists. When no
	// reranker is wired we keep the raw cosine order from
	// Qdrant and propagate `Score` → `FinalScore` so the
	// response projection below (which reads `c.FinalScore`)
	// still surfaces a meaningful similarity number instead
	// of the struct zero value.
	ranked := candidates
	rerankerVersion := ""
	if s.reranker != nil {
		ranked = s.reranker.Rank(candidates)
		rerankerVersion = s.reranker.ModelVersion()
	} else {
		for i := range ranked {
			ranked[i].FinalScore = ranked[i].Score
		}
	}

	// Project ranked candidates onto the response shape.
	// `Hits` (legacy) carries Method/Block in rank order;
	// `Nodes` is the new typed surface, `Concepts` carries
	// the Concept hits separately.  The combined cap is
	// `req.K` per the e2e scenario "len(nodes)+len(concepts)
	// == k" (e2e-scenarios.md line 461).
	totalCap := req.K
	hits := make([]Hit, 0, totalCap)
	nodes := make([]NodeHit, 0, totalCap)
	concepts := make([]ConceptHit, 0, totalCap)
	taken := 0
	for _, c := range ranked {
		if taken >= totalCap {
			break
		}
		if c.Kind == NodeKindConcept {
			concepts = append(concepts, conceptHitFromCandidate(c))
		} else {
			hits = append(hits, Hit{
				PointID: c.PointID,
				Score:   c.FinalScore,
				Payload: c.Payload,
			})
			nodes = append(nodes, nodeHitFromCandidate(c))
		}
		taken++
	}

	resp := RecallResponse{
		Hits:                 hits,
		Nodes:                nodes,
		Edges:                expansion.Edges,
		Concepts:             concepts,
		OverFetched:          totalOverFetched,
		Filtered:             len(pointIDs) - len(allowed),
		RerankerModelVersion: rerankerVersion,
	}
	s.populateDegraded(ctx, req, &resp)
	s.appendContextLog(ctx, req, &resp, kinds)
	return resp, nil
}

// normalizeKinds resolves the deprecated `Kind` and the new
// `Kinds` fields into a single validated + deduped slice the
// recall handler iterates over. See `RecallRequest.Kinds`
// for the full resolution contract (defaults / single-Kind
// alias / concept fan-out).
func (s *Service) normalizeKinds(req RecallRequest) ([]string, error) {
	// 1) Both Kinds and Kind set: Kind MUST be present in
	//    Kinds — a mismatch is almost always a copy-paste
	//    bug and silently dropping the legacy constraint
	//    would surprise the caller.
	if len(req.Kinds) > 0 && req.Kind != "" {
		found := false
		for _, k := range req.Kinds {
			if k == req.Kind {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf(
				"%w: req.Kind=%q not present in req.Kinds=%v",
				ErrInvalidKind, req.Kind, req.Kinds)
		}
	}

	var raw []string
	switch {
	case len(req.Kinds) > 0:
		// Multi-kind path. `Kinds` is the canonical input;
		// the validator below rejects any entry outside the
		// closed set {method, block, concept}.
		raw = append(raw, req.Kinds...)
	case req.Kind != "":
		// Deprecated single-Kind path. Stays strict on
		// method/block to keep the legacy
		// TestRecall_inputValidation "Kind=concept rejected"
		// contract intact; callers that want concept-only
		// recall should pass `Kinds: []string{"concept"}`.
		if req.Kind != embedding.NodeKindMethod && req.Kind != embedding.NodeKindBlock {
			return nil, fmt.Errorf("%w: got %q", ErrInvalidKind, req.Kind)
		}
		raw = []string{req.Kind}
		if s.conceptsEnabled {
			// Backward compat: legacy `Kind=method` +
			// `conceptsEnabled=true` callers expect the
			// concept fan-out the iter-1 tests pinned.
			raw = append(raw, NodeKindConcept)
		}
	default:
		// Both empty: the §7.8 mixed seed default set.
		raw = []string{embedding.NodeKindMethod, embedding.NodeKindBlock}
		if s.conceptsEnabled {
			raw = append(raw, NodeKindConcept)
		}
	}

	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, k := range raw {
		switch k {
		case embedding.NodeKindMethod, embedding.NodeKindBlock, NodeKindConcept:
		default:
			return nil, fmt.Errorf(
				"%w: %q (allowed: method/block/concept)",
				ErrInvalidKind, k)
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	if len(out) == 0 {
		// Unreachable via the normal flow (defaults always
		// produce at least method+block); defensive belt
		// for a future refactor that disables every default.
		return nil, fmt.Errorf("%w: no kinds resolved", ErrInvalidKind)
	}
	return out, nil
}

// collectionFor maps a recall kind to the Qdrant collection
// the publisher writes that kind's vectors into. Wraps
// `embedding.CollectionFor` so the Concept kind (which
// `embedding.CollectionFor` does NOT recognise — Concepts
// have their own collection but no publisher Kind enum
// value) is handled in one place.
func collectionFor(kind string) (string, error) {
	if kind == NodeKindConcept {
		return embedding.CollectionConcept, nil
	}
	return embedding.CollectionFor(kind)
}

// kindsToCollections is a tiny adapter for the unscoped-query
// warn log so an operator sees `agent_memory_method,agent_memory_concept`
// in the log line instead of `method,concept`.
func kindsToCollections(kinds []string) []string {
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		if c, err := collectionFor(k); err == nil {
			out = append(out, c)
		} else {
			out = append(out, k)
		}
	}
	return out
}

// appendFrontierCandidates promotes the expander's frontier
// onto the candidate set with an inherited seed score and a
// structural distance equal to the hop count. Resolves
// evaluator iter-1 #4 ("ExpansionResult.FrontierNodeIDs is
// never converted into rerankable Candidates and structural
// distance is effectively always zero for served candidates").
//
// The inherited-score formula is `BestSeedScore * 0.5^hop`
// (rubber-duck #3 on the iter-2 plan): a 1-hop neighbour of a
// 0.9-score seed scores 0.45, a 2-hop neighbour 0.225. The
// reranker then subtracts `StructuralWeight * hop = 0.1*hop`
// from that figure for the §9.5 cold-start ordering. The
// combination keeps a 1-hop frontier candidate competitive
// with a low-score direct hit while still preferring direct
// hits at parity.
//
// Frontier entries with the same NodeID as an existing seed
// candidate are skipped: the seed already scored against the
// reranker with its true cosine; promoting a frontier shadow
// of the same NodeID would double-count.
//
// When the expander did NOT hydrate the Frontier slice but
// populated the deprecated `FrontierNodeIDs`, the function
// synthesises one `FrontierNode{NodeID, Hop:1}` per id with
// the best seed score of the input candidates so legacy test
// fakes (which only set FrontierNodeIDs) still see promotion.
func appendFrontierCandidates(
	seeds []Candidate, exp ExpansionResult, allowSet map[string]struct{},
) []Candidate {
	// Compute the best seed score once. ANY frontier node
	// whose own BestSeedScore is unset (production
	// GraphReaderExpander leaves it at zero because the
	// underlying graph layer has no notion of the embedding
	// score) inherits this value so it ranks ahead of the
	// "structural-only" floor instead of as a negative
	// candidate. Resolves evaluator iter-2 finding #2.
	var bestSeedScore float32
	for _, s := range seeds {
		if s.Score > bestSeedScore {
			bestSeedScore = s.Score
		}
	}

	frontier := exp.Frontier
	if len(frontier) == 0 && len(exp.FrontierNodeIDs) > 0 {
		frontier = make([]FrontierNode, 0, len(exp.FrontierNodeIDs))
		for _, id := range exp.FrontierNodeIDs {
			frontier = append(frontier, FrontierNode{
				NodeID:        id,
				Hop:           1,
				BestSeedScore: bestSeedScore,
			})
		}
	}
	if len(frontier) == 0 {
		return seeds
	}

	// Build a set of seed NodeIDs so we don't double-count
	// nodes that are both a seed AND a frontier destination.
	seedSet := make(map[string]struct{}, len(seeds))
	for _, c := range seeds {
		if id, ok := c.Payload["node_id"].(string); ok && id != "" {
			seedSet[id] = struct{}{}
		}
	}

	out := seeds
	for _, fn := range frontier {
		if fn.NodeID == "" {
			continue
		}
		if _, dup := seedSet[fn.NodeID]; dup {
			continue
		}
		hop := fn.Hop
		if hop <= 0 {
			hop = 1
		}
		// Discount the best seed score by 0.5 per hop.
		// `0.5^hop` is computed inline (no math.Pow) so the
		// branch stays allocation-free.
		discount := float32(1)
		for i := 0; i < hop; i++ {
			discount *= 0.5
		}
		// Use the FrontierNode's own seed-score signal when the
		// expander supplied one (in-tree fakes do); fall back
		// to the aggregate best-seed-score when it's missing or
		// non-positive — this is the load-bearing branch for the
		// production GraphReaderExpander which doesn't track
		// embedding scores on the graph side.
		seedScore := fn.BestSeedScore
		if seedScore <= 0 {
			seedScore = bestSeedScore
		}
		inheritedScore := seedScore * discount
		kind := fn.Kind
		if kind == "" {
			// Unknown kind from a non-hydrated expander —
			// default to method so the reranker scores the
			// candidate against the seed prior, not the
			// concept bonus.
			kind = embedding.NodeKindMethod
		}
		// Frontier candidates don't carry a Qdrant point id
		// (graph-only nodes); the publish filter doesn't
		// apply, so we don't consult `allowSet`. We do
		// stamp the payload with the dereferenced NodeID
		// so `nodeHitFromCandidate` can project a NodeHit
		// onto the response.
		payload := map[string]any{
			"node_id":             fn.NodeID,
			"repo_id":             fn.RepoID,
			"kind":                kind,
			"canonical_signature": fn.CanonicalSignature,
		}
		out = append(out, Candidate{
			PointID:            "", // graph-only, no Qdrant id
			Score:              inheritedScore,
			Kind:               kind,
			StructuralDistance: hop,
			Payload:            payload,
		})
	}
	_ = allowSet // kept on signature for future per-frontier filtering
	return out
}

// degradedFallback projects the most recent valid snapshot
// onto a degraded response, appends a fresh
// `served_under_degraded=true` RecallContextLog row, and
// returns. When no `SnapshotSource` is wired the underlying
// dependency error propagates verbatim (matching legacy
// behaviour). When the snapshot lookup itself errors the
// handler emits a degraded ENVELOPE with empty hits — the
// degraded contract is the load-bearing signal here, even
// if there is no prior snapshot to dehydrate.
func (s *Service) degradedFallback(
	ctx context.Context, req RecallRequest, layer string, cause error,
) (RecallResponse, error) {
	reason := classifyDegradedReason(layer, cause)
	if reason == "" || s.snapshot == nil {
		// No snapshot wired (or unknown layer): preserve
		// the legacy hard-fail contract so a misconfigured
		// production binary surfaces the underlying error
		// instead of silently swallowing it.
		switch layer {
		case "qdrant":
			return RecallResponse{}, fmt.Errorf(
				"agentapi: recall: qdrant search: %w", cause)
		case "graph":
			return RecallResponse{}, fmt.Errorf(
				"agentapi: recall: graph reader: %w", cause)
		default:
			return RecallResponse{}, cause
		}
	}

	s.logger.Warn("agentapi.recall.degraded_fallback",
		slog.String("layer", layer),
		slog.String("reason", reason),
		slog.String("repo_id", req.RepoID),
		slog.String("cause", cause.Error()))

	// Resolve `kinds` for the context-log row WITHOUT
	// failing the fallback if the request's kinds are
	// invalid: we want the degraded envelope to surface
	// even on a malformed request. `normalizeKinds`
	// returning an error here is non-fatal — the row just
	// records an empty `kinds[]`.
	kinds, _ := s.normalizeKinds(req)

	snap, snapErr := s.snapshot.LatestForRepo(ctx, req.RepoID)
	if snapErr != nil {
		if !errors.Is(snapErr, ErrNoSnapshot) {
			s.logger.Warn("agentapi.recall.snapshot_lookup_failed",
				slog.String("repo_id", req.RepoID),
				slog.String("err", snapErr.Error()))
		}
		// Empty-hit degraded envelope — still useful (the
		// agent sees the degraded flag and can decide
		// whether to retry).
		resp := RecallResponse{
			Hits:           []Hit{},
			Nodes:          []NodeHit{},
			Edges:          []EdgeHit{},
			Concepts:       []ConceptHit{},
			Degraded:       true,
			DegradedReason: reason,
		}
		if s.reranker != nil {
			resp.RerankerModelVersion = s.reranker.ModelVersion()
		}
		s.appendContextLogDegraded(ctx, req, &resp, kinds)
		return resp, nil
	}

	resp := snapshotResponseForDegraded(snap, reason, req.K)
	// Project Nodes onto the legacy Hits slice so callers
	// reading the old surface still see the fallback hits.
	resp.Hits = make([]Hit, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		resp.Hits = append(resp.Hits, Hit{
			PointID: n.PointID,
			Score:   n.Score,
			Payload: map[string]any{
				"node_id":             n.NodeID,
				"repo_id":             n.RepoID,
				"kind":                n.Kind,
				"canonical_signature": n.CanonicalSignature,
			},
		})
	}
	s.appendContextLogDegraded(ctx, req, &resp, kinds)
	return resp, nil
}

// collectSeedNodeIDs extracts the `node_id` payload field
// from each Method/Block candidate so the expander has a
// concrete id set to walk outward from. Concepts are not
// part of the structural graph so they are skipped.
func collectSeedNodeIDs(cs []Candidate) []string {
	seen := make(map[string]struct{}, len(cs))
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.Kind == NodeKindConcept {
			continue
		}
		id, _ := c.Payload["node_id"].(string)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// nodeHitFromCandidate projects a ranked Method/Block
// Candidate onto the typed NodeHit shape. Pulls the
// canonical fields out of the Qdrant payload the publisher
// wrote.
func nodeHitFromCandidate(c Candidate) NodeHit {
	nh := NodeHit{
		PointID: c.PointID,
		Score:   c.FinalScore,
		Kind:    c.Kind,
	}
	if id, ok := c.Payload["node_id"].(string); ok {
		nh.NodeID = id
	}
	if id, ok := c.Payload["repo_id"].(string); ok {
		nh.RepoID = id
	}
	if sig, ok := c.Payload["canonical_signature"].(string); ok {
		nh.CanonicalSignature = sig
	}
	return nh
}

// conceptHitFromCandidate projects a Concept Candidate onto
// the typed ConceptHit shape. The publisher writes `name`
// onto the Concept payload (see
// `cmd/concept-promoter/main.go.buildPayload` once Stage
// 6.2 lands); until then the Name field may be empty.
func conceptHitFromCandidate(c Candidate) ConceptHit {
	ch := ConceptHit{
		PointID: c.PointID,
		Score:   c.FinalScore,
	}
	if id, ok := c.Payload["concept_id"].(string); ok {
		ch.ConceptID = id
	}
	if name, ok := c.Payload["name"].(string); ok {
		ch.Name = name
	}
	return ch
}

// appendContextLog writes the Step-6 RecallContextLog row
// for a happy-path recall (ServedUnderDegraded=false). The
// resolved `kinds` slice is stored verbatim on the row so a
// later operator inspection sees exactly which collections
// the recall fanned out across (not just the deprecated
// singular `kind` value).
func (s *Service) appendContextLog(
	ctx context.Context, req RecallRequest, resp *RecallResponse, kinds []string,
) {
	if s.contextLog == nil {
		return
	}
	in := s.buildContextLogInput(req, resp, false, kinds)
	rec, err := s.contextLog.Append(ctx, in)
	if err != nil {
		// Soft failure — recall response is still served.
		s.logger.Warn("agentapi.recall.context_log_append_failed",
			slog.String("repo_id", req.RepoID),
			slog.String("err", err.Error()))
		return
	}
	resp.ContextID = rec.ContextID
}

// appendContextLogDegraded writes the Step-6 row for a
// degraded-fallback recall. ServedUnderDegraded=true so a
// later Stage 5.2 observe knows to auto-stamp a
// `degraded_recall_context` Observation per
// architecture.md §6.1.2.
func (s *Service) appendContextLogDegraded(
	ctx context.Context, req RecallRequest, resp *RecallResponse, kinds []string,
) {
	if s.contextLog == nil {
		return
	}
	in := s.buildContextLogInput(req, resp, true, kinds)
	rec, err := s.contextLog.Append(ctx, in)
	if err != nil {
		s.logger.Warn("agentapi.recall.context_log_append_failed_degraded",
			slog.String("repo_id", req.RepoID),
			slog.String("err", err.Error()))
		return
	}
	resp.ContextID = rec.ContextID
}

// buildContextLogInput packs the response into the
// `ContextLogAppender.Append` shape. The reranker model
// version on the input is the response's value (which is
// the snapshot's version on the degraded path and the live
// reranker's version on the happy path).
//
// The `kinds` slice is the post-normalisation set of node
// kinds the recall fanned out across; stored under the
// `kinds` JSON key (plural) so a multi-kind recall round-
// trips faithfully. The deprecated singular `kind` field is
// retained on the doc shape (omitted from JSON when empty)
// so legacy operator scripts that still read it keep
// working.
func (s *Service) buildContextLogInput(
	req RecallRequest, resp *RecallResponse, degraded bool, kinds []string,
) ContextLogInput {
	in := ContextLogInput{
		Verb:                 "recall",
		RepoID:               req.RepoID,
		RerankerModelVersion: resp.RerankerModelVersion,
		ServedUnderDegraded:  degraded,
	}
	queryDoc := struct {
		Query  string   `json:"query"`
		Kind   string   `json:"kind,omitempty"`
		Kinds  []string `json:"kinds"`
		K      int      `json:"k"`
		RepoID string   `json:"repo_id,omitempty"`
	}{
		Query:  req.Query,
		Kind:   req.Kind,
		Kinds:  kinds,
		K:      req.K,
		RepoID: req.RepoID,
	}
	if buf, err := json.Marshal(queryDoc); err == nil {
		in.QueryJSON = buf
	} else {
		// Marshal of a fixed-shape struct should never
		// fail; fall back to an empty object so the
		// downstream validator (`json.Valid`) still
		// passes.
		in.QueryJSON = json.RawMessage(`{}`)
	}
	for _, n := range resp.Nodes {
		if n.NodeID != "" {
			in.NodeIDs = append(in.NodeIDs, n.NodeID)
		}
	}
	for _, e := range resp.Edges {
		if e.EdgeID != "" {
			in.EdgeIDs = append(in.EdgeIDs, e.EdgeID)
		}
	}
	for _, c := range resp.Concepts {
		if c.ConceptID != "" {
			in.ConceptIDs = append(in.ConceptIDs, c.ConceptID)
		}
	}
	return in
}

// populateDegraded reads the per-repo health state (if a
// HealthSource is wired) and writes it onto the response. A
// HealthSource error is logged at warn level and ignored —
// per the rubber-duck pass on the cross-process design, a
// degraded read of the health table itself MUST NOT cascade
// into a recall failure.
//
// An empty RecallRequest.RepoID skips the lookup: the recall
// path treats "no scope" as a global query (see the unscoped
// warning above), and there is no per-repo health row to
// consult when the recall isn't scoped to a repo.
func (s *Service) populateDegraded(
	ctx context.Context, req RecallRequest, resp *RecallResponse,
) {
	if s.health == nil || req.RepoID == "" {
		return
	}
	state, err := s.health.HealthForRepo(ctx, req.RepoID)
	if err != nil {
		s.logger.Warn("agentapi.recall.health_lookup_failed",
			slog.String("repo_id", req.RepoID),
			slog.String("err", err.Error()))
		return
	}
	resp.Degraded = state.Degraded
	resp.DegradedReason = state.Reason
}
