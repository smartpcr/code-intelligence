// Package agentapi: agent.summarize verb (architecture.md
// §6.1.4, implementation-plan.md Stage 5.4).
//
// Stage 5.4 contract
// ------------------
// The verb accepts EXACTLY ONE of `node_id` / `concept_id`,
// resolves its structural neighborhood (or the concept row),
// renders a prompt for a pluggable LLM `Summariser`, and
// returns a `SummarizeResponse` whose:
//
//   * `summary_md` is the LLM-rendered Markdown summary on
//     the happy path, OR a deterministic template built from
//     canonical signatures on the degraded path.
//   * `citations[]` carry every entity id the response is
//     grounded in. By construction each citation references
//     a row the `NeighborhoodResolver` returned (i.e. it
//     EXISTS) AND is reachable from the target id in the
//     structural graph (seed itself = 0 hops, outbound edges
//     = 1 hop, edge destinations = 1 hop). This is the
//     load-bearing invariant the Stage 5.4 acceptance
//     scenario "summary cites resolved nodes" asserts.
//   * `context_id` is the durable `recall_context_log(verb=
//     'summarize')` row id the handler appended exactly once
//     per call (soft-failure on append → empty `context_id`,
//     mirroring the recall path).
//   * `degraded` / `degraded_reason` use the six-value §C22
//     closed set pinned by Stage 8.1 (no augmentation). A
//     summariser/LLM outage is classified internally as
//     `summariser_unavailable` (see classifySummariserFailure
//     below) and the wire envelope is normalised to
//     `embedding_index_unavailable` by
//     applySummarizeDegradedContract in recall.go before
//     Enforce; the original classifier value is preserved in
//     the structured log `degraded_reason_raw` field for
//     audit. The `reranker_model_stale` reason is preferred
//     when the reranker run is older than 7 days per risk
//     §9.10; the wire normalisation is bypassed for that
//     reason since it is already a §C22 closed-set value.
//
// Why everything is an interface
// ------------------------------
// The verb has FOUR cross-process dependencies:
//
//   1. The LLM (`Summariser`) — vendor-pinned at deploy via
//      the `OpenAICompatibleSummariser` adapter, but the
//      interface is the abstraction so a self-hosted vLLM
//      or a stub-fake in tests both satisfy it.
//   2. The graph (`NeighborhoodResolver`) — narrow read-side
//      view of `*graphreader.Reader` for the seed + 1-hop
//      neighborhood. Narrower than the full Reader so unit
//      tests don't need to satisfy ListNodes/etc.
//   3. The reranker freshness signal
//      (`RerankerFreshnessSource`) — tiny adapter over a
//      SELECT on `reranker_model.trained_at` so the
//      degraded-reason classifier can pick
//      `reranker_model_stale` vs `summariser_unavailable`
//      without dragging the full Reader contract through
//      the unit test surface.
//   4. The RecallContextLog appender (`ContextLogAppender`)
//      — already defined for the recall path; reused here
//      with `Verb="summarize"`.
//
// Defence-in-depth on the timeout
// -------------------------------
// The 5 s summariser budget is enforced via
// `context.WithTimeout` even when the caller's parent ctx
// has a longer deadline. A caller forgetting to cap its
// summariser call would otherwise tie up the LLM endpoint
// indefinitely on a slow/dead model. The cap is a defence,
// not a UX-tunable; bumping it requires a code change.
//
// Cancellation vs timeout — distinct
// ----------------------------------
// Parent-ctx cancellation propagates as an error (the
// caller is gone; no point synthesising a degraded
// envelope for an absent client). Only the internal 5 s
// budget triggers the degraded template fallback. This
// distinction matters under gRPC: a deadline pushed down
// by the client SHOULD surface as a `DeadlineExceeded`
// status, not a `degraded=true` envelope the client never
// asked to honour.
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// VerbSummarize is the §6.3 verb identifier the Stage 8.1
// wiring stamps on degraded-metric counters and
// `FaultInjector` lookups. Package-level constant so a test
// (or operator-facing rule editor) cannot drift from the
// on-the-wire spelling.
const VerbSummarize = "agent.summarize"

// Summariser is the pluggable LLM client the summarize verb
// invokes to render `summary_md`. Vendor pin for v1 is any
// OpenAI-API-compatible HTTPS endpoint (see
// `OpenAICompatibleSummariser`); the interface stays here so
// self-hosted vLLM, a stub fake, or a future vendor-specific
// adapter all satisfy the same shape.
//
// Implementations MUST:
//   - Respect the supplied `ctx` (cancellation + deadline).
//   - Return a non-empty `SummaryMD` on success; an empty
//     string is treated by the verb as a soft failure
//     (degrade to template) so a misbehaving model cannot
//     silently surface as a zero-byte summary on the wire.
//   - Surface upstream errors verbatim — the verb's degraded
//     classifier inspects them only via `errors.Is` against
//     `context.DeadlineExceeded` (no substring matching).
type Summariser interface {
	Summarize(ctx context.Context, in SummariserInput) (SummariserOutput, error)
	// ModelVersion is the stable identifier the verb records
	// alongside the recall_context_log row (in the future
	// `summary_model_version` column; today the verb stamps
	// the *reranker_model_version* column because Stage 5.4
	// has not yet added a separate summary column — see the
	// RerankerModelVersion plumbing on `appendContextLog`).
	ModelVersion() string
}

// SummariserInput is the prompt + budget the verb hands to
// the LLM. The prompt is a fully-rendered Markdown string;
// the summariser MUST NOT re-template it.
type SummariserInput struct {
	// Prompt is the rendered Markdown instruction the LLM
	// receives verbatim. The verb structures it as
	// `## Seed`, `## Outbound edges`, `## Reachable
	// destinations` (or `## Concept` for concept targets)
	// so the data sections are clearly delimited from the
	// instruction preamble — important once a real LLM
	// client lands (rubber-duck #7: prompt-injection
	// hygiene).
	Prompt string
	// MaxTokens is the upper bound on the model's response
	// token budget. The verb clamps caller-supplied values
	// to `[1, maxSummarizeMaxTokens]` (with <= 0 defaulting
	// to `defaultSummarizeMaxTokens`) before this field is
	// populated, so summarisers can pass it through to the
	// vendor API without re-validating.
	MaxTokens int
}

// SummariserOutput is the LLM-rendered Markdown summary.
type SummariserOutput struct {
	SummaryMD string
}

// SummariserFunc adapts a plain function into a Summariser.
// `version` lets the function literal record the model
// identifier without standing up a wrapping struct.
type SummariserFunc struct {
	Fn      func(ctx context.Context, in SummariserInput) (SummariserOutput, error)
	Version string
}

// Summarize implements Summariser.
func (f SummariserFunc) Summarize(ctx context.Context, in SummariserInput) (SummariserOutput, error) {
	return f.Fn(ctx, in)
}

// ModelVersion implements Summariser.
func (f SummariserFunc) ModelVersion() string { return f.Version }

