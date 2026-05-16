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

	// Kind is the §6.2 node kind to search.  Either
	// `embedding.NodeKindMethod` ("method") or
	// `embedding.NodeKindBlock` ("block").  Required —
	// each kind lives in its OWN Qdrant collection per
	// tech-spec §8.1, so a kindless query has no
	// well-defined target collection.
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

// Recall implements the §6.4 read path:
//
//  1. Embed the query into a vector via `s.embedder`.
//  2. Search Qdrant with `K * overFetchMultiplier`
//     candidates AND the repo/kind filters (server-side
//     pushdown — keeps Qdrant from scanning unrelated
//     points).
//  3. Extract candidate point ids and feed them to
//     `s.filter.FilterPublishedPoints`, which returns the
//     subset that has reached §9.6a `published`.
//  4. Walk the original (score-ordered) hits, keep the
//     ones that survived the filter, cap at `req.K`, and
//     return.
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
	if req.Kind != embedding.NodeKindMethod && req.Kind != embedding.NodeKindBlock {
		return RecallResponse{}, fmt.Errorf("%w: got %q", ErrInvalidKind, req.Kind)
	}
	if req.K <= 0 {
		return RecallResponse{}, ErrInvalidK
	}
	if req.K > maxK {
		req.K = maxK
	}

	collection, err := embedding.CollectionFor(req.Kind)
	if err != nil {
		// Defence-in-depth: CollectionFor only returns an
		// error for kinds we've already rejected above, but
		// keeping the check here means a future kind added
		// to the enum without updating this switch surfaces
		// here, not as a nil-collection Qdrant request.
		return RecallResponse{}, fmt.Errorf("agentapi: recall: %w", err)
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
			slog.String("collection", collection),
			slog.Int("k", req.K),
			slog.String("hint", "set RecallRequest.RepoID for cross-tenant scoping"))
	}

	candidates, err := s.searcher.Search(ctx, collection, embedding.SearchRequest{
		Vector:       vec,
		Limit:        overFetch,
		RepoIDFilter: req.RepoID,
	})
	if err != nil {
		return RecallResponse{}, fmt.Errorf("agentapi: recall: qdrant search: %w", err)
	}
	if len(candidates) == 0 {
		resp := RecallResponse{Hits: []Hit{}, OverFetched: 0, Filtered: 0}
		// Surface the cross-process degraded flag even on
		// the empty-candidates path. Evaluator iter-1 #4:
		// before this fix, a backpressured repo with zero
		// Qdrant hits returned a Degraded=false envelope,
		// suppressing the §C22 contract signal.
		s.populateDegraded(ctx, req, &resp)
		return resp, nil
	}

	pointIDs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.PointID == "" {
			continue
		}
		pointIDs = append(pointIDs, c.PointID)
	}
	allowed, err := s.filter.FilterPublishedPoints(ctx, pointIDs)
	if err != nil {
		return RecallResponse{}, fmt.Errorf("agentapi: recall: filter published: %w", err)
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		allowSet[id] = struct{}{}
	}

	out := make([]Hit, 0, req.K)
	for _, c := range candidates {
		if len(out) >= req.K {
			break
		}
		if _, ok := allowSet[c.PointID]; !ok {
			continue
		}
		out = append(out, Hit{
			PointID: c.PointID,
			Score:   c.Score,
			Payload: c.Payload,
		})
	}

	resp := RecallResponse{
		Hits:        out,
		OverFetched: len(candidates),
		// Count only hits the filter actually examined and
		// rejected — candidates with empty PointID were
		// stripped before the filter call (see pointIDs
		// build loop above), so subtracting from
		// len(candidates) would conflate invalid Qdrant
		// responses with genuinely unpublished vectors and
		// pollute the recall_filter_unpublished_total
		// operator signal (§9.6a read-side).
		Filtered: len(pointIDs) - len(allowed),
	}
	s.populateDegraded(ctx, req, &resp)
	return resp, nil
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
