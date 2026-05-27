// Package scenarios owns the per-verb workload generators
// the Stage 8.4 load-test calibration harness composes into a
// single 30-minute run.
//
// Each Scenario drives ONE verb at ONE sustained RPS for the
// harness's configured duration. Scenarios are independent —
// the harness spawns one open-loop driver per scenario and
// aggregates their samples into a single calibration.Report.
//
// Why thin AgentClient / ManagementClient interfaces (not the
// generated gRPC client types directly):
//   - Unit tests inject a deterministic fake without importing
//     the heavy `google.golang.org/grpc` or the generated
//     `proto/agent` (agentpb) packages.
//   - The production binary wires a tiny adapter that
//     translates `agentpb.AgentServiceClient` ⇄
//     [AgentClient] (see `cmd/loadtest-harness/agent_adapter.go`).
//   - The harness reasons about domain-typed requests / responses
//     (a `RecallResponse.NodeIDs` slice instead of a
//     `*agentpb.RecallResponse` with `NodeCard` proto values).
package scenarios

import (
	"context"
	"time"
)

// AgentClient is the slice of the AgentService surface the
// calibration harness drives. All methods MUST be safe to call
// from many goroutines concurrently — the open-loop scheduler
// fires up to [Scenario]-defined in-flight requests in
// parallel.
type AgentClient interface {
	Recall(ctx context.Context, req RecallRequest) (RecallResponse, error)
	Observe(ctx context.Context, req ObserveRequest) (ObserveResponse, error)
	Expand(ctx context.Context, req ExpandRequest) (ExpandResponse, error)
	Summarize(ctx context.Context, req SummarizeRequest) (SummarizeResponse, error)
}

// ManagementClient is the slice of the Management surface the
// harness drives (currently `mgmt.ingest_spans`).
type ManagementClient interface {
	IngestSpans(ctx context.Context, req IngestSpansRequest) (IngestSpansResponse, error)
}

// RecallRequest mirrors the `agent.v1.RecallRequest` envelope
// the AgentService Recall RPC accepts. The harness fills in
// RepoID + Query (from the LabeledQuery rotation) and K (from
// the LearningQualityTargets pin); Kinds may be empty (the
// server then uses its default kind set).
type RecallRequest struct {
	RepoID string
	Query  string
	K      int
	Kinds  []string
}

// RecallResponse mirrors the `agent.v1.RecallResponse` slice
// the harness reasons about. NodeIDs is the order-preserved
// list of node ids in the response (used to compute the
// labelled-query rank-of-correct-node); ConceptIDs is the
// order-preserved list of concept ids (used to compute the
// concept-hit fraction). ContextID lets the harness optionally
// pipe a real recall into Observe; the calibration harness
// does NOT do this (Observe uses a synthetic context_id pool to
// keep the verbs independent), but the field is captured so
// downstream tooling can.
type RecallResponse struct {
	ContextID  string
	NodeIDs    []string
	ConceptIDs []string
	Degraded   bool
}

// ObserveRequest is the slice of `agent.v1.ObserveRequest` the
// harness sends. ContextID comes from the
// [Config.SyntheticContextIDs] pool; the remaining fields are
// synthetic so the wire shape is well-formed (the AgentService
// rejects unknown closed-set values per §C15 / §C23).
type ObserveRequest struct {
	ContextID  string
	RepoID     string
	SessionID  string
	TraceID    string
	Outcome    string // closed-set: "success" | "failure" | "refused" | "degraded"
	ActionJSON []byte
}

// ObserveResponse mirrors the wire shape we care about.
type ObserveResponse struct {
	EpisodeID string
	Degraded  bool
}

// ExpandRequest drives `agent.v1.ExpandRequest`. The harness
// rotates through a fixture seed-node pool and varies depth in
// a deterministic round-robin to stress the BFS.
type ExpandRequest struct {
	NodeID    string
	Direction string // "callees" | "callers"
	Depth     int32
	RepoID    string
	MaxNodes  int32
	MaxEdges  int32
}

// ExpandResponse mirrors the slice we care about.
type ExpandResponse struct {
	ContextID  string
	RootNodeID string
	NodeIDs    []string
	EdgeIDs    []string
	Truncated  bool
	Degraded   bool
}

// SummarizeRequest drives `agent.v1.SummarizeRequest`. The
// harness alternates `NodeID` and `ConceptID` so both
// summarise paths get coverage.
type SummarizeRequest struct {
	NodeID    string
	ConceptID string
	RepoID    string
	MaxTokens int32
}

// SummarizeResponse mirrors the slice we care about.
type SummarizeResponse struct {
	ContextID  string
	TargetKind string // "node" | "concept"
	TargetID   string
	SummaryMD  string
	Degraded   bool
}

// IngestSpansRequest drives the mgmt-api `POST /v1/spans`
// endpoint. The harness emits a small synthetic batch (one
// ResourceSpan with a handful of Spans); production calibration
// runs swap in a fixture loader that replays real OTel spans.
type IngestSpansRequest struct {
	RepoID    string
	BatchJSON []byte
}

