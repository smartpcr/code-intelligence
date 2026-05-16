// Package agentapi: gRPC server adapter for the §6.4 recall
// verb.
//
// Stage 5.1 mandates the gRPC service skeleton in
// `proto/agent.proto` actually be CALLABLE (evaluator iter-1
// #1: "The gRPC service is not actually registered or
// callable"). This file owns the translation between the
// `agentpb.AgentService` surface and the in-process
// `*agentapi.Service`. The binary composition root
// (`cmd/agent-api/main.go`) constructs both and registers
// the server on the running `*grpc.Server`.
//
// Why a separate adapter (not pollute Service)
// --------------------------------------------
// `Service` is the in-process domain object — it has no
// business owning protobuf types or status codes. The
// adapter:
//
//   - Decouples the wire shape from the domain shape: a
//     proto-only field (e.g. `repo_id` validation) is
//     enforced here, while domain-only fields (e.g. the
//     deprecated `Kind` alias) stay where the legacy
//     callers live.
//   - Keeps the agentapi package free of `google.golang.org/grpc`
//     beyond this file, so test packages that only need the
//     domain Service do not pull in the gRPC dependency.
//   - Maps domain errors onto status codes in ONE place so
//     two engineers cannot drift the mapping.
//
// Error mapping (`status.Code`)
// -----------------------------
//   - `ErrEmptyQuery` / `ErrInvalidKind` / `ErrInvalidK`
//     → `codes.InvalidArgument` (caller-correctable).
//   - Any other error from `Service.Recall` → `codes.Internal`
//     (server-side fault; the recall handler degrades to a
//     snapshot internally and only returns a hard error when
//     the snapshot fallback itself is unwired).
//   - `Observe` / `Expand` → `codes.Unimplemented`
//     (the placeholder body shapes belong to Stages 5.2 /
//     5.3). The embedded `UnimplementedAgentServiceServer`
//     already returns these codes, but we re-export tiny
//     stubs below so future implementers know exactly
//     where to plug in.
//   - `Summarize` → mapped per `summarizeErrorToStatus`
//     (Stage 5.4). Missing/ambiguous target, repo_id
//     requirements, repo mismatch, max_tokens range →
//     `codes.InvalidArgument`. Target not found →
//     `codes.NotFound`. `ErrSummarizeUnconfigured` →
//     `codes.Unimplemented` so a partially-wired binary
//     surfaces the same signal as a binary that hasn't
//     deployed Stage 5.4 yet. Anything else →
//     `codes.Internal`.
package agentapi

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/smartpcr/code-intelligence/services/agent-memory/proto/agent"
)

// GRPCServer adapts `*Service` onto `agentpb.AgentServiceServer`.
// Construct with `NewGRPCServer`; register on a
// `*grpc.Server` with `agentpb.RegisterAgentServiceServer`.
type GRPCServer struct {
	// UnimplementedAgentServiceServer satisfies the gRPC
	// forward-compatibility requirement: future verbs added
	// to the proto file will not break the build because
	// this embedded value returns
	// `codes.Unimplemented` for any unhandled method.
	agentpb.UnimplementedAgentServiceServer

	svc *Service
}

// NewGRPCServer constructs the gRPC adapter. A nil `svc`
// panics — the binary composition root MUST wire a real
// service, and a half-wired server is worse than a fail-fast
// crash.
func NewGRPCServer(svc *Service) *GRPCServer {
	if svc == nil {
		panic("agentapi: NewGRPCServer: nil *Service")
	}
	return &GRPCServer{svc: svc}
}

// Recall implements `agentpb.AgentServiceServer.Recall`.
// Translates the proto request shape into a `RecallRequest`,
// invokes `Service.Recall`, then projects the response onto
// `agentpb.RecallResponse`.
func (g *GRPCServer) Recall(ctx context.Context, req *agentpb.RecallRequest) (*agentpb.RecallResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "agent.recall: nil request")
	}
	in := RecallRequest{
		Query:  req.GetQuery(),
		Kinds:  req.GetKinds(),
		RepoID: req.GetRepoId(),
		K:      int(req.GetK()),
	}
	resp, err := g.svc.Recall(ctx, in)
	if err != nil {
		return nil, recallErrorToStatus(err)
	}

	out := &agentpb.RecallResponse{
		ContextId:            resp.ContextID,
		RerankerModelVersion: resp.RerankerModelVersion,
		Degraded:             resp.Degraded,
		DegradedReason:       resp.DegradedReason,
		OverFetched:          int32(clampInt32(resp.OverFetched)),
		Filtered:             int32(clampInt32(resp.Filtered)),
	}
	out.Nodes = make([]*agentpb.NodeCard, 0, len(resp.Nodes))
	for _, n := range resp.Nodes {
		out.Nodes = append(out.Nodes, &agentpb.NodeCard{
			NodeId:             n.NodeID,
			RepoId:             n.RepoID,
			Kind:               n.Kind,
			CanonicalSignature: n.CanonicalSignature,
			Score:              n.Score,
			PointId:            n.PointID,
		})
	}
	out.Edges = make([]*agentpb.EdgeCard, 0, len(resp.Edges))
	for _, e := range resp.Edges {
		out.Edges = append(out.Edges, &agentpb.EdgeCard{
			EdgeId:           e.EdgeID,
			RepoId:           e.RepoID,
			Kind:             e.Kind,
			SrcNodeId:        e.SrcNodeID,
			DstNodeId:        e.DstNodeID,
			ObservationCount: e.ObservationCount,
		})
	}
	out.Concepts = make([]*agentpb.ConceptCard, 0, len(resp.Concepts))
	for _, c := range resp.Concepts {
		out.Concepts = append(out.Concepts, &agentpb.ConceptCard{
			ConceptId: c.ConceptID,
			Name:      c.Name,
			Score:     c.Score,
			PointId:   c.PointID,
		})
	}
	return out, nil
}