// NeighborhoodResolver is the narrow read-side view the
// summarize verb consumes from the structural graph. The
// abstraction lives in agentapi (not graphreader) so the
// verb's unit tests can substitute deterministic fakes
// without standing up a pgxpool — matching the pattern set
// by `SeedExpander` / `SnapshotSource`.
//
// The two methods MUST:
//   - Return `ErrSummarizeTargetNotFound` when the requested
//     id does not exist (or is retired and the implementation
//     treats current-view as default). The verb maps this
//     onto gRPC `codes.NotFound`.
//   - Return `ErrGraphStoreUnavailable` when the underlying
//     graph store is unreachable. The verb pattern-matches
//     and routes the call into the degraded template
//     envelope (no hard 5xx).
//   - Surface any other error verbatim. The verb classifies
//     unknown errors as hard failures (Internal) so a future
//     "graph layer corrupt" signal is not silently swallowed.
type NeighborhoodResolver interface {
	NeighborhoodForNode(ctx context.Context, nodeID string) (SummarizeNodeNeighborhood, error)
	// FetchConcept resolves the concept row + the
	// `concept_support` rows that the e2e contract requires
	// the summarize verb to cite. `repoID` scopes the
	// support lookup because Concepts are cross-repo per
	// architecture §5.5.1 + G6 but `SummarizeRequest.repo_id`
	// is REQUIRED on the concept path, so the resolver
	// MUST filter supports to that repo (otherwise the
	// citation set would surface unrelated repos' Nodes /
	// Episodes).
	FetchConcept(ctx context.Context, conceptID, repoID string) (SummarizeConceptCard, error)
}

// NeighborhoodResolverFunc adapts a plain function into a
// NeighborhoodResolver. Used by tests + the binary
// composition root.
type NeighborhoodResolverFunc struct {
	NeighborhoodFn func(ctx context.Context, nodeID string) (SummarizeNodeNeighborhood, error)
	ConceptFn      func(ctx context.Context, conceptID, repoID string) (SummarizeConceptCard, error)
}

// NeighborhoodForNode implements NeighborhoodResolver.
func (f NeighborhoodResolverFunc) NeighborhoodForNode(ctx context.Context, nodeID string) (SummarizeNodeNeighborhood, error) {
	return f.NeighborhoodFn(ctx, nodeID)
}

// FetchConcept implements NeighborhoodResolver.
func (f NeighborhoodResolverFunc) FetchConcept(ctx context.Context, conceptID, repoID string) (SummarizeConceptCard, error) {
	return f.ConceptFn(ctx, conceptID, repoID)
}

// SummarizeNodeNeighborhood is the seed + 1-hop view the
// summarize verb prompts the LLM with and grounds its
// citations in.
//
// Invariants the verb relies on:
//   - `Node.NodeID == nodeID` the caller asked for.
//   - `Edges[*].SrcNodeID == Node.NodeID`. Inbound edges
//     are intentionally NOT surfaced — the architecture's
//     §6.1.4 contract is "neighborhood card", which the
//     existing `graphreader.NeighborhoodCard` defines as
//     outbound-only.
//   - `Targets[*].NodeID` is the unique set of destination
//     node ids appearing in `Edges[*].DstNodeID`, deduped
//     against `Node.NodeID` (a self-loop edge does NOT
//     produce a duplicate target). Resolvers MUST return
//     real Node cards (not synthetic stubs) so the
//     "citation references a row that exists" invariant
//     holds.
type SummarizeNodeNeighborhood struct {
	Node    SummarizeNodeCard
	Edges   []SummarizeEdgeCard
	Targets []SummarizeNodeCard
}

// SummarizeNodeCard is the per-Node projection the
// summarize verb consumes. Narrower than `graphreader.Node`
// (no Fingerprint / AttrsJSON) so a test fake doesn't have
// to fabricate those fields.
type SummarizeNodeCard struct {
	NodeID             string
	RepoID             string
	Kind               string
	CanonicalSignature string
}

// SummarizeEdgeCard is the per-Edge projection. Carries
// `ObservationCount` so the prompt can include "observed N
// times" hints and the template fallback can rank by
// hotness without an extra round-trip.
type SummarizeEdgeCard struct {
	EdgeID             string
	RepoID             string
	Kind               string
	SrcNodeID          string
	DstNodeID          string
	DstSignature       string
	ObservationCount   int64
}

// SummarizeConceptCard is the projection for concept
// targets. The verb stamps these onto the prompt and the
// template fallback.
//
// `Supports` carries the `concept_support` rows the e2e
// scenario "summary cites resolved nodes (concept_id case)"
// requires the summarize verb to surface as citations.
// Each entry is one (Node, Episode) provenance link
// joining the concept to the structural / runtime graph
// (architecture.md §5.5.1). The resolver MUST scope the
// list to `SummarizeRequest.repo_id` and SHOULD cap it at
// `maxSummarizeConceptSupports` (32) so the citation array
// stays bounded.
type SummarizeConceptCard struct {
	ConceptID     string
	RepoID        string
	Name          string
	DescriptionMD string
	Supports      []SummarizeConceptSupport
}

// SummarizeConceptSupport is one `concept_support` row the
// summarize verb cites alongside a concept summary. Either
// `NodeID` or `EpisodeID` MAY be set (the schema's check
// constraint requires at least one); `Polarity` is the
// concept-support polarity literal ("positive" / "negative"
// — migration 0011).
type SummarizeConceptSupport struct {
	SupportID string
	NodeID    string
	EpisodeID string
	Polarity  string
	// NodeKind / NodeSignature are populated when the
	// resolver was able to hydrate the Node row. Empty
	// when `NodeID` is empty OR when the lookup raced a
	// retirement.
	NodeKind      string
	NodeSignature string
}

// RerankerFreshnessSource exposes the wall-clock timestamp
// of the most recent reranker training. The verb consults
// this ONLY on the degraded-fallback path to pick between
// `reranker_model_stale` (latest run > 7 days old) and
// `summariser_unavailable` (everything else). On the happy
// path the source is not consulted.
//
// The (time.Time, bool, error) return shape distinguishes:
//   - `(t, true, nil)`     → a row exists; use `t` to
//     decide stale vs fresh.
//   - `(_, false, nil)`    → no row exists (cold-start
//     bootstrap); treat as fresh (we cannot prove the
//     reranker is stale without a baseline).
//   - `(_, _, err)`        → lookup failed; treat as fresh
//     too (per rubber-duck #8: a degraded reason source
//     itself going down must not change the verb's
//     classifier behaviour).
type RerankerFreshnessSource interface {
	LatestRerankerTrainedAt(ctx context.Context) (time.Time, bool, error)
}

// RerankerFreshnessFunc adapts a plain function.
type RerankerFreshnessFunc func(ctx context.Context) (time.Time, bool, error)

// LatestRerankerTrainedAt implements RerankerFreshnessSource.
func (f RerankerFreshnessFunc) LatestRerankerTrainedAt(ctx context.Context) (time.Time, bool, error) {
	return f(ctx)
}

