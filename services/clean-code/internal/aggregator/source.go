package aggregator

import (
	"context"
	"math"
)

// SampleSource is the read-side dependency the aggregator pulls
// active observations from. The production implementation is
// [PGSampleSource]; tests inject [InMemorySampleSource].
//
// The contract:
//
//   - `ReadActive` MUST return only ACTIVE non-retracted rows
//     (no `metric_retraction.sample_id` reference) whose
//     `value IS NOT NULL` and whose float is finite (not NaN /
//     +-Inf). Filtering happens INSIDE the source so the
//     aggregator's percentile math can assume clean inputs.
//   - The returned slice may be empty -- a fresh deployment with
//     no `metric_sample` rows yields zero observations and the
//     aggregator writes zero snapshot rows for that tick.
//   - Order is not significant; the aggregator groups by
//     `(repo_id, metric_kind, scope_kind)` regardless.
type SampleSource interface {
	ReadActive(ctx context.Context) ([]Observation, error)
}

// InMemorySampleSource is a deterministic test-only [SampleSource]
// that yields a fixed observation set. The aggregator never
// distinguishes the source implementation; the in-memory variant
// is the path used by [Aggregator.Tick] tests that do not need
// the PG round-trip.
//
// Concurrency: read-only after construction; safe for use across
// goroutines.
type InMemorySampleSource struct {
	observations []Observation
}

// NewInMemorySampleSource COPIES `obs` so subsequent mutations to
// the caller's slice do not affect the source. Returns a source
// that yields the supplied set on every [ReadActive] call.
func NewInMemorySampleSource(obs []Observation) *InMemorySampleSource {
	copied := make([]Observation, len(obs))
	copy(copied, obs)
	return &InMemorySampleSource{observations: copied}
}

// ReadActive implements [SampleSource]. The in-memory source
// applies the same null / non-finite filter the PG source does
// (degraded NULL values and NaN / +-Inf are dropped, never
// surfaced to the aggregator math).
func (s *InMemorySampleSource) ReadActive(ctx context.Context) ([]Observation, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Observation, 0, len(s.observations))
	for _, o := range s.observations {
		if math.IsNaN(o.Value) || math.IsInf(o.Value, 0) {
			continue
		}
		out = append(out, o)
	}
	return out, nil
}
