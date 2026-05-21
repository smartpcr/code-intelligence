package scenarios

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// CallChainQueryScenario drives `agent.expand` at the §8.3
// sustained rate (20 RPS in the nominal envelope). It exercises
// the **call-chain context** path called out in the AGENT-MEMORY
// story description: BFS expansion over `static_calls` /
// `observed_calls` edges from a seed Method/Block node. The
// scenario rotates direction (callees ↔ callers) and depth
// (1 .. MaxDepth) so the BFS budget logic is exercised across
// the full surface.
//
// Why this is named `CallChainQueryScenario` and not
// `ExpandScenario`: the workstream brief explicitly lists a
// `CallChainQueryScenario` (the C#-style target paths in the
// brief named it that), and the story description anchors on
// "build graph/expertise on top-down repo structure and
// call-chain context". The verb under the hood IS `agent.expand`
// (verified at `proto/agent.proto` lines 67-77), but the
// scenario name carries the workstream-level intent: "drive the
// call-chain BFS workload".
type CallChainQueryScenario struct {
	Client         AgentClient
	RepoID         string
	SeedNodeIDs    []string
	MaxDepth       int32 // hard ceiling per agent.expand contract (10); calibration default 3
	MaxNodes       int32
	MaxEdges       int32
	RequestTimeout time.Duration

	next atomic.Uint64
}

// Verb returns the canonical verb identifier (`agent.expand`).
// The §8.3 envelope and the harness's per-verb wiring key off
// the verb name, not the scenario type name.
func (s *CallChainQueryScenario) Verb() string { return reliability.VerbAgentExpand }

// Execute fires one Expand call. Rotates direction and depth
// deterministically so a fixed run produces a stable artifact.
func (s *CallChainQueryScenario) Execute(ctx context.Context, _ RNG) Sample {
	sample := Sample{Verb: s.Verb(), Started: time.Now()}
	if s.Client == nil {
		sample.Finished = sample.Started
		sample.Err = errNilClient("agent.expand")
		return sample
	}
	tick := s.next.Add(1) - 1
	req := ExpandRequest{
		RepoID:    s.RepoID,
		MaxNodes:  s.MaxNodes,
		MaxEdges:  s.MaxEdges,
		Direction: directionFromTick(tick),
		Depth:     s.depthFromTick(tick),
	}
	if len(s.SeedNodeIDs) > 0 {
		req.NodeID = s.SeedNodeIDs[tick%uint64(len(s.SeedNodeIDs))]
	} else {
		req.NodeID = "calibration-synthetic-seed"
	}

	if s.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.RequestTimeout)
		defer cancel()
	}

	sample.Started = time.Now()
	resp, err := s.Client.Expand(ctx, req)
	sample.Finished = time.Now()
	if err != nil {
		sample.Err = err
		return sample
	}
	sample.Degraded = resp.Degraded
	return sample
}

// directionFromTick rotates callees / callers per tick so the
// BFS exercises both edge directions across the run.
func directionFromTick(tick uint64) string {
	if tick%2 == 0 {
		return "callees"
	}
	return "callers"
}

// depthFromTick rotates 1..max so the BFS hop budget logic is
// exercised across the run. Defaults to depth 3 (the §8.3
// "depth ≤ 3" envelope clause) when MaxDepth is unset.
func (s *CallChainQueryScenario) depthFromTick(tick uint64) int32 {
	maxDepth := s.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 3
	}
	return int32(tick%uint64(maxDepth)) + 1
}