// SummarizeRequest is the input to `Service.Summarize`.
// Mirrors the proto SummarizeRequest with all string ids
// validated at the service layer (the gRPC adapter pushes
// validation here, matching the Recall pattern).
type SummarizeRequest struct {
	// NodeID is the node-summary target. Mutually exclusive
	// with ConceptID — exactly one MUST be set.
	NodeID string
	// ConceptID is the concept-summary target. Mutually
	// exclusive with NodeID.
	ConceptID string
	// RepoID scopes the recall_context_log row. REQUIRED
	// when ConceptID is set (concepts have no inherent repo
	// scope per architecture §5.5.1 + G6). For NodeID
	// requests RepoID MAY be empty; the verb derives it
	// from the seed node's `repo_id`. When supplied for a
	// NodeID request it MUST match the seed's repo or the
	// verb returns ErrSummarizeRepoMismatch.
	RepoID string
	// MaxTokens caps the summariser's response token budget.
	// Values <= 0 default to defaultSummarizeMaxTokens;
	// values > maxSummarizeMaxTokens are clamped server-side
	// so a single caller cannot saturate the shared LLM
	// endpoint.
	MaxTokens int
}

// Citation is one provenance entry on the summarize
// response. Exactly one of `NodeID` / `EdgeID` /
// `ConceptID` / `EpisodeID` is populated per citation;
// `Snippet` is an optional short excerpt (e.g. the
// canonical signature) the agent caller can render inline.
type Citation struct {
	NodeID    string
	EdgeID    string
	ConceptID string
	EpisodeID string
	Snippet   string
}

// SummarizeResponse is the verb's output. Projected onto
// the proto `SummarizeResponse` by the gRPC adapter.
type SummarizeResponse struct {
	SummaryMD      string
	ContextID      string
	TargetKind     string // "node" | "concept"
	TargetID       string
	Citations      []Citation
	Degraded       bool
	DegradedReason string
}

// Sentinel errors for caller-correctable input. The gRPC
// adapter maps each onto a specific status code:
//
//   - ErrSummarizeMissingTarget  → codes.InvalidArgument
//   - ErrSummarizeAmbiguousTarget→ codes.InvalidArgument
//   - ErrSummarizeRepoIDRequired → codes.InvalidArgument
//   - ErrSummarizeRepoMismatch   → codes.InvalidArgument
//   - ErrSummarizeMaxTokensRange → codes.InvalidArgument
//   - ErrSummarizeTargetNotFound → codes.NotFound
//   - ErrSummarizeUnconfigured   → codes.Unimplemented
//
// All other errors surface as codes.Internal.
var (
	// ErrSummarizeMissingTarget is returned when neither
	// node_id nor concept_id is supplied.
	ErrSummarizeMissingTarget = errors.New("agentapi: summarize: exactly one of node_id/concept_id required (neither supplied)")
	// ErrSummarizeAmbiguousTarget is returned when BOTH
	// node_id AND concept_id are supplied.
	ErrSummarizeAmbiguousTarget = errors.New("agentapi: summarize: exactly one of node_id/concept_id required (both supplied)")
	// ErrSummarizeRepoIDRequired is returned when the
	// concept_id path is taken without a non-empty repo_id.
	ErrSummarizeRepoIDRequired = errors.New("agentapi: summarize: repo_id is required when concept_id is supplied")
	// ErrSummarizeRepoMismatch is returned when the caller
	// supplies a node_id AND a non-empty repo_id that does
	// not match the seed node's repo_id.
	ErrSummarizeRepoMismatch = errors.New("agentapi: summarize: req.repo_id does not match the target node's repo")
	// ErrSummarizeMaxTokensRange is returned when max_tokens
	// is negative beyond the documented "<=0 means default"
	// convention. We accept zero / negative as "use the
	// default" but reject values < some sentinel only if we
	// want to be strict; today we just clamp silently to
	// keep the public contract permissive. The constant is
	// kept on the API surface so a future stricter rule can
	// land without an API churn.
	ErrSummarizeMaxTokensRange = errors.New("agentapi: summarize: max_tokens out of range")
	// ErrSummarizeTargetNotFound is returned when the
	// resolver reports the target row does not exist.
	ErrSummarizeTargetNotFound = errors.New("agentapi: summarize: target not found")
	// ErrSummarizeUnconfigured is returned when the
	// summarize verb is called against a Service that has
	// no NeighborhoodResolver wired. Matches the
	// `codes.Unimplemented` legacy behaviour of the
	// embedded `UnimplementedAgentServiceServer` so an
	// in-process caller sees the same signal as a wire
	// caller.
	ErrSummarizeUnconfigured = errors.New("agentapi: summarize: no NeighborhoodResolver wired")
)

// Degraded-reason constants surfaced from SummarizeResponse.
// Centralised so the gRPC adapter, the unit tests, and
// internal log/audit consumers can pattern-match against
// the exact value. `DegradedReasonSummariserUnavailable` is
// an INTERNAL classifier value: Stage 8.1 normalises it to
// the §C22 closed-set wire reason
// `embedding_index_unavailable` via
// `applySummarizeDegradedContract` in recall.go before
// Enforce. The original classifier is preserved on the
// `degraded_reason_raw` slog field for audit. The wire
// envelope itself only ever carries §C22 closed-set values.
const (
	// DegradedReasonSummariserUnavailable is the §6.3 +
	// Stage 5.4 internal classifier emitted when the
	// configured Summariser returns an error (including the
	// internal 5 s timeout) AND the reranker freshness signal
	// does NOT indicate staleness. It is NOT a
	// `recall_context_log.degraded_reason` ENUM value (that
	// ENUM applies to a different column) and Stage 8.1
	// confirms it is NOT a §C22 closed-set wire reason: the
	// SummarizeResponse envelope normalises it to
	// `embedding_index_unavailable` before Enforce.
	DegradedReasonSummariserUnavailable = "summariser_unavailable"

	// DegradedReasonRerankerModelStale is the §C22 ENUM
	// reason emitted when the latest reranker training is
	// older than 7 days at the moment of summariser failure.
	// Stage 5.4 brief pins this reason explicitly for
	// risk §9.10 "stale ranker can mis-rank citations and
	// the LLM amplifies the error".
	DegradedReasonRerankerModelStale = "reranker_model_stale"
)

