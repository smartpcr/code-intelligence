package scenarios

import (
	"context"
	"time"

	"github.com/smartpcr/code-intelligence/services/agent-memory/internal/reliability"
)

// RecallScenario drives `agent.recall` at the §8.3 sustained
// rate (50 RPS in the nominal envelope). On top of the latency
// observation every Scenario emits, RecallScenario ALSO
// computes the labelled-query learning-quality measurements
// (rank-of-correct-node and concept-hit) when the rotating
// LabeledQuery carries the expected ground truth.
type RecallScenario struct {
	Client         AgentClient
	RepoID         string
	K              int
	Queries        []RecallQuery
	DefaultKinds   []string
	RequestTimeout time.Duration
}

// RecallQuery is the per-tick fixture the scenario rotates
// through. Construct from a calibration.LabeledQuery via
// [QueriesFromLabeled].
type RecallQuery struct {
	Query              string
	ExpectedNodeID     string
	ExpectedConceptIDs []string
	Kinds              []string
}

// QueriesFromLabeled lifts the calibration.LabeledQuery fixture
// into the scenario's local fixture type. Kept here so the
// scenarios package does not import the calibration package
// (which would create a cycle once the harness imports both).
//
// We intentionally accept the small struct shape rather than
// the concrete type from the calibration package: a thin
// shape-conversion helper means a future fixture source (CSV,
// YAML, recorded prod traces) plugs in without touching this
// package.
func QueriesFromLabeled(src []struct {
	Query              string
	ExpectedNodeID     string
	ExpectedConceptIDs []string
	Kinds              []string
}) []RecallQuery {
	out := make([]RecallQuery, len(src))
	for i, q := range src {
		out[i] = RecallQuery{
			Query:              q.Query,
			ExpectedNodeID:     q.ExpectedNodeID,
			ExpectedConceptIDs: q.ExpectedConceptIDs,
			Kinds:              q.Kinds,
		}
	}
	return out
}

// Verb returns the canonical verb identifier.
func (s *RecallScenario) Verb() string { return reliability.VerbAgentRecall }

// Execute fires one Recall call and computes any applicable
// labelled-query measurements. Empty Queries falls back to a
// synthetic query string so the scenario still drives the
// envelope (the artifact then reports `Evaluated=0` and the
// learning-quality numbers as `n/a`).
func (s *RecallScenario) Execute(ctx context.Context, rng RNG) Sample {
	sample := Sample{Verb: s.Verb(), Started: time.Now()}
	if s.Client == nil {
		// Scenario contract: nil client is a failed sample,
		// not a panic. See scenario.go §"MUST NOT panic on a
		// nil or returned-error client".
		sample.Finished = sample.Started
		sample.Err = errNilClient("agent.recall")
		return sample
	}
	q := s.pickQuery(rng)
	req := RecallRequest{
		RepoID: s.RepoID,
		Query:  q.Query,
		K:      s.K,
		Kinds:  q.Kinds,
	}
	if len(req.Kinds) == 0 {
		req.Kinds = s.DefaultKinds
	}
	if req.K <= 0 {
		req.K = 20
	}

	if s.RequestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.RequestTimeout)
		defer cancel()
	}

	sample.Started = time.Now()
	resp, err := s.Client.Recall(ctx, req)
	sample.Finished = time.Now()
	if err != nil {
		sample.Err = err
		return sample
	}
	sample.Degraded = resp.Degraded
	if q.ExpectedNodeID != "" {
		sample.RankMeasured = true
		sample.RecallRank = rankOf(q.ExpectedNodeID, resp.NodeIDs)
	}
	if len(q.ExpectedConceptIDs) > 0 {
		sample.ConceptHitMeasured = true
		sample.ConceptHit = anyHit(q.ExpectedConceptIDs, resp.ConceptIDs, s.K)
	}
	return sample
}

// pickQuery rotates through the fixture deterministically by
// rng.Intn so a fixed seed reproduces the same tick order.
// Falls back to a fixed synthetic query when no fixture is
// supplied so the scenario can still drive the envelope.
func (s *RecallScenario) pickQuery(rng RNG) RecallQuery {
	if len(s.Queries) == 0 {
		return RecallQuery{Query: "calibration synthetic query"}
	}
	if rng == nil {
		return s.Queries[0]
	}
	idx := rng.Intn(len(s.Queries))
	return s.Queries[idx]
}

// rankOf returns the 1-based rank of needle in haystack, or 0
// when not found. Caller treats 0 as "miss"; the
// calibration.AggregateLearningQuality function buckets that
// into the worst-rank slot (K+1).
func rankOf(needle string, haystack []string) int {
	for i, v := range haystack {
		if v == needle {
			return i + 1
		}
	}
	return 0
}

// anyHit returns true when any of `expected` appears in the
// first `k` entries of `candidates`. k <= 0 scans the full
// slice.
func anyHit(expected, candidates []string, k int) bool {
	limit := len(candidates)
	if k > 0 && k < limit {
		limit = k
	}
	want := make(map[string]struct{}, len(expected))
	for _, e := range expected {
		want[e] = struct{}{}
	}
	for i := 0; i < limit; i++ {
		if _, ok := want[candidates[i]]; ok {
			return true
		}
	}
	return false
}
