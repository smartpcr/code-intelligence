package scenarios

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// ObserveScenario drives `agent.observe` at the §8.3 sustained
// rate (50 RPS in the nominal envelope). To keep observe
// latency independent of recall latency, the scenario consumes
// from a pre-seeded SyntheticContextIDs pool (round-robin) and
// falls back to a synthetic per-tick UUID when the pool is
// empty. The harness notes this fallback in the calibration
// artifact so a reviewer sees which path was exercised.
type ObserveScenario struct {
	Client              AgentClient
	RepoID              string
	SyntheticContextIDs []string
	OutcomeRotation     []string // closed-set: success/failure/refused/degraded
	RequestTimeout      time.Duration

	// next is a monotonically-incrementing tick counter the
	// scenario uses for context-id rotation, outcome
	// rotation, and synthetic id minting. Safe to mutate
	// from any goroutine via atomic.
	next atomic.Uint64
}

// Verb returns the canonical verb identifier.
func (s *ObserveScenario) Verb() string { return reliability.VerbAgentObserve }

// Execute fires one Observe call.
func (s *ObserveScenario) Execute(ctx context.Context, _ RNG) Sample {
	sample := Sample{Verb: s.Verb(), Started: time.Now()}
	if s.Client == nil {
		sample.Finished = sample.Started
		sample.Err = errNilClient("agent.observe")
		return sample
	}
	tick := s.next.Add(1) - 1
	contextID := s.pickContextID(tick)
	outcome := s.pickOutcome(tick)
	req := ObserveRequest{
		ContextID:  contextID,
		RepoID:     s.RepoID,
		SessionID:  fmt.Sprintf("calibration-session-%d", tick),
		TraceID:    fmt.Sprintf("calibration-trace-%d", tick),
		Outcome:    outcome,
		ActionJSON: []byte(`{"kind":"calibration","tick":` + fmt.Sprintf("%d", tick) + `}`),
	}

	if s.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.RequestTimeout)
		defer cancel()
	}

	sample.Started = time.Now()
	resp, err := s.Client.Observe(ctx, req)
	sample.Finished = time.Now()
	if err != nil {
		sample.Err = err
		return sample
	}
	sample.Degraded = resp.Degraded
	return sample
}

func (s *ObserveScenario) pickContextID(tick uint64) string {
	if len(s.SyntheticContextIDs) > 0 {
		return s.SyntheticContextIDs[tick%uint64(len(s.SyntheticContextIDs))]
	}
	// Synthetic fallback: well-formed UUID-shape id derived
	// from the tick. The AgentService rejects malformed ids
	// at the wire — keep this in the canonical text-UUID
	// shape so we exercise the validated path.
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", tick%999_999_999_999)
}

func (s *ObserveScenario) pickOutcome(tick uint64) string {
	if len(s.OutcomeRotation) == 0 {
		return "success"
	}
	return s.OutcomeRotation[tick%uint64(len(s.OutcomeRotation))]
}