const (
	// defaultSummarizeMaxTokens is the default response
	// budget when the caller supplies max_tokens <= 0. The
	// 512 figure mirrors the architecture.md §6.1.4 default
	// and the proto field comment.
	defaultSummarizeMaxTokens = 512

	// maxSummarizeMaxTokens is the server-side ceiling for
	// caller-supplied max_tokens. The proto comment pins
	// 4096; bumping it requires both a proto-comment update
	// and an operator review of the per-call LLM cost
	// envelope.
	maxSummarizeMaxTokens = 4096

	// defaultSummariserTimeout is the per-call cap on the
	// Summariser invocation. Enforced via
	// `context.WithTimeout` so a caller that supplied a
	// longer parent deadline still releases the LLM
	// connection promptly on a stuck model. Matches the
	// Stage 5.4 brief "summariser exceeds its 5 s deadline"
	// acceptance scenario.
	defaultSummariserTimeout = 5 * time.Second

	// rerankerStaleAfter is the threshold the degraded-
	// reason classifier compares `now - LatestRerankerTrainedAt`
	// against. Pinned at 7 days per risk §9.10 in the
	// implementation plan.
	rerankerStaleAfter = 7 * 24 * time.Hour

	// maxSummarizeEdges caps the per-call edge fan-out the
	// verb feeds into both the prompt AND the citation set.
	// Rubber-duck #3 (iter-2 plan): an unbounded fan-out
	// could blow the LLM's context window AND inflate the
	// recall_context_log `edge_ids[]` array. The number is
	// chosen as a "comfortable upper bound for an
	// agent-readable summary"; it can be raised by an
	// operator if rendered summaries start losing structural
	// fidelity, but the gate should be a deliberate code
	// change.
	//
	// Exported as `MaxSummarizeEdges` (iter-4 evaluator #3)
	// so production `NeighborhoodResolver` adapters can
	// short-circuit their N+1 dst-node hydration loops at
	// the same cap the verb enforces downstream, instead of
	// blindly hydrating every edge a hot-node card returns.
	maxSummarizeEdges = 32

	// maxSummarizeConceptSupports caps the per-call concept
	// support fan-out so the citation array (concept + N
	// Nodes + M Episodes) stays bounded for a concept that
	// accumulated thousands of support rows across its
	// lifetime. Same rationale as `maxSummarizeEdges` —
	// the cap is enforced in the verb so a misbehaving
	// resolver cannot inflate the response.
	maxSummarizeConceptSupports = 32
)

// MaxSummarizeEdges is the public-facing alias of the
// per-call edge cap the verb applies before prompt/citation
// rendering. Production `NeighborhoodResolver` adapters
// (`cmd/agent-api/main.go`) consult this to bound their own
// dst-node hydration loops to the same envelope — without
// it, a hot-node card with thousands of outbound edges would
// force the adapter into an unbounded N+1 read storm even
// though the verb would discard everything past index 32.
const MaxSummarizeEdges = maxSummarizeEdges

// MaxSummarizeConceptSupports is the public-facing alias of
// the per-call concept-support cap. Exposed for symmetry
// with `MaxSummarizeEdges`; the SQL-backed adapter pulls
// 2× this cap (so an operator can raise the verb cap
// without a schema-level change) and the verb re-caps at
// `MaxSummarizeConceptSupports` for prompt + citations.
const MaxSummarizeConceptSupports = maxSummarizeConceptSupports

const (

	// targetKindNode / targetKindConcept are the closed-set
	// values for `SummarizeResponse.TargetKind`. Mirrors the
	// architecture §6.1.4 enum.
	targetKindNode    = "node"
	targetKindConcept = "concept"
)

// summariser / neighborhood / freshness / summariserTimeout
// are the four Stage 5.4 optional dependencies plumbed onto
// the existing Service struct. Defined here (not on the
// recall.go Service literal) so the diff stays local to the
// summarize verb. The recall path does not touch any of
// these fields.
//
// We add new fields to `Service` via dedicated With* options
// below; the field declarations live on the struct in
// recall.go via a small struct literal here. This is the
// same pattern recall used when Stage 5.1 grew the original
// three-field Service into the seven-field one it is today.

// WithSummariser plumbs the LLM client. A nil summariser is
// equivalent to "no summariser configured" — every summarize
// call degrades to the template fallback with the internal
// classifier `summariser_unavailable` (Stage 8.1 normalises
// the wire reason to `embedding_index_unavailable` via
// applySummarizeDegradedContract in recall.go before
// Enforce; the original classifier is preserved in the
// `degraded_reason_raw` slog field). Production wiring
// supplies `OpenAICompatibleSummariser`.
func WithSummariser(s Summariser) Option {
	return func(svc *Service) {
		svc.summariser = s
	}
}

// WithNeighborhoodResolver plumbs the graph resolver. This
// is REQUIRED for the summarize verb to be operational; a
// nil resolver causes `Service.Summarize` to return
// `ErrSummarizeUnconfigured` (mapped to gRPC
// `codes.Unimplemented`), preserving the legacy "Stage 5.4
// not deployed yet" behaviour for binaries that haven't
// wired the resolver.
//
// The production adapter is
// `*graphreader.Reader.NeighborhoodCard` + `GetConcept`,
// wrapped in a closure that projects the read types onto
// `SummarizeNodeCard` / `SummarizeEdgeCard` /
// `SummarizeConceptCard`. The binary composition root in
// `cmd/agent-api/main.go` owns the projection.
func WithNeighborhoodResolver(r NeighborhoodResolver) Option {
	return func(svc *Service) {
		svc.neighborhood = r
	}
}

// WithRerankerFreshness plumbs the reranker freshness
// signal. OPTIONAL; nil means "no staleness signal", which
// pins every degraded summarize to the internal classifier
// `DegradedReasonSummariserUnavailable` (Stage 8.1 then
// normalises the wire reason to
// `embedding_index_unavailable` before Enforce). Production
// wiring runs a one-line `SELECT max(trained_at) FROM
// reranker_model WHERE status='published'` adapter.
func WithRerankerFreshness(r RerankerFreshnessSource) Option {
	return func(svc *Service) {
		svc.rerankerFreshness = r
	}
}

// WithSummariserTimeout overrides the per-call summariser
// budget. Defaults to `defaultSummariserTimeout` (5 s);
// values <= 0 are coerced to the default. Exported so the
// unit tests can drive the timeout path deterministically
// with a 1 ms budget; production should not need to tune
// this.
func WithSummariserTimeout(d time.Duration) Option {
	return func(svc *Service) {
		if d > 0 {
			svc.summariserTimeout = d
		}
	}
}

// Summarize implements the Stage 5.4 verb. See the package
// doc above for the full contract.
//
// Returns non-nil error only on caller-correctable
// validation failures (missing/ambiguous target, missing
// repo_id for concept, repo mismatch, target not found,
// service unconfigured) AND on parent-ctx cancellation
// (`context.Canceled`/`context.DeadlineExceeded` on the
// caller's deadline). EVERY OTHER FAILURE — including a
// timed-out summariser, a graph_store outage, or a busted
// freshness lookup — surfaces as a degraded envelope with
// `degraded=true` so the agent loop stays alive.
func (s *Service) Summarize(ctx context.Context, req SummarizeRequest) (resp SummarizeResponse, err error) {
	// Stage 8.1 — named returns + deferred wrap funnel every
	// successful exit through the closed-set contract helper.
	// On error we skip the helper (no metric / no overlay on
	// a 500). The helper shares the Recall/Expand Service's
	// `degradedMetric` + `faultInjector` fields wired via
	// `WithDegradedMetric` / `WithFaultInjector`.
	defer func() {
		if err != nil {
			return
		}
		resp, err = s.applySummarizeDegradedContract(req.RepoID, resp)
	}()

	// Validation runs FIRST so a malformed request against
	// a binary that hasn't wired the resolver (e.g. a stage
	// rollback) still surfaces as `InvalidArgument` (the
	// caller-correctable signal) rather than `Unimplemented`
	// — evaluator iter-2 #4. A well-formed-but-unwired
	// request still falls through to `ErrSummarizeUnconfigured`
	// → `Unimplemented`, signalling "this binary doesn't
	// ship Stage 5.4".
	if err := validateSummarizeRequest(req); err != nil {
		return SummarizeResponse{}, err
	}
	if s.neighborhood == nil {
		return SummarizeResponse{}, ErrSummarizeUnconfigured
	}
	req.MaxTokens = clampMaxTokens(req.MaxTokens)

	// Two distinct paths: node target vs concept target.
	// The shared post-resolution code (prompt build →
	// summariser call → citation build → context log append
	// → response shape) is factored into `runSummarize` so
	// neither branch duplicates it.
	if req.NodeID != "" {
		return s.summarizeNode(ctx, req)
	}
	return s.summarizeConcept(ctx, req)
}