// recallErrorToStatus maps domain errors from `Service.Recall`
// onto the gRPC status codes the agent caller can pattern-
// match against. Centralised here so adding a new domain
// sentinel does not silently drop to `codes.Unknown`.
func recallErrorToStatus(err error) error {
	switch {
	case errors.Is(err, ErrEmptyQuery):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrInvalidKind):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrInvalidK):
		return status.Error(codes.InvalidArgument, err.Error())
	}
	// Hard failures — the recall handler internally
	// degrades to a snapshot on dependency outages, so any
	// error reaching here is either a misconfigured binary
	// (no snapshot wired) or an embedder failure.
	return status.Error(codes.Internal, fmt.Sprintf("agent.recall: %v", err))
}

// clampInt32 saturates `n` into the int32 range. Used for
// the observability counters where a future repo with >2B
// candidates is mathematically possible but operationally
// absurd; we'd rather silently cap than overflow.
func clampInt32(n int) int {
	const maxInt32 = int(int32(^uint32(0) >> 1))
	if n > maxInt32 {
		return maxInt32
	}
	if n < 0 {
		return 0
	}
	return n
}

// Summarize implements `agentpb.AgentServiceServer.Summarize`.
// Translates the proto request shape into a
// `SummarizeRequest`, invokes `Service.Summarize`, then
// projects the response onto `agentpb.SummarizeResponse`.
//
// Stage 5.4 status-code policy (mirrors `recallErrorToStatus`):
//
//   - nil request                     → `codes.InvalidArgument`
//   - `ErrSummarizeMissingTarget`     → `codes.InvalidArgument`
//   - `ErrSummarizeAmbiguousTarget`   → `codes.InvalidArgument`
//   - `ErrSummarizeRepoIDRequired`    → `codes.InvalidArgument`
//   - `ErrSummarizeRepoMismatch`      → `codes.InvalidArgument`
//   - `ErrSummarizeMaxTokensRange`    → `codes.InvalidArgument`
//   - `ErrSummarizeTargetNotFound`    → `codes.NotFound`
//   - `ErrSummarizeUnconfigured`      → `codes.Unimplemented`
//   - parent ctx cancelled/expired    → `codes.Canceled` /
//                                       `codes.DeadlineExceeded`
//   - anything else                   → `codes.Internal`
func (g *GRPCServer) Summarize(ctx context.Context, req *agentpb.SummarizeRequest) (*agentpb.SummarizeResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "agent.summarize: nil request")
	}
	in := SummarizeRequest{
		NodeID:    req.GetNodeId(),
		ConceptID: req.GetConceptId(),
		RepoID:    req.GetRepoId(),
		MaxTokens: int(req.GetMaxTokens()),
	}
	resp, err := g.svc.Summarize(ctx, in)
	if err != nil {
		return nil, summarizeErrorToStatus(err)
	}
	out := &agentpb.SummarizeResponse{
		SummaryMd:      resp.SummaryMD,
		ContextId:      resp.ContextID,
		Degraded:       resp.Degraded,
		DegradedReason: resp.DegradedReason,
		TargetKind:     resp.TargetKind,
		TargetId:       resp.TargetID,
	}
	if len(resp.Citations) > 0 {
		out.Citations = make([]*agentpb.Citation, 0, len(resp.Citations))
		for _, c := range resp.Citations {
			out.Citations = append(out.Citations, &agentpb.Citation{
				NodeId:    c.NodeID,
				EdgeId:    c.EdgeID,
				ConceptId: c.ConceptID,
				EpisodeId: c.EpisodeID,
				Snippet:   c.Snippet,
			})
		}
	}
	return out, nil
}

// summarizeErrorToStatus maps the Stage 5.4 sentinels +
// caller-cancellation signals onto gRPC status codes.
// Centralised here so adding a new sentinel does not
// silently drop to `codes.Unknown`.
func summarizeErrorToStatus(err error) error {
	switch {
	case errors.Is(err, ErrSummarizeMissingTarget),
		errors.Is(err, ErrSummarizeAmbiguousTarget),
		errors.Is(err, ErrSummarizeRepoIDRequired),
		errors.Is(err, ErrSummarizeRepoMismatch),
		errors.Is(err, ErrSummarizeMaxTokensRange):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrSummarizeTargetNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, ErrSummarizeUnconfigured):
		return status.Error(codes.Unimplemented, err.Error())
	case errors.Is(err, context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	}
	return status.Error(codes.Internal, fmt.Sprintf("agent.summarize: %v", err))
}
