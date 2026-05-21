package scenarios

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// SummarizeScenario drives `agent.summarize` at the ┬º8.3
// sustained rate (5 RPS in the nominal envelope). Alternates
// between node-target and concept-target summaries so both
// surfaces of the verb are exercised.
type SummarizeScenario struct {
	Client         AgentClient
	RepoID         string
	NodeIDs        []string
	ConceptIDs     []string
	MaxTokens      int32
	RequestTimeout time.Duration

	next          atomic.Uint64 // alternation tick (node vs concept)
	nodeCursor    atomic.Uint64 // rotation cursor — advances by 1 per NodeIDs pick
	conceptCursor atomic.Uint64 // rotation cursor — advances by 1 per ConceptIDs pick
}

// Verb returns the canonical verb identifier.
func (s *SummarizeScenario) Verb() string { return reliability.VerbAgentSummarize }

// Execute fires one Summarize call.
func (s *SummarizeScenario) Execute(ctx context.Context, _ RNG) Sample {
	sample := Sample{Verb: s.Verb(), Started: time.Now()}
	if s.Client == nil {
		sample.Finished = sample.Started
		sample.Err = errNilClient("agent.summarize")
		return sample
	}
	tick := s.next.Add(1) - 1
	req := SummarizeRequest{
		RepoID:    s.RepoID,
		MaxTokens: s.MaxTokens,
	}
	// Alternate node Γåö concept target. When one pool is
	// empty fall back to the other so the harness still
	// drives the envelope.
	switch {
	case tick%2 == 0 && len(s.NodeIDs) > 0:
		idx := s.nodeCursor.Add(1) - 1
		req.NodeID = s.NodeIDs[idx%uint64(len(s.NodeIDs))]
	case len(s.ConceptIDs) > 0:
		idx := s.conceptCursor.Add(1) - 1
		req.ConceptID = s.ConceptIDs[idx%uint64(len(s.ConceptIDs))]
	case len(s.NodeIDs) > 0:
		idx := s.nodeCursor.Add(1) - 1
		req.NodeID = s.NodeIDs[idx%uint64(len(s.NodeIDs))]
	default:
		// Neither pool supplied: synthetic node id so the
		// wire shape is well-formed.
		req.NodeID = "calibration-synthetic-node"
	}

	if s.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.RequestTimeout)
		defer cancel()
	}

	sample.Started = time.Now()
	resp, err := s.Client.Summarize(ctx, req)
	sample.Finished = time.Now()
	if err != nil {
		sample.Err = err
		return sample
	}
	sample.Degraded = resp.Degraded
	return sample
}