// summarizeNode runs the node-target branch. The seed
// resolution doubles as the repo_id validation step: any
// repo mismatch the caller introduced surfaces as
// `ErrSummarizeRepoMismatch` *before* we burn an LLM call.
func (s *Service) summarizeNode(ctx context.Context, req SummarizeRequest) (SummarizeResponse, error) {
	neighborhood, err := s.neighborhood.NeighborhoodForNode(ctx, req.NodeID)
	if err != nil {
		return s.summarizeGraphFailure(ctx, req, targetKindNode, req.NodeID, err)
	}
	if neighborhood.Node.NodeID == "" {
		// Resolver returned an empty card without an error —
		// defensive: treat as not-found so the gRPC adapter
		// maps to NotFound instead of confusing the caller
		// with a zero-citation degraded envelope.
		return SummarizeResponse{}, fmt.Errorf("%w: node_id=%q",
			ErrSummarizeTargetNotFound, req.NodeID)
	}

	// Validate the repo_id rule for node targets.
	if req.RepoID != "" && neighborhood.Node.RepoID != "" &&
		req.RepoID != neighborhood.Node.RepoID {
		return SummarizeResponse{}, fmt.Errorf(
			"%w: req.repo_id=%q seed.repo_id=%q",
			ErrSummarizeRepoMismatch, req.RepoID, neighborhood.Node.RepoID)
	}
	if req.RepoID == "" {
		// Derive from the seed so the context log append has
		// a non-empty repo to scope against.
		req.RepoID = neighborhood.Node.RepoID
	}

	// Cap edge fan-out so the prompt + citations stay
	// bounded.
	cappedEdges := neighborhood.Edges
	if len(cappedEdges) > maxSummarizeEdges {
		cappedEdges = cappedEdges[:maxSummarizeEdges]
	}
	cappedTargets := deduplicatedTargets(neighborhood.Targets, neighborhood.Node.NodeID, cappedEdges)

	citations := buildNodeCitations(neighborhood.Node, cappedEdges, cappedTargets)

	prompt := renderNodePrompt(neighborhood.Node, cappedEdges, cappedTargets, req.MaxTokens)

	resp := SummarizeResponse{
		TargetKind: targetKindNode,
		TargetID:   neighborhood.Node.NodeID,
		Citations:  citations,
	}

	// Call summariser with the defence-in-depth 5s budget.
	summary, sErr := s.callSummariser(ctx, prompt, req.MaxTokens)
	if sErr != nil {
		// Parent-ctx cancellation surfaces hard; only
		// internal-budget exceedance + LLM errors degrade.
		if hardCancellation(ctx, sErr) {
			return SummarizeResponse{}, sErr
		}
		resp.SummaryMD = renderNodeTemplate(neighborhood.Node, cappedEdges, cappedTargets)
		resp.Degraded = true
		resp.DegradedReason = s.classifySummariserFailure(ctx)
		s.logger.Warn("agentapi.summarize.degraded",
			slog.String("target_kind", targetKindNode),
			slog.String("target_id", neighborhood.Node.NodeID),
			slog.String("reason", resp.DegradedReason),
			slog.String("err", sErr.Error()))
	} else {
		resp.SummaryMD = summary
	}

	s.appendSummarizeContextLog(ctx, req, &resp)
	return resp, nil
}

// summarizeConcept runs the concept-target branch.
func (s *Service) summarizeConcept(ctx context.Context, req SummarizeRequest) (SummarizeResponse, error) {
	concept, err := s.neighborhood.FetchConcept(ctx, req.ConceptID, req.RepoID)
	if err != nil {
		return s.summarizeGraphFailure(ctx, req, targetKindConcept, req.ConceptID, err)
	}
	if concept.ConceptID == "" {
		return SummarizeResponse{}, fmt.Errorf("%w: concept_id=%q",
			ErrSummarizeTargetNotFound, req.ConceptID)
	}

	// Cap supports so a concept that accumulated thousands
	// of `concept_support` rows over its lifetime cannot
	// blow the citation array or the prompt budget.
	cappedSupports := concept.Supports
	if len(cappedSupports) > maxSummarizeConceptSupports {
		cappedSupports = cappedSupports[:maxSummarizeConceptSupports]
	}

	citations := buildConceptCitations(concept, cappedSupports)

	prompt := renderConceptPrompt(concept, cappedSupports, req.MaxTokens)

	resp := SummarizeResponse{
		TargetKind: targetKindConcept,
		TargetID:   concept.ConceptID,
		Citations:  citations,
	}

	summary, sErr := s.callSummariser(ctx, prompt, req.MaxTokens)
	if sErr != nil {
		if hardCancellation(ctx, sErr) {
			return SummarizeResponse{}, sErr
		}
		resp.SummaryMD = renderConceptTemplate(concept, cappedSupports)
		resp.Degraded = true
		resp.DegradedReason = s.classifySummariserFailure(ctx)
		s.logger.Warn("agentapi.summarize.degraded",
			slog.String("target_kind", targetKindConcept),
			slog.String("target_id", concept.ConceptID),
			slog.String("reason", resp.DegradedReason),
			slog.String("err", sErr.Error()))
	} else {
		resp.SummaryMD = summary
	}

	s.appendSummarizeContextLog(ctx, req, &resp)
	return resp, nil
}

// summarizeGraphFailure handles the two error shapes the
// resolver can surface: target-not-found (caller-correctable
// → hard error) and graph_store_unavailable (degrade with
// empty template + no citations, per architecture §7.6).
//
// Any other resolver error is classified as Internal: the
// caller cannot correct it, but it's not a known degraded
// dependency either, so we want a hard error in operator
// dashboards rather than a silent degraded envelope.
func (s *Service) summarizeGraphFailure(
	ctx context.Context, req SummarizeRequest,
	targetKind, targetID string, cause error,
) (SummarizeResponse, error) {
	if errors.Is(cause, ErrSummarizeTargetNotFound) {
		return SummarizeResponse{}, fmt.Errorf("%w: %s_id=%q",
			ErrSummarizeTargetNotFound, targetKind, targetID)
	}
	if errors.Is(cause, ErrGraphStoreUnavailable) {
		// Degraded envelope: no neighborhood, no citations,
		// minimal template. Stage 5.4 / implementation-plan
		// §843-844 require a `recall_context_log(verb=
		// 'summarize')` row keyed by the returned
		// `context_id` for EVERY summarize call, including
		// the graph-outage path — evaluator iter-2 #3. We
		// unconditionally invoke the appender; the writer
		// adapter soft-fails on an unparseable / empty
		// RepoID (returning an empty ContextID) per the
		// existing recall-path semantics, so the audit
		// trail is best-effort but never silently skipped.
		resp := SummarizeResponse{
			TargetKind:     targetKind,
			TargetID:       targetID,
			Citations:      nil,
			SummaryMD:      renderGraphUnavailableTemplate(targetKind, targetID),
			Degraded:       true,
			DegradedReason: DegradedReasonGraphStoreUnavailable,
		}
		s.logger.Warn("agentapi.summarize.graph_unavailable",
			slog.String("target_kind", targetKind),
			slog.String("target_id", targetID),
			slog.String("err", cause.Error()))
		s.appendSummarizeContextLog(ctx, req, &resp)
		return resp, nil
	}
	// Surface unknown errors as hard failures so an
	// operator sees them instead of a silently-degraded
	// envelope.
	return SummarizeResponse{}, fmt.Errorf("agentapi: summarize: graph resolver: %w", cause)
}