// IngestSpansResponse mirrors the wire response we care about.
type IngestSpansResponse struct {
	Accepted int
	Rejected int
	// Degraded mirrors the mgmt-api's `degraded` wire flag
	// (`internal/mgmtapi.SpanIngestResponse.Degraded`). When
	// true the mgmt-api accepted the batch but in a degraded
	// state (e.g. backpressure, partial-store). The harness
	// surfaces it onto `Sample.Degraded` so the artifact's
	// "degraded responses" note captures elevated mgmt
	// backpressure.
	Degraded bool
	// DegradedReason is the operator-readable reason that
	// accompanies a `Degraded=true` response. The
	// `GraphIngestScenario.Execute` setter copies it onto
	// `Sample.DegradedReason`; the harness aggregator
	// (`harness.go::aggregator.degradedReasons`) collects
	// per-verb reason counts and the note emitter renders
	// them onto the artifact's degraded-responses note line
	// when non-empty.
	DegradedReason string
}

// Sample is the per-tick output every Scenario.Execute call
// produces. The harness collects samples into per-verb
// LatencyRecorders and learning-quality buckets.
type Sample struct {
	// Verb is the canonical verb name (matches
	// `reliability.VerbProfile.Verb`).
	Verb string

	// Started + Finished frame the wall-clock execution
	// window. Harness uses Finished-Started for the latency
	// observation.
	Started  time.Time
	Finished time.Time

	// Err is the non-nil failure that promotes this sample
	// into the per-verb Failed count. context.Canceled /
	// context.DeadlineExceeded count as failures (the
	// harness reports them; the operator decides whether to
	// re-run).
	Err error

	// RecallRank is the 1-based rank of the LabeledQuery's
	// expected_node_id in the recall response. Zero means
	// the expected node was NOT in the response. Inspect
	// [Sample.RankMeasured] to distinguish "scenario did not
	// measure rank" (RankMeasured=false, RecallRank ignored)
	// from "scenario measured rank and the expected node was
	// not found in the top-K" (RankMeasured=true, RecallRank=0
	// which the harness buckets into the worst-rank slot K+1).
	RecallRank int

	// RankMeasured is true when the scenario actually
	// attempted a rank measurement (i.e. the LabeledQuery
	// had a non-empty ExpectedNodeID). Distinguishes "not
	// measured" from "measured and missed".
	RankMeasured bool

	// ConceptHit is true when the recall scenario's
	// LabeledQuery had non-empty ExpectedConceptIDs AND any
	// of them appeared in `RecallResponse.ConceptIDs`.
	// Independent of RecallRank.
	ConceptHit bool

	// ConceptHitMeasured is true when the scenario emitted a
	// concept-hit measurement at all (i.e. the LabeledQuery
	// had at least one ExpectedConceptID). Distinguishes
	// "not measured" from "measured and false".
	ConceptHitMeasured bool

	// Degraded captures the wire-level `degraded=true` flag
	// from the response. Not a budget breach in itself (the
	// recall response is still served per §C22), but
	// surfaced in the artifact via Notes when the ratio is
	// high.
	Degraded bool

	// DegradedReason is the operator-readable reason that
	// accompanies a `Degraded=true` response (e.g.
	// "writer_backpressure", "qdrant_unreachable"). Optional;
	// scenarios whose response shape does not carry a reason
	// (today: agent.recall / observe / expand / summarize)
	// leave this empty and the harness's degraded-note
	// emitter falls back to a boolean-only note. The
	// mgmt.ingest_spans scenario populates it from
	// `IngestSpansResponse.DegradedReason`; the harness
	// aggregator collects a top-reason histogram and renders
	// it onto the artifact's degraded-responses note so an
	// operator can spot backpressure modes without scraping
	// the raw mgmt-api logs.
	DegradedReason string
}

// Latency is the per-sample latency observation. Zero when
// Err is non-nil and the scenario stopped before completing.
func (s Sample) Latency() time.Duration {
	if s.Finished.IsZero() || s.Started.IsZero() {
		return 0
	}
	return s.Finished.Sub(s.Started)
}

// RNG is the minimal random-number source a Scenario needs.
// Wrapping math/rand.Rand behind an interface lets the harness
// inject a deterministic source from a single seed and lets
// tests freeze the order.
type RNG interface {
	Intn(n int) int
}

// Scenario is the contract every per-verb workload satisfies.
// The harness owns timing; the Scenario only knows how to
// shape one request and decode one response.
type Scenario interface {
	// Verb is the canonical name; matches the
	// reliability.VerbProfile this scenario aligns with.
	Verb() string

	// Execute fires ONE request and returns the Sample. The
	// scenario MUST stamp Started before the wire call and
	// Finished immediately after, even on error, so the
	// harness can report wall-clock latency on failed
	// requests as well as successful ones. The scenario MUST
	// NOT panic on a nil or returned-error client — failed
	// requests are normal operating mode at the §8.3 budget.
	Execute(ctx context.Context, rng RNG) Sample
}
