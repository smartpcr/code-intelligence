package aggregator

import (
	"math"
	"sort"
)

// percentileContinuous returns the linearly-interpolated p-th
// percentile of `xs` (0 <= p <= 1). Returns 0 for an empty slice.
//
// The algorithm matches PostgreSQL `percentile_cont(p) WITHIN
// GROUP (ORDER BY x)` so a future SQL-side rewrite of the
// aggregator's percentile math (e.g. Phase 7+ when the
// `metric_sample` row count exceeds in-process feasibility)
// produces byte-identical outputs to today's Go path.
//
// Reference: PostgreSQL docs "9.21.2 Inverse Distribution
// Functions" -- `percentile_cont` interpolates between adjacent
// values at the rank `p*(N-1)`.
//
// The caller is responsible for filtering NaN / +-Inf BEFORE
// calling -- this function does not re-check.
func percentileContinuous(xs []float64, p float64) float64 {
	n := len(xs)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return xs[0]
	}
	// Caller may pass an unsorted slice; sort in-place.
	// Stage 7.1's caller (the [Aggregator.Tick] flow) already
	// allocates a fresh slice per cohort, so the in-place sort
	// has no observable side-effect on the input data layer.
	sort.Float64s(xs)
	if p <= 0 {
		return xs[0]
	}
	if p >= 1 {
		return xs[n-1]
	}
	rank := p * float64(n-1)
	low := int(math.Floor(rank))
	high := int(math.Ceil(rank))
	if low == high {
		return xs[low]
	}
	frac := rank - float64(low)
	return xs[low]*(1-frac) + xs[high]*frac
}

// summarise returns the count, mean, p50, p90, p99 of `values`.
// The input slice may be unsorted; it is sorted in place. Empty
// input returns the zero summary (count=0, all percentiles=0).
//
// Callers MUST have filtered NaN / +-Inf before passing values
// in. The summary is the canonical per-cohort shape consumed by
// the snapshot writers AND by the cross-repo per-repo histogram
// envelope.
type summary struct {
	count int64
	mean  float64
	p50   float64
	p90   float64
	p99   float64
}

func summarise(values []float64) summary {
	n := int64(len(values))
	if n == 0 {
		return summary{}
	}
	var total float64
	for _, v := range values {
		total += v
	}
	return summary{
		count: n,
		mean:  total / float64(n),
		p50:   percentileContinuous(values, 0.50),
		p90:   percentileContinuous(values, 0.90),
		p99:   percentileContinuous(values, 0.99),
	}
}