// callSummariser invokes the wired Summariser under the
// defence-in-depth 5 s budget. Returns (output, nil) on
// success; (\"\", err) on any failure including:
//
//   - Nil summariser  → returns ErrSummariserUnavailable.
//   - Parent ctx done → returns ctx.Err() (caller signalled
//     cancellation; the verb's caller will check this with
//     `hardCancellation` to decide whether to degrade).
//   - Internal-budget exceeded → returns the timeout-derived
//     ctx.Err(). `hardCancellation` returns false because the
//     parent ctx is still healthy, so the verb degrades.
//   - Summariser error → returned verbatim.
//   - Summariser returned empty SummaryMD → returns
//     errSummariserEmpty so the verb degrades instead of
//     emitting a zero-byte summary on the wire.
func (s *Service) callSummariser(ctx context.Context, prompt string, maxTokens int) (string, error) {
	if s.summariser == nil {
		return "", errSummariserUnconfigured
	}
	// Honour the parent ctx's earlier deadline if any, OR
	// cap at the per-call 5 s budget — whichever fires first.
	// This is the load-bearing defence the Stage 5.4 brief
	// names ("summariser exceeds its 5 s deadline").
	budget := s.summariserTimeout
	if budget <= 0 {
		budget = defaultSummariserTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	out, err := s.summariser.Summarize(callCtx, SummariserInput{
		Prompt:    prompt,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(out.SummaryMD) == "" {
		return "", errSummariserEmpty
	}
	return out.SummaryMD, nil
}

// hardCancellation distinguishes parent-context termination
// (which MUST propagate as an error) from internal-budget
// expiry (which MUST degrade to the template fallback).
// Parent cancellation is detected by inspecting the
// CALLER's ctx, not the derived one used for the LLM call:
// if `ctx.Err()` is non-nil the caller is gone.
func hardCancellation(ctx context.Context, sErr error) bool {
	if ctx.Err() == nil {
		return false
	}
	// The caller's deadline expired OR they cancelled. Most
	// summariser errors that bubble out under this condition
	// will be `context.Canceled`/`DeadlineExceeded`, but we
	// also propagate any error in this case to honour the
	// caller's stop signal.
	_ = sErr
	return true
}

// classifySummariserFailure picks between the two
// degraded_reason values the Stage 5.4 brief allows on the
// summariser-failure path. The classifier consults the
// reranker freshness signal ONLY here (not on the happy
// path or the graph-outage path) so the cost is paid only
// when it matters.
//
// Errors / missing data fall through to
// DegradedReasonSummariserUnavailable per rubber-duck #8:
// the classifier itself going down must not change the
// surface behaviour.
func (s *Service) classifySummariserFailure(ctx context.Context) string {
	if s.rerankerFreshness == nil {
		return DegradedReasonSummariserUnavailable
	}
	trainedAt, ok, err := s.rerankerFreshness.LatestRerankerTrainedAt(ctx)
	if err != nil {
		s.logger.Warn("agentapi.summarize.freshness_lookup_failed",
			slog.String("err", err.Error()))
		return DegradedReasonSummariserUnavailable
	}
	if !ok {
		return DegradedReasonSummariserUnavailable
	}
	if time.Since(trainedAt) > rerankerStaleAfter {
		return DegradedReasonRerankerModelStale
	}
	return DegradedReasonSummariserUnavailable
}

// appendSummarizeContextLog appends one recall_context_log
// row with `Verb="summarize"`. Mirrors the recall handler's
// soft-failure semantics: any append error is logged at
// warn level and the response is returned with an empty
// ContextID, so a transient PostgreSQL hiccup cannot block
// the summarize response.
func (s *Service) appendSummarizeContextLog(ctx context.Context, req SummarizeRequest, resp *SummarizeResponse) {
	if s.contextLog == nil {
		return
	}
	in := s.buildSummarizeContextLogInput(req, resp)
	rec, err := s.contextLog.Append(ctx, in)
	if err != nil {
		s.logger.Warn("agentapi.summarize.context_log_append_failed",
			slog.String("repo_id", req.RepoID),
			slog.String("err", err.Error()))
		return
	}
	resp.ContextID = rec.ContextID
}

// buildSummarizeContextLogInput packs the request +
// citations onto the appender's input shape. We project
// citations onto NodeIDs/EdgeIDs/ConceptIDs grouped by the
// populated id field, preserving rank order within each
// group. `RerankerModelVersion` is the wired reranker's
// version (recall path's reranker dep) or the v0 cold-start
// literal — the field is REQUIRED non-empty by the
// recall_context_log writer so we always supply a sensible
// fallback.
func (s *Service) buildSummarizeContextLogInput(
	req SummarizeRequest, resp *SummarizeResponse,
) ContextLogInput {
	in := ContextLogInput{
		Verb:                 "summarize",
		RepoID:               req.RepoID,
		RerankerModelVersion: rerankerVersionForLog(s.reranker),
		ServedUnderDegraded:  resp.Degraded,
	}
	queryDoc := struct {
		NodeID    string `json:"node_id,omitempty"`
		ConceptID string `json:"concept_id,omitempty"`
		RepoID    string `json:"repo_id,omitempty"`
		MaxTokens int    `json:"max_tokens"`
	}{
		NodeID:    req.NodeID,
		ConceptID: req.ConceptID,
		RepoID:    req.RepoID,
		MaxTokens: req.MaxTokens,
	}
	if buf, err := json.Marshal(queryDoc); err == nil {
		in.QueryJSON = buf
	} else {
		in.QueryJSON = json.RawMessage(`{}`)
	}
	for _, c := range resp.Citations {
		switch {
		case c.NodeID != "":
			in.NodeIDs = append(in.NodeIDs, c.NodeID)
		case c.EdgeID != "":
			in.EdgeIDs = append(in.EdgeIDs, c.EdgeID)
		case c.ConceptID != "":
			in.ConceptIDs = append(in.ConceptIDs, c.ConceptID)
		}
	}
	return in
}

// rerankerVersionForLog selects the model version string we
// stamp on the recall_context_log row. The writer requires
// a non-empty value; we prefer the wired reranker's version
// when present, falling back to the v0 cold-start literal so
// a deployment with no reranker wired still produces a
// well-formed row.
func rerankerVersionForLog(r Reranker) string {
	if r != nil {
		if v := r.ModelVersion(); v != "" {
			return v
		}
	}
	return V0ModelVersion
}

// validateSummarizeRequest enforces the caller-correctable
// constraints. Runs BEFORE any I/O so a malformed request
// does not waste a graph round-trip.
func validateSummarizeRequest(req SummarizeRequest) error {
	hasNode := req.NodeID != ""
	hasConcept := req.ConceptID != ""
	switch {
	case !hasNode && !hasConcept:
		return ErrSummarizeMissingTarget
	case hasNode && hasConcept:
		return ErrSummarizeAmbiguousTarget
	}
	if hasConcept && req.RepoID == "" {
		return ErrSummarizeRepoIDRequired
	}
	return nil
}

// clampMaxTokens applies the documented "<=0 → default,
// >max → max" rule.
func clampMaxTokens(n int) int {
	if n <= 0 {
		return defaultSummarizeMaxTokens
	}
	if n > maxSummarizeMaxTokens {
		return maxSummarizeMaxTokens
	}
	return n
}

// deduplicatedTargets returns the unique destination Node
// cards for the supplied edge set, excluding the seed itself
// (a self-loop edge does NOT inflate the citation set with a
// duplicate of the seed). Order preserves first-occurrence
// in the input `targets` slice for snapshot-test stability;
// any DstNodeID present in the edges but missing from
// `targets` is intentionally dropped — the resolver owns the
// "real Node row" guarantee, and citing a stub would violate
// the "row that exists" invariant.
func deduplicatedTargets(
	targets []SummarizeNodeCard, seedNodeID string, edges []SummarizeEdgeCard,
) []SummarizeNodeCard {
	if len(targets) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(edges))
	for _, e := range edges {
		if e.DstNodeID == "" || e.DstNodeID == seedNodeID {
			continue
		}
		wanted[e.DstNodeID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(targets))
	out := make([]SummarizeNodeCard, 0, len(targets))
	for _, t := range targets {
		if t.NodeID == "" || t.NodeID == seedNodeID {
			continue
		}
		if _, ok := wanted[t.NodeID]; !ok {
			continue
		}
		if _, dup := seen[t.NodeID]; dup {
			continue
		}
		seen[t.NodeID] = struct{}{}
		out = append(out, t)
	}
	return out
}

// buildNodeCitations returns the citation list for a
// node-target summary. Order is:
//
//  1. The seed node itself (always first).
//  2. Each outbound edge in the input order.
//  3. Each unique destination node in the input order.
//
// By construction every entry is reachable from the seed in
// the structural graph (seed=0 hops, edges=1 hop, dsts=1
// hop). The Stage 5.4 acceptance scenario "summary cites
// resolved nodes" pins this property.
func buildNodeCitations(
	seed SummarizeNodeCard, edges []SummarizeEdgeCard, targets []SummarizeNodeCard,
) []Citation {
	out := make([]Citation, 0, 1+len(edges)+len(targets))
	out = append(out, Citation{
		NodeID:  seed.NodeID,
		Snippet: seed.CanonicalSignature,
	})
	for _, e := range edges {
		out = append(out, Citation{
			EdgeID:  e.EdgeID,
			Snippet: e.Kind,
		})
	}
	for _, t := range targets {
		out = append(out, Citation{
			NodeID:  t.NodeID,
			Snippet: t.CanonicalSignature,
		})
	}
	return out
}

// buildConceptCitations returns the citation list for a
// concept-target summary. Order is:
//
//  1. The concept itself (always first).
//  2. Each unique supporting Node (input order, deduped).
//  3. Each unique supporting Episode (input order,
//     deduped).
//
// The e2e contract (e2e-scenarios.md §"summarize → concept"
// acceptance) requires the response to cite the
// `concept_support` rows that anchor the concept to the
// structural / runtime graph. Each `Supports[*]` row
// contributes at most one Node citation AND at most one
// Episode citation (rows can carry either or both per the
// migration 0011 CHECK constraint).
func buildConceptCitations(
	concept SummarizeConceptCard, supports []SummarizeConceptSupport,
) []Citation {
	out := make([]Citation, 0, 1+2*len(supports))
	out = append(out, Citation{
		ConceptID: concept.ConceptID,
		Snippet:   concept.Name,
	})
	seenNode := make(map[string]struct{}, len(supports))
	seenEpisode := make(map[string]struct{}, len(supports))
	// Node citations first so a caller iterating ranks
	// "structural support" above "runtime support" (the
	// architecture's preference per §5.5.1 — structural
	// links are stable; episode links can churn with each
	// trace flush).
	for _, sup := range supports {
		if sup.NodeID == "" {
			continue
		}
		if _, dup := seenNode[sup.NodeID]; dup {
			continue
		}
		seenNode[sup.NodeID] = struct{}{}
		snippet := sup.NodeSignature
		if snippet == "" {
			snippet = sup.Polarity
		}
		out = append(out, Citation{
			NodeID:  sup.NodeID,
			Snippet: snippet,
		})
	}
	for _, sup := range supports {
		if sup.EpisodeID == "" {
			continue
		}
		if _, dup := seenEpisode[sup.EpisodeID]; dup {
			continue
		}
		seenEpisode[sup.EpisodeID] = struct{}{}
		out = append(out, Citation{
			EpisodeID: sup.EpisodeID,
			Snippet:   sup.Polarity,
		})
	}
	return out
}

// renderNodePrompt builds the Markdown prompt the LLM
// receives for a node-target summary. The structure is:
//
//	## Instruction
//	(verb-level guidance + max_tokens budget)
//
//	## Seed
//	- kind:     ...
//	- repo:     ...
//	- signature: ...
//
//	## Outbound edges (N)
//	- {kind}: {src_signature} → {dst_signature} (observed N×)
//	- ...
//
//	## Reachable destinations (N)
//	- {kind}: {signature}
//
// The `## Instruction` block is the only one the LLM should
// follow as instructions; everything below it is data,
// clearly delimited by `## Section` headings so a future
// prompt-injection guard can mechanically split data from
// instructions (rubber-duck #7).
func renderNodePrompt(
	seed SummarizeNodeCard, edges []SummarizeEdgeCard, targets []SummarizeNodeCard, maxTokens int,
) string {
	var b strings.Builder
	b.WriteString("## Instruction\n")
	fmt.Fprintf(&b,
		"Produce a concise Markdown summary (≤ %d tokens) of the code-graph "+
			"neighborhood described below. Describe what the seed entity does and how it "+
			"connects to its outbound destinations. Respond with Markdown only; do not "+
			"echo identifiers verbatim.\n\n", maxTokens)
	b.WriteString("## Seed\n")
	fmt.Fprintf(&b, "- kind: %s\n", emptyAsDash(seed.Kind))
	fmt.Fprintf(&b, "- repo: %s\n", emptyAsDash(seed.RepoID))
	fmt.Fprintf(&b, "- signature: %s\n\n", emptyAsDash(seed.CanonicalSignature))
	fmt.Fprintf(&b, "## Outbound edges (%d)\n", len(edges))
	if len(edges) == 0 {
		b.WriteString("- (none)\n")
	}
	for _, e := range edges {
		dst := emptyAsDash(e.DstSignature)
		if e.ObservationCount > 0 {
			fmt.Fprintf(&b, "- %s: %s → %s (observed %d×)\n",
				emptyAsDash(e.Kind), emptyAsDash(seed.CanonicalSignature), dst, e.ObservationCount)
		} else {
			fmt.Fprintf(&b, "- %s: %s → %s\n",
				emptyAsDash(e.Kind), emptyAsDash(seed.CanonicalSignature), dst)
		}
	}
	fmt.Fprintf(&b, "\n## Reachable destinations (%d)\n", len(targets))
	if len(targets) == 0 {
		b.WriteString("- (none)\n")
	}
	for _, t := range targets {
		fmt.Fprintf(&b, "- %s: %s\n",
			emptyAsDash(t.Kind), emptyAsDash(t.CanonicalSignature))
	}
	return b.String()
}

// renderNodeTemplate is the deterministic fallback rendered
// when the summariser is unavailable / timed out. It mirrors
// the prompt's data sections (seed, edges, destinations) so
// the agent caller receives a faithful structural summary
// even when the LLM is down. The brief mandates "templated
// summary built from canonical signatures" (plural) — the
// dst signatures appear here when available.
func renderNodeTemplate(
	seed SummarizeNodeCard, edges []SummarizeEdgeCard, targets []SummarizeNodeCard,
) string {
	var b strings.Builder
	b.WriteString("**Summariser unavailable — templated fallback.**\n\n")
	fmt.Fprintf(&b, "## %s\n", emptyAsDash(seed.CanonicalSignature))
	fmt.Fprintf(&b, "Kind: `%s` · Repo: `%s`\n\n",
		emptyAsDash(seed.Kind), emptyAsDash(seed.RepoID))
	if len(edges) > 0 {
		fmt.Fprintf(&b, "### Outbound edges (%d)\n", len(edges))
		for _, e := range edges {
			dst := emptyAsDash(e.DstSignature)
			if e.ObservationCount > 0 {
				fmt.Fprintf(&b, "- **%s** → `%s` (observed %d×)\n",
					emptyAsDash(e.Kind), dst, e.ObservationCount)
			} else {
				fmt.Fprintf(&b, "- **%s** → `%s`\n",
					emptyAsDash(e.Kind), dst)
			}
		}
		b.WriteString("\n")
	}
	if len(targets) > 0 {
		fmt.Fprintf(&b, "### Reachable destinations (%d)\n", len(targets))
		for _, t := range targets {
			fmt.Fprintf(&b, "- `%s` (`%s`)\n",
				emptyAsDash(t.CanonicalSignature), emptyAsDash(t.Kind))
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// renderConceptPrompt is the concept-target counterpart to
// renderNodePrompt. When supporting Nodes / Episodes are
// available they appear under a `## Supports` section so
// the LLM can ground the summary in concrete provenance.
func renderConceptPrompt(c SummarizeConceptCard, supports []SummarizeConceptSupport, maxTokens int) string {
	var b strings.Builder
	b.WriteString("## Instruction\n")
	fmt.Fprintf(&b,
		"Produce a concise Markdown summary (≤ %d tokens) of the concept described below. "+
			"Use the concept name as the heading; the description carries the prior. "+
			"When supporting Nodes / Episodes are listed, ground the summary in them. "+
			"Respond with Markdown only.\n\n", maxTokens)
	b.WriteString("## Concept\n")
	fmt.Fprintf(&b, "- name: %s\n", emptyAsDash(c.Name))
	if c.DescriptionMD != "" {
		b.WriteString("- description: |\n")
		for _, line := range strings.Split(c.DescriptionMD, "\n") {
			b.WriteString("    ")
			b.WriteString(line)
			b.WriteString("\n")
		}
	}
	if len(supports) > 0 {
		fmt.Fprintf(&b, "\n## Supports (%d)\n", len(supports))
		for _, sup := range supports {
			switch {
			case sup.NodeID != "":
				fmt.Fprintf(&b, "- node: %s (%s) [%s]\n",
					emptyAsDash(sup.NodeSignature),
					emptyAsDash(sup.NodeKind),
					emptyAsDash(sup.Polarity))
			case sup.EpisodeID != "":
				fmt.Fprintf(&b, "- episode: %s [%s]\n",
					sup.EpisodeID, emptyAsDash(sup.Polarity))
			}
		}
	}
	return b.String()
}

// renderConceptTemplate is the deterministic fallback for
// concept targets. Includes a `### Supports` section when
// the resolver returned support rows so the agent caller
// receives a faithful provenance list even when the LLM is
// down.
func renderConceptTemplate(c SummarizeConceptCard, supports []SummarizeConceptSupport) string {
	var b strings.Builder
	b.WriteString("**Summariser unavailable — templated fallback.**\n\n")
	fmt.Fprintf(&b, "## %s\n", emptyAsDash(c.Name))
	if c.DescriptionMD != "" {
		b.WriteString(c.DescriptionMD)
		if !strings.HasSuffix(c.DescriptionMD, "\n") {
			b.WriteString("\n")
		}
	}
	if len(supports) > 0 {
		fmt.Fprintf(&b, "\n### Supports (%d)\n", len(supports))
		for _, sup := range supports {
			switch {
			case sup.NodeID != "":
				fmt.Fprintf(&b, "- **node** `%s` (`%s`) [%s]\n",
					emptyAsDash(sup.NodeSignature),
					emptyAsDash(sup.NodeKind),
					emptyAsDash(sup.Polarity))
			case sup.EpisodeID != "":
				fmt.Fprintf(&b, "- **episode** `%s` [%s]\n",
					sup.EpisodeID, emptyAsDash(sup.Polarity))
			}
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// renderGraphUnavailableTemplate is the minimal fallback
// when the graph store outage prevented even the seed
// lookup. We do not have a canonical signature to render,
// so the body is a single line naming the target id + the
// outage reason.
func renderGraphUnavailableTemplate(targetKind, targetID string) string {
	return fmt.Sprintf("**Summary unavailable — graph store unreachable.**\n\n"+
		"Target: `%s/%s`. Retry once `degraded_reason=%s` clears.\n",
		targetKind, targetID, DegradedReasonGraphStoreUnavailable)
}

// emptyAsDash renders a placeholder for missing string
// fields so the prompt + template stay readable even when a
// resolver returned partial data.
func emptyAsDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// errSummariserUnconfigured / errSummariserEmpty are the
// internal sentinels callSummariser surfaces when the
// summariser is missing / returned an empty response. Not
// exported because the only caller is the verb's own
// fallback classifier — external callers never need to
// pattern-match against these.
var (
	errSummariserUnconfigured = errors.New("agentapi: summarize: no summariser configured")
	errSummariserEmpty        = errors.New("agentapi: summarize: summariser returned empty SummaryMD")
)
