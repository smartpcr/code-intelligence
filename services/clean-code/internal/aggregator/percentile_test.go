package aggregator

import (
	"math"
	"testing"
)

// TestPercentileContinuous_MatchesPostgresPercentileCont pins
// the percentile algorithm to PostgreSQL's
// `percentile_cont(p) WITHIN GROUP (ORDER BY x)` so a future
// SQL-side rewrite produces byte-identical numbers.
//
// Examples are drawn from the PostgreSQL docs + a small sample
// computed by hand. The interpolation formula at rank `r =
// p*(n-1)` is `xs[floor(r)] + frac*(xs[ceil(r)] - xs[floor(r)])`.
func TestPercentileContinuous_MatchesPostgresPercentileCont(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		xs   []float64
		p    float64
		want float64
	}{
		{name: "empty returns zero", xs: nil, p: 0.5, want: 0},
		{name: "single element returns that element", xs: []float64{42}, p: 0.99, want: 42},
		{name: "p=0 returns min", xs: []float64{3, 1, 2}, p: 0, want: 1},
		{name: "p=1 returns max", xs: []float64{3, 1, 2}, p: 1, want: 3},
		{name: "p50 of even count -> midpoint interpolation",
			// sorted = [1,2,3,4]; rank = 0.5*3 = 1.5; xs[1] + 0.5*(xs[2]-xs[1]) = 2 + 0.5*1 = 2.5
			xs: []float64{4, 1, 3, 2}, p: 0.5, want: 2.5},
		{name: "p50 of odd count -> exact element",
			// sorted = [1,2,3,4,5]; rank = 2.0; xs[2] = 3
			xs: []float64{4, 1, 3, 2, 5}, p: 0.5, want: 3},
		{name: "p90 across 10 elements",
			// sorted = [1..10]; rank = 0.9*9 = 8.1; xs[8] + 0.1*(xs[9]-xs[8]) = 9 + 0.1 = 9.1
			xs: []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, p: 0.9, want: 9.1},
		{name: "p99 across 10 elements",
			// rank = 0.99*9 = 8.91; xs[8] + 0.91*(xs[9]-xs[8]) = 9 + 0.91 = 9.91
			xs: []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, p: 0.99, want: 9.91},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Defensive: copy the slice so the in-place sort
			// inside percentileContinuous can't perturb the
			// next test case.
			xs := append([]float64(nil), tc.xs...)
			got := percentileContinuous(xs, tc.p)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("percentileContinuous(%v, %v) = %v, want %v", tc.xs, tc.p, got, tc.want)
			}
		})
	}
}

// TestSummarise_CountAndMean covers the second leg of the
// summary -- the percentiles are covered above, the count and
// mean are tested explicitly here so a regression in the
// accumulator surfaces directly.
func TestSummarise_CountAndMean(t *testing.T) {
	t.Parallel()
	s := summarise([]float64{2, 4, 6, 8, 10})
	if s.count != 5 {
		t.Errorf("count = %d, want 5", s.count)
	}
	if math.Abs(s.mean-6) > 1e-9 {
		t.Errorf("mean = %v, want 6", s.mean)
	}
	// sorted = [2,4,6,8,10]; rank for p50 = 2.0 -> 6
	if math.Abs(s.p50-6) > 1e-9 {
		t.Errorf("p50 = %v, want 6", s.p50)
	}
}

// TestSummarise_EmptyReturnsZero pins the zero-value escape
// hatch. The aggregator never calls summarise on an empty slice
// (the cohort buckets are populated from observations), but a
// future caller that does should see all zeros rather than NaN.
func TestSummarise_EmptyReturnsZero(t *testing.T) {
	t.Parallel()
	s := summarise(nil)
	if s.count != 0 || s.mean != 0 || s.p50 != 0 || s.p90 != 0 || s.p99 != 0 {
		t.Fatalf("summarise(nil) = %+v, want zero summary", s)
	}
}
