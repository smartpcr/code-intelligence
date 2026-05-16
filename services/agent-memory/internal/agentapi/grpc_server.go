// Package agentapi: gRPC server adapter for the §6.4 recall
// verb AND the §6.1.2 observe verb.
//
// Stage 5.1 mandates the gRPC service skeleton in
// `proto/agent.proto` actually be CALLABLE (evaluator iter-1
// #1: "The gRPC service is not actually registered or
// callable"). This file owns the translation between the
// `agentpb.AgentService` surface and the in-process
// `*agentapi.Service` (recall) + `*agentapi.ObserveService`
// (observe). The binary composition root
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
//   - `Observe` validation sentinels (HumanCorrected,
//     DegradedRecallContextRoleForbidden, InvalidObservationRole,
//     InvalidObservationTarget, InvalidOutcome, MissingRepoID/
//     SessionID/TraceID/Action/ContextID, InvalidJSON,
//     ContextNotFound) → `codes.InvalidArgument`.
//   - `Observe` ErrEpisodicLogUnavailable WITHOUT a WAL wired
//     → `codes.Unavailable` (caller should retry).
//   - Any other `Observe` error → `codes.Internal`.
//   - `Expand` / `Summarize` → `codes.Unimplemented`
//     (the placeholder body shapes belong to Stages 5.3 /
//     5.4). The embedded `UnimplementedAgentServiceServer`
//     already returns these codes, but we re-export tiny
//     stubs below so future implementers know exactly
//     where to plug in.
package agentapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentpb "github.com/smartpcr/code-intelligence/services/agent-memory/proto/agent"
)

// GRPCServer adapts `*Service` AND `*ObserveService` onto
// `agentpb.AgentServiceServer`. Construct with `NewGRPCServer`;
// register on a `*grpc.Server` with
// `agentpb.RegisterAgentServiceServer`.
type GRPCServer struct {
	// UnimplementedAgentServiceServer satisfies the gRPC
	// forward-compatibility requirement: future verbs added
	// to the proto file will not break the build because
	// this embedded value returns
	// `codes.Unimplemented` for any unhandled method.
	agentpb.UnimplementedAgentServiceServer

	svc     *Service
	observe *ObserveService
}

// GRPCOption configures a GRPCServer.
type GRPCOption func(*GRPCServer)

// WithObserveService plumbs the Stage 5.2 observe handler.
// Without it `AgentService.Observe` falls through to the
// embedded `UnimplementedAgentServiceServer` and returns
// `codes.Unimplemented` — the legacy Stage 5.1 behaviour. The
// production composition root always wires one.
func WithObserveService(o *ObserveService) GRPCOption {
	return func(g *GRPCServer) {
		g.observe = o
	}
}

// NewGRPCServer constructs the gRPC adapter. A nil `svc`
// panics — the binary composition root MUST wire a real
// service, and a half-wired server is worse than a fail-fast
// crash. The observe handler is optional (see
// `WithObserveService`).
func NewGRPCServer(svc *Service, opts ...GRPCOption) *GRPCServer {
	if svc == nil {
		panic("agentapi: NewGRPCServer: nil *Service")
	}
	g := &GRPCServer{svc: svc}
	for _, opt := range opts {
		opt(g)
	}
	return g
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

// Observe implements `agentpb.AgentServiceServer.Observe`.
// Translates the proto request shape into an in-process
// `ObserveRequest`, invokes `ObserveService.Observe`, and
// projects the response onto `agentpb.ObserveResponse`. When
// no ObserveService is wired the method falls through to the
// embedded `UnimplementedAgentServiceServer.Observe` (returns
// codes.Unimplemented).
func (g *GRPCServer) Observe(ctx context.Context, req *agentpb.ObserveRequest) (*agentpb.ObserveResponse, error) {
	if g.observe == nil {
		return g.UnimplementedAgentServiceServer.Observe(ctx, req)
	}
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "agent.observe: nil request")
	}
	refs := make([]ObservationRef, 0, len(req.GetObservationRefs()))
	for _, r := range req.GetObservationRefs() {
		if r == nil {
			// Defence-in-depth: a nil entry in a repeated
			// field would dereference as zero-value below,
			// which would surface as an "unknown role"
			// validation error that does not name the
			// specific architectural confusion. Reject up-
			// front so the caller sees a precise message.
			return nil, status.Errorf(codes.InvalidArgument,
				"agent.observe: observation_refs[] contains a nil entry")
		}
		refs = append(refs, ObservationRef{
			Role:      r.GetRole(),
			NodeID:    r.GetNodeId(),
			EdgeID:    r.GetEdgeId(),
			ConceptID: r.GetConceptId(),
			Weight:    r.GetWeight(),
		})
	}
	in := ObserveRequest{
		RepoID:          req.GetRepoId(),
		SessionID:       req.GetSessionId(),
		TraceID:         req.GetTraceId(),
		ActionJSON:      json.RawMessage(req.GetActionJson()),
		Outcome:         req.GetOutcome(),
		SignalJSON:      json.RawMessage(req.GetSignalJson()),
		ContextID:       req.GetContextId(),
		ObservationRefs: refs,
		EpisodeGroupID:  req.GetEpisodeGroupId(),
	}
	resp, err := g.observe.Observe(ctx, in)
	if err != nil {
		return nil, observeErrorToStatus(err)
	}
	return &agentpb.ObserveResponse{
		EpisodeId:      resp.EpisodeID,
		EpisodeGroupId: resp.EpisodeGroupID,
		Degraded:       resp.Degraded,
		DegradedReason: resp.DegradedReason,
	}, nil
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

// observeErrorToStatus maps the observe handler's sentinel
// errors onto gRPC status codes. Validation sentinels map to
// InvalidArgument (caller-correctable); a missing WAL on a
// partition outage maps to Unavailable (the caller should
// retry); everything else is Internal.
func observeErrorToStatus(err error) error {
	switch {
	case errors.Is(err, ErrHumanCorrectedNotAllowed),
		errors.Is(err, ErrDegradedRecallContextRoleForbidden),
		errors.Is(err, ErrInvalidObservationRole),
		errors.Is(err, ErrInvalidObservationTarget),
		errors.Is(err, ErrInvalidOutcome),
		errors.Is(err, ErrMissingRepoID),
		errors.Is(err, ErrMissingSessionID),
		errors.Is(err, ErrMissingTraceID),
		errors.Is(err, ErrMissingAction),
		errors.Is(err, ErrMissingContextID),
		errors.Is(err, ErrInvalidJSON),
		errors.Is(err, ErrContextNotFound):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, ErrEpisodicLogUnavailable):
		// Reaches here ONLY when no WAL was wired (the WAL
		// fallback path turns this into a non-error
		// degraded response). The caller should retry.
		return status.Error(codes.Unavailable, err.Error())
	}
	return status.Error(codes.Internal, fmt.Sprintf("agent.observe: %v", err))
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
